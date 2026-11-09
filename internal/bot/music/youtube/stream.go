package youtube

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/music"
)

const (
	// defaultMaxStreamBytes bounds one track's Ogg Opus output: a 24 h track at 96 kbit/s plus headroom.
	defaultMaxStreamBytes = 1<<30 + 256<<20
	// streamBufferBytes is how much a started stream may run ahead of its reader (about 10 s of 96 kbit/s Opus).
	streamBufferBytes = 128 << 10
	// prefetchTTL is how long a pre-buffered stream waits to be used before it is dropped.
	prefetchTTL = 90 * time.Second
	// maxPrefetched bounds pre-buffered streams in the whole process (each holds one sandboxed ffmpeg and at most
	// 128 KiB); each owner holds at most one.
	maxPrefetched = 32
	// urlMargin is how much longer than the track a cached stream URL must stay valid.
	urlMargin = time.Minute
)

// ErrStreamURL means the resolved stream URL is not one the sandbox may fetch.
var ErrStreamURL = errors.New("youtube: stream URL refused")

// localStream is nil in every production build. Only fixture_testhost.go sets
// it, so that a file: stream (a test fixture) is read by ffmpeg directly.
var localStream func(ctx context.Context, ffmpeg, path string, startMS int64) (io.ReadCloser, error)

// StreamStarter turns a resolved stream URL into one Ogg Opus byte stream with
// a single ffmpeg, run in the same bubblewrap sandbox and behind the same
// egress broker as every other network process of the Bot. It implements
// PCMSourceStarter (Start) and StartAt; its readers are Ogg Opus, not PCM.
type StreamStarter struct {
	cfg      SandboxConfig
	resolver Resolver
	ctx      context.Context
	cancel   context.CancelFunc

	mu         sync.Mutex
	prefetched map[prefetchID]*prefetchedStream // keyed by owner and video: one community never claims another's buffer
	order      []prefetchID
}

type prefetchID struct{ owner, video string }

func prefetchKey(owner, canonical string) prefetchID { return prefetchID{owner, canonical} }

type prefetchedStream struct {
	stream *bufferedStream
	timer  *time.Timer
}

// NewStreamStarter validates the tool paths in cfg (bubblewrap, python3, ffmpeg).
func NewStreamStarter(cfg SandboxConfig, resolver Resolver) (*StreamStarter, error) {
	if resolver == nil {
		return nil, ErrRunnerRequired
	}
	var err error
	if cfg, err = resolveSandboxConfig(cfg, true); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &StreamStarter{cfg: cfg, resolver: resolver, ctx: ctx, cancel: cancel, prefetched: map[prefetchID]*prefetchedStream{}}, nil
}

// Check reports whether the tools the stream needs are present.
func (s *StreamStarter) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.resolver == nil {
		return ErrRunnerRequired
	}
	return nil
}

// Close drops every pre-buffered stream and stops their ffmpeg processes.
func (s *StreamStarter) Close() error {
	s.cancel()
	s.mu.Lock()
	entries := s.prefetched
	s.prefetched = map[prefetchID]*prefetchedStream{}
	s.order = nil
	s.mu.Unlock()
	for _, entry := range entries {
		entry.timer.Stop()
		_ = entry.stream.Close()
	}
	return nil
}

func (s *StreamStarter) Start(ctx context.Context, source string) (io.ReadCloser, error) {
	return s.StartAt(ctx, source, 0)
}

// StartAt starts the stream at ms into the track. Only a start at 0 can use a
// pre-buffered stream.
func (s *StreamStarter) StartAt(ctx context.Context, source string, ms int64) (io.ReadCloser, error) {
	canonical, err := canonicalVideo(source)
	if err != nil {
		return nil, err
	}
	owner, err := ownerFrom(ctx)
	if err != nil {
		return nil, err
	}
	began := time.Now()
	if ms <= 0 {
		if stream := s.takePrefetched(owner, canonical); stream != nil {
			if err := stream.waitFirst(ctx); err == nil {
				music.MarkResolve(ctx, 0, false)
				music.MarkFirstByte(ctx, time.Since(began))
				return &opusStream{stream}, nil
			}
			_ = stream.Close()
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
		}
	}
	stream, err := s.open(ctx, ctx, canonical, ms)
	if err != nil {
		return nil, err
	}
	music.MarkFirstByte(ctx, time.Since(began))
	return &opusStream{stream}, nil
}

// Prefetch resolves source and starts its ffmpeg ahead of time. The stream
// buffers a bounded amount and waits for the Start that claims it.
func (s *StreamStarter) Prefetch(ctx context.Context, source string) error {
	canonical, err := canonicalVideo(source)
	if err != nil {
		return err
	}
	owner, err := ownerFrom(ctx)
	if err != nil {
		return err
	}
	key := prefetchKey(owner, canonical)
	s.mu.Lock()
	_, held := s.prefetched[key]
	s.mu.Unlock()
	if held {
		return nil
	}
	// The pre-buffer outlives the caller's context: it belongs to the starter until claimed.
	stream, err := s.open(ctx, WithOwner(s.ctx, owner), canonical, 0)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		_ = stream.Close()
		return context.Canceled
	}
	if _, ok := s.prefetched[key]; ok {
		s.mu.Unlock()
		_ = stream.Close()
		return nil
	}
	var evicted []*prefetchedStream
	// One held stream per owner: the owner's next track replaces whatever it buffered before.
	for _, other := range append([]prefetchID(nil), s.order...) {
		if other.owner == owner {
			if entry, ok := s.prefetched[other]; ok {
				delete(s.prefetched, other)
				evicted = append(evicted, entry)
			}
			s.removeOrderLocked(other)
		}
	}
	for len(s.order) >= maxPrefetched {
		oldest := s.order[0]
		s.order = s.order[1:]
		if entry, ok := s.prefetched[oldest]; ok {
			delete(s.prefetched, oldest)
			evicted = append(evicted, entry)
		}
	}
	entry := &prefetchedStream{stream: stream}
	entry.timer = time.AfterFunc(prefetchTTL, func() {
		if s.takePrefetchedEntry(key, entry) {
			_ = stream.Close()
		}
	})
	s.prefetched[key] = entry
	s.order = append(s.order, key)
	s.mu.Unlock()
	for _, old := range evicted {
		old.timer.Stop()
		_ = old.stream.Close()
	}
	return nil
}

func (s *StreamStarter) takePrefetched(owner, canonical string) *bufferedStream {
	key := prefetchKey(owner, canonical)
	s.mu.Lock()
	entry := s.prefetched[key]
	delete(s.prefetched, key)
	s.removeOrderLocked(key)
	s.mu.Unlock()
	if entry == nil {
		return nil
	}
	entry.timer.Stop()
	return entry.stream
}

func (s *StreamStarter) takePrefetchedEntry(key prefetchID, entry *prefetchedStream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prefetched[key] != entry {
		return false
	}
	delete(s.prefetched, key)
	s.removeOrderLocked(key)
	return true
}

func (s *StreamStarter) removeOrderLocked(target prefetchID) {
	for i, key := range s.order {
		if key == target {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}

func canonicalVideo(source string) (string, error) {
	kind, canonical, err := NormalizeInput(source)
	if err != nil || kind != "video" || canonical != source {
		return "", ErrInvalidInput
	}
	return canonical, nil
}

// open resolves canonical and starts ffmpeg, retrying once with a fresh lookup
// when the stream fails before its first byte (an expired or refused URL).
// resolveCtx bounds the lookup and the wait; processCtx owns the process.
func (s *StreamStarter) open(resolveCtx, processCtx context.Context, canonical string, startMS int64) (*bufferedStream, error) {
	var lastErr error
	refreshed := false
	var refused *Resolved // a stream googlevideo refused (403): the retry asks the next player client
	for attempt := 0; attempt < 2; attempt++ {
		var resolved Resolved
		var err error
		if next, ok := s.resolver.(nextClientResolver); ok && refused != nil {
			resolved, err = next.ResolveNextClient(resolveCtx, canonical, *refused)
		} else {
			resolved, err = s.resolver.Resolve(resolveCtx, canonical)
		}
		if err != nil {
			return nil, err
		}
		// A cached URL must outlive the whole track (ffmpeg reconnects mid-track with it); if it will not, look again.
		if !refreshed && !resolved.Expires.IsZero() && resolved.Track.DurationMS > 0 &&
			time.Until(resolved.Expires) < time.Duration(resolved.Track.DurationMS)*time.Millisecond+urlMargin {
			refreshed = true
			s.invalidate(resolveCtx, canonical)
			attempt--
			continue
		}
		source, err := s.launch(WithDiagnosticLogger(context.WithValue(processCtx, diagnosticInputKey{}, canonical), s.cfg.Logger), resolved.StreamURL, resolved.UserAgent, startMS)
		if err != nil {
			return nil, err
		}
		stream := newBufferedStream(source, streamBufferBytes)
		if err := stream.waitFirst(resolveCtx); err != nil {
			_ = stream.Close()
			if resolveCtx.Err() != nil {
				return nil, context.Cause(resolveCtx)
			}
			lastErr = err
			s.invalidate(resolveCtx, canonical)
			if errors.Is(err, ErrSandboxUnavailable) || errors.Is(err, ErrPCMLimit) {
				return nil, err
			}
			if _, ok := s.resolver.(nextClientResolver); ok && streamRefused(err) && nextPlayerClient(resolved.Client) != "" {
				failed := resolved
				refused = &failed
				s.logger().WarnContext(resolveCtx, "youtube stream refused at its start; trying the next player client",
					"client", resolved.Client, "next_client", nextPlayerClient(resolved.Client))
			}
			continue
		}
		if refused != nil {
			s.logger().InfoContext(resolveCtx, "youtube stream started on the next player client", "client", resolved.Client, "refused_client", refused.Client)
		}
		return stream, nil
	}
	return nil, lastErr
}

// nextClientResolver is a resolver that can look a video up again with the next player client (WarmResolver).
type nextClientResolver interface {
	ResolveNextClient(ctx context.Context, input string, failed Resolved) (Resolved, error)
}

// streamRefused reports a stream start googlevideo refused with a 403.
func streamRefused(err error) bool {
	var process *ProcessError
	return errors.As(err, &process) && process.class == "http_403"
}

func (s *StreamStarter) logger() *slog.Logger {
	if s.cfg.Logger != nil {
		return s.cfg.Logger
	}
	return slog.Default()
}

func (s *StreamStarter) invalidate(ctx context.Context, canonical string) {
	switch r := s.resolver.(type) {
	case interface {
		InvalidateCtx(context.Context, string)
	}:
		r.InvalidateCtx(ctx, canonical)
	case interface{ Invalidate(string) }:
		r.Invalidate(canonical)
	}
}

func (s *StreamStarter) launch(ctx context.Context, streamURL, userAgent string, startMS int64) (io.ReadCloser, error) {
	if path, ok := strings.CutPrefix(streamURL, "file://"); ok {
		if localStream == nil {
			return nil, ErrStreamURL
		}
		return localStream(ctx, s.cfg.FFmpeg, path, startMS)
	}
	if _, _, err := checkStreamURL(streamURL); err != nil {
		return nil, ErrStreamURL
	}
	limit := s.cfg.MaxStreamBytes
	if limit <= 0 || limit > defaultMaxStreamBytes {
		limit = defaultMaxStreamBytes
	}
	maxDuration := s.cfg.StreamTimeout
	if maxDuration <= 0 || maxDuration > MaxDuration {
		maxDuration = MaxDuration
	}
	args := append([]string{"exec", strconv.FormatInt(limit, 10), s.cfg.FFmpeg}, ffmpegStreamArgs(streamURL, startMS, true, userAgent)...)
	process, err := launchNetworkWorker(ctx, s.cfg.Bubblewrap, s.cfg.Python, []string{s.cfg.FFmpeg}, args, defaultSandboxStartupTimeout, maxDuration)
	if err != nil {
		return nil, err
	}
	return &boundedProcessPCM{process: process, limit: limit}, nil
}

// ffmpegStreamArgs is the one ffmpeg of the music path: the stream (a URL the
// sandbox fetches through the egress broker, or a fixture file) to 48 kHz
// stereo Ogg Opus in 20 ms frames, which the Bot forwards without re-encoding.
func ffmpegStreamArgs(input string, startMS int64, network bool, userAgent string) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if network {
		// Only https over the loopback proxy: no file, concat, subfile, data or nested protocol may be opened.
		args = append(args, "-protocol_whitelist", "https,tls,tcp,httpproxy",
			"-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_delay_max", "2", "-http_proxy", "http://127.0.0.1:3128")
		if userAgent != "" {
			args = append(args, "-user_agent", userAgent)
		}
	} else {
		args = append(args, "-protocol_whitelist", "file")
	}
	if startMS > 0 {
		args = append(args, "-ss", strconv.FormatFloat(float64(startMS)/1000, 'f', 3, 64))
	}
	return append(args, "-i", input, "-vn", "-sn", "-dn",
		"-map", "0:a:0", "-ar", "48000", "-ac", "2",
		"-c:a", "libopus", "-application", "audio", "-b:a", "96k", "-vbr", "off", "-dtx", "0", "-frame_duration", "20",
		"-flush_packets", "1", "-page_duration", "20000", "-f", "ogg", "pipe:1")
}

// opusStream marks a reader as Ogg Opus so the session forwards it without a
// second encoder.
type opusStream struct{ *bufferedStream }

func (*opusStream) OggOpus() bool { return true }

// bufferedStream reads its source ahead of the consumer, up to limit bytes, so
// a pre-buffered track starts with audio already waiting and a slow reader
// never leaves ffmpeg blocked mid-handshake.
type bufferedStream struct {
	src    io.ReadCloser
	limit  int
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	err    error
	closed bool
	done   chan struct{}
}

func newBufferedStream(src io.ReadCloser, limit int) *bufferedStream {
	b := &bufferedStream{src: src, limit: limit, done: make(chan struct{})}
	b.cond = sync.NewCond(&b.mu)
	go b.fill()
	return b
}

func (b *bufferedStream) fill() {
	defer close(b.done)
	chunk := make([]byte, 16<<10)
	for {
		b.mu.Lock()
		for b.buf.Len() >= b.limit && !b.closed {
			b.cond.Wait()
		}
		closed := b.closed
		b.mu.Unlock()
		if closed {
			return
		}
		n, err := b.src.Read(chunk)
		b.mu.Lock()
		b.buf.Write(chunk[:n])
		if err != nil {
			b.err = err
		}
		b.cond.Broadcast()
		b.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// waitFirst blocks until the stream delivered a byte or failed.
func (b *bufferedStream) waitFirst(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() {
		b.mu.Lock()
		b.cond.Broadcast()
		b.mu.Unlock()
	})
	defer stop()
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 && b.err == nil && !b.closed {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		b.cond.Wait()
	}
	if b.buf.Len() > 0 {
		return nil
	}
	if b.closed {
		return io.ErrClosedPipe
	}
	if errors.Is(b.err, io.EOF) {
		return ErrPCMStart // a stream that ends before its first byte is a failed start
	}
	return b.err
}

func (b *bufferedStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 && b.err == nil && !b.closed {
		b.cond.Wait()
	}
	if b.buf.Len() > 0 {
		n, _ := b.buf.Read(p)
		b.cond.Broadcast()
		return n, nil
	}
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	return 0, b.err
}

func (b *bufferedStream) Close() error {
	b.mu.Lock()
	already := b.closed
	b.closed = true
	b.cond.Broadcast()
	b.mu.Unlock()
	err := b.src.Close()
	if !already {
		<-b.done
	}
	return err
}

// SourceGone reports that the source ended or failed and cannot be resumed.
func (b *bufferedStream) SourceGone() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || (b.err != nil && (!errors.Is(b.err, io.EOF) || b.buf.Len() == 0)) {
		return true
	}
	if b.err != nil {
		return false // finished cleanly with audio still buffered: still resumable
	}
	if gone, ok := b.src.(interface{ SourceGone() bool }); ok {
		return gone.SourceGone()
	}
	return false
}

package youtube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
)

const (
	DefaultMaxPCMBytes = 512 << 20
)

var (
	ErrPCMLimit           = errors.New("youtube: decoded PCM exceeds limit")
	ErrPCMStart           = errors.New("youtube: audio process failed")
	ErrPCMTooLarge        = ErrPCMLimit
	ErrPCMReaderInput     = errors.New("youtube: invalid bounded PCM reader input")
	ErrSandboxUnavailable = errors.New("youtube: production network sandbox unavailable")
)

// BoundedPCMReader limits bytes read from a streaming decoder and closes the
// source when its context is canceled. Reads must be made by one goroutine.
type BoundedPCMReader struct {
	ctx       context.Context
	source    io.ReadCloser
	limit     int64
	read      int64
	closeOnce sync.Once
	done      chan struct{}
	closeErr  error
}

// NewBoundedPCMReader wraps a decoder's PCM stream. It returns ErrPCMTooLarge
// only if the source has bytes beyond maxBytes.
func NewBoundedPCMReader(ctx context.Context, source io.ReadCloser, maxBytes int64) (*BoundedPCMReader, error) {
	if ctx == nil || source == nil || maxBytes <= 0 {
		return nil, ErrPCMReaderInput
	}
	ctx = client.WithoutCooperativeStop(client.WithBackground(ctx))
	r := &BoundedPCMReader{ctx: ctx, source: source, limit: maxBytes, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = r.Close()
		case <-r.done:
		}
	}()
	return r, nil
}

func (r *BoundedPCMReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	remaining := r.limit - r.read
	if remaining == 0 {
		var extra [1]byte
		n, err := r.source.Read(extra[:])
		if n > 0 {
			return 0, ErrPCMTooLarge
		}
		if ctxErr := r.ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, err
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.source.Read(p)
	r.read += int64(n)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func (r *BoundedPCMReader) Close() error {
	r.closeOnce.Do(func() { close(r.done); r.closeErr = r.source.Close() })
	return r.closeErr
}

// PCMStarter runs yt-dlp and FFmpeg in a bubblewrap network namespace. The only
// external path is a bounded CONNECT broker restricted to public YouTube hosts.
// Bubblewrap and Python 3 are explicit host prerequisites; absence fails closed.
type PCMStarter struct {
	// Logger carries installation-scoped sandbox diagnostics. Nil uses slog.Default.
	Logger      *slog.Logger
	Bubblewrap  string
	Python      string
	YTDLP       string
	FFmpeg      string
	Node        string
	Timeout     time.Duration
	MaxDuration time.Duration
	MaxPCMBytes int64
}

func (s PCMStarter) Start(ctx context.Context, source string) (io.ReadCloser, error) {
	if ctx == nil || s.YTDLP == "" || s.FFmpeg == "" {
		return nil, ErrPCMStart
	}
	kind, canonical, err := NormalizeInput(source)
	if err != nil || kind != "video" || canonical != source {
		return nil, ErrInvalidInput
	}
	return s.startIsolated(WithDiagnosticLogger(ctx, s.Logger), canonical)
}

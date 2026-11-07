package youtube

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/music"
)

const (
	// DefaultResolveCache is how long a lookup is reused (spec: lookups cached 6 h).
	DefaultResolveCache = 6 * time.Hour
	// DefaultLookupTimeout bounds one yt-dlp lookup; a worker that exceeds it is killed.
	DefaultLookupTimeout = 30 * time.Second

	maxWarmCacheEntries  = 512
	maxOwnerCacheEntries = 128
	maxWorkerStartTries  = 4
	workerBackoffInitial = 100 * time.Millisecond
	workerBackoffMax     = 10 * time.Second
	// A short life bounds what one worker's tmpfs and pid namespace share between lookups.
	workerMaxLifetime = time.Hour
	// A worker whose lookups fail this many times in a row (other than a video that is simply unavailable) is
	// replaced: that is what an exhausted egress budget or a broken session looks like.
	workerMaxFailures = 3
	// workerCancelGrace is how long a cancelled lookup waits for the worker to acknowledge the cancel.
	workerCancelGrace = 2 * time.Second
	// A reply line is JSON-escaped output: control characters grow six-fold.
	workerReplyLimit = 6*MaxMetadataBytes + 64<<10
	// The worker's egress broker stops a process at 1 GiB of tunnelled bytes (maxProcessBytes), and a lookup moves
	// a few MiB. The worker is replaced long before that, and its replacement starts before it is closed.
	workerRecycleAfter = 200
	workerRetireGrace  = 2 * time.Minute
	// streamURLSlack keeps a cached stream URL from expiring mid-fetch.
	streamURLSlack = time.Minute
)

// SandboxConfig names the tools the sandboxed workers run. The sandbox itself
// (mounts, network namespace, egress broker) is the one PCMStarter has always
// used; this type only selects the executables.
type SandboxConfig struct {
	// Logger carries installation-scoped diagnostics. Nil uses slog.Default.
	Logger     *slog.Logger
	Bubblewrap string
	Python     string
	YTDLP      string
	FFmpeg     string
	Node       string
	// LookupTimeout bounds one lookup before its worker is killed and restarted.
	LookupTimeout time.Duration
	// MaxStreamBytes bounds one track's Ogg Opus stream. Zero selects 512 MiB.
	MaxStreamBytes int64
	// StreamTimeout bounds how long ffmpeg may run for one track. Zero selects the maximum track length.
	StreamTimeout time.Duration
}

// Resolved is one cached lookup: the queue descriptor and the audio stream URL
// yt-dlp chose. Expires is when the cache stops serving it.
type Resolved struct {
	Track     music.Track
	StreamURL string
	Expires   time.Time
	// UserAgent is the User-Agent of the format yt-dlp chose; the stream is fetched with it.
	UserAgent string
}

type ownerKey struct{}

// WithOwner scopes lookups and pre-buffers made under ctx to one community (or
// installation). Nothing cached or buffered for one owner is visible to another.
func WithOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}

func ownerOf(ctx context.Context) string {
	owner, _ := ctx.Value(ownerKey{}).(string)
	return owner
}

// Resolver turns a video URL or search text into a Resolved.
type Resolver interface {
	Resolve(ctx context.Context, input string) (Resolved, error)
}

// lookupWorker runs yt-dlp for one request at a time per call, inside the sandbox.
type lookupWorker interface {
	Do(ctx context.Context, args []string, limit int) ([]byte, error)
	Close() error
	Done() <-chan struct{}
}

type warmDeps struct {
	start func(ctx context.Context) (lookupWorker, error)
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// cacheEntry is one cached lookup; used orders entries for LRU eviction.
type cacheEntry struct {
	resolved Resolved
	used     uint64
}

type warmCall struct {
	done chan struct{}
	res  Resolved
	err  error
}

// WarmResolver keeps one sandboxed yt-dlp worker alive and caches lookups.
// Every lookup costs at most one yt-dlp invocation; a repeated query inside the
// cache window costs none.
type WarmResolver struct {
	cfg     SandboxConfig
	ttl     time.Duration
	timeout time.Duration
	deps    warmDeps
	ctx     context.Context
	cancel  context.CancelFunc

	recycleAfter int
	retireGrace  time.Duration
	served       int
	startMu      sync.Mutex // serialises worker starts
	mu           sync.Mutex
	worker       lookupWorker
	failures     int
	consecutive  int // consecutive lookups on the current worker that failed for reasons other than the video
	nextStart    time.Time
	cache        map[ownedKey]*cacheEntry
	inflight     map[ownedKey]*warmCall
	tick         uint64
	closed       bool
}

// NewWarmResolver validates cfg's tool paths and starts the worker in the
// background so the first lookup finds it warm. cache <= 0 selects 6 h.
func NewWarmResolver(cfg SandboxConfig, cache time.Duration) (*WarmResolver, error) {
	var err error
	if cfg, err = resolveSandboxConfig(cfg, false); err != nil {
		return nil, err
	}
	r := newWarmResolver(cfg, cache, warmDeps{})
	r.deps.start = func(ctx context.Context) (lookupWorker, error) { return startSandboxWorker(ctx, cfg) }
	go r.warmUp()
	return r, nil
}

func resolveSandboxConfig(cfg SandboxConfig, needFFmpeg bool) (SandboxConfig, error) {
	var err error
	if cfg.Bubblewrap, err = lookupTool(cfg.Bubblewrap, "bwrap"); err != nil {
		return cfg, ErrSandboxUnavailable
	}
	if cfg.Python, err = lookupTool(cfg.Python, "python3"); err != nil {
		return cfg, ErrSandboxUnavailable
	}
	if cfg.YTDLP != "" || !needFFmpeg {
		if cfg.YTDLP, err = lookupTool(cfg.YTDLP, "yt-dlp"); err != nil {
			return cfg, ErrExtractFailed
		}
	}
	if cfg.Node != "" {
		if cfg.Node, err = lookupTool(cfg.Node, "node"); err != nil {
			return cfg, ErrExtractFailed
		}
	}
	if cfg.FFmpeg != "" || needFFmpeg {
		if cfg.FFmpeg, err = lookupTool(cfg.FFmpeg, "ffmpeg"); err != nil {
			return cfg, ErrPCMStart
		}
	}
	return cfg, nil
}

func lookupTool(path, name string) (string, error) {
	return PCMStarter{}.executable(path, name)
}

func newWarmResolver(cfg SandboxConfig, ttl time.Duration, deps warmDeps) *WarmResolver {
	if ttl <= 0 {
		ttl = DefaultResolveCache
	}
	timeout := cfg.LookupTimeout
	if timeout <= 0 {
		timeout = DefaultLookupTimeout
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.sleep == nil {
		deps.sleep = func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &WarmResolver{
		cfg: cfg, ttl: ttl, timeout: timeout, deps: deps, ctx: ctx, cancel: cancel,
		recycleAfter: workerRecycleAfter, retireGrace: workerRetireGrace,
		cache: map[ownedKey]*cacheEntry{}, inflight: map[ownedKey]*warmCall{},
	}
}

// Close kills the worker. Lookups after Close fail.
func (r *WarmResolver) Close() error {
	r.cancel()
	r.mu.Lock()
	r.closed = true
	w := r.worker
	r.worker = nil
	r.mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
	return nil
}

// warmUp starts the worker ahead of the first lookup. A failure only delays
// the cost to that lookup.
func (r *WarmResolver) warmUp() {
	ctx, cancel := context.WithTimeout(r.ctx, 60*time.Second)
	defer cancel()
	_, _ = r.acquire(ctx)
}

// Resolve returns the cached lookup for input or runs yt-dlp once. input is a
// video URL (any accepted form) or search text; the first search result is used.
func (r *WarmResolver) Resolve(ctx context.Context, input string) (Resolved, error) {
	if ctx == nil {
		return Resolved{}, ErrExtractFailed
	}
	kind, canonical, err := NormalizeInput(input)
	if err != nil {
		return Resolved{}, err
	}
	if kind == "playlist" {
		return Resolved{}, ErrInvalidInput
	}
	owner, err := ownerFrom(ctx)
	if err != nil {
		return Resolved{}, err
	}
	key := ownedKey{owner, cacheKey(kind, canonical)}
	started := time.Now()
	r.mu.Lock()
	if entry, ok := r.cache[key]; ok {
		if r.deps.now().Before(entry.resolved.Expires) {
			r.tick++
			entry.used = r.tick
			res := entry.resolved
			r.mu.Unlock()
			music.MarkResolve(ctx, time.Since(started), false)
			return res, nil
		}
		delete(r.cache, key)
	}
	if call, ok := r.inflight[key]; ok {
		r.mu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return Resolved{}, call.err
			}
			music.MarkResolve(ctx, time.Since(started), true)
			return call.res, nil
		case <-ctx.Done():
			return Resolved{}, context.Cause(ctx)
		}
	}
	call := &warmCall{done: make(chan struct{})}
	r.inflight[key] = call
	r.mu.Unlock()
	// The lookup is shared by every waiter, so it must not die with the first caller's context.
	lookupCtx := context.WithoutCancel(ctx)
	call.res, call.err = r.lookup(lookupCtx, input, kind, canonical)
	r.mu.Lock()
	delete(r.inflight, key)
	if call.err == nil {
		r.storeLocked(owner, key, call.res)
	}
	r.mu.Unlock()
	close(call.done)
	if call.err != nil {
		return Resolved{}, call.err
	}
	music.MarkResolve(ctx, time.Since(started), true)
	return call.res, nil
}

// InvalidateCtx drops the cached lookup for input of the owner ctx carries,
// for example after its stream URL stopped working. An owner-less ctx is refused.
func (r *WarmResolver) InvalidateCtx(ctx context.Context, input string) {
	if owner, err := ownerFrom(ctx); err == nil {
		r.invalidate(owner, input)
	}
}

func (r *WarmResolver) invalidate(owner, input string) {
	kind, canonical, err := NormalizeInput(input)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := ownedKey{owner, cacheKey(kind, canonical)}
	if entry, ok := r.cache[key]; ok {
		delete(r.cache, ownedKey{owner, urlKey(entry.resolved.Track.CanonicalID)})
	}
	delete(r.cache, key)
}

// ownedKey partitions the cache: a community never reads another's entries.
// A struct key cannot collide the way a joined string could.
type ownedKey struct{ owner, key string }

func cacheKey(kind, canonical string) string {
	if kind == "search" {
		text := strings.TrimPrefix(canonical, "ytsearch5:")
		return "search:" + strings.Join(strings.Fields(strings.ToLower(text)), " ")
	}
	return canonical
}

func urlKey(id string) string { return videoURL(id) }

func (r *WarmResolver) storeLocked(owner string, key ownedKey, res Resolved) {
	urlK := ownedKey{owner, urlKey(res.Track.CanonicalID)}
	r.dropExpiredLocked()
	// Each owner has its own cap, so one community's searches cannot push another's entries out; the
	// global cap is the second bound. Both evict the least recently used entry.
	for r.countLocked(owner)+2 > maxOwnerCacheEntries {
		if !r.evictLRULocked(func(k ownedKey) bool { return k.owner == owner }) {
			break
		}
	}
	for len(r.cache)+2 > maxWarmCacheEntries {
		if !r.evictLRULocked(func(ownedKey) bool { return true }) {
			break
		}
	}
	r.tick++
	r.cache[key] = &cacheEntry{resolved: res, used: r.tick}
	r.cache[urlK] = &cacheEntry{resolved: res, used: r.tick}
}

func (r *WarmResolver) dropExpiredLocked() {
	now := r.deps.now()
	for k, e := range r.cache {
		if !now.Before(e.resolved.Expires) {
			delete(r.cache, k)
		}
	}
}

func (r *WarmResolver) countLocked(owner string) int {
	n := 0
	for k := range r.cache {
		if k.owner == owner {
			n++
		}
	}
	return n
}

// evictLRULocked removes the least recently used entry accepted by match.
func (r *WarmResolver) evictLRULocked(match func(ownedKey) bool) bool {
	var oldest ownedKey
	var oldestUsed uint64
	found := false
	for k, e := range r.cache {
		if match(k) && (!found || e.used < oldestUsed) {
			oldest, oldestUsed, found = k, e.used, true
		}
	}
	if found {
		delete(r.cache, oldest)
	}
	return found
}

// lookupArgs is yt-dlp's whole command line for one lookup: one extraction that
// prints the metadata and the chosen audio stream URL.
func (r *WarmResolver) lookupArgs(kind, canonical string) []string {
	args := []string{"--ignore-config", "--no-plugin-dirs", "--no-remote-components", "--no-cache-dir"}
	if r.cfg.Node != "" {
		args = append(args, "--js-runtimes", "node:"+r.cfg.Node)
	}
	args = append(args,
		"--no-warnings", "--no-progress", "--color", "never",
		"--socket-timeout", "10", "--extractor-retries", "1", "--no-playlist",
		// Progressive https only: a manifest (HLS, DASH) is never handed to ffmpeg.
		"--format", "bestaudio[protocol=https]",
		"--print", "%("+printFields+")j",
		"--")
	if kind == "search" {
		return append(args, "ytsearch1:"+strings.TrimPrefix(canonical, "ytsearch5:"))
	}
	return append(args, canonical)
}

// printFields is what a lookup asks yt-dlp to print; printRequired must all come back.
const printFields = ".{id,title,uploader,duration,availability,live_status,is_live,extractor_key,url,protocol,http_headers}"

var printRequired = []string{"id", "title", "duration", "extractor_key", "url", "protocol"}

type printed struct {
	metadata
	URL      string            `json:"url"`
	Protocol string            `json:"protocol"`
	Headers  map[string]string `json:"http_headers"`
}

// userAgentFrom returns the chosen format's User-Agent when it is plain printable text.
func userAgentFrom(headers map[string]string) string {
	for name, value := range headers {
		if !strings.EqualFold(name, "User-Agent") || value == "" || len(value) > 512 {
			continue
		}
		for _, c := range value {
			if c < 0x20 || c > 0x7e {
				return ""
			}
		}
		return value
	}
	return ""
}

func (r *WarmResolver) lookup(ctx context.Context, input, kind, canonical string) (Resolved, error) {
	ctx = context.WithValue(ctx, diagnosticInputKey{}, input)
	out, err := r.run(ctx, r.lookupArgs(kind, canonical), MaxMetadataBytes, input)
	if err != nil {
		return Resolved{}, err
	}
	line := out
	if i := bytes.IndexByte(bytes.TrimSpace(out), '\n'); i >= 0 {
		line = bytes.TrimSpace(out)[:i]
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(line), &keys); err != nil {
		return Resolved{}, invalidMetadata("json")
	}
	for _, key := range printRequired {
		if _, ok := keys[key]; !ok {
			// yt-dlp did not honour the print template (a version change), so no field can be trusted.
			return Resolved{}, invalidMetadata("print_keys")
		}
	}
	var data printed
	if err := json.Unmarshal(bytes.TrimSpace(line), &data); err != nil {
		return Resolved{}, invalidMetadata("json")
	}
	if data.Protocol != "https" {
		return Resolved{}, invalidMetadata("protocol")
	}
	if liveMetadata(data.LiveStatus, data.IsLive) {
		return Resolved{}, invalidMetadata("live")
	}
	track, err := makeTrack(data.ID, data.Title, data.Uploader, data.Duration, data.Availability, data.Extractor, false)
	if err != nil {
		return Resolved{}, err
	}
	streamURL, urlExpiry, err := checkStreamURL(data.URL)
	if err != nil {
		return Resolved{}, err
	}
	now := r.deps.now()
	expires := now.Add(r.ttl)
	if !urlExpiry.IsZero() && urlExpiry.Add(-streamURLSlack).Before(expires) {
		expires = urlExpiry.Add(-streamURLSlack)
	}
	return Resolved{Track: track, StreamURL: streamURL, Expires: expires, UserAgent: userAgentFrom(data.Headers)}, nil
}

// checkStreamURL accepts only an https URL on the hosts the egress broker
// allows; the same broker still decides every connection ffmpeg makes.
func checkStreamURL(raw string) (string, time.Time, error) {
	if raw == "" || len(raw) > 8192 {
		return "", time.Time{}, invalidMetadata("stream_url")
	}
	for _, c := range raw {
		if c < 0x21 || c == 0x7f {
			return "", time.Time{}, invalidMetadata("stream_url")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !allowedYouTubeHost(strings.ToLower(u.Hostname())) {
		return "", time.Time{}, invalidMetadata("stream_url")
	}
	var expiry time.Time
	if seconds, err := strconv.ParseInt(u.Query().Get("expire"), 10, 64); err == nil && seconds > 0 {
		expiry = time.Unix(seconds, 0)
	}
	return raw, expiry, nil
}

// run sends args to the worker, restarting it with backoff when it is gone. A
// lookup that times out is cancelled alone; the worker is killed only when it
// is wedged (it answered nothing, not even the cancel).
func (r *WarmResolver) run(ctx context.Context, args []string, limit int, input string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		w, err := r.acquire(ctx)
		if err != nil {
			return nil, err
		}
		callCtx, cancel := context.WithTimeout(ctx, r.timeout)
		out, err := w.Do(callCtx, args, limit)
		timedOut := callCtx.Err() != nil && ctx.Err() == nil
		cancel()
		var failure *workerFailure
		switch {
		case err == nil:
			r.finished(w, true)
			return out, nil
		case errors.Is(err, errWorkerWedged):
			r.discard(w, true)
			return nil, errors.Join(ErrExtractFailed, context.DeadlineExceeded)
		case timedOut:
			r.finished(w, false)
			return nil, errors.Join(ErrExtractFailed, context.DeadlineExceeded)
		case errors.Is(err, errWorkerGone) && ctx.Err() == nil:
			// The worker died under this request (not a yt-dlp failure). One retry on a fresh worker.
			r.discard(w, true)
			lastErr = err
			continue
		case errors.As(err, &failure):
			// Full redaction of the input (and its canonical, search and id forms), as for any subprocess.
			processErr := newProcessError(ErrExtractFailed, failure.Stderr, failure.Code, input)
			r.finished(w, processErr.PermanentPlaybackFailure())
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			return nil, processErr
		}
		if ctx.Err() != nil {
			r.finished(w, true) // the caller gave up; the worker is fine, and the lookup still counts
			return nil, context.Cause(ctx)
		}
		r.finished(w, false)
		return nil, err
	}
	return nil, errors.Join(ErrExtractFailed, lastErr)
}

// workerFailure is a yt-dlp run that exited non-zero, as the worker reported it.
type workerFailure struct {
	Code   int
	Stderr string
}

func (f *workerFailure) Error() string { return ErrExtractFailed.Error() }
func (f *workerFailure) Unwrap() error { return ErrExtractFailed }

var errWorkerWedged = errors.New("youtube: lookup worker is wedged")

var errWorkerGone = errors.New("youtube: lookup worker exited")

// acquire returns a live worker, starting one after the backoff when needed.
// Starts are serialised, so the warm-up and the first lookup share one worker.
func (r *WarmResolver) acquire(ctx context.Context) (lookupWorker, error) {
	if w := r.liveWorker(); w != nil {
		return w, nil
	}
	r.startMu.Lock()
	defer r.startMu.Unlock()
	var lastErr error
	for attempt := 0; attempt < maxWorkerStartTries; attempt++ {
		if w := r.liveWorker(); w != nil {
			return w, nil
		}
		r.mu.Lock()
		closed, wait := r.closed, r.nextStart.Sub(r.deps.now())
		r.mu.Unlock()
		if closed {
			return nil, errors.Join(ErrSandboxUnavailable, context.Canceled)
		}
		if wait > 0 {
			if err := r.deps.sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		w, err := r.deps.start(r.ctx)
		r.mu.Lock()
		if err != nil || w == nil {
			r.failures++
			r.nextStart = r.deps.now().Add(backoffFor(r.failures))
			r.mu.Unlock()
			if err == nil {
				err = ErrSandboxUnavailable
			}
			lastErr = err
			continue
		}
		if r.closed {
			r.mu.Unlock()
			_ = w.Close()
			return nil, errors.Join(ErrSandboxUnavailable, context.Canceled)
		}
		r.worker = w
		r.mu.Unlock()
		return w, nil
	}
	return nil, lastErr
}

// liveWorker returns the current worker, forgetting one that has exited.
func (r *WarmResolver) liveWorker() lookupWorker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.worker == nil {
		return nil
	}
	select {
	case <-r.worker.Done():
		r.worker = nil
		return nil
	default:
		return r.worker
	}
}

func backoffFor(failures int) time.Duration {
	d := workerBackoffInitial
	for i := 1; i < failures && d < workerBackoffMax; i++ {
		d *= 2
	}
	return min(d, workerBackoffMax)
}

// finished counts one completed lookup toward replacing the worker, success or
// not. healthy means the worker did its job (the video may still have been
// unavailable). A worker is replaced after recycleAfter lookups, after
// workerMaxFailures unhealthy ones in a row, or once its egress broker has
// spent half of its byte budget; the replacement starts before the old
// worker is closed.
func (r *WarmResolver) finished(w lookupWorker, healthy bool) {
	var spent int64
	if budget, ok := w.(interface{ egressUsed() int64 }); ok {
		spent = budget.egressUsed()
	}
	r.mu.Lock()
	retired, broken := false, false
	if r.worker == w {
		r.served++
		if healthy {
			r.failures, r.consecutive = 0, 0
			r.nextStart = time.Time{}
		} else {
			r.consecutive++
		}
		switch {
		case r.consecutive >= workerMaxFailures:
			// The worker is failing, not merely used up: the replacement waits out a backoff (an outage
			// must not start a sandbox every few lookups) and the old worker is closed at once.
			r.worker, r.served, r.consecutive, broken = nil, 0, 0, true
			r.failures++
			r.nextStart = r.deps.now().Add(backoffFor(r.failures))
		case r.served >= r.recycleAfter || spent > maxProcessBytes/2:
			r.worker, r.served, r.consecutive, retired = nil, 0, 0, true
		}
	}
	grace := r.retireGrace
	r.mu.Unlock()
	if broken {
		go func() { _ = w.Close() }()
	}
	if retired {
		// Requests already on w finish; new ones start a fresh worker with a fresh egress budget.
		time.AfterFunc(grace, func() { _ = w.Close() })
	}
}

// discard kills w and forgets it. failed makes the next start wait out a backoff.
func (r *WarmResolver) discard(w lookupWorker, failed bool) {
	_ = w.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.worker == w {
		r.worker = nil
		if failed {
			r.failures++
			r.nextStart = r.deps.now().Add(backoffFor(r.failures))
		}
	}
}

// MetadataResolver returns the queue/search resolver that runs its yt-dlp
// through the same warm worker (and so the same sandbox) as Resolve.
func (r *WarmResolver) MetadataResolver() MetadataResolver {
	return MetadataResolver{YTDLP: r.cfg.YTDLP, Node: r.cfg.Node, runner: warmRunner{r}}
}

// warmRunner adapts the warm worker to the package's sandboxedRunner contract.
type warmRunner struct{ r *WarmResolver }

func (w warmRunner) youtubeSandboxedRunner() {}

func (w warmRunner) Run(ctx context.Context, executable string, args []string, outputLimit int) ([]byte, error) {
	if ctx == nil || outputLimit < 1 || outputLimit > MaxMetadataBytes || len(args) > 128 || executable != w.r.cfg.YTDLP {
		return nil, ErrExtractFailed
	}
	for i, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return nil, ErrExtractFailed
		}
		if arg == "--js-runtimes" && (i+1 >= len(args) || args[i+1] != "node:"+w.r.cfg.Node || w.r.cfg.Node == "") {
			return nil, ErrExtractFailed
		}
	}
	input, _ := ctx.Value(diagnosticInputKey{}).(string)
	if !containsString(args, "--no-cache-dir") {
		args = append([]string{"--no-cache-dir"}, args...)
	}
	return w.r.run(ctx, args, outputLimit, input)
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

var _ sandboxedRunner = warmRunner{}

// ---- the sandboxed worker process -----------------------------------------

type workerRequest struct {
	ID     uint64   `json:"id,omitempty"`
	Args   []string `json:"args,omitempty"`
	Cancel uint64   `json:"cancel,omitempty"`
}

type workerReply struct {
	ID   uint64 `json:"id"`
	Code int    `json:"code"`
	Out  string `json:"out"`
	Err  string `json:"err"`
}

// serveWorkerArgs are the sandbox worker's own arguments for the long-lived mode.
func serveWorkerArgs(ytdlp string, limit int) []string {
	return []string{"serve", strconv.Itoa(limit), ytdlp}
}

type lookupProcess struct {
	process *isolatedPCM

	wmu     sync.Mutex // serialises request lines
	mu      sync.Mutex
	pending map[uint64]chan workerReply
	next    uint64
	dead    bool
	closeMu sync.Once
	done    chan struct{}
	// base anchors lastReply on the monotonic clock: it holds time.Since(base) of the latest line the worker
	// wrote, so a wall-clock step cannot change the wedge check.
	base      time.Time
	grace     time.Duration // how long a cancelled lookup waits for the worker to acknowledge
	lastReply atomic.Int64
}

// egressUsed is how many bytes this worker's broker has tunnelled.
func (w *lookupProcess) egressUsed() int64 {
	broker := w.process.broker
	if broker == nil {
		return 0
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return broker.used
}

// startSandboxWorker launches the long-lived yt-dlp worker in the same
// bubblewrap sandbox, with the same mounts and egress broker, as the
// per-request metadata worker it replaces.
func startSandboxWorker(ctx context.Context, cfg SandboxConfig) (lookupWorker, error) {
	tools := []string{cfg.YTDLP}
	if cfg.Node != "" {
		tools = append(tools, cfg.Node)
	}
	ctx = WithDiagnosticLogger(ctx, cfg.Logger)
	process, err := launchNetworkWorker(ctx, cfg.Bubblewrap, cfg.Python, tools, serveWorkerArgs(cfg.YTDLP, MaxMetadataBytes), defaultSandboxStartupTimeout, workerMaxLifetime)
	if err != nil {
		return nil, err
	}
	w := &lookupProcess{process: process, pending: map[uint64]chan workerReply{}, done: make(chan struct{})}
	w.base, w.grace = time.Now(), workerCancelGrace
	w.lastReply.Store(int64(time.Since(w.base)))
	go w.readReplies()
	return w, nil
}

func (w *lookupProcess) Done() <-chan struct{} { return w.done }

func (w *lookupProcess) Close() error {
	w.closeMu.Do(func() {
		if w.process.stdin != nil {
			_ = w.process.stdin.Close()
		}
		_ = w.process.Close()
	})
	return nil
}

func (w *lookupProcess) readReplies() {
	defer close(w.done)
	reader := bufio.NewReaderSize(w.process.stdout, 64<<10)
	for {
		line, err := readBoundedLine(reader, workerReplyLimit)
		if err != nil {
			break
		}
		var reply workerReply
		if json.Unmarshal(line, &reply) != nil {
			continue
		}
		w.lastReply.Store(int64(time.Since(w.base)))
		w.mu.Lock()
		ch := w.pending[reply.ID]
		delete(w.pending, reply.ID)
		w.mu.Unlock()
		if ch != nil {
			ch <- reply
		}
	}
	w.mu.Lock()
	w.dead = true
	for id, ch := range w.pending {
		close(ch)
		delete(w.pending, id)
	}
	w.mu.Unlock()
	_ = w.process.Close()
}

func readBoundedLine(r *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > limit {
			return nil, io.ErrShortBuffer
		}
		if !isPrefix {
			return line, nil
		}
	}
}

func (w *lookupProcess) Do(ctx context.Context, args []string, limit int) ([]byte, error) {
	w.mu.Lock()
	if w.dead {
		w.mu.Unlock()
		return nil, errWorkerGone
	}
	w.next++
	id := w.next
	ch := make(chan workerReply, 1)
	w.pending[id] = ch
	w.mu.Unlock()
	forget := func() {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
	}
	line, err := json.Marshal(workerRequest{ID: id, Args: args})
	if err != nil {
		forget()
		return nil, ErrExtractFailed
	}
	sent := int64(time.Since(w.base))
	if err := w.write(line); err != nil {
		forget()
		return nil, errWorkerGone
	}
	select {
	case reply, ok := <-ch:
		if !ok {
			return nil, errWorkerGone
		}
		return w.result(reply, limit)
	case <-ctx.Done():
		return nil, w.cancel(id, ch, sent, ctx.Err())
	}
}

func (w *lookupProcess) write(line []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	_, err := w.process.stdin.Write(append(line, '\n'))
	return err
}

// cancel stops one lookup. The worker kills that yt-dlp's process group and
// answers the request, which proves it is alive. Only when nothing at all came
// back from the worker since the request was sent (and the cancel went
// unanswered) is the worker wedged.
func (w *lookupProcess) cancel(id uint64, ch chan workerReply, sent int64, cause error) error {
	if line, err := json.Marshal(workerRequest{Cancel: id}); err == nil {
		_ = w.write(line)
	}
	timer := time.NewTimer(w.grace)
	defer timer.Stop()
	select {
	case _, ok := <-ch:
		if !ok {
			return errWorkerGone
		}
		return cause
	case <-timer.C:
	}
	w.mu.Lock()
	delete(w.pending, id)
	w.mu.Unlock()
	if w.lastReply.Load() < sent {
		return errors.Join(errWorkerWedged, cause)
	}
	return cause
}

func (w *lookupProcess) result(reply workerReply, limit int) ([]byte, error) {
	switch reply.Code {
	case 0:
		if len(reply.Out) > limit {
			return nil, ErrMetadataTooLarge
		}
		return []byte(reply.Out), nil
	case 75:
		return nil, ErrMetadataTooLarge
	}
	return nil, &workerFailure{Code: reply.Code, Stderr: reply.Err}
}

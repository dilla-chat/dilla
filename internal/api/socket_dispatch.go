package api

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// SocketDispatchOptions configures a SocketDispatcher. Zero values take the defaults.
type SocketDispatchOptions struct {
	// ReplayWindow is how long a mutation's recorded answer is returned to a retry with the same
	// idem from the same device: 5 minutes, the spec's dedupe window.
	ReplayWindow time.Duration
	// ReplayEntries bounds the replay record, least recently used first out: 4096 per instance.
	ReplayEntries int
	Clock         clock.Clock
	// Log receives a handler panic's stack (server.Recover) and every request the socket guard
	// refuses; nil is the process default.
	Log *slog.Logger
	// Deadline bounds one dispatch, a duplicate's wait on the running request included: 60 s.
	Deadline time.Duration
	// Observe, when set, is called with every dispatched request's method, route (the frame's, query included) and its
	// answer's status and body, after the answer is built, and whether the answer came from the replay record (a retry,
	// not a new attempt). It is the test host's view of traffic that used to cross its HTTP handler (dilladtest);
	// production sets none. It runs on the dispatching goroutine and must return at once: no store reads, no waiting.
	Observe func(method, route string, status int, body []byte, replayed bool)
}

const (
	defaultReplayWindow  = 5 * time.Minute
	defaultReplayEntries = 4096
	defaultDeadline      = 60 * time.Second
	// maxReplayBytes bounds the recorded bodies across the instance as ReplayEntries bounds their
	// count: a socket response may be up to 16 MiB, and 4096 of those is not memory to hold for
	// retries. Past it the least recently used records go first.
	maxReplayBytes = 64 << 20
)

// SocketDispatcher runs a gateway request frame against the socket routes of a server.Mux: it
// builds the *http.Request the route's REST form received (method, route, body, the connection's
// client address), marks it with the connection's session (auth.WithSocketCaller, which only this
// type sets), serves it into a bounded recorder and answers the recorded status and body.
//
// It installs the mux's socket guard (server.Mux.SetSocketGuard), which every socket route runs
// innermost, immediately before its handler. The guard fails closed: a request that reached it
// without the session middleware having passed every check is 401 and logged, so a route
// registered without the middleware is refused rather than served unauthenticated.
//
// A mutation that carries an idem is recorded under (device, idem) for ReplayWindow, and the record
// is consulted inside the guard, after the whole middleware chain (session, scope, factor, Bot
// allow-list and eligibility, wire, the route's rate meter) passed for THIS request: a retry is
// answered from the record instead of running again, a duplicate that arrives while the first is still
// running waits for its answer, and a session the middleware now refuses gets that refusal, never
// the record. The key includes the device, so one device's record is never another's answer.
//
// The record is a best effort, not exactly-once: an answer that is not recorded (a 5xx, or a
// response the connection's byte budget refused) lets a retry run the handler again, and so does
// an entry evicted from the bounded record, a record older than ReplayWindow and a restart of the
// instance. A route with its own id (request_id, ...) has that as the real dedupe; a route with none (the message
// upload) has only this record, so the client stops writing a mutation again once it may be gone (4 minutes after its
// first write, or on another instance incarnation) and treats the answer as lost (E_OUTCOME_UNKNOWN).
type SocketDispatcher struct {
	mux     *server.Mux
	handler http.Handler
	opts    SocketDispatchOptions

	mu      sync.Mutex
	running map[replayKey]*replayEntry
	done    map[replayKey]*list.Element // of *replayEntry, most recently used at the front
	lru     *list.List
	bytes   int // the recorded bodies' total, bounded by maxReplayBytes
	// waiters counts duplicates parked on a running entry (tests read it).
	waiters atomic.Int64
}

var _ gateway.RequestDispatcher = (*SocketDispatcher)(nil)

type replayKey struct {
	device id.ID
	idem   string
}

type replayEntry struct {
	key         replayKey
	fingerprint [32]byte
	at          time.Time
	resp        gateway.SocketResponse
	finished    chan struct{}
}

// replayHook is what Dispatch puts on a mutation's context for the guard: the record's key and the
// request's fingerprint, and, once the guard claimed a new running entry, that entry, which
// Dispatch settles with the response.
type replayHook struct {
	key         replayKey
	fingerprint [32]byte
	claimed     *replayEntry
	// replayed is set when the guard answered from the record instead of running the handler.
	replayed bool
}

type replayHookKey struct{}

// NewSocketDispatcher serves into mux.SocketMux() and installs the mux's socket guard. The mux may
// still be gaining routes: the dispatcher reads it per request.
func NewSocketDispatcher(mux *server.Mux, opts SocketDispatchOptions) *SocketDispatcher {
	if opts.ReplayWindow <= 0 {
		opts.ReplayWindow = defaultReplayWindow
	}
	if opts.ReplayEntries <= 0 {
		opts.ReplayEntries = defaultReplayEntries
	}
	if opts.Deadline <= 0 {
		opts.Deadline = defaultDeadline
	}
	if opts.Clock == nil {
		opts.Clock = clock.System()
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	d := &SocketDispatcher{
		mux:     mux,
		handler: server.Recover(opts.Log)(mux.SocketMux()),
		opts:    opts,
		running: map[replayKey]*replayEntry{},
		done:    map[replayKey]*list.Element{},
		lru:     list.New(),
	}
	mux.SetSocketGuard(d.guard)
	return d
}

// Dispatch implements gateway.RequestDispatcher.
func (d *SocketDispatcher) Dispatch(ctx context.Context, caller gateway.SocketCaller, r gateway.SocketRequest) gateway.SocketResponse {
	if caller.Wire != auth.WireVersion {
		return refusal(r.ID, server.Errorf(server.CodeUpgradeRequired, "socket requests need wire version %d", auth.WireVersion))
	}
	// The dispatch outlives a dropped socket (the gateway passes an uncancelled context) but never
	// runs, or waits, without bound.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.opts.Deadline)
	defer cancel()
	var hook *replayHook
	if r.Idem != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		hook = &replayHook{key: replayKey{device: caller.DeviceID, idem: string(r.Idem)}, fingerprint: fingerprint(r)}
		ctx = context.WithValue(ctx, replayHookKey{}, hook)
	}
	req, err := buildSocketRequest(ctx, caller, r)
	if err != nil {
		return refusal(r.ID, err)
	}
	resp := gateway.SocketResponse{ID: r.ID, Status: http.StatusInternalServerError}
	defer func() {
		if hook != nil && hook.claimed != nil {
			d.settle(hook.claimed, resp)
		}
	}()
	resp = d.serve(req, r.ID)
	if d.opts.Observe != nil {
		d.opts.Observe(r.Method, r.Route, resp.Status, resp.Body, hook != nil && hook.replayed)
	}
	return resp
}

// guard is the socket routes' innermost leaf (server.SocketGuard). It refuses, with 401 and a log
// line, any request whose session middleware did not pass every check, then answers a mutation
// with an idem from the replay record, or runs the handler and lets Dispatch record the result.
func (d *SocketDispatcher) guard(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if socket, passed := auth.SocketCallPassed(r.Context()); !socket || !passed {
		d.opts.Log.Warn("socket route reached without the session middleware; refused",
			"route", r.Pattern, "socket_call", socket)
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	hook, _ := r.Context().Value(replayHookKey{}).(*replayHook)
	if hook == nil || hook.claimed != nil {
		next.ServeHTTP(w, r)
		return
	}
	// The record is keyed by the connection's device; it is consulted only for a session the
	// middleware resolved to that same device.
	if sess, ok := auth.FromContext(r.Context()); !ok || sess.DeviceID != hook.key.device {
		d.opts.Log.Warn("socket replay key names another device than the session; refused", "route", r.Pattern)
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	now := d.opts.Clock.Now()
	d.mu.Lock()
	entry, running := d.running[hook.key]
	if !running {
		if el, ok := d.done[hook.key]; ok {
			entry, _ = el.Value.(*replayEntry) // the list holds only *replayEntry
			if entry == nil || now.Sub(entry.at) > d.opts.ReplayWindow {
				d.dropLocked(el)
				entry = nil
			} else {
				d.lru.MoveToFront(el)
			}
		}
	}
	if entry == nil {
		entry = &replayEntry{key: hook.key, fingerprint: hook.fingerprint, finished: make(chan struct{})}
		d.running[hook.key] = entry
		hook.claimed = entry
		d.mu.Unlock()
		next.ServeHTTP(w, r)
		return
	}
	d.mu.Unlock()
	hook.replayed = true
	if entry.fingerprint != hook.fingerprint {
		server.WriteError(w, server.WithStatus(http.StatusConflict,
			server.Errorf(server.CodeInvalidRequest, "idem was already used for a different request")))
		return
	}
	if running {
		d.waiters.Add(1)
		select {
		case <-entry.finished:
			d.waiters.Add(-1)
		case <-r.Context().Done():
			d.waiters.Add(-1)
			server.WriteError(w, server.Unavailable(1000, "the request with this idem is still running"))
			return
		}
	}
	if entry.resp.Status != 0 {
		w.WriteHeader(entry.resp.Status)
	}
	_, _ = w.Write(entry.resp.Body)
}

// settle ends a running entry with its response: recorded when it is the request's outcome, and
// handed to the duplicates waiting on it either way.
func (d *SocketDispatcher) settle(entry *replayEntry, resp gateway.SocketResponse) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, entry.key)
	entry.resp = gateway.SocketResponse{Status: resp.Status, Body: resp.Body}
	entry.at = d.opts.Clock.Now()
	if recordable(resp.Status) && len(resp.Body) <= maxReplayBytes {
		d.done[entry.key] = d.lru.PushFront(entry)
		d.bytes += len(resp.Body)
		for d.lru.Len() > d.opts.ReplayEntries || d.bytes > maxReplayBytes {
			d.dropLocked(d.lru.Back())
		}
	}
	close(entry.finished)
}

// buildSocketRequest is the *http.Request a REST client would have sent, minus every header but
// Content-Type: the credential is the connection's own session, carried in the context.
func buildSocketRequest(ctx context.Context, caller gateway.SocketCaller, r gateway.SocketRequest) (*http.Request, error) {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil, server.Errorf(server.CodeInvalidRequest, "method %q is not a socket method", r.Method)
	}
	// ParseRequestURI is what net/http's server parses a request line's target with; a route that
	// names a scheme, a host, user info or a fragment is not a path on this instance.
	u, err := url.ParseRequestURI(r.Route)
	if err != nil || !strings.HasPrefix(r.Route, "/") || strings.HasPrefix(r.Route, "//") ||
		u.Scheme != "" || u.Host != "" || u.User != nil || u.Fragment != "" {
		return nil, server.Errorf(server.CodeInvalidRequest, "route must be an absolute path")
	}
	req, err := http.NewRequestWithContext(auth.WithSocketCaller(ctx, caller.TokenHash), r.Method, "/", bytes.NewReader(r.Body))
	if err != nil {
		return nil, server.Errorf(server.CodeInvalidRequest, "route: %v", err)
	}
	req.URL = u
	req.RequestURI = r.Route
	req.RemoteAddr = caller.RemoteAddr
	req.Header.Set("Content-Type", "application/cbor")
	return req, nil
}

// serve runs one request into a recorder bounded by the matched route's cap.
func (d *SocketDispatcher) serve(req *http.Request, reqID uint64) gateway.SocketResponse {
	// The matched route sets its own cap on the recorder (server.SocketCapSetter) before anything
	// writes; an unmatched route's 404 is bounded by the default.
	rec := &boundedRecorder{ctx: req.Context(), header: http.Header{}, limit: server.DefaultSocketBodyCap}
	d.handler.ServeHTTP(rec, req)
	switch {
	case rec.busy:
		return refusal(reqID, server.Errorf(server.CodeBusy, "the connection's in-flight bytes are spent"))
	case rec.over:
		return refusal(reqID, server.WithStatus(http.StatusRequestEntityTooLarge,
			server.Errorf("E_FRAME_LIMIT", "response body over %d bytes", rec.limit)))
	}
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	return gateway.SocketResponse{ID: reqID, Status: status, Body: rec.body.Bytes()}
}

// recordable reports whether an answer is the request's outcome. A refusal that ran nothing and
// asks the client to come back (not authenticated, update first, rate limited or busy) and every
// 5xx (a fault or a brief unavailability) are not recorded, so the retry runs and a transient
// error never sticks to an idem.
func recordable(status int) bool {
	switch {
	case status >= 500:
		return false
	case status == http.StatusUnauthorized, status == http.StatusUpgradeRequired, status == http.StatusTooManyRequests:
		return false
	default:
		return true
	}
}

// dropLocked removes one record. A record past the window is dropped when its key is next looked
// up; until then it only takes a place in the bounded list.
func (d *SocketDispatcher) dropLocked(el *list.Element) {
	e, _ := el.Value.(*replayEntry) // the list holds only *replayEntry
	d.lru.Remove(el)
	if e == nil {
		return
	}
	delete(d.done, e.key)
	d.bytes -= len(e.resp.Body)
}

// fingerprint binds a record to the request it answered, so an idem reused for another request is
// refused rather than answered with the wrong record.
func fingerprint(r gateway.SocketRequest) [32]byte {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00", r.Method, r.Route)
	_, _ = h.Write(r.Body)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// refusal is an error body in the HTTP form, as server.WriteError writes it.
func refusal(reqID uint64, err error) gateway.SocketResponse {
	rec := &boundedRecorder{header: http.Header{}, limit: server.DefaultSocketBodyCap}
	server.WriteError(rec, err)
	return gateway.SocketResponse{ID: reqID, Status: rec.status, Body: rec.body.Bytes()}
}

// errResponseOver is what a write past the recorder's bound returns to the handler.
var errResponseOver = errors.New("api: socket response over its bound")

// boundedRecorder is the socket request's http.ResponseWriter: it keeps the status and at most
// limit body bytes, charging each write to the connection's in-flight budget. A write past the
// limit marks the response over (E_FRAME_LIMIT); a write the budget refuses marks it busy (E_BUSY).
// Either way nothing more is kept.
type boundedRecorder struct {
	ctx    context.Context
	header http.Header
	status int
	body   bytes.Buffer
	limit  int64
	over   bool
	busy   bool
}

func (w *boundedRecorder) Header() http.Header { return w.header }

// SetSocketCap implements server.SocketCapSetter.
func (w *boundedRecorder) SetSocketCap(maxBody int64) {
	if maxBody > 0 && w.body.Len() == 0 {
		w.limit = maxBody
	}
}

func (w *boundedRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *boundedRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.over || w.busy {
		return 0, errResponseOver
	}
	if int64(w.body.Len())+int64(len(p)) > w.limit {
		w.over = true
		w.body.Reset()
		return 0, errResponseOver
	}
	if w.ctx != nil && !gateway.ChargeResponse(w.ctx, len(p)) {
		w.busy = true
		w.body.Reset()
		return 0, errResponseOver
	}
	return w.body.Write(p)
}

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
	// Log receives a handler panic's stack (server.Recover); nil is the process default.
	Log *slog.Logger
	// Sessions re-resolves the connection's session before a recorded answer is returned: a
	// replay never runs the route, so without it a revoked session's retry would still be
	// answered. Nil turns the replay record off (every request runs); the composition root
	// always passes *auth.Sessions.
	Sessions SessionResolver
}

// SessionResolver is the one *auth.Sessions method the replay record needs.
type SessionResolver interface {
	ResolveHash(ctx context.Context, tokenHash []byte) (auth.Session, error)
}

const (
	defaultReplayWindow  = 5 * time.Minute
	defaultReplayEntries = 4096
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
// A mutation that carries an idem is recorded under (device, idem) for ReplayWindow: a retry —
// typically the same request re-sent after a reconnect — is answered from the record and never
// runs twice, and a duplicate that arrives while the first is still running waits for its answer.
// The key includes the device, so one device's record is never another's answer.
type SocketDispatcher struct {
	mux     *server.Mux
	handler http.Handler
	opts    SocketDispatchOptions

	mu      sync.Mutex
	running map[replayKey]*replayEntry
	done    map[replayKey]*list.Element // of *replayEntry, most recently used at the front
	lru     *list.List
	bytes   int // the recorded bodies' total, bounded by maxReplayBytes
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

// NewSocketDispatcher serves into mux.SocketMux(). The mux may still be gaining routes: the
// dispatcher reads it per request.
func NewSocketDispatcher(mux *server.Mux, opts SocketDispatchOptions) *SocketDispatcher {
	if opts.ReplayWindow <= 0 {
		opts.ReplayWindow = defaultReplayWindow
	}
	if opts.ReplayEntries <= 0 {
		opts.ReplayEntries = defaultReplayEntries
	}
	if opts.Clock == nil {
		opts.Clock = clock.System()
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &SocketDispatcher{
		mux:     mux,
		handler: server.Recover(opts.Log)(mux.SocketMux()),
		opts:    opts,
		running: map[replayKey]*replayEntry{},
		done:    map[replayKey]*list.Element{},
		lru:     list.New(),
	}
}

// Dispatch implements gateway.RequestDispatcher.
func (d *SocketDispatcher) Dispatch(ctx context.Context, caller gateway.SocketCaller, r gateway.SocketRequest) gateway.SocketResponse {
	if caller.Wire != auth.WireVersion {
		return refusal(r.ID, server.Errorf(server.CodeUpgradeRequired, "socket requests need wire version %d", auth.WireVersion))
	}
	req, err := buildSocketRequest(ctx, caller, r)
	if err != nil {
		return refusal(r.ID, err)
	}
	if r.Idem == nil || r.Method == http.MethodGet || r.Method == http.MethodHead || d.opts.Sessions == nil {
		return d.serve(req, r.ID)
	}
	return d.serveOnce(req, caller, r)
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

// serveOnce is serve under the replay record of (device, idem).
func (d *SocketDispatcher) serveOnce(req *http.Request, caller gateway.SocketCaller, r gateway.SocketRequest) gateway.SocketResponse {
	key := replayKey{device: caller.DeviceID, idem: string(r.Idem)}
	fp := fingerprint(r)
	now := d.opts.Clock.Now()

	d.mu.Lock()
	entry, waiting := d.running[key]
	if !waiting {
		if el, ok := d.done[key]; ok {
			entry = el.Value.(*replayEntry) //nolint:forcetypeassert // the list holds only *replayEntry
			if now.Sub(entry.at) > d.opts.ReplayWindow {
				d.dropLocked(el)
				entry = nil
			} else {
				d.lru.MoveToFront(el)
			}
		}
	}
	if entry != nil {
		d.mu.Unlock()
		if entry.fingerprint != fp {
			return refusal(r.ID, server.WithStatus(http.StatusConflict,
				server.Errorf(server.CodeInvalidRequest, "idem was already used for a different request")))
		}
		<-entry.finished
		// The recorded answer goes only to a session that is still current and still the record's
		// device: the same revocation, expiry and device checks the route's middleware applies.
		sess, err := d.opts.Sessions.ResolveHash(req.Context(), caller.TokenHash)
		if err != nil {
			return refusal(r.ID, err)
		}
		if sess.DeviceID != key.device {
			return refusal(r.ID, server.Errorf(server.CodeUnauthenticated, ""))
		}
		resp := entry.resp
		resp.ID = r.ID
		return resp
	}
	entry = &replayEntry{key: key, fingerprint: fp, finished: make(chan struct{})}
	d.running[key] = entry
	d.mu.Unlock()

	resp := d.serve(req, r.ID)

	d.mu.Lock()
	delete(d.running, key)
	entry.resp = gateway.SocketResponse{Status: resp.Status, Body: resp.Body}
	entry.at = d.opts.Clock.Now()
	if recordable(resp.Status) && len(resp.Body) <= maxReplayBytes {
		d.done[key] = d.lru.PushFront(entry)
		d.bytes += len(resp.Body)
		for d.lru.Len() > d.opts.ReplayEntries || d.bytes > maxReplayBytes {
			d.dropLocked(d.lru.Back())
		}
	}
	close(entry.finished)
	d.mu.Unlock()
	return resp
}

// recordable reports whether an answer is the request's outcome. A refusal that ran nothing and
// asks the client to come back — not authenticated, update first, rate limited or busy, briefly
// unavailable — is not recorded, so the retry runs.
func recordable(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusUpgradeRequired, http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return false
	default:
		return true
	}
}

// dropLocked removes one record. A record past the window is dropped when its key is next looked
// up; until then it only takes a place in the bounded list.
func (d *SocketDispatcher) dropLocked(el *list.Element) {
	e := el.Value.(*replayEntry) //nolint:forcetypeassert // the list holds only *replayEntry
	d.lru.Remove(el)
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

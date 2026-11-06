package gateway

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// The socket request shim (Bot v2 task 2). A client sends op 59 `request`
// [id, method, route, body, idem|null]; the instance dispatches it to the same HTTP handler the
// route's REST form had and answers op 60 `response` [id, status, body]. Responses may arrive in
// any order. Neither frame is replayable: a request lost in a disconnect is re-sent by the client
// with the same idem, and the dispatcher's replay record answers it.
const (
	// MaxInFlightRequests is the per-connection cap on requests dispatched and not yet answered.
	// The 65th is answered E_BUSY at once.
	MaxInFlightRequests = 64
	// MaxRequestBodyBytes is the largest body a request frame may carry: the largest HTTP body cap
	// of any authenticated route (the commit's 16 MiB). Each route's handler still applies its own
	// cap, so a body over it gets the same refusal it would over HTTP.
	MaxRequestBodyBytes = server.DefaultSocketBodyCap
	// MaxRequestBytesInFlight bounds one connection's request plus response body bytes in flight.
	// A request that would take it over is answered E_BUSY, as the 65th request is. The bound can be
	// overshot by at most one in-flight frame read (a request frame is read whole, up to 16 MiB of
	// body, before it is admitted or refused) and one replayed body (an answer from the replay
	// record was not charged while it was recorded, so it is charged when it is sent).
	MaxRequestBytesInFlight int64 = 32 << 20

	maxIdemBytes  = 64
	maxRouteBytes = 8192
	// maxRequestFrameBytes is the frame reader's bound for a request frame of an identified
	// connection: the body plus room for the id, method, route and idem. Every other frame, and
	// every frame before identify, keeps the configured read limit.
	maxRequestFrameBytes = int(MaxRequestBodyBytes) + 16<<10
)

// SocketRequest is one decoded request frame.
type SocketRequest struct {
	ID            uint64
	Method, Route string
	Body, Idem    []byte
}

// SocketResponse is one response frame.
type SocketResponse struct {
	ID     uint64
	Status int
	Body   []byte
}

// SocketCaller is the identified connection a request arrived on. It comes from the connection's
// own state, never from the frame: TokenHash is the hash of the session the connection identified
// (or resumed) with, RemoteAddr the client address of the upgrade request as the HTTP layer
// resolves it behind trusted proxies.
type SocketCaller struct {
	TokenHash  []byte
	DeviceID   id.ID
	RemoteAddr string
	Wire       uint64
}

// RequestDispatcher runs one request against the socket routes. internal/api's SocketDispatcher
// is the implementation; the gateway declares the interface so it never imports internal/api.
type RequestDispatcher interface {
	Dispatch(ctx context.Context, caller SocketCaller, r SocketRequest) SocketResponse
}

// RequestPayload encodes op 59's payload.
func RequestPayload(r SocketRequest) (cbor.RawMessage, error) {
	var idem any
	if r.Idem != nil {
		idem = r.Idem
	}
	body := r.Body
	if body == nil {
		body = []byte{}
	}
	return payload(r.ID, r.Method, r.Route, body, idem)
}

// EncodeRequestFrame is a whole op 59 frame, n = 0. Clients and tests use it.
func EncodeRequestFrame(r SocketRequest) ([]byte, error) {
	return appendRequestFrame(make([]byte, 0, len(r.Body)+len(r.Route)+len(r.Idem)+32), r), nil
}

// ResponsePayload encodes op 60's payload.
func ResponsePayload(r SocketResponse) (cbor.RawMessage, error) {
	body := r.Body
	if body == nil {
		body = []byte{}
	}
	if r.Status < 100 || r.Status > 599 {
		return nil, fmt.Errorf("gateway: response status %d", r.Status)
	}
	return payload(r.ID, uint64(r.Status), body) //nolint:gosec // G115: checked 100..599 above
}

// DecodeResponseFrame reads a whole op 60 frame. Clients and tests use it.
func DecodeResponseFrame(b []byte) (SocketResponse, error) { return decodeResponseFrame(b) }

// decodeRequest reads op 59's payload. Decode calls it, so a request frame of the wrong shape is
// E_FRAME_SHAPE before it reaches handle; the method and route are only bounded here, the
// dispatcher judges them.
func decodeRequest(in Inbound) (SocketRequest, error) {
	shape := func(what string, err error) (SocketRequest, error) {
		detail := "request " + what
		if err != nil {
			detail += ": " + err.Error()
		}
		return SocketRequest{}, frameErr("E_FRAME_SHAPE", detail, CloseDecode)
	}
	if len(in.Payload) != 5 {
		return shape("payload", nil)
	}
	reqID, err := rawUint(in.Payload[0])
	if err != nil {
		return shape("id", err)
	}
	method, err := rawText(in.Payload[1])
	if err != nil || method == "" || len(method) > 16 {
		return shape("method", err)
	}
	route, err := rawText(in.Payload[2])
	if err != nil || route == "" || len(route) > maxRouteBytes {
		return shape("route", err)
	}
	body, err := rawBytes(in.Payload[3])
	if err != nil {
		return shape("body", err)
	}
	if int64(len(body)) > MaxRequestBodyBytes {
		return SocketRequest{}, frameErr("E_FRAME_LIMIT",
			fmt.Sprintf("request body is %d bytes, limit %d", len(body), MaxRequestBodyBytes), CloseRateLimited)
	}
	var idem []byte
	if !isNull(in.Payload[4]) {
		idem, err = rawBytes(in.Payload[4])
		if err != nil || len(idem) == 0 || len(idem) > maxIdemBytes {
			return shape("idem", err)
		}
	}
	return SocketRequest{ID: reqID, Method: method, Route: route, Body: body, Idem: idem}, nil
}

// isRequestFrame reads only the head of a frame: a four-element array whose op is 59, in the
// deterministic encoding (0x84, then 59 as the one-byte-argument uint 0x18 0x3b). The frame reader
// uses it to decide, before the rest of a long message is read, whether the long-frame bound
// applies.
func isRequestFrame(b []byte) bool {
	return len(b) >= 3 && b[0] == 0x84 && b[1] == 0x18 && b[2] == byte(OpRequest)
}

// frameReadLimit is the size bound Decode applies to an identified connection's frame.
func frameReadLimit(b []byte, readLimit int) int {
	if isRequestFrame(b) && maxRequestFrameBytes > readLimit {
		return maxRequestFrameBytes
	}
	return readLimit
}

// requestBudget is one connection's in-flight request accounting: at most MaxInFlightRequests
// requests and MaxRequestBytesInFlight request plus response body bytes. A connection gets a new
// budget on identify and on every resume, so a request still running for the old socket settles
// its own budget and never the new one's.
type requestBudget struct {
	mu       sync.Mutex
	inflight int
	bytes    int64
}

func newRequestBudget() *requestBudget { return &requestBudget{} }

// admit reserves one request slot and n bytes, or reports that the connection is busy.
func (b *requestBudget) admit(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inflight >= MaxInFlightRequests || b.bytes+n > MaxRequestBytesInFlight {
		return false
	}
	b.inflight++
	b.bytes += n
	return true
}

// charge reserves n more bytes for a response being recorded, or reports the budget spent.
func (b *requestBudget) charge(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bytes+n > MaxRequestBytesInFlight {
		return false
	}
	b.bytes += n
	return true
}

// force reserves n bytes whatever the budget holds: a response the dispatcher returned from its
// replay record was not charged while it was recorded.
func (b *requestBudget) force(n int64) {
	b.mu.Lock()
	b.bytes += n
	b.mu.Unlock()
}

// finish releases one request slot and its n bytes.
func (b *requestBudget) finish(n int64) {
	b.mu.Lock()
	b.inflight--
	b.bytes -= n
	b.mu.Unlock()
}

// responseCharge is what ChargeResponse reads off a dispatch context.
type responseCharge struct {
	budget  *requestBudget
	mu      sync.Mutex
	charged int64
}

type chargeKey struct{}

// ChargeResponse reserves n bytes of the dispatching connection's in-flight budget for the
// response being recorded. It reports false when the budget is spent; the recorder then answers
// E_BUSY instead of holding the body. A context that did not come from a gateway dispatch has no
// budget and is always charged.
func ChargeResponse(ctx context.Context, n int) bool {
	rc, ok := ctx.Value(chargeKey{}).(*responseCharge)
	if !ok || n <= 0 {
		return true
	}
	if !rc.budget.charge(int64(n)) {
		return false
	}
	rc.mu.Lock()
	rc.charged += int64(n)
	rc.mu.Unlock()
	return true
}

func (rc *responseCharge) total() int64 {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.charged
}

// errorResponse is a refusal the gateway answers itself, in the HTTP error body's shape.
func errorResponse(reqID uint64, e *server.Error) SocketResponse {
	body, err := cborx.Marshal([]any{string(e.Code), e.Detail, nil})
	if err != nil {
		body = []byte{}
	}
	return SocketResponse{ID: reqID, Status: e.Status(), Body: body}
}

// handleRequest admits one request frame and dispatches it on its own goroutine, so a slow
// request never holds up the read loop or a faster request behind it.
//
// It reports whether the request was admitted to a worker. A refused request (E_BUSY, no
// dispatcher, the pool shutting down) is answered here, and the read loop charges it to the
// inbound frame bucket (controller ruling B), so a flood of refused requests closes the
// connection as any other frame flood does.
func (g *Gateway) handleRequest(ctx context.Context, c *conn, in Inbound) bool {
	var req SocketRequest
	if in.request != nil {
		req = *in.request
	} else {
		var err error
		if req, err = decodeRequest(in); err != nil {
			return false // Decode already refused a malformed request frame; this is unreachable.
		}
	}
	c.mu.Lock()
	budget, w := c.requests, c.writer
	caller := SocketCaller{
		TokenHash: append([]byte(nil), c.tokenHash...), DeviceID: c.deviceID,
		RemoteAddr: c.remoteAddr, Wire: c.wire,
	}
	c.mu.Unlock()
	if g.opts.Requests == nil {
		g.sendResponse(c, w, errorResponse(req.ID, server.Errorf(server.CodeNotFound, "this instance serves no socket routes")), nil)
		return false
	}
	size := int64(len(req.Body))
	if budget == nil || !budget.admit(size) {
		g.sendResponse(c, w, errorResponse(req.ID, server.Errorf(server.CodeBusy,
			"at most %d requests and %d bytes in flight on one connection", MaxInFlightRequests, MaxRequestBytesInFlight)), nil)
		return false
	}
	// The dispatch outlives a dropped socket: a mutation runs to its end and lands in the replay
	// record, so the client's re-send with the same idem is answered with its real outcome rather
	// than with a cancellation.
	dctx := context.WithoutCancel(ctx)
	rc := &responseCharge{budget: budget}
	// release frees the request's slot and bytes exactly once, whichever path gets there first: the
	// writer once the response is written, or the recover below.
	var once sync.Once
	var held atomic.Int64
	release := func() { once.Do(func() { budget.finish(size + max(held.Load(), rc.total())) }) }
	admitted := g.workers.run(func() {
		answered := false
		// A fault in the dispatcher is answered 500 and still frees the request's slot and bytes:
		// without this, a panicking dispatch would hold one of the connection's 64 slots forever. A
		// fault after the answer was handed over (sendResponse) still frees them.
		defer func() {
			if v := recover(); v != nil {
				g.opts.Log.Error("gateway: socket request dispatch panicked", "panic", v, "stack", string(debug.Stack()))
				if !answered {
					g.sendResponse(c, w, errorResponse(req.ID, server.Errorf(server.CodeInternal, "")), release)
					return
				}
				release()
			}
		}()
		resp := g.opts.Requests.Dispatch(context.WithValue(dctx, chargeKey{}, rc), caller, req)
		resp.ID = req.ID
		answered = true
		h := rc.total()
		if extra := int64(len(resp.Body)) - h; extra > 0 {
			budget.force(extra)
			h += extra
		}
		held.Store(h)
		g.sendResponse(c, w, resp, release)
	})
	if !admitted {
		// The gateway is shutting down: nothing new is dispatched.
		budget.finish(size)
		g.sendResponse(c, w, errorResponse(req.ID, server.Unavailable(1000, "the instance is shutting down")), nil)
	}
	return admitted
}

// workerPool runs socket requests on goroutines that outlive one request. A request's handler
// stack (session resolve, SQLite, CBOR) grows a fresh goroutine's stack every time; an HTTP
// connection's goroutine keeps its grown stack across keep-alive requests, so a goroutine per
// request made the socket form measurably slower than the HTTP one (ruling 2). An idle worker
// takes the next job; with none idle a new one starts, so concurrency is never bounded here (the
// per-connection budget bounds it). A worker idle for workerIdle exits.
type workerPool struct {
	jobs chan func()

	// mu guards closing, active and drained. Once closing is set (Shutdown's wait began) run
	// refuses new work; drained is closed when the last active job finishes.
	mu      sync.Mutex
	closing bool
	active  int
	drained chan struct{}
}

const workerIdle = 30 * time.Second

func newWorkerPool() *workerPool {
	return &workerPool{jobs: make(chan func()), drained: make(chan struct{})}
}

// run hands job to a worker, or reports false once the pool is closing.
func (p *workerPool) run(job func()) bool {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return false
	}
	p.active++
	p.mu.Unlock()
	tracked := func() {
		defer p.finished()
		job()
	}
	select {
	case p.jobs <- tracked:
	default:
		go p.work(tracked)
	}
	return true
}

func (p *workerPool) finished() {
	p.mu.Lock()
	p.active--
	if p.active == 0 && p.closing {
		select {
		case <-p.drained:
		default:
			close(p.drained)
		}
	}
	p.mu.Unlock()
}

// wait closes the pool to new work and returns once every job handed out has finished, or when
// ctx ends. It starts no goroutine, so an expired ctx leaves nothing behind.
func (p *workerPool) wait(ctx context.Context) {
	p.mu.Lock()
	p.closing = true
	if p.active == 0 {
		select {
		case <-p.drained:
		default:
			close(p.drained)
		}
	}
	p.mu.Unlock()
	select {
	case <-p.drained:
	case <-ctx.Done():
	}
}

func (p *workerPool) closingForTest() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closing
}

func (p *workerPool) work(job func()) {
	job()
	idle := time.NewTimer(workerIdle)
	defer idle.Stop()
	for {
		select {
		case job := <-p.jobs:
			job()
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(workerIdle)
		case <-idle.C:
			return
		}
	}
}

// sendResponse queues a response on the writer the request arrived on. A connection that was
// suspended and resumed since then has a new writer, and the response is dropped: the client
// re-sends a request it did not see answered.
func (g *Gateway) sendResponse(c *conn, w *writer, resp SocketResponse, done func()) {
	finish := func() {
		if done != nil {
			done()
		}
	}
	b, err := responseFrame(resp)
	if err != nil {
		b, _ = responseFrame(errorResponse(resp.ID, server.Errorf(server.CodeInternal, "")))
	}
	c.mu.Lock()
	current := c.writer
	c.mu.Unlock()
	if w == nil || current != w {
		finish()
		return
	}
	if !w.writeResponse(b, finish) {
		w.enqueueResponse(b, finish)
	}
	if g.opts.Metrics != nil {
		g.opts.Metrics.GatewayFrames.WithLabelValues(opLabel(OpResponse), "out").Inc()
	}
}

// remoteAddrKey carries the upgrade request's client address from Handler to the connection.
type remoteAddrKey struct{}

func withRemoteAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, remoteAddrKey{}, addr)
}

func remoteAddrFrom(ctx context.Context) string {
	addr, _ := ctx.Value(remoteAddrKey{}).(string)
	return addr
}

// clientAddr is the address a socket request is attributed to: Options.ClientAddr's resolution of
// the upgrade request (the composition root passes the HTTP layer's trusted-proxy rule), or the
// peer address.
func (g *Gateway) clientAddr(r *http.Request) string {
	if g.opts.ClientAddr != nil {
		return g.opts.ClientAddr(r)
	}
	return r.RemoteAddr
}

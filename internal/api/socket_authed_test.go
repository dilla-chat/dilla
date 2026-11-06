package api_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/server"
)

// countingMeter stands in for a route's inline rate meter (dsMeter): it counts every request that
// reaches it and refuses with 429 once refuseAfter requests have passed (0 never refuses).
type countingMeter struct {
	seen        atomic.Int64
	refuseAfter atomic.Int64
}

func (m *countingMeter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := m.seen.Add(1)
		if limit := m.refuseAfter.Load(); limit > 0 && n > limit {
			server.WriteError(w, server.RateLimited(1000))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// A route whose middleware is built inline (the enrolled/scoped/dsMeter chains of api.go,
// groups.go and messages.go) moves with SocketAuthed: the guard sits between that chain and the
// bare handler, the meter is spent once per request (a replay hit included), and the replay record
// is consulted only after the chain passed.
func TestSocketAuthedRouteRunsItsOwnMiddlewareBeforeTheGuard(t *testing.T) {
	e := newSocketEnv(t, nil)
	meter := &countingMeter{}
	var runs atomic.Int64
	e.mux.SocketAuthed("POST /v1/test/inline", func(next http.Handler) http.Handler {
		return e.deps.Sessions.Middleware(meter.wrap(next), auth.ScopeEnrolled)
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.FromContext(r.Context()); !ok {
			t.Error("the handler ran without a session")
		}
		_ = server.EncodeBody(w, http.StatusCreated, runs.Add(1))
	}))
	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	idem := []byte("inline-idem")
	first := sockDo(t, c, http.MethodPost, "/v1/test/inline", nil, idem)
	if first.Status != http.StatusCreated || runs.Load() != 1 || meter.seen.Load() != 1 {
		t.Fatalf("first = %d, runs %d, meter %d", first.Status, runs.Load(), meter.seen.Load())
	}
	replay := sockDo(t, c, http.MethodPost, "/v1/test/inline", nil, idem)
	if replay.Status != http.StatusCreated || string(replay.Body) != string(first.Body) || runs.Load() != 1 || meter.seen.Load() != 2 {
		t.Fatalf("replay = %d %x, runs %d, meter %d; want the record, one run, the meter spent again", replay.Status, replay.Body, runs.Load(), meter.seen.Load())
	}
	// The meter now refuses: a retry with the same idem gets the meter's 429, not the record.
	meter.refuseAfter.Store(2)
	refused := sockDo(t, c, http.MethodPost, "/v1/test/inline", nil, idem)
	if code, _ := sockErrorCode(t, refused.Body); refused.Status != http.StatusTooManyRequests || code != string(server.CodeRateLimited) {
		t.Fatalf("retry past the meter = %d %s, want the meter's 429", refused.Status, code)
	}
}

// SocketAuthed with a chain that never runs the session middleware fails closed.
func TestSocketAuthedWithoutTheSessionMiddlewareIsRefused(t *testing.T) {
	e := newSocketEnv(t, nil)
	meter := &countingMeter{}
	var ran atomic.Bool
	e.mux.SocketAuthed("POST /v1/test/meter-only", meter.wrap, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	_, _, bearer := seedAPISession(t, e.deps)
	resp := sockDo(t, e.dial(bearer), http.MethodPost, "/v1/test/meter-only", nil, nil)
	if code, _ := sockErrorCode(t, resp.Body); resp.Status != http.StatusUnauthorized || code != string(server.CodeUnauthenticated) || ran.Load() || meter.seen.Load() != 1 {
		t.Fatalf("meter-only chain = %d %s, ran %v, meter %d; want 401 and no handler", resp.Status, code, ran.Load(), meter.seen.Load())
	}
}

// Ruling B: an admitted request frame does not spend the inbound bucket, a refused one does. With a
// bucket that never refills, 64 blocked requests are admitted and then more than 120 E_BUSY
// refusals close the connection with 4008.
func TestRefusedRequestFramesSpendTheInboundBucket(t *testing.T) {
	e := newSocketEnvWith(t, nil, nil, func(o *gateway.Options) { o.FramesPerSecond, o.FrameBurst = 60, 120 })
	entered, release := sockBlockingRoute(e, "/v1/test/block")
	_, _, bearer := seedAPISession(t, e.deps)
	c := e.dial(bearer)
	for range gateway.MaxInFlightRequests {
		if _, err := c.Send(http.MethodPost, "/v1/test/block", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	sockWaitFor(t, "64 requests in the handler", func() bool { return entered.Load() == gateway.MaxInFlightRequests })
	for range 130 {
		if _, err := c.Send(http.MethodPost, "/v1/test/block", nil, nil); err != nil {
			break
		}
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("130 refused requests did not spend the inbound bucket")
	}
	if got := c.CloseStatus(); got != websocket.StatusCode(gateway.CloseRateLimited) {
		t.Fatalf("closed with %d, want %d", got, gateway.CloseRateLimited)
	}
	for range gateway.MaxInFlightRequests {
		release <- struct{}{}
	}
}

// Minor 3: the replay record is consulted only for the device the session middleware resolved. A
// dispatch whose caller names another device than its token's session gets 401, never a record.
func TestReplayRecordIsRefusedToASessionOfAnotherDevice(t *testing.T) {
	e := newSocketEnv(t, nil)
	var runs atomic.Int64
	e.routes.Socket("POST /v1/test/count", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = server.EncodeBody(w, http.StatusCreated, runs.Add(1))
	}))
	_, _, bearerA := seedAPISession(t, e.deps)
	_, devB, _ := seedAPISession(t, e.deps)
	resp := e.disp.Dispatch(context.Background(), gateway.SocketCaller{
		TokenHash: auth.TokenHash(bearerA), DeviceID: devB.ID, RemoteAddr: "127.0.0.1:1", Wire: auth.WireVersion,
	}, gateway.SocketRequest{ID: 1, Method: http.MethodPost, Route: "/v1/test/count", Body: []byte{}, Idem: []byte("cross-device")})
	if code, _ := sockErrorCode(t, resp.Body); resp.Status != http.StatusUnauthorized || code != string(server.CodeUnauthenticated) || runs.Load() != 0 {
		t.Fatalf("mismatched device = %d %s, runs %d; want 401 and no run", resp.Status, code, runs.Load())
	}
}

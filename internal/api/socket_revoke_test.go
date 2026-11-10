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
)

// selfRevokeRoute registers a route whose handler declares it revokes the calling session, closes the device's
// connections as the revocation does, and then (when hold is not nil) keeps the request in flight until hold is
// released.
func selfRevokeRoute(e *socketEnv, path string, hold chan struct{}) {
	e.routes.Socket("POST "+path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := auth.FromContext(r.Context())
		gateway.SelfRevoking(r.Context())
		e.gw.CloseDevice(sess.DeviceID, gateway.CloseSessionRevoked, "session ended")
		if hold != nil {
			<-hold
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func awaitClose(t *testing.T, c interface {
	Done() <-chan struct{}
	CloseStatus() websocket.StatusCode
}, within time.Duration, want gateway.CloseCode) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(within):
		t.Fatalf("the connection was not closed within %v", within)
	}
	if got := c.CloseStatus(); got != websocket.StatusCode(want) {
		t.Fatalf("closed with %d, want %d", got, want)
	}
}

// A socket request that revokes its own session (DELETE /v1/accounts/me, a device deleting its own row) closes the
// device's connections from inside the handler. The connection carrying the request is closed after its answer was
// written: the caller receives the 204 and then the 4004 close.
func TestASelfRevokingRequestIsAnsweredBeforeTheConnectionCloses(t *testing.T) {
	e := newSocketEnv(t, nil)
	_, _, bearer := seedAPISession(t, e.deps)
	selfRevokeRoute(e, "/v1/test/self-revoke", nil)
	c := e.dial(bearer)
	resp, err := c.Do(http.MethodPost, "/v1/test/self-revoke", nil, []byte("self-revoke"))
	if err != nil {
		t.Fatalf("the self-revoking request got no answer: %v", err)
	}
	if resp.Status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.Status)
	}
	awaitClose(t, c, 10*time.Second, gateway.CloseSessionRevoked)
}

// Any other cause of a revocation (an admin, a disabled user, a device list, another device) closes at once, even
// with a request of the connection in flight.
func TestAnotherCauseOfARevocationClosesAtOnceWithARequestInFlight(t *testing.T) {
	e := newSocketEnv(t, nil)
	_, dev, bearer := seedAPISession(t, e.deps)
	entered, release := sockBlockingRoute(e, "/v1/test/block")
	c := e.dial(bearer)
	if _, err := c.Send(http.MethodPost, "/v1/test/block", nil, []byte("busy")); err != nil {
		t.Fatal(err)
	}
	sockWaitFor(t, "the request in the handler", func() bool { return entered.Load() == 1 })
	e.gw.CloseDevice(dev.ID, gateway.CloseSessionRevoked, "revoked by an admin")
	awaitClose(t, c, 2*time.Second, gateway.CloseSessionRevoked)
	release <- struct{}{}
}

// While a self-revoke close is pending the read loop drops every inbound frame: a request sent then is never
// dispatched, and only the in-flight answer is written.
func TestAFrameSentDuringTheSelfRevokeWindowHasNoEffect(t *testing.T) {
	e := newSocketEnv(t, nil)
	_, dev, bearer := seedAPISession(t, e.deps)
	hold := make(chan struct{})
	selfRevokeRoute(e, "/v1/test/self-revoke", hold)
	var runs atomic.Int64
	e.routes.Socket("POST /v1/test/count", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runs.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	c := e.dial(bearer)
	firstID, err := c.Send(http.MethodPost, "/v1/test/self-revoke", nil, []byte("self-revoke"))
	if err != nil {
		t.Fatal(err)
	}
	// The connection is sealed once the handler has declared and revoked; wait for that through a probe that the
	// sealed read loop would drop. Sending until one is surely after the seal is not possible, so seal first.
	sockWaitFor(t, "the connection to leave the registry", func() bool { return !e.gw.Online(dev.ID) })
	if _, err := c.Send(http.MethodPost, "/v1/test/count", nil, []byte("during")); err != nil {
		t.Fatal(err)
	}
	if err := c.SendRaw([]byte{0x84, 0x06, 0x00, 0xf6, 0x82, 0x00, 0x01}); err != nil { // a heartbeat frame
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	hold <- struct{}{}
	resp, err := c.Await(firstID, 10*time.Second)
	if err != nil || resp.Status != http.StatusNoContent {
		t.Fatalf("the in-flight answer = %+v, %v; want 204", resp, err)
	}
	awaitClose(t, c, 10*time.Second, gateway.CloseSessionRevoked)
	if got := runs.Load(); got != 0 {
		t.Fatalf("a request sent during the self-revoke window ran %d times, want 0", got)
	}
}

// A self-revoke close whose request never answers still closes, after the grace period on the injected clock.
func TestASelfRevokeCloseTimesOutOnTheInjectedClock(t *testing.T) {
	e := newSocketEnv(t, nil)
	_, dev, bearer := seedAPISession(t, e.deps)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	selfRevokeRoute(e, "/v1/test/self-revoke", hold)
	c := e.dial(bearer)
	if _, err := c.Send(http.MethodPost, "/v1/test/self-revoke", nil, []byte("hung")); err != nil {
		t.Fatal(err)
	}
	sockWaitFor(t, "the connection to leave the registry", func() bool { return !e.gw.Online(dev.ID) })
	select {
	case <-c.Done():
		t.Fatal("the connection closed before the grace period")
	case <-time.After(200 * time.Millisecond):
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.clk.Advance(6 * time.Second)
		select {
		case <-c.Done():
			awaitClose(t, c, time.Second, gateway.CloseSessionRevoked)
			return
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the connection did not close after the grace period")
		}
	}
}

// Shutdown closes a connection that waits for its self-revoking answer.
func TestShutdownClosesAConnectionWaitingOnASelfRevokeClose(t *testing.T) {
	e := newSocketEnv(t, nil)
	_, dev, bearer := seedAPISession(t, e.deps)
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	selfRevokeRoute(e, "/v1/test/self-revoke", hold)
	c := e.dial(bearer)
	if _, err := c.Send(http.MethodPost, "/v1/test/self-revoke", nil, []byte("hung")); err != nil {
		t.Fatal(err)
	}
	sockWaitFor(t, "the connection to leave the registry", func() bool { return !e.gw.Online(dev.ID) })
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_ = e.gw.Shutdown(ctx)
	awaitClose(t, c, 5*time.Second, gateway.CloseGoingAway)
}

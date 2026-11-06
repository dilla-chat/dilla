package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/gateway"
)

// A socket request that revokes its own session (DELETE /v1/accounts/me, a device deleting its own row) closes the
// device's connections from inside the handler. The connection carrying the request is closed after its answer was
// written: the caller receives the 204 and then the 4004 close.
func TestASelfRevokingRequestIsAnsweredBeforeTheConnectionCloses(t *testing.T) {
	e := newSocketEnv(t, nil)
	_, dev, bearer := seedAPISession(t, e.deps)
	e.routes.Socket("POST /v1/test/self-revoke", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := auth.FromContext(r.Context())
		e.gw.CloseDevice(sess.DeviceID, gateway.CloseSessionRevoked, "session ended")
		w.WriteHeader(http.StatusNoContent)
	}))
	c := e.dial(bearer)
	resp, err := c.Do(http.MethodPost, "/v1/test/self-revoke", nil, []byte("self-revoke"))
	if err != nil {
		t.Fatalf("the self-revoking request got no answer: %v (device %s)", err, dev.ID)
	}
	if resp.Status != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.Status)
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked connection was never closed")
	}
	if got := c.CloseStatus(); got != websocket.StatusCode(gateway.CloseSessionRevoked) {
		t.Fatalf("closed with %d, want %d", got, gateway.CloseSessionRevoked)
	}
}

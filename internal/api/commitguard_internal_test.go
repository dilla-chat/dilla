package api

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// One commit upload per device at a time: a second while the first is in flight is 429
// E_RATE_LIMITED, another device is not affected, and the slot frees when the first ends. The
// refusal comes before the delivery service is asked anything (this Groups has no DS at all).
func TestASecondConcurrentCommitFromOneDeviceIsRefused(t *testing.T) {
	h := &Groups{}
	device := id.New()
	done, err := h.beginCommit(device)
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/v1/groups/0102030405060708090a0b0c0d0e0f10/commit", bytes.NewReader([]byte{0x85}))
	r.SetPathValue("id", "0102030405060708090a0b0c0d0e0f10")
	r = r.WithContext(auth.WithSession(r.Context(), auth.Session{DeviceID: device, Scope: auth.ScopeEnrolled}))
	w := httptest.NewRecorder()
	h.commit(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 while the device's first commit is in flight", w.Code)
	}

	if release, err := h.beginCommit(id.New()); err != nil {
		t.Errorf("another device: %v", err)
	} else {
		release()
	}
	done()
	if release, err := h.beginCommit(device); err != nil {
		t.Errorf("after the first commit ended: %v", err)
	} else {
		release()
	}
}

// The commit route's body has a read deadline of its own: a client that sends the header and then
// trickles nothing is cut off, not held open for as long as it likes.
func TestTheCommitRouteBodyHasAReadDeadline(t *testing.T) {
	read := make(chan error, 1)
	srv := httptest.NewServer(withReadDeadline(200*time.Millisecond, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_, err := io.ReadAll(r.Body)
			read <- err
			server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "read body: %v", err))
		})))
	t.Cleanup(srv.Close)

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// A declared body of 1 MiB, of which one byte ever arrives.
	if _, err := conn.Write([]byte("POST /commit HTTP/1.1\r\nHost: x\r\nContent-Type: application/cbor\r\n" +
		"Content-Length: 1048576\r\n\r\n\x85")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("the stalled body read completed without error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled body was still being read after 5 s; the deadline did not fire")
	}
}

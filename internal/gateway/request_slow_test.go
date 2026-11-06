package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// A request frame longer than the read limit must arrive within the bound: a client that sends the head of one and then
// drips nothing is closed (Bot v2 task 5 I3; an HTTP commit had a 60 s body deadline for the same reason).
func TestASlowDrippingRequestFrameIsClosed(t *testing.T) {
	old := requestFrameReadTimeout
	requestFrameReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { requestFrameReadTimeout = old })

	read := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		sink := newLimitedWSSink(c, 1024).(*wsSink)
		sink.allowRequestFrames()
		_, _, err = sink.read(r.Context())
		read <- err
		_ = c.CloseNow()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	frame, _ := EncodeRequestFrame(SocketRequest{ID: 1, Method: "POST", Route: "/v1/x", Body: make([]byte, 64<<10)})
	w, err := conn.Writer(ctx, websocket.MessageBinary)
	if err != nil {
		t.Fatal(err)
	}
	// The head and a little more, past the 1 KiB limit, then nothing: the message is never finished.
	if _, err := w.Write(frame[:20000]); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("a stalled request frame was read to the end")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled request frame was still being read after 5 s; the bound did not fire")
	}
}

package gatewayclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestReadPumpWaitsForTheDecoderInsteadOfFailingTheConnection: a burst read faster than the decode loop takes it
// (a cold snapshot on a host with few CPUs) waits in the reader. It used to end the connection with ErrBackpressure,
// whose fresh identify replayed the same burst and failed again, so the Bot's readiness depended on the CPU count.
func TestReadPumpWaitsForTheDecoderInsteadOfFailingTheConnection(t *testing.T) {
	const burst = 8
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for n := range uint64(burst) {
			if sendServerFrame(r.Context(), ws, OpHeartbeatAck, n+1, nil, []any{n}) != nil {
				return
			}
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	cx, stop := context.WithCancel(ctx)
	conn := &connection{ws: ws, ctx: cx, cancel: stop, pending: map[uint64]chan ResponseFrame{}}
	out := make(chan []byte, 1) // room for one frame of the burst: the rest must wait for the decoder
	done := make(chan error, 1)
	go (&Client{}).readPump(conn, out, done)
	select {
	case err := <-done:
		t.Fatalf("the reader ended the connection while the decoder was busy: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	for i := range burst {
		select {
		case <-out:
		case err := <-done:
			t.Fatalf("frame %d: the reader ended the connection: %v", i+1, err)
		case <-ctx.Done():
			t.Fatalf("frame %d never reached the decoder", i+1)
		}
	}
	stop()
	<-done
}

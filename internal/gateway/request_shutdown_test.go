package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

type blockingDispatcher struct {
	started chan struct{}
	release chan struct{}
}

func (d blockingDispatcher) Dispatch(_ context.Context, _ SocketCaller, r SocketRequest) SocketResponse {
	close(d.started)
	<-d.release
	return SocketResponse{ID: r.ID, Status: 204}
}

// A socket request running when Shutdown begins finishes, and its answer is written before the connection closes: a client
// that never got it would write the mutation again to the next process, which has no replay record for it.
func TestAnswerOfARequestRunningAtShutdownIsWrittenBeforeTheConnectionCloses(t *testing.T) {
	h := newHarness(t)
	d := blockingDispatcher{started: make(chan struct{}), release: make(chan struct{})}
	h.gw.opts.Requests = d
	c := h.connect(t, id.New())
	h.waitFrame(t, c.deviceID) // ready
	req := SocketRequest{ID: 7, Method: "POST", Route: "/v1/x", Body: []byte{0x80}, Idem: []byte("idem-0123456789ab")}
	if !h.gw.handleRequest(context.Background(), c, Inbound{request: &req}) {
		t.Fatal("the request was not admitted")
	}
	<-d.started
	done := make(chan error, 1)
	go func() { done <- h.gw.Shutdown(context.Background()) }()
	select {
	case <-done:
		t.Fatal("Shutdown returned while a request was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(d.release)
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	var answered bool
	deadline := time.After(5 * time.Second)
	for !answered {
		select {
		case raw := <-h.sink(c).frames:
			if resp, err := DecodeResponseFrame(raw); err == nil && resp.ID == 7 && resp.Status == 204 {
				answered = true
			}
		case <-deadline:
			t.Fatal("the answer of the request running at Shutdown was never written")
		}
	}
	if code := h.waitClose(t, c); code != CloseGoingAway {
		t.Fatalf("close code = %d, want 4010", code)
	}
}

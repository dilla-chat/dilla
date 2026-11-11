package helper

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/prioritygate"
)

// A helper that aborted itself on an unknown outcome reports Closed() at once, without another call and without
// waiting on the call lock (a busy helper must not block a pass asking whether it is still usable).
func TestClosedReportsAnAbortedHelperAtOnce(t *testing.T) {
	c := &Client{gate: prioritygate.New(1), transport: &diedExchange{}, callTime: time.Second, openTime: time.Second, opened: true,
		log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	if c.Closed() {
		t.Fatal("an open helper reports closed")
	}
	if _, err := c.request(context.Background(), protocol.HelperOperation(11), nil); err == nil {
		t.Fatal("a dead helper answered")
	}
	if !c.Closed() {
		t.Fatal("an aborted helper does not report closed")
	}
	// Closed never takes the call lock.
	c.mu.Lock()
	done := make(chan bool, 1)
	go func() { done <- c.Closed() }()
	select {
	case closed := <-done:
		if !closed {
			t.Fatal("closed helper reported open")
		}
	case <-time.After(time.Second):
		t.Fatal("Closed blocked on the call lock")
	}
	c.mu.Unlock()
	var none *Client
	if !none.Closed() {
		t.Fatal("a nil helper is not closed")
	}
}

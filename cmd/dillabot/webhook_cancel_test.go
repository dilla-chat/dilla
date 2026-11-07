//go:build linux

package main

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

// A delivery whose connection ends mid-post (the delivery context is cancelled) posts nothing more, sends no ack
// (dillad takes it only from that connection) and logs nothing above debug.
func TestWebhookCancelledWithItsConnectionPostsNothingAndIsQuiet(t *testing.T) {
	dest := &whDestination{group: whID(9)}
	send := &whSend{gate: make(chan struct{})} // the send waits until released or cancelled
	var logs bytes.Buffer
	poster := newTestPoster(dest, send)
	poster.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	acks := newWhAck()
	ctx, cancel := context.WithCancel(context.Background())
	poster.deliver(ctx, whPayload(t, whID(1), whID(5), "Hook", nil, "hi", false), acks.ack)
	cancel()
	poster.wait()
	select {
	case <-acks.done:
		t.Fatal("a cancelled delivery was acknowledged")
	default:
	}
	send.mu.Lock()
	sent := len(send.sent)
	send.mu.Unlock()
	if sent != 0 {
		t.Fatalf("%d messages posted by a cancelled delivery", sent)
	}
	if logs.Len() != 0 {
		t.Fatalf("a cancelled delivery logged at info or above: %s", logs.String())
	}
}

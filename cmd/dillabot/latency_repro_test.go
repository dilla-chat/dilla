//go:build linux

package main

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonasthim/dilla/internal/id"
)

// Reproducer (CT 144, 2026-10-31): one member's /play is a chain of sequential interaction
// requests in one interaction group, each sent ~0.5 s after the previous reply. signal1.3's web
// client sends state, search, enqueue, announce(register), resume, announce(card): six. With a
// healthy gateway every request reaches the command worker, and every one must be polled within 1 s.
func TestReproSequentialCommandChainIsAnsweredWithinOneSecond(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		installation, group := id.New(), id.New()
		runtime := newBotGatewayRuntime(t.Context(), nil, false, nil)
		defer runtime.Close()
		polled := make(chan time.Time, 1)
		runtime.SetInteractionPoller(installation, func(context.Context, id.ID) error {
			polled <- time.Now()
			return nil
		})
		runtime.StartCommandWorkers()
		var latencies []time.Duration
		for request := range 6 {
			started := time.Now()
			runtime.notifyInteraction(installation, group)
			select {
			case at := <-polled:
				latencies = append(latencies, at.Sub(started))
			case <-time.After(time.Second):
				t.Fatalf("request %d was not polled within one second", request+1)
			}
			// The Bot's work, its reply, and the client's next request.
			time.Sleep(500 * time.Millisecond)
		}
		t.Logf("signal-to-poll latency per request: %v", latencies)
	})
}

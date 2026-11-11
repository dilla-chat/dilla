package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/health"
	"github.com/dilla-chat/dilla/internal/id"
)

// While the events stage stays degraded, one failed command per report period keeps dillad's latch on; a burst of
// failures inside one period counts once, and a clean poll stops the counting.
func TestPersistentEventsFailuresCountOncePerReportPeriod(t *testing.T) {
	installation := id.New()
	failing := true
	poll := func(context.Context) error {
		if failing {
			return &textPollError{stage: textPollEvents, err: errors.New("events failed")}
		}
		return nil
	}
	clock := time.Unix(1_800_000_000, 0)
	entry := &sealedEntry{installation: installation}
	r := &botGatewayRuntime{now: func() time.Time { return clock }, log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		pollers: map[id.ID]func(context.Context) error{installation: poll}}
	r.sealed.entries = map[id.ID]*sealedEntry{installation: entry}
	taken := func() uint32 { return entry.healthLoad(clock, nil).FailedCommands }

	for range 3 {
		r.PollText(context.Background())
	}
	if got := taken(); got != 1 {
		t.Fatalf("at the transition: %d failed commands, want 1", got)
	}
	// A report period later, a burst of failures counts once.
	clock = clock.Add(health.ReportEvery)
	r.PollText(context.Background())
	r.PollText(context.Background())
	if got := taken(); got != 1 {
		t.Fatalf("a burst in one report period: %d, want 1", got)
	}
	// Failures inside the same period as the last count add nothing.
	r.PollText(context.Background())
	if got := taken(); got != 0 {
		t.Fatalf("inside the same period: %d, want 0", got)
	}
	// A clean poll ends it: no more failed commands.
	failing = false
	clock = clock.Add(2 * health.ReportEvery)
	r.PollText(context.Background())
	r.PollText(context.Background())
	if got := taken(); got != 0 {
		t.Fatalf("after a clean poll: %d, want 0", got)
	}
}

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

func newTestMonitor() *health.Monitor {
	return health.New(health.ProberFunc(func(context.Context) error { return nil }),
		health.ReporterFunc(func(context.Context, health.Report) error { return nil }), nil)
}

// A failure before the events stage breaks the run: events, membership, events, events is not three in a row.
func TestNonEventsFailureResetsTheEventsRun(t *testing.T) {
	installation := id.New()
	stages := []string{textPollEvents, textPollMembership, textPollEvents, textPollEvents}
	next := 0
	poll := func(context.Context) error {
		stage := stages[next%len(stages)]
		next++
		return &textPollError{stage: stage, err: errors.New("failed")}
	}
	monitor := newTestMonitor()
	r := &botGatewayRuntime{log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		pollers: map[id.ID]func(context.Context) error{installation: poll}}
	r.sealed.entries = map[id.ID]*sealedEntry{installation: {installation: installation, monitor: monitor}}
	for range 4 {
		r.PollText(context.Background())
	}
	if _, ok := monitor.FailingSince(); ok {
		t.Fatal("degraded although a membership failure came between the events failures")
	}
}

// The mark made before the installation's health monitor exists is applied when the monitor appears.
func TestDegradedMarkWaitsForTheMonitor(t *testing.T) {
	installation := id.New()
	poll := func(context.Context) error {
		return &textPollError{stage: textPollEvents, err: errors.New("events failed")}
	}
	entry := &sealedEntry{installation: installation}
	r := &botGatewayRuntime{log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		pollers: map[id.ID]func(context.Context) error{installation: poll}}
	r.sealed.entries = map[id.ID]*sealedEntry{installation: entry}
	for range 3 {
		r.PollText(context.Background())
	}
	if got := entry.healthLoad(time.Now(), nil).FailedCommands; got != 1 {
		t.Fatalf("failed commands = %d, want 1 without a monitor", got)
	}
	monitor := newTestMonitor()
	entry.mu.Lock()
	entry.monitor = monitor
	entry.mu.Unlock()
	if _, ok := monitor.FailingSince(); ok {
		t.Fatal("marked before a poll")
	}
	r.PollText(context.Background())
	if _, ok := monitor.FailingSince(); !ok {
		t.Fatal("the monitor was never marked")
	}
	if got := entry.healthLoad(time.Now(), nil).FailedCommands; got != 0 {
		t.Fatalf("failed commands counted again: %d", got)
	}
}

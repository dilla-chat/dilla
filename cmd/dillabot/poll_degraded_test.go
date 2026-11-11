package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/health"
	"github.com/dilla-chat/dilla/internal/id"
)

// v0.1.0-bot2.4: three consecutive events-stage poll failures mark the installation degraded in its health report
// (failing_since); the next clean poll clears it. Each way logs once, not per wake.
func TestEventsStageFailuresDegradeHealth(t *testing.T) {
	var out bytes.Buffer
	installation := id.New()
	failing := true
	stage := textPollEvents
	poll := func(context.Context) error {
		if failing {
			return &textPollError{stage: stage, err: errors.New("events failed")}
		}
		return nil
	}
	monitor := health.New(health.ProberFunc(func(context.Context) error { return nil }), health.ReporterFunc(func(context.Context, health.Report) error { return nil }), nil)
	r := &botGatewayRuntime{log: slog.New(slog.NewTextHandler(&out, nil)),
		pollers: map[id.ID]func(context.Context) error{installation: poll}}
	r.sealed.entries = map[id.ID]*sealedEntry{installation: {installation: installation, monitor: monitor}}
	degraded := func() bool { _, ok := monitor.FailingSince(); return ok }
	count := func(msg string) int { return strings.Count(out.String(), msg) }

	r.PollText(context.Background())
	r.PollText(context.Background())
	if degraded() {
		t.Fatal("degraded after two failures")
	}
	r.PollText(context.Background())
	if !degraded() {
		t.Fatal("not degraded after three consecutive events failures")
	}
	for range 4 {
		r.PollText(context.Background())
	}
	// dillad ignores the Bot's failing_since; the degraded transition reaches it as one failed command, once.
	entry := r.sealed.lookup(installation)
	if got := entry.healthLoad(time.Now(), nil).FailedCommands; got != 1 {
		t.Fatalf("failed commands in the health load = %d, want 1", got)
	}
	if n := count("bot installation poll degraded"); n != 1 {
		t.Fatalf("degraded line %d times:\n%s", n, out.String())
	}

	failing = false
	r.PollText(context.Background())
	if degraded() {
		t.Fatal("still degraded after a clean poll")
	}
	r.PollText(context.Background())
	if n := count("bot installation poll recovered"); n != 1 {
		t.Fatalf("recovered line %d times:\n%s", n, out.String())
	}
	if n := count("bot installation poll degraded"); n != 1 {
		t.Fatalf("degraded line %d times after recovery", n)
	}
}

// A failure at another stage, or a clean poll between failures, never reaches three in a row.
func TestEventsStageFailuresMustBeConsecutive(t *testing.T) {
	installation := id.New()
	calls := 0
	poll := func(context.Context) error {
		calls++
		switch calls % 3 {
		case 0:
			return nil
		case 1:
			return &textPollError{stage: textPollEvents, err: errors.New("events failed")}
		}
		return &textPollError{stage: textPollMembership, err: errors.New("membership failed")}
	}
	monitor := health.New(health.ProberFunc(func(context.Context) error { return nil }), health.ReporterFunc(func(context.Context, health.Report) error { return nil }), nil)
	r := &botGatewayRuntime{log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		pollers: map[id.ID]func(context.Context) error{installation: poll}}
	r.sealed.entries = map[id.ID]*sealedEntry{installation: {installation: installation, monitor: monitor}}
	for range 12 {
		r.PollText(context.Background())
	}
	if _, ok := monitor.FailingSince(); ok {
		t.Fatal("degraded without three consecutive events failures")
	}
}

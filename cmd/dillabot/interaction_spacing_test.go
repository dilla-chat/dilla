//go:build linux

package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/id"
)

func TestCommandWorkerSpacingIsPerInstallation(t *testing.T) {
	r := newBotGatewayRuntime(t.Context(), nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	first, second, group := id.New(), id.New(), id.New()
	pool := newCommandBatchTestPool(t)
	for _, installation := range []id.ID{first, second} {
		r.SetInteractionPoller(installation, func(context.Context, id.ID) error { return nil })
	}
	now := time.Now()
	r.notifyInteraction(first, group)
	jobs, _, _ := r.commandBatch(pool, now)
	if len(jobs) != 1 {
		t.Fatal("first turn was not admitted")
	}
	releaseCommandBatch(pool, jobs)
	r.notifyInteraction(first, group)
	r.notifyInteraction(second, group)
	jobs, delay, pending := r.commandBatch(pool, now)
	if len(jobs) != 1 || jobs[0].work.installation != second || !pending || delay != interactionSpacing {
		t.Fatalf("jobs=%d, pending=%v, delay=%s; independent community must run while first remains spaced", len(jobs), pending, delay)
	}
	releaseCommandBatch(pool, jobs)
	jobs, _, _ = r.commandBatch(pool, now.Add(interactionSpacing))
	defer releaseCommandBatch(pool, jobs)
	if len(jobs) != 1 || jobs[0].work.installation != first {
		t.Fatal("first community did not run after its own spacing")
	}
}

func TestInteractionSpacingSurvivesRestartButNotRemoval(t *testing.T) {
	r := newBotGatewayRuntime(t.Context(), nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	kept, removed, group := id.New(), id.New(), id.New()
	pool := newCommandBatchTestPool(t)
	register := func(installation id.ID) {
		r.SetInteractionPoller(installation, func(context.Context, id.ID) error { return nil })
		r.notifyInteraction(installation, group)
	}
	register(kept)
	register(removed)
	now := time.Now()
	jobs, _, _ := r.commandBatch(pool, now)
	if len(jobs) != 2 {
		t.Fatalf("initial jobs=%d, want 2", len(jobs))
	}
	releaseCommandBatch(pool, jobs)
	r.RetainOnly([]id.ID{kept})
	r.Drop(kept)
	register(kept)
	register(removed)
	jobs, delay, pending := r.commandBatch(pool, now)
	if len(jobs) != 1 || jobs[0].work.installation != removed || !pending || delay != interactionSpacing {
		t.Fatalf("jobs=%v delay=%s pending=%v; restart must retain spacing while removed installation starts fresh", jobs, delay, pending)
	}
	releaseCommandBatch(pool, jobs)
	jobs, _, _ = r.commandBatch(pool, now.Add(interactionSpacing))
	defer releaseCommandBatch(pool, jobs)
	if len(jobs) != 1 || jobs[0].work.installation != kept {
		t.Fatal("retained installation did not become eligible after spacing")
	}
}

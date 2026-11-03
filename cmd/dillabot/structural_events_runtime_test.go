package main

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/service"
)

func TestDrainStructuralEventsResumesMoreWithinBound(t *testing.T) {
	calls := 0
	got, err := drainStructuralEvents(context.Background(), func(context.Context) (service.StructuralEventPollResult, error) {
		calls++
		return service.StructuralEventPollResult{Processed: 2, Pages: 2, AcknowledgedThrough: uint64(calls * 2), More: true}, nil
	})
	if err != nil {
		t.Fatalf("drainStructuralEvents: %v", err)
	}
	if calls != maxStructuralEventPollPasses || got.Processed != 2*maxStructuralEventPollPasses || got.Pages != 2*maxStructuralEventPollPasses || got.AcknowledgedThrough != 2*maxStructuralEventPollPasses || !got.More {
		t.Fatalf("calls=%d result=%+v", calls, got)
	}
}

func TestDrainStructuralEventsStopsWhenCaughtUp(t *testing.T) {
	calls := 0
	got, err := drainStructuralEvents(context.Background(), func(context.Context) (service.StructuralEventPollResult, error) {
		calls++
		return service.StructuralEventPollResult{Processed: 1, Pages: 1, AcknowledgedThrough: 7}, nil
	})
	if err != nil || calls != 1 || got.More {
		t.Fatalf("calls=%d result=%+v err=%v", calls, got, err)
	}
}

func TestDrainStructuralEventsStopsOnHandlerError(t *testing.T) {
	want := errors.New("kind-3 interaction has no authenticated roster")
	calls := 0
	_, err := drainStructuralEvents(context.Background(), func(context.Context) (service.StructuralEventPollResult, error) {
		calls++
		return service.StructuralEventPollResult{}, want
	})
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestFinishStructuralGapStagesBeforeAdvancing(t *testing.T) {
	gap := &service.StructuralEventGapError{Floor: 11, HighWater: 15}
	completed := 0
	stage := func(_ context.Context, floor, highWater uint64) (service.StructuralGapRecoveryResult, error) {
		if floor != 11 || highWater != 15 {
			t.Fatalf("unexpected gap %d..%d", floor, highWater)
		}
		return service.StructuralGapRecoveryResult{HighWater: 15, More: true}, nil
	}
	complete := func(_ context.Context, _ uint64) error { completed++; return nil }
	done, err := finishStructuralGap(context.Background(), gap, stage, complete)
	if err != nil || done || completed != 0 {
		t.Fatalf("incomplete scan advanced cursor: done=%t completed=%d err=%v", done, completed, err)
	}
	stage = func(_ context.Context, _, _ uint64) (service.StructuralGapRecoveryResult, error) {
		return service.StructuralGapRecoveryResult{HighWater: 15, Done: true}, nil
	}
	complete = func(_ context.Context, highWater uint64) error {
		if highWater != 15 {
			t.Fatalf("advanced to %d", highWater)
		}
		completed++
		return nil
	}
	done, err = finishStructuralGap(context.Background(), gap, stage, complete)
	if err != nil || !done || completed != 1 {
		t.Fatalf("completed scan did not advance cursor: done=%t completed=%d err=%v", done, completed, err)
	}
}

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestInteractionDrainRateLimitDefersWithoutPause(t *testing.T) {
	calls := 0
	refusal := &client.APIError{Status: 429, Code: "E_RATE_LIMITED", Outcome: client.OutcomeDefinitive}
	poller := interactionPollerFunc(func(context.Context, service.GroupEventHandler) (service.GroupEventPollResult, error) {
		calls++
		if calls == 1 {
			return service.GroupEventPollResult{}, &service.GroupEventHandlerError{DeliveryID: 1, Err: refusal}
		}
		return service.GroupEventPollResult{Handled: 1, AcknowledgedThrough: 1}, nil
	})
	runtime := newInteractionEventRuntime(poller, groupEventHandlerFuncForTest(func(context.Context, native.GroupEvent) error { return nil }))
	result, err := runtime.DrainHolding(t.Context(), nil)
	if err != nil || result.AcknowledgedThrough != 0 || calls != 1 {
		t.Fatalf("rate-limited drain=%+v err=%v calls=%d", result, err, calls)
	}
	result, err = runtime.DrainHolding(t.Context(), nil)
	if err != nil || result.Handled != 1 || result.AcknowledgedThrough != 1 {
		t.Fatalf("resumed drain=%+v err=%v", result, err)
	}
}

func TestReconciliationRateLimitDoesNotHideOtherFailures(t *testing.T) {
	refusal := &client.APIError{Status: 429, Code: "E_RATE_LIMITED", Outcome: client.OutcomeDefinitive}
	other := errors.New("native state write failed")
	for _, tc := range []struct {
		name      string
		err       error
		deferTick bool
	}{
		{"rate limit", refusal, true},
		{"deadline during retry", errors.Join(refusal, context.DeadlineExceeded), true},
		{"wrapped", fmt.Errorf("utility: %w", refusal), true},
		{"mixed failure", errors.Join(refusal, other), false},
		{"non rate failure", other, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reconciliationRateLimit(tc.err); (got != nil) != tc.deferTick {
				t.Fatalf("deferral=%v want=%v", got, tc.deferTick)
			}
		})
	}
}

func TestRateDeferredReadinessKeepsRetainedHelperAlive(t *testing.T) {
	community, installation := id.New(), id.New()
	source := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
	registry := newMusicQueueRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	if err := registry.RetainInteractions(installation, community, source); err != nil {
		t.Fatal(err)
	}
	moderation := newAutoModerationEventState()
	refusal := &client.APIError{Status: 429, Code: "E_RATE_LIMITED", Outcome: client.OutcomeDefinitive}
	cleanupUnreadyInstallation(installation, false, refusal, registry, moderation)
	if source.closed || registry.Native(installation, community) != source {
		t.Fatal("rate-deferred readiness closed the helper used by retained intake handlers")
	}
	cleanupUnreadyInstallation(installation, false, errors.New("membership denied"), registry, moderation)
	if !source.closed || registry.Native(installation, community) != nil {
		t.Fatal("non-transient readiness failure did not clean up the runtime")
	}
}

func TestDeferredMusicRestoreKeepsRetainedInteractionRuntime(t *testing.T) {
	community, installation := id.New(), id.New()
	source := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
	registry := newMusicQueueRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	if err := registry.RetainInteractions(installation, community, source); err != nil {
		t.Fatal(err)
	}
	refusal := &client.APIError{Status: 429, Code: "E_RATE_LIMITED", Outcome: client.OutcomeDefinitive}
	if cleanupFailedMusicRestore(installation, refusal, registry) == nil {
		t.Fatal("deferred restore must retain the prior handlers")
	}
	if source.closed || registry.Native(installation, community) != source {
		t.Fatal("deferred music restore closed the interaction helper")
	}
	if cleanupFailedMusicRestore(installation, errors.New("channels denied"), registry) != nil || !source.closed {
		t.Fatal("permanent restore failure retained an invalid runtime")
	}
}

func TestCacheCapacityDeferralKeepsRuntimeAndDoesNotHidePermanentErrors(t *testing.T) {
	c, err := client.New(client.Config{BaseURL: "https://example.test", InstanceID: id.New(), RateLimits: client.RateLimits{DeviceCacheLimit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SessionForDevice("first", id.New()); err != nil {
		t.Fatal(err)
	}
	_, capacity := c.SessionForDevice("second", id.New())
	if !errors.Is(capacity, client.ErrRateCacheCapacity) {
		t.Fatalf("not capacity: %v", capacity)
	}
	if !client.IsDeferred(capacity) || client.RateLimitError(capacity) == nil {
		t.Fatalf("capacity is not retry safe: %v", capacity)
	}
	permanent := errors.New("state write failed")
	if client.IsDeferred(errors.Join(capacity, permanent)) || client.RateLimitError(errors.Join(capacity, permanent)) != nil {
		t.Fatal("capacity hid a permanent failure")
	}
	if client.IsDeferred(&client.APIError{Status: 503, Code: "E_UNAVAILABLE", Outcome: client.OutcomeDefinitive}) {
		t.Fatal("unrelated server failure became capacity deferral")
	}
	community, installation := id.New(), id.New()
	source := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
	registry := newMusicQueueRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	if err = registry.RetainInteractions(installation, community, source); err != nil {
		t.Fatal(err)
	}
	cleanupUnreadyInstallation(installation, false, capacity, registry, newAutoModerationEventState())
	if cleanupFailedMusicRestore(installation, capacity, registry) == nil || source.closed || registry.Native(installation, community) != source {
		t.Fatal("capacity destroyed retained runtime")
	}
}

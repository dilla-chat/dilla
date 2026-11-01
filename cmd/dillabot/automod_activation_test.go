//go:build linux

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

func TestAutoModerationSaveActivatesBeforeDelayedReconciliation(t *testing.T) {
	ctx := t.Context()
	installation, community, group, channel, user, device := id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	now := time.Now().Add(-10 * time.Second)
	source := &routedEventSource{binding: native.Binding{Instance: id.New(), Installation: installation, Community: community}, page: native.GroupEventPage{HighWater: 4}}
	state := newAutoModerationEventState()
	state.now = func() time.Time { return now }
	disabled := logOnlyModerationConfig()
	initial, _, _, err := state.Configure(ctx, source, installation, community, 0, disabled, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var actions []service.ModerationAction
	authority := routedModerationAuthorityFunc(func(context.Context, native.GroupEvent) (bool, bool, error) { return false, false, nil })
	sink := routedModerationSinkFunc(func(_ context.Context, a service.ModerationAction) error { actions = append(actions, a); return nil })
	dependenciesUnavailable := false
	prepare := func(moderation.Config) (service.ModerationActorAuthority, service.ModerationActionSink, error) {
		if dependenciesUnavailable {
			return nil, nil, errors.New("dependency unavailable after activation")
		}
		return authority, sink, nil
	}
	config := state.ConfigRuntime(source, installation, community, prepare)
	cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleDuplicate: moderation.ActionWarn}}
	persisted := 0
	receivedDuringSave := now.Add(time.Second)
	save := func() error { persisted++; now = now.Add(2 * time.Second); return nil }
	if err := config.PersistConfig(ctx, cfg, 0, save); err != nil {
		t.Fatal(err)
	}
	if fence := state.entries[installation].replayReceivedThrough; fence != uint64(now.UnixMilli()) {
		t.Fatalf("activation fence=%d, want successful commit time %d", fence, now.UnixMilli())
	}
	inspector := groupEventInspectorFunc(func(context.Context, id.ID) (native.GroupInfo, error) {
		return native.GroupInfo{ID: group, Kind: 0}, nil
	})
	handler := newGroupEventRouter(inspector, nil, state.EventHandler(installation, community), 0, 0)
	send := func(delivery uint64, at time.Time, body string) {
		t.Helper()
		e := native.GroupEvent{DeliveryID: delivery, Community: community, Group: group, Channel: channel, Message: id.New(), SourceSequence: delivery, SenderUser: user, SenderDevice: device, ReceivedAt: uint64(at.Unix()), Body: &body}
		if err := handler.HandleGroupEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// The source page arrives after save, but its receipt predates activation.
	for i := uint64(5); i < 8; i++ {
		send(i, receivedDuringSave, "old repeated backlog")
	}
	if len(actions) != 0 {
		t.Fatalf("pre-save backlog caused actions: %+v", actions)
	}
	now = now.Add(time.Second)
	send(8, now, "new duplicate")
	send(9, now, "new duplicate")
	// An exact successful retry must not discard these two observations or move the fence.
	boundary := state.entries[installation].replayReceivedThrough
	dependenciesUnavailable = true
	if err := config.PersistConfig(ctx, cfg, 0, save); err != nil {
		t.Fatal(err)
	}
	if got := state.entries[installation]; got.runtime != initial || got.replayReceivedThrough != boundary {
		t.Fatalf("exact save retry replaced runtime/fence: %+v", got)
	}
	dependenciesUnavailable = false
	source.page.HighWater = 10
	now = now.Add(4 * time.Second)
	stale, _, staleFence, err := state.Configure(ctx, source, installation, community, 0, disabled, nil, nil)
	if err != nil || stale != initial || staleFence != boundary {
		t.Fatalf("stale reconciliation replaced successful save: same=%v fence=%d err=%v", stale == initial, staleFence, err)
	}
	restored, _, fence, err := state.Configure(ctx, source, installation, community, 1, cfg, authority, sink)
	if err != nil {
		t.Fatal(err)
	}
	if restored != initial || fence != boundary {
		t.Fatal("delayed reconciliation reset the acknowledged policy or activation fence")
	}
	send(10, now.Add(-4*time.Second), "new duplicate")
	if len(actions) != 1 || actions[0].Decision.Action != moderation.ActionWarn {
		t.Fatalf("post-save triplet produced actions=%+v, want one warning", actions)
	}
	if persisted != 2 {
		t.Fatalf("idempotent persistence receipt not checked: calls=%d", persisted)
	}
}

func TestAutoModerationActivationFailureKeepsPriorState(t *testing.T) {
	for _, stage := range []string{"dependencies", "source", "CAS"} {
		t.Run(stage, func(t *testing.T) {
			ctx := t.Context()
			installation, community := id.New(), id.New()
			source := &routedEventSource{binding: native.Binding{Instance: id.New(), Installation: installation, Community: community}, page: native.GroupEventPage{HighWater: 4}}
			state := newAutoModerationEventState()
			cfg := logOnlyModerationConfig()
			initial, _, boundary, err := state.Configure(ctx, source, installation, community, 0, cfg, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			fail := errors.New("activation unavailable")
			writes := 0
			prepare := func(moderation.Config) (service.ModerationActorAuthority, service.ModerationActionSink, error) {
				if stage == "dependencies" {
					return nil, nil, fail
				}
				return routedModerationAuthorityFunc(func(context.Context, native.GroupEvent) (bool, bool, error) { return false, false, nil }), routedModerationSinkFunc(func(context.Context, service.ModerationAction) error { return nil }), nil
			}
			if stage == "source" {
				source.page = native.GroupEventPage{Next: 6, HighWater: 5}
			}
			runtime := state.ConfigRuntime(source, installation, community, prepare)
			err = runtime.PersistConfig(ctx, moderation.Config{Enabled: true}, 0, func() error {
				writes++
				if stage == "CAS" {
					return fail
				}
				return nil
			})
			if err == nil {
				t.Fatal("activation failure hidden")
			}
			got := state.entries[installation]
			if got.runtime != initial || got.revision != 0 || got.replayReceivedThrough != boundary {
				t.Fatalf("failed activation changed prior state: %+v", got)
			}
			if stage != "CAS" && writes != 0 {
				t.Fatalf("persisted before %s was ready", stage)
			}
		})
	}
}

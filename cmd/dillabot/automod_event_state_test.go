//go:build linux

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/moderation"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestLogOnlyModerationConfigDefaults(t *testing.T) {
	cfg := logOnlyModerationConfig()
	if cfg.Enabled {
		t.Fatal("log-only defaults unexpectedly enabled")
	}
	for _, rule := range []moderation.Rule{moderation.RuleRate, moderation.RuleDuplicate, moderation.RuleMentions} {
		if cfg.Actions[rule] != moderation.ActionLog {
			t.Errorf("default action for %s = %q, want log", rule, cfg.Actions[rule])
		}
	}
}

type durableModerationEventSource struct {
	*utilityStateFake
	page    native.GroupEventPage
	retired bool
}

func (s *durableModerationEventSource) ReadGroupEvents(context.Context, uint64, uint64) (native.GroupEventPage, error) {
	return s.page, nil
}

func (s *durableModerationEventSource) StateTransaction(ctx context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if s.retired {
		return nil, errors.New("native helper retired")
	}
	return s.utilityStateFake.StateTransaction(ctx, tx, changes)
}

func TestAutoModerationReplacementHelperRestoresPendingCheckpoint(t *testing.T) {
	now := time.Unix(1800000000, 0)
	store := newUtilityStateFake()
	installation, community := store.binding.Installation, store.binding.Community
	source := &durableModerationEventSource{utilityStateFake: store}
	state := newAutoModerationEventState()
	state.now = func() time.Time { return now }
	cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn}}
	authority := routedModerationAuthorityFunc(func(context.Context, native.GroupEvent) (bool, bool, error) { return false, false, nil })
	calls := 0
	sink := routedModerationSinkFunc(func(_ context.Context, action service.ModerationAction) error {
		if action.Decision.Action != moderation.ActionWarn {
			t.Fatalf("pending warning changed to %s", action.Decision.Action)
		}
		calls++
		if calls == 1 {
			return errors.New("evidence persistence unavailable")
		}
		return nil
	})
	_, through, received, err := state.Configure(t.Context(), source, installation, community, 1, cfg, authority, sink)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	body := "message with mentions"
	event := native.GroupEvent{DeliveryID: 1, Community: community, Group: id.New(), Channel: id.New(), Message: id.New(),
		SourceSequence: 1, ReceivedAt: uint64(now.Unix()), SenderUser: id.New(), SenderDevice: id.New(), Body: &body}
	mentions := []string{"1", "2", "3", "4", "5", "6"}
	if _, err = state.EventHandler(installation, community).Handle(t.Context(), event, false, mentions); err == nil || calls != 1 {
		t.Fatalf("initial action: calls=%d err=%v", calls, err)
	}
	// A failed music restore can retire the helper while membership was ready.
	// Reconciliation reopens the same native store without changing the policy.
	source.retired = true
	replacement := &durableModerationEventSource{utilityStateFake: store, page: native.GroupEventPage{Next: 1, HighWater: 1}}
	now = now.Add(time.Second)
	_, restoredThrough, restoredReceived, err := state.Configure(t.Context(), replacement, installation, community, 1, cfg, authority, sink)
	if err != nil || restoredThrough != through || restoredReceived != received {
		t.Fatalf("restored replay boundary=%d,%d want=%d,%d err=%v", restoredThrough, restoredReceived, through, received, err)
	}
	if _, err = state.EventHandler(installation, community).Handle(t.Context(), event, true, mentions); err != nil || calls != 2 {
		t.Fatalf("pending action did not finish on replacement helper: calls=%d err=%v", calls, err)
	}
	// Completion must survive a process restart, even if the journal ACK was lost.
	restarted := newAutoModerationEventState()
	restarted.now = state.now
	if _, _, _, err = restarted.Configure(t.Context(), replacement, installation, community, 1, cfg, authority, sink); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.EventHandler(installation, community).Handle(t.Context(), event, true, mentions); err != nil || calls != 2 {
		t.Fatalf("completed action repeated after restart: calls=%d err=%v", calls, err)
	}
}

func TestUnavailableModerationBackendFailsClosedForConfiguredActions(t *testing.T) {
	cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{
		moderation.RuleRate: moderation.ActionWarn, moderation.RuleDuplicate: moderation.ActionTimeout,
		moderation.RuleMentions: moderation.ActionLog,
	}}
	setModerationActionsLogOnly(&cfg)
	for rule, action := range cfg.Actions {
		if action != moderation.ActionLog {
			t.Errorf("fallback action for %s = %q, want log", rule, action)
		}
	}
}

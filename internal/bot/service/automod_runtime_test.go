package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

type automodSinkFunc func(context.Context, ModerationAction) error

func (f automodSinkFunc) ApplyModerationAction(ctx context.Context, action ModerationAction) error {
	return f(ctx, action)
}

type automodAuthorityFunc func(context.Context, native.GroupEvent) (bool, bool, error)

func (f automodAuthorityFunc) ModerationActorRoles(ctx context.Context, event native.GroupEvent) (bool, bool, error) {
	return f(ctx, event)
}

func ordinaryAutomodAuthority(context.Context, native.GroupEvent) (bool, bool, error) {
	return false, false, nil
}

func TestAutoModerationRuntimeUsesNativeActorAndDeduplicates(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var got []ModerationAction
	runtime := NewAutoModerationRuntime(moderation.Config{
		Enabled: true,
		Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn},
	}, automodAuthorityFunc(ordinaryAutomodAuthority), automodSinkFunc(func(_ context.Context, action ModerationAction) error {
		got = append(got, action)
		return nil
	}), func() time.Time { return now })
	body := "private text"
	event := native.GroupEvent{DeliveryID: 1, Community: id.New(), Group: id.New(), Channel: id.New(),
		SourceSequence: 9, ReceivedAt: uint64(now.Unix()), SenderUser: id.New(), SenderDevice: id.New(),
		SenderKind: 0, SenderTier: 0, EnvelopeType: 0, Body: &body, Message: id.New()}

	for i := 0; i < 6; i++ {
		event.DeliveryID++
		event.SourceSequence++
		event.Message = id.New()
		var mentions []string
		if i == 5 {
			mentions = []string{"1", "2", "3", "4", "5", "6"}
		}
		decision, err := runtime.Handle(context.Background(), event, false, mentions)
		if err != nil {
			t.Fatal(err)
		}
		if i == 5 && decision.Action != moderation.ActionWarn {
			t.Fatalf("sixth message action = %q, want warn", decision.Action)
		}
	}
	last := event
	if _, err := runtime.Handle(context.Background(), last, false, []string{"1", "2", "3", "4", "5", "6"}); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("sink was not called for violating decisions")
	}
	lastCount := len(got)
	for _, action := range got {
		if action.SourceRevision == 0 {
			t.Fatal("sink action omitted the source revision")
		}
		if action.Decision.Action == moderation.ActionWarn {
			if action.Actor != event.SenderUser.String() || action.Group != event.Group.String() || action.Excerpt != moderation.Excerpt(body) || action.SourceRevision != event.SourceSequence {
				t.Fatalf("sink action actor/excerpt = %v/%q", action.Actor, action.Excerpt)
			}
			if !action.ReceivedAt.Equal(now) {
				t.Fatalf("sink receipt time = %s, want %s", action.ReceivedAt, now)
			}
			break
		}
	}
	if lastCount != 4 {
		t.Fatalf("sink calls before duplicate = %d, want four violating decisions", lastCount)
	}
}

func TestAutoModerationRuntimeDoesNotCountEditsAsNewMessages(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var got []ModerationAction
	runtime := NewAutoModerationRuntime(moderation.Config{
		Enabled: true,
		Actions: map[moderation.Rule]moderation.Action{moderation.RuleRate: moderation.ActionRemove},
	}, automodAuthorityFunc(ordinaryAutomodAuthority), automodSinkFunc(func(_ context.Context, action ModerationAction) error {
		got = append(got, action)
		return nil
	}), func() time.Time { return now })
	body := ""
	user, community, group, channel := id.New(), id.New(), id.New(), id.New()
	for i := 0; i < 5; i++ {
		body = fmt.Sprintf("hello %d", i)
		event := native.GroupEvent{DeliveryID: uint64(i + 1), Community: community, Group: group, Channel: channel,
			SourceSequence: uint64(i + 1), ReceivedAt: uint64(now.Unix()), SenderUser: user, SenderDevice: id.New(),
			SenderKind: 0, SenderTier: 0, EnvelopeType: 0, Body: &body, Message: id.New()}
		if _, err := runtime.Handle(context.Background(), event, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	body = "edited body"
	event := native.GroupEvent{DeliveryID: 6, Community: community, Group: group, Channel: channel,
		SourceSequence: 6, ReceivedAt: uint64(now.Unix()), SenderUser: user, SenderDevice: id.New(),
		SenderKind: 0, SenderTier: 0, EnvelopeType: 1, Body: &body, Message: id.New()}
	decision, err := runtime.Handle(context.Background(), event, false, nil)
	if err != nil || decision.Ignored || len(decision.Rules) != 0 || len(got) != 0 {
		t.Fatalf("edit counted toward rate rule: decision=%+v actions=%d err=%v", decision, len(got), err)
	}
}

func TestAutoModerationRuntimeReplayRebuildsWithoutSinkAndBoundsExcerpt(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	warnings := 0
	sinkCalls := 0
	runtime := NewAutoModerationRuntime(moderation.Config{Enabled: true,
		Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn}},
		automodAuthorityFunc(ordinaryAutomodAuthority), automodSinkFunc(func(_ context.Context, action ModerationAction) error {
			sinkCalls++
			if !action.Decision.DuplicateDelivery && action.Decision.Ignored {
				t.Fatal("ignored decision reached sink")
			}
			if action.Decision.Action == moderation.ActionWarn {
				warnings++
			}
			if len(action.Excerpt) > moderation.ExcerptMaxBytes {
				t.Fatalf("excerpt has %d bytes", len(action.Excerpt))
			}
			return nil
		}), func() time.Time { return now })
	body := "secret"
	base := native.GroupEvent{DeliveryID: 1, Community: id.New(), Group: id.New(), Channel: id.New(), SenderUser: id.New(), SenderDevice: id.New(),
		SenderKind: 0, EnvelopeType: 0, Body: &body}
	for i := 0; i < 6; i++ {
		event := base
		event.SourceSequence = uint64(i + 1)
		event.Message = id.New()
		event.ReceivedAt = uint64(now.Add(-time.Duration(5-i) * time.Second).Unix())
		if _, err := runtime.Handle(context.Background(), event, true, []string{"a", "b", "c", "d", "e", "f"}); err != nil {
			t.Fatal(err)
		}
	}
	if sinkCalls != 0 {
		t.Fatalf("historical replay called sink %d times", sinkCalls)
	}
	event := base
	event.SourceSequence = 7
	event.Message = id.New()
	event.ReceivedAt = uint64(now.Unix())
	if decision, err := runtime.Handle(context.Background(), event, false, []string{"a", "b", "c", "d", "e", "f"}); err != nil || decision.Action != moderation.ActionWarn {
		t.Fatalf("post replay decision = %+v, err %v", decision, err)
	}
	if warnings != 1 {
		t.Fatalf("warning actions = %d, want 1 after replay rebuild", warnings)
	}
}

func TestAutoModerationRuntimeDefaultsToDisabledAndLogOnly(t *testing.T) {
	calls := 0
	runtime := NewAutoModerationRuntime(moderation.Config{}, nil, automodSinkFunc(func(context.Context, ModerationAction) error {
		calls++
		return nil
	}), time.Now)
	body := "x"
	event := native.GroupEvent{Community: id.New(), Group: id.New(), Channel: id.New(), SourceSequence: 1, ReceivedAt: uint64(time.Now().Unix()),
		SenderUser: id.New(), SenderDevice: id.New(), EnvelopeType: 0, Body: &body, Message: id.New()}
	for i := 0; i < 7; i++ {
		event.SourceSequence++
		event.Message = id.New()
		if _, err := runtime.Handle(context.Background(), event, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatalf("disabled runtime called sink %d times", calls)
	}
}

func TestAutoModerationRuntimeClassifiesAuthorityAndSinkFailures(t *testing.T) {
	now := time.Now()
	body := "repeat"
	base := native.GroupEvent{DeliveryID: 1, Community: id.New(), Group: id.New(), Channel: id.New(), SourceSequence: 1,
		ReceivedAt: uint64(now.Unix()), SenderUser: id.New(), SenderDevice: id.New(), Message: id.New(), Body: &body}
	config := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn}}
	t.Run("authority", func(t *testing.T) {
		runtime := NewAutoModerationRuntime(config,
			automodAuthorityFunc(func(context.Context, native.GroupEvent) (bool, bool, error) {
				return false, false, errors.New("private authority details")
			}), automodSinkFunc(func(context.Context, ModerationAction) error { t.Fatal("sink ran after authority failure"); return nil }),
			func() time.Time { return now })
		_, err := runtime.Handle(context.Background(), base, false, []string{"1", "2", "3", "4", "5", "6"})
		var stage AutoModerationStageError
		ok := errors.As(err, &stage)
		if !ok || stage.Stage != "actor_authorization" || strings.Contains(err.Error(), "private") {
			t.Fatalf("authority error = %v, want redacted actor_authorization stage", err)
		}
	})
	t.Run("case sink", func(t *testing.T) {
		runtime := NewAutoModerationRuntime(config, automodAuthorityFunc(ordinaryAutomodAuthority),
			automodSinkFunc(func(context.Context, ModerationAction) error { return errors.New("private sink details") }),
			func() time.Time { return now })
		_, err := runtime.Handle(context.Background(), base, false, []string{"1", "2", "3", "4", "5", "6"})
		var stage AutoModerationStageError
		ok := errors.As(err, &stage)
		if !ok || stage.Stage != "case_sink" || strings.Contains(err.Error(), "private") {
			t.Fatalf("sink error = %v, want redacted case_sink stage", err)
		}
	})
}

func TestAutoModerationRuntimeConfigRevisionFencesInFlightActions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config moderation.Config
	}{
		{name: "disable", config: moderation.Config{}},
		{name: "change punitive rule to log", config: moderation.Config{Enabled: true,
			Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionLog}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			started, release := make(chan struct{}), make(chan struct{})
			var punitive atomic.Int32
			sink := automodSinkFunc(func(_ context.Context, action ModerationAction) error {
				if action.Decision.Action == moderation.ActionWarn || action.Decision.Action == moderation.ActionTimeout || action.Decision.Action == moderation.ActionRemove {
					punitive.Add(1)
					if punitive.Load() == 1 {
						close(started)
						<-release
					}
				}
				return nil
			})
			runtime := NewAutoModerationRuntime(moderation.Config{Enabled: true,
				Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionWarn}},
				automodAuthorityFunc(ordinaryAutomodAuthority), sink, func() time.Time { return now })
			makeEvent := func(sequence byte) native.GroupEvent {
				body := "a message"
				return native.GroupEvent{DeliveryID: uint64(sequence), Community: id.New(), Group: id.New(), Channel: id.New(),
					SourceSequence: uint64(sequence), ReceivedAt: uint64(now.Unix()), SenderUser: id.New(), SenderDevice: id.New(),
					SenderKind: 0, EnvelopeType: 0, Body: &body, Message: id.New()}
			}
			first := makeEvent(1)
			eventDone := make(chan error, 1)
			go func() {
				_, err := runtime.Handle(context.Background(), first, false, []string{"1", "2", "3", "4", "5", "6"})
				eventDone <- err
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("initial punitive action did not reach the sink")
			}
			var persistedRevision atomic.Uint64
			updateDone := make(chan error, 1)
			go func() {
				updateDone <- runtime.PersistConfig(context.Background(), tc.config, 0, func() error {
					persistedRevision.Add(1)
					return nil
				})
			}()
			select {
			case err := <-updateDone:
				t.Fatalf("config save completed while an old action was in flight: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			if persistedRevision.Load() != 0 {
				t.Fatal("config persisted before the old action drained")
			}
			close(release)
			if err := <-eventDone; err != nil {
				t.Fatalf("old event: %v", err)
			}
			if err := <-updateDone; err != nil || persistedRevision.Load() != 1 {
				t.Fatalf("config update revision=%d err=%v", persistedRevision.Load(), err)
			}
			second := makeEvent(2)
			decision, err := runtime.Handle(context.Background(), second, false, []string{"1", "2", "3", "4", "5", "6"})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Action != moderation.ActionLog || punitive.Load() != 1 {
				t.Fatalf("post-write decision=%+v punitive actions=%d", decision, punitive.Load())
			}
		})
	}
}

func TestAutoModerationRuntimeRequiresAuthorityAndSinkWhenEnabled(t *testing.T) {
	now := time.Now()
	body := "hello"
	event := native.GroupEvent{DeliveryID: 1, Community: id.New(), Group: id.New(), Channel: id.New(), SourceSequence: 1,
		ReceivedAt: uint64(now.Unix()), SenderUser: id.New(), SenderDevice: id.New(), Message: id.New(),
		EnvelopeType: 0, Body: &body}
	var calls int
	sink := automodSinkFunc(func(context.Context, ModerationAction) error { calls++; return nil })
	cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleRate: moderation.ActionTimeout}}
	for name, runtime := range map[string]*AutoModerationRuntime{
		"authority absent": NewAutoModerationRuntime(cfg, nil, sink, time.Now),
		"sink absent":      NewAutoModerationRuntime(cfg, automodAuthorityFunc(ordinaryAutomodAuthority), nil, time.Now),
	} {
		t.Run(name, func(t *testing.T) {
			if decision, err := runtime.Handle(context.Background(), event, false, nil); err == nil || !decision.Ignored {
				t.Fatalf("missing dependency decision/error = %+v / %v", decision, err)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("sink calls without required dependencies = %d", calls)
	}
	failingAuthority := automodAuthorityFunc(func(context.Context, native.GroupEvent) (bool, bool, error) {
		return false, false, context.DeadlineExceeded
	})
	runtime := NewAutoModerationRuntime(cfg, failingAuthority, sink, time.Now)
	if decision, err := runtime.Handle(context.Background(), event, false, nil); err == nil || !decision.Ignored {
		t.Fatalf("authority error decision/error = %+v / %v", decision, err)
	}
	if calls != 0 {
		t.Fatalf("sink called after authority lookup failed: %d", calls)
	}
}

func TestAutoModerationRuntimeConsumesDefinitiveRevocationButRetriesUnknownOutcome(t *testing.T) {
	now := time.Now()
	body := "same"
	cfg := moderation.Config{Enabled: true, Actions: map[moderation.Rule]moderation.Action{moderation.RuleRate: moderation.ActionRemove}}
	event := native.GroupEvent{DeliveryID: 1, Community: id.New(), Group: id.New(), Channel: id.New(), SourceSequence: 1,
		ReceivedAt: uint64(now.Unix()), SenderUser: id.New(), SenderDevice: id.New(), Message: id.New(), EnvelopeType: 0, Body: &body}
	sink := automodSinkFunc(func(_ context.Context, a ModerationAction) error {
		if a.Decision.Action == moderation.ActionLog {
			return nil
		}
		return ErrModerationActionRefused
	})
	runtime := NewAutoModerationRuntime(cfg, automodAuthorityFunc(ordinaryAutomodAuthority), sink, func() time.Time { return now })
	for i := 0; i < 6; i++ {
		event.SourceSequence++
		event.Message = id.New()

		decision, err := runtime.Handle(context.Background(), event, false, nil)
		if err != nil {
			t.Fatalf("definitive refusal should be terminal log-only handling: %v", err)
		}
		if i == 5 && !decision.EnforcementRefused {
			t.Fatal("downstream command intake cannot distinguish refused enforcement")
		}

	}
	event.SourceSequence++
	event.Message = id.New()
	sink = automodSinkFunc(func(context.Context, ModerationAction) error { return errors.New("unknown transport outcome") })
	runtime.UpdateActionDependencies(automodAuthorityFunc(ordinaryAutomodAuthority), sink)
	if _, err := runtime.Handle(context.Background(), event, false, nil); err == nil {
		t.Fatal("unknown action outcome must be retried")
	}
}

func TestAutoModerationRuntimeExemptsTrustedOwnersAndAdmins(t *testing.T) {
	now := time.Now()
	body := "hello"
	event := native.GroupEvent{DeliveryID: 1, Community: id.New(), Group: id.New(), Channel: id.New(), SourceSequence: 1,
		ReceivedAt: uint64(now.Unix()), SenderUser: id.New(), SenderDevice: id.New(), Message: id.New(),
		EnvelopeType: 0, Body: &body}
	for _, role := range []string{"owner", "admin"} {
		t.Run(role, func(t *testing.T) {
			calls := 0
			authority := automodAuthorityFunc(func(context.Context, native.GroupEvent) (bool, bool, error) {
				return role == "owner", role == "admin", nil
			})
			runtime := NewAutoModerationRuntime(moderation.Config{Enabled: true,
				Actions: map[moderation.Rule]moderation.Action{moderation.RuleMentions: moderation.ActionTimeout}}, authority,
				automodSinkFunc(func(context.Context, ModerationAction) error { calls++; return nil }), time.Now)
			for i := 0; i < 7; i++ {
				event.SourceSequence++
				event.Message = id.New()
				if _, err := runtime.Handle(context.Background(), event, false, []string{"1", "2", "3", "4", "5", "6"}); err != nil {
					t.Fatal(err)
				}
			}
			if calls != 0 {
				t.Fatalf("privileged actor caused %d sink calls", calls)
			}
		})
	}
}

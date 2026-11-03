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

type groupEventInspectorFunc func(context.Context, id.ID) (native.GroupInfo, error)

func (f groupEventInspectorFunc) InspectGroup(ctx context.Context, group id.ID) (native.GroupInfo, error) {
	return f(ctx, group)
}

type routedGroupEventHandler struct{ calls int }

func (h *routedGroupEventHandler) HandleGroupEvent(context.Context, native.GroupEvent) error {
	h.calls++
	return nil
}

type routedModerationRuntime struct {
	calls    int
	replays  []bool
	mentions [][]string
}

func (r *routedModerationRuntime) Handle(_ context.Context, _ native.GroupEvent, replay bool, mentions []string) (moderation.Decision, error) {
	r.calls++
	r.replays = append(r.replays, replay)
	r.mentions = append(r.mentions, append([]string(nil), mentions...))
	return moderation.Decision{}, nil
}

type routedModerationAuthorityFunc func(context.Context, native.GroupEvent) (bool, bool, error)

func (f routedModerationAuthorityFunc) ModerationActorRoles(ctx context.Context, event native.GroupEvent) (bool, bool, error) {
	return f(ctx, event)
}

type routedModerationSinkFunc func(context.Context, service.ModerationAction) error

func (f routedModerationSinkFunc) ApplyModerationAction(ctx context.Context, action service.ModerationAction) error {
	return f(ctx, action)
}

type routedEventSource struct {
	binding native.Binding
	page    native.GroupEventPage
	calls   int
}

func (s *routedEventSource) Binding() native.Binding { return s.binding }
func (s *routedEventSource) ReadGroupEvents(_ context.Context, after, limit uint64) (native.GroupEventPage, error) {
	if after != 0 || limit != 1 {
		return native.GroupEventPage{}, errors.New("unexpected replay boundary read")
	}
	s.calls++
	return s.page, nil
}
func (*routedEventSource) AcknowledgeGroupEvents(context.Context, uint64) error { return nil }

func TestGroupEventRouterRoutesTextAndEditEventsAndMarksStartupBacklogReplay(t *testing.T) {
	group := id.New()
	inspector := groupEventInspectorFunc(func(_ context.Context, got id.ID) (native.GroupInfo, error) {
		if got != group {
			t.Fatalf("inspected group %s, want %s", got, group)
		}
		return native.GroupInfo{ID: group, Kind: 0}, nil
	})
	interactions := &routedGroupEventHandler{}
	automod := &routedModerationRuntime{}
	handler := newGroupEventRouter(inspector, interactions, automod, 5, 0)
	body := "hello <@00000000000000000000000000000001>"
	for _, event := range []native.GroupEvent{
		{DeliveryID: 5, Group: group, EnvelopeType: 0, Body: &body},
		{DeliveryID: 6, Group: group, EnvelopeType: 1, Body: &body},
	} {
		if err := handler.HandleGroupEvent(context.Background(), event); err != nil {
			t.Fatalf("route event type %d: %v", event.EnvelopeType, err)
		}
	}
	if automod.calls != 2 || len(automod.replays) != 2 || !automod.replays[0] || automod.replays[1] {
		t.Fatalf("automod calls=%d replay=%v, want startup event replayed and later event live", automod.calls, automod.replays)
	}
	if interactions.calls != 0 {
		t.Fatalf("text events dispatched as interactions %d times", interactions.calls)
	}
	if len(automod.mentions[0]) != 1 || automod.mentions[0][0] != "00000000000000000000000000000001" {
		t.Fatalf("parsed mention IDs = %v", automod.mentions[0])
	}
}

func TestGroupEventRouterLeavesKindThreeInteractionDispatchUnchanged(t *testing.T) {
	group := id.New()
	inspector := groupEventInspectorFunc(func(context.Context, id.ID) (native.GroupInfo, error) {
		return native.GroupInfo{ID: group, Kind: 3}, nil
	})
	interactions := &routedGroupEventHandler{}
	automod := &routedModerationRuntime{}
	handler := newGroupEventRouter(inspector, interactions, automod, 100, 0)
	if err := handler.HandleGroupEvent(context.Background(), native.GroupEvent{DeliveryID: 1, Group: group, EnvelopeType: 0}); err != nil {
		t.Fatal(err)
	}
	if interactions.calls != 1 || automod.calls != 0 {
		t.Fatalf("interaction calls=%d automod calls=%d", interactions.calls, automod.calls)
	}
}

func TestGroupEventRouterHonorsCancellationBeforeGroupInspection(t *testing.T) {
	group := id.New()
	inspected := false
	inspector := groupEventInspectorFunc(func(context.Context, id.ID) (native.GroupInfo, error) {
		inspected = true
		return native.GroupInfo{ID: group, Kind: 0}, nil
	})
	automod := &routedModerationRuntime{}
	handler := newGroupEventRouter(inspector, &routedGroupEventHandler{}, automod, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := handler.HandleGroupEvent(ctx, native.GroupEvent{DeliveryID: 1, Group: group, EnvelopeType: 0})
	if !errors.Is(err, context.Canceled) || inspected || automod.calls != 0 {
		t.Fatalf("cancelled event inspected=%v automod_calls=%d err=%v", inspected, automod.calls, err)
	}
}

func TestGroupEventRouterNeverPunishesStartupBacklog(t *testing.T) {
	group, community, channel, user := id.New(), id.New(), id.New(), id.New()
	inspector := groupEventInspectorFunc(func(context.Context, id.ID) (native.GroupInfo, error) {
		return native.GroupInfo{ID: group, Kind: 0}, nil
	})
	var actions int
	automod := service.NewAutoModerationRuntime(moderation.Config{Enabled: true,
		Actions: map[moderation.Rule]moderation.Action{moderation.RuleRate: moderation.ActionRemove}},
		routedModerationAuthorityFunc(func(context.Context, native.GroupEvent) (bool, bool, error) { return false, false, nil }), routedModerationSinkFunc(func(context.Context, service.ModerationAction) error {
			actions++
			return nil
		}), func() time.Time { return time.Unix(1_800_000_100, 0) })
	handler := newGroupEventRouter(inspector, &routedGroupEventHandler{}, automod, 6, uint64(time.UnixMilli(1_800_000_000_000).UnixMilli()))
	body := "same"
	for i := 1; i <= 6; i++ {
		event := native.GroupEvent{DeliveryID: uint64(i), Community: community, Group: group, Channel: channel,
			SourceSequence: uint64(i), ReceivedAt: uint64(time.Unix(1_800_000_000, 0).Add(time.Duration(i) * time.Second).Unix()),
			SenderUser: user, SenderDevice: id.New(), SenderKind: 0, SenderTier: 0, EnvelopeType: 0, Body: &body, Message: id.New()}
		if err := handler.HandleGroupEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if actions != 0 {
		t.Fatalf("startup backlog caused %d punitive actions", actions)
	}
}

func TestGroupEventRouterTreatsDelayedPreActivationAndFutureEventsAsReplay(t *testing.T) {
	group := id.New()
	inspector := groupEventInspectorFunc(func(context.Context, id.ID) (native.GroupInfo, error) {
		return native.GroupInfo{ID: group, Kind: 0}, nil
	})
	automod := &routedModerationRuntime{}
	activation := uint64(time.Now().Add(-time.Minute).UnixMilli())
	handler := newGroupEventRouter(inspector, &routedGroupEventHandler{}, automod, 10, activation)
	body := "late synced message"
	for _, received := range []uint64{activation/1000 - 1, uint64(time.Now().Add(time.Minute).Unix())} {
		if err := handler.HandleGroupEvent(context.Background(), native.GroupEvent{DeliveryID: 100, Group: group, EnvelopeType: 0, ReceivedAt: received, Body: &body}); err != nil {
			t.Fatal(err)
		}
	}
	if len(automod.replays) != 2 || !automod.replays[0] || !automod.replays[1] {
		t.Fatalf("replay flags = %v, want delayed and future events treated as replay", automod.replays)
	}
}

func TestGroupEventRouterNormalizesNativeReceiptSecondsBeforeReplayFencing(t *testing.T) {
	group := id.New()
	inspector := groupEventInspectorFunc(func(context.Context, id.ID) (native.GroupInfo, error) {
		return native.GroupInfo{ID: group, Kind: 0}, nil
	})
	automod := &routedModerationRuntime{}
	now := time.Now()
	activation := uint64(now.Add(-time.Second).UnixMilli())
	handler := newGroupEventRouter(inspector, &routedGroupEventHandler{}, automod, 10, activation)
	body := "new live message"
	if err := handler.HandleGroupEvent(context.Background(), native.GroupEvent{DeliveryID: 11, Group: group,
		EnvelopeType: 0, ReceivedAt: uint64(now.Unix()), Body: &body}); err != nil {
		t.Fatal(err)
	}
	if len(automod.replays) != 1 || automod.replays[0] {
		t.Fatalf("native seconds timestamp replay flags = %v, want one live event", automod.replays)
	}
}

func TestAutoModerationEventStateKeepsStartupBoundaryAndResetsOnConfigRevision(t *testing.T) {
	installation, community := id.New(), id.New()
	source := &routedEventSource{binding: native.Binding{Instance: id.New(), Installation: installation, Community: community},
		page: native.GroupEventPage{Next: 4, HighWater: 9}}
	state := newAutoModerationEventState()
	cfg := moderation.Config{}
	first, firstBoundary, firstReceivedBoundary, err := state.Configure(context.Background(), source, installation, community, 1, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source.page = native.GroupEventPage{Next: 12, HighWater: 14}
	second, secondBoundary, secondReceivedBoundary, err := state.Configure(context.Background(), source, installation, community, 1, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || firstBoundary != 9 || secondBoundary != 9 || firstReceivedBoundary == 0 || secondReceivedBoundary != firstReceivedBoundary || source.calls != 1 {
		t.Fatalf("same revision reset runtime or boundary: same=%v delivery=%d,%d receipt=%d,%d reads=%d", first == second, firstBoundary, secondBoundary, firstReceivedBoundary, secondReceivedBoundary, source.calls)
	}
	third, thirdBoundary, thirdReceivedBoundary, err := state.Configure(context.Background(), source, installation, community, 2, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if third == first || thirdBoundary != 14 || thirdReceivedBoundary < firstReceivedBoundary || source.calls != 2 {
		t.Fatalf("new revision did not start a fresh replay boundary: same=%v delivery=%d receipt=%d reads=%d", third == first, thirdBoundary, thirdReceivedBoundary, source.calls)
	}
}

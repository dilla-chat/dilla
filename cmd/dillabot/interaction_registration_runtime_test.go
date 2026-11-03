//go:build linux

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

func TestInteractionRegistrationRuntimeUsesStableMemberJoinContext(t *testing.T) {
	var gotUser, gotEpisode id.ID
	calls := 0
	installation := id.New()
	user, episode := id.New(), id.New()
	handler := interactionRegistrationEventHandler(installation, func(_ context.Context, got id.ID, gotEpisodeValue id.ID) error {
		calls++
		gotUser, gotEpisode = got, gotEpisodeValue
		return nil
	})
	eventID := id.New()
	community := id.New()
	event := protocol.Event{ID: eventID, CommunityID: community, Kind: protocol.EventMemberJoin, Data: []any{user, episode}}
	if err := handler(context.Background(), event); err != nil {
		t.Fatalf("first event: %v", err)
	}
	if calls != 1 || gotUser != user || gotEpisode != episode {
		t.Fatalf("member join did not stage the producer's exact user and episode: calls=%d user=%s episode=%s", calls, gotUser, gotEpisode)
	}
}

func TestInteractionRegistrationRuntimeSkipsNonJoinEventsAndExpectedDenial(t *testing.T) {
	calls := 0
	handler := interactionRegistrationEventHandler(id.New(), func(context.Context, id.ID, id.ID) error {
		calls++
		return nil
	})
	if err := handler(context.Background(), protocol.Event{ID: id.New(), CommunityID: id.New(), Kind: protocol.EventMemberLeave, Data: []any{id.New(), id.New()}}); err != nil {
		t.Fatalf("leave event: %v", err)
	}
	if err := handler(context.Background(), protocol.Event{ID: id.New(), CommunityID: id.New(), Kind: protocol.EventMemberJoin, Data: []any{id.New(), id.New()}}); err != nil {
		t.Fatalf("expected ineligible member refusal: %v", err)
	}
	if calls != 1 {
		t.Fatalf("registration called %d times, want only for member join", calls)
	}
}

func TestInteractionRegistrationRuntimePropagatesOperationalFailure(t *testing.T) {
	want := errors.New("snapshot unavailable")
	handler := interactionRegistrationEventHandler(id.New(), func(context.Context, id.ID, id.ID) error { return want })
	if err := handler(context.Background(), protocol.Event{ID: id.New(), CommunityID: id.New(), Kind: protocol.EventMemberJoin, Data: []any{id.New(), id.New()}}); !errors.Is(err, want) {
		t.Fatalf("handler error = %v, want %v", err, want)
	}
}

type runtimeEpisodeResolver struct {
	episode id.ID
	present bool
	err     error
}

func (r runtimeEpisodeResolver) ResolveCurrentEpisode(context.Context, id.ID) (id.ID, bool, error) {
	return r.episode, r.present, r.err
}

func TestInteractionRegistrationRuntimeStagesCurrentEpisodeForDeviceListChange(t *testing.T) {
	user, episode := id.New(), id.New()
	var gotUser, gotEpisode id.ID
	calls := 0
	handler := interactionRegistrationDeviceListEventHandler(id.New(), runtimeEpisodeResolver{episode: episode, present: true},
		func(_ context.Context, user, episode id.ID) error {
			calls++
			gotUser, gotEpisode = user, episode
			return nil
		})
	event := protocol.Event{ID: id.New(), CommunityID: id.New(), Kind: protocol.EventDeviceList, Data: []any{user, uint64(2)}}
	if err := handler(context.Background(), event); err != nil {
		t.Fatalf("device-list event: %v", err)
	}
	if calls != 1 || gotUser != user || gotEpisode != episode {
		t.Fatalf("device-list event staged calls=%d user=%s episode=%s; want current episode for %s", calls, gotUser, gotEpisode, user)
	}
}

func TestInteractionRegistrationRuntimeSkipsDeviceListChangeWithoutCurrentMembership(t *testing.T) {
	calls := 0
	handler := interactionRegistrationDeviceListEventHandler(id.New(), runtimeEpisodeResolver{present: false},
		func(context.Context, id.ID, id.ID) error { calls++; return nil })
	event := protocol.Event{ID: id.New(), CommunityID: id.New(), Kind: protocol.EventDeviceList, Data: []any{id.New(), uint64(2)}}
	if err := handler(context.Background(), event); err != nil {
		t.Fatalf("device-list event for nonmember: %v", err)
	}
	if calls != 0 {
		t.Fatalf("staged %d jobs for a user without a current community membership", calls)
	}
}

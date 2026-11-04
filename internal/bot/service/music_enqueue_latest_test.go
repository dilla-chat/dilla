package service

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

func latestEnqueueFixture(t *testing.T) (MusicInteractions, *musicInteractionStoreFake, *musicInteractionResolverFake, protocol.Request) {
	t.Helper()
	channel, community := id.New(), id.New()
	state := music.InitialState()
	state.Revision = 2
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state, Revision: 2}}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "new"}}}
	h := MusicInteractions{Store: store, Resolver: resolver, Authority: &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}, CommunityID: community}
	req := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(1), Args: []any{"https://youtu.be/abcdefghijk"}}
	return h, store, resolver, req
}
func TestEnqueueWithAStaleRevisionAppendsToTheLatestQueue(t *testing.T) {
	h, store, resolver, req := latestEnqueueFixture(t)
	h.CurrentState = func(context.Context, id.ID, id.ID, id.ID) (music.State, uint64, error) {
		s := music.InitialState()
		s.Revision = 1
		return s, 1, nil
	}
	resolver.onResolve = func() {
		store.snapshot.State.Queue = []music.Track{{ID: "prior", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "prior", RequesterID: "member"}}
	}
	result, err := h.handle(t.Context(), InteractionActor{User: id.New()}, req, protocol.OpMusicEnqueue)
	if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 3 || len(store.snapshot.State.Queue) != 2 || store.snapshot.State.Queue[0].ID != "prior" || store.snapshot.State.Queue[1].Title != "new" {
		t.Fatalf("result=%+v queue=%+v err=%v", result, store.snapshot.State.Queue, err)
	}
}
func TestEnqueueAtTheQueueLimitIsRefusedEvenWithAStaleRevision(t *testing.T) {
	h, store, resolver, req := latestEnqueueFixture(t)
	resolver.onResolve = func() {
		for i := range music.QueueLimit {
			store.snapshot.State.Queue = append(store.snapshot.State.Queue, music.Track{ID: fmt.Sprint(i), Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "prior", RequesterID: "member"})
		}
	}
	result, err := h.handle(t.Context(), InteractionActor{User: id.New()}, req, protocol.OpMusicEnqueue)
	if err != nil || result.Status != interactionStatusUnavailable || store.saves != 0 || len(store.snapshot.State.Queue) != music.QueueLimit {
		t.Fatalf("result=%+v saves=%d queue=%d err=%v", result, store.saves, len(store.snapshot.State.Queue), err)
	}
}
func TestConflictReasonsAreDistinct(t *testing.T) {
	for _, reason := range []string{"runtime_revision_mismatch", "client_revision_stale"} {
		t.Run(reason, func(t *testing.T) {
			h, _, _, req := latestEnqueueFixture(t)
			var logs bytes.Buffer
			h.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			if reason == "runtime_revision_mismatch" {
				h.CurrentState = func(context.Context, id.ID, id.ID, id.ID) (music.State, uint64, error) {
					return music.InitialState(), 0, nil
				}
			}
			req.Args = []any{uint64(50)}
			result, err := h.handle(t.Context(), InteractionActor{User: id.New()}, req, protocol.OpMusicVolume)
			if err != nil || result.Status != interactionStatusConflict || !strings.Contains(logs.String(), `"reason":"`+reason+`"`) {
				t.Fatalf("result=%+v err=%v logs=%s", result, err, logs.String())
			}
		})
	}
}

type enqueueConflictStore struct {
	*musicInteractionStoreFake
	failures, attempts int
}

func (s *enqueueConflictStore) Save(ctx context.Context, ch id.ID, rev uint64, state music.State) (persistence.Snapshot, error) {
	s.attempts++
	if s.attempts <= s.failures {
		s.snapshot.State.Revision++
		s.snapshot.Revision++
		s.snapshot.State.Queue = append(s.snapshot.State.Queue, music.Track{ID: fmt.Sprint(s.attempts), Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "concurrent", RequesterID: "member"})
		return persistence.Snapshot{}, persistence.ErrConflict
	}
	return s.musicInteractionStoreFake.Save(ctx, ch, rev, state)
}
func TestEnqueueCompareAndSaveRetriesAreBounded(t *testing.T) {
	for _, failures := range []int{2, 3} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			h, store, resolver, req := latestEnqueueFixture(t)
			conflicts := &enqueueConflictStore{musicInteractionStoreFake: store, failures: failures}
			h.Store = conflicts
			result, err := h.handle(t.Context(), InteractionActor{User: id.New()}, req, protocol.OpMusicEnqueue)
			if err != nil || conflicts.attempts != 3 || len(resolver.inputs) != 1 {
				t.Fatalf("result=%+v attempts=%d resolves=%d err=%v", result, conflicts.attempts, len(resolver.inputs), err)
			}
			if failures == 2 {
				if result.Status != 0 || len(store.snapshot.State.Queue) != 3 || store.snapshot.State.Queue[2].Title != "new" {
					t.Fatalf("lost concurrent append: %+v %+v", result, store.snapshot.State.Queue)
				}
			} else if result.Status != interactionStatusConflict || store.saves != 0 {
				t.Fatalf("unbounded retry: %+v", result)
			}
		})
	}
}

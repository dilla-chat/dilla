package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

// musicItemIDFor is the v0.1.0-bot2.3 derivation, written out independently of the handler: the first 16 bytes of
// SHA-256("dilla/music-item/v1" ‖ request id ‖ index as 4-byte big-endian), as 32 lowercase hex characters.
func musicItemIDFor(request id.ID, index uint32) string {
	sum := sha256.New()
	sum.Write([]byte("dilla/music-item/v1"))
	sum.Write(request[:])
	var be [4]byte
	binary.BigEndian.PutUint32(be[:], index)
	sum.Write(be[:])
	return hex.EncodeToString(sum.Sum(nil)[:16])
}

func musicQueueEntries(state music.State) []string {
	var ids []string
	if state.Current != nil {
		ids = append(ids, state.Current.ID)
	}
	for _, track := range state.Queue {
		ids = append(ids, track.ID)
	}
	return ids
}

// TestPlayEnqueueIdempotentPerRequest: a /play's enqueue commits and its track starts, then the helper dies and the
// answer is never sealed. The same request handled again (a replay after the respawn) adds nothing, starts nothing
// and answers playing.
func TestPlayEnqueueIdempotentPerRequest(t *testing.T) {
	channel, community, requestID := interactionID(131), interactionID(132), interactionID(133)
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"}}}
	plays := 0
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver,
		OnPlay: func(_ context.Context, _, _, _ id.ID, _ music.State, revision uint64) (uint64, error) {
			plays++
			return revision, nil
		}}
	request := protocol.Request{RequestID: requestID, CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"one song"}}
	actor := InteractionActor{User: interactionID(134)}
	if _, err := h.handle(withSealedPlay(context.Background()), actor, request, protocol.OpMusicEnqueue); err != nil {
		t.Fatal(err)
	}
	if entries := musicQueueEntries(store.snapshot.State); len(entries) != 1 {
		t.Fatalf("after the first /play the queue holds %d entries, want 1", len(entries))
	}
	savesBefore, resolvesBefore := store.saves, len(resolver.inputs)
	result, err := h.handle(withSealedPlay(context.Background()), actor, request, protocol.OpMusicEnqueue)
	if err != nil || result.Status != 0 {
		t.Fatalf("replayed /play result=%+v err=%v, want success (playing)", result, err)
	}
	entries := musicQueueEntries(store.snapshot.State)
	if len(entries) != 1 || entries[0] != musicItemIDFor(requestID, 0) {
		t.Fatalf("after the replay the queue holds %q, want only the request's derived item", entries)
	}
	if store.saves != savesBefore || plays != 1 || len(resolver.inputs) != resolvesBefore {
		t.Fatalf("replay saves=%d (before %d) plays=%d resolves=%d, want no new commit, start or resolve", store.saves, savesBefore, plays, len(resolver.inputs)-resolvesBefore)
	}
	if result.Revision == nil || *result.Revision != store.snapshot.Revision {
		t.Fatalf("replay revision=%v, want the current revision %d", result.Revision, store.snapshot.Revision)
	}
	if result.Note != nowPlayingText(store.snapshot.State) {
		t.Fatalf("replay note=%q, want the now-playing answer", result.Note)
	}
}

// TestPlayReplayStartsUnstartedItem (fix round 1): the first /play commits its enqueue, then the helper dies before
// the start, so the item is current but never started. The replay completes the /play: it starts that item once, as
// a fresh /play does after it appends, and enqueues nothing.
func TestPlayReplayStartsUnstartedItem(t *testing.T) {
	channel, community, requestID := interactionID(161), interactionID(162), interactionID(163)
	inner := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"}}}
	request := protocol.Request{RequestID: requestID, CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"one song"}}
	actor := InteractionActor{User: interactionID(164)}
	dying := MusicInteractions{Store: inner, Authority: authority, CommunityID: community, Resolver: resolver,
		OnPlay: func(context.Context, id.ID, id.ID, id.ID, music.State, uint64) (uint64, error) {
			return 0, errors.New("helper closed")
		}}
	if _, err := dying.handle(withSealedPlay(context.Background()), actor, request, protocol.OpMusicEnqueue); err != nil {
		t.Fatal(err)
	}
	derived := musicItemIDFor(requestID, 0)
	if entries := musicQueueEntries(inner.snapshot.State); len(entries) != 1 || entries[0] != derived {
		t.Fatalf("after the first /play the queue holds %q, want the derived item", entries)
	}
	// The respawned Bot reads the restart-safe view: nothing plays.
	var started []music.State
	h := MusicInteractions{Store: musicInteractionRestartViewStore{inner}, Authority: authority, CommunityID: community, Resolver: resolver,
		OnPlay: func(_ context.Context, _, _, _ id.ID, state music.State, revision uint64) (uint64, error) {
			started = append(started, state)
			return revision, nil
		}}
	resolvesBefore := len(resolver.inputs)
	result, err := h.handle(withSealedPlay(context.Background()), actor, request, protocol.OpMusicEnqueue)
	if err != nil || result.Status != 0 {
		t.Fatalf("replayed /play result=%+v err=%v, want success", result, err)
	}
	if len(started) != 1 || started[0].Current == nil || started[0].Current.ID != derived || started[0].Paused {
		t.Fatalf("replay started %d times (%+v), want the request's item started once", len(started), started)
	}
	if entries := musicQueueEntries(inner.snapshot.State); len(entries) != 1 || entries[0] != derived {
		t.Fatalf("after the replay the queue holds %q, want only the derived item", entries)
	}
	if len(resolver.inputs) != resolvesBefore {
		t.Fatalf("replay resolved %d times, want none", len(resolver.inputs)-resolvesBefore)
	}
	if result.Note != nowPlayingText(started[0]) {
		t.Fatalf("replay note=%q, want the now-playing answer", result.Note)
	}
}

// TestPlayReplayUnderUserPauseMatchesFreshPlay (fix round 1): the original /play queued its item behind a track, and
// the user then paused. A fresh /play appends last and resumes; the replay does the same post-enqueue step (resume)
// without appending.
func TestPlayReplayUnderUserPauseMatchesFreshPlay(t *testing.T) {
	channel, community, requestID := interactionID(171), interactionID(172), interactionID(173)
	playing := music.Track{ID: "playing-item", Provider: music.ProviderYouTube, CanonicalID: "zyxwvutsrqp", Title: "Playing", RequesterID: "member"}
	ours := music.Track{ID: musicItemIDFor(requestID, 0), Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One", RequesterID: "member"}
	state := music.InitialState()
	state.Current, state.Queue, state.Paused, state.Revision = &playing, []music.Track{ours}, true, 4
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"}}}
	run := func(request protocol.Request) (music.State, []music.State) {
		store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state, Revision: state.Revision}}
		var started []music.State
		h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver,
			OnPlay: func(_ context.Context, _, _, _ id.ID, s music.State, revision uint64) (uint64, error) {
				started = append(started, s)
				return revision, nil
			}}
		result, err := h.handle(withSealedPlay(context.Background()), InteractionActor{User: interactionID(174)}, request, protocol.OpMusicEnqueue)
		if err != nil || result.Status != 0 {
			t.Fatalf("/play result=%+v err=%v, want success", result, err)
		}
		return store.snapshot.State, started
	}
	fresh, freshStarted := run(protocol.Request{RequestID: interactionID(175), CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"one song"}})
	replay, replayStarted := run(protocol.Request{RequestID: requestID, CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"one song"}})
	if fresh.Paused || len(freshStarted) != 1 || fresh.Current == nil || fresh.Current.ID != playing.ID || len(fresh.Queue) != 2 {
		t.Fatalf("fresh /play state=%+v starts=%d, want the paused track resumed and the new track last", fresh, len(freshStarted))
	}
	if replay.Paused || len(replayStarted) != 1 || replay.Current == nil || replay.Current.ID != fresh.Current.ID {
		t.Fatalf("replayed /play state=%+v starts=%d, want the same resume as a fresh /play", replay, len(replayStarted))
	}
	if entries := musicQueueEntries(replay); strings.Join(entries, ",") != playing.ID+","+ours.ID {
		t.Fatalf("replayed /play queue=%q, want nothing appended", entries)
	}
}

// musicConflictOnceStore commits a concurrent write on the first Save, so the handler's CAS conflicts and re-applies.
type musicConflictOnceStore struct {
	*musicInteractionStoreFake
	concurrent func(music.State) music.State
	conflicted bool
}

func (s *musicConflictOnceStore) Save(ctx context.Context, channel id.ID, expected uint64, state music.State) (persistence.Snapshot, error) {
	if !s.conflicted {
		s.conflicted = true
		next := s.concurrent(s.snapshot.State)
		next.Revision = s.snapshot.Revision + 1
		s.snapshot = persistence.Snapshot{State: next, Revision: next.Revision}
		return persistence.Snapshot{}, persistence.ErrConflict
	}
	return s.musicInteractionStoreFake.Save(ctx, channel, expected, state)
}

// TestPlayConflictRetryKeepsDerivedIDs: the CAS-conflict re-apply uses the same derived item ids, and a concurrent
// commit of this same request is seen as already applied.
func TestPlayConflictRetryKeepsDerivedIDs(t *testing.T) {
	channel, community, requestID := interactionID(141), interactionID(142), interactionID(143)
	tracks := []music.Track{
		{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"},
		{ID: "yt:12345678901", Provider: music.ProviderYouTube, CanonicalID: "12345678901", Title: "Two"},
	}
	other := music.Track{ID: "other-item", Provider: music.ProviderYouTube, CanonicalID: "zyxwvutsrqp", Title: "Other", RequesterID: "member"}
	for _, concurrent := range []string{"other_request", "same_request"} {
		t.Run(concurrent, func(t *testing.T) {
			store := &musicConflictOnceStore{musicInteractionStoreFake: &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}}
			store.concurrent = func(state music.State) music.State {
				if concurrent == "other_request" {
					state.Queue = append(state.Queue, other)
					return state
				}
				for i, track := range tracks {
					track.ID, track.RequesterID = musicItemIDFor(requestID, uint32(i)), "member"
					state.Queue = append(state.Queue, track)
				}
				return state
			}
			authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
			h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: &musicInteractionResolverFake{tracks: tracks}}
			request := protocol.Request{RequestID: requestID, CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"a playlist"}}
			result, err := h.handle(context.Background(), InteractionActor{User: interactionID(144)}, request, protocol.OpMusicEnqueue)
			if err != nil || result.Status != 0 || !store.conflicted {
				t.Fatalf("enqueue after a conflict result=%+v err=%v conflicted=%v, want success after the re-apply", result, err, store.conflicted)
			}
			want := []string{musicItemIDFor(requestID, 0), musicItemIDFor(requestID, 1)}
			if concurrent == "other_request" {
				want = append([]string{other.ID}, want...)
			}
			if got := musicQueueEntries(store.snapshot.State); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("queue after the re-apply=%q, want %q", got, want)
			}
		})
	}
}

// TestDistinctRequestsStillQueueTwice: two /plays with different request ids for the same query are two user actions.
func TestDistinctRequestsStillQueueTwice(t *testing.T) {
	channel, community := interactionID(151), interactionID(152)
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"}}}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver}
	actor := InteractionActor{User: interactionID(154)}
	first, second := interactionID(155), interactionID(156)
	for _, requestID := range []id.ID{first, second} {
		request := protocol.Request{RequestID: requestID, CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"one song"}}
		if result, err := h.handle(context.Background(), actor, request, protocol.OpMusicEnqueue); err != nil || result.Status != 0 {
			t.Fatalf("enqueue result=%+v err=%v, want success", result, err)
		}
	}
	want := []string{musicItemIDFor(first, 0), musicItemIDFor(second, 0)}
	if got := musicQueueEntries(store.snapshot.State); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("queue=%q, want two entries %q", got, want)
	}
}

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
// SHA-256("dilla/music-item/v1" ‖ sender user id ‖ sender device id ‖ request id ‖ index as 4-byte big-endian), as 32
// lowercase hex characters.
func musicItemIDFor(sender InteractionActor, request id.ID, index uint32) string {
	sum := sha256.New()
	sum.Write([]byte("dilla/music-item/v1"))
	sum.Write(sender.User[:])
	sum.Write(sender.Device[:])
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
	if len(entries) != 1 || entries[0] != musicItemIDFor(actor, requestID, 0) {
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
	derived := musicItemIDFor(actor, requestID, 0)
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
	actor := InteractionActor{User: interactionID(174), Device: interactionID(176)}
	playing := music.Track{ID: "playing-item", Provider: music.ProviderYouTube, CanonicalID: "zyxwvutsrqp", Title: "Playing", RequesterID: "member"}
	ours := music.Track{ID: musicItemIDFor(actor, requestID, 0), Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One", RequesterID: "member"}
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
		result, err := h.handle(withSealedPlay(context.Background()), actor, request, protocol.OpMusicEnqueue)
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
	actor := InteractionActor{User: interactionID(144), Device: interactionID(146)}
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
					track.ID, track.RequesterID = musicItemIDFor(actor, requestID, uint32(i)), "member"
					state.Queue = append(state.Queue, track)
				}
				return state
			}
			authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
			h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: &musicInteractionResolverFake{tracks: tracks}}
			request := protocol.Request{RequestID: requestID, CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"a playlist"}}
			result, err := h.handle(context.Background(), actor, request, protocol.OpMusicEnqueue)
			if err != nil || result.Status != 0 || !store.conflicted {
				t.Fatalf("enqueue after a conflict result=%+v err=%v conflicted=%v, want success after the re-apply", result, err, store.conflicted)
			}
			want := []string{musicItemIDFor(actor, requestID, 0), musicItemIDFor(actor, requestID, 1)}
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
	want := []string{musicItemIDFor(actor, first, 0), musicItemIDFor(actor, second, 0)}
	if got := musicQueueEntries(store.snapshot.State); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("queue=%q, want two entries %q", got, want)
	}
}

// TestMusicItemIDIsSenderScoped (fix round 2): the same request id and index from two senders derive two item ids,
// so one sender's request id can never collide with another's queue item.
func TestMusicItemIDIsSenderScoped(t *testing.T) {
	channel, community, requestID := interactionID(181), interactionID(182), interactionID(183)
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"}}}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver}
	senders := []InteractionActor{{User: interactionID(184), Device: interactionID(185)}, {User: interactionID(184), Device: interactionID(186)}, {User: interactionID(187), Device: interactionID(185)}}
	var want []string
	for _, sender := range senders {
		request := protocol.Request{RequestID: requestID, CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"one song"}}
		if result, err := h.handle(context.Background(), sender, request, protocol.OpMusicEnqueue); err != nil || result.Status != 0 {
			t.Fatalf("enqueue result=%+v err=%v, want success", result, err)
		}
		want = append(want, musicItemIDFor(sender, requestID, 0))
	}
	if want[0] == want[1] || want[0] == want[2] || want[1] == want[2] {
		t.Fatalf("derived ids %q, want one per sender", want)
	}
	if got := musicQueueEntries(store.snapshot.State); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("queue=%q, want one entry per sender %q", got, want)
	}
}

// TestPlaylistReplayAfterFirstItemPlayedOut (fix round 2): item 0 of a playlist /play has played out, items 1..n-1
// are current or queued. The replay is already applied: no resolve, no append, no unavailable answer.
func TestPlaylistReplayAfterFirstItemPlayedOut(t *testing.T) {
	channel, community, requestID := interactionID(191), interactionID(192), interactionID(193)
	actor := InteractionActor{User: interactionID(194), Device: interactionID(195)}
	track := func(i uint32, canonical string) music.Track {
		return music.Track{ID: musicItemIDFor(actor, requestID, i), Provider: music.ProviderYouTube, CanonicalID: canonical, Title: "T", RequesterID: actor.User.String()}
	}
	one, two := track(1, "12345678901"), track(2, "abcdefghijk")
	state := music.InitialState()
	state.Current, state.Queue, state.Paused, state.Revision = &one, []music.Track{two}, false, 7
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state, Revision: state.Revision}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{
		{ID: "yt:zyxwvutsrqp", Provider: music.ProviderYouTube, CanonicalID: "zyxwvutsrqp", Title: "Zero"},
		{ID: "yt:12345678901", Provider: music.ProviderYouTube, CanonicalID: "12345678901", Title: "One"},
		{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Two"},
	}}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver}
	request := protocol.Request{RequestID: requestID, CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"a playlist"}}
	result, err := h.handle(withSealedPlay(context.Background()), actor, request, protocol.OpMusicEnqueue)
	if err != nil || result.Status != 0 {
		t.Fatalf("playlist replay result=%+v err=%v, want already applied", result, err)
	}
	if len(resolver.inputs) != 0 || store.saves != 0 {
		t.Fatalf("playlist replay resolves=%d saves=%d, want none", len(resolver.inputs), store.saves)
	}
	if got := musicQueueEntries(store.snapshot.State); strings.Join(got, ",") != one.ID+","+two.ID {
		t.Fatalf("queue=%q, want items 1 and 2 only", got)
	}
}

// TestPlayReplayRecheckKeepsPlayingRoom (fix round 2): a concurrent run of this /play commits its item behind a
// playing track while this run resolves. The per-attempt recheck finds it applied; the room is playing, so the warm is
// not abandoned (the abandon signal follows the state, not the answer's note).
func TestPlayReplayRecheckKeepsPlayingRoom(t *testing.T) {
	channel, community, requestID := interactionID(201), interactionID(202), interactionID(203)
	actor := InteractionActor{User: interactionID(204), Device: interactionID(205)}
	playing := music.Track{ID: "playing-item", Provider: music.ProviderYouTube, CanonicalID: "zyxwvutsrqp", Title: "Playing", RequesterID: "member"}
	store := &musicConflictOnceStore{musicInteractionStoreFake: &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}}
	store.concurrent = func(state music.State) music.State {
		ours := music.Track{ID: musicItemIDFor(actor, requestID, 0), Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One", RequesterID: "member"}
		state.Current, state.Queue, state.Paused = &playing, []music.Track{ours}, false
		return state
	}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"}}}
	abandoned := 0
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver,
		OnPlayOpened:    func(context.Context, id.ID, id.ID, id.ID) {},
		OnPlayAbandoned: func(context.Context, id.ID, id.ID, id.ID) { abandoned++ },
		OnPlay: func(_ context.Context, _, _, _ id.ID, _ music.State, revision uint64) (uint64, error) {
			return revision, nil
		}}
	request := protocol.Request{RequestID: requestID, CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"one song"}}
	result, err := h.handle(withSealedPlay(context.Background()), actor, request, protocol.OpMusicEnqueue)
	if err != nil || result.Status != 0 || !store.conflicted {
		t.Fatalf("result=%+v err=%v conflicted=%v, want already applied after the conflict", result, err, store.conflicted)
	}
	if abandoned != 0 {
		t.Fatalf("abandoned=%d, want the playing room kept", abandoned)
	}
	if got := musicQueueEntries(store.snapshot.State); len(got) != 2 {
		t.Fatalf("queue=%q, want the playing track and the request's item only", got)
	}
}

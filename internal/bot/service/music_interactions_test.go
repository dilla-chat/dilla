package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/music/youtube"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type musicInteractionResolverFake struct {
	tracks    []music.Track
	err       error
	inputs    []string
	onResolve func()
}

type musicSpotifyResolverFake struct {
	tracks     []music.Track
	err        error
	inputs     []string
	requesters []id.ID
}

func (r *musicSpotifyResolverFake) ResolveSpotify(_ context.Context, input string, requester id.ID) ([]music.Track, error) {
	r.inputs = append(r.inputs, input)
	r.requesters = append(r.requesters, requester)
	return append([]music.Track(nil), r.tracks...), r.err
}

func (r *musicInteractionResolverFake) Resolve(_ context.Context, input string) ([]music.Track, error) {
	r.inputs = append(r.inputs, input)
	if r.onResolve != nil {
		r.onResolve()
	}
	return append([]music.Track(nil), r.tracks...), r.err
}

type musicInteractionStoreFake struct {
	snapshot persistence.Snapshot
	saves    int
}

func (s *musicInteractionStoreFake) Load(context.Context, id.ID) (persistence.Snapshot, error) {
	return s.snapshot, nil
}

type musicInteractionRestartViewStore struct{ *musicInteractionStoreFake }

func (s musicInteractionRestartViewStore) Load(_ context.Context, _ id.ID) (persistence.Snapshot, error) {
	snapshot := s.snapshot
	snapshot.State.Paused = true // Match persistence.Store's restart-safe read view.
	return snapshot, nil
}
func (s *musicInteractionStoreFake) Save(_ context.Context, _ id.ID, expected uint64, state music.State) (persistence.Snapshot, error) {
	if expected != s.snapshot.Revision {
		return persistence.Snapshot{}, persistence.ErrConflict
	}
	s.snapshot = persistence.Snapshot{State: state, Revision: state.Revision}
	s.saves++
	return s.snapshot, nil
}

type musicInteractionAuthorityFake struct {
	call           MusicCallBinding
	callAfterFirst *MusicCallBinding
	dj             bool
	djAfterFirst   *bool
	user           bool
	emergency      bool
	callChecks     int
	roleChecks     int
}

func (a *musicInteractionAuthorityFake) CurrentCall(_ context.Context, channel id.ID) (MusicCallBinding, error) {
	a.callChecks++
	if a.callChecks > 1 && a.callAfterFirst != nil {
		a.call = *a.callAfterFirst
	}
	call := a.call
	if call.ChannelID != channel {
		call.ChannelID = channel
	}
	return call, nil
}
func (a *musicInteractionAuthorityFake) CanUse(context.Context, InteractionActor, MusicCallBinding) (bool, error) {
	return a.user, nil
}
func (a *musicInteractionAuthorityFake) CanDJ(context.Context, InteractionActor, MusicCallBinding) (bool, error) {
	a.roleChecks++
	if a.roleChecks > 1 && a.djAfterFirst != nil {
		return *a.djAfterFirst, nil
	}
	return a.dj, nil
}
func (a *musicInteractionAuthorityFake) CanEmergencyStop(context.Context, InteractionActor, MusicCallBinding) (bool, error) {
	return a.emergency, nil
}

func TestMusicInteractionsRecheckDJAndPersistOnlyQueueState(t *testing.T) {
	channel, callID := id.New(), id.New()
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: callID, ChannelID: channel, Active: true}, user: true, dj: true}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: id.New()}
	request := protocol.Request{CommunityID: h.CommunityID, ChannelID: &channel, Revision: uint64Ptr(0)}
	result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicPause)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != 0 || result.Revision == nil || *result.Revision != 1 || store.saves != 1 {
		t.Fatalf("result=%+v saves=%d, want successful revision 1 state save", result, store.saves)
	}
	if !store.snapshot.State.Paused || store.snapshot.State.Revision != 1 {
		t.Fatalf("stored state=%+v, want paused revision 1", store.snapshot.State)
	}
	if authority.callChecks != 2 || authority.roleChecks != 2 {
		t.Fatalf("call checks=%d role checks=%d, want both rechecked before save", authority.callChecks, authority.roleChecks)
	}
}

func TestMusicInteractionsNotifiesSnapshotBridgeOnlyAfterSuccessfulCommit(t *testing.T) {
	channel, community, installation := interactionID(5), interactionID(2), interactionID(3)
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	var commits []uint64
	h := MusicInteractions{Store: store, Authority: authority, InstallationID: installation, CommunityID: community,
		OnPersistedSnapshot: func(_ context.Context, gotInstallation, gotCommunity, gotChannel id.ID, _ music.State, revision uint64, _ bool) (uint64, error) {
			if gotInstallation != installation || gotCommunity != community || gotChannel != channel {
				t.Errorf("bridge context=(%s,%s,%s), want (%s,%s,%s)", gotInstallation, gotCommunity, gotChannel, installation, community, channel)
			}
			if store.snapshot.Revision != revision {
				t.Errorf("bridge revision=%d before store revision=%d", revision, store.snapshot.Revision)
			}
			commits = append(commits, revision)
			return revision, nil
		}}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0)}
	result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicPause)
	if err != nil || result.Status != 0 || len(commits) != 1 || commits[0] != 1 {
		t.Fatalf("successful commit result=%+v err=%v bridge revisions=%v", result, err, commits)
	}
	request.Revision = uint64Ptr(0)
	result, err = h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicResume)
	if err != nil || result.Status != interactionStatusConflict || len(commits) != 1 {
		t.Fatalf("conflicting commit result=%+v err=%v bridge revisions=%v, want no second notification", result, err, commits)
	}
}

func TestMusicInteractionsMarksOnlyCommittedResumeForLiveHandoff(t *testing.T) {
	channel, community := interactionID(15), interactionID(12)
	state, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{
		ID: "track-1", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Track", RequesterID: "member",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state, Revision: state.Revision}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	var startPlayback bool
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community,
		OnPersistedSnapshot: func(_ context.Context, _, _, _ id.ID, _ music.State, revision uint64, start bool) (uint64, error) {
			startPlayback = start
			return revision, nil
		}}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(state.Revision)}
	result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicPause)
	if err != nil || result.Status != 0 || startPlayback {
		t.Fatalf("pause result=%+v err=%v startPlayback=%v", result, err, startPlayback)
	}
	request.Revision = result.Revision
	result, err = h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicResume)
	if err != nil || result.Status != 0 || !startPlayback || result.Revision == nil || *result.Revision != 3 {
		t.Fatalf("resume result=%+v err=%v startPlayback=%v, want committed explicit resume", result, err, startPlayback)
	}
}

func TestMusicInteractionsContinuesActivePlaybackAfterSkipOnly(t *testing.T) {
	channel, community := interactionID(25), interactionID(22)
	makeState := func(paused bool) music.State {
		state, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{
			{ID: "track-1", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "First", RequesterID: "member"},
			{ID: "track-2", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "Second", RequesterID: "member"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		state, err = music.Apply(state, state.Revision, music.Operation{Kind: music.OpResume})
		if err != nil {
			t.Fatal(err)
		}
		if paused {
			state, err = music.Apply(state, state.Revision, music.Operation{Kind: music.OpPause})
			if err != nil {
				t.Fatal(err)
			}
		}
		return state
	}
	for _, tc := range []struct {
		name      string
		paused    bool
		wantStart bool
	}{
		{name: "active playback continues", wantStart: true},
		{name: "paused queue stays paused", paused: true, wantStart: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := makeState(tc.paused)
			store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state, Revision: state.Revision}}
			authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
			var startPlayback bool
			h := MusicInteractions{Store: store, Authority: authority, CommunityID: community,
				CurrentState: func(context.Context, id.ID, id.ID, id.ID) (music.State, uint64, error) {
					return state, state.Revision, nil
				},
				OnPersistedSnapshot: func(_ context.Context, _, _, _ id.ID, committed music.State, revision uint64, shouldStart bool) (uint64, error) {
					if committed.Current == nil || committed.Current.Title != "Second" {
						t.Errorf("committed state after Skip = %+v, want Second as current", committed)
					}
					startPlayback = shouldStart
					return revision, nil
				}}
			request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(state.Revision)}
			result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicSkip)
			if err != nil || result.Status != 0 || startPlayback != tc.wantStart {
				t.Fatalf("skip result=%+v err=%v startPlayback=%v, want successful skip and startPlayback=%v", result, err, startPlayback, tc.wantStart)
			}
		})
	}
}

func TestMusicInteractionsPausesPersistedSkipWhenPlaybackHandoffFails(t *testing.T) {
	channel, community := interactionID(29), interactionID(26)
	state, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{
		{ID: "track-1", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "First", RequesterID: "member"},
		{ID: "track-2", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "Second", RequesterID: "member"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	state, err = music.Apply(state, state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state, Revision: state.Revision}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community,
		CurrentState: func(context.Context, id.ID, id.ID, id.ID) (music.State, uint64, error) {
			return state, state.Revision, nil
		},
		OnPersistedSnapshot: func(_ context.Context, _, _, _ id.ID, _ music.State, _ uint64, startPlayback bool) (uint64, error) {
			if !startPlayback {
				t.Error("active Skip was not marked for playback continuation")
			}
			return 0, errors.New("retained playback session unavailable")
		}}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(state.Revision)}
	result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicSkip)
	if err != nil || result.Status != interactionStatusUnavailable || result.Revision == nil || *result.Revision != state.Revision+2 {
		t.Fatalf("skip result=%+v err=%v, want unavailable after committed compensation pause", result, err)
	}
	if !store.snapshot.State.Paused || store.snapshot.Revision != *result.Revision || store.snapshot.State.Current == nil || store.snapshot.State.Current.Title != "Second" {
		t.Fatalf("failed skip handoff left persisted state=%+v revision=%d, want paused at Second", store.snapshot.State, store.snapshot.Revision)
	}
}

func TestMusicInteractionsPreservesLivePlaybackAcrossQueueMetadataMutations(t *testing.T) {
	channel, community := interactionID(35), interactionID(32)
	state, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{
		ID: "track-1", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Track", RequesterID: "member",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	state, err = music.Apply(state, state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	base := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state, Revision: state.Revision}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	var callbackState music.State
	h := MusicInteractions{Store: musicInteractionRestartViewStore{base}, Authority: authority, CommunityID: community,
		CurrentState: func(context.Context, id.ID, id.ID, id.ID) (music.State, uint64, error) {
			return state, state.Revision, nil
		},
		OnPersistedSnapshot: func(_ context.Context, _, _, _ id.ID, committed music.State, revision uint64, startPlayback bool) (uint64, error) {
			callbackState = committed
			if startPlayback {
				t.Error("volume change was incorrectly marked as Resume")
			}
			return revision, nil
		}}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(state.Revision), Args: []any{uint64(35)}}
	result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicVolume)
	if err != nil || result.Status != 0 || callbackState.Paused || base.snapshot.State.Paused || base.snapshot.State.Volume != 35 {
		t.Fatalf("live volume mutation result=%+v err=%v callback=%+v persisted=%+v", result, err, callbackState, base.snapshot.State)
	}
}

func TestMusicInteractionsRejectsUnsyncedRuntimeStateBeforeMutation(t *testing.T) {
	channel, community := interactionID(45), interactionID(42)
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	callbacks := 0
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community,
		CurrentState: func(context.Context, id.ID, id.ID, id.ID) (music.State, uint64, error) {
			return music.State{Revision: 1, Queue: []music.Track{}, Paused: false}, 1, nil
		},
		OnPersistedSnapshot: func(context.Context, id.ID, id.ID, id.ID, music.State, uint64, bool) (uint64, error) {
			callbacks++
			return 0, nil
		}}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{uint64(35)}}
	result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicVolume)
	if err != nil || result.Status != interactionStatusConflict || store.saves != 0 || callbacks != 0 {
		t.Fatalf("unsynced runtime result=%+v err=%v saves=%d callbacks=%d", result, err, store.saves, callbacks)
	}
}

func TestMusicInteractionsReportsUnavailableAndPersistsPauseWhenResumeHandoffFails(t *testing.T) {
	channel, community := interactionID(25), interactionID(22)
	state, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{
		ID: "track-1", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Track", RequesterID: "member",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state, Revision: state.Revision}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community,
		OnPersistedSnapshot: func(context.Context, id.ID, id.ID, id.ID, music.State, uint64, bool) (uint64, error) {
			return 0, errors.New("provider start failed")
		}}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(state.Revision)}
	result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicResume)
	if err != nil || result.Status != interactionStatusUnavailable || result.Revision == nil || *result.Revision != 3 {
		t.Fatalf("failed resume result=%+v err=%v, want unavailable at paused revision 3", result, err)
	}
	if !store.snapshot.State.Paused || store.snapshot.Revision != 3 {
		t.Fatalf("failed resume persisted state=%+v revision=%d, want compensating pause", store.snapshot.State, store.snapshot.Revision)
	}
}

func TestMusicInteractionsNonDJCannotMutateAndEmergencyStopIsNarrow(t *testing.T) {
	channel := id.New()
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, emergency: true}
	bridgeCalls := 0
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: id.New(), OnPersistedSnapshot: func(context.Context, id.ID, id.ID, id.ID, music.State, uint64, bool) (uint64, error) {
		bridgeCalls++
		return 0, nil
	}}
	request := protocol.Request{CommunityID: h.CommunityID, ChannelID: &channel, Revision: uint64Ptr(0)}
	if got, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicPause); err != nil || got.Status != interactionStatusForbidden {
		t.Fatalf("pause result=%+v err=%v, want forbidden", got, err)
	}
	if got, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicStop); err != nil || got.Status != 0 {
		t.Fatalf("emergency stop result=%+v err=%v, want allowed", got, err)
	}
	if store.saves != 1 {
		t.Fatalf("saves=%d, want only emergency stop persisted", store.saves)
	}
	if bridgeCalls != 1 {
		t.Fatalf("runtime handoffs=%d, want only the authorized stop handoff", bridgeCalls)
	}
}

func TestMusicInteractionsRejectsStaleBindingAndLeavesUnsupportedProvidersUnavailable(t *testing.T) {
	channel := id.New()
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: false}, user: true, dj: true}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: id.New()}
	request := protocol.Request{CommunityID: h.CommunityID, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"search"}}
	if got, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicSearch); err != nil || got.Status != interactionStatusUnavailable {
		t.Fatalf("inactive-call search result=%+v err=%v, want unavailable", got, err)
	}
	authority.call.Active = true
	if got, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicSearch); err != nil || got.Status != interactionStatusUnavailable {
		t.Fatalf("providerless search result=%+v err=%v, want unavailable", got, err)
	}
	if store.saves != 0 {
		t.Fatalf("unsupported operations wrote %d times", store.saves)
	}
}

func TestMusicSearchReturnsResolvedTracksWithoutWritingQueue(t *testing.T) {
	channel, community := interactionID(55), interactionID(52)
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: false}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{
		ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Track", Artist: "Artist", DurationMS: 123000,
	}}}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Args: []any{"search terms"}}
	result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicSearch)
	if err != nil || result.Status != 0 || result.Revision != nil {
		t.Fatalf("search result=%+v err=%v, want success without queue revision", result, err)
	}
	want, err := cborx.Marshal([]any{uint64(1), []any{[]any{"yt:abcdefghijk", "youtube", "abcdefghijk", "Track", "Artist", "", uint64(123000), ""}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Data) != string(want) {
		t.Fatalf("search data=%x, want track tuples %x", result.Data, want)
	}
	if len(resolver.inputs) != 1 || resolver.inputs[0] != "search terms" || store.saves != 0 {
		t.Fatalf("resolver inputs=%v saves=%d, want one lookup and no queue write", resolver.inputs, store.saves)
	}
}

func TestMusicEnqueueImportsResolvedPlaylistAtomicallyWithStableQueueIDs(t *testing.T) {
	channel, community := interactionID(65), interactionID(62)
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{
		{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"},
		{ID: "yt:12345678901", Provider: music.ProviderYouTube, CanonicalID: "12345678901", Title: "Two"},
	}}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"https://www.youtube.com/playlist?list=PLabc_123"}}
	actor := InteractionActor{User: interactionID(68)}
	result, err := h.handle(context.Background(), actor, request, protocol.OpMusicEnqueue)
	if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 1 {
		t.Fatalf("enqueue result=%+v err=%v, want committed revision 1", result, err)
	}
	if len(store.snapshot.State.Queue) != 2 || store.snapshot.State.Queue[0].Title != "One" || store.snapshot.State.Queue[1].Title != "Two" {
		t.Fatalf("stored queue=%+v, want both playlist tracks", store.snapshot.State.Queue)
	}
	for _, queued := range store.snapshot.State.Queue {
		if queued.ID == "yt:abcdefghijk" || queued.ID == "yt:12345678901" || queued.RequesterID != actor.User.String() {
			t.Fatalf("queued descriptor=%+v, want a unique queue ID and requesting actor", queued)
		}
	}
	if store.snapshot.State.Queue[0].ID == store.snapshot.State.Queue[1].ID || authority.callChecks != 2 || authority.roleChecks != 2 {
		t.Fatalf("queue IDs=%q/%q call checks=%d role checks=%d, want unique IDs and final authorization recheck", store.snapshot.State.Queue[0].ID, store.snapshot.State.Queue[1].ID, authority.callChecks, authority.roleChecks)
	}
}

func TestMusicEnqueueImportsSpotifyPlaylistAtomicallyForActor(t *testing.T) {
	channel, community, actorID := interactionID(165), interactionID(162), interactionID(168)
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	input := "https://open.spotify.com/playlist/BBBBBBBBBBBBBBBBBBBBBB"
	resolver := &musicSpotifyResolverFake{tracks: []music.Track{
		{Provider: music.ProviderSpotify, CanonicalID: "spotify:track:0123456789012345678901", Title: "One"},
		{Provider: music.ProviderSpotify, CanonicalID: "spotify:track:1123456789012345678901", Title: "Two"},
	}}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, SpotifyResolver: resolver}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{input}}
	result, err := h.handle(context.Background(), InteractionActor{User: actorID}, request, protocol.OpMusicEnqueue)
	if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 1 {
		t.Fatalf("Spotify enqueue result=%+v err=%v resolver inputs=%v, want committed revision 1", result, err, resolver.inputs)
	}
	if len(resolver.inputs) != 1 || resolver.inputs[0] != input || len(resolver.requesters) != 1 || resolver.requesters[0] != actorID {
		t.Fatalf("Spotify resolver inputs=%v requesters=%v, want one input with actor requester", resolver.inputs, resolver.requesters)
	}
	if store.saves != 1 || len(store.snapshot.State.Queue) != 2 || store.snapshot.State.Queue[0].Provider != music.ProviderSpotify || store.snapshot.State.Queue[1].Provider != music.ProviderSpotify {
		t.Fatalf("saved=%d queue=%+v, want one atomic Spotify playlist import", store.saves, store.snapshot.State.Queue)
	}
	for _, track := range store.snapshot.State.Queue {
		if track.ID == "" || track.RequesterID != actorID.String() {
			t.Fatalf("queued track=%+v, want stable ID and actor requester", track)
		}
	}
}

func TestMusicEnqueueRejectsOversizedSpotifyImportWithoutPartialWrite(t *testing.T) {
	channel, community := interactionID(175), interactionID(172)
	tracks := make([]music.Track, 501)
	for i := range tracks {
		tracks[i] = music.Track{Provider: music.ProviderSpotify, CanonicalID: "spotify:track:0123456789012345678901", Title: "Track"}
	}
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	resolver := &musicSpotifyResolverFake{tracks: tracks}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, SpotifyResolver: resolver}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"spotify:playlist:BBBBBBBBBBBBBBBBBBBBBB"}}
	result, err := h.handle(context.Background(), InteractionActor{User: interactionID(178)}, request, protocol.OpMusicEnqueue)
	if err != nil || result.Status != interactionStatusUnavailable || store.saves != 0 || len(store.snapshot.State.Queue) != 0 {
		t.Fatalf("oversized Spotify enqueue result=%+v err=%v saves=%d queue=%d, want unavailable without partial write", result, err, store.saves, len(store.snapshot.State.Queue))
	}
}

func TestMusicEnqueueRejectsWholePlaylistWhenQueueCapWouldBeExceeded(t *testing.T) {
	channel, community := interactionID(75), interactionID(72)
	state := music.InitialState()
	for i := 0; i < music.QueueLimit; i++ {
		state.Queue = append(state.Queue, music.Track{ID: fmt.Sprintf("existing-%d", i), Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Existing", RequesterID: "member"})
	}
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state, Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	resolver := &musicInteractionResolverFake{tracks: []music.Track{
		{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"},
		{ID: "yt:12345678901", Provider: music.ProviderYouTube, CanonicalID: "12345678901", Title: "Two"},
	}}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"https://www.youtube.com/playlist?list=PLabc_123"}}
	result, err := h.handle(context.Background(), InteractionActor{User: interactionID(78)}, request, protocol.OpMusicEnqueue)
	if err != nil || result.Status != interactionStatusUnavailable || store.saves != 0 || len(store.snapshot.State.Queue) != music.QueueLimit {
		t.Fatalf("overflow enqueue result=%+v err=%v saves=%d queue=%d, want unavailable and no partial import", result, err, store.saves, len(store.snapshot.State.Queue))
	}
}

func TestMusicEnqueueRequiresFinalCallAndDJRecheckAfterResolution(t *testing.T) {
	for _, change := range []string{"call", "dj"} {
		t.Run(change, func(t *testing.T) {
			channel, community := interactionID(85), interactionID(82)
			initialCall := MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}
			authority := &musicInteractionAuthorityFake{call: initialCall, user: true, dj: true}
			if change == "call" {
				changed := initialCall
				changed.CallID = id.New()
				authority.callAfterFirst = &changed
			} else {
				denied := false
				authority.djAfterFirst = &denied
			}
			store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
			resolver := &musicInteractionResolverFake{tracks: []music.Track{{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"}}}
			h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver}
			request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"one song"}}
			result, err := h.handle(context.Background(), InteractionActor{User: interactionID(88)}, request, protocol.OpMusicEnqueue)
			if err != nil || result.Status != interactionStatusConflict && result.Status != interactionStatusForbidden || store.saves != 0 {
				t.Fatalf("enqueue after %s change result=%+v err=%v saves=%d, want refused before persistence", change, result, err, store.saves)
			}
		})
	}
}

func TestMusicEnqueueRejectsExpiredRequestAfterSlowResolution(t *testing.T) {
	channel, community := interactionID(115), interactionID(112)
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	now := time.Unix(1000, 0)
	resolver := &musicInteractionResolverFake{tracks: []music.Track{{ID: "yt:abcdefghijk", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One"}}, onResolve: func() {
		now = time.Unix(1030, 0)
	}}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver, Now: func() time.Time { return now }}
	expires := uint64(1030)
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Expires: &expires, Args: []any{"one song"}}
	result, err := h.handle(context.Background(), InteractionActor{User: interactionID(118)}, request, protocol.OpMusicEnqueue)
	if err != nil || result.Status != interactionStatusExpired || store.saves != 0 || len(store.snapshot.State.Queue) != 0 {
		t.Fatalf("late enqueue result=%+v err=%v saves=%d queue=%d, want expired with no mutation", result, err, store.saves, len(store.snapshot.State.Queue))
	}
}

func TestMusicSearchRefusesSpotifyAndBoundsEncodedResults(t *testing.T) {
	t.Run("Spotify", func(t *testing.T) {
		channel, community := interactionID(95), interactionID(92)
		store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
		authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true}
		resolver := &musicInteractionResolverFake{tracks: []music.Track{{ID: "spotify-item", Provider: music.ProviderSpotify, CanonicalID: "spotify:track:0123456789012345678901", Title: "Track"}}}
		h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver}
		request := protocol.Request{CommunityID: community, ChannelID: &channel, Args: []any{"one song"}}
		result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicSearch)
		if err != nil || result.Status != interactionStatusUnavailable || len(result.Data) != 0 || store.saves != 0 {
			t.Fatalf("Spotify search result=%+v err=%v saves=%d, want unavailable and no queue write", result, err, store.saves)
		}
	})
	t.Run("response limit", func(t *testing.T) {
		channel, community := interactionID(105), interactionID(102)
		store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
		authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true}
		tracks := make([]music.Track, youtube.MaxSearchResults)
		for i := range tracks {
			canonicalID := fmt.Sprintf("a%010d", i)
			tracks[i] = music.Track{ID: "yt:" + canonicalID, Provider: music.ProviderYouTube, CanonicalID: canonicalID, Title: strings.Repeat("x", music.MaxMetadataBytes), Artist: strings.Repeat("y", music.MaxMetadataBytes)}
		}
		resolver := &musicInteractionResolverFake{tracks: tracks}
		h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver}
		request := protocol.Request{CommunityID: community, ChannelID: &channel, Args: []any{"one song"}}
		result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, request, protocol.OpMusicSearch)
		if err != nil || result.Status != 0 || len(result.Data) == 0 || len(result.Data) > 2400 || store.saves != 0 {
			t.Fatalf("bounded search result status=%d bytes=%d err=%v saves=%d, want success within 2400 bytes and no queue write", result.Status, len(result.Data), err, store.saves)
		}
	})
}

func TestMusicStateReportsCurrentDJControlGrant(t *testing.T) {
	channel := id.New()
	for _, test := range []struct {
		name string
		dj   bool
		want uint64
	}{{name: "DJ", dj: true, want: 1}, {name: "listener", dj: false, want: 0}} {
		t.Run(test.name, func(t *testing.T) {
			store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 0}}
			authority := &musicInteractionAuthorityFake{
				call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: test.dj,
			}
			h := MusicInteractions{Store: store, Authority: authority, CommunityID: id.New()}
			result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, protocol.Request{
				CommunityID: h.CommunityID, ChannelID: &channel,
			}, protocol.OpMusicState)
			if err != nil || result.Status != 0 {
				t.Fatalf("state response=%+v err=%v", result, err)
			}
			var snapshot []any
			if err := cborx.Unmarshal(result.Data, &snapshot); err != nil {
				t.Fatalf("decode music snapshot: %v", err)
			}
			if len(snapshot) != 12 {
				t.Fatalf("snapshot fields=%d, want 12 (Ruling AI: position stamp and Bot time)", len(snapshot))
			}
			if at, ok := snapshot[10].(uint64); !ok || at != 0 {
				t.Fatalf("an unstamped state's position time=%#v, want 0", snapshot[10])
			}
			if now, ok := snapshot[11].(uint64); !ok || now == 0 {
				t.Fatalf("the Bot's time=%#v, want its Unix ms", snapshot[11])
			}
			got, ok := snapshot[9].(uint64)
			if !ok || got != test.want || authority.roleChecks != 1 {
				t.Fatalf("control grant=%#v role checks=%d, want %d and one current authorization check", snapshot[9], authority.roleChecks, test.want)
			}
		})
	}
}

func uint64Ptr(v uint64) *uint64 { return &v }

func TestMusicMutationDefersRevisionCheckUntilAuthorization(t *testing.T) {
	channel := interactionID(5)
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState(), Revision: 7}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: false, dj: true}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: interactionID(2)}
	for operation, spec := range h.Handlers() {
		if operation != protocol.OpMusicState && operation != protocol.OpMusicSearch && (spec.CurrentRevision != nil || !spec.RevisionInHandler) {
			t.Fatalf("operation %v exposes revision before handler authorization", operation)
		}
	}
	d, event, _ := interactionFixture(t, h.Handlers())
	stale := uint64(1)
	expires := uint64(120)
	body, err := protocol.EncodeRequest(protocol.Request{RequestID: id.New(), CommunityID: h.CommunityID, ChannelID: &channel,
		Operation: protocol.OpMusicPause, Revision: &stale, Expires: &expires, Args: []any{}})
	if err != nil {
		t.Fatal(err)
	}
	event.Body = &body
	encoded, err := d.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	response := interactionResponse(t, encoded)
	if response.Status != interactionStatusForbidden || response.Revision != nil {
		t.Fatalf("unauthorized stale request response=%+v, want forbidden without revision", response)
	}
}

func TestMusicEnqueueResolveFailureIsDefiniteUnavailable(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	channel, installation := id.New(), id.New()
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState()}}
	resolver := LoggingMusicTrackResolver{Resolver: &musicInteractionResolverFake{err: &youtube.ProcessError{Cause: youtube.ErrSandboxUnavailable, Stderr: "namespace\n\x00" + strings.Repeat("x", 700), ExitCode: 1}}, Logger: logger, InstallationID: installation}
	handoffs := 0
	h := MusicInteractions{Store: store, Authority: &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}, Resolver: resolver, Logger: logger, InstallationID: installation, CommunityID: id.New(), OnPersistedSnapshot: func(context.Context, id.ID, id.ID, id.ID, music.State, uint64, bool) (uint64, error) {
		handoffs++
		return 0, nil
	}}
	input := "https://youtu.be/abcdefghijk"
	result, err := h.handle(context.Background(), InteractionActor{User: id.New()}, protocol.Request{CommunityID: h.CommunityID, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{input}}, protocol.OpMusicEnqueue)
	if err != nil || result.Status != interactionStatusUnavailable || result.Revision != nil || store.saves != 0 || handoffs != 0 {
		t.Fatalf("result=%+v err=%v saves=%d handoffs=%d", result, err, store.saves, handoffs)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("logs: %s", logs.String())
	}
	for _, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["level"] != "WARN" || row["installation_id"] != installation.String() {
			t.Fatalf("log=%v", row)
		}
		stderr, _ := row["stderr"].(string)
		if len(stderr) != 512 || !strings.HasSuffix(stderr, "…") || strings.ContainsAny(stderr, "\n\x00") {
			t.Fatalf("stderr=%q (%d bytes)", stderr, len(stderr))
		}
		if strings.Contains(line, input) || row["url"] != nil || row["input"] != nil {
			t.Fatalf("input leaked: %s", line)
		}
	}
	if !strings.Contains(lines[0], `"reason":"resolve_failed"`) {
		t.Fatalf("logs: %s", logs.String())
	}
}

// Refusal tests drive real handler branches; fault wrappers only replace the
// external authority/store boundary, not the refusal/logging implementation.
type musicFaultAuthority struct {
	MusicInteractionAuthority
	fault                       string
	at, calls, djs, emergencies int
}

func (a *musicFaultAuthority) CurrentCall(ctx context.Context, channel id.ID) (MusicCallBinding, error) {
	a.calls++
	if a.fault == "current_call_lookup" && a.calls == a.at {
		return MusicCallBinding{}, errors.New("lookup failed")
	}
	return a.MusicInteractionAuthority.CurrentCall(ctx, channel)
}
func (a *musicFaultAuthority) CanUse(ctx context.Context, actor InteractionActor, call MusicCallBinding) (bool, error) {
	if a.fault == "can_use_lookup" {
		return false, errors.New("lookup failed")
	}
	return a.MusicInteractionAuthority.CanUse(ctx, actor, call)
}
func (a *musicFaultAuthority) CanDJ(ctx context.Context, actor InteractionActor, call MusicCallBinding) (bool, error) {
	a.djs++
	if a.fault == "can_dj_lookup" && a.djs == a.at {
		return false, errors.New("lookup failed")
	}
	return a.MusicInteractionAuthority.CanDJ(ctx, actor, call)
}
func (a *musicFaultAuthority) CanEmergencyStop(ctx context.Context, actor InteractionActor, call MusicCallBinding) (bool, error) {
	a.emergencies++
	if a.fault == "can_emergency_stop_lookup" && a.emergencies == a.at {
		return false, errors.New("lookup failed")
	}
	return a.MusicInteractionAuthority.CanEmergencyStop(ctx, actor, call)
}

type musicFaultStore struct {
	musicStateStore
	loadErr, saveErr error
}

func (s musicFaultStore) Load(ctx context.Context, channel id.ID) (persistence.Snapshot, error) {
	if s.loadErr != nil {
		return persistence.Snapshot{}, s.loadErr
	}
	return s.musicStateStore.Load(ctx, channel)
}
func (s musicFaultStore) Save(ctx context.Context, channel id.ID, expected uint64, state music.State) (persistence.Snapshot, error) {
	if s.saveErr != nil {
		return persistence.Snapshot{}, s.saveErr
	}
	return s.musicStateStore.Save(ctx, channel, expected, state)
}

func TestMusicRefusalsLogReason(t *testing.T) {
	tests := []struct {
		name, reason string
		status       uint64
	}{
		{"dependency", "dependency_or_context", 9}, {"call lookup", "current_call_lookup", 9}, {"call recheck", "current_call_lookup", 9},
		{"no call", "no_current_call", 5}, {"can use lookup", "can_use_lookup", 9}, {"cannot use", "forbidden", 2},
		{"search no resolver", "provider_unavailable", 5}, {"search no args", "invalid_args", 5}, {"search arg type", "invalid_args", 5},
		{"search resolve", "resolve_failed", 5}, {"search too many", "resolve_failed", 5}, {"search encode", "search_encode", 5},
		{"state read", "state_read", 9}, {"runtime read", "runtime_state_read", 9}, {"runtime revision", "revision_conflict", 4},
		{"state dj lookup", "can_dj_lookup", 9}, {"state encode", "state_encode", 9}, {"revision", "revision_conflict", 4},
		{"dj lookup", "can_dj_lookup", 9}, {"dj recheck", "can_dj_lookup", 9}, {"stop dj lookup", "can_dj_lookup", 9}, {"stop dj recheck", "can_dj_lookup", 9},
		{"emergency lookup", "can_emergency_stop_lookup", 9}, {"emergency recheck", "can_emergency_stop_lookup", 9},
		{"cannot dj", "forbidden", 2}, {"dj revoked", "forbidden", 2},
		{"enqueue no args", "invalid_args", 5}, {"enqueue arg type", "invalid_args", 5}, {"youtube unavailable", "provider_unavailable", 5}, {"spotify unavailable", "provider_unavailable", 5},
		{"enqueue resolve", "resolve_failed", 5}, {"enqueue empty", "resolve_failed", 5}, {"enqueue too many", "resolve_failed", 5},
		{"provider mismatch", "provider_mismatch", 5}, {"spotify mismatch", "provider_mismatch", 5}, {"invalid op", "invalid_operation", 5},
		{"invalid track", "apply_failed", 5}, {"apply revision", "apply_failed", 4},
		{"call changed", "call_changed", 4}, {"expired", "expired", 3}, {"save failed", "save_failed", 9}, {"save conflict", "save_failed", 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			channel, community, installation := id.New(), id.New(), id.New()
			store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState()}}
			authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
			fault := &musicFaultAuthority{MusicInteractionAuthority: authority, at: 1}
			resolver := &musicInteractionResolverFake{tracks: []music.Track{{Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Track"}}}
			h := MusicInteractions{Store: store, Authority: fault, Resolver: resolver, CommunityID: community, InstallationID: installation, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			req := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"https://youtu.be/abcdefghijk"}}
			op := protocol.OpMusicEnqueue
			if strings.HasPrefix(tc.name, "search") {
				op = protocol.OpMusicSearch
			}
			switch tc.name {
			case "dependency":
				h.Store = nil
			case "call lookup":
				fault.fault = "current_call_lookup"
			case "call recheck":
				fault.fault, fault.at = "current_call_lookup", 2
			case "no call":
				authority.call.Active = false
			case "can use lookup":
				fault.fault = "can_use_lookup"
			case "cannot use":
				authority.user = false
			case "search no resolver", "youtube unavailable":
				h.Resolver = nil
			case "search no args", "enqueue no args":
				req.Args = nil
			case "search arg type", "enqueue arg type":
				req.Args = []any{uint64(1)}
			case "search resolve", "enqueue resolve":
				resolver.err = youtube.ErrExtractFailed
			case "search too many":
				resolver.tracks = make([]music.Track, youtube.MaxSearchResults+1)
			case "search encode":
				resolver.tracks[0].Provider = music.ProviderSpotify
			case "state read":
				h.Store = musicFaultStore{musicStateStore: store, loadErr: errors.New("load failed")}
			case "runtime read":
				h.CurrentState = func(context.Context, id.ID, id.ID, id.ID) (music.State, uint64, error) {
					return music.State{}, 0, errors.New("runtime failed")
				}
			case "runtime revision":
				h.CurrentState = func(context.Context, id.ID, id.ID, id.ID) (music.State, uint64, error) {
					return music.InitialState(), 1, nil
				}
			case "state dj lookup":
				op, fault.fault = protocol.OpMusicState, "can_dj_lookup"
			case "state encode":
				op = protocol.OpMusicState
				h.Now = func() time.Time { return time.Unix(-1, 0) }
			case "revision":
				req.Revision = nil
			case "dj lookup":
				fault.fault = "can_dj_lookup"
			case "dj recheck":
				fault.fault, fault.at = "can_dj_lookup", 2
			case "stop dj lookup":
				op, fault.fault = protocol.OpMusicStop, "can_dj_lookup"
			case "stop dj recheck":
				op, fault.fault, fault.at = protocol.OpMusicStop, "can_dj_lookup", 2
			case "emergency lookup":
				op, fault.fault, authority.dj = protocol.OpMusicStop, "can_emergency_stop_lookup", false
			case "emergency recheck":
				op, fault.fault, fault.at, authority.dj, authority.emergency = protocol.OpMusicStop, "can_emergency_stop_lookup", 2, false, true
			case "cannot dj":
				authority.dj = false
			case "dj revoked":
				revoked := false
				authority.djAfterFirst = &revoked
			case "spotify unavailable":
				req.Args = []any{"spotify:track:1234567890123456789012"}
			case "enqueue empty":
				resolver.tracks = nil
			case "enqueue too many":
				resolver.tracks = make([]music.Track, youtube.MaxPlaylistItems+1)
			case "provider mismatch":
				resolver.tracks[0].Provider = music.ProviderSpotify
			case "spotify mismatch":
				req.Args = []any{"spotify:track:1234567890123456789012"}
				h.SpotifyResolver = &musicSpotifyResolverFake{tracks: resolver.tracks}
			case "invalid op":
				op, req.Args = protocol.OpMusicVolume, nil
			case "invalid track":
				resolver.tracks[0].DurationMS = -1
			case "apply revision":
				store.snapshot.State.Revision = 1
			case "call changed":
				other := authority.call
				other.CallID = id.New()
				authority.callAfterFirst = &other
			case "expired":
				expires := uint64(1)
				req.Expires = &expires
			case "save failed":
				h.Store = musicFaultStore{musicStateStore: store, saveErr: errors.New("save failed")}
			case "save conflict":
				h.Store = musicFaultStore{musicStateStore: store, saveErr: persistence.ErrConflict}
			}
			result, err := h.handle(t.Context(), InteractionActor{User: id.New()}, req, op)
			status := result.Status
			if err != nil {
				status = interactionStatusInternal
			}
			if status != tc.status || store.saves != 0 {
				t.Fatalf("status=%d err=%v saves=%d", status, err, store.saves)
			}
			var row map[string]any
			if err := json.Unmarshal(logs.Bytes(), &row); err != nil {
				t.Fatalf("expected one log, got %q: %v", logs.String(), err)
			}
			if row["reason"] != tc.reason || row["status"] != float64(tc.status) || row["op"] != float64(op) || row["installation_id"] != installation.String() || row["level"] != "WARN" || row["msg"] != "bot music operation refused" {
				t.Fatalf("log=%v", row)
			}
		})
	}
}

func TestMusicEnqueueRejectedInputIsUnavailableWithoutRevision(t *testing.T) {
	dir := t.TempDir()
	marker, tool := filepath.Join(dir, "called"), filepath.Join(dir, "must-not-run")
	if err := os.WriteFile(tool, []byte(fmt.Sprintf("#!/bin/sh\ntouch %q\nexit 99\n", marker)), 0700); err != nil {
		t.Fatal(err)
	}
	resolver, err := youtube.NewMetadataResolver(tool, tool, tool, tool)
	if err != nil {
		t.Fatal(err)
	}
	channel, community := id.New(), id.New()
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState()}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: community, Resolver: resolver,
		OnPersistedSnapshot: func(context.Context, id.ID, id.ID, id.ID, music.State, uint64, bool) (uint64, error) {
			t.Fatal("rejected input handed to playback")
			return 0, nil
		}}
	request := protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"https://a@youtube.com/watch?v=aqz-KE-bpKQ"}}
	result, err := h.handle(t.Context(), InteractionActor{User: id.New()}, request, protocol.OpMusicEnqueue)
	if err != nil || result.Status != 5 || result.Revision != nil || store.saves != 0 || store.snapshot.Revision != 0 || len(store.snapshot.State.Queue) != 0 {
		t.Fatalf("result=%+v err=%v store=%+v", result, err, store)
	}
	if _, err = os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata runner was called: %v", err)
	}
}

func TestMusicSandboxFailureRedactsContentAndLogsOnce(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	tool, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	query := "private  query & song"
	raw := query + " " + url.QueryEscape(query) + " " + url.PathEscape(query) + " https://example.test/secret [youtube] ZYXwvutsrqp: failed"
	shim := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s\\n' '"+raw+"' >&2\nexit 42\n"), 0700); err != nil {
		t.Fatal(err)
	}
	resolver, err := youtube.NewMetadataResolver(tool, tool, shim, python)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	channel, community := id.New(), id.New()
	h := MusicInteractions{Store: &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState()}}, Authority: &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true},
		Resolver: LoggingMusicTrackResolver{Resolver: resolver, Logger: logger}, Logger: logger, CommunityID: community}
	result, err := h.handle(t.Context(), InteractionActor{User: id.New()}, protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{query}}, protocol.OpMusicEnqueue)
	if err != nil || result.Status != 5 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if strings.Count(logs.String(), "\n") != 1 {
		t.Fatalf("logs=%s", logs.String())
	}
	var row map[string]any
	if err := json.Unmarshal(logs.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if got := row["stderr"]; got != "[redacted] [redacted] [redacted] [redacted] [youtube] [redacted]: failed" {
		t.Fatalf("stderr=%q", got)
	}
}

func TestMusicRefusalRetainsIdentityWithoutLoggingStartCause(t *testing.T) {
	installation, device, community, channel, group := id.New(), id.New(), id.New(), id.New(), id.New()
	state := music.InitialState()
	state.Current = &music.Track{ID: "broken", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Private title", RequesterID: "member"}
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state}}
	var logs bytes.Buffer
	cause := errors.New("https://private.example/rtc?access_token=secret-token private-response-token")
	h := MusicInteractions{Store: store, Authority: &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: channel, GroupID: group, ChannelID: channel, Active: true}, user: true, dj: true},
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)), InstallationID: installation, DeviceID: device, CommunityID: community,
		OnPersistedSnapshot: func(context.Context, id.ID, id.ID, id.ID, music.State, uint64, bool) (uint64, error) {
			return 1, &music.StartError{Stage: "room_connect", Cause: cause}
		},
	}
	result, err := h.handle(t.Context(), InteractionActor{User: id.New()}, protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{}}, protocol.OpMusicResume)
	if err != nil || result.Status != 5 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var row map[string]any
	if err := json.Unmarshal(logs.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row["device_id"] != device.String() || row["group_id"] != group.String() || row["error"] != "room_connection" {
		t.Fatalf("row=%v", row)
	}
	for _, secret := range []string{"private.example", "secret-token", "private-response-token", "Private title"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("cause leaked: %s", logs.String())
		}
	}
}

type musicSearchProfileResolverFake struct{ searches, resolves int }

func (r *musicSearchProfileResolverFake) Resolve(context.Context, string) ([]music.Track, error) {
	r.resolves++
	return nil, youtube.ErrMetadataInvalid
}
func (r *musicSearchProfileResolverFake) Search(context.Context, string) ([]music.Track, error) {
	r.searches++
	return []music.Track{{ID: "yt:LvH_ft4XR1Q", Provider: music.ProviderYouTube, CanonicalID: "LvH_ft4XR1Q", Title: "Public search result", DurationMS: 0}}, nil
}

func TestMusicSearchUsesSearchProfileAndEnqueueStaysStrict(t *testing.T) {
	for _, op := range []protocol.Operation{protocol.OpMusicSearch, protocol.OpMusicEnqueue} {
		t.Run(fmt.Sprint(op), func(t *testing.T) {
			community, channel := id.New(), id.New()
			resolver := &musicSearchProfileResolverFake{}
			store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState()}}
			h := MusicInteractions{Store: store, CommunityID: community, Authority: &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: channel, ChannelID: channel, Active: true}, user: true, dj: true}, Resolver: LoggingMusicTrackResolver{Resolver: resolver}}
			result, err := h.handle(t.Context(), InteractionActor{User: id.New()}, protocol.Request{CommunityID: community, ChannelID: &channel, Revision: uint64Ptr(0), Args: []any{"Ullared Morgan Tjuklad"}}, op)
			if err != nil {
				t.Fatal(err)
			}
			if op == protocol.OpMusicSearch {
				if result.Status != 0 || resolver.searches != 1 || resolver.resolves != 0 {
					t.Fatalf("search result=%+v search=%d resolve=%d", result, resolver.searches, resolver.resolves)
				}
				var data []any
				if err := cborx.Unmarshal(result.Data, &data); err != nil {
					t.Fatal(err)
				}
				if len(data) != 2 {
					t.Fatalf("search data=%v", data)
				}
			} else if result.Status != 5 || resolver.resolves != 1 || resolver.searches != 0 {
				t.Fatalf("enqueue result=%+v search=%d resolve=%d", result, resolver.searches, resolver.resolves)
			}
			if store.saves != 0 {
				t.Fatal("search profile leaked into queue persistence")
			}
		})
	}
}

func TestMusicStateReadReturnsDurableSnapshotDespiteRuntimeSkew(t *testing.T) {
	for _, runtimeRevision := range []uint64{0, 2} {
		t.Run(fmt.Sprint(runtimeRevision), func(t *testing.T) {
			channel, community := id.New(), id.New()
			state := music.InitialState()
			state.Revision = 1
			stateStore := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state, Revision: 1}}
			authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
			h := MusicInteractions{Store: stateStore, Authority: authority, CommunityID: community, CurrentState: func(context.Context, id.ID, id.ID, id.ID) (music.State, uint64, error) {
				state := music.InitialState()
				state.Revision = runtimeRevision
				return state, runtimeRevision, nil
			}}
			result, err := h.handle(t.Context(), InteractionActor{User: id.New()}, protocol.Request{CommunityID: community, ChannelID: &channel}, protocol.OpMusicState)
			if err != nil || result.Status != 0 || result.Revision == nil || *result.Revision != 1 {
				t.Fatalf("runtime=%d read=%+v err=%v", runtimeRevision, result, err)
			}
		})
	}
}

type musicPriorityAuthority struct {
	*musicInteractionAuthorityFake
	priorities []int
}

func (a *musicPriorityAuthority) CurrentCall(ctx context.Context, channel id.ID) (MusicCallBinding, error) {
	a.priorities = append(a.priorities, client.RequestPriority(ctx))
	return a.musicInteractionAuthorityFake.CurrentCall(ctx, channel)
}
func (a *musicPriorityAuthority) CanUse(ctx context.Context, actor InteractionActor, call MusicCallBinding) (bool, error) {
	a.priorities = append(a.priorities, client.RequestPriority(ctx))
	return a.musicInteractionAuthorityFake.CanUse(ctx, actor, call)
}
func (a *musicPriorityAuthority) CanDJ(ctx context.Context, actor InteractionActor, call MusicCallBinding) (bool, error) {
	a.priorities = append(a.priorities, client.RequestPriority(ctx))
	return a.musicInteractionAuthorityFake.CanDJ(ctx, actor, call)
}

type musicUnreadableStore struct{ t *testing.T }

func (s musicUnreadableStore) Load(context.Context, id.ID) (persistence.Snapshot, error) {
	s.t.Error("immediate handler read the queue before supervisor serialization")
	return persistence.Snapshot{}, errors.New("unexpected handler load")
}
func (s musicUnreadableStore) Save(context.Context, id.ID, uint64, music.State) (persistence.Snapshot, error) {
	s.t.Error("immediate handler saved outside supervisor serialization")
	return persistence.Snapshot{}, errors.New("unexpected handler save")
}

func TestImmediateMusicControlsAuthorizeBeforePriorityAndSkipHandlerStateReads(t *testing.T) {
	for _, op := range []protocol.Operation{protocol.OpMusicPause, protocol.OpMusicSkip} {
		for _, access := range []struct {
			name     string
			user, dj bool
		}{
			{"allowed", true, true}, {"cannot_use", false, true}, {"cannot_dj", true, false},
		} {
			t.Run(fmt.Sprintf("%d/%s", op, access.name), func(t *testing.T) {
				channel := id.New()
				authority := &musicPriorityAuthority{musicInteractionAuthorityFake: &musicInteractionAuthorityFake{
					call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: access.user, dj: access.dj,
				}}
				called := false
				h := MusicInteractions{Store: musicUnreadableStore{t}, Authority: authority, CommunityID: id.New(),
					CurrentState: func(context.Context, id.ID, id.ID, id.ID) (music.State, uint64, error) {
						t.Error("immediate handler queried runtime before supervisor serialization")
						return music.State{}, 0, errors.New("unexpected runtime read")
					},
					OnImmediateControl: func(ctx context.Context, _, _, _ id.ID, control music.Operation) (music.State, error) {
						called = true
						if client.RequestPriority(ctx) != 2 {
							t.Error("authorized control lost immediate priority")
						}
						state := music.InitialState()
						state.Revision = 7
						return state, nil
					},
				}
				result, err := h.handle(client.WithInteractive(context.Background()), InteractionActor{User: id.New()}, protocol.Request{
					CommunityID: h.CommunityID, ChannelID: &channel, Revision: uint64Ptr(0),
				}, op)
				if err != nil {
					t.Fatal(err)
				}
				allowed := access.user && access.dj
				if called != allowed || allowed && (result.Status != 0 || result.Revision == nil || *result.Revision != 7) || !allowed && result.Status != interactionStatusForbidden {
					t.Fatalf("result=%+v callback=%v allowed=%v", result, called, allowed)
				}
				for i, priority := range authority.priorities {
					want := 1
					if allowed && i >= 3 {
						want = 2
					}
					if priority != want {
						t.Errorf("authority check %d priority=%d want %d; authorization must precede immediate priority", i, priority, want)
					}
				}
			})
		}
	}
}

func TestMusicSkipAcceptsOptionalExpectedCurrentItem(t *testing.T) {
	for _, args := range [][]any{nil, {id.New().String()}, {""}} {
		op, ok := musicOperation(protocol.OpMusicSkip, args)
		if !ok || op.Kind != music.OpSkip {
			t.Fatalf("args=%v operation=%+v ok=%v", args, op, ok)
		}
		if len(args) == 0 {
			if op.ExpectedCurrentItemID != nil {
				t.Fatal("legacy skip unexpectedly guarded")
			}
		} else if op.ExpectedCurrentItemID == nil || *op.ExpectedCurrentItemID != args[0] {
			t.Fatalf("expected-current guard lost: %+v", op)
		}
	}
	for _, args := range [][]any{{uint64(1)}, {"not-an-id"}, {nil}, {id.New().String(), id.New().String()}} {
		if _, ok := musicOperation(protocol.OpMusicSkip, args); ok {
			t.Errorf("accepted malformed skip args=%v", args)
		}
	}
}

func TestImmediateMusicSkipReplaysExactRequestWithoutAdvancingAgain(t *testing.T) {
	channel := interactionID(5)
	current := id.New().String()
	store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: music.InitialState()}}
	authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
	calls := 0
	h := MusicInteractions{Store: store, Authority: authority, CommunityID: interactionID(2), Now: func() time.Time { return time.Unix(100, 0) },
		OnImmediateControl: func(_ context.Context, _, _, _ id.ID, op music.Operation) (music.State, error) {
			calls++
			if op.ExpectedCurrentItemID == nil || *op.ExpectedCurrentItemID != current {
				t.Error("skip lost its expected current item")
			}
			state := music.InitialState()
			state.Revision = 1
			return state, nil
		},
	}
	dispatcher, event, _ := interactionFixture(t, h.Handlers())
	body, err := protocol.EncodeRequest(protocol.Request{RequestID: id.New(), CommunityID: h.CommunityID, ChannelID: &channel,
		Operation: protocol.OpMusicSkip, Revision: uint64Ptr(0), Expires: uint64Ptr(120), Args: []any{current}})
	if err != nil {
		t.Fatal(err)
	}
	event.Body = &body
	first, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil || first != replay || calls != 1 || interactionResponse(t, first).Status != 0 {
		t.Fatalf("replay=%v calls=%d err=%v response=%+v", first == replay, calls, err, interactionResponse(t, first))
	}
}

func TestImmediateMusicControlsStillRecheckCallDJAndExpiry(t *testing.T) {
	for _, op := range []protocol.Operation{protocol.OpMusicPause, protocol.OpMusicSkip} {
		for _, refusal := range []string{"call_changed", "dj_revoked", "expired"} {
			t.Run(fmt.Sprintf("%d/%s", op, refusal), func(t *testing.T) {
				channel := id.New()
				authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
				request := protocol.Request{ChannelID: &channel, Revision: uint64Ptr(0)}
				want := uint64(interactionStatusForbidden)
				switch refusal {
				case "call_changed":
					changed := authority.call
					changed.CallID = id.New()
					authority.callAfterFirst = &changed
					want = interactionStatusConflict
				case "dj_revoked":
					denied := false
					authority.djAfterFirst = &denied
				case "expired":
					request.Expires = uint64Ptr(100)
					want = interactionStatusExpired
				}
				h := MusicInteractions{Store: musicUnreadableStore{t}, Authority: authority, CommunityID: id.New(), Now: func() time.Time { return time.Unix(100, 0) },
					OnImmediateControl: func(context.Context, id.ID, id.ID, id.ID, music.Operation) (music.State, error) {
						t.Error("refused immediate control reached playback")
						return music.InitialState(), nil
					},
				}
				request.CommunityID = h.CommunityID
				result, err := h.handle(t.Context(), InteractionActor{User: id.New()}, request, op)
				if err != nil || result.Status != want {
					t.Fatalf("result=%+v err=%v want=%d", result, err, want)
				}
			})
		}
	}
}

func TestMusicSkipFallbackHonorsStaleGuardAfterFinalAuthorization(t *testing.T) {
	for _, name := range []string{"different_item", "expected_empty", "now_empty", "call_changed", "dj_revoked", "expired"} {
		t.Run(name, func(t *testing.T) {
			channel := id.New()
			state := music.InitialState()
			state.Revision = 7
			state.Current = &music.Track{ID: id.New().String(), Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Current", RequesterID: id.New().String()}
			state.Queue = []music.Track{{ID: id.New().String(), Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "Next", RequesterID: id.New().String()}}
			expected := id.New().String()
			if name == "expected_empty" {
				expected = ""
			}
			if name == "now_empty" {
				state.Current = nil
			}
			store := &musicInteractionStoreFake{snapshot: persistence.Snapshot{State: state, Revision: state.Revision}}
			authority := &musicInteractionAuthorityFake{call: MusicCallBinding{CallID: id.New(), ChannelID: channel, Active: true}, user: true, dj: true}
			want := uint64(0)
			expires := uint64(120)
			switch name {
			case "call_changed":
				changed := authority.call
				changed.CallID = id.New()
				authority.callAfterFirst = &changed
				want = interactionStatusConflict
			case "dj_revoked":
				denied := false
				authority.djAfterFirst = &denied
				want = interactionStatusForbidden
			case "expired":
				expires = 100
				want = interactionStatusExpired
			}
			h := MusicInteractions{Store: store, Authority: authority, CommunityID: id.New(), Now: func() time.Time { return time.Unix(100, 0) },
				OnPersistedSnapshot: func(context.Context, id.ID, id.ID, id.ID, music.State, uint64, bool) (uint64, error) {
					t.Error("stale guarded skip reached playback handoff")
					return 0, nil
				},
			}
			result, err := h.handle(t.Context(), InteractionActor{User: id.New()}, protocol.Request{CommunityID: h.CommunityID, ChannelID: &channel,
				Revision: uint64Ptr(0), Expires: &expires, Args: []any{expected}}, protocol.OpMusicSkip)
			if err != nil || result.Status != want || store.saves != 0 {
				t.Fatalf("result=%+v err=%v saves=%d want status=%d without writes", result, err, store.saves, want)
			}
			if want == 0 {
				data, err := encodeMusicSnapshot(state, true, h.clock().UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				if result.Revision == nil || *result.Revision != state.Revision || !bytes.Equal(result.Data, data) {
					t.Fatalf("stale skip must return unchanged snapshot: result=%+v", result)
				}
				if authority.callChecks != 2 || authority.roleChecks != 2 {
					t.Fatalf("stale skip bypassed final authorization: calls=%d roles=%d", authority.callChecks, authority.roleChecks)
				}
			}
		})
	}
}

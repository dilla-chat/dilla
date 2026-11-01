//go:build linux

package main

import (
	"context"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/music/spotify"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

type probeAuthority struct{ call service.MusicCallBinding }

func (a probeAuthority) CurrentCall(context.Context, id.ID) (service.MusicCallBinding, error) {
	return a.call, nil
}
func (probeAuthority) CanUse(context.Context, service.InteractionActor, service.MusicCallBinding) (bool, error) {
	return true, nil
}
func (probeAuthority) CanDJ(context.Context, service.InteractionActor, service.MusicCallBinding) (bool, error) {
	return true, nil
}
func (probeAuthority) CanEmergencyStop(context.Context, service.InteractionActor, service.MusicCallBinding) (bool, error) {
	return true, nil
}

func TestProbeSkipThenPause(t *testing.T) {
	installation, community, channel := id.New(), id.New(), id.New()
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
	store, err := persistence.New(n, community)
	if err != nil {
		t.Fatal(err)
	}
	calls := &runtimeCalls{call: client.Call{CallID: channel, GroupID: id.New(), LiveKitURL: "wss://media.example", Token: "secret"}}
	starter, err := newMusicQueueStarter(musicRuntimeDeps{Community: community, Channel: channel, Calls: calls,
		JoinCallGroup: func(context.Context) error { return nil }, SpotifyPCM: failingRuntimePCM{spotify.ErrProviderUnavailable},
		YouTubePCM: liveRuntimePCM{}, LeaseManager: testLeaseManager(t), Encoder: liveRuntimeEncoderStarter{},
		Publisher: &liveRuntimePublisherStarter{starts: map[id.ID]int{}}, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := newMusicSupervisor(context.Background(), channel, store, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	rt := &musicQueueRuntime{store: store, community: community, supervisors: map[id.ID]*music.Supervisor{channel: supervisor}}
	h := service.MusicInteractions{Store: store, Authority: probeAuthority{call: service.MusicCallBinding{CallID: channel, ChannelID: channel, Active: true}},
		InstallationID: installation, CommunityID: community,
		CurrentState: func(ctx context.Context, _, _, ch id.ID) (music.State, uint64, error) {
			return rt.CurrentState(ctx, ch)
		},
		OnPersistedSnapshot: func(ctx context.Context, _, _, ch id.ID, s music.State, r uint64, start bool) (uint64, error) {
			return rt.ApplyPersistedSnapshot(ctx, ch, s, r, start)
		},
	}
	tracks := []music.Track{
		{ID: "broken", Provider: music.ProviderSpotify, CanonicalID: "spotify:track:0123456789012345678901", Title: "Broken", RequesterID: "member"},
		{ID: "working", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Working", RequesterID: "member"},
	}
	state, _ := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
	saved, err := store.Save(context.Background(), channel, 0, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.ApplyPersistedSnapshot(context.Background(), channel, saved.State, saved.Revision, false); err != nil {
		t.Fatal(err)
	}
	hs := h.Handlers()
	actor := service.InteractionActor{User: id.New(), Device: id.New()}
	do := func(op protocol.Operation, rev *uint64) service.InteractionResult {
		ch := channel
		res, err := hs[op].Handle(context.Background(), actor, protocol.Request{RequestID: id.New(), CommunityID: community, ChannelID: &ch, Operation: op, Revision: rev})
		if err != nil {
			t.Fatal(err)
		}
		var r uint64
		if res.Revision != nil {
			r = *res.Revision
		}
		d, _ := store.Load(context.Background(), channel)
		t.Logf("op %d -> status %d rev %d; supervisor state.Revision %d; durable %d", op, res.Status, r, supervisor.State().Revision, d.Revision)
		return res
	}
	st := do(protocol.OpMusicState, nil)
	resumed := do(protocol.OpMusicResume, st.Revision)
	if resumed.Status != 0 || supervisor.State().Current == nil || supervisor.State().Current.ID != "working" {
		t.Fatalf("skip and resume: result=%+v state=%+v", resumed, supervisor.State())
	}
	st = do(protocol.OpMusicState, nil)
	res := do(protocol.OpMusicPause, st.Revision)
	if res.Status != 0 {
		t.Fatalf("pause status %d", res.Status)
	}
}

func TestHandoffFailureRecoversStateReadAndPause(t *testing.T) {
	installation, community, channel := id.New(), id.New(), id.New()
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
	store, _ := persistence.New(n, community)
	calls := &runtimeCalls{call: client.Call{CallID: channel, GroupID: id.New(), LiveKitURL: "wss://media.example", Token: "secret"}}
	starter, _ := newMusicQueueStarter(musicRuntimeDeps{Community: community, Channel: channel, Calls: calls,
		JoinCallGroup: func(context.Context) error { return nil }, SpotifyPCM: failingRuntimePCM{spotify.ErrPlaybackFailed},
		YouTubePCM: liveRuntimePCM{}, LeaseManager: testLeaseManager(t), Encoder: liveRuntimeEncoderStarter{},
		Publisher: &liveRuntimePublisherStarter{starts: map[id.ID]int{}}, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
	supervisor, _ := newMusicSupervisor(context.Background(), channel, store, starter)
	defer supervisor.Close()
	rt := &musicQueueRuntime{store: store, community: community, supervisors: map[id.ID]*music.Supervisor{channel: supervisor}}
	fail := true
	h := service.MusicInteractions{Store: store, Authority: probeAuthority{call: service.MusicCallBinding{CallID: channel, ChannelID: channel, Active: true}},
		InstallationID: installation, CommunityID: community,
		CurrentState: func(ctx context.Context, _, _, ch id.ID) (music.State, uint64, error) {
			return rt.CurrentState(ctx, ch)
		},
		OnPersistedSnapshot: func(ctx context.Context, _, _, ch id.ID, s music.State, r uint64, start bool) (uint64, error) {
			if fail {
				fail = false
				return 0, context.DeadlineExceeded
			}
			return rt.ApplyPersistedSnapshot(ctx, ch, s, r, start)
		},
	}
	tracks := []music.Track{{ID: "working", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Working", RequesterID: "member"}}
	state, _ := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
	saved, _ := store.Save(context.Background(), channel, 0, state)
	_, _ = rt.ApplyPersistedSnapshot(context.Background(), channel, saved.State, saved.Revision, false)
	hs := h.Handlers()
	actor := service.InteractionActor{User: id.New(), Device: id.New()}
	do := func(op protocol.Operation, rev *uint64) service.InteractionResult {
		ch := channel
		res, _ := hs[op].Handle(context.Background(), actor, protocol.Request{RequestID: id.New(), CommunityID: community, ChannelID: &ch, Operation: op, Revision: rev})
		var r uint64
		if res.Revision != nil {
			r = *res.Revision
		}
		d, _ := store.Load(context.Background(), channel)
		t.Logf("op %d -> status %d rev %d; supervisor %d; durable %d", op, res.Status, r, supervisor.State().Revision, d.Revision)
		return res
	}
	st := do(protocol.OpMusicState, nil)
	do(protocol.OpMusicResume, st.Revision)
	st = do(protocol.OpMusicState, nil)
	if st.Status != 0 {
		t.Fatalf("state read after handoff failure: status %d", st.Status)
	}
	paused := do(protocol.OpMusicPause, st.Revision)
	if paused.Status != 0 {
		t.Fatalf("pause after handoff failure: status %d", paused.Status)
	}
}

func TestScheduledResumeSurvivesAStateReadBetweenCommitAndHandoff(t *testing.T) {
	community, channel := id.New(), id.New()
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, rows: map[string]native.StateValue{}}
	stateStore, err := persistence.New(n, community)
	if err != nil {
		t.Fatal(err)
	}
	starter := &controlLatencyStarter{entered: make(chan struct{}), playback: &controlLatencyPlayback{done: make(chan struct{})}}
	supervisor, err := music.NewSupervisor(t.Context(), channel, persistedMusicQueue{stateStore}, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	state, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{ID: "queued", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Queued", RequesterID: "member"}}})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := stateStore.Save(t.Context(), channel, 0, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.ApplyPersistedSnapshot(t.Context(), queued.State, queued.Revision, false); err != nil {
		t.Fatal(err)
	}
	state, err = music.Apply(queued.State, queued.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	playing, err := stateStore.Save(t.Context(), channel, queued.Revision, state)
	if err != nil {
		t.Fatal(err)
	}
	// A racing read repairs the missed handoff without starting any playback.
	repaired, revision, err := supervisor.RefreshState(t.Context())
	if err != nil || revision != playing.Revision || !repaired.Paused {
		t.Fatalf("read repair state=%+v revision=%d err=%v", repaired, revision, err)
	}
	select {
	case <-starter.entered:
		t.Fatal("read repair started audio")
	default:
	}
	if _, err := supervisor.SchedulePersistedSnapshot(t.Context(), playing.State, playing.Revision, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-starter.entered:
	case <-time.After(time.Second):
		t.Fatal("the explicit resume handoff was lost after a read adopted its revision")
	}
}

//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/livekit"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/music/session"
	"github.com/jonasthim/dilla/internal/bot/music/spotify"
	"github.com/jonasthim/dilla/internal/bot/music/youtube"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/spotifyauth"
	botstrings "github.com/jonasthim/dilla/internal/bot/strings"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type spotifyCredentialsFake struct {
	token spotifyauth.Token
	err   error
}

func (f spotifyCredentialsFake) Load(context.Context) (spotifyauth.Token, error) {
	return f.token, f.err
}

type spotifyCatalogRoundTripper func(*http.Request) (*http.Response, error)

func (f spotifyCatalogRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestInstallationSpotifyCatalogResolverUsesCredentialAndActor(t *testing.T) {
	requester := id.New()
	client := &http.Client{Transport: spotifyCatalogRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://api.spotify.com/v1/tracks/0123456789012345678901" || request.Header.Get("Authorization") != "Bearer installation-access-token" {
			t.Fatalf("Spotify request URL=%q authorization=%q", request.URL, request.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"0123456789012345678901","type":"track","name":"Song","uri":"spotify:track:0123456789012345678901","duration_ms":123000,"artists":[{"name":"Artist"}],"album":{"name":"Record"}}`))}, nil
	})}
	resolver := installationSpotifyCatalogResolver{
		credentials: spotifyCredentialsFake{token: spotifyauth.Token{AccessToken: "installation-access-token"}},
		catalog:     spotify.NewCatalog(client),
	}
	tracks, err := resolver.ResolveSpotify(context.Background(), "spotify:track:0123456789012345678901", requester)
	if err != nil || len(tracks) != 1 || tracks[0].Provider != music.ProviderSpotify || tracks[0].RequesterID != requester.String() || tracks[0].Title != "Song" {
		t.Fatalf("resolved tracks=%+v err=%v, want one authenticated Spotify track attributed to actor", tracks, err)
	}
}

type runtimeCalls struct {
	call      client.Call
	err       error
	channel   id.ID
	starts    int
	startHook func()
}

func (c *runtimeCalls) StartCall(_ context.Context, channel id.ID) (client.Call, error) {
	c.channel = channel
	c.starts++
	if c.startHook != nil {
		c.startHook()
	}
	return c.call, c.err
}

func TestYouTubeQueueStarterDoesNotSubstituteForSpotify(t *testing.T) {
	community, channel := id.New(), id.New()
	calls := &runtimeCalls{call: client.Call{CallID: channel, GroupID: id.New(), LiveKitURL: "wss://media.example", Token: "token"}}
	starter, err := newMusicQueueStarter(musicRuntimeDeps{Community: community, Channel: channel, Calls: calls,
		YouTubePCM: youtube.PCMStarter{YTDLP: "yt-dlp", FFmpeg: "ffmpeg"}, Encoder: runtimeEncoder{}, Publisher: runtimePublisher{}, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, err = starter.OptionsFor(context.Background(), music.Track{Provider: music.ProviderSpotify, CanonicalID: "spotify:track:123"})
	if err == nil || calls.starts != 0 {
		t.Fatalf("Spotify fell through to YouTube runtime: error=%v calls=%d", err, calls.starts)
	}
}

type runtimeEpochs struct{}

func (runtimeEpochs) CallMediaEpoch(context.Context, id.ID) (native.CallMediaEpoch, error) {
	return native.CallMediaEpoch{}, nil
}

type runtimeBoundEpochSource struct{ binding native.Binding }

func (n *runtimeBoundEpochSource) Binding() native.Binding { return n.binding }
func (*runtimeBoundEpochSource) CallMediaEpoch(context.Context, id.ID) (native.CallMediaEpoch, error) {
	return native.CallMediaEpoch{}, errors.New("unused")
}

type runtimePCM struct{}

func (runtimePCM) Start(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

type runtimeEncoder struct{}

func (runtimeEncoder) Start(context.Context, io.ReadCloser) (session.Encoder, error) {
	return nil, errors.New("unused")
}

type runtimePublisher struct{}

func (runtimePublisher) Start(context.Context, client.Call, id.ID, livekit.EpochSource, session.Encoder) (session.Publisher, error) {
	return nil, errors.New("unused")
}

type liveRuntimePCM struct{}

func (liveRuntimePCM) Start(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("bounded test PCM")), nil
}

type liveRuntimeEncoder struct {
	done chan struct{}
	once sync.Once
}

func (e *liveRuntimeEncoder) Done() <-chan struct{} { return e.done }
func (*liveRuntimeEncoder) Wait() error             { return nil }
func (e *liveRuntimeEncoder) Close() error          { e.once.Do(func() { close(e.done) }); return nil }

type liveRuntimeEncoderStarter struct{}

func (liveRuntimeEncoderStarter) Start(context.Context, io.ReadCloser) (session.Encoder, error) {
	return &liveRuntimeEncoder{done: make(chan struct{})}, nil
}

type liveRuntimePublisher struct {
	done chan struct{}
}

func (p *liveRuntimePublisher) Done() <-chan struct{} { return p.done }
func (*liveRuntimePublisher) Wait() error             { return nil }
func (p *liveRuntimePublisher) Close() error          { return nil }

type liveRuntimePublisherStarter struct {
	mu     sync.Mutex
	starts map[id.ID]int
}

func (p *liveRuntimePublisherStarter) Start(_ context.Context, call client.Call, _ id.ID, _ livekit.EpochSource, _ session.Encoder) (session.Publisher, error) {
	p.mu.Lock()
	p.starts[call.CallID]++
	p.mu.Unlock()
	return &liveRuntimePublisher{done: make(chan struct{})}, nil
}

type runtimeStateNative struct {
	mu      sync.Mutex
	binding native.Binding
	rows    map[string]native.StateValue
	closed  bool
}

func (n *runtimeStateNative) Binding() native.Binding { return n.binding }
func (n *runtimeStateNative) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	return nil
}
func (n *runtimeStateNative) StateGet(_ context.Context, _ uint64, key []byte) (native.StateValue, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.rows[string(key)], nil
}
func (n *runtimeStateNative) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := changes[0]
	old := n.rows[string(c.Key)]
	if old.Revision != c.ExpectedRevision {
		return nil, persistence.ErrConflict
	}
	revision := old.Revision + 1
	n.rows[string(c.Key)] = native.StateValue{Revision: revision, Value: append([]byte(nil), c.Value...)}
	return []uint64{revision}, nil
}

func TestMusicSnapshotBridgeRejectsStaleAndMissingSessions(t *testing.T) {
	community, channel := id.New(), id.New()
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, rows: map[string]native.StateValue{}}
	store, err := persistence.New(n, community)
	if err != nil {
		t.Fatal(err)
	}
	starter := unavailableMusicQueueStarter()
	supervisor, err := newMusicSupervisor(context.Background(), channel, store, starter)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &musicQueueRuntime{community: community, store: store, supervisors: map[id.ID]*music.Supervisor{channel: supervisor}}
	next, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{ID: "one", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One", RequesterID: "member"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), channel, 0, next); err != nil {
		t.Fatal(err)
	}
	next, err = music.Apply(next, next.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(context.Background(), channel, 1, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ApplyPersistedSnapshot(context.Background(), channel, saved.State, saved.Revision-1, false); !errors.Is(err, music.ErrRevisionConflict) {
		t.Fatalf("stale bridge error = %v, want revision conflict", err)
	}
	if _, err := runtime.ApplyPersistedSnapshot(context.Background(), id.New(), saved.State, saved.Revision, false); err == nil {
		t.Fatal("missing session was accepted")
	}
	if got := runtime.supervisors[channel]; got != supervisor {
		t.Fatal("failed refresh replaced the current supervisor")
	}
}

func TestMusicSnapshotCommitCallbackRefreshesPausedMetadataAndToleratesMissingRuntime(t *testing.T) {
	installation, community, channel := id.New(), id.New(), id.New()
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
	store, err := persistence.New(n, community)
	if err != nil {
		t.Fatal(err)
	}
	queues, err := restoreMusicSupervisors(context.Background(), community, []client.Channel{{ID: channel, CommunityID: community, Kind: musicVoiceChannelKind}}, store, unavailableMusicQueueStarter())
	if err != nil {
		t.Fatal(err)
	}
	registry := &musicQueueRegistry{runtimes: map[id.ID]*retainedMusicRuntime{installation: {community: community, queues: queues, native: n}}}
	defer registry.Close()
	next, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpPause})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(context.Background(), channel, 0, next)
	if err != nil {
		t.Fatal(err)
	}
	callback := musicSnapshotCommitCallback(registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := callback(context.Background(), installation, community, channel, saved.State, saved.Revision, false); err != nil {
		t.Fatal(err)
	}
	if state := queues.supervisors[channel].State(); state.Revision != saved.Revision || !state.Paused {
		t.Fatalf("retained state=%+v, want paused revision %d", state, saved.Revision)
	}
	// Reconcile may tear down a runtime between the commit and this callback.
	// The caller logs that condition; the committed snapshot remains for later restore.
	_, _ = musicSnapshotCommitCallback(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))(context.Background(), installation, community, channel, saved.State, saved.Revision, false)
}

func TestMusicSnapshotBridgeRefreshesChannelsConcurrentlyAndClosesOnTeardown(t *testing.T) {
	community := id.New()
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, rows: map[string]native.StateValue{}}
	store, err := persistence.New(n, community)
	if err != nil {
		t.Fatal(err)
	}
	starter := unavailableMusicQueueStarter()
	channels := []id.ID{id.New(), id.New()}
	committed := make(map[id.ID]music.State, len(channels))
	runtime := &musicQueueRuntime{community: community, store: store, supervisors: map[id.ID]*music.Supervisor{}}
	for _, channel := range channels {
		s, err := newMusicSupervisor(context.Background(), channel, store, starter)
		if err != nil {
			t.Fatal(err)
		}
		runtime.supervisors[channel] = s
		state, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{ID: channel.String(), Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "One", RequesterID: "member"}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Save(context.Background(), channel, 0, state); err != nil {
			t.Fatal(err)
		}
		state, err = music.Apply(state, state.Revision, music.Operation{Kind: music.OpResume})
		if err != nil {
			t.Fatal(err)
		}
		saved, err := store.Save(context.Background(), channel, 1, state)
		if err != nil {
			t.Fatal(err)
		}
		committed[channel] = saved.State
	}
	installation := id.New()
	registry := &musicQueueRegistry{runtimes: map[id.ID]*retainedMusicRuntime{installation: {community: community, queues: runtime, native: n}}}
	var wg sync.WaitGroup
	errCh := make(chan error, len(channels))
	for _, channel := range channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := registry.ApplyPersistedSnapshot(context.Background(), installation, community, channel, committed[channel], 2, false)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, channel := range channels {
		if state := runtime.supervisors[channel].State(); state.Revision != 2 || state.Paused || state.Current == nil {
			t.Fatalf("channel %s refreshed to %#v; want persisted revision 2 adopted without starting", channel, state)
		}
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ApplyPersistedSnapshot(context.Background(), installation, community, channels[0], music.InitialState(), 2, false); err == nil {
		t.Fatal("bridge after teardown was accepted")
	}
}

func TestMusicLiveResumeStartsOnlyItsRoomAfterPausedRestart(t *testing.T) {
	community := id.New()
	installation := id.New()
	channels := []id.ID{id.New(), id.New()}
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
	store, err := persistence.New(n, community)
	if err != nil {
		t.Fatal(err)
	}
	committed := make(map[id.ID]music.State, len(channels))
	for _, channel := range channels {
		track := music.Track{ID: channel.String(), Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Track", RequesterID: "member"}
		queued, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track}})
		if err != nil {
			t.Fatal(err)
		}
		saved, err := store.Save(context.Background(), channel, 0, queued)
		if err != nil {
			t.Fatal(err)
		}
		_ = saved // The process will restart at this persisted queue revision.
	}
	publishers := &liveRuntimePublisherStarter{starts: map[id.ID]int{}}
	starterFor := func(channel id.ID) (session.QueueStarter, error) {
		return session.QueueStarter{OptionsFor: func(_ context.Context, track music.Track) (session.Options, error) {
			if track.ID != channel.String() {
				return session.Options{}, errors.New("track crossed voice channel")
			}
			group := id.New()
			return session.Options{Community: community, Channel: channel, Provider: music.ProviderYouTube, Track: track.CanonicalID,
				Call: client.Call{CallID: channel, GroupID: group, LiveKitURL: "wss://example.invalid", Token: "test"}, Group: group,
				PCM: liveRuntimePCM{}, Encoder: liveRuntimeEncoderStarter{}, Publisher: publishers, Epochs: runtimeEpochs{}}, nil
		}}, nil
	}
	queues, err := restoreMusicSupervisorsWithStarterFactory(context.Background(), community,
		[]client.Channel{{ID: channels[0], CommunityID: community, Kind: musicVoiceChannelKind}, {ID: channels[1], CommunityID: community, Kind: musicVoiceChannelKind}}, store, starterFor)
	if err != nil {
		t.Fatal(err)
	}
	defer queues.Close()
	for _, channel := range channels {
		if state := queues.supervisors[channel].State(); !state.Paused {
			t.Fatalf("restart did not restore channel %s paused: %+v", channel, state)
		}
	}
	if len(publishers.starts) != 0 {
		t.Fatalf("restart started publishers: %v", publishers.starts)
	}
	for _, channel := range channels {
		loaded, err := store.Load(context.Background(), channel)
		if err != nil {
			t.Fatal(err)
		}
		playing, err := music.Apply(loaded.State, loaded.Revision, music.Operation{Kind: music.OpResume})
		if err != nil {
			t.Fatal(err)
		}
		saved, err := store.Save(context.Background(), channel, loaded.Revision, playing)
		if err != nil {
			t.Fatal(err)
		}
		committed[channel] = saved.State
		if _, err := queues.ApplyPersistedSnapshot(context.Background(), channel, saved.State, saved.Revision, true); err != nil {
			t.Fatal(err)
		}
		liveState, liveRevision, err := queues.CurrentState(context.Background(), channel)
		if err != nil || liveRevision != saved.Revision || liveState.Paused {
			t.Fatalf("live runtime state=%+v revision=%d err=%v; want unpaused committed revision %d", liveState, liveRevision, err, saved.Revision)
		}
	}
	publishers.mu.Lock()
	if publishers.starts[channels[0]] != 1 || publishers.starts[channels[1]] != 1 {
		t.Fatalf("per-room publisher starts=%v", publishers.starts)
	}
	publishers.mu.Unlock()
	paused, err := music.Apply(committed[channels[0]], committed[channels[0]].Revision, music.Operation{Kind: music.OpPause})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(context.Background(), channels[0], committed[channels[0]].Revision, paused)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queues.ApplyPersistedSnapshot(context.Background(), channels[0], saved.State, saved.Revision, false); err != nil {
		t.Fatal(err)
	}
	if state := queues.supervisors[channels[0]].State(); !state.Paused {
		t.Fatalf("paused room state=%+v", state)
	}
	if state := queues.supervisors[channels[1]].State(); state.Paused {
		t.Fatalf("pause crossed into other room: %+v", state)
	}
}

func testLeaseManager(t *testing.T) *music.SpotifyLeaseManager {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := music.NewSpotifyLeaseManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestNewMusicQueueStarterResolvesTrackToRoomCallAndProvider(t *testing.T) {
	channel, community, group := id.New(), id.New(), id.New()
	calls := &runtimeCalls{call: client.Call{CallID: channel, GroupID: group, LiveKitURL: "wss://relay.example", Token: "call-token"}}
	youtubePCM := runtimePCM{}
	starter, err := newMusicQueueStarter(musicRuntimeDeps{
		Community: community, Calls: calls, SpotifyPCM: runtimePCM{}, YouTubePCM: youtubePCM,
		Encoder: runtimeEncoder{}, Publisher: runtimePublisher{}, Epochs: runtimeEpochs{},
		Channel: channel, LeaseManager: testLeaseManager(t), Lifetime: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	track := music.Track{Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk"}
	o, err := starter.OptionsFor(context.Background(), track)
	if err != nil {
		t.Fatal(err)
	}
	if calls.starts != 0 {
		t.Fatal("inert options touched call")
	}
	o.Call, err = o.PrepareCall(context.Background())
	o.Group = o.Call.GroupID
	if err != nil {
		t.Fatal(err)
	}
	if calls.channel != channel {
		t.Fatalf("call channel = %s, want %s", calls.channel, channel)
	}
	if _, ok := o.PCM.(session.YouTubePCM); !ok {
		t.Fatalf("YouTube PCM adapter type = %T", o.PCM)
	}
	if o.Community != community || o.Channel != channel || o.Provider != music.ProviderYouTube || o.Track != track.CanonicalID || o.Group != group || o.Call.CallID != calls.call.CallID || o.Epochs == nil || o.Encoder == nil || o.Publisher == nil {
		t.Fatalf("incomplete runtime options: %#v", o)
	}
}

func TestMusicQueueStarterJoinsCallGroupBeforeMintingCallCredentials(t *testing.T) {
	channel, community, group := id.New(), id.New(), id.New()
	calls := &runtimeCalls{call: client.Call{CallID: channel, GroupID: group, LiveKitURL: "wss://relay.example", Token: "call-token"}}
	order := []string{}
	calls.startHook = func() { order = append(order, "start-call") }
	joinErr := errors.New("call group join failed")
	starter, err := newMusicQueueStarter(musicRuntimeDeps{
		Community: community, Channel: channel, Calls: calls,
		JoinCallGroup: func(context.Context) error { order = append(order, "join-group"); return joinErr },
		YouTubePCM:    runtimePCM{}, Encoder: runtimeEncoder{}, Publisher: runtimePublisher{}, Epochs: runtimeEpochs{}, Lifetime: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	o, err := starter.OptionsFor(context.Background(), music.Track{Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk"})
	if err == nil {
		_, err = o.PrepareCall(context.Background())
	}
	if !errors.Is(err, joinErr) {
		t.Fatalf("OptionsFor error = %v, want call-group join error", err)
	}
	if calls.starts != 0 || len(order) != 1 || order[0] != "join-group" {
		t.Fatalf("call credentials were requested before successful group join: starts=%d order=%v", calls.starts, order)
	}

	order = nil
	starter, err = newMusicQueueStarter(musicRuntimeDeps{
		Community: community, Channel: channel, Calls: calls,
		JoinCallGroup: func(context.Context) error { order = append(order, "join-group"); return nil },
		YouTubePCM:    runtimePCM{}, Encoder: runtimeEncoder{}, Publisher: runtimePublisher{}, Epochs: runtimeEpochs{}, Lifetime: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	o, err = starter.OptionsFor(context.Background(), music.Track{Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk"})
	if err == nil {
		_, err = o.PrepareCall(context.Background())
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "join-group" || order[1] != "start-call" {
		t.Fatalf("call-group join/start order = %v", order)
	}
}

func TestMusicQueueStarterAttachesGlobalLeaseOnlyToSpotifyUse(t *testing.T) {
	channel, community, group := id.New(), id.New(), id.New()
	calls := &runtimeCalls{call: client.Call{CallID: channel, GroupID: group, LiveKitURL: "wss://relay.example", Token: "call-token"}}
	starter, err := newMusicQueueStarter(musicRuntimeDeps{Community: community, Channel: channel, Calls: calls, SpotifyPCM: runtimePCM{}, YouTubePCM: runtimePCM{}, LeaseManager: testLeaseManager(t), Encoder: runtimeEncoder{}, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	spotifyOptions, err := starter.OptionsFor(context.Background(), music.Track{Provider: music.ProviderSpotify, CanonicalID: "spotify:track:1234567890123456789012"})
	if err != nil {
		t.Fatal(err)
	}
	if spotifyOptions.Acquire == nil {
		t.Fatal("Spotify options must use the installation-wide lease")
	}
	youtubeOptions, err := starter.OptionsFor(context.Background(), music.Track{Provider: music.ProviderYouTube, CanonicalID: "https://www.youtube.com/watch?v=abcdefghijk"})
	if err != nil {
		t.Fatal(err)
	}
	if youtubeOptions.Acquire != nil {
		t.Fatal("YouTube options must not acquire the Spotify lease")
	}
}

func TestNewMusicQueueStarterRejectsMissingProviderConfiguration(t *testing.T) {
	_, err := newMusicQueueStarter(musicRuntimeDeps{})
	if err == nil {
		t.Fatal("expected missing runtime configuration to be rejected")
	}
}

func TestMusicQueueStarterIsolatesUnavailableProvider(t *testing.T) {
	channel, community, group := id.New(), id.New(), id.New()
	calls := &runtimeCalls{call: client.Call{CallID: channel, GroupID: group, LiveKitURL: "wss://relay.example", Token: "call-token"}}
	starter, err := newMusicQueueStarter(musicRuntimeDeps{
		Community: community, Channel: channel, Calls: calls, YouTubePCM: runtimePCM{},
		Encoder: runtimeEncoder{}, Epochs: runtimeEpochs{}, Lifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("YouTube-only configuration rejected: %v", err)
	}
	_, err = starter.OptionsFor(context.Background(), music.Track{Provider: music.ProviderSpotify, CanonicalID: "spotify:track:1234567890123456789012"})
	if err == nil {
		t.Fatal("Spotify request succeeded without a Spotify PCM provider")
	}
	if calls.channel != (id.ID{}) {
		t.Fatal("unavailable provider triggered a call before failing closed")
	}
	if _, err := starter.OptionsFor(context.Background(), music.Track{Provider: music.ProviderYouTube, CanonicalID: "https://www.youtube.com/watch?v=abcdefghijk"}); err != nil {
		t.Fatalf("configured YouTube provider unavailable: %v", err)
	}
}

func TestInstallationYouTubeStarterUsesScopedCallAndNativeMediaDependencies(t *testing.T) {
	installation, community, channel, group, instance := id.New(), id.New(), id.New(), id.New(), id.New()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/cbor")
		var response any
		switch r.URL.Path {
		case "/v1/instance":
			response = []any{[]any{uint64(3), uint64(2)}, []any{uint64(1)}, []any{uint64(1)}, instance, uint64(1), "chat.example", uint64(0), []any{uint64(0)}, uint64(1), id.New(), make([]byte, 32)}
		case "/v1/channels/" + channel.String() + "/calls":
			calls++
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer installation-session" {
				t.Errorf("call request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			}
			response = []any{channel, group, "wss://media.example/rtc", "short-lived-call-secret", []any{}, []any{uint64(64000), uint64(1000000), uint64(1)}}
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		body, err := cborx.Marshal(response)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = w.Write(body)
	}))
	defer server.Close()
	api, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instance, AllowLoopbackHTTP: true, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	callSession, err := api.Session("installation-session")
	if err != nil {
		t.Fatal(err)
	}
	epochs := &runtimeBoundEpochSource{binding: native.Binding{Instance: instance, Community: community, Installation: installation}}
	starter, err := newInstallationYouTubeQueueStarter(instance, installation, community, channel, callSession, epochs)
	if err != nil {
		t.Fatal(err)
	}
	options, err := starter.OptionsFor(context.Background(), music.Track{Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk"})
	if err != nil {
		t.Fatal(err)
	}
	options.Call, err = options.PrepareCall(context.Background())
	options.Group = options.Call.GroupID
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || options.Call.CallID != channel || options.Call.GroupID != group || options.Call.Token != "short-lived-call-secret" {
		t.Fatalf("scoped call credentials = %#v; calls=%d", options.Call, calls)
	}
	pcm, ok := options.PCM.(session.YouTubePCM)
	if !ok {
		t.Fatalf("YouTube PCM = %T", options.PCM)
	}
	source, ok := pcm.Source.(youtube.PCMStarter)
	if !ok {
		t.Fatalf("YouTube source = %T, want sandboxed PCMStarter", pcm.Source)
	}
	if source.Node == "" {
		t.Fatal("production YouTube PCM starter must enable its qualified Node JS runtime")
	}
	encoder, ok := options.Encoder.(session.RuntimeEncoder)
	if !ok || encoder.Config.Options.FFmpegPath != "ffmpeg" {
		t.Fatalf("encoder = %#v", options.Encoder)
	}
	if _, ok := options.Publisher.(session.RetainedLiveKitPublisher); !ok || options.Epochs != epochs || options.Acquire != nil {
		t.Fatalf("publisher/epoch/lease dependencies = %#v", options)
	}

	badEpochs := &runtimeBoundEpochSource{binding: native.Binding{Instance: instance, Community: community, Installation: id.New()}}
	if _, err := newInstallationYouTubeQueueStarter(instance, installation, community, channel, callSession, badEpochs); err == nil {
		t.Fatal("cross-installation native epoch source was accepted")
	}
	badInstanceEpochs := &runtimeBoundEpochSource{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}}
	if _, err := newInstallationYouTubeQueueStarter(instance, installation, community, channel, callSession, badInstanceEpochs); err == nil {
		t.Fatal("cross-instance native epoch source was accepted")
	}
	otherAPI, err := client.New(client.Config{BaseURL: server.URL, InstanceID: id.New(), AllowLoopbackHTTP: true, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := otherAPI.Session("other-instance-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newInstallationYouTubeQueueStarter(instance, installation, community, channel, otherSession, epochs); err == nil {
		t.Fatal("cross-instance call session was accepted")
	}
	if calls != 1 {
		t.Fatalf("binding rejection made an HTTP call, count=%d", calls)
	}
}

func TestMusicQueueStarterRejectsCallForDifferentChannel(t *testing.T) {
	calls := &runtimeCalls{call: client.Call{CallID: id.New(), GroupID: id.New(), LiveKitURL: "wss://relay.example", Token: "token"}}
	starter, err := newMusicQueueStarter(musicRuntimeDeps{Community: id.New(), Channel: id.New(), Calls: calls, SpotifyPCM: runtimePCM{}, YouTubePCM: runtimePCM{}, LeaseManager: testLeaseManager(t), Encoder: runtimeEncoder{}, Publisher: runtimePublisher{}, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	options, err := starter.OptionsFor(context.Background(), music.Track{Provider: music.ProviderYouTube, CanonicalID: "https://www.youtube.com/watch?v=abcdefghijk"})
	if err == nil {
		_, err = options.PrepareCall(context.Background())
	}
	if err == nil {
		t.Fatal("expected mismatched call group to be rejected")
	}
}

func TestNewMusicSupervisorRestoresPersistedStateWithoutStartingAudio(t *testing.T) {
	channel, community := id.New(), id.New()
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, rows: map[string]native.StateValue{}}
	store, err := persistence.New(n, community)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := store.Load(context.Background(), channel)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := music.Apply(initial.State, initial.Revision, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{ID: "track-1", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Track", RequesterID: "member"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), channel, initial.Revision, queued); err != nil {
		t.Fatal(err)
	}
	playing, err := music.Apply(queued, queued.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), channel, queued.Revision, playing); err != nil {
		t.Fatal(err)
	}

	starts := 0
	starter := session.QueueStarter{OptionsFor: func(context.Context, music.Track) (session.Options, error) {
		starts++
		return session.Options{}, errors.New("must not resolve track during restore")
	}}
	supervisor, err := newMusicSupervisor(context.Background(), channel, store, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	if starts != 0 {
		t.Fatalf("restore resolved/started %d tracks, want none", starts)
	}
	state := supervisor.State()
	if !state.Paused || state.Current == nil || state.Current.ID != "track-1" {
		t.Fatalf("restored state=%+v, want current track retained but paused", state)
	}
}

func TestRestoreMusicSupervisorsCreatesPausedVoiceQueuesOnly(t *testing.T) {
	community := id.New()
	voice, textChannel := id.New(), id.New()
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, rows: map[string]native.StateValue{}}
	starts := 0
	queues, err := restoreMusicQueuesIfEnabled(context.Background(), community, botMusicCapability, func(context.Context, id.ID) ([]client.Channel, error) {
		return []client.Channel{
			{ID: voice, CommunityID: community, Kind: 1},
			{ID: textChannel, CommunityID: community, Kind: 0},
		}, nil
	}, n, session.QueueStarter{OptionsFor: func(context.Context, music.Track) (session.Options, error) {
		starts++
		return session.Options{}, errors.New("provider must stay unavailable during restore")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer queues.Close()
	if queues.Len() != 1 || queues.supervisors[voice] == nil {
		t.Fatalf("restored supervisors = %d, want only voice channel %s", queues.Len(), voice)
	}
	if !queues.supervisors[voice].State().Paused || starts != 0 {
		t.Fatalf("restored queue paused=%v provider starts=%d, want paused and zero starts", queues.supervisors[voice].State().Paused, starts)
	}
}

func TestUnavailableMusicQueueStarterFailsClosed(t *testing.T) {
	_, err := unavailableMusicQueueStarter().OptionsFor(context.Background(), music.Track{
		Provider: music.ProviderYouTube, CanonicalID: "https://www.youtube.com/watch?v=abcdefghijk",
	})
	if !errors.Is(err, errMusicPCMUnavailable) {
		t.Fatalf("unavailable provider error = %v, want %v", err, errMusicPCMUnavailable)
	}
}

func TestMusicRuntimeWithoutApprovedCapabilityDoesNotLoadOrRestore(t *testing.T) {
	loads := 0
	runtime, err := restoreMusicQueuesIfEnabled(context.Background(), id.New(), 1<<1, func(context.Context, id.ID) ([]client.Channel, error) {
		loads++
		return nil, nil
	}, nil, unavailableMusicQueueStarter())
	if err != nil {
		t.Fatal(err)
	}
	if runtime != nil || loads != 0 {
		t.Fatalf("runtime=%v inventory loads=%d; disabled music capability must skip restoration", runtime, loads)
	}
}

func TestMusicQueueRegistryRetainsSupervisorsUntilCapabilityIsDisabled(t *testing.T) {
	community, installation, voice := id.New(), id.New(), id.New()
	nativeClient := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
	registry := newMusicQueueRegistry()
	channels := []client.Channel{{ID: voice, CommunityID: community, Kind: musicVoiceChannelKind}}
	owned, err := registry.Reconcile(context.Background(), installation, community, botMusicCapability, channels, nativeClient, unavailableMusicQueueStarter())
	if err != nil || !owned {
		t.Fatalf("first reconcile owned=%v err=%v, want ownership transfer", owned, err)
	}
	first := registry.runtimes[installation].queues.supervisors[voice]
	owned, err = registry.Reconcile(context.Background(), installation, community, botMusicCapability, channels, &runtimeStateNative{binding: nativeClient.binding, rows: map[string]native.StateValue{}}, unavailableMusicQueueStarter())
	if err != nil || owned {
		t.Fatalf("second reconcile owned=%v err=%v, want retained native client", owned, err)
	}
	if got := registry.runtimes[installation].queues.supervisors[voice]; got != first {
		t.Fatal("supervisor was reconstructed between polling iterations")
	}
	if got := registry.Native(installation, community); got != nativeClient {
		t.Fatal("reconcile did not expose the retained native client for reuse")
	}
	currentNative := &runtimeStateNative{binding: nativeClient.binding, rows: map[string]native.StateValue{}}
	owned, err = registry.Reconcile(context.Background(), installation, community, 0, channels, currentNative, unavailableMusicQueueStarter())
	if err != nil || owned {
		t.Fatalf("disabled reconcile owned=%v err=%v, want no ownership transfer", owned, err)
	}
	if !nativeClient.closed {
		t.Fatal("retained native client was not closed on capability loss")
	}
	if currentNative.closed {
		t.Fatal("caller-owned native client was closed by disabled reconcile")
	}
	if _, err := registry.ApplyPersistedSnapshot(context.Background(), installation, community, voice, music.InitialState(), 0, false); err == nil {
		t.Fatal("snapshot bridge remained available after capability loss")
	}
	if _, err := first.Apply(context.Background(), first.State().Revision, music.Operation{Kind: music.OpPause}); !errors.Is(err, music.ErrSupervisorClosed) {
		t.Fatalf("supervisor after capability loss error=%v, want closed", err)
	}
}

func TestMusicQueueRegistryClosesRuntimesForRemovedInstallations(t *testing.T) {
	community, installation, voice := id.New(), id.New(), id.New()
	nativeClient := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
	registry := newMusicQueueRegistry()
	_, err := registry.Reconcile(context.Background(), installation, community, botMusicCapability, []client.Channel{{ID: voice, CommunityID: community, Kind: musicVoiceChannelKind}}, nativeClient, unavailableMusicQueueStarter())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RetainOnly(nil); err != nil {
		t.Fatal(err)
	}
	if !nativeClient.closed || len(registry.runtimes) != 0 {
		t.Fatalf("removed installation cleanup closed=%v retained=%d", nativeClient.closed, len(registry.runtimes))
	}
}

func TestMusicSupervisorSyncFailureKeepsNativeHelperUsableForOtherInteractions(t *testing.T) {
	community, installation, voiceA, voiceB := id.New(), id.New(), id.New(), id.New()
	nativeClient := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
	registry := newMusicQueueRegistry()
	channels := []client.Channel{{ID: voiceA, CommunityID: community, Kind: musicVoiceChannelKind}}
	if _, err := registry.Reconcile(context.Background(), installation, community, botMusicCapability, channels, nativeClient, unavailableMusicQueueStarter()); err != nil {
		t.Fatal(err)
	}

	channels = append(channels, client.Channel{ID: voiceB, CommunityID: community, Kind: musicVoiceChannelKind})
	if _, err := registry.Reconcile(context.Background(), installation, community, botMusicCapability, channels, nativeClient, session.QueueStarter{}); err == nil {
		t.Fatal("supervisor sync succeeded with unavailable music dependencies")
	}
	if nativeClient.closed {
		t.Fatal("music sync failure closed the helper needed by non-music interactions")
	}
	if got := registry.Native(installation, community); got != nativeClient {
		t.Fatal("music sync failure discarded the live helper")
	}
	if _, err := nativeClient.StateGet(context.Background(), 0, []byte("still-live")); err != nil {
		t.Fatalf("native helper unusable after music failure: %v", err)
	}
	if _, ok := registry.QueueStore(installation, community); ok {
		t.Fatal("failed music runtime retained an interaction queue store")
	}
	if _, err := registry.Reconcile(context.Background(), installation, community, botMusicCapability, channels, nativeClient, unavailableMusicQueueStarter()); err != nil {
		t.Fatalf("music runtime did not recover on a later reconcile: %v", err)
	}
	if _, ok := registry.QueueStore(installation, community); !ok {
		t.Fatal("successful later reconcile did not restore the music queue store")
	}
}

func TestMusicQueueOwnsSeparateRetainedConnectionsPerChannel(t *testing.T) {
	community := id.New()
	var pools []*livekit.Pool
	var starters []session.QueueStarter
	for range 2 {
		channel, group := id.New(), id.New()
		starter, err := newMusicQueueStarter(musicRuntimeDeps{
			Community: community, Channel: channel,
			Calls:      &runtimeCalls{call: client.Call{CallID: channel, GroupID: group, LiveKitURL: "wss://relay.example", Token: "call-token"}},
			YouTubePCM: runtimePCM{}, Encoder: runtimeEncoder{}, Epochs: runtimeEpochs{}, Lifetime: time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer starter.Close()
		options, err := starter.OptionsFor(context.Background(), music.Track{Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk"})
		if err != nil {
			t.Fatal(err)
		}
		publisher, ok := options.Publisher.(session.RetainedLiveKitPublisher)
		if !ok || publisher.Pool == nil || starter.ResetConnection == nil || starter.CloseConnection == nil {
			t.Fatal("missing owned connection lifecycle")
		}
		pools = append(pools, publisher.Pool)
		starters = append(starters, starter)
	}
	if pools[0] == pools[1] {
		t.Fatal("channels share a retained connection")
	}
	if err := starters[0].Close(); err != nil {
		t.Fatal(err)
	}
	if err := pools[0].Reset(); err == nil {
		t.Fatal("closed owner allowed reconnect")
	}
	if err := pools[1].Reset(); err != nil {
		t.Fatalf("closing one channel affected another: %v", err)
	}
}

func TestPrivateCommandHelperSurvivesMusicDisableAndClosesOnInstallationRemoval(t *testing.T) {
	for _, musicEnabled := range []bool{false, true} {
		name := "without music"
		if musicEnabled {
			name = "after music capability loss"
		}
		t.Run(name, func(t *testing.T) {
			community, installation := id.New(), id.New()
			source := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
			registry := newMusicQueueRegistry()
			t.Cleanup(func() { _ = registry.Close() })
			if musicEnabled {
				if _, err := registry.Reconcile(t.Context(), installation, community, botMusicCapability, []client.Channel{{ID: id.New(), CommunityID: community, Kind: musicVoiceChannelKind}}, source, unavailableMusicQueueStarter()); err != nil {
					t.Fatal(err)
				}
			}
			if err := registry.RetainInteractions(installation, community, source); err != nil {
				t.Fatal(err)
			}
			if _, err := registry.Reconcile(t.Context(), installation, community, 0, nil, source, unavailableMusicQueueStarter()); err != nil {
				t.Fatal(err)
			}
			if err := registry.DisableMusic(installation); err != nil {
				t.Fatal(err)
			}
			if source.closed || registry.Native(installation, community) != source {
				t.Fatal("disabling music stopped the helper needed by private commands")
			}
			if _, ok := registry.QueueStore(installation, community); ok {
				t.Fatal("music queue store available without music capability")
			}
			if err := registry.RetainOnly(nil); err != nil {
				t.Fatal(err)
			}
			if !source.closed || registry.Native(installation, community) != nil {
				t.Fatal("removed installation retained its command helper")
			}
		})
	}
}

type eofRuntimePublisherStarter struct{ publisher *liveRuntimePublisher }

func (p eofRuntimePublisherStarter) Start(context.Context, client.Call, id.ID, livekit.EpochSource, session.Encoder) (session.Publisher, error) {
	return p.publisher, nil
}

func TestAutomaticPlaybackFailureLogsOnce(t *testing.T) {
	community, channel := id.New(), id.New()
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, rows: map[string]native.StateValue{}}
	store, err := persistence.New(n, community)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	publisher := &liveRuntimePublisher{done: make(chan struct{})}
	calls := 0
	starter := session.QueueStarter{OptionsFor: func(context.Context, music.Track) (session.Options, error) {
		calls++
		if calls > 1 {
			return session.Options{}, &youtube.ProcessError{Cause: youtube.ErrExtractFailed, Stderr: "[youtube] [redacted]: failed"}
		}
		group := id.New()
		return session.Options{Community: community, Channel: channel, Provider: music.ProviderYouTube, Track: "abcdefghijk",
			Call: client.Call{CallID: channel, GroupID: group, LiveKitURL: "wss://example.invalid", Token: "test"}, Group: group,
			PCM: liveRuntimePCM{}, Encoder: liveRuntimeEncoderStarter{}, Publisher: eofRuntimePublisherStarter{publisher}, Epochs: runtimeEpochs{}}, nil
	}}
	supervisor, err := newMusicSupervisor(t.Context(), channel, store, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	tracks := []music.Track{{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}, {ID: "b", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "B", RequesterID: "member"}}
	state, err := supervisor.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	close(publisher.done)
	deadline := time.Now().Add(time.Second)
	for !supervisor.State().Paused && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !supervisor.State().Paused {
		t.Fatal("automatic startup failure did not pause queue")
	}
	if strings.Count(logs.String(), `"msg":"bot music playback start failed"`) != 1 || strings.Count(logs.String(), `"msg":"bot music playback ended"`) != 1 {
		t.Fatalf("logs=%s", logs.String())
	}
}

func TestSnapshotCallbackLeavesFailureLoggingToInteraction(t *testing.T) {
	var logs bytes.Buffer
	_, err := musicSnapshotCommitCallback(nil, slog.New(slog.NewJSONHandler(&logs, nil)))(t.Context(), id.New(), id.New(), id.New(), music.InitialState(), 1, true)
	if err == nil || logs.Len() != 0 {
		t.Fatalf("error=%v logs=%s", err, logs.String())
	}
}

type boundaryPublisher struct{ started chan *liveRuntimePublisher }

func (p boundaryPublisher) Start(context.Context, client.Call, id.ID, livekit.EpochSource, session.Encoder) (session.Publisher, error) {
	track := &liveRuntimePublisher{done: make(chan struct{})}
	p.started <- track
	return track, nil
}

type boundaryCalls struct {
	probe *probeIntakePriority
	call  client.Call
}

func (c boundaryCalls) StartCall(ctx context.Context, _ id.ID) (client.Call, error) {
	return c.call, c.probe.probe(ctx)
}

func TestMusicSupervisorAutoAdvanceDoesNotInheritCooperativeStop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/cbor")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	api, err := client.New(client.Config{BaseURL: server.URL, InstanceID: id.New(), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	community, channel, group := id.New(), id.New(), id.New()
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, rows: map[string]native.StateValue{}}
	store, err := persistence.New(n, community)
	if err != nil {
		t.Fatal(err)
	}
	probe := &probeIntakePriority{api: api}
	published := make(chan *liveRuntimePublisher, 2)
	starter, err := newMusicQueueStarter(musicRuntimeDeps{Community: community, Channel: channel,
		Calls:         boundaryCalls{probe, client.Call{CallID: channel, GroupID: group, LiveKitURL: "wss://example.invalid", Token: "test"}},
		JoinCallGroup: probe.probe, YouTubePCM: liveRuntimePCM{}, Encoder: liveRuntimeEncoderStarter{}, Publisher: boundaryPublisher{published}, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var pending atomic.Bool
	sweepCtx := client.WithCooperativeStop(t.Context(), pending.Load)
	//nolint:staticcheck // SA1012: deliberately invalid lifetime tests rejection before context normalization.
	if _, err := newMusicSupervisor(nil, channel, store, starter); err == nil {
		t.Fatal("nil supervisor lifetime was accepted")
	}
	supervisor, err := newMusicSupervisor(sweepCtx, channel, store, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	tracks := []music.Track{{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}, {ID: "b", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "B", RequesterID: "member"}}
	state, err := supervisor.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	first := <-published
	pending.Store(true)
	close(first.done)
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("pending command stopped automatic JoinCallGroup/StartCall")
	}
	state = supervisor.State()
	if state.Paused || state.Current == nil || state.Current.ID != "b" {
		t.Fatalf("queue did not advance: %+v", state)
	}
}

type failingRuntimePCM struct{ err error }

func (p failingRuntimePCM) Start(context.Context, string) (io.ReadCloser, error) { return nil, p.err }

func TestMusicQueueStarterProviderFailsBeforeJoin(t *testing.T) {
	for _, provider := range []music.Provider{music.ProviderSpotify, music.ProviderYouTube} {
		t.Run(string(provider), func(t *testing.T) {
			community, channel := id.New(), id.New()
			calls := &runtimeCalls{call: client.Call{CallID: channel, GroupID: id.New(), LiveKitURL: "wss://media.example", Token: "secret"}}
			joins := 0
			failure := spotify.ErrPlaybackFailed
			starter, err := newMusicQueueStarter(musicRuntimeDeps{Community: community, Channel: channel, Calls: calls,
				JoinCallGroup: func(context.Context) error { joins++; return nil },
				SpotifyPCM:    failingRuntimePCM{failure}, YouTubePCM: failingRuntimePCM{failure}, LeaseManager: testLeaseManager(t),
				Encoder: liveRuntimeEncoderStarter{}, Publisher: runtimePublisher{}, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			canonical := "spotify:track:0123456789012345678901"
			if provider == music.ProviderYouTube {
				canonical = "abcdefghijk"
			}
			_, err = starter.StartExplicit(context.Background(), music.Track{Provider: provider, CanonicalID: canonical})
			if !errors.Is(err, failure) {
				t.Fatalf("start = %v", err)
			}
			if joins != 0 || calls.starts != 0 {
				t.Fatalf("provider failed after joins=%d calls=%d; want zero", joins, calls.starts)
			}
		})
	}
}

func TestMusicResumeSkipsBrokenProviderHead(t *testing.T) {
	for _, repeat := range []music.RepeatMode{music.RepeatOff, music.RepeatCurrent, music.RepeatQueue} {
		t.Run(string(rune('0'+repeat)), func(t *testing.T) {
			installation, community, channel := id.New(), id.New(), id.New()
			n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: map[string]native.StateValue{}}
			store, err := persistence.New(n, community)
			if err != nil {
				t.Fatal(err)
			}
			calls := &runtimeCalls{call: client.Call{CallID: channel, GroupID: id.New(), LiveKitURL: "wss://media.example", Token: "secret"}}
			joins, lines := 0, 0
			starter, err := newMusicQueueStarter(musicRuntimeDeps{Community: community, Channel: channel, Calls: calls,
				JoinCallGroup: func(context.Context) error { joins++; return nil }, SpotifyPCM: failingRuntimePCM{fmt.Errorf("%w: playback/provider_unavailable", spotify.ErrPlaybackFailed)},
				YouTubePCM: liveRuntimePCM{}, LeaseManager: testLeaseManager(t), Encoder: liveRuntimeEncoderStarter{},
				Publisher: &liveRuntimePublisherStarter{starts: map[id.ID]int{}}, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			starter.OnSkipped = func(_ context.Context, skips []music.SkippedTrack) { lines += len(skips) }
			supervisor, err := newMusicSupervisor(context.Background(), channel, store, starter)
			if err != nil {
				t.Fatal(err)
			}
			defer supervisor.Close()
			tracks := []music.Track{{ID: "broken", Provider: music.ProviderSpotify, CanonicalID: "spotify:track:0123456789012345678901", Title: "Broken", RequesterID: "member"}, {ID: "working", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Working", RequesterID: "member"}}
			state, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
			if err != nil {
				t.Fatal(err)
			}
			state.Repeat = repeat
			saved, err := store.Save(context.Background(), channel, 0, state)
			if err != nil {
				t.Fatal(err)
			}
			state, err = music.Apply(saved.State, saved.Revision, music.Operation{Kind: music.OpResume})
			if err != nil {
				t.Fatal(err)
			}
			saved, err = store.Save(context.Background(), channel, saved.Revision, state)
			if err != nil {
				t.Fatal(err)
			}
			runtime := &musicQueueRuntime{store: store, supervisors: map[id.ID]*music.Supervisor{channel: supervisor}}
			revision, err := runtime.ApplyPersistedSnapshot(context.Background(), channel, saved.State, saved.Revision, true)
			if err != nil {
				t.Fatal(err)
			}
			got := supervisor.State()
			if got.Current == nil || got.Current.ID != "working" || got.Paused || len(got.Queue) != 0 || joins != 1 || calls.starts != 1 || lines != 1 || revision != saved.Revision+1 {
				t.Fatalf("state=%+v joins=%d calls=%d lines=%d revision=%d", got, joins, calls.starts, lines, revision)
			}
			durable, err := store.Load(context.Background(), channel)
			if err != nil {
				t.Fatal(err)
			}
			if durable.State.Current == nil || durable.State.Current.ID != "working" || durable.Revision != revision {
				t.Fatalf("durable=%+v", durable.State)
			}
		})
	}
}

func TestMusicStartStagesFailOnce(t *testing.T) {
	for _, stage := range []string{"provider_check", "spotify_lease", "provider_pcm", "encoder", "call_group_join", "call_token", "call_fields"} {
		t.Run(stage, func(t *testing.T) {
			channel, community := id.New(), id.New()
			failure := errors.New("sensitive provider URL https://private.example/token")
			calls := &runtimeCalls{call: client.Call{CallID: channel, GroupID: id.New(), LiveKitURL: "wss://media.example", Token: "secret-token"}}
			leases := testLeaseManager(t)
			deps := musicRuntimeDeps{Community: community, Channel: channel, Calls: calls, SpotifyPCM: liveRuntimePCM{}, YouTubePCM: liveRuntimePCM{}, LeaseManager: leases, Encoder: liveRuntimeEncoderStarter{}, Publisher: runtimePublisher{}, Epochs: runtimeEpochs{}, Lifetime: time.Minute}
			switch stage {
			case "provider_check":
				deps.SpotifyPCM = nil
			case "spotify_lease":
				lease, err := leases.Acquire(community, channel)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Release()
			case "provider_pcm":
				deps.SpotifyPCM = failingRuntimePCM{failure}
			case "encoder":
				deps.Encoder = runtimeEncoder{}
			case "call_group_join":
				deps.JoinCallGroup = func(context.Context) error { return failure }
			case "call_token":
				calls.err = failure
			case "call_fields":
				calls.call.CallID = id.New()
			}
			starter, err := newMusicQueueStarter(deps)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			starter.Logger = slog.New(slog.NewJSONHandler(&output, nil)).With("installation_id", id.New().String(), "device_id", id.New().String(), "channel_id", channel.String())
			_, err = starter.StartExplicit(context.Background(), music.Track{Provider: music.ProviderSpotify, CanonicalID: "spotify:track:0123456789012345678901"})
			if err == nil {
				t.Fatal("failure accepted")
			}
			count, warnings := 0, 0
			for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
				var row map[string]any
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					t.Fatal(err)
				}
				if row["stage"] == stage {
					count++
					if row["level"] != "WARN" {
						t.Errorf("failure row=%v", row)
					}
				}
				if row["level"] == "WARN" {
					warnings++
				}
				for _, field := range []string{"installation_id", "device_id", "group_id", "channel_id", "class"} {
					if row[field] == nil {
						t.Errorf("missing %s: %s", field, line)
					}
				}
			}
			if count != 1 || warnings != 1 || strings.Contains(output.String(), "secret-token") || strings.Contains(output.String(), "private.example") {
				t.Fatalf("stage count=%d warnings=%d logs=%s", count, warnings, output.String())
			}
		})
	}
}

func TestMusicResumeCapsBrokenTracksWithoutRepeating(t *testing.T) {
	for _, repeat := range []music.RepeatMode{music.RepeatOff, music.RepeatCurrent, music.RepeatQueue} {
		t.Run(string(rune('0'+repeat)), func(t *testing.T) {
			community, channel := id.New(), id.New()
			n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, rows: map[string]native.StateValue{}}
			store, err := persistence.New(n, community)
			if err != nil {
				t.Fatal(err)
			}
			calls := &runtimeCalls{}
			starter, err := newMusicQueueStarter(musicRuntimeDeps{Community: community, Channel: channel, Calls: calls, SpotifyPCM: failingRuntimePCM{fmt.Errorf("%w: playback/provider_unavailable", spotify.ErrPlaybackFailed)}, LeaseManager: testLeaseManager(t), Encoder: liveRuntimeEncoderStarter{}, Publisher: runtimePublisher{}, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			var lines []string
			starter.OnSkipped = func(_ context.Context, skips []music.SkippedTrack) {
				for range skips {
					lines = append(lines, botstrings.MusicSkipped("", "provider unavailable"))
				}
			}
			supervisor, err := newMusicSupervisor(context.Background(), channel, store, starter)
			if err != nil {
				t.Fatal(err)
			}
			defer supervisor.Close()
			tracks := []music.Track{}
			for _, name := range []string{"one", "two", "three"} {
				tracks = append(tracks, music.Track{ID: name, Provider: music.ProviderSpotify, CanonicalID: "spotify:track:0123456789012345678901", Title: name, RequesterID: "member"})
			}
			state, err := supervisor.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
			if err != nil {
				t.Fatal(err)
			}
			state, err = supervisor.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpSetRepeat, Repeat: repeat})
			if err != nil {
				t.Fatal(err)
			}
			state, err = supervisor.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume})
			if err != nil {
				t.Fatal(err)
			}
			if state.Current == nil || state.Current.ID != "three" || len(state.Queue) != 0 || !state.Paused || len(lines) != 2 || calls.starts != 0 {
				t.Fatalf("state=%+v lines=%v calls=%d", state, lines, calls.starts)
			}
			durable, err := store.Load(context.Background(), channel)
			if err != nil {
				t.Fatal(err)
			}
			if durable.State.Current == nil || durable.State.Current.ID != "three" || len(durable.State.Queue) != 0 || !durable.State.Paused {
				t.Fatalf("durable=%+v", durable.State)
			}
			if _, err := supervisor.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
				t.Fatal(err)
			}
			if len(lines) != 3 {
				t.Fatal("next Resume did not skip the remaining failed item")
			}
		})
	}
}

func TestMusicNoticeHolderIsSharedAcrossDisableAndReenable(t *testing.T) {
	registry := newMusicQueueRegistry()
	installation := id.New()
	first := registry.SkipNotices(installation)
	if err := registry.Disable(installation); err != nil {
		t.Fatal(err)
	}
	second := registry.SkipNotices(installation)
	if first != second {
		t.Fatal("Disable split handler and starter notice holders")
	}
	if err := registry.DisableMusic(installation); err != nil {
		t.Fatal(err)
	}
	if registry.SkipNotices(installation) != first {
		t.Fatal("re-enable replaced the shared holder")
	}
}

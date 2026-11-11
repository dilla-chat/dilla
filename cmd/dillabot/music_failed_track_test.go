//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/livekit"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/music/session"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	botstrings "github.com/dilla-chat/dilla/internal/bot/strings"
	"github.com/dilla-chat/dilla/internal/id"
)

// v0.1.0-bot2.2 Task 7, end to end through a sealed /play, the real supervisor, starter and persisted queue: a failed
// track leaves the queue and the answer says so; a start whose own leaf was missing rejoins once.

// failedTrackResolver resolves each query to its tracks (a fresh queue id is assigned by the handler).
type failedTrackResolver struct{ tracks map[string][]music.Track }

func (r failedTrackResolver) Resolve(_ context.Context, input string) ([]music.Track, error) {
	return append([]music.Track(nil), r.tracks[input]...), nil
}

// failedTrackPCM refuses the video ids it is told to, as a provider's HTTP 403.
type failedTrackPCM struct{ refuse map[string]bool }

func (p failedTrackPCM) Start(_ context.Context, url string) (io.ReadCloser, error) {
	if p.refuse[strings.TrimPrefix(url, "https://www.youtube.com/watch?v=")] {
		return nil, &client.APIError{Status: 403}
	}
	return io.NopCloser(strings.NewReader("bounded test PCM")), nil
}

// leafPublisher fails its first failures publications as a media epoch without the Bot's own leaf.
type leafPublisher struct {
	failures int32
	attempts atomic.Int32
}

func (p *leafPublisher) Start(context.Context, client.Call, id.ID, livekit.EpochSource, session.Encoder) (session.Publisher, error) {
	if p.attempts.Add(1) <= p.failures {
		return nil, &music.StartError{Stage: "media_epoch", Cause: livekit.ErrLeafMissing}
	}
	return &liveRuntimePublisher{done: make(chan struct{})}, nil
}

type failedTrackRig struct {
	community, channel id.ID
	store              *persistence.Store
	dispatcher         *service.SealedDispatcher
	joins              atomic.Int32
	forcedJoins        atomic.Int32
	mu                 sync.Mutex
	notices            []string
}

func newFailedTrackRig(t *testing.T, resolver failedTrackResolver, pcm failedTrackPCM, publisher session.PublisherStarter) *failedTrackRig {
	t.Helper()
	r := &failedTrackRig{community: id.New(), channel: id.New()}
	installation, group := id.New(), id.New()
	calls := &coldPlayCalls{call: client.Call{CallID: r.channel, GroupID: group, LiveKitURL: "wss://media.example", Token: "token"}}
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: r.community, Installation: installation}, rows: map[string]native.StateValue{}}
	var err error
	if r.store, err = persistence.New(n, r.community); err != nil {
		t.Fatal(err)
	}
	starter, err := newMusicQueueStarter(musicRuntimeDeps{Community: r.community, Channel: r.channel, Calls: calls,
		JoinCallGroup: func(ctx context.Context) error {
			r.joins.Add(1)
			if music.ForcedRejoin(ctx) {
				r.forcedJoins.Add(1)
			}
			return nil
		},
		YouTubePCM: pcm, Encoder: liveRuntimeEncoderStarter{}, Publisher: publisher, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	starter.OnSkipped = func(_ context.Context, skips []music.SkippedTrack) {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, skip := range skips {
			r.notices = append(r.notices, skip.Track.ID)
		}
	}
	supervisor, err := newMusicSupervisor(t.Context(), r.channel, r.store, starter)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	queues := &musicQueueRuntime{store: r.store, community: r.community, supervisors: map[id.ID]*music.Supervisor{r.channel: supervisor}}
	registry := &musicQueueRegistry{runtimes: map[id.ID]*retainedMusicRuntime{installation: {community: r.community, queues: queues, native: n}}}
	h := &service.MusicInteractions{Store: r.store, Authority: djAuthority{call: service.MusicCallBinding{CallID: r.channel, ChannelID: r.channel, GroupID: group, Active: true}, dj: true},
		InstallationID: installation, CommunityID: r.community, Resolver: resolver,
		CurrentState: registry.CurrentState, OnPersistedSnapshot: musicSnapshotCommitCallback(registry, nil),
		OnImmediateControl: registry.applyImmediateControl,
		OnPlayOpened:       registry.warmMusicCall, OnPlay: registry.playPersistedSnapshot}
	r.dispatcher = &service.SealedDispatcher{Community: r.community, Handlers: h.Handlers()}
	return r
}

// play sends a sealed /play and returns its final answer's text.
func (r *failedTrackRig) play(t *testing.T, query string) string {
	t.Helper()
	req := protocol.SealedRequest{RequestID: id.New(), CommunityID: r.community, ChannelID: r.channel, Operation: uint64(protocol.OpMusicEnqueue), Args: []any{query},
		SentAt: uint64(time.Now().Unix()), UserID: id.New(), DeviceID: id.New()}
	final := ""
	r.dispatcher.Dispatch(t.Context(), req, func(a intake.Answer) {
		if a.Final {
			final = string(a.Body)
		}
	})
	return final
}

func (r *failedTrackRig) titles(t *testing.T) (current string, queue []string) {
	t.Helper()
	snapshot, err := r.store.Load(t.Context(), r.channel)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State.Current != nil {
		current = snapshot.State.Current.Title
	}
	for _, track := range snapshot.State.Queue {
		queue = append(queue, track.Title)
	}
	return current, queue
}

func (r *failedTrackRig) noticeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.notices)
}

func ytTrack(video, title string) music.Track {
	return music.Track{Provider: music.ProviderYouTube, CanonicalID: video, Title: title, Artist: "Band", DurationMS: 30_000}
}

func TestFailedStartRemovesItem(t *testing.T) {
	r := newFailedTrackRig(t,
		failedTrackResolver{tracks: map[string][]music.Track{"playlist": {ytTrack("refused0000", "Refused"), ytTrack("playable000", "Playable")}}},
		failedTrackPCM{refuse: map[string]bool{"refused0000": true}}, &coldPlayPublisher{})
	answer := r.play(t, "playlist")
	if answer != botstrings.MusicTrackRemoved()+" Now playing: Playable — Band." {
		t.Fatalf("answer = %q, want the removed track named as removed and the next one playing", answer)
	}
	if current, queue := r.titles(t); current != "Playable" || len(queue) != 0 {
		t.Fatalf("persisted current=%q queue=%v, want the refused track gone", current, queue)
	}
	if n := r.noticeCount(); n != 0 {
		t.Fatalf("notices = %d: the answer already says the track was removed", n)
	}
}

func TestPlayAfterFailureStartsNewTrack(t *testing.T) {
	r := newFailedTrackRig(t,
		failedTrackResolver{tracks: map[string][]music.Track{"a": {ytTrack("refused0000", "A")}, "b": {ytTrack("playable000", "B")}}},
		failedTrackPCM{refuse: map[string]bool{"refused0000": true}}, &coldPlayPublisher{})
	if answer := r.play(t, "a"); answer != botstrings.MusicTrackRemoved() {
		t.Fatalf("/play A = %q", answer)
	}
	if current, queue := r.titles(t); current != "" || len(queue) != 0 {
		t.Fatalf("after A failed: current=%q queue=%v, want A gone", current, queue)
	}
	if answer := r.play(t, "b"); answer != "Now playing: B — Band." {
		t.Fatalf("/play B = %q, want B playing (not A resumed)", answer)
	}
	if current, queue := r.titles(t); current != "B" || len(queue) != 0 {
		t.Fatalf("after /play B: current=%q queue=%v", current, queue)
	}
}

func TestMediaEpochRetriesAfterRejoin(t *testing.T) {
	tracks := failedTrackResolver{tracks: map[string][]music.Track{"song": {ytTrack("playable000", "Song")}}}
	t.Run("the retry after a rejoin plays", func(t *testing.T) {
		publisher := &leafPublisher{failures: 1}
		r := newFailedTrackRig(t, tracks, failedTrackPCM{}, publisher)
		if answer := r.play(t, "song"); answer != "Now playing: Song — Band." {
			t.Fatalf("answer = %q", answer)
		}
		if publisher.attempts.Load() != 2 || r.forcedJoins.Load() != 1 {
			t.Fatalf("publications=%d forced joins=%d, want one rejoin and one retry", publisher.attempts.Load(), r.forcedJoins.Load())
		}
	})
	t.Run("a second failure removes the track, with no third try", func(t *testing.T) {
		publisher := &leafPublisher{failures: 10}
		r := newFailedTrackRig(t, tracks, failedTrackPCM{}, publisher)
		if answer := r.play(t, "song"); answer != botstrings.MusicTrackRemoved() {
			t.Fatalf("answer = %q", answer)
		}
		if publisher.attempts.Load() != 2 || r.forcedJoins.Load() != 1 {
			t.Fatalf("publications=%d forced joins=%d, want exactly one retry", publisher.attempts.Load(), r.forcedJoins.Load())
		}
		if current, queue := r.titles(t); current != "" || len(queue) != 0 {
			t.Fatalf("current=%q queue=%v, want the track removed once", current, queue)
		}
	})
}

// countingSync counts source syncs and answers err.
type countingSync struct {
	calls atomic.Int32
	err   error
}

func (s *countingSync) Sync(context.Context, *client.Session, native.GroupExpected) (service.SourceSyncResult, error) {
	s.calls.Add(1)
	return service.SourceSyncResult{}, s.err
}

func TestCatchUpClosedHelperEndsAtOnce(t *testing.T) {
	group := id.New()
	t.Run("the installation's current helper is closed", func(t *testing.T) {
		sync := &countingSync{}
		catchUp := musicCallCatchUp{sync: sync, session: &client.Session{}, helperNow: func() error { return helper.ErrClosed }}
		err := catchUp.run(t.Context(), group)
		if !errors.Is(err, livekit.ErrCallCatchUpTerminal) || helper.Class(err) != "closed" || sync.calls.Load() != 0 {
			t.Fatalf("err=%v syncs=%d, want a terminal closed failure before any sync", err, sync.calls.Load())
		}
	})
	t.Run("the helper closes during the catch-up", func(t *testing.T) {
		sync := &countingSync{err: helper.ErrClosed}
		catchUp := musicCallCatchUp{sync: sync, session: &client.Session{}, helperNow: func() error { return nil }}
		if err := catchUp.run(t.Context(), group); !errors.Is(err, livekit.ErrCallCatchUpTerminal) || helper.Class(err) != "closed" {
			t.Fatalf("err=%v, want a terminal closed failure", err)
		}
	})
	t.Run("the registered helper is the current one", func(t *testing.T) {
		n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: id.New(), Installation: id.New()}, rows: map[string]native.StateValue{}}
		other := &runtimeStateNative{binding: n.binding, rows: map[string]native.StateValue{}}
		registry := &musicQueueRegistry{runtimes: map[id.ID]*retainedMusicRuntime{n.binding.Installation: {community: n.binding.Community, native: n}}}
		if err := musicHelperNow(registry, n.binding.Installation, n.binding.Community, n); err != nil {
			t.Fatalf("the registered usable helper = %v", err)
		}
		if err := musicHelperNow(registry, n.binding.Installation, n.binding.Community, other); !errors.Is(err, helper.ErrClosed) {
			t.Fatalf("a superseded helper = %v, want closed", err)
		}
		if err := musicHelperNow(registry, id.New(), n.binding.Community, n); !errors.Is(err, helper.ErrClosed) {
			t.Fatalf("no registered helper = %v, want closed", err)
		}
	})
}

// staleMemberCalls: dillad still answers the Bot's member-only route, so the membership check says "current".
type staleMemberCalls struct{ musicJoinCallsFake }

func (*staleMemberCalls) GroupPage(context.Context, id.ID, string, uint64, uint64) (client.Page, error) {
	return client.Page{}, nil
}

// staleMemberCore holds the call group as a member, but its epoch no longer has the Bot's own leaf.
type staleMemberCore struct{ musicJoinCoreFake }

func (c *staleMemberCore) Groups(context.Context) ([]native.GroupInfo, error) {
	return []native.GroupInfo{{ID: c.group, State: 2, Epoch: 3}}, nil
}
func (c *staleMemberCore) CallMediaEpoch(context.Context, id.ID) (native.CallMediaEpoch, error) {
	if c.forgets == 0 {
		return native.CallMediaEpoch{}, native.ErrCallEpochOwnLeafMissing
	}
	return native.CallMediaEpoch{}, nil // joined again: the new leaf is in the epoch
}

func TestForcedRejoinJoinsAgainWhenTheOwnLeafIsMissing(t *testing.T) {
	join := func(ctx context.Context) (*staleMemberCore, error) {
		calls := &staleMemberCalls{musicJoinCallsFake{channel: id.New(), group: id.New()}}
		core := &staleMemberCore{musicJoinCoreFake{group: calls.group}}
		return core, musicCallGroupJoinAttempt(ctx, calls, core, allowMusicJoinTrust{}, id.New(), id.New(), calls.channel, 1)
	}
	core, err := join(t.Context())
	if err == nil || core.joins != 0 {
		t.Fatalf("an ordinary join of a member = %v joins=%d, want the member gate unchanged", err, core.joins)
	}
	core, err = join(music.WithForcedRejoin(t.Context()))
	if err != nil || core.forgets != 1 || core.joins != 1 {
		t.Fatalf("a forced rejoin = %v forgets=%d joins=%d, want the stale group forgotten and joined again", err, core.forgets, core.joins)
	}
}

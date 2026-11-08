//go:build linux

package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/livekit"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/music/session"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 19: a cold /play joins the call and resolves the track at the same time, answers "Searching…" at once
// and "Now playing" once the first frame is published.

type coldPlayResolver struct {
	delay time.Duration
	calls atomic.Int32
}

func (r *coldPlayResolver) Resolve(ctx context.Context, _ string) ([]music.Track, error) {
	r.calls.Add(1)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(r.delay):
	}
	return []music.Track{{ID: "resolved", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Song", Artist: "Band", DurationMS: 30_000}}, nil
}

// coldPlayCalls is the Bot's call token source; safe for the warm goroutine and the start.
type coldPlayCalls struct {
	call   client.Call
	starts atomic.Int32
}

func (c *coldPlayCalls) StartCall(context.Context, id.ID) (client.Call, error) {
	c.starts.Add(1)
	return c.call, nil
}

type coldPlayPublisher struct {
	mu        sync.Mutex
	published []time.Time
}

func (p *coldPlayPublisher) Start(context.Context, client.Call, id.ID, livekit.EpochSource, session.Encoder) (session.Publisher, error) {
	p.mu.Lock()
	p.published = append(p.published, time.Now())
	p.mu.Unlock()
	return &liveRuntimePublisher{done: make(chan struct{})}, nil
}

func (p *coldPlayPublisher) first() (time.Time, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.published) == 0 {
		return time.Time{}, 0
	}
	return p.published[0], len(p.published)
}

type djAuthority struct {
	call service.MusicCallBinding
	dj   bool
	djs  *atomic.Int32
}

func (a djAuthority) CurrentCall(context.Context, id.ID) (service.MusicCallBinding, error) {
	return a.call, nil
}
func (djAuthority) CanUse(context.Context, service.InteractionActor, service.MusicCallBinding) (bool, error) {
	return true, nil
}
func (a djAuthority) CanDJ(context.Context, service.InteractionActor, service.MusicCallBinding) (bool, error) {
	if a.djs != nil {
		a.djs.Add(1)
	}
	return a.dj, nil
}
func (a djAuthority) CanEmergencyStop(context.Context, service.InteractionActor, service.MusicCallBinding) (bool, error) {
	return a.dj, nil
}

type timedAnswer struct {
	intake.Answer
	at time.Time
}

type coldPlayRig struct {
	installation, community, channel id.ID
	calls                            *coldPlayCalls
	publisher                        *coldPlayPublisher
	resolver                         *coldPlayResolver
	joins                            atomic.Int32
	joinStarted                      atomic.Int64
	djChecks                         atomic.Int32
	dispatcher                       *service.SealedDispatcher
}

func newColdPlayRig(t *testing.T, join, resolve time.Duration, dj bool) *coldPlayRig {
	t.Helper()
	r := &coldPlayRig{installation: id.New(), community: id.New(), channel: id.New(), publisher: &coldPlayPublisher{}, resolver: &coldPlayResolver{delay: resolve}}
	group := id.New()
	r.calls = &coldPlayCalls{call: client.Call{CallID: r.channel, GroupID: group, LiveKitURL: "wss://media.example", Token: "token"}}
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: r.community, Installation: r.installation}, rows: map[string]native.StateValue{}}
	store, err := persistence.New(n, r.community)
	if err != nil {
		t.Fatal(err)
	}
	starter, err := newMusicQueueStarter(musicRuntimeDeps{Community: r.community, Channel: r.channel, Calls: r.calls,
		JoinCallGroup: func(ctx context.Context) error {
			r.joins.Add(1)
			r.joinStarted.CompareAndSwap(0, time.Now().UnixNano())
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(join):
			}
			return nil
		},
		YouTubePCM: liveRuntimePCM{}, Encoder: liveRuntimeEncoderStarter{}, Publisher: r.publisher, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := newMusicSupervisor(t.Context(), r.channel, store, starter)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	queues := &musicQueueRuntime{store: store, community: r.community, supervisors: map[id.ID]*music.Supervisor{r.channel: supervisor}}
	registry := &musicQueueRegistry{runtimes: map[id.ID]*retainedMusicRuntime{r.installation: {community: r.community, queues: queues, native: n}}}
	h := &service.MusicInteractions{Store: store, Authority: djAuthority{call: service.MusicCallBinding{CallID: r.channel, ChannelID: r.channel, GroupID: group, Active: true}, dj: dj, djs: &r.djChecks},
		InstallationID: r.installation, CommunityID: r.community, Resolver: r.resolver,
		CurrentState: registry.CurrentState, OnPersistedSnapshot: musicSnapshotCommitCallback(registry, nil),
		OnImmediateControl: registry.applyImmediateControl,
		OnPlayOpened:       registry.warmMusicCall, OnPlay: registry.playPersistedSnapshot}
	r.dispatcher = &service.SealedDispatcher{Community: r.community, Handlers: h.Handlers()}
	return r
}

func (r *coldPlayRig) send(t *testing.T, op protocol.Operation, args ...any) []timedAnswer {
	t.Helper()
	req := protocol.SealedRequest{RequestID: id.New(), CommunityID: r.community, ChannelID: r.channel, Operation: uint64(op), Args: args,
		SentAt: uint64(time.Now().Unix()), UserID: id.New(), DeviceID: id.New()}
	var mu sync.Mutex
	var out []timedAnswer
	r.dispatcher.Dispatch(t.Context(), req, func(a intake.Answer) {
		mu.Lock()
		out = append(out, timedAnswer{Answer: a, at: time.Now()})
		mu.Unlock()
	})
	mu.Lock()
	defer mu.Unlock()
	return out
}

func TestColdPlayJoinsAndResolvesInParallel(t *testing.T) {
	const join, resolve = 400 * time.Millisecond, 500 * time.Millisecond
	// The first frame may follow the slower of the two by the encoder's start and the hand-off; -race slows both.
	const encode = 150 * time.Millisecond
	r := newColdPlayRig(t, join, resolve, true)
	enter := time.Now()
	answers := r.send(t, protocol.OpMusicEnqueue, "never gonna")
	if len(answers) != 2 {
		t.Fatalf("answers = %+v", answers)
	}
	if a := answers[0]; a.Final || string(a.Body) != "Searching…" || a.at.Sub(enter) > 100*time.Millisecond {
		t.Fatalf("first answer = %q final=%v after %v, want an immediate Searching…", a.Body, a.Final, a.at.Sub(enter))
	}
	published, publishes := r.publisher.first()
	if publishes != 1 {
		t.Fatalf("publishes = %d, want 1", publishes)
	}
	final := answers[1]
	if !final.Final || final.Status != intake.StatusOK || string(final.Body) != "Now playing: Song — Band." {
		t.Fatalf("final answer = %d %q final=%v", final.Status, final.Body, final.Final)
	}
	if final.at.Before(published) {
		t.Fatal(`"Now playing" was answered before the first frame was published`)
	}
	started := time.Unix(0, r.joinStarted.Load())
	if r.joinStarted.Load() == 0 || started.Sub(enter) > 100*time.Millisecond {
		t.Fatalf("the call join started %v after Enter, want it beside the resolve", started.Sub(enter))
	}
	firstFrame := published.Sub(enter)
	t.Logf("Enter → first frame %v (join %v ∥ resolve %v)", firstFrame, join, resolve)
	if firstFrame > resolve+50*time.Millisecond+encode {
		t.Fatalf("first frame after %v, want at most %v plus encode (%v): the join and the resolve ran one after the other", firstFrame, resolve+50*time.Millisecond, encode)
	}
	if n := r.joins.Load(); n != 1 {
		t.Fatalf("call-group joins = %d, want 1 (the start uses the join the /play began)", n)
	}
	if n := r.calls.starts.Load(); n != 1 {
		t.Fatalf("call tokens = %d, want 1", n)
	}
	if n := r.resolver.calls.Load(); n != 1 {
		t.Fatalf("resolves = %d, want 1", n)
	}
	// A second /play while the first track plays only queues it: no second join, no second start.
	again := r.send(t, protocol.OpMusicEnqueue, "next one")
	if last := again[len(again)-1]; !last.Final || string(last.Body) != "Track added to the music queue." {
		t.Fatalf("queued /play = %+v", again)
	}
	if _, publishes := r.publisher.first(); publishes != 1 || r.joins.Load() != 1 {
		t.Fatalf("a queued /play started playback again: publishes=%d joins=%d", publishes, r.joins.Load())
	}
}

// A sealed /play whose resolve was quick commits on the call and DJ check it just made; one whose resolve took longer
// than sealedPlayRecheckAfter checks both again before its commit.
func TestColdPlayRechecksTheDJOnlyAfterASlowResolve(t *testing.T) {
	quick := newColdPlayRig(t, 10*time.Millisecond, 10*time.Millisecond, true)
	quick.send(t, protocol.OpMusicEnqueue, "song")
	if n := quick.djChecks.Load(); n != 1 {
		t.Fatalf("DJ checks after a quick resolve = %d, want 1", n)
	}
	slow := newColdPlayRig(t, 10*time.Millisecond, 1100*time.Millisecond, true)
	slow.send(t, protocol.OpMusicEnqueue, "song")
	if n := slow.djChecks.Load(); n != 2 {
		t.Fatalf("DJ checks after a slow resolve = %d, want 2", n)
	}
}

func TestNoJoinWithoutPlay(t *testing.T) {
	t.Run("restore and reads join nothing", func(t *testing.T) {
		r := newColdPlayRig(t, 10*time.Millisecond, 10*time.Millisecond, true)
		if got := r.send(t, protocol.OpMusicState); len(got) != 1 || !got[0].Final {
			t.Fatalf("/queue = %+v", got)
		}
		time.Sleep(100 * time.Millisecond)
		if r.joins.Load() != 0 || r.calls.starts.Load() != 0 {
			t.Fatalf("joins=%d tokens=%d before any /play", r.joins.Load(), r.calls.starts.Load())
		}
	})
	t.Run("a refused /play joins nothing", func(t *testing.T) {
		r := newColdPlayRig(t, 10*time.Millisecond, 10*time.Millisecond, false)
		got := r.send(t, protocol.OpMusicEnqueue, "song")
		if last := got[len(got)-1]; last.Status != 403 {
			t.Fatalf("/play by a member who is not a DJ = %+v", got)
		}
		time.Sleep(100 * time.Millisecond)
		if r.joins.Load() != 0 || r.calls.starts.Load() != 0 || r.resolver.calls.Load() != 0 {
			t.Fatalf("joins=%d tokens=%d resolves=%d after a refused /play", r.joins.Load(), r.calls.starts.Load(), r.resolver.calls.Load())
		}
	})
}

// joinTrustFake is trust.Trust as the call join uses it.
type joinTrustFake struct {
	mu      sync.Mutex
	held    map[id.ID]bool
	asked   map[id.ID]int
	changed []id.ID
	core    *joinTrustCore
	err     error
}

func (f *joinTrustFake) Held(user id.ID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.asked == nil {
		f.asked = map[id.ID]int{}
	}
	f.asked[user]++
	return f.held[user]
}

func (f *joinTrustFake) Changed(_ context.Context, user id.ID) error {
	f.mu.Lock()
	f.changed = append(f.changed, user)
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.core.judged(user)
	return nil
}

// joinTrustCore reports pending leaves until their user is re-judged, and counts device-list reads.
type joinTrustCore struct {
	musicJoinCoreFake
	mu      sync.Mutex
	pending []native.GroupPendingLeaf
	stuck   map[id.ID]bool
}

func (c *joinTrustCore) PendingLeaves(context.Context) ([]native.GroupPendingLeaf, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]native.GroupPendingLeaf(nil), c.pending...), nil
}

func (c *joinTrustCore) judged(user id.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stuck[user] {
		return
	}
	kept := c.pending[:0]
	for _, l := range c.pending {
		if l.User != user {
			kept = append(kept, l)
		}
	}
	c.pending = kept
}

type deviceListCountingCalls struct {
	musicJoinCallsFake
	participants []client.CallParticipant
	listReads    atomic.Int32
}

func (c *deviceListCountingCalls) ChannelCall(ctx context.Context, ch id.ID) (client.ChannelCallState, error) {
	state, err := c.musicJoinCallsFake.ChannelCall(ctx, ch)
	state.Participants = c.participants
	return state, err
}
func (c *deviceListCountingCalls) OwnDeviceList(context.Context, id.ID) ([]byte, error) {
	c.listReads.Add(1)
	return nil, errors.New("a device-list read during the call join")
}
func (c *deviceListCountingCalls) DeviceListHistory(context.Context, id.ID, uint64) ([][]any, error) {
	c.listReads.Add(1)
	return nil, errors.New("a device-list read during the call join")
}

func joinTrustRig(users ...id.ID) (*deviceListCountingCalls, *joinTrustCore, *joinTrustFake) {
	calls := &deviceListCountingCalls{musicJoinCallsFake: musicJoinCallsFake{channel: id.New(), group: id.New()}}
	core := &joinTrustCore{musicJoinCoreFake: musicJoinCoreFake{group: calls.group}, stuck: map[id.ID]bool{}}
	for i, u := range users {
		core.pending = append(core.pending, native.GroupPendingLeaf{Group: calls.group, Leaf: uint32(i + 1), User: u, CachedVersion: 1, Reason: "changed"})
		calls.participants = append(calls.participants, client.CallParticipant{UserID: u, DeviceID: id.New()})
	}
	return calls, core, &joinTrustFake{held: map[id.ID]bool{}, core: core}
}

func TestColdJoinMakesNoDeviceListReads(t *testing.T) {
	calls, core, trust := joinTrustRig(id.New(), id.New(), id.New())
	if err := musicCallGroupJoinAttempt(t.Context(), calls, core, trust, id.New(), id.New(), calls.channel, 1); err != nil {
		t.Fatal(err)
	}
	if n := calls.listReads.Load(); n != 0 {
		t.Fatalf("device-list reads during a cold join = %d, want 0", n)
	}
	if core.joins != 1 || core.resolved != 1 {
		t.Fatalf("join did not complete: %+v", core.musicJoinCoreFake)
	}
}

func TestJoinRechecksOnlyChangedUsers(t *testing.T) {
	t.Run("only users with pending leaves in the call group are re-judged", func(t *testing.T) {
		changedUser, current, elsewhere := id.New(), id.New(), id.New()
		calls, core, trust := joinTrustRig(changedUser)
		// changedUser has two leaves (two devices) in the call; elsewhere's list changed but only in another group.
		core.pending = append(core.pending,
			native.GroupPendingLeaf{Group: calls.group, Leaf: 9, User: changedUser, CachedVersion: 2, Reason: "changed"},
			native.GroupPendingLeaf{Group: id.New(), Leaf: 1, User: elsewhere, CachedVersion: 1, Reason: "changed"})
		calls.participants = append(calls.participants, client.CallParticipant{UserID: current, DeviceID: id.New()})
		if err := musicCallGroupJoinAttempt(t.Context(), calls, core, trust, id.New(), id.New(), calls.channel, 1); err != nil {
			t.Fatal(err)
		}
		if len(trust.changed) != 1 || trust.changed[0] != changedUser {
			t.Fatalf("re-judged %v, want only the changed user %v once", trust.changed, changedUser)
		}
		if trust.asked[current] == 0 || trust.asked[changedUser] == 0 {
			t.Fatalf("held checks = %v: every user in the call is checked", trust.asked)
		}
		if trust.asked[elsewhere] != 0 {
			t.Fatalf("a user outside the call was checked: %v", trust.asked)
		}
	})
	t.Run("a held user's leaves are refused", func(t *testing.T) {
		held, other := id.New(), id.New()
		calls, core, trust := joinTrustRig(other)
		calls.participants = append(calls.participants, client.CallParticipant{UserID: held, DeviceID: id.New()})
		trust.held[held] = true
		err := musicCallGroupJoinAttempt(t.Context(), calls, core, trust, id.New(), id.New(), calls.channel, 1)
		var refused *errMusicCallJoinHeld
		if !errors.As(err, &refused) || !refused.Retryable() {
			t.Fatalf("join with a held user = %v, want a retryable held refusal", err)
		}
		if core.resolved != 0 {
			t.Fatal("a join with a held user was committed")
		}
	})
	t.Run("a leaf still pending after the re-check is refused", func(t *testing.T) {
		stuck := id.New()
		calls, core, trust := joinTrustRig(stuck)
		core.stuck[stuck] = true
		if err := musicCallGroupJoinAttempt(t.Context(), calls, core, trust, id.New(), id.New(), calls.channel, 1); err == nil || core.resolved != 0 {
			t.Fatalf("join with an unjudged leaf = %v resolved=%d", err, core.resolved)
		}
	})
	t.Run("a failed re-check refuses the join", func(t *testing.T) {
		calls, core, trust := joinTrustRig(id.New())
		trust.err = errors.New("helper busy")
		if err := musicCallGroupJoinAttempt(t.Context(), calls, core, trust, id.New(), id.New(), calls.channel, 1); !errors.Is(err, trust.err) || core.resolved != 0 {
			t.Fatalf("join after a failed re-check = %v resolved=%d", err, core.resolved)
		}
	})
}

// The gap between queued tracks: the automatic start of the next track joins nothing and asks for no call token; it
// plays in the room the last track played in.
func TestQueuedTrackStartReusesTheRoomsCall(t *testing.T) {
	community, channel := id.New(), id.New()
	calls := &coldPlayCalls{call: client.Call{CallID: channel, GroupID: id.New(), LiveKitURL: "wss://media.example", Token: "token"}}
	var joins atomic.Int32
	publisher := &coldPlayPublisher{}
	starter, err := newMusicQueueStarter(musicRuntimeDeps{Community: community, Channel: channel, Calls: calls,
		JoinCallGroup: func(context.Context) error { joins.Add(1); return nil },
		YouTubePCM:    liveRuntimePCM{}, Encoder: liveRuntimeEncoderStarter{}, Publisher: publisher, Epochs: runtimeEpochs{}, Lifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	track := music.Track{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "m"}
	first, err := starter.StartExplicit(t.Context(), track)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if joins.Load() != 1 || calls.starts.Load() != 1 {
		t.Fatalf("explicit start: joins=%d tokens=%d", joins.Load(), calls.starts.Load())
	}
	track.ID = "b"
	next, err := starter.Start(t.Context(), track)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Close() }()
	if _, published := publisher.first(); published != 2 || joins.Load() != 1 || calls.starts.Load() != 1 {
		t.Fatalf("queued start: published=%d joins=%d tokens=%d, want the room's call reused", published, joins.Load(), calls.starts.Load())
	}
	// A member's next explicit start prepares the call again (a new call may have begun).
	again, err := starter.StartExplicit(t.Context(), track)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if joins.Load() != 2 || calls.starts.Load() != 2 {
		t.Fatalf("explicit start after: joins=%d tokens=%d", joins.Load(), calls.starts.Load())
	}
}

func TestColdJoinReadsThePolicyVersionOncePerWindow(t *testing.T) {
	var cache musicPolicyCache
	reads := 0
	read := func(context.Context) (uint64, error) { reads++; return 7, nil }
	for range 3 {
		if v, err := cache.get(t.Context(), read); err != nil || v != 7 {
			t.Fatalf("policy = %d, %v", v, err)
		}
	}
	if reads != 1 {
		t.Fatalf("policy reads = %d, want 1 within the window", reads)
	}
	// A failed join forgets it: the next join reads it again.
	cache.forget()
	if _, err := cache.get(t.Context(), read); err != nil || reads != 2 {
		t.Fatalf("after forget: reads=%d err=%v", reads, err)
	}
	cache.forget()
	if _, err := cache.get(t.Context(), func(context.Context) (uint64, error) { return 0, errors.New("unavailable") }); err == nil {
		t.Fatal("a failed read was cached")
	}
}

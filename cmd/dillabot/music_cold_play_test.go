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
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 19: a cold /play joins the call and resolves the track at the same time, answers "Searching…" at once
// and "Now playing" once the first frame is published.

type coldPlayResolver struct {
	delay time.Duration
	calls atomic.Int32
	wait  func(context.Context, string, time.Duration) error
	fail  bool
}

func (r *coldPlayResolver) Resolve(ctx context.Context, _ string) ([]music.Track, error) {
	r.calls.Add(1)
	if err := r.wait(ctx, "resolve", r.delay); err != nil {
		return nil, err
	}
	if r.fail {
		return nil, errors.New("no such track")
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
	now       func() time.Time
}

func (p *coldPlayPublisher) Start(context.Context, client.Call, id.ID, livekit.EpochSource, session.Encoder) (session.Publisher, error) {
	p.mu.Lock()
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	p.published = append(p.published, now())
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
	handlers                         *service.MusicInteractions
	// clk, when set, drives the join and the resolve: each reports on waiting when its timer is set.
	clk     *clock.Fake
	waiting chan string
}

func (r *coldPlayRig) now() time.Time {
	if r.clk != nil {
		return r.clk.Now()
	}
	return time.Now()
}

// wait is a delay of d on the rig's clock.
func (r *coldPlayRig) wait(ctx context.Context, what string, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	if r.clk == nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}
	timer := r.clk.NewTimer(d)
	r.waiting <- what
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return nil
	}
}

func newColdPlayRig(t *testing.T, join, resolve time.Duration, dj bool, clk ...*clock.Fake) *coldPlayRig {
	t.Helper()
	r := &coldPlayRig{installation: id.New(), community: id.New(), channel: id.New(), publisher: &coldPlayPublisher{}, resolver: &coldPlayResolver{delay: resolve}, waiting: make(chan string, 4)}
	if len(clk) == 1 {
		r.clk = clk[0]
		r.publisher.now = r.clk.Now
	}
	r.resolver.wait = r.wait
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
			r.joinStarted.CompareAndSwap(0, r.now().UnixNano())
			return r.wait(ctx, "join", join)
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
	r.handlers = h
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

// M5: the join and the resolve wait on a fake clock. The /play's join begins before its resolve ends, and the first
// frame is published when the slower of the two (the 500 ms resolve) is done: 500 ms of clock time, not 900.
func TestColdPlayJoinsAndResolvesInParallel(t *testing.T) {
	const join, resolve = 400 * time.Millisecond, 500 * time.Millisecond
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	r := newColdPlayRig(t, join, resolve, true, clk)
	enter := clk.Now()
	done := make(chan []timedAnswer, 1)
	go func() { done <- r.send(t, protocol.OpMusicEnqueue, "never gonna") }()
	waitingFor := map[string]bool{}
	for len(waitingFor) < 2 {
		select {
		case what := <-r.waiting:
			waitingFor[what] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("waiting = %v, want the join and the resolve both under way before any time passed", waitingFor)
		}
	}
	if started := time.Unix(0, r.joinStarted.Load()); !started.Equal(enter) {
		t.Fatalf("the call join started at +%v, want at Enter, beside the resolve", started.Sub(enter))
	}
	clk.Advance(join)
	clk.Advance(resolve - join)
	var answers []timedAnswer
	select {
	case answers = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the /play did not finish after the resolve")
	}
	if len(answers) != 2 {
		t.Fatalf("answers = %+v", answers)
	}
	if a := answers[0]; a.Final || string(a.Body) != "Searching…" {
		t.Fatalf("first answer = %q final=%v, want Searching…", a.Body, a.Final)
	}
	published, publishes := r.publisher.first()
	if publishes != 1 {
		t.Fatalf("publishes = %d, want 1", publishes)
	}
	final := answers[1]
	if !final.Final || final.Status != intake.StatusOK || string(final.Body) != "Now playing: Song — Band." {
		t.Fatalf("final answer = %d %q final=%v", final.Status, final.Body, final.Final)
	}
	firstFrame := published.Sub(enter)
	t.Logf("Enter → first frame %v of clock time (join %v ∥ resolve %v)", firstFrame, join, resolve)
	if firstFrame > resolve+50*time.Millisecond {
		t.Fatalf("first frame after %v of clock time, want at most %v: the join and the resolve ran one after the other", firstFrame, resolve+50*time.Millisecond)
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
	r.resolver.delay = 0
	again := r.send(t, protocol.OpMusicEnqueue, "next one")
	if last := again[len(again)-1]; !last.Final || string(last.Body) != "Track added to the music queue." {
		t.Fatalf("queued /play = %+v", again)
	}
	if _, publishes := r.publisher.first(); publishes != 1 || r.joins.Load() != 1 {
		t.Fatalf("a queued /play started playback again: publishes=%d joins=%d", publishes, r.joins.Load())
	}
}

// Fix round 1 (I1): the call and DJ checks are made again right before every commit, in memory, however quick the
// resolve was.
func TestColdPlayAlwaysRechecksTheDJBeforeTheCommit(t *testing.T) {
	quick := newColdPlayRig(t, 10*time.Millisecond, 10*time.Millisecond, true)
	quick.send(t, protocol.OpMusicEnqueue, "song")
	if n := quick.djChecks.Load(); n != 2 {
		t.Fatalf("DJ checks after a quick resolve = %d, want 2 (the check and the recheck before the commit)", n)
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

// M2: the policy version is read once until the installation's next op 54; a join that fails on a reused version
// reads it again and is tried once more.
func TestColdJoinReadsThePolicyVersionOncePerCommunityChange(t *testing.T) {
	installation := id.New()
	cache := musicPolicyCache{installation: installation}
	reads, version := 0, uint64(7)
	read := func(context.Context) (uint64, error) { reads++; return version, nil }
	var joined []uint64
	join := func(_ context.Context, policy uint64) error { joined = append(joined, policy); return nil }
	for range 3 {
		if err := cache.joinWithPolicy(t.Context(), read, join); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 1 {
		t.Fatalf("policy reads = %d, want 1 until the community changes", reads)
	}
	musicSignals.communityChanged(installation)
	version = 8
	if err := cache.joinWithPolicy(t.Context(), read, join); err != nil || reads != 2 || joined[len(joined)-1] != 8 {
		t.Fatalf("after op 54: reads=%d joined=%v err=%v", reads, joined, err)
	}
	// The policy changed without an op 54 reaching the Bot: the join refuses the stale version once, reads it again
	// and joins.
	version = 9
	refusals := 0
	refusing := func(_ context.Context, policy uint64) error {
		if policy != 9 {
			refusals++
			return errors.New("binding refused")
		}
		joined = append(joined, policy)
		return nil
	}
	if err := cache.joinWithPolicy(t.Context(), read, refusing); err != nil || refusals != 1 || reads != 3 || joined[len(joined)-1] != 9 {
		t.Fatalf("stale policy: err=%v refusals=%d reads=%d joined=%v", err, refusals, reads, joined)
	}
	if err := cache.joinWithPolicy(t.Context(), func(context.Context) (uint64, error) { return 0, errors.New("unavailable") }, join); err != nil {
		t.Fatalf("a cached version needs no read: %v", err)
	}
}

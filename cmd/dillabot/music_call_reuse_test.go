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
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/livekit"
	"github.com/dilla-chat/dilla/internal/bot/music/session"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 19 fix round 1 (I2, M3): the room's call is reused only while the pool holds the room, never with its
// token, and a warm nobody takes leaves the room.

type prepRig struct {
	prep     *musicCallPrep
	prepares atomic.Int32
	leaves   atomic.Int32
	mu       sync.Mutex
	held     bool
	call     client.Call
}

func newPrepRig() *prepRig {
	r := &prepRig{held: true, call: client.Call{CallID: id.New(), GroupID: id.New(), LiveKitURL: "wss://media.example", Token: "secret-token"}}
	r.prep = &musicCallPrep{
		prepare: func(context.Context) (client.Call, error) { r.prepares.Add(1); return r.call, nil },
		connect: func(context.Context, client.Call) error { return nil },
		holds: func(client.Call) bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.held
		},
		leave: func() error { r.leaves.Add(1); return nil },
	}
	return r
}

func (r *prepRig) setHeld(held bool) {
	r.mu.Lock()
	r.held = held
	r.mu.Unlock()
}

// explicitStartContext is a member's start (StartExplicit's context).
func explicitStartContext(t *testing.T) context.Context {
	t.Helper()
	return session.WithExplicitStart(t.Context())
}

func TestCallEndClearsTheReusedCall(t *testing.T) {
	r := newPrepRig()
	if _, err := r.prep.take(explicitStartContext(t)); err != nil {
		t.Fatal(err)
	}
	if call, err := r.prep.take(t.Context()); err != nil || call.GroupID != r.call.GroupID {
		t.Fatalf("the next queued track in the held room = %v, %v", call, err)
	}
	r.prep.forget() // the room's call ended (the pool's room-ended report)
	if _, err := r.prep.take(t.Context()); !errors.Is(err, errMusicNeedsPlay) {
		t.Fatalf("an automatic start after the call ended = %v, want errMusicNeedsPlay", err)
	}
	if r.prepares.Load() != 1 {
		t.Fatalf("prepares = %d: an automatic start joined a call", r.prepares.Load())
	}
}

func TestSuccessorAfterIdleLeaveDoesNotReconnectWithAnOldToken(t *testing.T) {
	r := newPrepRig()
	call, err := r.prep.take(explicitStartContext(t))
	if err != nil || call.Token == "" {
		t.Fatalf("explicit start = %v, %v", call, err)
	}
	r.prep.mu.Lock()
	kept := r.prep.last
	r.prep.mu.Unlock()
	if kept == nil || kept.Token != "" {
		t.Fatal("the room's call is kept with its token")
	}
	r.setHeld(false) // left idle: the pool no longer holds the room
	if _, err := r.prep.take(t.Context()); !errors.Is(err, errMusicNeedsPlay) {
		t.Fatalf("an automatic start after an idle leave = %v, want errMusicNeedsPlay", err)
	}
}

func TestSuccessorStopsWhenTheBotLosesSpeak(t *testing.T) {
	community, channel, bot, member := id.New(), id.New(), id.New(), id.New()
	authz := &fakeMusicAuthz{ready: true, allowed: map[id.ID]uint64{bot: musicBotConnectSpeak}}
	trust := &joinTrustFake{held: map[id.ID]bool{}}
	ready := true
	a := musicAdmission{community: community, channel: channel, botUser: bot, authz: authz, trust: trust, ready: func(context.Context) bool { return ready }}
	epoch := native.CallMediaEpoch{Roster: []native.CallMediaMember{{Leaf: 1, UserID: bot}, {Leaf: 2, UserID: member}}}
	if err := a.admit(t.Context(), epoch); err != nil {
		t.Fatalf("admitted epoch = %v", err)
	}
	authz.set(bot, 1<<7) // speak removed
	if err := a.admit(t.Context(), epoch); !errors.Is(err, errMusicBotCannotSpeak) {
		t.Fatalf("without speak = %v", err)
	}
	authz.set(bot, musicBotConnectSpeak)
	trust.held[member] = true
	var held *errMusicCallJoinHeld
	if err := a.admit(t.Context(), epoch); !errors.As(err, &held) {
		t.Fatalf("a held user in the roster = %v", err)
	}
	trust.held[member] = false
	ready = false
	if err := a.admit(t.Context(), epoch); !errors.Is(err, errMusicInstallationNotReady) {
		t.Fatalf("installation not ready = %v", err)
	}
}

func TestRefusedPlayLeavesTheWarmedCall(t *testing.T) {
	r := newPrepRig()
	r.prep.warm(t.Context())
	deadline := time.Now().Add(time.Second)
	for {
		r.prep.mu.Lock()
		ready := r.prep.ready
		r.prep.mu.Unlock()
		if ready != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the warm did not prepare")
		}
		time.Sleep(time.Millisecond)
	}
	r.prep.abandon()
	r.prep.mu.Lock()
	ready := r.prep.ready
	r.prep.mu.Unlock()
	if ready != nil || r.leaves.Load() != 1 {
		t.Fatalf("after an abandoned /play: ready=%v leaves=%d, want the warmed call dropped and the room left", ready != nil, r.leaves.Load())
	}
	// A warm nobody takes expires the same way.
	r.prep.warm(t.Context())
	for {
		r.prep.mu.Lock()
		prepared := r.prep.ready
		r.prep.mu.Unlock()
		if prepared != nil {
			r.prep.expire(prepared)
			break
		}
		time.Sleep(time.Millisecond)
	}
	if r.leaves.Load() != 2 {
		t.Fatalf("leaves after expiry = %d, want 2", r.leaves.Load())
	}
}

// The handler tells the runtime when a /play whose warm began ends without starting (here: the resolve fails).
func TestColdPlayThatFailsToResolveAbandonsItsWarm(t *testing.T) {
	r := newColdPlayRig(t, 10*time.Millisecond, 10*time.Millisecond, true)
	r.resolver.fail = true
	var abandoned atomic.Int32
	r.handlers.OnPlayAbandoned = func(context.Context, id.ID, id.ID, id.ID) { abandoned.Add(1) }
	r.dispatcher = &service.SealedDispatcher{Community: r.community, Handlers: r.handlers.Handlers()}
	got := r.send(t, protocol.OpMusicEnqueue, "song")
	if last := got[len(got)-1]; last.Status == 200 || abandoned.Load() != 1 {
		t.Fatalf("/play that cannot resolve = %+v, abandoned %d", got, abandoned.Load())
	}
}

// fakeMusicAuthz is the feed-state cache with per-user community-wide bits.
type fakeMusicAuthz struct {
	mu      sync.Mutex
	ready   bool
	allowed map[id.ID]uint64
	timeout map[id.ID]bool
}

func (f *fakeMusicAuthz) set(user id.ID, bits uint64) {
	f.mu.Lock()
	f.allowed[user] = bits
	f.mu.Unlock()
}
func (f *fakeMusicAuthz) Ready() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.ready }
func (f *fakeMusicAuthz) Allowed(_, _, user id.ID, bits uint64, _ bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allowed[user]&bits == bits
}
func (f *fakeMusicAuthz) Permissions(user id.ID) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allowed[user]
}
func (f *fakeMusicAuthz) TimedOut(user id.ID, _ time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.timeout[user]
}

// C1: a join that finds the Bot already a member applies the members' newer commits before it reads the media epoch.
func TestAlreadyMemberJoinCatchesUpFirst(t *testing.T) {
	calls := &memberCalls{musicJoinCallsFake: musicJoinCallsFake{channel: id.New(), group: id.New()}}
	core := &memberCore{musicJoinCoreFake: musicJoinCoreFake{group: calls.group}}
	var caughtUp []id.ID
	ctx := withMusicCatchUp(t.Context(), func(_ context.Context, group id.ID) error {
		if core.helperReads > 1 {
			t.Error("the media epoch was read before the catch-up")
		}
		caughtUp = append(caughtUp, group)
		return nil
	})
	if err := musicCallGroupJoinAttempt(ctx, calls, core, allowMusicJoinTrust{}, id.New(), id.New(), calls.channel, 1); err != nil {
		t.Fatal(err)
	}
	if len(caughtUp) != 1 || caughtUp[0] != calls.group {
		t.Fatalf("catch-ups = %v, want the call group once", caughtUp)
	}
	failing := withMusicCatchUp(t.Context(), func(context.Context, id.ID) error { return errors.New("behind") })
	if err := musicCallGroupJoinAttempt(failing, calls, core, allowMusicJoinTrust{}, id.New(), id.New(), calls.channel, 1); err == nil {
		t.Fatal("a member join went on behind its call group")
	}
}

type catchUpSyncFake struct {
	results []service.SourceSyncResult
	errs    []error
	calls   int
}

func (f *catchUpSyncFake) Sync(context.Context, *client.Session, native.GroupExpected) (service.SourceSyncResult, error) {
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return service.SourceSyncResult{}, f.errs[i]
	}
	if i < len(f.results) {
		return f.results[i], nil
	}
	return service.SourceSyncResult{}, nil
}

func TestCallCatchUpSyncsOnlyWhenBehind(t *testing.T) {
	group := id.New()
	local := func(next uint64) func(context.Context, id.ID) (native.GroupInfo, error) {
		return func(context.Context, id.ID) (native.GroupInfo, error) {
			return native.GroupInfo{NextSequence: next}, nil
		}
	}
	remote := func(next uint64) func(context.Context, id.ID) (client.GroupInfo, error) {
		return func(context.Context, id.ID) (client.GroupInfo, error) {
			return client.GroupInfo{NextSequence: next}, nil
		}
	}
	current := &catchUpSyncFake{}
	c := musicCallCatchUp{sync: current, session: &client.Session{}, local: local(9), remote: remote(9)}
	if err := c.run(t.Context(), group); err != nil || current.calls != 0 {
		t.Fatalf("a current group: err=%v syncs=%d, want no paging", err, current.calls)
	}
	behind := &catchUpSyncFake{results: []service.SourceSyncResult{{}, {More: true}, {More: false}}, errs: []error{service.ErrSourceSyncBusy}}
	c = musicCallCatchUp{sync: behind, session: &client.Session{}, local: local(9), remote: remote(12)}
	if err := c.run(t.Context(), group); err != nil || behind.calls != 3 {
		t.Fatalf("a group behind: err=%v syncs=%d, want a busy retry then two passes", err, behind.calls)
	}
	gone := &catchUpSyncFake{errs: []error{service.ErrSourceGroupGone}}
	c = musicCallCatchUp{sync: gone, session: &client.Session{}, local: local(9), remote: remote(12)}
	if err := c.run(t.Context(), group); !errors.Is(err, livekit.ErrCallCatchUpTerminal) {
		t.Fatalf("a gone group = %v, want terminal", err)
	}
}

// Fix round 2 (N1): the Bot's own fresh join spares one publication its catch-up, once.
func TestAFreshJoinSparesOnlyTheFirstCatchUp(t *testing.T) {
	r := newPrepRig()
	if _, err := r.prep.take(explicitStartContext(t)); err != nil {
		t.Fatal(err)
	}
	if !r.prep.joinedRecently(r.call.GroupID) {
		t.Fatal("the first publication after the join is not spared")
	}
	if r.prep.joinedRecently(r.call.GroupID) {
		t.Fatal("a second publication was spared its catch-up too")
	}
}

// Fix round 2 (N5): a /play into a room the queue still holds (a reuse warm) never leaves it when nobody takes it.
func TestAReuseWarmNeverLeavesTheHeldRoom(t *testing.T) {
	r := newPrepRig()
	if _, err := r.prep.take(explicitStartContext(t)); err != nil {
		t.Fatal(err)
	}
	r.prep.warm(music.WithPlaybackGroup(t.Context(), r.call.GroupID))
	r.prep.mu.Lock()
	prepared := r.prep.ready
	r.prep.mu.Unlock()
	if prepared == nil || prepared.connected {
		t.Fatalf("reuse warm = %+v, want a prepared call that did not connect the room", prepared)
	}
	r.prep.expire(prepared)
	r.prep.abandon()
	if r.leaves.Load() != 0 {
		t.Fatalf("a reuse warm left the room %d times", r.leaves.Load())
	}
}

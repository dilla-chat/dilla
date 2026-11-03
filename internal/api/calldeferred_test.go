package api_test

// web3.7 3a-server fix round 3: the deferred leaves (callleave_test.go) never starve the retry pass's
// repairs (review S1), and the guards that keep a joined newer session are each pinned (T1, n5).

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
)

// slowListing is the stub SFU whose room listing hangs until the caller's deadline while slow is set
// (an SFU under load). Only the call events use it; the Calls' own repairs use the plain stub.
type slowListing struct {
	*stubSFU
	slow     atomic.Bool
	listings atomic.Int32
}

func (s *slowListing) Participants(ctx context.Context, room string) ([]*livekit.ParticipantInfo, error) {
	s.listings.Add(1)
	if s.slow.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.stubSFU.Participants(ctx, room)
}

// S1: with many deferred leaves past their windows and an SFU whose listing hangs, the retry pass
// still reaches a pending demotion (a sharer that lost video, its first push refused), and it lists
// the deferrals' room once.
func TestDeferredLeavesNeverStarveTheRepairs(t *testing.T) {
	for _, n := range []int{0, 9} {
		t.Run(fmt.Sprintf("%d deferrals", n), func(t *testing.T) {
			f := newEventsFixture(t, api.CallsConfig{})
			slow := &slowListing{stubSFU: f.stub}
			f.events = api.NewCallEvents(f.e.Repo, api.NewResolver(f.e.Repo), f.ds, slow, f.calls, f.gw, f.e.Clk, slog.New(slog.DiscardHandler))
			for i := range n {
				_, dev, _ := joinedMember(t, f.e, f.ch, f.group, fmt.Sprintf("m%d", i))
				admitThroughProxy(t, f.calls, f.room, dev)
				f.e.Clk.Advance(time.Second)
				joined := f.e.Clk.Now()
				sid := fmt.Sprintf("PA_m%d", i)
				f.sendJoined(t, dev, sid, joined)
				f.e.Clk.Advance(time.Second)
				admitThroughProxy(t, f.calls, f.room, dev) // a fresh rejoin that never joins
				f.stub.setPresent(f.room)
				f.sendLeft(t, dev, sid, joined) // deferred
			}
			if got := f.events.DeferredLeavesForTest(); got != n {
				t.Fatalf("deferred leaves = %d, want %d", got, n)
			}
			// A pending grant repair: a sharer loses video and the SFU refuses the first demotion.
			member, memberDev, memberTok := joinedMember(t, f.e, f.ch, f.group, "sharer")
			share := "/v1/calls/" + f.callID.String() + "/share"
			if status, _ := f.e.Do(http.MethodPost, share, memberTok, []any{}); status != http.StatusNoContent {
				t.Fatalf("member share = %d", status)
			}
			f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: memberDev.String()})
			denyInChannel(t, f.e, f.ch, member, api.PermVideo|api.PermScreenShare)
			f.stub.mu.Lock()
			f.stub.failUpdates = 1
			f.stub.mu.Unlock()
			if err := api.SyncCallGrants(t.Context(), f.e.Repo, api.NewResolver(f.e.Repo), f.stub, f.calls,
				ownerCommunityOf(t, f.e, f.ch), &member, nil); err == nil {
				t.Fatal("SyncCallGrants hid the failed demotion")
			}
			f.e.Clk.Advance(admissionWindow()) // every deferral is past its join window
			slow.slow.Store(true)
			slow.listings.Store(0)
			f.calls.RetryPending(t.Context())
			if hasCamera(f.stub.lastPerms()[memberDev.String()]) {
				t.Errorf("with %d deferred leaves the retry pass never reached the pending demotion", n)
			}
			if got := slow.listings.Load(); got > 1 {
				t.Errorf("the pass listed the deferrals' one room %d times, want at most once", got)
			}
		})
	}
}

// T1: the newer session joined LiveKit — the SFU lists D as PA_2 — but its participant_joined is late
// (SP-21, ~45 s, past the 30 s window). The deferral waits: no flags 0, D's presence is not dropped,
// and no Remove.
func TestADeferredLeaveWaitsWhileTheSFUListsTheDevice(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	deferLeave(t, f)
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String(), Sid: "PA_2"})
	f.e.Clk.Advance(admissionWindow())
	f.calls.RetryPending(t.Context())
	f.events.FlushAnnouncements()
	if f.leftAnnounced(f.dev) {
		t.Fatalf("voice_state = %+v: D was announced out while the SFU lists its new session", f.gw.all())
	}
	if got := f.events.DeferredLeavesForTest(); got != 1 {
		t.Fatalf("deferred leaves = %d, want D's still pending", got)
	}
	if got := f.ds.memberRemovals(); len(got) != 0 {
		t.Fatalf("membership-bound Removes = %+v: the listed session's leaf was removed", got)
	}
}

// n5: the newer session's participant_joined is processed, then the SFU lists D out (between two
// listings) past the window. The join cleared the deferral, so nothing announces D out.
func TestTheNewSessionsJoinClearsTheDeferral(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	deferLeave(t, f)
	f.e.Clk.Advance(5 * time.Second)
	f.sendJoined(t, f.dev, "PA_2", f.e.Clk.Now())
	if got := f.events.DeferredLeavesForTest(); got != 0 {
		t.Fatalf("deferred leaves = %d after the new session's join, want none", got)
	}
	f.stub.setPresent(f.room)
	f.e.Clk.Advance(admissionWindow())
	f.calls.RetryPending(t.Context())
	f.events.FlushAnnouncements()
	if f.leftAnnounced(f.dev) {
		t.Fatalf("voice_state = %+v: D was announced out though its new session joined", f.gw.all())
	}
}

// n5: the newer session's participant_joined is processed BEFORE the first session's late leave, so
// the deferral is recorded while the call events hold D under PA_2. When the SFU then lists D out
// past the window, completion finds the newer session recorded and announces nothing.
func TestADeferralBehindTheNewSessionsJoinAnnouncesNothing(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	joined := admitFirstSession(t, f)
	f.e.Clk.Advance(10 * time.Second)
	placeLeaves(t, f.e, f.group, f.dev, leafAt{leaf: 3, added: 5, removed: 6}, secondAdmission)
	admitThroughProxy(t, f.calls, f.room, f.dev) // A2
	f.e.Clk.Advance(time.Second)
	f.sendJoined(t, f.dev, "PA_2", f.e.Clk.Now())
	f.stub.setPresent(f.room)
	f.sendLeft(t, f.dev, "PA_1", joined) // late, behind PA_2's join
	f.e.Clk.Advance(admissionWindow())
	f.calls.RetryPending(t.Context())
	f.events.FlushAnnouncements()
	if f.leftAnnounced(f.dev) {
		t.Fatalf("voice_state = %+v: D was announced out though the call events hold its new session", f.gw.all())
	}
	if !inParticipants(f, f.dev) {
		t.Fatal("D's new session is no participant of the call")
	}
}

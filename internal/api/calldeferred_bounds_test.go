package api_test

// web3.7 3a-server fix round 4 (review t1): the deferred step's two bounds — at most
// maxDeferredPerTick (8) deferrals per pass, within deferredPassBudget (4 s) — are each pinned with
// deferrals in different calls, so the once-per-room listing cannot cover for either.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
)

// stuckSFU is the stub SFU whose room listing, while mode is set, hangs until the caller's deadline
// ("hang") or fails at once ("fail"), recording each room it was asked to list.
type stuckSFU struct {
	*stubSFU
	mode   atomic.Value // string
	mu     sync.Mutex
	listed []string
}

func (s *stuckSFU) Participants(ctx context.Context, room string) ([]*livekit.ParticipantInfo, error) {
	s.mu.Lock()
	s.listed = append(s.listed, room)
	s.mu.Unlock()
	switch m, _ := s.mode.Load().(string); m {
	case "hang":
		<-ctx.Done()
		return nil, ctx.Err()
	case "fail":
		return nil, errors.New("stuckSFU: the SFU is unavailable")
	}
	return s.stubSFU.Participants(ctx, room)
}

// takeListed is the rooms listed since the last take.
func (s *stuckSFU) takeListed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.listed
	s.listed = nil
	return out
}

// deferInNewCalls opens n more voice calls in the fixture's community, each started by its own
// member's device, and gives each a deferred leave: the device admitted and joined, admitted again by
// a fresh join that never comes, and the first session's late leave. It returns the calls' rooms.
func deferInNewCalls(t *testing.T, f *eventsFixture, n int) []string {
	t.Helper()
	cid := ownerCommunityOf(t, f.e, f.ch)
	rooms := make([]string, 0, n)
	for i := range n {
		ch, _, status := newChannel(t, f.e, cid, f.tok, 1 /* voice */, 1, 2, fmt.Sprintf("voice-%d", i))
		if status != http.StatusCreated {
			t.Fatalf("voice channel %d = %d", i, status)
		}
		group := seedCallGroup(t, f.e, ch, cid, callGroupEpoch)
		_, dev, tok := joinedMember(t, f.e, ch, group, fmt.Sprintf("caller%d", i))
		status, body := f.e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
		if status != http.StatusCreated {
			t.Fatalf("opening call %d = %d %s", i, status, f.e.ErrCode(body))
		}
		room := f.stub.lastMint().Room
		admitThroughProxy(t, f.calls, room, dev)
		f.e.Clk.Advance(time.Millisecond)
		joined := f.e.Clk.Now()
		sid := fmt.Sprintf("PA_c%d", i)
		f.sendIn(t, webhookJoined, room, dev, sid, joined)
		f.e.Clk.Advance(time.Millisecond)
		admitThroughProxy(t, f.calls, room, dev) // the reload's session, which never joins
		f.sendIn(t, webhookLeft, room, dev, sid, joined)
		rooms = append(rooms, room)
	}
	return rooms
}

const (
	webhookJoined = "participant_joined"
	webhookLeft   = "participant_left"
)

// sendIn is a participant event of dev's session sid in room, joined at joinedAt.
func (f *eventsFixture) sendIn(t *testing.T, event, room string, dev id.ID, sid string, joinedAt time.Time) {
	t.Helper()
	f.events.Handle(t.Context(), &livekit.WebhookEvent{Event: event, Id: "EV_" + id.New().String(),
		Room:        &livekit.Room{Name: room},
		Participant: &livekit.ParticipantInfo{Identity: dev.String(), Sid: sid, JoinedAtMs: joinedAt.UnixMilli()}})
	f.events.FlushAnnouncements()
}

func boundsFixture(t *testing.T, n int) (*eventsFixture, *stuckSFU, []string) {
	t.Helper()
	f := newEventsFixture(t, api.CallsConfig{})
	stuck := &stuckSFU{stubSFU: f.stub}
	f.events = api.NewCallEvents(f.e.Repo, api.NewResolver(f.e.Repo), f.ds, stuck, f.calls, f.gw, f.e.Clk, slog.New(slog.DiscardHandler))
	rooms := deferInNewCalls(t, f, n)
	if got := f.events.DeferredLeavesForTest(); got != n {
		t.Fatalf("deferred leaves = %d, want %d", got, n)
	}
	f.e.Clk.Advance(admissionWindow()) // every deferral is past its join window
	return f, stuck, rooms
}

// The budget: with 9 deferrals in 9 calls and a listing that hangs, one pass's deferred step returns
// within its 4 s budget (plus slack), whatever the cap allows.
func TestTheDeferredStepEndsWithinItsBudget(t *testing.T) {
	f, stuck, _ := boundsFixture(t, 9)
	stuck.mode.Store("hang")
	start := time.Now()
	f.calls.RetryPending(t.Context())
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("a retry pass with hanging listings took %s, past the deferred step's 4 s budget", took)
	}
	if got := f.events.DeferredLeavesForTest(); got != 9 {
		t.Fatalf("deferred leaves = %d, want all 9 still pending (no room could be seen)", got)
	}
}

// The cap: with 9 deferrals in 9 calls and a listing that fails at once (so the budget never bites),
// one pass lists at most 8 rooms, and the next pass takes the rest.
func TestTheDeferredStepTakesAtMostEightAPassAndTheRestNext(t *testing.T) {
	f, stuck, rooms := boundsFixture(t, 9)
	stuck.mode.Store("fail")
	stuck.takeListed()
	f.calls.RetryPending(t.Context())
	first := stuck.takeListed()
	if len(first) > 8 {
		t.Fatalf("one pass listed %d rooms, want at most 8", len(first))
	}
	f.calls.RetryPending(t.Context())
	seen := map[string]bool{}
	for _, r := range append(first, stuck.takeListed()...) {
		seen[r] = true
	}
	for _, r := range rooms {
		if !seen[r] {
			t.Fatalf("room %s was not listed in two passes: the rest were not taken on the next pass (seen %d of %d)", r, len(seen), len(rooms))
		}
	}
}

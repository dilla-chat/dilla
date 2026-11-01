package api_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// CT 144, 2026-10-31: the human (leaf 0) left a call in which the music Bot (leaf 1) stays. The
// only member left to commit the human's Remove is the Bot, and a managed Bot is never elected
// committer (gateway registry onlineIn), so the Remove is never committed: it voids after
// ProposalTTLCall (30 s) and the sweep proposes it again every sweep (35 s), and every rejoin of
// the human is refused 409 E_REMOVE_PENDING while it stands. A call whose only members left in the
// room are managed bots must end (the group closes), so the human's rejoin registers a fresh group.
func TestASweepEndsACallWhoseOnlyMemberLeftInTheRoomIsABot(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	botDev := seedCallBot(t, f.e, f.ch, f.group)
	// The human is out of the room; only the managed bot remains.
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: botDev.String()})
	f.e.Clk.Advance(api.RoomSweepInterval + time.Second)
	f.calls.SweepRooms(t.Context())
	if row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID); err != nil || row.Ended != nil {
		t.Fatalf("first bot-only observation ended the call: %+v, %v", row, err)
	}
	f.e.Clk.Advance(api.RoomSweepInterval + time.Second)
	f.calls.SweepRooms(t.Context())
	row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Ended == nil {
		t.Fatalf("the call stays live with only the bot in its room; the sweep proposed %+v, which only the bot could commit", f.ds.memberRemovals())
	}
	if removes := f.ds.memberRemovals(); len(removes) != 0 {
		t.Fatalf("the bot-only call queued uncommittable Removes: %+v", removes)
	}
	if rooms := f.stub.deletedRooms(); len(rooms) != 1 || rooms[0] != f.room {
		t.Fatalf("deleted rooms = %v, want the bot-only room", rooms)
	}
	f.calls.ProcessQueueForTest(t.Context())
	if group, err := f.e.Repo.GetGroup(t.Context(), f.group); err != nil || group.ClosedAt == nil {
		t.Fatalf("bot-only call group was not closed: %+v, %v", group, err)
	}
}

func TestASweepKeepsABotCallWhileAHumanIsJoining(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	botDev := seedCallBot(t, f.e, f.ch, f.group)
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: botDev.String()})
	f.calls.SweepRooms(t.Context())
	if row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID); err != nil || row.Ended != nil {
		t.Fatalf("the sweep ended a call inside the human's admission window: %+v, %v", row, err)
	}
	// The human arrives before its admission expires; bots never make this call end.
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: botDev.String()}, &livekit.ParticipantInfo{Identity: f.dev.String()})
	f.e.Clk.Advance(2 * api.RoomSweepInterval)
	f.calls.SweepRooms(t.Context())
	if row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID); err != nil || row.Ended != nil {
		t.Fatalf("the sweep ended a call with a human present: %+v, %v", row, err)
	}
}

func TestAnEndIfAloneIgnoresManagedBotLeaves(t *testing.T) {
	for _, withDS := range []bool{false, true} {
		t.Run(map[bool]string{false: "without delivery service", true: "with delivery service"}[withDS], func(t *testing.T) {
			var e *env
			var ch, group id.ID
			var tok string
			if withDS {
				f := newEventsFixture(t, api.CallsConfig{})
				e, ch, tok, group = f.e, f.ch, f.tok, f.group
			} else {
				e, ch, tok, group, _, _ = callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
				seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
				if status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{}); status != http.StatusCreated {
					t.Fatalf("start = %d %s", status, e.ErrCode(body))
				}
			}
			seedCallBot(t, e, ch, group)
			if status, body := e.Do(http.MethodDelete, "/v1/calls/"+ch.String()+"?if_alone=1", tok, nil); status != http.StatusNoContent {
				t.Fatalf("conditional end beside only a bot = %d %s, want 204", status, e.ErrCode(body))
			}
			if row, err := e.Repo.GetGroup(t.Context(), group); err != nil || row.ClosedAt == nil {
				t.Fatalf("conditional end did not close the group: %+v, %v", row, err)
			}
		})
	}
}

func seedCallBot(t *testing.T, e *env, ch, group id.ID) id.ID {
	t.Helper()
	botUser, botDev := id.New(), id.New()
	u := newAPITestUser(botUser, "musicbot", 1)
	u.Kind = 1
	if err := e.Repo.CreateUser(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	d := newAPITestDevice(botDev, botUser, 1)
	d.DSKPub = uniqueAPITestDSK(t)
	if err := e.Repo.CreateDevice(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: ownerCommunityOf(t, e, ch), UserID: botUser, Joined: 1}); err != nil {
		t.Fatal(err)
	}
	seedLeaf(t, e, group, botDev, 3, nil)
	return botDev
}

func TestWholeRoomGrantSyncNeedsSpacedBotOnlyObservationsAfterListingFailure(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	botDev := seedCallBot(t, f.e, f.ch, f.group)
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: botDev.String()})
	f.e.Clk.Advance(api.RoomSweepInterval + time.Second)
	sync := func() error {
		return api.SyncCallGrants(t.Context(), f.e.Repo, api.NewResolver(f.e.Repo), f.stub, f.calls, ownerCommunityOf(t, f.e, f.ch), nil, &f.ch)
	}
	live := func() {
		t.Helper()
		if row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID); err != nil || row.Ended != nil {
			t.Fatalf("call ended before two reliable spaced observations: %+v, %v", row, err)
		}
	}
	if err := sync(); err != nil {
		t.Fatal(err)
	}
	live()
	f.e.Clk.Advance(api.RoomSweepInterval)
	f.stub.mu.Lock()
	f.stub.listFail = errors.New("listing unavailable")
	f.stub.mu.Unlock()
	if err := sync(); err == nil {
		t.Fatal("listing failure was not reported")
	}
	f.stub.mu.Lock()
	f.stub.listFail = nil
	f.stub.mu.Unlock()
	if err := sync(); err != nil {
		t.Fatal(err)
	}
	live()
	f.e.Clk.Advance(api.RoomSweepInterval - time.Nanosecond)
	if err := sync(); err != nil {
		t.Fatal(err)
	}
	live()
	f.e.Clk.Advance(time.Nanosecond)
	if err := sync(); err != nil {
		t.Fatal(err)
	}
	if row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID); err != nil || row.Ended == nil {
		t.Fatalf("two reliable spaced whole-room syncs did not end: %+v, %v", row, err)
	}
}

func TestBotOnlyObservationResetsAfterRoomListingFailure(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	botDev := seedCallBot(t, f.e, f.ch, f.group)
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: botDev.String()})
	f.e.Clk.Advance(api.RoomSweepInterval + time.Second)
	f.calls.SweepRooms(t.Context())
	f.stub.mu.Lock()
	f.stub.roomsFail = errors.New("rooms unavailable")
	f.stub.mu.Unlock()
	f.e.Clk.Advance(api.RoomSweepInterval)
	f.calls.SweepRooms(t.Context())
	f.stub.mu.Lock()
	f.stub.roomsFail = nil
	f.stub.mu.Unlock()
	f.calls.SweepRooms(t.Context())
	if row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID); err != nil || row.Ended != nil {
		t.Fatalf("first observation after failed room listing ended the call: %+v, %v", row, err)
	}
	f.e.Clk.Advance(api.RoomSweepInterval)
	f.calls.SweepRooms(t.Context())
	if row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID); err != nil || row.Ended == nil {
		t.Fatalf("second observation after failed room listing did not end the call: %+v, %v", row, err)
	}
}

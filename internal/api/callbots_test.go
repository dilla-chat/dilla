package api_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
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

// putInstanceRemove stores a non-void instance Remove (origin 0) of dev's leaf at the group's
// current epoch, issued now: what the instance proposes when a device drops out of a call.
func putInstanceRemove(t *testing.T, e *env, group, dev id.ID, leaf uint32) {
	t.Helper()
	g, err := e.Repo.GetGroup(t.Context(), group)
	if err != nil {
		t.Fatal(err)
	}
	ref := id.New()
	if err := e.Repo.PutProposal(t.Context(), store.ProposalRow{
		GroupID: group, Ref: ref[:], Epoch: g.Epoch, Kind: 3, /* Remove */
		TargetLeaf: &leaf, TargetDevice: &dev, Origin: 0, ActionID: id.New(),
		IssuedAt: e.Clk.Now().Unix(), TTL: 30,
	}); err != nil {
		t.Fatal(err)
	}
}

// CT 144, 2026-11-25 (Casa Bonita): the human's device dropped and the instance proposed the Remove
// of its leaf; the Bot is the only other member and is never elected to commit it, so every rejoin
// was refused 409 E_REMOVE_PENDING. A human leaf that is both the target of an outstanding
// instance Remove and absent from the room is no active human, even inside its admission window
// (its session was admitted before the Remove), so two sweeps end the call bots_only and close its
// group: the person's next join registers a fresh group (the resync then answers E_GROUP_CLOSED,
// internal/ds TestAResyncByARemoveTargetOfAClosedGroupIsGroupClosed).
func TestBotsOnlyEndsWhenTheOnlyHumanIsAbsentAndPendingRemoval(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	var logs bytes.Buffer
	f.calls.SetLogForTest(slog.New(slog.NewTextHandler(&logs, nil)))
	botDev := seedCallBot(t, f.e, f.ch, f.group)
	f.e.Clk.Advance(5 * time.Second) // the human's session was admitted, then it dropped
	putInstanceRemove(t, f.e, f.group, f.dev, 0)
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: botDev.String()})
	f.calls.SweepRooms(t.Context())
	if row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID); err != nil || row.Ended != nil {
		t.Fatalf("the first observation ended the call: %+v, %v", row, err)
	}
	f.e.Clk.Advance(api.RoomSweepInterval + time.Second)
	f.calls.SweepRooms(t.Context())
	row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Ended == nil {
		t.Fatal("two sweeps kept a call whose only human is absent and pending removal, beside only the Bot")
	}
	ended := "msg=\"call ended\" call_id=" + f.callID.String() + " reason=bots_only"
	if !strings.Contains(logs.String(), ended) {
		t.Fatalf("the call did not end as bots_only; want %q in:\n%s", ended, logs.String())
	}
	if rooms := f.stub.deletedRooms(); len(rooms) != 1 || rooms[0] != f.room {
		t.Fatalf("deleted rooms = %v, want the bots-only room", rooms)
	}
	if removes := f.ds.memberRemovals(); len(removes) != 0 {
		t.Fatalf("the bots-only call queued Removes only the Bot could commit: %+v", removes)
	}
	f.calls.ProcessQueueForTest(t.Context())
	if g, err := f.e.Repo.GetGroup(t.Context(), f.group); err != nil || g.ClosedAt == nil {
		t.Fatalf("the call group was not closed: %+v, %v", g, err)
	}
}

// Review focus 1: a human whose Remove is outstanding but whose device is in the room (a live
// connection, slow to commit, or reconnected) keeps the call.
func TestBotsOnlyKeepsACallWhoseHumanIsStillInTheRoom(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	botDev := seedCallBot(t, f.e, f.ch, f.group)
	f.e.Clk.Advance(5 * time.Second)
	putInstanceRemove(t, f.e, f.group, f.dev, 0)
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: botDev.String()}, &livekit.ParticipantInfo{Identity: f.dev.String()})
	for range 3 {
		f.calls.SweepRooms(t.Context())
		f.e.Clk.Advance(api.RoomSweepInterval + time.Second)
	}
	if row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID); err != nil || row.Ended != nil {
		t.Fatalf("the sweep ended a call whose human is in the room: %+v, %v", row, err)
	}
}

// Review focus 2: two humans plus the Bot, one dropped and pending removal: the other human is in
// the room and can commit the Remove, so the call is kept.
func TestBotsOnlyKeepsACallWithAnotherHuman(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	botDev := seedCallBot(t, f.e, f.ch, f.group)
	_, other, _ := joinedMember(t, f.e, f.ch, f.group, "other")
	f.e.Clk.Advance(5 * time.Second)
	putInstanceRemove(t, f.e, f.group, f.dev, 0)
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: botDev.String()}, &livekit.ParticipantInfo{Identity: other.String()})
	for range 3 {
		f.calls.SweepRooms(t.Context())
		f.e.Clk.Advance(api.RoomSweepInterval + time.Second)
	}
	if row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID); err != nil || row.Ended != nil {
		t.Fatalf("the sweep ended a call with a second human in the room: %+v, %v", row, err)
	}
}

package api_test

import (
	"log/slog"
	"net/http"
	"testing"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
)

// bot2.2 task 8, Jonas 2026-11-11: "the last joined client should take over voice - just like
// discord". A user's device that LiveKit reports joined (participant_joined, so it is connected)
// removes every other device of the same user from every call room, each with pending reason 3.

// takeoverEnv is pendingEnv (the member's laptop in the owner's call room) with the member's phone,
// a leaf of the same call group, and call events over the same Calls.
type takeoverEnv struct {
	*pendingEnv
	phone  id.ID
	events *api.CallEvents
}

func newTakeoverEnv(t *testing.T) *takeoverEnv {
	t.Helper()
	p := newPendingEnv(t)
	phone := secondDevice(t, p.e, p.member, "member-phone")
	seedLeaf(t, p.e, p.group, phone, 3, nil)
	events := api.NewCallEvents(p.e.Repo, api.NewResolver(p.e.Repo), &callDS{repo: p.e.Repo}, p.stub, p.calls, &voiceGW{},
		p.e.Clk, slog.New(slog.DiscardHandler))
	return &takeoverEnv{pendingEnv: p, phone: phone, events: events}
}

// joined delivers LiveKit's participant_joined for identity in room.
func (k *takeoverEnv) joined(t *testing.T, room, identity string) {
	t.Helper()
	k.events.Handle(t.Context(), &livekit.WebhookEvent{Event: webhook.EventParticipantJoined, Id: "EV_" + id.New().String(),
		Room: &livekit.Room{Name: room}, Participant: &livekit.ParticipantInfo{Identity: identity}})
	k.events.FlushAnnouncements()
}

// connect admits dev to room through the /rtc gate and then reports it joined, with the room's
// participants then being present.
func (k *takeoverEnv) connect(t *testing.T, room string, dev id.ID, present ...*livekit.ParticipantInfo) {
	t.Helper()
	k.calls.Admitted(t.Context(), room, dev, false)
	k.stub.setPresent(room, present...)
	k.joined(t, room, dev.String())
}

// secondRoom opens a call in a second voice channel of the community, the owner and the member's
// phone leaves of its group, and answers its room.
func (k *takeoverEnv) secondRoom(t *testing.T) string {
	t.Helper()
	ch, _, status := newChannel(t, k.e, k.cid, k.ownerTok, 1 /* voice */, 1, 2, "voice-two")
	if status != http.StatusCreated {
		t.Fatalf("second voice channel = %d", status)
	}
	group := seedCallGroup(t, k.e, ch, k.cid, callGroupEpoch)
	seedLeaf(t, k.e, group, deviceOf(t, k.e, k.ownerTok), 3, nil)
	seedLeaf(t, k.e, group, k.phone, 3, nil)
	before := len(k.stub.minted())
	if status, body := k.e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", k.ownerTok, []any{}); status != http.StatusCreated {
		t.Fatalf("second call start = %d %x", status, body)
	}
	return k.stub.minted()[before][0]
}

func removalsOf(stub *stubSFU, dev id.ID) int {
	n := 0
	for _, r := range deviceRemovals(stub) {
		if r[1] == dev.String() {
			n++
		}
	}
	return n
}

func part(dev id.ID) *livekit.ParticipantInfo {
	return &livekit.ParticipantInfo{Identity: dev.String()}
}

func TestAdmitTakesOverOtherDevice(t *testing.T) {
	k := newTakeoverEnv(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.calls.Admitted(t.Context(), k.room, k.memberDev, false)
	room2 := k.secondRoom(t)
	// The phone is admitted at the gate but not yet connected: the laptop stays (cut only once the
	// new device is connected).
	k.calls.Admitted(t.Context(), room2, k.phone, false)
	if n := removalsOf(k.stub, k.memberDev); n != 0 {
		t.Fatalf("the laptop was cut %d times before the phone connected", n)
	}
	k.stub.setPresent(room2, part(owner), part(k.phone))
	k.joined(t, room2, k.phone.String())
	if !removedDevice(k.stub, k.room, k.memberDev) {
		t.Fatalf("the laptop was not removed from its room; removals %v", deviceRemovals(k.stub))
	}
	if got := k.pending(t, k.memberTok); got != 3 {
		t.Fatalf("the laptop's pending = %d, want 3 (taken over)", got)
	}
	if n := removalsOf(k.stub, k.phone); n != 0 {
		t.Fatalf("the phone was cut %d times", n)
	}
	if got := k.pending(t, "member-phone"); got != 0 {
		t.Fatalf("the phone's pending = %d, want 0", got)
	}
}

func TestAdmitSameRoomTakesOver(t *testing.T) {
	k := newTakeoverEnv(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.calls.Admitted(t.Context(), k.room, k.memberDev, false)
	k.connect(t, k.room, k.phone, part(owner), part(k.memberDev), part(k.phone))
	if !removedDevice(k.stub, k.room, k.memberDev) {
		t.Fatalf("the laptop was not removed; removals %v", deviceRemovals(k.stub))
	}
	if n := removalsOf(k.stub, k.phone); n != 0 {
		t.Fatalf("the phone was cut %d times", n)
	}
	if got := k.pending(t, k.memberTok); got != 3 {
		t.Fatalf("the laptop's pending = %d, want 3", got)
	}
}

// The laptop and the phone are admitted close together; the phone's participant_joined comes first
// (the laptop is not in the room yet, so nothing is cut), then the laptop's, late. The phone holds
// the newer admission: it is not cut, and the laptop is cut, once.
func TestTakeoverTiebreak(t *testing.T) {
	k := newTakeoverEnv(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.calls.Admitted(t.Context(), k.room, k.memberDev, false)
	k.calls.Admitted(t.Context(), k.room, k.phone, false)
	k.stub.setPresent(k.room, part(owner), part(k.phone))
	k.joined(t, k.room, k.phone.String())
	if n := len(deviceRemovals(k.stub)); n != 0 {
		t.Fatalf("the phone's join cut %v with nothing else of its user in a room", deviceRemovals(k.stub))
	}
	k.stub.setPresent(k.room, part(owner), part(k.phone), part(k.memberDev))
	k.joined(t, k.room, k.memberDev.String())
	if n := removalsOf(k.stub, k.phone); n != 0 {
		t.Fatalf("the late join of the older admission cut the phone %d times", n)
	}
	if n := removalsOf(k.stub, k.memberDev); n != 1 {
		t.Fatalf("the laptop was cut %d times, want once", n)
	}
	if got := k.pending(t, k.memberTok); got != 3 {
		t.Fatalf("the laptop's pending = %d, want 3", got)
	}
	if got := k.pending(t, "member-phone"); got != 0 {
		t.Fatalf("the phone's pending = %d, want 0", got)
	}
}

// A signal resume of the older device is no newer join: it never takes voice back.
func TestTakeoverResumeIsNoNewerJoin(t *testing.T) {
	k := newTakeoverEnv(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.calls.Admitted(t.Context(), k.room, k.memberDev, false)
	k.calls.Admitted(t.Context(), k.room, k.phone, false)
	k.calls.Admitted(t.Context(), k.room, k.memberDev, true) // the laptop's signalling reconnects
	k.stub.setPresent(k.room, part(owner), part(k.memberDev), part(k.phone))
	k.joined(t, k.room, k.phone.String())
	if n := removalsOf(k.stub, k.phone); n != 0 {
		t.Fatalf("the phone was cut %d times after the laptop's resume", n)
	}
	if !removedDevice(k.stub, k.room, k.memberDev) {
		t.Fatalf("the laptop was not removed; removals %v", deviceRemovals(k.stub))
	}
}

func TestTakeoverOtherUsersUntouched(t *testing.T) {
	k := newTakeoverEnv(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.calls.Admitted(t.Context(), k.room, owner, false)
	k.connect(t, k.room, k.phone, part(owner), part(k.phone))
	if n := len(deviceRemovals(k.stub)); n != 0 {
		t.Fatalf("a join of the member's only connected device cut %v", deviceRemovals(k.stub))
	}
	k.connect(t, k.room, k.phone, part(owner), part(k.memberDev), part(k.phone))
	if n := removalsOf(k.stub, owner); n != 0 {
		t.Fatalf("another user's device was cut %d times", n)
	}
	if got := k.pending(t, k.ownerTok); got != 0 {
		t.Fatalf("the owner's pending = %d, want 0", got)
	}
}

func TestTakeoverSkipsShadowIdentity(t *testing.T) {
	k := newTakeoverEnv(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.calls.Admitted(t.Context(), k.room, k.memberDev, false)
	k.calls.Admitted(t.Context(), k.room, k.phone, false)
	shadow := &livekit.ParticipantInfo{Identity: k.phone.String() + "#screen"}
	k.stub.setPresent(k.room, part(owner), part(k.memberDev), shadow)
	// A shadow's join is no device joining: it takes nothing over.
	k.joined(t, k.room, shadow.GetIdentity())
	if n := len(deviceRemovals(k.stub)); n != 0 {
		t.Fatalf("a shadow's join cut %v", deviceRemovals(k.stub))
	}
	// The phone itself joins: the laptop goes, and the phone's own shadow stays.
	k.stub.setPresent(k.room, part(owner), part(k.memberDev), part(k.phone), shadow)
	k.joined(t, k.room, k.phone.String())
	if !removedDevice(k.stub, k.room, k.memberDev) {
		t.Fatalf("the laptop was not removed; removals %v", deviceRemovals(k.stub))
	}
	if n := removalsOf(k.stub, k.phone); n != 0 {
		t.Fatalf("the phone or its shadow was cut %d times", n)
	}
}

// A managed Bot's devices never take voice over and are never cut by a takeover.
func TestTakeoverSkipsBots(t *testing.T) {
	k := newTakeoverEnv(t)
	botDev := seedCallBot(t, k.e, k.ch, k.group)
	row, err := k.e.Repo.GetDevice(t.Context(), botDev)
	if err != nil {
		t.Fatal(err)
	}
	bot := row.UserID
	botDev2 := id.New()
	d := newAPITestDevice(botDev2, bot, 1)
	d.DSKPub = uniqueAPITestDSK(t)
	if err := k.e.Repo.CreateDevice(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	k.calls.Admitted(t.Context(), k.room, botDev, false)
	k.calls.Admitted(t.Context(), k.room, botDev2, false)
	k.stub.setPresent(k.room, part(botDev), part(botDev2))
	k.calls.TakeOverVoiceForTest(t.Context(), bot, botDev2, k.room)
	if n := len(deviceRemovals(k.stub)); n != 0 {
		t.Fatalf("a Bot's join cut %v", deviceRemovals(k.stub))
	}
}

func TestTakeoverRetriesFailedRemove(t *testing.T) {
	k := newTakeoverEnv(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.calls.Admitted(t.Context(), k.room, k.memberDev, false)
	k.stub.mu.Lock()
	k.stub.failRemovals = 1
	k.stub.mu.Unlock()
	k.connect(t, k.room, k.phone, part(owner), part(k.memberDev), part(k.phone))
	if n := removalsOf(k.stub, k.memberDev); n != 1 {
		t.Fatalf("the laptop's removal was tried %d times, want once", n)
	}
	if got := k.pending(t, k.memberTok); got != 3 {
		t.Fatalf("a failed removal's pending = %d, want 3", got)
	}
	k.calls.AFKMaintenance(t.Context())
	if n := removalsOf(k.stub, k.memberDev); n != 2 {
		t.Fatalf("after a maintenance pass the laptop's removal was tried %d times, want 2", n)
	}
	// Done: the next pass tries nothing.
	k.calls.AFKMaintenance(t.Context())
	if n := removalsOf(k.stub, k.memberDev); n != 2 {
		t.Fatalf("a finished cut was retried again (%d tries)", n)
	}
}

// A queued retry of a device admitted again since is dropped: its rejoin is the newer join.
func TestTakeoverRetryDroppedOnReadmit(t *testing.T) {
	k := newTakeoverEnv(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.calls.Admitted(t.Context(), k.room, k.memberDev, false)
	k.stub.mu.Lock()
	k.stub.failRemovals = 1
	k.stub.mu.Unlock()
	k.connect(t, k.room, k.phone, part(owner), part(k.memberDev), part(k.phone))
	k.calls.Admitted(t.Context(), k.room, k.memberDev, false)
	k.calls.AFKMaintenance(t.Context())
	if n := removalsOf(k.stub, k.memberDev); n != 1 {
		t.Fatalf("a retry cut the readmitted laptop (%d tries)", n)
	}
}

func TestTakeoverReasonClearedOnReadmit(t *testing.T) {
	k := newTakeoverEnv(t)
	owner := deviceOf(t, k.e, k.ownerTok)
	k.calls.Admitted(t.Context(), k.room, k.memberDev, false)
	k.connect(t, k.room, k.phone, part(owner), part(k.memberDev), part(k.phone))
	if got := k.pending(t, k.memberTok); got != 3 {
		t.Fatalf("the laptop's pending = %d, want 3", got)
	}
	// The laptop joins again: its reason clears, and it takes voice back from the phone.
	k.connect(t, k.room, k.memberDev, part(owner), part(k.memberDev), part(k.phone))
	if got := k.pending(t, k.memberTok); got != 0 {
		t.Fatalf("the readmitted laptop's pending = %d, want 0", got)
	}
	if !removedDevice(k.stub, k.room, k.phone) {
		t.Fatalf("the laptop's rejoin did not take voice back; removals %v", deviceRemovals(k.stub))
	}
	if got := k.pending(t, "member-phone"); got != 3 {
		t.Fatalf("the phone's pending = %d, want 3", got)
	}
}

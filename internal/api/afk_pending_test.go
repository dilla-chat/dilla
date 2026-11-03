package api_test

import (
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// pendingEnv is membershipCallEnv (the owner's call in "voice", the member's device in its room)
// with the built-in AFK channel at a 60 s timeout and the moves announced through a recording
// gateway.
type pendingEnv struct {
	*membershipCallEnv
	afk id.ID
	gw  *changedGW
}

func newPendingEnv(t *testing.T) *pendingEnv {
	t.Helper()
	b := newMembershipCallEnvWithoutLoop(t)
	afk, _ := afkPointer(t, b.e, b.cid)
	if err := b.e.Repo.SetCommunityAFK(t.Context(), b.cid, store.CommunityAFK{ChannelID: &afk, TimeoutSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	gw := &changedGW{seen: map[id.ID][]id.ID{}}
	b.calls.WithAFKNotify(api.NewStructureNotifier(gw, b.e.Repo, slog.New(slog.DiscardHandler)))
	return &pendingEnv{membershipCallEnv: b, afk: afk, gw: gw}
}

func (p *pendingEnv) pending(t *testing.T, tok string) uint64 {
	t.Helper()
	ch, _, _, pending := afkSnapshotOf(t, p.e, p.cid, tok)
	if ch == nil || *ch != p.afk {
		t.Fatalf("snapshot channel = %v, want %s", ch, p.afk)
	}
	return pending
}

// idleMove admits the member's device, lets it idle past the timeout and sweeps.
func (p *pendingEnv) idleMove(t *testing.T) {
	t.Helper()
	p.calls.Admitted(t.Context(), p.room, p.memberDev, false)
	p.e.Clk.Advance(61 * time.Second)
	p.calls.SweepAFK(t.Context())
	if !removedDevice(p.stub, p.room, p.memberDev) {
		t.Fatal("the idle device was not removed from its room")
	}
}

// secondDevice is another enrolled device of user, and its bearer token.
func secondDevice(t *testing.T, e *env, user id.ID, tok string) id.ID {
	t.Helper()
	dev := id.New()
	if err := e.Repo.CreateDevice(t.Context(), newAPITestDevice(dev, user, e.Clk.Now().Unix())); err != nil {
		t.Fatal(err)
	}
	e.sess[tok] = auth.Session{UserID: user, DeviceID: dev, Scope: auth.ScopeEnrolled}
	return dev
}

func TestAFKSweepMovesAndMarksPending(t *testing.T) {
	p := newPendingEnv(t)
	secondDevice(t, p.e, p.member, "member-phone")
	if got := p.pending(t, p.memberTok); got != 0 {
		t.Fatalf("pending before any move = %d", got)
	}
	p.idleMove(t)
	if got := p.pending(t, p.memberTok); got != 1 {
		t.Fatalf("the moved device's pending = %d, want 1", got)
	}
	if got := p.pending(t, "member-phone"); got != 0 {
		t.Fatalf("the same user's other device's pending = %d, want 0", got)
	}
	if seen := p.gw.seen[p.member]; !slices.Contains(seen, p.cid) {
		t.Fatalf("the moved user was told %v, want op 54 for %s", seen, p.cid)
	}
}

func TestAFKPendingIsPerDevice(t *testing.T) {
	p := newPendingEnv(t)
	ownerDev := deviceOf(t, p.e, p.ownerTok)
	p.calls.Admitted(t.Context(), p.room, ownerDev, false)
	// Another member's activity, settings write and snapshot read move nobody.
	activity := "/v1/communities/" + p.cid.String() + "/afk/activity"
	if status, body := p.e.Do(http.MethodPost, activity, p.ownerTok, []any{p.ch[:]}); status != http.StatusNoContent {
		t.Fatalf("owner activity = %d (%x)", status, body)
	}
	if status, _ := p.e.Do(http.MethodPut, "/v1/communities/"+p.cid.String()+"/afk/settings", p.ownerTok, []any{uint64(60)}); status != http.StatusNoContent {
		t.Fatalf("settings = %d", status)
	}
	if got := p.pending(t, p.ownerTok); got != 0 {
		t.Fatalf("owner pending = %d", got)
	}
	if got := p.pending(t, p.memberTok); got != 0 {
		t.Fatalf("member pending after the owner's reports = %d", got)
	}
	if removedDevice(p.stub, p.room, p.memberDev) {
		t.Fatal("someone else's report moved the member")
	}
	// The member's move is the member device's alone.
	p.calls.Admitted(t.Context(), p.room, p.memberDev, false)
	p.e.Clk.Advance(40 * time.Second)
	if status, _ := p.e.Do(http.MethodPost, activity, p.ownerTok, []any{p.ch[:]}); status != http.StatusNoContent {
		t.Fatal("owner activity")
	}
	p.e.Clk.Advance(21 * time.Second)
	p.calls.SweepAFK(t.Context())
	if !removedDevice(p.stub, p.room, p.memberDev) {
		t.Fatal("the idle member was not moved")
	}
	if got := p.pending(t, p.memberTok); got != 1 {
		t.Fatalf("member pending after the move = %d, want 1", got)
	}
	if got := p.pending(t, p.ownerTok); got != 0 {
		t.Fatalf("owner pending after the member's move = %d", got)
	}
	if removedDevice(p.stub, p.room, ownerDev) {
		t.Fatal("the active owner was moved")
	}
}

func TestAFKPendingClearsOnAnyJoin(t *testing.T) {
	p := newPendingEnv(t)
	p.idleMove(t)
	if got := p.pending(t, p.memberTok); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	p.calls.Admitted(t.Context(), p.room, p.memberDev, false)
	if got := p.pending(t, p.memberTok); got != 0 {
		t.Fatalf("pending after an admission = %d, want 0", got)
	}
}

// The pending cut is device-scoped (fix round 4): every community's snapshot reports it to the
// moved device.
func TestAFKPendingIsDeviceScoped(t *testing.T) {
	p := newPendingEnv(t)
	other := createCommunity(t, p.e, p.ownerTok)
	joinCommunity(t, p.e, other, p.memberTok)
	p.idleMove(t)
	if _, _, _, pending := afkSnapshotOf(t, p.e, other, p.memberTok); pending != 1 {
		t.Fatalf("another community's snapshot reports pending %d, want 1", pending)
	}
}

// A call left live in the AFK channel by an earlier version (R-AW1 refuses any new one) is never
// tracked: its devices are not swept out of it.
func TestAFKCallIsNotTracked(t *testing.T) {
	p := newPendingEnv(t)
	group := seedCallGroup(t, p.e, p.afk, p.cid, callGroupEpoch)
	seedLeaf(t, p.e, group, p.memberDev, 3, nil)
	call := id.New()
	room := call.String() + "-1-abcdef01"
	if err := p.e.Repo.PutVoiceSession(t.Context(), store.VoiceSessionRow{CallID: call, ChannelID: p.afk, GroupID: &group,
		LivekitRoom: room, Started: p.e.Clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	p.stub.setPresent(room, &livekit.ParticipantInfo{Identity: p.memberDev.String()})
	p.calls.Admitted(t.Context(), room, p.memberDev, false)
	p.e.Clk.Advance(2 * time.Hour)
	p.calls.SweepAFK(t.Context())
	if removedDevice(p.stub, room, p.memberDev) {
		t.Fatal("a device in the AFK channel's own call was swept")
	}
}

func TestAFKExemptIsNeverSwept(t *testing.T) {
	p := newPendingEnv(t)
	p.calls.Admitted(t.Context(), p.room, p.memberDev, false)
	activity := "/v1/communities/" + p.cid.String() + "/afk/activity"
	if status, body := p.e.Do(http.MethodPost, activity, p.memberTok, []any{p.ch[:], uint64(1)}); status != http.StatusNoContent {
		t.Fatalf("exempt report = %d (%x)", status, body)
	}
	p.e.Clk.Advance(2 * time.Hour)
	p.calls.SweepAFK(t.Context())
	if removedDevice(p.stub, p.room, p.memberDev) {
		t.Fatal("an exempt device was swept")
	}
}

// W-1: exempt is a uint 0 or 1; any other uint and a raw CBOR boolean are refused, and exempt 0 is
// an ordinary report.
func TestAFKActivityExemptIsUintZeroOrOne(t *testing.T) {
	p := newPendingEnv(t)
	activity := "/v1/communities/" + p.cid.String() + "/afk/activity"
	if status, _ := p.e.Do(http.MethodPost, activity, p.memberTok, []any{p.ch[:], uint64(2)}); status != http.StatusBadRequest {
		t.Fatalf("exempt 2 = %d, want 400", status)
	}
	for _, tail := range []byte{0xf5, 0xf4} { // true, false
		head, err := cborx.Marshal([]any{p.ch[:]})
		if err != nil {
			t.Fatal(err)
		}
		raw := append([]byte{0x82}, append(head[1:], tail)...)
		if status, _ := p.e.DoRaw(http.MethodPost, activity, p.memberTok, "application/cbor", raw); status != http.StatusBadRequest {
			t.Fatalf("exempt as CBOR simple %#x = %d, want 400", tail, status)
		}
	}
	p.calls.Admitted(t.Context(), p.room, p.memberDev, false)
	if status, _ := p.e.Do(http.MethodPost, activity, p.memberTok, []any{p.ch[:], uint64(0)}); status != http.StatusNoContent {
		t.Fatalf("exempt 0 = %d, want 204 (control)", status)
	}
	p.e.Clk.Advance(61 * time.Second)
	p.calls.SweepAFK(t.Context())
	if !removedDevice(p.stub, p.room, p.memberDev) {
		t.Fatal("exempt 0 exempted the device")
	}
}

func TestAFKExemptBeforeAdmission(t *testing.T) {
	p := newPendingEnv(t)
	activity := "/v1/communities/" + p.cid.String() + "/afk/activity"
	if status, body := p.e.Do(http.MethodPost, activity, p.memberTok, []any{p.ch[:], uint64(1)}); status != http.StatusNoContent {
		t.Fatalf("exempt report = %d (%x)", status, body)
	}
	p.calls.Admitted(t.Context(), p.room, p.memberDev, false)
	p.e.Clk.Advance(2 * time.Hour)
	p.calls.SweepAFK(t.Context())
	if removedDevice(p.stub, p.room, p.memberDev) {
		t.Fatal("an exempt report sent before the admission did not hold")
	}
	// A later admission into another room is tracked again.
	other, _, status := newChannel(t, p.e, p.cid, p.ownerTok, uint64(api.ChannelVoice), 1, 2, "other")
	if status != http.StatusCreated {
		t.Fatalf("other = %d", status)
	}
	group := seedCallGroup(t, p.e, other, p.cid, callGroupEpoch)
	seedLeaf(t, p.e, group, p.memberDev, 3, nil)
	if status, body := p.e.Do(http.MethodPost, "/v1/channels/"+other.String()+"/calls", p.memberTok, []any{}); status != http.StatusCreated {
		t.Fatalf("join other = %d (%x)", status, body)
	}
	room := p.stub.lastMint().Room
	p.stub.setPresent(room, &livekit.ParticipantInfo{Identity: p.memberDev.String()})
	p.calls.Admitted(t.Context(), room, p.memberDev, false)
	p.e.Clk.Advance(61 * time.Second)
	p.calls.SweepAFK(t.Context())
	if !removedDevice(p.stub, room, p.memberDev) {
		t.Fatal("an admission into another room was not tracked again")
	}
}

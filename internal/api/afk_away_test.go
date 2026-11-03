package api_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// AW2: being in the AFK channel means no voice connection. The instance keeps a stored away roster
// per (community, user), announces each change with op 55 afk_away to the AFK channel's audience,
// and refuses every call in the AFK channel (R-AW1).

// awayFrame is one op 55 as a user's live connections received it.
type awayFrame struct {
	to, community, user id.ID
	since               uint64
}

// awayGW records every op 55 afk_away per recipient.
type awayGW struct {
	mu     sync.Mutex
	frames []awayFrame
	// flushes wait until every Calls writing to this recorder has sent what it queued (fix round
	// 2: op 55 goes out on a drainer goroutine).
	flushMu sync.Mutex
	flushes []func()
}

func (g *awayGW) addFlush(f func()) {
	g.flushMu.Lock()
	defer g.flushMu.Unlock()
	g.flushes = append(g.flushes, f)
}

func (g *awayGW) flush() {
	g.flushMu.Lock()
	fs := append([]func(){}, g.flushes...)
	g.flushMu.Unlock()
	for _, f := range fs {
		f()
	}
}

func (g *awayGW) DeliverUser(u id.ID, f gateway.Frame) {
	if f.Op != gateway.OpAFKAway {
		return
	}
	var raw []cbor.RawMessage
	if cborx.Unmarshal(f.Payload, &raw) != nil || len(raw) != 3 {
		return
	}
	var cid, uid id.ID
	var since uint64
	if cborx.Unmarshal(raw[0], &cid) != nil || cborx.Unmarshal(raw[1], &uid) != nil || cborx.Unmarshal(raw[2], &since) != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.frames = append(g.frames, awayFrame{to: u, community: cid, user: uid, since: since})
}

// sent is every op 55 about user in community, as (recipient, since), in order.
func (g *awayGW) sent(community, user id.ID) []awayFrame {
	g.flush()
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []awayFrame
	for _, f := range g.frames {
		if f.community == community && f.user == user {
			out = append(out, f)
		}
	}
	return out
}

func (g *awayGW) count() int {
	g.flush()
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.frames)
}

// awayEnv is pendingEnv with op 55 recorded and a switchable gateway connection per user.
type awayEnv struct {
	*pendingEnv
	away    *awayGW
	owner   id.ID
	connMu  sync.Mutex
	offline map[id.ID]bool
}

func newAwayEnv(t *testing.T) *awayEnv {
	t.Helper()
	p := newPendingEnv(t)
	a := &awayEnv{pendingEnv: p, away: &awayGW{}, owner: p.e.sess[p.ownerTok].UserID, offline: map[id.ID]bool{}}
	p.calls.WithAFKAway(a.away, a.connected)
	t.Cleanup(p.calls.StopAFK)
	a.away.addFlush(p.calls.FlushAFKAwayForTest)
	if err := p.calls.SeedAFKAway(t.Context()); err != nil {
		t.Fatal(err)
	}
	return a
}

func (a *awayEnv) connected(u id.ID) bool {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return !a.offline[u]
}

func (a *awayEnv) setOffline(u id.ID, off bool) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	a.offline[u] = off
}

// roster is the stored away roster of community cid.
func (a *awayEnv) roster(t *testing.T, cid id.ID) []store.AFKAway {
	t.Helper()
	got, err := a.e.Repo.ListAFKAway(t.Context(), cid)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (a *awayEnv) isAway(t *testing.T, cid, user id.ID) bool {
	t.Helper()
	return slices.ContainsFunc(a.roster(t, cid), func(r store.AFKAway) bool { return r.UserID == user })
}

// markAway writes an away row directly, as an earlier sweep would have.
func (a *awayEnv) markAway(t *testing.T, cid, user id.ID, since int64) {
	t.Helper()
	if _, err := a.e.Repo.SetAFKAway(t.Context(), cid, user, since); err != nil {
		t.Fatal(err)
	}
	// A row written behind the instance's back is known to it only after a seed, as at boot.
	if err := a.calls.SeedAFKAway(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// awayEntry is one element of the snapshot's fifth element.
type awayEntry struct {
	_     struct{} `cbor:",toarray"`
	User  id.ID
	Since uint64
}

// afkAwayOf is the fifth element of GET /v1/communities/{id}/afk as tok: [[user_id, since_ts]].
func afkAwayOf(t *testing.T, e *env, cid id.ID, tok string) []awayEntry {
	t.Helper()
	status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/afk", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET afk = %d (%x)", status, body)
	}
	var raw []cbor.RawMessage
	mustUnmarshal(t, body, &raw)
	if len(raw) != 5 {
		t.Fatalf("AFK snapshot has %d elements, want 5 (%x)", len(raw), body)
	}
	var out []awayEntry
	mustUnmarshal(t, raw[4], &out)
	return out
}

// toEveryone checks that the op 55 frames are one per eligible user (owner and member) with since.
func toEveryone(t *testing.T, frames []awayFrame, want []id.ID, since uint64) {
	t.Helper()
	var to []id.ID
	for _, f := range frames {
		if f.since != since {
			t.Fatalf("op 55 since = %d, want %d (%v)", f.since, since, frames)
		}
		to = append(to, f.to)
	}
	slices.SortFunc(to, func(x, y id.ID) int { return bytes.Compare(x[:], y[:]) })
	want = slices.Clone(want)
	slices.SortFunc(want, func(x, y id.ID) int { return bytes.Compare(x[:], y[:]) })
	if !slices.Equal(to, want) {
		t.Fatalf("op 55 went to %v, want %v", to, want)
	}
}

// R-AW3: the sweep's cut marks the user away when no other device of theirs is in a call, with
// since = the cut, tells the AFK channel's audience once (op 55), lists them in the snapshot, and
// keeps the device-scoped pending (R-AW6).
func TestAFKSweepMarksAway(t *testing.T) {
	a := newAwayEnv(t)
	a.idleMove(t)
	now := uint64(a.e.Clk.Now().Unix())
	if !a.isAway(t, a.cid, a.member) {
		t.Fatalf("roster after the sweep = %v, want the member", a.roster(t, a.cid))
	}
	toEveryone(t, a.away.sent(a.cid, a.member), []id.ID{a.owner, a.member}, now)
	got := afkAwayOf(t, a.e, a.cid, a.ownerTok)
	if len(got) != 1 || got[0].User != a.member || got[0].Since != now {
		t.Fatalf("snapshot away = %+v, want [[member, %d]]", got, now)
	}
	if p := a.pending(t, a.memberTok); p != 1 {
		t.Fatalf("pending after the sweep = %d, want 1 (R-AW6)", p)
	}
	// A second sweep finds nothing to mark again and sends nothing.
	before := a.away.count()
	a.calls.SweepAFK(t.Context())
	if a.away.count() != before {
		t.Fatal("a sweep with nothing to change sent op 55")
	}
}

// R-AW2, R-AW3: another device of the user is in a call, so the idle device is only cut.
func TestAFKSweepDoesNotMarkWhenAnotherDeviceIsInACall(t *testing.T) {
	a := newAwayEnv(t)
	phone := secondDevice(t, a.e, a.member, "member-phone")
	a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: deviceOf(t, a.e, a.ownerTok).String()},
		&livekit.ParticipantInfo{Identity: a.memberDev.String()}, &livekit.ParticipantInfo{Identity: phone.String()})
	a.idleMove(t)
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("the member was marked away while their phone is in a call")
	}
	if n := len(a.away.sent(a.cid, a.member)); n != 0 {
		t.Fatalf("op 55 sent %d times, want none", n)
	}
	if removedDevice(a.stub, a.room, phone) {
		t.Fatal("the active phone was cut")
	}
}

// A device admitted within the join window but not yet listed by the SFU is in a call too.
func TestAFKSweepDoesNotMarkWhenAnotherDeviceIsJoining(t *testing.T) {
	a := newAwayEnv(t)
	phone := secondDevice(t, a.e, a.member, "member-phone")
	seedLeaf(t, a.e, a.group, phone, 3, nil)
	a.calls.Admitted(t.Context(), a.room, a.memberDev, false)
	a.e.Clk.Advance(50 * time.Second)
	a.calls.Admitted(t.Context(), a.room, phone, false) // not yet in the SFU's list
	a.e.Clk.Advance(11 * time.Second)
	a.calls.SweepAFK(t.Context())
	if !removedDevice(a.stub, a.room, a.memberDev) {
		t.Fatal("the idle device was not cut")
	}
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("the member was marked away while their phone is joining a call")
	}
}

// R-AW4: any admission of any device of the user clears every away row, in every community, with
// op 55 since 0 for each.
func TestAFKAdmissionClearsAway(t *testing.T) {
	a := newAwayEnv(t)
	other := createCommunity(t, a.e, a.ownerTok)
	joinCommunity(t, a.e, other, a.memberTok)
	a.markAway(t, a.cid, a.member, 100)
	a.markAway(t, other, a.member, 100)
	a.calls.Admitted(t.Context(), a.room, a.memberDev, false)
	for _, cid := range []id.ID{a.cid, other} {
		if a.isAway(t, cid, a.member) {
			t.Fatalf("the member is still away in %s after an admission", cid)
		}
		toEveryone(t, a.away.sent(cid, a.member), []id.ID{a.owner, a.member}, 0)
	}
	// An admission of a user who is not away sends nothing.
	before := a.away.count()
	a.calls.Admitted(t.Context(), a.room, a.memberDev, false)
	if a.away.count() != before {
		t.Fatal("an admission with nothing to clear sent op 55")
	}
}

// R-AW7, edge "swept while rejoining": a fresh admission that lands after the cut and before the
// mark wins, so the user is not left away while in a call.
func TestAFKMarkRacingAFreshAdmissionLosesToIt(t *testing.T) {
	a := newAwayEnv(t)
	a.calls.SetAFKHookForTest(func(stage string) {
		if stage == "cut" {
			a.calls.Admitted(t.Context(), a.room, a.memberDev, false)
		}
	})
	a.idleMove(t)
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("the mark won over an admission that came after the cut")
	}
}

// Q2 under the race: a user whose last connection is gone by the mark is not marked.
func TestAFKSweepDoesNotMarkAnOfflineUser(t *testing.T) {
	a := newAwayEnv(t)
	a.setOffline(a.member, true)
	a.idleMove(t)
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("an offline user was marked away")
	}
}

// Q1: a message the user posts in a community clears that community's row only.
func TestAFKMessageClearsAway(t *testing.T) {
	a := newAwayEnv(t)
	log := slog.New(slog.DiscardHandler)
	keys := api.NewStaticFrankingKeys(api.FrankingKey{ID: id.New(), Key: bytes.Repeat([]byte{0x09}, 32)})
	api.NewReadable(a.e.Repo, api.NewResolver(a.e.Repo), &recordingFanout{}, keys, a.e.Clk, log).WithAFK(a.calls).Register(a.e.Mux)
	other := createCommunity(t, a.e, a.ownerTok)
	joinCommunity(t, a.e, other, a.memberTok)
	text := readableChannel(t, a.e, a.cid, a.ownerTok)
	a.markAway(t, a.cid, a.member, 100)
	a.markAway(t, other, a.member, 100)
	postReadable(t, a.e, text, a.memberTok, envelope0(t, "back"))
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("the member is still away after posting in the community")
	}
	toEveryone(t, a.away.sent(a.cid, a.member), []id.ID{a.owner, a.member}, 0)
	if !a.isAway(t, other, a.member) {
		t.Fatal("a post in one community cleared another community's row")
	}
}

// Q1: an end-to-end encrypted upload into a community text group clears that community's row; a
// call group's or a DM's upload clears nothing.
func TestAFKUploadClearsAway(t *testing.T) {
	a := newAwayEnv(t)
	c := a.cid
	text := store.GroupRow{GroupID: id.New(), Binding: []byte{0x80}, Kind: api.GroupText, CommunityID: &c, TargetID: a.ch,
		Ciphersuite: 1, Epoch: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1,
		Created: a.e.Clk.Now().Unix()}
	if err := a.e.Repo.CreateGroup(t.Context(), text); err != nil {
		t.Fatal(err)
	}
	a.markAway(t, a.cid, a.member, 100)
	a.calls.AFKActiveInGroup(t.Context(), a.member, a.group) // the call group
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("an upload into a call group cleared away")
	}
	a.calls.AFKActiveInGroup(t.Context(), a.member, text.GroupID)
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("an upload into the community's text group did not clear away")
	}
	toEveryone(t, a.away.sent(a.cid, a.member), []id.ID{a.owner, a.member}, 0)
}

// Q1: typing in a channel of the community clears that community's row.
func TestAFKTypingClearsAway(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100)
	a.calls.AFKActiveInChannel(t.Context(), a.member, a.ch)
	if a.isAway(t, a.cid, a.member) {
		t.Fatal("typing did not clear away")
	}
	toEveryone(t, a.away.sent(a.cid, a.member), []id.ID{a.owner, a.member}, 0)
}

// Q2: the user's last gateway connection closing clears every row at once, with op 55 since 0.
// The gateway calls this only for the last connection (gateway.TestOnUserOfflineFiresOnTheLast...).
func TestAFKLastConnectionClosingClearsAway(t *testing.T) {
	a := newAwayEnv(t)
	other := createCommunity(t, a.e, a.ownerTok)
	joinCommunity(t, a.e, other, a.memberTok)
	a.markAway(t, a.cid, a.member, 100)
	a.markAway(t, other, a.member, 100)
	a.setOffline(a.member, true)
	a.calls.AFKOffline(t.Context(), a.member)
	a.e.Clk.Advance(31 * time.Second) // Q2 as revised: the clear waits out a 30 s grace
	waitOffline(t, a.calls)
	for _, cid := range []id.ID{a.cid, other} {
		if a.isAway(t, cid, a.member) {
			t.Fatalf("still away in %s after going offline", cid)
		}
		toEveryone(t, a.away.sent(cid, a.member), []id.ID{a.owner, a.member}, 0)
	}
}

// R-AW1: nobody starts or joins a call in the AFK channel, and the /rtc gate refuses its room.
func TestAFKCallIsRefused(t *testing.T) {
	a := newAwayEnv(t)
	group := seedCallGroup(t, a.e, a.afk, a.cid, callGroupEpoch)
	seedLeaf(t, a.e, group, a.memberDev, 3, nil)
	before := len(a.stub.minted())
	status, body := a.e.Do(http.MethodPost, "/v1/channels/"+a.afk.String()+"/calls", a.memberTok, []any{})
	if status != http.StatusForbidden || a.e.ErrCode(body) != "E_FORBIDDEN" {
		t.Fatalf("start in AFK = %d (%x), want 403 E_FORBIDDEN", status, body)
	}
	if len(a.stub.minted()) != before {
		t.Fatal("a token was minted for the AFK channel")
	}
	// A call a web3.10 instance left live there: the gate refuses its room.
	room := legacyAFKCall(t, a, group)
	if _, err := a.calls.AdmitRoom(t.Context(), room, a.memberDev); codeOf(err) != "E_FORBIDDEN" {
		t.Fatalf("AdmitRoom(AFK) = %v, want E_FORBIDDEN", err)
	}
}

// legacyAFKCall writes a live call in the AFK channel, as a web3.10 instance left one, and answers
// its room.
func legacyAFKCall(t *testing.T, a *awayEnv, group id.ID) string {
	t.Helper()
	call := id.New()
	room := call.String() + "-1-abcdef01"
	if err := a.e.Repo.PutVoiceSession(t.Context(), store.VoiceSessionRow{CallID: call, ChannelID: a.afk, GroupID: &group,
		LivekitRoom: room, Started: a.e.Clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	return room
}

// R-AW3 "by choosing AFK": PUT …/afk/away cuts every device of the user in every room — a second
// device too — and marks the user away; a non-member is 404, a body other than [] is 400, and
// there is no DELETE (Q3).
func TestAFKPutAwayCutsEveryDeviceAndMarksAway(t *testing.T) {
	a := newAwayEnv(t)
	path := "/v1/communities/" + a.cid.String() + "/afk/away"
	phone := secondDevice(t, a.e, a.member, "member-phone")
	ownerDev := deviceOf(t, a.e, a.ownerTok)
	a.stub.setPresent(a.room, &livekit.ParticipantInfo{Identity: ownerDev.String()},
		&livekit.ParticipantInfo{Identity: a.memberDev.String()}, &livekit.ParticipantInfo{Identity: phone.String() + "#screen"})
	if status, _ := a.e.Do(http.MethodPut, path, a.memberTok, []any{uint64(1)}); status != http.StatusBadRequest {
		t.Fatalf("PUT [1] = %d, want 400", status)
	}
	if status, body := a.e.Do(http.MethodPut, path, a.memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("PUT [] = %d (%x), want 204", status, body)
	}
	for _, d := range []id.ID{a.memberDev, phone} {
		if !removedDevice(a.stub, a.room, d) {
			t.Fatalf("device %s was not cut", d)
		}
	}
	if removedDevice(a.stub, a.room, ownerDev) {
		t.Fatal("the owner was cut")
	}
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("PUT did not mark the member away")
	}
	toEveryone(t, a.away.sent(a.cid, a.member), []id.ID{a.owner, a.member}, uint64(a.e.Clk.Now().Unix()))
	// Idempotent: a second PUT keeps the first since and sends nothing.
	before := a.away.count()
	a.e.Clk.Advance(time.Minute)
	if status, _ := a.e.Do(http.MethodPut, path, a.memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("second PUT = %d", status)
	}
	if a.away.count() != before {
		t.Fatal("a second PUT sent op 55")
	}
	_, strangerTok := a.e.NewUser("stranger")
	if status, _ := a.e.Do(http.MethodPut, path, strangerTok, []any{}); status != http.StatusNotFound {
		t.Fatalf("PUT as a non-member = %d, want 404", status)
	}
	if status, _ := a.e.Do(http.MethodDelete, path, a.memberTok, nil); status == http.StatusNoContent {
		t.Fatal("DELETE …/afk/away answered 204; there is no client route to clear away (Q3)")
	}
}

// M1 of the AW1 review: a kick that lands between the membership check and the insert answers as a
// non-member (404), never 500.
func TestAFKPutAwayAfterAKickIsNotFound(t *testing.T) {
	a := newAwayEnv(t)
	a.calls.SetAFKHookForTest(func(stage string) {
		if stage == "away-decided" {
			if err := a.e.Repo.DeleteMember(t.Context(), a.cid, a.member); err != nil {
				t.Error(err)
			}
		}
	})
	status, body := a.e.Do(http.MethodPut, "/v1/communities/"+a.cid.String()+"/afk/away", a.memberTok, []any{})
	if status != http.StatusNotFound {
		t.Fatalf("PUT after a racing kick = %d (%x), want 404", status, body)
	}
}

// R-AW5: at boot, rows of users with a gateway connection survive; after the 60 s grace, the rows
// of users with none are cleared (and announced).
func TestAFKRestartKeepsRowsForConnectedUsers(t *testing.T) {
	a := newAwayEnv(t)
	a.markAway(t, a.cid, a.member, 100)
	a.markAway(t, a.cid, a.owner, 100)
	a.setOffline(a.owner, true)
	if _, err := a.calls.EndAFKCalls(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.e.Clk.Advance(59 * time.Second)
	a.calls.AFKMaintenance(t.Context())
	if !a.isAway(t, a.cid, a.owner) || !a.isAway(t, a.cid, a.member) {
		t.Fatalf("rows cleared inside the grace: %v", a.roster(t, a.cid))
	}
	a.e.Clk.Advance(2 * time.Second)
	a.calls.AFKMaintenance(t.Context())
	if a.isAway(t, a.cid, a.owner) {
		t.Fatal("the disconnected owner's row survived the grace")
	}
	if !a.isAway(t, a.cid, a.member) {
		t.Fatal("the connected member's row was cleared")
	}
	toEveryone(t, a.away.sent(a.cid, a.owner), []id.ID{a.owner, a.member}, 0)
	// It runs once: a row written later for an offline user is the offline hook's, not the boot's.
	a.markAway(t, a.cid, a.owner, 200)
	a.e.Clk.Advance(time.Hour)
	a.calls.AFKMaintenance(t.Context())
	if !a.isAway(t, a.cid, a.owner) {
		t.Fatal("the boot clear ran a second time")
	}
}

// Release note: a live AFK call at upgrade ends and the users with a device in it become away,
// since the boot.
func TestAFKLiveAFKCallAtUpgradeEndsAndItsUsersBecomeAway(t *testing.T) {
	a := newAwayEnv(t)
	group := seedCallGroup(t, a.e, a.afk, a.cid, callGroupEpoch)
	seedLeaf(t, a.e, group, a.memberDev, 3, nil)
	room := legacyAFKCall(t, a, group)
	call, _, _ := cutRoom(room)
	// At boot nobody holds a gateway connection yet: the boot grace, not the connection, decides.
	a.setOffline(a.member, true)
	n, err := a.calls.EndAFKCalls(t.Context())
	if err != nil || n != 1 {
		t.Fatalf("EndAFKCalls = %d, %v; want 1 call ended", n, err)
	}
	row, err := a.e.Repo.GetVoiceSession(t.Context(), call)
	if err != nil || row.Ended == nil {
		t.Fatalf("the AFK call is still live: %+v, %v", row, err)
	}
	got := a.roster(t, a.cid)
	if len(got) != 1 || got[0].UserID != a.member || got[0].Since != a.e.Clk.Now().Unix() {
		t.Fatalf("roster after the upgrade = %v, want the member since now", got)
	}
	// The ordinary call in "voice" is left alone.
	if live, err := a.e.Repo.ListLiveVoiceSessions(t.Context(), a.ch); err != nil || len(live) != 1 {
		t.Fatalf("the ordinary call = %v, %v; want it live", live, err)
	}
}

// cutRoom splits a room name "<call hex>-<unix>-<8 hex>" into its call id.
func cutRoom(room string) (id.ID, string, bool) {
	callHex := room[:32]
	call, err := id.Parse(callHex)
	return call, room[33:], err == nil
}

// The snapshot's away list is capped at 1024 entries.
func TestAFKSnapshotAwayIsCapped(t *testing.T) {
	a := newAwayEnv(t)
	for i := 0; i < 1030; i++ {
		u := id.New()
		if err := a.e.Repo.CreateUser(t.Context(), newAPITestUser(u, "away-"+u.String()[:12], a.e.Clk.Now().Unix())); err != nil {
			t.Fatal(err)
		}
		if err := a.e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: a.cid, UserID: u, Joined: 1}); err != nil {
			t.Fatal(err)
		}
		a.markAway(t, a.cid, u, 5)
	}
	if got := afkAwayOf(t, a.e, a.cid, a.memberTok); len(got) != 1024 {
		t.Fatalf("snapshot away has %d entries, want 1024", len(got))
	}
}

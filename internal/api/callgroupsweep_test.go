package api_test

// web-3 task 3b, L-HTTP-105 (RV-2): the instance closes an open call group that no live call names
// 600 s after it was created, at most 1,024 per pass, oldest first, and never a live call's group.

import (
	"net/http"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func groupClosed(t *testing.T, e *env, g id.ID) bool {
	t.Helper()
	row, err := e.Repo.GetGroup(t.Context(), g)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	return row.ClosedAt != nil
}

func TestAnIdleCallGroupClosesAfterTenMinutes(t *testing.T) {
	e, ch, tok, group, _, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	e.Clk.Advance(599 * time.Second)
	calls.CloseIdleCallGroups(t.Context())
	if groupClosed(t, e, group) {
		t.Fatal("a call group 599 s old was closed")
	}
	e.Clk.Advance(time.Second)
	if n := calls.CloseIdleCallGroups(t.Context()); n != 1 || !groupClosed(t, e, group) {
		t.Fatalf("a pass at 600 s closed %d groups (this one closed: %v), want it closed", n, groupClosed(t, e, group))
	}
	if _, _, snap := getChannelCall(t, e, ch, tok); snap.Group != nil || snap.Call != nil {
		t.Fatalf("the snapshot after the idle close names group %v call %v, want null", snap.Group, snap.Call)
	}
	// The next joiner registers a fresh call group, and the next read names it.
	fresh := seedCallGroup(t, e, ch, ownerCommunityOf(t, e, ch), callGroupEpoch)
	if _, _, snap := getChannelCall(t, e, ch, tok); snap.Group == nil || *snap.Group != fresh {
		t.Fatalf("the snapshot after a fresh registration names %v, want %s", snap.Group, fresh)
	}
}

// Attacker: a member who wants to end a call they are not allowed to end by waiting for the idle
// close. Can a member force-close a live call? It must not be possible.
func TestTheIdleCloseNeverTouchesALiveCallsGroup(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
	for range 60 {
		f.e.Clk.Advance(60 * time.Second)
		f.calls.CloseIdleCallGroups(t.Context())
	}
	f.events.FlushAnnouncements()
	if groupClosed(t, f.e, f.group) {
		t.Fatal("the idle close closed a live call's group")
	}
	if callEnded(t, f.e, f.callID) {
		t.Fatal("the idle close ended a live call")
	}
	for _, fr := range f.gw.all() {
		if fr.Op == gateway.OpVoiceState && fr.Flags == 0 {
			t.Fatalf("the idle close announced voice_state 0: %+v", fr)
		}
	}
}

// Attacker: a member keeping a poisoned group open by starting a call nobody joins. The SFU reaps
// the empty room (room_finished after its empty timeout), which ends the call and closes its group.
func TestAnUnjoinedCallStillEndsAndClosesItsGroup(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.e.Clk.Advance(300 * time.Second)
	f.events.Handle(t.Context(), &livekit.WebhookEvent{Event: webhook.EventRoomFinished, Id: "EV_" + id.New().String(),
		Room: &livekit.Room{Name: f.room}})
	if !callEnded(t, f.e, f.callID) || !groupClosed(t, f.e, f.group) {
		t.Fatalf("an unjoined call's room_finished: call ended %v, group closed %v; want both",
			callEnded(t, f.e, f.callID), groupClosed(t, f.e, f.group))
	}
	if _, _, snap := getChannelCall(t, f.e, f.ch, f.tok); snap.Group != nil || snap.Call != nil {
		t.Fatalf("the snapshot names group %v call %v, want null", snap.Group, snap.Call)
	}
}

// A start that read the call group open and writes its voice session after the idle close is ended
// at once by its re-read (CALLS-2) and answers 503 with retry_after_ms; the client retries with a
// fresh read.
func TestAStartRacingTheIdleCloseRetries(t *testing.T) {
	e, ch, tok, group, _, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	e.Clk.Advance(600 * time.Second)
	calls.SetStartHookForTest(func(stage string) {
		if stage == "put" {
			calls.CloseIdleCallGroups(t.Context())
		}
	})
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	calls.SetStartHookForTest(nil)
	if status != http.StatusServiceUnavailable || e.ErrCode(body) != "E_UNAVAILABLE" || retryAfterOf(t, body) == 0 {
		t.Fatalf("a start racing the idle close = %d %x, want 503 E_UNAVAILABLE with retry_after_ms", status, body)
	}
	if !groupClosed(t, e, group) || !callEnded(t, e, ch) {
		t.Fatalf("group closed %v, call ended %v; want both", groupClosed(t, e, group), callEnded(t, e, ch))
	}
}

func TestTheIdleClosePassIsBounded(t *testing.T) {
	e, ch, _, first, _, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	cid := ownerCommunityOf(t, e, ch)
	base := e.Clk.Now().Unix()
	groups := []id.ID{first} // created at base: the oldest
	for i := range 1499 {
		c, call := cid, ch
		g := store.GroupRow{
			GroupID: id.New(), Binding: []byte{0x80}, Kind: api.GroupCall, CommunityID: &c, TargetID: ch,
			CallID: &call, Ciphersuite: 1, Epoch: callGroupEpoch, ExternalSenderKeyID: id.New(),
			E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: base + 1 + int64(i),
		}
		if err := e.Repo.CreateGroup(t.Context(), g); err != nil {
			t.Fatalf("CreateGroup: %v", err)
		}
		groups = append(groups, g.GroupID)
	}
	e.Clk.Advance(3600 * time.Second)
	if n := calls.CloseIdleCallGroups(t.Context()); n != 1024 {
		t.Fatalf("the first pass closed %d groups, want 1,024", n)
	}
	for i, g := range groups {
		if closed := groupClosed(t, e, g); closed != (i < 1024) {
			t.Fatalf("group %d (created %d s after the first) closed = %v after one pass, want the 1,024 oldest", i, i, closed)
		}
	}
	if n := calls.CloseIdleCallGroups(t.Context()); n != 476 {
		t.Fatalf("the second pass closed %d groups, want the remaining 476", n)
	}
	for i, g := range groups {
		if !groupClosed(t, e, g) {
			t.Fatalf("group %d is still open after two passes", i)
		}
	}
}

package api_test

import (
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
)

// shareEnv is an open call with the owner's device a leaf, and n more devices of the owner, each a
// leaf with a session named "dev<i>".
func shareEnv(t *testing.T, maxPublishers, n int) (*env, *stubSFU, id.ID, string, []id.ID) {
	t.Helper()
	e, ch, tok, group, stub := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: maxPublishers})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	callID := decodeCall(t, body).CallID
	user := userOf(t, e, tok)
	devs := seedDevices(t, e, user, n)
	for i, d := range devs {
		e.sess[fmt.Sprintf("dev%d", i)] = auth.Session{UserID: user, DeviceID: d, Scope: auth.ScopeEnrolled}
		seedLeaf(t, e, group, d, 3, nil)
	}
	return e, stub, callID, tok, devs
}

func hasCamera(p *livekit.ParticipantPermission) bool {
	return slices.Contains(p.GetCanPublishSources(), livekit.TrackSource_CAMERA)
}

// SP-18 (lease race): eleven devices asking at once for ten slots get exactly ten 204s and one
// 409 E_CALL_SHARERS_FULL, and each 204 pushed a complete permission with the video sources.
func TestTheEleventhConcurrentShareIsRefused(t *testing.T) {
	e, stub, callID, _, _ := shareEnv(t, 10, 11)
	path := "/v1/calls/" + callID.String() + "/share"
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	for i := range 11 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body := e.Do(http.MethodPost, path, fmt.Sprintf("dev%d", i), []any{})
			mu.Lock()
			defer mu.Unlock()
			codes[status]++
			if status == http.StatusConflict && e.ErrCode(body) != "E_CALL_SHARERS_FULL" {
				t.Errorf("the refusal carried %s", e.ErrCode(body))
			}
		}()
	}
	wg.Wait()
	if codes[http.StatusNoContent] != 10 || codes[http.StatusConflict] != 1 {
		t.Fatalf("answers = %v, want ten 204 and one 409", codes)
	}
	promoted := 0
	for _, u := range stub.permUpdates() {
		if hasCamera(u.Perm) {
			promoted++
			if !u.Perm.GetCanSubscribe() || !u.Perm.GetCanPublish() || u.Perm.GetCanPublishData() {
				t.Errorf("a promotion was not a complete permission: %+v", u.Perm)
			}
		}
	}
	if promoted != 10 {
		t.Fatalf("%d promotions pushed, want 10", promoted)
	}
}

// Release demotes first and frees the slot after: when the SFU applies the demotion the slot is
// still held, and another device's share — serialised behind the release by the call's lock —
// succeeds once it is freed. A DELETE with no slot is 204 and pushes nothing.
func TestReleasingAShareDemotesBeforeFreeingTheSlot(t *testing.T) {
	l := newLeaseEnv(t, 1, 2)
	e, stub, devs, path := l.e, l.stub, l.devs, l.sharePath()
	if status, _ := e.Do(http.MethodPost, path, "dev0", []any{}); status != http.StatusNoContent {
		t.Fatalf("dev0 share = %d", status)
	}
	var heldDuringDemotion atomic.Bool
	stub.mu.Lock()
	stub.onUpdate = func(u permUpdate) {
		if u.Identity == devs[0].String() && !hasCamera(u.Perm) {
			heldDuringDemotion.Store(slices.Contains(l.calls.SharersOf(l.callID), devs[0]))
		}
	}
	stub.mu.Unlock()
	if status, _ := e.Do(http.MethodDelete, path, "dev0", nil); status != http.StatusNoContent {
		t.Fatalf("dev0 unshare = %d", status)
	}
	if !heldDuringDemotion.Load() {
		t.Fatal("the slot was freed before the SFU applied the demotion")
	}
	stub.mu.Lock()
	stub.onUpdate = nil
	stub.mu.Unlock()
	if status, _ := e.Do(http.MethodPost, path, "dev1", []any{}); status != http.StatusNoContent {
		t.Fatalf("dev1 share after the release = %d, want 204", status)
	}
	before := len(stub.permUpdates())
	if status, _ := e.Do(http.MethodDelete, path, "dev0", nil); status != http.StatusNoContent || len(stub.permUpdates()) != before {
		t.Fatalf("a DELETE without a slot = %d with %d pushes, want 204 and none", status, len(stub.permUpdates())-before)
	}
}

// A device the room does not hold gets 404 and its slot is rolled back.
func TestAShareForADeviceNotInTheRoomRollsBack(t *testing.T) {
	e, stub, callID, _, devs := shareEnv(t, 1, 2)
	path := "/v1/calls/" + callID.String() + "/share"
	stub.mu.Lock()
	stub.absent = map[string]bool{devs[0].String(): true}
	stub.mu.Unlock()
	if status, body := e.Do(http.MethodPost, path, "dev0", []any{}); status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
		t.Fatalf("a share for a device not in the room = %d %s, want 404 E_NOT_FOUND", status, e.ErrCode(body))
	}
	if status, _ := e.Do(http.MethodPost, path, "dev1", []any{}); status != http.StatusNoContent {
		t.Fatalf("the slot was not rolled back: dev1 share = %d", status)
	}
}

// The share route needs video or screen_share, a current leaf, and a live call.
func TestShareNeedsVideoOrScreenShareAndALeaf(t *testing.T) {
	e, ch, tok, group, _ := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: 10})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	path := "/v1/calls/" + decodeCall(t, body).CallID.String() + "/share"
	member, _, memberTok := joinedMember(t, e, ch, group, "member")
	denyInChannel(t, e, ch, member, api.PermVideo|api.PermScreenShare)
	if status, resp := e.Do(http.MethodPost, path, memberTok, []any{}); status != http.StatusForbidden || e.ErrCode(resp) != "E_FORBIDDEN" {
		t.Fatalf("a share without video or screen_share = %d %s, want 403 E_FORBIDDEN", status, e.ErrCode(resp))
	}
	denyInChannel(t, e, ch, member, api.PermConnect)
	if status, resp := e.Do(http.MethodPost, path, memberTok, []any{}); status != http.StatusForbidden || e.ErrCode(resp) != "E_FORBIDDEN" {
		t.Fatalf("a share without connect = %d %s, want 403 E_FORBIDDEN", status, e.ErrCode(resp))
	}
	user := userOf(t, e, tok)
	stranger := seedDevices(t, e, user, 1)[0]
	e.sess["noleaf"] = auth.Session{UserID: user, DeviceID: stranger, Scope: auth.ScopeEnrolled}
	if status, resp := e.Do(http.MethodPost, path, "noleaf", []any{}); status != http.StatusForbidden || e.ErrCode(resp) != "E_LEAF_NOT_CURRENT" {
		t.Fatalf("a share from a device that is no leaf = %d %s, want 403 E_LEAF_NOT_CURRENT", status, e.ErrCode(resp))
	}
	if status, _ := e.Do(http.MethodPost, "/v1/calls/"+id.New().String()+"/share", tok, []any{}); status != http.StatusNotFound {
		t.Fatalf("a share in an unknown call = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodPost, path, tok, []any{"x"}); status != http.StatusBadRequest {
		t.Fatalf("a share with a non-empty body = %d, want 400", status)
	}
}

// A token is minted with the base grant only — the microphone for speak, never a camera or screen
// source — even for a device that holds a sharing slot: promotion exists only as the push a share
// makes, so a token can never be replayed for video after an unshare. A reconnecting sharer keeps
// its slot and re-POSTs share, which is idempotent.
func TestATokenCarriesTheBaseGrantOnlyEvenForASharer(t *testing.T) {
	e, ch, tok, group, stub := callEnvWith(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: 10})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	path := "/v1/channels/" + ch.String() + "/calls"
	_, body := e.Do(http.MethodPost, path, tok, []any{})
	share := "/v1/calls/" + decodeCall(t, body).CallID.String() + "/share"
	if status, _ := e.Do(http.MethodPost, share, tok, []any{}); status != http.StatusNoContent {
		t.Fatalf("share = %d", status)
	}
	e.Do(http.MethodPost, path, tok, []any{})
	want := []livekit.TrackSource{livekit.TrackSource_MICROPHONE}
	if got := stub.lastMint().Perm.GetCanPublishSources(); !slices.Equal(got, want) {
		t.Fatalf("the leased device's new token = %v, want %v", got, want)
	}
	before := len(stub.permUpdates())
	if status, _ := e.Do(http.MethodPost, share, tok, []any{}); status != http.StatusNoContent ||
		len(stub.permUpdates()) != before+1 || !hasCamera(stub.permUpdates()[before].Perm) {
		t.Fatalf("the reconnected sharer's repeated share = %d, want 204 and the promotion pushed again", status)
	}
}

// A call keeps its call id from one call to the next (R9), so ending a call drops its sharing slots:
// the next call of the channel starts with every slot free and no device holding video sources.
func TestEndingACallDropsItsSharingSlots(t *testing.T) {
	e, stub, callID, tok, devs := shareEnv(t, 1, 1)
	share := "/v1/calls/" + callID.String() + "/share"
	if status, _ := e.Do(http.MethodPost, share, tok, []any{}); status != http.StatusNoContent {
		t.Fatalf("owner share = %d", status)
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/calls/"+callID.String(), tok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE call = %d", status)
	}
	e.Clk.Advance(time.Minute)
	row, err := e.Repo.GetVoiceSession(t.Context(), callID)
	if err != nil {
		t.Fatalf("GetVoiceSession: %v", err)
	}
	// Ending a call closes its call group (DEV-46): the next call is on a freshly registered one,
	// which keeps the channel's call id.
	next := seedCallGroup(t, e, row.ChannelID, ownerCommunityOf(t, e, row.ChannelID), callGroupEpoch)
	seedLeaf(t, e, next, deviceOf(t, e, tok), 3, nil)
	seedLeaf(t, e, next, devs[0], 3, nil)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+row.ChannelID.String()+"/calls", tok, []any{})
	if status != http.StatusCreated || decodeCall(t, body).CallID != callID {
		t.Fatalf("the next call = %d, want 201 on the same call id", status)
	}
	if hasCamera(stub.lastMint().Perm) {
		t.Fatalf("the next call's token kept the previous call's slot: %+v", stub.lastMint().Perm)
	}
	if status, _ := e.Do(http.MethodPost, share, "dev0", []any{}); status != http.StatusNoContent {
		t.Fatalf("another device's share in the next call = %d, want 204: the old slot was kept", status)
	}
}

package api_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// G26: a live role change re-pushes the complete permission of each affected participant; a
// userID scope touches only that user's devices, and "#" shadows are never addressed.
func TestSyncCallGrantsPushesTheCurrentPermission(t *testing.T) {
	e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: 10})
	ownerDev := deviceOf(t, e, tok)
	seedLeaf(t, e, group, ownerDev, 3, nil)
	e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	room := stub.minted()[0][0]
	member, memberDev, _ := joinedMember(t, e, ch, group, "member")
	stub.setPresent(room, &livekit.ParticipantInfo{Identity: ownerDev.String()},
		&livekit.ParticipantInfo{Identity: memberDev.String()}, &livekit.ParticipantInfo{Identity: memberDev.String() + "#x"})
	denyInChannel(t, e, ch, member, api.PermSpeak)
	cid := ownerCommunityOf(t, e, ch)

	if err := api.SyncCallGrants(t.Context(), e.Repo, api.NewResolver(e.Repo), stub, calls, cid, &member, nil); err != nil {
		t.Fatalf("SyncCallGrants(user): %v", err)
	}
	updates := stub.permUpdates()
	if len(updates) != 1 || updates[0].Identity != memberDev.String() || updates[0].Perm.GetCanPublish() {
		t.Fatalf("updates = %+v, want one listen-only push for the member's device", updates)
	}
	if err := api.SyncCallGrants(t.Context(), e.Repo, api.NewResolver(e.Repo), stub, calls, cid, nil, &ch); err != nil {
		t.Fatalf("SyncCallGrants(channel): %v", err)
	}
	if got := stub.permUpdates(); len(got) != 3 || got[1].Identity != ownerDev.String() || !got[1].Perm.GetCanPublish() {
		t.Fatalf("updates after the channel-wide sync = %+v", got)
	}
}

// A user who lost both video and screen_share loses the slot too, after the demotion is pushed.
func TestSyncCallGrantsReleasesTheSlotOfAUserWhoLostVideoAndScreen(t *testing.T) {
	e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: 1})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	share := "/v1/calls/" + decodeCall(t, body).CallID.String() + "/share"
	room := stub.minted()[0][0]
	member, memberDev, memberTok := joinedMember(t, e, ch, group, "member")
	if status, _ := e.Do(http.MethodPost, share, memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("member share = %d", status)
	}
	stub.setPresent(room, &livekit.ParticipantInfo{Identity: memberDev.String()})
	denyInChannel(t, e, ch, member, api.PermVideo|api.PermScreenShare)
	if err := api.SyncCallGrants(t.Context(), e.Repo, api.NewResolver(e.Repo), stub, calls,
		ownerCommunityOf(t, e, ch), &member, nil); err != nil {
		t.Fatalf("SyncCallGrants: %v", err)
	}
	if got := stub.permUpdates(); hasCamera(got[len(got)-1].Perm) {
		t.Fatalf("the member kept the camera: %+v", got[len(got)-1].Perm)
	}
	if status, _ := e.Do(http.MethodPost, share, tok, []any{}); status != http.StatusNoContent {
		t.Fatalf("the owner's share after the member lost video = %d, want 204: the slot was not released", status)
	}
}

// countingCounters is the call routes' metric surface, counted.
type countingCounters struct{ full, refused, cut, retries, pending atomic.Int64 }

func (c *countingCounters) CallFull()                { c.full.Add(1) }
func (c *countingCounters) ShareRefused()            { c.refused.Add(1) }
func (c *countingCounters) CallCut()                 { c.cut.Add(1) }
func (c *countingCounters) CallGrantRetry()          { c.retries.Add(1) }
func (c *countingCounters) CallRepairsPending(n int) { c.pending.Store(int64(n)) }

// The spec's error handling: the SFU session is cut on kick at once. A participant whose user lost
// view_channel or connect is removed from the room (its "#" shadows with it) by the sync itself, not
// left with its old permission until some member commits the MLS Remove; its sharing slot is freed
// and the cut is counted.
func TestSyncCallGrantsCutsAParticipantWhoLostViewOrConnect(t *testing.T) {
	for _, lost := range []api.Bits{api.PermConnect, api.PermViewChannel} {
		e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: 1})
		counters := &countingCounters{}
		calls.WithCounters(counters)
		seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
		_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
		share := "/v1/calls/" + decodeCall(t, body).CallID.String() + "/share"
		room := stub.minted()[0][0]
		member, memberDev, memberTok := joinedMember(t, e, ch, group, "member")
		if status, _ := e.Do(http.MethodPost, share, memberTok, []any{}); status != http.StatusNoContent {
			t.Fatalf("member share = %d", status)
		}
		stub.setPresent(room, &livekit.ParticipantInfo{Identity: memberDev.String()})
		denyInChannel(t, e, ch, member, lost)
		before := len(stub.permUpdates())
		if err := api.SyncCallGrants(t.Context(), e.Repo, api.NewResolver(e.Repo), stub, calls,
			ownerCommunityOf(t, e, ch), &member, nil); err != nil {
			t.Fatalf("SyncCallGrants: %v", err)
		}
		devices, _ := stub.removals()
		if len(devices) != 1 || devices[0] != [2]string{room, memberDev.String()} {
			t.Fatalf("lost %#x: removals = %v, want the member's device (and its shadows) cut from %s", lost, devices, room)
		}
		for _, u := range stub.permUpdates()[before:] {
			if u.Perm.GetCanPublish() {
				t.Fatalf("lost %#x: the cut participant was pushed a publishing permission: %+v", lost, u.Perm)
			}
		}
		if counters.cut.Load() != 1 {
			t.Fatalf("lost %#x: %d cuts counted, want 1", lost, counters.cut.Load())
		}
		if status, _ := e.Do(http.MethodPost, share, tok, []any{}); status != http.StatusNoContent {
			t.Fatalf("lost %#x: the owner's share after the cut = %d, want 204: the slot was kept", lost, status)
		}
	}
}

// failingDeviceRepo answers GetDevice for one device with a store failure.
type failingDeviceRepo struct {
	store.Repository
	dev id.ID
}

func (r failingDeviceRepo) GetDevice(ctx context.Context, dev id.ID) (store.DeviceRow, error) {
	if dev == r.dev {
		return store.DeviceRow{}, errors.New("disk on fire")
	}
	return r.Repository.GetDevice(ctx, dev)
}

// A lookup that fails is fail-closed: the participant is pushed the no-publish grant, the error is
// reported, and the repair stays pending — the slot held, the device refused a token and the /rtc
// gate for the call — until a retry resolves it and pushes its real permission.
func TestSyncCallGrantsFailsClosedOnALookupError(t *testing.T) {
	e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: 1})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	path := "/v1/channels/" + ch.String() + "/calls"
	_, body := e.Do(http.MethodPost, path, tok, []any{})
	share := "/v1/calls/" + decodeCall(t, body).CallID.String() + "/share"
	room := stub.minted()[0][0]
	_, memberDev, memberTok := joinedMember(t, e, ch, group, "member")
	if status, _ := e.Do(http.MethodPost, share, memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("member share = %d", status)
	}
	stub.setPresent(room, &livekit.ParticipantInfo{Identity: memberDev.String()})
	repo := failingDeviceRepo{Repository: e.Repo, dev: memberDev}
	if err := api.SyncCallGrants(t.Context(), repo, api.NewResolver(repo), stub, calls,
		ownerCommunityOf(t, e, ch), nil, &ch); err == nil {
		t.Fatal("SyncCallGrants hid the lookup failure")
	}
	got := stub.lastPerms()[memberDev.String()]
	if got.GetCanPublish() || len(got.GetCanPublishSources()) != 0 {
		t.Fatalf("after a failed lookup the member holds %+v, want the no-publish grant", got)
	}
	if perm, err := calls.AdmitRoom(t.Context(), room, memberDev); perm == nil {
		t.Logf("AdmitRoom: %v", err)
		// The retry on the gate's own read (e.Repo answers) lands the repair at once.
		t.Fatal("the gate's retry did not land the repair once the lookup answered")
	}
	if got := stub.lastPerms()[memberDev.String()]; !hasCamera(got) {
		t.Fatalf("after the repair the member holds %+v, want its permission with the slot it holds", got)
	}
	if status, _ := e.Do(http.MethodPost, share, tok, []any{}); status != http.StatusConflict {
		t.Fatalf("the owner's share while the member holds the slot = %d, want 409", status)
	}
}

// A cut the SFU does not take stays pending and converges: the device keeps its slot (so the cap
// holds), is refused a token and the /rtc gate, and the maintenance retry drives the cut until it
// lands, counting and logging each retry.
func TestACutTheSFURefusesIsRetriedUntilItLands(t *testing.T) {
	e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: 1})
	counters := &countingCounters{}
	calls.WithCounters(counters)
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	path := "/v1/channels/" + ch.String() + "/calls"
	_, body := e.Do(http.MethodPost, path, tok, []any{})
	share := "/v1/calls/" + decodeCall(t, body).CallID.String() + "/share"
	room := stub.minted()[0][0]
	member, memberDev, memberTok := joinedMember(t, e, ch, group, "member")
	if status, _ := e.Do(http.MethodPost, share, memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("member share = %d", status)
	}
	stub.setPresent(room, &livekit.ParticipantInfo{Identity: memberDev.String()})
	denyInChannel(t, e, ch, member, api.PermSpeak)
	stub.mu.Lock()
	stub.failRemovals, stub.failUpdates = 2, 2 // the cut and its no-publish fallback fail twice
	stub.mu.Unlock()
	denyInChannel(t, e, ch, member, api.PermConnect)
	if err := api.SyncCallGrants(t.Context(), e.Repo, api.NewResolver(e.Repo), stub, calls,
		ownerCommunityOf(t, e, ch), &member, nil); err == nil {
		t.Fatal("SyncCallGrants hid the failed cut")
	}
	if status, _ := e.Do(http.MethodPost, share, tok, []any{}); status != http.StatusConflict {
		t.Fatalf("the owner's share while the member's cut is pending = %d, want 409: the slot must stay held", status)
	}
	// The owner's share drives only its own repair, so it leaves the member's pending; the gate's own
	// retry of the member's repair is failure two, so it refuses; the mint refuses too, and the retry loop lands the cut.
	if perm, _ := calls.AdmitRoom(t.Context(), room, memberDev); perm != nil {
		t.Fatal("the /rtc gate admitted a device whose cut is pending")
	}
	if status, resp := e.Do(http.MethodPost, path, memberTok, []any{}); status == http.StatusOK || status == http.StatusCreated {
		t.Fatalf("the token mint answered %d for a device whose cut is pending (%s)", status, e.ErrCode(resp))
	}
	calls.RetryPending(t.Context())
	devices, _ := stub.removals()
	if last := devices[len(devices)-1]; last != [2]string{room, memberDev.String()} {
		t.Fatalf("the last removal = %v", last)
	}
	if _, inRoom := stub.lastPerms()[memberDev.String()]; inRoom {
		t.Fatal("the member is still in the room after the retry")
	}
	if counters.retries.Load() < 2 || counters.cut.Load() != 1 {
		t.Fatalf("retries %d, cuts %d; want at least 2 retries and the one cut", counters.retries.Load(), counters.cut.Load())
	}
	if status, _ := e.Do(http.MethodPost, share, tok, []any{}); status != http.StatusNoContent {
		t.Fatalf("the owner's share once the cut landed = %d, want 204", status)
	}
}

// A demotion the SFU fails (the user lost video) leaves the slot held and the device pending; the
// retry pushes it again until it lands, and only then frees the slot.
func TestADemotionTheSFUFailsIsRetriedAndHoldsTheSlot(t *testing.T) {
	e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: 1})
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	_, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	share := "/v1/calls/" + decodeCall(t, body).CallID.String() + "/share"
	room := stub.minted()[0][0]
	member, memberDev, memberTok := joinedMember(t, e, ch, group, "member")
	if status, _ := e.Do(http.MethodPost, share, memberTok, []any{}); status != http.StatusNoContent {
		t.Fatalf("member share = %d", status)
	}
	stub.setPresent(room, &livekit.ParticipantInfo{Identity: memberDev.String()})
	denyInChannel(t, e, ch, member, api.PermVideo|api.PermScreenShare)
	stub.mu.Lock()
	stub.failUpdates = 1
	stub.mu.Unlock()
	if err := api.SyncCallGrants(t.Context(), e.Repo, api.NewResolver(e.Repo), stub, calls,
		ownerCommunityOf(t, e, ch), &member, nil); err == nil {
		t.Fatal("SyncCallGrants hid the failed demotion")
	}
	if !hasCamera(stub.lastPerms()[memberDev.String()]) || len(calls.SharersOf(decodeCall(t, body).CallID)) != 1 {
		t.Fatal("the failed demotion should have left the camera and the held slot as they were")
	}
	calls.RetryPending(t.Context())
	if hasCamera(stub.lastPerms()[memberDev.String()]) {
		t.Fatal("the retry did not demote the member")
	}
	if status, _ := e.Do(http.MethodPost, share, tok, []any{}); status != http.StatusNoContent {
		t.Fatalf("the owner's share once the demotion landed = %d, want 204", status)
	}
}

// A participant in a call room whose identity is no device of this instance — a "#" shadow, a
// foreign identity, an id that names no device — is removed by its exact identity.
func TestSyncCallGrantsRemovesANonDeviceIdentity(t *testing.T) {
	e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	ownerDev := deviceOf(t, e, tok)
	seedLeaf(t, e, group, ownerDev, 3, nil)
	e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	room := stub.minted()[0][0]
	ghost := id.New().String()
	stub.setPresent(room, &livekit.ParticipantInfo{Identity: ownerDev.String()},
		&livekit.ParticipantInfo{Identity: ownerDev.String() + "#x"},
		&livekit.ParticipantInfo{Identity: "intruder"}, &livekit.ParticipantInfo{Identity: ghost})
	if err := api.SyncCallGrants(t.Context(), e.Repo, api.NewResolver(e.Repo), stub, calls,
		ownerCommunityOf(t, e, ch), nil, &ch); err != nil {
		t.Fatalf("SyncCallGrants: %v", err)
	}
	devices, identities := stub.removals()
	want := [][2]string{{room, ownerDev.String() + "#x"}, {room, "intruder"}, {room, ghost}}
	if len(devices) != 0 || !slices.Equal(identities, want) {
		t.Fatalf("removals = devices %v, identities %v; want identities %v and the owner's device kept", devices, identities, want)
	}
}

// The role routes run the sync after their commit: a PUT overwrite reaches the SFU.
func TestAnOverwriteChangeReachesTheLiveCall(t *testing.T) {
	e, ch, tok, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).WithDS(e.DS).WithCalls(stub, calls).Register(e.Mux)
	seedLeaf(t, e, group, deviceOf(t, e, tok), 3, nil)
	e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", tok, []any{})
	member, memberDev, _ := joinedMember(t, e, ch, group, "member")
	stub.setPresent(stub.minted()[0][0], &livekit.ParticipantInfo{Identity: memberDev.String()})
	if status, _ := e.Do(http.MethodPut, "/v1/channels/"+ch.String()+"/overwrites/1/"+member.String(), tok,
		[]any{uint64(0), uint64(api.PermSpeak)}); status != http.StatusNoContent {
		t.Fatal("PUT overwrite failed")
	}
	calls.ProcessQueueForTest(t.Context()) // the route queues its sync to the retry loop (parked item)
	if got := stub.permUpdates(); len(got) != 1 || got[0].Identity != memberDev.String() || got[0].Perm.GetCanPublish() {
		t.Fatalf("updates after the overwrite = %+v, want the member demoted to listen-only", got)
	}
}

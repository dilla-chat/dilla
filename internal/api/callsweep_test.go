package api_test

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// removedDevice reports whether RemoveParticipants was asked to cut dev from room.
func removedDevice(stub *stubSFU, room string, dev id.ID) bool {
	devices, _ := stub.removals()
	return slices.Contains(devices, [2]string{room, dev.String()})
}

// The room sweep cuts a connected barred device — revoked, quarantined, disabled or deleted, written
// by a process that told this one nothing (`dillad admin user disable`, an admin device revoke) —
// from the call's room with its "#" shadows, frees its sharing slot, and leaves everyone else as
// they are. A cut the SFU refuses stays pending and the gate keeps refusing the device.
func TestTheRoomSweepCutsAConnectedBarredDevice(t *testing.T) {
	for _, st := range barredStates() {
		t.Run(st.name, func(t *testing.T) {
			b := newBarredEnv(t)
			if status, _ := b.e.Do(http.MethodPost, b.sharePath(), b.memberTok, []any{}); status != http.StatusNoContent {
				t.Fatalf("member share = %d", status)
			}
			b.stub.setPresent(b.room, &livekit.ParticipantInfo{Identity: b.ownerDev.String()},
				&livekit.ParticipantInfo{Identity: b.memberDev.String()},
				&livekit.ParticipantInfo{Identity: b.memberDev.String() + "#x"})
			st.bar(t, b.e, b.member, b.memberDev)
			b.calls.SweepRooms(t.Context())
			if !removedDevice(b.stub, b.room, b.memberDev) {
				t.Fatalf("the sweep did not cut the %s device", st.name)
			}
			if _, ok := b.stub.lastPerms()[b.memberDev.String()]; ok {
				t.Fatalf("the %s device is still in the room", st.name)
			}
			if len(b.calls.SharersOf(b.callID)) != 0 {
				t.Fatalf("the %s device kept its sharing slot", st.name)
			}
			if removedDevice(b.stub, b.room, b.ownerDev) {
				t.Fatal("the sweep cut the owner, who is not barred")
			}
			if slices.Contains(b.stub.deletedRooms(), b.room) {
				t.Fatal("the sweep deleted a live call's room")
			}
		})
	}
	t.Run("a cut the SFU refuses stays pending", func(t *testing.T) {
		b := newBarredEnv(t)
		barredStates()[0].bar(t, b.e, b.member, b.memberDev)
		b.stub.mu.Lock()
		b.stub.failRemovals = 1
		b.stub.mu.Unlock()
		b.calls.SweepRooms(t.Context())
		if b.calls.PendingRepairs() != 1 {
			t.Fatalf("pending repairs = %d, want the refused cut", b.calls.PendingRepairs())
		}
		b.calls.RetryPending(t.Context())
		if _, in := b.stub.lastPerms()[b.memberDev.String()]; b.calls.PendingRepairs() != 0 || in {
			t.Fatalf("the retry did not land the cut: %d pending, still in the room %v", b.calls.PendingRepairs(), in)
		}
	})
}

// waitRemoved waits up to two seconds for RemoveParticipants of dev in room.
func waitRemoved(stub *stubSFU, room string, dev id.ID) bool {
	deadline := time.Now().Add(2 * time.Second)
	for !removedDevice(stub, room, dev) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// The in-process revocation and quarantine paths cut without waiting for the sweep: CutDevice
// queues the device and the retry loop — woken at once, not on its tick (an hour here) — cuts a
// barred device from every live call room it is in, and leaves a device that is not barred exactly
// as it is. CutUser does the same for every device of a disabled user.
func TestCutDeviceAndCutUserCutABarredDevicePromptly(t *testing.T) {
	b := newBarredEnv(t)
	stop := b.calls.StartRetries(time.Hour)
	defer stop()
	b.calls.CutDevice(t.Context(), b.ownerDev)
	if waitRemoved(b.stub, b.room, b.ownerDev) {
		t.Fatal("CutDevice cut a device that is not barred")
	}
	if err := b.e.Repo.QuarantineDevice(t.Context(), b.memberDev, b.e.Clk.Now().Unix(), "fork quorum"); err != nil {
		t.Fatalf("QuarantineDevice: %v", err)
	}
	b.calls.CutDevice(t.Context(), b.memberDev)
	if !waitRemoved(b.stub, b.room, b.memberDev) {
		t.Fatal("CutDevice did not cut the quarantined device")
	}

	u := newBarredEnv(t)
	ustop := u.calls.StartRetries(time.Hour)
	defer ustop()
	if err := u.e.Repo.SetUserDisabled(t.Context(), u.member, ptr(u.e.Clk.Now().Unix())); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	u.calls.CutUser(t.Context(), u.member)
	if !waitRemoved(u.stub, u.room, u.memberDev) {
		t.Fatal("CutUser did not cut the disabled user's device")
	}
}

// A cut request never makes an SFU call on its caller's goroutine: with an SFU that hangs on every
// call, a burst of CutDevice and CutUser calls (what the revocation, quarantine and admin-disable
// paths make) returns well under a second, while the loop's own pass waits out its budget.
func TestACutRequestNeverWaitsOnTheSFU(t *testing.T) {
	b := newBarredEnv(t)
	b.stub.mu.Lock()
	b.stub.hang = true
	b.stub.mu.Unlock()
	stop := b.calls.StartRetries(time.Hour)
	defer stop()
	start := time.Now()
	for range 200 {
		b.calls.CutDevice(t.Context(), id.New())
		b.calls.CutUser(t.Context(), id.New())
	}
	b.calls.CutDevice(t.Context(), b.memberDev)
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("401 cut requests against a hung SFU took %s", took)
	}
}

// A burst of cut requests for one device coalesces: queued before the loop runs, they are cut in a
// single pass, with one removal.
func TestABurstOfCutRequestsCoalescesIntoOnePass(t *testing.T) {
	b := newBarredEnv(t)
	if err := b.e.Repo.RevokeDevice(t.Context(), b.memberDev, b.e.Clk.Now().Unix()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	for range 50 {
		b.calls.CutDevice(t.Context(), b.memberDev)
	}
	stop := b.calls.StartRetries(time.Hour)
	defer stop()
	if !waitRemoved(b.stub, b.room, b.memberDev) {
		t.Fatal("the queued cut never landed")
	}
	time.Sleep(50 * time.Millisecond)
	if n := b.calls.CutPasses(); n != 1 {
		t.Fatalf("cut passes = %d, want the burst in one", n)
	}
	devices, _ := b.stub.removals()
	if n := len(devices); n != 1 {
		t.Fatalf("removals = %v, want one", devices)
	}
}

// The sweep deletes every room that belongs to no live call — a name that is no call's, a call id
// with no call, an older room of a live call, the room of an ended call — and keeps the live call's
// room. A DeleteRoom that fails is retried by the next sweep (minor m4: an ended call's room whose
// close failed is no longer left open with its participants in it).
func TestTheRoomSweepDeletesRoomsThatBelongToNoLiveCall(t *testing.T) {
	b := newBarredEnv(t)
	orphans := []string{"not-a-call", id.New().String() + "-1790000000", b.callID.String() + "-1"}
	for _, r := range orphans {
		b.stub.addRoom(r)
	}
	b.calls.SweepRooms(t.Context())
	if got := b.stub.heldRooms(); !slices.Equal(got, []string{b.room}) {
		t.Fatalf("rooms after the sweep = %v, want only the live call's %q", got, b.room)
	}
	if err := b.e.Repo.EndVoiceSession(t.Context(), b.callID, b.e.Clk.Now().Unix()); err != nil {
		t.Fatalf("EndVoiceSession: %v", err)
	}
	b.stub.mu.Lock()
	b.stub.deleteFail = errors.New("sfu down")
	b.stub.mu.Unlock()
	b.calls.SweepRooms(t.Context())
	if got := b.stub.heldRooms(); !slices.Equal(got, []string{b.room}) {
		t.Fatalf("rooms after a failed close = %v", got)
	}
	b.stub.mu.Lock()
	b.stub.deleteFail = nil
	b.stub.mu.Unlock()
	b.calls.SweepRooms(t.Context())
	if got := b.stub.heldRooms(); len(got) != 0 {
		t.Fatalf("rooms after the next sweep = %v, want the ended call's room closed", got)
	}
}

// The retry loop runs the sweep on its own: a device barred by another process is cut without any
// event in the call.
func TestTheRetryLoopSweepsTheRooms(t *testing.T) {
	b := newBarredEnv(t)
	b.calls.SetSweepEvery(10 * time.Millisecond)
	if err := b.e.Repo.RevokeDevice(t.Context(), b.memberDev, b.e.Clk.Now().Unix()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	stop := b.calls.StartRetries(10 * time.Millisecond)
	defer stop()
	deadline := time.Now().Add(3 * time.Second)
	for !removedDevice(b.stub, b.room, b.memberDev) {
		if time.Now().After(deadline) {
			t.Fatal("the retry loop never swept the revoked device out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// membershipCallEnv mounts the call, community, channel and ban routes over one stub SFU with the
// membership routes wired to the call routes, as the composition root wires them; the owner has
// opened a call in a voice channel and a member's device is a leaf in its room.
type membershipCallEnv struct {
	e                 *env
	stub              *stubSFU
	calls             *api.Calls
	group             id.ID
	cid, ch           id.ID
	ownerTok          string
	member, memberDev id.ID
	memberTok         string
	room              string
}

func newMembershipCallEnv(t *testing.T) *membershipCallEnv {
	t.Helper()
	return newMembershipCallEnvLoop(t, true)
}

// newMembershipCallEnvWithoutLoop is newMembershipCallEnv without the retry loop: only what the test
// itself drives (a retry pass, a sweep) serves the work queue.
func newMembershipCallEnvWithoutLoop(t *testing.T) *membershipCallEnv {
	t.Helper()
	return newMembershipCallEnvLoop(t, false)
}

func newMembershipCallEnvLoop(t *testing.T, loop bool) *membershipCallEnv {
	t.Helper()
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	stub := &stubSFU{}
	calls := api.NewCalls(e.Repo, api.NewResolver(e.Repo), stub, api.CallsConfig{LiveKitURL: testLiveKitURL}, e.Clk, log)
	calls.Register(e.Mux)
	// The membership routes queue their call cuts to the retry loop (parked item): run it, on an
	// hour tick so only the wake drives it.
	if loop {
		t.Cleanup(calls.StartRetries(time.Hour))
	}
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).WithCalls(calls).Register(e.Mux)
	channels := api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).WithCalls(calls)
	channels.Register(e.Mux)
	channels.RegisterMembers(e.Mux)
	api.NewBans(e.Repo, e.DS, e.Clk, log).WithCalls(calls).Register(e.Mux)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	ch, _, status := newChannel(t, e, cid, ownerTok, 1 /* voice */, 1, 2, "voice")
	if status != http.StatusCreated {
		t.Fatalf("voice channel = %d", status)
	}
	group := seedCallGroup(t, e, ch, cid, callGroupEpoch)
	seedLeaf(t, e, group, deviceOf(t, e, ownerTok), 3, nil)
	if status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", ownerTok, []any{}); status != http.StatusCreated {
		t.Fatalf("owner start = %d %x", status, body)
	}
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	memberDev := deviceOf(t, e, memberTok)
	seedLeaf(t, e, group, memberDev, 3, nil)
	room := stub.minted()[0][0]
	stub.setPresent(room, &livekit.ParticipantInfo{Identity: deviceOf(t, e, ownerTok).String()},
		&livekit.ParticipantInfo{Identity: memberDev.String()})
	return &membershipCallEnv{e: e, stub: stub, calls: calls, group: group, cid: cid, ch: ch, ownerTok: ownerTok, member: member,
		memberDev: memberDev, memberTok: memberTok, room: room}
}

// Important finding I1 of the task 10 review: a kick, a ban and a leave cut the user's connected
// call session at once, after their commit (the MLS Remove still follows through the delivery
// service), and touch nobody else.
func TestAKickABanAndALeaveCutTheUsersCallSessionAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(t *testing.T, m *membershipCallEnv) (int, []byte)
	}{
		{"kick", func(t *testing.T, m *membershipCallEnv) (int, []byte) {
			return m.e.Do(http.MethodDelete, "/v1/communities/"+m.cid.String()+"/members/"+m.member.String(), m.ownerTok, nil)
		}},
		{"ban", func(t *testing.T, m *membershipCallEnv) (int, []byte) {
			return m.e.Do(http.MethodPut, "/v1/communities/"+m.cid.String()+"/bans/"+m.member.String(), m.ownerTok, []any{"spam", nil})
		}},
		{"leave", func(t *testing.T, m *membershipCallEnv) (int, []byte) {
			return m.e.Do(http.MethodPost, "/v1/communities/"+m.cid.String()+"/leave", m.memberTok, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMembershipCallEnv(t)
			if status, body := tc.act(t, m); status != http.StatusNoContent {
				t.Fatalf("%s = %d (%x)", tc.name, status, body)
			}
			if !waitRemoved(m.stub, m.room, m.memberDev) {
				t.Fatalf("the %s left the user's device in the call's room", tc.name)
			}
			if removedDevice(m.stub, m.room, deviceOf(t, m.e, m.ownerTok)) {
				t.Fatalf("the %s cut the owner too", tc.name)
			}
		})
	}
}

// A group-DM participant removed by another participant is cut from the DM's call at once.
func TestAGroupDMRemovalCutsTheParticipantsCallSessionAtOnce(t *testing.T) {
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	if err := e.Repo.CreateInstance(t.Context(), store.InstanceRow{
		InstanceID: id.New(), ExternalSenderKeyID: id.New(), KeyHistory: []byte{1},
		FrankingKeyID: id.New(), Generation: 1, PolicyVersion: 1, Created: e.Clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst, err := e.Repo.GetInstance(t.Context())
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	stub := &stubSFU{}
	calls := api.NewCalls(e.Repo, api.NewResolver(e.Repo), stub, api.CallsConfig{LiveKitURL: testLiveKitURL}, e.Clk, log)
	calls.Register(e.Mux)
	t.Cleanup(calls.StartRetries(time.Hour)) // the removal queues its cut to the retry loop
	api.NewDMs(e.Repo, e.DS, e.Clk, inst.InstanceID, 10, log).Register(e.Mux)
	api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).WithCalls(calls).RegisterMembers(e.Mux)
	_, aliceTok := e.NewUser("alice")
	bob, bobTok := e.NewUser("bob")
	carol, _ := e.NewUser("carol")
	dm, status := openDM(t, e, aliceTok, bob, carol)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("open group DM = %d", status)
	}
	call := dm
	g := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: api.GroupCall, TargetID: dm, CallID: &call,
		Ciphersuite: 1, Epoch: callGroupEpoch, ExternalSenderKeyID: id.New(), E2EEVersion: 1,
		MediaVersion: 1, PolicyVersion: 1, Created: e.Clk.Now().Unix(),
	}
	if err := e.Repo.CreateGroup(t.Context(), g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	aliceDev, bobDev := deviceOf(t, e, aliceTok), deviceOf(t, e, bobTok)
	seedLeaf(t, e, g.GroupID, aliceDev, 3, nil)
	seedLeaf(t, e, g.GroupID, bobDev, 3, nil)
	if status, body := e.Do(http.MethodPost, "/v1/channels/"+dm.String()+"/calls", aliceTok, []any{}); status != http.StatusCreated {
		t.Fatalf("DM call start = %d %x", status, body)
	}
	room := stub.minted()[0][0]
	stub.setPresent(room, &livekit.ParticipantInfo{Identity: aliceDev.String()}, &livekit.ParticipantInfo{Identity: bobDev.String()})
	if status, body := e.Do(http.MethodDelete, "/v1/channels/"+dm.String()+"/members/"+bob.String(), aliceTok, nil); status != http.StatusNoContent {
		t.Fatalf("remove bob = %d (%x)", status, body)
	}
	if !waitRemoved(stub, room, bobDev) {
		t.Fatal("the removed participant's device is still in the DM's call room")
	}
	if removedDevice(stub, room, aliceDev) {
		t.Fatal("the removal cut alice too")
	}
}

// Minor m2 of the task 10 review: a request that holds a call's lock drives only its own device's
// repair — another device's pending cut is the retry loop's — so its hold stays bounded by its own
// work, however many repairs the call has outstanding.
func TestARequestDrivesOnlyItsOwnRepair(t *testing.T) {
	l := newLeaseEnv(t, 2, 3)
	l.calls.MarkPending(l.callID, l.devs[2], l.room)
	if status := l.share(0); status != http.StatusNoContent {
		t.Fatalf("dev0 share = %d", status)
	}
	if _, code, err := admitted(t, l.calls, l.room, l.devs[1]); code != "" || err != nil {
		t.Fatalf("dev1 at the gate = %q %v", code, err)
	}
	for _, u := range l.stub.permUpdates() {
		if u.Identity == l.devs[2].String() {
			t.Fatalf("a share or the gate of another device drove dev2's repair (%+v)", u)
		}
	}
	if devices, _ := l.stub.removals(); len(devices) != 0 {
		t.Fatalf("a request of another device cut dev2: %v", devices)
	}
	if l.calls.PendingRepairs() != 1 {
		t.Fatalf("pending = %d, want dev2's repair still left for the retry loop", l.calls.PendingRepairs())
	}
	l.calls.RetryPending(t.Context())
	if l.calls.PendingRepairs() != 0 {
		t.Fatal("the retry loop did not drive the repair")
	}
}

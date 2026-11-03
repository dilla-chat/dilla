package api_test

// The server-half fix wave, integration round (lens calls, round 2): DS-7's membership-bound Remove
// recorded at admission (N2), ruling (a) — the sweep removes a call-group leaf whose device stays out
// of the room — ruling (b) — F11 from the sweep's participant list, struck once per track — and the
// queue's bounds (N1, N3, N6).

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
)

// rejoinLeaf rewrites dev's member row of group as a rejoin at the same leaf index would leave it:
// the same leaf, added again in epoch added.
func rejoinLeaf(t *testing.T, e *env, group, dev id.ID, added uint64) {
	t.Helper()
	members, err := e.Repo.ListMembers(t.Context(), group)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	for i := range members {
		if members[i].DeviceID == dev {
			members[i].AddedEpoch = added
		}
	}
	if err := e.Repo.ReplaceMembers(t.Context(), group, callGroupEpoch, members); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
}

// leafOf is dev's live member row's leaf index in group.
func leafOf(t *testing.T, e *env, group, dev id.ID) uint32 {
	t.Helper()
	members, err := e.Repo.ListMembers(t.Context(), group)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	for _, m := range members {
		if m.DeviceID == dev && m.RemovedEpoch == nil {
			return m.LeafIndex
		}
	}
	t.Fatalf("%s holds no leaf of %s", dev, group)
	return 0
}

// N2: the leave's Remove is bound to the membership the /rtc gate admitted for that session, not to
// whatever the device holds when the (late) participant_joined is processed. The device was admitted
// at its epoch-3 leaf, its Remove committed and it rejoined at the same leaf in epoch 7 before the
// late join and leave of the old session were processed: the old session's leave proposes nothing.
func TestALateJoinAndLeaveAreBoundToTheAdmittedMembership(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	admitThroughProxy(t, f.calls, f.room, f.dev)
	rejoinLeaf(t, f.e, f.group, f.dev, callGroupEpoch)
	f.sendAs(t, webhook.EventParticipantJoined, f.dev.String(), "PA_1", nil)
	f.sendAs(t, webhook.EventParticipantLeft, f.dev.String(), "PA_1", nil)
	if got := f.ds.removeDevices(); len(got) != 0 {
		t.Fatalf("a late leave of the session admitted at the old membership proposed %v", got)
	}
}

// A leave whose join was never seen and that names no join time: TestAZeroJoinTimeBindsTheRecordedMember
// (callleave_test.go, web3.7) replaced the test that bound it to the latest admission, which after a
// reload is the newer session's.

// Ruling (a): a member whose participant_left never arrives is removed from the call group by the
// sweep: absent in one sweep, still absent a sweep period later, it gets a Remove bound to its
// (leaf, AddedEpoch).
func TestTheSweepRemovesTheLeafOfADeviceWhoseLeaveWasLost(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	_, memberDev, _ := joinedMember(t, f.e, f.ch, f.group, "member")
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String()})
	f.calls.SweepRooms(t.Context())
	if got := f.ds.removeDevices(); len(got) != 0 {
		t.Fatalf("a first sight of an absent leaf proposed %v", got)
	}
	f.e.Clk.Advance(api.RoomSweepInterval + time.Second)
	f.calls.SweepRooms(t.Context())
	got := f.ds.memberRemovals()
	want := memberRemove{Group: f.group, Device: memberDev, Leaf: leafOf(t, f.e, f.group, memberDev), Added: 3}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("membership-bound Removes = %+v, want %+v", got, want)
	}
}

// Ruling (a): a device cut from the room for losing connect, whose participant_left is then lost,
// is removed from the call group too.
func TestTheSweepRemovesTheLeafOfADeviceCutForLosingConnect(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	member, memberDev, _ := joinedMember(t, f.e, f.ch, f.group, "member")
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String()},
		&livekit.ParticipantInfo{Identity: memberDev.String()})
	denyInChannel(t, f.e, f.ch, member, api.PermConnect)
	f.calls.SweepRooms(t.Context())
	if !removedDevice(f.stub, f.room, memberDev) {
		t.Fatal("the sweep did not cut the device that lost connect")
	}
	// LiveKit's participant_left for the cut is lost; the stub still lists nobody new.
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String()})
	f.e.Clk.Advance(api.RoomSweepInterval + time.Second)
	f.calls.SweepRooms(t.Context())
	got := f.ds.memberRemovals()
	if len(got) != 1 || got[0].Device != memberDev || got[0].Added != 3 {
		t.Fatalf("membership-bound Removes = %+v, want the cut device's", got)
	}
}

// Ruling (a): a device admitted a second ago that has not reached the room yet is inside its join
// window and is not removed, however long its leaf was seen absent before.
func TestTheSweepDoesNotRemoveADeviceInsideItsJoinWindow(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	_, memberDev, _ := joinedMember(t, f.e, f.ch, f.group, "member")
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String()})
	f.calls.SweepRooms(t.Context())
	f.e.Clk.Advance(api.RoomSweepInterval)
	admitThroughProxy(t, f.calls, f.room, memberDev)
	f.e.Clk.Advance(time.Second)
	f.calls.SweepRooms(t.Context())
	if got := f.ds.removeDevices(); len(got) != 0 {
		t.Fatalf("a device admitted a second ago was removed: %v", got)
	}
}

// Ruling (a): an absent membership the device has since replaced by a rejoin at the same leaf (a new
// AddedEpoch) is never removed: the old one is gone, and the new one is seen for the first time.
func TestTheSweepNeverRemovesAMembershipTheDeviceReplaced(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	_, memberDev, _ := joinedMember(t, f.e, f.ch, f.group, "member")
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String()})
	f.calls.SweepRooms(t.Context())
	rejoinLeaf(t, f.e, f.group, memberDev, callGroupEpoch)
	f.e.Clk.Advance(api.RoomSweepInterval + time.Second)
	f.calls.SweepRooms(t.Context())
	if got := f.ds.removeDevices(); len(got) != 0 {
		t.Fatalf("the sweep removed a membership the device had replaced: %v", got)
	}
}

// noneTrack is a published microphone track flagged unencrypted (F11).
func noneTrack(sid string) *livekit.TrackInfo {
	return &livekit.TrackInfo{Sid: sid, Type: livekit.TrackType_AUDIO, Source: livekit.TrackSource_MICROPHONE,
		Encryption: livekit.Encryption_NONE}
}

// Ruling (b): a NONE track whose track_published was shed or lost is found by the sweep in the
// participant list and penalised there: the device is demoted to listen-only.
func TestTheSweepPenalisesANoneTrackWhoseWebhookWasLost(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String(), Tracks: []*livekit.TrackInfo{noneTrack("TR_none")}})
	f.calls.SweepRooms(t.Context())
	u := f.stub.permUpdates()
	if len(u) == 0 || u[len(u)-1].Identity != f.dev.String() || u[len(u)-1].Perm.GetCanPublish() {
		t.Fatalf("after the sweep the last push = %+v, want the device listen-only", u)
	}
	if perm, code, err := admitted(t, f.calls, f.room, f.dev); err != nil || code != "" || perm.GetCanPublish() {
		t.Fatalf("the gate after the sweep's strike = %v %q %v, want listen-only", perm, code, err)
	}
}

// Ruling (b): one bad publication counts once, whether the webhook or the sweep saw it first; a
// second bad publication is the repeat that removes the device.
func TestTheWebhookAndTheSweepStrikeOncePerTrack(t *testing.T) {
	for _, first := range []string{"webhook", "sweep"} {
		t.Run(first+" first", func(t *testing.T) {
			f := newEventsFixture(t, api.CallsConfig{})
			f.send(t, webhook.EventParticipantJoined, f.dev.String(), nil)
			f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String(), Tracks: []*livekit.TrackInfo{noneTrack("TR_none")}})
			webhookStrike := func() { f.send(t, webhook.EventTrackPublished, f.dev.String(), noneTrack("TR_none")) }
			sweepStrike := func() { f.calls.SweepRooms(t.Context()) }
			if first == "webhook" {
				webhookStrike()
				sweepStrike()
			} else {
				sweepStrike()
				webhookStrike()
			}
			sweepStrike()
			if got := deviceRemovals(f.stub); len(got) != 0 {
				t.Fatalf("one bad publication seen by the webhook and the sweep removed the device: %v", got)
			}
			f.send(t, webhook.EventTrackPublished, f.dev.String(), noneTrack("TR_second"))
			if got := deviceRemovals(f.stub); len(got) != 1 {
				t.Fatalf("a second bad publication = removals %v, want one", got)
			}
		})
	}
}

// N6 (m18): a retry pass over a call whose channel was tombstoned without its end ends the call and
// closes its room.
func TestARetryPassEndsACallWhoseChannelIsGone(t *testing.T) {
	b := newBarredEnv(t)
	b.calls.MarkPending(b.callID, b.memberDev, b.room)
	if err := b.e.Repo.DeleteChannel(t.Context(), b.ch, b.e.Clk.Now().Unix()); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	b.calls.RetryPending(t.Context())
	if !callEnded(t, b.e, b.callID) {
		t.Fatal("the retry pass left a call whose channel is gone live")
	}
	b.calls.ProcessQueueForTest(t.Context())
	if roomHeld(b.stub, b.room) {
		t.Fatal("the retry pass's end left the room open")
	}
}

// fillQueue fills the call work queue to its bound with cuts of unknown devices, without overflowing.
func fillQueue(t *testing.T, calls *api.Calls) {
	t.Helper()
	for {
		n, over := calls.QueuedForTest()
		if over {
			t.Fatal("filling the queue overflowed it")
		}
		if n >= 1024 {
			return
		}
		calls.CutDevice(t.Context(), id.New())
	}
}

// N6 (m20 and the other kinds): a sync, an eviction or a teardown that does not fit the full work
// queue raises the resync-all flag, so the loop's next pass sweeps every room.
func TestEveryKindThatDoesNotFitTheQueueRaisesTheSweep(t *testing.T) {
	t.Run("sync", func(t *testing.T) {
		b := newBarredEnv(t)
		fillQueue(t, b.calls)
		b.calls.RequestSync(t.Context(), ownerCommunityOf(t, b.e, b.ch), nil, nil)
		if _, over := b.calls.QueuedForTest(); !over {
			t.Fatal("a grant sync the full queue could not hold did not raise the sweep")
		}
	})
	t.Run("eviction", func(t *testing.T) {
		f := newEventsFixture(t, api.CallsConfig{})
		fillQueue(t, f.calls)
		f.events.Evict(t.Context(), f.group, []id.ID{f.dev})
		if _, over := f.calls.QueuedForTest(); !over {
			t.Fatal("an eviction the full queue could not hold did not raise the sweep")
		}
	})
	t.Run("teardown", func(t *testing.T) {
		m := newMembershipCallEnvWithoutLoop(t)
		fillQueue(t, m.calls)
		if status, _ := m.e.Do(http.MethodDelete, "/v1/channels/"+m.ch.String(), m.ownerTok, nil); status != http.StatusNoContent {
			t.Fatalf("DELETE channel = %d", status)
		}
		if _, over := m.calls.QueuedForTest(); !over {
			t.Fatal("a teardown the full queue could not hold did not raise the sweep")
		}
		m.calls.ProcessQueueForTest(t.Context())
		if roomHeld(m.stub, m.room) {
			t.Fatalf("the raised sweep did not close the deleted channel's room: rooms %v", m.stub.heldRooms())
		}
	})
}

// N1: a queued grant sync is served between the calls of a retry pass that meets busy calls, not
// after the whole pass.
func TestARetryPassServesAQueuedSyncBetweenCalls(t *testing.T) {
	m := newMembershipCallEnvWithoutLoop(t)
	for range 3 {
		call := id.New()
		m.calls.MarkPending(call, id.New(), call.String()+"-1")
		release, err := m.calls.LockCallForTest(t.Context(), call)
		if err != nil {
			t.Fatalf("LockCallForTest: %v", err)
		}
		defer release()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.calls.RetryPending(t.Context())
	}()
	time.Sleep(100 * time.Millisecond)
	// The kick queues its sync; no loop runs, so only the retry pass can serve it.
	if status, _ := m.e.Do(http.MethodDelete, "/v1/communities/"+m.cid.String()+"/members/"+m.member.String(), m.ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("kick = %d", status)
	}
	cut := waitFor(3500*time.Millisecond, func() bool { return removedDevice(m.stub, m.room, m.memberDev) })
	<-done
	if !cut {
		t.Fatal("a sync queued during a retry pass waited for the whole pass")
	}
}

// N1: a community-wide sync that overruns its budget leaves the calls it did not visit as pending
// room resyncs (driven by the next retry pass), rather than being put back whole.
func TestASyncThatOverrunsItsBudgetLeavesPendingResyncs(t *testing.T) {
	m := newMembershipCallEnvWithoutLoop(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := api.SyncCallGrants(ctx, m.e.Repo, api.NewResolver(m.e.Repo), m.stub, m.calls, m.cid, nil, nil); err == nil {
		t.Log("the sync over a spent budget answered no error")
	}
	if m.calls.PendingRepairs() == 0 {
		t.Fatal("a sync whose budget ran out left its live call without a pending room resync")
	}
	m.calls.RetryPending(t.Context())
	if m.calls.PendingRepairs() != 0 {
		t.Fatalf("the retry pass did not drive the room resync: %d pending", m.calls.PendingRepairs())
	}
}

// CALLS-2 (N6): a start served while a DELETE is parked at its "closed" point — after the call group
// is closed — never leaves a live call on that group: the group is closed before the row is ended, so
// the start finds no open group (404) instead of reopening the call on a group the end closes after.
// Each step is bounded, so a wrong order fails here instead of hanging.
func TestAStartDuringAnEndNeverLeavesACallOnAClosedGroup(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	parked, resume := make(chan struct{}), make(chan struct{})
	var once, unpark sync.Once
	release := func() { unpark.Do(func() { close(resume) }) }
	t.Cleanup(release)
	f.calls.SetEndHookForTest(func(stage string) {
		if stage == "closed" {
			once.Do(func() {
				close(parked)
				<-resume
			})
		}
	})
	done := make(chan int, 1)
	go func() {
		status, _ := f.e.Do(http.MethodDelete, "/v1/calls/"+f.callID.String(), f.tok, nil)
		done <- status
	}()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the DELETE never reached its \"closed\" point")
	}
	started := make(chan int, 1)
	go func() {
		status, _ := f.e.Do(http.MethodPost, "/v1/channels/"+f.ch.String()+"/calls", f.tok, []any{})
		started <- status
	}()
	var status int
	select {
	case status = <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the start during the end did not answer within 10 s")
	}
	release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the DELETE did not finish")
	}
	row, err := f.e.Repo.GetVoiceSession(t.Context(), f.callID)
	if err != nil {
		t.Fatalf("GetVoiceSession: %v", err)
	}
	g, err := f.e.Repo.GetGroup(t.Context(), f.group)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if row.Ended == nil && g.ClosedAt != nil && row.GroupID != nil && *row.GroupID == f.group {
		t.Fatalf("the start during the end (%d) left the call live on its closed group", status)
	}
}

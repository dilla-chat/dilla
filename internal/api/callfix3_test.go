package api_test

// The server-half fix wave, final re-review of the integrated tree (round 3): M-2 (an admission is
// recorded only once the /rtc proxy admits the join), M-3 (the tests that kill m4a, m12 and m6) and
// M-4 (a leave after an in-call resync still removes the device's current membership).

import (
	"context"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
)

// admitThroughProxy is what the /rtc proxy does for a fresh join it lets through: the gate's checks
// (AdmitRoom), then, once the proxy's own token and client checks passed too, the admission.
func admitThroughProxy(t *testing.T, calls *api.Calls, room string, dev id.ID) {
	t.Helper()
	if _, code, err := admitted(t, calls, room, dev); code != "" || err != nil {
		t.Fatalf("admission = %q %v", code, err)
	}
	calls.Admitted(t.Context(), room, dev, false)
}

// sendLeft is a participant_left of dev's session sid that LiveKit reports joined at joinedAt.
func (f *eventsFixture) sendLeft(t *testing.T, dev id.ID, sid string, joinedAt time.Time) {
	t.Helper()
	f.events.Handle(t.Context(), &livekit.WebhookEvent{Event: webhook.EventParticipantLeft, Id: "EV_" + id.New().String(),
		Room:        &livekit.Room{Name: f.room},
		Participant: &livekit.ParticipantInfo{Identity: dev.String(), Sid: sid, JoinedAtMs: joinedAt.UnixMilli()}})
	f.events.FlushAnnouncements()
}

// M-2: a /rtc request the gate's checks pass but the proxy then refuses (a bad token grant, a bad
// client protocol) records no admission, so it cannot hold a device's join window open: the sweep
// still removes the leaf of a device that stays out of the room.
func TestARefusedJoinOpensNoJoinWindow(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	_, memberDev, _ := joinedMember(t, f.e, f.ch, f.group, "member")
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String()})
	f.calls.SweepRooms(t.Context())
	f.e.Clk.Advance(api.RoomSweepInterval)
	// The gate's checks pass; the proxy refuses the join afterwards, so it records nothing.
	if _, code, err := admitted(t, f.calls, f.room, memberDev); code != "" || err != nil {
		t.Fatalf("the gate = %q %v", code, err)
	}
	f.e.Clk.Advance(time.Second)
	f.calls.SweepRooms(t.Context())
	if got := f.ds.memberRemovals(); len(got) != 1 || got[0].Device != memberDev {
		t.Fatalf("membership-bound Removes = %+v, want the absent device's: a refused join held its join window open", got)
	}
}

// M-2: refused /rtc requests cannot push a session's real admission out of the remembered ones. The
// device was admitted at its epoch-3 membership; after its leaf was re-added in epoch 7 it sends
// five requests the proxy refuses; the late leave of the admitted session is still bound to epoch 3,
// which the device no longer holds, so nothing is proposed.
func TestRefusedJoinsCannotDisplaceTheAdmission(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	admitThroughProxy(t, f.calls, f.room, f.dev)
	rejoinLeaf(t, f.e, f.group, f.dev, callGroupEpoch)
	for range 5 {
		if _, code, err := admitted(t, f.calls, f.room, f.dev); code != "" || err != nil {
			t.Fatalf("the gate = %q %v", code, err)
		}
	}
	// Still inside the session's join window, so M-4's fallback (the current membership) stays out
	// of it: what the leave proposes is the binding alone.
	f.sendLeft(t, f.dev, "PA_1", time.Time{})
	if got := f.ds.removeDevices(); len(got) != 0 {
		t.Fatalf("refused joins displaced the admission: the old session's leave proposed %v", got)
	}
}

// admissionWindow is past the join window, so a test's absent device is outside it.
func admissionWindow() time.Duration { return 31 * time.Second }

// M-3 (m4a): a late leave is bound to the admission that preceded its session's join, not to the
// latest one. The session joined after the epoch-3 admission; the device's leaf was re-added in
// epoch 7 and a later session admitted with it; the first session's leave, whose join was never
// seen, proposes nothing (epoch 3 is gone), where the latest admission would remove epoch 7.
func TestALateLeaveIsBoundToTheAdmissionBeforeItsJoin(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	admitThroughProxy(t, f.calls, f.room, f.dev)
	f.e.Clk.Advance(time.Second)
	joined := f.e.Clk.Now()
	f.e.Clk.Advance(10 * time.Second)
	rejoinLeaf(t, f.e, f.group, f.dev, callGroupEpoch)
	admitThroughProxy(t, f.calls, f.room, f.dev) // the next session, at epoch 7, connecting
	f.e.Clk.Advance(time.Second)                 // inside its join window: M-4's fallback stays out
	f.sendLeft(t, f.dev, "PA_1", joined)
	if got := f.ds.memberRemovals(); len(got) != 0 {
		t.Fatalf("the first session's leave removed %+v: it was bound to the later session's admission", got)
	}
}

// M-3 (m12): two absent sightings less than a sweep period apart remove nothing — a device whose
// session dropped for a moment keeps its leaf.
func TestTwoAbsentSightingsWithinOnePeriodRemoveNothing(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	joinedMember(t, f.e, f.ch, f.group, "member")
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String()})
	f.calls.SweepRooms(t.Context())
	f.e.Clk.Advance(api.RoomSweepInterval - time.Second)
	f.calls.SweepRooms(t.Context())
	if got := f.ds.removeDevices(); len(got) != 0 {
		t.Fatalf("a leaf absent for less than a sweep period was removed: %v", got)
	}
}

// M-3 (m6): the teardown pass is bounded. A teardown whose call group's close hangs in the delivery
// service gives up at its own deadline, the pass ends within its budget, and a cut queued meanwhile
// is served by the pass rather than after it.
func TestTheTeardownPassIsBoundedAndServesQueuedCuts(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	f.ds.mu.Lock()
	f.ds.closeBlocks = true
	f.ds.mu.Unlock()
	f.calls.EndChannelCalls(t.Context(), []id.ID{f.ch}, "test")
	f.calls.CutDevice(t.Context(), id.New())
	passes := f.calls.CutPasses()
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		f.calls.ProcessTeardownsForTest(context.WithoutCancel(t.Context()))
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the teardown pass did not end within 20 s with a hung group close")
	}
	if took := time.Since(start); took > 16*time.Second {
		t.Fatalf("the teardown pass took %s, past its 15 s budget", took)
	}
	if f.calls.CutPasses() == passes {
		t.Fatal("the teardown pass did not serve the queued cut")
	}
}

// M-4: after an in-call resync the device holds a new membership (its leaf re-added in a later
// epoch), so the leave's recorded one is gone. Its leave still removes it at once, bound to the
// membership it holds now, when it is out of the room and outside its join window.
func TestALeaveAfterAnInCallResyncRemovesTheCurrentMembership(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	admitThroughProxy(t, f.calls, f.room, f.dev)
	f.sendAs(t, webhook.EventParticipantJoined, f.dev.String(), "PA_1", nil)
	rejoinLeaf(t, f.e, f.group, f.dev, callGroupEpoch) // the in-call resync
	f.e.Clk.Advance(admissionWindow())
	f.sendAs(t, webhook.EventParticipantLeft, f.dev.String(), "PA_1", nil)
	got := f.ds.memberRemovals()
	if len(got) != 1 || got[0].Device != f.dev || got[0].Added != callGroupEpoch {
		t.Fatalf("membership-bound Removes = %+v, want the device's current epoch-%d membership", got, callGroupEpoch)
	}
}

// M-4: the fallback never removes a device it cannot see out of the room: when the SFU cannot list
// the room, nothing is proposed (the sweep decides later).
func TestALeaveAfterAResyncProposesNothingWhileTheRoomCannotBeSeen(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	admitThroughProxy(t, f.calls, f.room, f.dev)
	f.sendAs(t, webhook.EventParticipantJoined, f.dev.String(), "PA_1", nil)
	rejoinLeaf(t, f.e, f.group, f.dev, callGroupEpoch)
	f.e.Clk.Advance(admissionWindow())
	f.stub.mu.Lock()
	f.stub.listFail = errSFUDown
	f.stub.mu.Unlock()
	f.sendAs(t, webhook.EventParticipantLeft, f.dev.String(), "PA_1", nil)
	if got := f.ds.removeDevices(); len(got) != 0 {
		t.Fatalf("a leave whose room could not be listed removed %v", got)
	}
}

// M-4: nor a device inside its join window — it was admitted again a moment ago.
func TestALeaveAfterAResyncProposesNothingInsideTheJoinWindow(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	admitThroughProxy(t, f.calls, f.room, f.dev)
	f.sendAs(t, webhook.EventParticipantJoined, f.dev.String(), "PA_1", nil)
	rejoinLeaf(t, f.e, f.group, f.dev, callGroupEpoch)
	f.e.Clk.Advance(admissionWindow())
	admitThroughProxy(t, f.calls, f.room, f.dev) // the device is reconnecting at its new membership
	f.sendAs(t, webhook.EventParticipantLeft, f.dev.String(), "PA_1", nil)
	if got := f.ds.removeDevices(); len(got) != 0 {
		t.Fatalf("a leave of a device admitted a moment ago removed %v", got)
	}
}

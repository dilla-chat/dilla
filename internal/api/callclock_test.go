package api_test

// The instance clock and the SFU's clock are two clocks. LiveKit stamps a participant's join with
// its own (the in-process SFU reads the process's wall clock); the instance stamps an admission
// with Options.Clock. The test host freezes that one and moves it forward (advance_clock, a TOTP
// pick that waits for the next period), so it can run ahead of LiveKit's: a session's join then
// reads as earlier than its admission, its leave is bound to no membership, and its leaf stays in
// the call group (the remote tile that never went in leave-reload after blocked.spec).

import (
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/clock"
)

// A leave is bound to its session's admission by the SFU's clock, not the instance's: with the
// instance clock 75 s ahead of the SFU's, the session LiveKit reports joined a second after its
// admission still leaves at that membership, so its Remove is proposed at once.
func TestALeaveIsBoundToItsAdmissionWhenTheInstanceClockRunsAheadOfTheSFUs(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	// The start the fixture made admitted the device already: the SFU's clock is 75 s behind both
	// that admission and this one.
	sfuClk := clock.NewFake(f.e.Clk.Now().Add(-75 * time.Second))
	f.calls.WithSFUClock(sfuClk)
	admitThroughProxy(t, f.calls, f.room, f.dev)
	sfuClk.Advance(time.Second)
	joined := sfuClk.Now()
	f.events.Handle(t.Context(), &livekit.WebhookEvent{Event: webhook.EventParticipantJoined, Id: "EV_join",
		Room:        &livekit.Room{Name: f.room},
		Participant: &livekit.ParticipantInfo{Identity: f.dev.String(), Sid: "PA_1", JoinedAtMs: joined.UnixMilli()}})
	f.events.FlushAnnouncements()
	f.sendLeft(t, f.dev, "PA_1", joined)
	got := f.ds.memberRemovals()
	if len(got) != 1 || got[0].Device != f.dev || got[0].Added != 3 {
		t.Fatalf("membership-bound Removes = %+v, want the device's admitted epoch-3 membership", got)
	}
}

// LiveKit reports a join in whole milliseconds (JoinedAtMs); the admission is stamped by a clock with
// finer ticks. An admission 0.6 ms into a millisecond and the join reported at that millisecond are
// the same instant to the SFU, so the join still binds that admission.
func TestAnAdmissionInTheSameMillisecondAsTheJoinBindsIt(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	// The start the fixture made admitted the device at the instance's now; the SFU's clock is earlier,
	// so that admission is not the one the join finds.
	sfuClk := clock.NewFake(f.e.Clk.Now().Add(-time.Hour).Truncate(time.Millisecond).Add(600 * time.Microsecond))
	f.calls.WithSFUClock(sfuClk)
	admitThroughProxy(t, f.calls, f.room, f.dev)
	joined := time.UnixMilli(sfuClk.Now().UnixMilli())
	if !joined.Before(sfuClk.Now()) {
		t.Fatalf("the join %v is not before the admission %v: the test lost its sub-millisecond", joined, sfuClk.Now())
	}
	if _, _, ok := f.calls.AdmittedAsForTest(f.room, f.dev, joined); !ok {
		t.Fatalf("a join reported in the admission's own millisecond bound no admission")
	}
}

// With the SFU's clock 75 s ahead of the instance's (the web3.6 ghost), two admissions of one device
// bind by the SFU's stamps: a session joined between them binds the first, one joined after the
// second binds the second, and a join with no time binds the latest.
func TestTwoMembershipsAcrossASkewBindTheRightOne(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	s := f.e.Clk.Now().Add(75 * time.Second)
	sfuClk := clock.NewFake(s)
	f.calls.WithSFUClock(sfuClk)
	admitThroughProxy(t, f.calls, f.room, f.dev) // A: the epoch-3 membership, at S
	rejoinLeaf(t, f.e, f.group, f.dev, 5)
	sfuClk.Advance(40 * time.Second)
	f.e.Clk.Advance(time.Second)                 // the instance clock moves on its own
	admitThroughProxy(t, f.calls, f.room, f.dev) // B: the epoch-5 membership, at S+40 s
	for _, tc := range []struct {
		name  string
		join  time.Time
		added uint64
	}{
		{"joined between the two", s.Add(10 * time.Second), 3},
		{"joined after the second", s.Add(41 * time.Second), 5},
		{"no join time", time.Time{}, 5},
	} {
		_, added, ok := f.calls.AdmittedAsForTest(f.room, f.dev, tc.join)
		if !ok || added != tc.added {
			t.Errorf("%s: bound epoch %d (found %v), want %d", tc.name, added, ok, tc.added)
		}
	}
}

// Without an SFU clock of its own (a Calls built for a test, or an instance whose clock is the SFU's)
// the admission is stamped by the instance clock, as before.
func TestWithoutAnSFUClockTheInstanceClockBindsTheLeave(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	admitThroughProxy(t, f.calls, f.room, f.dev)
	f.e.Clk.Advance(time.Second)
	f.sendLeft(t, f.dev, "PA_1", f.e.Clk.Now())
	if got := f.ds.memberRemovals(); len(got) != 1 || got[0].Device != f.dev {
		t.Fatalf("membership-bound Removes = %+v, want the device's admitted membership", got)
	}
}

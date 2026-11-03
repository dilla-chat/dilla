package api_test

// web3.7 3a-server: a late leave of a device's earlier session never cuts the membership the device
// was admitted again with since (a page reload and rejoin). LiveKit's webhooks can arrive ~45 s late
// (SP-21 row 8), so the leave of the first session can be processed after the second admission's
// join window has closed and before the second session's participant_joined, at a moment the SFU
// lists the device out of the room.

import (
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// leafAt is one member row of a device: its leaf index, the epoch it was added in, and the epoch it
// was removed in (zero: a current leaf).
type leafAt struct {
	leaf           uint32
	added, removed uint64
}

// placeLeaves replaces dev's member rows of group with the given ones; every other device's rows stay.
func placeLeaves(t *testing.T, e *env, group, dev id.ID, rows ...leafAt) {
	t.Helper()
	members, err := e.Repo.ListMembers(t.Context(), group)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	d, err := e.Repo.GetDevice(t.Context(), dev)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	kept := members[:0]
	for _, m := range members {
		if m.DeviceID != dev {
			kept = append(kept, m)
		}
	}
	for _, r := range rows {
		for _, m := range kept {
			if m.LeafIndex == r.leaf {
				t.Fatalf("leaf %d is another device's", r.leaf)
			}
		}
		row := store.MemberRow{GroupID: group, LeafIndex: r.leaf, UserID: d.UserID, DeviceID: dev,
			SignatureKey: make([]byte, 32), AddedEpoch: r.added}
		if r.removed != 0 {
			removed := r.removed
			row.RemovedEpoch = &removed
		}
		kept = append(kept, row)
	}
	if err := e.Repo.ReplaceMembers(t.Context(), group, callGroupEpoch, kept); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
}

// sendJoined is a participant_joined of dev's session sid that LiveKit reports joined at joinedAt.
func (f *eventsFixture) sendJoined(t *testing.T, dev id.ID, sid string, joinedAt time.Time) {
	t.Helper()
	f.events.Handle(t.Context(), &livekit.WebhookEvent{Event: webhook.EventParticipantJoined, Id: "EV_" + id.New().String(),
		Room:        &livekit.Room{Name: f.room},
		Participant: &livekit.ParticipantInfo{Identity: dev.String(), Sid: sid, JoinedAtMs: joinedAt.UnixMilli()}})
	f.events.FlushAnnouncements()
}

// sendLeftNoTime is a participant_left of dev's session sid that names no join time.
func (f *eventsFixture) sendLeftNoTime(t *testing.T, dev id.ID, sid string) {
	t.Helper()
	f.events.Handle(t.Context(), &livekit.WebhookEvent{Event: webhook.EventParticipantLeft, Id: "EV_" + id.New().String(),
		Room:        &livekit.Room{Name: f.room},
		Participant: &livekit.ParticipantInfo{Identity: dev.String(), Sid: sid}})
	f.events.FlushAnnouncements()
}

// leftAnnounced reports whether flags 0 (out of the call) was announced for dev.
func (f *eventsFixture) leftAnnounced(dev id.ID) bool {
	for _, fr := range f.gw.all() {
		if fr.Device == dev && fr.Flags == 0 {
			return true
		}
	}
	return false
}

var (
	firstAdmission  = leafAt{leaf: 3, added: 5}
	secondAdmission = leafAt{leaf: 4, added: 7}
)

// admitFirstSession is D's first session: admitted at leaf 3 (epoch 5) and joined as PA_1. It returns
// the join time LiveKit reports for it.
func admitFirstSession(t *testing.T, f *eventsFixture) time.Time {
	t.Helper()
	placeLeaves(t, f.e, f.group, f.dev, firstAdmission)
	f.e.Clk.Advance(time.Second)
	admitThroughProxy(t, f.calls, f.room, f.dev) // A1
	f.e.Clk.Advance(time.Second)
	joined := f.e.Clk.Now()
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String(), Sid: "PA_1"})
	f.sendJoined(t, f.dev, "PA_1", joined)
	return joined
}

// The reload: D's first session is gone from the room, its leaf 3 removed in epoch 6, and D holds leaf
// 4 (epoch 7). With readmit, the /rtc gate admitted D's second session at leaf 4 (A2) — its
// participant_joined not processed yet, D out of the SFU's listing a moment (it is reconnecting), and
// A2 older than the join window — when the first session's late participant_left arrives with that
// session's join time. Without readmit (the control) D's membership moved with no new admission (an
// in-call resync), and the leave is its current session's: M-4's fallback removes leaf 4, as before.
// With sameLeaf, the second session was admitted at the membership the first held (a LiveKit
// restartConnection keeps the MLS leaf): the late leave's bound Remove would cut it directly.
func TestALateLeaveNeverCutsANewerAdmission(t *testing.T) {
	for _, tc := range []struct {
		name              string
		readmit, sameLeaf bool
	}{
		{"admitted again", true, false},
		{"admitted again at the same leaf", true, true},
		{"control: no second admission", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEventsFixture(t, api.CallsConfig{})
			joined := admitFirstSession(t, f)
			f.e.Clk.Advance(10 * time.Second)
			newer := secondAdmission
			if tc.sameLeaf {
				newer = firstAdmission
			} else {
				placeLeaves(t, f.e, f.group, f.dev, leafAt{leaf: 3, added: 5, removed: 6}, secondAdmission)
			}
			f.stub.setPresent(f.room) // D is out of the room for a moment
			if tc.readmit {
				admitThroughProxy(t, f.calls, f.room, f.dev) // A2, under a new session
			}
			f.e.Clk.Advance(admissionWindow()) // the first session's leave is ~45 s late
			f.sendLeft(t, f.dev, "PA_1", joined)
			var cut bool
			for _, r := range f.ds.memberRemovals() {
				if r.Device == f.dev && r.Leaf == newer.leaf && r.Added == newer.added {
					cut = true
				}
			}
			if tc.readmit {
				if cut {
					t.Fatalf("membership-bound Removes = %+v: the first session's late leave cut D's newer admission", f.ds.memberRemovals())
				}
				if f.leftAnnounced(f.dev) {
					t.Fatalf("voice_state = %+v: the first session's late leave announced D out of the call", f.gw.all())
				}
				return
			}
			if got := f.ds.memberRemovals(); !cut || len(got) != 1 {
				t.Fatalf("membership-bound Removes = %+v, want the one Remove of D's current leaf 4 (epoch 7)", got)
			}
			if !f.leftAnnounced(f.dev) {
				t.Fatal("the leave of D's current session announced nothing")
			}
		})
	}
}

// resumeThroughProxy is a LiveKit signal resume of the session already in the room: the /rtc proxy
// admits it like a join, flagged a resume.
func resumeThroughProxy(t *testing.T, calls *api.Calls, room string, dev id.ID) {
	t.Helper()
	if _, code, err := admitted(t, calls, room, dev); code != "" || err != nil {
		t.Fatalf("admission = %q %v", code, err)
	}
	calls.Admitted(t.Context(), room, dev, true)
}

// A signal resume goes through /rtc but is the same session: it is not a newer admission. A real
// leave after one still proposes its Remove and announces the device out; after an in-call resync
// moved the leaf, M-4's fallback still removes the current membership.
func TestAResumeIsNotANewerAdmission(t *testing.T) {
	t.Run("a real leave after a resume", func(t *testing.T) {
		f := newEventsFixture(t, api.CallsConfig{})
		joined := admitFirstSession(t, f)
		f.e.Clk.Advance(5 * time.Second)
		resumeThroughProxy(t, f.calls, f.room, f.dev)
		f.e.Clk.Advance(5 * time.Second)
		f.stub.setPresent(f.room)
		f.sendLeft(t, f.dev, "PA_1", joined)
		got := f.ds.memberRemovals()
		if len(got) != 1 || got[0].Leaf != firstAdmission.leaf || got[0].Added != firstAdmission.added {
			t.Fatalf("membership-bound Removes = %+v, want the one of leaf 3 (epoch 5)", got)
		}
		if !f.leftAnnounced(f.dev) {
			t.Fatal("a real leave after a resume announced nothing: D stays in the call for everyone")
		}
	})
	t.Run("a real leave after a resume and a resync", func(t *testing.T) {
		f := newEventsFixture(t, api.CallsConfig{})
		joined := admitFirstSession(t, f)
		f.e.Clk.Advance(5 * time.Second)
		resumeThroughProxy(t, f.calls, f.room, f.dev)
		placeLeaves(t, f.e, f.group, f.dev, leafAt{leaf: 3, added: 5, removed: 6}, secondAdmission)
		f.e.Clk.Advance(admissionWindow())
		f.stub.setPresent(f.room)
		f.sendLeft(t, f.dev, "PA_1", joined)
		got := f.ds.memberRemovals()
		if len(got) != 1 || got[0].Leaf != secondAdmission.leaf || got[0].Added != secondAdmission.added {
			t.Fatalf("membership-bound Removes = %+v, want M-4's one of leaf 4 (epoch 7)", got)
		}
		if !f.leftAnnounced(f.dev) {
			t.Fatal("a real leave after a resume and a resync announced nothing")
		}
	})
}

// The reload's new session reports self-mute before its participant_joined is processed; the first
// session's late leave does not wipe it, so the new session's in_call carries it.
func TestALateLeaveKeepsTheNewSessionsSelfFlags(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	joined := admitFirstSession(t, f)
	f.e.Clk.Advance(10 * time.Second)
	placeLeaves(t, f.e, f.group, f.dev, leafAt{leaf: 3, added: 5, removed: 6}, secondAdmission)
	admitThroughProxy(t, f.calls, f.room, f.dev) // A2, the reload's session
	f.events.SetSelf(f.callID, f.dev, 1)         // self-muted, from the new session
	f.stub.setPresent(f.room)
	f.e.Clk.Advance(time.Second)
	f.sendLeft(t, f.dev, "PA_1", joined)
	f.e.Clk.Advance(time.Second)
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String(), Sid: "PA_2"})
	f.sendJoined(t, f.dev, "PA_2", f.e.Clk.Now())
	if got := f.gw.last(); got.Device != f.dev || got.Flags != api.VoiceInCall|2 {
		t.Fatalf("the new session's voice_state = %+v, want in_call with self-mute (flags %d)", got, api.VoiceInCall|2)
	}
}

// One admission, one join, its own leave: exactly one Remove, of that membership.
func TestALeaveOfTheCurrentSessionStillProposesItsRemove(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	joined := admitFirstSession(t, f)
	f.stub.setPresent(f.room)
	f.e.Clk.Advance(time.Second)
	f.sendLeft(t, f.dev, "PA_1", joined)
	got := f.ds.memberRemovals()
	if len(got) != 1 || got[0].Device != f.dev || got[0].Leaf != firstAdmission.leaf || got[0].Added != firstAdmission.added {
		t.Fatalf("membership-bound Removes = %+v, want exactly the one of leaf 3 (epoch 5)", got)
	}
	if !f.leftAnnounced(f.dev) {
		t.Fatal("the leave announced nothing")
	}
}

// A leave that names no join time binds the membership recorded at its session's participant_joined
// and proposes its Remove; with no recorded member it proposes nothing (the room sweep decides); it
// never binds the second admission merely because it is the latest.
func TestAZeroJoinTimeBindsTheRecordedMember(t *testing.T) {
	t.Run("the recorded member", func(t *testing.T) {
		f := newEventsFixture(t, api.CallsConfig{})
		admitFirstSession(t, f)
		f.stub.setPresent(f.room)
		f.e.Clk.Advance(time.Second)
		f.sendLeftNoTime(t, f.dev, "PA_1")
		got := f.ds.memberRemovals()
		if len(got) != 1 || got[0].Device != f.dev || got[0].Leaf != firstAdmission.leaf || got[0].Added != firstAdmission.added {
			t.Fatalf("membership-bound Removes = %+v, want the one of the recorded leaf 3 (epoch 5)", got)
		}
	})
	t.Run("no recorded member", func(t *testing.T) {
		f := newEventsFixture(t, api.CallsConfig{})
		placeLeaves(t, f.e, f.group, f.dev, firstAdmission)
		f.e.Clk.Advance(time.Second)
		admitThroughProxy(t, f.calls, f.room, f.dev) // A1; its participant_joined is never seen
		f.e.Clk.Advance(time.Second)
		f.sendLeftNoTime(t, f.dev, "PA_1")
		if got := f.ds.memberRemovals(); len(got) != 0 {
			t.Fatalf("membership-bound Removes = %+v: a leave with no join time and no recorded member bound one", got)
		}
	})
	t.Run("never the latest admission", func(t *testing.T) {
		f := newEventsFixture(t, api.CallsConfig{})
		placeLeaves(t, f.e, f.group, f.dev, firstAdmission)
		f.e.Clk.Advance(time.Second)
		admitThroughProxy(t, f.calls, f.room, f.dev) // A1; PA_1's participant_joined is never seen
		f.e.Clk.Advance(10 * time.Second)
		placeLeaves(t, f.e, f.group, f.dev, leafAt{leaf: 3, added: 5, removed: 6}, secondAdmission)
		admitThroughProxy(t, f.calls, f.room, f.dev) // A2: the reload's session, still connecting
		f.e.Clk.Advance(time.Second)
		f.sendLeftNoTime(t, f.dev, "PA_1")
		if got := f.ds.memberRemovals(); len(got) != 0 {
			t.Fatalf("membership-bound Removes = %+v: the first session's leave bound the latest admission", got)
		}
	})
}

// deferLeave is the reload whose first session's late leave is deferred: D admitted and joined as
// PA_1 at leaf 3, then admitted again (A2, leaf 4) by a fresh join that self-mutes before its
// participant_joined is processed, and PA_1's leave arriving while D is out of the SFU's listing.
func deferLeave(t *testing.T, f *eventsFixture) {
	t.Helper()
	joined := admitFirstSession(t, f)
	f.e.Clk.Advance(10 * time.Second)
	placeLeaves(t, f.e, f.group, f.dev, leafAt{leaf: 3, added: 5, removed: 6}, secondAdmission)
	admitThroughProxy(t, f.calls, f.room, f.dev) // A2
	f.events.SetSelf(f.callID, f.dev, 1)         // the new session self-mutes
	f.stub.setPresent(f.room)
	f.e.Clk.Advance(time.Second)
	f.sendLeft(t, f.dev, "PA_1", joined)
	if f.leftAnnounced(f.dev) {
		t.Fatal("the deferred leave announced D out at once")
	}
}

func inParticipants(f *eventsFixture, dev id.ID) bool {
	for _, p := range f.events.Participants(f.callID) {
		if p.Device == dev {
			return true
		}
	}
	return false
}

// M1/n3: the newer session never joins (its LiveKit join failed, or the page closed). Inside A2's join
// window the leave stays deferred; once the window has passed with no participant_joined, the retry
// loop completes it: D is gone from the call, flags 0 is announced and its self flags are cleared.
// The leaf is left to the room sweep and its two-sighting rule (review n4): completion proposes no
// Remove.
func TestADeferredLeaveCompletesWhenTheRejoinNeverComes(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	deferLeave(t, f)
	f.e.Clk.Advance(10 * time.Second) // inside A2's join window
	f.calls.RetryPending(t.Context())
	f.events.FlushAnnouncements()
	if f.leftAnnounced(f.dev) {
		t.Fatal("the deferred leave completed inside the newer admission's join window")
	}
	f.e.Clk.Advance(admissionWindow())
	f.calls.RetryPending(t.Context())
	f.events.FlushAnnouncements()
	if !f.leftAnnounced(f.dev) {
		t.Fatalf("voice_state = %+v: the rejoin never came and D was never announced out", f.gw.all())
	}
	if inParticipants(f, f.dev) {
		t.Fatal("D is still a participant of the call after its deferred leave completed")
	}
	if got := f.ds.memberRemovals(); len(got) != 0 {
		t.Fatalf("membership-bound Removes = %+v: completion proposed a Remove; the room sweep decides the leaf", got)
	}
	// The self flags went with it: D's next session joins without the stale self-mute.
	admitThroughProxy(t, f.calls, f.room, f.dev)
	f.e.Clk.Advance(time.Second)
	f.sendJoined(t, f.dev, "PA_3", f.e.Clk.Now())
	if last := f.gw.last(); last.Device != f.dev || last.Flags != api.VoiceInCall {
		t.Fatalf("the next session's voice_state = %+v, want in_call without the stale self-mute", last)
	}
}

// The newer session joins inside its window: the deferred leave never completes, nothing announces D
// out, and the new session keeps its place and its self flags.
func TestADeferredLeaveEndsWhenTheRejoinJoins(t *testing.T) {
	f := newEventsFixture(t, api.CallsConfig{})
	deferLeave(t, f)
	f.e.Clk.Advance(5 * time.Second)
	f.stub.setPresent(f.room, &livekit.ParticipantInfo{Identity: f.dev.String(), Sid: "PA_2"})
	f.sendJoined(t, f.dev, "PA_2", f.e.Clk.Now())
	f.e.Clk.Advance(admissionWindow())
	f.calls.RetryPending(t.Context())
	f.events.FlushAnnouncements()
	if f.leftAnnounced(f.dev) {
		t.Fatalf("voice_state = %+v: D was announced out though its new session joined", f.gw.all())
	}
	if !inParticipants(f, f.dev) {
		t.Fatal("the new session is no participant of the call")
	}
	if got := f.ds.memberRemovals(); len(got) != 0 {
		t.Fatalf("membership-bound Removes = %+v: the joined session's leaf was removed", got)
	}
	if last := f.gw.last(); last.Device != f.dev || last.Flags != api.VoiceInCall|2 {
		t.Fatalf("the new session's voice_state = %+v, want in_call with its self-mute", last)
	}
}

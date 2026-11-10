package gateway

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 12 fix round 1, extended to user connections by Jonas's ruling: a resume whose whole
// unacknowledged gap aged out of the ring (the ring is empty, so its floor says nothing) is refused
// with invalid_session, and the client heals through identify, ready's digest and ?from= catch-up.
// A gap where only acknowledged frames aged out still resumes.
func TestUserResumeIsRefusedWhenItsUnackedGapAgedOut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ack     uint64
		resumes bool
	}{
		{"the unacknowledged frame aged out", 1, false},
		{"only acknowledged frames aged out", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			device := id.New()
			c := h.connect(t, device)
			token := h.readyResumeToken(t, c) // ready is n = 1
			group := id.New()
			p, err := EpochChangedPayload(4, 9)
			if err != nil {
				t.Fatal(err)
			}
			c.send(Frame{Op: OpMLSEpochChanged, GroupID: &group, Payload: p, Replay: true})
			if in := h.waitFrame(t, device); in.Op != OpMLSEpochChanged || in.CID != 2 {
				t.Fatalf("op %d n %d, want mls.epoch_changed n 2", in.Op, in.CID)
			}
			h.gw.suspend(c)
			h.clk.Advance(h.gw.opts.RingTTL + time.Second)
			_, resumed := h.resumeFrom(t, c, token, tc.ack)
			if tc.resumes && resumed.Op != OpResumed {
				t.Fatalf("op %d, want resumed: everything that aged out was acknowledged", resumed.Op)
			}
			if !tc.resumes && resumed.Op != OpInvalidSession {
				t.Fatalf("op %d, want invalid_session: the unacknowledged n 2 aged out", resumed.Op)
			}
		})
	}
}

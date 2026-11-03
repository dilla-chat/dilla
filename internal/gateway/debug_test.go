package gateway

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// debugUntil polls Debug until ok accepts the report: the writer counts a frame written only
// after the sink returns, which is after the test has already read it off the sink.
func debugUntil(t *testing.T, h *harness, device, group id.ID, ok func(DebugReport) bool) DebugReport {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r := h.gw.Debug(device, group)
		if ok(r) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("Debug never reached the expected state: %+v", r)
		}
		time.Sleep(time.Millisecond)
	}
}

func epochChanged(t *testing.T, g id.ID) Frame {
	t.Helper()
	p, err := EpochChangedPayload(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	return Frame{Op: OpMLSEpochChanged, GroupID: &g, Payload: p, Replay: true}
}

// Debug is what explains a client that stops receiving with its socket open: whether the device is
// in the group's fan-out list, and, per connection, how many frames were handed to its writer and
// how many the writer put on the wire. A stalled socket shows the gap between the two.
func TestDebugReportsFanOutMembershipAndTheWritersCounters(t *testing.T) {
	h := newHarness(t)
	device, other, group := id.New(), id.New(), id.New()
	h.gw.SetGroupMembers(group, []id.ID{other, device})
	h.gw.SetGroupLeaves(group, map[id.ID]uint32{other: 0, device: 5})
	c := h.connect(t, device)
	h.drainReady(t, c)

	r := debugUntil(t, h, device, group, func(r DebugReport) bool {
		return len(r.Conns) == 1 && r.Conns[0].Written == 1
	})
	if r.Device != device.String() || r.Group != group.String() {
		t.Errorf("report names %s in %s, want %s in %s", r.Device, r.Group, device, group)
	}
	if !r.InMembers || r.Members != 2 {
		t.Errorf("in_members = %v, members = %d; want true, 2", r.InMembers, r.Members)
	}
	if r.Leaf == nil || *r.Leaf != 5 {
		t.Errorf("leaf = %v, want 5", r.Leaf)
	}
	conn := r.Conns[0]
	if conn.State != "ready" || !conn.HasWriter || !conn.WriterGateOpen || conn.WriterStopped ||
		conn.WriterFinished {
		t.Errorf("a live connection reads %+v", conn)
	}
	if conn.N != 1 || conn.Enqueued != 1 || conn.RingFrames != 1 || conn.RingTop != 1 {
		t.Errorf("after ready alone: n %d, enqueued %d, ring %d up to %d; want 1, 1, 1, 1",
			conn.N, conn.Enqueued, conn.RingFrames, conn.RingTop)
	}

	// A stalled socket: the frames are handed to the writer, and not written.
	h.stall(c)
	for range 3 {
		h.gw.DeliverGroup(group, epochChanged(t, group))
	}
	r = debugUntil(t, h, device, group, func(r DebugReport) bool {
		return r.Conns[0].Enqueued == 4 && r.Conns[0].QueuedFrames == 2
	})
	if got := r.Conns[0]; got.Written != 1 || got.N != 4 || got.QueuedBytes == 0 {
		t.Errorf("stalled: written %d, n %d, queued bytes %d; want 1, 4, > 0",
			got.Written, got.N, got.QueuedBytes)
	}

	h.release(c)
	for range 3 {
		h.waitFrame(t, device)
	}
	debugUntil(t, h, device, group, func(r DebugReport) bool {
		return r.Conns[0].Written == 4 && r.Conns[0].QueuedFrames == 0 && r.Conns[0].QueuedBytes == 0
	})

	// A group the device is not in, and a device the group has no leaf for.
	stranger := h.gw.Debug(device, id.New())
	if stranger.InMembers || stranger.Members != 0 || stranger.Leaf != nil {
		t.Errorf("an unknown group reads %+v", stranger)
	}

	// Suspended: out of the live registry, counted in the resume window, its writer stopped.
	h.suspend(c)
	r = h.gw.Debug(device, group)
	if len(r.Conns) != 0 || r.Suspended != 1 {
		t.Errorf("after suspend: %d live connections, %d suspended; want 0, 1", len(r.Conns), r.Suspended)
	}
	if d := c.debug(); d.State != "closed" || !d.WriterStopped {
		t.Errorf("the suspended connection reads %+v", d)
	}
}

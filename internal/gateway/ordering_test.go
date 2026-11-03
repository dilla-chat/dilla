package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// A connection must not be reachable by a producer until its handshake frame is on the writer.
// registerVersions publishes to the registry and only then runs sendReady, which makes three
// store round-trips — a wide window in production. Anything delivered in it takes n = 1 and
// `ready` then carries n = 2, against protocol/02's "n ... the per-session replay sequence,
// starting at 1" and against the hello/identify/ready order every client is written to.
//
// Options.Outstanding is called from inside sendReady, which makes the window reachable from the
// test goroutine without a sleep or a race.
func TestTheFirstFrameOnANewConnectionIsAlwaysReady(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	group := id.New()
	h.setGroupsForDevice(device, group)
	h.gw.SetGroupMembers(group, []id.ID{device})
	h.gw.opts.Outstanding = func(_ context.Context, g id.ID) uint64 {
		p, err := EpochChangedPayload(1, 1)
		if err != nil {
			t.Error(err)
			return 0
		}
		h.gw.DeliverGroup(g, Frame{Op: OpMLSEpochChanged, GroupID: &g, Payload: p, Replay: true})
		return 0
	}

	c := h.connect(t, device)

	in := h.waitFrame(t, device)
	if in.Op != OpReady {
		t.Fatalf("the first frame a client sees is op %d, not ready(3)", in.Op)
	}
	if in.CID != 1 {
		t.Fatalf("ready carries n = %d, want 1", in.CID)
	}
	// And the fan-out that raced the handshake reached nothing: a client that has not been told
	// its replay window yet is not a delivery target.
	select {
	case b := <-h.sink(c).frames:
		got, err := decodeAny(b)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		t.Fatalf("op %d reached the connection before it finished its handshake", got.Op)
	case <-time.After(50 * time.Millisecond):
	}
}

// The same rule on the resume path: `resumed` and the replayed frames go on the writer before the
// connection goes back into the live registry, so a concurrent fan-out can neither precede
// `resumed` nor interleave with the replay. Either is the non-monotonic n a client answers with
// close 4007.
//
// The hook fires on the sixth Clock.Now() of resumeConnection, which is writer.enqueue stamping
// the `resumed` frame. The five before it are ring.floor and ring.since inside tryResume (one
// each, from evictLocked), resumeConnection's own `now`, and the two inside ring.add for
// `resumed`. At that instant the OLD ordering has already published the connection.
func TestTheFirstFrameAfterAResumeIsAlwaysResumed(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	group := id.New()
	c := h.connect(t, device)
	h.gw.SetGroupMembers(group, []id.ID{device})
	for n := uint64(1); n <= 2; n++ {
		p, err := EpochChangedPayload(1, n)
		if err != nil {
			t.Fatalf("payload: %v", err)
		}
		c.send(Frame{Op: OpMLSEpochChanged, GroupID: &group, Payload: p, Replay: true})
	}
	token := c.resumeToken()
	h.gw.suspend(c)

	s := newRecordingSink(16, false)
	go h.gw.serve(context.Background(), s, "")
	h.readFrom(t, s) // hello

	h.clk.armNth(6, func() {
		// The fan-out runs on its own goroutine and is waited for, so a future regression that
		// holds a lock across the enqueue fails the test instead of deadlocking the package.
		done := make(chan struct{})
		go func() {
			defer close(done)
			p, err := EpochChangedPayload(9, 9)
			if err != nil {
				t.Error(err)
				return
			}
			h.gw.DeliverGroup(group, Frame{Op: OpMLSEpochChanged, GroupID: &group, Payload: p, Replay: true})
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("a producer blocked on a connection that was mid-resume")
		}
	})
	t.Cleanup(h.clk.disarm)

	p, err := payload(h.tokenFor(device), token, h.generation, uint64(1))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpResume, Payload: p, Replay: true}, 2))

	if in := h.readFrom(t, s); in.Op != OpResumed {
		t.Fatalf("the first frame after a resume is op %d, not resumed(4)", in.Op)
	}
	// Exactly the replay follows, with the n each frame was first sent with. Anything the racing
	// producer managed to slip in shows up here as a frame carrying a fresh n.
	for _, want := range []uint64{2, 3} {
		in := h.readFrom(t, s)
		if in.Op != OpMLSEpochChanged {
			t.Fatalf("replayed op %d, want mls.epoch_changed", in.Op)
		}
		if in.CID != want {
			t.Fatalf("frame carries n = %d, want the replayed %d", in.CID, want)
		}
	}
}

// Two producers delivering to the same connection must not be stamped in one order and enqueued
// in the other. Allocating n, ringing the frame and handing it to the writer is one critical
// section; when it is three, the client sees n go backwards and closes 4007. `go test -race`
// cannot see this — it is an ordering bug, not a data race — so the hook clock is what drives it.
//
// The hook fires on the third Clock.Now() of the first send: ring.add, its evictLocked, and then
// writer.enqueue. The second producer is given a real 100 ms to overtake, which it can only do if
// the first send has already let go of the connection.
func TestTwoProducersCannotEnqueueOutOfNOrder(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())
	h.drainReady(t, c)

	second, err := EpochChangedPayload(2, 2)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	overtaken := make(chan struct{})
	h.clk.armNth(3, func() {
		go func() {
			defer close(overtaken)
			c.send(Frame{Op: OpMLSEpochChanged, Payload: second, Replay: true})
		}()
		time.Sleep(100 * time.Millisecond)
	})
	t.Cleanup(h.clk.disarm)

	first, err := EpochChangedPayload(1, 1)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	c.send(Frame{Op: OpMLSEpochChanged, Payload: first, Replay: true})
	<-overtaken

	// ready took n = 1, so the two fan-out frames are n = 2 and n = 3 and must reach the wire in
	// that order.
	for _, want := range []uint64{2, 3} {
		in := h.waitFrame(t, c.deviceID)
		if in.CID != want {
			t.Fatalf("n on the wire = %d, want %d: n was allocated in one order and enqueued in another", in.CID, want)
		}
	}
}

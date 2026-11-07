package gateway

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 10: the gateway half of the sealed relay. A user's sealed command reaches only the Bot
// runtime's connection (op 64), a reply reaches only the requesting device (op 62) and is held in a
// suspended connection's ring for at most two minutes, and a state hint (op 63) reaches only the
// connections subscribed to the scope's group.

func botReplyFrame(t *testing.T, requestID id.ID, seq uint64, sealed []byte, final bool) Frame {
	t.Helper()
	p, err := BotReplyPayload(requestID, seq, sealed, final)
	if err != nil {
		t.Fatalf("BotReplyPayload: %v", err)
	}
	return Frame{Op: OpBotReply, Payload: p, Replay: true}
}

// resumeFrom resumes a suspended connection over a new sink from lastN and returns the sink, after
// reading the `resumed` frame.
func (h *harness) resumeFrom(t *testing.T, c *conn, token []byte, lastN uint64) (*recordingSink, Inbound) {
	t.Helper()
	s := newRecordingSink(16, false)
	go h.gw.serve(context.Background(), s, "")
	h.readFrom(t, s) // hello
	p, err := payload(h.tokenFor(c.deviceID), token, h.generation, lastN)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpResume, Payload: p, Replay: true}, 2))
	return s, h.readFrom(t, s)
}

// noFrameOn asserts that nothing more reaches the sink within a short wait.
func noFrameOn(t *testing.T, s *recordingSink) {
	t.Helper()
	select {
	case b := <-s.frames:
		in, _ := decodeAny(b)
		t.Fatalf("an unexpected frame op %d arrived", in.Op)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestReplyHeldForSuspendedDeviceIsDroppedAfterTwoMinutes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		after   time.Duration
		replays bool
	}{
		{"resumed inside two minutes", 2*time.Minute - time.Second, true},
		{"resumed after two minutes", 2*time.Minute + time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			device := id.New()
			c := h.connect(t, device)
			token := h.readyResumeToken(t, c) // ready is n = 1
			h.gw.suspend(c)

			requestID := id.New()
			sealed := bytes.Repeat([]byte{0x5a}, 96)
			// The device has no live connection: the reply is held in the suspended connection's ring.
			h.gw.DeliverDevice(device, botReplyFrame(t, requestID, 0, sealed, true))

			h.clk.Advance(tc.after)
			s, resumed := h.resumeFrom(t, c, token, 1)
			if !tc.replays {
				if resumed.Op == OpResumed {
					noFrameOn(t, s)
				}
				return
			}
			if resumed.Op != OpResumed {
				t.Fatalf("op %d, want resumed", resumed.Op)
			}
			in := h.readFrom(t, s)
			if in.Op != OpBotReply || in.CID != 2 {
				t.Fatalf("replayed op %d n %d, want bot_reply n 2", in.Op, in.CID)
			}
			var got []byte
			if err := cborx.Unmarshal(in.Payload[2], &got); err != nil || !bytes.Equal(got, sealed) {
				t.Fatalf("replayed sealed reply %x: %v", got, err)
			}
		})
	}
}

// The two-minute hold is op 62's own bound: in a ring whose TTL is longer, a reply at the head is
// gone after two minutes while every other replayable frame keeps the ring's TTL.
func TestReplyRingEntryExpiresBeforeTheRingTTL(t *testing.T) {
	h := newHarness(t)
	r := newRing(ringLimits{Frames: 16, Bytes: 1 << 20, TTL: 10 * time.Minute}, h.clk)
	r.addOp(1, []byte{1}, FrameScope{}, OpBotReply)
	h.clk.Advance(time.Second)
	r.addOp(2, []byte{2}, FrameScope{}, OpMessageCT)
	h.clk.Advance(2 * time.Minute)
	if got := r.floor(); got != 2 {
		t.Fatalf("floor = %d, want 2: the reply expired after two minutes", got)
	}
	h.clk.Advance(5 * time.Minute)
	if got := r.floor(); got != 2 {
		t.Fatalf("floor = %d, want 2: a message.ct keeps the ring's ten-minute TTL", got)
	}
}

// A reply delivered to one device reaches that device's connections, live or suspended, and no
// other device of the same user.
func TestReplyGoesOnlyToTheRequestingDeviceConnections(t *testing.T) {
	h := newHarness(t)
	asking, other := id.New(), id.New()
	a := h.connect(t, asking)
	h.drainReady(t, a)
	b := h.connect(t, other)
	h.drainReady(t, b)
	h.gw.DeliverDevice(asking, botReplyFrame(t, id.New(), 1, []byte{1, 2, 3}, false))
	if in := h.waitFrame(t, asking); in.Op != OpBotReply {
		t.Fatalf("asking device got op %d, want bot_reply", in.Op)
	}
	noFrameOn(t, h.sink(b))
}

func TestRelayDeliversACommandOnlyToTheBotConnection(t *testing.T) {
	h := newHarness(t)
	botDevice, humanDevice := id.New(), id.New()
	bot, _ := h.finishedBot(t, botDevice)
	human := h.connect(t, humanDevice)
	h.drainReady(t, human)

	p, err := BotCommandPayload(id.New(), id.New(), id.New(), humanDevice, []byte{0x85})
	if err != nil {
		t.Fatal(err)
	}
	// Task 12: op 64 is sequenced in the Bot connection's ring, so a resume replays it.
	f := Frame{Op: OpBotCommand, Payload: p, Replay: true}
	if !h.gw.DeliverBot(botDevice, f) {
		t.Fatal("DeliverBot to a live Bot connection reported offline")
	}
	if in := h.waitFrame(t, botDevice); in.Op != OpBotCommand || in.CID != 3 {
		t.Fatalf("Bot got op %d n %d, want bot_command n 3", in.Op, in.CID)
	}
	// A device whose connections are not Bot runtime sessions is no Bot, and nothing is sent.
	if h.gw.DeliverBot(humanDevice, f) {
		t.Fatal("DeliverBot to a human connection reported delivered")
	}
	noFrameOn(t, h.sink(human))
	if h.gw.DeliverBot(id.New(), f) {
		t.Fatal("DeliverBot to a device with no connection reported delivered")
	}
	// A suspended Bot connection holds the command for its resume.
	h.gw.suspend(bot)
	if !h.gw.DeliverBot(botDevice, f) {
		t.Fatal("DeliverBot to a suspended Bot connection was not held")
	}
	// Once the resume window has passed, the Bot is offline.
	h.clk.Advance(h.gw.opts.ResumeWindow + time.Second)
	h.gw.sweepSuspended()
	if h.gw.DeliverBot(botDevice, f) {
		t.Fatal("DeliverBot to an expired Bot connection reported delivered")
	}
}

func TestHintReachesOnlySubscribedConnections(t *testing.T) {
	h := newHarness(t)
	group := id.New()
	in, out, stranger := id.New(), id.New(), id.New()
	h.setGroupsForDevice(in, group)
	h.setGroupsForDevice(out, group)
	ci := h.connect(t, in)
	h.drainReady(t, ci)
	co := h.connect(t, out)
	h.drainReady(t, co)
	cs := h.connect(t, stranger)
	h.drainReady(t, cs)
	h.gw.SetGroupMembers(group, []id.ID{in, out})
	// `out` unsubscribed from the group: it is a member but is not showing the channel.
	unsub, err := cborx.Marshal([]any{[]id.ID{group}})
	if err != nil {
		t.Fatal(err)
	}
	var elems []cbor.RawMessage
	if err := cborx.Unmarshal(unsub, &elems); err != nil {
		t.Fatal(err)
	}
	h.gw.handle(context.Background(), co, Inbound{Op: OpUnsubscribe, Payload: elems})

	p, err := BotStateChangedPayload(1, id.New(), 7)
	if err != nil {
		t.Fatal(err)
	}
	h.gw.DeliverSubscribed(group, Frame{Op: OpBotStateChanged, Payload: p})
	got := h.waitFrame(t, in)
	if got.Op != OpBotStateChanged || got.CID != 0 || len(got.Payload) != 3 {
		t.Fatalf("subscribed device got op %d n %d with %d elements", got.Op, got.CID, len(got.Payload))
	}
	noFrameOn(t, h.sink(co))
	noFrameOn(t, h.sink(cs))
}

// Once a resume has snapshotted a suspended connection's ring, no reply is held for it: a frame
// stamped after the snapshot would fall between the announced replay range and `resumed`.
func TestReplyIsNotHeldOnceAResumeTookItsSnapshot(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c := h.connect(t, device)
	token := h.readyResumeToken(t, c)
	h.gw.suspend(c)
	f := botReplyFrame(t, id.New(), 0, []byte{1}, true)
	if !c.hold(f) {
		t.Fatal("a suspended connection did not hold a reply")
	}
	if _, err := c.tryResumeSnapshot(token, h.generation, 1, h.gw.Generation()); err != nil {
		t.Fatal(err)
	}
	if c.hold(f) {
		t.Fatal("a reply was held after the resume's snapshot")
	}
	// Suspended again, it holds again; a live connection never takes the hold path.
	h.gw.suspend(c)
	if !c.hold(f) {
		t.Fatal("a re-suspended connection did not hold a reply")
	}
	live := h.connect(t, id.New())
	if live.hold(f) {
		t.Fatal("a live connection took the hold path")
	}
	if c.hold(Frame{Op: OpBotStateChanged}) {
		t.Fatal("a non-replayable frame was held")
	}
}

// ringFrames is the number of entries in a connection's ring, read without evicting.
func ringFrames(c *conn) int {
	frames, _, _, _ := c.ring.stats()
	return frames
}

// A reply for one device is never held in another device's suspended connection, even one of the
// same user (fix round 1, M7).
func TestReplyIsNotHeldForAnotherDevicesSuspendedConnection(t *testing.T) {
	h := newHarness(t)
	asking, other := id.New(), id.New()
	a := h.connect(t, asking)
	b := h.connect(t, other)
	h.gw.suspend(a)
	h.gw.suspend(b)
	before := ringFrames(b)
	h.gw.DeliverDevice(asking, botReplyFrame(t, id.New(), 0, []byte{1}, true))
	if got := ringFrames(b); got != before {
		t.Fatalf("another device's suspended ring went from %d to %d frames", before, got)
	}
	if got := ringFrames(a); got != before+1 {
		t.Fatalf("the asking device's suspended ring holds %d frames, want %d", got, before+1)
	}
}

// A connection CloseDevice dropped with 4004 holds nothing, whether it was live or already
// suspended when the revocation came (fix round 1, M2 and M7).
func TestReplyIsNotHeldAfterCloseDeviceRevokes(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	live := h.connect(t, device)
	parked := h.connect(t, device)
	h.gw.suspend(parked)
	h.gw.CloseDevice(device, CloseSessionRevoked, "session revoked")
	h.gw.suspend(live) // readLoop's deferred suspend after the close
	f := botReplyFrame(t, id.New(), 0, []byte{1}, true)
	liveBefore, parkedBefore := ringFrames(live), ringFrames(parked)
	h.gw.DeliverDevice(device, f)
	if ringFrames(live) != liveBefore || ringFrames(parked) != parkedBefore {
		t.Fatal("a revoked device's connection held a reply")
	}
	// Defence in depth: hold itself refuses a connection that may not resume.
	if live.hold(f) || parked.hold(f) {
		t.Fatal("hold accepted a connection closed with a non-resumable code")
	}
}

// A sealed connection (a self-revoking request's deferred close) never holds a reply (M7).
func TestReplyIsNotHeldForASealedConnection(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c := h.connect(t, device)
	// What CloseDevice does to a connection whose self-revoking request is still in flight.
	c.denyResume()
	c.sealed.Store(true)
	h.gw.removeLive(c)
	h.gw.suspend(c)
	before := ringFrames(c)
	f := botReplyFrame(t, id.New(), 0, []byte{1}, true)
	h.gw.DeliverDevice(device, f)
	if ringFrames(c) != before || c.hold(f) {
		t.Fatal("a sealed connection held a reply")
	}
}

// A resume refused at capability persistence leaves the connection parked, and it holds replies
// again (fix round 1, M3).
func TestReplyIsHeldAgainAfterAResumeRefusedAtCapabilityPersistence(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c := h.connect(t, device)
	token := h.readyResumeToken(t, c)
	h.gw.suspend(c)
	h.gw.opts.Auth = &capabilityRecordingAuth{Authenticator: h, err: errors.New("storage unavailable")}
	_, in := h.resumeFrom(t, c, token, 1)
	if in.Op != OpInvalidSession {
		t.Fatalf("op %d, want invalid_session", in.Op)
	}
	if reason, err := rawText(in.Payload[1]); err != nil || reason != "session capability persistence failed" {
		t.Fatalf("refusal reason %q (%v), want the capability persistence refusal", reason, err)
	}
	if _, parked := h.gw.suspended.Load(string(token)); !parked {
		t.Fatal("the refused resume took the connection out of the resume window")
	}
	before := ringFrames(c)
	h.gw.DeliverDevice(device, botReplyFrame(t, id.New(), 0, []byte{1}, true))
	if got := ringFrames(c); got != before+1 {
		t.Fatalf("after a refused resume the ring holds %d frames, want %d", got, before+1)
	}
}

// Op 62 has its own ring budget: replies never evict a message frame, and a reply past the budget
// is not ringed (fix round 1, M4).
func TestReplyRingBudgetNeverEvictsMessageFrames(t *testing.T) {
	h := newHarness(t)
	r := newRing(ringLimits{Frames: 4, Bytes: 4096, TTL: 10 * time.Minute}, h.clk)
	n := uint64(0)
	for range 4 {
		n++
		if !r.addOp(n, make([]byte, 1000), FrameScope{}, OpMessageCT) {
			t.Fatal("a message frame was refused")
		}
	}
	accepted := 0
	for range replyRingFrames + 4 {
		if r.addOp(n+1, make([]byte, 1000), FrameScope{}, OpBotReply) {
			n++
			accepted++
		}
	}
	if accepted != replyRingFrames {
		t.Fatalf("%d replies ringed, want the budget of %d", accepted, replyRingFrames)
	}
	if got := r.floor(); got != 1 {
		t.Fatalf("floor = %d, want 1: replies pushed a message frame out", got)
	}
	// The byte budget holds too.
	r2 := newRing(ringLimits{Frames: 64, Bytes: 1 << 20, TTL: 10 * time.Minute}, h.clk)
	if !r2.addOp(1, make([]byte, replyRingBytes-10), FrameScope{}, OpBotReply) {
		t.Fatal("a reply inside the byte budget was refused")
	}
	if r2.addOp(2, make([]byte, 11), FrameScope{}, OpBotReply) {
		t.Fatal("a reply past the byte budget was ringed")
	}
	// Once the client acknowledges, the budget frees.
	r2.trim(1)
	if !r2.addOp(2, make([]byte, 11), FrameScope{}, OpBotReply) {
		t.Fatal("the reply budget was not freed by trim")
	}
}

// On a live connection a reply past the ring's reply budget still goes out, unringed with n = 0,
// and the replay sequence keeps no hole.
func TestReplyPastTheRingBudgetGoesOutUnringed(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c := h.connect(t, device)
	h.drainReady(t, c)
	for i := range replyRingFrames + 1 {
		h.gw.DeliverDevice(device, botReplyFrame(t, id.New(), uint64(i), []byte{1}, false))
	}
	for i := range replyRingFrames {
		if in := h.waitFrame(t, device); in.Op != OpBotReply || in.CID != uint64(i+2) {
			t.Fatalf("reply %d: op %d n %d, want bot_reply n %d", i, in.Op, in.CID, i+2)
		}
	}
	if in := h.waitFrame(t, device); in.Op != OpBotReply || in.CID != 0 {
		t.Fatalf("the reply past the budget: op %d n %d, want bot_reply n 0", in.Op, in.CID)
	}
	// The next replayable frame continues the sequence.
	p, err := cborx.Marshal([]any{id.New(), id.New(), uint64(1), []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	h.gw.DeliverDevice(device, Frame{Op: OpInteraction, Payload: p, Replay: true})
	if in := h.waitFrame(t, device); in.CID != uint64(replyRingFrames+2) {
		t.Fatalf("the next replayable frame has n %d, want %d", in.CID, replyRingFrames+2)
	}
}

// Ops 62, 63 and 64 are server-to-client: a client that sends one is refused at the opcode check.
func TestBotRelayOpsAreNotClientFrames(t *testing.T) {
	for _, op := range []Op{OpBotReply, OpBotStateChanged, OpBotCommand} {
		frame, err := cborx.MarshalFrame(uint64(op), 1, nil, mustRawArray(t, opSpecs[op].elements))
		if err != nil {
			t.Fatal(err)
		}
		var fe *FrameError
		if _, err := Decode(frame, 16384); !errors.As(err, &fe) || fe.Code != "E_FRAME_TYPE" {
			t.Fatalf("client op %d: got %v, want E_FRAME_TYPE", op, err)
		}
	}
}

func mustRawArray(t *testing.T, n int) cbor.RawMessage {
	t.Helper()
	elems := make([]any, n)
	for i := range elems {
		elems[i] = uint64(0)
	}
	b, err := cborx.Marshal(elems)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

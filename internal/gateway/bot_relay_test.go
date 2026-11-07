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
	bot := h.connect(t, botDevice)
	h.drainReady(t, bot)
	bot.mu.Lock()
	bot.isBot = true
	bot.mu.Unlock()
	human := h.connect(t, humanDevice)
	h.drainReady(t, human)

	p, err := BotCommandPayload(id.New(), id.New(), id.New(), humanDevice, []byte{0x85})
	if err != nil {
		t.Fatal(err)
	}
	f := Frame{Op: OpBotCommand, Payload: p}
	if !h.gw.DeliverBot(botDevice, f) {
		t.Fatal("DeliverBot to a live Bot connection reported offline")
	}
	if in := h.waitFrame(t, botDevice); in.Op != OpBotCommand || in.CID != 0 {
		t.Fatalf("Bot got op %d n %d, want bot_command n 0", in.Op, in.CID)
	}
	// A device whose connections are not Bot runtime sessions is no Bot, and nothing is sent.
	if h.gw.DeliverBot(humanDevice, f) {
		t.Fatal("DeliverBot to a human connection reported delivered")
	}
	noFrameOn(t, h.sink(human))
	if h.gw.DeliverBot(id.New(), f) {
		t.Fatal("DeliverBot to a device with no connection reported delivered")
	}
	// A suspended Bot connection is offline.
	h.gw.suspend(bot)
	if h.gw.DeliverBot(botDevice, f) {
		t.Fatal("DeliverBot to a suspended Bot connection reported delivered")
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

package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// A slow consumer is closed 4008 and, on reconnect, resumes every frame it missed: 4008 is
// resumable precisely so the queue drop is not data loss.
func TestASlowConsumerIsClosedAndResumesEveryMissedFrame(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	group := id.New()
	c := h.connect(t, device)
	h.gw.SetGroupMembers(group, []id.ID{device})
	h.stall(c)
	t.Cleanup(func() { h.release(c) })

	for range 300 { // deeper than QueueFrames
		p, err := EpochChangedPayload(42, 4129)
		if err != nil {
			t.Fatalf("payload: %v", err)
		}
		h.gw.DeliverGroup(group, Frame{Op: OpMLSEpochChanged, GroupID: &group, Payload: p, Replay: true})
	}
	if code := h.waitClose(t, c); code != CloseRateLimited {
		t.Fatalf("close code = %d, want 4008", code)
	}
	if !code4008Resumable() {
		t.Fatal("4008 must be resumable")
	}

	out, err := c.tryResume(c.resume, h.generation, 0, h.generation)
	if err != nil {
		t.Fatalf("resume after a queue overflow must succeed: %v", err)
	}
	if len(out.frames) == 0 {
		t.Fatal("the ring replayed nothing; the queue drop became data loss")
	}
}

func code4008Resumable() bool { return CloseRateLimited.Resumable() }

func TestResumeIsRefusedForAStaleGenerationAnUnknownTokenAndAnNBelowTheFloor(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())
	for n := uint64(1); n <= 4; n++ {
		p, _ := EpochChangedPayload(1, n)
		c.send(Frame{Op: OpMLSEpochChanged, Payload: p, Replay: true})
	}
	c.ring.trim(3)

	if _, err := c.tryResume(c.resume, h.generation+1, 3, h.generation); err == nil {
		t.Error("a stale generation must be refused")
	}
	if _, err := c.tryResume([]byte("not the token"), h.generation, 3, h.generation); err == nil {
		t.Error("an unknown resume token must be refused")
	}
	if _, err := c.tryResume(c.resume, h.generation, 1, h.generation); err == nil {
		t.Error("an n below the ring floor must be refused")
	}
	if _, err := c.tryResume(c.resume, h.generation, 3, h.generation); err != nil {
		t.Errorf("an n at the floor must be accepted: %v", err)
	}
}

func TestResumeTokenRotatesOnEveryResumed(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())
	first := append([]byte(nil), c.resume...)
	if err := h.gw.rotateResume(c); err != nil {
		t.Fatalf("rotateResume: %v", err)
	}
	if string(first) == string(c.resume) {
		t.Fatal("the resume token must rotate on every resumed")
	}
}

func TestShutdownSendsReconnectThenClosesGoingAway(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())
	h.waitFrame(t, c.deviceID) // `ready`, which register sent
	if err := h.gw.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := h.waitFrame(t, c.deviceID); got.Op != OpReconnect {
		t.Fatalf("first frame on shutdown is op %d, want reconnect", got.Op)
	}
	if code := h.waitClose(t, c); code != CloseGoingAway {
		t.Fatalf("close code = %d, want 4010", code)
	}
}

func TestAMissedHeartbeatClosesFourZeroZeroNine(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())
	h.clk.Advance(30*time.Second*2 + 6*time.Second)
	h.gw.sweepLiveness()
	if code := h.waitClose(t, c); code != CloseSessionTimeout {
		t.Fatalf("close code = %d, want 4009", code)
	}
}

// --- the handshake, end to end over the sink -----------------------------------------------------

// serve's contract: hello first, then exactly one identify or resume. A heartbeat before identify
// is close 4001, which is the only thing CloseNotIdentified is for.
func TestAFrameBeforeIdentifyClosesFourZeroZeroOne(t *testing.T) {
	h := newHarness(t)
	s := newRecordingSink(8, false)
	go h.gw.serve(context.Background(), s, "bearer")

	if in := h.readFrom(t, s); in.Op != OpHello {
		t.Fatalf("first frame is op %d, want hello", in.Op)
	}
	beat, err := payload(uint64(0), uint64(1))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpHeartbeat, Payload: beat, Replay: true}, 1))
	if code := h.waitCloseOn(t, s); code != CloseNotIdentified {
		t.Fatalf("close code = %d, want 4001", code)
	}
}

// A version the instance never advertised is an error frame naming E_VERSION and close 4006.
func TestAnUnadvertisedVersionIsRefusedWithFourZeroZeroSix(t *testing.T) {
	h := newHarness(t)
	s := newRecordingSink(8, false)
	go h.gw.serve(context.Background(), s, "bearer")
	h.readFrom(t, s) // hello

	p, err := payload("bearer", uint64(1), uint64(99), uint64(1), uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1))

	in := h.readFrom(t, s)
	if in.Op != OpError {
		t.Fatalf("op %d, want error", in.Op)
	}
	code, err := rawText(in.Payload[1])
	if err != nil {
		t.Fatalf("error code: %v", err)
	}
	if code != "E_VERSION" {
		t.Fatalf("code = %s, want E_VERSION", code)
	}
	if got := h.waitCloseOn(t, s); got != CloseVersion {
		t.Fatalf("close code = %d, want 4006", got)
	}
}

// identify's happy path: hello, identify, ready — and the versions the client chose come back in
// ready's elements 4, 5 and 6.
func TestIdentifyIsAnsweredWithReadyCarryingTheNegotiatedVersions(t *testing.T) {
	h := newHarness(t)
	s := newRecordingSink(8, false)
	go h.gw.serve(context.Background(), s, "bearer")
	h.readFrom(t, s) // hello

	p, err := payload("bearer", uint64(1), uint64(1), uint64(1), uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1))

	in := h.readFrom(t, s)
	if in.Op != OpReady {
		t.Fatalf("op %d, want ready", in.Op)
	}
	for i, want := range map[int]uint64{4: 1, 5: 1, 6: 1} {
		got, err := rawUint(in.Payload[i])
		if err != nil {
			t.Fatalf("ready element %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("ready element %d = %d, want %d", i, got, want)
		}
	}
}

// The resume half, end to end: a suspended connection resumes over a new sink, gets `resumed`
// [from, to], is replayed the frames it missed, and its resume token is rotated — the sentence
// interfaces §6.1 states and nothing else in the plan proves.
func TestResumeReplaysTheMissedFramesAndRotatesTheToken(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())
	for n := uint64(1); n <= 3; n++ {
		p, _ := EpochChangedPayload(1, n)
		c.send(Frame{Op: OpMLSEpochChanged, Payload: p, Replay: true})
	}
	before := append([]byte(nil), c.resume...)
	h.suspend(c)

	s := newRecordingSink(16, false)
	go h.gw.serve(context.Background(), s, h.tokenFor(c.deviceID))
	h.readFrom(t, s) // hello

	// Element 0 is the session token, and it is resolved: a resume is not authenticated by the
	// resume token alone.
	p, err := payload(h.tokenFor(c.deviceID), before, h.generation, uint64(1))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpResume, Payload: p, Replay: true}, 2))

	in := h.readFrom(t, s)
	if in.Op != OpResumed {
		t.Fatalf("op %d, want resumed", in.Op)
	}
	from, err := rawUint(in.Payload[0])
	if err != nil {
		t.Fatalf("replayed_from: %v", err)
	}
	if from != 2 {
		t.Fatalf("replayed_from = %d, want 2", from)
	}
	for range 2 { // n = 2 and n = 3 are replayed
		if got := h.readFrom(t, s); got.Op != OpMLSEpochChanged {
			t.Fatalf("replayed op %d, want mls.epoch_changed", got.Op)
		}
	}
	// The rotation is written by serve's goroutine, so it is read through the accessor and not off
	// the field: `go test -race` is this package's gate.
	if string(before) == string(c.resumeToken()) {
		t.Fatal("the resume token must rotate on every resumed")
	}
}

// An unknown token is invalid_session with resumable = 0, then close 4001: the client falls back
// to identify.
func TestAnUnknownResumeTokenIsInvalidSession(t *testing.T) {
	h := newHarness(t)
	s := newRecordingSink(8, false)
	go h.gw.serve(context.Background(), s, "bearer")
	h.readFrom(t, s) // hello

	p, err := payload("bearer", make([]byte, 32), h.generation, uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpResume, Payload: p, Replay: true}, 2))

	in := h.readFrom(t, s)
	if in.Op != OpInvalidSession {
		t.Fatalf("op %d, want invalid_session", in.Op)
	}
	resumable, err := rawUint(in.Payload[0])
	if err != nil {
		t.Fatalf("resumable: %v", err)
	}
	if resumable != 0 {
		t.Fatalf("resumable = %d, want 0", resumable)
	}
}

// The resume window is swept against the injected clock, so it can be driven at all.
func TestASuspendedConnectionIsDroppedAfterTheResumeWindow(t *testing.T) {
	h := newHarness(t)
	c := h.connect(t, id.New())
	token := append([]byte(nil), c.resume...)
	h.suspend(c)
	if _, ok := h.gw.suspended.Load(string(token)); !ok {
		t.Fatal("a suspended connection must be resumable straight away")
	}
	h.clk.Advance(121 * time.Second) // ResumeWindow defaults to 120 s
	h.gw.sweepLiveness()
	if _, ok := h.gw.suspended.Load(string(token)); ok {
		t.Fatal("a suspended connection past the resume window must be dropped")
	}
}

// Invariant 7's acknowledgement reaches the delivery service through opcode 12.
func TestACommitAckFrameReachesTheDeliveryServiceHandler(t *testing.T) {
	h := newHarness(t)
	group := id.New()
	acked := make(chan uint64, 1)
	h.gw.opts.CommitAck = func(_ context.Context, g, _ id.ID, round uint64) {
		if g == group {
			acked <- round
		}
	}
	c := h.connect(t, id.New())
	h.setGroupsForDevice(c.deviceID, group)
	p, err := payload(uint64(7))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	// The frame goes through the real codec rather than being hand-built: `payload` returns the
	// whole one-element ARRAY, so Inbound.Payload[0] is only the round once Decode has split it,
	// and opcode 12's group-scope rule is exercised at the same time.
	in, err := Decode(mustEncode(t, Frame{Op: OpCommitAck, GroupID: &group, Payload: p, Replay: true}, 1), 16384)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	h.gw.handle(context.Background(), c, in)
	select {
	case round := <-acked:
		if round != 7 {
			t.Fatalf("round = %d, want 7", round)
		}
	case <-time.After(time.Second):
		t.Fatal("commit_ack never reached the handler; invariant 7's lost-round count is dead")
	}
}

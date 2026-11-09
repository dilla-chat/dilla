package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// Task 21b fix round 2: a Bot whose gateway identifies afresh after its session reset ("requires session
// reconciliation") gets the op 64 commands relayed around the reset. Each was answered 202 to its member, so
// none may be lost with the old connection's ring.

func carryCommand(t *testing.T) Frame {
	t.Helper()
	p, err := BotCommandPayload(id.New(), id.New(), id.New(), id.New(), []byte{0x85})
	if err != nil {
		t.Fatal(err)
	}
	return Frame{Op: OpBotCommand, Payload: p, Replay: true}
}

// finishSnapshot streams an empty cold-start snapshot on the newest identify and reads its two frames.
func (h *harness) finishSnapshot(t *testing.T, streams *botStreams, c *conn) {
	t.Helper()
	s := streams.next(t)
	if !s.Send(context.Background(), botEventFrame(t, BotEventSnapshotBegin, 0)) || !s.Finish(botEventFrame(t, BotEventSnapshotEnd, 0)) {
		t.Fatal("the new connection's snapshot was refused")
	}
	if in := h.readFrom(t, h.sink(c)); in.Op != OpBotEvent {
		t.Fatalf("op %d, want snapshot_begin", in.Op)
	}
	if in := h.readFrom(t, h.sink(c)); in.Op != OpBotEvent {
		t.Fatalf("op %d, want snapshot_end", in.Op)
	}
}

// A command held for a parked connection follows the device's fresh identify, after its snapshot.
func TestACommandHeldForAParkedBotFollowsItsFreshIdentify(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	first, _ := h.finishedBot(t, device)
	h.gw.suspend(first)
	if !h.gw.DeliverBot(device, carryCommand(t)) {
		t.Fatal("the command was not held for the parked connection")
	}
	streams := newBotStreams()
	second := h.connectBot(t, device, streams)
	if in := h.readFrom(t, h.sink(second)); in.Op != OpReady {
		t.Fatalf("op %d, want ready", in.Op)
	}
	h.finishSnapshot(t, streams, second)
	if in := h.readFrom(t, h.sink(second)); in.Op != OpBotCommand || in.CID == 0 {
		t.Fatalf("op %d n %d, want the carried bot_command, ringed", in.Op, in.CID)
	}
	noFrameOn(t, h.sink(second))
}

// A resume refused (the snapshot was not acknowledged) drops the connection: its held command, and one relayed
// before the Bot identifies again, both reach the next identify. A device that never had a Bot connection is still
// offline.
func TestCommandsAroundARefusedResumeReachTheNextIdentify(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	streams := newBotStreams()
	c := h.connectBot(t, device, streams)
	token := h.readyResumeToken(t, c)
	s := streams.next(t)
	if !s.Send(context.Background(), botEventFrame(t, BotEventSnapshotBegin, 0)) {
		t.Fatal("snapshot_begin refused")
	}
	if !h.gw.DeliverBot(device, carryCommand(t)) {
		t.Fatal("the command was not taken while the snapshot streams")
	}
	h.gw.suspend(c)
	if _, resumed := h.resumeFrom(t, c, token, 1); resumed.Op != OpInvalidSession {
		t.Fatalf("op %d, want invalid_session", resumed.Op)
	}
	if got := h.gw.bots.of(device); len(got) != 0 {
		t.Fatalf("%d Bot connections after the refused resume, want none", len(got))
	}
	if !h.gw.DeliverBot(device, carryCommand(t)) {
		t.Fatal("a command relayed while the Bot reconnects was refused as offline")
	}
	if h.gw.DeliverBot(id.New(), carryCommand(t)) {
		t.Fatal("a device with no Bot connection took a command")
	}
	if h.gw.DeliverBot(device, botEventFrame(t, BotEventMember, 1)) {
		t.Fatal("an op 65 was carried: the next snapshot supersedes it")
	}
	second := h.connectBot(t, device, streams)
	if in := h.readFrom(t, h.sink(second)); in.Op != OpReady {
		t.Fatalf("op %d, want ready", in.Op)
	}
	h.finishSnapshot(t, streams, second)
	for i := 0; i < 2; i++ {
		if in := h.readFrom(t, h.sink(second)); in.Op != OpBotCommand {
			t.Fatalf("frame %d is op %d, want a carried bot_command", i, in.Op)
		}
	}
	noFrameOn(t, h.sink(second))
}

// Past the resume window the device is offline again, and what was carried is forgotten.
func TestCarriedCommandsEndWithTheResumeWindow(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	streams := newBotStreams()
	c := h.connectBot(t, device, streams)
	h.readyResumeToken(t, c)
	streams.next(t)
	if !h.gw.DeliverBot(device, carryCommand(t)) {
		t.Fatal("the command was not taken")
	}
	h.gw.bots.drop(c)
	now := h.gw.opts.Clock.Now().Add(h.gw.opts.ResumeWindow + time.Second)
	if h.gw.bots.carryRelayed(device, carryCommand(t), now, h.gw.opts.ResumeWindow, h.gw.opts.RingTTL) {
		t.Fatal("a command was carried past the resume window")
	}
	h.gw.bots.sweepCarry(now, h.gw.opts.ResumeWindow)
	h.gw.bots.mu.Lock()
	_, kept := h.gw.bots.carry[device]
	h.gw.bots.mu.Unlock()
	if kept {
		t.Fatal("the sweep kept a gone device's carried commands")
	}
}

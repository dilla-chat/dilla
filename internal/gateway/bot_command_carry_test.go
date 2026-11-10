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

func carryCommandFrom(t *testing.T, sender id.ID) Frame {
	t.Helper()
	p, err := BotCommandPayload(id.New(), id.New(), id.New(), sender, []byte{0x85})
	if err != nil {
		t.Fatal(err)
	}
	return Frame{Op: OpBotCommand, Payload: p, Replay: true}
}

func carriedSenders(h *harness, device id.ID) map[id.ID]int {
	h.gw.bots.mu.Lock()
	defer h.gw.bots.mu.Unlock()
	out := map[id.ID]int{}
	if bc := h.gw.bots.carry[device]; bc != nil {
		for _, x := range bc.frames {
			out[x.sender]++
		}
	}
	return out
}

// Task 21b fix round 3, M1: one member's burst cannot evict another member's carried commands. Each sending device
// keeps at most botCarrySenderCap carried commands, its newest, inside the device total.
func TestOneSendersBurstDoesNotEvictAnothersCarriedCommands(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	streams := newBotStreams()
	c := h.connectBot(t, device, streams)
	h.readyResumeToken(t, c)
	streams.next(t)
	h.gw.bots.drop(c)
	quiet, loud := id.New(), id.New()
	for range 3 {
		if !h.gw.DeliverBot(device, carryCommandFrom(t, quiet)) {
			t.Fatal("a command relayed while the Bot reconnects was refused")
		}
	}
	for range botCarryCap + 10 {
		if !h.gw.DeliverBot(device, carryCommandFrom(t, loud)) {
			t.Fatal("a command relayed while the Bot reconnects was refused")
		}
	}
	got := carriedSenders(h, device)
	if got[quiet] != 3 || got[loud] != botCarrySenderCap {
		t.Fatalf("carried %d of the quiet member's and %d of the loud member's, want 3 and %d", got[quiet], got[loud], botCarrySenderCap)
	}
}

// Task 21b fix round 3, M3: a Bot connection closed as revoked (4004) carries nothing and opens no carry window: what it
// held is dropped, and a command relayed after it is offline.
func TestARevokedBotConnectionCarriesNothing(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c, _ := h.finishedBot(t, device)
	if !h.gw.DeliverBot(device, carryCommand(t)) {
		t.Fatal("the command was not taken")
	}
	h.gw.closeConn(c, CloseSessionRevoked, "session ended")
	h.gw.suspend(c)
	if h.gw.DeliverBot(device, carryCommand(t)) {
		t.Fatal("a command relayed after the revocation was carried")
	}
	if n := len(carriedSenders(h, device)); n != 0 {
		t.Fatalf("the revoked connection's commands were carried (%d senders)", n)
	}

	// A parked connection whose device is revoked is dropped the same way.
	h2 := newHarness(t)
	device2 := id.New()
	c2, _ := h2.finishedBot(t, device2)
	h2.gw.suspend(c2)
	if !h2.gw.DeliverBot(device2, carryCommand(t)) {
		t.Fatal("the command was not held for the parked connection")
	}
	h2.gw.CloseDevice(device2, CloseSessionRevoked, "device revoked")
	if got := h2.gw.bots.of(device2); len(got) != 0 {
		t.Fatalf("%d Bot connections after the revocation, want none", len(got))
	}
	if h2.gw.DeliverBot(device2, carryCommand(t)) {
		t.Fatal("a command relayed after the parked connection's revocation was carried")
	}
	if n := len(carriedSenders(h2, device2)); n != 0 {
		t.Fatal("the parked revoked connection's commands were carried")
	}
}

// A heartbeat timeout (4009) is not a revocation: the Bot reconnects, and what it held is carried.
func TestATimedOutBotConnectionStillCarries(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c, _ := h.finishedBot(t, device)
	if !h.gw.DeliverBot(device, carryCommand(t)) {
		t.Fatal("the command was not taken")
	}
	h.gw.closeConn(c, CloseSessionTimeout, "heartbeat overdue")
	h.gw.suspend(c)
	if !h.gw.DeliverBot(device, carryCommand(t)) {
		t.Fatal("a command relayed after a heartbeat timeout was refused")
	}
	if n := len(carriedSenders(h, device)); n != 2 {
		t.Fatalf("carried from %d senders, want 2", n)
	}
}

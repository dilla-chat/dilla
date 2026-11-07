package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Bot v2 Task 12: the gateway half of the Bot's event feed. Op 65 and op 64 are sequenced in the Bot
// connection's ring, so a resume from the Bot's last ack replays the gap in order; a cold-start snapshot
// is streamed unringed and bracketed by a ringed snapshot_end, and a Bot whose snapshot did not finish
// identifies again instead of resuming.

// botStreams records the snapshot streams OnBotIdentified hands out, one per Bot identify.
type botStreams struct {
	mu      sync.Mutex
	streams []*BotStream
	got     chan *BotStream
}

func newBotStreams() *botStreams { return &botStreams{got: make(chan *BotStream, 8)} }

func (b *botStreams) identified(s *BotStream) {
	b.mu.Lock()
	b.streams = append(b.streams, s)
	b.mu.Unlock()
	b.got <- s
}

func (b *botStreams) next(t *testing.T) *BotStream {
	t.Helper()
	select {
	case s := <-b.got:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("no Bot identify reached the feed")
		return nil
	}
}

// connectBot identifies a managed Bot on wire 6 and returns its connection; streams hears the identify.
func (h *harness) connectBot(t *testing.T, device id.ID, streams *botStreams) *conn {
	t.Helper()
	h.gw.opts.WireVersions = []uint64{6}
	h.gw.opts.Auth = &capabilityRecordingAuth{Authenticator: h}
	if streams != nil {
		h.gw.opts.OnBotIdentified = streams.identified
	}
	bot := auth.BotSessionBinding{InstallationID: id.New(), CommunityID: id.New(), State: store.BotInstallationReady}
	hash := make([]byte, 32)
	copy(hash, device[:])
	session := auth.Session{DeviceID: device, UserID: id.New(), Scope: auth.ScopeEnrolled, WireVersion: 6, TokenHash: hash, Bot: &bot}
	h.mu.Lock()
	h.tokens[h.tokenFor(device)] = session
	h.mu.Unlock()
	sink := newRecordingSink(1024, false)
	c, err := h.gw.registerVersions(context.Background(), session, sink, [3]uint64{6, 1, 1})
	if err != nil {
		t.Fatalf("register Bot: %v", err)
	}
	h.mu.Lock()
	h.sinks[c] = sink
	h.mu.Unlock()
	return c
}

func botEventFrame(t *testing.T, kind, marker uint64) Frame {
	t.Helper()
	p, err := BotEventPayload(kind, []any{marker})
	if err != nil {
		t.Fatalf("BotEventPayload: %v", err)
	}
	return Frame{Op: OpBotEvent, Payload: p, Replay: true}
}

// eventMarker reads the marker botEventFrame put in an op 65 payload.
func eventMarker(t *testing.T, in Inbound) uint64 {
	t.Helper()
	if in.Op != OpBotEvent || len(in.Payload) != 2 {
		t.Fatalf("op %d with %d elements, want bot_event", in.Op, len(in.Payload))
	}
	var body []uint64
	if err := cborx.Unmarshal(in.Payload[1], &body); err != nil || len(body) != 1 {
		t.Fatalf("bot_event body: %v", err)
	}
	return body[0]
}

// finishedBot is a Bot connection whose cold-start snapshot is complete (begin, then the ringed end),
// with the frames up to snapshot_end read; it returns the connection and its resume token.
func (h *harness) finishedBot(t *testing.T, device id.ID) (*conn, []byte) {
	t.Helper()
	streams := newBotStreams()
	c := h.connectBot(t, device, streams)
	token := h.readyResumeToken(t, c) // ready is n = 1
	s := streams.next(t)
	if !s.Send(context.Background(), botEventFrame(t, BotEventSnapshotBegin, 0)) {
		t.Fatal("snapshot_begin refused")
	}
	if !s.Finish(botEventFrame(t, BotEventSnapshotEnd, 0)) {
		t.Fatal("snapshot_end refused")
	}
	if in := h.waitFrame(t, device); in.Op != OpBotEvent || in.CID != 0 {
		t.Fatalf("snapshot_begin op %d n %d, want an unringed bot_event", in.Op, in.CID)
	}
	if in := h.waitFrame(t, device); in.Op != OpBotEvent || in.CID != 2 {
		t.Fatalf("snapshot_end op %d n %d, want bot_event n 2", in.Op, in.CID)
	}
	return c, token
}

// The "dropped socket resumes without losing a request" fault test, server half: the Bot connection
// drops after event 5 of 10 and resumes with ack 5; events 6 to 10 arrive in order, and an op 64
// command sent in the gap is replayed in its place.
func TestFeedResumesFromLastAckWithoutLoss(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c, token := h.finishedBot(t, device)
	var ack uint64
	for i := uint64(1); i <= 5; i++ {
		if !h.gw.DeliverBot(device, botEventFrame(t, BotEventMember, i)) {
			t.Fatalf("event %d not delivered", i)
		}
		in := h.waitFrame(t, device)
		if eventMarker(t, in) != i || in.CID == 0 {
			t.Fatalf("event %d arrived as marker %d n %d", i, eventMarker(t, in), in.CID)
		}
		ack = in.CID
	}
	h.gw.suspend(c)
	command, err := BotCommandPayload(id.New(), id.New(), id.New(), id.New(), []byte{0x85})
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(6); i <= 10; i++ {
		if !h.gw.DeliverBot(device, botEventFrame(t, BotEventMember, i)) {
			t.Fatalf("event %d was not held for the suspended Bot connection", i)
		}
		if i == 7 && !h.gw.DeliverBot(device, Frame{Op: OpBotCommand, Payload: command, Replay: true}) {
			t.Fatal("a command in the gap was not held for the suspended Bot connection")
		}
	}
	s, resumed := h.resumeFrom(t, c, token, ack)
	if resumed.Op != OpResumed {
		t.Fatalf("op %d, want resumed", resumed.Op)
	}
	want := []uint64{6, 7, 0, 8, 9, 10} // 0 is the command
	n := ack
	for _, marker := range want {
		in := h.readFrom(t, s)
		if in.CID != n+1 {
			t.Fatalf("replayed n %d, want %d", in.CID, n+1)
		}
		n = in.CID
		if marker == 0 {
			if in.Op != OpBotCommand {
				t.Fatalf("op %d where the command was, want bot_command", in.Op)
			}
			continue
		}
		if got := eventMarker(t, in); got != marker {
			t.Fatalf("replayed event %d, want %d", got, marker)
		}
	}
	// Live delivery resumes after the replay, past `resumed` (which takes the n after the replay range).
	if !h.gw.DeliverBot(device, botEventFrame(t, BotEventMember, 11)) {
		t.Fatal("event 11 not delivered after resume")
	}
	if in := h.readFrom(t, s); eventMarker(t, in) != 11 || in.CID != n+2 {
		t.Fatalf("event after resume: marker %d n %d, want 11 n %d", eventMarker(t, in), in.CID, n+2)
	}
}

// A cold start's body frames are unringed and bracketed by snapshot_begin and the ringed snapshot_end;
// a live frame delivered while the snapshot streams waits until after snapshot_end.
func TestColdStartSnapshotIsBracketedOnTheWire(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	streams := newBotStreams()
	c := h.connectBot(t, device, streams)
	h.readyResumeToken(t, c)
	s := streams.next(t)
	if s.Device() != device || s.Installation().IsZero() || s.Community().IsZero() {
		t.Fatal("the stream does not name the Bot's device, installation and community")
	}
	if !s.Send(context.Background(), botEventFrame(t, BotEventSnapshotBegin, 0)) ||
		!s.Send(context.Background(), botEventFrame(t, BotEventDeviceList, 1)) {
		t.Fatal("snapshot frames refused")
	}
	// A command arrives mid-snapshot: it is held until the snapshot ends.
	command, err := BotCommandPayload(id.New(), id.New(), id.New(), id.New(), []byte{0x85})
	if err != nil {
		t.Fatal(err)
	}
	if !h.gw.DeliverBot(device, Frame{Op: OpBotCommand, Payload: command, Replay: true}) {
		t.Fatal("a command during the snapshot was refused")
	}
	if !s.Send(context.Background(), botEventFrame(t, BotEventMember, 2)) || !s.Finish(botEventFrame(t, BotEventSnapshotEnd, 0)) {
		t.Fatal("snapshot frames refused")
	}
	for i, want := range []struct {
		op Op
		n  uint64
	}{{OpBotEvent, 0}, {OpBotEvent, 0}, {OpBotEvent, 0}, {OpBotEvent, 2}, {OpBotCommand, 3}} {
		in := h.waitFrame(t, device)
		if in.Op != want.op || in.CID != want.n {
			t.Fatalf("frame %d: op %d n %d, want op %d n %d", i, in.Op, in.CID, want.op, want.n)
		}
	}
	if s.Send(context.Background(), botEventFrame(t, BotEventMember, 3)) {
		t.Fatal("a finished snapshot accepted another body frame")
	}
}

// A Bot whose connection dropped before snapshot_end cannot resume: its next connection identifies,
// and that identify streams the snapshot again.
func TestBotResumeBeforeSnapshotEndIsRefused(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	streams := newBotStreams()
	c := h.connectBot(t, device, streams)
	token := h.readyResumeToken(t, c)
	s := streams.next(t)
	if !s.Send(context.Background(), botEventFrame(t, BotEventSnapshotBegin, 0)) {
		t.Fatal("snapshot_begin refused")
	}
	h.gw.suspend(c)
	if s.Send(context.Background(), botEventFrame(t, BotEventDeviceList, 1)) || s.Finish(botEventFrame(t, BotEventSnapshotEnd, 0)) {
		t.Fatal("a suspended connection's snapshot accepted a frame")
	}
	_, resumed := h.resumeFrom(t, c, token, 1)
	if resumed.Op != OpInvalidSession {
		t.Fatalf("op %d, want invalid_session for a resume before snapshot_end", resumed.Op)
	}
	if _, parked := h.gw.suspended.Load(string(token)); parked {
		t.Fatal("the refused connection stayed in the resume window")
	}
}

// A resume whose ack is below snapshot_end missed part of the snapshot, so it is refused too.
func TestBotResumeBelowSnapshotEndIsRefused(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c, token := h.finishedBot(t, device)
	h.gw.suspend(c)
	_, resumed := h.resumeFrom(t, c, token, 1) // snapshot_end is n = 2
	if resumed.Op != OpInvalidSession {
		t.Fatalf("op %d, want invalid_session for an ack below snapshot_end", resumed.Op)
	}
}

// Only the Bot's connection is ever fed: a human connection of the same device gets no op 64 or 65,
// and a device with no Bot connection is offline.
func TestFeedReachesOnlyTheBotConnection(t *testing.T) {
	h := newHarness(t)
	botDevice, humanDevice := id.New(), id.New()
	h.finishedBot(t, botDevice)
	human := h.connect(t, humanDevice)
	h.drainReady(t, human)
	f := botEventFrame(t, BotEventMember, 1)
	if h.gw.DeliverBot(humanDevice, f) {
		t.Fatal("DeliverBot to a human connection reported delivered")
	}
	noFrameOn(t, h.sink(human))
	if h.gw.DeliverBot(id.New(), f) {
		t.Fatal("DeliverBot to a device with no connection reported delivered")
	}
}

// The Bot connection's ring and queue are its own; every user connection keeps the configured ones.
func TestUserConnectionRingIsUnchanged(t *testing.T) {
	h := newHarness(t)
	human := h.connect(t, id.New())
	bot := h.connectBot(t, id.New(), nil)
	o := h.gw.opts
	if human.ring.limits != (ringLimits{Frames: o.RingFrames, Bytes: o.RingBytes, TTL: o.RingTTL}) {
		t.Fatalf("user ring %+v, want the configured ring", human.ring.limits)
	}
	if human.writer.limits.Frames != o.QueueFrames || human.writer.limits.Bytes != o.QueueBytes {
		t.Fatalf("user queue %+v, want the configured queue", human.writer.limits)
	}
	if bot.ring.limits.Frames != o.BotRingFrames || bot.ring.limits.Bytes != o.BotRingBytes {
		t.Fatalf("Bot ring %+v, want the Bot budget", bot.ring.limits)
	}
	if bot.ring.limits.Bytes < o.BotQueueBytes || bot.ring.limits.Frames < o.BotQueueFrames {
		t.Fatal("the Bot ring is smaller than its queue")
	}
}

// --- Fix round 1 ---------------------------------------------------------------------------------

// I-2: a live op 65 computed before a snapshot is superseded by it. Delivered while the snapshot is
// owed, it is dropped (and still reported taken, so the feed does not go offline); never sent after
// snapshot_end, where it would overwrite the newer state. An op 64 command still waits and follows.
func TestDeliverBotDropsAStaleEventWhileTheSnapshotIsOwed(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	streams := newBotStreams()
	c := h.connectBot(t, device, streams)
	h.readyResumeToken(t, c)
	s := streams.next(t)
	if !s.Send(context.Background(), botEventFrame(t, BotEventSnapshotBegin, 0)) {
		t.Fatal("snapshot_begin refused")
	}
	if !h.gw.DeliverBot(device, botEventFrame(t, BotEventMember, 99)) {
		t.Fatal("a stale live event during the snapshot was reported not taken")
	}
	command, err := BotCommandPayload(id.New(), id.New(), id.New(), id.New(), []byte{0x85})
	if err != nil {
		t.Fatal(err)
	}
	if !h.gw.DeliverBot(device, Frame{Op: OpBotCommand, Payload: command, Replay: true}) {
		t.Fatal("a command during the snapshot was refused")
	}
	if !s.Finish(botEventFrame(t, BotEventSnapshotEnd, 0)) {
		t.Fatal("snapshot_end refused")
	}
	for i, want := range []struct {
		op Op
		n  uint64
	}{{OpBotEvent, 0}, {OpBotEvent, 2}, {OpBotCommand, 3}} {
		in := h.waitFrame(t, device)
		if in.Op != want.op || in.CID != want.n {
			t.Fatalf("frame %d: op %d n %d, want op %d n %d", i, in.Op, in.CID, want.op, want.n)
		}
	}
	noFrameOn(t, h.sink(c))
	// After snapshot_end, live events flow again.
	if !h.gw.DeliverBot(device, botEventFrame(t, BotEventMember, 100)) {
		t.Fatal("a live event after the snapshot was not taken")
	}
	if in := h.waitFrame(t, device); eventMarker(t, in) != 100 || in.CID != 4 {
		t.Fatalf("live event after the snapshot: marker %d n %d, want 100 n 4", eventMarker(t, in), in.CID)
	}
}

// I-3: a Bot resume whose unacknowledged gap aged out of the ring is refused, so the Bot identifies
// and gets a fresh snapshot instead of an empty replay that silently lost events.
func TestBotRingRefusesAResumeWhoseGapAgedOut(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c, token := h.finishedBot(t, device) // snapshot_end is n = 2
	if !h.gw.DeliverBot(device, botEventFrame(t, BotEventMember, 1)) {
		t.Fatal("event not delivered")
	}
	h.waitFrame(t, device) // n = 3, never acknowledged
	h.gw.suspend(c)
	h.clk.Advance(h.gw.opts.RingTTL + time.Second)
	_, resumed := h.resumeFrom(t, c, token, 2)
	if resumed.Op != OpInvalidSession {
		t.Fatalf("op %d, want invalid_session for a resume whose gap aged out", resumed.Op)
	}
	// The Bot's identify streams a new snapshot: nothing is lost.
	streams := newBotStreams()
	h.connectBot(t, device, streams)
	streams.next(t)
}

// The control for I-3: everything that aged out was acknowledged, so the resume goes ahead.
func TestBotRingResumesWhenOnlyAcknowledgedFramesAgedOut(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c, token := h.finishedBot(t, device)
	h.gw.suspend(c)
	h.clk.Advance(h.gw.opts.RingTTL + time.Second)
	if _, resumed := h.resumeFrom(t, c, token, 2); resumed.Op != OpResumed {
		t.Fatalf("op %d, want resumed: nothing unacknowledged aged out", resumed.Op)
	}
}

// DeliverBot to a parked Bot connection rings the frame for its resume, in order with the rest.
func TestDeliverBotHoldsForAParkedConnection(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c, token := h.finishedBot(t, device)
	h.gw.suspend(c)
	for i := uint64(1); i <= 3; i++ {
		if !h.gw.DeliverBot(device, botEventFrame(t, BotEventMember, i)) {
			t.Fatalf("event %d was not held for the parked connection", i)
		}
	}
	s, resumed := h.resumeFrom(t, c, token, 2)
	if resumed.Op != OpResumed {
		t.Fatalf("op %d, want resumed", resumed.Op)
	}
	for i := uint64(1); i <= 3; i++ {
		if in := h.readFrom(t, s); eventMarker(t, in) != i || in.CID != 2+i {
			t.Fatalf("replayed marker %d n %d, want %d n %d", eventMarker(t, in), in.CID, i, 2+i)
		}
	}
}

// DeliverBot while a resume is in progress (its ring snapshot taken, the connection not yet live)
// holds the frame and sends it right after the replay, on the next n: it is neither lost nor sent
// ahead of `resumed`.
func TestDeliverBotDuringAResumeFollowsTheReplay(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c, token := h.finishedBot(t, device)
	h.gw.suspend(c)
	if !h.gw.DeliverBot(device, botEventFrame(t, BotEventMember, 1)) {
		t.Fatal("event 1 not held")
	}
	h.gw.resumedPayloadFn = func(from, to uint64, resumeToken []byte) (cbor.RawMessage, error) {
		if !h.gw.DeliverBot(device, botEventFrame(t, BotEventMember, 2)) {
			t.Error("event 2 sent mid-resume was not taken")
		}
		return ResumedPayload(from, to, resumeToken)
	}
	s, resumed := h.resumeFrom(t, c, token, 2)
	if resumed.Op != OpResumed || resumed.CID != 4 {
		t.Fatalf("op %d n %d, want resumed n 4", resumed.Op, resumed.CID)
	}
	if in := h.readFrom(t, s); eventMarker(t, in) != 1 || in.CID != 3 {
		t.Fatalf("replay: marker %d n %d, want 1 n 3", eventMarker(t, in), in.CID)
	}
	if in := h.readFrom(t, s); eventMarker(t, in) != 2 || in.CID != 5 {
		t.Fatalf("mid-resume event: marker %d n %d, want 2 n 5", eventMarker(t, in), in.CID)
	}
}

// M-1: what a Bot connection holds during its snapshot is capped at half its writer queue, so the
// flush after snapshot_end cannot overflow the queue and send the Bot into a snapshot loop. Past the
// cap the snapshot ends the connection instead, and the Bot identifies again.
func TestBotPendingIsCappedAtHalfTheQueue(t *testing.T) {
	for _, over := range []bool{false, true} {
		h := newHarness(t)
		h.gw.opts.BotQueueFrames = 8
		device := id.New()
		streams := newBotStreams()
		c := h.connectBot(t, device, streams)
		h.readyResumeToken(t, c)
		s := streams.next(t)
		command, err := BotCommandPayload(id.New(), id.New(), id.New(), id.New(), []byte{0x85})
		if err != nil {
			t.Fatal(err)
		}
		n := 4
		if over {
			n = 5
		}
		taken := 0
		for range n {
			if h.gw.DeliverBot(device, Frame{Op: OpBotCommand, Payload: command, Replay: true}) {
				taken++
			}
		}
		if taken != 4 {
			t.Fatalf("over %v: %d commands held, want 4 (half the 8-frame queue)", over, taken)
		}
		if ok := s.Finish(botEventFrame(t, BotEventSnapshotEnd, 0)); ok == over {
			t.Fatalf("over %v: Finish = %v", over, ok)
		}
		if over {
			if code := h.waitClose(t, c); code != CloseUnknown {
				t.Fatalf("close %d, want 4000 after the overflow", code)
			}
		}
	}
}

// staticLaunch admits no peer: the gateway is serving with a supervised Bot, so the public path must
// not admit a managed-Bot session.
type staticLaunch struct{}

func (staticLaunch) Peer(int32, uint32) bool  { return false }
func (staticLaunch) Spend(int32, []byte) bool { return false }
func (staticLaunch) Authenticated(int32) bool { return false }

// M-5: with a supervised Bot (BotLaunch set), a managed-Bot session identifying or resuming on the
// public gateway, without the Bot socket's peer, is refused. Without a supervisor (dev, testhost) it
// is admitted as before.
func TestManagedBotIsRefusedOnThePublicGatewayWhenSupervised(t *testing.T) {
	for _, supervised := range []bool{true, false} {
		h := newHarness(t)
		h.gw.opts.WireVersions = []uint64{6}
		h.gw.opts.Auth = &capabilityRecordingAuth{Authenticator: h}
		if supervised {
			h.gw.opts.BotLaunch = staticLaunch{}
		}
		device := id.New()
		bot := auth.BotSessionBinding{InstallationID: id.New(), CommunityID: id.New(), State: store.BotInstallationReady}
		token := scopedSession(h, device, auth.Session{Scope: auth.ScopeEnrolled, WireVersion: 6, TokenHash: []byte("01234567890123456789012345678901"), Bot: &bot})
		sink := newRecordingSink(16, false)
		_, ok := h.gw.identifyConnection(context.Background(), sink, identifyInbound(t, token, 6))
		if ok == supervised {
			t.Fatalf("supervised %v: public identify admitted = %v", supervised, ok)
		}
		if supervised {
			if in := h.readFrom(t, sink); in.Op != OpError {
				t.Fatalf("op %d, want an error frame", in.Op)
			}
		}
	}
}

func TestManagedBotResumeIsRefusedOnThePublicGatewayWhenSupervised(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	c, token := h.finishedBot(t, device)
	h.gw.suspend(c)
	h.gw.opts.BotLaunch = staticLaunch{}
	if _, resumed := h.resumeFrom(t, c, token, 2); resumed.Op != OpInvalidSession {
		t.Fatalf("op %d, want invalid_session for a public resume of a supervised Bot", resumed.Op)
	}
}

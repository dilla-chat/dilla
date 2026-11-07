package gateway

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// The Bot runtime connection's feed (Bot v2 task 12). Op 64 bot_command and op 65 bot_event reach
// only the Bot's connection, through DeliverBot, and both are sequenced in that connection's ring:
// a Bot whose socket dropped resumes from its last ack and gets the gap replayed in order.
//
// The cold-start snapshot is the one exception. Its body frames can be many megabytes (a device-list
// history per member), more than any ring should hold, so they go out with n = 0 and are never ringed;
// only the closing snapshot_end is. A Bot connection whose snapshot is owed may resume only from an
// ack at or above that snapshot_end. A drop before it means the Bot identifies again, and the new
// identify streams a new snapshot from the current state: the restart is the snapshot's recovery.
// While the snapshot streams, the connection's live op 64 and 65 frames wait (botPending) and follow
// snapshot_end in arrival order, so no live event is applied before the state it changes.

// botPendingCap bounds the frames one Bot connection holds while its snapshot or a resume is in
// progress: half its writer queue (fix round 1 M-1), so the flush after snapshot_end, into a queue
// Finish first lets drain below half, cannot overflow it. Past it the connection is not fed again:
// Finish fails and the Bot identifies afresh.
func (c *conn) botPendingCap() int {
	return max(1, c.gw.opts.BotQueueFrames/2)
}

// botIndex is the Bot runtime connections by device. A connection stays in it from its identify until
// it can never be fed again (a non-resumable close, the resume window elapsing, a refused resume), so
// DeliverBot reaches it while it is live, parked in the resume window, or mid-resume.
type botIndex struct {
	mu       sync.Mutex
	byDevice map[id.ID][]*conn
}

// add indexes c as its device's one Bot runtime connection. A device has at most one (Bot v2 task 14, fix round 2,
// N-2): the newest identify replaces every older connection of the device, live, parked in the resume window or
// mid-resume. Each replaced one is never fed again, loses its resume token and is closed 4004, which is not
// resumable. Each live connection streams its own snapshot, so the replaced ones would otherwise multiply that cost; the
// supervised Bot keeps one gateway per installation, and an identify reaching ready means it moved to the new one.
func (b *botIndex) add(c *conn) {
	b.mu.Lock()
	if b.byDevice == nil {
		b.byDevice = map[id.ID][]*conn{}
	}
	replaced := b.byDevice[c.deviceID]
	b.byDevice[c.deviceID] = []*conn{c}
	b.mu.Unlock()
	for _, old := range replaced {
		if old == c {
			continue
		}
		old.mu.Lock()
		old.botDead = true
		old.botPending = nil
		old.noResume = true
		token := string(old.resume)
		w := old.writer
		old.mu.Unlock()
		old.gw.suspended.CompareAndDelete(token, old)
		if w != nil {
			w.sink.close(CloseSessionRevoked, "replaced by a newer Bot connection")
		}
	}
}

// drop marks the connection as never fed again and forgets it, with whatever it was holding.
func (b *botIndex) drop(c *conn) {
	if !c.isBot {
		return
	}
	c.mu.Lock()
	c.botDead = true
	c.botPending = nil
	c.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	list := slices.DeleteFunc(b.byDevice[c.deviceID], func(x *conn) bool { return x == c })
	if len(list) == 0 {
		delete(b.byDevice, c.deviceID)
		return
	}
	b.byDevice[c.deviceID] = list
}

func (b *botIndex) of(device id.ID) []*conn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.byDevice[device])
}

// ringLimitsFor and writerLimitsFor are a connection's replay ring and writer queue: the Bot runtime
// connection's own budget, or the one every user connection keeps.
func (g *Gateway) ringLimitsFor(bot bool) ringLimits {
	if bot {
		return ringLimits{Frames: g.opts.BotRingFrames, Bytes: g.opts.BotRingBytes, TTL: g.opts.RingTTL}
	}
	return ringLimits{Frames: g.opts.RingFrames, Bytes: g.opts.RingBytes, TTL: g.opts.RingTTL}
}

func (g *Gateway) writerLimitsFor(bot bool) writerLimits {
	if bot {
		return writerLimits{Frames: g.opts.BotQueueFrames, Bytes: g.opts.BotQueueBytes, Deadline: g.opts.WriteDeadline}
	}
	return writerLimits{Frames: g.opts.QueueFrames, Bytes: g.opts.QueueBytes, Deadline: g.opts.WriteDeadline}
}

// deliverBot takes one op 64 or 65 frame for this Bot connection, always ringed: sent now when the
// connection is live, held in botPending while a snapshot or resume is in progress, or rung for the
// resume while it is parked in the resume window. It reports whether the frame was taken.
func (c *conn) deliverBot(f Frame) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.isBot || c.botDead {
		return false
	}
	f.Replay = true
	// While the cold-start snapshot is owed, a live op 65 predates the snapshot's reads (the feed's
	// one worker streams the snapshot before any later live work), so the snapshot supersedes it.
	// Flushed after snapshot_end it would overwrite the newer state (fix round 1 I-2). It is dropped,
	// and reported taken so the feed stays online. Op 64 commands still wait and follow.
	if f.Op == OpBotEvent && c.botSnapshot && c.botSnapshotEnd == 0 {
		return true
	}
	if c.botHolding {
		if len(c.botPending) >= c.botPendingCap() {
			c.botOverflow = true
			return false
		}
		c.botPending = append(c.botPending, f)
		return true
	}
	switch c.state {
	case stateReady:
		c.sendLocked(f)
		return true
	case stateClosed:
		if c.noResume {
			return false
		}
		return c.holdLocked(f)
	default:
		return false
	}
}

// botSnapshotAcked reports whether a resume from lastN may go ahead: always for a user connection or
// a Bot connection that owes no snapshot, and otherwise only once the Bot acknowledged snapshot_end.
func (c *conn) botSnapshotAcked(lastN uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.isBot || !c.botSnapshot {
		return true
	}
	return c.botSnapshotEnd != 0 && lastN >= c.botSnapshotEnd
}

// flushBotPending sends the frames that waited during a resume, after the replay, and lets the next
// ones through. It reports false when more waited than botPendingCap: the Bot missed frames, and the
// caller ends the connection.
func (c *conn) flushBotPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.isBot {
		return true
	}
	pending, overflow := c.botPending, c.botOverflow
	c.botPending, c.botHolding, c.botOverflow = nil, false, false
	if overflow {
		return false
	}
	for _, f := range pending {
		c.sendLocked(f)
	}
	return true
}

// parkBotPending rings the frames that waited during a resume that was refused but left the
// connection parked, so its next resume replays them. False is an overflow, as for flushBotPending.
func (c *conn) parkBotPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.isBot {
		return true
	}
	pending, overflow := c.botPending, c.botOverflow
	c.botPending, c.botHolding, c.botOverflow = nil, false, false
	if overflow {
		return false
	}
	for _, f := range pending {
		c.holdLocked(f)
	}
	return true
}

// BotStream is the cold-start snapshot's path onto one Bot connection (Options.OnBotIdentified): the
// connection that identified, and no other. Send streams an unringed body frame, Finish sends the
// ringed snapshot_end and releases the live frames that waited, and Abort ends the connection when the
// snapshot cannot be built. Every call reports false once the connection is gone or the snapshot is
// over; the producer then stops.
type BotStream struct{ c *conn }

// Installation is the Bot connection's installation.
func (s *BotStream) Installation() id.ID { return s.c.botInstallation }

// Community is the installation's community.
func (s *BotStream) Community() id.ID { return s.c.botCommunity }

// Device is the Bot's managed device.
func (s *BotStream) Device() id.ID { return s.c.deviceID }

// User is the Bot's user.
func (s *BotStream) User() id.ID { return s.c.userID }

// streamOpenLocked is whether the snapshot may still send: the connection is live and fed, and its
// snapshot_end is not sent yet.
func (c *conn) streamOpenLocked() bool {
	return c.state == stateReady && !c.botDead && c.botSnapshot && c.botSnapshotEnd == 0
}

// Send queues one unringed snapshot frame. It waits while the connection's writer queue is half full,
// so a snapshot of any size never overflows it; ctx bounds the wait.
func (s *BotStream) Send(ctx context.Context, f Frame) bool {
	c := s.c
	c.mu.Lock()
	w, open := c.writer, c.streamOpenLocked()
	c.mu.Unlock()
	if !open || w == nil || !w.waitRoom(ctx, len(f.Payload)+frameOverhead) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.streamOpenLocked() || c.writer != w {
		return false
	}
	f.Replay = false
	c.sendLocked(f)
	return true
}

// Finish sends snapshot_end, ringed, records its n as the lowest ack a resume may name, and sends the
// live frames that waited during the snapshot after it, in arrival order. When more waited than
// botPendingCap, the connection is ended instead and the Bot identifies again.
func (s *BotStream) Finish(f Frame) bool {
	c := s.c
	// Let the snapshot's tail drain below half the queue first, so snapshot_end and the held frames
	// (at most half the queue) fit (fix round 1 M-1). The wait is bounded by the write deadline; past it
	// the flush goes ahead, and an overflow closes resumably as for any slow consumer.
	c.mu.Lock()
	w := c.writer
	c.mu.Unlock()
	if w != nil {
		ctx, cancel := context.WithTimeout(context.Background(), c.gw.opts.WriteDeadline)
		w.waitRoom(ctx, w.limits.Bytes/4)
		cancel()
	}
	c.mu.Lock()
	if !c.streamOpenLocked() {
		c.mu.Unlock()
		return false
	}
	if c.botOverflow {
		c.mu.Unlock()
		s.Abort("bot feed overflow during the snapshot")
		return false
	}
	f.Replay = true
	c.sendLocked(f)
	c.botSnapshotEnd = c.n
	pending := c.botPending
	c.botPending, c.botHolding = nil, false
	for _, p := range pending {
		c.sendLocked(p)
	}
	c.mu.Unlock()
	return true
}

// Abort ends the connection without a resume: its next connection identifies and is sent a new
// snapshot.
func (s *BotStream) Abort(reason string) {
	c := s.c
	c.denyResume()
	c.gw.bots.drop(c)
	c.gw.closeConn(c, CloseUnknown, reason)
}

// waitRoom blocks until the queue has room for size more bytes with half its budget to spare, so a
// producer that streams many frames paces itself to the socket instead of overflowing the queue. It
// reports false when the writer stopped or ctx ended. The one-millisecond poll is real time for
// drain's reason: a clock.Fake never fires on its own.
func (w *writer) waitRoom(ctx context.Context, size int) bool {
	for {
		w.mu.Lock()
		room := w.queuedBytes+size <= w.limits.Bytes/2 || w.queuedBytes == 0
		w.mu.Unlock()
		if room && len(w.queue) < cap(w.queue)/2 {
			return true
		}
		t := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-w.done:
			t.Stop()
			return false
		case <-t.C:
		}
	}
}

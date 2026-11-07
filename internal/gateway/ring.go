package gateway

import (
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
)

type ringLimits struct {
	Frames int
	Bytes  int
	TTL    time.Duration
}

type ringEntry struct {
	n     uint64
	bytes []byte
	at    time.Time
	// expires is when the entry may be dropped: at + the ring's TTL, or at + BotReplyHold for an
	// op 62 reply when that is sooner.
	expires time.Time
	scope   FrameScope
	// reply marks an op 62 entry, which counts against the reply budget and not the ring's own.
	reply bool
}

// The op 62 reply budget of one ring (Bot v2 task 10): Bot replies are counted apart from every
// other frame, so held or unacknowledged replies never push a message frame out of the ring. A
// reply that does not fit is not ringed (ring.addOp reports false). The byte bound counts whole
// encoded frames, so it is the per-request sealed cap (api.BotReplyBytesPerRequest, 256 KiB) plus
// 1 KiB of framing per frame: one request's full allowance always fits.
const (
	replyRingFrames = 16
	replyRingBytes  = 256<<10 + replyRingFrames<<10
)

// ring is a per-connection replay buffer, bounded on count, bytes and age. It is an optimisation:
// the per-group seq cursors are the truth, and a client whose n is below the floor heals from
// ready's digest plus GET /handshakes?from= and GET /messages?from=.
type ring struct {
	mu      sync.Mutex
	limits  ringLimits
	clk     clock.Clock
	entries []ringEntry
	// bytes and the frame count the limits bound are those of the non-reply entries; replyFrames
	// and replyBytes are the op 62 entries', bounded by the reply budget.
	bytes       int
	replyFrames int
	replyBytes  int
}

func newRing(l ringLimits, clk clock.Clock) *ring {
	return &ring{limits: l, clk: clk}
}

func (r *ring) add(n uint64, frame []byte, scope FrameScope) {
	r.addOp(n, frame, scope, 0)
}

// addOp is add for a frame of opcode op, and reports whether the frame was ringed. Every opcode
// keeps the ring's TTL except op 62 bot_reply, whose entry expires after BotReplyHold when that is
// sooner (Bot v2 task 10: an undelivered reply is dropped after two minutes). Eviction stays
// head-first, so the replay range stays contiguous: with the default TTL (also two minutes) the two
// bounds coincide. An op 62 entry counts against the reply budget only, and one that would exceed
// it is not added: the caller sends it unringed (n = 0) or not at all, and nothing else is evicted
// for it.
func (r *ring) addOp(n uint64, frame []byte, scope FrameScope, op Op) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clk.Now()
	ttl := r.limits.TTL
	reply := op == OpBotReply
	if reply {
		r.evictLocked()
		if r.replyFrames+1 > replyRingFrames || r.replyBytes+len(frame) > replyRingBytes {
			return false
		}
		if BotReplyHold < ttl {
			ttl = BotReplyHold
		}
	}
	r.entries = append(r.entries, ringEntry{n: n, bytes: frame, at: now, expires: now.Add(ttl), scope: scope.clone(), reply: reply})
	if reply {
		r.replyFrames++
		r.replyBytes += len(frame)
	} else {
		r.bytes += len(frame)
	}
	r.evictLocked()
	return true
}

// dropHeadLocked removes the oldest entry from whichever budget it counts against.
func (r *ring) dropHeadLocked() {
	head := r.entries[0]
	if head.reply {
		r.replyFrames--
		r.replyBytes -= len(head.bytes)
	} else {
		r.bytes -= len(head.bytes)
	}
	r.entries = r.entries[1:]
}

// evictLocked drops the oldest entries until all three bounds hold. The frame and byte bounds are
// over the non-reply entries; an op 62 entry at the head goes with them, being older.
func (r *ring) evictLocked() {
	now := r.clk.Now()
	for len(r.entries) > 0 {
		head := r.entries[0]
		tooMany := len(r.entries)-r.replyFrames > r.limits.Frames
		tooBig := r.bytes > r.limits.Bytes
		tooOld := now.After(head.expires)
		if !tooMany && !tooBig && !tooOld {
			return
		}
		r.dropHeadLocked()
	}
}

// trim drops everything the client has acknowledged with a heartbeat's last_n.
func (r *ring) trim(lastN uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.entries) > 0 && r.entries[0].n <= lastN {
		r.dropHeadLocked()
	}
}

// floor is the lowest n still replayable, or 0 when the ring is empty.
func (r *ring) floor() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked()
	if len(r.entries) == 0 {
		return 0
	}
	return r.entries[0].n
}

func (r *ring) replayable(n uint64) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.n == n {
			return e.bytes, true
		}
	}
	return nil, false
}

// stats reports the ring's size and its lowest and highest n as they stand, WITHOUT evicting: a
// diagnostic read must not change what a resume would replay.
func (r *ring) stats() (frames, bytes int, floor, top uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) > 0 {
		floor, top = r.entries[0].n, r.entries[len(r.entries)-1].n
	}
	return len(r.entries), r.bytes + r.replyBytes, floor, top
}

// since returns every frame after lastN, oldest first.
func (r *ring) since(lastN uint64) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked()
	out := make([][]byte, 0, len(r.entries))
	for _, e := range r.entries {
		if e.n > lastN {
			out = append(out, e.bytes)
		}
	}
	return out
}

// entriesSince returns replay bytes and their internal scopes, oldest first. Callers must release
// the ring lock before consulting the authoritative compatibility source.
func (r *ring) entriesSince(lastN uint64) []ringEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked()
	out := make([]ringEntry, 0, len(r.entries))
	for _, e := range r.entries {
		if e.n > lastN {
			out = append(out, ringEntry{n: e.n, bytes: e.bytes, at: e.at, expires: e.expires, scope: e.scope.clone()})
		}
	}
	return out
}

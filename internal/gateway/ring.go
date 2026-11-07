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
}

// ring is a per-connection replay buffer, bounded on count, bytes and age. It is an optimisation:
// the per-group seq cursors are the truth, and a client whose n is below the floor heals from
// ready's digest plus GET /handshakes?from= and GET /messages?from=.
type ring struct {
	mu      sync.Mutex
	limits  ringLimits
	clk     clock.Clock
	entries []ringEntry
	bytes   int
}

func newRing(l ringLimits, clk clock.Clock) *ring {
	return &ring{limits: l, clk: clk}
}

func (r *ring) add(n uint64, frame []byte, scope FrameScope) {
	r.addOp(n, frame, scope, 0)
}

// addOp is add for a frame of opcode op. Every opcode keeps the ring's TTL except op 62 bot_reply,
// whose entry expires after BotReplyHold when that is sooner (Bot v2 task 10: an undelivered reply
// is dropped after two minutes). Eviction stays head-first, so the replay range stays contiguous:
// with the default TTL (also two minutes) the two bounds coincide.
func (r *ring) addOp(n uint64, frame []byte, scope FrameScope, op Op) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clk.Now()
	ttl := r.limits.TTL
	if op == OpBotReply && BotReplyHold < ttl {
		ttl = BotReplyHold
	}
	r.entries = append(r.entries, ringEntry{n: n, bytes: frame, at: now, expires: now.Add(ttl), scope: scope.clone()})
	r.bytes += len(frame)
	r.evictLocked()
}

// evictLocked drops the oldest entries until all three bounds hold.
func (r *ring) evictLocked() {
	now := r.clk.Now()
	for len(r.entries) > 0 {
		head := r.entries[0]
		tooMany := len(r.entries) > r.limits.Frames
		tooBig := r.bytes > r.limits.Bytes
		tooOld := now.After(head.expires)
		if !tooMany && !tooBig && !tooOld {
			return
		}
		r.bytes -= len(head.bytes)
		r.entries = r.entries[1:]
	}
}

// trim drops everything the client has acknowledged with a heartbeat's last_n.
func (r *ring) trim(lastN uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.entries) > 0 && r.entries[0].n <= lastN {
		r.bytes -= len(r.entries[0].bytes)
		r.entries = r.entries[1:]
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
	return len(r.entries), r.bytes, floor, top
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

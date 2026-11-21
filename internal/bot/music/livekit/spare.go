package livekit

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"

	"github.com/dilla-chat/dilla/internal/bot/music/counter"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/sframe"
)

// The next track's frame-counter block is reserved while the current track plays, so the only durable write left
// between two queued tracks is the queue's save (G3: the gap is at most 150 ms; a slow disk sync on the switch was CI
// run 44790568832's 550 ms gap).
//
// The safety of a block does not change. counter.Reserve commits the cursor past the block before the spare exists, so
// a crash or restart before the spare is used skips it and never reuses it; the spare lives only in the pool's memory
// and is taken once (takeSpare clears it whether it matches or not); it is used only in the room, call group, epoch,
// Bot leaf and base key it was reserved for, which are the durable cursor's own key (group, epoch, leaf) plus the key
// the counters run under. A spare for anything else is dropped and the start reserves afresh.
//
// The pool's reservations must never overlap (lockReserve): two reservations of one (group, epoch, leaf) that read the
// same cursor revision build the same transaction id and changes, and the helper replays the first one's receipt to the
// second (dilla-bot-core state::transaction) instead of refusing it, so both would get the same block: two senders
// under one key with the same counters, AES-GCM nonce reuse. The helper's compare-and-swap does not prevent that; only
// the serialization does.

// spareBlock is a counter block reserved for the pool's next track.
type spareBlock struct {
	generation uint64 // the room it was reserved in
	group      id.ID
	epoch      uint64
	leaf       uint64
	keyHash    [32]byte // never the base key itself
	block      counter.Range
}

func spareKeyHash(epoch native.CallMediaEpoch) [32]byte {
	seed := make([]byte, 0, 40)
	seed = append(seed, []byte("dilla.bot.music.spare.v1\x00")...)
	seed = append(seed, epoch.BaseKey[:]...)
	return sha256.Sum256(seed)
}

func (b *spareBlock) reservedFor(generation uint64, group id.ID, epoch native.CallMediaEpoch) bool {
	h := spareKeyHash(epoch)
	return b.generation == generation && b.group == group && b.epoch == epoch.Epoch && b.leaf == epoch.OwnLeaf &&
		subtle.ConstantTimeCompare(b.keyHash[:], h[:]) == 1
}

// lockReserve admits one reservation of the pool at a time (see above). A waiter gives up with its own ctx: a start
// behind a spare whose helper write stalls can still be cancelled by its deadline, a stop or a skip.
func (p *Pool) lockReserve(ctx context.Context) (func(), error) {
	p.mu.Lock()
	if p.reserveSem == nil {
		p.reserveSem = make(chan struct{}, 1)
	}
	sem := p.reserveSem
	p.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// takeSpare hands out the spare when it was reserved in the current room for group under epoch, and drops it in every
// case: a block is used at most once.
func (p *Pool) takeSpare(group id.ID, epoch native.CallMediaEpoch) (counter.Range, bool) {
	p.mu.Lock()
	spare, generation := p.spare, p.generation
	p.spare = nil
	p.mu.Unlock()
	if spare == nil || !spare.reservedFor(generation, group, epoch) {
		return counter.Range{}, false
	}
	return spare.block, true
}

// reserveFor returns the counter block for a sender under epoch: the spare when it was reserved for exactly this room,
// group, epoch, leaf and key, else a fresh durable reservation, under lockReserve. A session outside a pool (nil p) has
// no spare and reserves afresh: it is the only reserver of its call group (one Bot track per voice channel).
func (p *Pool) reserveFor(ctx context.Context, state counter.State, group id.ID, epoch native.CallMediaEpoch) (counter.Range, error) {
	if p != nil {
		unlock, err := p.lockReserve(ctx)
		if err != nil {
			return counter.Range{}, err
		}
		defer unlock()
		if block, ok := p.takeSpare(group, epoch); ok {
			return block, nil
		}
	}
	if err := validateSenderEpoch(epoch); err != nil {
		return counter.Range{}, err
	}
	return counter.Reserve(ctx, state, group, epoch.Epoch, uint16(epoch.OwnLeaf)) //nolint:gosec // G115: validateSenderEpoch bounds OwnLeaf to uint16
}

// prepareSpare reserves the next track's block under epoch in the room of generation, off the switch. A failed
// reservation leaves no spare (the next start reserves its own); one whose commit landed but whose answer was lost, or
// that lands after its room ended, only skips its block. A spare already held for this epoch is kept; one for an older
// epoch of the same group is replaced.
func (p *Pool) prepareSpare(ctx context.Context, state counter.State, group id.ID, epoch native.CallMediaEpoch, generation uint64) {
	if p == nil || ctx == nil || state == nil || validateSenderEpoch(epoch) != nil {
		return
	}
	unlock, err := p.lockReserve(ctx)
	if err != nil {
		return
	}
	defer unlock()
	newer := func(held *spareBlock) bool { return held != nil && held.group == group && held.epoch > epoch.Epoch }
	p.mu.Lock()
	held := p.spare
	live := !p.closed && !p.dead && p.generation == generation
	p.mu.Unlock()
	if !live || newer(held) || (held != nil && held.reservedFor(generation, group, epoch)) {
		return
	}
	block, err := counter.Reserve(ctx, state, group, epoch.Epoch, uint16(epoch.OwnLeaf)) //nolint:gosec // G115: as above
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.dead || p.generation != generation || newer(p.spare) {
		return // the block is skipped: its cursor is already past it
	}
	p.spare = &spareBlock{generation: generation, group: group, epoch: epoch.Epoch, leaf: epoch.OwnLeaf, keyHash: spareKeyHash(epoch), block: block}
}

// roomContext is the context of the room of generation, or nil once that room is gone: a spare reserved for it lives
// as long as the room, not one track.
func (p *Pool) roomContext(generation uint64) context.Context {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.dead || p.generation != generation {
		return nil
	}
	return p.roomCtx
}

// senderForEpoch is senderForReservedEpoch through the pool: its block is the spare when it fits, else a fresh one.
func (p *Pool) senderForEpoch(ctx context.Context, state counter.State, group id.ID, epoch native.CallMediaEpoch) (*sframe.Sender, error) {
	if ctx == nil || state == nil || group.IsZero() {
		return nil, ErrBinding
	}
	if err := validateSenderEpoch(epoch); err != nil {
		return nil, err
	}
	block, err := p.reserveFor(ctx, state, group, epoch)
	if err != nil {
		return nil, fmt.Errorf("music livekit: sender counter reservation failed: %w", err)
	}
	sender, err := sframe.NewSenderRange(epoch.BaseKey, uint16(epoch.OwnLeaf), epoch.Epoch, block.Start, block.End) //nolint:gosec // G115: validateSenderEpoch bounds OwnLeaf to uint16
	if err != nil {
		return nil, fmt.Errorf("music livekit: sender range initialization failed: %w", err)
	}
	return sender, nil
}

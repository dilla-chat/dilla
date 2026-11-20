package livekit

import (
	"context"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/music/counter"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/sframe"
)

// firstCounter is the SFrame counter of the first frame sender encrypts.
func firstCounter(t *testing.T, sender *sframe.Sender) uint64 {
	t.Helper()
	frame, err := sender.Encrypt(sframe.Opus, sframe.Mic, 0, []byte{0xfc, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	_, ctr, _, err := sframe.DecodeHeader(frame)
	if err != nil {
		t.Fatal(err)
	}
	return ctr
}

func spareOf(p *Pool) *spareBlock {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spare
}

func waitForSpare(t *testing.T, p *Pool) *spareBlock {
	t.Helper()
	waitFor(t, func() bool { return spareOf(p) != nil })
	return spareOf(p)
}

func newSparePool(t *testing.T) (*Pool, *poolEpochs) {
	t.Helper()
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p.backend = &poolBackendStub{}
	t.Cleanup(func() { _ = p.Close() })
	return p, newPoolEpochs()
}

// The next queued track's switch makes no counter commit (one disk sync on the helper) when the block reserved while the
// last track played is for its epoch: CI run 44790568832's gap was the switch's syncs.
func TestTheNextTrackUsesTheSpareBlockWithoutACounterCommit(t *testing.T) {
	p, epochs := newSparePool(t)
	call, group := poolCall(1, 2)
	first, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	spare := waitForSpare(t, p)
	if spare.block.Start != counter.BlockSize {
		t.Fatalf("spare block starts at %d, want %d (after the first track's block)", spare.block.Start, counter.BlockSize)
	}
	_ = first.Close()

	before := epochs.state.commits()
	second, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	if got := firstCounter(t, second.sender); got != spare.block.Start {
		t.Fatalf("the next track's first counter is %d, want the spare's %d", got, spare.block.Start)
	}
	// The commit after the start is the following track's spare, reserved off this switch.
	next := waitForSpare(t, p)
	if after := epochs.state.commits(); after != before+1 {
		t.Fatalf("counter commits went %d -> %d, want exactly one (the following spare, not the switch)", before, after)
	}
	if next.block.Start < spare.block.End {
		t.Fatalf("the following spare %v overlaps the used one %v", next.block, spare.block)
	}
	_ = second.Close()
}

// A spare is used under exactly the group, epoch, leaf and key it was reserved for; for anything else it is dropped and
// the start reserves afresh, never reusing its counters under another key.
func TestAStaleSpareIsDroppedAndAFreshBlockReserved(t *testing.T) {
	base := native.CallMediaEpoch{Epoch: 1, OwnLeaf: 1, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 1}}}
	_, group := poolCall(1, 2)
	_, otherGroup := poolCall(1, 3)
	cases := []struct {
		name  string
		group func() [16]byte
		epoch native.CallMediaEpoch
	}{
		{"another epoch", nil, native.CallMediaEpoch{Epoch: 2, OwnLeaf: 1, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 1}}}},
		{"another leaf", nil, native.CallMediaEpoch{Epoch: 1, OwnLeaf: 2, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 2}}}},
		{"another key", nil, native.CallMediaEpoch{Epoch: 1, OwnLeaf: 1, BaseKey: [16]byte{9}, Roster: []native.CallMediaMember{{Leaf: 1}}}},
		{"another group", func() [16]byte { return otherGroup }, base},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, epochs := newSparePool(t)
			p.prepareSpare(context.Background(), epochs.state, group, base, p.generation)
			spare := spareOf(p)
			if spare == nil {
				t.Fatal("no spare was reserved")
			}
			target := group
			if c.group != nil {
				target = c.group()
			}
			before := epochs.state.commits()
			block, err := p.reserveFor(context.Background(), epochs.state, target, c.epoch)
			if err != nil {
				t.Fatal(err)
			}
			if block == spare.block || block.Start < spare.block.End {
				t.Fatalf("the start got %v for a spare reserved under another binding %v", block, spare.block)
			}
			if after := epochs.state.commits(); after != before+1 {
				t.Fatalf("counter commits went %d -> %d, want one fresh reservation", before, after)
			}
			if spareOf(p) != nil {
				t.Fatal("the stale spare was kept")
			}
		})
	}
}

// A spare is used at most once, and a dead or reset room drops it.
func TestASpareIsUsedOnceAndARoomEndDropsIt(t *testing.T) {
	p, epochs := newSparePool(t)
	_, group := poolCall(1, 2)
	epoch := epochs.epoch
	p.prepareSpare(context.Background(), epochs.state, group, epoch, p.generation)
	if _, ok := p.takeSpare(group, epoch); !ok {
		t.Fatal("the fitting spare was not taken")
	}
	if _, ok := p.takeSpare(group, epoch); ok {
		t.Fatal("a spare was handed out twice")
	}
	p.prepareSpare(context.Background(), epochs.state, group, epoch, p.generation)
	p.markDead(p.generation, ErrPoolDead)
	if spareOf(p) != nil {
		t.Fatal("a dead room kept its spare")
	}
	// A spare whose reservation lands after the room ended is skipped, not held.
	p.prepareSpare(context.Background(), epochs.state, group, epoch, p.generation)
	if spareOf(p) != nil {
		t.Fatal("a dead room took a new spare")
	}
}

// A crash (or restart) between the spare's reservation and its use never reuses its block: the durable cursor is past
// it before the spare exists, so the next process reserves after it.
func TestACrashBetweenReservationAndUseNeverReusesTheSpare(t *testing.T) {
	p, epochs := newSparePool(t)
	call, group := poolCall(1, 2)
	first, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	spare := waitForSpare(t, p)
	_ = first.Close()
	_ = p.Close() // the process ends with the spare unused

	restarted, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	restarted.backend = &poolBackendStub{}
	t.Cleanup(func() { _ = restarted.Close() })
	again, err := restarted.startSource(context.Background(), call, group, epochs, newPoolSource()) // same durable state
	if err != nil {
		t.Fatal(err)
	}
	if got := firstCounter(t, again.sender); got < spare.block.End {
		t.Fatalf("after a restart the first counter is %d, inside the unused spare %v", got, spare.block)
	}
	_ = again.Close()
}

package livekit

import (
	"context"
	"errors"
	"sync"
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

// gateCommits makes state's next commit signal entered and wait for release.
func gateCommits(state *counterStateStub) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	state.beforeCommit = func() {
		first := false
		once.Do(func() { first = true })
		if first {
			entered <- struct{}{}
			<-release
		}
	}
	return entered, release
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

// A spare is used only under exactly the group, epoch, leaf and key it was reserved for; for anything else it is dropped
// and the start makes one fresh reservation of its own durable cursor. A different key (same group, epoch and leaf) is
// the same cursor, so its fresh block lies past the spare's.
func TestAStaleSpareIsDroppedAndAFreshBlockReserved(t *testing.T) {
	base := native.CallMediaEpoch{Epoch: 1, OwnLeaf: 1, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 1}}}
	_, group := poolCall(1, 2)
	_, otherGroup := poolCall(1, 3)
	cases := []struct {
		name       string
		otherGroup bool
		epoch      native.CallMediaEpoch
		sameCursor bool
	}{
		{"another epoch", false, native.CallMediaEpoch{Epoch: 2, OwnLeaf: 1, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 1}}}, false},
		{"another leaf", false, native.CallMediaEpoch{Epoch: 1, OwnLeaf: 2, BaseKey: [16]byte{1}, Roster: []native.CallMediaMember{{Leaf: 2}}}, false},
		{"another key", false, native.CallMediaEpoch{Epoch: 1, OwnLeaf: 1, BaseKey: [16]byte{9}, Roster: []native.CallMediaMember{{Leaf: 1}}}, true},
		{"another group", true, base, false},
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
			if c.otherGroup {
				target = otherGroup
			}
			before := epochs.state.commits()
			block, err := p.reserveFor(context.Background(), epochs.state, target, c.epoch)
			if err != nil {
				t.Fatal(err)
			}
			if after := epochs.state.commits(); after != before+1 {
				t.Fatalf("counter commits went %d -> %d, want one fresh reservation", before, after)
			}
			if spareOf(p) != nil {
				t.Fatal("the stale spare was kept")
			}
			if c.sameCursor && block.Start < spare.block.End {
				t.Fatalf("the fresh block %v overlaps the dropped spare %v on the same cursor", block, spare.block)
			}
		})
	}
}

// A spare is used at most once; a new room, a dead room or a closed pool drops it, and a spare of an older room is
// never taken.
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
	p.mu.Lock()
	p.generation++ // another room, the spare left behind
	p.mu.Unlock()
	if _, ok := p.takeSpare(group, epoch); ok {
		t.Fatal("a spare of an older room was taken")
	}

	p.prepareSpare(context.Background(), epochs.state, group, epoch, p.generation)
	if spareOf(p) == nil {
		t.Fatal("no spare was reserved")
	}
	p.markDead(p.generation, ErrPoolDead)
	if spareOf(p) != nil {
		t.Fatal("a dead room kept its spare")
	}

	closing, closingEpochs := newSparePool(t)
	closing.prepareSpare(context.Background(), closingEpochs.state, group, epoch, closing.generation)
	if spareOf(closing) == nil {
		t.Fatal("no spare was reserved")
	}
	_ = closing.Close()
	if spareOf(closing) != nil {
		t.Fatal("a closed pool kept its spare")
	}
}

// A reservation that commits after its room ended is skipped: the spare is not held (its cursor is past it anyway).
func TestASpareThatLandsAfterTheRoomEndedIsSkipped(t *testing.T) {
	p, epochs := newSparePool(t)
	_, group := poolCall(1, 2)
	entered, release := gateCommits(epochs.state)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.prepareSpare(context.Background(), epochs.state, group, epochs.epoch, p.generation)
	}()
	<-entered // past the live check, its commit in flight
	p.markDead(p.generation, ErrPoolDead)
	close(release)
	<-done
	if n := epochs.state.commits(); n != 1 {
		t.Fatalf("the late reservation made %d commits, want 1", n)
	}
	if spareOf(p) != nil {
		t.Fatal("a reservation that landed after the room ended was held")
	}
}

// A spare held for a newer epoch of another group does not keep this group's spare out.
func TestAnotherGroupsNewerSpareDoesNotBlockThisOne(t *testing.T) {
	p, epochs := newSparePool(t)
	_, group := poolCall(1, 2)
	_, other := poolCall(1, 3)
	p.mu.Lock()
	p.spare = &spareBlock{generation: p.generation, group: other, epoch: 9, leaf: 1}
	p.mu.Unlock()
	p.prepareSpare(context.Background(), epochs.state, group, epochs.epoch, p.generation)
	if spare := spareOf(p); spare == nil || spare.group != group {
		t.Fatalf("the spare is %+v, want this group's", spare)
	}
}

// A start waiting behind another reservation gives up with its own context (a stop, a skip or its deadline).
func TestAStartWaitingOnAReservationHonoursItsContext(t *testing.T) {
	p, epochs := newSparePool(t)
	_, group := poolCall(1, 2)
	entered, release := gateCommits(epochs.state)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.prepareSpare(context.Background(), epochs.state, group, epochs.epoch, p.generation)
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	begun := time.Now()
	_, err := p.reserveFor(ctx, epochs.state, group, epochs.epoch)
	waited := time.Since(begun)
	close(release)
	<-done
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reserveFor behind a stalled reservation = %v, want its own deadline", err)
	}
	if waited > time.Second {
		t.Fatalf("the start waited %v past its deadline", waited)
	}
}

// Every reservation of the pool runs alone. Two reservations of one cursor that read the same revision build the same
// transaction, and the helper replays the first's receipt to the second: both would get one block (nonce reuse under
// one key). A start, a spare and an epoch follow racing must still get pairwise disjoint blocks.
func TestRacingReservationsGetDisjointBlocks(t *testing.T) {
	for round := range 5 {
		p, epochs := newSparePool(t)
		_, group := poolCall(1, 2)
		// Each read returns once all three reservers have read, or after 100 ms: unserialized, all three read one revision.
		var mu sync.Mutex
		arrived, all := 0, make(chan struct{})
		epochs.state.afterGet = func() {
			mu.Lock()
			arrived++
			if arrived == 3 {
				close(all)
			}
			mu.Unlock()
			select {
			case <-all:
			case <-time.After(100 * time.Millisecond):
			}
		}
		var wg sync.WaitGroup
		blocks := make([]counter.Range, 2)
		errs := make([]error, 2)
		for i := range 2 { // the start and the epoch follow both reserve through reserveFor
			wg.Go(func() { blocks[i], errs[i] = p.reserveFor(context.Background(), epochs.state, group, epochs.epoch) })
		}
		wg.Go(func() { p.prepareSpare(context.Background(), epochs.state, group, epochs.epoch, p.generation) })
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if spare := spareOf(p); spare != nil {
			blocks = append(blocks, spare.block)
		}
		for i := range blocks {
			for j := i + 1; j < len(blocks); j++ {
				if blocks[i].Start < blocks[j].End && blocks[j].Start < blocks[i].End {
					t.Fatalf("round %d: blocks %v and %v overlap: two senders would share counters under one key", round, blocks[i], blocks[j])
				}
			}
		}
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

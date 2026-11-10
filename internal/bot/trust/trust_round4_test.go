package trust

// Task 16 fix round 4: the per-user serial mailbox (N-7). Every message for a user is handled in one hold of the lock,
// a fresh push takes its generation and is queued in the same hold, and no job replaces a waiting job of a newer
// generation.

import (
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// TestAnOlderFreshPushNeverSupersedesTheLivePush (N-7, the reviewer's probe): two feed workers push for one user at
// once. Before round 4, Pushed took its generation in one hold of the lock and enqueued in a second, and a fresh job
// superseded whatever waited, whatever its generation. So the older generation's job, enqueued last, superseded the
// newer (live) push: the live push never ran, nothing retried it, the older push ran as not live and changed no hold,
// and the user ended not held with the revocation unapplied. The split is modelled as the probe did: the older push's
// generation and stack entry first, then the revocation pushed whole, then the older push's job arriving at the
// mailbox, with the worker parked (running=true) and started after both. The older job must be the superseded one.
func TestAnOlderFreshPushNeverSupersedesTheLivePush(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada, phone := uid(1), uid(0xa1)
	core.member(uid(0x91), 1, ada, phone)
	older, revocation := []byte{0x01, 0x02}, push(phone)
	core.pushFail = map[string]error{string(older): &helper.StatusError{Status: 6}} // transient
	tr := New(core, testPolicy).WithClock(clk).WithInlineBudget(20 * time.Millisecond)
	defer tr.Close()
	tr.mu.Lock()
	u := tr.userLocked(ada)
	u.running = true // the worker is parked until both messages are in
	// Worker A (the old connection): the older list's generation and stack entry, as a split Pushed left them.
	u.gen++
	gOld := u.gen
	u.live = append(u.live, livePush{gen: gOld, push: older, attempts: 1})
	u.boundLive()
	jOld := &job{push: older, gen: gOld, attempts: 1, done: make(chan outcome, 1)}
	// Worker B (the new connection's snapshot): the revocation, pushed whole.
	jNew := tr.pushLocked(ada, u, revocation)
	// Worker A resumes: its job reaches the mailbox after the newer one.
	queued := tr.enqueueLocked(ada, u, jOld)
	tr.mu.Unlock()
	go tr.drive(ada, u)
	oNew := <-jNew.done
	oOld := <-jOld.done
	waitFor(t, "idle", func() bool { return idle(tr, ada) })
	clk.Advance(time.Hour)
	time.Sleep(20 * time.Millisecond)
	tr.mu.Lock()
	live := u.live
	tr.mu.Unlock()
	t.Logf("older job queued=%v; live push: %v; older push: %v; held=%v revocationApplied=%v retry=%v live=%v", queued, oNew.err,
		oOld.err, tr.Held(ada), core.isRefused(leafKey{uid(0x91), 1}), retryOf(tr, ada), live)
	if !tr.Held(ada) && !core.isRefused(leafKey{uid(0x91), 1}) {
		t.Fatal("FAIL OPEN: the live (revocation) push never ran, nothing retries it, and the user is not held")
	}
	if queued || !errors.Is(oOld.err, errSuperseded) {
		t.Fatalf("the older job was queued (%v) with outcome %v; want superseded by the newer waiting job", queued, oOld.err)
	}
	if oNew.err != nil || !core.isRefused(leafKey{uid(0x91), 1}) || tr.Held(ada) {
		t.Fatalf("the live push: %v, applied %v, held %v; want applied and released", oNew.err, core.isRefused(leafKey{uid(0x91), 1}), tr.Held(ada))
	}
}

// pushKind is one kind of push in the stress test, and the hold state it implies when it is the user's newest push
// that the helper did not refuse outright.
type pushKind int

const (
	kindRevocation pushKind = iota // applies and revokes the phone: released, the leaf refused
	kindPlain                      // applies, revokes nothing: released
	kindTransient                  // op 24 answers busy: held, its retry pending
	kindRefused                    // op 24 answers invalid: leaves the stack; the push before it decides
)

func (k pushKind) String() string {
	return [...]string{"revocation", "plain", "transient", "refused"}[k]
}

// lastApplied is the body of the last push op 24 applied for user.
func (c *fakeCore) lastApplied(user id.ID) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last[user]
}

// TestConcurrentPushesEndInTheStateOfTheNewestPush (N-7, through the public API only): many goroutines call Pushed
// for one user at once, with a random mix of revocations, plain lists, transient failures and refusals, 5000 times.
// Every time, once the worker is idle, the user's hold state is the one implied by the newest push the helper did not
// refuse outright (the live stack's top, which the round 2 generations define; the drive is public API only): either
// it applied and released the user (a revocation also refused the leaf), or it holds the user with its own retry
// pending and has not applied (a transient failure, or a push that never ran because the push refused after it rolled
// back to it). Never is the user released with that push unapplied, and never held once it applied.
func TestConcurrentPushesEndInTheStateOfTheNewestPush(t *testing.T) {
	const iterations, pushes = 5000, 4
	slow := RetryPolicy{First: time.Hour, Max: time.Hour, Attempts: 8} // no retry runs within the test
	rng := rand.New(rand.NewPCG(16, 7))
	var released, heldTransient, heldRolledBack, allRefused int
	for i := range iterations {
		core := newFakeCore()
		ada, phone := uid(1), uid(0xa1)
		core.member(uid(0x91), 1, ada, phone)
		kinds := make([]pushKind, pushes)
		bodies := make([][]byte, pushes)
		core.pushFail = map[string]error{}
		for p := range pushes {
			kinds[p] = pushKind(rng.IntN(4))
			switch kinds[p] {
			case kindRevocation:
				bodies[p] = append(push(phone), byte(p)) // distinct bodies; the fake ignores the trailing byte
			case kindPlain:
				bodies[p] = []byte{0x50, byte(p)}
			case kindTransient:
				bodies[p] = []byte{0x60, byte(p)}
				core.pushFail[string(bodies[p])] = &helper.StatusError{Status: 6}
			case kindRefused:
				bodies[p] = []byte{0x70, byte(p)}
				core.pushFail[string(bodies[p])] = &helper.StatusError{Status: 1}
			}
		}
		if rng.IntN(2) == 0 { // half the runs end on a revocation
			kinds[pushes-1], bodies[pushes-1] = kindRevocation, append(push(phone), byte(pushes-1))
			delete(core.pushFail, string([]byte{0x60, byte(pushes - 1)}))
			delete(core.pushFail, string([]byte{0x70, byte(pushes - 1)}))
		}
		tr := New(core, slow).WithInlineBudget(time.Second)
		var wg sync.WaitGroup
		for p := range pushes {
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = tr.Pushed(t.Context(), ada, bodies[p]) }()
		}
		wg.Wait()
		waitFor(t, "idle", func() bool { return idle(tr, ada) })
		tr.mu.Lock()
		u := tr.users[ada]
		var top []byte
		if len(u.live) > 0 {
			top = u.live[len(u.live)-1].push
		}
		retry, held := u.retry, u.held()
		tr.mu.Unlock()
		if top == nil {
			// Every push ran and was refused outright: nothing applied, nothing holds.
			for _, k := range kinds {
				if k != kindRefused {
					t.Fatalf("iteration %d: an empty live stack after pushes %v", i, kinds)
				}
			}
			if held {
				t.Fatalf("iteration %d: only refused pushes, yet held", i)
			}
			allRefused++
			tr.Close()
			continue
		}
		kind, found := kindOf(bodies, kinds, top)
		if !found {
			t.Fatalf("iteration %d: the live push %x is none of the pushes %v", i, top, kinds)
		}
		applied := core.lastApplied(ada) == string(top)
		switch {
		case held:
			if retry == nil || string(retry.push) != string(top) {
				t.Fatalf("iteration %d: held, but not by the newest push's retry: retry %v, newest %v (pushes %v)", i, retry, kind, kinds)
			}
			if applied {
				t.Fatalf("iteration %d: FAIL CLOSED: the newest push (%v) applied, yet the user is held with its retry pending (pushes %v)", i, kind, kinds)
			}
			if kind == kindTransient {
				heldTransient++
			} else {
				heldRolledBack++
			}
		case kind == kindTransient || kind == kindRefused || !applied:
			t.Fatalf("iteration %d: FAIL OPEN: released with the newest push (%v, applied %v) unapplied (pushes %v)", i, kind, applied, kinds)
		case kind == kindRevocation && !core.isRefused(leafKey{uid(0x91), 1}):
			t.Fatalf("iteration %d: the newest push, a revocation, applied without refusing the leaf (pushes %v)", i, kinds)
		default:
			released++
		}
		tr.Close()
	}
	t.Logf("%d iterations: released %d, held by a transient failure %d, held by a rollback's retry %d, all refused %d",
		iterations, released, heldTransient, heldRolledBack, allRefused)
}

func kindOf(bodies [][]byte, kinds []pushKind, body []byte) (pushKind, bool) {
	for p, b := range bodies {
		if string(b) == string(body) {
			return kinds[p], true
		}
	}
	return 0, false
}

// TestGenerationOrderIsQueueOrder (N-7, rule 1): under contention, the job that waits is always of a newer generation
// than every job it superseded, and the push that applies last is the newest. Checked by the outcomes: a superseded
// Pushed answers errSuperseded, and the live stack's top is the push that applied.
func TestGenerationOrderIsQueueOrder(t *testing.T) {
	core := newFakeCore()
	ada := uid(1)
	core.member(uid(0x91), 1, ada, uid(0xa1))
	gate := make(chan struct{})
	first := []byte{0x01}
	core.blockPush = map[string]chan struct{}{string(first): gate}
	tr := New(core, testPolicy).WithInlineBudget(time.Second)
	defer tr.Close()
	go func() { _, _ = tr.Pushed(t.Context(), ada, first) }() // runs, blocked
	waitFor(t, "the first attempt running", func() bool { tr.mu.Lock(); defer tr.mu.Unlock(); u := tr.users[ada]; return u != nil && u.running })
	const n = 16
	outcomes := make([]error, n)
	var wg sync.WaitGroup
	for p := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, outcomes[p] = tr.Pushed(t.Context(), ada, []byte{0x10, byte(p)})
		}()
	}
	// Every push is in: one waits, the rest were superseded.
	waitFor(t, "every push in", func() bool { tr.mu.Lock(); defer tr.mu.Unlock(); return tr.users[ada].gen == n+1 })
	core.mu.Lock()
	delete(core.blockPush, string(first))
	core.mu.Unlock()
	close(gate)
	wg.Wait()
	waitFor(t, "idle", func() bool { return idle(tr, ada) })
	tr.mu.Lock()
	u := tr.users[ada]
	live, gen := u.live, u.gen
	tr.mu.Unlock()
	superseded, applied := 0, 0
	for _, err := range outcomes {
		switch {
		case errors.Is(err, errSuperseded):
			superseded++
		case err == nil:
			applied++
		default:
			t.Fatalf("a push answered %v", err)
		}
	}
	if applied != 1 || superseded != n-1 || len(live) != 1 || live[0].gen != gen || gen != n+1 || tr.Held(ada) {
		t.Fatalf("applied %d superseded %d live %v gen %d held %v; want one applied (the newest, gen %d), %d superseded, released",
			applied, superseded, live, gen, tr.Held(ada), n+1, n-1)
	}
	t.Logf("%d pushes under one hung attempt: the newest applied, %d superseded", n, superseded)
}

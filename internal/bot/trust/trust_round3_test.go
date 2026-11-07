package trust

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/clock"
)

// Fix round 3, N-5 (the reviewer's probe, on the fix round 4 mailbox): a stale retry falling due and a fresh revocation
// push for the same user arrive at the user's mailbox back to back while the worker is busy, in either order. Before
// round 3, queueRetry checked under t.mu, unlocked, and enqueued in a second hold, so a fresh push in between was
// superseded by the stale retry. Now each is one message in one hold, and no job replaces a waiting job of a newer
// generation, so whatever the order the user ends held or with the revocation applied, never released with it
// unapplied.
func TestAStaleRetryNeverSupersedesANewerPushInQueueRetrysWindow(t *testing.T) {
	for _, retryFirst := range []bool{false, true} {
		clk := clock.NewFake(time.Unix(1_800_000_000, 0))
		core := newFakeCore()
		ada, phone := uid(1), uid(0xa1)
		core.member(uid(0x91), 1, ada, phone)
		older, revocation := []byte{0x01, 0x02}, push(phone)
		core.pushFail = map[string]error{string(older): &helper.StatusError{Status: 6}}
		tr := New(core, testPolicy).WithClock(clk).WithInlineBudget(20 * time.Millisecond)
		_, _ = tr.Pushed(t.Context(), ada, older)
		waitFor(t, "idle", func() bool { return idle(tr, ada) })
		core.mu.Lock()
		delete(core.pushFail, string(older)) // the stale retry would now succeed
		core.mu.Unlock()
		r := retryOf(tr, ada)
		// The worker is busy (modelled by running=true; drive is started below, after both messages).
		tr.mu.Lock()
		u := tr.users[ada]
		u.running = true
		var jRev *job
		if retryFirst {
			tr.queueRetryLocked(ada, u, r)
			jRev = tr.pushLocked(ada, u, revocation)
		} else {
			jRev = tr.pushLocked(ada, u, revocation)
			tr.queueRetryLocked(ada, u, r)
		}
		tr.mu.Unlock()
		go tr.drive(ada, u)
		o := <-jRev.done
		waitFor(t, "idle", func() bool { return idle(tr, ada) })
		tr.mu.Lock()
		live := u.live
		tr.mu.Unlock()
		t.Logf("retry first %v: revocation push outcome: %v; held=%v revocationApplied=%v retry=%v live=%v", retryFirst, o.err,
			tr.Held(ada), core.isRefused(leafKey{uid(0x91), 1}), retryOf(tr, ada), live)
		if !tr.Held(ada) && !core.isRefused(leafKey{uid(0x91), 1}) {
			t.Fatal("FAIL OPEN: the revocation push was superseded by a stale retry and the user was released")
		}
		if o.err != nil {
			t.Fatalf("retry first %v: the revocation push was not applied: %v", retryFirst, o.err)
		}
		tr.Close()
	}
}

// TestKickNeverSupersedesANewerPush (fix round 3, N-5, Kick's variant): a fresh helper is bound (Kick) while the
// user's revocation is pushed. Kick checks and queues the stale retry in one hold of the lock, so whatever the
// interleaving the user ends either held or with the revocation applied; never released with it unapplied.
func TestKickNeverSupersedesANewerPush(t *testing.T) {
	slow := RetryPolicy{First: time.Hour, Max: time.Hour, Attempts: 8} // only Kick retries
	for i := 0; i < 200; i++ {
		core := newFakeCore()
		ada, phone := uid(1), uid(0xa1)
		core.member(uid(0x91), 1, ada, phone)
		older, revocation := []byte{0x01, byte(i)}, push(phone)
		core.pushFail = map[string]error{string(older): &helper.StatusError{Status: 6}}
		tr := New(core, slow).WithInlineBudget(time.Second)
		_, _ = tr.Pushed(t.Context(), ada, older)
		core.mu.Lock()
		delete(core.pushFail, string(older))
		core.mu.Unlock()
		done := make(chan struct{})
		go func() { tr.Kick(); close(done) }()
		_, _ = tr.Pushed(t.Context(), ada, revocation)
		<-done
		waitFor(t, "idle", func() bool { return idle(tr, ada) })
		if !tr.Held(ada) && !core.isRefused(leafKey{uid(0x91), 1}) {
			t.Fatalf("iteration %d: released with the revocation unapplied", i)
		}
		tr.Close()
	}
}

// TestASupersededRevocationSurvivesTheNewerPushBeingRefused (fix round 3, N-6): push N runs while the revocation N+1
// waits; a newer push N+2 supersedes the revocation in the queue and is refused outright. The rollback goes back to
// the superseded revocation, not to N: the user stays held until the revocation applies.
func TestASupersededRevocationSurvivesTheNewerPushBeingRefused(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada, phone := uid(1), uid(0xa1)
	core.member(uid(0x91), 1, ada, phone)
	first, revocation, junk := []byte{0x01}, push(phone), []byte{0xf1}
	gate := make(chan struct{})
	core.blockPush = map[string]chan struct{}{string(first): gate}
	core.pushFail = map[string]error{string(junk): &helper.StatusError{Status: 1}}
	tr := New(core, testPolicy).WithClock(clk).WithInlineBudget(20 * time.Millisecond)
	defer tr.Close()
	_, _ = tr.Pushed(t.Context(), ada, first)      // runs, blocked
	_, _ = tr.Pushed(t.Context(), ada, revocation) // waits
	_, _ = tr.Pushed(t.Context(), ada, junk)       // supersedes the revocation in the queue
	core.mu.Lock()
	delete(core.blockPush, string(first))
	core.mu.Unlock()
	close(gate) // first applies (not live), then junk is refused outright
	waitFor(t, "the worker idle", func() bool { return idle(tr, ada) })
	if r := retryOf(tr, ada); !tr.Held(ada) || r == nil || string(r.push) != string(revocation) {
		t.Fatalf("held %v, retry %v; want held with the superseded revocation retried", tr.Held(ada), r)
	}
	if core.isRefused(leafKey{uid(0x91), 1}) {
		t.Fatal("the revocation applied before its retry")
	}
	clk.Advance(time.Second)
	waitFor(t, "the revocation applied", func() bool { return core.isRefused(leafKey{uid(0x91), 1}) && !tr.Held(ada) })
}

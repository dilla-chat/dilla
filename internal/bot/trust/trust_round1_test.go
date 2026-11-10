package trust

// Task 16 fix round 1: only an applied push releases a held user (I-2), the first attempt waits at most the inline
// budget and never on another attempt of the user (M-1), re-judging failures are transient and one bad leaf does not
// stop the rest (M-2), and left groups cost no trust work (M-5).

import (
	"errors"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// TestADefinitiveRefusalOfANewerPushKeepsTheUserHeld (I-2): a revocation is being retried (transient failures), then a
// garbage push for the same user is refused outright. The user stays held and the revocation keeps being retried; it
// releases the user once it applies.
func TestADefinitiveRefusalOfANewerPushKeepsTheUserHeld(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada, phone := uid(1), uid(0xa1)
	core.member(uid(0x91), 1, ada, phone)
	revocation, garbage := push(phone), []byte{0xff, 0xee}
	core.pushFail = map[string]error{string(revocation): &helper.StatusError{Status: 6}, string(garbage): &helper.StatusError{Status: 1}}
	tr := New(core, testPolicy).WithClock(clk)
	defer tr.Close()
	if _, err := tr.Pushed(t.Context(), ada, revocation); err == nil || !tr.Held(ada) {
		t.Fatalf("the revocation: err %v held %v; want a transient failure and held", err, tr.Held(ada))
	}
	if _, err := tr.Pushed(t.Context(), ada, garbage); !Definitive(err) {
		t.Fatalf("the garbage push: err %v; want a definitive refusal", err)
	}
	if !tr.Held(ada) {
		t.Fatal("a refused garbage push released a user whose revocation was still being retried")
	}
	if !settled(tr, ada, 1) {
		t.Fatal("the revocation's retry was dropped")
	}
	core.mu.Lock()
	delete(core.pushFail, string(revocation))
	core.mu.Unlock()
	clk.Advance(time.Second)
	waitFor(t, "the revocation applied by its retry", func() bool { return core.isRefused(leafKey{uid(0x91), 1}) && !tr.Held(ada) })
}

// TestARetryRefusedOutrightGivesUp (I-2): a retried push the helper then refuses outright never applied: the user is
// given up and stays held.
func TestARetryRefusedOutrightGivesUp(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada := uid(1)
	core.member(uid(0x91), 1, ada, uid(0xa1))
	body := push()
	core.pushFail = map[string]error{string(body): &helper.StatusError{Status: 6}}
	tr := New(core, testPolicy).WithClock(clk)
	defer tr.Close()
	_, _ = tr.Pushed(t.Context(), ada, body)
	core.mu.Lock()
	core.pushFail[string(body)] = &helper.StatusError{Status: 4}
	core.mu.Unlock()
	clk.Advance(time.Second)
	waitFor(t, "the retry settled", func() bool { tr.mu.Lock(); defer tr.mu.Unlock(); return tr.users[ada].givenUp })
	if !tr.Held(ada) {
		t.Fatal("a retry refused outright released the user")
	}
}

// TestASlowFirstAttemptDoesNotHoldTheCaller (M-1): op 24 for A hangs. Pushed returns within the inline budget with
// ErrDeferred and A is held; a second push for A while the first is mid-call returns at once too (no wait on A's lock);
// B's push applies meanwhile. When A's call returns, A's newest push applies and A is released.
func TestASlowFirstAttemptDoesNotHoldTheCaller(t *testing.T) {
	core := newFakeCore()
	ada, bea := uid(1), uid(2)
	core.member(uid(0x91), 1, ada, uid(0xa1))
	core.member(uid(0x91), 2, bea, uid(0xb1))
	gate := make(chan struct{})
	core.block = map[id.ID]chan struct{}{ada: gate}
	tr := New(core, testPolicy).WithInlineBudget(50 * time.Millisecond)
	defer tr.Close()
	start := time.Now()
	if _, err := tr.Pushed(t.Context(), ada, push()); !errors.Is(err, ErrDeferred) || !tr.Held(ada) {
		t.Fatalf("a hung first attempt: err %v held %v; want ErrDeferred and held", err, tr.Held(ada))
	}
	if _, err := tr.Pushed(t.Context(), ada, push(uid(0xa1))); !errors.Is(err, ErrDeferred) {
		t.Fatalf("a second push mid-call: err %v; want ErrDeferred", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("two pushes of a hung user took %v, want about two inline budgets", took)
	}
	if _, err := tr.Pushed(t.Context(), bea, push()); err != nil || tr.Held(bea) {
		t.Fatalf("B beside A's hung call: err %v held %v", err, tr.Held(bea))
	}
	core.mu.Lock()
	core.block = nil
	core.mu.Unlock()
	close(gate)
	waitFor(t, "A's newest push applied", func() bool { return core.isRefused(leafKey{uid(0x91), 1}) && !tr.Held(ada) })
}

// TestChangedSkipsNotFoundLeavesAndRetriesOtherFailures (M-2): a leaf answered not_found is skipped and the user's other
// leaves are still judged. Any other failure of the re-judging, a definitive status included, is transient: the user
// stays held and is retried, and the retry re-judges although op 24 then changes nothing.
func TestChangedSkipsNotFoundLeavesAndRetriesOtherFailures(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada := uid(1)
	g := uid(0x91)
	core.member(g, 1, ada, uid(0xa1))
	core.member(g, 2, ada, uid(0xa2))
	core.member(g, 3, ada, uid(0xa3))
	core.revalErr = map[leafKey]error{{g, 1}: &helper.StatusError{Status: 7}, {g, 2}: &helper.StatusError{Status: 1}}
	tr := New(core, testPolicy).WithClock(clk)
	defer tr.Close()
	_, err := tr.Pushed(t.Context(), ada, push(uid(0xa3)))
	if err == nil || Definitive(err) || !tr.Held(ada) {
		t.Fatalf("a failed re-judging: err %v definitive %v held %v; want transient and held", err, Definitive(err), tr.Held(ada))
	}
	if !core.isRefused(leafKey{g, 3}) {
		t.Fatal("the leaf after the failing ones was not judged")
	}
	core.mu.Lock()
	delete(core.revalErr, leafKey{g, 2})
	core.mu.Unlock()
	clk.Advance(time.Second)
	waitFor(t, "released by the retry", func() bool { return !tr.Held(ada) })
}

// TestSweepContinuesPastAFailingLeaf (M-2): one leaf whose op 13 always fails does not stop the sweep.
func TestSweepContinuesPastAFailingLeaf(t *testing.T) {
	core := newFakeCore()
	g := uid(0x91)
	core.member(g, 1, uid(1), uid(0xa1))
	core.member(g, 2, uid(2), uid(0xb1))
	core.mu.Lock()
	core.version[uid(1)]++
	core.version[uid(2)]++
	core.revoked[uid(0xb1)] = true
	core.mu.Unlock()
	core.revalErr = map[leafKey]error{{g, 1}: errors.New("helper timeout")}
	tr := New(core, testPolicy)
	defer tr.Close()
	if err := tr.Sweep(t.Context()); err == nil {
		t.Fatal("a failing leaf was not reported")
	}
	if !core.isRefused(leafKey{g, 2}) {
		t.Fatal("the sweep stopped at the failing leaf")
	}
}

// TestLeftGroupsCostNoTrustWork (M-5): a group the Bot left (not a Bot channel) is neither re-judged on a push nor by a
// sweep.
func TestLeftGroupsCostNoTrustWork(t *testing.T) {
	core := newFakeCore()
	ada := uid(1)
	bot, left := uid(0x91), uid(0x92)
	core.member(bot, 1, ada, uid(0xa1))
	core.member(left, 1, ada, uid(0xa1))
	tr := New(core, testPolicy).WithGroups(func(g id.ID) bool { return g != left })
	defer tr.Close()
	if _, err := tr.Pushed(t.Context(), ada, push()); err != nil {
		t.Fatal(err)
	}
	if err := tr.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, revalid, _ := core.calls(); revalid[ada] != 1 {
		t.Fatalf("revalidations %d, want 1 (the Bot channel's leaf only)", revalid[ada])
	}
}

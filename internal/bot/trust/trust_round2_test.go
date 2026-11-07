package trust

// Task 16 fix round 2: per-user push generations (N-1), a retry left without a waiter by a refused push is retried
// (N-3), and every applied change is reported to the caller (N-4).

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

func retryOf(tr *Trust, user id.ID) *pendingRetry {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.retries[user]
}

func idle(tr *Trust, user id.ID) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	w := tr.work[user]
	return w == nil || !w.running && w.queued == nil
}

// TestAnOlderRetryNeverReleasesAUserWhoseRevocationFailed (N-1, the reviewer's probe): push N fails transiently and
// its retry is due; the revocation N+1 is mid-call when that retry fires and queues behind it; N+1 then fails
// transiently and N's retry would now succeed. The stale retry is dropped: the user stays held, the revocation's retry
// is pending, and the revocation applies only through it.
func TestAnOlderRetryNeverReleasesAUserWhoseRevocationFailed(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada, phone := uid(1), uid(0xa1)
	core.member(uid(0x91), 1, ada, phone)
	older, revocation := []byte{0x01, 0x02}, push(phone)
	gate := make(chan struct{})
	core.pushFail = map[string]error{string(older): &helper.StatusError{Status: 6}, string(revocation): &helper.StatusError{Status: 6}}
	core.blockPush = map[string]chan struct{}{string(revocation): gate}
	tr := New(core, testPolicy).WithClock(clk).WithInlineBudget(20 * time.Millisecond)
	defer tr.Close()
	_, _ = tr.Pushed(t.Context(), ada, older)
	core.mu.Lock()
	delete(core.pushFail, string(older)) // N's retry would now succeed
	core.mu.Unlock()
	if _, err := tr.Pushed(t.Context(), ada, revocation); !errors.Is(err, ErrDeferred) {
		t.Fatalf("the revocation mid-call: %v", err)
	}
	clk.Advance(time.Second)          // N's retry fires and queues behind the revocation
	time.Sleep(20 * time.Millisecond) // (before the fix it queued here; now it is dropped as stale)
	core.mu.Lock()
	delete(core.blockPush, string(revocation))
	core.mu.Unlock()
	close(gate) // the revocation fails transiently
	waitFor(t, "the worker idle", func() bool { return idle(tr, ada) })
	r := retryOf(tr, ada)
	if !tr.Held(ada) || r == nil || string(r.push) != string(revocation) {
		t.Fatalf("held %v, retry %v; want held with the revocation's retry pending", tr.Held(ada), r)
	}
	if core.isRefused(leafKey{uid(0x91), 1}) {
		t.Fatal("the revocation applied without its retry")
	}
	core.mu.Lock()
	delete(core.pushFail, string(revocation))
	core.mu.Unlock()
	clk.Advance(time.Second)
	waitFor(t, "the revocation applied by its retry", func() bool { return core.isRefused(leafKey{uid(0x91), 1}) && !tr.Held(ada) })
}

// TestARetryLeftWaitingByARefusedPushIsRetried (N-3): N's retry fires while a newer push waits behind a running one,
// so it is not queued. Both newer pushes are then refused outright. N is live again and is retried on its own, without
// a fresh helper, and its success releases the user.
func TestARetryLeftWaitingByARefusedPushIsRetried(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada, phone := uid(1), uid(0xa1)
	core.member(uid(0x91), 1, ada, phone)
	revocation, junk1, junk2 := push(phone), []byte{0xf1}, []byte{0xf2}
	gate := make(chan struct{})
	core.pushFail = map[string]error{string(revocation): &helper.StatusError{Status: 6},
		string(junk1): &helper.StatusError{Status: 1}, string(junk2): &helper.StatusError{Status: 1}}
	core.blockPush = map[string]chan struct{}{string(junk1): gate}
	tr := New(core, testPolicy).WithClock(clk).WithInlineBudget(20 * time.Millisecond)
	defer tr.Close()
	_, _ = tr.Pushed(t.Context(), ada, revocation)
	_, _ = tr.Pushed(t.Context(), ada, junk1) // mid-call
	_, _ = tr.Pushed(t.Context(), ada, junk2) // waits behind it
	clk.Advance(time.Second)                  // the revocation's retry fires: a newer push waits, so it is not queued
	time.Sleep(10 * time.Millisecond)
	core.mu.Lock()
	delete(core.pushFail, string(revocation))
	delete(core.blockPush, string(junk1))
	core.mu.Unlock()
	close(gate) // junk1 then junk2 are refused outright
	waitFor(t, "the revocation retried after the refusals", func() bool {
		clk.Advance(100 * time.Millisecond)
		return core.isRefused(leafKey{uid(0x91), 1}) && !tr.Held(ada)
	})
}

// TestAStaleFailureBecomesLiveAgainWhenTheNewerPushIsRefused (N-1, N-3): the revocation N is mid-call when a newer push
// N+1 queues; N fails transiently (stale, so no retry yet), then N+1 is refused outright. N is live again: the user
// stays held and N is retried until it applies.
func TestAStaleFailureBecomesLiveAgainWhenTheNewerPushIsRefused(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	core := newFakeCore()
	ada, phone := uid(1), uid(0xa1)
	core.member(uid(0x91), 1, ada, phone)
	revocation, junk := push(phone), []byte{0xf1}
	gate := make(chan struct{})
	core.pushFail = map[string]error{string(revocation): &helper.StatusError{Status: 6}, string(junk): &helper.StatusError{Status: 1}}
	core.blockPush = map[string]chan struct{}{string(revocation): gate}
	tr := New(core, testPolicy).WithClock(clk).WithInlineBudget(20 * time.Millisecond)
	defer tr.Close()
	_, _ = tr.Pushed(t.Context(), ada, revocation)
	_, _ = tr.Pushed(t.Context(), ada, junk)
	core.mu.Lock()
	delete(core.blockPush, string(revocation))
	core.mu.Unlock()
	close(gate)
	waitFor(t, "the worker idle", func() bool { return idle(tr, ada) })
	if r := retryOf(tr, ada); !tr.Held(ada) || r == nil || string(r.push) != string(revocation) {
		t.Fatalf("held %v, retry %v; want held with the revocation retried", tr.Held(ada), r)
	}
	core.mu.Lock()
	delete(core.pushFail, string(revocation))
	core.mu.Unlock()
	clk.Advance(time.Second)
	waitFor(t, "the revocation applied", func() bool { return core.isRefused(leafKey{uid(0x91), 1}) && !tr.Held(ada) })
}

// TestAppliedChangesAreReported (N-4): a push that applies and changes the list is reported to the caller, also when it
// applied after Pushed returned ErrDeferred; a push that changes nothing is not.
func TestAppliedChangesAreReported(t *testing.T) {
	core := newFakeCore()
	ada := uid(1)
	core.member(uid(0x91), 1, ada, uid(0xa1))
	gate := make(chan struct{})
	slow := []byte{0x51}
	core.blockPush = map[string]chan struct{}{string(slow): gate}
	var reports atomic.Int64
	tr := New(core, testPolicy).WithInlineBudget(20 * time.Millisecond).
		WithApplied(func(_ context.Context, u id.ID, got native.DeviceListIngest) {
			if u == ada && got.Changed {
				reports.Add(1)
			}
		})
	defer tr.Close()
	if _, err := tr.Pushed(t.Context(), ada, slow); !errors.Is(err, ErrDeferred) {
		t.Fatalf("slow push: %v", err)
	}
	core.mu.Lock()
	delete(core.blockPush, string(slow))
	core.mu.Unlock()
	close(gate)
	waitFor(t, "the deferred change reported", func() bool { return reports.Load() == 1 })
	if _, err := tr.Pushed(t.Context(), ada, slow); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if reports.Load() != 1 {
		t.Fatalf("an unchanged push was reported: %d", reports.Load())
	}
}

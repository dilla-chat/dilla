//go:build linux

package main

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Other gateway hints refresh text sources at most once per five seconds.
func TestGeneralGatewayWakeStaysThrottled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		wake := make(chan struct{}, 1)
		var polls []time.Time
		start := time.Now()
		w := reconcileWait{interval: 30 * time.Second, wake: wake,
			poll: func(context.Context) { polls = append(polls, time.Now()) },
			now:  time.Now, after: time.After}
		done := make(chan bool, 1)
		go func() { done <- w.wait(ctx, start.Add(-time.Second)) }()
		for elapsed := time.Duration(0); elapsed < 9*time.Second; elapsed += 100 * time.Millisecond {
			wake <- struct{}{}
			synctest.Wait()
			time.Sleep(100 * time.Millisecond)
		}
		synctest.Wait()
		if len(polls) != 2 || polls[0].Sub(start) != 4*time.Second || polls[1].Sub(start) != 9*time.Second {
			t.Fatalf("general polls under sustained hints = %v, want at 4s and 9s", polls)
		}
		cancel()
		if <-done {
			t.Fatal("cancelled wait requested another reconciliation")
		}
	})
}

// Text polls must not postpone the full reconciliation safety net.
func TestReconcileIntervalEndsTheWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		wake := make(chan struct{}, 1)
		polls := 0
		w := reconcileWait{interval: 30 * time.Second, wake: wake,
			poll: func(context.Context) { polls++ }, now: time.Now, after: time.After}
		done := make(chan bool, 1)
		go func() { done <- w.wait(t.Context(), start) }()
		synctest.Wait()
		for range 5 {
			wake <- struct{}{}
			synctest.Wait()
			time.Sleep(5 * time.Second)
			synctest.Wait()
		}
		time.Sleep(5 * time.Second)
		if !<-done || time.Since(start) != 30*time.Second || polls != 5 {
			t.Fatalf("interval wait: elapsed=%s, text polls=%d", time.Since(start), polls)
		}
	})
}

func TestReconcileWaitStopsOnCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		w := reconcileWait{interval: time.Hour, now: time.Now, after: time.After}
		done := make(chan bool, 1)
		go func() { done <- w.wait(ctx, time.Now()) }()
		synctest.Wait()
		cancel()
		if <-done {
			t.Fatal("cancelled wait requested another reconciliation")
		}
	})
}

func TestInteractionBudgetBurstAndTwoTokensPerSecond(t *testing.T) {
	now := time.Unix(1800000000, 0)
	var budget interactionBudget
	budget.spend(now, 10)
	if delay := budget.delay(now); delay != 500*time.Millisecond {
		t.Fatalf("empty burst delay=%s, want 500ms", delay)
	}
	if delay := budget.delay(now.Add(250 * time.Millisecond)); delay != 250*time.Millisecond {
		t.Fatalf("partial refill delay=%s", delay)
	}
	if delay := budget.delay(now.Add(500 * time.Millisecond)); delay > 0 {
		t.Fatalf("refilled token delay=%s", delay)
	}
	budget.spend(now.Add(500*time.Millisecond), 1)
	if delay := budget.delay(now.Add(500 * time.Millisecond)); delay != 500*time.Millisecond {
		t.Fatalf("second token delay=%s", delay)
	}
	budget.refill(now.Add(time.Hour))
	if budget.tokens != 10 {
		t.Fatalf("idle burst=%v, want cap10", budget.tokens)
	}
}

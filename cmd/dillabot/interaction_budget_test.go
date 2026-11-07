//go:build linux

package main

import (
	"testing"
	"time"
)

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

package youtube

import (
	"context"
	"testing"
	"time"
)

func TestWarmResolverReplacesTheWorkerBeforeItsEgressBudgetRunsOut(t *testing.T) {
	first, second := newFakeLookup(), newFakeLookup()
	r, _, _, starts := testWarm(t, first, second)
	r.recycleAfter, r.retireGrace = 3, 20*time.Millisecond
	for i := 0; i < 5; i++ {
		if _, err := r.Resolve(context.Background(), "song number "+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if first.calls.Load() != 3 || second.calls.Load() != 2 || *starts != 2 {
		t.Fatalf("first=%d second=%d starts=%d, want 3, 2 and 2", first.calls.Load(), second.calls.Load(), *starts)
	}
	deadline := time.After(2 * time.Second)
	for !first.closed.Load() {
		select {
		case <-deadline:
			t.Fatal("the retired worker was never closed")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

package client

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// blockingHints is a HintSender that blocks every send until released, and counts the sends per scope.
type blockingHints struct {
	release chan struct{}
	mu      sync.Mutex
	sent    map[id.ID]int
	total   int
}

func (b *blockingHints) Hint(ctx context.Context, _ uint8, scope id.ID) error {
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent[scope]++
	b.total++
	return nil
}

func (b *blockingHints) count(scope id.ID) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sent[scope]
}

// Task 17 fix round 2 (I4): a hint never waits on the socket. While the sender is blocked, Hint returns at once,
// repeated hints for one scope coalesce into one (latest wins), and the waiting scopes are bounded.
func TestAsyncHintsNeverBlockAndCoalescePerScope(t *testing.T) {
	inner := &blockingHints{release: make(chan struct{}), sent: map[id.ID]int{}}
	h := NewAsyncHints(inner)
	h.coalesce = 10 * time.Millisecond
	scope := id.New()
	start := time.Now()
	for range 100 {
		if err := h.Hint(context.Background(), StateScopeCall, scope); err != nil {
			t.Fatal(err)
		}
	}
	other := id.New()
	_ = h.Hint(context.Background(), StateScopeChannel, other)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("101 hints with a blocked sender took %v; Hint must not wait", elapsed)
	}
	close(inner.release)
	deadline := time.Now().Add(2 * time.Second)
	for (inner.count(scope) == 0 || inner.count(other) == 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	// The first hint may have been taken before the rest queued; everything after it is one more send.
	if got := inner.count(scope); got < 1 || got > 2 {
		t.Fatalf("100 hints for one scope were sent %d times, want 1 or 2 (coalesced)", got)
	}
	if inner.count(other) != 1 {
		t.Fatalf("the other scope was hinted %d times, want 1", inner.count(other))
	}
}

func TestAsyncHintsAreBounded(t *testing.T) {
	inner := &blockingHints{release: make(chan struct{}), sent: map[id.ID]int{}}
	h := NewAsyncHints(inner)
	defer close(inner.release)
	// The worker takes one batch (all that waited) and blocks on it; the queue then fills again up to the bound.
	for range 2*MaxPendingHints + 10 {
		_ = h.Hint(context.Background(), StateScopeUser, id.New())
	}
	// One batch may be in flight and the rest wait, at most MaxPendingHints of them.
	if dropped := h.Dropped(); dropped < 10 {
		t.Fatalf("dropped %d hints past the bound, want at least 10", dropped)
	}
	h.mu.Lock()
	waiting := len(h.pending)
	h.mu.Unlock()
	if waiting > MaxPendingHints {
		t.Fatalf("%d scopes waiting, want at most %d", waiting, MaxPendingHints)
	}
}

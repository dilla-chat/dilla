package client

import (
	"context"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// HintSender tells dillad that one scope's Bot state changed (Bot v2 Task 17): kind is StateScopeUser (scope is the
// member), StateScopeChannel (a Bot channel) or StateScopeCall (the voice channel). A hint carries no content; dillad
// raises the scope's revision and pushes op 63 to its audience.
type HintSender interface {
	Hint(ctx context.Context, kind uint8, scope id.ID) error
}

// SessionHints sends hints over a Bot session's state-hints route.
type SessionHints struct{ Session *Session }

// Hint implements HintSender.
func (h SessionHints) Hint(ctx context.Context, kind uint8, scope id.ID) error {
	_, err := h.Session.StateHint(ctx, kind, scope)
	return err
}

const (
	// HintCoalesce is the clients' hint coalescing window (spec "Hint coalescing 250 ms"): a scope is hinted at most
	// once per window, the latest change winning.
	HintCoalesce = 250 * time.Millisecond
	// HintTimeout bounds one hint's request.
	HintTimeout = 2 * time.Second
	// MaxPendingHints bounds the scopes waiting to be hinted; past it a new scope's hint is dropped (a hint is best
	// effort: a client compares revisions at connect).
	MaxPendingHints = 1024
)

type hintScope struct {
	kind  uint8
	scope id.ID
}

// AsyncHints sends hints off the caller's goroutine (Bot v2 Task 17 fix round 2, I4): Hint never blocks, so the music
// supervisor's save path and a command's reply never wait on a slow or reconnecting socket. Hints for the same scope
// coalesce (latest wins) into one per HintCoalesce; at most MaxPendingHints scopes wait. One worker sends them, and
// it exits once nothing is pending, so an AsyncHints that is dropped leaks nothing.
type AsyncHints struct {
	inner HintSender
	// coalesce and timeout are HintCoalesce and HintTimeout (tests shorten them).
	coalesce, timeout time.Duration

	mu      sync.Mutex
	pending map[hintScope]struct{}
	order   []hintScope
	running bool
	dropped uint64
}

// NewAsyncHints sends inner's hints asynchronously, coalesced.
func NewAsyncHints(inner HintSender) *AsyncHints {
	return &AsyncHints{inner: inner, coalesce: HintCoalesce, timeout: HintTimeout, pending: map[hintScope]struct{}{}}
}

// Hint queues one hint and returns at once. It reports no error: the send happens later and is best effort.
func (h *AsyncHints) Hint(_ context.Context, kind uint8, scope id.ID) error {
	if h == nil || h.inner == nil || scope.IsZero() {
		return nil
	}
	key := hintScope{kind, scope}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, queued := h.pending[key]; !queued {
		if len(h.pending) >= MaxPendingHints {
			h.dropped++
			return nil
		}
		h.pending[key] = struct{}{}
		h.order = append(h.order, key)
	}
	if !h.running {
		h.running = true
		// The worker outlives the caller's request on purpose: it sends what is pending, then exits.
		go h.run() //nolint:gosec // G118: detached by design; each send has its own HintTimeout
	}
	return nil
}

// Dropped is how many hints were dropped because MaxPendingHints scopes were already waiting.
func (h *AsyncHints) Dropped() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}

func (h *AsyncHints) run() {
	for {
		h.mu.Lock()
		batch := h.order
		h.order = nil
		clear(h.pending)
		if len(batch) == 0 {
			h.running = false
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()
		for _, key := range batch {
			ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
			_ = h.inner.Hint(ctx, key.kind, key.scope)
			cancel()
		}
		// The window: what changes meanwhile is sent once, after it.
		time.Sleep(h.coalesce)
	}
}

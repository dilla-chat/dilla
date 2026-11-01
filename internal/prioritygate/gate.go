// Package prioritygate provides cancellable, prioritized admission to a bounded
// number of resources. Running work is never interrupted by higher priority work.
package prioritygate

import (
	"context"
	"errors"
	"sync"
)

var ErrClosed = errors.New("priority gate is closed")

// ErrDepleted means all resources have been permanently retired.
var ErrDepleted = errors.New("priority gate has no resources")

type waiter struct {
	ctx      context.Context
	priority int
	ready    chan struct{}
	granted  bool
	err      error
}

// Gate admits higher numeric priorities first, preserving arrival order within
// a priority. Its mutex protects only admission bookkeeping, never resource use.
type Gate struct {
	mu       sync.Mutex
	capacity int
	active   int
	waiters  []*waiter
	closed   bool
	done     chan struct{}
}

func New(capacity int) *Gate {
	if capacity < 1 {
		panic("prioritygate: capacity must be positive")
	}
	return &Gate{capacity: capacity, done: make(chan struct{})}
}

// Acquire waits for a resource. The returned release function is idempotent.
// Cancellation before admission never consumes a resource or interrupts others.
func (g *Gate) Acquire(ctx context.Context, priority int) (func(), error) {
	g.mu.Lock()
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	if g.closed {
		g.mu.Unlock()
		return nil, ErrClosed
	}
	if g.capacity == 0 {
		g.mu.Unlock()
		return nil, ErrDepleted
	}
	w := &waiter{ctx: ctx, priority: priority, ready: make(chan struct{})}
	g.waiters = append(g.waiters, w)
	g.dispatchLocked()
	g.mu.Unlock()

	select {
	case <-w.ready:
	case <-ctx.Done():
	}
	g.mu.Lock()
	if err := ctx.Err(); err != nil {
		if w.granted {
			g.releaseLocked()
		} else {
			g.removeLocked(w)
		}
		g.mu.Unlock()
		return nil, err
	}
	err := w.err
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.releaseLocked()
		})
	}, nil
}

func (g *Gate) removeLocked(w *waiter) {
	for n, queued := range g.waiters {
		if queued == w {
			copy(g.waiters[n:], g.waiters[n+1:])
			g.waiters[len(g.waiters)-1] = nil
			g.waiters = g.waiters[:len(g.waiters)-1]
			return
		}
	}
}

func (g *Gate) dispatchLocked() {
	for !g.closed && g.active < g.capacity && len(g.waiters) > 0 {
		best := 0
		for n, w := range g.waiters {
			if w.priority > g.waiters[best].priority {
				best = n
			}
		}
		w := g.waiters[best]
		g.removeLocked(w)
		if err := w.ctx.Err(); err != nil {
			w.err = err
		} else {
			w.granted = true
			g.active++
		}
		close(w.ready)
	}
}

func (g *Gate) releaseLocked() {
	g.active--
	if g.closed {
		if g.active == 0 {
			close(g.done)
		}
		return
	}
	g.dispatchLocked()
}

// Retire permanently removes one resource. The caller must still hold that
// resource's admission and call its release function afterwards. Reducing
// capacity before releasing prevents a queued caller from being admitted for
// the lost resource. Retirement never interrupts owners of surviving resources.
func (g *Gate) Retire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.capacity == 0 {
		panic("prioritygate: no resource to retire")
	}
	g.capacity--
	if g.capacity == 0 {
		for _, w := range g.waiters {
			w.err = ErrDepleted
			close(w.ready)
		}
		g.waiters = nil
	}
}

// Close rejects new and queued acquisitions immediately. Its returned channel
// closes after all admitted work releases its resources. Close is idempotent.
func (g *Gate) Close() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.closed = true
		for _, w := range g.waiters {
			w.err = ErrClosed
			close(w.ready)
		}
		g.waiters = nil
		if g.active == 0 {
			close(g.done)
		}
	}
	return g.done
}

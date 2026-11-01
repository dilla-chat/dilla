package prioritygate

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitQueued(t *testing.T, g *Gate, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		n := len(g.waiters)
		g.mu.Unlock()
		if n == count {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("gate did not reach %d queued requests", count)
}

func TestPriorityAndFIFO(t *testing.T) {
	g := New(1)
	release, err := g.Acquire(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	type admitted struct {
		name    string
		release func()
	}
	got := make(chan admitted)
	requests := []struct {
		name     string
		priority int
	}{{"background1", 0}, {"background2", 0}, {"interactive1", 1}, {"control", 2}, {"interactive2", 1}}
	for n, request := range requests {
		go func() {
			release, err := g.Acquire(t.Context(), request.priority)
			if err != nil {
				t.Error(err)
				return
			}
			got <- admitted{request.name, release}
		}()
		waitQueued(t, g, n+1)
	}
	release()
	for _, want := range []string{"control", "interactive1", "interactive2", "background1", "background2"} {
		select {
		case result := <-got:
			if result.name != want {
				t.Errorf("admission = %q, want %q", result.name, want)
			}
			result.release()
		case <-time.After(3 * time.Second):
			t.Fatal("admission stalled")
		}
	}
}

func TestQueuedCancellationDoesNotInterruptActiveWork(t *testing.T) {
	g := New(1)
	release, err := g.Acquire(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := g.Acquire(ctx, 2); done <- err }()
	waitQueued(t, g, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = %v", err)
	}
	waitQueued(t, g, 0)
	g.mu.Lock()
	active := g.active
	g.mu.Unlock()
	if active != 1 {
		t.Fatalf("active resource count = %d", active)
	}
	release()
	next, err := g.Acquire(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	next()
}

func TestCloseRejectsQueuedWorkAndWaitsForActiveRelease(t *testing.T) {
	g := New(1)
	release, err := g.Acquire(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	waiterDone := make(chan error, 1)
	go func() { _, err := g.Acquire(t.Context(), 2); waiterDone <- err }()
	waitQueued(t, g, 1)
	done := g.Close()
	if err := <-waiterDone; !errors.Is(err, ErrClosed) {
		t.Fatalf("queued acquisition after close = %v", err)
	}
	if _, err := g.Acquire(t.Context(), 3); !errors.Is(err, ErrClosed) {
		t.Fatalf("new acquisition after close = %v", err)
	}
	select {
	case <-done:
		t.Fatal("shutdown completed with a resource still active")
	default:
	}
	release()
	release()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not complete")
	}
	if g.Close() != done {
		t.Fatal("close is not idempotent")
	}
}

func TestConcurrentAdmissionCancellationAndClose(t *testing.T) {
	for round := range 10 {
		g := New(4)
		var active atomic.Int32
		var wg sync.WaitGroup
		for n := range 64 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 16 {
					ctx, cancel := context.WithCancel(t.Context())
					if (n+round)%3 == 0 {
						cancel()
					}
					release, err := g.Acquire(ctx, n%3)
					cancel()
					if err != nil {
						if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrClosed) {
							t.Error(err)
						}
						continue
					}
					if count := active.Add(1); count > 4 {
						t.Errorf("%d resources in use, capacity is 4", count)
					}
					runtime.Gosched()
					active.Add(-1)
					release()
					release()
				}
			}()
		}
		runtime.Gosched()
		done := g.Close()
		wg.Wait()
		<-done
	}
}

func TestRetirementPreservesPriorityAtReducedCapacity(t *testing.T) {
	g := New(2)
	failed, err := g.Acquire(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	surviving, err := g.Acquire(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer surviving()
	type result struct {
		priority int
		release  func()
		err      error
	}
	admitted := make(chan result, 2)
	for n, priority := range []int{0, 2} {
		go func() {
			release, err := g.Acquire(t.Context(), priority)
			admitted <- result{priority, release, err}
		}()
		waitQueued(t, g, n+1)
	}
	g.Retire()
	failed()
	failed()            // Releasing a retired admission must still be idempotent.
	waitQueued(t, g, 2) // No caller can consume the lost resource.
	surviving()
	for _, want := range []int{2, 0} {
		select {
		case got := <-admitted:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.priority != want {
				t.Errorf("priority = %d, want %d", got.priority, want)
			}
			got.release()
		case <-time.After(3 * time.Second):
			t.Fatal("surviving capacity did not admit work")
		}
	}
	<-g.Close()
}

func TestRetirementOfFinalResourceRejectsWaitersAndPreservesClose(t *testing.T) {
	g := New(1)
	release, err := g.Acquire(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan error, 1)
	go func() { _, err := g.Acquire(t.Context(), 2); queued <- err }()
	waitQueued(t, g, 1)
	g.Retire()
	if err := <-queued; !errors.Is(err, ErrDepleted) {
		t.Fatalf("queued acquisition = %v, want ErrDepleted", err)
	}
	if _, err := g.Acquire(t.Context(), 2); !errors.Is(err, ErrDepleted) {
		t.Fatalf("new acquisition = %v, want ErrDepleted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := g.Acquire(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquisition = %v, want context.Canceled", err)
	}
	done := g.Close()
	select {
	case <-done:
		t.Fatal("Close completed before retired owner released")
	default:
	}
	release()
	release()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not complete")
	}
	if _, err := g.Acquire(t.Context(), 2); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed depleted gate acquisition = %v, want ErrClosed", err)
	}
	if g.Close() != done {
		t.Fatal("Close is not idempotent")
	}
}

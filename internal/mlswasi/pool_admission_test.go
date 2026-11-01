package mlswasi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func TestPoolAdmissionCancellationAndShutdown(t *testing.T) {
	var priorities atomic.Int32
	r := newTestRuntime(t, Options{PoolSize: 1, Priority: func(context.Context) int {
		priorities.Add(1)
		return 1
	}})
	held, err := r.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	ctx, cancel := context.WithCancel(t.Context())
	waiterDone := make(chan error, 1)
	go func() { _, err := r.Acquire(ctx); waiterDone <- err }()
	cancel()
	if err := <-waiterDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquisition = %v", err)
	}
	if priorities.Load() != 2 {
		t.Fatal("pool did not apply its priority callback to acquisitions")
	}
	go func() { _, err := r.Acquire(t.Context()); waiterDone <- err }()
	closeDone := make(chan error, 1)
	go func() { closeDone <- r.Close(context.Background()) }()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("acquisition during shutdown = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not wake queued acquisition")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		// Ensure a failure can still complete test cleanup.
		held.Release()
		<-closeDone
		t.Fatal("shutdown waited for a retained instance instead of closing its module")
	}
	held.Release()
	if _, err := r.Acquire(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("acquisition after shutdown = %v", err)
	}
}

func TestCloseReturnsWithRetainedInstanceAndRejectsUse(t *testing.T) {
	r := newTestRuntime(t, Options{PoolSize: 1})
	held, err := r.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	closed := make(chan error, 1)
	go func() { closed <- r.Close(context.Background()) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		held.Release()
		<-closed
		t.Fatal("Close blocked on a retained DS group instance")
	}
	if _, err := held.Call(t.Context(), "dilla_abi", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("retained instance call after shutdown = %v, want ErrClosed", err)
	}
	// DS cache cleanup may release an instance after its runtime has closed.
	held.Release()
	held.Release()
	if _, err := held.Call(t.Context(), "dilla_abi", nil); !errors.Is(err, ErrReleased) {
		t.Fatalf("call after late release = %v, want ErrReleased", err)
	}
}

func TestCloseWaitsForActiveCallButNotRetainedInstance(t *testing.T) {
	entered, resume := make(chan struct{}), make(chan struct{})
	r := newTestRuntime(t, Options{PoolSize: 1, OnCall: func(string) {
		close(entered)
		<-resume
	}})
	held, err := r.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	req, err := encodeRequest()
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan error, 1)
	go func() { _, err := held.Call(t.Context(), "dilla_abi", req); called <- err }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- r.Close(context.Background()) }()
	<-r.done // Shutdown has rejected new work before waiting for the active call.
	premature := false
	select {
	case err := <-closed:
		premature = true
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
	}
	close(resume)
	if err := <-called; err != nil {
		t.Errorf("active call interrupted by shutdown: %v", err)
	}
	if premature {
		t.Fatal("Close destroyed the runtime while a Call was still active")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		held.Release()
		<-closed
		t.Fatal("Close waited for retained instance ownership after the call finished")
	}
}

// A replacement may fail even though startup succeeded. Keep the real runtime,
// compiled module and pool, and fail only the resource allocation boundary.
type replacementFailureRuntime struct {
	wazero.Runtime
	entered chan struct{}
	resume  chan struct{}
}

func (r *replacementFailureRuntime) InstantiateModule(context.Context, wazero.CompiledModule, wazero.ModuleConfig) (api.Module, error) {
	if r.entered != nil {
		close(r.entered)
		<-r.resume
	}
	return nil, errors.New("replacement resource allocation failed")
}

func TestReplacementFailureRejectsQueuedAndNewAcquisitions(t *testing.T) {
	r := newTestRuntime(t, Options{PoolSize: 1})
	held, err := r.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	// Cause a real guest trap rather than just toggling the poisoned flag.
	if _, err := held.invoke(t.Context(), "dilla_alloc", held.alloc, 0xffff_ffff); !errors.Is(err, ErrTrap) {
		t.Fatalf("impossible allocation = %v, want ErrTrap", err)
	}
	replacement := &replacementFailureRuntime{Runtime: r.rt, entered: make(chan struct{}), resume: make(chan struct{})}
	r.rt = replacement
	released := make(chan struct{})
	go func() { held.Release(); close(released) }()
	<-replacement.entered
	queued := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	waiting := &admissionWaitContext{Context: ctx, waiting: make(chan struct{})}
	go func() {
		inst, err := r.Acquire(waiting)
		if inst != nil {
			inst.Release()
		}
		queued <- err
	}()
	<-waiting.waiting
	close(replacement.resume)
	<-released
	if err := <-queued; !errors.Is(err, ErrDepleted) {
		t.Errorf("queued acquisition after losing final instance = %v, want depletion error", err)
	}
	ctx, cancelNew := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancelNew()
	if inst, err := r.Acquire(ctx); !errors.Is(err, ErrDepleted) {
		if inst != nil {
			inst.Release()
		}
		t.Errorf("new acquisition after losing final instance = %v, want depletion error", err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Acquire(t.Context()); !errors.Is(err, ErrClosed) {
		t.Errorf("closed depleted runtime acquisition = %v, want ErrClosed", err)
	}
}

// Done is inspected after the gate queues the request and unlocks its mutex.
type admissionWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *admissionWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestReplacementFailureRetainsUsableCapacity(t *testing.T) {
	r := newTestRuntime(t, Options{PoolSize: 2, Priority: func(ctx context.Context) int {
		priority, _ := ctx.Value(replacementPriorityKey{}).(int)
		return priority
	}})
	failed, err := r.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer failed.Release()
	surviving, err := r.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer surviving.Release()
	if _, err := failed.invoke(t.Context(), "dilla_alloc", failed.alloc, 0xffff_ffff); !errors.Is(err, ErrTrap) {
		t.Fatalf("impossible allocation = %v, want ErrTrap", err)
	}
	r.rt = &replacementFailureRuntime{Runtime: r.rt}
	failed.Release()
	type result struct {
		priority int
		inst     *Instance
		err      error
	}
	admitted := make(chan result, 2)
	for _, priority := range []int{0, 2} {
		ctx := context.WithValue(t.Context(), replacementPriorityKey{}, priority)
		waiting := &admissionWaitContext{Context: ctx, waiting: make(chan struct{})}
		go func() {
			inst, err := r.Acquire(waiting)
			admitted <- result{priority, inst, err}
		}()
		<-waiting.waiting
	}
	surviving.Release()
	for _, want := range []int{2, 0} {
		select {
		case got := <-admitted:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.priority != want {
				t.Errorf("admitted priority = %d, want %d", got.priority, want)
			}
			req, err := encodeRequest()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := got.inst.Call(t.Context(), "dilla_abi", req); err != nil {
				t.Errorf("surviving module is unusable: %v", err)
			}
			got.inst.Release()
		case <-time.After(3 * time.Second):
			t.Fatal("acquisition stalled despite surviving capacity")
		}
	}
}

type replacementPriorityKey struct{}

package ds

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
)

type scopedVerifierProbe struct {
	calls int
	err   error
}

func (v *scopedVerifierProbe) DeviceListEntries(context.Context, []byte, []byte, []byte) ([]mlswasi.DeviceListEntry, error) {
	v.calls++
	return nil, v.err
}

// Native signature semantics are covered by real API tests; this verifies the
// lease ownership/lifetime state machine independently of native trap mechanics.
func TestDeviceListScopeRejectsExpiredMismatchCancelledAndPoisonedUse(t *testing.T) {
	for _, mode := range []string{"expired", "mismatch", "owner_cancelled", "caller_cancelled", "poisoned"} {
		t.Run(mode, func(t *testing.T) {
			runtime := &mlswasi.Runtime{}
			other := &mlswasi.Runtime{}
			probe := &scopedVerifierProbe{}
			owner, cancelOwner := context.WithCancel(t.Context())
			defer cancelOwner()
			scope := &deviceListScope{runtime: runtime, verifier: probe, owner: owner}
			scope.active.Store(true)
			caller, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()
			switch mode {
			case "expired":
				scope.close()
			case "mismatch":
				runtime = other
			case "owner_cancelled":
				cancelOwner()
				caller = context.WithoutCancel(owner)
			case "caller_cancelled":
				cancelCaller()
			case "poisoned":
				probe.err = mlswasi.ErrTrap
			}
			if _, err := scope.verify(caller, runtime, nil, nil, id.ID{}); err == nil {
				t.Fatal("invalid borrowed verifier use was accepted")
			}
			if mode == "poisoned" {
				probe.err = nil
				if _, err := scope.verify(caller, runtime, nil, nil, id.ID{}); err == nil {
					t.Fatal("poisoned scope was reused")
				}
			}
			wantCalls := 0
			if mode == "poisoned" {
				wantCalls = 1
			}
			if probe.calls != wantCalls {
				t.Fatalf("native calls=%d want=%d", probe.calls, wantCalls)
			}
		})
	}
}

func TestDeviceListScopeClosesEscapedContextsAndSerializesVerification(t *testing.T) {
	runtime := &mlswasi.Runtime{}
	probe := &scopedVerifierProbe{}
	var escaped context.Context
	err := withBorrowedDeviceListVerifier(t.Context(), runtime, probe, func(ctx context.Context) error {
		escaped = ctx
		const count = 8
		var wg sync.WaitGroup
		results := make(chan error, count)
		for range count {
			wg.Go(func() {
				scope := ctx.Value(deviceListScopeKey{}).(*deviceListScope)
				_, err := scope.verify(ctx, runtime, nil, nil, id.ID{})
				results <- err
			})
		}
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if probe.calls != 8 {
		t.Fatalf("native calls=%d", probe.calls)
	}
	lists := &coreDeviceLists{wasm: runtime}
	if _, err := lists.Verify(context.WithoutCancel(escaped), nil, nil, id.ID{}); !errors.Is(err, ErrDeviceListUnavailable) {
		t.Fatalf("escaped context did not fail closed: %v", err)
	}
	called := false
	if err := lists.WithVerification(escaped, func(context.Context) error { called = true; return nil }); err == nil || called {
		t.Fatalf("expired context reacquired or entered callback: called=%v err=%v", called, err)
	}
}

type drainingVerifierProbe struct {
	entered chan struct{}
	release chan struct{}
	calls   int
}

func (v *drainingVerifierProbe) DeviceListEntries(context.Context, []byte, []byte, []byte) ([]mlswasi.DeviceListEntry, error) {
	v.calls++
	close(v.entered)
	<-v.release
	return nil, nil
}

func TestDeviceListScopeRejectsEscapedQueuedUseWhileOwnerCancellationDrains(t *testing.T) {
	runtime := &mlswasi.Runtime{}
	owner, cancel := context.WithCancel(t.Context())
	defer cancel()
	probe := &drainingVerifierProbe{entered: make(chan struct{}), release: make(chan struct{})}
	scope := &deviceListScope{runtime: runtime, verifier: probe, owner: owner}
	scope.active.Store(true)
	escaped := context.WithValue(context.Background(), deviceListScopeKey{}, scope)
	lists := &coreDeviceLists{wasm: runtime}
	first := make(chan error, 1)
	go func() { _, err := lists.Verify(escaped, nil, nil, id.ID{}); first <- err }()
	<-probe.entered
	queued := make(chan error, 1)
	go func() { _, err := lists.Verify(escaped, nil, nil, id.ID{}); queued <- err }()
	cancel()
	closed := make(chan struct{})
	go func() { scope.close(); close(closed) }()
	// Reject escaped callback entry immediately, while an already-running native
	// call drains. The callback must not wait for or reuse that native instance.
	callback := make(chan error, 1)
	go func() {
		callback <- scope.run(escaped, func(context.Context) error { return errors.New("expired callback entered") })
	}()
	select {
	case err := <-callback:
		if !errors.Is(err, ErrDeviceListUnavailable) {
			t.Errorf("escaped callback result=%v", err)
		}
	case <-time.After(time.Second):
		t.Error("expired callback waited on in-flight native use")
	}
	select {
	case <-closed:
		t.Error("scope closed before in-flight native verification drained")
	default:
	}
	close(probe.release)
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Errorf("owner-cancelled in-flight verification=%v", err)
	}
	if err := <-queued; err == nil {
		t.Error("queued escaped verification reused the guest")
	}
	<-closed
	if probe.calls != 1 {
		t.Fatalf("native calls=%d want exactly the original in-flight call", probe.calls)
	}
}

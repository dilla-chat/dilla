package ds_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/mlswasi"
)

func TestNativeDeviceListScopePreservesSignaturesLifetimeRuntimeAndCancellation(t *testing.T) {
	fixture := newKind3RegistrationFixture(t)
	ctx := t.Context()
	wasm, err := mlswasi.New(ctx, loadWasmForDS(t), mlswasi.Options{PoolSize: 1, CacheDir: sharedWasmCacheDir(t), Now: fixture.h.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wasm.Close(context.Background()) })
	lists := ds.NewDeviceLists(fixture.h.repo, wasm)
	other := ds.NewDeviceLists(fixture.h.repo, fixture.h.wasm)
	user := fixture.request.TrustedPreflight.Expected.ParticipantUserID
	row, err := fixture.h.repo.GetUser(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	list, err := fixture.h.repo.GetDeviceList(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	var escaped context.Context
	if err := lists.WithVerification(ctx, func(scoped context.Context) error {
		escaped = scoped
		if _, err := other.Verify(scoped, list.Blob, row.SSKPub, user); !errors.Is(err, ds.ErrDeviceListUnavailable) {
			return errors.New("mismatched native runtime was accepted")
		}
		if err := lists.WithVerification(scoped, func(nested context.Context) error {
			entries, err := lists.Verify(nested, list.Blob, row.SSKPub, user)
			if err != nil {
				return err
			}
			if len(entries) != 1 {
				return errors.New("signed list entries changed")
			}
			return nil
		}); err != nil {
			return err
		}
		var wg sync.WaitGroup
		results := make(chan error, 4)
		for range 4 {
			wg.Go(func() { _, err := lists.Verify(scoped, list.Blob, row.SSKPub, user); results <- err })
		}
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				return err
			}
		}
		tampered := append([]byte(nil), list.Blob...)
		tampered[len(tampered)-1] ^= 1
		if _, err := lists.Verify(scoped, tampered, row.SSKPub, user); err == nil {
			return errors.New("borrowed verifier accepted a changed signature")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := lists.Verify(context.WithoutCancel(escaped), list.Blob, row.SSKPub, user); !errors.Is(err, ds.ErrDeviceListUnavailable) {
		t.Fatalf("escaped native scope did not fail closed: %v", err)
	}
	called := false
	if err := lists.WithVerification(escaped, func(context.Context) error { called = true; return nil }); err == nil || called {
		t.Fatalf("expired scope callback=%v err=%v", called, err)
	}
	owner, cancelOwner := context.WithCancel(ctx)
	err = lists.WithVerification(owner, func(scoped context.Context) error {
		cancelOwner()
		_, err := lists.Verify(context.WithoutCancel(scoped), list.Blob, row.SSKPub, user)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("owner cancellation was detached: %v", err)
	}
	lease, err := wasm.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	called = false
	err = lists.WithVerification(cancelled, func(context.Context) error { called = true; return nil })
	lease.Release()
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("cancelled acquire callback=%v err=%v", called, err)
	}
	final, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if err := lists.WithVerification(final, func(scoped context.Context) error {
		_, err := lists.Verify(scoped, list.Blob, row.SSKPub, user)
		return err
	}); err != nil {
		t.Fatalf("lease was leaked after cancellation: %v", err)
	}
}

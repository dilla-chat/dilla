package ds_test

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

// A 1-vCPU dillad must serve its first upload. mlswasi sizes its pool from GOMAXPROCS, and the
// state cache keeps up to PoolSize-1 instances for itself (state.go, rule 1): on one CPU that was a
// pool of one, the cache's capacity was clamped up to one, the first read parked the only instance
// in the cache, and every Upload — which takes its own instance — blocked in Runtime.Acquire forever.
// The bounded context turns that hang into a failure.
func TestASingleCPUDeliveryServiceServesAnUploadAfterACachedRead(t *testing.T) {
	prev := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	h := newDSHarnessWithPool(t, 0, 0) // the pool size a production dillad gets
	if got := h.wasm.PoolSize(); got < 2 {
		t.Fatalf("GOMAXPROCS=1 gave a pool of %d instances; the delivery service needs two", got)
	}
	g := h.group(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A read imports the group's state into the cache, which then holds one instance.
	if _, err := h.ds.Tree(ctx, g.id, g.session); err != nil {
		t.Fatalf("Tree on a single-CPU delivery service: %v", err)
	}
	if _, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0); err != nil {
		t.Fatalf("Upload on a single-CPU delivery service: %v", err)
	}
	// And the cached handle still serves the next read.
	if _, err := h.ds.Tree(ctx, g.id, g.session); err != nil {
		t.Fatalf("second Tree on a single-CPU delivery service: %v", err)
	}
}

// A pool of one instance cannot run the delivery service at all: the cache would hold the only
// instance, and the paths that take their own (Upload, proposals, KeyPackage publication, the
// device-list check inside a cached call) would wait on it forever. ds.New refuses it up front
// instead of deadlocking on the first request.
func TestTheDeliveryServiceRefusesAPoolOfOneInstance(t *testing.T) {
	ctx := context.Background()
	wasm, err := mlswasi.New(ctx, loadWasmForDS(t), mlswasi.Options{PoolSize: 1, CacheDir: sharedWasmCacheDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wasm.Close(context.Background()) })
	h := newDSHarness(t)
	_, err = ds.New(ds.Options{Store: h.repo, Wasm: wasm, Clock: h.clk, Keys: testInstanceKeys(t), Policy: ds.DefaultPolicy()})
	if !errors.Is(err, ds.ErrPoolTooSmall) {
		t.Fatalf("ds.New over a pool of one: got %v, want ErrPoolTooSmall", err)
	}
}

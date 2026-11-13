package mlswasi

import (
	"runtime"
	"testing"
)

// The default pool is GOMAXPROCS instances, but never fewer than MinDefaultPoolSize: internal/ds
// keeps up to PoolSize-1 instances in its state cache and needs one more for the paths that take
// their own, so a 1-vCPU host defaulting to one instance deadlocked on its first upload.
func TestTheDefaultPoolHasAtLeastTwoInstancesOnOneCPU(t *testing.T) {
	prev := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })

	r := newTestRuntime(t, Options{})
	if got := r.PoolSize(); got != MinDefaultPoolSize {
		t.Fatalf("PoolSize() on GOMAXPROCS=1 = %d, want %d", got, MinDefaultPoolSize)
	}
}

// An explicit size is the caller's: dillabot runs one instance on purpose and has no state cache.
func TestAnExplicitPoolSizeOfOneIsKept(t *testing.T) {
	r := newTestRuntime(t, Options{PoolSize: 1})
	if got := r.PoolSize(); got != 1 {
		t.Fatalf("PoolSize() = %d, want the explicit 1", got)
	}
}

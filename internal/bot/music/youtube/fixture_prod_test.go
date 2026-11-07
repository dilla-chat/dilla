//go:build !dilla_testhost

package youtube

import (
	"errors"
	"testing"
)

func TestFixtureResolverRefusedInProductionBuild(t *testing.T) {
	t.Setenv(FixturesEnv, t.TempDir())
	backend, err := NewBackend(SandboxConfig{}, 0)
	if !errors.Is(err, ErrFixturesRefused) || backend != nil {
		t.Fatalf("NewBackend with %s set = (%v, %v), want ErrFixturesRefused", FixturesEnv, backend, err)
	}
	// The local stream hook is how a fixture file would be read; it must not exist here.
	if localStream != nil {
		t.Fatal("a production build carries the local fixture stream hook")
	}
}

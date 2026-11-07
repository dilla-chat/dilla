//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A supervised Bot's tuning file holds reconcile_interval and [rate_limits] only, is its own and owner-only, and may
// be absent.
func TestSupervisedTuningFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, supervisedTuningFile)
	c := config{ReconcileInterval: supervisedReconcileTime}
	if err := c.loadTuning(path); err != nil || c.ReconcileInterval != supervisedReconcileTime {
		t.Fatalf("no tuning file: %v, %s", err, c.ReconcileInterval)
	}
	write := func(body string, mode os.FileMode) {
		t.Helper()
		_ = os.Remove(path)
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("reconcile_interval = \"5s\"\n[rate_limits]\nwrite_per_second = 1000\nwrite_burst = 1000\n", 0o600)
	if err := c.loadTuning(path); err != nil {
		t.Fatalf("loadTuning: %v", err)
	}
	if c.ReconcileInterval != 5*time.Second || c.RateLimits.WritePerSecond != 1000 || c.RateLimits.WriteBurst != 1000 {
		t.Fatalf("tuning = %s, %+v", c.ReconcileInterval, c.RateLimits)
	}
	for _, tc := range []struct {
		body string
		mode os.FileMode
	}{
		{"base_url = \"https://elsewhere.example\"\n", 0o600},
		{"helper_executable = \"/tmp/evil\"\n", 0o600},
		{"reconcile_interval = \"10ms\"\n", 0o600},
		{"reconcile_interval = \"5s\"\n", 0o640},
	} {
		write(tc.body, tc.mode)
		if err := c.loadTuning(path); !errors.Is(err, errConfigInvalid) {
			t.Errorf("tuning %q (mode %04o): %v, want errConfigInvalid", tc.body, tc.mode, err)
		}
	}
}

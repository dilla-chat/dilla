//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// doctorConfig writes a config that passes every other doctor check, and returns its path and state directory.
func doctorConfig(t *testing.T) (configPath, stateDir string) {
	t.Helper()
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	stateDir = filepath.Join(root, "state")
	for _, dir := range []string{etc, stateDir, filepath.Join(stateDir, "04040404040404040404040404040404")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	credential := filepath.Join(etc, "runtime.token")
	if err := os.WriteFile(credential, []byte("dbr_"+base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(root, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(root, "identity.wasm")
	if err := os.WriteFile(module, []byte("fixture wasm"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(etc, "bot.toml")
	config := "base_url = \"https://dilla.example\"\n" +
		"instance_id = \"01010101010101010101010101010101\"\n" +
		"runtime_credential_file = \"" + credential + "\"\n" +
		"helper_executable = \"" + helper + "\"\n" +
		"wasi_module = \"" + module + "\"\n" +
		"state_dir = \"" + stateDir + "\"\n" +
		"reconcile_interval = \"30s\"\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	old := doctorWASI
	doctorWASI = func(context.Context, []byte) error { return nil }
	t.Cleanup(func() { doctorWASI = old })
	return configPath, stateDir
}

// Task 17: `dillabot doctor` says since when the running Bot's commands fail, from the last health report it left.
func TestDoctorPrintsCommandsFailingSince(t *testing.T) {
	configPath, stateDir := doctorConfig(t)
	installation := filepath.Join(stateDir, "04040404040404040404040404040404")
	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-3 * time.Minute)

	run := func() (int, string) {
		var stdout, stderr bytes.Buffer
		code := runDoctorCommand([]string{"--config", configPath}, &stdout, &stderr)
		return code, stdout.String()
	}

	// No report yet: a warning, not a failure.
	if code, out := run(); code != 0 || !strings.Contains(out, "commands WARN") {
		t.Fatalf("no report: exit %d\n%s", code, out)
	}
	// Commands work.
	if err := writeHealthState(installation, healthState{ProbeOK: true, ProbeAt: now, ReportedAt: now}); err != nil {
		t.Fatal(err)
	}
	if code, out := run(); code != 0 || !strings.Contains(out, "commands OK") {
		t.Fatalf("healthy: exit %d\n%s", code, out)
	}
	// Commands fail since a time: doctor says when, and fails.
	if err := writeHealthState(installation, healthState{ProbeOK: false, ProbeAt: now, FailingSince: since, ReportedAt: now}); err != nil {
		t.Fatal(err)
	}
	code, out := run()
	if code == 0 || !strings.Contains(out, "commands failing since "+since.Format(time.RFC3339)) {
		t.Fatalf("failing: exit %d, want a failure that says \"commands failing since %s\"\n%s", code, since.Format(time.RFC3339), out)
	}
	// The file is owner-only, and a report that is old is the report of a Bot that is not running.
	if info, err := os.Stat(filepath.Join(installation, healthStateFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("health file mode/info = %v, %v; want 0600", info, err)
	}
	stale := now.Add(-time.Hour)
	if err := writeHealthState(installation, healthState{ProbeOK: false, ProbeAt: stale, FailingSince: stale, ReportedAt: stale}); err != nil {
		t.Fatal(err)
	}
	if code, out := run(); code != 0 || strings.Contains(out, "failing since") {
		t.Fatalf("a stale report must not be shown as current: exit %d\n%s", code, out)
	}
}

func TestHealthStateRoundTripsAndRefusesGarbage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	in := healthState{ProbeOK: false, ProbeAt: now, FailingSince: now.Add(-time.Minute), ReportedAt: now}
	out, ok := decodeHealthState(in.encode())
	if !ok || out != in {
		t.Fatalf("round trip = %+v %v, want %+v", out, ok, in)
	}
	for _, bad := range []string{"", "probe_ok=2\nprobe_at=1\nfailing_since=-\nreported_at=1\n", "probe_ok=1\n", "x\n"} {
		if _, ok := decodeHealthState(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

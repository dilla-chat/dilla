//go:build windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsInitialFinalizationReleasesOrdinaryLaunchOnlyAfterExactCompleteProgram(t *testing.T) {
	cfg, l, payload := initialNativeFixture(t)
	state, err := executeNativeInitialRepair(l, cfg, payload)
	if err != nil || state != initialCommitted {
		t.Fatal("initial native copy", state, err)
	}
	calls := 0
	launch := func(*selection, []string, bool) (uint32, error) { calls++; return 0, nil }
	if code, e := runLauncher(l, cfg, arguments{mode: "launch"}, new(bytes.Buffer), launch); e != nil || code != 0 || calls != 0 {
		t.Fatal("unfinished setup launched", e)
	}
	pkg, err := verifyNativeInitialComplete(l, cfg, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = finalizeNativeInitialMarker(l, cfg); err != nil {
		_ = pkg.Close()
		t.Fatal(err)
	}
	if err = pkg.Close(); err != nil {
		t.Fatal(err)
	}
	if code, e := runLauncher(l, cfg, arguments{mode: "launch"}, new(bytes.Buffer), launch); e != nil || code != 0 || calls != 1 {
		t.Fatal("finalized setup could not launch", e)
	}
	if raw, e := os.ReadFile(filepath.Join(l.userData, "account.sentinel")); e != nil || string(raw) != "opaque account/KEK/OPFS" {
		t.Fatal("account changed", e)
	}
}

func TestWindowsInitialCompleteCheckHoldsDamagedCommittedProgram(t *testing.T) {
	cfg, l, payload := initialNativeFixture(t)
	if _, err := executeNativeInitialRepair(l, cfg, payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.stableRoot, initialUninstallerName(cfg)), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if pkg, err := verifyNativeInitialComplete(l, cfg, payload); err == nil {
		_ = pkg.Close()
		t.Fatal("accepted damaged complete seed program")
	}
	if _, err := os.Lstat(filepath.Join(l.userData, "initial-install.json")); err != nil {
		t.Fatal("removed unfinished setup marker", err)
	}
}

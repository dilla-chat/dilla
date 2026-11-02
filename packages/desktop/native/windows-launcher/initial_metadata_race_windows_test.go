//go:build windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

func TestWindowsInitialMetadataReclassifiesAllRetainedNamesAfterLatePublication(t *testing.T) {
	for _, fault := range []string{"add-temp", "add-final", "exact-final"} {
		t.Run(fault, func(t *testing.T) {
			cfg, l, payload := initialNativeFixture(t)
			pkg, err := loadInitialPackage(payload, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pkg.Close()
			final, temp := filepath.Join(l.updates, "windows-active-0.json"), filepath.Join(l.updates, initialSeedTemporary)
			if fault == "add-final" {
				if err = os.Rename(final, temp); err != nil {
					t.Fatal(err)
				}
			}
			retry := nativeInitialRetry{l: l, cfg: cfg, payload: payload, pkg: pkg}
			if err = retry.checkMetadata(l); err != nil {
				t.Fatal("pre-publication metadata", err)
			}
			// The former bug checked here, then accepted both fixed names or an exact
			// final seed appearing before the retained file leases were acquired.
			switch fault {
			case "add-temp":
				err = os.WriteFile(temp, pkg.seed[:12], 0600)
			case "add-final":
				err = os.WriteFile(final, pkg.seed[:12], 0600)
			case "exact-final":
				err = os.WriteFile(final, pkg.seed, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			lease, err := desktoplaunch.PinInitialRetryTree(l.updates, map[string]bool{".": true, "windows-active-0.json": false, initialSeedTemporary: false})
			if err != nil {
				t.Fatal(err)
			}
			if err = validateRetainedInitialMetadata(lease, pkg.seed); err == nil {
				_ = lease.Close()
				t.Fatal("late allowed-name authority bypassed repair policy")
			}
			if err = lease.Close(); err != nil {
				t.Fatal(err)
			}
			if fault == "exact-final" {
				raw, e := os.ReadFile(final)
				if e != nil || !bytes.Equal(raw, pkg.seed) {
					t.Fatal("changed published seed", e)
				}
			}
		})
	}
}

func TestWindowsInitialRepairHoldsExactFinalPublishedAfterPrepare(t *testing.T) {
	for _, stage := range []string{"before-program-move", "before-updates-move"} {
		t.Run(stage, func(t *testing.T) {
			cfg, l, payload := initialNativeFixture(t)
			seed, err := os.ReadFile(filepath.Join(payload, "initial-slot.json"))
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(l.stableRoot)
			if err != nil {
				t.Fatal(err)
			}
			retry := &nativeInitialRetry{l: l, cfg: cfg, payload: payload}
			defer func() { _ = retry.pkg.Close() }()
			publish := func() error { return os.WriteFile(filepath.Join(l.updates, "windows-active-0.json"), seed, 0600) }
			var operations initialRetryOperations = mappedInitialRetry{retry, publish}
			if stage == "before-updates-move" {
				operations = lateInitialHistory{retry, publish}
			}
			state, err := runInitialRetryTransaction(operations)
			if err == nil || state != initialPrecommit || retry.programClaim != nil || retry.updatesClaim != nil {
				t.Fatalf("late exact final was recopied: %v %v", state, err)
			}
			after, e := os.Stat(l.stableRoot)
			if e != nil || !os.SameFile(before, after) {
				t.Fatal("original program identity not restored", e)
			}
			raw, e := os.ReadFile(filepath.Join(l.updates, "windows-active-0.json"))
			if e != nil || !bytes.Equal(raw, seed) {
				t.Fatal("late exact authority changed", e)
			}
		})
	}
}

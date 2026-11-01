package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestIncompleteInitialRetryRequiresExactOwnedPackageSubsetAndZeroHistory(t *testing.T) {
	for _, fault := range []string{"partial valid", "zero-filled copy", "root Go mismatch", "runtime mismatch", "unknown root file", "unknown version", "active transaction", "second slot", "release history", "wrong marker", "missing marker"} {
		t.Run(fault, func(t *testing.T) {
			cfg, l := initialInstallation(t)
			payload := t.TempDir()
			if err := os.CopyFS(payload, os.DirFS(l.stableRoot)); err != nil {
				t.Fatal(err)
			}
			self := []byte("the exact incoming packaged native launcher")
			if err := os.WriteFile(filepath.Join(payload, "Dilla.exe"), self, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(l.stableRoot, "Dilla.exe"), self[:12], 0600); err != nil {
				t.Fatal(err)
			}
			slot, err := os.ReadFile(filepath.Join(l.updates, "windows-active-0.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(payload, "initial-slot.json"), slot, 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(l.updates, "windows-active-0.json"), slot[:15], 0600); err != nil {
				t.Fatal(err)
			}
			marker, err := json.Marshal(map[string]any{"format": 1, "kind": "dilla-initial-install", "appId": cfg.AppID, "manifestSha512": cfg.InitialManifestSHA512})
			if err != nil {
				t.Fatal(err)
			}
			markerPath := filepath.Join(l.userData, "initial-install.json")
			if err = os.WriteFile(markerPath, marker, 0600); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(filepath.Join(l.stableRoot, "versions"))
			if err != nil {
				t.Fatal(err)
			}
			id := entries[0].Name()
			switch fault {
			case "zero-filled copy":
				padded := make([]byte, len(self))
				copy(padded, self[:12])
				err = os.WriteFile(filepath.Join(l.stableRoot, "Dilla.exe"), padded, 0600)
			case "root Go mismatch":
				err = os.WriteFile(filepath.Join(l.stableRoot, "Dilla.exe"), []byte("not same image"), 0600)
			case "runtime mismatch":
				err = os.WriteFile(filepath.Join(l.stableRoot, "versions", id, "dilla-desktop.exe"), []byte("wrong program"), 0600)
			case "unknown root file":
				err = os.WriteFile(filepath.Join(l.stableRoot, "personal.txt"), []byte("must not quarantine"), 0600)
			case "unknown version":
				err = os.Mkdir(filepath.Join(l.stableRoot, "versions", "v-unknown"), 0700)
			case "active transaction":
				err = os.WriteFile(filepath.Join(l.updates, "active.json"), []byte("{}"), 0600)
			case "second slot":
				err = os.WriteFile(filepath.Join(l.updates, "windows-active-1.json"), slot, 0600)
			case "release history":
				err = os.WriteFile(filepath.Join(l.updates, "windows-active-0.json"), []byte(`{"kind":"release","generation":1}`), 0600)
			case "wrong marker":
				err = os.WriteFile(markerPath, []byte(`{"format":1,"kind":"dilla-initial-install","appId":"other","manifestSha512":"other"}`), 0600)
			case "missing marker":
				err = os.Remove(markerPath)
			}
			if err != nil {
				t.Fatal(err)
			}
			account := filepath.Join(l.userData, "existing-account.sentinel")
			if err = os.WriteFile(account, []byte("opaque KEK, OPFS and preferences"), 0600); err != nil {
				t.Fatal(err)
			}
			err = incompleteInitial(l, cfg, payload)
			if (err == nil) != (fault == "partial valid" || fault == "zero-filled copy") {
				t.Fatalf("unexpected retry classification: %v", err)
			}
			got, err := os.ReadFile(account)
			if err != nil || string(got) != "opaque KEK, OPFS and preferences" {
				t.Fatal("preflight touched account")
			}
			if _, err = os.Stat(filepath.Join(l.stableRoot, "Dilla.exe")); err != nil {
				t.Fatal("preflight mutated program", err)
			}
		})
	}
}

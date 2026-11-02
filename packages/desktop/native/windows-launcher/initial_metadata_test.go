package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInitialFixedSeedTemporaryIsAuthenticatedAndNeverMixedWithFinal(t *testing.T) {
	for _, fault := range []string{"temp prefix", "temp zero tail", "temp unrelated", "temp directory", "temp hardlink", "final and temp", "unknown temp"} {
		t.Run(fault, func(t *testing.T) {
			cfg, l := initialInstallation(t)
			payload := t.TempDir()
			if err := os.CopyFS(payload, os.DirFS(l.stableRoot)); err != nil {
				t.Fatal(err)
			}
			seed, err := os.ReadFile(filepath.Join(l.updates, "windows-active-0.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(payload, "initial-slot.json"), seed, 0600); err != nil {
				t.Fatal(err)
			}
			for _, root := range []string{payload, l.stableRoot} {
				if err = os.WriteFile(filepath.Join(root, "Dilla.exe"), []byte("same launcher"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			marker, _ := json.Marshal(map[string]any{"format": 1, "kind": "dilla-initial-install", "appId": cfg.AppID, "manifestSha512": cfg.InitialManifestSHA512})
			if err = os.WriteFile(filepath.Join(l.userData, "initial-install.json"), marker, 0600); err != nil {
				t.Fatal(err)
			}
			if fault != "final and temp" {
				if err = os.Remove(filepath.Join(l.updates, "windows-active-0.json")); err != nil {
					t.Fatal(err)
				}
			}
			name := "initial-slot.tmp"
			if fault == "unknown temp" {
				name = "initial-slot-other.tmp"
			}
			path := filepath.Join(l.updates, name)
			raw := seed[:12]
			switch fault {
			case "temp zero tail":
				raw = append([]byte(nil), seed...)
				for i := 12; i < len(raw); i++ {
					raw[i] = 0
				}
			case "temp unrelated":
				raw = []byte("unrelated metadata")
			}
			if fault == "temp directory" {
				err = os.Mkdir(path, 0700)
			} else {
				err = os.WriteFile(path, raw, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if fault == "temp hardlink" {
				if err = os.Link(path, filepath.Join(t.TempDir(), "alias")); err != nil {
					t.Fatal(err)
				}
			}
			err = authorizeInitialStagingMetadata(l, seed)
			if (err == nil) != (fault == "temp prefix" || fault == "temp zero tail") {
				t.Fatalf("unexpected staging authorization: %v", err)
			}
		})
	}
}

func TestIncompleteInitialAcceptsOnlyAuthenticatedFixedTemporary(t *testing.T) {
	cfg, l := initialInstallation(t)
	payload := t.TempDir()
	if err := os.CopyFS(payload, os.DirFS(l.stableRoot)); err != nil {
		t.Fatal(err)
	}
	seed, err := os.ReadFile(filepath.Join(l.updates, "windows-active-0.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(payload, "initial-slot.json"), seed, 0600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{payload, l.stableRoot} {
		if err = os.WriteFile(filepath.Join(root, "Dilla.exe"), []byte("same launcher"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	marker, _ := json.Marshal(map[string]any{"format": 1, "kind": "dilla-initial-install", "appId": cfg.AppID, "manifestSha512": cfg.InitialManifestSHA512})
	if err = os.WriteFile(filepath.Join(l.userData, "initial-install.json"), marker, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(l.updates, "windows-active-0.json")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(l.updates, initialSeedTemporary), seed[:12], 0600); err != nil {
		t.Fatal(err)
	}
	if err = incompleteInitial(l, cfg, payload); err != nil {
		t.Fatal("authenticated prepublication crash residue did not authorize explicit repair", err)
	}
}

func TestInitialInstallationDispositionRequiresCompleteExactAuthority(t *testing.T) {
	for _, fault := range []string{"complete", "exact damaged", "seed prefix", "temporary", "final plus temporary", "other history", "missing marker"} {
		t.Run(fault, func(t *testing.T) {
			cfg, l := initialInstallation(t)
			payload := t.TempDir()
			if err := os.CopyFS(payload, os.DirFS(l.stableRoot)); err != nil {
				t.Fatal(err)
			}
			seed, err := os.ReadFile(filepath.Join(l.updates, "windows-active-0.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(payload, "initial-slot.json"), seed, 0600); err != nil {
				t.Fatal(err)
			}
			for _, root := range []string{payload, l.stableRoot} {
				for name, raw := range map[string][]byte{"Dilla.exe": []byte("same launcher"), initialUninstallerName(cfg): []byte("same generated uninstaller")} {
					if err = os.WriteFile(filepath.Join(root, name), raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			marker, _ := json.Marshal(map[string]any{"format": 1, "kind": "dilla-initial-install", "appId": cfg.AppID, "manifestSha512": cfg.InitialManifestSHA512})
			if fault != "missing marker" {
				if err = os.WriteFile(filepath.Join(l.userData, "initial-install.json"), marker, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch fault {
			case "exact damaged":
				if err = os.WriteFile(filepath.Join(l.stableRoot, "Dilla.exe"), []byte("same"), 0600); err != nil {
					t.Fatal(err)
				}
			case "seed prefix":
				if err = os.WriteFile(filepath.Join(l.updates, "windows-active-0.json"), seed[:12], 0600); err != nil {
					t.Fatal(err)
				}
			case "temporary":
				if err = os.Rename(filepath.Join(l.updates, "windows-active-0.json"), filepath.Join(l.updates, initialSeedTemporary)); err != nil {
					t.Fatal(err)
				}
			case "final plus temporary":
				if err = os.WriteFile(filepath.Join(l.updates, initialSeedTemporary), seed[:12], 0600); err != nil {
					t.Fatal(err)
				}
			case "other history":
				if err = os.WriteFile(filepath.Join(l.updates, "active.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			pkg, err := loadInitialPackage(payload, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = pkg.Close() }()
			disposition, err := classifyInitialInstallation(l, cfg, payload, pkg)
			want := initialHold
			if fault == "complete" {
				want = initialFinalizeOnly
			}
			if fault == "seed prefix" || fault == "temporary" {
				want = initialRepair
			}
			if disposition != want || (err == nil) != (want != initialHold) {
				t.Fatalf("got %v/%v, want %v", disposition, err, want)
			}
		})
	}
}

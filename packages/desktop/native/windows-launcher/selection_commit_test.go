package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"github.com/jonasthim/dilla/internal/desktoplaunch"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func committedUpgrade(t *testing.T, cfg *buildConfig, l layout) (string, []byte) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PublicKeys = []string{string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}
	manifest := desktoplaunch.ProgramManifest{Format: 1, Kind: "dilla-desktop-program", Version: "1.2.4", Channel: "stable", Platform: "win32", Arch: "x64", Files: []desktoplaunch.ProgramFile{{Path: "dilla-desktop.exe", Size: 10, SHA512: hash([]byte("new PE exe"))}, {Path: "resources/app.asar", Size: 8, SHA512: hash([]byte("new ASAR"))}}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest := hash(raw)
	id, err := desktoplaunch.TreeID(digest)
	if err != nil {
		t.Fatal(err)
	}
	name, err := desktoplaunch.ManifestName(digest)
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(l.stableRoot, "versions", id)
	if err = os.MkdirAll(filepath.Join(tree, "resources"), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	nonce := strings.Repeat("a", 64)
	ticket := filepath.Join(l.updates, "startup-"+nonce)
	if err = os.Mkdir(ticket, 0700); err != nil {
		t.Fatal(err)
	}
	release := map[string]any{"format": 1, "version": "1.2.4", "channel": "stable", "publishedAt": now.Add(-time.Hour).UTC().Format(time.RFC3339), "expiresAt": now.Add(time.Hour).UTC().Format(time.RFC3339), "artifacts": []any{map[string]any{"platform": "win32", "arch": "x64", "url": "https://github.com/jonasthim/dilla/releases/download/desktop-v1.2.4/dilla.exe", "size": 9, "sha512": hash([]byte("installer")), "programManifest": map[string]any{"url": "https://github.com/jonasthim/dilla/releases/download/desktop-v1.2.4/dilla.manifest.json", "size": len(raw), "sha512": digest}}}}
	descriptor, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(key, descriptor))
	slot, err := json.Marshal(desktoplaunch.Slot{Format: 1, Kind: "release", Generation: 1, Version: "1.2.4", ManifestSHA512: digest, Nonce: nonce, AcceptedAt: now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(filepath.Join(l.updates, "windows-active-0.json"))
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string][]byte{filepath.Join(l.stableRoot, "manifests", name): raw, filepath.Join(tree, "dilla-desktop.exe"): []byte("new PE exe"), filepath.Join(tree, "resources/app.asar"): []byte("new ASAR"), filepath.Join(ticket, "release.json"): descriptor, filepath.Join(ticket, "release.json.sig"): []byte(signature), filepath.Join(ticket, "update.exe"): []byte("installer"), filepath.Join(ticket, "old-slot.json"): previous, filepath.Join(ticket, "journal.json"): []byte(`{"format":1,"nonce":"` + nonce + `","phase":"complete","childPid":123,"version":"1.2.4"}`), filepath.Join(l.updates, "windows-active-1.json"): slot} {
		if err = os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return tree, slot
}
func TestHighestAuthenticatedCommitNeverFallsBackForDamagedProgram(t *testing.T) {
	for _, fault := range []string{"valid", "manifest", "PE", "ASAR", "missing PE", "torn slot metadata", "same generation conflict"} {
		t.Run(fault, func(t *testing.T) {
			cfg, l := initialInstallation(t)
			tree, slot := committedUpgrade(t, &cfg, l)
			switch fault {
			case "manifest":
				entries, e := os.ReadDir(filepath.Join(l.stableRoot, "manifests"))
				if e != nil {
					t.Fatal(e)
				}
				for _, entry := range entries {
					raw, e := os.ReadFile(filepath.Join(l.stableRoot, "manifests", entry.Name()))
					if e != nil {
						t.Fatal(e)
					}
					if strings.Contains(string(raw), "1.2.4") {
						if e = os.WriteFile(filepath.Join(l.stableRoot, "manifests", entry.Name()), []byte("{damaged"), 0600); e != nil {
							t.Fatal(e)
						}
					}
				}
			case "PE":
				if e := os.WriteFile(filepath.Join(tree, "dilla-desktop.exe"), []byte("corrupt PE"), 0600); e != nil {
					t.Fatal(e)
				}
			case "ASAR":
				if e := os.WriteFile(filepath.Join(tree, "resources/app.asar"), []byte("bad ASAR"), 0600); e != nil {
					t.Fatal(e)
				}
			case "missing PE":
				if e := os.Remove(filepath.Join(tree, "dilla-desktop.exe")); e != nil {
					t.Fatal(e)
				}
			case "torn slot metadata":
				if e := os.WriteFile(filepath.Join(l.updates, "windows-active-1.json"), []byte("{torn"), 0600); e != nil {
					t.Fatal(e)
				}
			case "same generation conflict":
				ticket := filepath.Join(l.updates, "startup-"+strings.Repeat("a", 64))
				other := filepath.Join(l.updates, "startup-"+strings.Repeat("b", 64))
				if e := os.CopyFS(other, os.DirFS(ticket)); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(filepath.Join(other, "journal.json"), []byte(`{"format":1,"nonce":"`+strings.Repeat("b", 64)+`","phase":"complete","childPid":123,"version":"1.2.4"}`), 0600); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(filepath.Join(l.updates, "windows-active-0.json"), []byte(strings.ReplaceAll(string(slot), strings.Repeat("a", 64), strings.Repeat("b", 64))), 0600); e != nil {
					t.Fatal(e)
				}
			}
			selected, err := selectCommitted(l, cfg, time.Now())
			if selected != nil {
				defer selected.Close()
			}
			if fault == "valid" {
				if err != nil || selected.slot.Generation != 1 {
					t.Fatalf("valid commit: %+v %v", selected, err)
				}
			} else if err == nil {
				t.Fatalf("committed %s silently fell back to old generation %d", fault, selected.slot.Generation)
			}
		})
	}
}

func TestObservedPublishedSlotReadFailureNeverSelectsOldGeneration(t *testing.T) {
	for _, fault := range []error{syscall.EIO, os.ErrNotExist} {
		cfg, l := initialInstallation(t)
		committedUpgrade(t, &cfg, l)
		read := func(path string, limit int64) ([]byte, error) {
			if filepath.Base(path) == "windows-active-1.json" {
				return nil, &os.PathError{Op: "read", Path: path, Err: fault}
			}
			return readMetadata(path, limit)
		}
		selected, err := selectCommittedUsing(l, cfg, time.Now(), read)
		if selected != nil {
			_ = selected.Close()
		}
		if err == nil {
			t.Fatalf("present slot read %v silently selected previous program", fault)
		}
	}
}

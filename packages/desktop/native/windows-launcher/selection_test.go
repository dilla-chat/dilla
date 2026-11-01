package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

func hash(data []byte) string {
	s := sha512.Sum512(data)
	return base64.StdEncoding.EncodeToString(s[:])
}
func initialInstallation(t *testing.T) (buildConfig, layout) {
	t.Helper()
	pub, _, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	der, e := x509.MarshalPKIXPublicKey(pub)
	if e != nil {
		t.Fatal(e)
	}
	cfg := buildConfig{Format: 1, AppID: "dev.dilla.desktop.test", ProductName: "DillaFixture", Channel: "stable", Arch: "x64", InitialVersion: "1.2.3", PublicKeys: []string{string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}
	root := t.TempDir()
	data := t.TempDir()
	l := layout{stableRoot: root, userData: data, updates: filepath.Join(data, "updates")}
	m := desktoplaunch.ProgramManifest{Format: 1, Kind: "dilla-desktop-program", Version: "1.2.3", Channel: "stable", Platform: "win32", Arch: "x64", Files: []desktoplaunch.ProgramFile{{Path: "dilla-desktop.exe", Size: 7, SHA512: hash([]byte("program"))}}}
	raw, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	cfg.InitialManifestSHA512 = hash(raw)
	name, e := desktoplaunch.ManifestName(cfg.InitialManifestSHA512)
	if e != nil {
		t.Fatal(e)
	}
	id, e := desktoplaunch.TreeID(cfg.InitialManifestSHA512)
	if e != nil {
		t.Fatal(e)
	}
	for _, dir := range []string{filepath.Join(root, "manifests"), filepath.Join(root, "versions", id), l.updates} {
		if e = os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
	}
	for path, bytes := range map[string][]byte{filepath.Join(root, "manifests", name): raw, filepath.Join(root, "versions", id, "dilla-desktop.exe"): []byte("program")} {
		if e = os.WriteFile(path, bytes, 0600); e != nil {
			t.Fatal(e)
		}
	}
	slot, e := json.Marshal(desktoplaunch.Slot{Format: 1, Kind: "initial", Generation: 0, Version: cfg.InitialVersion, ManifestSHA512: cfg.InitialManifestSHA512})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(l.updates, "windows-active-0.json"), slot, 0600); e != nil {
		t.Fatal(e)
	}
	return cfg, l
}

func TestSelectRequiresVerifiedCommittedSlotAndCompleteTree(t *testing.T) {
	for _, fault := range []string{"valid", "torn other slot", "no slots", "tamper", "wrong manifest"} {
		t.Run(fault, func(t *testing.T) {
			cfg, l := initialInstallation(t)
			switch fault {
			case "torn other slot":
				if e := os.WriteFile(filepath.Join(l.updates, "windows-active-1.json"), []byte("{torn"), 0600); e != nil {
					t.Fatal(e)
				}
			case "no slots":
				if e := os.Remove(filepath.Join(l.updates, "windows-active-0.json")); e != nil {
					t.Fatal(e)
				}
			case "tamper":
				id, _ := desktoplaunch.TreeID(cfg.InitialManifestSHA512)
				if e := os.WriteFile(filepath.Join(l.stableRoot, "versions", id, "dilla-desktop.exe"), []byte("corrupt"), 0600); e != nil {
					t.Fatal(e)
				}
			case "wrong manifest":
				cfg.InitialManifestSHA512 = hash([]byte("wrong"))
			}
			selected, e := selectCommitted(l, cfg, time.Now())
			if selected != nil {
				defer selected.Close()
			}
			valid := fault == "valid"
			if valid != (e == nil) {
				t.Fatalf("valid=%v err=%v", valid, e)
			}
		})
	}
}

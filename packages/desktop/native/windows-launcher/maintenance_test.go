package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallerArgumentBoundaryIsFixedReadOnlyModes(t *testing.T) {
	for _, mode := range []string{"initial", "uninstall"} {
		got, err := parseArguments([]string{"--installer-preflight=" + mode})
		if err != nil || got.mode != "installer-"+mode {
			t.Fatalf("%s: %+v %v", mode, got, err)
		}
	}
	for _, args := range [][]string{{"--installer-preflight=C:/other"}, {"--installer-preflight=uninstall", "--root=C:/other"}, {"--installer-preflight=initial", "--hidden"}} {
		if _, err := parseArguments(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}
func TestUninstallPreflightPreservesHistoryAndRejectsPendingOrLiveMutators(t *testing.T) {
	for _, state := range []string{"no transaction", "prepared", "installing", "launching", "recovering", "failed", "complete mismatch", "failed live actor", "failed inaccessible actor", "failed live ready", "failed reused ready", "failed bad ready", "invalid active"} {
		t.Run(state, func(t *testing.T) {
			cfg, l := initialInstallation(t)
			sentinel := filepath.Join(l.userData, "account-keystore.sentinel")
			if err := os.WriteFile(sentinel, []byte("preserve key and preferences"), 0600); err != nil {
				t.Fatal(err)
			}
			if state != "no transaction" {
				pendingFixture(t, l)
				journal := `{"format":1,"nonce":"` + strings.Repeat("a", 64) + `","phase":"` + state + `"}`
				if strings.HasPrefix(state, "failed ") {
					journal = `{"format":1,"nonce":"` + strings.Repeat("a", 64) + `","phase":"failed","failure":"interrupted","installerActorPid":1234}`
				}
				if state == "complete mismatch" {
					journal = `{"format":1,"nonce":"` + strings.Repeat("a", 64) + `","phase":"complete","childPid":1234,"version":"9.0.0"}`
				}
				if err := os.WriteFile(filepath.Join(l.updates, "startup-"+strings.Repeat("a", 64), "journal.json"), []byte(journal), 0600); err != nil {
					t.Fatal(err)
				}
				if state == "invalid active" {
					if err := os.WriteFile(filepath.Join(l.updates, "active.json"), []byte("{torn"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if strings.HasSuffix(state, "ready") {
				nonce := strings.Repeat("a", 64)
				if state == "failed bad ready" {
					nonce = strings.Repeat("b", 64)
				}
				ready, err := json.Marshal(map[string]any{"nonce": nonce, "version": cfg.InitialVersion, "pid": 4321})
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(l.updates, "startup-"+strings.Repeat("a", 64), "watchdog-ready.json"), ready, 0600); err != nil {
					t.Fatal(err)
				}
			}
			alive := func(pid int64) (bool, error) {
				if state == "failed inaccessible actor" {
					return false, os.ErrPermission
				}
				return state == "failed live actor" || (state == "failed live ready" && pid == 4321), nil
			}
			err := maintenancePreflight(l, cfg, "uninstall", alive, 99, "")
			want := state == "no transaction" || state == "failed" || state == "failed reused ready"
			if (err == nil) != want {
				t.Fatalf("want success %v, got %v", want, err)
			}
			if raw, e := os.ReadFile(sentinel); e != nil || string(raw) != "preserve key and preferences" {
				t.Fatalf("account changed: %q %v", raw, e)
			}
			if _, e := os.Stat(filepath.Join(l.updates, "windows-active-0.json")); e != nil {
				t.Fatal("preflight mutated slot", e)
			}
		})
	}
}
func TestInitialPreflightRequiresAbsentRootsButAllowsPreservedAccountProfile(t *testing.T) {
	cfg, l := initialInstallation(t)
	if err := os.RemoveAll(l.stableRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(l.updates); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(l.userData, "account-keystore.sentinel")
	if err := os.WriteFile(sentinel, []byte("existing account"), 0600); err != nil {
		t.Fatal(err)
	}
	alive := func(int64) (bool, error) { return false, nil }
	if err := maintenancePreflight(l, cfg, "initial", alive, 99, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(l.updates, 0700); err != nil {
		t.Fatal(err)
	}
	if err := maintenancePreflight(l, cfg, "initial", alive, 99, ""); err == nil {
		t.Fatal("reset existing update history")
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "existing account" {
		t.Fatal("changed profile")
	}
}
func TestStagingPreflightRequiresActualRegisteredInstaller(t *testing.T) {
	cfg, l := initialInstallation(t)
	pendingFixture(t, l)
	path := filepath.Join(l.updates, "startup-"+strings.Repeat("a", 64), "journal.json")
	raw := `{"format":1,"nonce":"` + strings.Repeat("a", 64) + `","phase":"installing","installerPid":1234}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PublicKeys = []string{string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}
	directory := filepath.Dir(path)
	now := time.Now()
	release := map[string]any{"format": 1, "version": "1.2.4", "channel": "stable", "publishedAt": now.Add(-time.Hour).UTC().Format(time.RFC3339), "expiresAt": now.Add(time.Hour).UTC().Format(time.RFC3339), "artifacts": []any{map[string]any{"platform": "win32", "arch": "x64", "url": "https://github.com/jonasthim/dilla/releases/download/desktop-v1.2.4/dilla.exe", "size": 9, "sha512": hash([]byte("installer")), "programManifest": map[string]any{"url": "https://github.com/jonasthim/dilla/releases/download/desktop-v1.2.4/dilla.manifest.json", "size": 8, "sha512": hash([]byte("manifest"))}}}}
	descriptor, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(private, descriptor))
	accepted, err := json.Marshal(map[string]any{"verifiedAt": now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	for name, bytes := range map[string][]byte{"release.json": descriptor, "release.json.sig": []byte(signature), "plan.json": accepted} {
		if err := os.WriteFile(filepath.Join(directory, name), bytes, 0600); err != nil {
			t.Fatal(err)
		}
	}
	alive := func(int64) (bool, error) { return false, nil }
	if err := maintenancePreflight(l, cfg, "stage", alive, 9999, hash([]byte("manifest"))); err == nil {
		t.Fatal("accepted unrelated stage parent")
	}
	if err := maintenancePreflight(l, cfg, "stage", alive, 1234, hash([]byte("manifest"))); err != nil {
		t.Fatal(err)
	}
	if err := maintenancePreflight(l, cfg, "stage", alive, 1234, hash([]byte("other package"))); err == nil {
		t.Fatal("accepted another signed package digest")
	}
}

func TestExplicitUninstallOfDamagedNoActiveProgramPreservesDeviceData(t *testing.T) {
	for _, damage := range []string{"manifest", "executable", "missing executable", "ASAR", "missing slot", "missing updates", "nonterminal active"} {
		t.Run(damage, func(t *testing.T) {
			cfg, l := initialInstallation(t)
			entries, err := os.ReadDir(filepath.Join(l.stableRoot, "versions"))
			if err != nil {
				t.Fatal(err)
			}
			tree := filepath.Join(l.stableRoot, "versions", entries[0].Name())
			account := filepath.Join(l.userData, "opaque-account.sentinel")
			if err = os.WriteFile(account, []byte("KEK OPFS preferences"), 0600); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "manifest":
				files, e := os.ReadDir(filepath.Join(l.stableRoot, "manifests"))
				if e != nil {
					t.Fatal(e)
				}
				err = os.WriteFile(filepath.Join(l.stableRoot, "manifests", files[0].Name()), []byte("{damaged"), 0600)
			case "executable":
				err = os.WriteFile(filepath.Join(tree, "dilla-desktop.exe"), []byte("corrupt PE"), 0600)
			case "missing executable":
				err = os.Remove(filepath.Join(tree, "dilla-desktop.exe"))
			case "ASAR":
				err = os.Mkdir(filepath.Join(tree, "resources"), 0700)
				if err == nil {
					err = os.WriteFile(filepath.Join(tree, "resources/app.asar"), []byte("damaged ASAR"), 0600)
				}
			case "missing updates":
				err = os.RemoveAll(l.updates)
			case "missing slot":
				err = os.Remove(filepath.Join(l.updates, "windows-active-0.json"))
			case "nonterminal active":
				pendingFixture(t, l)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = maintenancePreflight(l, cfg, "uninstall", func(int64) (bool, error) { return false, nil }, 99, "")
			if (err == nil) != (damage != "nonterminal active") {
				t.Fatalf("damaged uninstall authorization: %v", err)
			}
			if err = maintenancePreflight(l, cfg, "initial", func(int64) (bool, error) { return false, nil }, 99, ""); err == nil {
				t.Fatal("initial setup reset existing program/history")
			}
			raw, err := os.ReadFile(account)
			if err != nil || string(raw) != "KEK OPFS preferences" {
				t.Fatal("changed device data")
			}
		})
	}
}

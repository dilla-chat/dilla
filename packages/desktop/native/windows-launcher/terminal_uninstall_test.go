package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTerminalUninstallAuthenticatesMetadataWithoutRequiringAnyProgramHealthy(t *testing.T) {
	for _, phase := range []string{"complete", "failed"} {
		for _, damage := range []string{"new ASAR", "new manifest", "old PE", "old manifest", "all versions missing", "all manifests missing", "unresolved journal", "bad release signature", "bad exact installer", "live owned helper", "wrong active nonce"} {
			t.Run(phase+"/"+damage, func(t *testing.T) {
				cfg, l := initialInstallation(t)
				tree, _ := committedUpgrade(t, &cfg, l)
				nonce := strings.Repeat("a", 64)
				ticket := filepath.Join(l.updates, "startup-"+nonce)
				journal := map[string]any{"format": 1, "nonce": nonce, "phase": phase}
				if phase == "complete" {
					journal["childPid"] = 123
					journal["version"] = "1.2.4"
				} else {
					journal["failure"] = "interrupted"
					if err := os.Remove(filepath.Join(l.updates, "windows-active-1.json")); err != nil {
						t.Fatal(err)
					}
				}
				if damage == "live owned helper" {
					journal["watchdogPid"] = 4321
				}
				if damage == "unresolved journal" {
					journal = map[string]any{"format": 1, "nonce": nonce, "phase": "installing"}
				}
				raw, err := json.Marshal(journal)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(ticket, "journal.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				activeNonce := nonce
				if damage == "wrong active nonce" {
					activeNonce = strings.Repeat("b", 64)
				}
				if err = os.WriteFile(filepath.Join(l.updates, "active.json"), []byte(`{"format":1,"nonce":"`+activeNonce+`"}`), 0600); err != nil {
					t.Fatal(err)
				}
				oldName, err := os.ReadDir(filepath.Join(l.stableRoot, "manifests"))
				if err != nil {
					t.Fatal(err)
				}
				initialName := ""
				for _, entry := range oldName {
					bytes, e := os.ReadFile(filepath.Join(l.stableRoot, "manifests", entry.Name()))
					if e != nil {
						t.Fatal(e)
					}
					if strings.Contains(string(bytes), "1.2.3") {
						initialName = entry.Name()
					}
				}
				switch damage {
				case "new ASAR":
					err = os.WriteFile(filepath.Join(tree, "resources/app.asar"), []byte("corrupt committed ASAR"), 0600)
				case "new manifest":
					for _, entry := range oldName {
						if entry.Name() != initialName {
							err = os.WriteFile(filepath.Join(l.stableRoot, "manifests", entry.Name()), []byte("{corrupt"), 0600)
						}
					}
				case "old PE":
					entries, e := os.ReadDir(filepath.Join(l.stableRoot, "versions"))
					if e != nil {
						t.Fatal(e)
					}
					for _, entry := range entries {
						old := filepath.Join(l.stableRoot, "versions", entry.Name())
						if old != tree {
							err = os.WriteFile(filepath.Join(old, "dilla-desktop.exe"), []byte("corrupt old PE"), 0600)
						}
					}
				case "old manifest":
					err = os.WriteFile(filepath.Join(l.stableRoot, "manifests", initialName), []byte("{corrupt"), 0600)
				case "all versions missing":
					err = os.RemoveAll(filepath.Join(l.stableRoot, "versions"))
				case "all manifests missing":
					err = os.RemoveAll(filepath.Join(l.stableRoot, "manifests"))
				case "bad release signature":
					err = os.WriteFile(filepath.Join(ticket, "release.json.sig"), []byte("bad signature"), 0600)
				case "bad exact installer":
					err = os.WriteFile(filepath.Join(ticket, "update.exe"), []byte("bad exact installer"), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				account := filepath.Join(l.userData, "account-keystore.sentinel")
				if err = os.WriteFile(account, []byte("retain opaque profile"), 0600); err != nil {
					t.Fatal(err)
				}
				err = maintenancePreflight(l, cfg, "uninstall", func(pid int64) (bool, error) { return pid == 4321, nil }, 99, "")
				allowed := damage != "unresolved journal" && damage != "live owned helper" && damage != "wrong active nonce" && (phase != "complete" || (damage != "bad release signature" && damage != "bad exact installer"))
				if (err == nil) != allowed {
					t.Fatalf("terminal removal allowed=%v: %v", allowed, err)
				}
				got, err := os.ReadFile(account)
				if err != nil || string(got) != "retain opaque profile" {
					t.Fatal("preflight changed profile")
				}
			})
		}
	}
}

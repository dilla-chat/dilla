package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func pendingFixture(t *testing.T, l layout) {
	t.Helper()
	nonce := strings.Repeat("a", 64)
	dir := filepath.Join(l.updates, "startup-"+nonce)
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	old, e := os.ReadFile(filepath.Join(l.updates, "windows-active-0.json"))
	if e != nil {
		t.Fatal(e)
	}
	for path, data := range map[string][]byte{filepath.Join(dir, "old-slot.json"): old, filepath.Join(dir, "journal.json"): []byte(`{"format":1,"nonce":"` + nonce + `","phase":"prepared"}`), filepath.Join(l.updates, "active.json"): []byte(`{"format":1,"nonce":"` + nonce + `"}`)} {
		if e = os.WriteFile(path, data, 0600); e != nil {
			t.Fatal(e)
		}
	}
}

func TestPendingBootstrapNeverForwardsIntentOrFallsThroughUnverified(t *testing.T) {
	for _, exit := range []uint32{20, 21, 22, 1} {
		t.Run("exit-"+strconv.FormatUint(uint64(exit), 10), func(t *testing.T) {
			cfg, l := initialInstallation(t)
			pendingFixture(t, l)
			calls := 0
			launch := func(s *selection, args []string, wait bool) (uint32, error) {
				calls++
				if calls == 1 {
					if !wait || len(args) != 1 || args[0] != "--update-bootstrap="+strings.Repeat("a", 64) {
						t.Fatalf("unsafe bootstrap args: %q wait=%v", args, wait)
					}
					if exit == 22 {
						path := filepath.Join(l.updates, "startup-"+strings.Repeat("a", 64), "journal.json")
						if e := os.WriteFile(path, []byte(`{"format":1,"nonce":"`+strings.Repeat("a", 64)+`","phase":"failed","failure":"interrupted"}`), 0600); e != nil {
							t.Fatal(e)
						}
					}
					return exit, nil
				}
				if wait || len(args) != 1 || args[0] != "dilla://invite/example.com/code" {
					t.Fatalf("ordinary args %q", args)
				}
				return 0, nil
			}
			_, e := runLauncher(l, cfg, arguments{mode: "launch", forwarded: []string{"dilla://invite/example.com/code"}}, new(bytes.Buffer), launch)
			if exit == 22 && (e != nil || calls != 2) {
				t.Fatalf("clear role: calls=%d err=%v", calls, e)
			}
			if exit != 22 && calls != 1 {
				t.Fatalf("fell through exit %d", exit)
			}
			if exit == 1 && e == nil {
				t.Fatal("accepted unexpected role exit")
			}
		})
	}
}

func TestReadOnlyStateNeverLaunchesBootstrapOrUI(t *testing.T) {
	cfg, l := initialInstallation(t)
	pendingFixture(t, l)
	var output bytes.Buffer
	_, e := runLauncher(l, cfg, arguments{mode: "state"}, &output, func(*selection, []string, bool) (uint32, error) { t.Fatal("state launched a process"); return 0, nil })
	if e != nil {
		t.Fatal(e)
	}
	var state map[string]any
	if e = json.Unmarshal(output.Bytes(), &state); e != nil {
		t.Fatal(e)
	}
	if state["status"] != "pending" || state["version"] != "1.2.3" || state["generation"] != float64(0) {
		t.Fatalf("unverified state %v", state)
	}
	for _, forbidden := range []string{"publicKeys", "stableRoot", "userData", "executable"} {
		if _, exists := state[forbidden]; exists {
			t.Fatalf("state exposed %s", forbidden)
		}
	}
}

func TestBootstrapClearRejectsChangedNonceAndNonterminalJournal(t *testing.T) {
	for _, fault := range []string{"changed nonce", "nonterminal"} {
		t.Run(fault, func(t *testing.T) {
			cfg, l := initialInstallation(t)
			pendingFixture(t, l)
			calls := 0
			_, e := runLauncher(l, cfg, arguments{mode: "launch"}, new(bytes.Buffer), func(*selection, []string, bool) (uint32, error) {
				calls++
				if fault == "changed nonce" {
					if e := os.WriteFile(filepath.Join(l.updates, "active.json"), []byte(`{"format":1,"nonce":"`+strings.Repeat("b", 64)+`"}`), 0600); e != nil {
						t.Fatal(e)
					}
				}
				return 22, nil
			})
			if e == nil || calls != 1 {
				t.Fatalf("unsafe fallthrough: calls=%d err=%v", calls, e)
			}
		})
	}
}

func TestNoActivePointerIgnoresAbandonedPreparation(t *testing.T) {
	cfg, l := initialInstallation(t)
	if e := os.Mkdir(filepath.Join(l.updates, "startup-"+strings.Repeat("a", 64)), 0700); e != nil {
		t.Fatal(e)
	}
	calls := 0
	_, e := runLauncher(l, cfg, arguments{mode: "launch"}, new(bytes.Buffer), func(*selection, []string, bool) (uint32, error) { calls++; return 0, nil })
	if e != nil || calls != 1 {
		t.Fatalf("abandoned preparation stranded committed version: calls=%d err=%v", calls, e)
	}
}

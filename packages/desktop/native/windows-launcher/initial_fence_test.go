package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInitialMarkerFencesCompleteSeedUntilExplicitFinalization(t *testing.T) {
	for _, marker := range []string{`{"format":1,"kind":"dilla-initial-install"}`, `malformed`, `{"appId":"foreign"}`} {
		t.Run(marker, func(t *testing.T) {
			cfg, l := initialInstallation(t)
			path := filepath.Join(l.userData, "initial-install.json")
			if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(l.userData, "account.sentinel")
			if err := os.WriteFile(sentinel, []byte("account unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			launch := func(*selection, []string, bool) (uint32, error) { calls++; return 0, nil }
			var out bytes.Buffer
			code, err := runLauncher(l, cfg, arguments{mode: "launch"}, &out, launch)
			if err != nil || code != 0 || calls != 0 {
				t.Fatalf("pending setup launched: %d %d %v", code, calls, err)
			}
			code, err = runLauncher(l, cfg, arguments{mode: "state"}, &out, launch)
			if err == nil || code == 0 || out.Len() != 0 || calls != 0 {
				t.Fatalf("pending setup published state: %d %d %v", code, calls, err)
			}
			// This portable test models only the authorized finalization boundary. The
			// Windows regression separately proves same-handle marker removal.
			if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
			code, err = runLauncher(l, cfg, arguments{mode: "launch"}, &out, launch)
			if err != nil || code != 0 || calls != 1 {
				t.Fatalf("completed setup stayed fenced: %d %d %v", code, calls, err)
			}
			if got, e := os.ReadFile(sentinel); e != nil || string(got) != "account unchanged" {
				t.Fatal("account changed", e)
			}
		})
	}
}

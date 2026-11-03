package testkit_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dilla-chat/dilla/internal/testkit"
)

// The Global Constraints give every DS invariant both a Go test and a testkit scenario, and
// protocol/02 names each scenario after its invariant. The fifteen of interfaces.md §7.4 cover
// invariants 2, 3, 5, 6, 7, 9, 10 and 11; these three cover the rest: 1 (Registration), 4 (Commit
// validity) and 8 (Current-leaf sends). They are a list of their own so the fifteen stay exactly
// the fifteen TestEveryChaosScenarioRunsGreen names.
func TestTheInvariantOneFourAndEightScenariosRunGreen(t *testing.T) {
	for _, name := range []string{
		"registration.scn",
		"commit_validity.scn",
		"current_leaf_sends.scn",
	} {
		path := filepath.Join("..", "..", "testkit", "scenarios", name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("scenario %s is missing: %v", name, err)
		}
		t.Run(name, func(t *testing.T) {
			h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
			t.Cleanup(h.Stop)
			result := h.Run(t, path)
			if result.Err != nil {
				t.Fatalf("%s failed: %v\nstdout:\n%s\nstderr:\n%s",
					result.Name, result.Err, result.Stdout, result.Stderr)
			}
			if result.Steps == 0 {
				t.Fatal("the scenario ran no steps")
			}
			// Each refusal step prints what it got; -v shows which rule refused what.
			t.Logf("%s", result.Stdout)
		})
	}
}

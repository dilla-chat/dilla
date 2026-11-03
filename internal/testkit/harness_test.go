package testkit_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/build"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/store/sqlite"
	"github.com/dilla-chat/dilla/internal/testkit"
)

// recordingTB is a testing.TB whose Skip and Skipf record instead of stopping the goroutine, so
// a test can assert that Start skipped and why. Every other method is the embedded TB's.
type recordingTB struct {
	testing.TB
	skipped    bool
	skipReason string
	failed     bool
	failReason string
}

func (r *recordingTB) Skip(args ...any) {
	r.skipped = true
	r.skipReason = fmt.Sprint(args...)
}

func (r *recordingTB) Skipf(format string, args ...any) {
	r.skipped = true
	r.skipReason = fmt.Sprintf(format, args...)
}

func (r *recordingTB) SkipNow()      { r.skipped = true }
func (r *recordingTB) Skipped() bool { return r.skipped }

// Fatal and Fatalf record too, so a test can assert that Start failed rather than skipped.
func (r *recordingTB) Fatal(args ...any) {
	r.failed = true
	r.failReason = fmt.Sprint(args...)
}

func (r *recordingTB) Fatalf(format string, args ...any) {
	r.failed = true
	r.failReason = fmt.Sprintf(format, args...)
}

func TestTheHarnessSkipsWithAClearReasonWhenTheBinaryIsAbsent(t *testing.T) {
	t.Setenv("DILLA_TESTKIT", "")
	t.Setenv("DILLA_TESTKIT_REQUIRED", "")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Start must skip, not panic: %v", r)
		}
	}()
	fake := &recordingTB{TB: t}
	testkit.Start(fake, testkit.Options{DataDir: t.TempDir()})
	if !fake.skipped {
		t.Fatal("Start must Skip when DILLA_TESTKIT is unset")
	}
	if !strings.Contains(fake.skipReason, "DILLA_TESTKIT") {
		t.Fatalf("skip reason %q must name the variable and how to build the binary", fake.skipReason)
	}
}

// Where the harness is required — CI, which builds dilla-testkit and the wasi core before this
// package runs and sets DILLA_TESTKIT_REQUIRED — a missing or unusable binary is a FAILURE. A skip
// there would turn every accepted-commit test and every chaos scenario into a silent pass.
func TestAMissingBinaryFailsWhereTheHarnessIsRequired(t *testing.T) {
	for name, binary := range map[string]string{
		"unset":    "",
		"unusable": filepath.Join(t.TempDir(), "no-such-dilla-testkit"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("DILLA_TESTKIT", binary)
			t.Setenv("DILLA_TESTKIT_REQUIRED", "1")
			fake := &recordingTB{TB: t}
			if h := testkit.Start(fake, testkit.Options{DataDir: t.TempDir()}); h != nil {
				h.Stop()
				t.Fatal("Start answered a harness without a binary")
			}
			if fake.skipped {
				t.Fatalf("Start skipped (%q) although DILLA_TESTKIT_REQUIRED is set", fake.skipReason)
			}
			if !fake.failed {
				t.Fatal("Start must fail when the harness is required and the binary is missing")
			}
			for _, want := range []string{"DILLA_TESTKIT", "DILLA_TESTKIT_REQUIRED"} {
				if !strings.Contains(fake.failReason, want) {
					t.Errorf("failure %q must name %s", fake.failReason, want)
				}
			}
		})
	}
}

// Every scenario of §7.4 runs green against the in-process dillad.
//
// Each scenario gets an instance of its own. They cannot share one: five of them move the
// instance clock forward by up to 91 days, which takes every later scenario's freshly minted
// KeyPackages (a 90-day lifetime against the wall clock) past their expiry, and two replace the
// database with a snapshot.
func TestEveryChaosScenarioRunsGreen(t *testing.T) {

	// The fifteen are named, not globbed: `testkit/scenarios/` already holds
	// commit-conflict.scn, external-join.scn and two-client-text.scn from week 1, so a glob
	// returns eighteen and the count assertion fails before a single scenario runs.
	names := []string{
		"concurrent_commits_5.scn",
		"kick_while_offline_then_join.scn",
		"external_commit_during_freeze_online.scn",
		"external_commit_during_freeze_offline.scn",
		"expired_keypackage_void.scn",
		"remove_gone_leaf.scn",
		"malformed_commit_fork_report.scn",
		"resync_to_head.scn",
		"watchdog_three_lost_rounds.scn",
		"restore_heal_from_tail.scn",
		"restore_force_recreate.scn",
		"inactivity_remove_90d.scn",
		"join_storm_256_batched.scn",
		"resume_always_refused.scn",
		"retention_prune_then_resync.scn",
	}
	if len(names) != 15 {
		t.Fatalf("the list names %d scenarios, want the 15 of interfaces.md §7.4", len(names))
	}
	for _, name := range names {
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
		})
	}
}

// The three week-1 scenarios were written against the in-memory stub; they run unchanged against
// the instance too, which is what makes them the regression check for invariants 2 and 3 on the
// real delivery service (the stub has no instance proposals, so there `join … via=welcome` is a
// member Add; here it is the instance's Add committed by a member).
func TestTheWeekOneScenariosRunAgainstTheInstance(t *testing.T) {
	for _, name := range []string{"two-client-text.scn", "external-join.scn", "commit-conflict.scn"} {
		t.Run(name, func(t *testing.T) {
			h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
			t.Cleanup(h.Stop)
			result := h.Run(t, filepath.Join("..", "..", "testkit", "scenarios", name))
			if result.Err != nil {
				t.Fatalf("%s failed: %v\nstdout:\n%s\nstderr:\n%s",
					result.Name, result.Err, result.Stdout, result.Stderr)
			}
			if result.Steps == 0 {
				t.Fatal("the scenario ran no steps")
			}
		})
	}
}

// The control listener is what five of the fifteen scenarios drive, so its encoding is asserted
// end to end rather than assumed: the Rust client posts to it and the Go handler decodes it.
func TestTheControlListenerAdvancesTheClockAndReportsItBack(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
	t.Cleanup(h.Stop)

	before := h.DebugState(t).NowUnix
	body, err := json.Marshal(map[string]int64{"seconds": 3600})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := httpPost(t, h.ControlURL()+"/debug/clock", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /debug/clock: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.StatusCode)
	}
	if after := h.DebugState(t).NowUnix; after != before+3600 {
		t.Fatalf("now = %d, want %d: the clock did not advance through the control listener",
			after, before+3600)
	}
}

// join_storm_256_batched: a 1,000-device private channel completes in exactly four commits (256,
// 256, 256, 232), each writing exactly one Welcome payload row, and leaves nothing queued.
//
// Exactly four, not "at most": every joiner has to be in for the scenario to pass, so fewer
// commits would mean one of them carried more than 256 Adds — the cap this scenario exists for.
// The Welcome-row count is Plan 2 task 7's "one Welcome per commit" asserted where real commits
// write real rows; a recording double could only count its own increments.
func TestAThousandDeviceJoinStormCompletesInExactlyFourCommits(t *testing.T) {
	dir := t.TempDir()
	h := testkit.Start(t, testkit.Options{DataDir: dir})
	t.Cleanup(h.Stop)
	result := h.Run(t, filepath.Join("..", "..", "testkit", "scenarios", "join_storm_256_batched.scn"))
	if result.Err != nil {
		t.Fatalf("scenario: %v\nstdout:\n%s\nstderr:\n%s", result.Err, result.Stdout, result.Stderr)
	}
	commits := h.CommitCount()
	if commits != 4 {
		t.Fatalf("%d commits for 1,000 devices, want exactly 4 (256 Adds per commit)", commits)
	}
	db, err := sqlite.OpenRead(filepath.Join(dir, "dilla.db"))
	if err != nil {
		t.Fatalf("sqlite.OpenRead: %v", err)
	}
	defer func() { _ = db.Close() }()
	count := func(table string) int {
		var n int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	if n := count("mls_welcome_payloads"); n != commits {
		t.Fatalf("mls_welcome_payloads holds %d rows, want one per commit (%d)", n, commits)
	}
	if n := count("pending_joins"); n != 0 {
		t.Fatalf("pending_joins still holds %d devices after the storm completed", n)
	}
}

// advance_clock drives the 24-hour text TTL without waiting: TTLs are evaluated lazily at
// decision points, never with an armed timer.
func TestAdvanceClockDrivesTheTwentyFourHourTTL(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
	t.Cleanup(h.Stop)
	start := time.Now()
	result := h.Run(t, filepath.Join("..", "..", "testkit", "scenarios", "expired_keypackage_void.scn"))
	if result.Err != nil {
		t.Fatalf("scenario: %v\n%s", result.Err, result.Stderr)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("the scenario took %s: a TTL must be evaluated lazily, never waited out", elapsed)
	}
}

// The test-only seeding and the harness must never reach a dillad build.
func TestTheReleaseBinaryDependsOnNeitherDilladtestNorTestkit(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), goToolPath(t), "list", "-deps",
		"github.com/dilla-chat/dilla/cmd/dillad").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, forbidden := range []string{
		"github.com/dilla-chat/dilla/internal/dillad/dilladtest",
		"github.com/dilla-chat/dilla/internal/testkit",
	} {
		if strings.Contains(string(out), forbidden) {
			t.Errorf("cmd/dillad depends on %s: the seeding convenience must never ship", forbidden)
		}
	}
	if os.Getenv("DILLA_TESTKIT") == "" {
		t.Log("DILLA_TESTKIT unset; this assertion is independent of it")
	}
}

// goToolPath is the go binary of the toolchain running this test, or PATH's: the developer box
// keeps Go outside PATH and CI keeps it on PATH, so neither is a literal path.
func goToolPath(t *testing.T) string {
	t.Helper()
	if root := build.Default.GOROOT; root != "" {
		p := filepath.Join(root, "bin", "go")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	p, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go toolchain: GOROOT %q holds no bin/go and PATH has none: %v", build.Default.GOROOT, err)
	}
	return p
}

// httpPost is http.Post bound to the test's context.
func httpPost(t *testing.T, url, contentType string, body io.Reader) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	return http.DefaultClient.Do(req)
}

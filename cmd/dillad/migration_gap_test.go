package main

// migration_gap_test.go: a database at the binary's highest version that never applied a lower
// migration (a build that carried 23 but not 21 once ran against it, and a later build carries
// both) must not be served, migrated past or reported healthy. goose's Up refuses it, but serve
// skipped Up whenever the two highest versions were equal.

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/exit"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// gapHarness is an instance whose goose table lacks one migration below its highest: the second
// highest embedded version, so the test holds both with a gap in the tree and without one.
func gapHarness(t *testing.T) (h *cliHarness, missing int) {
	h, ms := gapHarnessN(t, 1)
	return h, ms[0]
}

// gapHarnessN forgets the n migrations below the highest, newest first, and returns their versions.
func gapHarnessN(t *testing.T, n int) (h *cliHarness, missing []int) {
	t.Helper()
	h = newCLIHarness(t)
	var versions []int
	entries, err := sqlitemigrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(prefix, "%d", &v); err != nil {
			continue
		}
		versions = append(versions, v)
	}
	slices.Sort(versions)
	missing = versions[len(versions)-1-n : len(versions)-1]
	db, err := openSQLiteForTest(h.cfg.DB.Path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, v := range missing {
		if _, err := db.ExecContext(t.Context(), `DELETE FROM goose_db_version WHERE version_id = ?`, v); err != nil {
			t.Fatalf("forget migration %d: %v", v, err)
		}
	}
	return h, missing
}

func TestServeRefusesADatabaseThatSkippedAMigration(t *testing.T) {
	h, missing := gapHarness(t)
	code, out := h.Run(t, "serve", "--config="+h.ConfigPath)
	if code != int(exit.Data) {
		t.Fatalf("serve exit = %d, want %d (exit.Data):\n%s", code, exit.Data, out)
	}
	if !strings.Contains(out, fmt.Sprintf("missing migration %d", missing)) {
		t.Fatalf("serve does not name the missing migration %d:\n%s", missing, out)
	}
}

func TestMigrateStatusAndUpRefuseADatabaseThatSkippedAMigration(t *testing.T) {
	h, missing := gapHarness(t)
	for _, verb := range []string{"status", "up"} {
		code, out := h.Run(t, "migrate", verb, "--config="+h.ConfigPath)
		if code == 0 {
			t.Fatalf("migrate %s accepted a database missing migration %d:\n%s", verb, missing, out)
		}
		if !strings.Contains(out, fmt.Sprint(missing)) {
			t.Fatalf("migrate %s does not name migration %d:\n%s", verb, missing, out)
		}
	}
}

func TestDoctorFailsADatabaseThatSkippedAMigration(t *testing.T) {
	stubWasi(t, nil)
	h, missing := gapHarness(t)
	out, _, err := run(t, "doctor", "--config="+h.ConfigPath)
	if code, ok := exitCode(err); !ok || code != exit.Unavailable {
		t.Fatalf("doctor gave %v, want exit.Unavailable:\n%s", err, out)
	}
	if !strings.Contains(out, "FAIL  database") || !strings.Contains(out, fmt.Sprintf("missing migration %d", missing)) {
		t.Fatalf("the database leg does not fail naming migration %d:\n%s", missing, out)
	}
}

// The refusal leads with the near-lossless way back: migrate down to the last version below the
// first missing one (which undoes only the migrations that ran out of order), then start a build
// that carries every migration; the pre-migration copy and a backup come after.
func TestTheGapRefusalGivesTheWayBackInOrder(t *testing.T) {
	for _, n := range []int{1, 2} {
		h, missing := gapHarnessN(t, n)
		code, out := h.Run(t, "migrate", "status", "--config="+h.ConfigPath)
		if code == 0 {
			t.Fatalf("n=%d: migrate status accepted the gap:\n%s", n, out)
		}
		down := fmt.Sprintf("dillad migrate down --to=%d --i-know-what-i-am-doing", missing[0]-1)
		pre, backup := strings.Index(out, ".pre-migration-"), strings.Index(out, "backup")
		at := strings.Index(out, down)
		if at < 0 || pre < at || backup < pre {
			t.Fatalf("n=%d: want %q, then the .pre-migration- copy, then a backup, in that order:\n%s", n, down, out)
		}
		word := "migration " + fmt.Sprint(missing[0])
		if n == 2 {
			word = fmt.Sprintf("migrations %d, %d", missing[0], missing[1])
		}
		if !strings.Contains(out, "missing "+word) {
			t.Fatalf("n=%d: want %q (plural agrees with the count):\n%s", n, "missing "+word, out)
		}
	}
}

// A database ahead of the binary (a rollback without a down migration) is not healthy either: serve
// refuses it, so doctor says so instead of reporting its version green.
func TestDoctorFailsADatabaseNewerThanTheBinary(t *testing.T) {
	stubWasi(t, nil)
	h := newCLIHarness(t)
	db, err := openSQLiteForTest(h.cfg.DB.Path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)`, 1_000_000); err != nil {
		t.Fatalf("forge a newer version: %v", err)
	}
	out, _, err := run(t, "doctor", "--config="+h.ConfigPath)
	if code, ok := exitCode(err); !ok || code != exit.Unavailable {
		t.Fatalf("doctor gave %v, want exit.Unavailable:\n%s", err, out)
	}
	if !strings.Contains(out, "FAIL  database") || !strings.Contains(out, "newer than this binary") {
		t.Fatalf("the database leg does not fail as newer than the binary:\n%s", out)
	}
}

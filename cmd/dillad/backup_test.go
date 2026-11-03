package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/ops"
)

// initInstance runs `dillad init` into a fresh data directory and returns the
// config path.
func initInstance(t *testing.T) (dir, cfgPath string) {
	t.Helper()
	dir = t.TempDir()
	if _, _, err := run(t, "init", "--agree-tos", "--config="+filepath.Join(dir, "dilla.toml"), "--data-dir="+dir, "--domain=chat.example", "--public-ip=203.0.113.7"); err != nil {
		t.Fatalf("init: %v", err)
	}
	return dir, filepath.Join(dir, "dilla.toml")
}

func TestBackupToStdoutWritesTheArchiveAndNothingElse(t *testing.T) {
	_, cfgPath := initInstance(t)
	out, errBuf, err := run(t, "backup", "--config="+cfgPath, "--out=-")
	if err != nil {
		t.Fatalf("backup --out=-: %v (stderr %q)", err, errBuf)
	}
	// Everything on stdout is the gzip stream: it starts with the gzip magic and
	// verifies to its last byte, so no summary line was mixed into it.
	if !strings.HasPrefix(out, "\x1f\x8b") {
		t.Fatalf("stdout does not start with a gzip stream: %q", out[:min(len(out), 16)])
	}
	man, err := ops.Verify(t.Context(), strings.NewReader(out))
	if err != nil {
		t.Fatalf("the stdout stream does not verify: %v", err)
	}
	if man.Engine != "sqlite" {
		t.Fatalf("engine = %q", man.Engine)
	}
	// The operator is told what the archive holds, on stderr.
	if !strings.Contains(errBuf, ops.ContentNotice) {
		t.Fatalf("stderr does not carry R38's wording: %q", errBuf)
	}
}

func TestBackupVerifyExitsZeroOnAGoodArchiveAndDataOnADamagedOne(t *testing.T) {
	dir, cfgPath := initInstance(t)
	archive := filepath.Join(dir, "nightly.tar.gz")
	out, _, err := run(t, "backup", "--config="+cfgPath, "--out="+archive, "--label=nightly")
	if err != nil {
		t.Fatalf("backup --out=PATH: %v", err)
	}
	if !strings.Contains(out, archive) || !strings.Contains(out, ops.ContentNotice) {
		t.Fatalf("backup did not report the archive and what it holds: %q", out)
	}
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the archive has mode %04o, want 0600", perm)
	}
	if _, _, err := run(t, "backup", "verify", "--config="+cfgPath, "--from="+archive); err != nil {
		t.Fatalf("backup verify of a good archive: %v", err)
	}

	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	damaged := filepath.Join(dir, "damaged.tar.gz")
	if err := os.WriteFile(damaged, raw[:len(raw)*2/3], 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err = run(t, "backup", "verify", "--from="+damaged)
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Data {
		t.Fatalf("backup verify of a damaged archive = %v, want exit.Data (65)", err)
	}
}

func TestBackupRefusesADatabaseNewerThanTheBinary(t *testing.T) {
	dir, cfgPath := initInstance(t)
	db, err := openSQLiteForTest(filepath.Join(dir, "dilla.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (1000000, 1)`); err != nil {
		t.Fatalf("forge goose version: %v", err)
	}
	_ = db.Close()
	_, _, err = run(t, "backup", "--config="+cfgPath, "--out="+filepath.Join(dir, "x.tar.gz"))
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Config {
		t.Fatalf("backup of a newer schema = %v, want exit.Config (78)", err)
	}
	for _, v := range []string{"1000000", fmt.Sprint(countMigrations(t))} {
		if !strings.Contains(err.Error(), v) {
			t.Fatalf("error %q does not name version %s", err, v)
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, "x.tar.gz")); statErr == nil {
		t.Fatal("a refused backup left an archive behind")
	}
}

func TestBackupReachesItsOwnConfigFlag(t *testing.T) {
	_, _, err := run(t, "backup", "--config=/nonexistent/dilla.toml", "--out=-")
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/dilla.toml") {
		t.Fatalf("backup --config did not reach the flag: %v", err)
	}
}

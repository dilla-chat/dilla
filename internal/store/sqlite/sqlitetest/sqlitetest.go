// Package sqlitetest hands tests a fully migrated SQLite database without running every migration
// for every test. The migrations run once per test binary into a template; each later call writes a
// byte copy of it. Under the race detector a full migration run cost ~1.4 s per database after
// migrations 00050 and 00051 (an ADD COLUMN with a CHECK and a DROP/RENAME COLUMN re-parse the
// whole schema), and internal/api builds about 900 of them, which pushed its CI job past its budget
// (PR #121). What the migrations do is pinned by internal/store's migration tests, which still run
// them one by one. Nothing in a production build imports this package; it is imported only from
// _test.go files.
package sqlitetest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/jonasthim/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/jonasthim/dilla/internal/store/sqlite/migrations"
)

var (
	once     sync.Once
	template []byte
	buildErr error
)

// MigratedFile writes a database at path holding every migration, the same file a goose Up over an
// empty one leaves (its goose_db_version table included). The caller opens it with sqlite.OpenWrite
// and sqlite.OpenRead as before.
func MigratedFile(t testing.TB, path string) {
	t.Helper()
	once.Do(func() { template, buildErr = build() })
	if buildErr != nil {
		t.Fatalf("sqlitetest: migrate the template: %v", buildErr)
	}
	if err := os.WriteFile(path, template, 0o600); err != nil {
		t.Fatalf("sqlitetest: write %s: %v", path, err)
	}
}

// build migrates an empty file, checkpoints its WAL into the main file and returns the file's bytes.
func build() ([]byte, error) {
	dir, err := os.MkdirTemp("", "dilla-sqlitetest-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }() // a leftover temp dir is the OS's to clean
	path := filepath.Join(dir, "template.db")
	db, err := sqlite.OpenWrite(path)
	if err != nil {
		return nil, err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sqlitemigrations.FS)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("provider: %w", err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("up: %w", err)
	}
	if _, err := db.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("checkpoint: %w", err)
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path) //nolint:gosec // G304: the template this function just wrote
}

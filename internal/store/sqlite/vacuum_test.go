package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pragmaInt(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var v int
	if err := db.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("pragma %s: %v", name, err)
	}
	return v
}

func fillAndDelete(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, body BLOB)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 500) INSERT INTO t(id, body) SELECT i, zeroblob(4000) FROM n`,
		`DELETE FROM t WHERE id > 10`,
	} {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// legacyDB builds a database the way an install from before this change has it: auto_vacuum=0, rows deleted so
// pages are free, then opened through OpenWrite like dillad does.
func legacyDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=auto_vacuum(0)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	fillAndDelete(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestEnsureIncrementalVacuumConverts(t *testing.T) {
	db := legacyDB(t)
	if got := pragmaInt(t, db, "auto_vacuum"); got != 0 {
		t.Fatalf("setup: auto_vacuum = %d, want 0", got)
	}
	if pragmaInt(t, db, "freelist_count") == 0 {
		t.Fatal("setup: no free pages")
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	if err := EnsureIncrementalVacuum(context.Background(), db, log); err != nil {
		t.Fatal(err)
	}
	if got := pragmaInt(t, db, "auto_vacuum"); got != 2 {
		t.Errorf("auto_vacuum = %d, want 2", got)
	}
	if got := pragmaInt(t, db, "freelist_count"); got != 0 {
		t.Errorf("freelist_count = %d, want 0", got)
	}
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM t`).Scan(&n); err != nil || n != 10 {
		t.Errorf("rows = %d, %v; want 10", n, err)
	}
	out := buf.String()
	for _, want := range []string{"compacting the database", "database compacted", "page_count_before", "freelist_count_before", "page_count_after", "freelist_count_after", "duration"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if c := strings.Count(strings.TrimSpace(out), "\n") + 1; c != 2 {
		t.Errorf("%d log lines, want 2:\n%s", c, out)
	}
}

func TestEnsureIncrementalVacuumIdempotent(t *testing.T) {
	db := legacyDB(t)
	ctx := context.Background()
	if err := EnsureIncrementalVacuum(ctx, db, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := EnsureIncrementalVacuum(ctx, db, slog.New(slog.NewTextHandler(&buf, nil))); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("second call logged: %s", buf.String())
	}
}

func TestIncrementalVacuumFrees(t *testing.T) {
	db, err := OpenWrite(filepath.Join(t.TempDir(), "new.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fillAndDelete(t, db)
	before := pragmaInt(t, db, "freelist_count")
	if before == 0 {
		t.Fatal("setup: no free pages")
	}
	freed, err := IncrementalVacuum(context.Background(), db, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if freed <= 0 || freed != before-pragmaInt(t, db, "freelist_count") {
		t.Errorf("freed = %d, before = %d", freed, before)
	}
}

// A VACUUM INTO copy (dillad backup, the pre-migration backup) of a converted database stays incremental, so a
// restored backup is not compacted again at the next start.
func TestVacuumIntoKeepsIncremental(t *testing.T) {
	db := legacyDB(t)
	if err := EnsureIncrementalVacuum(context.Background(), db, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if _, err := db.ExecContext(context.Background(), `VACUUM INTO ?`, copyPath); err != nil {
		t.Fatal(err)
	}
	cp, err := OpenRead(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cp.Close() })
	if got := pragmaInt(t, cp, "auto_vacuum"); got != 2 {
		t.Errorf("copy auto_vacuum = %d, want 2", got)
	}
}

// The VACUUM of a WAL database lands in the -wal file; without a checkpoint the main file keeps its old size.
func TestEnsureIncrementalVacuumShrinksFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=auto_vacuum(0)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, body BLOB)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 2000) INSERT INTO t(id, body) SELECT i, zeroblob(4000) FROM n`,
		`DELETE FROM t WHERE id > 10`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
	} {
		if _, err := raw.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	read, err := OpenRead(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	if _, err := read.ExecContext(context.Background(), `SELECT count(*) FROM t`); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Size() < 6_000_000 {
		t.Fatalf("setup: file is %v, err %v", st, err)
	}
	var buf bytes.Buffer
	if err := EnsureIncrementalVacuum(context.Background(), db, slog.New(slog.NewTextHandler(&buf, nil))); err != nil {
		t.Fatal(err)
	}
	pages := pragmaInt(t, db, "page_count")
	size := int64(pragmaInt(t, db, "page_size"))
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > int64(pages)*size+4096 {
		t.Errorf("file is %d bytes, want <= %d (page_count %d)", st.Size(), int64(pages)*size+4096, pages)
	}
	if w, err := os.Stat(path + "-wal"); err == nil && w.Size() != 0 {
		t.Errorf("-wal is %d bytes, want 0", w.Size())
	}
	if !strings.Contains(buf.String(), "file_bytes_before") || !strings.Contains(buf.String(), "file_bytes_after") {
		t.Errorf("log lacks file_bytes:\n%s", buf.String())
	}
}

// IncrementalVacuum's freed pages are in the WAL until a checkpoint; the pass checkpoints so the file shrinks.
func TestIncrementalVacuumShrinksFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inc.db")
	db, err := OpenWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, body BLOB)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 500) INSERT INTO t(id, body) SELECT i, zeroblob(4000) FROM n`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
		`DELETE FROM t WHERE id > 10`,
	} {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := IncrementalVacuum(context.Background(), db, 1000); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() >= before.Size() {
		t.Errorf("file did not shrink: %d -> %d", before.Size(), after.Size())
	}
}

func TestNewDatabaseIsIncremental(t *testing.T) {
	db, err := OpenWrite(filepath.Join(t.TempDir(), "new.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE x (a)`); err != nil {
		t.Fatal(err)
	}
	if got := pragmaInt(t, db, "auto_vacuum"); got != 2 {
		t.Errorf("auto_vacuum = %d, want 2", got)
	}
}

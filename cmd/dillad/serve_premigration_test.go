package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/dilla-chat/dilla/internal/exit"
)

func writeCopies(t *testing.T, dbPath string, stamps ...int) {
	t.Helper()
	for _, s := range stamps {
		if err := os.WriteFile(fmt.Sprintf("%s.pre-migration-%d", dbPath, s), []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPreMigrationCopiesKeepTheNewestThree(t *testing.T) {
	db := filepath.Join(t.TempDir(), "dilla.db")
	writeCopies(t, db, 1, 2, 3, 4, 5)
	if err := prunePreMigrationCopies(db, 3, "", quietLog()); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for s, want := range map[int]bool{1: false, 2: false, 3: true, 4: true, 5: true} {
		if got := exists(fmt.Sprintf("%s.pre-migration-%d", db, s)); got != want {
			t.Errorf("copy %d present = %v, want %v", s, got, want)
		}
	}
}

func TestPreMigrationPruneLeavesUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "dilla.db")
	writeCopies(t, db, 1, 2, 3, 4)
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := db + ".pre-migration-0"
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	others := []string{db + "-wal", db + "-shm", db + ".bak", db + ".pre-migration-abc", db + ".pre-migration-", db + ".pre-migration-9.bak", filepath.Join(dir, "other.db.pre-migration-1")}
	for _, o := range others {
		if err := os.WriteFile(o, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prunePreMigrationCopies(db, 3, "", quietLog()); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, o := range append(others, link, outside) {
		if !exists(o) {
			t.Errorf("%s was removed", o)
		}
	}
	if exists(db + ".pre-migration-1") {
		t.Error("the oldest numeric copy survived")
	}
}

func TestPreMigrationPruneRunsOnlyAfterSuccess(t *testing.T) {
	h := newCLIHarness(t)
	h.SeedRows(t, 5)
	h.UseMigrations(t, brokenMigrationFS(t))
	writeCopies(t, h.cfg.DB.Path, 1, 2, 3, 4, 5)
	code, out := h.Run(t, "serve", "--config="+h.ConfigPath)
	if code != int(exit.Data) {
		t.Fatalf("exit = %d, want %d: %s", code, exit.Data, out)
	}
	for s := 1; s <= 5; s++ {
		if p := fmt.Sprintf("%s.pre-migration-%d", h.cfg.DB.Path, s); !exists(p) {
			t.Errorf("%s was pruned by a failed migration", p)
		}
	}
}

func TestPreMigrationPruneKeepsThisStartsCopyWhenTheClockWentBack(t *testing.T) {
	db := filepath.Join(t.TempDir(), "dilla.db")
	writeCopies(t, db, 100, 150, 200, 300)
	if err := prunePreMigrationCopies(db, 3, db+".pre-migration-150", quietLog()); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for s, want := range map[int]bool{100: false, 150: true, 200: true, 300: true} {
		if got := exists(fmt.Sprintf("%s.pre-migration-%d", db, s)); got != want {
			t.Errorf("copy %d present = %v, want %v", s, got, want)
		}
	}
}

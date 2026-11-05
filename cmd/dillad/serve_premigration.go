package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// preMigrationKeep is how many <db>.pre-migration-<unix> copies stay after a
// successful migration; every upgrade leaves one, and a large database piles
// them up fast.
const preMigrationKeep = 3

// prunePreMigrationCopies removes all but the newest keep copies named
// <dbPath>.pre-migration-<unix time>, newest by the number in the name. Only
// regular files with exactly that name are considered, so the -wal and -shm
// files, a .bak and anything with a non-numeric suffix are never touched, and
// a symlink is skipped rather than followed. protect is the copy this start
// took: it is always kept and counts toward keep, because a clock that went
// backwards can rank it below older copies and it is the one an operator is
// told to restore. A copy that cannot be removed is logged here and the rest
// are still tried; only a failure to list the directory is returned.
func prunePreMigrationCopies(dbPath string, keep int, protect string, log *slog.Logger) error {
	dir, prefix := filepath.Dir(dbPath), filepath.Base(dbPath)+".pre-migration-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list pre-migration copies: %w", err)
	}
	type copyFile struct {
		path string
		unix uint64
	}
	var copies []copyFile
	for _, e := range entries {
		suffix, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok || suffix == "" || strings.Trim(suffix, "0123456789") != "" {
			continue
		}
		unix, err := strconv.ParseUint(suffix, 10, 64)
		if err != nil {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		copies = append(copies, copyFile{p, unix})
	}
	if len(copies) <= keep {
		return nil
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i].unix > copies[j].unix })
	slots, protected := keep, false
	for _, c := range copies {
		if c.path == protect {
			slots, protected = slots-1, true
		}
	}
	var removed []string
	for _, c := range copies {
		if c.path == protect && protected {
			continue
		}
		if slots > 0 {
			slots--
			continue
		}
		if err := os.Remove(c.path); err != nil {
			log.Warn("could not remove an old pre-migration copy", "path", c.path, "err", err)
			continue
		}
		removed = append(removed, c.path)
	}
	if len(removed) > 0 {
		log.Info("pruned pre-migration copies", "kept", keep, "removed", len(removed), "files", removed)
	}
	return nil
}

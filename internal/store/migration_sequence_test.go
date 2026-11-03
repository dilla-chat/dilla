package store_test

import (
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	pgmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// The embedded migrations are numbered 1..N with no gap, on both engines, and the two engines
// carry the same number of them. goose applies a gap in order on a fresh database, but a database
// that ran a build with the gap (23 without 21) is refused by a later build that fills it: the
// missing migration never ran. A branch that takes a number early must merge after the ones below
// it, and this test is what fails on main when it does not.
func TestMigrationsAreNumberedWithoutGaps(t *testing.T) {
	counts := map[string]int{}
	for name, fsys := range map[string]fs.FS{"sqlite": sqlitemigrations.FS, "postgres": pgmigrations.FS} {
		entries, err := fs.ReadDir(fsys, ".")
		if err != nil {
			t.Fatalf("%s: read the migrations: %v", name, err)
		}
		seen := map[int]string{}
		for _, e := range entries {
			prefix, _, ok := strings.Cut(e.Name(), "_")
			if !ok || !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			v, err := strconv.Atoi(prefix)
			if err != nil || v < 1 {
				t.Fatalf("%s: migration %s has no positive numeric prefix", name, e.Name())
			}
			if other, dup := seen[v]; dup {
				t.Fatalf("%s: migrations %s and %s share version %d", name, other, e.Name(), v)
			}
			seen[v] = e.Name()
		}
		if len(seen) == 0 {
			t.Fatalf("%s: no embedded migrations at all", name)
		}
		for v := 1; v <= len(seen); v++ {
			if _, ok := seen[v]; !ok {
				t.Errorf("%s: migration %d is missing, but %d migrations exist: the numbering must run 1..%d with no gap", name, v, len(seen), len(seen))
			}
		}
		counts[name] = len(seen)
	}
	if counts["sqlite"] != counts["postgres"] {
		t.Errorf("sqlite has %d migrations and postgres %d; the engines carry the same set", counts["sqlite"], counts["postgres"])
	}
}

// The schema files sqlc reads are numbered 001..M with no gap on both engines (a letter after the
// number, 006a..006f, splits one number across files and is still one number), and the engines
// carry the same count. Each migration carries one schema file's body, so a gap here is a schema
// file that went missing or was numbered past its turn.
func TestSchemaFilesAreNumberedWithoutGaps(t *testing.T) {
	lead := regexp.MustCompile(`^([0-9]+)[a-z]?_.*\.sql$`)
	counts := map[string]int{}
	for _, engine := range []string{"sqlite", "postgres"} {
		entries, err := os.ReadDir(engine + "/schema")
		if err != nil {
			t.Fatalf("%s: read the schema files: %v", engine, err)
		}
		seen := map[int]bool{}
		for _, e := range entries {
			m := lead.FindStringSubmatch(e.Name())
			if m == nil {
				t.Fatalf("%s: schema file %s is not NNN_name.sql", engine, e.Name())
			}
			v, _ := strconv.Atoi(m[1])
			seen[v] = true
		}
		for v := 1; v <= len(seen); v++ {
			if !seen[v] {
				t.Errorf("%s: schema file %03d is missing, but %d numbers exist: they must run 001..%03d with no gap", engine, v, len(seen), len(seen))
			}
		}
		counts[engine] = len(seen)
	}
	if counts["sqlite"] != counts["postgres"] {
		t.Errorf("sqlite has %d schema numbers and postgres %d; the engines carry the same set", counts["sqlite"], counts["postgres"])
	}
}

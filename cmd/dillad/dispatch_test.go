package main

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// countMigrations is the goose version a fully migrated database reports: the highest number among the
// embedded migration files. It is not the file count, because the numbers need not be contiguous (00021 can be
// taken by a branch that is not released yet).
func countMigrations(t *testing.T) int {
	t.Helper()
	entries, err := sqlitemigrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	highest := 0
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			t.Fatalf("migration %s does not start with a number: %v", e.Name(), err)
		}
		highest = max(highest, v)
	}
	if highest == 0 {
		t.Fatal("no embedded migrations at all")
	}
	return highest
}

func run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	err := dispatch(args, &out, &errBuf)
	return out.String(), errBuf.String(), err
}

func TestServeReachesItsOwnConfigFlag(t *testing.T) {
	// gap-82 verdict 7, measured: a flag registered only on the global FlagSet
	// is NOT reached by `dillad serve --config=X`. Each subcommand registers it.
	_, _, err := run(t, "serve", "--config=/nonexistent/dilla.toml")
	if err == nil {
		t.Fatal("serve accepted a config path that does not exist")
	}
	if !strings.Contains(err.Error(), "/nonexistent/dilla.toml") {
		t.Fatalf("error %q does not name the path from --config; the flag was not reached", err)
	}
}

func TestHelpGoesToStdoutAndExitsZero(t *testing.T) {
	out, errBuf, err := run(t, "--help")
	if err != nil {
		t.Fatalf("--help returned %v, want nil (exit 0)", err)
	}
	if !strings.Contains(out, "serve") || !strings.Contains(out, "migrate") {
		t.Fatalf("--help did not list the verbs on stdout: %q", out)
	}
	if errBuf != "" {
		t.Fatalf("--help wrote to stderr: %q", errBuf)
	}
}

// I15 (fix wave): --help on any verb prints that verb's usage once, on stdout, exits 0 and runs
// nothing: no backup archive in the working directory, no error from a verb body that went on.
// backup gets a real config, so a verb that ran on would write its archive; the others get a
// missing one, so a verb that ran on would fail rather than serve.
func TestHelpOnEveryVerbPrintsUsageOnceAndRunsNothing(t *testing.T) {
	cfg := bootstrapServeConfig(t)
	missing := "--config=" + filepath.Join(t.TempDir(), "missing.toml")
	for _, args := range [][]string{
		{"serve", missing, "--help"},
		{"init", "--help"},
		{"migrate", "up", missing, "--help"},
		{"migrate", "status", missing, "-h"},
		{"doctor", missing, "--help"},
		{"version", "--help"},
		{"backup", "--config=" + cfg, "--help"},
		{"backup", "verify", "--help"},
		{"restore", "--config=" + cfg, "--help"},
	} {
		t.Run(strings.Join(args[:len(args)-1], " "), func(t *testing.T) {
			t.Chdir(t.TempDir())
			out, errBuf, err := run(t, args...)
			if err != nil {
				t.Fatalf("%v = %v, want nil (exit 0)", args, err)
			}
			if n := strings.Count(out, "Usage of dillad"); n != 1 {
				t.Errorf("%v printed the usage %d times on stdout, want once:\n%s", args, n, out)
			}
			if errBuf != "" {
				t.Errorf("%v wrote to stderr: %q", args, errBuf)
			}
			if entries, _ := os.ReadDir("."); len(entries) != 0 {
				t.Errorf("%v left %d files in the working directory: the verb ran", args, len(entries))
			}
		})
	}
}

func TestUnknownFlagGoesToStderrAndExitsTwo(t *testing.T) {
	_, errBuf, err := run(t, "serve", "--nope")
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Usage {
		t.Fatalf("unknown flag gave %v, want exit.Usage (2)", err)
	}
	if errBuf == "" {
		t.Fatal("the usage message did not reach stderr")
	}
	if strings.Count(errBuf, "flag provided but not defined") > 1 {
		t.Fatalf("the usage message was printed twice:\n%s", errBuf)
	}
}

// admin was the last reserved verb ("not in this build", exit 3); it is a dispatcher now, so the
// bare verb is a usage error with the admin usage block, and nothing answers exit 3 any more.
func TestAdminIsDispatchedNotReserved(t *testing.T) {
	_, errBuf, err := run(t, "admin")
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Usage {
		t.Fatalf("admin gave %v, want exit.Usage (2)", err)
	}
	if !strings.Contains(errBuf, "usage: dillad admin") {
		t.Fatalf("admin did not print its usage block: %q", errBuf)
	}
	for name, v := range verbs() {
		if v.run == nil {
			t.Fatalf("verb %s has no handler", name)
		}
	}
}

func TestInitBootstrapsAndRefusesASecondRun(t *testing.T) {
	dir := t.TempDir()
	out, _, err := run(t, "init", "--agree-tos", "--config="+filepath.Join(dir, "dilla.toml"), "--data-dir="+dir, "--domain=chat.example", "--public-ip=203.0.113.7")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	cfgPath := filepath.Join(dir, "dilla.toml")
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat dilla.toml: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("dilla.toml has mode %04o, want 0600", perm)
	}
	// I12 (fix wave): the relay binds a local interface address resolved at serve, never the public
	// IP, which bridged Docker and a NATed LXC cannot bind.
	written, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load dilla.toml: %v", err)
	}
	if written.TURN.RelayIP != "auto" {
		t.Fatalf("init wrote turn.relay_ip = %q, want \"auto\"", written.TURN.RelayIP)
	}
	if !strings.Contains(out, "https://chat.example/i/") {
		t.Fatalf("init did not print one invite link: %q", out)
	}
	if strings.Count(out, "/i/") != 1 {
		t.Fatalf("init printed %d invite links, want exactly 1", strings.Count(out, "/i/"))
	}
	statusOut, _, err := run(t, "migrate", "status", "--config="+cfgPath)
	if err != nil {
		t.Fatalf("migrate status: %v", err)
	}
	// One version per embedded migration, derived rather than written down: task 19 added
	// 00002_mls.sql and the literal "1" this used to look for would still have matched the
	// "current 2, target 2" line by accident for another eight migrations.
	want := fmt.Sprintf("current %d, target %d", countMigrations(t), countMigrations(t))
	if !strings.Contains(statusOut, want) {
		t.Fatalf("migrate status = %q, want it to report %q", statusOut, want)
	}
	if _, _, err := run(t, "init", "--agree-tos", "--config="+filepath.Join(dir, "dilla.toml"), "--data-dir="+dir, "--domain=chat.example", "--public-ip=203.0.113.7"); err == nil {
		t.Fatal("a second init overwrote an initialised data directory")
	}
}

// The file init writes must be one every other verb can read back. Without the
// TURN and LiveKit secret files init now generates, Validate refuses it with
// `turn.shared_secret_file: is required` and every later verb exits 78.
func TestInitWritesAConfigThatLoadsBack(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := run(t, "init", "--agree-tos", "--config="+filepath.Join(dir, "dilla.toml"), "--data-dir="+dir, "--domain=chat.example", "--public-ip=203.0.113.7"); err != nil {
		t.Fatalf("init: %v", err)
	}
	c, err := config.Load(filepath.Join(dir, "dilla.toml"))
	if err != nil {
		t.Fatalf("config.Load after init: %v", err)
	}
	if c.TURN.SharedSecretFile == "" || c.LiveKit.APISecretFile == "" {
		t.Fatal("init left the TURN or LiveKit secret file unset while their features are enabled")
	}
	for _, path := range []string{c.TURN.SharedSecretFile, c.LiveKit.APISecretFile} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s has mode %04o, want 0600", path, perm)
		}
	}
}

// init must not accept the CA's subscriber agreement on the operator's behalf.
func TestInitRefusesWithoutAgreeTOS(t *testing.T) {
	dir := t.TempDir()
	_, errBuf, err := run(t, "init", "--config="+filepath.Join(dir, "dilla.toml"), "--data-dir="+dir, "--domain=chat.example", "--public-ip=203.0.113.7")
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Usage {
		t.Fatalf("init without --agree-tos gave %v, want exit.Usage (2)", err)
	}
	if !strings.Contains(errBuf, "--agree-tos") {
		t.Fatalf("the refusal does not name the flag: %q", errBuf)
	}
	if _, err := os.Stat(filepath.Join(dir, "dilla.toml")); err == nil {
		t.Fatal("a refused init still wrote dilla.toml")
	}
}

// init's key history must be the parseable structure protocol/03 § Instance
// keys fixes, and a second init must not rotate it (it must refuse outright).
func TestInitWritesAParseableKeyHistory(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := run(t, "init", "--agree-tos", "--config="+filepath.Join(dir, "dilla.toml"), "--data-dir="+dir, "--domain=chat.example", "--public-ip=203.0.113.7"); err != nil {
		t.Fatalf("init: %v", err)
	}
	db, err := openSQLiteForTest(filepath.Join(dir, "dilla.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var blob []byte
	if err := db.QueryRowContext(t.Context(), `SELECT key_history FROM instances`).Scan(&blob); err != nil {
		t.Fatalf("read key_history: %v", err)
	}
	var doc []any
	if err := cborx.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("key_history is not deterministic CBOR: %v", err)
	}
	if len(doc) != 2 {
		t.Fatalf("key_history has %d elements, want [v, entries]", len(doc))
	}
	entries, ok := doc[1].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("key_history holds %v, want one external-sender entry and one franking entry", doc[1])
	}
	first, _ := entries[0].([]any)
	if len(first) != 6 {
		t.Fatalf("an entry has %d elements, want 6", len(first))
	}
	if pub, _ := first[2].([]byte); len(pub) != ed25519.PublicKeySize {
		t.Fatalf("the external sender public key is %d bytes, want 32", len(pub))
	}
	if secret, _ := first[3].([]byte); len(secret) != ed25519.SeedSize {
		t.Fatalf("the external sender seed is %d bytes, want 32", len(secret))
	}
	second, _ := entries[1].([]any)
	if key, _ := second[3].([]byte); len(key) != 32 {
		t.Fatalf("K_frank is %d bytes, want 32", len(key))
	}
}

// `migrate status --config=X` must reach its own --config flag. Go's flag
// package stops parsing at the first non-flag argument, so the sub-verb has to
// be popped before Parse or --config is never seen and the default
// /etc/dilla/dilla.toml is read instead.
func TestMigrateStatusReachesItsConfigFlag(t *testing.T) {
	_, _, err := run(t, "migrate", "status", "--config=/nonexistent/dilla.toml")
	if err == nil {
		t.Fatal("migrate status accepted a config path that does not exist")
	}
	if !strings.Contains(err.Error(), "/nonexistent/dilla.toml") {
		t.Fatalf("error %q does not name the path from --config; the sub-verb swallowed the flag", err)
	}
}

func TestInitInsertsExactlyOneInvite(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := run(t, "init", "--agree-tos", "--config="+filepath.Join(dir, "dilla.toml"), "--data-dir="+dir, "--domain=chat.example", "--public-ip=203.0.113.7"); err != nil {
		t.Fatalf("init: %v", err)
	}
	db, err := openSQLiteForTest(filepath.Join(dir, "dilla.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM invites WHERE grants_admin = 1`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d admin invites, want 1", n)
	}
}

func TestDoctorExitsUnavailableOnAPragmaMismatch(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := run(t, "init", "--agree-tos", "--config="+filepath.Join(dir, "dilla.toml"), "--data-dir="+dir, "--domain=chat.example", "--public-ip=203.0.113.7"); err != nil {
		t.Fatalf("init: %v", err)
	}
	cfgPath := filepath.Join(dir, "dilla.toml")
	// Turn WAL off behind dillad's back. doctor's first pragma half reads
	// journal_mode on a mode=ro connection that sets no pragmas, so it sees
	// "delete"; a read-back through sqlite.OpenWrite could not, because that
	// DSN carries _pragma=journal_mode(WAL) and modernc would convert the file
	// back to WAL before anything read it.
	db, err := openSQLiteForTest(filepath.Join(dir, "dilla.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `PRAGMA journal_mode = DELETE`); err != nil {
		t.Fatalf("set journal_mode: %v", err)
	}
	db.Close()
	out, _, err := run(t, "doctor", "--config="+cfgPath)
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Unavailable {
		t.Fatalf("doctor gave %v, want exit.Unavailable (69)", err)
	}
	if !strings.Contains(out, "delete") {
		t.Fatalf("doctor did not name the journal mode it found: %q", out)
	}
}

// openSQLiteForTest must NOT set any pragma of its own, or it would convert the
// file back to WAL and mask the state the test above sets up.
func openSQLiteForTest(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path)
}

func TestVersionPrintsTheVCSRevision(t *testing.T) {
	out, _, err := run(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	for _, want := range []string{"dillad", "go1.", "CGO_ENABLED="} {
		if !strings.Contains(out, want) {
			t.Fatalf("version output %q does not contain %q", out, want)
		}
	}
}

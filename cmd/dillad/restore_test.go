package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/ops"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// cliHarness is one `dillad init`-ed instance driven through the verb
// dispatcher in-process, with the restore verb's clock held still.
type cliHarness struct {
	DataDir    string
	ConfigPath string
	cfg        *config.Config
	clk        *clock.Fake
	archives   string
	device     id.ID
	n          int

	// Added with the admin verbs' tests (admin_harness_test.go).
	stderr strings.Builder // everything every Run wrote to stderr, for LogOutput
	shared store.Repository
	tokens map[id.ID]string // user -> the bearer token SeedSession minted
}

func newCLIHarness(t *testing.T) *cliHarness {
	t.Helper()
	dir, cfgPath := initInstance(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	h := &cliHarness{
		DataDir: dir, ConfigPath: cfgPath, cfg: cfg,
		clk:      clock.NewFake(time.Unix(1_790_000_000, 0).UTC()),
		archives: t.TempDir(),
	}
	prev := cliClock
	cliClock = h.clk
	t.Cleanup(func() { cliClock = prev })
	return h
}

// Run dispatches one verb and returns its exit code and everything it said:
// stdout, stderr and the returned error's text, which is what exit.Exit
// prints to stderr in the real binary.
func (h *cliHarness) Run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	stdout, stderr, err := run(t, args...)
	h.stderr.WriteString(stderr)
	out := stdout + stderr
	if err == nil {
		return 0, out
	}
	out += "dillad: " + err.Error() + "\n"
	var code exit.Code
	if errors.As(err, &code) {
		return int(code), out
	}
	return int(exit.Fail), out
}

func (h *cliHarness) Now() int64 { return h.clk.Now().Unix() }

// repo opens the instance's database as it stands now: a restore replaces the
// directory, so nothing is held across one.
func (h *cliHarness) repo(t *testing.T) store.Repository {
	t.Helper()
	write, err := sqlite.OpenWrite(h.cfg.DB.Path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	read, err := sqlite.OpenRead(h.cfg.DB.Path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	return sqlite.New(write, read)
}

func (h *cliHarness) with(t *testing.T, fn func(ctx context.Context, repo store.Repository)) {
	t.Helper()
	repo := h.repo(t)
	defer func() { _ = repo.Close() }()
	fn(t.Context(), repo)
}

func (h *cliHarness) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	db, err := openSQLiteForTest(h.cfg.DB.Path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int64
	if err := db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// MakeBackup runs `dillad backup` into a directory outside the data directory
// and returns the archive's path.
func (h *cliHarness) MakeBackup(t *testing.T) string {
	t.Helper()
	h.n++
	path := filepath.Join(h.archives, fmt.Sprintf("backup-%d.tar.gz", h.n))
	if code, out := h.Run(t, "backup", "--config="+h.ConfigPath, "--out="+path); code != 0 {
		t.Fatalf("backup exit %d: %s", code, out)
	}
	return path
}

// MakeBackupWithSchemaVersion is MakeBackup with the manifest's schema_version
// rewritten. The manifest is member 0 and covers every OTHER member, so the
// rewritten archive still verifies member by member.
func (h *cliHarness) MakeBackupWithSchemaVersion(t *testing.T, v int64) string {
	t.Helper()
	path := h.MakeBackup(t)
	rewriteManifest(t, path, func(m map[string]any) { m["schema_version"] = v })
	return path
}

func rewriteManifest(t *testing.T, path string, edit func(map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gw)
	for i := 0; ; i++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read member: %v", err)
		}
		if i == 0 {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("manifest: %v", err)
			}
			edit(m)
			if body, err = json.Marshal(m); err != nil {
				t.Fatalf("manifest: %v", err)
			}
			hdr.Size = int64(len(body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write member: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (h *cliHarness) Generation(t *testing.T) uint64 {
	t.Helper()
	var g uint64
	h.with(t, func(ctx context.Context, repo store.Repository) {
		in, err := repo.GetInstance(ctx)
		if err != nil {
			t.Fatalf("GetInstance: %v", err)
		}
		g = in.Generation
	})
	return g
}

// ensureDevice creates, once, the user and device the seeded KeyPackages
// belong to.
func (h *cliHarness) ensureDevice(t *testing.T, ctx context.Context, repo store.Repository) id.ID {
	t.Helper()
	if h.device != (id.ID{}) {
		return h.device
	}
	now := h.Now()
	user, device := id.New(), id.New()
	if err := repo.CreateUser(ctx, store.UserRow{
		ID: user, Username: "seed", Display: "seed",
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := repo.CreateDevice(ctx, store.DeviceRow{
		ID: device, UserID: user, DSKPub: make([]byte, 32), CredentialBlob: []byte{1},
		LastSeen: now, Created: now,
	}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	h.device = device
	return device
}

func (h *cliHarness) group(callID *id.ID) store.GroupRow {
	return store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, TargetID: id.New(), CallID: callID, Ciphersuite: 1,
		ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: h.Now(),
	}
}

// SeedGroupsAndKeyPackages writes n open groups, n ordinary KeyPackages and
// one last-resort KeyPackage.
func (h *cliHarness) SeedGroupsAndKeyPackages(t *testing.T, n int) {
	t.Helper()
	h.with(t, func(ctx context.Context, repo store.Repository) {
		device := h.ensureDevice(t, ctx, repo)
		kps := make([]store.KeyPackageRow, 0, n+1)
		for i := range n {
			if err := repo.CreateGroup(ctx, h.group(nil)); err != nil {
				t.Fatalf("CreateGroup: %v", err)
			}
			kps = append(kps, store.KeyPackageRow{
				DeviceID: device, KPRef: []byte(fmt.Sprintf("kp-%d", i)), Blob: []byte{1},
				Expires: h.Now() + 86400*30, Created: h.Now(),
			})
		}
		kps = append(kps, store.KeyPackageRow{
			DeviceID: device, KPRef: []byte("kp-last-resort"), Blob: []byte{1}, LastResort: 1,
			Expires: h.Now() + 86400*30, Created: h.Now(),
		})
		if err := repo.PutKeyPackages(ctx, device, kps); err != nil {
			t.Fatalf("PutKeyPackages: %v", err)
		}
	})
}

// SeedLiveCall writes an open call group: on this schema a live call IS its
// call group (R9), which EndAllVoiceSessions closes.
func (h *cliHarness) SeedLiveCall(t *testing.T) {
	t.Helper()
	h.with(t, func(ctx context.Context, repo store.Repository) {
		call := id.New()
		g := h.group(&call)
		g.Kind = 1
		if err := repo.CreateGroup(ctx, g); err != nil {
			t.Fatalf("CreateGroup (call): %v", err)
		}
	})
}

func (h *cliHarness) Groups(t *testing.T) []store.GroupRow {
	t.Helper()
	var out []store.GroupRow
	h.with(t, func(ctx context.Context, repo store.Repository) {
		rows, err := repo.ListGroupsForRetention(ctx, id.ID{}, 1000)
		if err != nil {
			t.Fatalf("ListGroupsForRetention: %v", err)
		}
		out = rows
	})
	return out
}

// CountKeyPackages returns the last-resort KeyPackages and the ordinary ones.
func (h *cliHarness) CountKeyPackages(t *testing.T) (lastResort, ordinary int64) {
	t.Helper()
	return h.count(t, `SELECT COUNT(*) FROM key_packages WHERE last_resort = 1`),
		h.count(t, `SELECT COUNT(*) FROM key_packages WHERE last_resort = 0`)
}

func (h *cliHarness) LiveCalls(t *testing.T) int64 {
	t.Helper()
	return h.count(t, `SELECT COUNT(*) FROM mls_groups WHERE call_id IS NOT NULL AND closed_at IS NULL`)
}

// SeedRows writes n audit rows the rollback test counts.
func (h *cliHarness) SeedRows(t *testing.T, n int) {
	t.Helper()
	h.with(t, func(ctx context.Context, repo store.Repository) {
		for i := range n {
			if err := repo.Audit(ctx, store.AuditRow{
				Action: "test.seed", Target: strconv.Itoa(i), At: h.Now(),
			}); err != nil {
				t.Fatalf("Audit: %v", err)
			}
		}
	})
}

func (h *cliHarness) Rows(t *testing.T) int64 {
	t.Helper()
	return h.count(t, `SELECT COUNT(*) FROM audit_log WHERE action = 'test.seed'`)
}

// UseMigrations swaps the SQLite migration set serve builds its goose provider
// over, for the rest of the test.
func (h *cliHarness) UseMigrations(t *testing.T, fsys fs.FS) {
	t.Helper()
	prev := sqliteMigrations
	sqliteMigrations = fsys
	t.Cleanup(func() { sqliteMigrations = prev })
}

// brokenMigrationFS is every embedded SQLite migration plus one, numbered past
// all of them, whose Up calls a function that does not exist.
func brokenMigrationFS(t *testing.T) fs.FS {
	t.Helper()
	m := fstest.MapFS{}
	entries, err := sqlitemigrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read the migrations: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := sqlitemigrations.FS.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		m[e.Name()] = &fstest.MapFile{Data: body}
	}
	m["99999_broken.sql"] = &fstest.MapFile{Data: []byte(
		"-- +goose Up\nCREATE TABLE broken_half (x INTEGER) STRICT;\nSELECT no_such_function();\n\n-- +goose Down\nDROP TABLE broken_half;\n")}
	return m
}

func TestRestoreRefusesWhileServeHoldsTheLock(t *testing.T) {
	h := newCLIHarness(t)
	archive := h.MakeBackup(t)
	l, err := ops.AcquireLock(h.DataDir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer func() { _ = l.Release() }()
	code, stderr := h.Run(t, "restore", "--config="+h.ConfigPath, "--from="+archive)
	if code != int(exit.TempFail) {
		t.Fatalf("exit = %d, want %d: %s", code, exit.TempFail, stderr)
	}
	if !strings.Contains(stderr, "in use") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRestoreRefusesANewerSchema(t *testing.T) {
	h := newCLIHarness(t)
	archive := h.MakeBackupWithSchemaVersion(t, 1_000_000)
	code, stderr := h.Run(t, "restore", "--config="+h.ConfigPath, "--from="+archive)
	if code != int(exit.Config) {
		t.Fatalf("exit = %d, want %d: %s", code, exit.Config, stderr)
	}
	if !strings.Contains(stderr, "1000000") {
		t.Fatalf("stderr does not name the archive's schema version: %q", stderr)
	}
	if !strings.Contains(stderr, strconv.Itoa(countMigrations(t))) {
		t.Fatalf("stderr does not name the binary's highest migration: %q", stderr)
	}
}

func TestRestoreBumpsTheGenerationAndArmsTheHeal(t *testing.T) {
	h := newCLIHarness(t)
	h.SeedGroupsAndKeyPackages(t, 3)
	// Seeded BEFORE the backup, unlike the plan's snippet: a call seeded after
	// it is not in the archive, and the assertion below would hold whatever
	// restore did.
	h.SeedLiveCall(t)
	archive := h.MakeBackup(t)
	before := h.Generation(t)

	code, stderr := h.Run(t, "restore", "--config="+h.ConfigPath, "--from="+archive)
	if code != 0 {
		t.Fatalf("restore exit = %d: %s", code, stderr)
	}
	if after := h.Generation(t); after != before+1 {
		t.Fatalf("generation = %d, want %d", after, before+1)
	}
	groups := h.Groups(t)
	if len(groups) != 4 {
		t.Fatalf("%d groups after the restore, want 4", len(groups))
	}
	for _, g := range groups {
		if !g.EpochUnknown {
			t.Fatalf("group %x is not epoch-unknown after restore", g.GroupID)
		}
		if g.HealDeadline == nil || *g.HealDeadline != h.Now()+24*3600 {
			t.Fatalf("group %x heal deadline = %v, want now+24h", g.GroupID, g.HealDeadline)
		}
	}
	kept, purged := h.CountKeyPackages(t)
	if purged != 0 {
		t.Fatalf("%d non-last-resort KeyPackages survived the restore", purged)
	}
	if kept == 0 {
		t.Fatal("the last-resort KeyPackages were purged too")
	}
	if h.LiveCalls(t) != 0 {
		t.Fatal("a live call survived the restore")
	}
	// serve finishes the restore on its next start (ds.OnRestore), which is
	// what makes the CLI path and the in-process path converge.
	h.with(t, func(ctx context.Context, repo store.Repository) {
		pending, err := repo.GetSetting(ctx, ds.RestorePendingKey)
		if err != nil || string(pending) != strconv.FormatUint(before+1, 10) {
			t.Fatalf("%s = %q, %v; want %d", ds.RestorePendingKey, pending, err, before+1)
		}
	})
	// The operator is told exactly what happens next.
	if !strings.Contains(stderr, "24h") || !strings.Contains(stderr, "live calls ended") {
		t.Fatalf("restore did not print the operator summary: %q", stderr)
	}
	// I17 (fix wave): R38's wording is on the restore output too, not only in the archive and the
	// backup output: the operator restoring an archive is told what it holds.
	if !strings.Contains(stderr, "dillad restore: note: "+ops.ContentNotice) {
		t.Fatalf("restore did not print R38's content notice: %q", stderr)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	h := newCLIHarness(t)
	h.SeedGroupsAndKeyPackages(t, 2)
	archive := h.MakeBackup(t)
	before := h.Generation(t)
	h.SeedRows(t, 3) // written after the backup: a dry run leaves them
	code, stdout := h.Run(t, "restore", "--config="+h.ConfigPath, "--from="+archive, "--dry-run")
	if code != 0 {
		t.Fatalf("dry run exit = %d: %s", code, stdout)
	}
	if h.Generation(t) != before {
		t.Fatal("--dry-run bumped the generation")
	}
	if n := h.Rows(t); n != 3 {
		t.Fatalf("--dry-run replaced the database: %d seeded rows left, want 3", n)
	}
	for _, want := range []string{"generation", "blobs", "rows", "mls_groups", "dillad restore: note: " + ops.ContentNotice} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the plan does not mention %q: %q", want, stdout)
		}
	}
	siblings, err := filepath.Glob(h.DataDir + ".*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	inside, err := filepath.Glob(filepath.Join(h.DataDir, ".*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(siblings)+len(inside) != 0 {
		t.Fatalf("--dry-run left %v %v", siblings, inside)
	}
}

func TestServeRefusesAnInterruptedRestore(t *testing.T) {
	h := newCLIHarness(t)
	if err := os.WriteFile(filepath.Join(h.DataDir, ops.RestoreMarkerFile), []byte("half-way\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	code, out := h.Run(t, "serve", "--config="+h.ConfigPath)
	if code != int(exit.Data) || !strings.Contains(out, "interrupted") {
		t.Fatalf("serve over an interrupted restore = %d, want %d naming it: %s", code, exit.Data, out)
	}
}

// The migration set is INJECTED, not embedded: the migrations directory is
// //go:embed-ed at compile time, so a test cannot add a file to it at run
// time. serve builds its goose provider over the package variable
// sqliteMigrations (deviation: the plan's sqlite.MigrateFS does not exist,
// because nothing in internal/store/sqlite runs migrations).
func TestAFailedMigrationAtServeRestoresThePreMigrationBackup(t *testing.T) {
	h := newCLIHarness(t)
	h.SeedRows(t, 20)
	h.UseMigrations(t, brokenMigrationFS(t))
	code, stderr := h.Run(t, "serve", "--config="+h.ConfigPath)
	if code == 0 {
		t.Fatal("serve started despite a failing migration")
	}
	if code != int(exit.Data) {
		t.Fatalf("exit = %d, want %d: %s", code, exit.Data, stderr)
	}
	if !strings.Contains(stderr, "restored the pre-migration backup") {
		t.Fatalf("stderr = %q", stderr)
	}
	if n := h.Rows(t); n != 20 {
		t.Fatalf("rows after the rollback = %d, want 20", n)
	}
	if !strings.Contains(stderr, "heal") {
		t.Fatalf("the operator was not told the heal protocol is pending: %q", stderr)
	}
	if v := h.count(t, `SELECT MAX(version_id) FROM goose_db_version`); v != int64(countMigrations(t)) {
		t.Fatalf("schema after the rollback = %d, want %d", v, countMigrations(t))
	}
	if n := h.count(t, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'broken_half'`); n != 0 {
		t.Fatal("the failed migration's half is still in the database")
	}
}

// A backup taken while a restore is pending carries the pending mark, and a
// failed migration rolls back to a database that still carries it: the
// operator is told the heal is still owed.
func TestAFailedMigrationKeepsAPendingRestorePending(t *testing.T) {
	h := newCLIHarness(t)
	archive := h.MakeBackup(t)
	if code, out := h.Run(t, "restore", "--config="+h.ConfigPath, "--from="+archive); code != 0 {
		t.Fatalf("restore exit %d: %s", code, out)
	}
	h.UseMigrations(t, brokenMigrationFS(t))
	code, stderr := h.Run(t, "serve", "--config="+h.ConfigPath)
	if code != int(exit.Data) {
		t.Fatalf("exit = %d, want %d: %s", code, exit.Data, stderr)
	}
	if !strings.Contains(stderr, "the group heal protocol is still pending") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// The lock rule end to end, against a real `dillad serve`: a backup runs beside
// it, a second serve and a restore are refused with 75, and once serve has
// stopped the restore goes through and the next serve finishes it (OnRestore at
// start: the deadlines are re-armed from the start and the mark is cleared).
func TestServeAdmitsABackupRefusesARestoreAndFinishesOneAtStart(t *testing.T) {
	h := newCLIHarness(t)
	setListen(t, h.ConfigPath, "127.0.0.1:0")
	h.SeedGroupsAndKeyPackages(t, 2)

	var stdout, stderr syncBuffer
	served := make(chan error, 1)
	go func() { served <- dispatch([]string{"serve", "--config=" + h.ConfigPath}, &stdout, &stderr) }()
	addr := waitForListenAddr(t, &stdout, served)
	if err := probe(addr); err != nil {
		t.Fatalf("probe /healthz: %v", err)
	}
	before := h.Generation(t)
	assertServedGeneration(t, addr, before)

	archive := h.MakeBackup(t)
	if code, out := h.Run(t, "serve", "--config="+h.ConfigPath); code != int(exit.TempFail) {
		t.Fatalf("a second serve = %d, want %d: %s", code, exit.TempFail, out)
	}
	if code, out := h.Run(t, "restore", "--config="+h.ConfigPath, "--from="+archive); code != int(exit.TempFail) ||
		!strings.Contains(out, "stop dillad first") {
		t.Fatalf("restore under a live serve = %d, want %d: %s", code, exit.TempFail, out)
	}
	stopServe(t, served)

	if code, out := h.Run(t, "restore", "--config="+h.ConfigPath, "--from="+archive); code != 0 {
		t.Fatalf("restore exit %d: %s", code, out)
	}
	restored := h.Generation(t)
	if restored <= before {
		t.Fatalf("restore left the generation at %d, from %d", restored, before)
	}

	stdout, stderr = syncBuffer{}, syncBuffer{}
	served = make(chan error, 1)
	go func() { served <- dispatch([]string{"serve", "--config=" + h.ConfigPath}, &stdout, &stderr) }()
	addr = waitForListenAddr(t, &stdout, served)
	if err := probe(addr); err != nil {
		t.Fatalf("probe /healthz: %v", err)
	}
	// Invariant 11 on the wire: the restarted instance tells every client about
	// the restore, in the header on every response and in the discovery
	// document, not only in its database.
	assertServedGeneration(t, addr, restored)
	stopServe(t, served)

	if g := h.Generation(t); g != restored {
		t.Fatalf("serve moved the generation from %d to %d finishing the restore", restored, g)
	}
	h.with(t, func(ctx context.Context, repo store.Repository) {
		pending, err := repo.GetSetting(ctx, ds.RestorePendingKey)
		if err != nil || len(pending) != 0 {
			t.Fatalf("%s after serve = %q, %v; want cleared", ds.RestorePendingKey, pending, err)
		}
	})
	// serve runs on the system clock; the restore ran on the harness's, years
	// behind it. The re-armed deadline is therefore a day past the wall clock.
	floor := time.Now().Add(23 * time.Hour).Unix()
	for _, g := range h.Groups(t) {
		if !g.EpochUnknown || g.HealDeadline == nil || *g.HealDeadline < floor {
			t.Fatalf("group %x after serve: epoch_unknown %v, deadline %v; want re-armed from serve's start",
				g.GroupID, g.EpochUnknown, g.HealDeadline)
		}
	}
}

// assertServedGeneration fails unless the serve at addr puts want on the wire:
// the X-Dilla-Generation header of a response and element 4 (generation) of
// the GET /v1/instance discovery document (protocol/09).
func assertServedGeneration(t *testing.T, addr string, want uint64) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/v1/instance", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/instance: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/instance = %d, %v", res.StatusCode, err)
	}
	if got := res.Header.Get("X-Dilla-Generation"); got != strconv.FormatUint(want, 10) {
		t.Fatalf("X-Dilla-Generation = %q, want %d", got, want)
	}
	var doc []any
	if err := cborx.Unmarshal(body, &doc); err != nil || len(doc) != 11 {
		t.Fatalf("decode the discovery document: %d elements, %v", len(doc), err)
	}
	if got, ok := doc[4].(uint64); !ok || got != want {
		t.Fatalf("discovery generation = %#v, want %d", doc[4], want)
	}
}

func setListen(t *testing.T, cfgPath, listen string) {
	t.Helper()
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	servePlain(c, listen)
	f, err := os.OpenFile(cfgPath, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("reopen dilla.toml: %v", err)
	}
	if err := c.WriteConfig(f); err != nil {
		_ = f.Close()
		t.Fatalf("rewrite dilla.toml: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close dilla.toml: %v", err)
	}
}

func stopServe(t *testing.T, served <-chan error) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve never returned")
	}
}

func TestRestoreNeedsFrom(t *testing.T) {
	h := newCLIHarness(t)
	if code, out := h.Run(t, "restore", "--config="+h.ConfigPath); code != int(exit.Usage) {
		t.Fatalf("restore without --from = %d, want %d: %s", code, exit.Usage, out)
	}
}

func TestRestoreRefusesADamagedArchive(t *testing.T) {
	h := newCLIHarness(t)
	archive := h.MakeBackup(t)
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(archive, raw[:len(raw)*2/3], 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	before := h.Generation(t)
	if code, out := h.Run(t, "restore", "--config="+h.ConfigPath, "--from="+archive); code != int(exit.Data) {
		t.Fatalf("restore of a damaged archive = %d, want %d: %s", code, exit.Data, out)
	}
	if h.Generation(t) != before {
		t.Fatal("a refused restore changed the instance")
	}
}

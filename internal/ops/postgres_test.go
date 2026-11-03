package ops_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/ops"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	pgmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
)

// The Postgres leg: db/dilla.dump is store.DumpPostgres's DILLADMP container,
// taken inside its one repeatable-read transaction, and the archive verifies.
func TestPostgresBackupArchivesTheDumpAndVerifies(t *testing.T) {
	dsn := os.Getenv("DILLA_TEST_PG")
	if dsn == "" {
		t.Skip("DILLA_TEST_PG is unset: Postgres tests run in CI's service container")
	}
	ctx := t.Context()
	// A database of the test's own: migrating the shared CI database here raced the
	// store packages' own migrations ("relation instances already exists").
	dsn = freshPostgres(t, dsn)
	db, err := postgres.Open(dsn, 8, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
	if err != nil {
		t.Fatalf("postgres provider: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("postgres up: %v", err)
	}
	repo := postgres.New(db)
	t.Cleanup(func() { _ = repo.Close() })
	// The CI database is shared by every package's Postgres leg: create the
	// instance row only when no earlier test did.
	if _, err := repo.GetInstance(ctx); errors.Is(err, store.ErrNotFound) {
		if err := repo.CreateInstance(ctx, store.InstanceRow{
			InstanceID: id.New(), ExternalSenderKeyID: id.New(), KeyHistory: []byte{0x82, 0x01, 0x80},
			FrankingKeyID: id.New(), Generation: 1, PolicyVersion: 1, Created: 1,
		}); err != nil {
			t.Fatalf("CreateInstance: %v", err)
		}
	} else if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}

	dir := t.TempDir()
	c := config.Default()
	c.Instance.Domain = "dilla.test"
	c.Instance.DataDir = dir
	c.DB.Driver, c.DB.Path, c.DB.DSN = "postgres", "", dsn
	c.Blobs.Dir = filepath.Join(dir, "blobs")
	c.Derive()

	var buf bytes.Buffer
	man, err := ops.Backup(ctx, c, repo, ops.BackupOptions{Out: &buf, Clock: clock.NewFake(time.Unix(1_790_000_000, 0))})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if man.Engine != "postgres" {
		t.Fatalf("engine = %q", man.Engine)
	}
	if names := tarNames(t, buf.Bytes()); names[1] != "dilla-backup/db/dilla.dump" {
		t.Fatalf("member 1 = %q, want the dump", names[1])
	}
	if dump := tarMember(t, buf.Bytes(), "dilla-backup/db/dilla.dump"); !strings.HasPrefix(string(dump), store.DumpMagic) {
		t.Fatalf("db/dilla.dump does not start with %q", store.DumpMagic)
	}
	if _, err := ops.Verify(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// freshPostgres creates a database of the test's own, dropped at cleanup. A
// restore empties every dilla table, which the CI database every package's
// Postgres leg shares cannot allow while those legs run beside it.
func freshPostgres(t *testing.T, dsn string) string {
	t.Helper()
	admin, err := postgres.Open(dsn, 1, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	name := "dilla_restore_" + id.New().String()[:12]
	if _, err := admin.ExecContext(t.Context(), `CREATE DATABASE `+name); err != nil {
		_ = admin.Close()
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DILLA_TEST_PG is not a URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func postgresConfig(t *testing.T, dsn string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	c := config.Default()
	c.Instance.Domain = "dilla.test"
	c.Instance.DataDir = dir
	c.DB.Driver, c.DB.Path, c.DB.DSN = "postgres", "", dsn
	c.Blobs.Dir = filepath.Join(dir, "blobs")
	c.Derive()
	return c
}

// The Postgres restore: the dump is loaded in one transaction into a database at
// the archive's schema (an empty one is migrated up to it first), the identity
// sequences are moved past the restored rows, and the heal transaction runs on
// the result.
func TestPostgresRestoreLoadsTheDumpAndArmsTheHeal(t *testing.T) {
	base := os.Getenv("DILLA_TEST_PG")
	if base == "" {
		t.Skip("DILLA_TEST_PG is unset: Postgres tests run in CI's service container")
	}
	ctx := t.Context()
	dsn := freshPostgres(t, base)
	db, err := postgres.Open(dsn, 4, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
	if err != nil {
		t.Fatalf("postgres provider: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("postgres up: %v", err)
	}
	repo := postgres.New(db)
	t.Cleanup(func() { _ = repo.Close() })
	clk := clock.NewFake(time.Unix(1_790_000_000, 0))
	now := clk.Now().Unix()

	if err := repo.CreateInstance(ctx, store.InstanceRow{
		InstanceID: id.New(), ExternalSenderKeyID: id.New(), KeyHistory: []byte{0x82, 0x01, 0x80},
		FrankingKeyID: id.New(), Generation: 1, PolicyVersion: 1, Created: now,
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	user, device := id.New(), id.New()
	if err := repo.CreateUser(ctx, store.UserRow{
		ID: user, Username: "pg", Display: "pg",
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := repo.CreateDevice(ctx, store.DeviceRow{
		ID: device, UserID: user, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, LastSeen: now, Created: now,
	}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	group := id.New()
	if err := repo.CreateGroup(ctx, store.GroupRow{
		GroupID: group, Binding: []byte{0x80}, TargetID: id.New(), Ciphersuite: 1,
		ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: now,
	}); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := repo.PutKeyPackages(ctx, device, []store.KeyPackageRow{
		{DeviceID: device, KPRef: []byte("ordinary"), Blob: []byte{1}, Expires: now + 86400, Created: now},
		{DeviceID: device, KPRef: []byte("last-resort"), Blob: []byte{1}, LastResort: 1, Expires: now + 86400, Created: now},
	}); err != nil {
		t.Fatalf("PutKeyPackages: %v", err)
	}
	for i := range 3 {
		if err := repo.Audit(ctx, store.AuditRow{Action: "pg.seed", Target: strconv.Itoa(i), At: now}); err != nil {
			t.Fatalf("Audit: %v", err)
		}
	}

	c := postgresConfig(t, dsn)
	var buf bytes.Buffer
	if _, err := ops.Backup(ctx, c, repo, ops.BackupOptions{Out: &buf, Clock: clk}); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	dry, err := ops.Restore(ctx, c, ops.RestoreOptions{From: bytes.NewReader(buf.Bytes()), DryRun: true, Clock: clk})
	if err != nil {
		t.Fatalf("Restore --dry-run: %v", err)
	}
	if dry.RowCounts["audit_log"] != 3 || dry.RowCounts["key_packages"] != 2 || dry.RowCounts["mls_groups"] != 1 {
		t.Fatalf("dry-run row counts = %v", dry.RowCounts)
	}

	// After the backup: more audit rows and a generation bump, which the
	// restore must undo and outrun respectively.
	for i := range 4 {
		if err := repo.Audit(ctx, store.AuditRow{Action: "pg.after", Target: strconv.Itoa(i), At: now}); err != nil {
			t.Fatalf("Audit: %v", err)
		}
	}
	if _, err := repo.BumpGeneration(ctx); err != nil {
		t.Fatalf("BumpGeneration: %v", err)
	}

	plan, err := ops.Restore(ctx, c, ops.RestoreOptions{From: bytes.NewReader(buf.Bytes()), Clock: clk})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if plan.GenerationOld != 2 || plan.GenerationNew != 3 {
		t.Fatalf("generations %d -> %d, want 2 -> 3", plan.GenerationOld, plan.GenerationNew)
	}
	audit, err := repo.ListAudit(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(audit) != 3 {
		t.Fatalf("%d audit rows after the restore, want the archive's 3", len(audit))
	}
	// The identity sequence was moved past the restored rows: a new row lands.
	if err := repo.Audit(ctx, store.AuditRow{Action: "pg.new", Target: "x", At: now}); err != nil {
		t.Fatalf("Audit after the restore: %v", err)
	}
	g, err := repo.GetGroup(ctx, group)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if !g.EpochUnknown || g.HealDeadline == nil || *g.HealDeadline != plan.HealDeadline {
		t.Fatalf("group after the restore: epoch_unknown %v, deadline %v", g.EpochUnknown, g.HealDeadline)
	}
	if plan.KeyPackagesPurged != 1 {
		t.Fatalf("KeyPackagesPurged = %d, want 1", plan.KeyPackagesPurged)
	}
	in, err := repo.GetInstance(ctx)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if in.Generation != 3 {
		t.Fatalf("generation = %d, want 3", in.Generation)
	}

	// Into an empty database: restore migrates it to the archive's schema first.
	empty := postgresConfig(t, freshPostgres(t, base))
	if _, err := ops.Restore(ctx, empty, ops.RestoreOptions{From: bytes.NewReader(buf.Bytes()), Clock: clk}); err != nil {
		t.Fatalf("Restore into an empty database: %v", err)
	}
	edb, err := postgres.Open(empty.DB.DSN, 2, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	erepo := postgres.New(edb)
	defer func() { _ = erepo.Close() }()
	ein, err := erepo.GetInstance(ctx)
	if err != nil {
		t.Fatalf("GetInstance on the empty database: %v", err)
	}
	if ein.InstanceID != in.InstanceID {
		t.Fatal("the empty database did not receive the archived instance")
	}
}

// The load and the heal are ONE transaction on Postgres: a heal that fails
// after the dump has loaded leaves the database exactly as it was, never the
// archive at its old generation with no group epoch-unknown (invariant 11).
func TestPostgresAFailedHealLeavesTheDatabaseUntouched(t *testing.T) {
	base := os.Getenv("DILLA_TEST_PG")
	if base == "" {
		t.Skip("DILLA_TEST_PG is unset: Postgres tests run in CI's service container")
	}
	ctx := t.Context()
	dsn := freshPostgres(t, base)
	db, err := postgres.Open(dsn, 4, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
	if err != nil {
		t.Fatalf("postgres provider: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("postgres up: %v", err)
	}
	repo := postgres.New(db)
	t.Cleanup(func() { _ = repo.Close() })
	clk := clock.NewFake(time.Unix(1_790_000_000, 0))
	now := clk.Now().Unix()
	if err := repo.CreateInstance(ctx, store.InstanceRow{
		InstanceID: id.New(), ExternalSenderKeyID: id.New(), KeyHistory: []byte{0x82, 0x01, 0x80},
		FrankingKeyID: id.New(), Generation: 1, PolicyVersion: 1, Created: now,
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	for i := range 3 {
		if err := repo.Audit(ctx, store.AuditRow{Action: "pg.seed", Target: strconv.Itoa(i), At: now}); err != nil {
			t.Fatalf("Audit: %v", err)
		}
	}
	c := postgresConfig(t, dsn)
	var buf bytes.Buffer
	if _, err := ops.Backup(ctx, c, repo, ops.BackupOptions{Out: &buf, Clock: clk}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	for i := range 4 {
		if err := repo.Audit(ctx, store.AuditRow{Action: "pg.after", Target: strconv.Itoa(i), At: now}); err != nil {
			t.Fatalf("Audit: %v", err)
		}
	}

	ops.SetBeforeHealCommit(t, func() error { return errors.New("injected: the heal transaction fails") })
	_, err = ops.Restore(ctx, c, ops.RestoreOptions{From: bytes.NewReader(buf.Bytes()), Clock: clk})
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("Restore with a failing heal = %v, want the injected error", err)
	}
	if strings.Contains(err.Error(), "already holds the archive") {
		t.Fatalf("a failed heal claims the database was replaced: %v", err)
	}
	audit, err := repo.ListAudit(ctx, 0, 100)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(audit) != 7 {
		t.Fatalf("%d audit rows after a failed restore, want the live 7: the load committed without its heal", len(audit))
	}
	in, err := repo.GetInstance(ctx)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if in.Generation != 1 {
		t.Fatalf("generation = %d after a failed restore, want 1", in.Generation)
	}
}

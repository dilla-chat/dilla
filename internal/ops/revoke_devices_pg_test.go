package ops_test

import (
	"os"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/ops"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	pgmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
)

// TestRevokeAllDevicesOnPostgres runs the revoke-all on the Postgres engine: a database of the
// test's own, migrated, with the same two-device person as the SQLite tests.
func TestRevokeAllDevicesOnPostgres(t *testing.T) {
	dsn := os.Getenv("DILLA_TEST_PG")
	if dsn == "" {
		t.Skip("DILLA_TEST_PG is unset: Postgres tests run in CI's service container")
	}
	ctx := t.Context()
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
	h := &opsHarness{t: t, Repo: repo, Clock: clock.NewFake(time.Unix(1_790_000_000, 0).UTC())}

	uid, devices := seedFactorUser(t, h)
	res, err := ops.RevokeAllDevices(ctx, repo, uid, h.Clock.Now().Unix())
	if err != nil {
		t.Fatalf("RevokeAllDevices: %v", err)
	}
	if len(res.Revoked) != len(devices) || res.Sessions != int64(len(devices)) {
		t.Fatalf("result = %d devices and %d sessions, want %d and %d", len(res.Revoked), res.Sessions, len(devices), len(devices))
	}
	for _, d := range devices {
		row, err := repo.GetDevice(ctx, d)
		if err != nil || row.RevokedAt == nil {
			t.Errorf("device %s was not revoked: %+v %v", d, row, err)
		}
		if n, _ := repo.CountSessionsByDevice(ctx, d); n != 0 {
			t.Errorf("device %s keeps %d sessions, want 0", d, n)
		}
	}
}

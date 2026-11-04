package dillad

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/jonasthim/dilla/internal/store/sqlite/migrations"
)

// Wave B task 3 security review I1: the maintenance tick prunes expired WebAuthn ceremonies, so a login begin nobody
// finished does not stay in webauthn_ceremonies for good.
func TestTheMaintenanceTickPrunesExpiredPasskeyCeremonies(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sweep.db")
	write, err := sqlite.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	read, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { repo.Close() })
	clk := clock.NewFake(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	cfg := config.Default()
	wa := cfg.Auth.WebAuthn
	wa.RPID, wa.RPDisplayName, wa.RPOrigins = "dilla.test", "dilla test", []string{"https://dilla.test"}
	pk, err := auth.NewPasskeys(auth.ConfigFromDilla(wa), repo, clk)
	if err != nil {
		t.Fatalf("NewPasskeys: %v", err)
	}
	s := &Server{
		o:        Options{Repo: repo, Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		throttle: auth.NewThrottle(cfg.Limits.Rate, cfg.Auth.Lockout, clk),
		limiter:  server.NewRateLimiter(cfg.Limits.Rate, clk), passkeys: pk,
	}
	// Every row, expired or not: expires > 0.
	rows := func() int64 {
		n, err := repo.CountOpenCeremonies(ctx, nil, 0)
		if err != nil {
			t.Fatalf("CountOpenCeremonies: %v", err)
		}
		return n
	}

	if _, _, err := pk.BeginLogin(ctx); err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	s.sweep(ctx)
	if got := rows(); got != 1 {
		t.Fatalf("a live ceremony after the sweep: %d rows, want 1", got)
	}
	clk.Advance(5*time.Minute + time.Second)
	if open, _ := pk.OpenLogins(ctx); open != 0 {
		t.Fatalf("an expired ceremony still counts as open: %d", open)
	}
	s.sweep(ctx)
	if got := rows(); got != 0 {
		t.Fatalf("an expired ceremony after the sweep: %d rows, want 0", got)
	}
}

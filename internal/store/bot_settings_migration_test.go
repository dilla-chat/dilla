package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/postgres"
	pgmigrations "github.com/jonasthim/dilla/internal/store/postgres/migrations"
	"github.com/jonasthim/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/jonasthim/dilla/internal/store/sqlite/migrations"
)

// Migration 00050 (Task 10c): an installation from before it keeps its capability byte, gains
// Polls and Reminders (bits 8 and 9, which it had implicitly), today's music defaults (100, 30
// minutes), no restart request and no version. Running the migration down and up again is safe.
func TestBotSettingsMigrationKeepsExistingInstallationsBehaviour(t *testing.T) {
	ctx := context.Background()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bot-settings.db")
		write, err := sqlite.OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		read, err := sqlite.OpenRead(path)
		if err != nil {
			t.Fatal(err)
		}
		p, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
		if err != nil {
			t.Fatal(err)
		}
		repo := sqlite.New(write, read)
		t.Cleanup(func() { repo.Close() })
		botSettingsMigration(ctx, t, p, repo, write, false)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("DILLA_TEST_PG")
		if dsn == "" {
			t.Skip("DILLA_TEST_PG is not set")
		}
		db, err := postgres.Open(freshPostgresDSN(t, dsn), 4, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		p, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
		if err != nil {
			t.Fatal(err)
		}
		repo := postgres.New(db)
		t.Cleanup(func() { repo.Close() })
		botSettingsMigration(ctx, t, p, repo, db, true)
	})
}

func botSettingsMigration(ctx context.Context, t *testing.T, p *goose.Provider, repo store.Repository, db *sql.DB, pg bool) {
	t.Helper()
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("initial up: %v", err)
	}
	community := seedCommunity(ctx, t, repo)
	approver := seedUser(ctx, t, repo).ID
	if _, err := p.DownTo(ctx, 49); err != nil {
		t.Fatalf("down to 49: %v", err)
	}
	installation := id.New()
	q := `INSERT INTO bot_installations(id, community_id, state, revision, capabilities, approved_by, approved_at, created, updated) VALUES(?,?,?,?,?,?,?,?,?)`
	if pg {
		q = `INSERT INTO bot_installations(id, community_id, state, revision, capabilities, approved_by, approved_at, created, updated) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`
	}
	if _, err := db.ExecContext(ctx, q, installation, community, int64(store.BotInstallationReady), int64(1), int64(0x91), approver, int64(1), int64(1), int64(1)); err != nil {
		t.Fatalf("seed a pre-00050 installation: %v", err)
	}
	for round := range 2 {
		if round == 1 {
			if _, err := p.DownTo(ctx, 49); err != nil {
				t.Fatalf("down again: %v", err)
			}
		}
		if _, err := p.Up(ctx); err != nil {
			t.Fatalf("round %d: apply 00050: %v", round, err)
		}
		row, err := repo.GetBotInstallation(ctx, installation)
		if err != nil {
			t.Fatal(err)
		}
		if row.Capabilities != 0x91|0x300 || row.MusicVolume != 100 || row.MusicIdleMinutes != 30 ||
			row.RestartRequested != 0 || row.RestartHandled != 0 || row.RuntimeVersion != nil {
			t.Fatalf("round %d: migrated installation = %+v", round, row)
		}
	}
}

// The settings methods on both engines: only the Music page's values, restart requests coalesce
// while one is pending, the runtime status is bound to the owning runtime and never claims more
// than was requested.
func TestBotSettingsStoreMethods(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			community := seedCommunity(ctx, t, repo)
			approver := seedUser(ctx, t, repo).ID
			runtimeID, otherRuntime := id.New(), id.New()
			for _, r := range []id.ID{runtimeID, otherRuntime} {
				if err := repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: r, TokenHash: append(r[:], r[:]...), Created: 1, Rotated: 1}); err != nil {
					t.Fatal(err)
				}
			}
			row := store.BotInstallationRow{ID: id.New(), CommunityID: community, RuntimeID: &runtimeID, State: store.BotInstallationReady,
				Revision: 1, Capabilities: 1 | 1<<9, ApprovedBy: approver, ApprovedAt: 1, Created: 1, Updated: 1}
			if err := repo.CreateBotInstallation(ctx, row); err != nil {
				t.Fatal(err)
			}
			if err := repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: id.New(), CommunityID: seedCommunity(ctx, t, repo),
				State: store.BotInstallationPending, Revision: 1, Capabilities: 1 << 10, ApprovedBy: approver, ApprovedAt: 1, Created: 1, Updated: 1}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("unknown capability bit 10 = %v, want ErrConflict", err)
			}
			got, err := repo.GetBotInstallation(ctx, row.ID)
			if err != nil || got.Capabilities != 1|1<<9 {
				t.Fatalf("capabilities round trip = %#x, %v", got.Capabilities, err)
			}
			for _, bad := range [][2]uint8{{60, 15}, {50, 10}, {0, 30}} {
				if err := repo.SetBotMusicSettings(ctx, row.ID, bad[0], bad[1]); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("SetBotMusicSettings(%v) = %v, want ErrConflict", bad, err)
				}
			}
			if err := repo.SetBotMusicSettings(ctx, row.ID, 25, 5); err != nil {
				t.Fatal(err)
			}
			if err := repo.SetBotMusicSettings(ctx, id.New(), 25, 5); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("settings on an unknown installation = %v", err)
			}
			for i, want := range []uint64{1, 1, 1} {
				n, err := repo.RequestBotRestart(ctx, row.ID)
				if err != nil || n != want {
					t.Fatalf("restart request %d = %d, %v, want %d (coalesced while pending)", i, n, err, want)
				}
			}
			v := "0.4.2"
			if err := repo.SetBotRuntimeStatus(ctx, row.ID, otherRuntime, &v, 1); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("status from another runtime = %v, want ErrNotFound", err)
			}
			if err := repo.SetBotRuntimeStatus(ctx, row.ID, runtimeID, &v, 7); err != nil {
				t.Fatal(err)
			}
			got, err = repo.GetBotInstallation(ctx, row.ID)
			if err != nil || got.MusicVolume != 25 || got.MusicIdleMinutes != 5 || got.RestartRequested != 1 || got.RestartHandled != 1 || got.RuntimeVersion == nil || *got.RuntimeVersion != v {
				t.Fatalf("after settings = %+v, %v", got, err)
			}
			if n, err := repo.RequestBotRestart(ctx, row.ID); err != nil || n != 2 {
				t.Fatalf("restart after the runtime handled one = %d, %v, want 2", n, err)
			}
			if err := repo.SetBotRuntimeStatus(ctx, row.ID, runtimeID, nil, 0); err != nil {
				t.Fatal(err)
			}
			got, _ = repo.GetBotInstallation(ctx, row.ID)
			if got.RestartHandled != 1 || got.RuntimeVersion != nil {
				t.Fatalf("handled went backwards or version kept: %+v", got)
			}
		})
	}
}

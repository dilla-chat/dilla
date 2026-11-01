package store_test

import (
	"context"
	"database/sql"
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

// controlMusicBit is api.PermControlMusic (protocol/09 § Permissions, bit 25).
const controlMusicBit uint64 = 1 << 25

// Migration 00048 maps the old per-installation DJ approvals onto the control_music role bit:
// every role the community's current (not removed) installation approved gains the bit, roles
// approved only by a removed installation and roles nobody approved do not, other bits are kept,
// and running the mapping again on the same data changes nothing.
func TestControlMusicMigrationMapsCurrentDJApprovals(t *testing.T) {
	ctx := context.Background()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "control-music.db")
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
		controlMusicMapping(ctx, t, p, repo, write, false)
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
		controlMusicMapping(ctx, t, p, repo, db, true)
	})
}

func controlMusicMapping(ctx context.Context, t *testing.T, p *goose.Provider, repo store.Repository, db *sql.DB, pg bool) {
	t.Helper()
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("initial up: %v", err)
	}
	community := seedCommunity(ctx, t, repo)
	other := seedCommunity(ctx, t, repo)
	approver := seedUser(ctx, t, repo).ID
	role := func(cid id.ID, name string, position, allow uint64) id.ID {
		t.Helper()
		r := store.RoleRow{ID: id.New(), CommunityID: cid, Name: name, Position: position, Allow: allow, Created: 1}
		if err := repo.PutRole(ctx, r); err != nil {
			t.Fatalf("PutRole %s: %v", name, err)
		}
		return r.ID
	}
	const send, kick = uint64(1 << 1), uint64(1 << 12)
	dj := role(community, "dj", 1, send)
	alreadyDJ := role(community, "already", 2, send|controlMusicBit)
	plain := role(community, "plain", 3, send|kick)
	oldDJ := role(other, "old dj", 1, send)
	installation := func(cid id.ID, removed bool) id.ID {
		t.Helper()
		row := store.BotInstallationRow{ID: id.New(), CommunityID: cid, State: store.BotInstallationReady, Revision: 1,
			Capabilities: 1, ApprovedBy: approver, ApprovedAt: 1, Created: 1, Updated: 1}
		if err := repo.CreateBotInstallation(ctx, row); err != nil {
			t.Fatalf("CreateBotInstallation: %v", err)
		}
		if removed {
			at := int64(2)
			row.RemovedAt = &at
			row.State = store.BotInstallationDisabled
			row.Revision = 2
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.UpdateBotInstallation(ctx, row.ID, 1, row) }); err != nil {
				t.Fatalf("remove installation: %v", err)
			}
		}
		return row.ID
	}
	current := installation(community, false)
	removed := installation(other, true)

	approve := func(installation, role id.ID) {
		t.Helper()
		q := `INSERT INTO dj_role_policy(installation_id,role_id,approved_by,approved_at) VALUES(?,?,?,?)`
		if pg {
			q = `INSERT INTO dj_role_policy(installation_id,role_id,approved_by,approved_at) VALUES($1,$2,$3,$4)`
		}
		if _, err := db.ExecContext(ctx, q, installation, role, approver, int64(1)); err != nil {
			t.Fatalf("seed dj_role_policy: %v", err)
		}
	}
	check := func(round string) {
		t.Helper()
		for _, tc := range []struct {
			name string
			role id.ID
			want uint64
		}{
			{"approved by the current installation", dj, send | controlMusicBit},
			{"already holding the bit", alreadyDJ, send | controlMusicBit},
			{"never approved", plain, send | kick},
			{"approved only by a removed installation", oldDJ, send},
		} {
			got, err := repo.GetRole(ctx, tc.role)
			if err != nil {
				t.Fatalf("%s: GetRole %s: %v", round, tc.name, err)
			}
			if got.Allow != tc.want {
				t.Fatalf("%s: role %s allow = %#x, want %#x", round, tc.name, got.Allow, tc.want)
			}
		}
	}

	for _, round := range []string{"first run", "second run"} {
		if _, err := p.DownTo(ctx, 47); err != nil {
			t.Fatalf("%s: down to the DJ policy schema: %v", round, err)
		}
		approve(current, dj)
		approve(current, alreadyDJ)
		approve(removed, oldDJ)
		if _, err := p.Up(ctx); err != nil {
			t.Fatalf("%s: apply 00048: %v", round, err)
		}
		check(round)
	}
	if _, err := db.ExecContext(ctx, `SELECT 1 FROM dj_role_policy`); err == nil {
		t.Fatal("dj_role_policy survived migration 00048")
	}
}

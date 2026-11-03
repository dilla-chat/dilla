package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	pgmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

func TestMemberEpisodeMigrationBackfillsLegacyMemberships(t *testing.T) {
	ctx := context.Background()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "episodes.db")
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
		legacyMembershipBackfill(ctx, t, p, repo, write, false)
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
		legacyMembershipBackfill(ctx, t, p, repo, db, true)
	})
}

func legacyMembershipBackfill(ctx context.Context, t *testing.T, p *goose.Provider, repo store.Repository, db *sql.DB, pg bool) {
	t.Helper()
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("initial up: %v", err)
	}
	community := seedCommunity(ctx, t, repo)
	user := seedUser(ctx, t, repo).ID
	if _, err := p.DownTo(ctx, 26); err != nil {
		t.Fatalf("down to pre-episode schema: %v", err)
	}
	query := `INSERT INTO members(community_id,user_id,joined,nick) VALUES(?,?,?,?)`
	if pg {
		query = `INSERT INTO members(community_id,user_id,joined,nick) VALUES($1,$2,$3,$4)`
	}
	if _, err := db.ExecContext(ctx, query, community, user, int64(123), "legacy"); err != nil {
		t.Fatalf("insert legacy membership: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("apply member episode migration: %v", err)
	}
	member, err := repo.GetMember(ctx, community, user)
	if err != nil || member.EpisodeID.IsZero() || member.Joined != 123 || member.Nick != "legacy" {
		t.Fatalf("backfilled legacy membership = %+v, %v", member, err)
	}
	if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.DeleteMember(ctx, community, user) }); err != nil {
		t.Fatalf("delete legacy member: %v", err)
	}
	if _, err := repo.GetMember(ctx, community, user); err == nil {
		t.Fatal("backfilled member survived deletion")
	}
}

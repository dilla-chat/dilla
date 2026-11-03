package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	pgmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// Migration 00018 goes down to 17 and up again on both engines: the messages survive, the class
// column and the slow-mode marks are dropped, and a fresh up starts with no class and no mark.
func TestMigration00018GoesDownAndUpAgain(t *testing.T) {
	ctx := context.Background()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "m18.db")
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
		downAndUp00018(ctx, t, p, repo, write)
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
		downAndUp00018(ctx, t, p, repo, db)
	})
}

func downAndUp00018(ctx context.Context, t *testing.T, p *goose.Provider, repo store.Repository, db *sql.DB) {
	t.Helper()
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	g := seedGroup(ctx, t, repo, 1)
	for i, c := range []*uint8{classPtr(0), classPtr(1), nil} {
		row := store.AppMessageRow{GroupID: g.GroupID, Seq: uint64(i + 1), Epoch: 1, UploaderDevice: id.New(),
			Blob: []byte("ct"), FrankingTag: make([]byte, 32), Size: 2, Created: 1, Class: c}
		if err := repo.PutAppMessage(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	ch := seedChannel(ctx, t, repo)
	u := seedUser(ctx, t, repo).ID
	if err := repo.PutSlowmodeMark(ctx, ch, u, 5); err != nil {
		t.Fatal(err)
	}

	if _, err := p.DownTo(ctx, 17); err != nil {
		t.Fatalf("down to 17: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mls_app_messages").Scan(&n); err != nil || n != 3 {
		t.Fatalf("after down: %d messages, %v; want 3", n, err)
	}
	if _, err := db.ExecContext(ctx, "SELECT class FROM mls_app_messages"); err == nil {
		t.Fatal("after down: mls_app_messages.class still exists")
	}
	if _, err := db.ExecContext(ctx, "SELECT 1 FROM slowmode_marks"); err == nil {
		t.Fatal("after down: slowmode_marks still exists")
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	rows, err := repo.ListAppMessages(ctx, g.GroupID, 1, 10)
	if err != nil || len(rows) != 3 || rows[0].Class != nil {
		t.Fatalf("after up again: %v, %v; want 3 messages with no class", rows, err)
	}
	if _, err := repo.LastClassZeroUploadAt(ctx, ch, u); err == nil {
		t.Fatal("after up again: the slow-mode mark came back")
	}
}

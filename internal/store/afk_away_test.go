package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	pgmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// seedAFKCommunity creates a community owned by a fresh user and makes each of members a member.
func seedAFKCommunity(ctx context.Context, t *testing.T, repo store.Repository, members ...id.ID) id.ID {
	t.Helper()
	owner := seedUser(ctx, t, repo)
	cid := id.New()
	if err := repo.CreateCommunity(ctx, store.CommunityRow{ID: cid, Owner: owner.ID, Name: "AFK", PolicyJSON: []byte("{}"), PolicyVersion: 1, Created: 1}); err != nil {
		t.Fatal(err)
	}
	for _, u := range members {
		if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: u, Joined: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return cid
}

// Setting away twice is idempotent and keeps the first since (R-AW7: since_ts settles races, so a
// second mark must not move it later). ClearAFKAway answers whether a row went.
func TestAFKAwaySetIsIdempotentAndKeepsTheFirstSince(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			a, b := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			cid := seedAFKCommunity(ctx, t, repo, a, b)

			if got, err := repo.ListAFKAway(ctx, cid); err != nil || len(got) != 0 {
				t.Fatalf("initial roster = %v, %v; want empty", got, err)
			}
			if set, err := repo.SetAFKAway(ctx, cid, a, 100); err != nil || !set {
				t.Fatalf("first SetAFKAway = %v, %v; want true", set, err)
			}
			if set, err := repo.SetAFKAway(ctx, cid, a, 200); err != nil || set {
				t.Fatalf("second SetAFKAway = %v, %v; want false", set, err)
			}
			if _, err := repo.SetAFKAway(ctx, cid, b, 300); err != nil {
				t.Fatal(err)
			}
			got, err := repo.ListAFKAway(ctx, cid)
			if err != nil {
				t.Fatal(err)
			}
			want := []store.AFKAway{{UserID: a, Since: 100}, {UserID: b, Since: 300}}
			slices.SortFunc(want, func(x, y store.AFKAway) int { return bytes.Compare(x.UserID[:], y.UserID[:]) })
			if !slices.Equal(got, want) {
				t.Fatalf("roster = %v, want %v (ordered by user id, first since kept)", got, want)
			}

			if cleared, err := repo.ClearAFKAway(ctx, cid, a); err != nil || !cleared {
				t.Fatalf("ClearAFKAway = %v, %v; want true", cleared, err)
			}
			if cleared, err := repo.ClearAFKAway(ctx, cid, a); err != nil || cleared {
				t.Fatalf("second ClearAFKAway = %v, %v; want false", cleared, err)
			}
			got, err = repo.ListAFKAway(ctx, cid)
			if err != nil || !slices.Equal(got, []store.AFKAway{{UserID: b, Since: 300}}) {
				t.Fatalf("roster after clear = %v, %v", got, err)
			}
		})
	}
}

// Only a member can be away: the composite foreign key onto members refuses anyone else, and the
// refusal is the typed ErrNotFound on both engines (AW1 review M1): a kick that lands between a
// handler's membership check and the insert must answer as a non-member does, never as a 500.
func TestAFKAwayRefusesANonMember(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			stranger := seedUser(ctx, t, repo).ID
			cid := seedAFKCommunity(ctx, t, repo)
			set, err := repo.SetAFKAway(ctx, cid, stranger, 1)
			if set || !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("SetAFKAway for a non-member = %v, %v; want false, store.ErrNotFound", set, err)
			}
			// The same for a community that does not exist.
			if _, err := repo.SetAFKAway(ctx, id.New(), stranger, 1); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("SetAFKAway in no community = %v; want store.ErrNotFound", err)
			}
		})
	}
}

// Leaving the community (and so a kick or a ban, which delete the membership) drops the away row
// by cascade, and only that community's.
func TestAFKAwayCascadesWhenTheMemberLeaves(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			u := seedUser(ctx, t, repo).ID
			left := seedAFKCommunity(ctx, t, repo, u)
			kept := seedAFKCommunity(ctx, t, repo, u)
			for _, cid := range []id.ID{left, kept} {
				if _, err := repo.SetAFKAway(ctx, cid, u, 5); err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.DeleteMember(ctx, left, u); err != nil {
				t.Fatal(err)
			}
			if got, err := repo.ListAFKAway(ctx, left); err != nil || len(got) != 0 {
				t.Fatalf("roster of the left community = %v, %v; want empty", got, err)
			}
			if got, err := repo.ListAFKAway(ctx, kept); err != nil || len(got) != 1 {
				t.Fatalf("roster of the other community = %v, %v; want the one row", got, err)
			}
		})
	}
}

// ClearAFKAwayUser (an admission anywhere, or the last gateway connection closing) clears every
// community of that user, answers which, and leaves other users alone.
func TestClearAFKAwayUserClearsEveryCommunity(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			u, other := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			c1 := seedAFKCommunity(ctx, t, repo, u, other)
			c2 := seedAFKCommunity(ctx, t, repo, u)
			c3 := seedAFKCommunity(ctx, t, repo, u)
			for _, cid := range []id.ID{c1, c2} {
				if _, err := repo.SetAFKAway(ctx, cid, u, 7); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := repo.SetAFKAway(ctx, c1, other, 8); err != nil {
				t.Fatal(err)
			}
			cleared, err := repo.ClearAFKAwayUser(ctx, u)
			if err != nil {
				t.Fatal(err)
			}
			want := []id.ID{c1, c2}
			slices.SortFunc(want, func(x, y id.ID) int { return bytes.Compare(x[:], y[:]) })
			if !slices.Equal(cleared, want) {
				t.Fatalf("cleared = %v, want %v (c3 had no row)", cleared, want)
			}
			for _, cid := range []id.ID{c2, c3} {
				if got, err := repo.ListAFKAway(ctx, cid); err != nil || len(got) != 0 {
					t.Fatalf("roster of %v = %v, %v; want empty", cid, got, err)
				}
			}
			if got, err := repo.ListAFKAway(ctx, c1); err != nil || !slices.Equal(got, []store.AFKAway{{UserID: other, Since: 8}}) {
				t.Fatalf("roster of c1 = %v, %v; want only the other user", got, err)
			}
			if again, err := repo.ClearAFKAwayUser(ctx, u); err != nil || len(again) != 0 {
				t.Fatalf("second ClearAFKAwayUser = %v, %v; want none", again, err)
			}
		})
	}
}

// ListAFKAwayFirst bounds the read in the query (AW2 review M3): the first limit rows by user id.
func TestListAFKAwayFirstLimitsTheRead(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			users := []id.ID{seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID}
			cid := seedAFKCommunity(ctx, t, repo, users...)
			for _, u := range users {
				if _, err := repo.SetAFKAway(ctx, cid, u, 7); err != nil {
					t.Fatal(err)
				}
			}
			all, err := repo.ListAFKAway(ctx, cid)
			if err != nil || len(all) != 3 {
				t.Fatalf("ListAFKAway = %v, %v", all, err)
			}
			got, err := repo.ListAFKAwayFirst(ctx, cid, 2)
			if err != nil || !slices.Equal(got, all[:2]) {
				t.Fatalf("ListAFKAwayFirst(2) = %v, %v; want %v", got, err, all[:2])
			}
		})
	}
}

// AFKAwayExists and AFKAwayExistsUser are the read-only lookups of an unseeded index (fix round 3,
// N-B): a row in the community, and a row in any community.
func TestAFKAwayExistsLookups(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			u, v := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			one := seedAFKCommunity(ctx, t, repo, u, v)
			two := seedAFKCommunity(ctx, t, repo, u, v)
			if _, err := repo.SetAFKAway(ctx, one, u, 3); err != nil {
				t.Fatal(err)
			}
			for _, c := range []struct {
				cid, uid id.ID
				want     bool
			}{{one, u, true}, {two, u, false}, {one, v, false}} {
				if got, err := repo.AFKAwayExists(ctx, c.cid, c.uid); err != nil || got != c.want {
					t.Fatalf("AFKAwayExists = %v, %v; want %v", got, err, c.want)
				}
			}
			if got, err := repo.AFKAwayExistsUser(ctx, u); err != nil || !got {
				t.Fatalf("AFKAwayExistsUser(u) = %v, %v; want true", got, err)
			}
			if got, err := repo.AFKAwayExistsUser(ctx, v); err != nil || got {
				t.Fatalf("AFKAwayExistsUser(v) = %v, %v; want false", got, err)
			}
		})
	}
}

// Migration 00021_afk_away.sql exists in both dialects, so a fully migrated database reports at
// least schema version 21 (wantSchemaVersion counts the embedded files).
func TestAFKAwayMigrationIsSchemaVersion21(t *testing.T) {
	if _, err := sqlitemigrations.FS.Open("00021_afk_away.sql"); err != nil {
		t.Fatalf("sqlite 00021_afk_away.sql: %v", err)
	}
	if _, err := pgmigrations.FS.Open("00021_afk_away.sql"); err != nil {
		t.Fatalf("postgres 00021_afk_away.sql: %v", err)
	}
	if v := wantSchemaVersion(t); v < 21 {
		t.Fatalf("wantSchemaVersion = %d, want at least 21", v)
	}
	for engine, repo := range engines(t) {
		v, err := repo.SchemaVersion(context.Background())
		if err != nil || v < 21 {
			t.Fatalf("%s SchemaVersion = %d, %v; want at least 21", engine, v, err)
		}
	}
}

// The away roster survives a Postgres dump and restore (it is in DumpTables, after members).
func TestADumpRoundTripsTheAFKAwayRoster(t *testing.T) {
	dsn := os.Getenv("DILLA_TEST_PG")
	if dsn == "" {
		t.Skip("DILLA_TEST_PG is unset: Postgres tests run in CI's service container")
	}
	ctx := context.Background()
	open := func() (*sql.DB, string) {
		d := freshPostgresDSN(t, dsn)
		db, err := postgres.Open(d, 8, time.Hour)
		if err != nil {
			t.Fatalf("postgres Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		p, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
		if err != nil {
			t.Fatalf("postgres provider: %v", err)
		}
		if _, err := p.Up(ctx); err != nil {
			t.Fatalf("postgres up: %v", err)
		}
		return db, d
	}
	srcDB, srcDSN := open()
	src := postgres.New(srcDB)
	u := seedUser(ctx, t, src).ID
	cid := seedAFKCommunity(ctx, t, src, u)
	if _, err := src.SetAFKAway(ctx, cid, u, 1_700_000_123); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := store.DumpPostgres(ctx, srcDSN, &buf); err != nil {
		t.Fatalf("DumpPostgres: %v", err)
	}
	dstDB, _ := open()
	if err := store.LoadPostgres(ctx, dstDB, &buf, nil); err != nil {
		t.Fatalf("LoadPostgres: %v", err)
	}
	got, err := postgres.New(dstDB).ListAFKAway(ctx, cid)
	if err != nil || !slices.Equal(got, []store.AFKAway{{UserID: u, Since: 1_700_000_123}}) {
		t.Fatalf("restored roster = %v, %v", got, err)
	}
}

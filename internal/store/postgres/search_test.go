package postgres

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres/migrations"
)

// newPostgresRepo opens and migrates a database of the test's own on the CI server, or
// skips. The database is created for the test and dropped at cleanup, so the plan the test
// reads depends on its own rows and indexes only, never on what other packages' Postgres
// legs left in the shared database.
func newPostgresRepo(t *testing.T) (store.Repository, *sql.DB) {
	t.Helper()
	base := os.Getenv("DILLA_TEST_PG")
	if base == "" {
		t.Skip("DILLA_TEST_PG is unset: no local Postgres server on this box; CI's postgres service container runs this test")
	}
	admin, err := Open(base, 1, time.Hour)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	name := "dilla_t_" + id.New().String()[:16]
	if _, err := admin.ExecContext(context.Background(), `CREATE DATABASE `+name); err != nil {
		_ = admin.Close()
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("DILLA_TEST_PG is not a URL: %v", err)
	}
	u.Path = "/" + name
	db, err := Open(u.String(), 4, time.Hour)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	repo := New(db)
	t.Cleanup(func() { _ = repo.Close() })
	return repo, db
}

// seedSearchCorpus writes a handful of messages into one readable channel. The
// plan assertion needs rows to exist, not a particular corpus; the shared
// cross-engine corpus lives in internal/store's search_test.go.
func seedSearchCorpus(t *testing.T, repo store.Repository) []id.ID {
	t.Helper()
	ctx := context.Background()
	owner := id.New()
	if err := repo.CreateUser(ctx, store.UserRow{
		ID: owner, Username: "plan" + owner.String()[:8], Display: "Plan", UMKPub: make([]byte, 32),
		SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: 1,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	cid := id.New()
	if err := repo.CreateCommunity(ctx, store.CommunityRow{
		ID: cid, Owner: owner, Name: "c", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: 1,
	}); err != nil {
		t.Fatalf("CreateCommunity: %v", err)
	}
	ch := store.ChannelRow{
		ID: id.New(), CommunityID: &cid, Kind: 0, Mode: 1, Visibility: 2, Name: "readable",
		SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: 1,
	}
	if err := repo.CreateChannel(ctx, ch); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	for _, body := range []string{"hej vad händer med krypteringen", "kryptering", "the server never sees plaintext"} {
		seq, err := repo.NextChannelSeq(ctx, ch.ID)
		if err != nil {
			t.Fatalf("NextChannelSeq: %v", err)
		}
		if _, err := repo.PutReadableMessage(ctx, store.ReadableMessageRow{
			ChannelID: ch.ID, ChannelHex: ch.ID.String(), Seq: seq, Sender: owner, Envelope: []byte{0x80},
			Body: body, FrankingTag: make([]byte, 32), FrankingKeyID: id.New(), Created: 1,
		}); err != nil {
			t.Fatalf("PutReadableMessage: %v", err)
		}
	}
	return []id.ID{ch.ID}
}

// The statement the repository runs must reach the composite GIN index, and the
// bound configuration must keep the tsquery a scan constant rather than a
// row-derived Function Scan (gap-69 §3.5). A handful of rows would plan as a
// sequential scan whatever the indexes, and with sequential scans off the planner
// still prefers the btrees that lead with channel_id (readable_messages_by_channel,
// readable_messages_by_sender and the (channel_id, seq) unique constraint), which
// a larger corpus plus ANALYZE does not change. So inside one transaction that is
// rolled back, sequential scans are turned off and those competitors are dropped:
// the assertion is that the GIN index is USABLE by this exact statement.
func TestSearchUsesTheCompositeGINIndex(t *testing.T) {
	repo, db := newPostgresRepo(t)
	chans := seedSearchCorpus(t, repo)
	q, err := store.ParseQuery("kryptering")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var unique string
	if err := tx.QueryRowContext(ctx, `SELECT conname FROM pg_constraint
		WHERE conrelid = 'readable_messages'::regclass AND contype = 'u'`).Scan(&unique); err != nil {
		t.Fatalf("look up the (channel_id, seq) unique constraint: %v", err)
	}
	for _, stmt := range []string{
		`SET LOCAL enable_seqscan = off`,
		`DROP INDEX readable_messages_by_channel`,
		`DROP INDEX readable_messages_by_sender`,
		`ALTER TABLE readable_messages DROP CONSTRAINT ` + pgx.Identifier{unique}.Sanitize(),
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	rows, err := tx.QueryContext(ctx, `EXPLAIN (FORMAT TEXT) `+searchSQL, searchConfig, q.TSQuery(),
		byteaArray(store.ReadableSearchQuery{ChannelIDs: chans}), int64(0), int64(20))
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	// rows.Next returning false closes rows, which frees the transaction for the Rollback below.
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	// The dropped indexes come back, and the ACCESS EXCLUSIVE lock the DROPs took is
	// released before the repository's own connection reads the table below.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	plan := strings.Join(lines, "\n")
	if !strings.Contains(plan, "readable_messages_fts") {
		t.Fatalf("the plan does not use the composite GIN index:\n%s", plan)
	}
	if strings.Contains(plan, "Function Scan") {
		t.Fatalf("the tsquery became a row-derived Function Scan:\n%s", plan)
	}

	// And the statement itself answers, through the repository.
	hits, err := repo.SearchReadable(ctx, store.ReadableSearchQuery{ChannelIDs: chans, Query: q, Limit: 20})
	if err != nil || len(hits) != 1 {
		t.Fatalf("SearchReadable = %d hits, %v; want the one exact kryptering", len(hits), err)
	}
}

package sqlite

import (
	"database/sql"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "dilla-test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

func TestHasFTS5(t *testing.T) {
	db := openTestDB(t)
	ok, err := HasFTS5(db)
	if err != nil {
		t.Fatalf("HasFTS5: %v", err)
	}
	if !ok {
		opts, _ := CompileOptions(db)
		t.Fatalf("ENABLE_FTS5 is not in pragma_compile_options; modernc.org/sqlite was transpiled "+
			"without -DSQLITE_ENABLE_FTS5 for %s/%s. compile_options = %v",
			runtime.GOOS, runtime.GOARCH, opts)
	}
}

func TestCompileOptionsAreReadable(t *testing.T) {
	db := openTestDB(t)
	opts, err := CompileOptions(db)
	if err != nil {
		t.Fatalf("CompileOptions: %v", err)
	}
	if len(opts) == 0 {
		t.Fatal("pragma_compile_options returned no rows")
	}
	for _, want := range []string{"ENABLE_FTS5", "THREADSAFE=1"} {
		if !slices.Contains(opts, want) {
			t.Errorf("compile_options is missing %q: %v", want, opts)
		}
	}
}

// The real check: an fts5 virtual table, an insert, a MATCH and highlight().
func TestFTS5VirtualTableMatchesAndHighlights(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.ExecContext(t.Context(),
		`CREATE VIRTUAL TABLE msgs USING fts5(body, channel_id UNINDEXED, tokenize='unicode61')`,
	); err != nil {
		t.Fatalf("CREATE VIRTUAL TABLE ... USING fts5: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO msgs (body, channel_id) VALUES (?, ?), (?, ?)`,
		"hello encrypted world", "c1",
		"nothing to see here", "c2",
	); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	var body, highlighted, channel string
	row := db.QueryRowContext(t.Context(),
		`SELECT body, highlight(msgs, 0, '[', ']'), channel_id FROM msgs WHERE msgs MATCH ?`,
		"encrypted")
	if err := row.Scan(&body, &highlighted, &channel); err != nil {
		t.Fatalf("MATCH query: %v", err)
	}
	if body != "hello encrypted world" {
		t.Errorf("body = %q", body)
	}
	if highlighted != "hello [encrypted] world" {
		t.Errorf("highlight = %q, want %q", highlighted, "hello [encrypted] world")
	}
	if channel != "c1" {
		t.Errorf("channel_id = %q, want c1", channel)
	}

	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM msgs WHERE msgs MATCH ?`, "nothing").Scan(&n); err != nil {
		t.Fatalf("second MATCH: %v", err)
	}
	if n != 1 {
		t.Errorf("MATCH 'nothing' returned %d rows, want 1", n)
	}
}

// VACUUM INTO is the pre-migration backup the spec's "Upgrades" section wants.
func TestVacuumIntoWorks(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE t (a TEXT)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if _, err := db.ExecContext(t.Context(), `VACUUM INTO ?`, backup); err != nil {
		t.Fatalf("VACUUM INTO: %v", err)
	}
}

// The silent bug this whole design exists to avoid: plain VACUUM renumbers
// implicit rowids, the external-content index keeps pointing at the old one,
// and integrity-check reports success. The explicit INTEGER PRIMARY KEY is what
// prevents it; this test proves the prevention, not the bug.
func TestFTSIndexSurvivesVacuum(t *testing.T) {
	db := openMigratedDB(t)
	seedThreeReadableMessages(t, db)
	if _, err := db.ExecContext(t.Context(), `DELETE FROM readable_messages WHERE seq = 2`); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `VACUUM`); err != nil {
		t.Fatalf("VACUUM: %v", err)
	}
	var body string
	err := db.QueryRowContext(t.Context(), `
SELECT m.body FROM readable_messages_fts
JOIN readable_messages m ON m.id = readable_messages_fts.rowid
WHERE readable_messages_fts MATCH 'body : ("raids")'`).Scan(&body)
	if err != nil {
		t.Fatalf("the FTS index lost its join after VACUUM: %v", err)
	}
	if !strings.Contains(body, "raids") {
		t.Fatalf("joined body = %q", body)
	}
	// integrity-check compares the external-content index against the content
	// table and fails the statement on any mismatch.
	if _, err := db.ExecContext(t.Context(), `INSERT INTO readable_messages_fts(readable_messages_fts) VALUES('integrity-check')`); err != nil {
		t.Fatalf("integrity-check: %v", err)
	}
}

// An edit and a delete move the index through the _au trigger, and the index
// stays consistent with the content table afterwards.
func TestTheTriggersKeepTheIndexConsistent(t *testing.T) {
	db := openMigratedDB(t)
	seedThreeReadableMessages(t, db)
	count := func(match string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(t.Context(),
			`SELECT count(*) FROM readable_messages_fts WHERE readable_messages_fts MATCH ?`, match).Scan(&n); err != nil {
			t.Fatalf("MATCH %s: %v", match, err)
		}
		return n
	}
	if n := count(`body : ("raids")`); n != 1 {
		t.Fatalf("raids = %d before the edit, want 1", n)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE readable_messages SET body = 'nothing here' WHERE seq = 3`); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if n := count(`body : ("raids")`); n != 0 {
		t.Fatalf("raids = %d after the edit, want 0", n)
	}
	if n := count(`body : ("nothing")`); n != 1 {
		t.Fatalf("the edited body is not indexed: %d", n)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM readable_messages WHERE seq = 3`); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if n := count(`body : ("nothing")`); n != 0 {
		t.Fatalf("a deleted row is still indexed: %d", n)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO readable_messages_fts(readable_messages_fts) VALUES('integrity-check')`); err != nil {
		t.Fatalf("integrity-check: %v", err)
	}
}

// openMigratedDB is a write pool over a fresh, fully migrated database file.
func openMigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenWrite(filepath.Join(t.TempDir(), "migrated.db"))
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.FS)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	if _, err := p.Up(t.Context()); err != nil {
		t.Fatalf("goose up: %v", err)
	}
	return db
}

// seedThreeReadableMessages writes seqs 1-3 into one readable channel through
// the repository, so each row passes the real constraints and triggers. The word
// "raids" is only in seq 3, the row whose rowid a renumbering VACUUM would move
// once seq 2 is gone.
func seedThreeReadableMessages(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := t.Context()
	repo := New(db, db)
	owner := id.New()
	if err := repo.CreateUser(ctx, store.UserRow{
		ID: owner, Username: "fts" + owner.String()[:8], Display: "FTS", UMKPub: make([]byte, 32),
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
	for i, body := range []string{"hej vad händer", "the server never sees plaintext", "join raids are a metadata problem"} {
		if _, err := repo.PutReadableMessage(ctx, store.ReadableMessageRow{
			ChannelID: ch.ID, ChannelHex: ch.ID.String(), Seq: uint64(i + 1), Sender: owner,
			Envelope: []byte{0x80}, Body: body, FrankingTag: make([]byte, 32), FrankingKeyID: id.New(),
			Created: int64(i + 1),
		}); err != nil {
			t.Fatalf("PutReadableMessage: %v", err)
		}
	}
}

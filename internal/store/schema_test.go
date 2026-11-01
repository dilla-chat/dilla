package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
	pgmigrations "github.com/jonasthim/dilla/internal/store/postgres/migrations"
	"github.com/jonasthim/dilla/internal/store/postgres/pgdb"
	sqlitemigrations "github.com/jonasthim/dilla/internal/store/sqlite/migrations"
	"github.com/jonasthim/dilla/internal/store/sqlite/sqlitedb"
)

// methodShapes returns one line per method of an interface type: its name, its
// arity and the KIND of every parameter and result, with struct parameters
// spelled out field by field. Names alone would pass while one engine returned
// :many where the other returned :one, or took one parameter more — which is
// precisely the drift this test exists to catch, and which would otherwise only
// surface when postgres/repo.go is written in task 3. The two param structs
// live in different packages, so their identity can never be equal; their field
// names and kinds can.
func methodShapes(t reflect.Type) []string {
	out := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		m := t.Method(i)
		parts := []string{m.Name}
		for j := 0; j < m.Type.NumIn(); j++ {
			parts = append(parts, "in:"+shape(m.Type.In(j)))
		}
		for j := 0; j < m.Type.NumOut(); j++ {
			parts = append(parts, "out:"+shape(m.Type.Out(j)))
		}
		out = append(out, strings.Join(parts, " "))
	}
	sort.Strings(out)
	return out
}

func shape(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Slice, reflect.Pointer:
		return t.Kind().String() + "<" + shape(t.Elem()) + ">"
	case reflect.Struct:
		fields := make([]string, 0, t.NumField())
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			fields = append(fields, f.Name+":"+shape(f.Type))
		}
		return "struct{" + strings.Join(fields, ",") + "}"
	default:
		return t.Kind().String()
	}
}

func TestQuerierInterfacesAreIdentical(t *testing.T) {
	a := methodShapes(reflect.TypeOf((*sqlitedb.Querier)(nil)).Elem())
	b := methodShapes(reflect.TypeOf((*pgdb.Querier)(nil)).Elem())
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("Querier method shapes diverge:\nsqlite: %v\npg:     %v", a, b)
	}
	if len(a) == 0 {
		t.Fatal("no generated queries at all")
	}
}

// The shapes above can only be identical if the two query sets declare the same
// names with the same annotations, so the header lines are compared directly as
// well: a `:one` that should be `:many` is a one-word diff here and a confusing
// type error three tasks later.
func TestQueryHeadersAreByteIdenticalAcrossEngines(t *testing.T) {
	headers := func(dir string) []string {
		entries, err := os.ReadDir(filepath.Join(dir, "queries"))
		if err != nil {
			t.Fatalf("read %s/queries: %v", dir, err)
		}
		var out []string
		for _, e := range entries {
			body, err := os.ReadFile(filepath.Join(dir, "queries", e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			for _, line := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(line, "-- name:") {
					out = append(out, strings.TrimSpace(line))
				}
			}
		}
		sort.Strings(out)
		return out
	}
	a, b := headers("sqlite"), headers("postgres")
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("query headers diverge:\nsqlite: %v\npg:     %v", a, b)
	}
}

// wantSchemaVersion is the goose version a fully migrated database reports: the highest version
// among the embedded migration files, derived rather than written down so that the task which adds
// a migration does not also have to edit two unrelated assertions. It is the highest number, not the
// file count, because goose does not need the numbers to be contiguous: a branch that takes 00022
// while 00021 is still unreleased reports 22 with 21 files.
func wantSchemaVersion(t *testing.T) int64 {
	t.Helper()
	entries, err := sqlitemigrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	var highest int64
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !strings.HasSuffix(e.Name(), ".sql") || !ok {
			continue
		}
		v, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			t.Fatalf("migration %s does not start with a number: %v", e.Name(), err)
		}
		highest = max(highest, v)
	}
	if highest == 0 {
		t.Fatal("no embedded migrations at all")
	}
	return highest
}

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "schema.db")+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestGooseUpDownUpOnSQLite(t *testing.T) {
	db := openSQLite(t)
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sqlitemigrations.FS)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	ctx := context.Background()
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := p.Down(ctx); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}
	current, target, err := p.GetVersions(ctx)
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if current != target {
		t.Fatalf("current %d != target %d after up", current, target)
	}
}

func TestGooseUpDownUpOnPostgres(t *testing.T) {
	dsn := os.Getenv("DILLA_TEST_PG")
	if dsn == "" {
		t.Skip("DILLA_TEST_PG is unset: no local Postgres server on this box; CI's postgres service container runs this test")
	}
	// Down drops every table, so this runs in a database of its own, never the one
	// the other Postgres legs might be using.
	db, err := sql.Open("pgx", freshPostgresDSN(t, dsn))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	ctx := context.Background()
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := p.Down(ctx); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}
}

func TestEverySQLiteTableIsStrictAndTyped(t *testing.T) {
	db := openSQLite(t)
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sqlitemigrations.FS)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatalf("up: %v", err)
	}
	rows, err := db.QueryContext(t.Context(), `SELECT name, sql FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("query schema: %v", err)
	}
	defer rows.Close()
	seen := 0
	names := make([]string, 0, 17)
	var fts []string
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "goose_db_version" {
			// goose's own table cannot be STRICT: its tstamp column is TIMESTAMP,
			// which STRICT does not permit (gap-67 §2.3).
			continue
		}
		// The FTS5 index of 007_readable.sql (Plan 2 task 8) is a virtual table,
		// and FTS5 creates its own untyped shadow tables beside it (_data, _idx,
		// _docsize, _config); none of them can be STRICT and none is dilla's to
		// type. They are collected and asserted separately below.
		if name == "readable_messages_fts" || strings.HasPrefix(name, "readable_messages_fts_") {
			fts = append(fts, name)
			continue
		}
		seen++
		names = append(names, name)
		if !strings.Contains(ddl, ") STRICT") {
			t.Errorf("table %s is not STRICT: %s", name, ddl)
		}
		for _, banned := range []string{"DATETIME", "TIMESTAMP", "BOOLEAN"} {
			if strings.Contains(strings.ToUpper(ddl), banned) {
				t.Errorf("table %s declares %s: %s", name, banned, ddl)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema: %v", err)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, wantTables) {
		t.Fatalf("tables = %v, want the set 001/002/003/004/005/006/009 declare: %v", names, wantTables)
	}
	if seen != len(wantTables) {
		t.Fatalf("%d dilla tables found; 001/002/003/004/005/006/009 declare %d", seen, len(wantTables))
	}
	sort.Strings(fts)
	wantFTS := []string{"readable_messages_fts", "readable_messages_fts_config", "readable_messages_fts_data",
		"readable_messages_fts_docsize", "readable_messages_fts_idx"}
	if !reflect.DeepEqual(fts, wantFTS) {
		t.Fatalf("FTS5 tables = %v, want the external-content index and its four shadow tables %v", fts, wantFTS)
	}
}

// wantTables is the exact set of tables declared by the current SQLite schema, sorted, so that a
// dropped or renamed table is caught and not just a change in the count. Task 19
// added 004_mls.sql's twelve; task 23 added 005_messages.sql's one; Plan 2 task 1
// added 006_structure.sql's four (communities, members, roles, member_roles); Plan 2
// task 2 added 006a_channels.sql's channels; Plan 2 task 3 added 006b_overwrites.sql's
// channel_overwrites; Plan 2 task 4 added 006c_bans.sql's bans; Plan 2 task 6 added
// 006d_channel_members.sql's channel_members; Plan 2 task 7 added 006e_pending_joins.sql's
// pending_joins (Plan 1 follow-up card 8); Plan 2 task 8 added 007_readable.sql's readable_messages
// and read_state (its FTS5 index is asserted on its own, because a virtual table is not STRICT);
// Plan 2 task 10 added 008_blobs.sql's blobs, blob_refs, blob_tombstones and backups; Plan 2
// task 16 added 006f_voice.sql's voice_sessions; dilla-web-3.1 added 014_invite_signups.sql's
// invite_signups; dilla web3.7 SB1 added 016_message_class.sql's slowmode_marks; Task4B adds the
// runtime, installation, claim and session-binding tables, task 5B4B adds publication receipts,
// and Task5 adds managed group-registration receipts.
var wantTables = []string{
	"audit_log", "automod_stepup_grants", "backups", "bans", "blob_refs", "blob_tombstones", "blobs", "bot_claims", "bot_event_acks", "bot_event_streams", "bot_group_registration_admission_receipts", "bot_group_registration_receipts", "bot_installations", "bot_interaction_admission_receipts", "bot_interaction_eligibility_revisions", "bot_join_role_grant_receipts", "bot_join_role_grant_sources", "bot_join_role_policy", "bot_keypackage_publication_receipts", "bot_operations", "bot_runtimes", "bot_send_attempts", "bot_send_operations", "bot_session_bindings", "bot_structural_events", "channel_members", "channel_overwrites", "channels", "communities", "community_afk", "community_afk_away", "community_member_episodes", "community_member_timeouts", "device_cursors", "device_lists", "devices", "fork_reports",
	"instance_settings", "instances", "invite_signups", "invites", "key_packages", "login_attempts",
	"manual_warning_actions", "member_role_grant_sources", "member_roles", "members",
	"mls_app_messages", "mls_epoch_trees", "mls_groups", "mls_handshakes", "mls_members",
	"mls_pending_proposals", "mls_welcome_payloads", "mls_welcomes", "moderation_removal_heads", "moderation_removals", "oidc_identities",
	"password_credentials", "pending_joins", "reaction_role_grant_sources", "read_state", "readable_messages", "recovery_codes", "reports", "role_panel_options", "role_panels", "roles", "self_role_policy", "sessions", "slowmode_marks", "totp_secrets", "user_presence", "users",
	"voice_sessions", "webauthn_ceremonies", "webauthn_credentials", "webauthn_users",
}

// DumpTables must name every schema table exactly once. LoadPostgres truncates only the
// tables it lists, so a table with a foreign key into a listed one that is itself missing
// makes every Postgres restore fail ("cannot truncate a table referenced in a foreign key
// constraint"), and the Postgres dump silently leaves its rows out (C5: voice_sessions).
func TestDumpTablesNamesEverySchemaTable(t *testing.T) {
	got := slices.Clone(store.DumpTables)
	sort.Strings(got)
	if !slices.Equal(got, wantTables) {
		t.Fatalf("sorted DumpTables = %v\nwant the schema's tables %v", got, wantTables)
	}
}

// The two AUTOINCREMENT surrogate keys must survive sqlc's `*.id` wildcard as
// int64 (deviation ID7): if a future override re-types them as id.ID, ListAudit
// fails at scan time and this test says so at build time instead.
func TestSurrogateKeysAreInt64InTheGeneratedModels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model any
	}{
		{"sqlite audit_log", sqlitedb.AuditLog{}},
		{"sqlite login_attempts", sqlitedb.LoginAttempts{}},
		{"pg audit_log", pgdb.AuditLog{}},
		{"pg login_attempts", pgdb.LoginAttempts{}},
	} {
		f, ok := reflect.TypeOf(tc.model).FieldByName("ID")
		if !ok {
			t.Fatalf("%s: generated model has no ID field", tc.name)
		}
		if f.Type.Kind() != reflect.Int64 {
			t.Fatalf("%s: ID is %s, want int64", tc.name, f.Type)
		}
	}
}

// The WebAuthn credential id is 16-1023 bytes, so it must survive the `*.*_id`
// wildcard as []byte (deviation ID7).
func TestCredIDIsAByteSliceInTheGeneratedModels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model any
	}{
		{"sqlite", sqlitedb.WebauthnCredentials{}},
		{"pg", pgdb.WebauthnCredentials{}},
	} {
		f, ok := reflect.TypeOf(tc.model).FieldByName("CredID")
		if !ok {
			f, ok = reflect.TypeOf(tc.model).FieldByName("CredId")
		}
		if !ok {
			t.Fatalf("%s: generated model has no cred_id field", tc.name)
		}
		if f.Type != reflect.TypeOf([]byte(nil)) {
			t.Fatalf("%s: cred_id is %s, want []byte", tc.name, f.Type)
		}
	}
}

func TestMigrationTableIsGooseDefault(t *testing.T) {
	db := openSQLite(t)
	p, _ := goose.NewProvider(goose.DialectSQLite3, db, sqlitemigrations.FS)
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatalf("up: %v", err)
	}
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='goose_db_version'`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Fatalf("goose_db_version missing")
	}
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='schema_migrations'`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 0 {
		t.Fatalf("schema_migrations exists; D3 says goose's default name is kept")
	}
}

func TestIdentifierColumnsRejectFifteenBytes(t *testing.T) {
	db := openSQLite(t)
	p, _ := goose.NewProvider(goose.DialectSQLite3, db, sqlitemigrations.FS)
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatalf("up: %v", err)
	}
	short := make([]byte, 15)
	_, err := db.ExecContext(t.Context(), `INSERT INTO instance_settings (key, value, updated) VALUES ('k', ?, 1)`, short)
	if err != nil {
		t.Fatalf("a 15-byte blob is legal in a plain BLOB column: %v", err)
	}
	_, err = db.ExecContext(t.Context(), `INSERT INTO instances (instance_id, external_sender_key_id, key_history, franking_key_id, generation, policy_version, created)
	                  VALUES (?, ?, x'00', ?, 1, 1, 1)`, short, short, short)
	if err == nil {
		t.Fatal("instances accepted a 15-byte instance_id; the length CHECK is missing")
	}
}

func TestMigrationCarriesEverySchemaFileVerbatim(t *testing.T) {
	for _, dir := range []string{"sqlite", "postgres"} {
		t.Run(dir, func(t *testing.T) {
			// Every migration file concatenated: a schema file may land in any of
			// them — 001/002/003/009 in 00001_init.sql, 004_mls.sql in
			// 00002_mls.sql (task 19 step 1, deviation B19's one sequence) — and
			// what matters is that goose and sqlc see the same DDL, not which
			// file carries it.
			migEntries, err := os.ReadDir(filepath.Join(dir, "migrations"))
			if err != nil {
				t.Fatalf("read %s/migrations: %v", dir, err)
			}
			var mig []byte
			for _, e := range migEntries {
				if !strings.HasSuffix(e.Name(), ".sql") {
					continue
				}
				body, err := os.ReadFile(filepath.Join(dir, "migrations", e.Name()))
				if err != nil {
					t.Fatalf("read %s: %v", e.Name(), err)
				}
				mig = append(mig, body...)
			}
			if len(mig) == 0 {
				t.Fatalf("%s/migrations holds no .sql file", dir)
			}
			entries, err := os.ReadDir(filepath.Join(dir, "schema"))
			if err != nil {
				t.Fatalf("read schema dir: %v", err)
			}
			for _, e := range entries {
				body, err := os.ReadFile(filepath.Join(dir, "schema", e.Name()))
				if err != nil {
					t.Fatalf("read %s: %v", e.Name(), err)
				}
				if !strings.Contains(string(mig), strings.TrimSpace(string(body))) {
					t.Fatalf("no migration in %s/migrations contains %s verbatim; sqlc and goose would see different schemas", dir, e.Name())
				}
			}
		})
	}
}

// The WebAuthn Relying-Party id is a domain name, `rp_id TEXT NOT NULL` in both
// schemas (interfaces.md §4.3), and NOT a 16-byte identifier — but its name
// matches sqlc's `*.*_id` wildcard, which would type it id.ID and make every
// WebAuthn write fail with `cannot store BLOB value in TEXT column` and every
// read fail with `id: not 16 bytes`. gap-65 §4.1 warned that the columns that
// are 16-byte ids and the columns that merely end in `_id` are different sets;
// this pins the enumeration for the two tables that hold the odd one out.
func TestRpIDIsAStringInTheGeneratedModels(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model any
	}{
		{"sqlite webauthn_users", sqlitedb.WebauthnUsers{}},
		{"sqlite webauthn_credentials", sqlitedb.WebauthnCredentials{}},
		{"pg webauthn_users", pgdb.WebauthnUsers{}},
		{"pg webauthn_credentials", pgdb.WebauthnCredentials{}},
	} {
		f, ok := reflect.TypeOf(tc.model).FieldByName("RpID")
		if !ok {
			f, ok = reflect.TypeOf(tc.model).FieldByName("RpId")
		}
		if !ok {
			t.Fatalf("%s: generated model has no rp_id field", tc.name)
		}
		if f.Type.Kind() != reflect.String {
			t.Errorf("%s: rp_id is %s, want string", tc.name, f.Type)
		}
	}
}

// Every other test in this file inspects the generated code with reflect, reads
// the query files as text, or talks to the database through hand-written SQL —
// so an override that types a column wrongly passes them all, in both engines
// at once, and only fails when a real row is written (the rp_id mistype did
// exactly that). This test drives the generated Querier end to end against a
// migrated SQLite database instead: it writes and reads back the accounts and
// WebAuthn paths, which between them cover an id.ID primary key, an id.ID
// foreign key, a TEXT `*_id` column, a variable-length cred_id and a 64-byte
// handle. A future override mistake fails here, locally, on `go test`.
func TestGeneratedQueriesRoundTripOnSQLite(t *testing.T) {
	db := openSQLite(t)
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sqlitemigrations.FS)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	ctx := context.Background()
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	q := sqlitedb.New(db)

	uid := id.New()
	const rpID = "dilla.example"
	handle := bytes.Repeat([]byte{0xa7}, 64)
	credID := bytes.Repeat([]byte{0x5c}, 200) // WebAuthn credential ids run 16-1023 bytes.

	if err := q.CreateUser(ctx, sqlitedb.CreateUserParams{
		ID:        uid,
		Username:  "jonas",
		Display:   "Jonas",
		Kind:      0,
		UmkPub:    bytes.Repeat([]byte{1}, 32),
		SskPub:    bytes.Repeat([]byte{2}, 32),
		SigUmkSsk: bytes.Repeat([]byte{3}, 64),
		Created:   1000,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	user, err := q.GetUser(ctx, sqlitedb.GetUserParams{ID: uid})
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if user.ID != uid || user.Username != "jonas" {
		t.Fatalf("GetUser = %v/%q, want %v/%q", user.ID, user.Username, uid, "jonas")
	}

	if err := q.PutWebauthnUser(ctx, sqlitedb.PutWebauthnUserParams{
		RpID:       rpID,
		UserID:     uid,
		UserHandle: handle,
		Created:    1001,
	}); err != nil {
		t.Fatalf("PutWebauthnUser: %v", err)
	}
	gotHandle, err := q.GetWebauthnUserHandle(ctx, sqlitedb.GetWebauthnUserHandleParams{RpID: rpID, UserID: uid})
	if err != nil {
		t.Fatalf("GetWebauthnUserHandle: %v", err)
	}
	if !bytes.Equal(gotHandle, handle) {
		t.Fatalf("GetWebauthnUserHandle = %x, want %x", gotHandle, handle)
	}
	gotUser, err := q.GetWebauthnUserByHandle(ctx, sqlitedb.GetWebauthnUserByHandleParams{RpID: rpID, UserHandle: handle})
	if err != nil {
		t.Fatalf("GetWebauthnUserByHandle: %v", err)
	}
	if gotUser != uid {
		t.Fatalf("GetWebauthnUserByHandle = %v, want %v", gotUser, uid)
	}

	if err := q.PutWebauthnCredential(ctx, sqlitedb.PutWebauthnCredentialParams{
		CredID:         credID,
		RpID:           rpID,
		UserID:         uid,
		PublicKey:      bytes.Repeat([]byte{4}, 77),
		SignCount:      0,
		Flags:          []byte{0x01},
		ExtensionsJson: "{}",
		Name:           "yubikey",
		Created:        1002,
	}); err != nil {
		t.Fatalf("PutWebauthnCredential: %v", err)
	}
	creds, err := q.ListWebauthnCredentials(ctx, sqlitedb.ListWebauthnCredentialsParams{RpID: rpID, UserID: uid})
	if err != nil {
		t.Fatalf("ListWebauthnCredentials: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("ListWebauthnCredentials = %d rows, want 1", len(creds))
	}
	if !bytes.Equal(creds[0].CredID, credID) {
		t.Fatalf("cred_id = %x, want %x", creds[0].CredID, credID)
	}
	if creds[0].RpID != rpID {
		t.Fatalf("rp_id = %q, want %q", creds[0].RpID, rpID)
	}
	if creds[0].UserID != uid {
		t.Fatalf("user_id = %v, want %v", creds[0].UserID, uid)
	}

	// The empty case must come back as an empty slice, not nil: emit_empty_slices
	// is on so repo.go can hand it straight to a CBOR encoder.
	none, err := q.ListWebauthnCredentials(ctx, sqlitedb.ListWebauthnCredentialsParams{RpID: rpID, UserID: id.New()})
	if err != nil {
		t.Fatalf("ListWebauthnCredentials (empty): %v", err)
	}
	if none == nil || len(none) != 0 {
		t.Fatalf("ListWebauthnCredentials (empty) = %#v, want an empty non-nil slice", none)
	}
}

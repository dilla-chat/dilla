package store_test

// Task S1 fix round 1: migration 00051 swaps sessions.wire_version for a column whose CHECK admits
// wire 4. A wire-3 session recorded before the migration keeps its version; afterwards 4 is
// accepted and 5 is not.

import (
	"bytes"
	"context"
	"testing"

	"github.com/pressly/goose/v3"

	sqlitemigrations "github.com/jonasthim/dilla/internal/store/sqlite/migrations"
)

func TestAWire3SessionSurvivesTheWire4Migration(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sqlitemigrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 50); err != nil {
		t.Fatalf("up to the migration before wire 4: %v", err)
	}
	user, device, token := bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16), bytes.Repeat([]byte{3}, 32)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, username, display, kind, umk_pub, ssk_pub, sig_umk_ssk, created) VALUES (?, 'u', 'u', 0, ?, ?, ?, 1)`,
			[]any{user, make([]byte, 32), make([]byte, 32), make([]byte, 64)}},
		{`INSERT INTO devices (id, user_id, dsk_pub, tier, signer_tier, credential_blob, last_seen, created) VALUES (?, ?, ?, 1, 1, x'01', 1, 1)`,
			[]any{device, user, make([]byte, 32)}},
		{`INSERT INTO sessions (token_hash, device_id, user_id, scope, tier, created, expires, idle_expires) VALUES (?, ?, ?, 0, 1, 1, 9, 9)`,
			[]any{token, device, user}},
		{`UPDATE sessions SET wire_version = 3, e2ee_version = 1, media_version = 1 WHERE token_hash = ?`, []any{token}},
	} {
		if _, err := db.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("%s: %v", stmt.sql, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET wire_version = 4 WHERE token_hash = ?`, token); err == nil {
		t.Fatal("wire 4 was accepted before the migration")
	}
	if _, err := p.UpTo(ctx, 51); err != nil {
		t.Fatalf("up: %v", err)
	}
	var wire, e2ee int
	if err := db.QueryRowContext(ctx, `SELECT wire_version, e2ee_version FROM sessions WHERE token_hash = ?`, token).Scan(&wire, &e2ee); err != nil {
		t.Fatal(err)
	}
	if wire != 3 || e2ee != 1 {
		t.Fatalf("after the migration: wire %d e2ee %d, want 3 and 1", wire, e2ee)
	}
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET wire_version = 4 WHERE token_hash = ?`, token); err != nil {
		t.Fatalf("wire 4 refused after the migration: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET wire_version = 5 WHERE token_hash = ?`, token); err == nil {
		t.Fatal("wire 5 was accepted")
	}
}

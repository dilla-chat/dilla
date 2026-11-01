package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// VacuumInto writes a consistent copy of an open SQLite database to path. It is
// the pre-migration backup of §4.6: VACUUM INTO takes a read lock rather than
// stopping the writer, and the copy is a plain database file, so `dillad
// restore` needs no special case for it.
func VacuumInto(ctx context.Context, db *sql.DB, path string) error {
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("store: vacuum into %s: %w", path, err)
	}
	return nil
}

// DumpMagic, DumpVersion and the record header below are the container format
// `dillad restore` parses. A text marker cannot be used: a binary COPY payload
// can contain any byte sequence, including "-- table ", so a text-delimited
// stream cannot be split back into per-table segments unambiguously. Each table
// is therefore a length-prefixed record, and the reader never scans for a
// delimiter.
//
//	file   = magic(8) || version(uint32 BE) || record*
//	record = nameLen(uint32 BE) || name || payloadLen(uint64 BE) || payload
const (
	DumpMagic   = "DILLADMP"
	DumpVersion = uint32(1)
)

// DumpTables is the replay order: parents before children. It grows with the
// schema — task 19 appends the mls_* tables, task 23 mls_app_messages, Plan 2
// the structure, readable and blob tables — and a restore replays it front to
// back.
var DumpTables = []string{
	// Restored before its source rows so their revision triggers run during replay.
	// This can only advance the fence and conservatively invalidates old tickets.
	"bot_interaction_eligibility_revisions",
	"instances", "instance_settings", "users", "user_presence", "devices", "device_lists", "sessions",
	"password_credentials", "totp_secrets", "recovery_codes", "webauthn_users",
	"webauthn_credentials", "webauthn_ceremonies", "oidc_identities", "login_attempts",
	"invites", "reports", "audit_log",
	// 004_mls.sql, appended by task 19, parents before children: a group before
	// its handshakes, proposals and leaves, and a Welcome payload before the
	// per-device rows that reference it.
	"mls_groups", "mls_handshakes", "mls_pending_proposals", "mls_members",
	"mls_welcome_payloads", "mls_epoch_trees", "mls_welcomes", "key_packages",
	"device_cursors", "fork_reports",
	// 005_messages.sql, appended by task 23. Its only parent is mls_groups, so
	// it replays after the whole 004 block; a dump that omitted it would restore
	// an instance with every group intact and no message in any of them.
	"mls_app_messages",
	// 006_structure.sql, appended by Plan 2 task 1, parents before children: a
	// community before its members and roles, and both of those before the
	// member_roles rows that reference a (member, role) pair.
	"communities", "members", "community_member_episodes", "community_member_timeouts", "manual_warning_actions", "roles", "member_roles", "member_role_grant_sources", "self_role_policy",
	// 031_moderation_removals.sql: permanent per-group markers reference both communities and groups.
	"moderation_removal_heads", "moderation_removals",
	// 006a_channels.sql, appended by Plan 2 task 2. Its parent is communities; its
	// self-reference (parent_id) is checked at the end of the one COPY statement
	// that replays the table, so the rows' order within it does not matter.
	"channels",
	"community_afk",
	// 019_afk_away.sql, appended by afk-disconnect AW1. Its parent is members (a composite key);
	// a restore that dropped it would forget who is away after a restart (R-AW5).
	"community_afk_away",
	// 006f_voice.sql, appended by Plan 2 task 16 (fix wave C5). Its only parent is channels. A
	// dump that left it out restored no call history, and LoadPostgres could not TRUNCATE
	// channels at all while this child table was missing from the list, so every Postgres
	// restore failed.
	"voice_sessions",
	// 006b_overwrites.sql, appended by Plan 2 task 3. Its parent is channels.
	"channel_overwrites",
	// 006c_bans.sql, appended by Plan 2 task 4. Its parents are communities and
	// users; a restore that dropped it would let every banned user join again.
	"bans",
	// 006d_channel_members.sql, appended by Plan 2 task 6. Its parents are channels
	// and users; a restore that dropped it would lose every DM's participant list,
	// which is stored nowhere else.
	"channel_members",
	// 006e_pending_joins.sql, appended by Plan 2 task 7 (Plan 1 follow-up card 8). Its parent
	// is mls_groups; a dump that dropped it would stall every join storm in flight at the
	// backup, with no row left to say which devices were still waiting.
	"pending_joins",
	// 007_readable.sql, appended by Plan 2 task 8. Both tables' parents are channels (and
	// users for read_state); readable_messages is server-readable channel content, which D15
	// says a backup holds. COPY TO leaves out the generated body_tsv, and the restore
	// recomputes it.
	"readable_messages", "read_state",
	// 008_blobs.sql, appended by Plan 2 task 10. blobs before blob_refs, which names it with
	// ON DELETE RESTRICT and whose other parent is channels; blob_tombstones has no parent, and
	// backups' parent is users. A restore that dropped blob_tombstones would let a purged blob be
	// uploaded again under its old name.
	"blobs", "blob_refs", "blob_tombstones", "backups",
	// 014_invite_signups.sql, appended by dilla-web-3.1. Its parents are users and invites; a
	// restore that dropped it would make the join after a backed-up sign-up spend a second use.
	"invite_signups",
	// 016_message_class.sql, appended by dilla web3.7 SB1. Its parents are channels and users; a
	// restore that dropped it would reset every end-to-end encrypted channel's slow mode.
	"slowmode_marks",
	// 020_bot_installations.sql: runtime before installation, then claim/operation/session children.
	"bot_runtimes", "bot_installations", "automod_stepup_grants", "bot_join_role_policy", "bot_join_role_grant_receipts", "bot_claims", "bot_operations", "bot_session_bindings", "bot_interaction_admission_receipts",
	// 032_role_panels.sql: panels reference both channels and installations;
	// options reference panels and roles. Restore them only after all parents.
	"role_panels", "role_panel_options", "reaction_role_grant_sources", "bot_join_role_grant_sources",
	// 023_bot_keypackage_publications.sql: receipts are children of installations, users and devices.
	"bot_keypackage_publication_receipts",
	// 024_bot_sends.sql: logical operations before their immutable attempt children.
	"bot_send_operations", "bot_send_attempts",
	// 025_bot_group_registration_receipts.sql: immutable receipts scoped to an installation.
	"bot_event_streams", "bot_structural_events",
	"bot_group_registration_receipts",
	// Admission consumption bindings depend on the installation and accompany
	// the accepted registration in logical backups.
	"bot_group_registration_admission_receipts",
	"bot_event_acks",
}

// DumpPostgres writes a logical dump of dillad's tables to w using COPY TO.
// pg_dump is not assumed to exist on the host, which is why this is in-process.
//
// The whole dump runs inside ONE repeatable-read read-only transaction, so it is
// a snapshot: without it a write landing between two CopyTo calls yields a
// backup with a child row whose parent is missing, which D15/R38 cannot tolerate.
// Each table's payload is buffered so its length is known before the record
// header is written; the largest of these tables is bounded by the instance's
// account count, not by its message history.
func DumpPostgres(ctx context.Context, dsn string, w io.Writer) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("store: connect for dump: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return fmt.Errorf("store: begin dump snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := io.WriteString(w, DumpMagic); err != nil {
		return fmt.Errorf("store: write dump magic: %w", err)
	}
	if err := binary.Write(w, binary.BigEndian, DumpVersion); err != nil {
		return fmt.Errorf("store: write dump version: %w", err)
	}
	for _, table := range DumpTables {
		var buf bytes.Buffer
		if _, err := tx.Conn().PgConn().CopyTo(ctx, &buf, `COPY `+table+` TO STDOUT (FORMAT binary)`); err != nil {
			return fmt.Errorf("store: copy %s: %w", table, err)
		}
		if err := binary.Write(w, binary.BigEndian, uint32(len(table))); err != nil { //nolint:gosec // G115: the length of a table name from the fixed DumpTables list
			return fmt.Errorf("store: write dump header for %s: %w", table, err)
		}
		if _, err := io.WriteString(w, table); err != nil {
			return fmt.Errorf("store: write dump header for %s: %w", table, err)
		}
		if err := binary.Write(w, binary.BigEndian, uint64(buf.Len())); err != nil { //nolint:gosec // G115: a buffer length is never negative
			return fmt.Errorf("store: write dump header for %s: %w", table, err)
		}
		if _, err := buf.WriteTo(w); err != nil {
			return fmt.Errorf("store: write dump payload for %s: %w", table, err)
		}
	}
	return tx.Commit(ctx)
}

// maxDumpTableName bounds a record's table name; every name in DumpTables is
// far shorter, so a longer one is a damaged or foreign stream.
const maxDumpTableName = 64

// ReadDump walks a DumpPostgres stream record by record, calling fn with each
// table's name and a reader bounded to exactly its COPY payload. fn need not
// read the payload to its end: whatever it leaves is skipped. A table name that
// is not in DumpTables is refused before fn sees it, so a crafted stream can
// never name an identifier the restore would splice into SQL.
func ReadDump(r io.Reader, fn func(table string, payload io.Reader) error) error {
	magic := make([]byte, len(DumpMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != DumpMagic {
		return fmt.Errorf("store: not a %s stream", DumpMagic)
	}
	var version uint32
	if err := binary.Read(r, binary.BigEndian, &version); err != nil {
		return fmt.Errorf("store: dump version: %w", err)
	}
	if version != DumpVersion {
		return fmt.Errorf("store: dump version %d, this binary reads %d", version, DumpVersion)
	}
	for {
		var nameLen uint32
		if err := binary.Read(r, binary.BigEndian, &nameLen); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("store: dump record header: %w", err)
		}
		if nameLen == 0 || nameLen > maxDumpTableName {
			return fmt.Errorf("store: dump record names a table of %d bytes", nameLen)
		}
		name := make([]byte, nameLen)
		if _, err := io.ReadFull(r, name); err != nil {
			return fmt.Errorf("store: dump record header: %w", err)
		}
		table := string(name)
		if !slices.Contains(DumpTables, table) {
			return fmt.Errorf("store: dump record names %q, which is not a dilla table", table)
		}
		var size uint64
		if err := binary.Read(r, binary.BigEndian, &size); err != nil {
			return fmt.Errorf("store: dump record header for %s: %w", table, err)
		}
		if size > math.MaxInt64 {
			return fmt.Errorf("store: dump record for %s claims %d bytes", table, size)
		}
		payload := &io.LimitedReader{R: r, N: int64(size)}
		if err := fn(table, payload); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, payload); err != nil {
			return fmt.Errorf("store: dump payload for %s: %w", table, err)
		}
		if payload.N != 0 {
			return fmt.Errorf("store: dump payload for %s is short by %d bytes", table, payload.N)
		}
	}
}

// copySignature opens every PostgreSQL binary COPY stream.
var copySignature = []byte("PGCOPY\n\xff\r\n\x00")

// CountCopyRows counts the tuples in one binary COPY payload (PostgreSQL's
// "Binary Format": an 11-byte signature, a 32-bit flags field, a header
// extension, then per tuple a 16-bit field count and each field's 32-bit
// length and bytes, ended by a field count of -1). It keeps nothing, so a dry
// run can report a Postgres archive's row counts without a database.
func CountCopyRows(r io.Reader) (int64, error) {
	sig := make([]byte, len(copySignature))
	if _, err := io.ReadFull(r, sig); err != nil || !bytes.Equal(sig, copySignature) {
		return 0, errors.New("store: not a binary COPY payload")
	}
	var flags, extLen int32
	if err := binary.Read(r, binary.BigEndian, &flags); err != nil {
		return 0, fmt.Errorf("store: COPY header: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &extLen); err != nil {
		return 0, fmt.Errorf("store: COPY header: %w", err)
	}
	if extLen < 0 {
		return 0, errors.New("store: negative COPY header extension")
	}
	if _, err := io.CopyN(io.Discard, r, int64(extLen)); err != nil {
		return 0, fmt.Errorf("store: COPY header: %w", err)
	}
	var rows int64
	for {
		var fields int16
		if err := binary.Read(r, binary.BigEndian, &fields); err != nil {
			return 0, fmt.Errorf("store: COPY tuple header: %w", err)
		}
		if fields == -1 {
			return rows, nil
		}
		if fields < 0 {
			return 0, fmt.Errorf("store: COPY tuple with %d fields", fields)
		}
		for range fields {
			var n int32
			if err := binary.Read(r, binary.BigEndian, &n); err != nil {
				return 0, fmt.Errorf("store: COPY field header: %w", err)
			}
			if n > 0 {
				if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
					return 0, fmt.Errorf("store: COPY field: %w", err)
				}
			}
		}
		rows++
	}
}

// LoadPostgres replays a DumpPostgres stream into db, in ONE transaction: every
// dilla table the schema holds is emptied in one TRUNCATE, then each table the
// stream names is refilled with COPY FROM STDIN in the stream's order, which is
// DumpTables' parents-before-children order. then, when it is not nil, runs on
// the same transaction before it commits, so what it writes lands with the
// archive or not at all: `dillad restore` arms invariant 11's heal there, and a
// heal that fails leaves the database exactly as it was, never the archive at
// its old generation. Either everything lands or nothing changes.
//
// db must be a pgx (pgx/v5/stdlib) pool: the COPY runs on the transaction's own
// connection through database/sql's Conn.Raw, since database/sql has no COPY.
//
// The target must already be at the archive's schema version: a binary COPY
// carries no column names, so a table whose columns differ from the dump's
// fails its COPY, and the transaction with it. COPY FROM writes the dumped
// value into a GENERATED ALWAYS identity column (PostgreSQL's COPY
// documentation), so each identity sequence is then moved past its table's
// highest value; a generated column (readable_messages.body_tsv) is left out by
// COPY in both directions and recomputed as the row lands.
func LoadPostgres(ctx context.Context, db *sql.DB, r io.Reader, then func(tx *sql.Tx) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: connect for load: %w", err)
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin load: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	present := make([]string, 0, len(DumpTables))
	for _, table := range DumpTables {
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok); err != nil {
			return fmt.Errorf("store: look up %s: %w", table, err)
		}
		if ok {
			present = append(present, pgx.Identifier{table}.Sanitize())
		}
	}
	if len(present) > 0 {
		if _, err := tx.ExecContext(ctx, `TRUNCATE `+strings.Join(present, ", ")); err != nil {
			return fmt.Errorf("store: empty the tables: %w", err)
		}
	}
	err = ReadDump(r, func(table string, payload io.Reader) error {
		q := `COPY ` + pgx.Identifier{table}.Sanitize() + ` FROM STDIN (FORMAT binary)`
		// tx was begun on this connection, so the COPY runs inside it.
		return conn.Raw(func(driverConn any) error {
			pc, ok := driverConn.(interface{ Conn() *pgx.Conn })
			if !ok {
				return fmt.Errorf("store: copy %s: the connection is a %T, not pgx's", table, driverConn)
			}
			if _, err := pc.Conn().PgConn().CopyFrom(ctx, payload, q); err != nil {
				return fmt.Errorf("store: copy %s: %w", table, err)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	if err := resetIdentities(ctx, tx); err != nil {
		return err
	}
	if then != nil {
		if err := then(tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit the load: %w", err)
	}
	return nil
}

// resetIdentities moves every dilla table's identity sequence past the highest
// value COPY wrote, so the next INSERT does not collide with a restored row.
func resetIdentities(ctx context.Context, tx *sql.Tx) error {
	ids, err := identityColumns(ctx, tx)
	if err != nil {
		return fmt.Errorf("store: list identity columns: %w", err)
	}
	for _, c := range ids {
		table, column := pgx.Identifier{c.table}.Sanitize(), pgx.Identifier{c.column}.Sanitize()
		// Both names are DumpTables' own, read back from information_schema and quoted.
		q := `SELECT setval(pg_get_serial_sequence($1, $2), COALESCE((SELECT MAX(` + column + `) FROM ` + table + `), 0) + 1, false)` //nolint:gosec // G202: quoted identifiers of dilla's own tables, never input
		if _, err := tx.ExecContext(ctx, q, c.table, c.column); err != nil {
			return fmt.Errorf("store: move the %s.%s sequence: %w", c.table, c.column, err)
		}
	}
	return nil
}

type identityColumn struct{ table, column string }

// identityColumns lists the identity columns of the DumpTables tables.
func identityColumns(ctx context.Context, tx *sql.Tx) ([]identityColumn, error) {
	rows, err := tx.QueryContext(ctx, `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND is_identity = 'YES' ORDER BY table_name, column_name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []identityColumn
	for rows.Next() {
		var c identityColumn
		if err := rows.Scan(&c.table, &c.column); err != nil {
			return nil, err
		}
		if slices.Contains(DumpTables, c.table) {
			ids = append(ids, c)
		}
	}
	return ids, rows.Err()
}

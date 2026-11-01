package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/sqlite"
)

// Every admission compacts its installation's receipts inside the writer transaction. The Bot asks
// for an admission on every private-group sync and every private reply, and the tombstones stay
// for 30 days, so an installation holds hundreds of thousands of rows. Searched by installation
// alone, both compaction statements visit all of them while every other write on the instance
// waits on the one SQLite writer (CT 144, 2026-10-30: about 6 s per admission, cursor and upload
// POSTs queued behind it). Both statements must be range searches on the expiry.
func TestBotInteractionAdmissionCompactionSearchesByExpiry(t *testing.T) {
	ctx := context.Background()
	repo := engines(t)["sqlite"]
	db := rawWriteDB(t, repo)
	// The two statements sqlite.(*Repo).CompactExpiredBotInteractionAdmissionReceipts runs.
	// The payload compaction must use the partial index of receipts that still hold a payload: a
	// range over every expired receipt would still visit the 30 days of tombstones.
	for name, c := range map[string]struct{ statement, index string }{
		"payload compaction": {sqlite.CompactAdmissionPayloadsSQL, "bot_interaction_admission_receipts_live"},
		"tombstone deletion": {sqlite.DeleteAdmissionTombstonesSQL, "bot_interaction_admission_receipts_expiry"},
	} {
		args := []any{make([]byte, 16), int64(1)}
		if name == "tombstone deletion" {
			args = []any{make([]byte, 16), make([]byte, 16), int64(1), int64(store.BotInteractionAdmissionTombstoneBatch)}
		}
		joined := strings.Join(explainPlan(ctx, t, db, name, c.statement, args...), "; ")
		if !strings.Contains(joined, "INDEX "+c.index+" (installation_id=? AND not_after<?)") {
			t.Errorf("%s plan = %q, want a range on %s (installation_id, not_after)", name, joined, c.index)
		}
	}
}

// After an upgrade, or after the Bot was offline, many tombstones can cross the 30-day line at
// once. One admission deletes at most BotInteractionAdmissionTombstoneBatch of them, oldest first,
// so the first compaction cannot stall a live single-writer database; later admissions take the
// rest. (Payload compaction needs no bound: every admission blanks the expired payloads, so live
// payloads never outnumber the admissions of the last 30 s.)
func TestBotInteractionAdmissionTombstoneDeletionIsBounded(t *testing.T) {
	ctx := context.Background()
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			f := newSendFixture(ctx, t, repo)
			const extra = 88
			total := store.BotInteractionAdmissionTombstoneBatch + extra
			oldest := int64(1_000)
			if err := repo.Tx(ctx, func(tx store.Repository) error {
				for i := range total {
					r := store.BotInteractionAdmissionReceipt{InstallationID: f.installation.ID, RequestID: id.New(), RequestHash: bytes.Repeat([]byte{0x41}, 32),
						Payload: []byte{0x81, 0x01}, Signature: bytes.Repeat([]byte{0x52}, 64), NotAfter: oldest + int64(i)}
					if err := tx.CreateBotInteractionAdmissionReceipt(ctx, r); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			// A receipt that is expired but inside the 30-day window must stay.
			now := oldest + int64(total) + 30*24*60*60
			kept := store.BotInteractionAdmissionReceipt{InstallationID: f.installation.ID, RequestID: id.New(), RequestHash: bytes.Repeat([]byte{0x42}, 32),
				Payload: []byte{0x81, 0x02}, Signature: bytes.Repeat([]byte{0x53}, 64), NotAfter: now - 60}
			if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.CreateBotInteractionAdmissionReceipt(ctx, kept) }); err != nil {
				t.Fatalf("seed kept: %v", err)
			}
			countRows := func() int {
				q := `SELECT count(*) FROM bot_interaction_admission_receipts WHERE installation_id=?`
				if engine == "postgres" {
					q = `SELECT count(*) FROM bot_interaction_admission_receipts WHERE installation_id=$1`
				}
				var n int
				if err := rawWriteDB(t, repo).QueryRowContext(ctx, q, f.installation.ID).Scan(&n); err != nil {
					t.Fatalf("count: %v", err)
				}
				return n
			}
			compact := func() {
				if err := repo.Tx(ctx, func(tx store.Repository) error {
					return tx.CompactExpiredBotInteractionAdmissionReceipts(ctx, f.installation.ID, now)
				}); err != nil {
					t.Fatalf("compact: %v", err)
				}
			}
			compact()
			if got := countRows(); got != extra+1 {
				t.Fatalf("after one compaction %d receipts remain, want %d (one batch of %d deleted)", got, extra+1, store.BotInteractionAdmissionTombstoneBatch)
			}
			// Oldest first: the batch took the 512 oldest tombstones, so the oldest left past the
			// window is the 513th (a newest-first batch would leave the very oldest behind).
			q := `SELECT min(not_after) FROM bot_interaction_admission_receipts WHERE installation_id=?`
			if engine == "postgres" {
				q = `SELECT min(not_after) FROM bot_interaction_admission_receipts WHERE installation_id=$1`
			}
			var oldestLeft int64
			if err := rawWriteDB(t, repo).QueryRowContext(ctx, q, f.installation.ID).Scan(&oldestLeft); err != nil {
				t.Fatalf("oldest left: %v", err)
			}
			if want := oldest + store.BotInteractionAdmissionTombstoneBatch; oldestLeft != want {
				t.Fatalf("oldest receipt left after one batch has not_after %d, want %d (the batch must delete oldest first)", oldestLeft, want)
			}
			compact()
			if got := countRows(); got != 1 {
				t.Fatalf("after two compactions %d receipts remain, want only the one inside 30 days", got)
			}
			got, err := repo.GetBotInteractionAdmissionReceipt(ctx, kept.InstallationID, kept.RequestID)
			if err != nil || len(got.Payload) != 0 || len(got.Signature) != 0 {
				t.Fatalf("receipt inside the window = %+v, %v; want a kept tombstone with its payload blanked", got, err)
			}
		})
	}
}

func explainPlan(ctx context.Context, t *testing.T, db *sql.DB, name, statement string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+statement, args...)
	if err != nil {
		t.Fatalf("%s: explain: %v", name, err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("%s: scan plan: %v", name, err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: plan rows: %v", name, err)
	}
	return plan
}

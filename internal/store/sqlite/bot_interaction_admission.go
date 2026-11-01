package sqlite

import (
	"context"
	"database/sql"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

func (r *Repo) GetBotInteractionAdmissionReceipt(ctx context.Context, installation, request id.ID) (store.BotInteractionAdmissionReceipt, error) {
	q := interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}(r.read)
	if r.tx != nil {
		q = r.tx
	}
	var v store.BotInteractionAdmissionReceipt
	err := q.QueryRowContext(ctx, `SELECT installation_id,request_id,request_hash,payload,signature,not_after FROM bot_interaction_admission_receipts WHERE installation_id=? AND request_id=?`, installation, request).Scan(&v.InstallationID, &v.RequestID, &v.RequestHash, &v.Payload, &v.Signature, &v.NotAfter)
	if err != nil {
		return store.BotInteractionAdmissionReceipt{}, wrap(err)
	}
	return v, nil
}

func (r *Repo) CreateBotInteractionAdmissionReceipt(ctx context.Context, v store.BotInteractionAdmissionReceipt) error {
	if r.tx == nil || v.InstallationID.IsZero() || v.RequestID.IsZero() || len(v.RequestHash) != 32 || len(v.Payload) == 0 || len(v.Payload) > 512 || len(v.Signature) != 64 || v.NotAfter <= 0 {
		return store.ErrConflict
	}
	_, err := r.tx.ExecContext(ctx, `INSERT INTO bot_interaction_admission_receipts(installation_id,request_id,request_hash,payload,signature,not_after) VALUES(?,?,?,?,?,?)`, v.InstallationID, v.RequestID, v.RequestHash, v.Payload, v.Signature, v.NotAfter)
	return wrap(err)
}

func (r *Repo) UpdateBotInteractionAdmissionReceipt(ctx context.Context, v store.BotInteractionAdmissionReceipt) error {
	if r.tx == nil || v.InstallationID.IsZero() || v.RequestID.IsZero() || len(v.RequestHash) != 32 || len(v.Payload) == 0 || len(v.Payload) > 512 || len(v.Signature) != 64 || v.NotAfter <= 0 {
		return store.ErrConflict
	}
	res, err := r.tx.ExecContext(ctx, `UPDATE bot_interaction_admission_receipts SET payload=?,signature=?,not_after=? WHERE installation_id=? AND request_id=? AND request_hash=? AND length(payload)=0`, v.Payload, v.Signature, v.NotAfter, v.InstallationID, v.RequestID, v.RequestHash)
	if err != nil {
		return wrap(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return store.ErrConflict
	}
	return nil
}

// The two compaction statements, shared with the store package's plan test so it checks the SQL
// that runs.
const (
	// CompactAdmissionPayloadsSQL blanks expired payloads; args: installation, now. The partial
	// index (migration 00053) holds only receipts that still carry a payload. Without statistics
	// the planner prefers the full expiry index, whose range is every tombstone of the last 30
	// days, so the index is named rather than left to the planner. It needs no batch bound: every
	// admission runs it, so live payloads never outnumber the admissions of the last 30 s.
	CompactAdmissionPayloadsSQL = `UPDATE bot_interaction_admission_receipts INDEXED BY bot_interaction_admission_receipts_live SET payload=x'',signature=x'' WHERE installation_id=? AND not_after<=? AND length(payload)>0`
	// DeleteAdmissionTombstonesSQL forgets request hashes 30 days past expiry, at most one batch
	// per call, oldest first; args: installation, installation, cutoff, batch.
	DeleteAdmissionTombstonesSQL = `DELETE FROM bot_interaction_admission_receipts WHERE installation_id=? AND request_id IN (SELECT request_id FROM bot_interaction_admission_receipts WHERE installation_id=? AND not_after<=? ORDER BY not_after LIMIT ?)`
)

func (r *Repo) CompactExpiredBotInteractionAdmissionReceipts(ctx context.Context, installation id.ID, now int64) error {
	if r.tx == nil {
		return sql.ErrTxDone
	}
	if _, err := r.tx.ExecContext(ctx, CompactAdmissionPayloadsSQL, installation, now); err != nil {
		return wrap(err)
	}
	// Request hashes remain as compact tombstones for 30 days after expiry, then the bounded
	// idempotency window allows that request ID to be reused. A backlog drains one batch per
	// admission, so it never holds the single writer for long.
	_, err := r.tx.ExecContext(ctx, DeleteAdmissionTombstonesSQL, installation, installation, now-30*24*60*60, store.BotInteractionAdmissionTombstoneBatch)
	return wrap(err)
}

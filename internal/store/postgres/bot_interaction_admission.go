package postgres

import (
	"context"
	"database/sql"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

func (r *Repo) GetBotInteractionAdmissionReceipt(ctx context.Context, installation, request id.ID) (store.BotInteractionAdmissionReceipt, error) {
	q := interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}(r.rawRead)
	if r.tx != nil {
		q = r.tx
	}
	var v store.BotInteractionAdmissionReceipt
	err := q.QueryRowContext(ctx, `SELECT installation_id,request_id,request_hash,payload,signature,not_after FROM bot_interaction_admission_receipts WHERE installation_id=$1 AND request_id=$2`, installation, request).Scan(&v.InstallationID, &v.RequestID, &v.RequestHash, &v.Payload, &v.Signature, &v.NotAfter)
	if err != nil {
		return store.BotInteractionAdmissionReceipt{}, wrap(err)
	}
	return v, nil
}

func (r *Repo) CreateBotInteractionAdmissionReceipt(ctx context.Context, v store.BotInteractionAdmissionReceipt) error {
	if r.tx == nil || v.InstallationID.IsZero() || v.RequestID.IsZero() || len(v.RequestHash) != 32 || len(v.Payload) == 0 || len(v.Payload) > 512 || len(v.Signature) != 64 || v.NotAfter <= 0 {
		return store.ErrConflict
	}
	_, err := r.tx.ExecContext(ctx, `INSERT INTO bot_interaction_admission_receipts(installation_id,request_id,request_hash,payload,signature,not_after) VALUES($1,$2,$3,$4,$5,$6)`, v.InstallationID, v.RequestID, v.RequestHash, v.Payload, v.Signature, v.NotAfter)
	return wrap(err)
}

func (r *Repo) UpdateBotInteractionAdmissionReceipt(ctx context.Context, v store.BotInteractionAdmissionReceipt) error {
	if r.tx == nil || v.InstallationID.IsZero() || v.RequestID.IsZero() || len(v.RequestHash) != 32 || len(v.Payload) == 0 || len(v.Payload) > 512 || len(v.Signature) != 64 || v.NotAfter <= 0 {
		return store.ErrConflict
	}
	res, err := r.tx.ExecContext(ctx, `UPDATE bot_interaction_admission_receipts SET payload=$1,signature=$2,not_after=$3 WHERE installation_id=$4 AND request_id=$5 AND request_hash=$6 AND octet_length(payload)=0`, v.Payload, v.Signature, v.NotAfter, v.InstallationID, v.RequestID, v.RequestHash)
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

func (r *Repo) CompactExpiredBotInteractionAdmissionReceipts(ctx context.Context, installation id.ID, now int64) error {
	if r.tx == nil {
		return sql.ErrTxDone
	}
	_, err := r.tx.ExecContext(ctx, `UPDATE bot_interaction_admission_receipts SET payload=''::bytea,signature=''::bytea WHERE installation_id=$1 AND not_after<=$2 AND octet_length(payload)>0`, installation, now)
	if err != nil {
		return wrap(err)
	}
	// Request hashes remain as compact tombstones for 30 days after expiry, then
	// the bounded idempotency window allows that request ID to be reused. At most one batch per
	// call, oldest first, so a backlog never holds the writer for long.
	_, err = r.tx.ExecContext(ctx, `DELETE FROM bot_interaction_admission_receipts WHERE installation_id=$1 AND request_id IN (SELECT request_id FROM bot_interaction_admission_receipts WHERE installation_id=$1 AND not_after<=$2 ORDER BY not_after LIMIT $3)`,
		installation, now-30*24*60*60, store.BotInteractionAdmissionTombstoneBatch)
	return wrap(err)
}

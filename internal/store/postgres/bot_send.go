package postgres

import (
	"context"
	"database/sql"
	"math"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func validSendOperation(v store.BotSendOperationRow) bool {
	return !v.InstallationID.IsZero() && !v.OperationID.IsZero() && !v.UserID.IsZero() && !v.DeviceID.IsZero() && !v.CommunityID.IsZero() && !v.GroupID.IsZero() && !v.CurrentAttempt.IsZero() && v.State <= uint8(store.BotSendOperationUncertain) && ((v.State == uint8(store.BotSendOperationAccepted)) == (v.AcceptedAttempt != nil)) && (v.AcceptedAttempt == nil || !v.AcceptedAttempt.IsZero())
}

func validSendAttempt(v store.BotSendAttemptRow) bool {
	return !v.InstallationID.IsZero() && !v.OperationID.IsZero() && !v.AttemptID.IsZero() && (v.PredecessorID == nil || !v.PredecessorID.IsZero()) && v.Generation <= math.MaxInt64 && v.Epoch <= math.MaxInt64 && v.Class <= 1 && len(v.CiphertextSHA256) == 32 && len(v.FingerprintSHA256) == 32 && v.Outcome <= uint8(store.BotSendAttemptUnknown) && len(v.Receipt) > 0 && len(v.Receipt) <= 512
}

func (r *Repo) CreateBotSendOperationIfAbsent(ctx context.Context, v store.BotSendOperationRow) (store.BotSendOperationRow, bool, error) {
	// A logical operation is born active and unaccepted. Terminal states are
	// reached only through SealBotSendOperation after an immutable attempt has
	// been inserted in this transaction.
	if !r.inTx || !validSendOperation(v) || v.State != uint8(store.BotSendOperationActive) || v.AcceptedAttempt != nil {
		return store.BotSendOperationRow{}, false, store.ErrConflict
	}
	res, err := r.tx.ExecContext(ctx, `INSERT INTO bot_send_operations(installation_id,operation_id,user_id,device_id,community_id,group_id,current_attempt_id,accepted_attempt_id,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(installation_id,operation_id) DO NOTHING`, v.InstallationID, v.OperationID, v.UserID, v.DeviceID, v.CommunityID, v.GroupID, v.CurrentAttempt, nullableID(v.AcceptedAttempt), int64(v.State))
	if err != nil {
		return store.BotSendOperationRow{}, false, wrap(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return store.BotSendOperationRow{}, false, err
	}
	got, err := scanBotSendOperation(r.tx.QueryRowContext(ctx, `SELECT installation_id,operation_id,user_id,device_id,community_id,group_id,current_attempt_id,accepted_attempt_id,state FROM bot_send_operations WHERE installation_id=$1 AND operation_id=$2`, v.InstallationID, v.OperationID))
	return got, n == 1, err
}

func (r *Repo) LockBotSendOperation(ctx context.Context, installation, operation id.ID) (store.BotSendOperationRow, error) {
	if !r.inTx {
		return store.BotSendOperationRow{}, sql.ErrTxDone
	}
	return scanBotSendOperation(r.tx.QueryRowContext(ctx, `SELECT installation_id,operation_id,user_id,device_id,community_id,group_id,current_attempt_id,accepted_attempt_id,state FROM bot_send_operations WHERE installation_id=$1 AND operation_id=$2 FOR UPDATE`, installation, operation))
}

func (r *Repo) GetBotSendAttempt(ctx context.Context, installation, operation, attempt id.ID) (store.BotSendAttemptRow, error) {
	q := r.db
	if r.tx != nil {
		q = nil
	}
	query := `SELECT installation_id,operation_id,attempt_id,predecessor_attempt_id,expected_generation,epoch,class,ciphertext_sha256,fingerprint_sha256,outcome,receipt FROM bot_send_attempts WHERE installation_id=$1 AND operation_id=$2 AND attempt_id=$3`
	if r.tx != nil {
		return scanBotSendAttempt(r.tx.QueryRowContext(ctx, query, installation, operation, attempt))
	}
	return scanBotSendAttempt(q.QueryRowContext(ctx, query, installation, operation, attempt))
}

func (r *Repo) InsertBotSendAttempt(ctx context.Context, v store.BotSendAttemptRow) error {
	if !r.inTx || !validSendAttempt(v) {
		return store.ErrConflict
	}
	_, err := r.tx.ExecContext(ctx, `INSERT INTO bot_send_attempts(installation_id,operation_id,attempt_id,predecessor_attempt_id,expected_generation,epoch,class,ciphertext_sha256,fingerprint_sha256,outcome,receipt) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, v.InstallationID, v.OperationID, v.AttemptID, nullableID(v.PredecessorID), int64(v.Generation), int64(v.Epoch), int64(v.Class), v.CiphertextSHA256, v.FingerprintSHA256, int64(v.Outcome), v.Receipt)
	return wrap(err)
}

func (r *Repo) AdvanceBotSendAttempt(ctx context.Context, installation, operation, predecessor, next id.ID) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	if installation.IsZero() || operation.IsZero() || predecessor.IsZero() || next.IsZero() {
		return store.ErrConflict
	}
	res, err := r.tx.ExecContext(ctx, `UPDATE bot_send_operations SET current_attempt_id=$1 WHERE installation_id=$2 AND operation_id=$3 AND current_attempt_id=$4 AND accepted_attempt_id IS NULL AND state=$5 AND EXISTS(SELECT 1 FROM bot_send_attempts WHERE installation_id=$2 AND operation_id=$3 AND attempt_id=$4 AND outcome=$6) AND EXISTS(SELECT 1 FROM bot_send_attempts WHERE installation_id=$2 AND operation_id=$3 AND attempt_id=$1 AND predecessor_attempt_id=$4)`, next, installation, operation, predecessor, int64(store.BotSendOperationActive), int64(store.BotSendAttemptClosed))
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

func (r *Repo) SealBotSendOperation(ctx context.Context, installation, operation, attempt id.ID, state store.BotSendOperationState) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	if installation.IsZero() || operation.IsZero() || attempt.IsZero() || (state != store.BotSendOperationAccepted && state != store.BotSendOperationUncertain) {
		return store.ErrConflict
	}
	var accepted any
	var outcome int64
	if state == store.BotSendOperationAccepted {
		accepted = attempt
		outcome = int64(store.BotSendAttemptAccepted)
	} else {
		outcome = int64(store.BotSendAttemptUnknown)
	}
	res, err := r.tx.ExecContext(ctx, `UPDATE bot_send_operations SET accepted_attempt_id=$1,state=$2 WHERE installation_id=$3 AND operation_id=$4 AND current_attempt_id=$5 AND state=$6 AND accepted_attempt_id IS NULL AND EXISTS(SELECT 1 FROM bot_send_attempts WHERE installation_id=$3 AND operation_id=$4 AND attempt_id=$5 AND outcome=$7)`, accepted, int64(state), installation, operation, attempt, int64(store.BotSendOperationActive), outcome)
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

func (r *Repo) DeleteBotSendsByInstallation(ctx context.Context, installation id.ID) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	if installation.IsZero() {
		return store.ErrConflict
	}
	if _, err := r.tx.ExecContext(ctx, `DELETE FROM bot_send_attempts WHERE installation_id=$1`, installation); err != nil {
		return wrap(err)
	}
	_, err := r.tx.ExecContext(ctx, `DELETE FROM bot_send_operations WHERE installation_id=$1`, installation)
	return wrap(err)
}

func scanBotSendOperation(row *sql.Row) (store.BotSendOperationRow, error) {
	var out store.BotSendOperationRow
	var accepted []byte
	var state int64
	if err := row.Scan(&out.InstallationID, &out.OperationID, &out.UserID, &out.DeviceID, &out.CommunityID, &out.GroupID, &out.CurrentAttempt, &accepted, &state); err != nil {
		return store.BotSendOperationRow{}, wrap(err)
	}
	if accepted != nil {
		var v id.ID
		if err := v.Scan(accepted); err != nil {
			return store.BotSendOperationRow{}, err
		}
		out.AcceptedAttempt = &v
	}
	out.State = uint8(state)
	return out, nil
}

func scanBotSendAttempt(row *sql.Row) (store.BotSendAttemptRow, error) {
	var out store.BotSendAttemptRow
	var predecessor []byte
	var generation, epoch, class, outcome int64
	if err := row.Scan(&out.InstallationID, &out.OperationID, &out.AttemptID, &predecessor, &generation, &epoch, &class, &out.CiphertextSHA256, &out.FingerprintSHA256, &outcome, &out.Receipt); err != nil {
		return store.BotSendAttemptRow{}, wrap(err)
	}
	if predecessor != nil {
		var v id.ID
		if err := v.Scan(predecessor); err != nil {
			return store.BotSendAttemptRow{}, err
		}
		out.PredecessorID = &v
	}
	out.Generation = uint64(generation)
	out.Epoch = uint64(epoch)
	out.Class = uint8(class)
	out.Outcome = uint8(outcome)
	return out, nil
}

func nullableID(v *id.ID) any {
	if v == nil {
		return nil
	}
	return *v
}

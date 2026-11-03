package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func validModerationRemoval(v store.ModerationRemovalRow) bool {
	return !v.RequestID.IsZero() && !v.ActionID.IsZero() && !v.CommunityID.IsZero() && !v.GroupID.IsZero() &&
		v.TargetSequence > 0 && v.TargetSequence <= math.MaxInt64 && v.Revision == 0 &&
		len(v.Proof) > 0 && len(v.Proof) <= 105 && len(v.Signature) == 64 && v.AcceptedAt >= 0
}

func (r *Repo) AcceptModerationRemoval(ctx context.Context, v store.ModerationRemovalRow) (store.ModerationRemovalRow, error) {
	if !r.inTx {
		return store.ModerationRemovalRow{}, errors.New("store: accepting a moderation removal requires a transaction")
	}
	if !validModerationRemoval(v) {
		return store.ModerationRemovalRow{}, store.ErrConflict
	}
	if _, err := r.tx.ExecContext(ctx, `INSERT INTO moderation_removal_heads(group_id,revision) VALUES(?,0) ON CONFLICT(group_id) DO NOTHING`, v.GroupID); err != nil {
		return store.ModerationRemovalRow{}, err
	}
	if err := r.tx.QueryRowContext(ctx, `UPDATE moderation_removal_heads SET revision=revision+1 WHERE group_id=? RETURNING revision`, v.GroupID).Scan(&v.Revision); err != nil {
		return store.ModerationRemovalRow{}, wrap(err)
	}
	_, err := r.tx.ExecContext(ctx, `INSERT INTO moderation_removals(request_id,action_id,community_id,group_id,target_seq,revision,proof,signature,accepted_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		v.RequestID, v.ActionID, v.CommunityID, v.GroupID, int64(v.TargetSequence), int64(v.Revision), v.Proof, v.Signature, v.AcceptedAt)
	if err != nil {
		return store.ModerationRemovalRow{}, wrap(err)
	}
	result, err := r.tx.ExecContext(ctx, `UPDATE mls_app_messages SET blob=NULL,deleted_at=? WHERE group_id=? AND seq=? AND deleted_at IS NULL`, v.AcceptedAt, v.GroupID, int64(v.TargetSequence))
	if err != nil {
		return store.ModerationRemovalRow{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return store.ModerationRemovalRow{}, err
	}
	if n != 1 {
		return store.ModerationRemovalRow{}, store.ErrNotFound
	}
	return v, nil
}

func scanModerationRemoval(row interface{ Scan(...any) error }) (store.ModerationRemovalRow, error) {
	var out store.ModerationRemovalRow
	var seq, rev int64
	err := row.Scan(&out.RequestID, &out.ActionID, &out.CommunityID, &out.GroupID, &seq, &rev, &out.Proof, &out.Signature, &out.AcceptedAt)
	if err != nil {
		return out, wrap(err)
	}
	if seq <= 0 || rev <= 0 {
		return store.ModerationRemovalRow{}, store.ErrConflict
	}
	out.TargetSequence, out.Revision = uint64(seq), uint64(rev)
	return out, nil
}

func (r *Repo) GetModerationRemoval(ctx context.Context, groupID, requestID id.ID) (store.ModerationRemovalRow, error) {
	var q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	} = r.read
	if r.tx != nil {
		q = r.tx
	}
	return scanModerationRemoval(q.QueryRowContext(ctx, `SELECT request_id,action_id,community_id,group_id,target_seq,revision,proof,signature,accepted_at FROM moderation_removals WHERE group_id=? AND request_id=?`, groupID, requestID))
}

func (r *Repo) GetModerationRemovalByRequest(ctx context.Context, requestID id.ID) (store.ModerationRemovalRow, error) {
	if requestID.IsZero() {
		return store.ModerationRemovalRow{}, store.ErrConflict
	}
	var q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	} = r.read
	if r.tx != nil {
		q = r.tx
	}
	return scanModerationRemoval(q.QueryRowContext(ctx, `SELECT request_id,action_id,community_id,group_id,target_seq,revision,proof,signature,accepted_at FROM moderation_removals WHERE request_id=?`, requestID))
}

func (r *Repo) ListModerationRemovals(ctx context.Context, groupID id.ID, after uint64, limit int32) (out store.ModerationRemovalPage, retErr error) {
	if groupID.IsZero() || after > math.MaxInt64 || limit < 1 || limit > 250 {
		return out, store.ErrConflict
	}
	var db interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	} = r.read
	if r.tx != nil {
		db = r.tx
	}
	rows, err := db.QueryContext(ctx, `SELECT request_id,action_id,community_id,group_id,target_seq,revision,proof,signature,accepted_at FROM moderation_removals WHERE group_id=? AND revision>? ORDER BY revision LIMIT ?`, groupID, int64(after), limit)
	if err != nil {
		return out, err
	}
	defer func() { retErr = errors.Join(retErr, wrap(rows.Close())) }()
	out.NextRevision = after
	for rows.Next() {
		v, err := scanModerationRemoval(rows)
		if err != nil {
			return store.ModerationRemovalPage{}, err
		}
		out.Rows = append(out.Rows, v)
		out.NextRevision = v.Revision
	}
	return out, rows.Err()
}

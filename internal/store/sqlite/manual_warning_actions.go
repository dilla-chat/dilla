package sqlite

import (
	"context"
	"math"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func (r *Repo) PutManualWarningAction(ctx context.Context, v store.ManualWarningActionRow) error {
	if !r.inTx || v.CommunityID.IsZero() || v.RequestID.IsZero() || v.ActionID.IsZero() || v.ActorUserID.IsZero() ||
		v.ActorDeviceID.IsZero() || v.TargetUserID.IsZero() || v.Revision == 0 || v.Revision > math.MaxInt64 || v.AcceptedAt <= 0 {
		return store.ErrConflict
	}
	_, err := r.tx.ExecContext(ctx, `INSERT INTO manual_warning_actions(community_id,request_id,action_id,actor_user_id,actor_device_id,target_user_id,revision,accepted_at) VALUES(?,?,?,?,?,?,?,?)`,
		v.CommunityID, v.RequestID, v.ActionID, v.ActorUserID, v.ActorDeviceID, v.TargetUserID, int64(v.Revision), v.AcceptedAt)
	return wrap(err)
}

func (r *Repo) GetManualWarningAction(ctx context.Context, community, request id.ID) (store.ManualWarningActionRow, error) {
	var row store.ManualWarningActionRow
	var revision int64
	query := r.rawRead
	if r.tx != nil {
		query = nil
	}
	var err error
	if r.tx != nil {
		err = r.tx.QueryRowContext(ctx, `SELECT community_id,request_id,action_id,actor_user_id,actor_device_id,target_user_id,revision,accepted_at FROM manual_warning_actions WHERE community_id=? AND request_id=?`, community, request).
			Scan(&row.CommunityID, &row.RequestID, &row.ActionID, &row.ActorUserID, &row.ActorDeviceID, &row.TargetUserID, &revision, &row.AcceptedAt)
	} else {
		err = query.QueryRowContext(ctx, `SELECT community_id,request_id,action_id,actor_user_id,actor_device_id,target_user_id,revision,accepted_at FROM manual_warning_actions WHERE community_id=? AND request_id=?`, community, request).
			Scan(&row.CommunityID, &row.RequestID, &row.ActionID, &row.ActorUserID, &row.ActorDeviceID, &row.TargetUserID, &revision, &row.AcceptedAt)
	}
	if err != nil {
		return store.ManualWarningActionRow{}, wrap(err)
	}
	if revision <= 0 {
		return store.ManualWarningActionRow{}, store.ErrConflict
	}
	row.Revision = uint64(revision)
	return row, nil
}

package postgres

import (
	"context"
	"database/sql"

	"github.com/dilla-chat/dilla/internal/id"
)

// LockGroup is the cross-process fence shared by human and managed-Bot message
// acceptance and all group mutations that take this lock.
func (r *Repo) LockGroup(ctx context.Context, groupID id.ID) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	var locked id.ID
	if err := r.tx.QueryRowContext(ctx, `SELECT group_id FROM mls_groups WHERE group_id=$1 FOR UPDATE`, groupID).Scan(&locked); err != nil {
		return wrap(err)
	}
	return nil
}

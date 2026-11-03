package sqlite

import (
	"context"
	"database/sql"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// LockGroup takes SQLite's database-wide writer fence before policy reads. The
// no-op update intentionally serializes with all other write transactions; SQLite
// does not provide row-level locks.
func (r *Repo) LockGroup(ctx context.Context, groupID id.ID) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	res, err := r.tx.ExecContext(ctx, `UPDATE mls_groups SET seq=seq WHERE group_id=?`, groupID)
	if err != nil {
		return wrap(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

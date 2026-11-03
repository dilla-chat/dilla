package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func (r *Repo) SetBotJoinRolePolicy(ctx context.Context, installation, role, approver id.ID, at int64) error {
	if !r.inTx {
		return fmt.Errorf("store: bot join role policy requires transaction")
	}
	if role.IsZero() {
		_, err := r.tx.ExecContext(ctx, `DELETE FROM bot_join_role_policy WHERE installation_id=?`, installation)
		return wrap(err)
	}
	var community id.ID
	if err := r.tx.QueryRowContext(ctx, `SELECT community_id FROM bot_installations WHERE id=? AND removed_at IS NULL`, installation).Scan(&community); err != nil {
		return wrap(err)
	}
	_, err := r.tx.ExecContext(ctx, `INSERT INTO bot_join_role_policy(installation_id,community_id,role_id,approved_by,approved_at) VALUES(?,?,?,?,?) ON CONFLICT(installation_id) DO UPDATE SET community_id=excluded.community_id,role_id=excluded.role_id,approved_by=excluded.approved_by,approved_at=excluded.approved_at`, installation, community, role, approver, at)
	return wrap(err)
}

func (r *Repo) GetBotJoinRolePolicy(ctx context.Context, installation id.ID) (store.BotJoinRolePolicyRow, error) {
	q := `SELECT installation_id,community_id,role_id,approved_by,approved_at FROM bot_join_role_policy WHERE installation_id=?`
	var v store.BotJoinRolePolicyRow
	var err error
	if r.inTx {
		err = r.tx.QueryRowContext(ctx, q, installation).Scan(&v.InstallationID, &v.CommunityID, &v.RoleID, &v.ApprovedBy, &v.ApprovedAt)
	} else {
		err = r.read.QueryRowContext(ctx, q, installation).Scan(&v.InstallationID, &v.CommunityID, &v.RoleID, &v.ApprovedBy, &v.ApprovedAt)
	}
	if err == sql.ErrNoRows {
		return store.BotJoinRolePolicyRow{}, store.ErrNotFound
	}
	return v, wrap(err)
}

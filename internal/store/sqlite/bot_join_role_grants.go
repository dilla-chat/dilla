package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dilla-chat/dilla/internal/store"
)

func scanBotJoinReceipt(row interface{ Scan(...any) error }) (store.BotJoinRoleGrantReceipt, error) {
	var v store.BotJoinRoleGrantReceipt
	err := row.Scan(&v.RequestID, &v.InstallationID, &v.CommunityID, &v.UserID, &v.EpisodeID, &v.RoleID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.BotJoinRoleGrantReceipt{}, store.ErrNotFound
	}
	return v, wrap(err)
}

func (r *Repo) GrantBotJoinRoleForEpisode(ctx context.Context, req store.BotJoinRoleGrantRequest) (store.BotJoinRoleGrantReceipt, bool, error) {
	if req.RequestID.IsZero() || req.InstallationID.IsZero() || req.CommunityID.IsZero() || req.UserID.IsZero() || req.EpisodeID.IsZero() {
		return store.BotJoinRoleGrantReceipt{}, false, fmt.Errorf("store: invalid bot join role grant request")
	}
	if !r.inTx {
		return store.BotJoinRoleGrantReceipt{}, false, fmt.Errorf("store: bot join role grant requires caller transaction")
	}
	query := `SELECT request_id,installation_id,community_id,user_id,episode_id,role_id FROM bot_join_role_grant_receipts WHERE installation_id=? AND user_id=? AND episode_id=?`
	got, err := scanBotJoinReceipt(r.tx.QueryRowContext(ctx, query, req.InstallationID, req.UserID, req.EpisodeID))
	if err == nil {
		if got.RequestID != req.RequestID || got.CommunityID != req.CommunityID {
			return store.BotJoinRoleGrantReceipt{}, false, store.ErrConflict
		}
		return got, false, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.BotJoinRoleGrantReceipt{}, false, err
	}
	var priorID []byte
	err = r.tx.QueryRowContext(ctx, `SELECT request_id FROM bot_join_role_grant_receipts WHERE installation_id=? AND request_id=?`, req.InstallationID, req.RequestID).Scan(&priorID)
	if err == nil {
		return store.BotJoinRoleGrantReceipt{}, false, store.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return store.BotJoinRoleGrantReceipt{}, false, wrap(err)
	}
	var active int
	if err := r.tx.QueryRowContext(ctx, `SELECT 1 FROM bot_installations WHERE id=? AND community_id=? AND state=? AND removed_at IS NULL`, req.InstallationID, req.CommunityID, int64(store.BotInstallationReady)).Scan(&active); err != nil {
		return store.BotJoinRoleGrantReceipt{}, false, wrap(err)
	}
	var episode []byte
	if err := r.tx.QueryRowContext(ctx, `SELECT episode_id FROM community_member_episodes WHERE community_id=? AND user_id=?`, req.CommunityID, req.UserID).Scan(&episode); err != nil {
		return store.BotJoinRoleGrantReceipt{}, false, wrap(err)
	}
	if string(episode) != string(req.EpisodeID[:]) {
		return store.BotJoinRoleGrantReceipt{}, false, store.ErrNotFound
	}
	var receipt store.BotJoinRoleGrantReceipt
	if err := r.tx.QueryRowContext(ctx, `SELECT installation_id,community_id,role_id FROM bot_join_role_policy WHERE installation_id=? AND community_id=?`, req.InstallationID, req.CommunityID).Scan(&receipt.InstallationID, &receipt.CommunityID, &receipt.RoleID); err != nil {
		return store.BotJoinRoleGrantReceipt{}, false, wrap(err)
	}
	receipt.RequestID, receipt.UserID, receipt.EpisodeID = req.RequestID, req.UserID, req.EpisodeID
	if _, err := r.tx.ExecContext(ctx, `INSERT INTO member_roles(community_id,user_id,role_id) VALUES(?,?,?) ON CONFLICT DO NOTHING`, receipt.CommunityID, receipt.UserID, receipt.RoleID); err != nil {
		return store.BotJoinRoleGrantReceipt{}, false, wrap(err)
	}
	if _, err := r.tx.ExecContext(ctx, `INSERT INTO bot_join_role_grant_receipts(installation_id,request_id,community_id,user_id,episode_id,role_id) VALUES(?,?,?,?,?,?)`, receipt.InstallationID, receipt.RequestID, receipt.CommunityID, receipt.UserID, receipt.EpisodeID, receipt.RoleID); err != nil {
		return store.BotJoinRoleGrantReceipt{}, false, wrap(err)
	}
	if _, err := r.tx.ExecContext(ctx, `INSERT INTO bot_join_role_grant_sources(installation_id,user_id,episode_id,community_id,role_id) VALUES(?,?,?,?,?)`, receipt.InstallationID, receipt.UserID, receipt.EpisodeID, receipt.CommunityID, receipt.RoleID); err != nil {
		return store.BotJoinRoleGrantReceipt{}, false, wrap(err)
	}
	return receipt, true, nil
}

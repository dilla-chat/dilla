package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func validateReactionRoleGrant(g store.ReactionRoleGrant) error {
	if g.CommunityID.IsZero() || g.UserID.IsZero() || g.RoleID.IsZero() || g.ChannelID.IsZero() || g.MessageSeq == 0 || g.MessageSeq > math.MaxInt64 || len([]byte(g.Reaction)) == 0 || len([]byte(g.Reaction)) > 32 {
		return fmt.Errorf("store: invalid reaction role grant")
	}
	return nil
}

func (r *Repo) AddReactionRoleGrant(ctx context.Context, g store.ReactionRoleGrant) error {
	if g.MessageSeq > math.MaxInt64 {
		return fmt.Errorf("store: invalid reaction role grant")
	}
	if err := validateReactionRoleGrant(g); err != nil {
		return err
	}
	if !r.inTx {
		return r.Tx(ctx, func(tx store.Repository) error {
			return tx.AddReactionRoleGrant(ctx, g)
		})
	}
	if _, err := r.tx.ExecContext(ctx, `INSERT INTO member_roles(community_id,user_id,role_id) VALUES(?,?,?) ON CONFLICT DO NOTHING`, g.CommunityID, g.UserID, g.RoleID); err != nil {
		return wrap(err)
	}
	if _, err := r.tx.ExecContext(ctx, `INSERT INTO reaction_role_grant_sources(community_id,user_id,role_id,channel_id,message_seq,reaction) VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, g.CommunityID, g.UserID, g.RoleID, g.ChannelID, int64(g.MessageSeq), g.Reaction); err != nil {
		return wrap(err)
	}
	return nil
}

func (r *Repo) RemoveReactionRoleGrant(ctx context.Context, g store.ReactionRoleGrant) error {
	if g.MessageSeq > math.MaxInt64 {
		return fmt.Errorf("store: invalid reaction role grant")
	}
	if err := validateReactionRoleGrant(g); err != nil {
		return err
	}
	if !r.inTx {
		return r.Tx(ctx, func(tx store.Repository) error {
			return tx.RemoveReactionRoleGrant(ctx, g)
		})
	}
	res, err := r.tx.ExecContext(ctx, `DELETE FROM reaction_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=? AND channel_id=? AND message_seq=? AND reaction=?`, g.CommunityID, g.UserID, g.RoleID, g.ChannelID, int64(g.MessageSeq), g.Reaction)
	if err != nil {
		return wrap(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	var remaining int64
	if err := r.tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM member_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?) + (SELECT COUNT(*) FROM reaction_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?) + (SELECT COUNT(*) FROM bot_join_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?)`, g.CommunityID, g.UserID, g.RoleID, g.CommunityID, g.UserID, g.RoleID, g.CommunityID, g.UserID, g.RoleID).Scan(&remaining); err != nil {
		return err
	}
	if remaining == 0 {
		_, err = r.tx.ExecContext(ctx, `DELETE FROM member_roles WHERE community_id=? AND user_id=? AND role_id=?`, g.CommunityID, g.UserID, g.RoleID)
		return wrap(err)
	}
	return nil
}

func (r *Repo) RemoveReactionRolePanelGrants(ctx context.Context, community, channel id.ID, seq uint64) ([]id.ID, error) {
	if seq == 0 || seq > math.MaxInt64 {
		return nil, store.ErrNotFound
	}
	if !r.inTx {
		var users []id.ID
		err := r.Tx(ctx, func(tx store.Repository) error {
			var e error
			users, e = tx.RemoveReactionRolePanelGrants(ctx, community, channel, seq)
			return e
		})
		return users, err
	}
	rows, err := r.tx.QueryContext(ctx, `SELECT DISTINCT user_id FROM reaction_role_grant_sources WHERE community_id=? AND channel_id=? AND message_seq=?`, community, channel, int64(seq))
	if err != nil {
		return nil, wrap(err)
	}
	var users []id.ID
	scanErr := func() (retErr error) {
		defer func() { retErr = errors.Join(retErr, wrap(rows.Close())) }()
		for rows.Next() {
			var u id.ID
			if err := rows.Scan(&u); err != nil {
				return wrap(err)
			}
			users = append(users, u)
		}
		return wrap(rows.Err())
	}()
	if scanErr != nil {
		return nil, scanErr
	}
	type pair struct{ user, role id.ID }
	pr, err := r.tx.QueryContext(ctx, `SELECT DISTINCT user_id,role_id FROM reaction_role_grant_sources WHERE community_id=? AND channel_id=? AND message_seq=?`, community, channel, int64(seq))
	if err != nil {
		return nil, wrap(err)
	}
	var pairs []pair
	scanErr = func() (retErr error) {
		defer func() { retErr = errors.Join(retErr, wrap(pr.Close())) }()
		for pr.Next() {
			var p pair
			if err := pr.Scan(&p.user, &p.role); err != nil {
				return wrap(err)
			}
			pairs = append(pairs, p)
		}
		return wrap(pr.Err())
	}()
	if scanErr != nil {
		return nil, scanErr
	}
	if _, err := r.tx.ExecContext(ctx, `DELETE FROM reaction_role_grant_sources WHERE community_id=? AND channel_id=? AND message_seq=?`, community, channel, int64(seq)); err != nil {
		return nil, wrap(err)
	}
	for _, p := range pairs {
		var count int
		if err := r.tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM member_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?)+(SELECT COUNT(*) FROM reaction_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?)+(SELECT COUNT(*) FROM bot_join_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?)`, community, p.user, p.role, community, p.user, p.role, community, p.user, p.role).Scan(&count); err != nil {
			return nil, err
		}
		if count == 0 {
			if _, err := r.tx.ExecContext(ctx, `DELETE FROM member_roles WHERE community_id=? AND user_id=? AND role_id=?`, community, p.user, p.role); err != nil {
				return nil, wrap(err)
			}
		}
	}
	return users, nil
}

func (r *Repo) ListReactionRolePanelGrantUsers(ctx context.Context, community, channel id.ID, seq uint64) (users []id.ID, retErr error) {
	if seq == 0 || seq > math.MaxInt64 {
		return nil, store.ErrNotFound
	}
	var rows *sql.Rows
	var err error
	if r.inTx {
		rows, err = r.tx.QueryContext(ctx, `SELECT DISTINCT user_id FROM reaction_role_grant_sources WHERE community_id=? AND channel_id=? AND message_seq=?`, community, channel, int64(seq))
	} else {
		rows, err = r.rawRead.QueryContext(ctx, `SELECT DISTINCT user_id FROM reaction_role_grant_sources WHERE community_id=? AND channel_id=? AND message_seq=?`, community, channel, int64(seq))
	}
	if err != nil {
		return nil, wrap(err)
	}
	defer func() { retErr = errors.Join(retErr, wrap(rows.Close())) }()
	users = nil
	for rows.Next() {
		var u id.ID
		if err := rows.Scan(&u); err != nil {
			return nil, wrap(err)
		}
		users = append(users, u)
	}
	return users, wrap(rows.Err())
}

func (r *Repo) RemoveReactionRoleChannelGrants(ctx context.Context, community, channel id.ID) ([]id.ID, error) {
	if !r.inTx {
		var users []id.ID
		err := r.Tx(ctx, func(tx store.Repository) error {
			var e error
			users, e = tx.RemoveReactionRoleChannelGrants(ctx, community, channel)
			return e
		})
		return users, err
	}
	rows, err := r.tx.QueryContext(ctx, `SELECT DISTINCT user_id FROM reaction_role_grant_sources WHERE community_id=? AND channel_id=?`, community, channel)
	if err != nil {
		return nil, wrap(err)
	}
	var users []id.ID
	scanErr := func() (retErr error) {
		defer func() { retErr = errors.Join(retErr, wrap(rows.Close())) }()
		for rows.Next() {
			var u id.ID
			if err := rows.Scan(&u); err != nil {
				return wrap(err)
			}
			users = append(users, u)
		}
		return wrap(rows.Err())
	}()
	if scanErr != nil {
		return nil, scanErr
	}
	type pair struct{ user, role id.ID }
	pr, err := r.tx.QueryContext(ctx, `SELECT DISTINCT user_id,role_id FROM reaction_role_grant_sources WHERE community_id=? AND channel_id=?`, community, channel)
	if err != nil {
		return nil, wrap(err)
	}
	var pairs []pair
	scanErr = func() (retErr error) {
		defer func() { retErr = errors.Join(retErr, wrap(pr.Close())) }()
		for pr.Next() {
			var p pair
			if err := pr.Scan(&p.user, &p.role); err != nil {
				return wrap(err)
			}
			pairs = append(pairs, p)
		}
		return wrap(pr.Err())
	}()
	if scanErr != nil {
		return nil, scanErr
	}
	if _, err := r.tx.ExecContext(ctx, `DELETE FROM reaction_role_grant_sources WHERE community_id=? AND channel_id=?`, community, channel); err != nil {
		return nil, wrap(err)
	}
	for _, p := range pairs {
		var count int
		if err := r.tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM member_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?)+(SELECT COUNT(*) FROM reaction_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?)+(SELECT COUNT(*) FROM bot_join_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?)`, community, p.user, p.role, community, p.user, p.role, community, p.user, p.role).Scan(&count); err != nil {
			return nil, err
		}
		if count == 0 {
			if _, err := r.tx.ExecContext(ctx, `DELETE FROM member_roles WHERE community_id=? AND user_id=? AND role_id=?`, community, p.user, p.role); err != nil {
				return nil, wrap(err)
			}
		}
	}
	return users, nil
}

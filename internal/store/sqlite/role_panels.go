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

type rolePanelQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (r *Repo) SetRolePanel(ctx context.Context, panel store.RolePanelRow, options []store.RolePanelOptionRow) error {
	if !r.inTx {
		return fmt.Errorf("store: role panel write requires transaction")
	}
	if panel.CommunityID.IsZero() || panel.ChannelID.IsZero() || panel.MessageSeq == 0 || panel.MessageSeq > math.MaxInt64 || panel.AuthorID.IsZero() || panel.InstallationID.IsZero() || len(options) == 0 || len(options) > 100 {
		return fmt.Errorf("store: invalid role panel")
	}
	seen := map[string]bool{}
	for _, option := range options {
		if option.RoleID.IsZero() || len(option.Emoji) == 0 || len(option.Emoji) > 32 || seen[option.Emoji] {
			return fmt.Errorf("store: invalid role panel option")
		}
		seen[option.Emoji] = true
	}
	res, err := r.tx.ExecContext(ctx, `INSERT INTO role_panels(community_id,channel_id,message_seq,author_id,installation_id,send_operation_id) VALUES(?,?,?,?,?,?) ON CONFLICT(channel_id,message_seq) DO UPDATE SET community_id=excluded.community_id WHERE role_panels.community_id=excluded.community_id AND role_panels.author_id=excluded.author_id AND role_panels.installation_id=excluded.installation_id AND role_panels.send_operation_id=excluded.send_operation_id`, panel.CommunityID, panel.ChannelID, int64(panel.MessageSeq), panel.AuthorID, panel.InstallationID, panel.SendOperationID)
	if err != nil {
		return wrap(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return store.ErrConflict
	}
	// Remove provenance for options that are being withdrawn before deleting
	// the old option rows. Recompute the effective role in this transaction.
	old, err := r.tx.QueryContext(ctx, `SELECT emoji,role_id FROM role_panel_options WHERE channel_id=? AND message_seq=?`, panel.ChannelID, int64(panel.MessageSeq))
	if err != nil {
		return wrap(err)
	}
	type oldOption struct {
		emoji string
		role  id.ID
	}
	var oldOptions []oldOption
	scanErr := func() (retErr error) {
		defer func() { retErr = errors.Join(retErr, wrap(old.Close())) }()
		for old.Next() {
			var v oldOption
			if err := old.Scan(&v.emoji, &v.role); err != nil {
				return wrap(err)
			}
			oldOptions = append(oldOptions, v)
		}
		return wrap(old.Err())
	}()
	if scanErr != nil {
		return scanErr
	}
	for _, prior := range oldOptions {
		keep := false
		for _, next := range options {
			if next.Emoji == prior.emoji && next.RoleID == prior.role {
				keep = true
				break
			}
		}
		if keep {
			continue
		}
		users, err := r.tx.QueryContext(ctx, `SELECT DISTINCT user_id FROM reaction_role_grant_sources WHERE community_id=? AND channel_id=? AND message_seq=? AND reaction=? AND role_id=?`, panel.CommunityID, panel.ChannelID, int64(panel.MessageSeq), prior.emoji, prior.role)
		if err != nil {
			return wrap(err)
		}
		var affected []id.ID
		scanErr := func() (retErr error) {
			defer func() { retErr = errors.Join(retErr, wrap(users.Close())) }()
			for users.Next() {
				var u id.ID
				if err := users.Scan(&u); err != nil {
					return wrap(err)
				}
				affected = append(affected, u)
			}
			return wrap(users.Err())
		}()
		if scanErr != nil {
			return scanErr
		}
		if _, err := r.tx.ExecContext(ctx, `DELETE FROM reaction_role_grant_sources WHERE community_id=? AND channel_id=? AND message_seq=? AND reaction=? AND role_id=?`, panel.CommunityID, panel.ChannelID, int64(panel.MessageSeq), prior.emoji, prior.role); err != nil {
			return wrap(err)
		}
		for _, u := range affected {
			var count int
			if err := r.tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM member_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?)+(SELECT COUNT(*) FROM reaction_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?)+(SELECT COUNT(*) FROM bot_join_role_grant_sources WHERE community_id=? AND user_id=? AND role_id=?)`, panel.CommunityID, u, prior.role, panel.CommunityID, u, prior.role, panel.CommunityID, u, prior.role).Scan(&count); err != nil {
				return wrap(err)
			}
			if count == 0 {
				if _, err := r.tx.ExecContext(ctx, `DELETE FROM member_roles WHERE community_id=? AND user_id=? AND role_id=?`, panel.CommunityID, u, prior.role); err != nil {
					return wrap(err)
				}
			}
		}
	}
	if _, err := r.tx.ExecContext(ctx, `DELETE FROM role_panel_options WHERE channel_id=? AND message_seq=?`, panel.ChannelID, int64(panel.MessageSeq)); err != nil {
		return wrap(err)
	}
	for _, option := range options {
		if _, err := r.tx.ExecContext(ctx, `INSERT INTO role_panel_options(channel_id,message_seq,emoji,role_id) VALUES(?,?,?,?)`, panel.ChannelID, int64(panel.MessageSeq), option.Emoji, option.RoleID); err != nil {
			return wrap(err)
		}
	}
	return nil
}

func (r *Repo) GetRolePanel(ctx context.Context, channel id.ID, seq uint64) (store.RolePanelRow, error) {
	if seq == 0 || seq > math.MaxInt64 {
		return store.RolePanelRow{}, store.ErrNotFound
	}
	var p store.RolePanelRow
	var q rolePanelQueryer = r.rawRead
	if r.inTx {
		q = r.tx
	}
	err := q.QueryRowContext(ctx, `SELECT community_id,channel_id,message_seq,author_id,installation_id,send_operation_id FROM role_panels WHERE channel_id=? AND message_seq=?`, channel, int64(seq)).Scan(&p.CommunityID, &p.ChannelID, &p.MessageSeq, &p.AuthorID, &p.InstallationID, &p.SendOperationID)
	return p, wrap(err)
}

func (r *Repo) DeleteRolePanel(ctx context.Context, channel id.ID, seq uint64) error {
	if !r.inTx {
		return fmt.Errorf("store: role panel delete requires transaction")
	}
	if seq == 0 || seq > math.MaxInt64 {
		return store.ErrNotFound
	}
	res, err := r.tx.ExecContext(ctx, `DELETE FROM role_panels WHERE channel_id=? AND message_seq=?`, channel, int64(seq))
	if err != nil {
		return wrap(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) ListRolePanelOptions(ctx context.Context, channel id.ID, seq uint64) (out []store.RolePanelOptionRow, retErr error) {
	if seq == 0 || seq > math.MaxInt64 {
		return nil, store.ErrNotFound
	}
	var q rolePanelQueryer = r.rawRead
	if r.inTx {
		q = r.tx
	}
	rows, err := q.QueryContext(ctx, `SELECT emoji,role_id FROM role_panel_options WHERE channel_id=? AND message_seq=? ORDER BY emoji COLLATE BINARY`, channel, int64(seq))
	if err != nil {
		return nil, wrap(err)
	}
	defer func() { retErr = errors.Join(retErr, wrap(rows.Close())) }()
	out = []store.RolePanelOptionRow{}
	for rows.Next() {
		var v store.RolePanelOptionRow
		if err := rows.Scan(&v.Emoji, &v.RoleID); err != nil {
			return nil, wrap(err)
		}
		out = append(out, v)
	}
	return out, wrap(rows.Err())
}

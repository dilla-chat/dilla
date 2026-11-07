package postgres

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres/pgdb"
)

// botScopeChunk bounds one IN list of BotStateRevisions, far below Postgres's 65535 parameters.
const botScopeChunk = 500

func (r *Repo) PutBotCatalog(ctx context.Context, row store.BotCatalogRow) error {
	if row.CommunityID.IsZero() || row.Revision == 0 || row.Revision > math.MaxInt64 || row.Body == nil {
		return store.ErrConflict
	}
	return wrap(r.w.PutBotCatalog(ctx, pgdb.PutBotCatalogParams{CommunityID: row.CommunityID, Revision: int64(row.Revision), Body: row.Body}))
}

func (r *Repo) GetBotCatalog(ctx context.Context, community id.ID) (store.BotCatalogRow, error) {
	c, err := r.r.GetBotCatalog(ctx, pgdb.GetBotCatalogParams{CommunityID: community})
	if err != nil {
		return store.BotCatalogRow{}, wrap(err)
	}
	if c.Revision <= 0 {
		return store.BotCatalogRow{}, store.ErrConflict
	}
	return store.BotCatalogRow{CommunityID: c.CommunityID, Revision: uint64(c.Revision), Body: c.Body}, nil
}

func (r *Repo) PutBotCommandKey(ctx context.Context, row store.BotCommandKeyRow) error {
	if row.CommunityID.IsZero() || row.KeyID.IsZero() || len(row.Publication) == 0 || row.PublishedAt < 0 {
		return store.ErrConflict
	}
	return wrap(r.w.PutBotCommandKey(ctx, pgdb.PutBotCommandKeyParams{CommunityID: row.CommunityID, KeyID: row.KeyID, Publication: row.Publication, PublishedAt: row.PublishedAt}))
}

func (r *Repo) GetBotCommandKey(ctx context.Context, community id.ID) (store.BotCommandKeyRow, error) {
	k, err := r.r.GetBotCommandKey(ctx, pgdb.GetBotCommandKeyParams{CommunityID: community})
	if err != nil {
		return store.BotCommandKeyRow{}, wrap(err)
	}
	return store.BotCommandKeyRow{CommunityID: k.CommunityID, KeyID: k.KeyID, Publication: k.Publication, PublishedAt: k.PublishedAt}, nil
}

func (r *Repo) MarkBotChannel(ctx context.Context, row store.BotChannelRow) (bool, error) {
	n, err := r.w.MarkBotChannel(ctx, pgdb.MarkBotChannelParams{ChannelID: row.ChannelID, CommunityID: row.CommunityID, MarkedAt: row.MarkedAt, MarkedBy: row.MarkedBy})
	if err != nil {
		return false, wrap(err)
	}
	return n == 1, nil
}

func (r *Repo) UnmarkBotChannel(ctx context.Context, community, channel id.ID) (bool, error) {
	n, err := r.w.UnmarkBotChannel(ctx, pgdb.UnmarkBotChannelParams{CommunityID: community, ChannelID: channel})
	if err != nil {
		return false, wrap(err)
	}
	return n == 1, nil
}

func (r *Repo) ListBotChannels(ctx context.Context, community id.ID) ([]store.BotChannelRow, error) {
	rows, err := r.r.ListBotChannels(ctx, pgdb.ListBotChannelsParams{CommunityID: community})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.BotChannelRow, 0, len(rows))
	for _, b := range rows {
		out = append(out, store.BotChannelRow{ChannelID: b.ChannelID, CommunityID: b.CommunityID, MarkedAt: b.MarkedAt, MarkedBy: b.MarkedBy})
	}
	return out, nil
}

func (r *Repo) BumpBotStateRevision(ctx context.Context, kind uint8, community, scope id.ID) (uint64, error) {
	if kind > store.BotScopeCall || scope.IsZero() || community.IsZero() {
		return 0, store.ErrConflict
	}
	rev, err := r.w.BumpBotStateRevision(ctx, pgdb.BumpBotStateRevisionParams{ScopeKind: int64(kind), ScopeID: scope, CommunityID: community})
	if err != nil {
		return 0, wrap(err)
	}
	if rev <= 0 {
		return 0, store.ErrConflict
	}
	return uint64(rev), nil
}

func (r *Repo) BotStateRevisions(ctx context.Context, user id.ID, scopes []id.ID) ([]store.BotStateRevisionRow, error) {
	users, err := r.r.ListBotUserStateRevisions(ctx, pgdb.ListBotUserStateRevisionsParams{ScopeID: user})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.BotStateRevisionRow, 0, len(users))
	for _, u := range users {
		if u.Revision <= 0 {
			return nil, store.ErrConflict
		}
		out = append(out, store.BotStateRevisionRow{ScopeKind: store.BotScopeUser, ScopeID: user, CommunityID: u.CommunityID, Revision: uint64(u.Revision)})
	}
	var q interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	} = r.rawRead
	if r.tx != nil {
		q = r.tx
	}
	for start := 0; start < len(scopes); start += botScopeChunk {
		chunk := scopes[start:min(start+botScopeChunk, len(scopes))]
		args := make([]any, 0, len(chunk))
		marks := make([]string, 0, len(chunk))
		for i, s := range chunk {
			args = append(args, s)
			marks = append(marks, "$"+strconv.Itoa(i+1))
		}
		rows, err := q.QueryContext(ctx, `SELECT scope_kind, scope_id, community_id, revision FROM bot_state_revisions WHERE scope_kind IN (1, 2) AND scope_id IN (`+
			strings.Join(marks, ",")+`)`, args...)
		if err != nil {
			return nil, wrap(err)
		}
		got, err := scanBotStateRevisions(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	store.SortBotStateRevisions(out)
	return out, nil
}

func scanBotStateRevisions(rows *sql.Rows) (out []store.BotStateRevisionRow, retErr error) {
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = wrap(err)
		}
	}()
	for rows.Next() {
		var kind, rev int64
		var scope, community id.ID
		if err := rows.Scan(&kind, &scope, &community, &rev); err != nil {
			return nil, wrap(err)
		}
		if kind < 0 || kind > int64(store.BotScopeCall) || rev <= 0 {
			return nil, store.ErrConflict
		}
		out = append(out, store.BotStateRevisionRow{ScopeKind: uint8(kind), ScopeID: scope, CommunityID: community, Revision: uint64(rev)})
	}
	return out, wrap(rows.Err())
}

// DeleteBotCommunityState removes a community's Bot channels, catalog, command key, webhooks and state revisions in
// the caller's transaction (a community's soft delete fires no cascade) and returns the unmarked
// channels.
func (r *Repo) DeleteBotCommunityState(ctx context.Context, community id.ID) ([]id.ID, error) {
	if !r.inTx {
		return nil, errors.New("store: DeleteBotCommunityState outside a transaction")
	}
	channels, err := r.w.DeleteBotChannelsOfCommunity(ctx, pgdb.DeleteBotChannelsOfCommunityParams{CommunityID: community})
	if err != nil {
		return nil, wrap(err)
	}
	if err := r.w.DeleteBotCatalog(ctx, pgdb.DeleteBotCatalogParams{CommunityID: community}); err != nil {
		return nil, wrap(err)
	}
	if err := r.w.DeleteBotCommandKey(ctx, pgdb.DeleteBotCommandKeyParams{CommunityID: community}); err != nil {
		return nil, wrap(err)
	}
	if err := r.w.DeleteBotWebhooksOfCommunity(ctx, pgdb.DeleteBotWebhooksOfCommunityParams{CommunityID: community}); err != nil {
		return nil, wrap(err)
	}
	if err := r.w.DeleteBotStateRevisionsOfCommunity(ctx, pgdb.DeleteBotStateRevisionsOfCommunityParams{CommunityID: community}); err != nil {
		return nil, wrap(err)
	}
	return channels, nil
}

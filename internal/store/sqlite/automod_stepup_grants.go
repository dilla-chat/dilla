package sqlite

import (
	"bytes"
	"context"
	"database/sql"

	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite/sqlitedb"
)

func autoModGrantRow(v sqlitedb.AutomodStepupGrants) store.BotAutoModerationGrantRow {
	out := store.BotAutoModerationGrantRow{TokenHash: v.TokenHash, CommunityID: v.CommunityID, InstallationID: v.InstallationID, GroupID: v.GroupID, RequestID: v.RequestID,
		ExpectedRevision: v.ExpectedRevision, RequestExpires: v.RequestExpires, Expires: v.Expires, ConfigHash: v.ConfigHash, RequestHash: v.AcceptedRequestHash, Receipt: v.Receipt}
	copy(out.ActorUser[:], v.ActorUser)
	copy(out.ActorDevice[:], v.ActorDevice)
	if v.ConsumedAt.Valid {
		out.ConsumedAt = v.ConsumedAt.Int64
	}
	return out
}

func (r *Repo) CreateBotAutoModerationGrant(ctx context.Context, v store.BotAutoModerationGrantRow) error {
	if len(v.TokenHash) != 32 || len(v.ConfigHash) != 32 || v.CommunityID.IsZero() || v.InstallationID.IsZero() || v.GroupID.IsZero() || v.RequestID.IsZero() || v.ActorUser.IsZero() || v.ActorDevice.IsZero() || v.Expires <= 0 {
		return store.ErrConflict
	}
	return wrap(r.w.CreateBotAutoModerationGrant(ctx, sqlitedb.CreateBotAutoModerationGrantParams{TokenHash: v.TokenHash, CommunityID: v.CommunityID, InstallationID: v.InstallationID, GroupID: v.GroupID, RequestID: v.RequestID, ActorUser: v.ActorUser[:], ActorDevice: v.ActorDevice[:], ExpectedRevision: v.ExpectedRevision, RequestExpires: v.RequestExpires, ConfigHash: v.ConfigHash, Expires: v.Expires}))
}

func (r *Repo) GetBotAutoModerationGrant(ctx context.Context, hash []byte) (store.BotAutoModerationGrantRow, error) {
	if len(hash) != 32 {
		return store.BotAutoModerationGrantRow{}, store.ErrNotFound
	}
	v, err := r.r.GetBotAutoModerationGrant(ctx, sqlitedb.GetBotAutoModerationGrantParams{TokenHash: hash})
	if err != nil {
		return store.BotAutoModerationGrantRow{}, wrap(err)
	}
	return autoModGrantRow(v), nil
}

func (r *Repo) AcceptBotAutoModerationGrant(ctx context.Context, v store.BotAutoModerationGrantRow) (store.BotAutoModerationGrantRow, bool, error) {
	if len(v.TokenHash) != 32 || len(v.RequestHash) != 32 || len(v.Receipt) != 16 || v.ConsumedAt <= 0 {
		return store.BotAutoModerationGrantRow{}, false, store.ErrConflict
	}
	if !r.inTx {
		var out store.BotAutoModerationGrantRow
		var retry bool
		err := r.Tx(ctx, func(tx store.Repository) error {
			var e error
			out, retry, e = tx.AcceptBotAutoModerationGrant(ctx, v)
			return e
		})
		return out, retry, err
	}
	n, err := r.w.AcceptBotAutoModerationGrant(ctx, sqlitedb.AcceptBotAutoModerationGrantParams{ConsumedAt: sql.NullInt64{Int64: v.ConsumedAt, Valid: true}, AcceptedRequestHash: v.RequestHash, Receipt: v.Receipt, TokenHash: v.TokenHash, CommunityID: v.CommunityID, InstallationID: v.InstallationID, GroupID: v.GroupID, RequestID: v.RequestID, ActorUser: v.ActorUser[:], ActorDevice: v.ActorDevice[:], ExpectedRevision: v.ExpectedRevision, RequestExpires: v.RequestExpires, ConfigHash: v.ConfigHash, Expires: v.ConsumedAt})
	if err != nil {
		return store.BotAutoModerationGrantRow{}, false, wrap(err)
	}
	if n == 1 {
		return v, false, nil
	}
	old, err := r.GetBotAutoModerationGrant(ctx, v.TokenHash)
	if err != nil {
		return store.BotAutoModerationGrantRow{}, false, err
	}
	if old.ConsumedAt == 0 || old.CommunityID != v.CommunityID || old.InstallationID != v.InstallationID || old.GroupID != v.GroupID || old.RequestID != v.RequestID || old.ActorUser != v.ActorUser || old.ActorDevice != v.ActorDevice || old.ExpectedRevision != v.ExpectedRevision || old.RequestExpires != v.RequestExpires || !bytes.Equal(old.ConfigHash, v.ConfigHash) || !bytes.Equal(old.RequestHash, v.RequestHash) {
		return store.BotAutoModerationGrantRow{}, false, store.ErrConflict
	}
	return old, true, nil
}

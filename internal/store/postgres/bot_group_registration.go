package postgres

import (
	"context"
	"database/sql"
	"math"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func (r *Repo) GetBotGroupRegistrationReceipt(ctx context.Context, installation, operation id.ID) (store.BotGroupRegistrationReceipt, error) {
	queryer := interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}(r.rawRead)
	if r.tx != nil {
		queryer = r.tx
	}
	var out store.BotGroupRegistrationReceipt
	var generation, epoch, nextSeq int64
	err := queryer.QueryRowContext(ctx, `SELECT installation_id,operation_id,user_id,device_id,community_id,group_id,body_sha256,generation,epoch,next_seq,accepted_at FROM bot_group_registration_receipts WHERE installation_id=$1 AND operation_id=$2`, installation, operation).Scan(&out.InstallationID, &out.OperationID, &out.UserID, &out.DeviceID, &out.CommunityID, &out.GroupID, &out.BodySHA256, &generation, &epoch, &nextSeq, &out.AcceptedAt)
	if err != nil {
		return store.BotGroupRegistrationReceipt{}, wrap(err)
	}
	out.Generation, out.Epoch, out.NextSeq = uint64(generation), uint64(epoch), uint64(nextSeq)
	return out, nil
}

func (r *Repo) GetBotGroupRegistrationReceiptByGroup(ctx context.Context, group id.ID) (store.BotGroupRegistrationReceipt, error) {
	queryer := interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}(r.rawRead)
	if r.tx != nil {
		queryer = r.tx
	}
	var out store.BotGroupRegistrationReceipt
	var generation, epoch, nextSeq int64
	err := queryer.QueryRowContext(ctx, `SELECT installation_id,operation_id,user_id,device_id,community_id,group_id,body_sha256,generation,epoch,next_seq,accepted_at FROM bot_group_registration_receipts WHERE group_id=$1`, group).Scan(&out.InstallationID, &out.OperationID, &out.UserID, &out.DeviceID, &out.CommunityID, &out.GroupID, &out.BodySHA256, &generation, &epoch, &nextSeq, &out.AcceptedAt)
	if err != nil {
		return store.BotGroupRegistrationReceipt{}, wrap(err)
	}
	out.Generation, out.Epoch, out.NextSeq = uint64(generation), uint64(epoch), uint64(nextSeq)
	return out, nil
}

func (r *Repo) CreateBotGroupRegistrationReceipt(ctx context.Context, v store.BotGroupRegistrationReceipt) error {
	if !r.inTx || invalidBotGroupRegistrationReceipt(v) {
		return store.ErrConflict
	}
	_, err := r.tx.ExecContext(ctx, `INSERT INTO bot_group_registration_receipts(installation_id,operation_id,user_id,device_id,community_id,group_id,body_sha256,generation,epoch,next_seq,accepted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, v.InstallationID, v.OperationID, v.UserID, v.DeviceID, v.CommunityID, v.GroupID, v.BodySHA256, int64(v.Generation), int64(v.Epoch), int64(v.NextSeq), v.AcceptedAt)
	return wrap(err)
}

func (r *Repo) DeleteBotGroupRegistrationReceiptsByInstallation(ctx context.Context, installation id.ID) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	_, err := r.tx.ExecContext(ctx, `DELETE FROM bot_group_registration_receipts WHERE installation_id=$1`, installation)
	return wrap(err)
}

func invalidBotGroupRegistrationReceipt(v store.BotGroupRegistrationReceipt) bool {
	return v.InstallationID.IsZero() || v.OperationID.IsZero() || v.UserID.IsZero() || v.DeviceID.IsZero() || v.CommunityID.IsZero() || v.GroupID.IsZero() || len(v.BodySHA256) != 32 || v.Generation > math.MaxInt64 || v.Epoch > math.MaxInt64 || v.NextSeq == 0 || v.NextSeq > math.MaxInt64
}

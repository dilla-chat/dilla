package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"math"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func (r *Repo) GetBotGroupRegistrationReceipt(ctx context.Context, installation, operation id.ID) (store.BotGroupRegistrationReceipt, error) {
	queryer := interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}(r.read)
	if r.tx != nil {
		queryer = r.tx
	}
	var out store.BotGroupRegistrationReceipt
	var generation, epoch, nextSeq int64
	err := queryer.QueryRowContext(ctx, `SELECT installation_id,operation_id,user_id,device_id,community_id,group_id,body_sha256,generation,epoch,next_seq,accepted_at FROM bot_group_registration_receipts WHERE installation_id=? AND operation_id=?`, installation, operation).Scan(&out.InstallationID, &out.OperationID, &out.UserID, &out.DeviceID, &out.CommunityID, &out.GroupID, &out.BodySHA256, &generation, &epoch, &nextSeq, &out.AcceptedAt)
	if err != nil {
		return store.BotGroupRegistrationReceipt{}, wrap(err)
	}
	out.Generation, out.Epoch, out.NextSeq = uint64(generation), uint64(epoch), uint64(nextSeq)
	return out, nil
}

func (r *Repo) GetBotGroupRegistrationReceiptByGroup(ctx context.Context, group id.ID) (store.BotGroupRegistrationReceipt, error) {
	queryer := interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}(r.read)
	if r.tx != nil {
		queryer = r.tx
	}
	var out store.BotGroupRegistrationReceipt
	var generation, epoch, nextSeq int64
	err := queryer.QueryRowContext(ctx, `SELECT installation_id,operation_id,user_id,device_id,community_id,group_id,body_sha256,generation,epoch,next_seq,accepted_at FROM bot_group_registration_receipts WHERE group_id=?`, group).Scan(&out.InstallationID, &out.OperationID, &out.UserID, &out.DeviceID, &out.CommunityID, &out.GroupID, &out.BodySHA256, &generation, &epoch, &nextSeq, &out.AcceptedAt)
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
	_, err := r.tx.ExecContext(ctx, `INSERT INTO bot_group_registration_receipts(installation_id,operation_id,user_id,device_id,community_id,group_id,body_sha256,generation,epoch,next_seq,accepted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, v.InstallationID, v.OperationID, v.UserID, v.DeviceID, v.CommunityID, v.GroupID, v.BodySHA256, int64(v.Generation), int64(v.Epoch), int64(v.NextSeq), v.AcceptedAt)
	return wrap(err)
}

func (r *Repo) AcceptBotGroupRegistrationAdmission(ctx context.Context, request id.ID, v store.BotGroupRegistrationReceipt) (store.BotGroupRegistrationReceipt, bool, error) {
	if !r.inTx || request.IsZero() || invalidBotGroupRegistrationReceipt(v) {
		return store.BotGroupRegistrationReceipt{}, false, store.ErrConflict
	}
	res, err := r.tx.ExecContext(ctx, `INSERT INTO bot_group_registration_admission_receipts(installation_id,admission_request_id,operation_id,user_id,device_id,community_id,group_id,body_sha256,generation,epoch,next_seq,accepted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(installation_id,admission_request_id) DO NOTHING`, v.InstallationID, request, v.OperationID, v.UserID, v.DeviceID, v.CommunityID, v.GroupID, v.BodySHA256, int64(v.Generation), int64(v.Epoch), int64(v.NextSeq), v.AcceptedAt)
	if err != nil {
		return store.BotGroupRegistrationReceipt{}, false, wrap(err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return store.BotGroupRegistrationReceipt{}, false, err
	}
	var existing store.BotGroupRegistrationReceipt
	var generation, epoch, nextSeq int64
	err = r.tx.QueryRowContext(ctx, `SELECT installation_id,operation_id,user_id,device_id,community_id,group_id,body_sha256,generation,epoch,next_seq,accepted_at FROM bot_group_registration_admission_receipts WHERE installation_id=? AND admission_request_id=?`, v.InstallationID, request).Scan(&existing.InstallationID, &existing.OperationID, &existing.UserID, &existing.DeviceID, &existing.CommunityID, &existing.GroupID, &existing.BodySHA256, &generation, &epoch, &nextSeq, &existing.AcceptedAt)
	if err != nil {
		return store.BotGroupRegistrationReceipt{}, false, wrap(err)
	}
	existing.Generation, existing.Epoch, existing.NextSeq = uint64(generation), uint64(epoch), uint64(nextSeq)
	if !equalBotGroupRegistrationReceipt(existing, v) {
		return store.BotGroupRegistrationReceipt{}, false, store.ErrConflict
	}
	if inserted == 1 {
		if _, err := r.tx.ExecContext(ctx, `INSERT INTO bot_group_registration_receipts(installation_id,operation_id,user_id,device_id,community_id,group_id,body_sha256,generation,epoch,next_seq,accepted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, v.InstallationID, v.OperationID, v.UserID, v.DeviceID, v.CommunityID, v.GroupID, v.BodySHA256, int64(v.Generation), int64(v.Epoch), int64(v.NextSeq), v.AcceptedAt); err != nil {
			return store.BotGroupRegistrationReceipt{}, false, wrap(err)
		}
	}
	registered, err := r.GetBotGroupRegistrationReceipt(ctx, v.InstallationID, v.OperationID)
	if err != nil || !equalBotGroupRegistrationReceipt(registered, v) {
		if err != nil {
			return store.BotGroupRegistrationReceipt{}, false, err
		}
		return store.BotGroupRegistrationReceipt{}, false, store.ErrConflict
	}
	return existing, inserted == 0, nil
}

// GetBotGroupRegistrationAdmission reads the immutable result consumed by one admission request.
// Reading through an open transaction keeps lost-response recovery in the same authority fence.
func (r *Repo) GetBotGroupRegistrationAdmission(ctx context.Context, installation, request id.ID) (store.BotGroupRegistrationReceipt, error) {
	queryer := interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}(r.read)
	if r.tx != nil {
		queryer = r.tx
	}
	var out store.BotGroupRegistrationReceipt
	var generation, epoch, nextSeq int64
	err := queryer.QueryRowContext(ctx, `SELECT installation_id,operation_id,user_id,device_id,community_id,group_id,body_sha256,generation,epoch,next_seq,accepted_at FROM bot_group_registration_admission_receipts WHERE installation_id=? AND admission_request_id=?`, installation, request).Scan(&out.InstallationID, &out.OperationID, &out.UserID, &out.DeviceID, &out.CommunityID, &out.GroupID, &out.BodySHA256, &generation, &epoch, &nextSeq, &out.AcceptedAt)
	if err != nil {
		return store.BotGroupRegistrationReceipt{}, wrap(err)
	}
	out.Generation, out.Epoch, out.NextSeq = uint64(generation), uint64(epoch), uint64(nextSeq)
	return out, nil
}

func equalBotGroupRegistrationReceipt(a, b store.BotGroupRegistrationReceipt) bool {
	return a.InstallationID == b.InstallationID && a.OperationID == b.OperationID && a.UserID == b.UserID && a.DeviceID == b.DeviceID && a.CommunityID == b.CommunityID && a.GroupID == b.GroupID && bytes.Equal(a.BodySHA256, b.BodySHA256) && a.Generation == b.Generation && a.Epoch == b.Epoch && a.NextSeq == b.NextSeq && a.AcceptedAt == b.AcceptedAt
}

func (r *Repo) DeleteBotGroupRegistrationReceiptsByInstallation(ctx context.Context, installation id.ID) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	_, err := r.tx.ExecContext(ctx, `DELETE FROM bot_group_registration_receipts WHERE installation_id=?`, installation)
	return wrap(err)
}

func invalidBotGroupRegistrationReceipt(v store.BotGroupRegistrationReceipt) bool {
	return v.InstallationID.IsZero() || v.OperationID.IsZero() || v.UserID.IsZero() || v.DeviceID.IsZero() || v.CommunityID.IsZero() || v.GroupID.IsZero() || len(v.BodySHA256) != 32 || v.Generation > math.MaxInt64 || v.Epoch > math.MaxInt64 || v.NextSeq == 0 || v.NextSeq > math.MaxInt64
}

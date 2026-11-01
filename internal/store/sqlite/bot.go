package sqlite

import (
	"context"
	"database/sql"
	"math"
	"slices"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/sqlite/sqlitedb"
)

func (r *Repo) CreateBotRuntime(ctx context.Context, b store.BotRuntimeRow) error {
	if len(b.TokenHash) != 32 {
		return store.ErrConflict
	}
	return wrap(r.w.CreateBotRuntime(ctx, sqlitedb.CreateBotRuntimeParams{ID: b.ID, TokenHash: b.TokenHash, Created: b.Created, Rotated: b.Rotated, RevokedAt: nullInt64(b.RevokedAt)}))
}

func botRuntimeRow(b sqlitedb.BotRuntimes) store.BotRuntimeRow {
	return store.BotRuntimeRow{ID: b.ID, TokenHash: b.TokenHash, Created: b.Created, Rotated: b.Rotated, RevokedAt: ptrInt64(b.RevokedAt)}
}

func (r *Repo) GetBotRuntime(ctx context.Context, id id.ID) (store.BotRuntimeRow, error) {
	b, err := r.r.GetBotRuntime(ctx, sqlitedb.GetBotRuntimeParams{ID: id})
	if err != nil {
		return store.BotRuntimeRow{}, wrap(err)
	}
	return botRuntimeRow(b), nil
}

func (r *Repo) GetBotRuntimeByHash(ctx context.Context, hash []byte) (store.BotRuntimeRow, error) {
	if len(hash) != 32 {
		return store.BotRuntimeRow{}, store.ErrNotFound
	}
	b, err := r.r.GetBotRuntimeByHash(ctx, sqlitedb.GetBotRuntimeByHashParams{TokenHash: hash})
	if err != nil {
		return store.BotRuntimeRow{}, wrap(err)
	}
	return botRuntimeRow(b), nil
}

func (r *Repo) LockBotRuntime(ctx context.Context, runtimeID id.ID) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	_, err := r.w.LockBotRuntime(ctx, sqlitedb.LockBotRuntimeParams{ID: runtimeID})
	if err != nil {
		return wrap(err)
	}
	return nil
}

func (r *Repo) RotateBotRuntime(ctx context.Context, runtimeID id.ID, expectedHash, hash []byte, at int64) error {
	if len(hash) != 32 {
		return store.ErrConflict
	}
	if len(expectedHash) != 32 {
		return store.ErrConflict
	}
	if !r.inTx {
		return r.Tx(ctx, func(tx store.Repository) error { return tx.RotateBotRuntime(ctx, runtimeID, expectedHash, hash, at) })
	}
	n, err := r.w.RotateBotRuntime(ctx, sqlitedb.RotateBotRuntimeParams{ID: runtimeID, TokenHash: hash, Rotated: at, TokenHash_2: expectedHash})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		if _, getErr := r.r.GetBotRuntime(ctx, sqlitedb.GetBotRuntimeParams{ID: runtimeID}); getErr != nil {
			return wrap(getErr)
		}
		return store.ErrConflict
	}
	_, err = r.w.DeleteBotClaimsByRuntime(ctx, sqlitedb.DeleteBotClaimsByRuntimeParams{RuntimeID: runtimeID})
	return wrap(err)
}

func (r *Repo) RevokeBotRuntime(ctx context.Context, runtimeID id.ID, at int64) error {
	if !r.inTx {
		return r.Tx(ctx, func(tx store.Repository) error { return tx.RevokeBotRuntime(ctx, runtimeID, at) })
	}
	n, err := r.w.RevokeBotRuntime(ctx, sqlitedb.RevokeBotRuntimeParams{ID: runtimeID, RevokedAt: nullInt64(&at)})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		if _, getErr := r.r.GetBotRuntime(ctx, sqlitedb.GetBotRuntimeParams{ID: runtimeID}); getErr != nil {
			return wrap(getErr)
		}
		return store.ErrConflict
	}
	_, err = r.w.DeleteBotClaimsByRuntime(ctx, sqlitedb.DeleteBotClaimsByRuntimeParams{RuntimeID: runtimeID})
	return wrap(err)
}

func (r *Repo) ListBotRuntimes(ctx context.Context, after id.ID, limit int32) ([]store.BotRuntimeRow, error) {
	if limit <= 0 || limit > 100 {
		return nil, store.ErrConflict
	}
	rows, err := r.r.ListBotRuntimes(ctx, sqlitedb.ListBotRuntimesParams{ID: after, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.BotRuntimeRow, len(rows))
	for i := range rows {
		out[i] = botRuntimeRow(rows[i])
	}
	return out, nil
}

func checkBotInstallation(b store.BotInstallationRow) error {
	if b.State > store.BotInstallationError || b.Revision == 0 || b.Revision > math.MaxInt64 {
		return store.ErrConflict
	}
	if b.RemovedAt != nil && b.State != store.BotInstallationDisabled {
		return store.ErrConflict
	}
	if (b.UserID == nil) != (b.DeviceID == nil) {
		return store.ErrConflict
	}
	if b.Capabilities&^store.BotKnownCapabilities != 0 {
		return store.ErrConflict
	}
	return nil
}

func botInstallationRow(b sqlitedb.BotInstallations) store.BotInstallationRow {
	var version *string
	if b.RuntimeVersion.Valid {
		v := b.RuntimeVersion.String
		version = &v
	}
	return store.BotInstallationRow{ID: b.ID, CommunityID: b.CommunityID, RuntimeID: b.RuntimeID, UserID: b.UserID, DeviceID: b.DeviceID, RoleID: b.RoleID, State: uint8(b.State), Revision: uint64(b.Revision),
		Capabilities: uint16(b.Capabilities) | uint16(b.FeatureCapabilities)<<8, ApprovedBy: b.ApprovedBy, ApprovedAt: b.ApprovedAt, CredentialVerifiedAt: ptrInt64(b.CredentialVerifiedAt), Created: b.Created, Updated: b.Updated, RemovedAt: ptrInt64(b.RemovedAt),
		MusicVolume: uint8(b.MusicVolume), MusicIdleMinutes: uint8(b.MusicIdleMinutes), RestartRequested: uint64(b.RestartRequested), RestartHandled: uint64(b.RestartHandled), RuntimeVersion: version}
}

func botInstallationArgs(b store.BotInstallationRow) sqlitedb.CreateBotInstallationParams {
	return sqlitedb.CreateBotInstallationParams{ID: b.ID, CommunityID: b.CommunityID, RuntimeID: b.RuntimeID, UserID: b.UserID, DeviceID: b.DeviceID, RoleID: b.RoleID, State: int64(b.State), Revision: int64(b.Revision), Capabilities: int64(b.Capabilities & 0xff), ApprovedBy: b.ApprovedBy, ApprovedAt: b.ApprovedAt, CredentialVerifiedAt: nullInt64(b.CredentialVerifiedAt), Created: b.Created, Updated: b.Updated, RemovedAt: nullInt64(b.RemovedAt), FeatureCapabilities: int64(b.Capabilities >> 8)}
}

func (r *Repo) CreateBotInstallation(ctx context.Context, b store.BotInstallationRow) error {
	if err := checkBotInstallation(b); err != nil {
		return err
	}
	return wrap(r.w.CreateBotInstallation(ctx, botInstallationArgs(b)))
}

func (r *Repo) GetBotInstallation(ctx context.Context, id id.ID) (store.BotInstallationRow, error) {
	b, err := r.r.GetBotInstallation(ctx, sqlitedb.GetBotInstallationParams{ID: id})
	if err != nil {
		return store.BotInstallationRow{}, wrap(err)
	}
	return botInstallationRow(b), nil
}

func (r *Repo) GetActiveBotInstallationByCommunity(ctx context.Context, community id.ID) (store.BotInstallationRow, error) {
	b, err := r.r.GetActiveBotInstallationByCommunity(ctx, sqlitedb.GetActiveBotInstallationByCommunityParams{CommunityID: community})
	if err != nil {
		return store.BotInstallationRow{}, wrap(err)
	}
	return botInstallationRow(b), nil
}

func (r *Repo) ListBotInstallationsAfter(ctx context.Context, after id.ID, limit int32) ([]store.BotInstallationRow, error) {
	if limit <= 0 || limit > 100 {
		return nil, store.ErrConflict
	}
	rows, err := r.r.ListBotInstallationsAfter(ctx, sqlitedb.ListBotInstallationsAfterParams{ID: after, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.BotInstallationRow, len(rows))
	for i := range rows {
		out[i] = botInstallationRow(rows[i])
	}
	return out, nil
}

func (r *Repo) ListBotInstallationsForRuntime(ctx context.Context, runtimeID, after id.ID, limit int32) ([]store.BotInstallationRow, error) {
	if limit <= 0 || limit > 100 {
		return nil, store.ErrConflict
	}
	rows, err := r.r.ListBotInstallationsForRuntime(ctx, sqlitedb.ListBotInstallationsForRuntimeParams{
		ID: after, RuntimeID: &runtimeID, MaxRows: int64(limit),
	})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.BotInstallationRow, len(rows))
	for i := range rows {
		out[i] = botInstallationRow(rows[i])
	}
	return out, nil
}

func (r *Repo) ListBotInstallationsByDevice(ctx context.Context, deviceID, after id.ID, limit int32) ([]store.BotInstallationRow, error) {
	if limit <= 0 || limit > 100 {
		return nil, store.ErrConflict
	}
	rows, err := r.r.ListBotInstallationsByDevice(ctx, sqlitedb.ListBotInstallationsByDeviceParams{DeviceID: &deviceID, ID: after, MaxRows: int64(limit)})
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]store.BotInstallationRow, len(rows))
	for i := range rows {
		out[i] = botInstallationRow(rows[i])
	}
	return out, nil
}

func (r *Repo) UpdateBotInstallation(ctx context.Context, installationID id.ID, expected uint64, next store.BotInstallationRow) error {
	if expected == 0 || expected >= math.MaxInt64 || next.Revision != expected+1 || next.ID != installationID {
		return store.ErrConflict
	}
	if next.RemovedAt != nil && !r.inTx {
		return store.ErrConflict
	}
	if err := checkBotInstallation(next); err != nil {
		return err
	}
	old, err := r.GetBotInstallation(ctx, installationID)
	if err != nil {
		return err
	}
	if old.CommunityID != next.CommunityID || old.Created != next.Created || (old.RemovedAt != nil && (next.RemovedAt == nil || *old.RemovedAt != *next.RemovedAt)) {
		return store.ErrConflict
	}
	params := sqlitedb.UpdateBotInstallationParams{RuntimeID: next.RuntimeID, UserID: next.UserID, DeviceID: next.DeviceID, RoleID: next.RoleID, State: int64(next.State), Revision: int64(next.Revision), Capabilities: int64(next.Capabilities & 0xff), ApprovedBy: next.ApprovedBy, ApprovedAt: next.ApprovedAt, CredentialVerifiedAt: nullInt64(next.CredentialVerifiedAt), Updated: next.Updated, RemovedAt: nullInt64(next.RemovedAt), ID: installationID, Revision_2: int64(expected), FeatureCapabilities: int64(next.Capabilities >> 8)}
	n, err := r.w.UpdateBotInstallation(ctx, params)
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		return store.ErrConflict
	}
	if next.RemovedAt != nil {
		if err := r.DeleteBotKeyPackagePublicationReceipts(ctx, installationID); err != nil {
			return err
		}
		if err := r.DeleteBotGroupRegistrationReceiptsByInstallation(ctx, installationID); err != nil {
			return err
		}
		if err := r.DeleteBotSendsByInstallation(ctx, installationID); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo) LockBotInstallation(ctx context.Context, installationID id.ID) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	_, err := r.w.LockBotInstallation(ctx, sqlitedb.LockBotInstallationParams{ID: installationID})
	if err != nil {
		return wrap(err)
	}
	return nil
}

func (r *Repo) LockBotCommunity(ctx context.Context, communityID id.ID) error {
	if !r.inTx {
		return sql.ErrTxDone
	}
	_, err := r.w.LockBotCommunity(ctx, sqlitedb.LockBotCommunityParams{ID: communityID})
	if err != nil {
		return wrap(err)
	}
	return nil
}

func botClaimRow(b sqlitedb.BotClaims) store.BotClaimRow {
	return store.BotClaimRow{InstallationID: b.InstallationID, RuntimeID: b.RuntimeID, ClaimID: b.ClaimID, RequestID: b.RequestID, Expires: b.Expires}
}
func (r *Repo) GetBotClaim(ctx context.Context, installationID id.ID) (store.BotClaimRow, error) {
	b, err := r.r.GetBotClaim(ctx, sqlitedb.GetBotClaimParams{InstallationID: installationID})
	if err != nil {
		return store.BotClaimRow{}, wrap(err)
	}
	return botClaimRow(b), nil
}
func (r *Repo) PutBotClaim(ctx context.Context, b store.BotClaimRow) error {
	return wrap(r.w.PutBotClaim(ctx, sqlitedb.PutBotClaimParams{InstallationID: b.InstallationID, RuntimeID: b.RuntimeID, ClaimID: b.ClaimID, RequestID: b.RequestID, Expires: b.Expires}))
}
func (r *Repo) DeleteBotClaim(ctx context.Context, installationID id.ID) error {
	n, err := r.w.DeleteBotClaim(ctx, sqlitedb.DeleteBotClaimParams{InstallationID: installationID})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}
func (r *Repo) DeleteBotClaimsByRuntime(ctx context.Context, runtimeID id.ID) (int64, error) {
	n, err := r.w.DeleteBotClaimsByRuntime(ctx, sqlitedb.DeleteBotClaimsByRuntimeParams{RuntimeID: runtimeID})
	return n, wrap(err)
}

func botOperationRow(b sqlitedb.BotOperations) store.BotOperationRow {
	return store.BotOperationRow{CommunityID: b.CommunityID, RequestID: b.RequestID, InstallationID: b.InstallationID, ActorKind: uint8(b.ActorKind), ActorID: b.ActorID, ActorDevice: b.ActorDevice, Operation: uint8(b.Operation), PayloadHash: b.PayloadHash, ResultCBOR: b.ResultCbor, Created: b.Created}
}
func (r *Repo) CreateBotOperation(ctx context.Context, b store.BotOperationRow) error {
	if b.ActorKind > 2 || b.Operation < 1 || b.Operation > 8 || len(b.PayloadHash) != 32 || len(b.ResultCBOR) > 8192 {
		return store.ErrConflict
	}
	return wrap(r.w.CreateBotOperation(ctx, sqlitedb.CreateBotOperationParams{CommunityID: b.CommunityID, RequestID: b.RequestID, InstallationID: b.InstallationID, ActorKind: int64(b.ActorKind), ActorID: b.ActorID, ActorDevice: b.ActorDevice, Operation: int64(b.Operation), PayloadHash: b.PayloadHash, ResultCbor: b.ResultCBOR, Created: b.Created}))
}
func (r *Repo) GetBotOperation(ctx context.Context, communityID, requestID id.ID) (store.BotOperationRow, error) {
	b, err := r.r.GetBotOperation(ctx, sqlitedb.GetBotOperationParams{CommunityID: communityID, RequestID: requestID})
	if err != nil {
		return store.BotOperationRow{}, wrap(err)
	}
	return botOperationRow(b), nil
}

func (r *Repo) CreateBotSessionBinding(ctx context.Context, b store.BotSessionBindingRow) error {
	if len(b.TokenHash) != 32 {
		return store.ErrConflict
	}
	return wrap(r.w.CreateBotSessionBinding(ctx, sqlitedb.CreateBotSessionBindingParams{TokenHash: b.TokenHash, InstallationID: b.InstallationID, UserID: b.UserID, DeviceID: b.DeviceID, Created: b.Created}))
}
func (r *Repo) GetBotSessionBinding(ctx context.Context, tokenHash []byte) (store.BotSessionBindingRow, error) {
	if len(tokenHash) != 32 {
		return store.BotSessionBindingRow{}, store.ErrNotFound
	}
	b, err := r.r.GetBotSessionBinding(ctx, sqlitedb.GetBotSessionBindingParams{TokenHash: tokenHash})
	if err != nil {
		return store.BotSessionBindingRow{}, wrap(err)
	}
	return store.BotSessionBindingRow{TokenHash: b.TokenHash, InstallationID: b.InstallationID, UserID: b.UserID, DeviceID: b.DeviceID, Created: b.Created}, nil
}
func (r *Repo) DeleteBotSessionBindingsByInstallation(ctx context.Context, installationID id.ID) (int64, error) {
	n, err := r.w.DeleteBotSessionBindingsByInstallation(ctx, sqlitedb.DeleteBotSessionBindingsByInstallationParams{InstallationID: installationID})
	return n, wrap(err)
}

func (r *Repo) UpdateDeviceCredential(ctx context.Context, deviceID id.ID, credential []byte) error {
	n, err := r.w.UpdateDeviceCredential(ctx, sqlitedb.UpdateDeviceCredentialParams{ID: deviceID, CredentialBlob: credential})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) FinalizeBotDeviceCredential(ctx context.Context, deviceID id.ID, credential []byte, verifiedAt int64) error {
	n, err := r.w.FinalizeBotDeviceCredential(ctx, sqlitedb.FinalizeBotDeviceCredentialParams{
		CredentialBlob: credential, VerifiedAt: nullInt64(&verifiedAt), ID: deviceID,
	})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) SetBotMusicSettings(ctx context.Context, installationID id.ID, volume, idleMinutes uint8) error {
	if !slices.Contains(store.BotMusicVolumes, volume) || !slices.Contains(store.BotMusicIdleMinutes, idleMinutes) {
		return store.ErrConflict
	}
	n, err := r.w.SetBotMusicSettings(ctx, sqlitedb.SetBotMusicSettingsParams{MusicVolume: int64(volume), MusicIdleMinutes: int64(idleMinutes), ID: installationID})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

func (r *Repo) RequestBotRestart(ctx context.Context, installationID id.ID) (uint64, error) {
	n, err := r.w.RequestBotRestart(ctx, sqlitedb.RequestBotRestartParams{ID: installationID})
	if err != nil {
		return 0, wrap(err)
	}
	return uint64(n), nil
}

func (r *Repo) SetBotRuntimeStatus(ctx context.Context, installationID, runtimeID id.ID, version *string, handled uint64) error {
	if handled > math.MaxInt64 || version != nil && (len(*version) == 0 || len(*version) > 32) {
		return store.ErrConflict
	}
	v := sql.NullString{}
	if version != nil {
		v = sql.NullString{String: *version, Valid: true}
	}
	n, err := r.w.SetBotRuntimeStatus(ctx, sqlitedb.SetBotRuntimeStatusParams{RuntimeVersion: v, Handled: int64(handled), ID: installationID, RuntimeID: &runtimeID})
	if err != nil {
		return wrap(err)
	}
	if n != 1 {
		return store.ErrNotFound
	}
	return nil
}

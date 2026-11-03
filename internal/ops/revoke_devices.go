package ops

import (
	"context"
	"fmt"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// RevokeAllResult is what RevokeAllDevices changed: the devices it revoked now (a device revoked
// before keeps its first revoked_at and is not listed) and the sessions it deleted.
type RevokeAllResult struct {
	Revoked  []id.ID
	Sessions int64
}

// RevokeAllDevices is the operator's sign-out of every device of one person (the recovery reset,
// R1): each device that is not yet revoked is revoked, every session of the person is deleted and
// the audit row is written, all in ONE transaction under the user lock, so a device registered
// just before the transaction is revoked with the rest and one registered after it finds the lock
// taken.
//
// Nothing is closed here: like RevokeDevice this runs in a separate `dillad admin` process with no
// gateway handle, and the serving process's liveness tick closes each live socket once its session
// row is gone. The rows are not erased: the devices stay listed with their revoked_at, so the
// audit trail keeps every device the person had.
func RevokeAllDevices(ctx context.Context, repo store.Repository, uid id.ID, now int64) (RevokeAllResult, error) {
	var res RevokeAllResult
	err := repo.Tx(ctx, func(tx store.Repository) error {
		res = RevokeAllResult{}
		if err := tx.LockUserForDeviceRegistration(ctx, uid); err != nil {
			return err
		}
		if _, err := tx.GetUser(ctx, uid); err != nil {
			return err
		}
		rows, err := tx.ListDevicesByUser(ctx, uid)
		if err != nil {
			return err
		}
		for _, d := range rows {
			if d.RevokedAt != nil {
				continue
			}
			if err := tx.RevokeDevice(ctx, d.ID, now); err != nil {
				return err
			}
			res.Revoked = append(res.Revoked, d.ID)
		}
		n, err := tx.DeleteSessionsByUser(ctx, uid)
		if err != nil {
			return err
		}
		res.Sessions = n
		return tx.Audit(ctx, store.AuditRow{
			Action: "device.revoke_all", Target: uid.String(),
			Detail: fmt.Sprintf("%d devices, %d sessions", len(res.Revoked), n), At: now,
		})
	})
	if err != nil {
		return RevokeAllResult{}, fmt.Errorf("ops: revoke every device of %s: %w", uid, err)
	}
	return res, nil
}

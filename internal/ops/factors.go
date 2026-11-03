package ops

import (
	"context"
	"fmt"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// ResetFactors is the operator's reset of one person's second factor (web3.6 S3, protocol/09
// § Admin): their TOTP secret, backup codes and passkeys (under every rp_id) are deleted and
// users.factor_owed is set, in ONE transaction under the user lock, so a factor another device
// confirms just before the transaction is removed with the rest and one confirmed after it
// finds the lock taken. The audit row is written in the same transaction.
//
// Sessions are kept (D5): the person is not signed out. Every device meets E_FACTOR_REQUIRED on
// its next request and sets a factor up before it is used again. After the commit, closer runs
// once for each of the user's devices so a live gateway socket (and a live call) does not
// outlast the reset; a separate `dillad admin` process has no gateway, passes nil and relies on
// the serving process noticing the owed flag on its liveness tick.
func ResetFactors(ctx context.Context, repo store.Repository, closer func(device id.ID), uid id.ID, now int64) error {
	var devices []id.ID
	err := repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockUserForDeviceRegistration(ctx, uid); err != nil {
			return err
		}
		if _, err := tx.GetUser(ctx, uid); err != nil {
			return err
		}
		if err := tx.DeleteTOTP(ctx, uid); err != nil {
			return err
		}
		if err := tx.DeleteRecoveryCodes(ctx, uid); err != nil {
			return err
		}
		if err := tx.DeleteWebauthnCredentialsByUser(ctx, uid); err != nil {
			return err
		}
		if err := tx.SetFactorOwed(ctx, uid, true); err != nil {
			return err
		}
		rows, err := tx.ListDevicesByUser(ctx, uid)
		if err != nil {
			return err
		}
		devices = devices[:0]
		for _, d := range rows {
			devices = append(devices, d.ID)
		}
		return tx.Audit(ctx, store.AuditRow{Action: "user.factors_reset", Target: uid.String(), At: now})
	})
	if err != nil {
		return fmt.Errorf("ops: reset the second factors of %s: %w", uid, err)
	}
	if closer != nil {
		for _, d := range devices {
			closer(d)
		}
	}
	return nil
}

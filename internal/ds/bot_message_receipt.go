package ds

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// BotMessageReceipt returns the immutable receipt for one exact attempt. The
// community comes from the bound enrolled Bot installation; every lookup and
// current access check runs in one transaction, and absence is never recorded.
func (d *DS) BotMessageReceipt(ctx context.Context, session Session, communityID, operationID, attemptID id.ID) (BotMessageResult, error) {
	if session.Scope != auth.ScopeEnrolled || session.Bot == nil || session.Bot.InstallationID.IsZero() || communityID.IsZero() || operationID.IsZero() || attemptID.IsZero() {
		return BotMessageResult{}, errNotFound("send receipt")
	}
	installationID := session.Bot.InstallationID
	var result BotMessageResult
	err := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		installation, _, err := d.lockManagedKeyPackageAuthority(ctx, tx, session, installationID)
		if err != nil {
			return concealBotReceiptAuthorityError(err)
		}
		if installation.State != store.BotInstallationReady || installation.CommunityID != communityID || session.Bot.CommunityID != communityID {
			return errNotFound("send receipt")
		}

		operation, err := tx.LockBotSendOperation(ctx, installationID, operationID)
		if errors.Is(err, store.ErrNotFound) {
			return errNotFound("send receipt")
		}
		if err != nil {
			return err
		}
		if operation.InstallationID != installationID || operation.OperationID != operationID || operation.UserID != session.UserID || operation.DeviceID != session.DeviceID || operation.CommunityID != communityID || operation.GroupID.IsZero() {
			return errNotFound("send receipt")
		}

		if err := tx.LockGroup(ctx, operation.GroupID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errNotFound("send receipt")
			}
			return err
		}
		group, err := tx.GetGroup(ctx, operation.GroupID)
		if errors.Is(err, store.ErrNotFound) {
			return errNotFound("send receipt")
		}
		if err != nil {
			return err
		}
		if group.ClosedAt != nil || group.CommunityID == nil || *group.CommunityID != communityID {
			return errNotFound("send receipt")
		}
		currentLeaf, err := txHasCurrentLeaf(ctx, tx, operation.GroupID, session.DeviceID)
		if err != nil {
			return err
		}
		if !currentLeaf {
			return errNotFound("send receipt")
		}
		if err := d.requireGroupCapabilityTx(ctx, tx, session, group); err != nil {
			return concealBotReceiptAuthorityError(err)
		}

		attempt, err := tx.GetBotSendAttempt(ctx, installationID, operationID, attemptID)
		if errors.Is(err, store.ErrNotFound) {
			return errNotFound("send receipt")
		}
		if err != nil {
			return err
		}
		receipt, err := protocol.DecodeReceipt(attempt.Receipt)
		if err != nil {
			return fmt.Errorf("stored Bot send receipt is invalid: %w", err)
		}
		if attempt.InstallationID != installationID || attempt.OperationID != operationID || attempt.AttemptID != attemptID || receipt.OperationID != operationID || receipt.AttemptID != attemptID || receipt.CommunityID != communityID || receipt.GroupID != operation.GroupID || receipt.ExpectedGeneration != attempt.Generation || receipt.Epoch != attempt.Epoch || receipt.Class != uint64(attempt.Class) || receipt.Outcome > protocol.ReceiptUnknownAfterRestore || uint8(receipt.Outcome) != attempt.Outcome || !sameNullableReceiptID(receipt.PredecessorAttemptID, attempt.PredecessorID) || !bytes.Equal(receipt.CiphertextHash, attempt.CiphertextSHA256) {
			return fmt.Errorf("stored Bot send receipt disagrees with its durable attempt")
		}
		result.Receipt = append([]byte(nil), attempt.Receipt...)
		return nil
	})
	if err != nil {
		return BotMessageResult{}, err
	}
	return result, nil
}

func concealBotReceiptAuthorityError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("send receipt")
	}
	var refusal *Error
	if errors.As(err, &refusal) && refusal.Code == "E_FORBIDDEN" {
		return errNotFound("send receipt")
	}
	return err
}

func sameNullableReceiptID(a, b *id.ID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

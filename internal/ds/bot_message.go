package ds

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// BotMessageRequest is the authenticated managed-Bot send intent. Installation
// identity comes from the session binding and must match InstallationID.
type BotMessageRequest struct {
	Session            Session
	InstallationID     id.ID
	OperationID        id.ID
	AttemptID          id.ID
	PredecessorAttempt *id.ID
	CommunityID        id.ID
	GroupID            id.ID
	ExpectedGeneration uint64
	Epoch              uint64
	Class              uint8
	Ciphertext         []byte
}

// BotMessageResult contains the exact durable twelve-field receipt and whether
// this call created a new accepted message (the only case that notifies clients).
type BotMessageResult struct {
	Receipt       []byte
	NewlyAccepted bool
}

// BotMessageResolveRequest closes or recovers one exact send attempt. Ciphertext
// is deliberately absent; CiphertextSHA256 must match the original intent.
type BotMessageResolveRequest struct {
	Session            Session
	InstallationID     id.ID
	OperationID        id.ID
	AttemptID          id.ID
	PredecessorAttempt *id.ID
	CommunityID        id.ID
	GroupID            id.ID
	ExpectedGeneration uint64
	Epoch              uint64
	Class              uint8
	CiphertextSHA256   []byte
}

// AcceptBotMessage serializes managed sends with commits and ordinary uploads at
// the database group row. The immutable attempt, message, slowmode mark and
// canonical receipt share one transaction. Gateway notification follows commit.
func (d *DS) AcceptBotMessage(ctx context.Context, r BotMessageRequest) (BotMessageResult, error) {
	if err := validateBotMessageRequest(r); err != nil {
		return BotMessageResult{}, err
	}
	if len(r.Ciphertext) > d.opts.Policy.MaxCiphertextBytes {
		return BotMessageResult{}, errTooLarge(len(r.Ciphertext), d.opts.Policy.MaxCiphertextBytes)
	}
	hash := sha256.Sum256(r.Ciphertext)
	fingerprint, err := botSendFingerprint(r.InstallationID, r.Session.UserID, r.Session.DeviceID, r.OperationID, r.AttemptID, r.PredecessorAttempt, r.CommunityID, r.GroupID, r.ExpectedGeneration, r.Epoch, r.Class, hash[:])
	if err != nil {
		return BotMessageResult{}, err
	}
	unlock := d.lock(r.GroupID)
	defer unlock()

	// Native work and SQL use the same pool-before-transaction order as uploads.
	wasm, err := d.opts.Wasm.Acquire(ctx)
	if err != nil {
		return BotMessageResult{}, err
	}
	defer wasm.Release()
	var result BotMessageResult
	var upload UploadResult
	var acceptedDevice id.ID
	err = d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		installation, _, err := d.lockManagedKeyPackageAuthority(ctx, tx, r.Session, r.InstallationID)
		if err != nil {
			return err
		}
		if installation.State != store.BotInstallationReady || installation.CommunityID != r.CommunityID {
			return errForbidden("the managed installation is not ready for sends")
		}
		op := store.BotSendOperationRow{InstallationID: r.InstallationID, OperationID: r.OperationID, UserID: r.Session.UserID, DeviceID: r.Session.DeviceID, CommunityID: r.CommunityID, GroupID: r.GroupID, CurrentAttempt: r.AttemptID, State: uint8(store.BotSendOperationActive)}
		_, inserted, err := tx.CreateBotSendOperationIfAbsent(ctx, op)
		if err != nil {
			return err
		}
		if inserted && r.PredecessorAttempt != nil {
			return botSendConflict("the first attempt must not name a predecessor")
		}
		storedOp, err := tx.LockBotSendOperation(ctx, r.InstallationID, r.OperationID)
		if err != nil {
			return err
		}
		if storedOp.UserID != r.Session.UserID || storedOp.DeviceID != r.Session.DeviceID || storedOp.CommunityID != r.CommunityID || storedOp.GroupID != r.GroupID {
			return botSendConflict("operation ID is already bound to different send scope")
		}
		if err := tx.LockGroup(ctx, r.GroupID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errNotFound("group")
			}
			return err
		}
		group, err := tx.GetGroup(ctx, r.GroupID)
		if errors.Is(err, store.ErrNotFound) {
			return errNotFound("group")
		}
		if err != nil {
			return err
		}
		if group.ClosedAt != nil || group.CommunityID == nil || *group.CommunityID != r.CommunityID {
			return errNotFound("group")
		}
		if group.EpochUnknown {
			return errLeafNotCurrent()
		}
		if group.Kind == 3 {
			registration, registrationErr := tx.GetBotGroupRegistrationReceiptByGroup(ctx, r.GroupID)
			if registrationErr != nil {
				if errors.Is(registrationErr, store.ErrNotFound) {
					return errForbidden("the interaction group is not registered to this Bot installation")
				}
				return registrationErr
			}
			instance, instanceErr := tx.GetInstance(ctx)
			if instanceErr != nil {
				return instanceErr
			}
			if !validCurrentInteractionRegistration(group, registration, d.opts.Keys.InstanceID) ||
				registration.InstallationID != r.InstallationID || registration.UserID != r.Session.UserID ||
				registration.DeviceID != r.Session.DeviceID || registration.CommunityID != r.CommunityID ||
				instance.InstanceID != d.opts.Keys.InstanceID {
				return errForbidden("the interaction group registration does not match this Bot session")
			}
		} else if err := d.requireGroupCapabilityTx(ctx, tx, r.Session, group); err != nil {
			return err
		}
		attempt, attemptErr := tx.GetBotSendAttempt(ctx, r.InstallationID, r.OperationID, r.AttemptID)
		if attemptErr == nil {
			if !sameBotAttempt(attempt, r.PredecessorAttempt, r.ExpectedGeneration, r.Epoch, r.Class, hash[:], fingerprint[:]) {
				return botSendConflict("attempt ID is already bound to different send data")
			}
			receipt, err := protocol.DecodeReceipt(attempt.Receipt)
			if err != nil {
				return fmt.Errorf("stored Bot message receipt is invalid: %w", err)
			}
			result.Receipt = append([]byte(nil), attempt.Receipt...)
			if receipt.Outcome == protocol.ReceiptAccepted {
				return nil
			}
			return botSendConflict("the attempt was already resolved")
		}
		if !errors.Is(attemptErr, store.ErrNotFound) {
			return attemptErr
		}
		if storedOp.State != uint8(store.BotSendOperationActive) {
			return botSendConflict("the send operation is already terminal")
		}
		if storedOp.CurrentAttempt != r.AttemptID {
			if r.PredecessorAttempt == nil || *r.PredecessorAttempt != storedOp.CurrentAttempt {
				return botSendConflict("attempt is not the current operation attempt")
			}
			predecessor, err := tx.GetBotSendAttempt(ctx, r.InstallationID, r.OperationID, *r.PredecessorAttempt)
			if err != nil || predecessor.Outcome != uint8(store.BotSendAttemptClosed) {
				return botSendConflict("predecessor is missing or not closed")
			}
		} else if r.PredecessorAttempt != nil {
			return botSendConflict("the first attempt must not name a predecessor")
		}
		instance, err := tx.GetInstance(ctx)
		if err != nil {
			return err
		}
		if instance.Generation != r.ExpectedGeneration {
			return botSendConflict(fmt.Sprintf("expected generation %d, current generation %d", r.ExpectedGeneration, instance.Generation))
		}
		if group.Epoch != r.Epoch {
			return errCommitInvalid("epoch", "the message's epoch is not the group's")
		}
		if err := requireNoFreezeTx(ctx, tx, group, uint64(d.opts.Policy.CommitDeadline.Milliseconds())); err != nil { //nolint:gosec // G115: normalisePolicy makes this duration positive.
			return err
		}
		currentLeaf, err := txHasCurrentLeaf(ctx, tx, r.GroupID, r.Session.DeviceID)
		if err != nil {
			return err
		}
		if !currentLeaf {
			return errLeafNotCurrent()
		}
		if err := d.requireGroupCapabilityTx(ctx, tx, r.Session, group); err != nil {
			return err
		}
		recvAt := d.now()
		if policy, ok := d.opts.PostPolicy.(TransactionalPostPolicy); ok {
			if err := policy.CheckTx(ctx, tx, group, r.Session.UserID, r.Class, recvAt); err != nil {
				return err
			}
		} else if group.Kind == groupKindText && group.CommunityID != nil {
			return errors.New("managed Bot message acceptance requires transaction-scoped post policy")
		}
		meta, err := wasm.PrivateMessageAAD(ctx, r.Ciphertext)
		if err != nil {
			return errCommitmentInvalid(0)
		}
		if len(meta.AuthenticatedData) != 32 {
			return errCommitmentInvalid(len(meta.AuthenticatedData))
		}
		if meta.ContentType != contentTypeApplication {
			return errCommitInvalid("content_type", "only application messages may be uploaded here")
		}
		if meta.Epoch != group.Epoch {
			return errCommitInvalid("epoch", "the message's epoch is not the group's")
		}
		seq, err := nextSeq(ctx, tx, r.GroupID)
		if err != nil {
			return err
		}
		recvTS := uint64(recvAt) //nolint:gosec // G115: a server unix timestamp
		tag := FrankingTag(d.opts.Keys.FrankingKey, r.GroupID, group.Epoch, seq, r.Session.DeviceID, meta.AuthenticatedData, recvTS)
		responseBody, err := cborx.Marshal([]any{seq, tag, recvTS})
		if err != nil {
			return err
		}
		upload = UploadResult{Seq: seq, Epoch: group.Epoch, FrankingTag: tag, RecvTS: recvTS}
		if err := tx.PutAppMessage(ctx, store.AppMessageRow{GroupID: r.GroupID, Seq: seq, Epoch: group.Epoch, UploaderDevice: r.Session.DeviceID, Blob: r.Ciphertext, CommitmentC: meta.AuthenticatedData, FrankingTag: tag, Size: uint64(len(r.Ciphertext)), Created: recvAt, Expires: nil, FrankingKeyID: d.opts.Keys.FrankingKeyID, Class: &r.Class}); err != nil {
			return err
		}
		if r.Class == 0 && group.Kind == groupKindText {
			if err := tx.PutSlowmodeMark(ctx, group.TargetID, r.Session.UserID, recvAt); err != nil {
				return err
			}
		}
		accepted := &protocol.OriginalSendResponse{Sequence: seq, FrankingTag: tag, ReceiveTimestamp: recvTS, Body: responseBody}
		receiptBody, err := protocol.EncodeReceipt(protocol.SendReceipt{OperationID: r.OperationID, AttemptID: r.AttemptID, PredecessorAttemptID: r.PredecessorAttempt, CommunityID: r.CommunityID, GroupID: r.GroupID, ExpectedGeneration: r.ExpectedGeneration, CiphertextHash: hash[:], Class: uint64(r.Class), Outcome: protocol.ReceiptAccepted, Epoch: r.Epoch, SendResponse: accepted})
		if err != nil {
			return err
		}
		row := store.BotSendAttemptRow{InstallationID: r.InstallationID, OperationID: r.OperationID, AttemptID: r.AttemptID, PredecessorID: r.PredecessorAttempt, Generation: r.ExpectedGeneration, Epoch: r.Epoch, Class: r.Class, CiphertextSHA256: hash[:], FingerprintSHA256: fingerprint[:], Outcome: uint8(store.BotSendAttemptAccepted), Receipt: receiptBody}
		if err := tx.InsertBotSendAttempt(ctx, row); err != nil {
			return err
		}
		if storedOp.CurrentAttempt != r.AttemptID {
			if err := tx.AdvanceBotSendAttempt(ctx, r.InstallationID, r.OperationID, storedOp.CurrentAttempt, r.AttemptID); err != nil {
				return err
			}
		}
		if err := tx.SealBotSendOperation(ctx, r.InstallationID, r.OperationID, r.AttemptID, store.BotSendOperationAccepted); err != nil {
			return err
		}
		acceptedDevice = r.Session.DeviceID
		result.Receipt = receiptBody
		result.NewlyAccepted = true
		return nil
	})
	if err != nil {
		return BotMessageResult{}, err
	}
	if result.NewlyAccepted {
		payload, err := gateway.MessageCTPayload(upload.Seq, upload.Epoch, acceptedDevice, r.Ciphertext, upload.FrankingTag, upload.RecvTS, &r.Class)
		if err == nil && d.opts.Gateway != nil {
			d.opts.Gateway.DeliverGroup(r.GroupID, gateway.Frame{Op: gateway.OpMessageCT, GroupID: &r.GroupID, Payload: payload, Replay: true})
		}
	}
	return result, nil
}

// ResolveBotMessage consults the immutable receipt first. A missing attempt is
// durably closed at the current generation, or marked unknown after restore.
func (d *DS) ResolveBotMessage(ctx context.Context, r BotMessageResolveRequest) (BotMessageResult, error) {
	if err := validateBotResolveRequest(r); err != nil {
		return BotMessageResult{}, err
	}
	fingerprint, err := botSendFingerprint(r.InstallationID, r.Session.UserID, r.Session.DeviceID, r.OperationID, r.AttemptID, r.PredecessorAttempt, r.CommunityID, r.GroupID, r.ExpectedGeneration, r.Epoch, r.Class, r.CiphertextSHA256)
	if err != nil {
		return BotMessageResult{}, err
	}
	var result BotMessageResult
	err = d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		installation, _, err := d.lockManagedKeyPackageAuthority(ctx, tx, r.Session, r.InstallationID)
		if err != nil {
			return err
		}
		if installation.CommunityID != r.CommunityID || installation.State != store.BotInstallationReady {
			return errForbidden("the managed installation is not ready for sends")
		}
		operation := store.BotSendOperationRow{InstallationID: r.InstallationID, OperationID: r.OperationID, UserID: r.Session.UserID, DeviceID: r.Session.DeviceID, CommunityID: r.CommunityID, GroupID: r.GroupID, CurrentAttempt: r.AttemptID, State: uint8(store.BotSendOperationActive)}
		_, inserted, err := tx.CreateBotSendOperationIfAbsent(ctx, operation)
		if err != nil {
			return err
		}
		storedOp, err := tx.LockBotSendOperation(ctx, r.InstallationID, r.OperationID)
		if err != nil {
			return err
		}
		if storedOp.UserID != r.Session.UserID || storedOp.DeviceID != r.Session.DeviceID || storedOp.CommunityID != r.CommunityID || storedOp.GroupID != r.GroupID {
			return botSendConflict("operation ID is already bound to different send scope")
		}
		if err := tx.LockGroup(ctx, r.GroupID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errNotFound("group")
			}
			return err
		}
		group, err := tx.GetGroup(ctx, r.GroupID)
		if err != nil {
			return err
		}
		if group.ClosedAt != nil || group.CommunityID == nil || *group.CommunityID != r.CommunityID {
			return errNotFound("group")
		}
		currentLeaf, err := txHasCurrentLeaf(ctx, tx, r.GroupID, r.Session.DeviceID)
		if err != nil {
			return err
		}
		if !currentLeaf {
			return errLeafNotCurrent()
		}
		if err := d.requireGroupCapabilityTx(ctx, tx, r.Session, group); err != nil {
			return err
		}
		attempt, err := tx.GetBotSendAttempt(ctx, r.InstallationID, r.OperationID, r.AttemptID)
		if err == nil {
			if !sameBotAttempt(attempt, r.PredecessorAttempt, r.ExpectedGeneration, r.Epoch, r.Class, r.CiphertextSHA256, fingerprint[:]) {
				return botSendConflict("attempt ID is already bound to different send data")
			}
			result.Receipt = append([]byte(nil), attempt.Receipt...)
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if storedOp.State != uint8(store.BotSendOperationActive) {
			return botSendConflict("the send operation is already terminal")
		}
		if storedOp.CurrentAttempt != r.AttemptID {
			if r.PredecessorAttempt == nil || *r.PredecessorAttempt != storedOp.CurrentAttempt {
				return botSendConflict("attempt is not the current operation attempt")
			}
			prior, err := tx.GetBotSendAttempt(ctx, r.InstallationID, r.OperationID, *r.PredecessorAttempt)
			if err != nil || prior.Outcome != uint8(store.BotSendAttemptClosed) {
				return botSendConflict("predecessor is missing or not closed")
			}
		} else if r.PredecessorAttempt != nil && !inserted {
			return botSendConflict("the stored operation has no matching predecessor attempt")
		}
		instance, err := tx.GetInstance(ctx)
		if err != nil {
			return err
		}
		if r.ExpectedGeneration > instance.Generation {
			return botSendConflict("expected generation is in the future")
		}
		if inserted && r.PredecessorAttempt != nil && r.ExpectedGeneration >= instance.Generation {
			return botSendConflict("an orphan predecessor is valid only after restore")
		}
		outcome := protocol.ReceiptClosedNotAccepted
		storedOutcome := store.BotSendAttemptClosed
		if r.ExpectedGeneration < instance.Generation {
			outcome, storedOutcome = protocol.ReceiptUnknownAfterRestore, store.BotSendAttemptUnknown
		}
		body, err := protocol.EncodeReceipt(protocol.SendReceipt{OperationID: r.OperationID, AttemptID: r.AttemptID, PredecessorAttemptID: r.PredecessorAttempt, CommunityID: r.CommunityID, GroupID: r.GroupID, ExpectedGeneration: r.ExpectedGeneration, CiphertextHash: r.CiphertextSHA256, Class: uint64(r.Class), Outcome: outcome, Epoch: r.Epoch})
		if err != nil {
			return err
		}
		row := store.BotSendAttemptRow{InstallationID: r.InstallationID, OperationID: r.OperationID, AttemptID: r.AttemptID, PredecessorID: r.PredecessorAttempt, Generation: r.ExpectedGeneration, Epoch: r.Epoch, Class: r.Class, CiphertextSHA256: append([]byte(nil), r.CiphertextSHA256...), FingerprintSHA256: fingerprint[:], Outcome: uint8(storedOutcome), Receipt: body}
		if err := tx.InsertBotSendAttempt(ctx, row); err != nil {
			return err
		}
		if storedOp.CurrentAttempt != r.AttemptID {
			if err := tx.AdvanceBotSendAttempt(ctx, r.InstallationID, r.OperationID, storedOp.CurrentAttempt, r.AttemptID); err != nil {
				return err
			}
		}
		if storedOutcome == store.BotSendAttemptUnknown {
			if err := tx.SealBotSendOperation(ctx, r.InstallationID, r.OperationID, r.AttemptID, store.BotSendOperationUncertain); err != nil {
				return err
			}
		}
		result.Receipt = body
		return nil
	})
	if err != nil {
		return BotMessageResult{}, err
	}
	return result, nil
}

func validateBotMessageRequest(r BotMessageRequest) error {
	if r.Session.Scope != auth.ScopeEnrolled || r.Session.Bot == nil || r.Session.Bot.InstallationID != r.InstallationID || r.InstallationID.IsZero() || r.OperationID.IsZero() || r.AttemptID.IsZero() || r.CommunityID.IsZero() || r.GroupID.IsZero() || r.Class > 1 || r.ExpectedGeneration > math.MaxInt64 || r.Epoch > math.MaxInt64 || len(r.Ciphertext) == 0 {
		return errInvalid("invalid managed Bot message intent")
	}
	if r.PredecessorAttempt != nil && (r.PredecessorAttempt.IsZero() || *r.PredecessorAttempt == r.AttemptID) {
		return errInvalid("invalid predecessor attempt")
	}
	return nil
}

func validateBotResolveRequest(r BotMessageResolveRequest) error {
	if r.Session.Scope != auth.ScopeEnrolled || r.Session.Bot == nil || r.Session.Bot.InstallationID != r.InstallationID || r.InstallationID.IsZero() || r.OperationID.IsZero() || r.AttemptID.IsZero() || r.CommunityID.IsZero() || r.GroupID.IsZero() || r.Class > 1 || r.ExpectedGeneration > math.MaxInt64 || r.Epoch > math.MaxInt64 || len(r.CiphertextSHA256) != sha256.Size {
		return errInvalid("invalid managed Bot message resolution")
	}
	if r.PredecessorAttempt != nil && (r.PredecessorAttempt.IsZero() || *r.PredecessorAttempt == r.AttemptID) {
		return errInvalid("invalid predecessor attempt")
	}
	return nil
}

func botSendFingerprint(installation, user, device, operation, attempt id.ID, predecessor *id.ID, community, group id.ID, generation, epoch uint64, class uint8, ciphertextHash []byte) ([32]byte, error) {
	var zero [32]byte
	if installation.IsZero() || user.IsZero() || device.IsZero() || operation.IsZero() || attempt.IsZero() || community.IsZero() || group.IsZero() || class > 1 || generation > math.MaxInt64 || epoch > math.MaxInt64 || len(ciphertextHash) != sha256.Size || predecessor != nil && (predecessor.IsZero() || *predecessor == attempt) {
		return zero, errInvalid("invalid managed Bot message fingerprint")
	}
	var prior any
	if predecessor != nil {
		prior = *predecessor
	}
	canonical, err := cborx.Marshal([]any{uint64(1), installation, user, device, operation, attempt, prior, community, group, generation, epoch, uint64(class), append([]byte(nil), ciphertextHash...)})
	if err != nil {
		return zero, err
	}
	return sha256.Sum256(canonical), nil
}

func sameBotAttempt(row store.BotSendAttemptRow, predecessor *id.ID, generation, epoch uint64, class uint8, ciphertextHash, fingerprint []byte) bool {
	return row.Generation == generation && row.Epoch == epoch && row.Class == class && equalNullableID(row.PredecessorID, predecessor) && bytes.Equal(row.CiphertextSHA256, ciphertextHash) && bytes.Equal(row.FingerprintSHA256, fingerprint)
}

func equalNullableID(a, b *id.ID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func requireNoFreezeTx(ctx context.Context, tx store.Repository, group store.GroupRow, deadline uint64) error {
	proposals, err := tx.ListProposals(ctx, group.GroupID, group.Epoch, false)
	if err != nil {
		return err
	}
	refs := make([][]byte, 0, len(proposals))
	for _, proposal := range proposals {
		if proposal.Origin == 0 && proposal.VoidAt == nil {
			refs = append(refs, proposal.Ref)
		}
	}
	if len(refs) > 0 {
		return errCommitRequired(refs, deadline)
	}
	return nil
}

func txHasCurrentLeaf(ctx context.Context, tx store.Repository, group, device id.ID) (bool, error) {
	members, err := tx.ListMembers(ctx, group)
	if err != nil {
		return false, err
	}
	for _, member := range members {
		if member.DeviceID == device && member.RemovedEpoch == nil {
			return true, nil
		}
	}
	return false, nil
}

func botSendConflict(detail string) error {
	return &Error{Code: "E_CONFLICT", Detail: detail, Status: http.StatusConflict}
}

func (d *DS) requireGroupCapabilityTx(ctx context.Context, tx store.Repository, session Session, group store.GroupRow) error {
	return d.opts.BotCapability.RequireGroupTx(ctx, tx, session.WireVersion, group.Kind, group.CommunityID)
}

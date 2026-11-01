package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"net/http"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/id"
)

var ErrSendTerminal = errors.New("bot send attempt is already accepted or unknown")

// ErrSendNotPrepared is a terminal resolve-only result: native cannot replay
// ciphertext for this exact immutable input. It is never a reason to upload a
// replacement or fabricate a receipt; the caller may discard its notification.
var ErrSendNotPrepared = errors.New("bot send has no recoverable prepared ciphertext")

// sendRefusedError marks a definitive resolve refusal after receipt lookup has
// proved this exact attempt has no receipt. Recovery cannot upload a substitute.
type sendRefusedError struct{ cause error }

func (e sendRefusedError) Error() string        { return "bot prepared send resolution refused" }
func (e sendRefusedError) Unwrap() error        { return e.cause }
func (e sendRefusedError) Is(target error) bool { return target == ErrSendTerminal }
func (e sendRefusedError) FailureClass() string { return "send_terminal" }

type sendNotPreparedError struct{ cause error }

func (e sendNotPreparedError) Error() string        { return ErrSendNotPrepared.Error() }
func (e sendNotPreparedError) Unwrap() error        { return e.cause }
func (e sendNotPreparedError) Is(target error) bool { return target == ErrSendNotPrepared }
func (e sendNotPreparedError) FailureClass() string { return "send_not_prepared" }

// SendNative is the bounded native-state and crypto boundary consumed by the
// coordinator. Production passes native.Client; runtime integration tests can
// supply a deterministic fake while exercising the coordinator itself.
type SendNative interface {
	Binding() native.Binding
	InspectGroup(context.Context, id.ID) (native.GroupInfo, error)
	InteractionGroups(context.Context) ([]native.InteractionGroupDescriptor, error)
	PrepareSend(context.Context, id.ID, id.ID, id.ID, []byte, uint64, uint64) (native.PreparedSend, error)
	ConfirmSendReceipt(context.Context, id.ID, []byte) error
}

type sendNative = SendNative

// SendAuthority supplies live instance identity and generation.
type SendAuthority interface {
	DiscoverObserved(context.Context) (client.Discovery, client.ResponseObservation, error)
}

type sendAuthority = SendAuthority

// SendSession is the live managed-bot transport used by SendCoordinator.
// Keeping it structural lets runtime assembly tests exercise the real
// coordinator with a controlled network boundary.
type SendSession interface {
	PostBotMessage(context.Context, id.ID, id.ID, client.BotMessageUpload) (protocol.SendReceipt, error)
	BotMessageReceipt(context.Context, id.ID, id.ID, id.ID, id.ID) (protocol.SendReceipt, error)
	ResolveBotMessage(context.Context, id.ID, id.ID, client.BotMessageResolution) (protocol.SendReceipt, error)
}

type sendSession = SendSession

type interactionSendSession interface {
	IssueBotInteractionAdmission(context.Context, id.ID, id.ID, id.ID, id.ID, id.ID) ([]byte, error)
	BotInteractionContextSnapshot(context.Context, id.ID, id.ID, id.ID, id.ID, id.ID) ([]byte, error)
}

type interactionSendNative interface {
	PrepareInteractionSend(context.Context, native.InteractionGroupExpected, []byte, id.ID, id.ID, []byte, uint64, uint64) (native.PreparedSend, error)
}

// SendCoordinator serializes one bot send against the identity binding and the
// server generation advertised by discovery. Calls with the same input are
// safe to resume after restart because native PrepareSend returns its durable
// ciphertext unchanged.
type SendCoordinator struct {
	native    sendNative
	authority sendAuthority
	binding   native.Binding
}

type SendInput struct {
	OperationID, AttemptID id.ID
	PredecessorAttemptID   *id.ID
	GroupID                id.ID
	RequestBody            []byte
	OriginalNow            uint64
	ExpectedGeneration     uint64
}

func NewSendCoordinator(n *native.Client, authority *client.Client) (*SendCoordinator, error) {
	if n == nil || authority == nil {
		return nil, errors.New("missing send dependency")
	}
	return newSendCoordinator(n, authority)
}

// NewSendCoordinatorWithAdapters is the shared construction path for the
// daemon and tests that provide controlled native/network boundaries.
func NewSendCoordinatorWithAdapters(n SendNative, authority SendAuthority) (*SendCoordinator, error) {
	return newSendCoordinator(n, authority)
}

func newSendCoordinator(n sendNative, authority sendAuthority) (*SendCoordinator, error) {
	if n == nil || authority == nil {
		return nil, errors.New("missing send dependency")
	}
	b := n.Binding()
	if b.Instance.IsZero() || b.Community.IsZero() || b.Installation.IsZero() {
		return nil, errors.New("invalid native send binding")
	}
	return &SendCoordinator{native: n, authority: authority, binding: b}, nil
}

// Send prepares one immutable attempt, then either posts its exact ciphertext
// or recovers the same attempt by receipt lookup and resolution. It never
// creates a replacement attempt automatically.
func (s *SendCoordinator) Send(ctx context.Context, session *client.Session, in SendInput) (protocol.SendReceipt, error) {
	if session == nil {
		return protocol.SendReceipt{}, errors.New("missing send session")
	}
	return s.send(ctx, session, in)
}

// SendAuthorized resumes the same durable send attempt as Send, while checking
// caller supplied authorization after receipt recovery and before any new
// ciphertext is prepared or uploaded.
func (s *SendCoordinator) SendAuthorized(ctx context.Context, session SendSession, in SendInput, preflight func(context.Context) error) (protocol.SendReceipt, error) {
	if session == nil || preflight == nil {
		return protocol.SendReceipt{}, errors.New("missing authorized send prerequisite")
	}
	return s.sendWithPreflight(ctx, session, in, preflight)
}

// ResolvePrepared settles an ordinary-group attempt after its caller loses
// authority. It recovers an existing receipt first, otherwise reconstructs the
// same durable ciphertext and asks the server to close or resolve that attempt.
// It never calls PostBotMessage and never creates a replacement send identity.
func (s *SendCoordinator) ResolvePrepared(ctx context.Context, session SendSession, in SendInput) (protocol.SendReceipt, error) {
	return s.sendAttempt(ctx, session, in, nil, true)
}

func (s *SendCoordinator) send(ctx context.Context, session sendSession, in SendInput) (protocol.SendReceipt, error) {
	return s.sendWithPreflight(ctx, session, in, nil)
}

// sendWithPreflight checks authorization after looking up a durable receipt.
// A receipt already accepted by the server remains authoritative and is
// confirmed unchanged; an attempt with no receipt must pass preflight before
// any native preparation or network send can occur.
func (s *SendCoordinator) sendWithPreflight(ctx context.Context, session sendSession, in SendInput, preflight func(context.Context) error) (protocol.SendReceipt, error) {
	return s.sendAttempt(ctx, session, in, preflight, false)
}

func (s *SendCoordinator) sendAttempt(ctx context.Context, session sendSession, in SendInput, preflight func(context.Context) error, resolveOnly bool) (protocol.SendReceipt, error) {
	if s == nil || session == nil || in.OperationID.IsZero() || in.AttemptID.IsZero() || in.GroupID.IsZero() || in.ExpectedGeneration == 0 || in.ExpectedGeneration > math.MaxInt64 || in.OriginalNow > math.MaxInt64 || len(in.RequestBody) == 0 || in.PredecessorAttemptID != nil && in.PredecessorAttemptID.IsZero() {
		return protocol.SendReceipt{}, errors.New("invalid send input")
	}
	if err := ctx.Err(); err != nil {
		return protocol.SendReceipt{}, err
	}
	discovery, _, err := s.authority.DiscoverObserved(ctx)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	if discovery.InstanceID != s.binding.Instance || discovery.Generation == 0 || discovery.Generation > math.MaxInt64 {
		return protocol.SendReceipt{}, client.ErrInstanceChanged
	}
	// A durable server receipt is authoritative even if the local helper has
	// already sealed its outbox entry and no longer permits PrepareSend.
	receipt, lookupErr := session.BotMessageReceipt(ctx, s.binding.Community, in.OperationID, in.AttemptID, in.GroupID)
	if lookupErr == nil {
		if !receiptMatchesInput(receipt, s.binding.Community, in) {
			return protocol.SendReceipt{}, client.ErrProtocol
		}
		body, encodeErr := protocol.EncodeReceipt(receipt)
		if encodeErr != nil {
			return protocol.SendReceipt{}, encodeErr
		}
		if err = s.native.ConfirmSendReceipt(ctx, in.OperationID, body); err != nil {
			return protocol.SendReceipt{}, err
		}
		if receipt.Outcome == protocol.ReceiptUnknownAfterRestore {
			return receipt, ErrSendTerminal
		}
		return receipt, nil
	}
	if !errors.Is(lookupErr, client.ErrBotSendReceiptNotFound) {
		return protocol.SendReceipt{}, lookupErr
	}
	if resolveOnly {
		// PrepareSend replays the original immutable native attempt. No active
		// group or posting permission is needed to recover or close old ciphertext.
		prepared, err := s.native.PrepareSend(ctx, in.OperationID, in.AttemptID, in.GroupID, in.RequestBody, in.OriginalNow, in.ExpectedGeneration)
		if err != nil {
			var status *helper.StatusError
			// Exact ready ciphertext replays before native's active-group gate. A
			// permanent conflict/not-found here cannot reconstruct this attempt; keep
			// unknown, transport and deadline failures retryable instead.
			if errors.As(err, &status) && (status.Status == 4 || status.Status == 7) {
				return protocol.SendReceipt{}, sendNotPreparedError{cause: err}
			}
			return protocol.SendReceipt{}, err
		}
		upload, resolution, err := preparedSendArtifacts(in, prepared)
		if err != nil {
			return protocol.SendReceipt{}, err
		}
		receipt, err := session.ResolveBotMessage(ctx, s.binding.Community, in.OperationID, resolution)
		if err != nil {
			var refusal *client.APIError
			if errors.As(err, &refusal) && refusal.Outcome == client.OutcomeDefinitive &&
				(refusal.Status == http.StatusNotFound || refusal.Status == http.StatusForbidden || refusal.Code == "E_LEAF_NOT_CURRENT") {
				return protocol.SendReceipt{}, sendRefusedError{cause: err}
			}
			return protocol.SendReceipt{}, err
		}
		return s.confirm(ctx, in.OperationID, receipt, upload, resolution)
	}
	if preflight != nil {
		if err := preflight(ctx); err != nil {
			return protocol.SendReceipt{}, err
		}
	}
	group, interaction, err := s.inspectSendGroup(ctx, in.GroupID)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	if group.ID != in.GroupID || group.State != 2 || group.Community == nil || *group.Community != s.binding.Community || group.Epoch > math.MaxInt64 {
		return protocol.SendReceipt{}, errors.New("native send group does not match identity binding")
	}
	if in.PredecessorAttemptID != nil {
		previous, lookupErr := session.BotMessageReceipt(ctx, s.binding.Community, in.OperationID, *in.PredecessorAttemptID, in.GroupID)
		if lookupErr != nil {
			return protocol.SendReceipt{}, lookupErr
		}
		if previous.AttemptID != *in.PredecessorAttemptID || previous.OperationID != in.OperationID || previous.Outcome != protocol.ReceiptClosedNotAccepted || protocol.ValidateReceiptContext(previous, s.binding.Community, in.GroupID) != nil {
			return protocol.SendReceipt{}, ErrSendTerminal
		}
		previousBody, encodeErr := protocol.EncodeReceipt(previous)
		if encodeErr != nil {
			return protocol.SendReceipt{}, encodeErr
		}
		if err = s.native.ConfirmSendReceipt(ctx, in.OperationID, previousBody); err != nil {
			return protocol.SendReceipt{}, err
		}
	}

	var prepared native.PreparedSend
	if interaction == nil {
		prepared, err = s.native.PrepareSend(ctx, in.OperationID, in.AttemptID, in.GroupID, in.RequestBody, in.OriginalNow, in.ExpectedGeneration)
	} else {
		prepared, err = s.prepareInteractionSend(ctx, session, *interaction, in)
	}
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	upload, resolution, err := preparedSendArtifacts(in, prepared)
	if err != nil {
		return protocol.SendReceipt{}, err
	}

	// A retry after a crash first checks whether the previous process completed
	// the POST and lost its response.
	if discovery.Generation != in.ExpectedGeneration || prepared.Epoch != group.Epoch {
		receipt, err = session.ResolveBotMessage(ctx, s.binding.Community, in.OperationID, resolution)
		if err != nil {
			return protocol.SendReceipt{}, err
		}
		return s.confirm(ctx, in.OperationID, receipt, upload, resolution)
	}
	// Preparation may take substantial time. Recheck authorization at the last
	// local boundary before a fresh upload, including attempts whose ciphertext
	// was prepared by an earlier process and resumed here.
	if preflight != nil {
		if err := preflight(ctx); err != nil {
			return protocol.SendReceipt{}, err
		}
	}

	receipt, postErr := session.PostBotMessage(ctx, s.binding.Community, in.GroupID, upload)
	if postErr == nil {
		return s.confirm(ctx, in.OperationID, receipt, upload, resolution)
	}
	// A definitive rate refusal never entered the upload handler. Keep the
	// prepared attempt pending; receipt resolution would close an unsent attempt.
	if client.IsDeferred(postErr) {
		return protocol.SendReceipt{}, postErr
	}
	// A lost or malformed HTTP result is not evidence of refusal. Recheck the
	// exact attempt, then let the server close/resolve that same ciphertext.
	receipt, lookupErr = session.BotMessageReceipt(ctx, s.binding.Community, in.OperationID, in.AttemptID, in.GroupID)
	if lookupErr == nil {
		return s.confirm(ctx, in.OperationID, receipt, upload, resolution)
	}
	if !errors.Is(lookupErr, client.ErrBotSendReceiptNotFound) {
		return protocol.SendReceipt{}, errors.Join(postErr, lookupErr)
	}
	receipt, err = session.ResolveBotMessage(ctx, s.binding.Community, in.OperationID, resolution)
	if err != nil {
		return protocol.SendReceipt{}, errors.Join(postErr, err)
	}
	return s.confirm(ctx, in.OperationID, receipt, upload, resolution)
}

func preparedSendArtifacts(in SendInput, prepared native.PreparedSend) (client.BotMessageUpload, client.BotMessageResolution, error) {
	if len(prepared.Ciphertext) == 0 || prepared.Epoch > math.MaxInt64 || prepared.Class > 1 {
		return client.BotMessageUpload{}, client.BotMessageResolution{}, errors.New("native helper returned invalid prepared send")
	}
	predecessor := cloneID(in.PredecessorAttemptID)
	upload := client.BotMessageUpload{OperationID: in.OperationID, AttemptID: in.AttemptID, PredecessorAttemptID: predecessor, ExpectedGeneration: in.ExpectedGeneration, Epoch: prepared.Epoch, PrivateMessage: append([]byte(nil), prepared.Ciphertext...), Class: prepared.Class}
	resolution := client.BotMessageResolution{AttemptID: in.AttemptID, PredecessorAttemptID: cloneID(predecessor), GroupID: in.GroupID, ExpectedGeneration: in.ExpectedGeneration, Epoch: prepared.Epoch, Class: prepared.Class, CiphertextSHA256: sha256.Sum256(prepared.Ciphertext)}
	return upload, resolution, nil
}

// Ordinary groups use the generic helper inspection. A managed Bot interaction is listed only by
// its dedicated native descriptor operation; the server still authorizes the actual message POST.
func (s *SendCoordinator) inspectSendGroup(ctx context.Context, groupID id.ID) (native.GroupInfo, *native.InteractionGroupDescriptor, error) {
	group, err := s.native.InspectGroup(ctx, groupID)
	if err == nil {
		return group, nil, nil
	}
	var status *helper.StatusError
	if !errors.As(err, &status) || status.Status != 2 {
		return native.GroupInfo{}, nil, err
	}
	descriptors, listErr := s.native.InteractionGroups(ctx)
	if listErr != nil {
		return native.GroupInfo{}, nil, listErr
	}
	var found *native.InteractionGroupDescriptor
	for i := range descriptors {
		if descriptors[i].Group == groupID {
			if found != nil {
				return native.GroupInfo{}, nil, errors.New("duplicate native interaction send group")
			}
			found = &descriptors[i]
		}
	}
	if found == nil {
		return native.GroupInfo{}, nil, err
	}
	b := found.Binding
	if found.Group.IsZero() || found.State != 2 || found.Epoch == 0 || found.BotDevice.IsZero() ||
		b.Instance != s.binding.Instance || b.Installation != s.binding.Installation || b.Community != s.binding.Community ||
		b.BotUser.IsZero() || b.ParticipantUser.IsZero() || b.ParticipantDevice.IsZero() ||
		b.BotUser == b.ParticipantUser || found.BotDevice == b.ParticipantDevice ||
		b.PolicyVersion == 0 || b.E2EEVersion != 1 || b.MediaVersion != 0 {
		return native.GroupInfo{}, nil, errors.New("native interaction send group does not match installation")
	}
	community := b.Community
	return native.GroupInfo{ID: found.Group, Kind: 3, Community: &community, Target: b.ParticipantUser,
		State: found.State, Epoch: found.Epoch}, found, nil
}

func (s *SendCoordinator) prepareInteractionSend(ctx context.Context, session sendSession, descriptor native.InteractionGroupDescriptor, in SendInput) (native.PreparedSend, error) {
	remote, ok := session.(interactionSendSession)
	if !ok {
		return native.PreparedSend{}, errors.New("private send admission is unavailable")
	}
	local, ok := s.native.(interactionSendNative)
	if !ok {
		return native.PreparedSend{}, errors.New("native private send is unavailable")
	}
	b := descriptor.Binding
	requestID := id.New()
	if _, err := remote.IssueBotInteractionAdmission(ctx, s.binding.Installation, requestID, descriptor.Group, b.ParticipantUser, b.ParticipantDevice); err != nil {
		return native.PreparedSend{}, err
	}
	wire, err := remote.BotInteractionContextSnapshot(ctx, s.binding.Installation, requestID, descriptor.Group, b.ParticipantUser, b.ParticipantDevice)
	if err != nil {
		return native.PreparedSend{}, err
	}
	details, err := interactionSnapshotDetailsFromWire(wire)
	if err != nil || details.Request != requestID || details.Instance != b.Instance || details.Installation != b.Installation || details.Community != b.Community ||
		details.BotUser != b.BotUser || details.BotDevice != descriptor.BotDevice || details.ParticipantUser != b.ParticipantUser || details.ParticipantDevice != b.ParticipantDevice ||
		details.Group != descriptor.Group || details.PolicyVersion != b.PolicyVersion || details.E2EEVersion != b.E2EEVersion || details.MediaVersion != b.MediaVersion {
		return native.PreparedSend{}, errors.New("private send admission does not match the installed group")
	}
	expected := native.InteractionGroupExpected{Instance: b.Instance, Installation: b.Installation, Community: b.Community,
		BotUser: b.BotUser, BotDevice: descriptor.BotDevice, ParticipantUser: b.ParticipantUser, ParticipantDevice: b.ParticipantDevice,
		Group: descriptor.Group, MinimumEligibilityRevision: details.MinimumRevision, ServerNow: details.Now, PolicyVersion: details.PolicyVersion}
	return local.PrepareInteractionSend(ctx, expected, wire, in.OperationID, in.AttemptID, in.RequestBody, in.OriginalNow, in.ExpectedGeneration)
}

func receiptMatchesInput(receipt protocol.SendReceipt, community id.ID, in SendInput) bool {
	if receipt.OperationID != in.OperationID || receipt.AttemptID != in.AttemptID || receipt.ExpectedGeneration != in.ExpectedGeneration || protocol.ValidateReceiptContext(receipt, community, in.GroupID) != nil {
		return false
	}
	if receipt.PredecessorAttemptID == nil {
		return in.PredecessorAttemptID == nil
	}
	return in.PredecessorAttemptID != nil && *receipt.PredecessorAttemptID == *in.PredecessorAttemptID
}

func (s *SendCoordinator) confirm(ctx context.Context, operation id.ID, receipt protocol.SendReceipt, upload client.BotMessageUpload, resolution client.BotMessageResolution) (protocol.SendReceipt, error) {
	if receipt.OperationID != upload.OperationID || receipt.AttemptID != upload.AttemptID || receipt.PredecessorAttemptID != nil && (upload.PredecessorAttemptID == nil || *receipt.PredecessorAttemptID != *upload.PredecessorAttemptID) || receipt.PredecessorAttemptID == nil && upload.PredecessorAttemptID != nil || receipt.ExpectedGeneration != upload.ExpectedGeneration || receipt.Epoch != upload.Epoch || receipt.Class != upload.Class || !equalHash(receipt.CiphertextHash, resolution.CiphertextSHA256) || protocol.ValidateReceiptContext(receipt, s.binding.Community, resolution.GroupID) != nil {
		return protocol.SendReceipt{}, client.ErrProtocol
	}
	if receipt.Outcome == protocol.ReceiptAccepted || receipt.Outcome == protocol.ReceiptUnknownAfterRestore {
		body, err := protocol.EncodeReceipt(receipt)
		if err != nil {
			return protocol.SendReceipt{}, err
		}
		if err = s.native.ConfirmSendReceipt(ctx, operation, body); err != nil {
			return protocol.SendReceipt{}, err
		}
		if receipt.Outcome == protocol.ReceiptUnknownAfterRestore {
			return receipt, ErrSendTerminal
		}
		return receipt, nil
	}
	if receipt.Outcome != protocol.ReceiptClosedNotAccepted {
		return protocol.SendReceipt{}, client.ErrProtocol
	}
	body, err := protocol.EncodeReceipt(receipt)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	if err = s.native.ConfirmSendReceipt(ctx, operation, body); err != nil {
		return protocol.SendReceipt{}, err
	}
	return receipt, nil
}

func cloneID(v *id.ID) *id.ID {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}

func equalHash(got []byte, want [32]byte) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

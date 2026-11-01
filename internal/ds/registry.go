package ds

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sync"

	"github.com/jonasthim/dilla/internal/auth"
	botaccess "github.com/jonasthim/dilla/internal/bot/access"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

type RegisterRequest struct {
	Session     Session
	GroupID     id.ID
	Binding     []byte
	GroupInfo   []byte
	RatchetTree []byte
	// InitialWelcome is present only for managed kind-3 first registration.
	InitialWelcome []byte
}

type RegisterResult struct {
	GroupID id.ID
	NextSeq uint64
}

// BotRegisterRequest is the scoped managed-bot registration intent. RegistrationBody is the exact
// canonical four-field body supplied by the client; its bytes are what the receipt authenticates.
type BotRegisterRequest struct {
	Session            Session
	InstallationID     id.ID
	OperationID        id.ID
	ExpectedGeneration uint64
	AdmissionRequestID id.ID
	RegistrationBody   []byte
	Registration       RegisterRequest
}

// BotInteractionRegisterRequest is the admission-scoped kind-3 registration intent. The
// trusted preflight inputs must come from the server's admission loader, never from request data.
type BotInteractionRegisterRequest struct {
	Session            Session
	InstallationID     id.ID
	OperationID        id.ID
	ExpectedGeneration uint64
	AdmissionRequestID id.ID
	AdmissionID        id.ID
	RegistrationBody   []byte
	Registration       RegisterRequest
	TrustedPreflight   InteractionRegistrationPreflight
}

// BotRegisterReceipt is the durable first-acceptance result returned for new and replayed
// managed-bot registrations.
type BotRegisterReceipt struct {
	OperationID id.ID
	GroupID     id.ID
	BodySHA256  [32]byte
	Generation  uint64
	Epoch       uint64
	NextSeq     uint64
}

type botRegistrationIntent struct {
	receipt store.BotGroupRegistrationReceipt
}

// Register is invariant 1. group_id is 16 bytes chosen by the creating device; the registry
// enforces uniqueness and binds it to the dilla_binding the GroupInfo carries.
func (d *DS) Register(ctx context.Context, r RegisterRequest) (RegisterResult, error) {
	out, _, err := d.register(ctx, r, nil)
	return out, err
}

// RegisterWithBotReceipt admits a managed Bot's first group only through its installation-scoped
// operation. The immutable receipt is coupled to the group, public MLS state and initial members.
// The kind-3 interaction preflight remains an independent fail-closed check: do not use it to
// admit registrations until current authorization, receipt-consumption and restore-recovery
// checks are connected to this transaction.
func (d *DS) RegisterWithBotReceipt(ctx context.Context, r BotRegisterRequest) (BotRegisterReceipt, bool, error) {
	if r.OperationID.IsZero() || r.InstallationID.IsZero() || r.ExpectedGeneration > math.MaxInt64 || len(r.RegistrationBody) == 0 {
		return BotRegisterReceipt{}, false, errInvalid("invalid managed group registration intent")
	}
	if r.Session.Bot == nil || r.Session.Scope != auth.ScopeEnrolled || r.Session.Bot.InstallationID != r.InstallationID {
		return BotRegisterReceipt{}, false, errNotFound("managed group registration")
	}
	r.Registration.Session = r.Session
	canonical, err := cborx.Marshal([]any{r.Registration.GroupID, r.Registration.Binding, r.Registration.GroupInfo, r.Registration.RatchetTree})
	if err != nil || !bytes.Equal(canonical, r.RegistrationBody) {
		return BotRegisterReceipt{}, false, errInvalid("registration_body is not canonical")
	}
	// The admission-aware registration path must run the authenticated MLS roster
	// preflight and consume its issued receipt in the same final transaction as
	// group persistence. Until that path exists, reject both v2 interaction
	// bindings and legacy kind-3-shaped bindings before a receipt lookup or write.
	var interaction interactionBindingV2
	if err := cborx.Unmarshal(r.Registration.Binding, &interaction); err == nil && interaction.Version == 2 && interaction.Kind == 3 {
		return BotRegisterReceipt{}, false, errForbidden("managed Bot interaction registration is unavailable")
	}
	if binding, err := decodeBinding(r.Registration.Binding); err == nil && binding.Kind == 3 {
		return BotRegisterReceipt{}, false, errForbidden("managed Bot interaction registration is unavailable")
	}
	if !r.AdmissionRequestID.IsZero() {
		return BotRegisterReceipt{}, false, errInvalid("admission_request_id is only valid for kind-3 registration")
	}
	domainBody := append([]byte("dilla bot group registration v1\x00"), r.RegistrationBody...)
	hash := sha256.Sum256(domainBody)
	var receipt store.BotGroupRegistrationReceipt
	var found bool
	err = d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		_, _, err := d.lockManagedKeyPackageAuthority(ctx, tx, r.Session, r.InstallationID)
		if err != nil {
			return err
		}
		prior, err := tx.GetBotGroupRegistrationReceipt(ctx, r.InstallationID, r.OperationID)
		if err == nil {
			if prior.UserID != r.Session.UserID || prior.DeviceID != r.Session.DeviceID || prior.CommunityID != r.Session.Bot.CommunityID || prior.GroupID != r.Registration.GroupID || prior.Generation != r.ExpectedGeneration || !equalHash(prior.BodySHA256, hash[:]) {
				return &Error{Code: "E_INVALID_REQUEST", Detail: "operation ID was already accepted with different registration data", Status: http.StatusConflict}
			}
			if _, err := tx.GetGroup(ctx, prior.GroupID); errors.Is(err, store.ErrNotFound) {
				return &Error{Code: "E_INTERNAL", Detail: "registration receipt has no group row", Status: http.StatusInternalServerError}
			} else if err != nil {
				return err
			}
			receipt, found = prior, true
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		instance, err := tx.GetInstance(ctx)
		if err != nil {
			return err
		}
		if instance.Generation != r.ExpectedGeneration {
			return &Error{Code: "E_INVALID_REQUEST", Detail: "expected_generation is stale", Status: http.StatusConflict}
		}
		return nil
	})
	if err != nil {
		return BotRegisterReceipt{}, false, err
	}
	if found {
		return botRegisterReceipt(receipt), false, nil
	}

	intent := &botRegistrationIntent{receipt: store.BotGroupRegistrationReceipt{
		InstallationID: r.InstallationID, OperationID: r.OperationID, UserID: r.Session.UserID,
		DeviceID: r.Session.DeviceID, CommunityID: r.Session.Bot.CommunityID, GroupID: r.Registration.GroupID,
		BodySHA256: hash[:], Generation: r.ExpectedGeneration, NextSeq: 1, AcceptedAt: d.now(),
	}}
	_, persisted, err := d.register(ctx, r.Registration, intent)
	if err != nil {
		// A competing DS may have committed the same operation while WASI validated this copy. Read
		// the winner only after the failed transaction has rolled back (especially for PostgreSQL).
		if prior, readErr := d.botRegistrationReceipt(ctx, r); readErr == nil {
			if prior.OperationID != r.OperationID || prior.GroupID != r.Registration.GroupID || prior.Generation != r.ExpectedGeneration || !equalHash(prior.BodySHA256[:], hash[:]) {
				return BotRegisterReceipt{}, false, &Error{Code: "E_INVALID_REQUEST", Detail: "operation ID was already accepted with different registration data", Status: http.StatusConflict}
			}
			return prior, false, nil
		} else if !errors.Is(readErr, store.ErrNotFound) && !isDSNotFound(readErr) {
			return BotRegisterReceipt{}, false, readErr
		}
		return BotRegisterReceipt{}, false, err
	}
	if persisted != nil {
		return botRegisterReceipt(*persisted), false, nil
	}
	return botRegisterReceipt(intent.receipt), true, nil
}

// ReplayBotInteractionGroup checks only the durable receipt pair and current authenticated Bot
// session. It deliberately does not require a live admission proof, so an exact retry can recover
// after expiry or admission-receipt compaction. A conflicting operation or admission reuse fails.
func (d *DS) ReplayBotInteractionGroup(ctx context.Context, r BotInteractionRegisterRequest) (BotRegisterReceipt, bool, error) {
	if d == nil || d.opts.Store == nil || r.OperationID.IsZero() || r.InstallationID.IsZero() || r.AdmissionRequestID.IsZero() ||
		r.ExpectedGeneration == 0 || r.ExpectedGeneration > math.MaxInt64 || len(r.RegistrationBody) == 0 ||
		r.Session.Scope != auth.ScopeEnrolled || r.Session.Bot == nil || r.Session.Bot.InstallationID != r.InstallationID || len(r.Session.TokenHash) == 0 {
		return BotRegisterReceipt{}, false, errInvalid("invalid kind-3 managed registration replay intent")
	}
	canonical, err := cborx.Marshal([]any{r.Registration.GroupID, r.Registration.Binding, r.Registration.GroupInfo, r.Registration.RatchetTree, r.Registration.InitialWelcome})
	if err != nil || !bytes.Equal(canonical, r.RegistrationBody) || len(r.Registration.InitialWelcome) == 0 {
		return BotRegisterReceipt{}, false, errInvalid("registration_body is not canonical")
	}
	bodyHash := sha256.Sum256(append([]byte("dilla bot group registration v1\x00"), r.RegistrationBody...))
	prior, err := d.lookupBotInteractionRegistration(ctx, r, bodyHash)
	if err != nil {
		return BotRegisterReceipt{}, false, err
	}
	if prior == nil {
		return BotRegisterReceipt{}, false, nil
	}
	if err := d.reconcileBotInteractionRegistration(ctx, prior.GroupID); err != nil {
		return BotRegisterReceipt{}, false, err
	}
	return botRegisterReceipt(*prior), true, nil
}

// BotInteractionGroupBinding contains routing hints read from an authenticated GroupContext.
type BotInteractionGroupBinding struct {
	InstanceID          id.ID
	BotUserID           id.ID
	InstallationID      id.ID
	CommunityID         id.ID
	ParticipantUserID   id.ID
	ParticipantDeviceID id.ID
	PolicyVersion       uint64
	E2EEVersion         uint64
	MediaVersion        uint64
}

// AuthenticatedBotInteractionBinding returns routing hints only after importing GroupInfo and its
// ratchet tree through OpenMLS and reading the resulting GroupContext binding. The binding itself
// must be a canonical, complete kind-3 v2 tuple.
func (d *DS) AuthenticatedBotInteractionBinding(ctx context.Context, registration RegisterRequest) (BotInteractionGroupBinding, error) {
	var binding interactionBindingV2
	if d == nil || d.opts.Wasm == nil || registration.GroupID.IsZero() || len(registration.GroupInfo) == 0 || len(registration.RatchetTree) == 0 {
		return BotInteractionGroupBinding{}, errInvalid("invalid kind-3 registration context")
	}
	inst, err := d.opts.Wasm.Acquire(ctx)
	if err != nil {
		return BotInteractionGroupBinding{}, err
	}
	defer inst.Release()
	group, err := inst.PublicGroupFromExternal(ctx, registration.RatchetTree, registration.GroupInfo)
	if err != nil {
		return BotInteractionGroupBinding{}, errBinding("invalid authenticated kind-3 GroupInfo")
	}
	defer func() { _ = group.Close(ctx) }()
	state, err := group.State(ctx)
	if err != nil {
		return BotInteractionGroupBinding{}, err
	}
	if !bytes.Equal(state.GroupID, registration.GroupID[:]) || state.LeafCount != 2 || len(state.Members) != 2 || !bytes.Equal(state.Binding, registration.Binding) {
		return BotInteractionGroupBinding{}, errBinding("authenticated kind-3 GroupContext does not match registration")
	}
	if err := cborx.Unmarshal(state.Binding, &binding); err != nil {
		return BotInteractionGroupBinding{}, errBinding("invalid kind-3 v2 GroupContext binding")
	}
	encoded, err := cborx.Marshal(binding)
	if err != nil || !bytes.Equal(encoded, state.Binding) || binding.Version != 2 || binding.LegacyCommunityID != nil || binding.Kind != 3 ||
		binding.InstanceID.IsZero() || binding.BotUserID.IsZero() || binding.InstallationID.IsZero() || binding.CommunityID.IsZero() ||
		binding.ParticipantUserID.IsZero() || binding.ParticipantDeviceID.IsZero() || binding.PolicyVersion == 0 || binding.E2EEVersion != 1 || binding.MediaVersion != 0 {
		return BotInteractionGroupBinding{}, errBinding("invalid kind-3 v2 GroupContext binding")
	}
	return BotInteractionGroupBinding{InstanceID: binding.InstanceID, BotUserID: binding.BotUserID, InstallationID: binding.InstallationID,
		CommunityID: binding.CommunityID, ParticipantUserID: binding.ParticipantUserID, ParticipantDeviceID: binding.ParticipantDeviceID,
		PolicyVersion: binding.PolicyVersion, E2EEVersion: binding.E2EEVersion, MediaVersion: binding.MediaVersion}, nil
}

// RegisterBotInteractionGroup is the kind-3 atomic acceptance path. Fresh calls must supply
// TrustedPreflight from the server admission loader; RegisterWithBotReceipt retains its explicit
// kind-3 refusal.
func (d *DS) RegisterBotInteractionGroup(ctx context.Context, r BotInteractionRegisterRequest) (BotRegisterReceipt, bool, error) {
	if d == nil || d.opts.Store == nil || d.opts.Wasm == nil || d.opts.BotCapability == nil || d.opts.Clock == nil || d.opts.DeviceLists == nil ||
		r.OperationID.IsZero() || r.InstallationID.IsZero() || r.AdmissionRequestID.IsZero() || r.AdmissionID.IsZero() || r.Registration.GroupID.IsZero() ||
		r.ExpectedGeneration == 0 || r.ExpectedGeneration > math.MaxInt64 || len(r.RegistrationBody) == 0 ||
		r.Session.Scope != auth.ScopeEnrolled || r.Session.Bot == nil || r.Session.Bot.InstallationID != r.InstallationID || len(r.Session.TokenHash) == 0 {
		return BotRegisterReceipt{}, false, errInvalid("invalid kind-3 managed registration intent")
	}
	r.Registration.Session = r.Session
	r.TrustedPreflight.Registration = r.Registration
	canonical, err := cborx.Marshal([]any{r.Registration.GroupID, r.Registration.Binding, r.Registration.GroupInfo, r.Registration.RatchetTree, r.Registration.InitialWelcome})
	if err != nil || !bytes.Equal(canonical, r.RegistrationBody) || len(r.Registration.InitialWelcome) == 0 || len(r.Registration.InitialWelcome) > 65_505 {
		return BotRegisterReceipt{}, false, errInvalid("registration_body is not canonical")
	}
	tuple := r.TrustedPreflight.Expected
	if tuple.InstallationID != r.InstallationID || tuple.BotUserID != r.Session.UserID || tuple.BotDeviceID != r.Session.DeviceID || tuple.GroupID != r.Registration.GroupID || tuple.CommunityID != r.Session.Bot.CommunityID {
		return BotRegisterReceipt{}, false, errInvalid("trusted interaction tuple does not match authenticated registration")
	}
	bodyHash := sha256.Sum256(append([]byte("dilla bot group registration v1\x00"), r.RegistrationBody...))
	unlock := d.lock(r.Registration.GroupID)
	defer unlock()
	defer d.flushEvictions(ctx, r.Registration.GroupID)

	prior, err := d.lookupBotInteractionRegistration(ctx, r, bodyHash)
	if err != nil {
		return BotRegisterReceipt{}, false, err
	}
	if prior != nil {
		if err := d.reconcileBotInteractionRegistration(ctx, r.Registration.GroupID); err != nil {
			return BotRegisterReceipt{}, false, err
		}
		return botRegisterReceipt(*prior), false, nil
	}

	inst, err := d.opts.Wasm.Acquire(ctx)
	if err != nil {
		return BotRegisterReceipt{}, false, err
	}
	release := true
	defer func() {
		if release {
			inst.Release()
		}
	}()
	evidence, err := PrepareKind3PreflightEvidence(ctx, WASIInteractionRegistrationExtractor{Instance: inst}, r.TrustedPreflight)
	if err != nil {
		return BotRegisterReceipt{}, false, err
	}
	group, err := inst.PublicGroupFromExternal(ctx, r.Registration.RatchetTree, r.Registration.GroupInfo)
	if err != nil {
		return BotRegisterReceipt{}, false, errInvalid("group_info or ratchet_tree: " + err.Error())
	}
	closeGroup := true
	defer func() {
		if closeGroup {
			_ = group.Close(ctx)
		}
	}()
	state, err := group.State(ctx)
	if err != nil {
		return BotRegisterReceipt{}, false, err
	}
	var binding interactionBindingV2
	bindingErr := cborx.Unmarshal(r.Registration.Binding, &binding)
	if bindingErr != nil || binding.Version != 2 || binding.Kind != 3 || binding.LegacyCommunityID != nil ||
		binding.InstanceID != tuple.InstanceID || !bytes.Equal(state.Binding, r.Registration.Binding) || !bytes.Equal(state.GroupID, r.Registration.GroupID[:]) ||
		state.LeafCount != 2 || len(state.Members) != 2 {
		return BotRegisterReceipt{}, false, errBinding("authenticated MLS group is not the exact admitted kind-3 initial group")
	}
	if problem := d.externalSendersProblem(3, state.ExternalSenders); problem != "" {
		return BotRegisterReceipt{}, false, errBinding(problem)
	}
	refs, err := inst.WelcomeKeyPackageRefs(ctx, r.Registration.InitialWelcome)
	if err != nil || len(refs) != 1 || len(refs[0]) != 32 {
		return BotRegisterReceipt{}, false, errInvalid("initial Welcome must address exactly one admitted KeyPackage")
	}
	if err := d.opts.BotCapability.RequireGroup(ctx, r.Session.WireVersion, 3, nil); err != nil {
		return BotRegisterReceipt{}, false, err
	}
	registered := RegisteredKind3State{GroupID: r.Registration.GroupID, Binding: append([]byte(nil), state.Binding...), LeafCount: state.LeafCount, Generation: r.ExpectedGeneration, Epoch: state.Epoch, NextSeq: 1}
	communityID := binding.CommunityID
	row := store.GroupRow{GroupID: r.Registration.GroupID, Binding: r.Registration.Binding, Kind: 3, CommunityID: &communityID, TargetID: binding.ParticipantUserID,
		Ciphersuite: 1, Epoch: state.Epoch, Seq: 0, GroupInfoBlob: r.Registration.GroupInfo, TreeHash: state.TreeHash,
		ExternalSenderKeyID: d.opts.Keys.ExternalSenderKeyID, E2EEVersion: binding.E2EEVersion, MediaVersion: binding.MediaVersion,
		PolicyVersion: binding.PolicyVersion, Created: d.now()}
	var members memberView
	var receipt store.BotGroupRegistrationReceipt
	var replay bool
	var welcomeTree, welcomeTreeHash []byte
	err = d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		verified, err := d.ValidateKind3RegistrationAuthority(ctx, tx, inst, r.Session, r.InstallationID, r.AdmissionRequestID, r.AdmissionID,
			r.OperationID, tuple, r.Registration, r.RegistrationBody, registered, evidence)
		if err != nil {
			return err
		}
		keyPackage, err := tx.GetKeyPackage(ctx, tuple.ParticipantDeviceID, refs[0])
		if err != nil || keyPackage.DeviceID != tuple.ParticipantDeviceID || !bytes.Equal(keyPackage.KPRef, refs[0]) || keyPackage.Expires <= d.now() {
			return errForbidden("initial Welcome does not address the admitted participant KeyPackage")
		}
		prior, getErr := tx.GetBotGroupRegistrationReceipt(ctx, r.InstallationID, r.OperationID)
		if getErr == nil {
			consumed, consumedErr := tx.GetBotGroupRegistrationAdmission(ctx, r.InstallationID, r.AdmissionRequestID)
			if consumedErr != nil || prior.Epoch != 1 || prior.NextSeq != 1 || verifyKind3RegistrationReplay(prior, consumed, kind3RegistrationReceiptIntent(r, bodyHash, 1)) != nil {
				return &Error{Code: "E_INVALID_REQUEST", Detail: "operation or admission request was already accepted with different registration data", Status: http.StatusConflict}
			}
			groupRow, groupErr := tx.GetGroup(ctx, prior.GroupID)
			if groupErr != nil || groupRow.Kind != 3 || !bytes.Equal(groupRow.Binding, r.Registration.Binding) {
				return &Error{Code: "E_INTERNAL", Detail: "accepted kind-3 registration has inconsistent group state", Status: http.StatusInternalServerError}
			}
			receipt, replay = prior, true
			return nil
		}
		if !errors.Is(getErr, store.ErrNotFound) {
			return getErr
		}
		if err := tx.CreateGroup(ctx, row); err != nil {
			return err
		}
		if err := persistState(ctx, tx, r.Registration.GroupID, group, r.Registration.GroupInfo); err != nil {
			return err
		}
		members, err = d.replaceMembersTx(ctx, tx, r.Registration.GroupID, 3, state)
		if err != nil {
			return err
		}
		welcomeTree, welcomeTreeHash, err = d.storeWelcomesTx(ctx, tx, r.Registration.GroupID, state.Epoch, 0, group,
			[]WelcomeFor{{DeviceID: tuple.ParticipantDeviceID, Blob: r.Registration.InitialWelcome}})
		if err != nil {
			return err
		}
		acceptances, ok := tx.(store.BotGroupRegistrationAdmissionReceipts)
		if !ok {
			return errors.New("ds: admission-consumption store contract unavailable")
		}
		receipt, _, err = acceptances.AcceptBotGroupRegistrationAdmission(ctx, r.AdmissionRequestID, verified.Receipt)
		return err
	})
	if err != nil {
		// A competing process may have committed while WASI was validating. Recover only from
		// the exact durable operation/admission mapping; otherwise preserve the original failure.
		prior, replayErr := d.lookupBotInteractionRegistration(ctx, r, bodyHash)
		if replayErr != nil {
			return BotRegisterReceipt{}, false, replayErr
		}
		if prior == nil {
			return BotRegisterReceipt{}, false, err
		}
		if err := d.reconcileBotInteractionRegistration(ctx, r.Registration.GroupID); err != nil {
			return BotRegisterReceipt{}, false, err
		}
		return botRegisterReceipt(*prior), false, nil
	}
	if replay {
		if err := d.reconcileBotInteractionRegistration(ctx, r.Registration.GroupID); err != nil {
			return BotRegisterReceipt{}, false, err
		}
		return botRegisterReceipt(receipt), false, nil
	}
	d.queueEviction(r.Registration.GroupID, members.removed)
	if d.opts.Gateway != nil {
		d.opts.Gateway.SetGroupMembers(r.Registration.GroupID, members.devices)
		d.opts.Gateway.SetGroupLeaves(r.Registration.GroupID, members.leaves)
	}
	if err := d.states.put(ctx, r.Registration.GroupID, inst, group); err != nil {
		// The acceptance is durable. Returning an error makes the caller retry; that exact retry
		// rehydrates the cache and member view from the committed rows before returning the receipt.
		return BotRegisterReceipt{}, false, err
	}
	release, closeGroup = false, false
	// The durable queue is authoritative; notification failure never invalidates registration.
	d.fanOutWelcomes(ctx, r.Registration.GroupID, state.Epoch, welcomeTree, welcomeTreeHash,
		[]WelcomeFor{{DeviceID: tuple.ParticipantDeviceID, Blob: r.Registration.InitialWelcome}})
	return botRegisterReceipt(receipt), true, nil
}

func kind3RegistrationReceiptIntent(r BotInteractionRegisterRequest, bodyHash [32]byte, epoch uint64) store.BotGroupRegistrationReceipt {
	return store.BotGroupRegistrationReceipt{InstallationID: r.InstallationID, OperationID: r.OperationID, UserID: r.Session.UserID, DeviceID: r.Session.DeviceID,
		CommunityID: r.Session.Bot.CommunityID, GroupID: r.Registration.GroupID, BodySHA256: bodyHash[:], Generation: r.ExpectedGeneration, Epoch: epoch, NextSeq: 1}
}

func (d *DS) lookupBotInteractionRegistration(ctx context.Context, r BotInteractionRegisterRequest, bodyHash [32]byte) (*store.BotGroupRegistrationReceipt, error) {
	var prior *store.BotGroupRegistrationReceipt
	err := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		installation, device, err := d.lockManagedKeyPackageAuthority(ctx, tx, r.Session, r.InstallationID)
		if err != nil {
			return err
		}
		bound, err := tx.GetBotSessionBinding(ctx, r.Session.TokenHash)
		if err != nil || bound.InstallationID != r.InstallationID || bound.UserID != r.Session.UserID || bound.DeviceID != r.Session.DeviceID || installation.State != r.Session.Bot.State || device.UserID != r.Session.UserID || device.VerifiedAt == nil || device.RevokedAt != nil || device.QuarantinedAt != nil {
			return errForbidden("the authenticated Bot session is no longer current")
		}
		user, err := tx.GetUser(ctx, r.Session.UserID)
		if err != nil || user.Kind != 1 || user.DisabledAt != nil || user.DeletedAt != nil {
			return errForbidden("the managed Bot account is no longer active")
		}
		consumed, consumedErr := tx.GetBotGroupRegistrationAdmission(ctx, r.InstallationID, r.AdmissionRequestID)
		if consumedErr != nil && !errors.Is(consumedErr, store.ErrNotFound) {
			return consumedErr
		}
		operation, opErr := tx.GetBotGroupRegistrationReceipt(ctx, r.InstallationID, r.OperationID)
		if errors.Is(opErr, store.ErrNotFound) {
			if consumedErr == nil {
				return &Error{Code: "E_INVALID_REQUEST", Detail: "admission request was consumed by another operation", Status: http.StatusConflict}
			}
			return nil
		}
		if opErr != nil {
			return opErr
		}
		if consumedErr != nil || operation.Epoch != 1 || operation.NextSeq != 1 || verifyKind3RegistrationReplay(operation, consumed, kind3RegistrationReceiptIntent(r, bodyHash, 1)) != nil {
			return &Error{Code: "E_INVALID_REQUEST", Detail: "operation or admission request was already accepted with different registration data", Status: http.StatusConflict}
		}
		group, err := tx.GetGroup(ctx, operation.GroupID)
		if err != nil {
			return &Error{Code: "E_INTERNAL", Detail: "registration receipt has no group row", Status: http.StatusInternalServerError}
		}
		if group.Kind != 3 || !bytes.Equal(group.Binding, r.Registration.Binding) {
			return &Error{Code: "E_INTERNAL", Detail: "registration receipt group does not match the accepted kind-3 request", Status: http.StatusInternalServerError}
		}
		prior = &operation
		return nil
	})
	return prior, err
}

func (d *DS) reconcileBotInteractionRegistration(ctx context.Context, groupID id.ID) error {
	group, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return err
	}
	if group.Kind != 3 {
		return errBinding("accepted interaction group has unexpected kind")
	}
	if _, err := d.states.acquire(ctx, groupID, func() ([]byte, error) { return group.PublicGroupState, nil }); err != nil {
		return err
	}
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return err
	}
	devices := make([]id.ID, 0, len(members))
	leaves := make(map[id.ID]uint32, len(members))
	for _, member := range members {
		devices = append(devices, member.DeviceID)
		leaves[member.DeviceID] = member.LeafIndex
	}
	if d.opts.Gateway != nil {
		d.opts.Gateway.SetGroupMembers(groupID, devices)
		d.opts.Gateway.SetGroupLeaves(groupID, leaves)
	}
	return nil
}

// BotGroupRegistrationReceipt returns an installation-scoped historical receipt. Current bot
// authority is verified before any operation lookup so an old bearer cannot become a receipt oracle.
func (d *DS) BotGroupRegistrationReceipt(ctx context.Context, s Session, installation, operation id.ID) (BotRegisterReceipt, error) {
	if s.Bot == nil || s.Scope != auth.ScopeEnrolled || s.Bot.InstallationID != installation {
		return BotRegisterReceipt{}, errNotFound("managed group registration receipt")
	}
	var receipt store.BotGroupRegistrationReceipt
	err := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		if _, _, err := d.lockManagedKeyPackageAuthority(ctx, tx, s, installation); err != nil {
			return err
		}
		var err error
		receipt, err = tx.GetBotGroupRegistrationReceipt(ctx, installation, operation)
		if err != nil {
			return err
		}
		if _, err := tx.GetGroup(ctx, receipt.GroupID); errors.Is(err, store.ErrNotFound) {
			return &Error{Code: "E_INTERNAL", Detail: "registration receipt has no group row", Status: http.StatusInternalServerError}
		} else if err != nil {
			return err
		}
		if receipt.UserID != s.UserID || receipt.DeviceID != s.DeviceID || receipt.CommunityID != s.Bot.CommunityID {
			return store.ErrNotFound
		}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return BotRegisterReceipt{}, errNotFound("managed group registration receipt")
	}
	if err != nil {
		return BotRegisterReceipt{}, err
	}
	return botRegisterReceipt(receipt), nil
}

func (d *DS) botRegistrationReceipt(ctx context.Context, r BotRegisterRequest) (BotRegisterReceipt, error) {
	return d.BotGroupRegistrationReceipt(ctx, r.Session, r.InstallationID, r.OperationID)
}

func botRegisterReceipt(r store.BotGroupRegistrationReceipt) BotRegisterReceipt {
	out := BotRegisterReceipt{OperationID: r.OperationID, GroupID: r.GroupID, Generation: r.Generation, Epoch: r.Epoch, NextSeq: r.NextSeq}
	copy(out.BodySHA256[:], r.BodySHA256)
	return out
}

func isDSNotFound(err error) bool {
	var refusal *Error
	return errors.As(err, &refusal) && refusal.Code == "E_NOT_FOUND"
}

func (d *DS) register(ctx context.Context, r RegisterRequest, botIntent *botRegistrationIntent) (RegisterResult, *store.BotGroupRegistrationReceipt, error) {
	defer d.flushEvictions(ctx, r.GroupID) // after the unlock below: defers run last-in, first-out
	unlock := d.lock(r.GroupID)
	defer unlock()

	if _, err := d.opts.Store.GetGroup(ctx, r.GroupID); err == nil {
		return RegisterResult{}, nil, errGroupExists(r.GroupID.String())
	} else if !errors.Is(err, store.ErrNotFound) {
		return RegisterResult{}, nil, err
	}

	inst, err := d.opts.Wasm.Acquire(ctx)
	if err != nil {
		return RegisterResult{}, nil, err
	}
	release := true
	defer func() {
		if release {
			inst.Release()
		}
	}()

	// from_external is correct here and only here: registration is the one moment the DS has no
	// state of its own and the creator uploads the tree (invariant 2).
	group, err := inst.PublicGroupFromExternal(ctx, r.RatchetTree, r.GroupInfo)
	if err != nil {
		return RegisterResult{}, nil, errInvalid("group_info or ratchet_tree: " + err.Error())
	}
	closeGroup := true
	defer func() {
		if closeGroup {
			_ = group.Close(ctx)
		}
	}()
	state, err := group.State(ctx)
	if err != nil {
		return RegisterResult{}, nil, err
	}

	// The body's binding must be byte-identical to extension 0xF001 inside the group context: a
	// client that signs one binding and declares another is exactly the confusion the extension
	// exists to prevent.
	if string(state.Binding) != string(r.Binding) {
		return RegisterResult{}, nil, errBinding("the body's binding is not the group context's dilla_binding")
	}
	if string(state.GroupID) != string(r.GroupID[:]) {
		return RegisterResult{}, nil, errBinding("group_id does not match the GroupInfo's")
	}

	binding, err := decodeBinding(r.Binding)
	if err != nil {
		return RegisterResult{}, nil, errBinding("undecodable dilla_binding: " + err.Error())
	}
	// Invariant 1's instance_id: a group bound to another instance is that instance's group, and
	// registering it here would let a client carry one instance's binding — and every signature
	// over it — into another.
	if binding.InstanceID != d.opts.Keys.InstanceID {
		return RegisterResult{}, nil, errBinding(fmt.Sprintf(
			"dilla_binding names instance %s; this instance is %s", binding.InstanceID, d.opts.Keys.InstanceID))
	}
	// Invariant 1's external senders (DS-MEMBERSHIP-01): the group context names who may propose
	// from outside the group, and the instance governs a text or call group only through that.
	if problem := d.externalSendersProblem(binding.Kind, state.ExternalSenders); problem != "" {
		return RegisterResult{}, nil, errBinding(problem)
	}
	if err := d.checkChannelMode(ctx, binding); err != nil {
		return RegisterResult{}, nil, err
	}
	// The registration ACL runs after the mode rule, so a text group on a readable channel is
	// E_MODE_READABLE whoever registers it, and before anything is written.
	if err := d.checkRegistrant(ctx, r.Session.UserID, binding); err != nil {
		return RegisterResult{}, nil, err
	}
	// One text group per end-to-end-encrypted channel, one call group per voice channel
	// (protocol/01 § Group kinds). The target lock is held until the group row is written, so two
	// first registrations for one target cannot both pass the check; it is always taken after the
	// group lock and nothing takes the two in the other order.
	if isChannelGroupKind(binding.Kind) {
		unlockTarget := d.lockTarget(binding.Kind, binding.TargetID)
		defer unlockTarget()
		if err := d.checkNoLiveGroup(ctx, r.Session.UserID, binding); err != nil {
			return RegisterResult{}, nil, err
		}
	}
	// The tree adopts exactly one leaf, the registering device's own (finding G2). It runs after
	// every refusal above so each keeps its own code, and before anything is written.
	if err := d.checkRegisteredLeaf(ctx, group, r.Session, state); err != nil {
		return RegisterResult{}, nil, err
	}
	// The wire gate uses the binding proved against the signed GroupInfo above and runs only after
	// the target and registrar ACL have succeeded. Interactions require wire 3 even though their
	// bindings intentionally carry no community id. This remains before the first persistent write.
	if err := d.opts.BotCapability.RequireGroup(ctx, r.Session.WireVersion, binding.Kind, binding.CommunityID); err != nil {
		return RegisterResult{}, nil, err
	}

	row := store.GroupRow{
		GroupID: r.GroupID,
		// Deviation B11: the column is a BLOB and holds the binding's own CBOR bytes. It is not
		// re-encoded from the struct: what the group context signed is what is stored.
		Binding:     r.Binding,
		Kind:        binding.Kind,
		CommunityID: binding.CommunityID,
		TargetID:    binding.TargetID,
		// R9 puts the call id in a companion column, not in the binding: `dilla_binding` has no
		// slot for one. A call group's CallID comes from the request's own target, which for
		// GroupKind Call is the call id.
		CallID:              callIDOf(binding),
		Ciphersuite:         1,
		Epoch:               state.Epoch,
		Seq:                 0,
		GroupInfoBlob:       r.GroupInfo,
		TreeHash:            state.TreeHash,
		ExternalSenderKeyID: d.opts.Keys.ExternalSenderKeyID,
		E2EEVersion:         binding.E2EEVersion,
		MediaVersion:        binding.MediaVersion,
		// NV10: the DS records policy_version at registration and does not refuse a stale value;
		// the refusal arrives when policy_version becomes mutable (Plan 2 task 1).
		PolicyVersion: binding.PolicyVersion,
		Created:       d.now(),
	}
	// R12: the group row, its state blob AND its member rows are one transaction. The member
	// write is not deferred until after the commit: `leafOf` is built on `ListMembers`, so a
	// registered group with no member rows refuses every later commit, proposal and upload from
	// its own creator with E_LEAF_NOT_CURRENT. Only the two gateway calls, which are in-memory
	// and idempotent, happen outside it.
	var members memberView
	var replay *store.BotGroupRegistrationReceipt
	err = d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		if botIntent != nil {
			if _, _, err := d.lockManagedKeyPackageAuthority(ctx, tx, r.Session, botIntent.receipt.InstallationID); err != nil {
				return err
			}
			prior, err := tx.GetBotGroupRegistrationReceipt(ctx, botIntent.receipt.InstallationID, botIntent.receipt.OperationID)
			if err == nil {
				if prior.UserID != botIntent.receipt.UserID || prior.DeviceID != botIntent.receipt.DeviceID || prior.CommunityID != botIntent.receipt.CommunityID || prior.GroupID != botIntent.receipt.GroupID || prior.Generation != botIntent.receipt.Generation || !equalHash(prior.BodySHA256, botIntent.receipt.BodySHA256) {
					return &Error{Code: "E_INVALID_REQUEST", Detail: "operation ID was already accepted with different registration data", Status: http.StatusConflict}
				}
				if _, err := tx.GetGroup(ctx, prior.GroupID); errors.Is(err, store.ErrNotFound) {
					return &Error{Code: "E_INTERNAL", Detail: "registration receipt has no group row", Status: http.StatusInternalServerError}
				} else if err != nil {
					return err
				}
				replay = &prior
				return nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			instance, err := tx.GetInstance(ctx)
			if err != nil {
				return err
			}
			if instance.Generation != botIntent.receipt.Generation {
				return &Error{Code: "E_INVALID_REQUEST", Detail: "expected_generation is stale", Status: http.StatusConflict}
			}
		}
		// Community-channel mutations (channel structure, membership, roles and
		// permission overwrites) serialize on this row in the API. Taking the same
		// fence here also serializes human and managed-bot registrations across DS
		// processes, so the final channel/ACL/one-live-group reads below cannot be
		// invalidated before this transaction writes the new group.
		if isChannelGroupKind(binding.Kind) && binding.CommunityID != nil {
			if err := tx.LockCommunity(ctx, *binding.CommunityID); err != nil {
				return err
			}
		}
		// These checks ran before WASI validation so refusals remain early. Repeat
		// them after acquiring the community fence because ACL or target state may
		// have changed while the registration was being validated. This applies to
		// both human and managed-bot registrations.
		if err := d.checkChannelMode(ctx, binding); err != nil {
			return err
		}
		if err := d.checkRegistrant(ctx, r.Session.UserID, binding); err != nil {
			return err
		}
		if isChannelGroupKind(binding.Kind) {
			if err := d.checkNoLiveGroup(ctx, r.Session.UserID, binding); err != nil {
				return err
			}
		}
		if err := tx.CreateGroup(ctx, row); err != nil {
			return err
		}
		if err := persistState(ctx, tx, r.GroupID, group, r.GroupInfo); err != nil {
			return err
		}
		var mErr error
		members, mErr = d.replaceMembersTx(ctx, tx, r.GroupID, binding.Kind, state)
		if mErr != nil {
			return mErr
		}
		if botIntent != nil {
			botIntent.receipt.Epoch = state.Epoch
			if err := tx.CreateBotGroupRegistrationReceipt(ctx, botIntent.receipt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return RegisterResult{}, nil, err
	}
	if replay != nil {
		return RegisterResult{}, replay, nil
	}
	// Always empty for a fresh group; the writer is treated like the other member-set writers.
	d.queueEviction(r.GroupID, members.removed)

	if d.opts.Gateway != nil {
		d.opts.Gateway.SetGroupMembers(r.GroupID, members.devices)
		d.opts.Gateway.SetGroupLeaves(r.GroupID, members.leaves)
	}

	if err := d.states.put(ctx, r.GroupID, inst, group); err != nil {
		return RegisterResult{}, nil, err
	}
	release = false
	closeGroup = false
	return RegisterResult{GroupID: r.GroupID, NextSeq: row.Seq + 1}, nil, nil
}

// checkRegisteredLeaf is what registration adopts (finding G2 of the second hardening review): a
// tree of exactly one leaf, the registering device's own. Every honest client registers the group
// it has just created, before it adds anybody (`DillaGroup::create`, then the register call), so
// every member after the first enters through a commit whose Adds invariant 4 checks, or through an
// external join checkExternalJoiner checks; a registered tree with more leaves would put devices in
// the group that passed neither.
//
// The one leaf passes what an external joiner's leaf passes (checkExternalJoiner): its credential
// names the session's own device and user, its signature key is that device's registered key, the
// device is not revoked, and its key is in the user's newest signed device list. The channel ACL
// half is the registration ACL, which Register has already asked (checkRegistrant: the permission
// invariant 4's ACL resolves too). The count is the tree's own `LeafCount`, not the member list's
// length: the member list leaves out a leaf whose credential is no dilla identity.
//
// Every refusal is E_INVALID_REQUEST, the answer registration already gives a tree or GroupInfo it
// cannot adopt.
func (d *DS) checkRegisteredLeaf(ctx context.Context, v DeviceListVerifier, s Session, state mlswasi.GroupState) error {
	if state.LeafCount != 1 || len(state.Members) != 1 {
		return errInvalid(fmt.Sprintf(
			"a registered group holds exactly one leaf, the registering device's own; this tree holds %d", state.LeafCount))
	}
	leaf := state.Members[0]
	deviceID, userID, err := decodeCredentialIdentity(leaf.CredentialIdentity)
	if err != nil {
		return errInvalid("the registered leaf's credential is not a dilla identity")
	}
	if deviceID != s.DeviceID || userID != s.UserID {
		return errInvalid("the registered leaf must be the registering device's own")
	}
	device, err := d.opts.Store.GetDevice(ctx, deviceID)
	if errors.Is(err, store.ErrNotFound) {
		return errInvalid("the registering device is unknown to this instance")
	}
	if err != nil {
		return err
	}
	if device.UserID != userID {
		return errInvalid("the registered leaf's user does not own the device")
	}
	if device.RevokedAt != nil {
		return errInvalid("the registering device is revoked")
	}
	if !leafKeyIsRegistered(device, leaf.SignatureKey) {
		return errInvalid("the registered leaf's key is not the device's registered key")
	}
	if err := credentialKindVerdict(checkCredentialKind(ctx, d.opts.Store, userID, leaf.CredentialIdentity),
		func(detail string) error { return errInvalid("the registered leaf: " + detail) }); err != nil {
		return err
	}
	if _, err := checkManagedBotCredential(ctx, d.opts.Store, userID, device, leaf.CredentialIdentity, true); err != nil {
		return errInvalid("the registered leaf does not carry the current verified Bot credential")
	}
	entries, err := d.opts.DeviceLists.Entries(ctx, v, userID)
	if err != nil {
		return errInvalid("no verifiable signed device list for the registering user: " + err.Error())
	}
	if auth.Listed(entries, device.ID, device.DSKPub) {
		return nil
	}
	return errInvalid("the registering device's key is not in its user's newest signed device list")
}

// basicCredentialType is RFC 9420's code point for a basic credential, the only kind dilla issues.
const basicCredentialType = 1

// externalSendersProblem is invariant 1's external-sender rule (DS-MEMBERSHIP-01), shared by
// registration and heal's reseed, the two places the delivery service adopts a tree it did not
// build. OpenMLS verifies every external proposal against the group context's external_senders
// extension, so the instance can add to or remove from a group only when the extension names it:
//
//   - a `text` or `call` group carries exactly one entry, this instance's: a basic credential
//     whose identity is CBOR [1, "instance", instance_id] (core/dilla-core/src/mls/binding.rs,
//     instance_credential_identity) over the public half of the key the delivery service signs
//     its proposals with (Keys.ExternalSenderPriv, the key GET /v1/instance publishes as
//     external_sender_pub). A group naming any other key could never be governed: every
//     registration Add, kick, ban, revocation, inactivity and election Remove would fail to queue
//     for the group's life;
//   - a `pairing` or `interaction` group carries none (protocol/01 § External senders).
//
// It answers "" when the rule holds and what is wrong otherwise; each caller wraps that in its own
// refusal.
func (d *DS) externalSendersProblem(kind uint8, senders []mlswasi.ExternalSender) string {
	if !isChannelGroupKind(kind) {
		if len(senders) != 0 {
			return fmt.Sprintf("a group of kind %d carries no external sender; this one names %d", kind, len(senders))
		}
		return ""
	}
	if len(senders) != 1 {
		return fmt.Sprintf(
			"a text or call group names exactly one external sender, this instance; this one names %d", len(senders))
	}
	s := senders[0]
	if s.CredentialType != basicCredentialType || !bytes.Equal(s.Credential, d.instanceCredential) {
		return "the group's external sender is not this instance's \"instance\" credential"
	}
	if !bytes.Equal(s.SignatureKey, d.externalSenderPub) {
		return "the group's external sender key is not this instance's external-sender key"
	}
	return ""
}

// Channels is the sliver of the community structure registration needs: invariant 1's channel
// mode, and the registration ACL. It is an injected interface, not a `store.Repository` call,
// because the delivery service deliberately does not learn the channel vocabulary (kinds,
// visibilities, communities, membership): that lives in internal/api, whose
// `api.StructureChannels` is the production implementation over the `channels`, `communities`
// and `members` tables (Plan 2 task 2, which retired Plan 1's `PermissiveChannels` stub, NV-B5).
// The composition root injects it; a DS built without one refuses every registration.
type Channels interface {
	// Channel returns the channel's visibility and mode, or ErrNoChannel when the target is not a
	// channel at all (a DM or a pairing group has no channel row).
	Channel(ctx context.Context, targetID id.ID) (visibility, mode uint8, err error)
	// MayRegister is the registration ACL (Plan 1 follow-up card 14, "any enrolled device may
	// register any group"): may userID register a group bound by b? nil admits. An error wrapping
	// ErrNotEligible refuses with 403 E_FORBIDDEN, one wrapping ErrBindingTarget with 400
	// E_BINDING_INVALID, and any other error means "the source cannot answer", which Register
	// returns as it is — a refusal, never a pass.
	MayRegister(ctx context.Context, userID id.ID, b Binding) error
}

var (
	// ErrNoChannel is what a Channels implementation returns for a target that is not a channel.
	ErrNoChannel = errors.New("ds: no channel row")
	// ErrNotEligible is MayRegister's "this user may not register this group": not a member of
	// the community the binding names, for instance.
	ErrNotEligible = errors.New("not eligible to register this group")
	// ErrBindingTarget is MayRegister's "the binding names no target this group kind can be
	// registered for": a channel of another community, a text group on a voice channel, or a
	// community text group whose target is not a channel at all.
	ErrBindingTarget = errors.New("the binding names no registrable target")
)

// closedChannels is the channel source of a delivery service built without one: no target is a
// channel, and nobody may register anything. Plan 1 shipped a permissive stub here, because it
// had no channels table to read; the table exists now, so the default is the conservative one,
// as ACL's and DeviceLists' are.
type closedChannels struct{}

func (closedChannels) Channel(context.Context, id.ID) (visibility, mode uint8, err error) {
	return 0, 0, ErrNoChannel
}

func (closedChannels) MayRegister(context.Context, id.ID, Binding) error {
	return fmt.Errorf("%w: this delivery service has no channel source", ErrNotEligible)
}

// checkRegistrant is the registration ACL's call site: it asks the channel source and maps its
// two refusals onto the protocol's codes.
func (d *DS) checkRegistrant(ctx context.Context, userID id.ID, b Binding) error {
	err := d.opts.Channels.MayRegister(ctx, userID, b)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotEligible):
		return errForbidden(err.Error())
	case errors.Is(err, ErrBindingTarget):
		return errBinding(err.Error())
	default:
		return err
	}
}

// GroupRecreation is the optional half of Channels that invariant 11's re-creation path reads:
// "if no heal succeeds … the group is closed and re-created by the channel owner's device". While
// a restore's heal is pending, every open group of the target is epoch-unknown, and MayRecreate
// says whether userID is the one who may register its replacement (nil admits; an error wrapping
// ErrNotEligible refuses; any other error is "cannot answer"). A Channels that does not implement
// it admits no re-creation before the heal window closes the old group.
type GroupRecreation interface {
	MayRecreate(ctx context.Context, userID id.ID, b Binding) error
}

// isChannelGroupKind is true for the two kinds a channel or DM carries one of: text (0) and
// call (1). Pairing and interaction groups are not channel groups.
func isChannelGroupKind(kind uint8) bool { return kind == 0 || kind == 1 }

// checkNoLiveGroup refuses a registration whose target already has an open group of the same kind
// (409 E_GROUP_EXISTS). A second text group forks the channel's end-to-end encryption, and every
// registration starts an Add storm that spends one KeyPackage of every eligible device; a second
// call group takes over the live call, whose call id every call group of the channel shares. The
// one exception is invariant 11's: when every open group of the target is epoch-unknown, the
// registrant the channel source names (GroupRecreation) may re-create it.
func (d *DS) checkNoLiveGroup(ctx context.Context, userID id.ID, b Binding) error {
	groups, err := d.opts.Store.GroupsForTarget(ctx, b.TargetID, b.Kind)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return nil
	}
	for _, g := range groups {
		if !g.EpochUnknown {
			return errGroupExists(fmt.Sprintf(
				"the target already has an open group of this kind (%s); a channel carries one", g.GroupID))
		}
	}
	rc, ok := d.opts.Channels.(GroupRecreation)
	if !ok {
		return errGroupExists("the target's group awaits its heal and this instance names no one to re-create it")
	}
	if err := rc.MayRecreate(ctx, userID, b); err != nil {
		if errors.Is(err, ErrNotEligible) {
			return errGroupExists("the target's group awaits its heal; only the channel owner's device may re-create it")
		}
		return err
	}
	return nil
}

// lockTarget serialises registrations of one kind of group for one target. Its locks live apart
// from the group locks, so a target id that happens to equal some group id cannot contend with,
// or deadlock against, that group's lock.
func (d *DS) lockTarget(kind uint8, target id.ID) func() {
	v, _ := d.targetLocks.LoadOrStore(targetKey{kind: kind, target: target}, &sync.Mutex{})
	// targetLocks only ever holds *sync.Mutex values stored by the line above.
	mu, _ := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

type targetKey struct {
	kind   uint8
	target id.ID
}

// checkChannelMode is invariant 1's refusal. A call group is allowed on any channel; a text group
// is refused when the channel's visibility is invite or discoverable, or its mode is readable.
func (d *DS) checkChannelMode(ctx context.Context, b Binding) error {
	const (
		kindText     = 0
		modeReadable = 1
	)
	if b.Kind != kindText {
		return nil
	}
	visibility, mode, err := d.opts.Channels.Channel(ctx, b.TargetID)
	if errors.Is(err, ErrNoChannel) {
		// DMs and pairing groups have no channel row; only a text group bound to a channel is
		// subject to the mode rule.
		return nil
	}
	if err != nil {
		return err
	}
	if mode == modeReadable || visibility != 0 {
		return errModeReadable()
	}
	return nil
}

type memberView struct {
	devices []id.ID
	leaves  map[id.ID]uint32
	// removed is, for a call group, every device that held a leaf before this write and holds none
	// after it — read before mls_members is rewritten, because a removed device has no row
	// afterwards and a resync's Remove of a device's old leaf is not a removal of the device (G29).
	removed []id.ID
}

// replaceMembersTx rewrites mls_members from the PublicGroup's own view inside the caller's
// transaction and returns the fan-out list and, for a call group (kind), the removed devices. The
// credential identity is the core's ten-element CredentialIdentity CBOR
// (`core/dilla-core/src/identity/credential.rs:28-42`), decoded by decodeCredentialIdentity.
//
// joined names the leaves this write's commit put a device into by external commit (an own-leaf
// resync or an external join): each starts a new membership at state.Epoch, whatever held the
// index before.
func (d *DS) replaceMembersTx(ctx context.Context, tx store.Repository, groupID id.ID, kind uint8, state mlswasi.GroupState, joined ...uint32) (memberView, error) {
	before, err := tx.ListMembers(ctx, groupID)
	if err != nil {
		return memberView{}, err
	}
	// added_epoch is the epoch the device took its leaf, kept across rewrites while the same device
	// holds the same leaf: it is what tells a membership from a later one at the same index
	// (ProposeRemoveOfMember). A device that left and came back starts a new membership.
	since := make(map[uint32]store.MemberRow, len(before))
	for _, m := range before {
		if m.RemovedEpoch == nil {
			since[m.LeafIndex] = m
		}
	}
	rows := make([]store.MemberRow, 0, len(state.Members))
	view := memberView{leaves: map[id.ID]uint32{}}
	for _, m := range state.Members {
		deviceID, userID, err := decodeCredentialIdentity(m.CredentialIdentity)
		if err != nil {
			return memberView{}, errCommitInvalid("credential_identity", err.Error())
		}
		// Member rewrites have historically carried human rows directly from MLS state without
		// requiring their user/device rows to remain registered. Keep that behavior, and apply the
		// verified-device credential fence only when the account is still a managed Bot.
		memberUser, userErr := tx.GetUser(ctx, userID)
		if userErr != nil && !errors.Is(userErr, store.ErrNotFound) {
			return memberView{}, userErr
		}
		if userErr == nil && memberUser.Kind == 1 {
			device, err := tx.GetDevice(ctx, deviceID)
			if err != nil {
				return memberView{}, errCommitInvalid("credential_identity", "the Bot member credential names an unknown device")
			}
			if device.UserID != userID {
				return memberView{}, errCommitInvalid("credential_identity", "the Bot member credential names a device owned by another account")
			}
			if _, err := checkManagedBotCredential(ctx, tx, userID, device, m.CredentialIdentity, false); err != nil {
				return memberView{}, errCommitInvalid("credential_identity", "a managed Bot member differs from its verified device credential")
			}
		}
		added := state.Epoch
		if prev, ok := since[m.LeafIndex]; ok && prev.DeviceID == deviceID && prev.AddedEpoch <= state.Epoch &&
			!slices.Contains(joined, m.LeafIndex) {
			added = prev.AddedEpoch
		}
		rows = append(rows, store.MemberRow{
			GroupID:      groupID,
			LeafIndex:    m.LeafIndex,
			UserID:       userID,
			DeviceID:     deviceID,
			SignatureKey: m.SignatureKey,
			AddedEpoch:   added,
		})
		view.devices = append(view.devices, deviceID)
		view.leaves[deviceID] = m.LeafIndex
	}
	if err := tx.ReplaceMembers(ctx, groupID, state.Epoch, rows); err != nil {
		return memberView{}, err
	}
	for _, m := range before {
		if kind != groupKindCall {
			break
		}
		if _, still := view.leaves[m.DeviceID]; !still && !slices.Contains(view.removed, m.DeviceID) {
			view.removed = append(view.removed, m.DeviceID)
		}
	}
	return view, nil
}

type GroupInfo struct {
	Epoch     uint64
	GroupInfo []byte
	TreeHash  []byte
	NextSeq   uint64
}

// requireMember is the read-path authorisation every group-scoped GET shares. A caller whose
// device is not a current member is answered E_NOT_FOUND, not E_FORBIDDEN, so group existence is
// not probeable: a 403 here would tell any authenticated device on the instance which group ids
// are live.
//
// Without it `info`, `tree`, `handshakes` and `messages` would let any authenticated device read
// another group's ratchet tree, its whole handshake log and its application ciphertext,
// commitments and franking tags — enough to correlate membership and message timing across
// communities. protocol/02 requires a device session on every endpoint in the document and the
// ACL on top.
func (d *DS) requireMember(ctx context.Context, groupID id.ID, session Session) error {
	if err := d.requireMemberOnly(ctx, groupID, session); err != nil {
		return err
	}
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("group")
	}
	if err != nil {
		return err
	}
	return d.requireGroupCapability(ctx, session, row)
}

// requireMemberOnly answers membership without revealing the group, and is kept separate from
// the capability gate so reader-ACL fallback can establish authority before returning E_VERSION.
func (d *DS) requireMemberOnly(ctx context.Context, groupID id.ID, session Session) error {
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.DeviceID == session.DeviceID {
			return nil
		}
	}
	return errNotFound("group")
}

func (d *DS) requireGroupCapability(ctx context.Context, session Session, row store.GroupRow) error {
	return d.opts.BotCapability.RequireGroup(ctx, session.WireVersion, row.Kind, row.CommunityID)
}

// refuseQuarantined is invariant 9's flag, READ: a device three distinct reporters have
// quarantined is removed from the group by an instance Remove, and it must not read its way back
// in — the GroupInfo and the tree are exactly what a rejoin by external commit needs (deviation
// B36) — nor resync itself back into the epoch. E_FORBIDDEN, not E_NOT_FOUND: the answer is about
// the device, which already knows it was quarantined, not about the group. A device the instance
// holds no row for (the fixture's leaves) is not quarantined.
func (d *DS) refuseQuarantined(ctx context.Context, deviceID id.ID) error {
	row, err := d.opts.Store.GetDevice(ctx, deviceID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.QuarantinedAt != nil {
		return errForbidden("this device is quarantined by a fork quorum (invariant 9)")
	}
	return nil
}

// requireReader is requireMember widened by exactly one reader: a device whose user the channel
// ACL admits, which is who joins a text or call group by external commit (protocol/01 § Joining:
// "An online device joins by external commit using the GroupInfo and ratchet tree served by the
// DS"). The GroupInfo and the tree are the two reads such a join needs, so only Info and Tree take
// this path; the handshake log and the ciphertext stay member-only. A device the ACL does not
// admit is E_NOT_FOUND, exactly as a stranger is to requireMember, so existence is still not
// probeable. ds.DenyUnlessMember, Plan 1's ACL, admits only a user already in the group.
func (d *DS) requireReader(ctx context.Context, groupID id.ID, session Session) error {
	if err := d.refuseQuarantined(ctx, session.DeviceID); err != nil {
		return err
	}
	err := d.requireMemberOnly(ctx, groupID, session)
	if err == nil {
		row, rowErr := d.opts.Store.GetGroup(ctx, groupID)
		if rowErr != nil {
			return rowErr
		}
		return d.requireGroupCapability(ctx, session, row)
	}
	if session.Scope != auth.ScopeEnrolled {
		return err
	}
	ok, aclErr := d.opts.ACL.Eligible(ctx, groupID, session.UserID)
	if aclErr != nil || !ok {
		return err
	}
	row, rowErr := d.opts.Store.GetGroup(ctx, groupID)
	if rowErr != nil {
		return rowErr
	}
	return d.requireGroupCapability(ctx, session, row)
}

func (d *DS) Info(ctx context.Context, groupID id.ID, session Session) (GroupInfo, error) {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return GroupInfo{}, errNotFound("group")
	}
	if err != nil {
		return GroupInfo{}, err
	}
	if err := d.requireReader(ctx, groupID, session); err != nil {
		return GroupInfo{}, err
	}
	return GroupInfo{
		Epoch:     row.Epoch,
		GroupInfo: row.GroupInfoBlob,
		TreeHash:  row.TreeHash,
		NextSeq:   row.Seq + 1,
	}, nil
}

type Tree struct {
	Epoch       uint64
	RatchetTree []byte
	TreeHash    []byte
}

// Tree is invariant 2: the DS serves the tree from its own PublicGroup, not from anything a
// committer uploaded.
//
// It takes the group lock like every other path that enters withGroup. Two concurrent
// GET /v1/groups/{id}/tree — the ordinary joiner path — otherwise reach the same
// mlswasi.Instance at once, which the package forbids; the handle lock inside withGroup is the
// second line of defence, and this one keeps the whole read sequence serialised with commits.
func (d *DS) Tree(ctx context.Context, groupID id.ID, session Session) (Tree, error) {
	unlock := d.lock(groupID)
	defer unlock()
	if err := d.requireReader(ctx, groupID, session); err != nil {
		return Tree{}, err
	}
	var out Tree
	err := d.withGroup(ctx, groupID, func(g *mlswasi.PublicGroup) error {
		tree, hash, epoch, err := g.Tree(ctx)
		if err != nil {
			return err
		}
		out = Tree{Epoch: epoch, RatchetTree: tree, TreeHash: hash}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return Tree{}, errNotFound("group")
	}
	return out, err
}

func (d *DS) Close(ctx context.Context, groupID id.ID) error {
	unlock := d.lock(groupID)
	defer unlock()
	if err := d.opts.Store.CloseGroup(ctx, groupID, d.now()); err != nil {
		return err
	}
	return d.states.evict(ctx, groupID)
}

// ErrNotAlone is CloseIfAlone's refusal: the device is not the group's only current non-bot leaf.
var ErrNotAlone = errors.New("ds: the device is not the group's only current non-bot leaf")

// CloseIfAlone is Close on the condition that deviceID holds the group's only current non-bot leaf
// (protocol/09 § Voice, `DELETE /v1/calls/{call_id}?if_alone=1`): any other non-bot member that is not
// removed, a joiner whose external commit has landed included, or no current leaf of deviceID,
// refuses it with ErrNotAlone. The members are read under the group lock that every commit takes,
// so no commit lands between the check and the close; a commit that comes after it finds the
// group closed. A group that is closed already is left as it is (nil).
func (d *DS) CloseIfAlone(ctx context.Context, groupID, deviceID id.ID) error {
	unlock := d.lock(groupID)
	defer unlock()
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return err
	}
	if row.ClosedAt != nil {
		return nil
	}
	if row.EpochUnknown {
		return ErrNotAlone
	}
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return err
	}
	sole, err := SoleLeaf(ctx, d.opts.Store, members, row.Epoch, deviceID)
	if err != nil {
		return err
	}
	if !sole {
		return ErrNotAlone
	}
	if err := d.opts.Store.CloseGroup(ctx, groupID, d.now()); err != nil {
		return err
	}
	return d.states.evict(ctx, groupID)
}

// SoleLeaf reports whether deviceID holds a current leaf of a group at epoch (added at or before
// it, not removed) and no other non-bot member is left in it (CloseIfAlone's rule).
// Unknown accounts refuse the check rather than being assumed to be bots.
func SoleLeaf(ctx context.Context, repo store.Repository, members []store.MemberRow, epoch uint64, deviceID id.ID) (bool, error) {
	sole := false
	for _, m := range members {
		if m.RemovedEpoch != nil {
			continue
		}
		if m.DeviceID != deviceID {
			bot, err := botaccess.IsBotUser(ctx, repo, m.UserID)
			if errors.Is(err, store.ErrNotFound) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if !bot {
				return false, nil
			}
			continue
		}
		if m.AddedEpoch <= epoch {
			sole = true
		}
	}
	return sole, nil
}

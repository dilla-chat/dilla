package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	interactionRegistrationStateArea       = 6
	interactionRegistrationStatePrefix     = "dilla.bot.interaction-registration.v1/"
	interactionRegistrationStateVersion    = 2
	interactionRegistrationInitialEpoch    = 1
	interactionRegistrationInitialSequence = 1
)

var (
	ErrInteractionRegistrationRecovery       = errors.New("bot interaction registration requires recovery")
	ErrInteractionRegistrationOutcomeUnknown = errors.New("bot interaction registration outcome is unknown")
	ErrInteractionRegistrationManualRecovery = errors.New("bot interaction registration outcome is unknown; manual recovery required")
	ErrInteractionNotEligible                = errors.New("member is not eligible for bot interaction registration")
)

type interactionRegistrationNativeAPI interface {
	Binding() native.Binding
	Bootstrap(context.Context, [32]byte) (native.BootstrapResult, error)
	Identity(context.Context) (native.Identity, error)
	CreateInteractionGroup(context.Context, id.ID, native.InteractionGroupExpected, []byte, []byte) (native.InteractionGroupCreateResult, error)
	InspectInteractionGroup(context.Context, native.InteractionGroupExpected, []byte) (native.InteractionGroupInspectResult, error)
	GroupRegistered(context.Context, id.ID, id.ID, uint64) error
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

type interactionRegistrationDiscoveryAPI interface {
	Discover(context.Context) (client.Discovery, error)
}
type interactionRegistrationStateScanAPI interface {
	StateScan(context.Context, uint64, []byte, uint64) (native.StatePage, error)
}
type interactionRegistrationSessionAPI interface {
	IssueBotInteractionAdmission(context.Context, id.ID, id.ID, id.ID, id.ID, id.ID) ([]byte, error)
	BotInteractionContextSnapshot(context.Context, id.ID, id.ID, id.ID, id.ID, id.ID) ([]byte, error)
	RegisterBotGroupWithAdmissionRequest(context.Context, id.ID, id.ID, uint64, []byte, id.ID) (client.BotGroupRegistrationReceipt, error)
	BotGroupRegistrationReceipt(context.Context, id.ID, id.ID) (client.BotGroupRegistrationReceipt, error)
	BotInteractionGroupAbsent(context.Context, id.ID, id.ID) (bool, error)
	GroupInfo(context.Context, id.ID) (client.GroupInfo, error)
	KeyPackage(context.Context, id.ID) ([]byte, error)
}

type InteractionRegistrationCoordinator struct {
	mu                                sync.Mutex
	jobsMu                            sync.Mutex
	instance, community, installation id.ID
	native                            interactionRegistrationNativeAPI
	discovery                         interactionRegistrationDiscoveryAPI
	session                           interactionRegistrationSessionAPI
}

func NewInteractionRegistrationCoordinator(instance, community, installation id.ID, n interactionRegistrationNativeAPI, discovery interactionRegistrationDiscoveryAPI, session interactionRegistrationSessionAPI) (*InteractionRegistrationCoordinator, error) {
	if instance.IsZero() || community.IsZero() || installation.IsZero() || n == nil || discovery == nil || session == nil || n.Binding() != (native.Binding{Instance: instance, Community: community, Installation: installation}) {
		return nil, errors.New("invalid bot interaction registration binding")
	}
	return &InteractionRegistrationCoordinator{instance: instance, community: community, installation: installation, native: n, discovery: discovery, session: session}, nil
}

type InteractionRegistrationRequest struct {
	Group, ParticipantUser, ParticipantDevice id.ID
	MinimumEligibilityRevision                uint64
	ParticipantKeyPackage                     []byte
}

// InteractionRegistrationResult deliberately reports Usable=false. A server
// receipt is not an authenticated initial-Welcome delivery receipt; callers
// must keep the interaction pending until that separate protocol exists.
type InteractionRegistrationResult struct {
	Receipt        client.BotGroupRegistrationReceipt
	PendingWelcome []byte
	Usable         bool
}

// InteractionRegistrationRecoveryDiagnostic is a read-only view of one
// durable intent that has been stopped for operator recovery.
type InteractionRegistrationRecoveryDiagnostic struct {
	Installation       id.ID
	Operation          id.ID
	Group              id.ID
	Generation         uint64
	RegistrationSHA256 [32]byte
	Phase              uint64
	ReceiptStatus      string
	GroupStatus        string
}

type interactionRegistrationIntent struct {
	Operation, AdmissionRequest, Group, ParticipantUser, ParticipantDevice id.ID
	BotUser, BotDevice                                                     id.ID
	MinimumRevision, Policy, Generation                                    uint64
	KeyPackage, Admission, Snapshot, Body, Welcome                         []byte
	Submitted                                                              bool
	ManualRecovery                                                         bool
	Phase                                                                  uint64
	Receipt                                                                client.BotGroupRegistrationReceipt
}

type interactionSnapshotDetails struct {
	Request, Instance, Installation, Community                     id.ID
	BotUser, BotDevice, ParticipantUser, ParticipantDevice, Group  id.ID
	Now, MinimumRevision, PolicyVersion, E2EEVersion, MediaVersion uint64
}

func (c *InteractionRegistrationCoordinator) Register(ctx context.Context, req InteractionRegistrationRequest) (InteractionRegistrationResult, error) {
	if c == nil {
		return InteractionRegistrationResult{}, errors.New("invalid interaction registration request")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if req.Group.IsZero() || req.ParticipantUser.IsZero() || req.ParticipantDevice.IsZero() || req.MinimumEligibilityRevision > math.MaxInt64 || len(req.ParticipantKeyPackage) > 65505 {
		return InteractionRegistrationResult{}, errors.New("invalid interaction registration request")
	}
	d, err := c.discovery.Discover(ctx)
	if err != nil {
		return InteractionRegistrationResult{}, err
	}
	if d.InstanceID != c.instance || d.Generation == 0 || d.Generation > math.MaxInt64 || d.PolicyVersion == 0 || d.PolicyVersion > math.MaxInt64 {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	if len(d.ExternalSenderPub) != 32 || allZeroBytes(d.ExternalSenderPub) {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	if _, err := c.native.Bootstrap(ctx, bytes32(d.ExternalSenderPub)); err != nil {
		return InteractionRegistrationResult{}, err
	}
	identity, err := c.native.Identity(ctx)
	if err != nil || identity.Phase != 2 || identity.Instance == nil || *identity.Instance != c.instance || identity.User == nil || identity.Device == nil || identity.User.IsZero() || identity.Device.IsZero() {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	key := interactionRegistrationKey(req.Group)
	v, err := c.native.StateGet(ctx, interactionRegistrationStateArea, key)
	if err != nil {
		return InteractionRegistrationResult{}, err
	}
	var in interactionRegistrationIntent
	revision := v.Revision
	if v.Tombstone {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	if revision == 0 {
		in = interactionRegistrationIntent{Operation: id.New(), AdmissionRequest: id.New(), Group: req.Group, ParticipantUser: req.ParticipantUser, ParticipantDevice: req.ParticipantDevice, BotUser: *identity.User, BotDevice: *identity.Device, MinimumRevision: req.MinimumEligibilityRevision, Policy: d.PolicyVersion, Generation: d.Generation, KeyPackage: append([]byte(nil), req.ParticipantKeyPackage...)}
		revision, err = c.saveInteractionRegistration(ctx, key, revision, in)
		if err != nil {
			return InteractionRegistrationResult{}, fmt.Errorf("persist interaction intent: %w", err)
		}
	} else {
		in, err = decodeInteractionRegistrationIntent(v.Value)
		if err != nil || in.Group != req.Group || in.ParticipantUser != req.ParticipantUser || in.ParticipantDevice != req.ParticipantDevice || (req.MinimumEligibilityRevision != 0 && in.MinimumRevision != 0 && in.MinimumRevision < req.MinimumEligibilityRevision) || in.BotUser != *identity.User || in.BotDevice != *identity.Device || (len(req.ParticipantKeyPackage) != 0 && !equalBytes(in.KeyPackage, req.ParticipantKeyPackage)) {
			return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
		}
		if in.ManualRecovery {
			return InteractionRegistrationResult{}, ErrInteractionRegistrationManualRecovery
		}
		if in.Submitted && (in.Policy != d.PolicyVersion || in.Generation != d.Generation) {
			in.ManualRecovery = true
			if _, err := c.saveInteractionRegistration(ctx, key, revision, in); err != nil {
				return InteractionRegistrationResult{}, err
			}
			return InteractionRegistrationResult{}, ErrInteractionRegistrationManualRecovery
		}
		if in.Policy != d.PolicyVersion || in.Generation != d.Generation {
			return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
		}
		if in.Phase == 2 {
			return InteractionRegistrationResult{Receipt: in.Receipt, PendingWelcome: append([]byte(nil), in.Welcome...), Usable: false}, nil
		}
	}
	if len(in.Body) == 0 {
		if len(in.Admission) == 0 {
			in.Admission, err = c.session.IssueBotInteractionAdmission(ctx, c.installation, in.AdmissionRequest, in.Group, in.ParticipantUser, in.ParticipantDevice)
			if err != nil {
				var apiErr *client.APIError
				if errors.As(err, &apiErr) && apiErr.Status == 403 {
					return InteractionRegistrationResult{}, fmt.Errorf("%w: %w", ErrInteractionNotEligible, err)
				}
				return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationRecovery, "admission request remains pending", err)
			}
			revision, err = c.saveInteractionRegistration(ctx, key, revision, in)
			if err != nil {
				return InteractionRegistrationResult{}, err
			}
		}
		if len(in.Snapshot) == 0 {
			in.Snapshot, err = c.session.BotInteractionContextSnapshot(ctx, c.installation, in.AdmissionRequest, in.Group, in.ParticipantUser, in.ParticipantDevice)
			if err != nil {
				return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationRecovery, "context snapshot unavailable", err)
			}
			revision, err = c.saveInteractionRegistration(ctx, key, revision, in)
			if err != nil {
				return InteractionRegistrationResult{}, err
			}
		}
		snapshot, err := interactionSnapshotDetailsFromWire(in.Snapshot)
		if err != nil || snapshot.Request != in.AdmissionRequest || snapshot.PolicyVersion != d.PolicyVersion || (in.MinimumRevision != 0 && in.MinimumRevision != snapshot.MinimumRevision) || (req.MinimumEligibilityRevision != 0 && req.MinimumEligibilityRevision > snapshot.MinimumRevision) ||
			snapshot.Instance != c.instance || snapshot.Installation != c.installation || snapshot.Community != c.community || snapshot.BotUser != in.BotUser || snapshot.BotDevice != in.BotDevice || snapshot.ParticipantUser != in.ParticipantUser || snapshot.ParticipantDevice != in.ParticipantDevice || snapshot.Group != in.Group {
			return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
		}
		in.MinimumRevision = snapshot.MinimumRevision
		if len(in.KeyPackage) == 0 {
			in.KeyPackage, err = c.session.KeyPackage(ctx, in.ParticipantDevice)
			if err != nil || len(in.KeyPackage) == 0 || len(in.KeyPackage) > 65505 {
				return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationRecovery, "exact participant KeyPackage unavailable", err)
			}
			revision, err = c.saveInteractionRegistration(ctx, key, revision, in)
			if err != nil {
				return InteractionRegistrationResult{}, err
			}
		}
		expected := native.InteractionGroupExpected{Instance: c.instance, Installation: c.installation, Community: c.community, BotUser: in.BotUser, BotDevice: in.BotDevice, ParticipantUser: in.ParticipantUser, ParticipantDevice: in.ParticipantDevice, Group: in.Group, MinimumEligibilityRevision: in.MinimumRevision, ServerNow: snapshot.Now, PolicyVersion: in.Policy}
		created, err := c.native.CreateInteractionGroup(ctx, in.Operation, expected, in.Snapshot, in.KeyPackage)
		if err != nil {
			var statusErr *helper.StatusError
			if errors.As(err, &statusErr) && statusErr.Status == 3 {
				// The signed admission snapshot is short lived. The helper's
				// definitive expired status guarantees no group was created, so
				// rotate both one-shot IDs while retaining the pinned participants
				// and package. Uncertain helper outcomes never reach this branch.
				in.Operation = id.New()
				in.AdmissionRequest = id.New()
				in.MinimumRevision = 0
				in.Admission = nil
				in.Snapshot = nil
				if _, saveErr := c.saveInteractionRegistration(ctx, key, revision, in); saveErr != nil {
					return InteractionRegistrationResult{}, fmt.Errorf("persist renewed interaction admission: %w", saveErr)
				}
				return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
			}
			return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationRecovery, "native create remains pending", err)
		}
		if !validInteractionCreateResult(created, expected, d) {
			return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
		}
		in.Welcome = append([]byte(nil), created.Welcome...)
		// Preserve the native helper's exact authenticated binding bytes. The
		// versioned kind-3 registration contract adds Welcome as its fifth field.
		in.Body, err = cborx.Marshal([]any{created.Group, created.BindingBody, created.GroupInfo, created.RatchetTree, created.Welcome})
		if err != nil || len(in.Body) == 0 || len(in.Body) > 65505 {
			return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
		}
		revision, err = c.saveInteractionRegistration(ctx, key, revision, in)
		if err != nil {
			return InteractionRegistrationResult{}, fmt.Errorf("persist interaction registration body: %w", err)
		}
	}
	if in.Phase == 1 {
		return c.resolveInteractionReceipt(ctx, key, revision, in, in.Receipt)
	}
	receipt, err := c.session.BotGroupRegistrationReceipt(ctx, c.installation, in.Operation)
	if err == nil {
		return c.acceptInteractionReceipt(ctx, key, revision, in, receipt)
	}
	if !errors.Is(err, client.ErrRegistrationReceiptNotFound) {
		return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationOutcomeUnknown, "receipt lookup failed", err)
	}
	if in.Submitted {
		// A 404 receipt is not a cancellation fence: the original POST may still
		// commit after this read. Preserve its exact native intent and stop here.
		in.ManualRecovery = true
		if _, err := c.saveInteractionRegistration(ctx, key, revision, in); err != nil {
			return InteractionRegistrationResult{}, err
		}
		return InteractionRegistrationResult{}, ErrInteractionRegistrationManualRecovery
	}
	if err := c.proveInteractionRetrySafe(ctx, in); err != nil {
		return InteractionRegistrationResult{}, err
	}
	in.Submitted = true
	revision, err = c.saveInteractionRegistration(ctx, key, revision, in)
	if err != nil {
		return InteractionRegistrationResult{}, err
	}
	receipt, postErr := c.session.RegisterBotGroupWithAdmissionRequest(ctx, c.installation, in.Operation, in.Generation, in.Body, in.AdmissionRequest)
	if postErr == nil {
		return c.acceptInteractionReceipt(ctx, key, revision, in, receipt)
	}
	if client.IsDeferred(postErr) {
		// The middleware did not run registration. Make the exact durable intent
		// retryable instead of treating it as an ambiguously submitted request.
		in.Submitted = false
		_, saveErr := c.saveInteractionRegistration(ctx, key, revision, in)
		return InteractionRegistrationResult{}, errors.Join(postErr, saveErr)
	}
	receipt, err = c.session.BotGroupRegistrationReceipt(ctx, c.installation, in.Operation)
	if err == nil {
		return c.acceptInteractionReceipt(ctx, key, revision, in, receipt)
	}
	if !errors.Is(err, client.ErrRegistrationReceiptNotFound) {
		return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationOutcomeUnknown, "receipt lookup failed", err)
	}
	return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationOutcomeUnknown, "registration response lost", postErr)
}

// InspectInteractionRegistrationRecovery lists manual-recovery intents and
// checks their server receipt and group status without changing native or
// server state. ReceiptStatus is present, absent, mismatch, or unavailable;
// GroupStatus is present, absent, or unavailable.
func (c *InteractionRegistrationCoordinator) InspectInteractionRegistrationRecovery(ctx context.Context) ([]InteractionRegistrationRecoveryDiagnostic, error) {
	if c == nil {
		return nil, errors.New("missing interaction registration coordinator")
	}
	scanner, ok := c.native.(interactionRegistrationStateScanAPI)
	if !ok {
		return nil, errors.New("native state scanning is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []InteractionRegistrationRecoveryDiagnostic
	var after []byte
	for {
		page, err := scanner.StateScan(ctx, interactionRegistrationStateArea, after, 100)
		if err != nil {
			return nil, fmt.Errorf("scan interaction registration state: %w", err)
		}
		for _, entry := range page.Entries {
			if entry.Tombstone || entry.Revision == 0 || !bytes.HasPrefix(entry.Key, []byte(interactionRegistrationStatePrefix)) {
				continue
			}
			in, err := decodeInteractionRegistrationIntent(entry.Value)
			if err != nil || !bytes.Equal(entry.Key, interactionRegistrationKey(in.Group)) {
				return nil, ErrInteractionRegistrationRecovery
			}
			if !in.ManualRecovery {
				continue
			}
			diagnostic := InteractionRegistrationRecoveryDiagnostic{Installation: c.installation, Operation: in.Operation, Group: in.Group, Generation: in.Generation, RegistrationSHA256: interactionRegistrationHash(in.Body), Phase: in.Phase}
			receipt, err := c.session.BotGroupRegistrationReceipt(ctx, c.installation, in.Operation)
			switch {
			case err == nil:
				if validInteractionReceipt(in, receipt) {
					diagnostic.ReceiptStatus = "present"
				} else {
					diagnostic.ReceiptStatus = "mismatch"
				}
			case errors.Is(err, client.ErrRegistrationReceiptNotFound):
				diagnostic.ReceiptStatus = "absent"
			default:
				diagnostic.ReceiptStatus = "unavailable"
			}
			_, err = c.session.GroupInfo(ctx, in.Group)
			switch {
			case err == nil:
				diagnostic.GroupStatus = "present"
			case apiNotFound(err):
				diagnostic.GroupStatus = "absent"
			default:
				diagnostic.GroupStatus = "unavailable"
			}
			out = append(out, diagnostic)
		}
		if len(page.Next) == 0 {
			break
		}
		if bytes.Equal(after, page.Next) {
			return nil, ErrInteractionRegistrationRecovery
		}
		after = append(after[:0], page.Next...)
	}
	return out, nil
}

// RecoverInteractionRegistration resolves a submitted manual-recovery intent
// only after the server returns a receipt matching the original operation,
// body hash, and accepted generation. It never repeats the registration POST.
func (c *InteractionRegistrationCoordinator) RecoverInteractionRegistration(ctx context.Context, group id.ID) (InteractionRegistrationResult, error) {
	if c == nil || group.IsZero() {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := interactionRegistrationKey(group)
	value, err := c.native.StateGet(ctx, interactionRegistrationStateArea, key)
	if err != nil || value.Revision == 0 || value.Tombstone {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	in, err := decodeInteractionRegistrationIntent(value.Value)
	if err != nil || in.Group != group || !in.ManualRecovery || !in.Submitted || len(in.Body) == 0 || in.Phase > 1 {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	receipt, err := c.session.BotGroupRegistrationReceipt(ctx, c.installation, in.Operation)
	if err != nil {
		return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationManualRecovery, "authoritative receipt unavailable", err)
	}
	if !validInteractionReceipt(in, receipt) || (in.Phase == 1 && in.Receipt != receipt) {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	revision := value.Revision
	if in.Phase == 0 {
		in.Phase = 1
		in.Receipt = receipt
		revision, err = c.saveInteractionRegistration(ctx, key, revision, in)
		if err != nil {
			return InteractionRegistrationResult{}, err
		}
	}
	return c.resolveInteractionReceipt(ctx, key, revision, in, receipt)
}

// ReadmitInteractionRegistration gives an exact, previously submitted group a fresh
// admission when its first POST had an unknown outcome and no receipt is available.
// It reuses the original operation ID, group ID, and registration body. The server's
// atomic operation and group uniqueness checks therefore fence a late original POST:
// at most one can commit, and either receipt must match these same bytes.
func (c *InteractionRegistrationCoordinator) ReadmitInteractionRegistration(ctx context.Context, group id.ID) (InteractionRegistrationResult, error) {
	if c == nil || group.IsZero() {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := interactionRegistrationKey(group)
	value, err := c.native.StateGet(ctx, interactionRegistrationStateArea, key)
	if err != nil || value.Revision == 0 || value.Tombstone {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	in, err := decodeInteractionRegistrationIntent(value.Value)
	if err != nil || in.Group != group || !in.ManualRecovery || !in.Submitted || len(in.Body) == 0 || in.Phase != 0 {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	if receipt, lookupErr := c.session.BotGroupRegistrationReceipt(ctx, c.installation, in.Operation); lookupErr == nil {
		return c.acceptInteractionReceipt(ctx, key, value.Revision, in, receipt)
	} else if !errors.Is(lookupErr, client.ErrRegistrationReceiptNotFound) {
		return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationManualRecovery, "receipt lookup failed", lookupErr)
	}
	discovery, err := c.discovery.Discover(ctx)
	if err != nil || discovery.InstanceID != c.instance || discovery.Generation != in.Generation || discovery.PolicyVersion != in.Policy ||
		len(discovery.ExternalSenderPub) != 32 || allZeroBytes(discovery.ExternalSenderPub) {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	identity, err := c.native.Identity(ctx)
	if err != nil || identity.Phase != 2 || identity.Instance == nil || *identity.Instance != c.instance ||
		identity.User == nil || *identity.User != in.BotUser || identity.Device == nil || *identity.Device != in.BotDevice {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	requestID := id.New()
	if _, err := c.session.IssueBotInteractionAdmission(ctx, c.installation, requestID, in.Group, in.ParticipantUser, in.ParticipantDevice); err != nil {
		return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationManualRecovery, "fresh admission unavailable", err)
	}
	wire, err := c.session.BotInteractionContextSnapshot(ctx, c.installation, requestID, in.Group, in.ParticipantUser, in.ParticipantDevice)
	if err != nil {
		return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationManualRecovery, "fresh context unavailable", err)
	}
	snapshot, err := interactionSnapshotDetailsFromWire(wire)
	if err != nil || snapshot.Request != requestID || snapshot.Instance != c.instance || snapshot.Installation != c.installation ||
		snapshot.Community != c.community || snapshot.BotUser != in.BotUser || snapshot.BotDevice != in.BotDevice ||
		snapshot.ParticipantUser != in.ParticipantUser || snapshot.ParticipantDevice != in.ParticipantDevice ||
		snapshot.Group != in.Group || snapshot.PolicyVersion != in.Policy || snapshot.MinimumRevision == 0 ||
		!containsVersion(discovery.E2EEVersions, snapshot.E2EEVersion) || snapshot.MediaVersion != 0 {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	expected := native.InteractionGroupExpected{Instance: c.instance, Installation: c.installation, Community: c.community,
		BotUser: in.BotUser, BotDevice: in.BotDevice, ParticipantUser: in.ParticipantUser, ParticipantDevice: in.ParticipantDevice,
		Group: in.Group, MinimumEligibilityRevision: snapshot.MinimumRevision, ServerNow: snapshot.Now, PolicyVersion: in.Policy}
	inspected, err := c.native.InspectInteractionGroup(ctx, expected, wire)
	if err != nil || inspected.Binding.Instance != c.instance || inspected.Binding.Installation != c.installation ||
		inspected.Binding.Community != c.community || inspected.Binding.BotUser != in.BotUser || inspected.BotDevice != in.BotDevice ||
		inspected.Binding.ParticipantUser != in.ParticipantUser || inspected.Binding.ParticipantDevice != in.ParticipantDevice ||
		inspected.Binding.PolicyVersion != in.Policy || inspected.Binding.E2EEVersion != snapshot.E2EEVersion ||
		inspected.Binding.MediaVersion != snapshot.MediaVersion || inspected.Epoch != interactionRegistrationInitialEpoch ||
		inspected.State != 0 || inspected.NextSequence != interactionRegistrationInitialSequence || inspected.PendingCommit {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	receipt, postErr := c.session.RegisterBotGroupWithAdmissionRequest(ctx, c.installation, in.Operation, in.Generation, in.Body, requestID)
	if postErr != nil {
		receipt, err = c.session.BotGroupRegistrationReceipt(ctx, c.installation, in.Operation)
		if err != nil {
			return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationManualRecovery, "readmission outcome unknown", postErr)
		}
	}
	return c.acceptInteractionReceipt(ctx, key, value.Revision, in, receipt)
}

func validInteractionReceipt(in interactionRegistrationIntent, r client.BotGroupRegistrationReceipt) bool {
	return r.OperationID == in.Operation && r.GroupID == in.Group && r.RegistrationSHA256 == interactionRegistrationHash(in.Body) && r.AcceptedGeneration == in.Generation && r.InitialEpoch == interactionRegistrationInitialEpoch && r.NextSequence == interactionRegistrationInitialSequence
}

// InteractionSnapshot reloads the durable registration binding and asks the
// server for a fresh signed context. The native dispatcher remains responsible
// for authenticating the complete snapshot and the group's current state.
func (c *InteractionRegistrationCoordinator) InteractionSnapshot(ctx context.Context, event native.GroupEvent) (native.InteractionGroupExpected, []byte, error) {
	var expected native.InteractionGroupExpected
	if c == nil || event.Group.IsZero() || event.Community != c.community || event.SenderUser.IsZero() || event.SenderDevice.IsZero() {
		return expected, nil, ErrInteractionRegistrationRecovery
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	value, err := c.native.StateGet(ctx, interactionRegistrationStateArea, interactionRegistrationKey(event.Group))
	if err != nil || value.Revision == 0 || value.Tombstone {
		return expected, nil, ErrInteractionRegistrationRecovery
	}
	in, err := decodeInteractionRegistrationIntent(value.Value)
	if err != nil || in.Group != event.Group || in.Phase != 2 || in.ParticipantUser != event.SenderUser || in.ParticipantDevice != event.SenderDevice {
		return expected, nil, ErrInteractionRegistrationRecovery
	}
	identity, err := c.native.Identity(ctx)
	if err != nil || identity.Phase != 2 || identity.Instance == nil || *identity.Instance != c.instance || identity.User == nil || *identity.User != in.BotUser || identity.Device == nil || *identity.Device != in.BotDevice {
		return expected, nil, ErrInteractionRegistrationRecovery
	}
	discovery, err := c.discovery.Discover(ctx)
	if err != nil || discovery.InstanceID != c.instance || discovery.PolicyVersion != in.Policy {
		return expected, nil, ErrInteractionRegistrationRecovery
	}
	requestID := id.New()
	if _, err := c.session.IssueBotInteractionAdmission(ctx, c.installation, requestID, in.Group, in.ParticipantUser, in.ParticipantDevice); err != nil {
		return expected, nil, err
	}
	wire, err := c.session.BotInteractionContextSnapshot(ctx, c.installation, requestID, in.Group, in.ParticipantUser, in.ParticipantDevice)
	if err != nil {
		return expected, nil, ErrInteractionRegistrationRecovery
	}
	snapshot, err := interactionSnapshotDetailsFromWire(wire)
	if err != nil || snapshot.Request != requestID || snapshot.Instance != c.instance || snapshot.Installation != c.installation || snapshot.Community != c.community || snapshot.BotUser != in.BotUser || snapshot.BotDevice != in.BotDevice || snapshot.ParticipantUser != in.ParticipantUser || snapshot.ParticipantDevice != in.ParticipantDevice || snapshot.Group != in.Group || snapshot.PolicyVersion != in.Policy || !containsVersion(discovery.E2EEVersions, snapshot.E2EEVersion) || snapshot.MediaVersion != 0 {
		return expected, nil, ErrInteractionRegistrationRecovery
	}
	expected = native.InteractionGroupExpected{Instance: snapshot.Instance, Installation: snapshot.Installation, Community: snapshot.Community, BotUser: snapshot.BotUser, BotDevice: snapshot.BotDevice, ParticipantUser: snapshot.ParticipantUser, ParticipantDevice: snapshot.ParticipantDevice, Group: snapshot.Group, MinimumEligibilityRevision: snapshot.MinimumRevision, ServerNow: snapshot.Now, PolicyVersion: snapshot.PolicyVersion}
	return expected, wire, nil
}

func (c *InteractionRegistrationCoordinator) proveInteractionRetrySafe(ctx context.Context, in interactionRegistrationIntent) error {
	d, err := c.discovery.Discover(ctx)
	if err != nil {
		return interactionRegistrationError(ErrInteractionRegistrationOutcomeUnknown, "generation unavailable", err)
	}
	if d.InstanceID != c.instance || d.Generation != in.Generation {
		return ErrInteractionRegistrationOutcomeUnknown
	}
	absent, err := c.session.BotInteractionGroupAbsent(ctx, c.installation, in.AdmissionRequest)
	if err != nil {
		return interactionRegistrationError(ErrInteractionRegistrationOutcomeUnknown, "cannot prove group absence", err)
	}
	if !absent {
		return ErrInteractionRegistrationRecovery
	}
	return nil
}

func (c *InteractionRegistrationCoordinator) acceptInteractionReceipt(ctx context.Context, key []byte, revision uint64, in interactionRegistrationIntent, r client.BotGroupRegistrationReceipt) (InteractionRegistrationResult, error) {
	if r.OperationID != in.Operation || r.GroupID != in.Group || r.RegistrationSHA256 != interactionRegistrationHash(in.Body) || r.AcceptedGeneration != in.Generation || r.InitialEpoch != interactionRegistrationInitialEpoch || r.NextSequence != interactionRegistrationInitialSequence {
		return InteractionRegistrationResult{}, ErrInteractionRegistrationRecovery
	}
	in.Phase = 1
	in.Receipt = r
	revision, err := c.saveInteractionRegistration(ctx, key, revision, in)
	if err != nil {
		return InteractionRegistrationResult{}, err
	}
	return c.resolveInteractionReceipt(ctx, key, revision, in, r)
}

func (c *InteractionRegistrationCoordinator) resolveInteractionReceipt(ctx context.Context, key []byte, revision uint64, in interactionRegistrationIntent, r client.BotGroupRegistrationReceipt) (InteractionRegistrationResult, error) {
	if err := c.native.GroupRegistered(ctx, in.Operation, in.Group, r.NextSequence); err != nil {
		return InteractionRegistrationResult{}, interactionRegistrationError(ErrInteractionRegistrationRecovery, "native resolution failed", err)
	}
	in.Phase = 2
	in.Receipt = r
	in.ManualRecovery = false
	if _, err := c.saveInteractionRegistration(ctx, key, revision, in); err != nil {
		return InteractionRegistrationResult{}, err
	}
	return InteractionRegistrationResult{Receipt: r, PendingWelcome: append([]byte(nil), in.Welcome...), Usable: false}, nil
}

func (c *InteractionRegistrationCoordinator) saveInteractionRegistration(ctx context.Context, key []byte, revision uint64, in interactionRegistrationIntent) (uint64, error) {
	value, err := encodeInteractionRegistrationIntent(in)
	if err != nil {
		return revision, err
	}
	seed := append([]byte("dilla.bot.interaction-registration.tx.v1\x00"), key...)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], revision)
	seed = append(seed, b[:]...)
	seed = append(seed, value...)
	h := sha256.Sum256(seed)
	var tx id.ID
	copy(tx[:], h[:id.Size])
	revs, err := c.native.StateTransaction(ctx, tx, []native.StateChange{{Area: interactionRegistrationStateArea, Key: key, ExpectedRevision: revision, Value: value}})
	if err != nil {
		return revision, err
	}
	if len(revs) != 1 || revs[0] != revision+1 {
		return revision, ErrInteractionRegistrationRecovery
	}
	return revs[0], nil
}

func validInteractionCreateResult(r native.InteractionGroupCreateResult, e native.InteractionGroupExpected, d client.Discovery) bool {
	b := r.Binding
	return r.Group == e.Group && b.Instance == e.Instance && b.Installation == e.Installation && b.Community == e.Community && b.BotUser == e.BotUser && b.ParticipantUser == e.ParticipantUser && b.ParticipantDevice == e.ParticipantDevice && b.PolicyVersion == e.PolicyVersion && b.E2EEVersion == 1 && b.MediaVersion == 0 && containsVersion(d.E2EEVersions, b.E2EEVersion) && r.Epoch == 1 && r.MemberCount == 2 && len(r.BindingBody) > 0 && len(r.GroupInfo) > 0 && len(r.RatchetTree) > 0 && len(r.Welcome) > 0
}
func interactionRegistrationHash(body []byte) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("dilla bot group registration v1\x00"))
	_, _ = h.Write(body)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
func interactionRegistrationKey(group id.ID) []byte {
	return []byte(interactionRegistrationStatePrefix + group.String())
}
func containsVersion(v []uint64, want uint64) bool {
	for _, x := range v {
		if x == want {
			return true
		}
	}
	return false
}

// The snapshot envelope and signed payload are parsed only to correlate the
// durable intent. No signature or admission is trusted here; the native helper
// verifies the complete wire against pinned keys before creating the group.
func interactionSnapshotDetailsFromWire(wire []byte) (interactionSnapshotDetails, error) {
	var details interactionSnapshotDetails
	if cborx.CheckDeterministic(wire) != nil {
		return details, ErrInteractionRegistrationRecovery
	}
	var envelope []any
	if cborx.Unmarshal(wire, &envelope) != nil || len(envelope) != 2 {
		return details, ErrInteractionRegistrationRecovery
	}
	payload, ok := envelope[0].([]byte)
	sig, sok := envelope[1].([]byte)
	if !ok || !sok || len(sig) != 64 || cborx.CheckDeterministic(payload) != nil {
		return details, ErrInteractionRegistrationRecovery
	}
	var a []any
	if cborx.Unmarshal(payload, &a) != nil || len(a) != 16 {
		return details, ErrInteractionRegistrationRecovery
	}
	version, vok := a[0].(uint64)
	var ids [8]id.ID
	var idOK [8]bool
	expected, eok := a[13].([]any)
	if !eok || len(expected) != 8 {
		return details, ErrInteractionRegistrationRecovery
	}
	for i := range ids {
		ids[i], idOK[i] = registrationID(expected[i])
	}
	var request id.ID
	var rok bool
	request, rok = registrationID(a[1])
	var nums [5]uint64
	var numOK [5]bool
	for i, idx := range []int{5, 6, 7, 8, 9} {
		nums[i], numOK[i] = a[idx].(uint64)
	}
	if !vok || version != 1 || !rok || request.IsZero() || !allIDsPresent(idOK[:]) || !numOK[0] || nums[0] == 0 || nums[0] > math.MaxInt64 || !numOK[1] || nums[1] == 0 || nums[1] > math.MaxInt64 || !numOK[2] || nums[2] == 0 || nums[2] > math.MaxInt64 || !numOK[3] || nums[3] != 1 || !numOK[4] || nums[4] != 0 {
		return details, ErrInteractionRegistrationRecovery
	}
	return interactionSnapshotDetails{Request: request, Now: nums[0], MinimumRevision: nums[1], PolicyVersion: nums[2], E2EEVersion: nums[3], MediaVersion: nums[4], Instance: ids[0], Installation: ids[1], Community: ids[2], BotUser: ids[3], BotDevice: ids[4], ParticipantUser: ids[5], ParticipantDevice: ids[6], Group: ids[7]}, nil
}

func allIDsPresent(values []bool) bool {
	for _, value := range values {
		if !value {
			return false
		}
	}
	return true
}

func encodeInteractionRegistrationIntent(i interactionRegistrationIntent) ([]byte, error) {
	var receipt any
	if i.Phase >= 1 {
		receipt = []any{i.Receipt.OperationID, i.Receipt.GroupID, i.Receipt.RegistrationSHA256[:], i.Receipt.AcceptedGeneration, i.Receipt.InitialEpoch, i.Receipt.NextSequence}
	}
	return cborx.Marshal([]any{uint64(interactionRegistrationStateVersion), i.Operation, i.AdmissionRequest, i.Group, i.ParticipantUser, i.ParticipantDevice, i.BotUser, i.BotDevice, i.MinimumRevision, i.Policy, i.Generation, nullableBytes(i.KeyPackage), nullableBytes(i.Admission), nullableBytes(i.Snapshot), nullableBytes(i.Body), nullableBytes(i.Welcome), boolUint(i.Submitted), i.Phase, receipt, boolUint(i.ManualRecovery)})
}

func decodeInteractionRegistrationIntent(raw []byte) (interactionRegistrationIntent, error) {
	var o interactionRegistrationIntent
	if cborx.CheckDeterministic(raw) != nil {
		return o, ErrInteractionRegistrationRecovery
	}
	var a []any
	if cborx.Unmarshal(raw, &a) != nil || len(a) != 19 && len(a) != 20 {
		return o, ErrInteractionRegistrationRecovery
	}
	u, ok := a[0].(uint64)
	if !ok || (u != 1 && u != interactionRegistrationStateVersion) || (u == 1 && len(a) != 19) || (u == 2 && len(a) != 20) {
		return o, ErrInteractionRegistrationRecovery
	}
	ids := []*id.ID{&o.Operation, &o.AdmissionRequest, &o.Group, &o.ParticipantUser, &o.ParticipantDevice, &o.BotUser, &o.BotDevice}
	for i, p := range ids {
		*p, ok = registrationID(a[i+1])
		if !ok || p.IsZero() {
			return o, ErrInteractionRegistrationRecovery
		}
	}
	if o.MinimumRevision, ok = a[8].(uint64); !ok || o.MinimumRevision > math.MaxInt64 {
		return o, ErrInteractionRegistrationRecovery
	}
	if o.Policy, ok = a[9].(uint64); !ok || o.Policy == 0 {
		return o, ErrInteractionRegistrationRecovery
	}
	if o.Generation, ok = a[10].(uint64); !ok || o.Generation == 0 {
		return o, ErrInteractionRegistrationRecovery
	}
	fields := []*[]byte{&o.KeyPackage, &o.Admission, &o.Snapshot, &o.Body, &o.Welcome}
	for i, p := range fields {
		if a[11+i] != nil {
			*p, ok = a[11+i].([]byte)
			if !ok {
				return o, ErrInteractionRegistrationRecovery
			}
			*p = append([]byte(nil), (*p)...)
		}
	}
	if o.Submitted, ok = uintBool(a[16]); !ok {
		return o, ErrInteractionRegistrationRecovery
	}
	if o.Phase, ok = a[17].(uint64); !ok || o.Phase > 2 {
		return o, ErrInteractionRegistrationRecovery
	}
	if o.Phase > 0 && o.MinimumRevision == 0 {
		return o, ErrInteractionRegistrationRecovery
	}
	if o.Phase >= 1 {
		r, ok := a[18].([]any)
		if !ok || len(r) != 6 {
			return o, ErrInteractionRegistrationRecovery
		}
		if o.Receipt.OperationID, ok = registrationID(r[0]); !ok {
			return o, ErrInteractionRegistrationRecovery
		}
		if o.Receipt.GroupID, ok = registrationID(r[1]); !ok {
			return o, ErrInteractionRegistrationRecovery
		}
		hash, ok := r[2].([]byte)
		if !ok || len(hash) != 32 {
			return o, ErrInteractionRegistrationRecovery
		}
		copy(o.Receipt.RegistrationSHA256[:], hash)
		if o.Receipt.AcceptedGeneration, ok = r[3].(uint64); !ok {
			return o, ErrInteractionRegistrationRecovery
		}
		if o.Receipt.InitialEpoch, ok = r[4].(uint64); !ok {
			return o, ErrInteractionRegistrationRecovery
		}
		if o.Receipt.NextSequence, ok = r[5].(uint64); !ok {
			return o, ErrInteractionRegistrationRecovery
		}
		if len(o.Body) == 0 || o.Receipt.OperationID != o.Operation || o.Receipt.GroupID != o.Group || o.Receipt.RegistrationSHA256 != interactionRegistrationHash(o.Body) || o.Receipt.AcceptedGeneration != o.Generation || o.Receipt.InitialEpoch != 1 || o.Receipt.NextSequence != 1 {
			return o, ErrInteractionRegistrationRecovery
		}
	} else if a[18] != nil {
		return o, ErrInteractionRegistrationRecovery
	}
	if len(a) == 20 {
		if o.ManualRecovery, ok = uintBool(a[19]); !ok {
			return o, ErrInteractionRegistrationRecovery
		}
	}
	return o, nil
}

var _ = fmt.Sprintf

// A recovery category is context, not an independent failure. Keep definitive
// rate refusals intact so dispatch/reconciliation can defer them without aging.
func interactionRegistrationError(category error, detail string, cause error) error {
	if client.IsDeferred(cause) {
		return cause
	}
	return fmt.Errorf("%w: %s: %w", category, detail, cause)
}

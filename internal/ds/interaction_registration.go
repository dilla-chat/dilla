package ds

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

const interactionAdmissionDomain = "dilla bot interaction admission v1\x00"
const maxInteractionAdmissionPayload = 250

// AuthenticatedInteractionRegistration contains identity and scope read back from the imported
// MLS group, plus the credential-authenticated roster. LeafCount includes every occupied MLS leaf,
// including leaves the roster extractor cannot decode as dilla identities.
type AuthenticatedInteractionRegistration struct {
	GroupID   []byte
	Binding   []byte
	LeafCount uint64
	Roster    []mlswasi.RosterLeaf
}

// InteractionGroupLeaf is one current leaf read from imported PublicGroup state.
// It is returned only after the leaf count, authenticated identities, persisted
// member projection and immutable kind-3 registration receipt have been checked.
type InteractionGroupLeaf struct {
	LeafIndex    uint32
	UserID       id.ID
	DeviceID     id.ID
	SignatureKey []byte
}

// CurrentBotInteractionGroupContext is a read-only proof input for a human-facing
// admission context. The registration receipt is immutable acceptance evidence;
// Binding and Roster describe the currently persisted, WASI-authenticated MLS state.
type CurrentBotInteractionGroupContext struct {
	Registration store.BotGroupRegistrationReceipt
	Binding      BotInteractionGroupBinding
	Epoch        uint64
	LeafCount    uint64
	TreeHash     []byte
	Roster       []InteractionGroupLeaf
	verification *deviceListScope
}

// CurrentBotInteractionGroupContext loads the accepted kind-3 registration and imports
// the current persisted MLS state under the group's local and repository locks. It refuses
// state whose public roster does not exactly match the two leaves accepted at registration.
func (d *DS) CurrentBotInteractionGroupContext(ctx context.Context, groupID id.ID) (CurrentBotInteractionGroupContext, error) {
	var out CurrentBotInteractionGroupContext
	err := d.WithCurrentBotInteractionGroupContext(ctx, groupID, func(current CurrentBotInteractionGroupContext) error {
		out = current
		return nil
	})
	return out, err
}

// WithCurrentBotInteractionGroupContext runs fn while both the local group mutex and the
// repository group fence are held. Callers that issue a proof based on this context use it to
// revalidate and sign before a concurrent commit or close can change the group's epoch/state.
// fn must not start another repository transaction.
func (d *DS) WithCurrentBotInteractionGroupContext(ctx context.Context, groupID id.ID, fn func(CurrentBotInteractionGroupContext) error) error {
	if fn == nil {
		return errors.New("ds: current kind-3 interaction context callback unavailable")
	}
	return d.WithCurrentBotInteractionGroupRead(ctx, groupID, func(current CurrentBotInteractionGroupContext, _ store.Repository) error {
		return fn(current)
	})
}

// WithCurrentBotInteractionGroupRead runs fn while both the local group mutex and the
// repository group fence are held, after verifying the accepted kind-3 registration and its
// exact current two-leaf roster. Reads performed through the supplied tx therefore observe the
// same group state that was validated, before a concurrent commit or close can proceed. fn must
// use tx for all repository reads and must not start another repository transaction.
func (d *DS) WithCurrentBotInteractionGroupRead(ctx context.Context, groupID id.ID, fn func(CurrentBotInteractionGroupContext, store.Repository) error) error {
	return d.withCurrentBotInteractionGroupRead(ctx, groupID, false, fn)
}

// WithCurrentBotInteractionGroupHealRead validates the accepted private group roster for the Bot
// heal endpoint while permitting the restored row's EpochUnknown marker. All other registration,
// binding, public MLS state, and two-leaf roster checks remain identical to ordinary reads.
func (d *DS) WithCurrentBotInteractionGroupHealRead(ctx context.Context, groupID id.ID, fn func(CurrentBotInteractionGroupContext, store.Repository) error) error {
	return d.withCurrentBotInteractionGroupRead(ctx, groupID, true, fn)
}

func (d *DS) withCurrentBotInteractionGroupRead(ctx context.Context, groupID id.ID, allowEpochUnknown bool, fn func(CurrentBotInteractionGroupContext, store.Repository) error) error {
	if d == nil || d.opts.Store == nil || d.opts.Wasm == nil || groupID.IsZero() {
		return errors.New("ds: current kind-3 interaction context dependencies unavailable")
	}
	if fn == nil {
		return errors.New("ds: current kind-3 interaction context callback unavailable")
	}
	unlock := d.lock(groupID)
	defer unlock()
	// Reserve the guest before SQL, matching uploads and other native writers.
	// Keep it through the callback, whose signed-list checks borrow this lease.
	instance, err := d.opts.Wasm.Acquire(ctx)
	if err != nil {
		return err
	}
	defer instance.Release()
	err = d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		// Match the repository lock order used by message uploads and permission mutations:
		// community first, then group. This initial read only chooses the community fence; the
		// group row is reloaded after both locks and must still have the same scope.
		initial, err := tx.GetGroup(ctx, groupID)
		if err != nil {
			return err
		}
		if initial.CommunityID == nil || initial.CommunityID.IsZero() {
			return errors.New("ds: current kind-3 group has no community scope")
		}
		if err := tx.LockCommunity(ctx, *initial.CommunityID); err != nil {
			return err
		}
		if err := tx.LockGroup(ctx, groupID); err != nil {
			return err
		}
		row, err := tx.GetGroup(ctx, groupID)
		if err != nil {
			return err
		}
		if row.CommunityID == nil || *row.CommunityID != *initial.CommunityID {
			return errors.New("ds: current kind-3 group community changed during read")
		}
		registration, err := tx.GetBotGroupRegistrationReceiptByGroup(ctx, groupID)
		if err != nil {
			return err
		}
		valid := validCurrentInteractionRegistration(row, registration, d.opts.Keys.InstanceID)
		if allowEpochUnknown && row.EpochUnknown {
			valid = validCurrentInteractionRegistrationAfterRestore(row, registration, d.opts.Keys.InstanceID)
		}
		if !valid {
			return errors.New("ds: accepted kind-3 registration and current group row disagree")
		}
		var binding interactionBindingV2
		if err := cborx.Unmarshal(row.Binding, &binding); err != nil {
			return errors.New("ds: current kind-3 group binding is malformed")
		}
		canonicalBinding, err := cborx.Marshal(binding)
		if err != nil || !bytes.Equal(canonicalBinding, row.Binding) || binding.Version != 2 || binding.LegacyCommunityID != nil || binding.Kind != 3 ||
			binding.InstanceID != d.opts.Keys.InstanceID || binding.InstallationID != registration.InstallationID || binding.CommunityID != registration.CommunityID ||
			binding.BotUserID != registration.UserID || binding.ParticipantUserID.IsZero() || binding.ParticipantDeviceID.IsZero() || binding.BotUserID == binding.ParticipantUserID ||
			binding.PolicyVersion == 0 || binding.E2EEVersion != 1 || binding.MediaVersion != 0 || binding.PolicyVersion != row.PolicyVersion {
			return errors.New("ds: current kind-3 binding does not match accepted receipt")
		}
		if len(row.PublicGroupState) == 0 {
			return errors.New("ds: current kind-3 group has no persisted public state")
		}
		group, err := instance.PublicGroupImport(ctx, row.PublicGroupState, groupID[:])
		if err != nil {
			return errors.New("ds: current kind-3 public state cannot be imported")
		}
		groupOpen := true
		defer func() {
			if groupOpen {
				_ = group.Close(ctx)
			}
		}()
		state, err := group.State(ctx)
		if err != nil {
			return err
		}
		if !bytes.Equal(state.GroupID, groupID[:]) || !bytes.Equal(state.Binding, row.Binding) || state.Epoch != row.Epoch ||
			!bytes.Equal(state.TreeHash, row.TreeHash) || state.LeafCount != 2 || len(state.Members) != 2 {
			return errors.New("ds: current kind-3 MLS state is not the exact accepted two-leaf group")
		}
		members, err := tx.ListMembers(ctx, groupID)
		if err != nil {
			return err
		}
		if len(members) != 2 {
			return errors.New("ds: current kind-3 member projection is not exactly two leaves")
		}
		byLeaf := make(map[uint32]store.MemberRow, len(members))
		for _, member := range members {
			if member.GroupID != groupID {
				return errors.New("ds: current kind-3 member projection names another group")
			}
			if _, exists := byLeaf[member.LeafIndex]; exists {
				return errors.New("ds: current kind-3 member projection has duplicate leaves")
			}
			byLeaf[member.LeafIndex] = member
		}
		roster := make([]InteractionGroupLeaf, 0, 2)
		seenLeaves := make(map[uint32]struct{}, 2)
		for _, leaf := range state.Members {
			deviceID, userID, err := decodeCredentialIdentity(leaf.CredentialIdentity)
			if err != nil || userID.IsZero() || deviceID.IsZero() || len(leaf.SignatureKey) != ed25519.PublicKeySize {
				return errors.New("ds: current kind-3 MLS leaf has no valid authenticated identity")
			}
			member, ok := byLeaf[leaf.LeafIndex]
			if _, duplicate := seenLeaves[leaf.LeafIndex]; duplicate || !ok || member.UserID != userID || member.DeviceID != deviceID || !bytes.Equal(member.SignatureKey, leaf.SignatureKey) {
				return errors.New("ds: current kind-3 MLS roster differs from the stored member projection")
			}
			seenLeaves[leaf.LeafIndex] = struct{}{}
			roster = append(roster, InteractionGroupLeaf{LeafIndex: leaf.LeafIndex, UserID: userID, DeviceID: deviceID, SignatureKey: append([]byte(nil), leaf.SignatureKey...)})
		}
		if len(seenLeaves) != len(byLeaf) {
			return errors.New("ds: current kind-3 member projection contains a leaf absent from MLS state")
		}
		if !containsInteractionLeaf(roster, binding.BotUserID, registration.DeviceID) || !containsInteractionLeaf(roster, binding.ParticipantUserID, binding.ParticipantDeviceID) {
			return errors.New("ds: current kind-3 MLS roster does not match the accepted Bot and participant")
		}
		out := CurrentBotInteractionGroupContext{
			Registration: registration,
			Binding: BotInteractionGroupBinding{InstanceID: binding.InstanceID, BotUserID: binding.BotUserID, InstallationID: binding.InstallationID,
				CommunityID: binding.CommunityID, ParticipantUserID: binding.ParticipantUserID, ParticipantDeviceID: binding.ParticipantDeviceID,
				PolicyVersion: binding.PolicyVersion, E2EEVersion: binding.E2EEVersion, MediaVersion: binding.MediaVersion},
			Epoch: state.Epoch, LeafCount: state.LeafCount, TreeHash: append([]byte(nil), state.TreeHash...), Roster: roster,
		}
		if err := group.Close(ctx); err != nil {
			return err
		}
		groupOpen = false
		return withBorrowedDeviceListVerifier(ctx, d.opts.Wasm, instance, func(scoped context.Context) error {
			out.verification, _ = scoped.Value(deviceListScopeKey{}).(*deviceListScope)
			return fn(out, tx)
		})
	})
	return err
}

func validCurrentInteractionRegistrationAfterRestore(row store.GroupRow, receipt store.BotGroupRegistrationReceipt, instanceID id.ID) bool {
	row.EpochUnknown = false
	return validCurrentInteractionRegistration(row, receipt, instanceID)
}

func validCurrentInteractionRegistration(row store.GroupRow, receipt store.BotGroupRegistrationReceipt, instanceID id.ID) bool {
	return row.GroupID == receipt.GroupID && row.Kind == 3 && row.CommunityID != nil && *row.CommunityID == receipt.CommunityID &&
		row.ClosedAt == nil && !row.EpochUnknown && receipt.InstallationID != (id.ID{}) && receipt.OperationID != (id.ID{}) &&
		receipt.UserID != (id.ID{}) && receipt.DeviceID != (id.ID{}) && receipt.CommunityID != (id.ID{}) && len(receipt.BodySHA256) == sha256.Size &&
		receipt.Generation > 0 && receipt.Epoch == 1 && receipt.NextSeq == 1 && receipt.AcceptedAt >= 0 && len(row.Binding) > 0 &&
		len(row.TreeHash) == sha256.Size && row.ExternalSenderKeyID != (id.ID{}) && instanceID != (id.ID{}) && row.E2EEVersion == 1 && row.MediaVersion == 0
}

func containsInteractionLeaf(roster []InteractionGroupLeaf, userID, deviceID id.ID) bool {
	for _, leaf := range roster {
		if leaf.UserID == userID && leaf.DeviceID == deviceID {
			return true
		}
	}
	return false
}

// InteractionRegistrationExtractor must import the submitted GroupInfo/tree through WASI and
// return both authenticated public group state and the authenticated roster.
type InteractionRegistrationExtractor interface {
	ExtractInteractionRegistration(context.Context, []byte, []byte) (AuthenticatedInteractionRegistration, error)
}

// WASIInteractionRegistrationExtractor adapts an acquired WASI instance to the preflight contract.
type WASIInteractionRegistrationExtractor struct {
	Instance *mlswasi.Instance
}

func (w WASIInteractionRegistrationExtractor) ExtractInteractionRegistration(ctx context.Context, tree, info []byte) (AuthenticatedInteractionRegistration, error) {
	if w.Instance == nil {
		return AuthenticatedInteractionRegistration{}, errors.New("ds: kind-3 registration preflight has no WASI instance")
	}
	roster, err := w.Instance.ExtractAuthenticatedRoster(ctx, tree, info)
	if err != nil {
		return AuthenticatedInteractionRegistration{}, err
	}
	group, err := w.Instance.PublicGroupFromExternal(ctx, tree, info)
	if err != nil {
		return AuthenticatedInteractionRegistration{}, err
	}
	defer func() { _ = group.Close(ctx) }()
	state, err := group.State(ctx)
	if err != nil {
		return AuthenticatedInteractionRegistration{}, err
	}
	return AuthenticatedInteractionRegistration{GroupID: append([]byte(nil), state.GroupID...), Binding: append([]byte(nil), state.Binding...), LeafCount: state.LeafCount, Roster: roster}, nil
}

// InteractionRegistrationPreflight is the complete trusted-input contract for examining a
// proposed kind-3 group. Receipt fields are verified against PinnedInstanceKey; Expected,
// MinimumEligibilityRevision and device-list entries must be fetched and verified by the server
// under the current installation/eligibility fence. This preflight does not persist or authorize
// registration.
type InteractionRegistrationPreflight struct {
	Expected                   InteractionAdmissionTuple
	MinimumEligibilityRevision uint64
	ExpectedPolicyVersion      uint64
	ExpectedE2EEVersion        uint64
	ExpectedMediaVersion       uint64
	ReceiptPayload             []byte
	ReceiptSignature           []byte
	PinnedInstanceKey          ed25519.PublicKey
	BotDeviceList              []mlswasi.DeviceListEntry
	ParticipantDeviceList      []mlswasi.DeviceListEntry
	Now                        uint64
	Registration               RegisterRequest
}

type interactionAdmissionWire struct {
	_                   struct{} `cbor:",toarray"`
	Version             uint64
	AdmissionID         id.ID
	InstanceID          id.ID
	InstallationID      id.ID
	CommunityID         id.ID
	BotUserID           id.ID
	BotDeviceID         id.ID
	ParticipantUserID   id.ID
	ParticipantDeviceID id.ID
	GroupID             id.ID
	EligibilityRevision uint64
	NotAfter            uint64
}

type interactionBindingV2 struct {
	_                   struct{} `cbor:",toarray"`
	Version             uint64
	InstanceID          id.ID
	LegacyCommunityID   *id.ID
	BotUserID           id.ID
	Kind                uint8
	PolicyVersion       uint64
	E2EEVersion         uint64
	MediaVersion        uint64
	InstallationID      id.ID
	CommunityID         id.ID
	ParticipantUserID   id.ID
	ParticipantDeviceID id.ID
}

// PreflightKind3Registration authenticates the server-issued admission receipt, checks that the
// exact submitted registration is its kind-3 group, extracts authenticated MLS leaves through the
// supplied WASI instance, and applies VerifyInteractionRoster. It is deliberately a preflight only:
// the caller still needs transaction-fenced account, membership, ban, receipt-consumption,
// idempotency and recovery authorization before registration can be enabled.
func PreflightKind3Registration(ctx context.Context, extractor InteractionRegistrationExtractor, r InteractionRegistrationPreflight) error {
	if extractor == nil {
		return errors.New("ds: kind-3 registration preflight has no WASI roster extractor")
	}
	if len(r.PinnedInstanceKey) != ed25519.PublicKeySize || len(r.ReceiptSignature) != ed25519.SignatureSize || len(r.ReceiptPayload) == 0 || len(r.ReceiptPayload) > maxInteractionAdmissionPayload {
		return errors.New("ds: kind-3 registration preflight has no valid pinned receipt material")
	}
	if r.ExpectedPolicyVersion == 0 || r.ExpectedE2EEVersion != 1 || r.ExpectedMediaVersion != 0 {
		return errors.New("ds: kind-3 registration preflight has invalid trusted binding versions")
	}
	if !ed25519.Verify(r.PinnedInstanceKey, append([]byte(interactionAdmissionDomain), r.ReceiptPayload...), r.ReceiptSignature) {
		return errors.New("ds: interaction admission receipt signature is invalid")
	}
	var wire interactionAdmissionWire
	if err := cborx.Unmarshal(r.ReceiptPayload, &wire); err != nil {
		return fmt.Errorf("ds: decode interaction admission receipt: %w", err)
	}
	canonical, err := cborx.Marshal(wire)
	if err != nil || !bytes.Equal(canonical, r.ReceiptPayload) {
		return errors.New("ds: interaction admission receipt is not canonical")
	}
	admission := InteractionAdmission{
		Version:     wire.Version,
		AdmissionID: wire.AdmissionID,
		InteractionAdmissionTuple: InteractionAdmissionTuple{
			InstanceID: wire.InstanceID, InstallationID: wire.InstallationID, CommunityID: wire.CommunityID,
			BotUserID: wire.BotUserID, BotDeviceID: wire.BotDeviceID,
			ParticipantUserID: wire.ParticipantUserID, ParticipantDeviceID: wire.ParticipantDeviceID,
			GroupID: wire.GroupID,
		},
		EligibilityRevision: wire.EligibilityRevision,
		NotAfter:            wire.NotAfter,
	}
	if r.Registration.GroupID != r.Expected.GroupID {
		return errors.New("ds: submitted registration group does not match trusted interaction group")
	}
	var binding interactionBindingV2
	if err := cborx.Unmarshal(r.Registration.Binding, &binding); err != nil || binding.Version != 2 || binding.LegacyCommunityID != nil || binding.Kind != 3 || binding.InstanceID != r.Expected.InstanceID || binding.BotUserID != r.Expected.BotUserID || binding.InstallationID != r.Expected.InstallationID || binding.CommunityID != r.Expected.CommunityID || binding.ParticipantUserID != r.Expected.ParticipantUserID || binding.ParticipantDeviceID != r.Expected.ParticipantDeviceID || binding.PolicyVersion != r.ExpectedPolicyVersion || binding.E2EEVersion != r.ExpectedE2EEVersion || binding.MediaVersion != r.ExpectedMediaVersion {
		return errors.New("ds: submitted registration binding is not the admitted kind-3 group")
	}
	if len(r.Registration.GroupInfo) == 0 || len(r.Registration.RatchetTree) == 0 {
		return errors.New("ds: submitted registration is missing GroupInfo or ratchet tree")
	}
	actual, err := extractor.ExtractInteractionRegistration(ctx, r.Registration.RatchetTree, r.Registration.GroupInfo)
	if err != nil {
		return fmt.Errorf("ds: authenticate kind-3 registration roster: %w", err)
	}
	if !bytes.Equal(actual.GroupID, r.Expected.GroupID[:]) || !bytes.Equal(actual.Binding, r.Registration.Binding) || actual.LeafCount != 2 {
		return errors.New("ds: authenticated MLS group id, binding, or occupied leaf count does not match the admitted registration")
	}
	leaves := make([]AuthenticatedInteractionLeaf, len(actual.Roster))
	for i, leaf := range actual.Roster {
		if len(leaf.UserID) != len(id.ID{}) || len(leaf.DeviceID) != len(id.ID{}) {
			return fmt.Errorf("ds: authenticated roster leaf %d has invalid identity length", i)
		}
		var userID, deviceID id.ID
		copy(userID[:], leaf.UserID)
		copy(deviceID[:], leaf.DeviceID)
		leaves[i] = AuthenticatedInteractionLeaf{UserID: userID, DeviceID: deviceID, DSKPub: append([]byte(nil), leaf.DSKPub...), Tier: leaf.Tier, Kind: leaf.Kind}
	}
	return VerifyInteractionRoster(r.Expected, r.MinimumEligibilityRevision, admission, leaves, r.BotDeviceList, r.ParticipantDeviceList, r.Now)
}

package ds

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// Kind3PreflightEvidence is an opaque capability created only after the WASI-backed
// preflight authenticated a kind-3 roster. It binds that result to the exact request
// and signed receipt so a caller cannot substitute a different group at final acceptance.
type Kind3PreflightEvidence struct {
	tuple                 InteractionAdmissionTuple
	admissionID           id.ID
	policyVersion         uint64
	authenticatedGroupID  id.ID
	authenticatedBinding  []byte
	authenticatedLeaves   uint64
	receiptPayload        []byte
	receiptSig            []byte
	registration          RegisterRequest
	requestHash           [32]byte
	botDeviceList         []mlswasi.DeviceListEntry
	participantDeviceList []mlswasi.DeviceListEntry
}

// RegisteredKind3State is the authenticated initial MLS state extracted from the exact
// submitted GroupInfo/tree before the group is persisted in the final transaction.
type RegisteredKind3State struct {
	GroupID    id.ID
	Binding    []byte
	LeafCount  uint64
	Generation uint64
	Epoch      uint64
	NextSeq    uint64
}

// VerifiedKind3Registration contains the durable acceptance record to pass directly to
// AcceptBotGroupRegistrationAdmission in the caller's still-open transaction.
type VerifiedKind3Registration struct {
	Admission store.BotInteractionAdmissionReceipt
	Receipt   store.BotGroupRegistrationReceipt
}

type interactionAdmissionAuthorityWire struct {
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

// PrepareKind3PreflightEvidence runs the cryptographic/WASI preflight and returns an
// opaque token for the final transaction fence. The caller must retain this token and
// use it only with the same registration request.
func PrepareKind3PreflightEvidence(ctx context.Context, extractor InteractionRegistrationExtractor, preflight InteractionRegistrationPreflight) (Kind3PreflightEvidence, error) {
	if err := PreflightKind3Registration(ctx, extractor, preflight); err != nil {
		return Kind3PreflightEvidence{}, err
	}
	var wire interactionAdmissionAuthorityWire
	if err := cborx.Unmarshal(preflight.ReceiptPayload, &wire); err != nil {
		return Kind3PreflightEvidence{}, fmt.Errorf("ds: decode preflight admission receipt: %w", err)
	}
	hash, err := hashInteractionRegistration(preflight.Registration)
	if err != nil {
		return Kind3PreflightEvidence{}, err
	}
	return Kind3PreflightEvidence{
		tuple: preflight.Expected, admissionID: wire.AdmissionID, policyVersion: preflight.ExpectedPolicyVersion,
		authenticatedGroupID: preflight.Expected.GroupID, authenticatedBinding: append([]byte(nil), preflight.Registration.Binding...), authenticatedLeaves: 2,
		receiptPayload: append([]byte(nil), preflight.ReceiptPayload...),
		receiptSig:     append([]byte(nil), preflight.ReceiptSignature...),
		registration:   cloneRegisterRequest(preflight.Registration), requestHash: hash,
		botDeviceList: cloneDeviceListEntries(preflight.BotDeviceList), participantDeviceList: cloneDeviceListEntries(preflight.ParticipantDeviceList),
	}, nil
}

// ValidateKind3RegistrationAuthority reloads all mutable authority inside the caller's
// final transaction. It acquires Community -> Installation -> sorted Bot/participant
// User locks itself. The returned record is suitable for atomic admission consumption.
// The verifier must be the guest already held by registration; acquiring another
// instance while the transaction is open deadlocks a single-instance WASM pool.
// It does not create or accept a registration itself.
func (d *DS) ValidateKind3RegistrationAuthority(ctx context.Context, tx store.Repository, verifier DeviceListVerifier, session auth.Session, installationID, admissionRequestID, admissionID, operationID id.ID, expected InteractionAdmissionTuple, registration RegisterRequest, registrationBody []byte, state RegisteredKind3State, evidence Kind3PreflightEvidence) (VerifiedKind3Registration, error) {
	var out VerifiedKind3Registration
	if tx == nil || verifier == nil || d == nil || d.opts.Clock == nil || session.Bot == nil || len(session.TokenHash) == 0 || installationID.IsZero() || admissionRequestID.IsZero() || admissionID.IsZero() || operationID.IsZero() {
		return out, errors.New("ds: incomplete final kind-3 authority context")
	}
	if evidence.admissionID.IsZero() || evidence.admissionID != admissionID || evidence.tuple != expected || expected.InstallationID != installationID || expected.BotUserID != session.UserID || expected.BotDeviceID != session.DeviceID || session.Bot.InstallationID != installationID || session.Bot.CommunityID != expected.CommunityID || registration.Session.UserID != session.UserID || registration.Session.DeviceID != session.DeviceID || !bytes.Equal(registration.Session.TokenHash, session.TokenHash) || registration.Session.Bot == nil || registration.Session.Bot.InstallationID != session.Bot.InstallationID || registration.Session.Bot.CommunityID != session.Bot.CommunityID || registration.Session.Bot.State != session.Bot.State {
		return out, errors.New("ds: kind-3 authority evidence does not match authenticated Bot context")
	}
	requestHash, err := hashInteractionRegistration(registration)
	if err != nil || requestHash != evidence.requestHash || !sameRegistration(registration, evidence.registration) || state.GroupID != expected.GroupID || !bytes.Equal(state.Binding, registration.Binding) {
		return out, errors.New("ds: final kind-3 registration differs from authenticated preflight")
	}
	// The admitted Bot+participant initial group is built by the MLS Add operation, which
	// advances the authenticated group to epoch 1. Requiring this exact epoch rejects both
	// a pre-Add tree and a group with additional commits before registration.
	if registration.GroupID != expected.GroupID || state.Generation == 0 || state.Epoch != 1 || state.NextSeq != 1 || len(registrationBody) == 0 || state.LeafCount != 2 || state.GroupID != evidence.authenticatedGroupID || evidence.authenticatedLeaves != 2 || !bytes.Equal(state.Binding, evidence.authenticatedBinding) {
		return out, errors.New("ds: invalid final kind-3 registered state")
	}
	if session.Bot.State != store.BotInstallationJoining && session.Bot.State != store.BotInstallationReady {
		return out, errors.New("ds: Bot session state is not eligible for kind-3 registration")
	}
	initial, err := tx.GetBotInstallation(ctx, installationID)
	if err != nil || initial.CommunityID != expected.CommunityID || initial.RuntimeID == nil || initial.RemovedAt != nil {
		return out, errors.New("ds: active Bot runtime installation required")
	}
	if err := tx.LockInstanceGeneration(ctx); err != nil {
		return out, err
	}
	if err := tx.LockBotRuntime(ctx, *initial.RuntimeID); err != nil {
		return out, err
	}
	if err := tx.LockBotCommunity(ctx, expected.CommunityID); err != nil {
		return out, err
	}
	if err := tx.LockCommunity(ctx, expected.CommunityID); err != nil {
		return out, err
	}
	if err := tx.LockBotInstallation(ctx, installationID); err != nil {
		return out, err
	}
	users := []id.ID{expected.BotUserID, expected.ParticipantUserID}
	slices.SortFunc(users, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	for i, userID := range users {
		if i > 0 && userID == users[i-1] { //nolint:gosec // G602: i-1 is accessed only when i > 0.
			continue
		}
		if err := tx.LockUserForDeviceRegistration(ctx, userID); err != nil {
			return out, err
		}
	}

	installation, err := tx.GetBotInstallation(ctx, installationID)
	if err != nil || installation.ID != installationID || installation.CommunityID != expected.CommunityID || installation.RuntimeID == nil || *installation.RuntimeID != *initial.RuntimeID || installation.State != session.Bot.State || installation.RemovedAt != nil || installation.UserID == nil || installation.DeviceID == nil || installation.CredentialVerifiedAt == nil || *installation.UserID != session.UserID || *installation.DeviceID != session.DeviceID {
		return out, errors.New("ds: current installation does not match kind-3 Bot session")
	}
	runtime, err := tx.GetBotRuntime(ctx, *installation.RuntimeID)
	if err != nil || runtime.RevokedAt != nil {
		return out, errors.New("ds: managed Bot runtime is no longer active")
	}
	bound, err := tx.GetBotSessionBinding(ctx, session.TokenHash)
	if err != nil || bound.InstallationID != installation.ID || bound.UserID != session.UserID || bound.DeviceID != session.DeviceID {
		return out, errors.New("ds: authenticated Bot session binding is no longer current")
	}
	now := d.opts.Clock.Now().Unix()
	if now < 0 {
		return out, errors.New("ds: server time is outside interaction admission range")
	}
	bot, err := tx.GetUser(ctx, session.UserID)
	if err != nil || bot.Kind != 1 || bot.Flags != 0 || bot.DisabledAt != nil || bot.DeletedAt != nil {
		return out, errors.New("ds: active managed Bot account required")
	}
	botDevice, err := tx.GetDevice(ctx, session.DeviceID)
	if err != nil || botDevice.UserID != bot.ID || botDevice.VerifiedAt == nil || botDevice.RevokedAt != nil || botDevice.QuarantinedAt != nil {
		return out, errors.New("ds: managed Bot device is no longer active")
	}
	participant, err := tx.GetUser(ctx, expected.ParticipantUserID)
	if err != nil || participant.Kind != 0 || participant.DisabledAt != nil || participant.DeletedAt != nil {
		return out, errors.New("ds: active human participant required")
	}
	participantDevice, err := tx.GetDevice(ctx, expected.ParticipantDeviceID)
	if err != nil || !ActiveHumanInteractionDevice(participant.ID, participantDevice) {
		return out, errors.New("ds: participant device is no longer active")
	}
	if _, err = tx.GetMember(ctx, installation.CommunityID, bot.ID); err != nil {
		return out, errors.New("ds: Bot is no longer a community member")
	}
	if _, err = tx.GetMember(ctx, installation.CommunityID, participant.ID); err != nil {
		return out, errors.New("ds: participant is no longer a community member")
	}
	if err = interactionNotBanned(ctx, tx, installation.CommunityID, bot.ID, now); err != nil {
		return out, err
	}
	if err = interactionNotBanned(ctx, tx, installation.CommunityID, participant.ID, now); err != nil {
		return out, err
	}
	botEntries, err := currentInteractionDeviceList(ctx, tx, verifier, bot, botDevice)
	if err != nil {
		return out, fmt.Errorf("ds: current Bot device list: %w", err)
	}
	participantEntries, err := currentInteractionDeviceList(ctx, tx, verifier, participant, participantDevice)
	if err != nil {
		return out, fmt.Errorf("ds: current participant device list: %w", err)
	}
	eligibility, ok := tx.(store.BotInteractionEligibility)
	if !ok {
		return out, errors.New("ds: interaction eligibility revision source unavailable")
	}
	revision, err := eligibility.GetBotInteractionEligibilityRevision(ctx, installation.CommunityID, participant.ID)
	if err != nil || revision == 0 {
		return out, errors.New("ds: current participant eligibility revision unavailable")
	}
	community, err := tx.GetCommunity(ctx, installation.CommunityID)
	if err != nil || community.DeletedAt != nil || community.PolicyVersion == 0 {
		return out, errors.New("ds: current community policy unavailable")
	}
	var binding interactionBindingV2
	if err := cborx.Unmarshal(registration.Binding, &binding); err != nil || binding.PolicyVersion != community.PolicyVersion || evidence.policyVersion != community.PolicyVersion || binding.E2EEVersion != 1 || binding.MediaVersion != 0 {
		return out, errors.New("ds: kind-3 registration policy or protocol versions are stale")
	}
	instance, err := tx.GetInstance(ctx)
	if err != nil {
		return out, err
	}
	if instance.Generation != state.Generation {
		return out, errors.New("ds: registered MLS state has a stale instance generation")
	}
	if instance.InstanceID != d.opts.Keys.InstanceID || instance.ExternalSenderKeyID != d.opts.Keys.ExternalSenderKeyID || expected.InstanceID != instance.InstanceID {
		return out, errors.New("ds: current instance key context is stale")
	}
	instanceKey := ed25519.NewKeyFromSeed(d.opts.Keys.ExternalSenderPriv[:])
	pub, ok := instanceKey.Public().(ed25519.PublicKey)
	if !ok {
		return out, errors.New("ds: instance signing key has an unexpected type")
	}
	receipts, ok := tx.(store.BotInteractionAdmissionReceipts)
	if !ok {
		return out, errors.New("ds: interaction admission receipt source unavailable")
	}
	issued, err := receipts.GetBotInteractionAdmissionReceipt(ctx, installation.ID, admissionRequestID)
	if err != nil {
		return out, err
	}
	if issued.InstallationID != installation.ID || issued.RequestID != admissionRequestID || issued.NotAfter <= now || len(issued.Payload) == 0 || len(issued.Signature) != ed25519.SignatureSize || !bytes.Equal(issued.Payload, evidence.receiptPayload) || !bytes.Equal(issued.Signature, evidence.receiptSig) || !ed25519.Verify(pub, append([]byte(interactionAdmissionDomain), issued.Payload...), issued.Signature) {
		return out, errors.New("ds: current issued admission receipt is expired, changed, or invalid")
	}
	issuedNotAfter := uint64(issued.NotAfter) //nolint:gosec // G115: issued.NotAfter > now >= 0 above.
	var wire interactionAdmissionAuthorityWire
	if err := cborx.Unmarshal(issued.Payload, &wire); err != nil {
		return out, err
	}
	canonical, err := cborx.Marshal(wire)
	if err != nil || !bytes.Equal(canonical, issued.Payload) || wire.Version != 1 || wire.AdmissionID != admissionID || wire.InstanceID != instance.InstanceID || wire.InstallationID != installation.ID || wire.CommunityID != installation.CommunityID || wire.BotUserID != bot.ID || wire.BotDeviceID != botDevice.ID || wire.ParticipantUserID != participant.ID || wire.ParticipantDeviceID != participantDevice.ID || wire.GroupID != expected.GroupID || wire.EligibilityRevision != revision || wire.NotAfter <= uint64(now) || wire.NotAfter != issuedNotAfter {
		return out, errors.New("ds: current admission receipt does not match final trusted context")
	}
	requestTuple, err := cborx.Marshal([]any{expected.GroupID, expected.ParticipantUserID, expected.ParticipantDeviceID})
	if err != nil {
		return out, err
	}
	requestDigest := sha256.Sum256(requestTuple)
	if !bytes.Equal(requestDigest[:], issued.RequestHash) {
		return out, errors.New("ds: current admission request hash does not match expected tuple")
	}
	if !equalDeviceListEntries(botEntries, evidence.botDeviceList) || !equalDeviceListEntries(participantEntries, evidence.participantDeviceList) {
		return out, errors.New("ds: signed device list changed after kind-3 roster preflight")
	}
	canonicalBody, err := cborx.Marshal([]any{registration.GroupID, registration.Binding, registration.GroupInfo, registration.RatchetTree, registration.InitialWelcome})
	if err != nil || !bytes.Equal(canonicalBody, registrationBody) {
		return out, errors.New("ds: registration body is not the canonical preflight request")
	}
	domainBody := append([]byte("dilla bot group registration v1\x00"), canonicalBody...)
	bodyHash := sha256.Sum256(domainBody)
	out.Admission = issued
	out.Receipt = store.BotGroupRegistrationReceipt{InstallationID: installation.ID, OperationID: operationID, UserID: bot.ID, DeviceID: botDevice.ID, CommunityID: community.ID, GroupID: state.GroupID, BodySHA256: bodyHash[:], Generation: state.Generation, Epoch: state.Epoch, NextSeq: state.NextSeq, AcceptedAt: now}
	return out, nil
}

func interactionNotBanned(ctx context.Context, tx store.Repository, community, user id.ID, now int64) error {
	ban, err := tx.GetBan(ctx, community, user)
	if err == nil && (ban.Expires == nil || *ban.Expires > now) {
		return errors.New("ds: Bot or participant is currently banned")
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

func currentInteractionDeviceList(ctx context.Context, tx store.Repository, verifier DeviceListVerifier, user store.UserRow, device store.DeviceRow) ([]mlswasi.DeviceListEntry, error) {
	list, err := tx.GetDeviceList(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	entries, err := verifier.DeviceListEntries(ctx, list.Blob, user.SSKPub, user.ID[:])
	if err != nil {
		return nil, err
	}
	if interactionDeviceMatchesSignedList(device, entries) {
		return entries, nil
	}
	return nil, errors.New("device is absent from current signed list")
}

// EligibleHumanInteractionDevice is the shared kind-3 participant device
// predicate. Human device trust comes from the current verified signed list;
// VerifiedAt is reserved for Bot native-device attestation and is not set on
// ordinary human browser or native signup devices.
func EligibleHumanInteractionDevice(user id.ID, device store.DeviceRow, entries []mlswasi.DeviceListEntry) bool {
	if !ActiveHumanInteractionDevice(user, device) {
		return false
	}
	return interactionDeviceMatchesSignedList(device, entries)
}

// ActiveHumanInteractionDevice checks that an enrolled human device belongs
// to the account and remains active. Signed-list membership is checked by
// EligibleHumanInteractionDevice after signature verification.
func ActiveHumanInteractionDevice(user id.ID, device store.DeviceRow) bool {
	return !user.IsZero() && !device.ID.IsZero() && device.UserID == user && len(device.DSKPub) == 32 && device.Tier <= 1 && device.RevokedAt == nil && device.QuarantinedAt == nil
}

func interactionDeviceMatchesSignedList(device store.DeviceRow, entries []mlswasi.DeviceListEntry) bool {
	for _, entry := range entries {
		if bytes.Equal(entry.DeviceID, device.ID[:]) && !entry.Revoked && entry.Tier == device.Tier && bytes.Equal(entry.DSKPub, device.DSKPub) {
			return true
		}
	}
	return false
}

func hashInteractionRegistration(r RegisterRequest) ([32]byte, error) {
	body, err := cborx.Marshal([]any{r.GroupID, r.Binding, r.GroupInfo, r.RatchetTree, r.InitialWelcome})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(body), nil
}

func cloneRegisterRequest(r RegisterRequest) RegisterRequest {
	r.Binding = append([]byte(nil), r.Binding...)
	r.GroupInfo = append([]byte(nil), r.GroupInfo...)
	r.RatchetTree = append([]byte(nil), r.RatchetTree...)
	r.InitialWelcome = append([]byte(nil), r.InitialWelcome...)
	r.Session.TokenHash = append([]byte(nil), r.Session.TokenHash...)
	return r
}

func cloneDeviceListEntries(in []mlswasi.DeviceListEntry) []mlswasi.DeviceListEntry {
	out := make([]mlswasi.DeviceListEntry, len(in))
	for i, entry := range in {
		out[i] = entry
		out[i].DeviceID = append([]byte(nil), entry.DeviceID...)
		out[i].DSKPub = append([]byte(nil), entry.DSKPub...)
	}
	return out
}

func equalDeviceListEntries(a, b []mlswasi.DeviceListEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i].DeviceID, b[i].DeviceID) || !bytes.Equal(a[i].DSKPub, b[i].DSKPub) || a[i].Tier != b[i].Tier || a[i].Revoked != b[i].Revoked {
			return false
		}
	}
	return true
}

func sameRegistration(a, b RegisterRequest) bool {
	return a.GroupID == b.GroupID && bytes.Equal(a.Binding, b.Binding) && bytes.Equal(a.GroupInfo, b.GroupInfo) && bytes.Equal(a.RatchetTree, b.RatchetTree) && bytes.Equal(a.InitialWelcome, b.InitialWelcome)
}

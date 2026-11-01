package ds

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/jonasthim/dilla/internal/store/sqlite/migrations"
)

type authorityDeviceLists map[id.ID][]mlswasi.DeviceListEntry

func (authorityDeviceLists) Entries(context.Context, DeviceListVerifier, id.ID) ([]auth.ListedDevice, error) {
	return nil, nil
}
func (v authorityDeviceLists) Verify(_ context.Context, _ []byte, _ []byte, user id.ID) ([]mlswasi.DeviceListEntry, error) {
	return cloneDeviceListEntries(v[user]), nil
}

func (v authorityDeviceLists) DeviceListEntries(_ context.Context, _ []byte, _ []byte, user []byte) ([]mlswasi.DeviceListEntry, error) {
	var userID id.ID
	copy(userID[:], user)
	return cloneDeviceListEntries(v[userID]), nil
}

type authorityTxStub struct{ store.Repository }

func TestPrepareKind3PreflightEvidenceBindsAdmissionAndRegistration(t *testing.T) {
	tuple, admission, leaves, botList, participantList := validInteractionRosterFixture()
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	wire := interactionReceiptWire{Version: admission.Version, AdmissionID: admission.AdmissionID, InstanceID: tuple.InstanceID, InstallationID: tuple.InstallationID, CommunityID: tuple.CommunityID, BotUserID: tuple.BotUserID, BotDeviceID: tuple.BotDeviceID, ParticipantUserID: tuple.ParticipantUserID, ParticipantDeviceID: tuple.ParticipantDeviceID, GroupID: tuple.GroupID, EligibilityRevision: admission.EligibilityRevision, NotAfter: admission.NotAfter}
	payload, err := cborx.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(private, append([]byte(interactionAdmissionDomain), payload...))
	binding := interactionBindingForTest(t, tuple)
	registration := RegisterRequest{GroupID: tuple.GroupID, Binding: binding, GroupInfo: []byte{1}, RatchetTree: []byte{2}}
	preflight := InteractionRegistrationPreflight{Expected: tuple, MinimumEligibilityRevision: 1, ExpectedPolicyVersion: 1, ExpectedE2EEVersion: 1, ReceiptPayload: payload, ReceiptSignature: signature, PinnedInstanceKey: private.Public().(ed25519.PublicKey), BotDeviceList: botList, ParticipantDeviceList: participantList, Now: 100, Registration: registration}
	extractor := rosterExtractorFunc(func(context.Context, []byte, []byte) (AuthenticatedInteractionRegistration, error) {
		roster := make([]mlswasi.RosterLeaf, len(leaves))
		for i, leaf := range leaves {
			roster[i] = mlswasi.RosterLeaf{UserID: leaf.UserID[:], DeviceID: leaf.DeviceID[:], DSKPub: leaf.DSKPub, Tier: leaf.Tier, Kind: leaf.Kind}
		}
		return AuthenticatedInteractionRegistration{GroupID: tuple.GroupID[:], Binding: binding, LeafCount: 2, Roster: roster}, nil
	})
	evidence, err := PrepareKind3PreflightEvidence(context.Background(), extractor, preflight)
	if err != nil {
		t.Fatalf("prepare evidence: %v", err)
	}
	if evidence.admissionID != admission.AdmissionID || evidence.tuple != tuple {
		t.Fatal("evidence lost the admitted identity")
	}
	registration.GroupInfo[0] ^= 1
	if evidence.registration.GroupInfo[0] != 1 {
		t.Fatal("evidence retained caller-owned mutable request bytes")
	}

	// The opaque evidence rejects substitution before any repository read.
	session := auth.Session{UserID: tuple.BotUserID, DeviceID: tuple.BotDeviceID, TokenHash: []byte{1}, Bot: &auth.BotSessionBinding{InstallationID: tuple.InstallationID, CommunityID: tuple.CommunityID, State: store.BotInstallationReady}}
	request := evidence.registration
	request.Session = session
	d := &DS{opts: Options{Clock: clock.NewFake(time.Unix(100, 0)), DeviceLists: authorityDeviceLists{}}}
	state := RegisteredKind3State{GroupID: tuple.GroupID, Binding: binding, LeafCount: 2, Generation: 1, Epoch: 1, NextSeq: 1}
	badTuple := tuple
	badTuple.ParticipantDeviceID = interactionID(101)
	if _, err := d.ValidateKind3RegistrationAuthority(context.Background(), authorityTxStub{}, authorityDeviceLists{}, session, tuple.InstallationID, interactionID(102), admission.AdmissionID, interactionID(103), badTuple, request, []byte{1}, state, evidence); err == nil {
		t.Fatal("validator accepted a participant device different from preflight")
	}
	if _, err := d.ValidateKind3RegistrationAuthority(context.Background(), authorityTxStub{}, authorityDeviceLists{}, session, interactionID(104), interactionID(102), admission.AdmissionID, interactionID(103), tuple, request, []byte{1}, state, evidence); err == nil {
		t.Fatal("validator accepted cross-installation authority")
	}
}

func TestPrepareKind3PreflightEvidenceRejectsExpiredProof(t *testing.T) {
	tuple, admission, _, botList, participantList := validInteractionRosterFixture()
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	wire := interactionReceiptWire{Version: admission.Version, AdmissionID: admission.AdmissionID, InstanceID: tuple.InstanceID, InstallationID: tuple.InstallationID, CommunityID: tuple.CommunityID, BotUserID: tuple.BotUserID, BotDeviceID: tuple.BotDeviceID, ParticipantUserID: tuple.ParticipantUserID, ParticipantDeviceID: tuple.ParticipantDeviceID, GroupID: tuple.GroupID, EligibilityRevision: admission.EligibilityRevision, NotAfter: 100}
	payload, _ := cborx.Marshal(wire)
	binding := interactionBindingForTest(t, tuple)
	preflight := InteractionRegistrationPreflight{Expected: tuple, MinimumEligibilityRevision: 1, ExpectedPolicyVersion: 1, ExpectedE2EEVersion: 1, ReceiptPayload: payload, ReceiptSignature: ed25519.Sign(private, append([]byte(interactionAdmissionDomain), payload...)), PinnedInstanceKey: private.Public().(ed25519.PublicKey), BotDeviceList: botList, ParticipantDeviceList: participantList, Now: 100, Registration: RegisterRequest{GroupID: tuple.GroupID, Binding: binding, GroupInfo: []byte{1}, RatchetTree: []byte{2}}}
	_, err := PrepareKind3PreflightEvidence(context.Background(), rosterExtractorFunc(func(context.Context, []byte, []byte) (AuthenticatedInteractionRegistration, error) {
		return AuthenticatedInteractionRegistration{GroupID: tuple.GroupID[:], Binding: binding, LeafCount: 2}, nil
	}), preflight)
	if err == nil {
		t.Fatal("expired admission proof produced authority evidence")
	}
}

func TestValidateKind3RegistrationAuthorityUsesCurrentReceiptAndGeneration(t *testing.T) {
	ctx := context.Background()
	repo := newAuthorityRepository(t)
	tuple, admission, leaves, botList, participantList := validInteractionRosterFixture()
	now := int64(100)
	privateSeed := [32]byte{0x5a}
	private := ed25519.NewKeyFromSeed(privateSeed[:])
	keys := InstanceKeys{InstanceID: tuple.InstanceID, ExternalSenderKeyID: interactionID(12), ExternalSenderPriv: privateSeed}
	if err := repo.CreateInstance(ctx, store.InstanceRow{InstanceID: keys.InstanceID, ExternalSenderKeyID: keys.ExternalSenderKeyID, KeyHistory: []byte{0x80}, FrankingKeyID: interactionID(13), Generation: 1, PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	verified := now
	for _, user := range []store.UserRow{
		{ID: tuple.BotUserID, Username: "authbot", Display: "Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now},
		{ID: tuple.ParticipantUserID, Username: "authhuman", Display: "Human", Kind: 0, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now},
	} {
		if err := repo.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.CreateCommunity(ctx, store.CommunityRow{ID: tuple.CommunityID, Owner: tuple.ParticipantUserID, Name: "authority", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	for _, device := range []store.DeviceRow{
		{ID: tuple.BotDeviceID, UserID: tuple.BotUserID, DSKPub: botList[0].DSKPub, Tier: botList[0].Tier, CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now},
		{ID: tuple.ParticipantDeviceID, UserID: tuple.ParticipantUserID, DSKPub: participantList[0].DSKPub, Tier: participantList[0].Tier, CredentialBlob: []byte{1}, Created: now},
	} {
		if err := repo.CreateDevice(ctx, device); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []id.ID{tuple.BotUserID, tuple.ParticipantUserID} {
		if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: tuple.CommunityID, UserID: user, Joined: now}); err != nil {
			t.Fatal(err)
		}
		if err := repo.PutDeviceList(ctx, store.DeviceListRow{UserID: user, Version: 1, Blob: []byte{0x80}, SSKSignature: make([]byte, 64), PrevHash: make([]byte, 32), Created: now}); err != nil {
			t.Fatal(err)
		}
	}
	runtimeID := interactionID(14)
	if err := repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtimeID, TokenHash: make([]byte, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: tuple.InstallationID, CommunityID: tuple.CommunityID, RuntimeID: &runtimeID, UserID: &tuple.BotUserID, DeviceID: &tuple.BotDeviceID, State: store.BotInstallationJoining, Revision: 1, ApprovedBy: tuple.ParticipantUserID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	tokenHash := make([]byte, 32)
	tokenHash[0] = 1
	if err := repo.CreateSession(ctx, store.SessionRow{TokenHash: tokenHash, DeviceID: tuple.BotDeviceID, UserID: tuple.BotUserID, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: tokenHash, InstallationID: tuple.InstallationID, UserID: tuple.BotUserID, DeviceID: tuple.BotDeviceID, Created: now}); err != nil {
		t.Fatal(err)
	}
	var revision uint64
	if err := repo.Tx(ctx, func(tx store.Repository) error {
		var err error
		revision, err = tx.(store.BotInteractionEligibility).GetBotInteractionEligibilityRevision(ctx, tuple.CommunityID, tuple.ParticipantUserID)
		return err
	}); err != nil || revision == 0 {
		t.Fatalf("eligibility revision=%d err=%v", revision, err)
	}
	requestID := interactionID(15)
	wire := interactionReceiptWire{Version: 1, AdmissionID: admission.AdmissionID, InstanceID: tuple.InstanceID, InstallationID: tuple.InstallationID, CommunityID: tuple.CommunityID, BotUserID: tuple.BotUserID, BotDeviceID: tuple.BotDeviceID, ParticipantUserID: tuple.ParticipantUserID, ParticipantDeviceID: tuple.ParticipantDeviceID, GroupID: tuple.GroupID, EligibilityRevision: revision, NotAfter: 200}
	payload, err := cborx.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(private, append([]byte(interactionAdmissionDomain), payload...))
	requestTuple, _ := cborx.Marshal([]any{tuple.GroupID, tuple.ParticipantUserID, tuple.ParticipantDeviceID})
	requestDigest := sha256.Sum256(requestTuple)
	issued := store.BotInteractionAdmissionReceipt{InstallationID: tuple.InstallationID, RequestID: requestID, RequestHash: requestDigest[:], Payload: payload, Signature: signature, NotAfter: 200}
	if err := repo.Tx(ctx, func(tx store.Repository) error {
		return tx.(store.BotInteractionAdmissionReceipts).CreateBotInteractionAdmissionReceipt(ctx, issued)
	}); err != nil {
		t.Fatal(err)
	}
	session := auth.Session{UserID: tuple.BotUserID, DeviceID: tuple.BotDeviceID, Scope: auth.ScopeEnrolled, TokenHash: tokenHash, Bot: &auth.BotSessionBinding{InstallationID: tuple.InstallationID, CommunityID: tuple.CommunityID, State: store.BotInstallationJoining}}
	binding := interactionBindingForTest(t, tuple)
	registration := RegisterRequest{Session: session, GroupID: tuple.GroupID, Binding: binding, GroupInfo: []byte{1}, RatchetTree: []byte{2}, InitialWelcome: []byte{3}}
	preflight := InteractionRegistrationPreflight{Expected: tuple, MinimumEligibilityRevision: revision, ExpectedPolicyVersion: 1, ExpectedE2EEVersion: 1, ReceiptPayload: payload, ReceiptSignature: signature, PinnedInstanceKey: private.Public().(ed25519.PublicKey), BotDeviceList: botList, ParticipantDeviceList: participantList, Now: uint64(now), Registration: registration}
	extractor := rosterExtractorFunc(func(context.Context, []byte, []byte) (AuthenticatedInteractionRegistration, error) {
		roster := make([]mlswasi.RosterLeaf, len(leaves))
		for i, leaf := range leaves {
			roster[i] = mlswasi.RosterLeaf{UserID: leaf.UserID[:], DeviceID: leaf.DeviceID[:], DSKPub: leaf.DSKPub, Tier: leaf.Tier, Kind: leaf.Kind}
		}
		return AuthenticatedInteractionRegistration{GroupID: tuple.GroupID[:], Binding: binding, LeafCount: 2, Roster: roster}, nil
	})
	evidence, err := PrepareKind3PreflightEvidence(ctx, extractor, preflight)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	body, err := cborx.Marshal([]any{registration.GroupID, registration.Binding, registration.GroupInfo, registration.RatchetTree, registration.InitialWelcome})
	if err != nil {
		t.Fatal(err)
	}
	registered := RegisteredKind3State{GroupID: tuple.GroupID, Binding: binding, LeafCount: 2, Generation: 1, Epoch: 1, NextSeq: 1}
	d := &DS{opts: Options{Store: repo, Clock: clock.NewFake(time.Unix(now, 0)), Keys: keys, DeviceLists: authorityDeviceLists{tuple.BotUserID: botList, tuple.ParticipantUserID: participantList}}}
	validate := func() (VerifiedKind3Registration, error) {
		var verifiedResult VerifiedKind3Registration
		err := repo.Tx(ctx, func(tx store.Repository) error {
			var err error
			verifiedResult, err = d.ValidateKind3RegistrationAuthority(ctx, tx, authorityDeviceLists{tuple.BotUserID: botList, tuple.ParticipantUserID: participantList}, session, tuple.InstallationID, requestID, admission.AdmissionID, interactionID(16), tuple, registration, body, registered, evidence)
			return err
		})
		return verifiedResult, err
	}
	verifiedReceipt, err := validate()
	if err != nil {
		t.Fatalf("valid final authority refused: %v", err)
	}
	domainBody := append([]byte("dilla bot group registration v1\x00"), body...)
	wantBodyHash := sha256.Sum256(domainBody)
	if string(verifiedReceipt.Receipt.BodySHA256) != string(wantBodyHash[:]) {
		t.Fatal("registration receipt hash omitted the versioned domain")
	}
	for _, epoch := range []uint64{0, 2} {
		registered.Epoch = epoch
		if _, err := validate(); err == nil {
			t.Fatalf("epoch %d passed initial kind-3 authority validation", epoch)
		}
	}
	registered.Epoch = 1
	if err := repo.Tx(ctx, func(tx store.Repository) error { return tx.SetGeneration(ctx, 2) }); err != nil {
		t.Fatal(err)
	}
	if _, err := validate(); err == nil {
		t.Fatal("stale registered generation passed final authority validation")
	}
}

func newAuthorityRepository(t *testing.T) store.Repository {
	t.Helper()
	db, err := sqlite.OpenWrite(t.TempDir() + "/authority.db")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, sqlitemigrations.FS)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	repo := sqlite.New(db, db)
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func (authorityDeviceLists) WithVerification(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

package ds_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

type kind3RegistrarMetadata struct {
	BindingHex              string `json:"binding_cbor_hex"`
	GroupIDHex              string `json:"group_id_hex"`
	InstanceIDHex           string `json:"instance_id_hex"`
	BotUserIDHex            string `json:"bot_user_id_hex"`
	BotDeviceIDHex          string `json:"bot_device_id_hex"`
	BotDSKPublicHex         string `json:"bot_dsk_public_hex"`
	BotSSKPublicHex         string `json:"bot_ssk_public_hex"`
	InstallationIDHex       string `json:"installation_id_hex"`
	CommunityIDHex          string `json:"community_id_hex"`
	ParticipantUserIDHex    string `json:"participant_user_id_hex"`
	ParticipantDeviceIDHex  string `json:"participant_device_id_hex"`
	ParticipantDSKPublicHex string `json:"participant_dsk_public_hex"`
	ParticipantSSKPublicHex string `json:"participant_ssk_public_hex"`
	PolicyVersion           uint64 `json:"policy_version"`
}

type kind3AdmissionWire struct {
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

type kind3CredentialIdentity struct {
	_          struct{} `cbor:",toarray"`
	Version    uint64
	UMKPub     []byte
	UserID     []byte
	DeviceID   []byte
	Kind       uint64
	Tier       uint64
	SignerTier uint64
	SSKPub     []byte
	SigUMKSSK  []byte
	SigSSKDev  []byte
}

type kind3RegistrationFixture struct {
	h       *dsHarness
	request ds.BotInteractionRegisterRequest
	meta    kind3RegistrarMetadata
}

func newKind3RegistrationFixture(t *testing.T) kind3RegistrationFixture {
	t.Helper()
	ctx := context.Background()
	h := newDSHarness(t)
	root := filepath.Join("dstest", "testdata", "kind3-interaction")
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		return b
	}
	var meta kind3RegistrarMetadata
	if err := json.Unmarshal(read("metadata.json"), &meta); err != nil {
		t.Fatal(err)
	}
	decodeID := func(name, value string) id.ID {
		b, err := hex.DecodeString(value)
		if err != nil || len(b) != 16 {
			t.Fatalf("fixture %s ID is invalid", name)
		}
		var out id.ID
		copy(out[:], b)
		return out
	}
	decode := func(name, value string) []byte {
		b, err := hex.DecodeString(value)
		if err != nil {
			t.Fatalf("fixture %s: %v", name, err)
		}
		return b
	}
	tuple := ds.InteractionAdmissionTuple{
		InstanceID: decodeID("instance", meta.InstanceIDHex), InstallationID: decodeID("installation", meta.InstallationIDHex),
		CommunityID: decodeID("community", meta.CommunityIDHex), BotUserID: decodeID("Bot user", meta.BotUserIDHex),
		BotDeviceID: decodeID("Bot device", meta.BotDeviceIDHex), ParticipantUserID: decodeID("participant user", meta.ParticipantUserIDHex),
		ParticipantDeviceID: decodeID("participant device", meta.ParticipantDeviceIDHex), GroupID: decodeID("group", meta.GroupIDHex),
	}
	now := h.clk.Now().Unix()
	verified := now
	botSSK, participantSSK := decode("Bot SSK", meta.BotSSKPublicHex), decode("participant SSK", meta.ParticipantSSKPublicHex)
	botCredential, participantCredential := read("bot_credential.cbor"), read("participant_credential.cbor")
	var botIdentity, participantIdentity kind3CredentialIdentity
	if err := cborx.Unmarshal(botCredential, &botIdentity); err != nil {
		t.Fatalf("decode Bot credential: %v", err)
	}
	if err := cborx.Unmarshal(participantCredential, &participantIdentity); err != nil {
		t.Fatalf("decode participant credential: %v", err)
	}
	botList, participantList := read("bot_device_list.cbor"), read("participant_device_list.cbor")
	for _, user := range []store.UserRow{
		{ID: tuple.BotUserID, Username: "k3bot" + tuple.BotUserID.String()[:8], Display: "Kind 3 Bot", Kind: 1, UMKPub: botIdentity.UMKPub, SSKPub: botSSK, SigUMKSSK: botIdentity.SigUMKSSK, Created: now},
		{ID: tuple.ParticipantUserID, Username: "k3human" + tuple.ParticipantUserID.String()[:8], Display: "Kind 3 Human", Kind: 0, UMKPub: participantIdentity.UMKPub, SSKPub: participantSSK, SigUMKSSK: participantIdentity.SigUMKSSK, Created: now},
	} {
		if err := h.repo.CreateUser(ctx, user); err != nil {
			t.Fatalf("create user: %v", err)
		}
	}
	for _, device := range []store.DeviceRow{
		{ID: tuple.BotDeviceID, UserID: tuple.BotUserID, DSKPub: decode("Bot DSK", meta.BotDSKPublicHex), CredentialBlob: botCredential, VerifiedAt: &verified, Created: now, LastSeen: now},
		{ID: tuple.ParticipantDeviceID, UserID: tuple.ParticipantUserID, DSKPub: decode("participant DSK", meta.ParticipantDSKPublicHex), CredentialBlob: participantCredential, VerifiedAt: &verified, Created: now, LastSeen: now},
	} {
		if err := h.repo.CreateDevice(ctx, device); err != nil {
			t.Fatalf("create device: %v", err)
		}
	}
	if err := h.repo.CreateCommunity(ctx, store.CommunityRow{ID: tuple.CommunityID, Owner: tuple.ParticipantUserID, Name: "kind3", PolicyJSON: []byte(`{}`), PolicyVersion: meta.PolicyVersion, Created: now}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []id.ID{tuple.BotUserID, tuple.ParticipantUserID} {
		if err := h.repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: tuple.CommunityID, UserID: user, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		user id.ID
		blob []byte
	}{{tuple.BotUserID, botList}, {tuple.ParticipantUserID, participantList}} {
		if len(item.blob) < ed25519.SignatureSize {
			t.Fatal("device-list fixture is too short")
		}
		if err := h.repo.PutDeviceList(ctx, store.DeviceListRow{UserID: item.user, Version: 1, Blob: item.blob, SSKSignature: item.blob[len(item.blob)-ed25519.SignatureSize:], PrevHash: make([]byte, 32), Created: now}); err != nil {
			t.Fatal(err)
		}
	}
	runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: bytes.Repeat([]byte{0x41}, 32), Created: now, Rotated: now}
	if err := h.repo.CreateBotRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: tuple.InstallationID, CommunityID: tuple.CommunityID, RuntimeID: &runtime.ID, UserID: &tuple.BotUserID, DeviceID: &tuple.BotDeviceID, State: store.BotInstallationJoining, Revision: 1, ApprovedBy: tuple.ParticipantUserID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("kind3 registration session"))
	if err := h.repo.CreateSession(ctx, store.SessionRow{TokenHash: tokenHash[:], DeviceID: tuple.BotDeviceID, UserID: tuple.BotUserID, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: tokenHash[:], InstallationID: tuple.InstallationID, UserID: tuple.BotUserID, DeviceID: tuple.BotDeviceID, Created: now}); err != nil {
		t.Fatal(err)
	}
	var eligibilityRevision uint64
	if err := h.repo.Tx(ctx, func(tx store.Repository) error {
		var err error
		eligibilityRevision, err = tx.(store.BotInteractionEligibility).GetBotInteractionEligibilityRevision(ctx, tuple.CommunityID, tuple.ParticipantUserID)
		return err
	}); err != nil || eligibilityRevision == 0 {
		t.Fatalf("eligibility revision=%d err=%v", eligibilityRevision, err)
	}
	requestID, admissionID, operationID := id.New(), id.New(), id.New()
	keys := testInstanceKeys(t)
	instanceKey := ed25519.NewKeyFromSeed(keys.ExternalSenderPriv[:])
	notAfter := uint64(now + 60)
	wire := kind3AdmissionWire{Version: 1, AdmissionID: admissionID, InstanceID: tuple.InstanceID, InstallationID: tuple.InstallationID, CommunityID: tuple.CommunityID, BotUserID: tuple.BotUserID, BotDeviceID: tuple.BotDeviceID, ParticipantUserID: tuple.ParticipantUserID, ParticipantDeviceID: tuple.ParticipantDeviceID, GroupID: tuple.GroupID, EligibilityRevision: eligibilityRevision, NotAfter: notAfter}
	payload, err := cborx.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(instanceKey, append([]byte("dilla bot interaction admission v1\x00"), payload...))
	requestTuple, err := cborx.Marshal([]any{tuple.GroupID, tuple.ParticipantUserID, tuple.ParticipantDeviceID})
	if err != nil {
		t.Fatal(err)
	}
	requestHash := sha256.Sum256(requestTuple)
	if err := h.repo.Tx(ctx, func(tx store.Repository) error {
		return tx.(store.BotInteractionAdmissionReceipts).CreateBotInteractionAdmissionReceipt(ctx, store.BotInteractionAdmissionReceipt{InstallationID: tuple.InstallationID, RequestID: requestID, RequestHash: requestHash[:], Payload: payload, Signature: signature, NotAfter: int64(notAfter)})
	}); err != nil {
		t.Fatal(err)
	}
	inst, err := h.wasm.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	botEntries, err := inst.DeviceListEntries(ctx, botList, botSSK, tuple.BotUserID[:])
	if err != nil {
		inst.Release()
		t.Fatal(err)
	}
	participantEntries, err := inst.DeviceListEntries(ctx, participantList, participantSSK, tuple.ParticipantUserID[:])
	keyPackage := read("participant_keypackage.mls")
	kpInfo, kpErr := inst.ValidateKeyPackage(ctx, keyPackage)
	inst.Release()
	if err != nil {
		t.Fatal(err)
	}
	if kpErr != nil || !bytes.Equal(kpInfo.DeviceID, tuple.ParticipantDeviceID[:]) {
		t.Fatalf("participant KeyPackage fixture: %+v, %v", kpInfo, kpErr)
	}
	if err := h.repo.PutKeyPackages(ctx, tuple.ParticipantDeviceID, []store.KeyPackageRow{{DeviceID: tuple.ParticipantDeviceID, KPRef: kpInfo.KPRef, Blob: keyPackage, Expires: int64(kpInfo.NotAfter), Created: now}}); err != nil {
		t.Fatal(err)
	}
	tree, groupInfo := read("ratchet_tree.mls"), read("group_info.mls")
	registration := ds.RegisterRequest{GroupID: tuple.GroupID, Binding: decode("v2 binding", meta.BindingHex), GroupInfo: groupInfo, RatchetTree: tree, InitialWelcome: read("welcome.mls")}
	body, err := cborx.Marshal([]any{registration.GroupID, registration.Binding, registration.GroupInfo, registration.RatchetTree, registration.InitialWelcome})
	if err != nil {
		t.Fatal(err)
	}
	session := auth.Session{UserID: tuple.BotUserID, DeviceID: tuple.BotDeviceID, Scope: auth.ScopeEnrolled, WireVersion: 3, TokenHash: tokenHash[:], Bot: &auth.BotSessionBinding{InstallationID: tuple.InstallationID, CommunityID: tuple.CommunityID, State: store.BotInstallationJoining}}
	preflight := ds.InteractionRegistrationPreflight{Expected: tuple, MinimumEligibilityRevision: eligibilityRevision, ExpectedPolicyVersion: meta.PolicyVersion, ExpectedE2EEVersion: 1, ExpectedMediaVersion: 0, ReceiptPayload: payload, ReceiptSignature: signature, PinnedInstanceKey: instanceKey.Public().(ed25519.PublicKey), BotDeviceList: botEntries, ParticipantDeviceList: participantEntries, Now: uint64(now), Registration: registration}
	return kind3RegistrationFixture{h: h, meta: meta, request: ds.BotInteractionRegisterRequest{Session: session, InstallationID: tuple.InstallationID, OperationID: operationID, ExpectedGeneration: 1, AdmissionRequestID: requestID, AdmissionID: admissionID, RegistrationBody: body, Registration: registration, TrustedPreflight: preflight}}
}

func TestBotInteractionRegistrationAtomicallyAcceptsAndRecoversExpiredProof(t *testing.T) {
	fixture := newKind3RegistrationFixture(t)
	ctx := context.Background()
	receipt, created, err := fixture.h.ds.RegisterBotInteractionGroup(ctx, fixture.request)
	if err != nil || !created {
		t.Fatalf("first registration created=%v err=%v", created, err)
	}
	groupID := receipt.GroupID
	group, err := fixture.h.repo.GetGroup(ctx, groupID)
	if err != nil || group.Kind != 3 {
		t.Fatalf("persisted group=%+v err=%v", group, err)
	}
	members, err := fixture.h.repo.ListMembers(ctx, groupID)
	if err != nil || len(members) != 2 {
		t.Fatalf("initial members=%d err=%v", len(members), err)
	}
	welcomeRows, err := fixture.h.repo.ListWelcomes(ctx, fixture.request.TrustedPreflight.Expected.ParticipantDeviceID, 0, 10)
	if err != nil || len(welcomeRows) != 1 || welcomeRows[0].GroupID != groupID || welcomeRows[0].Epoch != 1 || welcomeRows[0].CommitSeq != 0 || !bytes.Equal(welcomeRows[0].Blob, fixture.request.Registration.InitialWelcome) {
		t.Fatalf("initial Welcome was not atomically queued: rows=%d err=%v", len(welcomeRows), err)
	}
	operation, err := fixture.h.repo.GetBotGroupRegistrationReceipt(ctx, fixture.request.InstallationID, fixture.request.OperationID)
	if err != nil || operation.GroupID != groupID {
		t.Fatalf("operation receipt=%+v err=%v", operation, err)
	}
	consumed, err := fixture.h.repo.GetBotGroupRegistrationAdmission(ctx, fixture.request.InstallationID, fixture.request.AdmissionRequestID)
	if err != nil || consumed.OperationID != fixture.request.OperationID || consumed.AcceptedAt != operation.AcceptedAt {
		t.Fatalf("consumed admission=%+v err=%v", consumed, err)
	}

	fixture.h.clk.Advance(61)
	compactAt := int64(fixture.request.TrustedPreflight.Now) + 60 + 30*24*60*60 + 1
	if err := fixture.h.repo.Tx(ctx, func(tx store.Repository) error {
		return tx.(store.BotInteractionAdmissionReceipts).CompactExpiredBotInteractionAdmissionReceipts(ctx, fixture.request.InstallationID, compactAt)
	}); err != nil {
		t.Fatalf("compact expired admission payload: %v", err)
	}
	fixture.h.restartDS()
	replayIntent := fixture.request
	replayIntent.TrustedPreflight = ds.InteractionRegistrationPreflight{}
	replayOnly, found, replayErr := fixture.h.ds.ReplayBotInteractionGroup(ctx, replayIntent)
	if replayErr != nil || !found || replayOnly != receipt {
		t.Fatalf("receipt-only expired-proof replay=%+v found=%v err=%v", replayOnly, found, replayErr)
	}
	replayed, created, err := fixture.h.ds.RegisterBotInteractionGroup(ctx, fixture.request)
	if err != nil || created || replayed != receipt {
		t.Fatalf("expired-proof retry receipt=%+v created=%v err=%v", replayed, created, err)
	}
	if got := fixture.h.countRows(t, "mls_welcomes"); got != 1 {
		t.Fatalf("retry produced %d Welcomes, want one", got)
	}
	if _, err := fixture.h.ds.Info(ctx, groupID, fixture.request.Session); err != nil {
		t.Fatalf("recovered group state unavailable: %v", err)
	}

	conflict := fixture.request
	conflict.OperationID = fixture.request.OperationID
	conflict.Registration.RatchetTree = append(bytes.Clone(fixture.request.Registration.RatchetTree), 0)
	conflict.RegistrationBody, err = cborx.Marshal([]any{conflict.Registration.GroupID, conflict.Registration.Binding, conflict.Registration.GroupInfo, conflict.Registration.RatchetTree, conflict.Registration.InitialWelcome})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.h.ds.ReplayBotInteractionGroup(ctx, conflict); err == nil {
		t.Fatal("receipt-only replay accepted a changed registration body")
	}
	if _, _, err := fixture.h.ds.RegisterBotInteractionGroup(ctx, conflict); err == nil {
		t.Fatal("same operation with a changed registration body succeeded")
	}
	changedWelcome := fixture.request
	changedWelcome.Registration.InitialWelcome = append(bytes.Clone(fixture.request.Registration.InitialWelcome), 0)
	changedWelcome.RegistrationBody, err = cborx.Marshal([]any{changedWelcome.Registration.GroupID, changedWelcome.Registration.Binding,
		changedWelcome.Registration.GroupInfo, changedWelcome.Registration.RatchetTree, changedWelcome.Registration.InitialWelcome})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.h.ds.ReplayBotInteractionGroup(ctx, changedWelcome); err == nil {
		t.Fatal("receipt-only replay accepted a changed initial Welcome")
	}
}

func TestBotInteractionRegistrationWithSingleWASMInstance(t *testing.T) {
	fixture := newKind3RegistrationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wasm, err := mlswasi.New(ctx, loadWasmForDS(t), mlswasi.Options{PoolSize: 1, CacheDir: sharedWasmCacheDir(t), Now: fixture.h.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wasm.Close(context.Background()) })
	fixture.h.ds, err = ds.New(ds.Options{Store: fixture.h.repo, BotCapability: capability.New(fixture.h.repo), Wasm: wasm, Gateway: fixture.h.gw,
		Clock: fixture.h.clk, Keys: testInstanceKeys(t), Policy: ds.DefaultPolicy(), Channels: fixture.h.channels,
		ACL: fixture.h.acl, PostPolicy: fixture.h.postPolicy})
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := fixture.h.ds.RegisterBotInteractionGroup(ctx, fixture.request); err != nil || !created {
		t.Fatalf("single-instance registration created=%v err=%v", created, err)
	}
}

func TestReadyBotInteractionSendPassesTheExactCurrentRegistrationGate(t *testing.T) {
	fixture := newKind3RegistrationFixture(t)
	ctx := context.Background()
	registered, created, err := fixture.h.ds.RegisterBotInteractionGroup(ctx, fixture.request)
	if err != nil || !created {
		t.Fatalf("register interaction group created=%v err=%v", created, err)
	}
	installation, err := fixture.h.repo.GetBotInstallation(ctx, fixture.request.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	installation.State = store.BotInstallationReady
	installation.Revision++
	installation.Updated = fixture.h.clk.Now().Unix()
	if err := fixture.h.repo.UpdateBotInstallation(ctx, installation.ID, installation.Revision-1, installation); err != nil {
		t.Fatalf("make registered installation Ready: %v", err)
	}
	session := fixture.request.Session
	session.Bot.State = store.BotInstallationReady
	group, err := fixture.h.repo.GetGroup(ctx, registered.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := fixture.h.repo.GetInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.h.ds.AcceptBotMessage(ctx, ds.BotMessageRequest{
		Session: session, InstallationID: installation.ID, OperationID: id.New(), AttemptID: id.New(),
		CommunityID: fixture.request.TrustedPreflight.Expected.CommunityID, GroupID: registered.GroupID,
		ExpectedGeneration: instance.Generation, Epoch: group.Epoch, Class: 1, Ciphertext: []byte{0x01},
	})
	var refusal *ds.Error
	if !errors.As(err, &refusal) || refusal.Code != "E_COMMITMENT_INVALID" {
		t.Fatalf("valid current kind-3 registration did not pass the authorization gate before ciphertext validation: err=%v", err)
	}
}

func TestBotInteractionRegistrationRollsBackWelcomeWithGroup(t *testing.T) {
	fixture := newKind3RegistrationFixture(t)
	fixture.h.failNextTx("PutWelcomes")
	if _, _, err := fixture.h.ds.RegisterBotInteractionGroup(context.Background(), fixture.request); err == nil {
		t.Fatal("injected Welcome write failure succeeded")
	}
	if _, err := fixture.h.repo.GetGroup(context.Background(), fixture.request.Registration.GroupID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("group after Welcome failure: %v", err)
	}
	if got := fixture.h.countRows(t, "mls_welcomes"); got != 0 {
		t.Fatalf("Welcome failure retained %d outbox rows", got)
	}
	if _, err := fixture.h.repo.GetBotGroupRegistrationReceipt(context.Background(), fixture.request.InstallationID, fixture.request.OperationID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("operation receipt after Welcome failure: %v", err)
	}
}

func TestBotInteractionRegistrationRefusesMissingInitialWelcome(t *testing.T) {
	fixture := newKind3RegistrationFixture(t)
	fixture.request.Registration.InitialWelcome = nil
	fixture.request.RegistrationBody, _ = cborx.Marshal([]any{fixture.request.Registration.GroupID, fixture.request.Registration.Binding,
		fixture.request.Registration.GroupInfo, fixture.request.Registration.RatchetTree})
	if _, _, err := fixture.h.ds.RegisterBotInteractionGroup(context.Background(), fixture.request); err == nil {
		t.Fatal("kind-3 registration without initial Welcome succeeded")
	}
	if _, err := fixture.h.repo.GetGroup(context.Background(), fixture.request.Registration.GroupID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing Welcome wrote group: %v", err)
	}
}

func TestBotInteractionRegistrationRefusesWelcomeForUnpublishedKeyPackage(t *testing.T) {
	fixture := newKind3RegistrationFixture(t)
	ctx := context.Background()
	inst, err := fixture.h.wasm.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := inst.WelcomeKeyPackageRefs(ctx, fixture.request.Registration.InitialWelcome)
	inst.Release()
	if err != nil || len(refs) != 1 {
		t.Fatalf("fixture Welcome refs=%d err=%v", len(refs), err)
	}
	welcome := bytes.Clone(fixture.request.Registration.InitialWelcome)
	offset := bytes.Index(welcome, refs[0])
	if offset < 0 {
		t.Fatal("fixture Welcome ref not found in TLS wire")
	}
	copy(welcome[offset:offset+32], bytes.Repeat([]byte{0xa5}, 32))
	fixture.request.Registration.InitialWelcome = welcome
	fixture.request.TrustedPreflight.Registration.InitialWelcome = welcome
	fixture.request.RegistrationBody, err = cborx.Marshal([]any{fixture.request.Registration.GroupID, fixture.request.Registration.Binding,
		fixture.request.Registration.GroupInfo, fixture.request.Registration.RatchetTree, welcome})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.h.ds.RegisterBotInteractionGroup(ctx, fixture.request); err == nil {
		t.Fatal("Welcome for an unpublished KeyPackage was accepted")
	}
	if _, err := fixture.h.repo.GetGroup(ctx, fixture.request.Registration.GroupID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("invalid Welcome recipient wrote group: %v", err)
	}
	if got := fixture.h.countRows(t, "mls_welcomes"); got != 0 {
		t.Fatalf("invalid Welcome recipient queued %d Welcomes", got)
	}
}

func TestBotInteractionRegistrationRollsBackAllRowsOnGroupWriteFailure(t *testing.T) {
	fixture := newKind3RegistrationFixture(t)
	fixture.h.failNextTx("CreateGroup")
	if _, _, err := fixture.h.ds.RegisterBotInteractionGroup(context.Background(), fixture.request); err == nil {
		t.Fatal("injected group write failure succeeded")
	}
	if _, err := fixture.h.repo.GetGroup(context.Background(), fixture.request.Registration.GroupID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("group after rollback: %v", err)
	}
	if _, err := fixture.h.repo.GetBotGroupRegistrationReceipt(context.Background(), fixture.request.InstallationID, fixture.request.OperationID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("operation receipt after rollback: %v", err)
	}
	if _, err := fixture.h.repo.GetBotGroupRegistrationAdmission(context.Background(), fixture.request.InstallationID, fixture.request.AdmissionRequestID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("consumption after rollback: %v", err)
	}
	if _, created, err := fixture.h.ds.RegisterBotInteractionGroup(context.Background(), fixture.request); err != nil || !created {
		t.Fatalf("retry after rollback created=%v err=%v", created, err)
	}
}

func TestBotInteractionRegistrationRechecksEligibilityInsideAcceptanceTransaction(t *testing.T) {
	fixture := newKind3RegistrationFixture(t)
	now := fixture.h.clk.Now().Unix()
	// The authority test exercises the real ban fence using this fixture's participant.
	meta := fixture.meta
	community := mustFixtureID(t, meta.CommunityIDHex)
	participant := mustFixtureID(t, meta.ParticipantUserIDHex)
	if err := fixture.h.repo.PutBan(context.Background(), store.BanRow{CommunityID: community, UserID: participant, ByUser: participant, Created: now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.h.ds.RegisterBotInteractionGroup(context.Background(), fixture.request); err == nil {
		t.Fatal("registration passed after participant eligibility changed")
	}
	if _, err := fixture.h.repo.GetGroup(context.Background(), fixture.request.Registration.GroupID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("eligibility refusal wrote group: %v", err)
	}
}

func mustFixtureID(t *testing.T, value string) id.ID {
	t.Helper()
	b, err := hex.DecodeString(value)
	if err != nil || len(b) != 16 {
		t.Fatal("invalid fixture ID")
	}
	var out id.ID
	copy(out[:], b)
	return out
}

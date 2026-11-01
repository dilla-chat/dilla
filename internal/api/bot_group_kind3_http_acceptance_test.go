package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/capability"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/jonasthim/dilla/internal/store/sqlite/migrations"
)

const kind3APIFixtureDir = "../ds/dstest/testdata/kind3-interaction"

type kind3APIAdmission struct {
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

type kind3APICredential struct {
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

type kind3APIMetadata struct {
	InstanceID          string `json:"instance_id_hex"`
	BotUserID           string `json:"bot_user_id_hex"`
	BotDeviceID         string `json:"bot_device_id_hex"`
	BotDSK              string `json:"bot_dsk_public_hex"`
	BotSSK              string `json:"bot_ssk_public_hex"`
	InstallationID      string `json:"installation_id_hex"`
	CommunityID         string `json:"community_id_hex"`
	ParticipantUserID   string `json:"participant_user_id_hex"`
	ParticipantDeviceID string `json:"participant_device_id_hex"`
	ParticipantDSK      string `json:"participant_dsk_public_hex"`
	ParticipantSSK      string `json:"participant_ssk_public_hex"`
	BindingHex          string `json:"binding_cbor_hex"`
	PolicyVersion       uint64 `json:"policy_version"`
}

type fixedKind3InstanceRepo struct{ store.Repository }

func (r fixedKind3InstanceRepo) CreateInstance(ctx context.Context, row store.InstanceRow) error {
	for i := range row.InstanceID {
		row.InstanceID[i] = 0x11
	}
	for i := range row.ExternalSenderKeyID {
		row.ExternalSenderKeyID[i] = 0x22
	}
	for i := range row.FrankingKeyID {
		row.FrankingKeyID[i] = 0x33
	}
	return r.Repository.CreateInstance(ctx, row)
}

func newKind3HTTPAPI(t *testing.T) (*groupsAPI, kind3APIMetadata, map[string][]byte) {
	t.Helper()
	ctx := context.Background()
	var repo store.Repository
	if dsn := os.Getenv("DILLA_TEST_PG"); dsn != "" {
		repo = botRegistrationPostgresRepo(t, dsn)
	} else {
		path := filepath.Join(t.TempDir(), "kind3-api.db")
		write, err := sqlite.OpenWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		p, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Up(ctx); err != nil {
			t.Fatal(err)
		}
		read, err := sqlite.OpenRead(path)
		if err != nil {
			t.Fatal(err)
		}
		sqliteRepo := sqlite.New(write, read)
		repo = sqliteRepo
		t.Cleanup(func() { _ = sqliteRepo.Close() })
	}
	repo = fixedKind3InstanceRepo{repo}
	handler, deps := newTestAPIFullWithRepo(t, repo, nil, nil)
	mux, ok := handler.(*server.Mux)
	if !ok {
		t.Fatalf("test API mux type=%T", handler)
	}
	f := apiFixtureData(t)
	clk := clock.NewFake(time.Unix(time.Now().Unix(), 0))
	wasm, err := mlswasi.New(ctx, f.wasm, mlswasi.Options{PoolSize: 2, CacheDir: apiWasmCacheDir(t), Now: clk.Now})
	if err != nil {
		t.Fatalf("start real WASI: %v", err)
	}
	t.Cleanup(func() { _ = wasm.Close(context.Background()) })
	botCapability := capability.New(deps.Repo)
	gw := gateway.New(gateway.Options{Clock: clk, Store: apiListenStore{apiGatewayStore{deps.Repo}}, Generation: 1,
		Auth: deps.Sessions, WireVersions: []uint64{3, 2}, E2EEVersions: []uint64{1}, MediaVersions: []uint64{1},
		ScopeResolver: apiGatewayScopeResolver{repo: deps.Repo}, CommunityCompatibility: botCapability})
	t.Cleanup(func() { _ = gw.Shutdown(context.Background()) })
	keys := ds.InstanceKeys{}
	for i := range keys.InstanceID {
		keys.InstanceID[i] = 0x11
	}
	for i := range keys.ExternalSenderKeyID {
		keys.ExternalSenderKeyID[i] = 0x22
	}
	for i := range keys.FrankingKeyID {
		keys.FrankingKeyID[i] = 0x33
	}
	keys.ExternalSenderPriv = fixtureExternalSenderKey()
	for i := range keys.FrankingKey {
		keys.FrankingKey[i] = byte(0x80 + i)
	}
	delivery, err := ds.New(ds.Options{Store: deps.Repo, Wasm: wasm, Gateway: gw, Clock: clk, BotCapability: botCapability,
		Policy: ds.DefaultPolicy(), Keys: keys, Channels: openChannels{}, DeviceLists: ds.NewDeviceLists(deps.Repo, wasm)})
	if err != nil {
		t.Fatalf("ds.New: %v", err)
	}
	t.Cleanup(func() { _ = delivery.Shutdown(context.Background()) })
	groups := &api.Groups{DS: delivery, MaxBody: 1 << 21}
	groups.Register(mux, deps.Sessions)
	groups.RegisterSequencer(mux, deps.Sessions)
	groups.RegisterRecovery(mux, deps.Sessions)
	groups.RegisterHeal(mux, deps.Sessions)
	user, _, sessionToken := seedAPISession(t, deps)
	h := &groupsAPI{mux: mux, deps: deps, ds: delivery, groups: groups, groupID: f.groupID, fixture: f,
		session: sessionToken, user: user.ID, clk: clk, wasm: wasm, keys: keys, gw: gw}
	// Align the API and delivery-service clocks so a freshly signed proof is fresh to both.
	if delta := h.clk.Now().Sub(h.deps.Clock.Now()); delta > 0 {
		h.deps.Clock.(interface{ Advance(time.Duration) }).Advance(delta)
	}

	readFixture := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(kind3APIFixtureDir, name))
		if err != nil {
			t.Fatalf("read kind-3 fixture %s: %v", name, err)
		}
		return b
	}
	var meta kind3APIMetadata
	if err := json.Unmarshal(readFixture("metadata.json"), &meta); err != nil {
		t.Fatal(err)
	}
	decode := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	decodeID := func(s string) id.ID {
		var out id.ID
		b := decode(s)
		if len(b) != len(out) {
			t.Fatalf("invalid fixture ID %q", s)
		}
		copy(out[:], b)
		return out
	}
	botUser, botDevice := decodeID(meta.BotUserID), decodeID(meta.BotDeviceID)
	participant, participantDevice := decodeID(meta.ParticipantUserID), decodeID(meta.ParticipantDeviceID)
	community, installation, groupID := decodeID(meta.CommunityID), decodeID(meta.InstallationID), mustFixtureGroupID(t, readFixture("metadata.json"))
	botCredential, participantCredential := readFixture("bot_credential.cbor"), readFixture("participant_credential.cbor")
	var botIdentity, participantIdentity kind3APICredential
	if err := cborx.Unmarshal(botCredential, &botIdentity); err != nil {
		t.Fatal(err)
	}
	if err := cborx.Unmarshal(participantCredential, &participantIdentity); err != nil {
		t.Fatal(err)
	}
	botSSK, participantSSK := decode(meta.BotSSK), decode(meta.ParticipantSSK)
	botDSK, participantDSK := decode(meta.BotDSK), decode(meta.ParticipantDSK)
	now := h.deps.Clock.Now().Unix()
	verified := now
	for _, user := range []store.UserRow{
		{ID: botUser, Username: "k3bot" + botUser.String()[:8], Display: "Kind 3 Bot", Kind: 1, UMKPub: botIdentity.UMKPub, SSKPub: botSSK, SigUMKSSK: botIdentity.SigUMKSSK, Created: now},
		{ID: participant, Username: "k3human" + participant.String()[:8], Display: "Kind 3 Human", Kind: 0, UMKPub: participantIdentity.UMKPub, SSKPub: participantSSK, SigUMKSSK: participantIdentity.SigUMKSSK, Created: now},
	} {
		if err := h.deps.Repo.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	for _, device := range []store.DeviceRow{
		{ID: botDevice, UserID: botUser, DSKPub: botDSK, CredentialBlob: botCredential, VerifiedAt: &verified, Created: now, LastSeen: now},
		// Browser signup devices are trusted through their current signed list;
		// they intentionally have no VerifiedAt stamp.
		{ID: participantDevice, UserID: participant, DSKPub: participantDSK, CredentialBlob: participantCredential, Created: now, LastSeen: now},
	} {
		if err := h.deps.Repo.CreateDevice(ctx, device); err != nil {
			t.Fatal(err)
		}
	}
	inst, err := h.wasm.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keyPackage := readFixture("participant_keypackage.mls")
	kpInfo, kpErr := inst.ValidateKeyPackage(ctx, keyPackage)
	inst.Release()
	if kpErr != nil || !bytes.Equal(kpInfo.DeviceID, participantDevice[:]) {
		t.Fatalf("participant KeyPackage fixture: %+v, %v", kpInfo, kpErr)
	}
	if err := h.deps.Repo.PutKeyPackages(ctx, participantDevice, []store.KeyPackageRow{{DeviceID: participantDevice, KPRef: kpInfo.KPRef, Blob: keyPackage, Expires: int64(kpInfo.NotAfter), Created: now}}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: community, Owner: participant, Name: "kind3-http", PolicyJSON: []byte(`{}`), PolicyVersion: meta.PolicyVersion, Created: now}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []id.ID{botUser, participant} {
		if err := h.deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: user, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	for _, list := range []struct {
		user id.ID
		name string
	}{{botUser, "bot_device_list.cbor"}, {participant, "participant_device_list.cbor"}} {
		blob := readFixture(list.name)
		if len(blob) < ed25519.SignatureSize {
			t.Fatal("short signed device list fixture")
		}
		if err := h.deps.Repo.PutDeviceList(ctx, store.DeviceListRow{UserID: list.user, Version: 1, Blob: blob, SSKSignature: blob[len(blob)-ed25519.SignatureSize:], PrevHash: make([]byte, 32), Created: now}); err != nil {
			t.Fatal(err)
		}
	}
	runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: bytes.Repeat([]byte{0x41}, 32), Created: now, Rotated: now}
	if err := h.deps.Repo.CreateBotRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: community, RuntimeID: &runtime.ID, UserID: &botUser, DeviceID: &botDevice, State: store.BotInstallationJoining, Revision: 1, ApprovedBy: participant, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	token := "kind3-http-acceptance"
	tokenHash := auth.TokenHash(token)
	if err := h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateSession(ctx, store.SessionRow{TokenHash: tokenHash, DeviceID: botDevice, UserID: botUser, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
			return err
		}
		return tx.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: tokenHash, InstallationID: installation, UserID: botUser, DeviceID: botDevice, Created: now})
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Sessions.RecordCapabilities(ctx, tokenHash, 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	instance, err := h.deps.Repo.GetInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.deps.Instance = instance
	externalSenderSeed := fixtureExternalSenderKey()
	externalSenderKey := ed25519.NewKeyFromSeed(externalSenderSeed[:])
	h.deps.ExternalSenderPub = externalSenderKey.Public().(ed25519.PublicKey)
	h.deps.DeviceLists = ds.NewDeviceLists(h.deps.Repo, h.wasm)
	h.deps.SignBotCurrentInteractionContext = func(payload []byte) ([]byte, error) {
		return ed25519.Sign(externalSenderKey, append([]byte("dilla bot current interaction context v1\x00"), payload...)), nil
	}
	h.groups.Deps = h.deps
	h.groups.LoadBotInteractionPreflight = h.deps.LoadBotInteractionRegistrationPreflight

	if _, ok := h.deps.Repo.(store.BotInteractionEligibility); !ok {
		t.Fatal("repository lacks interaction eligibility")
	}
	var revision uint64
	if err := h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		var e error
		revision, e = tx.(store.BotInteractionEligibility).GetBotInteractionEligibilityRevision(ctx, community, participant)
		return e
	}); err != nil || revision == 0 {
		t.Fatalf("eligibility revision=%d err=%v", revision, err)
	}
	requestID, admissionID := id.New(), id.New()
	notAfter := uint64(h.deps.Clock.Now().Unix() + 60)
	payload, err := cborx.Marshal(kind3APIAdmission{Version: 1, AdmissionID: admissionID, InstanceID: instance.InstanceID, InstallationID: installation, CommunityID: community, BotUserID: botUser, BotDeviceID: botDevice, ParticipantUserID: participant, ParticipantDeviceID: participantDevice, GroupID: groupID, EligibilityRevision: revision, NotAfter: notAfter})
	if err != nil {
		t.Fatal(err)
	}
	signer := externalSenderKey
	sig := ed25519.Sign(signer, append([]byte("dilla bot interaction admission v1\x00"), payload...))
	requestBytes, err := cborx.Marshal([]any{groupID, participant, participantDevice})
	if err != nil {
		t.Fatal(err)
	}
	requestHash := sha256.Sum256(requestBytes)
	if err := h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		return tx.(store.BotInteractionAdmissionReceipts).CreateBotInteractionAdmissionReceipt(ctx, store.BotInteractionAdmissionReceipt{InstallationID: installation, RequestID: requestID, RequestHash: requestHash[:], Payload: payload, Signature: sig, NotAfter: int64(notAfter)})
	}); err != nil {
		t.Fatal(err)
	}
	return h, meta, map[string][]byte{"token": []byte(token), "request": requestID[:], "admission": admissionID[:], "registration": mustCBOR(t, []any{groupID, decode(meta.BindingHex), readFixture("group_info.mls"), readFixture("ratchet_tree.mls"), readFixture("welcome.mls")})}
}

func mustFixtureGroupID(t *testing.T, metadata []byte) id.ID {
	t.Helper()
	var m struct {
		Group string `json:"group_id_hex"`
	}
	if err := json.Unmarshal(metadata, &m); err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(m.Group)
	if err != nil || len(b) != 16 {
		t.Fatal("invalid group id in fixture")
	}
	var out id.ID
	copy(out[:], b)
	return out
}

func TestBotInteractionHTTPGroupRegistrationAcceptanceReplayAndRefusals(t *testing.T) {
	h, meta, fixture := newKind3HTTPAPI(t)
	ctx := context.Background()
	token := string(fixture["token"])
	requestID, admissionID := id.ID{}, id.ID{}
	copy(requestID[:], fixture["request"])
	copy(admissionID[:], fixture["admission"])
	groupID := mustFixtureGroupID(t, mustReadKind3Metadata(t))
	instance, err := h.deps.Repo.GetInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	operation := id.New()
	path := "/v1/bot-runtime/installations/" + meta.InstallationID + "/groups"
	body := mustCBOR(t, []any{operation, instance.Generation, fixture["registration"], requestID})
	var hookCalls atomic.Int32
	h.groups.AfterRegister = func(context.Context, id.ID) { hookCalls.Add(1) }
	created := h.do(t, http.MethodPost, path, token, body)
	if created.Code != http.StatusCreated {
		_, groupErr := h.deps.Repo.GetGroup(ctx, groupID)
		t.Fatalf("kind-3 HTTP registration: %d %x, want 201; group lookup after failure: %v", created.Code, created.Body.Bytes(), groupErr)
	}
	if _, err := h.deps.Repo.GetGroup(ctx, groupID); err != nil {
		t.Fatalf("accepted group missing: %v", err)
	}
	members, err := h.deps.Repo.ListMembers(ctx, groupID)
	if err != nil || len(members) != 2 {
		t.Fatalf("authenticated roster=%d err=%v, want two members", len(members), err)
	}
	welcomes, err := h.deps.Repo.ListWelcomes(ctx, idFromHex(t, meta.ParticipantDeviceID), 0, 10)
	if err != nil || len(welcomes) != 1 || welcomes[0].GroupID != groupID || welcomes[0].Epoch != 1 {
		t.Fatalf("initial participant Welcome=%d err=%v", len(welcomes), err)
	}
	if err := h.groups.Drain(ctx); err != nil {
		t.Fatalf("drain after acceptance: %v", err)
	}
	if hookCalls.Load() != 0 {
		t.Fatalf("AfterRegister ran for kind-3 interaction: %d calls", hookCalls.Load())
	}
	// Only the exact participant device from the accepted v2 binding can obtain a
	// signed snapshot, and the envelope verifies under the currently published key.
	participant := idFromHex(t, meta.ParticipantUserID)
	participantDevice := idFromHex(t, meta.ParticipantDeviceID)
	humanToken := "kind3-human-context"
	humanHash := auth.TokenHash(humanToken)
	now := h.deps.Clock.Now().Unix()
	if err := h.deps.Repo.CreateSession(ctx, store.SessionRow{TokenHash: humanHash, DeviceID: participantDevice, UserID: participant, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Sessions.RecordCapabilities(ctx, humanHash, 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	contextPath := "/v1/groups/" + groupID.String() + "/interaction-admission-context"
	contextResponse := h.do(t, http.MethodGet, contextPath, humanToken, nil)
	if contextResponse.Code != http.StatusOK {
		t.Fatalf("human current context: %d %x", contextResponse.Code, contextResponse.Body.Bytes())
	}
	var envelope []any
	if err := cborx.Unmarshal(contextResponse.Body.Bytes(), &envelope); err != nil || len(envelope) != 2 {
		t.Fatalf("context envelope: %v (%d items)", err, len(envelope))
	}
	payload, ok := envelope[0].([]byte)
	if !ok {
		t.Fatalf("context payload type %T", envelope[0])
	}
	signature, ok := envelope[1].([]byte)
	if !ok || !ed25519.Verify(h.deps.ExternalSenderPub, append([]byte("dilla bot current interaction context v1\x00"), payload...), signature) {
		t.Fatalf("current context signature invalid")
	}
	var proof []any
	if err := cborx.Unmarshal(payload, &proof); err != nil || len(proof) != 18 || proof[0] != uint64(1) {
		t.Fatalf("current context proof invalid: %v (%d fields)", err, len(proof))
	}
	if _, _, wrongHuman := seedAPISession(t, h.deps); h.do(t, http.MethodGet, contextPath, wrongHuman, nil).Code < 400 {
		t.Fatal("nonparticipant human obtained current interaction context")
	}
	if h.do(t, http.MethodGet, contextPath, token, nil).Code < 400 {
		t.Fatal("managed Bot session obtained human current interaction context")
	}
	// A regular channel group can have the same participant and community, but it
	// has no accepted kind-3 registration. It must not obtain the signed proof that
	// authorizes accepting an interaction Welcome.
	ordinaryGroupID := id.New()
	ordinaryCommunityID := idFromHex(t, meta.CommunityID)
	if err := h.deps.Repo.CreateGroup(ctx, store.GroupRow{
		GroupID: ordinaryGroupID, Binding: []byte{0x80}, Kind: 0, CommunityID: &ordinaryCommunityID,
		TargetID: id.New(), Ciphersuite: 1, Epoch: 1, ExternalSenderKeyID: id.New(),
		E2EEVersion: 1, MediaVersion: 1, PolicyVersion: meta.PolicyVersion, Created: now,
	}); err != nil {
		t.Fatalf("create ordinary Welcome group: %v", err)
	}
	ordinaryContextPath := "/v1/groups/" + ordinaryGroupID.String() + "/interaction-admission-context"
	ordinaryContext := h.do(t, http.MethodGet, ordinaryContextPath, humanToken, nil)
	if ordinaryContext.Code != http.StatusForbidden {
		t.Fatalf("ordinary group current context: %d %x, want 403", ordinaryContext.Code, ordinaryContext.Body.Bytes())
	}
	var ordinaryRefusal []any
	if err := cborx.Unmarshal(ordinaryContext.Body.Bytes(), &ordinaryRefusal); err != nil || len(ordinaryRefusal) != 3 ||
		ordinaryRefusal[0] != string(server.CodeForbidden) || ordinaryRefusal[1] != "current interaction context unavailable" || ordinaryRefusal[2] != nil {
		t.Fatalf("ordinary group current context refusal: %v (%v), want E_FORBIDDEN/current interaction context unavailable", ordinaryRefusal, err)
	}
	readyInstallationID := idFromHex(t, meta.InstallationID)
	installRow, err := h.deps.Repo.GetBotInstallation(ctx, readyInstallationID)
	if err != nil {
		t.Fatal(err)
	}
	// Re-enable retains this installation and its accepted private groups, but
	// transitions back through Joining. Private restore/read APIs remain Ready-only;
	// runtime membership/readiness must finish before it attempts either surface.
	joiningCursor, joiningCursorErr := h.deps.Repo.GetCursor(ctx, idFromHex(t, meta.BotDeviceID), groupID)
	joiningBase := "/v1/bot-runtime/installations/" + readyInstallationID.String() + "/interaction-groups/" + groupID.String()
	for _, suffix := range []string{"/heal", "/info", "/messages?from=1&limit=1", "/handshakes?from=1&limit=1"} {
		if got := h.do(t, http.MethodGet, joiningBase+suffix, token, nil); got.Code != http.StatusForbidden {
			t.Fatalf("Joining installation private read %s: %d, want 403: %x", suffix, got.Code, got.Body.Bytes())
		}
	}
	if got := h.do(t, http.MethodPost, joiningBase+"/cursor", token, mustCBOR(t, []any{uint64(0), uint64(0)})).Code; got != http.StatusForbidden {
		t.Fatalf("Joining installation private acknowledgement: %d, want 403", got)
	}
	if cursor, err := h.deps.Repo.GetCursor(ctx, idFromHex(t, meta.BotDeviceID), groupID); !errors.Is(err, joiningCursorErr) || cursor != joiningCursor {
		t.Fatalf("denied Joining source changed cursor: %+v %v; before %+v %v", cursor, err, joiningCursor, joiningCursorErr)
	}
	installRevision := installRow.Revision
	installRow.State = store.BotInstallationReady
	installRow.Revision++
	installRow.Updated = h.deps.Clock.Now().Unix()
	if err := h.deps.Repo.UpdateBotInstallation(ctx, readyInstallationID, installRevision, installRow); err != nil {
		t.Fatalf("set fixture installation ready: %v", err)
	}
	h.deps.CurrentBotInteractionGroupContext = func(ctx context.Context, group id.ID) (ds.CurrentBotInteractionGroupContext, error) {
		return h.ds.CurrentBotInteractionGroupContext(ctx, group)
	}
	installationID := readyInstallationID
	botGroupBase := "/v1/bot-runtime/installations/" + installationID.String() + "/interaction-groups/" + groupID.String()
	botGroupInfoPath := botGroupBase + "/info"
	if got := h.do(t, http.MethodGet, botGroupBase+"/heal", token, nil).Code; got != http.StatusOK {
		t.Fatalf("same-install registered Bot heal status: %d", got)
	}
	if got := h.do(t, http.MethodGet, botGroupInfoPath, token, nil); got.Code != http.StatusOK {
		t.Fatalf("current registered Bot interaction group read: %d, want 200: %x", got.Code, got.Body.Bytes())
	}
	for _, suffix := range []string{"/messages?from=1&limit=1", "/handshakes?from=1&limit=1"} {
		if got := h.do(t, http.MethodGet, botGroupBase+suffix, token, nil).Code; got != http.StatusOK {
			t.Fatalf("dedicated group page %s: %d, want 200", suffix, got)
		}
	}
	if got := h.do(t, http.MethodPost, botGroupBase+"/cursor", token, mustCBOR(t, []any{uint64(0), uint64(0)})).Code; got != http.StatusNoContent {
		t.Fatalf("dedicated group cursor: %d, want 204", got)
	}
	if got := h.do(t, http.MethodGet, "/v1/bot-runtime/installations/"+id.New().String()+"/interaction-groups/"+groupID.String()+"/info", token, nil).Code; got < 400 {
		t.Fatalf("foreign installation read accepted: %d", got)
	}
	unregisteredGroup := id.New()
	communityID := idFromHex(t, meta.CommunityID)
	if err := h.deps.Repo.CreateGroup(ctx, store.GroupRow{GroupID: unregisteredGroup, Binding: []byte{0x80}, Kind: 3,
		CommunityID: &communityID, TargetID: id.New(), Ciphersuite: 1, ExternalSenderKeyID: id.New(),
		E2EEVersion: 1, PolicyVersion: meta.PolicyVersion, Created: h.deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if got := h.do(t, http.MethodGet, "/v1/bot-runtime/installations/"+installationID.String()+"/interaction-groups/"+unregisteredGroup.String()+"/info", token, nil).Code; got < 400 {
		t.Fatalf("unregistered kind-3 group read accepted: %d", got)
	}
	if got := h.do(t, http.MethodGet, "/v1/bot-runtime/installations/"+installationID.String()+"/interaction-groups/"+unregisteredGroup.String()+"/heal", token, nil).Code; got < 400 {
		t.Fatalf("unregistered kind-3 group heal accepted: %d", got)
	}
	if got := h.do(t, http.MethodGet, "/v1/groups/"+groupID.String()+"/info", token, nil).Code; got < 400 {
		t.Fatalf("generic kind-3 read remained enabled: %d", got)
	}
	if got := h.do(t, http.MethodPost, "/v1/groups/"+groupID.String()+"/message", token, mustCBOR(t, []any{})).Code; got < 400 {
		t.Fatalf("Bot wrote directly to private interaction group: %d", got)
	}
	if err := h.ds.OnRestore(ctx, instance.Generation+1); err != nil {
		t.Fatalf("restore before Bot heal status: %v", err)
	}
	healStatus := h.do(t, http.MethodGet, botGroupBase+"/heal", token, nil)
	if healStatus.Code != http.StatusOK {
		t.Fatalf("same-install Bot could not read restored-group heal status: %d %x", healStatus.Code, healStatus.Body.Bytes())
	}
	var healFields []any
	if err := cborx.Unmarshal(healStatus.Body.Bytes(), &healFields); err != nil || len(healFields) != 5 || healFields[4] != uint64(1) {
		t.Fatalf("restored Bot heal status = %v, err=%v; want five fields with needs_heal=1", healFields, err)
	}
	if got := h.do(t, http.MethodGet, "/v1/bot-runtime/installations/"+id.New().String()+"/interaction-groups/"+groupID.String()+"/heal", token, nil).Code; got < 400 {
		t.Fatalf("foreign installation heal status accepted: %d", got)
	}
	groupRow, err := h.deps.Repo.GetGroup(ctx, groupID)
	if err != nil {
		t.Fatal(err)
	}
	healed := h.do(t, http.MethodPost, botGroupBase+"/heal", token, mustCBOR(t, []any{groupRow.GroupInfoBlob, []any{}, nil}))
	if healed.Code != http.StatusOK {
		t.Fatalf("same-install Bot heal: %d %x, want 200", healed.Code, healed.Body.Bytes())
	}
	if err := h.deps.Repo.DeleteMember(ctx, idFromHex(t, meta.CommunityID), idFromHex(t, meta.ParticipantUserID)); err != nil {
		t.Fatal(err)
	}
	if got := h.do(t, http.MethodGet, botGroupInfoPath, token, nil).Code; got < 400 {
		t.Fatalf("interaction group remained readable after participant removal: %d", got)
	}
	if got := h.do(t, http.MethodGet, botGroupBase+"/heal", token, nil).Code; got != http.StatusForbidden {
		t.Fatalf("current private group heal accepted removed participant: %d, want 403", got)
	}
	if err := h.deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: idFromHex(t, meta.CommunityID), UserID: idFromHex(t, meta.ParticipantUserID), Joined: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.RevokeDevice(ctx, idFromHex(t, meta.ParticipantDeviceID), now+1); err != nil {
		t.Fatal(err)
	}
	if got := h.do(t, http.MethodGet, botGroupInfoPath, token, nil).Code; got < 400 {
		t.Fatalf("interaction group remained readable after participant device revocation: %d", got)
	}

	// Expire and compact the server proof. A new valid Bot session then exercises only the durable
	// operation+admission-consumption replay path through HTTP.
	h.deps.Clock.(interface{ Advance(time.Duration) }).Advance(31 * 24 * time.Hour)
	h.clk.Advance(31 * 24 * time.Hour)
	compactAt := h.deps.Clock.Now().Unix()
	if err := h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		return tx.(store.BotInteractionAdmissionReceipts).CompactExpiredBotInteractionAdmissionReceipts(ctx, idFromHex(t, meta.InstallationID), compactAt)
	}); err != nil {
		t.Fatal(err)
	}
	newToken := "kind3-http-replay-after-compaction"
	newHash := auth.TokenHash(newToken)
	now = h.deps.Clock.Now().Unix()
	if err := h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateSession(ctx, store.SessionRow{TokenHash: newHash, DeviceID: idFromHex(t, meta.BotDeviceID), UserID: idFromHex(t, meta.BotUserID), Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
			return err
		}
		return tx.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: newHash, InstallationID: idFromHex(t, meta.InstallationID), UserID: idFromHex(t, meta.BotUserID), DeviceID: idFromHex(t, meta.BotDeviceID), Created: now})
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Sessions.RecordCapabilities(ctx, newHash, 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	replay := h.do(t, http.MethodPost, path, newToken, body)
	if replay.Code != http.StatusOK || !bytes.Equal(replay.Body.Bytes(), created.Body.Bytes()) {
		t.Fatalf("exact replay after proof compaction: %d %x, want %x", replay.Code, replay.Body.Bytes(), created.Body.Bytes())
	}
	if err := h.groups.Drain(ctx); err != nil {
		t.Fatalf("drain after replay: %v", err)
	}
	if hookCalls.Load() != 0 {
		t.Fatalf("AfterRegister ran on kind-3 replay: %d calls", hookCalls.Load())
	}

	_, _, nonBotToken := seedAPISession(t, h.deps)
	receiptPath := path + "/" + operation.String()
	lookup := h.do(t, http.MethodGet, receiptPath, newToken, nil)
	if lookup.Code != http.StatusOK || !bytes.Equal(lookup.Body.Bytes(), created.Body.Bytes()) {
		t.Fatalf("receipt GET after proof compaction: %d %x, want exact accepted receipt %x", lookup.Code, lookup.Body.Bytes(), created.Body.Bytes())
	}
	unauthorizedLookup := h.do(t, http.MethodGet, receiptPath, nonBotToken, nil)
	if unauthorizedLookup.Code < 400 {
		t.Fatalf("non-Bot session read managed registration receipt: %d", unauthorizedLookup.Code)
	}
	crossInstallationLookup := h.do(t, http.MethodGet, "/v1/bot-runtime/installations/"+id.New().String()+"/groups/"+operation.String(), newToken, nil)
	if crossInstallationLookup.Code < 400 {
		t.Fatalf("cross-installation receipt GET accepted: %d", crossInstallationLookup.Code)
	}

	var registrationTuple []any
	if err := cborx.Unmarshal(fixture["registration"], &registrationTuple); err != nil || len(registrationTuple) != 5 {
		t.Fatalf("decode fixture registration: %v", err)
	}
	tree, ok := registrationTuple[3].([]byte)
	if !ok {
		t.Fatal("fixture registration tree is not bytes")
	}
	conflictingTree := append(append([]byte(nil), tree...), 0)
	conflictingRegistration := mustCBOR(t, []any{registrationTuple[0], registrationTuple[1], registrationTuple[2], conflictingTree, registrationTuple[4]})
	conflictingBody := h.do(t, http.MethodPost, path, newToken, mustCBOR(t, []any{operation, instance.Generation, conflictingRegistration, requestID}))
	if conflictingBody.Code < 400 {
		t.Fatalf("changed registration body accepted: %d", conflictingBody.Code)
	}
	conflictingOperation := h.do(t, http.MethodPost, path, newToken, mustCBOR(t, []any{id.New(), instance.Generation, fixture["registration"], requestID}))
	if conflictingOperation.Code < 400 {
		t.Fatalf("consumed admission under a different operation accepted: %d", conflictingOperation.Code)
	}
	wrongAdmission := id.New()
	conflictingAdmission := h.do(t, http.MethodPost, path, newToken, mustCBOR(t, []any{operation, instance.Generation, fixture["registration"], wrongAdmission}))
	if conflictingAdmission.Code < 400 {
		t.Fatalf("different admission request accepted: %d", conflictingAdmission.Code)
	}
	unauthorized := h.do(t, http.MethodPost, path, nonBotToken, body)
	if unauthorized.Code < 400 {
		t.Fatalf("non-Bot session accepted kind-3 registration: %d", unauthorized.Code)
	}
	crossInstallation := h.do(t, http.MethodPost, "/v1/bot-runtime/installations/"+id.New().String()+"/groups", newToken, body)
	if crossInstallation.Code < 400 {
		t.Fatalf("cross-installation registration accepted: %d", crossInstallation.Code)
	}
	if err := h.groups.Drain(ctx); err != nil {
		t.Fatalf("drain after refusals: %v", err)
	}
	if hookCalls.Load() != 0 {
		t.Fatalf("AfterRegister ran after refusals: %d calls", hookCalls.Load())
	}
}

func TestBotInteractionHTTPGroupRegistrationRejectsMalformedInitialWelcome(t *testing.T) {
	h, meta, fixture := newKind3HTTPAPI(t)
	var fields []any
	if err := cborx.Unmarshal(fixture["registration"], &fields); err != nil || len(fields) != 5 {
		t.Fatalf("fixture body: %v", err)
	}
	fields[4] = []byte{1, 2, 3}
	badRegistration := mustCBOR(t, fields)
	var requestID id.ID
	copy(requestID[:], fixture["request"])
	instance, err := h.deps.Repo.GetInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := h.do(t, http.MethodPost, "/v1/bot-runtime/installations/"+meta.InstallationID+"/groups", string(fixture["token"]),
		mustCBOR(t, []any{id.New(), instance.Generation, badRegistration, requestID}))
	if result.Code < 400 {
		t.Fatalf("malformed Welcome was accepted: %d", result.Code)
	}
	groupID := mustFixtureGroupID(t, mustReadKind3Metadata(t))
	if _, err := h.deps.Repo.GetGroup(context.Background(), groupID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("malformed Welcome wrote group: %v", err)
	}
}

func idFromHex(t *testing.T, s string) id.ID {
	t.Helper()
	x, err := id.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return x
}
func mustReadKind3Metadata(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(kind3APIFixtureDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

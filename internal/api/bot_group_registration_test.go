package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"net/http"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	dillaapi "github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	pgmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
)

func TestManagedBotGroupRegistrationReceiptSurvivesGenerationChange(t *testing.T) {
	testManagedBotGroupRegistrationReceipt(t, newGroupsAPIWith(t, func(r store.Repository) ds.Channels {
		return &revokeMemberAfterEarlyRegistrationACL{repo: r}
	}))
}

func TestManagedBotGroupRegistrationReceiptSurvivesGenerationChangePostgres(t *testing.T) {
	dsn := os.Getenv("DILLA_TEST_PG")
	if dsn == "" {
		t.Skip("DILLA_TEST_PG unset")
	}
	repo := botRegistrationPostgresRepo(t, dsn)
	h := newGroupsAPIWithRepository(t, repo, func(r store.Repository) ds.Channels {
		return &revokeMemberAfterEarlyRegistrationACL{repo: r}
	})
	testManagedBotGroupRegistrationReceipt(t, h)
}

type revokeMemberAfterEarlyRegistrationACL struct {
	repo      store.Repository
	done      bool
	community id.ID
	user      id.ID
}

func (c *revokeMemberAfterEarlyRegistrationACL) Channel(ctx context.Context, target id.ID) (uint8, uint8, error) {
	return (dillaapi.StructureChannels{Repo: c.repo}).Channel(ctx, target)
}

func (c *revokeMemberAfterEarlyRegistrationACL) MayRegister(ctx context.Context, user id.ID, binding ds.Binding) error {
	if err := (dillaapi.StructureChannels{Repo: c.repo}).MayRegister(ctx, user, binding); err != nil {
		return err
	}
	if !c.done {
		c.done = true
		c.community, c.user = *binding.CommunityID, user
		if err := c.repo.Tx(ctx, func(tx store.Repository) error {
			if err := tx.LockCommunity(ctx, c.community); err != nil {
				return err
			}
			return tx.DeleteMember(ctx, c.community, c.user)
		}); err != nil {
			return err
		}
	}
	return nil
}

func testManagedBotGroupRegistrationReceipt(t *testing.T, h *groupsAPI) {
	t.Helper()
	ctx := context.Background()
	f := apiRegistrationData(t)
	now := h.deps.Clock.Now().Unix()
	fixtureGroup := f.groups["community-one-leaf"]
	var fixtureBinding struct {
		_             struct{} `cbor:",toarray"`
		V             uint64
		InstanceID    id.ID
		CommunityID   *id.ID
		TargetID      id.ID
		Kind          uint8
		PolicyVersion uint64
		E2EEVersion   uint64
		MediaVersion  uint64
	}
	err := cborx.Unmarshal(fixtureGroup.binding, &fixtureBinding)
	if err != nil || fixtureBinding.CommunityID == nil {
		t.Fatalf("decode community registration fixture: %+v %v", fixtureBinding, err)
	}
	guest, err := h.wasm.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	publicGroup, err := guest.PublicGroupFromExternal(ctx, fixtureGroup.ratchetTree, fixtureGroup.groupInfo)
	if err != nil {
		guest.Release()
		t.Fatal(err)
	}
	groupState, err := publicGroup.State(ctx)
	if err != nil || len(groupState.Members) != 1 {
		_ = publicGroup.Close(ctx)
		guest.Release()
		t.Fatalf("read registration fixture leaf: %v", err)
	}
	verifiedCredential := append([]byte(nil), groupState.Members[0].CredentialIdentity...)
	_ = publicGroup.Close(ctx)
	guest.Release()
	ssk := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x74}, 32))
	entries := []apiListEntry{{DeviceID: f.deviceID[:], DSKPub: f.dsk, AddedAt: 1}}
	unsigned, err := cborx.Marshal(apiListUnsigned{V: 1, UserID: f.userID[:], Version: 1, PrevHash: make([]byte, 32), Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(ssk, append([]byte("dilla devices v1"), unsigned...))
	signed, err := cborx.Marshal(apiListSigned{V: 1, UserID: f.userID[:], Version: 1, PrevHash: make([]byte, 32), Entries: entries, SigSSK: sig})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateUser(ctx, store.UserRow{ID: f.userID, Username: "bot" + f.userID.String()[:8], Display: "Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: ssk.Public().(ed25519.PublicKey), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	verified := now
	if err := h.deps.Repo.CreateDevice(ctx, store.DeviceRow{ID: f.deviceID, UserID: f.userID, DSKPub: f.dsk, CredentialBlob: verifiedCredential, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.PutDeviceList(ctx, store.DeviceListRow{UserID: f.userID, Version: 1, Blob: signed, SSKSignature: sig, PrevHash: make([]byte, 32), Created: now}); err != nil {
		t.Fatal(err)
	}
	community := *fixtureBinding.CommunityID
	communityOwner, _, _ := seedAPIDevice(t, h.deps)
	if err := h.deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: community, Owner: communityOwner.ID, Name: "bot-registration", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: f.userID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	role := store.RoleRow{ID: id.New(), CommunityID: community, Name: "@everyone", Position: 0,
		Allow: uint64(dillaapi.PermViewChannel | dillaapi.PermConnect), Created: now}
	if err := h.deps.Repo.PutRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateChannel(ctx, store.ChannelRow{ID: fixtureBinding.TargetID, CommunityID: &community,
		Kind: dillaapi.ChannelText, Mode: dillaapi.ModeE2EE, Visibility: dillaapi.VisPrivate,
		SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: bytes.Repeat([]byte{1}, 32), Created: now, Rotated: now}
	if err := h.deps.Repo.CreateBotRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, RuntimeID: &runtime.ID, UserID: &f.userID, DeviceID: &f.deviceID, State: store.BotInstallationJoining, Revision: 1, ApprovedBy: f.userID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}
	if err := h.deps.Repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	var token string
	if err := h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		token = "test-managed-bot-registration-token"
		if err := tx.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(token), DeviceID: f.deviceID, UserID: f.userID, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
			return err
		}
		return tx.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(token), InstallationID: installation.ID, UserID: f.userID, DeviceID: f.deviceID, Created: now})
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.do(t, http.MethodPost, "/v1/sessions/negotiate", token, mustCBOR(t, []any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}})); got.Code != http.StatusOK {
		t.Fatalf("negotiate Bot session: %d %x", got.Code, got.Body.Bytes())
	}

	registrationBody, groupID := oneLeafBody(t, "community-one-leaf")
	operation := id.New()
	instance, err := h.deps.Repo.GetInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := mustCBOR(t, []any{operation, instance.Generation, registrationBody})
	path := "/v1/bot-runtime/installations/" + installation.ID.String() + "/groups"
	for _, badAdmissionID := range [][]byte{make([]byte, len(id.ID{})), []byte("short")} {
		bad := h.do(t, http.MethodPost, path, token, mustCBOR(t, []any{id.New(), instance.Generation, registrationBody, badAdmissionID}))
		if bad.Code != http.StatusBadRequest || errorCode(t, bad) != "E_INVALID_REQUEST" {
			t.Fatalf("malformed admission request id: %d %x, want 400 E_INVALID_REQUEST", bad.Code, bad.Body.Bytes())
		}
	}
	boundaryBody := mustCBOR(t, []any{groupID, []byte{}, []byte{}, make([]byte, 65_482)})
	if len(boundaryBody) != 65_505 {
		t.Fatalf("boundary body length = %d, want 65,505", len(boundaryBody))
	}
	boundary := h.do(t, http.MethodPost, path, token, mustCBOR(t, []any{id.New(), instance.Generation, boundaryBody}))
	if boundary.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("65,505-byte registration body was rejected by the 65,506-byte cap: %x", boundary.Body.Bytes())
	}
	overBody := mustCBOR(t, []any{groupID, []byte{}, []byte{}, make([]byte, 65_483)})
	if len(overBody) != 65_506 {
		t.Fatalf("over-boundary body length = %d, want 65,506", len(overBody))
	}
	over := h.do(t, http.MethodPost, path, token, mustCBOR(t, []any{id.New(), instance.Generation, overBody}))
	if over.Code != http.StatusRequestEntityTooLarge || errorCode(t, over) != "E_TOO_LARGE" {
		t.Fatalf("65,506-byte registration body: %d %x, want 413 E_TOO_LARGE", over.Code, over.Body.Bytes())
	}
	// The channel source grants the early ACL check, then revokes membership under
	// LockCommunity before the acceptance transaction begins. The real resolver's
	// transactional recheck below must observe that revocation and write nothing.
	permissionDenied := h.do(t, http.MethodPost, path, token, mustCBOR(t, []any{id.New(), instance.Generation, registrationBody}))
	if permissionDenied.Code != http.StatusForbidden || errorCode(t, permissionDenied) != "E_FORBIDDEN" {
		t.Fatalf("registration after real resolver permission revocation: %d %x, want 403 E_FORBIDDEN", permissionDenied.Code, permissionDenied.Body.Bytes())
	}
	if _, err := h.deps.Repo.GetGroup(ctx, groupID); err == nil {
		t.Fatal("permission-revoked registration wrote a group")
	}
	if err := h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockCommunity(ctx, community); err != nil {
			return err
		}
		return tx.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: f.userID, Joined: now})
	}); err != nil {
		t.Fatal(err)
	}
	created := h.do(t, http.MethodPost, path, token, request)
	if created.Code != http.StatusCreated {
		t.Fatalf("managed registration: %d %x", created.Code, created.Body.Bytes())
	}
	var receipt []any
	if err := cborx.Unmarshal(created.Body.Bytes(), &receipt); err != nil || len(receipt) != 6 {
		t.Fatalf("receipt tuple=%v err=%v", receipt, err)
	}
	if got, ok := receipt[0].([]byte); !ok || !bytes.Equal(got, operation[:]) {
		t.Fatalf("operation id in receipt = %v", receipt[0])
	}
	if got, ok := receipt[1].([]byte); !ok || !bytes.Equal(got, groupID[:]) {
		t.Fatalf("group id in receipt = %v", receipt[1])
	}
	if receipt[5] != uint64(1) {
		t.Fatalf("next_seq = %v, want 1", receipt[5])
	}
	hashInput := append([]byte("dilla bot group registration v1\x00"), registrationBody...)
	wantHash := sha256.Sum256(hashInput)
	if got, ok := receipt[2].([]byte); !ok || !bytes.Equal(got, wantHash[:]) {
		t.Fatalf("receipt hash = %x, want exact canonical-body hash %x", receipt[2], wantHash)
	}
	members, err := h.deps.Repo.ListMembers(ctx, groupID)
	if err != nil || len(members) != 1 || members[0].DeviceID != f.deviceID {
		t.Fatalf("atomic initial member set = %+v, %v", members, err)
	}

	if err := h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockInstanceGeneration(ctx); err != nil {
			return err
		}
		return tx.SetGeneration(ctx, instance.Generation+1)
	}); err != nil {
		t.Fatal(err)
	}
	var hookMu sync.Mutex
	var hooked []id.ID
	h.groups.AfterRegister = func(_ context.Context, got id.ID) {
		hookMu.Lock()
		hooked = append(hooked, got)
		hookMu.Unlock()
	}
	replay := h.do(t, http.MethodPost, path, token, request)
	if replay.Code != http.StatusOK || !bytes.Equal(replay.Body.Bytes(), created.Body.Bytes()) {
		t.Fatalf("exact replay after generation change: %d %x, want original %x", replay.Code, replay.Body.Bytes(), created.Body.Bytes())
	}
	if err := h.groups.Drain(ctx); err != nil {
		t.Fatalf("Drain after accepted replay: %v", err)
	}
	hookMu.Lock()
	if len(hooked) != 1 || hooked[0] != groupID {
		t.Fatalf("AfterRegister after accepted replay = %v, want [%s]", hooked, groupID)
	}
	hookMu.Unlock()
	lookup := h.do(t, http.MethodGet, path+"/"+operation.String(), token, nil)
	if lookup.Code != http.StatusOK || !bytes.Equal(lookup.Body.Bytes(), created.Body.Bytes()) {
		t.Fatalf("receipt lookup: %d %x, want %x", lookup.Code, lookup.Body.Bytes(), created.Body.Bytes())
	}
	if err := h.groups.Drain(ctx); err != nil {
		t.Fatalf("Drain after receipt lookup: %v", err)
	}
	hookMu.Lock()
	if len(hooked) != 1 {
		t.Fatalf("receipt lookup scheduled AfterRegister: %v", hooked)
	}
	hookMu.Unlock()
	changedGeneration := h.do(t, http.MethodPost, path, token, mustCBOR(t, []any{operation, instance.Generation + 1, registrationBody}))
	if changedGeneration.Code != http.StatusConflict || errorCode(t, changedGeneration) != "E_INVALID_REQUEST" {
		t.Fatalf("same operation with changed generation: %d %x, want 409 E_INVALID_REQUEST", changedGeneration.Code, changedGeneration.Body.Bytes())
	}
	if err := h.groups.Drain(ctx); err != nil {
		t.Fatalf("Drain after refused replay: %v", err)
	}
	hookMu.Lock()
	if len(hooked) != 1 {
		t.Fatalf("refused replay scheduled AfterRegister: %v", hooked)
	}
	hookMu.Unlock()
	foreign := h.do(t, http.MethodGet, "/v1/bot-runtime/installations/"+id.New().String()+"/groups/"+operation.String(), token, nil)
	if foreign.Code != http.StatusNotFound || errorCode(t, foreign) != "E_NOT_FOUND" {
		t.Fatalf("foreign-installation lookup: %d %x, want indistinguishable 404", foreign.Code, foreign.Body.Bytes())
	}
	refused := h.do(t, http.MethodPost, "/v1/groups", token, registrationBody)
	if refused.Code != http.StatusForbidden || errorCode(t, refused) != "E_FORBIDDEN" {
		t.Fatalf("generic registration for managed Bot: %d %x, want 403 E_FORBIDDEN", refused.Code, refused.Body.Bytes())
	}
	if got, err := h.deps.Repo.GetGroup(ctx, groupID); err != nil || got.GroupID != groupID {
		t.Fatalf("generic registration changed the accepted group: %+v %v", got, err)
	}
}

func botRegistrationPostgresRepo(t *testing.T, dsn string) store.Repository {
	t.Helper()
	admin, err := postgres.Open(dsn, 1, time.Hour)
	if err != nil {
		t.Fatalf("postgres Open: %v", err)
	}
	name := "dilla_t_" + id.New().String()[:16]
	if _, err := admin.ExecContext(context.Background(), `CREATE DATABASE `+name); err != nil {
		_ = admin.Close()
		t.Fatalf("CREATE DATABASE: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
		t.Fatalf("DILLA_TEST_PG is not a URL: %v", err)
	}
	u.Path = "/" + name
	db, err := postgres.Open(u.String(), 4, time.Hour)
	if err != nil {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
		t.Fatalf("postgres Open test database: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, pgmigrations.FS)
	if err != nil {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
		t.Fatalf("postgres provider: %v", err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		_ = db.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
		t.Fatalf("postgres migrations: %v", err)
	}
	repo := postgres.New(db)
	t.Cleanup(func() {
		_ = repo.Close()
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
	})
	return repo
}

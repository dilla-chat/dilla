package auth_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/jonasthim/dilla/internal/store/sqlite/migrations"
)

// authDBPaths lets corruptDSKPub reach the file behind a repository, which is
// the only way to write a row the schema's CHECK forbids.
var authDBPaths = map[store.Repository]string{}

type routeGroupFaultRepo struct {
	store.Repository
	groupID id.ID
}

func (r routeGroupFaultRepo) GetGroup(ctx context.Context, groupID id.ID) (store.GroupRow, error) {
	if groupID == r.groupID {
		return store.GroupRow{}, errors.New("private database failure detail")
	}
	return r.Repository.GetGroup(ctx, groupID)
}

func assertBotVersionError(t *testing.T, body []byte) {
	t.Helper()
	var fields []any
	if err := cborx.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 6 || fields[0] != "E_VERSION" || fields[2] != nil {
		t.Fatalf("version error fields = %v, want six-field E_VERSION with null retry slot", fields)
	}
	got, err := cborx.Marshal(fields[3:])
	if err != nil {
		t.Fatal(err)
	}
	want, err := cborx.Marshal([][]uint64{{3}, {1}, {1}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("supported versions = %x, want %x", got, want)
	}
}

// newSessions returns a Sessions over a migrated temporary database and the
// fake clock that drives it. The generation is 7, so a Token.Generation of 0 is
// a failure rather than a coincidence.
func newSessions(t *testing.T) (*auth.Sessions, store.Repository, *clock.Fake) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	write, err := sqlite.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatalf("up: %v", err)
	}
	read, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { repo.Close() })
	authDBPaths[repo] = path
	clk := clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	s := auth.NewSessions(repo, clk, config.Default().Auth.Session, id.New(), 7)
	// The device-list gate of protocol/02 § Device sessions item 4: a user the lister has no entry
	// for has published no list, which is the no-list clause every fixture here relies on.
	s.DeviceLists = newStaticLister()
	return s, repo, clk
}

// seedDevice creates a user and one native device, and returns the device's
// signing key. seedDeviceTier does the same at a chosen tier: 0 native,
// 1 browser.
func seedDevice(t *testing.T, repo store.Repository) (id.ID, id.ID, ed25519.PrivateKey) {
	t.Helper()
	return seedDeviceTier(t, repo, 0)
}

func seedDeviceTier(t *testing.T, repo store.Repository, tier uint8) (id.ID, id.ID, ed25519.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	u := store.UserRow{ID: id.New(), Username: "jonas" + id.New().String()[:8], Display: "Jonas",
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: 1}
	if err := repo.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	verified := int64(1)
	d := store.DeviceRow{ID: id.New(), UserID: u.ID, DSKPub: pub, Tier: tier, SignerTier: 0,
		CredentialBlob: []byte{1}, VerifiedAt: &verified, LastSeen: 1, Created: 1}
	if err := repo.CreateDevice(ctx, d); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return u.ID, d.ID, priv
}

// corruptDSKPub writes a wrong-length dsk_pub straight into the row. The
// schema's CHECK (length(dsk_pub) = 32) is expected to refuse, which is the
// stronger guarantee; the caller skips in that case.
func corruptDSKPub(repo store.Repository, device id.ID, n int) error {
	db, err := sql.Open("sqlite", "file:"+authDBPaths[repo])
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(context.Background(), `UPDATE devices SET dsk_pub = ? WHERE id = ?`, make([]byte, n), device[:])
	return err
}

// fakeAssertions is the enrolment-assertion store Sessions spends on the
// pending path. The concrete type lives in internal/api (task 9); Sessions
// takes the one-method interface, so a test needs no import cycle.
type fakeAssertions struct {
	items map[string]id.ID
	needs map[string]bool // tokens still waiting for their second factor
}

func newFakeAssertions() *fakeAssertions {
	return &fakeAssertions{items: map[string]id.ID{}, needs: map[string]bool{}}
}

func (f *fakeAssertions) issue(user id.ID) string {
	token := id.New().String()
	f.items[token] = user
	return token
}

// issueNeedingSecondFactor is an assertion from a password login on an account with TOTP whose
// code was never verified.
func (f *fakeAssertions) issueNeedingSecondFactor(user id.ID) string {
	token := f.issue(user)
	f.needs[token] = true
	return token
}

func (f *fakeAssertions) Spend(token string) (id.ID, bool, bool) {
	user, ok := f.items[token]
	if !ok {
		return id.ID{}, false, false
	}
	needs := f.needs[token]
	delete(f.items, token)
	delete(f.needs, token)
	return user, needs, true
}

// unspent reports whether token is still in the store: a refusal that comes before the spend must
// leave the person's assertion usable.
func (f *fakeAssertions) unspent(token string) bool {
	_, ok := f.items[token]
	return ok
}

func TestPreimageIsEightyOneBytesAndMatchesTheVector(t *testing.T) {
	instance, _ := id.Parse("00112233445566778899aabbccddeeff")
	device, _ := id.Parse("0102030405060708090a0b0c0d0e0f10")
	nonce := bytes.Repeat([]byte{0xab}, 32)
	got := auth.SessionPreimage(instance, device, nonce, auth.PurposeSession)
	if len(got) != 81 {
		t.Fatalf("preimage is %d bytes, want 81 (16 + 16 + 16 + 32 + 1)", len(got))
	}
	if string(got[:16]) != "dilla session v1" {
		t.Fatalf("domain separator = %q", got[:16])
	}
	if !bytes.Equal(got[16:32], instance[:]) || !bytes.Equal(got[32:48], device[:]) {
		t.Fatal("instance_id or device_id is not in its documented position")
	}
	if !bytes.Equal(got[48:80], nonce) || got[80] != 0 {
		t.Fatal("nonce or purpose is not in its documented position")
	}

	// The same bytes are in protocol/vectors/identity.json, so a client in any
	// language can check itself against the instance.
	body, err := os.ReadFile("../../protocol/vectors/identity.json")
	if err != nil {
		t.Fatalf("read identity.json: %v", err)
	}
	var doc struct {
		SessionPreimage struct {
			InstanceID, DeviceID, Nonce, Preimage string
			Purpose                               int
		} `json:"session_preimage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse identity.json: %v", err)
	}
	if doc.SessionPreimage.Preimage != hex.EncodeToString(got) {
		t.Fatalf("vector preimage %s != computed %s", doc.SessionPreimage.Preimage, hex.EncodeToString(got))
	}
}

func TestManagedBotSessionIsAtomicallyBoundAndResolved(t *testing.T) {
	s, repo, clk := newSessions(t)
	ctx := context.Background()
	owner, ownerDevice, _ := seedDevice(t, repo)
	community := store.CommunityRow{ID: id.New(), Owner: owner, Name: "bot session",
		PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: clk.Now().Unix()}
	if err := repo.CreateCommunity(ctx, community); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community.ID, UserID: owner, Joined: 1}); err != nil {
		t.Fatal(err)
	}
	communityID := community.ID
	voiceChannel := store.ChannelRow{ID: id.New(), CommunityID: &communityID, Kind: 1, Mode: 0, Visibility: 0,
		Name: "voice", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: clk.Now().Unix()}
	if err := repo.CreateChannel(ctx, voiceChannel); err != nil {
		t.Fatal(err)
	}
	callID := id.New()
	if err := repo.PutVoiceSession(ctx, store.VoiceSessionRow{CallID: callID, ChannelID: voiceChannel.ID,
		LivekitRoom: "bot-session-test", Started: clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	botUser := store.UserRow{ID: id.New(), Username: "bot_" + id.New().String()[:8], Kind: 1,
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: 1}
	if err := repo.CreateUser(ctx, botUser); err != nil {
		t.Fatal(err)
	}
	deviceID := id.New()
	dskPub, dskPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateDevice(ctx, store.DeviceRow{ID: deviceID, UserID: botUser.ID, DSKPub: dskPub,
		CredentialBlob: []byte{1}, Tier: 0, SignerTier: 0, Created: 1, LastSeen: 1}); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community.ID, UserID: botUser.ID, Joined: 1}); err != nil {
		t.Fatal(err)
	}
	runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: bytes.Repeat([]byte{0x44}, 32), Created: 1, Rotated: 1}
	if err := repo.CreateBotRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	uid, did, rid := botUser.ID, deviceID, runtime.ID
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: community.ID, RuntimeID: &rid,
		UserID: &uid, DeviceID: &did, State: store.BotInstallationEnrolling, Revision: 1,
		ApprovedBy: owner, ApprovedAt: 1, Created: 1, Updated: 1}
	if err := repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	nonce, _, err := s.Challenge(ctx, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(dskPriv, auth.SessionPreimage(s.InstanceID(), deviceID, nonce, auth.PurposeSession))
	tok, err := s.Establish(ctx, auth.EstablishRequest{DeviceID: deviceID, Nonce: nonce, Purpose: auth.PurposeSession, Sig: sig})
	if err != nil {
		t.Fatalf("proof-based bot Establish: %v", err)
	}
	bound, err := repo.GetBotSessionBinding(ctx, auth.TokenHash(tok.Token))
	if err != nil || bound.InstallationID != installation.ID || bound.UserID != botUser.ID || bound.DeviceID != deviceID {
		t.Fatalf("binding = %+v, %v", bound, err)
	}
	got, err := s.Resolve(ctx, tok.Token)
	if err != nil || got.Bot == nil || got.Bot.InstallationID != installation.ID || got.Bot.State != store.BotInstallationEnrolling {
		t.Fatalf("Resolve = %+v, %v", got, err)
	}
	renewNonce, _, err := s.Challenge(ctx, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	renewSig := ed25519.Sign(dskPriv, auth.SessionPreimage(s.InstanceID(), deviceID, renewNonce, auth.PurposeRenew))
	renewed, err := s.Establish(ctx, auth.EstablishRequest{DeviceID: deviceID, Nonce: renewNonce, Purpose: auth.PurposeRenew, Sig: renewSig})
	if err != nil {
		t.Fatalf("proof-based bot Renew: %v", err)
	}
	renewBinding, err := repo.GetBotSessionBinding(ctx, auth.TokenHash(renewed.Token))
	if err != nil || renewBinding.InstallationID != installation.ID || renewed.Scope != auth.ScopeEnrolled {
		t.Fatalf("renewed binding = %+v, token=%+v, err=%v", renewBinding, renewed, err)
	}
	orphanBearer := "orphaned-seeded-bot-session"
	orphanHash := auth.TokenHash(orphanBearer)
	now := clk.Now().Unix()
	if err := repo.CreateSession(ctx, store.SessionRow{TokenHash: orphanHash, DeviceID: deviceID, UserID: botUser.ID,
		Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, orphanBearer); err == nil {
		t.Fatal("Resolve accepted a seeded bot session without exact token-hash binding")
	}
	provisionalNonce, _, err := s.Challenge(ctx, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	provisionalSig := ed25519.Sign(dskPriv, auth.SessionPreimage(s.InstanceID(), deviceID, provisionalNonce, auth.PurposeProvisional))
	if _, err := s.Establish(ctx, auth.EstablishRequest{DeviceID: deviceID, Nonce: provisionalNonce, Purpose: auth.PurposeProvisional, Sig: provisionalSig}); err == nil {
		t.Fatal("bot account received a provisional session")
	}
	loginNonce, _, err := s.Challenge(ctx, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	loginSig := ed25519.Sign(dskPriv, auth.SessionPreimage(s.InstanceID(), deviceID, loginNonce, auth.PurposeSession))
	if _, err := s.Establish(ctx, auth.EstablishRequest{DeviceID: deviceID, Nonce: loginNonce, Purpose: auth.PurposeSession,
		Sig: loginSig, Login: []byte("assertion")}); err == nil {
		t.Fatal("bot account received an assertion/login session")
	}
	if err := s.RecordCapabilities(ctx, auth.TokenHash(tok.Token), 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, pattern, method, pathValue string
		wantNext                         bool
	}{
		{name: "negotiate while enrolling", pattern: "POST /v1/sessions/negotiate", method: http.MethodPost, wantNext: true},
		{name: "credential finalization for same installation", pattern: "PUT /v1/bot-runtime/installations/{installation}/credential", method: http.MethodPut, pathValue: installation.ID.String(), wantNext: true},
		{name: "credential finalization for another installation", pattern: "PUT /v1/bot-runtime/installations/{installation}/credential", method: http.MethodPut, pathValue: id.New().String(), wantNext: false},
		{name: "account settings denied", pattern: "GET /v1/accounts/me", method: http.MethodGet, wantNext: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), auth.ScopeEnrolled)
			r := httptest.NewRequestWithContext(context.Background(), tc.method, "/", nil)
			r.Header.Set("Authorization", "Bearer "+tok.Token)
			r.Pattern = tc.pattern
			if tc.pathValue != "" {
				r.SetPathValue("installation", tc.pathValue)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if called != tc.wantNext {
				t.Fatalf("next called=%v, want %v; response=%d %s", called, tc.wantNext, w.Code, w.Body.String())
			}
		})
	}
	joiningRow, err := repo.GetBotInstallation(ctx, installation.ID)
	if err != nil {
		t.Fatal(err)
	}
	verifiedAt := clk.Now().Unix()
	joiningRow.State = store.BotInstallationJoining
	joiningRow.CredentialVerifiedAt = &verifiedAt
	joiningRow.Revision++
	joiningRow.Updated = verifiedAt
	if err := repo.UpdateBotInstallation(ctx, installation.ID, joiningRow.Revision-1, joiningRow); err != nil {
		t.Fatalf("advance seeded installation to joining: %v", err)
	}
	foreignCommunity := store.CommunityRow{ID: id.New(), Owner: owner, Name: "foreign",
		PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: clk.Now().Unix()}
	if err := repo.CreateCommunity(ctx, foreignCommunity); err != nil {
		t.Fatal(err)
	}
	foreignCommunityID := foreignCommunity.ID
	foreignChannel := store.ChannelRow{ID: id.New(), CommunityID: &foreignCommunityID, Kind: 1, Mode: 0, Visibility: 0,
		Name: "foreign voice", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: clk.Now().Unix()}
	if err := repo.CreateChannel(ctx, foreignChannel); err != nil {
		t.Fatal(err)
	}
	group := func(communityID *id.ID) id.ID {
		t.Helper()
		row := store.GroupRow{GroupID: id.New(), Binding: []byte{0x80}, Kind: 0, CommunityID: communityID,
			TargetID: id.New(), Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1,
			PolicyVersion: 1, Created: clk.Now().Unix()}
		if err := repo.CreateGroup(ctx, row); err != nil {
			t.Fatal(err)
		}
		return row.GroupID
	}
	ownGroup := group(&community.ID)
	foreignGroup := group(&foreignCommunity.ID)
	dmGroup := group(nil)
	// A bot with no selection or persisted wire 2 must first pass the route and
	// resource checks, then receive the protocol's exact supported-version error.
	for _, tc := range []struct {
		name, token, group string
		wantCode           string
	}{
		{name: "wire not negotiated", token: renewed.Token, group: ownGroup.String(), wantCode: "E_VERSION"},
		{name: "wire two", token: tok.Token, group: ownGroup.String(), wantCode: "E_VERSION"},
		{name: "foreign resource visibility precedes wire refusal", token: renewed.Token, group: foreignGroup.String(), wantCode: "E_FORBIDDEN"},
	} {
		if tc.name == "wire two" {
			if err := s.RecordCapabilities(ctx, auth.TokenHash(tok.Token), 2, 1, 1); err != nil {
				t.Fatal(err)
			}
		}
		t.Run(tc.name, func(t *testing.T) {
			called := false
			mux := http.NewServeMux()
			mux.Handle("GET /v1/groups/{id}/info", s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), auth.ScopeEnrolled))
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/groups/"+tc.group+"/info", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if called {
				t.Fatal("handler ran before version/resource refusal")
			}
			var fields []any
			if err := cborx.Unmarshal(rec.Body.Bytes(), &fields); err != nil || len(fields) == 0 || fields[0] != tc.wantCode {
				t.Fatalf("response=%d body=%x err=%v, want %s", rec.Code, rec.Body.Bytes(), err, tc.wantCode)
			}
			if tc.wantCode == "E_VERSION" {
				assertBotVersionError(t, rec.Body.Bytes())
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("version response status = %d, want 400", rec.Code)
				}
			} else if rec.Code != http.StatusForbidden {
				t.Fatalf("resource refusal status = %d, want 403", rec.Code)
			}
		})
	}
	negotiationReached := false
	negotiationMux := http.NewServeMux()
	negotiationMux.Handle("POST /v1/sessions/negotiate", s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		negotiationReached = true
	}), auth.ScopeEnrolled))
	negotiationReq := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/sessions/negotiate", nil)
	negotiationReq.Header.Set("Authorization", "Bearer "+renewed.Token)
	negotiationRec := httptest.NewRecorder()
	negotiationMux.ServeHTTP(negotiationRec, negotiationReq)
	if !negotiationReached || negotiationRec.Code != http.StatusOK {
		t.Fatalf("capability negotiation was refused before wire selection: reached=%v status=%d body=%x", negotiationReached, negotiationRec.Code, negotiationRec.Body.Bytes())
	}
	if err := s.RecordCapabilities(ctx, auth.TokenHash(tok.Token), 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	// Resolve succeeds through this wrapper; the subsequent route-specific group
	// read fails operationally and must become sanitized 503 E_UNAVAILABLE.
	faultSessions := auth.NewSessions(routeGroupFaultRepo{Repository: repo, groupID: ownGroup}, clk,
		config.Default().Auth.Session, id.New(), 7)
	mux := http.NewServeMux()
	mux.Handle("GET /v1/groups/{id}/info", faultSessions.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), auth.ScopeEnrolled))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/groups/"+ownGroup.String()+"/info", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var faultFields []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &faultFields); err != nil || len(faultFields) < 1 || faultFields[0] != "E_UNAVAILABLE" || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("route store failure response=%d body=%x err=%v, want 503 E_UNAVAILABLE", rec.Code, rec.Body.Bytes(), err)
	}
	if len(faultFields) < 2 || faultFields[1] != "authorization state unavailable" {
		t.Fatalf("route store failure leaked or lost sanitized detail: %v", faultFields)
	}
	for _, tc := range []struct {
		name, pattern, method, pathName, pathValue string
		wantNext                                   bool
	}{
		{name: "joining can publish own signed device list", pattern: "PUT /v1/users/{user_id}/device-list", method: http.MethodPut, pathName: "user_id", pathValue: botUser.ID.String(), wantNext: true},
		{name: "joining can read eligible same-community device list", pattern: "GET /v1/users/{user_id}/device-list", method: http.MethodGet, pathName: "user_id", pathValue: owner.String(), wantNext: true},
		{name: "joining can retry credential receipt", pattern: "PUT /v1/bot-runtime/installations/{installation}/credential", method: http.MethodPut, pathName: "installation", pathValue: installation.ID.String(), wantNext: true},
		{name: "joining can request ready", pattern: "POST /v1/bot-runtime/installations/{installation}/ready", method: http.MethodPost, pathName: "installation", pathValue: installation.ID.String(), wantNext: true},
		{name: "joining cannot use generic key package publication", pattern: "POST /v1/keypackages", method: http.MethodPost, wantNext: false},
		{name: "joining can publish through its installation", pattern: "POST /v1/bot-runtime/installations/{installation}/keypackages", method: http.MethodPost, pathName: "installation", pathValue: installation.ID.String(), wantNext: true},
		{name: "joining can read its publication receipt", pattern: "GET /v1/bot-runtime/installations/{installation}/keypackages/{operation}", method: http.MethodGet, pathName: "installation", pathValue: installation.ID.String(), wantNext: true},
		{name: "joining can register its first group through the scoped route", pattern: "POST /v1/bot-runtime/installations/{installation}/groups", method: http.MethodPost, pathName: "installation", pathValue: installation.ID.String(), wantNext: true},
		{name: "joining can request admission only for its installation", pattern: "POST /v1/bot-runtime/installations/{installation}/interaction-admissions", method: http.MethodPost, pathName: "installation", pathValue: installation.ID.String(), wantNext: true},
		{name: "joining cannot request admission for another installation", pattern: "POST /v1/bot-runtime/installations/{installation}/interaction-admissions", method: http.MethodPost, pathName: "installation", pathValue: id.New().String(), wantNext: false},
		{name: "joining can probe a foreign registration path for indistinguishable 404", pattern: "GET /v1/bot-runtime/installations/{installation}/groups/{operation}", method: http.MethodGet, pathName: "installation", pathValue: id.New().String(), wantNext: true},
		{name: "joining can list its device-scoped welcomes", pattern: "GET /v1/welcomes", method: http.MethodGet, wantNext: true},
		{name: "joining can acknowledge its device-scoped welcome", pattern: "DELETE /v1/welcomes/{welcome_id}", method: http.MethodDelete, wantNext: true},
		{name: "joining can register a typed group binding", pattern: "POST /v1/groups", method: http.MethodPost, wantNext: true},
		{name: "joining can sync own community group", pattern: "GET /v1/groups/{id}/info", method: http.MethodGet, pathName: "id", pathValue: ownGroup.String(), wantNext: true},
		{name: "joining cannot use human heal-state route", pattern: "GET /v1/groups/{id}/heal-state", method: http.MethodGet, pathName: "id", pathValue: ownGroup.String(), wantNext: false},
		{name: "joining cannot sync foreign group", pattern: "GET /v1/groups/{id}/info", method: http.MethodGet, pathName: "id", pathValue: foreignGroup.String(), wantNext: false},
		{name: "joining cannot sync DM group", pattern: "GET /v1/groups/{id}/info", method: http.MethodGet, pathName: "id", pathValue: dmGroup.String(), wantNext: false},
		{name: "joining cannot upload application message", pattern: "POST /v1/groups/{id}/message", method: http.MethodPost, pathName: "id", pathValue: ownGroup.String(), wantNext: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), auth.ScopeEnrolled)
			r := httptest.NewRequestWithContext(context.Background(), tc.method, "/", nil)
			r.Header.Set("Authorization", "Bearer "+tok.Token)
			r.Pattern = tc.pattern
			if tc.pathName != "" {
				r.SetPathValue(tc.pathName, tc.pathValue)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if called != tc.wantNext {
				t.Fatalf("next called=%v, want %v; response=%d %s", called, tc.wantNext, w.Code, w.Body.String())
			}
		})
	}
	readyRow, err := repo.GetBotInstallation(ctx, installation.ID)
	if err != nil {
		t.Fatal(err)
	}
	readyRow.State = store.BotInstallationReady
	readyRow.Revision++
	readyRow.Updated = clk.Now().Unix()
	if err := repo.UpdateBotInstallation(ctx, installation.ID, readyRow.Revision-1, readyRow); err != nil {
		t.Fatalf("advance seeded installation to ready: %v", err)
	}
	interactionGroupRow := store.GroupRow{GroupID: id.New(), Binding: []byte{0x80}, Kind: 3, CommunityID: &community.ID,
		TargetID: owner, Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 0,
		PolicyVersion: 1, Created: clk.Now().Unix()}
	if err := repo.CreateGroup(ctx, interactionGroupRow); err != nil {
		t.Fatal(err)
	}
	registeredInteraction := store.BotGroupRegistrationReceipt{
		InstallationID: installation.ID, OperationID: id.New(), UserID: botUser.ID, DeviceID: deviceID,
		CommunityID: community.ID, GroupID: interactionGroupRow.GroupID, BodySHA256: bytes.Repeat([]byte{0x31}, 32),
		Generation: 1, Epoch: 1, NextSeq: 1, AcceptedAt: clk.Now().Unix(),
	}
	if err := repo.Tx(ctx, func(tx store.Repository) error {
		return tx.CreateBotGroupRegistrationReceipt(ctx, registeredInteraction)
	}); err != nil {
		t.Fatal(err)
	}
	unregisteredInteraction := store.GroupRow{GroupID: id.New(), Binding: []byte{0x80}, Kind: 3, CommunityID: &community.ID,
		TargetID: owner, Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 0,
		PolicyVersion: 1, Created: clk.Now().Unix()}
	if err := repo.CreateGroup(ctx, unregisteredInteraction); err != nil {
		t.Fatal(err)
	}
	foreignInstallInteraction := store.GroupRow{GroupID: id.New(), Binding: []byte{0x80}, Kind: 3, CommunityID: &community.ID,
		TargetID: owner, Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 0,
		PolicyVersion: 1, Created: clk.Now().Unix()}
	if err := repo.CreateGroup(ctx, foreignInstallInteraction); err != nil {
		t.Fatal(err)
	}
	foreignInstallReceipt := registeredInteraction
	foreignInstallReceipt.DeviceID = ownerDevice
	foreignInstallReceipt.OperationID = id.New()
	foreignInstallReceipt.GroupID = foreignInstallInteraction.GroupID
	if err := repo.Tx(ctx, func(tx store.Repository) error {
		return tx.CreateBotGroupRegistrationReceipt(ctx, foreignInstallReceipt)
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, pattern, method, installation, group string
		allow                                      bool
	}{
		{name: "ready bot can enter dedicated interaction info route", pattern: "GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/info", method: http.MethodGet, allow: true},
		{name: "ready bot can enter dedicated interaction messages route", pattern: "GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/messages", method: http.MethodGet, allow: true},
		{name: "ready bot can enter dedicated interaction handshakes route", pattern: "GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/handshakes", method: http.MethodGet, allow: true},
		{name: "ready bot can enter dedicated interaction heal status route", pattern: "GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/heal", method: http.MethodGet, allow: true},
		{name: "ready bot can enter dedicated interaction heal route", pattern: "POST /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/heal", method: http.MethodPost, allow: true},
		{name: "ready bot can enter dedicated interaction cursor route", pattern: "POST /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/cursor", method: http.MethodPost, allow: true},
		{name: "ready bot cannot enter human command status route", pattern: "GET /v1/communities/{community}/bot/command-status", method: http.MethodGet},
		{name: "ready bot cannot use a foreign installation path", pattern: "GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/info", method: http.MethodGet, installation: id.New().String()},
		{name: "ready bot cannot use a foreign community group", pattern: "GET /v1/bot-runtime/installations/{installation}/interaction-groups/{group}/info", method: http.MethodGet, group: foreignGroup.String()},
		{name: "ready bot cannot use generic group messages route", pattern: "GET /v1/groups/{id}/messages", method: http.MethodGet},
		{name: "ready bot cannot use generic group cursor route", pattern: "POST /v1/groups/{id}/cursor", method: http.MethodPost},
		{name: "ready bot cannot use generic group heal status route", pattern: "GET /v1/groups/{id}/heal", method: http.MethodGet},
		{name: "ready bot cannot use generic group heal route", pattern: "POST /v1/groups/{id}/heal", method: http.MethodPost},
		{name: "ready bot cannot upload to interaction group", pattern: "POST /v1/groups/{id}/message", method: http.MethodPost},
		{name: "ready bot cannot commit to interaction group", pattern: "POST /v1/groups/{id}/commit", method: http.MethodPost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), auth.ScopeEnrolled)
			r := httptest.NewRequestWithContext(context.Background(), tc.method, "/", nil)
			r.Header.Set("Authorization", "Bearer "+tok.Token)
			r.Pattern = tc.pattern
			installationValue, groupValue := tc.installation, tc.group
			if installationValue == "" {
				installationValue = installation.ID.String()
			}
			if groupValue == "" {
				groupValue = interactionGroupRow.GroupID.String()
			}
			r.SetPathValue("installation", installationValue)
			r.SetPathValue("group", groupValue)
			r.SetPathValue("id", interactionGroupRow.GroupID.String())
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if called != tc.allow {
				t.Fatalf("next called=%v allow=%v response=%d %s", called, tc.allow, w.Code, w.Body.String())
			}
		})
	}
	channelAuthorityPattern := "GET /v1/communities/{community}/bot/actors/{actor}/channels/{channel}/authority"
	for _, tc := range []struct {
		name, communityID, actorID, channelID string
		wantNext                              bool
	}{
		{name: "ready bot can resolve a live channel in its community", communityID: community.ID.String(), actorID: owner.String(), channelID: voiceChannel.ID.String(), wantNext: true},
		{name: "wrong community is denied", communityID: id.New().String(), actorID: owner.String(), channelID: voiceChannel.ID.String()},
		{name: "foreign channel is denied", communityID: community.ID.String(), actorID: owner.String(), channelID: foreignChannel.ID.String()},
		{name: "malformed actor is denied", communityID: community.ID.String(), actorID: id.ID{}.String(), channelID: voiceChannel.ID.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), auth.ScopeEnrolled)
			r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			r.Header.Set("Authorization", "Bearer "+tok.Token)
			r.Pattern = channelAuthorityPattern
			r.SetPathValue("community", tc.communityID)
			r.SetPathValue("actor", tc.actorID)
			r.SetPathValue("channel", tc.channelID)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if called != tc.wantNext {
				t.Fatalf("next called=%v, want %v; response=%d %s", called, tc.wantNext, w.Code, w.Body.String())
			}
		})
	}
	readyInstallation, err := repo.GetBotInstallation(ctx, installation.ID)
	if err != nil {
		t.Fatal(err)
	}
	disabledInstallation := readyInstallation
	disabledInstallation.State = store.BotInstallationDisabled
	disabledInstallation.Revision++
	disabledInstallation.Updated = clk.Now().Unix()
	if err := repo.UpdateBotInstallation(ctx, installation.ID, readyInstallation.Revision, disabledInstallation); err != nil {
		t.Fatal(err)
	}
	calledDisabled := false
	disabledGuard := s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calledDisabled = true }), auth.ScopeEnrolled)
	disabledReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	disabledReq.Header.Set("Authorization", "Bearer "+tok.Token)
	disabledReq.Pattern = channelAuthorityPattern
	disabledReq.SetPathValue("community", community.ID.String())
	disabledReq.SetPathValue("actor", owner.String())
	disabledReq.SetPathValue("channel", voiceChannel.ID.String())
	disabledResp := httptest.NewRecorder()
	disabledGuard.ServeHTTP(disabledResp, disabledReq)
	if calledDisabled {
		t.Fatal("disabled installation reached channel authority handler")
	}
	disabledInstallation.State = store.BotInstallationReady
	disabledInstallation.Revision++
	disabledInstallation.Updated = clk.Now().Unix()
	if err := repo.UpdateBotInstallation(ctx, installation.ID, disabledInstallation.Revision-1, disabledInstallation); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, pattern, method, pathName, pathValue string
	}{
		{name: "ready bot can register a typed group binding", pattern: "POST /v1/groups", method: http.MethodPost},
		{name: "ready bot can redeem its automatic moderation grant", pattern: "POST /v1/bot-runtime/installations/{installation}/automod-grants/redeem", method: http.MethodPost, pathName: "installation", pathValue: installation.ID.String()},
		{name: "ready bot can upload to own community group", pattern: "POST /v1/groups/{group}/bot/messages", method: http.MethodPost, pathName: "group", pathValue: ownGroup.String()},
		{name: "ready bot can send to its accepted interaction group", pattern: "POST /v1/groups/{group}/bot/messages", method: http.MethodPost, pathName: "group", pathValue: interactionGroupRow.GroupID.String()},
		{name: "ready bot can read own community send receipt", pattern: "GET /v1/communities/{community}/bot/receipts/{operation}", method: http.MethodGet, pathName: "community", pathValue: community.ID.String()},
		{name: "ready bot can resolve own community send receipt", pattern: "POST /v1/communities/{community}/bot/receipts/{operation}/resolve", method: http.MethodPost, pathName: "community", pathValue: community.ID.String()},
		{name: "ready bot can start own community channel call", pattern: "POST /v1/channels/{id}/calls", method: http.MethodPost, pathName: "id", pathValue: voiceChannel.ID.String()},
		{name: "ready bot can read own channel call", pattern: "GET /v1/channels/{id}/call", method: http.MethodGet, pathName: "id", pathValue: voiceChannel.ID.String()},
		{name: "ready bot can read own channel permissions", pattern: "GET /v1/channels/{id}/permissions", method: http.MethodGet, pathName: "id", pathValue: voiceChannel.ID.String()},
		{name: "ready bot can read own community calls", pattern: "GET /v1/communities/{id}/calls", method: http.MethodGet, pathName: "id", pathValue: community.ID.String()},
		{name: "ready bot can end own community call", pattern: "DELETE /v1/calls/{call_id}", method: http.MethodDelete, pathName: "call_id", pathValue: callID.String()},
		{name: "ready bot can share own community call", pattern: "POST /v1/calls/{call_id}/share", method: http.MethodPost, pathName: "call_id", pathValue: callID.String()},
		{name: "ready bot can decline own community call", pattern: "POST /v1/calls/{call_id}/decline", method: http.MethodPost, pathName: "call_id", pathValue: callID.String()},
		{name: "ready bot can report its rtt in own community call", pattern: "POST /v1/calls/{call_id}/rtt", method: http.MethodPost, pathName: "call_id", pathValue: callID.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), auth.ScopeEnrolled)
			r := httptest.NewRequestWithContext(context.Background(), tc.method, "/", nil)
			r.Header.Set("Authorization", "Bearer "+tok.Token)
			r.Pattern = tc.pattern
			r.SetPathValue(tc.pathName, tc.pathValue)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if !called {
				t.Fatalf("route refused; response=%d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, tc := range []struct {
		name, pattern, method, pathName, pathValue string
	}{
		{name: "ready bot cannot upload to foreign community group", pattern: "POST /v1/groups/{group}/bot/messages", method: http.MethodPost, pathName: "group", pathValue: foreignGroup.String()},
		{name: "ready bot cannot send to an unregistered interaction group", pattern: "POST /v1/groups/{group}/bot/messages", method: http.MethodPost, pathName: "group", pathValue: unregisteredInteraction.GroupID.String()},
		{name: "ready bot cannot send to an interaction group registered to another device", pattern: "POST /v1/groups/{group}/bot/messages", method: http.MethodPost, pathName: "group", pathValue: foreignInstallInteraction.GroupID.String()},
		{name: "ready bot cannot read foreign community send receipt", pattern: "GET /v1/communities/{community}/bot/receipts/{operation}", method: http.MethodGet, pathName: "community", pathValue: foreignCommunity.ID.String()},
		{name: "ready bot cannot resolve foreign community send receipt", pattern: "POST /v1/communities/{community}/bot/receipts/{operation}/resolve", method: http.MethodPost, pathName: "community", pathValue: foreignCommunity.ID.String()},
		{name: "ready bot cannot read foreign channel permissions", pattern: "GET /v1/channels/{id}/permissions", method: http.MethodGet, pathName: "id", pathValue: foreignChannel.ID.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := s.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), auth.ScopeEnrolled)
			r := httptest.NewRequestWithContext(context.Background(), tc.method, "/", nil)
			r.Header.Set("Authorization", "Bearer "+renewed.Token)
			r.Pattern = tc.pattern
			r.SetPathValue(tc.pathName, tc.pathValue)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if called {
				t.Fatal("foreign community route reached handler")
			}
		})
	}
	if err := repo.PutBan(ctx, store.BanRow{CommunityID: community.ID, UserID: botUser.ID, ByUser: owner, Created: clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, tok.Token); err == nil {
		t.Fatal("Resolve accepted a session after the bot member was banned")
	}
	if err := repo.DeleteBan(ctx, community.ID, botUser.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteMember(ctx, community.ID, botUser.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, tok.Token); err == nil {
		t.Fatal("Resolve accepted a session after community membership was removed")
	}
	if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community.ID, UserID: botUser.ID, Joined: 1}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RevokeBotRuntime(ctx, runtime.ID, clk.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, tok.Token); err == nil {
		t.Fatal("Resolve accepted a session after runtime revocation")
	}
}

func TestPurposeIsBoundIntoTheSignature(t *testing.T) {
	instance := id.New()
	device := id.New()
	nonce := bytes.Repeat([]byte{7}, 32)
	establish := auth.SessionPreimage(instance, device, nonce, auth.PurposeSession)
	renew := auth.SessionPreimage(instance, device, nonce, auth.PurposeRenew)
	if bytes.Equal(establish, renew) {
		t.Fatal("purpose is not bound into the preimage; a renew signature could be replayed as an establish")
	}
}

func TestNonceIsSingleUseAndSixtySeconds(t *testing.T) {
	s, repo, clk := newSessions(t)
	ctx := context.Background()
	user, device, priv := seedDevice(t, repo)

	nonce, expires, err := s.Challenge(ctx, device)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	if len(nonce) != 32 {
		t.Fatalf("nonce is %d bytes, want 32", len(nonce))
	}
	if expires != clk.Now().Add(60*time.Second).Unix() {
		t.Fatalf("nonce TTL is not 60s: expires %d, now %d", expires, clk.Now().Unix())
	}
	req := signed(t, s, device, nonce, auth.PurposeSession, priv)
	tok, err := s.Establish(ctx, req)
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	if tok.UserID != user {
		t.Fatalf("token bound to %s, want %s", tok.UserID, user)
	}
	if _, err := s.Establish(ctx, req); err == nil {
		t.Fatal("the same nonce was accepted twice")
	}

	nonce, _, _ = s.Challenge(ctx, device)
	clk.Advance(61 * time.Second)
	if _, err := s.Establish(ctx, signed(t, s, device, nonce, auth.PurposeSession, priv)); err == nil {
		t.Fatal("an expired nonce was accepted")
	}
}

func TestAReplayedNonceIsRefusedBeforeTheSignatureIsChecked(t *testing.T) {
	s, repo, _ := newSessions(t)
	ctx := context.Background()
	_, device, priv := seedDevice(t, repo)
	nonce, _, _ := s.Challenge(ctx, device)
	req := signed(t, s, device, nonce, auth.PurposeSession, priv)
	if _, err := s.Establish(ctx, req); err != nil {
		t.Fatalf("Establish: %v", err)
	}
	// A replay with a DELIBERATELY BROKEN signature must still be refused for
	// the nonce, which is what proves the nonce is consumed first: otherwise a
	// replay costs one Ed25519 verification per attempt.
	req.Sig = bytes.Repeat([]byte{0}, 64)
	before := s.VerifyCount()
	if _, err := s.Establish(ctx, req); err == nil {
		t.Fatal("a replayed nonce was accepted")
	}
	if s.VerifyCount() != before {
		t.Fatal("the signature was verified before the nonce was checked")
	}
}

func TestAWrongLengthPublicKeyDoesNotPanic(t *testing.T) {
	// ed25519.Verify PANICS on a key that is not 32 bytes. A device row can only
	// hold 32 by CHECK constraint, but Establish length-checks anyway, because a
	// panic here is a remote denial of service.
	s, repo, _ := newSessions(t)
	ctx := context.Background()
	_, device, priv := seedDevice(t, repo)
	if err := corruptDSKPub(repo, device, 31); err != nil {
		t.Skipf("the schema refuses a 31-byte dsk_pub, which is the stronger guarantee: %v", err)
	}
	nonce, _, _ := s.Challenge(ctx, device)
	req := signed(t, s, device, nonce, auth.PurposeSession, priv)
	if _, err := s.Establish(ctx, req); err == nil {
		t.Fatal("a device with a malformed key established a session")
	}
}

func TestANinthSessionEvictsTheOldest(t *testing.T) {
	s, repo, clk := newSessions(t)
	ctx := context.Background()
	_, device, priv := seedDevice(t, repo)
	for i := 0; i < 9; i++ {
		nonce, _, _ := s.Challenge(ctx, device)
		if _, err := s.Establish(ctx, signed(t, s, device, nonce, auth.PurposeSession, priv)); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		clk.Advance(time.Second)
	}
	n, err := repo.CountSessionsByDevice(ctx, device)
	if err != nil {
		t.Fatalf("CountSessionsByDevice: %v", err)
	}
	if n != int64(config.Default().Auth.Session.MaxPerDevice) {
		t.Fatalf("%d live sessions, want %d", n, config.Default().Auth.Session.MaxPerDevice)
	}
}

func TestRevokingADeviceKillsEverySessionAndSocketInOneTransaction(t *testing.T) {
	s, repo, _ := newSessions(t)
	ctx := context.Background()
	_, device, priv := seedDevice(t, repo)
	nonce, _, _ := s.Challenge(ctx, device)
	tok, err := s.Establish(ctx, signed(t, s, device, nonce, auth.PurposeSession, priv))
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	closed := make([]id.ID, 0, 1)
	s.OnRevoke = func(d id.ID) { closed = append(closed, d) }
	if err := s.RevokeDevice(ctx, device); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if _, err := s.Resolve(ctx, tok.Token); err == nil {
		t.Fatal("a revoked device's token still resolves")
	}
	if len(closed) != 1 || closed[0] != device {
		t.Fatalf("the gateway was not told to close the device's sockets: %v", closed)
	}
}

func TestProvisionalIsRefusedOutsideThePairingSurface(t *testing.T) {
	s, repo, _ := newSessions(t)
	ctx := context.Background()
	_, device, priv := seedDevice(t, repo)
	nonce, _, _ := s.Challenge(ctx, device)
	tok, err := s.Establish(ctx, signed(t, s, device, nonce, auth.PurposeProvisional, priv))
	if err != nil {
		t.Fatalf("Establish provisional: %v", err)
	}
	if tok.Scope != auth.ScopeProvisional {
		t.Fatalf("scope = %d, want provisional", tok.Scope)
	}
	guarded := s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), auth.ScopeEnrolled)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/accounts/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a provisional session reached an enrolled-only route: %d", rec.Code)
	}
	// protocol/02 § Device sessions item 4 names the code, so the instance must
	// emit that code and not a generic E_FORBIDDEN.
	if !bytes.Contains(rec.Body.Bytes(), []byte("E_PROVISIONAL_OUTSIDE_PAIRING")) {
		t.Fatalf("refusal body %x does not carry E_PROVISIONAL_OUTSIDE_PAIRING", rec.Body.Bytes())
	}
}

// protocol/02 § Device sessions item 5: 30 days for native devices, 7 days with
// a 12-hour idle window for browser devices. The window follows the DEVICE
// TIER, never the session scope — a browser device holds an ordinary `enrolled`
// session, so a scope-keyed window would give it 30 days.
func TestTheIdleWindowFollowsTheDeviceTierNotTheScope(t *testing.T) {
	s, repo, clk := newSessions(t)
	ctx := context.Background()
	c := config.Default().Auth.Session

	_, browser, browserPriv := seedDeviceTier(t, repo, 1)
	nonce, _, _ := s.Challenge(ctx, browser)
	tok, err := s.Establish(ctx, signed(t, s, browser, nonce, auth.PurposeSession, browserPriv))
	if err != nil {
		t.Fatalf("Establish browser: %v", err)
	}
	if tok.Scope != auth.ScopeEnrolled {
		t.Fatalf("a browser device got scope %d, want enrolled", tok.Scope)
	}
	if want := clk.Now().Add(c.BrowserIdle.Value()).Unix(); tok.IdleExpires != want {
		t.Fatalf("browser idle_expires = %d, want %d (12h)", tok.IdleExpires, want)
	}
	// One hour later a Resolve must slide the window by browser_idle, not by
	// native_lifetime: the scope is `enrolled` either way.
	clk.Advance(time.Hour)
	sess, err := s.Resolve(ctx, tok.Token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ceiling := clk.Now().Add(c.BrowserIdle.Value()).Unix(); sess.IdleExpires > ceiling {
		t.Fatalf("Resolve slid a browser session's idle window to %d, past now+browser_idle %d",
			sess.IdleExpires, ceiling)
	}
	if sess.IdleExpires > sess.Expires {
		t.Fatalf("idle_expires %d is past expires %d", sess.IdleExpires, sess.Expires)
	}

	_, native, nativePriv := seedDeviceTier(t, repo, 0)
	nonce, _, _ = s.Challenge(ctx, native)
	nativeTok, err := s.Establish(ctx, signed(t, s, native, nonce, auth.PurposeSession, nativePriv))
	if err != nil {
		t.Fatalf("Establish native: %v", err)
	}
	if want := clk.Now().Add(c.NativeLifetime.Value()).Unix(); nativeTok.IdleExpires != want {
		t.Fatalf("native idle_expires = %d, want %d (now + native_lifetime)", nativeTok.IdleExpires, want)
	}
}

// Resolve writes the idle slide only when it moves the expiry by more than a minute: a burst of
// requests is one write, not one per request on the single writer.
func TestResolveTouchesTheSessionOnlyWhenTheWindowMovesByMoreThanAMinute(t *testing.T) {
	s, repo, clk := newSessions(t)
	ctx := context.Background()
	_, browser, priv := seedDeviceTier(t, repo, 1)
	nonce, _, _ := s.Challenge(ctx, browser)
	tok, err := s.Establish(ctx, signed(t, s, browser, nonce, auth.PurposeSession, priv))
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	sum := sha256.Sum256([]byte(tok.Token))
	stored := func() int64 {
		t.Helper()
		row, err := repo.GetSessionByHash(ctx, sum[:], clk.Now().Unix())
		if err != nil {
			t.Fatalf("GetSessionByHash: %v", err)
		}
		return row.IdleExpires
	}
	before := stored()

	clk.Advance(30 * time.Second)
	sess, err := s.Resolve(ctx, tok.Token)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := stored(); got != before {
		t.Fatalf("a Resolve 30 s in wrote idle_expires %d over %d; want no write under a minute", got, before)
	}
	if sess.IdleExpires != before {
		t.Fatalf("Resolve reported idle_expires %d, want the stored %d", sess.IdleExpires, before)
	}

	clk.Advance(61 * time.Second)
	if _, err := s.Resolve(ctx, tok.Token); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := stored(); got <= before {
		t.Fatalf("a Resolve 91 s in left idle_expires at %d; the slide must be written", got)
	}
}

// R29 / D12: the session response carries the instance generation, so a client
// learns about a restore from the same exchange that carries X-Dilla-Generation.
func TestTheTokenCarriesTheInstanceGeneration(t *testing.T) {
	s, repo, _ := newSessions(t)
	ctx := context.Background()
	_, device, priv := seedDevice(t, repo)
	nonce, _, _ := s.Challenge(ctx, device)
	tok, err := s.Establish(ctx, signed(t, s, device, nonce, auth.PurposeSession, priv))
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	if tok.Generation != 7 {
		t.Fatalf("Token.Generation = %d, want the 7 NewSessions was given", tok.Generation)
	}
}

// The challenge route is unauthenticated and answers identically for an unknown
// device, so anyone can mint nonces for arbitrary 16-byte ids. The map that
// holds them for 60 seconds must therefore have a ceiling.
func TestThePendingNonceMapIsCapped(t *testing.T) {
	s, repo, _ := newSessions(t)
	ctx := context.Background()
	for i := 0; i < auth.MaxPendingNonces+1000; i++ {
		if _, _, err := s.Challenge(ctx, id.New()); err != nil {
			t.Fatalf("Challenge %d: %v", i, err)
		}
	}
	if n := s.PendingNonces(); n > auth.MaxPendingNonces {
		t.Fatalf("%d pending nonces, want at most %d", n, auth.MaxPendingNonces)
	}
	// A nonce minted after the eviction still establishes.
	_, device, priv := seedDevice(t, repo)
	nonce, _, _ := s.Challenge(ctx, device)
	if _, err := s.Establish(ctx, signed(t, s, device, nonce, auth.PurposeSession, priv)); err != nil {
		t.Fatalf("a nonce minted after eviction did not establish: %v", err)
	}
}

// interfaces.md §5.1: the enrolment assertion "is spent by
// POST /v1/devices/{device_id}/sessions on the pending path". Without this the
// six auth-ceremony routes hand back an assertion no endpoint accepts, and
// ScopePending is never issued by anything.
func TestAPendingSessionRequiresASpentAssertion(t *testing.T) {
	s, repo, _ := newSessions(t)
	ctx := context.Background()
	assertions := newFakeAssertions()
	s.Assertions = assertions
	user, device, priv := seedDevice(t, repo)

	// An unknown assertion is refused outright.
	nonce, _, _ := s.Challenge(ctx, device)
	req := signed(t, s, device, nonce, auth.PurposeSession, priv)
	req.Login = []byte("not-an-assertion")
	if _, err := s.Establish(ctx, req); err == nil {
		t.Fatal("an unknown enrolment assertion was accepted")
	}

	// A valid one yields a pending session.
	token := assertions.issue(user)
	nonce, _, _ = s.Challenge(ctx, device)
	req = signed(t, s, device, nonce, auth.PurposeSession, priv)
	req.Login = []byte(token)
	tok, err := s.Establish(ctx, req)
	if err != nil {
		t.Fatalf("Establish with an assertion: %v", err)
	}
	if tok.Scope != auth.ScopePending {
		t.Fatalf("scope = %d, want pending (1)", tok.Scope)
	}

	// Replay: the assertion was spent, so the same bytes must not work twice.
	nonce, _, _ = s.Challenge(ctx, device)
	req = signed(t, s, device, nonce, auth.PurposeSession, priv)
	req.Login = []byte(token)
	if _, err := s.Establish(ctx, req); err == nil {
		t.Fatal("a replayed enrolment assertion was accepted")
	}

	// An assertion belonging to another user does not reach this device.
	other := assertions.issue(id.New())
	nonce, _, _ = s.Challenge(ctx, device)
	req = signed(t, s, device, nonce, auth.PurposeSession, priv)
	req.Login = []byte(other)
	if _, err := s.Establish(ctx, req); err == nil {
		t.Fatal("an assertion for another user established a session on this device")
	}

	// And with no assertion store wired at all, a login byte string is refused
	// rather than silently ignored.
	s.Assertions = nil
	nonce, _, _ = s.Challenge(ctx, device)
	req = signed(t, s, device, nonce, auth.PurposeSession, priv)
	req.Login = []byte(assertions.issue(user))
	if _, err := s.Establish(ctx, req); err == nil {
		t.Fatal("an assertion was accepted with no assertion store configured")
	}
}

func TestAProvisionalSessionActsOnlyInItsPairingGroup(t *testing.T) {
	pairing, other := id.New(), id.New()
	s := auth.Session{Scope: auth.ScopeProvisional, PairingGroup: &pairing}
	if !s.InPairingGroup(pairing) {
		t.Error("a provisional session must act in its own pairing group")
	}
	if s.InPairingGroup(other) {
		t.Error("a provisional session must not act outside its pairing group")
	}
	if (auth.Session{Scope: auth.ScopeProvisional}).InPairingGroup(pairing) {
		t.Error("a provisional session with no pairing group may act on nothing")
	}
	if !(auth.Session{Scope: auth.ScopeEnrolled}).InPairingGroup(other) {
		t.Error("an enrolled session is unrestricted")
	}
}

func signed(t *testing.T, s *auth.Sessions, device id.ID, nonce []byte, p auth.Purpose, priv ed25519.PrivateKey) auth.EstablishRequest {
	t.Helper()
	pre := auth.SessionPreimage(s.InstanceID(), device, nonce, p)
	return auth.EstablishRequest{DeviceID: device, Nonce: nonce, Purpose: p, Sig: ed25519.Sign(priv, pre)}
}

// staticLister is the device-list gate's view of the instance in these tests: a user it holds no
// entry for has published no list (auth.ErrNoDeviceList); a user it holds entries for has a
// verified newest list naming exactly those unrevoked (device_id, dsk_pub) pairs; err, when set, is
// what every call answers (a verifier that is down, or a stored list that no longer verifies).
type staticLister struct {
	mu      sync.Mutex
	entries map[id.ID][]auth.ListedDevice
	err     error
	calls   int
}

func newStaticLister() *staticLister {
	return &staticLister{entries: map[id.ID][]auth.ListedDevice{}}
}

func (l *staticLister) ListedDevices(_ context.Context, user id.ID) ([]auth.ListedDevice, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	entries, ok := l.entries[user]
	if !ok {
		return nil, auth.ErrNoDeviceList
	}
	return entries, nil
}

func (l *staticLister) resetCalls() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = 0
}

func (l *staticLister) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// list makes entries the user's newest verified list; an empty call is a list naming no device.
func (l *staticLister) list(user id.ID, entries ...auth.ListedDevice) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[user] = append([]auth.ListedDevice{}, entries...)
}

// entry is one list entry: a device id and its key.
func entry(device id.ID, pub []byte) auth.ListedDevice {
	return auth.ListedDevice{DeviceID: device, DSKPub: pub}
}

func (l *staticLister) fail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.err = err
}

func listerOf(t *testing.T, s *auth.Sessions) *staticLister {
	t.Helper()
	l, ok := s.DeviceLists.(*staticLister)
	if !ok {
		t.Fatalf("Sessions.DeviceLists is %T, want the test's *staticLister", s.DeviceLists)
	}
	return l
}

func pubOf(priv ed25519.PrivateKey) []byte { return []byte(priv.Public().(ed25519.PublicKey)) }

func makeDeviceYoung(t *testing.T, repo store.Repository, device id.ID, now int64) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+authDBPaths[repo])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `UPDATE devices SET created = ? WHERE id = ?`, now, device[:]); err != nil {
		t.Fatal(err)
	}
}

// refusal is err as the *server.Error the handler would write.
func refusal(t *testing.T, err error) *server.Error {
	t.Helper()
	var se *server.Error
	if !errors.As(err, &se) {
		t.Fatalf("got %v, want a *server.Error refusal", err)
	}
	return se
}

// establishOnce runs one challenge and one establish (or renew, for purpose 1) for an existing device.
func establishOnce(t *testing.T, s *auth.Sessions, device id.ID, priv ed25519.PrivateKey, p auth.Purpose) (auth.Token, error) {
	t.Helper()
	nonce, _, err := s.Challenge(context.Background(), device)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	req := signed(t, s, device, nonce, p, priv)
	if p == auth.PurposeRenew {
		return s.Renew(context.Background(), req)
	}
	return s.Establish(context.Background(), req)
}

// newRegistration is one browser device the assertion path registers: a fresh id and key and the
// five-element array element 3 carries, as auth receives it after the handler decoded it. The
// credential is the zero-signed placeholder of L-CORE-26 in spirit; auth never reads it.
func newRegistration(t *testing.T) (auth.DeviceRegistration, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return auth.DeviceRegistration{DeviceID: id.New(), DSKPub: pub, Tier: 1, SignerTier: 1,
		Credential: bytes.Repeat([]byte{0}, 10)}, priv
}

// registerWith runs challenge and establish for reg's device with the login and the registration
// array, signing with signer (reg's own key for an honest client).
func registerWith(t *testing.T, s *auth.Sessions, reg auth.DeviceRegistration, signer ed25519.PrivateKey, login string) (auth.Token, error) {
	t.Helper()
	nonce, _, err := s.Challenge(context.Background(), reg.DeviceID)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	req := signed(t, s, reg.DeviceID, nonce, auth.PurposeSession, signer)
	req.Login = []byte(login)
	req.Registration = &reg
	return s.Establish(context.Background(), req)
}

func noDeviceRow(t *testing.T, repo store.Repository, device id.ID) {
	t.Helper()
	if _, err := repo.GetDevice(context.Background(), device); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("device %s has a row (GetDevice err %v); a refused registration must write nothing", device, err)
	}
}

// protocol/02 § Device sessions item 4 (Q26): without a login, the newest verified device list
// decides the scope. Attacker statement: a listed user's unlisted device — a stolen password's
// device — gets `pending`, which reaches only its own backups and its own list; the honest
// enroller's next establish after its list is accepted is enrolled. The 401s are triggered only by
// an instance fault (no verifier, a stored list that no longer verifies) and refuse rather than admit.
func TestTheDeviceListGateDecidesTheScope(t *testing.T) {
	s, repo, clk := newSessions(t)
	lister := listerOf(t, s)
	user, device, priv := seedDevice(t, repo)
	makeDeviceYoung(t, repo, device, clk.Now().Unix())
	scope := func(p auth.Purpose) auth.Scope {
		t.Helper()
		tok, err := establishOnce(t, s, device, priv, p)
		if err != nil {
			t.Fatalf("establish (purpose %d): %v", p, err)
		}
		return tok.Scope
	}
	refused := func(p auth.Purpose) *server.Error {
		t.Helper()
		_, err := establishOnce(t, s, device, priv, p)
		return refusal(t, err)
	}

	if got := scope(auth.PurposeSession); got != auth.ScopeEnrolled {
		t.Fatalf("a user with no device list: scope %d, want enrolled (the no-list clause)", got)
	}
	lister.list(user, entry(device, pubOf(priv)))
	if got := scope(auth.PurposeSession); got != auth.ScopeEnrolled {
		t.Fatalf("a device its user's newest list names: scope %d, want enrolled", got)
	}
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	lister.list(user, entry(id.New(), pubOf(otherPriv)))
	if got := scope(auth.PurposeSession); got != auth.ScopePending {
		t.Fatalf("a listed user's unlisted device: scope %d, want pending", got)
	}
	if got := scope(auth.PurposeRenew); got != auth.ScopePending {
		t.Fatalf("a listed user's unlisted device renewing: scope %d, want pending", got)
	}
	lister.list(user)
	if got := scope(auth.PurposeSession); got != auth.ScopePending {
		t.Fatalf("a list that names no unrevoked key: scope %d, want pending", got)
	}
	// Purpose 2 is gated too (branch review REGISTRATION-DEVICES-01, which changed this assertion:
	// it read "pairing is not gated" and the list was never consulted). The list is read, and a
	// young unlisted native row is still provisional; the 24-hour expiry and the refusal for
	// assertion-registered browser rows are TestPurposeTwoExpiresAnUnlistedRowAfter24Hours and
	// TestPurposeTwoIsRefusedForAnAssertionRegisteredBrowserRow.
	lister.resetCalls()
	if got := scope(auth.PurposeProvisional); got != auth.ScopeProvisional {
		t.Fatalf("purpose 2 on a young unlisted native row: scope %d, want provisional", got)
	}
	if lister.callCount() == 0 {
		t.Fatal("purpose 2 never read the device list: the gate does not apply to it")
	}
	lister.fail(errors.New("the device-list verifier is down"))
	for _, p := range []auth.Purpose{auth.PurposeSession, auth.PurposeProvisional} {
		if e := refused(p); e.Code != server.CodeUnavailable || e.Status() != http.StatusServiceUnavailable {
			t.Fatalf("a verifier fault, purpose %d: %s %d, want 503 E_UNAVAILABLE", p, e.Code, e.Status())
		}
	}
	lister.fail(&mlswasi.ABIError{Code: "E_CREDENTIAL"})
	for _, p := range []auth.Purpose{auth.PurposeSession, auth.PurposeProvisional} {
		if e := refused(p); e.Code != server.CodeUnauthenticated || e.Status() != http.StatusUnauthorized {
			t.Fatalf("a list that fails verification, purpose %d: %s %d, want 401 E_UNAUTHENTICATED", p, e.Code, e.Status())
		}
	}
	s.DeviceLists = nil
	// Changed by REGISTRATION-DEVICES-01 for purpose 2: with no lister it was provisional; the
	// expiry cannot be judged without the list, so it fails closed like purposes 0 and 1.
	for _, p := range []auth.Purpose{auth.PurposeSession, auth.PurposeRenew, auth.PurposeProvisional} {
		if e := refused(p); e.Code != server.CodeUnauthenticated || e.Status() != http.StatusUnauthorized {
			t.Fatalf("no lister wired, purpose %d: %s %d, want 401 E_UNAUTHENTICATED (fail closed)", p, e.Code, e.Status())
		}
	}
}

// REGISTRATION-DEVICES-01 (b), protocol/02 § Device sessions item 2: an unlisted row expires 24
// hours after creation whatever the purpose. Attacker statement: before this, purpose 2 returned
// provisional before the list and the expiry were read, so a password holder's registered row
// minted provisional sessions for ever. Now purpose 2 on a 24-hour-old unlisted row is 401, the row
// is revoked with its sessions and its sockets are closed.
func TestPurposeTwoExpiresAnUnlistedRowAfter24Hours(t *testing.T) {
	s, repo, clk := newSessions(t)
	ctx := context.Background()
	lister := listerOf(t, s)
	user, device, priv := seedDevice(t, repo)
	makeDeviceYoung(t, repo, device, clk.Now().Unix())
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	lister.list(user, entry(id.New(), pubOf(otherPriv)))
	young, err := establishOnce(t, s, device, priv, auth.PurposeProvisional)
	if err != nil || young.Scope != auth.ScopeProvisional {
		t.Fatalf("a young unlisted row, purpose 2: scope %d, err %v; want provisional", young.Scope, err)
	}
	closed := make([]id.ID, 0, 1)
	s.OnRevoke = func(d id.ID) { closed = append(closed, d) }
	clk.Advance(24 * time.Hour)
	_, err = establishOnce(t, s, device, priv, auth.PurposeProvisional)
	if e := refusal(t, err); e.Code != server.CodeUnauthenticated || e.Status() != http.StatusUnauthorized {
		t.Fatalf("a 24-hour-old unlisted row, purpose 2: %s %d, want 401 E_UNAUTHENTICATED", e.Code, e.Status())
	}
	row, err := repo.GetDevice(ctx, device)
	if err != nil || row.RevokedAt == nil {
		t.Fatalf("the expired row = %+v, %v; want it revoked", row, err)
	}
	if len(closed) != 1 || closed[0] != device {
		t.Fatalf("the expired row's sockets were not closed: %v", closed)
	}
	if _, err := s.Resolve(ctx, young.Token); err == nil {
		t.Fatal("the expired row's provisional session still resolves")
	}
	if _, err := establishOnce(t, s, device, priv, auth.PurposeProvisional); err == nil {
		t.Fatal("a revoked row established a provisional session")
	}
}

// REGISTRATION-DEVICES-01 (b): pairing is not built (deviation B27), and protocol/03 pairing is the
// native path, so a browser row registered by a host-login assertion (tier 1, never verified, its
// user has a list) has no use for purpose 2 and is refused it, listed or not, without being revoked.
// Attacker statement: the row a password holder registers gets pending at most, never provisional.
func TestPurposeTwoIsRefusedForAnAssertionRegisteredBrowserRow(t *testing.T) {
	s, repo, _ := newSessions(t)
	ctx := context.Background()
	assertions := newFakeAssertions()
	s.Assertions = assertions
	lister := listerOf(t, s)
	user, owner, ownerPriv := seedDevice(t, repo)
	lister.list(user, entry(owner, pubOf(ownerPriv)))
	reg, regPriv := newRegistration(t)
	if _, err := registerWith(t, s, reg, regPriv, assertions.issue(user)); err != nil {
		t.Fatalf("registration: %v", err)
	}
	for _, listed := range []bool{false, true} {
		if listed {
			lister.list(user, entry(owner, pubOf(ownerPriv)), entry(reg.DeviceID, reg.DSKPub))
		}
		_, err := establishOnce(t, s, reg.DeviceID, regPriv, auth.PurposeProvisional)
		if e := refusal(t, err); e.Code != server.CodeUnauthenticated || e.Status() != http.StatusUnauthorized {
			t.Fatalf("an assertion-registered browser row (listed %v), purpose 2: %s %d, want 401", listed, e.Code, e.Status())
		}
		if row, err := repo.GetDevice(ctx, reg.DeviceID); err != nil || row.RevokedAt != nil {
			t.Fatalf("the refused row = %+v, %v; want it live: a refusal of purpose 2 is not a revocation", row, err)
		}
	}
	// The same row still gets what its list decides through purpose 0.
	if tok, err := establishOnce(t, s, reg.DeviceID, regPriv, auth.PurposeSession); err != nil || tok.Scope != auth.ScopeEnrolled {
		t.Fatalf("the listed registered row, purpose 0: scope %d, err %v; want enrolled", tok.Scope, err)
	}
}

// Q26: the upgrade is by re-establish. A pending token keeps its scope; the device's next
// establish after a list names it is enrolled.
func TestAPendingDeviceIsEnrolledByEstablishingAgainOnceListed(t *testing.T) {
	s, repo, clk := newSessions(t)
	lister := listerOf(t, s)
	user, device, priv := seedDevice(t, repo)
	makeDeviceYoung(t, repo, device, clk.Now().Unix())
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	lister.list(user, entry(id.New(), pubOf(otherPriv)))
	pending, err := establishOnce(t, s, device, priv, auth.PurposeSession)
	if err != nil || pending.Scope != auth.ScopePending {
		t.Fatalf("before the list names it: scope %d, err %v; want pending", pending.Scope, err)
	}
	lister.list(user, entry(id.New(), pubOf(otherPriv)), entry(device, pubOf(priv)))
	enrolled, err := establishOnce(t, s, device, priv, auth.PurposeRenew)
	if err != nil || enrolled.Scope != auth.ScopeEnrolled {
		t.Fatalf("after the list names it: scope %d, err %v; want enrolled", enrolled.Scope, err)
	}
	sess, err := s.Resolve(context.Background(), pending.Token)
	if err != nil {
		t.Fatalf("Resolve the pending token: %v", err)
	}
	if sess.Scope != auth.ScopePending {
		t.Fatalf("the pending token now resolves to scope %d; a scope is fixed at mint", sess.Scope)
	}
}

// L-HTTP-54 step 2: a host-login assertion plus the registration array registers a browser
// device and mints a pending session for it.
func TestAssertionRegistrationCreatesAPendingBrowserDevice(t *testing.T) {
	s, repo, clk := newSessions(t)
	ctx := context.Background()
	assertions := newFakeAssertions()
	s.Assertions = assertions
	lister := listerOf(t, s)
	user, owner, priv := seedDevice(t, repo)
	lister.list(user, entry(owner, pubOf(priv)))

	reg, regPriv := newRegistration(t)
	login := assertions.issue(user)
	tok, err := registerWith(t, s, reg, regPriv, login)
	if err != nil {
		t.Fatalf("registration: %v", err)
	}
	if tok.Scope != auth.ScopePending || tok.UserID != user || tok.DeviceID != reg.DeviceID {
		t.Fatalf("token = scope %d user %s device %s; want pending, %s, %s",
			tok.Scope, tok.UserID, tok.DeviceID, user, reg.DeviceID)
	}
	if want := clk.Now().Add(config.Default().Auth.Session.BrowserIdle.Value()).Unix(); tok.IdleExpires != want {
		t.Fatalf("idle_expires = %d, want %d: the registered tier is browser", tok.IdleExpires, want)
	}
	row, err := repo.GetDevice(ctx, reg.DeviceID)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if row.UserID != user || !bytes.Equal(row.DSKPub, reg.DSKPub) || row.Tier != 1 || row.SignerTier != 1 ||
		!bytes.Equal(row.CredentialBlob, reg.Credential) || row.VerifiedAt != nil || row.RevokedAt != nil ||
		row.Created != clk.Now().Unix() || row.LastSeen != clk.Now().Unix() {
		t.Fatalf("device row = %+v, want the registration as sent, unverified, created now", row)
	}
	// The assertion was spent: the same bytes register nothing more.
	again, againPriv := newRegistration(t)
	_, err = registerWith(t, s, again, againPriv, login)
	if e := refusal(t, err); e.Code != server.CodeUnauthenticated {
		t.Fatalf("a spent assertion: %s, want E_UNAUTHENTICATED", e.Code)
	}
	noDeviceRow(t, repo, again.DeviceID)
	// The registered device is unlisted: its ordinary establish is pending until a list names it.
	plain, err := establishOnce(t, s, reg.DeviceID, regPriv, auth.PurposeSession)
	if err != nil || plain.Scope != auth.ScopePending {
		t.Fatalf("the unlisted registered device: scope %d, err %v; want pending", plain.Scope, err)
	}
	lister.list(user, entry(owner, pubOf(priv)), entry(reg.DeviceID, reg.DSKPub))
	plain, err = establishOnce(t, s, reg.DeviceID, regPriv, auth.PurposeSession)
	if err != nil || plain.Scope != auth.ScopeEnrolled {
		t.Fatalf("the listed registered device: scope %d, err %v; want enrolled", plain.Scope, err)
	}
}

// Q26, gap-G7 §2. Attacker statement: a stolen host password alone cannot create a device row for
// an account whose owner never published a device list; an honest person enrolling into a web-1
// account always has list v1, which web-1 publishes at signup.
func TestAssertionRegistrationIsRefusedForAUserWithNoList(t *testing.T) {
	s, repo, _ := newSessions(t)
	assertions := newFakeAssertions()
	s.Assertions = assertions
	user, _, _ := seedDevice(t, repo) // the lister has no entry: no list

	reg, priv := newRegistration(t)
	_, err := registerWith(t, s, reg, priv, assertions.issue(user))
	if err == nil {
		t.Fatal("a stolen password registered a device for a user with no device list")
	}
	if e := refusal(t, err); e.Code != server.CodeUnauthenticated || e.Status() != http.StatusUnauthorized {
		t.Fatalf("no list: %s %d, want 401 E_UNAUTHENTICATED", e.Code, e.Status())
	}
	noDeviceRow(t, repo, reg.DeviceID)
}

// A transient verifier fault costs an assertion but answers 503 so the browser
// retries the login ceremony; only E_CREDENTIAL is a confirmed 401 refusal.
func TestAssertionRegistrationDistinguishesListFaultFromBadList(t *testing.T) {
	s, repo, _ := newSessions(t)
	a := newFakeAssertions()
	s.Assertions = a
	user, owner, priv := seedDevice(t, repo)
	l := listerOf(t, s)
	l.list(user, entry(owner, pubOf(priv)))
	reg, signer := newRegistration(t)
	l.fail(errors.New("guest closed"))
	login := a.issue(user)
	if _, err := registerWith(t, s, reg, signer, login); refusal(t, err).Code != server.CodeUnavailable {
		t.Fatalf("verifier fault: %v, want 503 E_UNAVAILABLE", err)
	}
	if a.unspent(login) {
		t.Fatal("assertion was not spent before the list fault")
	}
	noDeviceRow(t, repo, reg.DeviceID)
	l.fail(&mlswasi.ABIError{Code: "E_CREDENTIAL"})
	if _, err := registerWith(t, s, reg, signer, a.issue(user)); refusal(t, err).Code != server.CodeUnauthenticated {
		t.Fatalf("bad signed list: %v, want 401 E_UNAUTHENTICATED", err)
	}
	noDeviceRow(t, repo, reg.DeviceID)
}

// Every refusal of the registration path writes nothing, and the ones before the spend leave the
// person's assertion usable. Attacker statement: each is triggered only by the registering
// client's own request (its array, its key, its login); none can be aimed at another device.
func TestAssertionRegistrationRefusals(t *testing.T) {
	s, repo, _ := newSessions(t)
	ctx := context.Background()
	assertions := newFakeAssertions()
	s.Assertions = assertions
	lister := listerOf(t, s)
	user, owner, priv := seedDevice(t, repo)
	lister.list(user, entry(owner, pubOf(priv)))
	reg, regPriv := newRegistration(t)
	login := assertions.issue(user)

	// A login and no registration array for an unknown device.
	nonce, _, _ := s.Challenge(ctx, reg.DeviceID)
	req := signed(t, s, reg.DeviceID, nonce, auth.PurposeSession, regPriv)
	req.Login = []byte(login)
	_, err := s.Establish(ctx, req)
	if e := refusal(t, err); e.Code != server.CodeInvalidRequest || e.Detail != "registration array required" {
		t.Fatalf("no array: %s %q, want E_INVALID_REQUEST \"registration array required\"", e.Code, e.Detail)
	}
	// The array names another device than the path.
	mismatched := reg
	mismatched.DeviceID = id.New()
	nonce, _, _ = s.Challenge(ctx, reg.DeviceID)
	req = signed(t, s, reg.DeviceID, nonce, auth.PurposeSession, regPriv)
	req.Login = []byte(login)
	req.Registration = &mismatched
	_, err = s.Establish(ctx, req)
	if e := refusal(t, err); e.Code != server.CodeInvalidRequest || e.Detail != "device_id does not match" {
		t.Fatalf("mismatched array: %s %q, want E_INVALID_REQUEST \"device_id does not match\"", e.Code, e.Detail)
	}
	// Head ruling 31: a native tier or a native signer tier. Attacker statement: only the
	// registering client's own body; a password alone can no longer mint a `native` row with 30-day
	// sessions and the `app` label in Settings; protocol/03's pairing ceremony is the native path.
	for name, tiers := range map[string][2]uint8{"a native tier": {0, 0}, "a native signer tier": {1, 0}} {
		native := reg
		native.Tier, native.SignerTier = tiers[0], tiers[1]
		_, err = registerWith(t, s, native, regPriv, login)
		if e := refusal(t, err); e.Code != server.CodeInvalidRequest || e.Status() != http.StatusBadRequest ||
			e.Detail != "assertion registration is for browser devices" {
			t.Fatalf("%s: %s %d %q, want 400 E_INVALID_REQUEST \"assertion registration is for browser devices\"",
				name, e.Code, e.Status(), e.Detail)
		}
	}
	// The signature is not by the array's key.
	_, wrongPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, err = registerWith(t, s, reg, wrongPriv, login)
	if e := refusal(t, err); e.Code != server.CodeUnauthenticated {
		t.Fatalf("a signature by another key: %s, want E_UNAUTHENTICATED", e.Code)
	}
	if !assertions.unspent(login) {
		t.Fatal("a refusal before the spend burned the person's assertion")
	}
	// An assertion still waiting for its second factor, and an unknown one.
	_, err = registerWith(t, s, reg, regPriv, assertions.issueNeedingSecondFactor(user))
	if e := refusal(t, err); e.Code != server.CodeUnauthenticated {
		t.Fatalf("an assertion without its second factor: %s, want E_UNAUTHENTICATED", e.Code)
	}
	_, err = registerWith(t, s, reg, regPriv, "not-an-assertion")
	if e := refusal(t, err); e.Code != server.CodeUnauthenticated {
		t.Fatalf("an unknown assertion: %s, want E_UNAUTHENTICATED", e.Code)
	}
	// No assertion store wired.
	s.Assertions = nil
	_, err = registerWith(t, s, reg, regPriv, assertions.issue(user))
	if e := refusal(t, err); e.Code != server.CodeUnauthenticated {
		t.Fatalf("no assertion store: %s, want E_UNAUTHENTICATED", e.Code)
	}
	s.Assertions = assertions
	// The asserted user is disabled.
	disabledAt := int64(1)
	if err := repo.SetUserDisabled(ctx, user, &disabledAt); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	_, err = registerWith(t, s, reg, regPriv, assertions.issue(user))
	if e := refusal(t, err); e.Code != server.CodeForbidden || e.Status() != http.StatusForbidden {
		t.Fatalf("a disabled user: %s %d, want 403 E_FORBIDDEN", e.Code, e.Status())
	}
	noDeviceRow(t, repo, reg.DeviceID)
	noDeviceRow(t, repo, mismatched.DeviceID)
}

// Q04: at most 8 live devices per user. Attacker statement (head L-HTTP-54, ruling 30, as amended
// by the security review's F1): the registration path creates a live, unlisted row for every spent
// assertion, and a registration at the cap replaces the oldest unlisted row, so a holder of the
// password without the recovery key never fills the cap against the owner; only eight LISTED rows
// refuse, which only the SSK holder can produce. The owner also clears unlisted rows without the
// key: Settings → Devices lists them as `not yet in the device list` and their `remove` calls
// DELETE /v1/devices/{id}; the rows are evidence of the compromise. Another user cannot spend this
// user's allowance.
func TestTheDeviceCapIsEightLiveDevices(t *testing.T) {
	s, repo, clk := newSessions(t)
	ctx := context.Background()
	c := config.Default().Auth.Session
	if c.MaxDevicesPerUser != 8 || c.EnrolmentsPerHour != 3 {
		t.Fatalf("defaults are %d devices and %d enrolments an hour, want 8 and 3 (Q04)",
			c.MaxDevicesPerUser, c.EnrolmentsPerHour)
	}
	assertions := newFakeAssertions()
	s.Assertions = assertions
	lister := listerOf(t, s)
	user, first, priv := seedDevice(t, repo)
	listed := []auth.ListedDevice{entry(first, pubOf(priv))}
	// Six more listed live devices, created two hours ago so the hourly rate is not what replaces.
	for i := 0; i < 6; i++ {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		row := store.DeviceRow{ID: id.New(), UserID: user, DSKPub: pub,
			CredentialBlob: []byte{1}, LastSeen: 1, Created: clk.Now().Unix() - 7200}
		if err := repo.CreateDevice(ctx, row); err != nil {
			t.Fatalf("CreateDevice: %v", err)
		}
		listed = append(listed, entry(row.ID, pub))
	}
	lister.list(user, listed...)
	eighth, eighthPriv := newRegistration(t)
	if _, err := registerWith(t, s, eighth, eighthPriv, assertions.issue(user)); err != nil {
		t.Fatalf("the eighth live device: %v", err)
	}
	lister.list(user, append(listed, entry(eighth.DeviceID, eighth.DSKPub))...)
	ninth, ninthPriv := newRegistration(t)
	_, err := registerWith(t, s, ninth, ninthPriv, assertions.issue(user))
	if e := refusal(t, err); e.Code != server.CodeForbidden || e.Status() != http.StatusForbidden || e.Detail != "device cap reached" {
		t.Fatalf("the ninth live device: %s %d %q, want 403 E_FORBIDDEN \"device cap reached\"", e.Code, e.Status(), e.Detail)
	}
	noDeviceRow(t, repo, ninth.DeviceID)
	// A revoked device does not count.
	if err := repo.RevokeDevice(ctx, first, clk.Now().Unix()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if _, err := registerWith(t, s, ninth, ninthPriv, assertions.issue(user)); err != nil {
		t.Fatalf("after one revocation the next device registers: %v", err)
	}
}

// Q04 as amended by the security review's F1: enrolments_per_hour counts the live rows created in
// any hour (derived from devices.created), and past it a registration REPLACES the oldest unlisted
// live row instead of being refused. Attacker statement: a holder of the password alone could keep
// the owner's own registration at 429 for ever with three logins an hour while the rate refused; now
// each of its registrations costs one password login (the per-address `login` bucket) and evicts its
// own oldest row first. A creation leaves the window exactly 3600 s after it.
func TestTheEnrolmentRateIsThreePerHour(t *testing.T) {
	s, repo, clk := newSessions(t)
	assertions := newFakeAssertions()
	s.Assertions = assertions
	lister := listerOf(t, s)
	user, owner, priv := seedDevice(t, repo) // created at unix second 1, outside every window here
	lister.list(user, entry(owner, pubOf(priv)))

	start := clk.Now().Unix()
	var regs []auth.DeviceRegistration
	for i := 0; i < 3; i++ {
		reg, p := newRegistration(t)
		if _, err := registerWith(t, s, reg, p, assertions.issue(user)); err != nil {
			t.Fatalf("enrolment %d of 3: %v", i+1, err)
		}
		regs = append(regs, reg)
		clk.Advance(time.Minute)
	}
	// Assertion 1 — the fourth creation in the hour (start + 180) is admitted and replaces the
	// oldest unlisted row, the one created at start; the two younger rows stay.
	fourth, fourthPriv := newRegistration(t)
	if tok, err := registerWith(t, s, fourth, fourthPriv, assertions.issue(user)); err != nil || tok.Scope != auth.ScopePending {
		t.Fatalf("assertion 1, the fourth enrolment in an hour: scope %d, err %v; want a pending registration", tok.Scope, err)
	}
	wantLive(t, repo, "the first enrolment after the fourth", regs[0].DeviceID, false)
	wantLive(t, repo, "the second enrolment after the fourth", regs[1].DeviceID, true)
	wantLive(t, repo, "the third enrolment after the fourth", regs[2].DeviceID, true)
	// Assertion 2 — at start + 60 + 3599 the second creation is still inside the hour: three live
	// creations in the window, so the fifth replaces the oldest unlisted row, the second.
	clk.Advance(time.Duration(60+3599-180) * time.Second)
	if clk.Now().Unix() != start+60+3599 {
		t.Fatalf("the clock is at %d, want start + 3659", clk.Now().Unix()-start)
	}
	fifth, fifthPriv := newRegistration(t)
	if _, err := registerWith(t, s, fifth, fifthPriv, assertions.issue(user)); err != nil {
		t.Fatalf("assertion 2, one second before the second creation leaves the hour: %v, want admitted", err)
	}
	wantLive(t, repo, "the second enrolment after the fifth", regs[1].DeviceID, false)
	wantLive(t, repo, "the third enrolment after the fifth", regs[2].DeviceID, true)
	// Assertion 3 — at exactly start + 120 + 3600 the third creation has left the hour: two live
	// creations in the window (the fourth and the fifth), so the sixth replaces nothing.
	clk.Advance(time.Duration(120+3600-60-3599) * time.Second)
	sixth, sixthPriv := newRegistration(t)
	if _, err := registerWith(t, s, sixth, sixthPriv, assertions.issue(user)); err != nil {
		t.Fatalf("assertion 3, the boundary second start + 3720: %v, want admitted", err)
	}
	for _, r := range []auth.DeviceRegistration{regs[2], fourth, fifth, sixth} {
		wantLive(t, repo, "a row inside the hour or older after the sixth", r.DeviceID, true)
	}
}

// Head ruling 32 as amended by F1: the rate counts every live device row the user created inside
// the window, the account's first device included, so in a fresh account's first hour the third
// enrolment already replaces the oldest unlisted row. The listed first device is never replaced.
func TestTheEnrolmentRateCountsTheAccountsFirstDevice(t *testing.T) {
	s, repo, clk := newSessions(t)
	ctx := context.Background()
	assertions := newFakeAssertions()
	s.Assertions = assertions
	lister := listerOf(t, s)
	start := clk.Now().Unix()
	user := store.UserRow{ID: id.New(), Username: "fresh" + id.New().String()[:8], Display: "Fresh",
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: start}
	if err := repo.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	firstPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	verified := start
	firstID := id.New()
	if err := repo.CreateDevice(ctx, store.DeviceRow{ID: firstID, UserID: user.ID, DSKPub: firstPub, Tier: 1,
		SignerTier: 1, CredentialBlob: []byte{1}, VerifiedAt: &verified, LastSeen: start, Created: start}); err != nil {
		t.Fatalf("CreateDevice (the account's first device, at start): %v", err)
	}
	lister.list(user.ID, entry(firstID, firstPub))

	var regs []auth.DeviceRegistration
	for i := int64(1); i <= 2; i++ {
		clk.Advance(time.Second) // start + 1, start + 2
		reg, p := newRegistration(t)
		if _, err := registerWith(t, s, reg, p, assertions.issue(user.ID)); err != nil {
			t.Fatalf("enrolment at start + %d: %v, want 201 (the first device and %d enrolments are fewer than 3)", i, err, i-1)
		}
		regs = append(regs, reg)
	}
	wantLive(t, repo, "the first enrolment before the rate is reached", regs[0].DeviceID, true)
	clk.Advance(time.Second) // start + 3
	third, thirdPriv := newRegistration(t)
	if _, err := registerWith(t, s, third, thirdPriv, assertions.issue(user.ID)); err != nil {
		t.Fatalf("the third enrolment in the account's first hour: %v, want admitted", err)
	}
	wantLive(t, repo, "the first enrolment (the first device counted toward the rate)", regs[0].DeviceID, false)
	wantLive(t, repo, "the second enrolment", regs[1].DeviceID, true)
	wantLive(t, repo, "the account's listed first device", firstID, true)
}

// A password holder's unlisted pending row and its session stop working after 24 hours.
// Its expiry frees capacity without an owner device being online.
func TestUnlistedRegistrationExpiresWithItsPendingSession(t *testing.T) {
	s, repo, clk := newSessions(t)
	a := newFakeAssertions()
	s.Assertions = a
	user, owner, priv := seedDevice(t, repo)
	listerOf(t, s).list(user, entry(owner, pubOf(priv)))
	reg, signer := newRegistration(t)
	tok, err := registerWith(t, s, reg, signer, a.issue(user))
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(24 * time.Hour)
	if _, err := establishOnce(t, s, reg.DeviceID, signer, auth.PurposeSession); refusal(t, err).Code != server.CodeUnauthenticated {
		t.Fatalf("expired device re-established: %v", err)
	}
	if _, err := s.Resolve(t.Context(), tok.Token); refusal(t, err).Code != server.CodeUnauthenticated {
		t.Fatalf("expired pending session: %v, want 401", err)
	}
	row, err := repo.GetDevice(t.Context(), reg.DeviceID)
	if err != nil || row.RevokedAt == nil {
		t.Fatalf("expired row = %+v, err %v; want revoked", row, err)
	}
}

// A fresh host login cannot renew the lifetime of a 24-hour-old unlisted row.
func TestExpiredUnlistedRowRejectsAnotherAssertion(t *testing.T) {
	s, repo, clk := newSessions(t)
	a := newFakeAssertions()
	s.Assertions = a
	user, owner, priv := seedDevice(t, repo)
	listerOf(t, s).list(user, entry(owner, pubOf(priv)))
	reg, signer := newRegistration(t)
	if _, err := registerWith(t, s, reg, signer, a.issue(user)); err != nil {
		t.Fatal(err)
	}
	clk.Advance(24 * time.Hour)
	if _, err := registerWith(t, s, reg, signer, a.issue(user)); refusal(t, err).Code != server.CodeUnauthenticated {
		t.Fatalf("a renewed host assertion resurrected the expired row: %v", err)
	}
	row, err := repo.GetDevice(t.Context(), reg.DeviceID)
	if err != nil || row.RevokedAt == nil {
		t.Fatalf("expired row = %+v, err %v; want revoked", row, err)
	}
}

// A young pending row cannot expire, so resolving it or logging in again must
// not acquire a device-list guest. An unavailable guest must not block it.
func TestYoungPendingSessionSkipsDeviceListInResolveAndLogin(t *testing.T) {
	s, repo, _ := newSessions(t)
	a := newFakeAssertions()
	s.Assertions = a
	user, owner, priv := seedDevice(t, repo)
	l := listerOf(t, s)
	l.list(user, entry(owner, pubOf(priv)))
	reg, signer := newRegistration(t)
	tok, err := registerWith(t, s, reg, signer, a.issue(user))
	if err != nil {
		t.Fatal(err)
	}
	l.resetCalls()
	l.fail(errors.New("guest unavailable"))
	if _, err := s.Resolve(t.Context(), tok.Token); err != nil {
		t.Fatalf("young pending Resolve: %v", err)
	}
	if _, err := registerWith(t, s, reg, signer, a.issue(user)); err != nil {
		t.Fatalf("young pending login: %v", err)
	}
	if got := l.callCount(); got != 0 {
		t.Fatalf("young pending list calls = %d, want 0", got)
	}
}

// At the cap, the oldest live unlisted row is evicted while listed rows remain.
// A password holder therefore cannot permanently bar the owner's recovery.
func TestRegistrationAtCapEvictsOldestUnlistedRow(t *testing.T) {
	s, repo, clk := newSessions(t)
	a := newFakeAssertions()
	s.Assertions = a
	user, owner, priv := seedDevice(t, repo)
	keys := []auth.ListedDevice{entry(owner, pubOf(priv))}
	for i := 0; i < 5; i++ {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		row := store.DeviceRow{ID: id.New(), UserID: user, DSKPub: pub,
			CredentialBlob: []byte{1}, Created: 1, LastSeen: 1}
		keys = append(keys, entry(row.ID, pub))
		if err := repo.CreateDevice(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	listerOf(t, s).list(user, keys...)
	old, oldKey := newRegistration(t)
	oldToken, err := registerWith(t, s, old, oldKey, a.issue(user))
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	newer, newerKey := newRegistration(t)
	if _, err := registerWith(t, s, newer, newerKey, a.issue(user)); err != nil {
		t.Fatalf("second unlisted registration: %v", err)
	}
	clk.Advance(time.Second)
	next, nextKey := newRegistration(t)
	if _, err := registerWith(t, s, next, nextKey, a.issue(user)); err != nil {
		t.Fatalf("cap eviction: %v", err)
	}
	row, err := repo.GetDevice(t.Context(), old.DeviceID)
	if err != nil || row.RevokedAt == nil {
		t.Fatalf("oldest unlisted row = %+v, err %v; want revoked", row, err)
	}
	if _, err := s.Resolve(t.Context(), oldToken.Token); refusal(t, err).Code != server.CodeUnauthenticated {
		t.Fatalf("evicted pending session: %v", err)
	}
	if row, err := repo.GetDevice(t.Context(), newer.DeviceID); err != nil || row.RevokedAt != nil {
		t.Fatalf("newer unlisted row = %+v, err %v; want live", row, err)
	}
	if row, err := repo.GetDevice(t.Context(), next.DeviceID); err != nil || row.RevokedAt != nil {
		t.Fatalf("replacement row = %+v, err %v", row, err)
	}
}

// F1 (replacing boundary ruling 3 of the first fix round): the cap evicts the oldest unlisted row
// whatever its age. The first round spared a row younger than the hourly window and refused 403
// instead; a holder of the password alone then kept the owner at 403 by refreshing its young
// unlisted rows every hour (the review's probe B). A row mid-enrolment is now exposed for the
// seconds before the list PUT that names it, which is the owner's to make.
func TestCapEvictsTheOldestUnlistedRowWhateverItsAge(t *testing.T) {
	s, repo, clk := newSessions(t)
	ctx := context.Background()
	a := newFakeAssertions()
	s.Assertions = a
	user, owner, priv := seedDevice(t, repo)
	keys := []auth.ListedDevice{entry(owner, pubOf(priv))}
	for i := 0; i < 6; i++ {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		row := store.DeviceRow{ID: id.New(), UserID: user, DSKPub: pub,
			CredentialBlob: []byte{1}, Created: clk.Now().Unix() - 7200, LastSeen: 1}
		keys = append(keys, entry(row.ID, pub))
		if err := repo.CreateDevice(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	listerOf(t, s).list(user, keys...)
	// An unlisted row registered 30 minutes ago, not yet in the list.
	young, _ := newRegistration(t)
	if err := repo.CreateDevice(ctx, store.DeviceRow{ID: young.DeviceID, UserID: user, DSKPub: young.DSKPub,
		Tier: 1, SignerTier: 1, CredentialBlob: young.Credential, Created: clk.Now().Unix() - 1800, LastSeen: 1}); err != nil {
		t.Fatal(err)
	}
	next, nextKey := newRegistration(t)
	if _, err := registerWith(t, s, next, nextKey, a.issue(user)); err != nil {
		t.Fatalf("a registration at the cap whose only unlisted row is 30 minutes old: %v, want it admitted", err)
	}
	wantLive(t, repo, "the unlisted row 30 minutes old", young.DeviceID, false)
	wantLive(t, repo, "the replacement row", next.DeviceID, true)
}

// Revoked rows release the rate immediately and expired rows after 24 hours: neither counts toward
// the three live creations an hour, so neither makes a registration replace a live row.
func TestEnrolmentRateReleasesRevokedAndExpiredRows(t *testing.T) {
	s, repo, clk := newSessions(t)
	a := newFakeAssertions()
	s.Assertions = a
	user, owner, priv := seedDevice(t, repo)
	listerOf(t, s).list(user, entry(owner, pubOf(priv)))
	var regs []auth.DeviceRegistration
	for i := 0; i < 3; i++ {
		reg, key := newRegistration(t)
		if _, err := registerWith(t, s, reg, key, a.issue(user)); err != nil {
			t.Fatal(err)
		}
		regs = append(regs, reg)
	}
	if err := repo.RevokeDevice(t.Context(), regs[0].DeviceID, clk.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	fourth, fourthKey := newRegistration(t)
	if _, err := registerWith(t, s, fourth, fourthKey, a.issue(user)); err != nil {
		t.Fatalf("after revocation: %v", err)
	}
	// Two live creations and a revoked one in the hour: the fourth replaced nothing.
	wantLive(t, repo, "the second enrolment after the fourth", regs[1].DeviceID, true)
	wantLive(t, repo, "the third enrolment after the fourth", regs[2].DeviceID, true)
	clk.Advance(24 * time.Hour)
	fifth, fifthKey := newRegistration(t)
	if _, err := registerWith(t, s, fifth, fifthKey, a.issue(user)); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	wantLive(t, repo, "the fifth enrolment", fifth.DeviceID, true)
}

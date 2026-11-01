package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/blob"
	"github.com/jonasthim/dilla/internal/bot/identity"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/sqlite"
	"github.com/jonasthim/dilla/internal/store/sqlite/sqlitetest"
)

var (
	testVerifierOnce sync.Once
	testVerifier     *identity.Verifier
	testVerifierErr  error
)

func sharedTestVerifier(t *testing.T) *identity.Verifier {
	t.Helper()
	testVerifierOnce.Do(func() {
		wasm, err := os.ReadFile("../mlswasi/testdata/dilla_core_wasi.wasm")
		if err != nil {
			testVerifierErr = err
			return
		}
		runtime, err := mlswasi.New(context.Background(), wasm, mlswasi.Options{PoolSize: 4})
		if err != nil {
			testVerifierErr = err
			return
		}
		testVerifier = identity.NewVerifier(runtime)
	})
	if testVerifierErr != nil {
		t.Fatalf("start shared WASI credential verifier: %v", testVerifierErr)
	}
	return testVerifier
}

type testHumanCredential struct {
	UMKPub, SSKPub, DSKPub []byte
	SigUMKSSK              []byte
	Credential             []byte
	DSKPriv, SSKPriv       []byte
}

func makeTestCredential(t *testing.T, deviceID id.ID, kind byte) testHumanCredential {
	t.Helper()
	seed := func(label string) []byte { sum := sha256.Sum256([]byte(label)); return sum[:] }
	umk := ed25519.NewKeyFromSeed(seed("dilla human test umk " + deviceID.String()))
	ssk := ed25519.NewKeyFromSeed(seed("dilla human test ssk " + deviceID.String()))
	dsk := ed25519.NewKeyFromSeed(seed("dilla human test dsk " + deviceID.String()))
	umkPub := umk.Public().(ed25519.PublicKey)
	sskPub := ssk.Public().(ed25519.PublicKey)
	dskPub := dsk.Public().(ed25519.PublicKey)
	sigUMKSSK := ed25519.Sign(umk, append([]byte("dilla ssk v1"), sskPub...))
	dskMessage := append([]byte("dilla dsk v1"), deviceID[:]...)
	dskMessage = append(dskMessage, dskPub...)
	dskMessage = append(dskMessage, kind, 0, 0)
	sigSSKDev := ed25519.Sign(ssk, dskMessage)
	credential, err := cborx.Marshal([]any{uint64(1), umkPub, make([]byte, 16), deviceID[:], uint64(kind), uint64(0), uint64(0), sskPub, sigUMKSSK, sigSSKDev})
	if err != nil {
		t.Fatalf("encode Human credential: %v", err)
	}
	return testHumanCredential{UMKPub: umkPub, SSKPub: sskPub, DSKPub: dskPub,
		SigUMKSSK: sigUMKSSK, Credential: credential, DSKPriv: dsk, SSKPriv: ssk}
}

func makeTestHumanCredential(t *testing.T, deviceID id.ID) testHumanCredential {
	t.Helper()
	return makeTestCredential(t, deviceID, 0)
}

func validHumanSignupBody(t *testing.T, code, username, display string, password any) []byte {
	t.Helper()
	deviceID := id.New()
	cred := makeTestHumanCredential(t, deviceID)
	body, err := cborx.Marshal([]any{code, username, display, cred.UMKPub, cred.SSKPub,
		cred.SigUMKSSK, password, []any{deviceID, cred.DSKPub, uint64(0), uint64(0), cred.Credential}})
	if err != nil {
		t.Fatalf("marshal valid Human signup: %v", err)
	}
	return body
}

// testHasher stands in for task 9's Argon2id *auth.Hasher, which does not exist
// yet. It is NOT a mock of the thing under test: what these tests assert is that
// a supplied password is hashed OUTSIDE the transaction and its PHC string
// stored, and the identity of the KDF is task 9's own tests' business. A real
// Argon2id hash here would add ~19 MiB and tens of milliseconds to every
// registration in the suite and prove nothing this file claims.
type testHasher struct{}

func (testHasher) Hash(_ context.Context, pw string) (string, error) {
	sum := sha256.Sum256([]byte(pw))
	return "$dilla-test$" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

func (testHasher) Verify(_ context.Context, pw, phc string) (bool, bool, error) {
	got, err := testHasher{}.Hash(context.Background(), pw)
	return err == nil && got == phc, false, err
}

// newTestAPI opens a migrated SQLite repository in a temporary directory,
// registers every route this task owns on a real server.Mux, and returns the
// handler and the Deps behind it. Nothing is stubbed but the password hasher:
// the store is real SQLite, the sessions are real auth.Sessions, and the
// requests go through the real mux.
func newTestAPI(t *testing.T) (http.Handler, api.Deps) {
	t.Helper()
	return newTestAPIWithConfig(t, nil)
}

// newTestAPIWithConfig is newTestAPI with one hook into the configuration the
// harness is built from, called after config.Default() and before anything
// reads it. A throttling test has to be able to move one limit out of the way
// so the limit it is actually about is the one that fires; every other test
// keeps the defaults.
func newTestAPIWithConfig(t *testing.T, tune func(*config.Config)) (http.Handler, api.Deps) {
	t.Helper()
	return newTestAPIFull(t, tune, nil)
}

func newTestAPIFull(t *testing.T, tune func(*config.Config), wire func(*api.Deps)) (http.Handler, api.Deps) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.db")
	sqlitetest.MigratedFile(t, path)
	write, err := sqlite.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	read, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { repo.Close() })
	return newTestAPIFullWithRepo(t, repo, tune, wire)
}

// newTestAPIWithRepository builds the normal API mux and dependencies over a caller-owned,
// already-migrated repository. Lifecycle tests use it to exercise the same handlers on Postgres.
func newTestAPIWithRepository(t *testing.T, repo store.Repository, wire func(*api.Deps)) (http.Handler, api.Deps) {
	t.Helper()
	return newTestAPIFullWithRepo(t, repo, nil, wire)
}

func newTestAPIFullWithRepo(t *testing.T, repo store.Repository, tune func(*config.Config), wire func(*api.Deps)) (http.Handler, api.Deps) {
	return newTestAPIFullWithRepoAndMount(t, repo, tune, wire, nil)
}

// newTestAPIFullWithRepoAndMount adds a narrow composition seam for integration tests whose
// routes are mounted by the delivery-service composition root rather than api.Register.
func newTestAPIFullWithRepoAndMount(t *testing.T, repo store.Repository, tune func(*config.Config), wire func(*api.Deps), mount func(*server.Mux, api.Deps)) (http.Handler, api.Deps) {
	t.Helper()
	bs, err := blob.Open(filepath.Join(t.TempDir(), "blobs"), "fs")
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })

	clk := clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	cfg := config.Default()
	cfg.Instance.Domain = "dilla.test"
	if tune != nil {
		tune(cfg)
	}
	instance := store.InstanceRow{
		InstanceID: id.New(), ExternalSenderKeyID: id.New(), KeyHistory: []byte{1},
		FrankingKeyID: id.New(), Generation: 7, PolicyVersion: 1, Created: clk.Now().Unix(),
	}
	if err := repo.CreateInstance(context.Background(), instance); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	sessions := auth.NewSessions(repo, clk, cfg.Auth.Session, instance.InstanceID, instance.Generation)
	sessions.DeviceLists = &testLister{entries: map[id.ID][]auth.ListedDevice{}}

	deps := api.Deps{
		Repo:         repo,
		Clock:        clk,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Limiter:      server.NewRateLimiter(cfg.Limits.Rate, clk),
		Instance:     instance,
		Domain:       cfg.Instance.Domain,
		Config:       cfg,
		Registration: cfg.Registration,
		Sessions:     sessions,
		Hasher:       testHasher{},
		Throttle:     auth.NewThrottle(cfg.Limits.Rate, cfg.Auth.Lockout, clk),
		Assertions:   api.NewAssertions(clk, api.AssertionTTL),
		Blobs:        bs,
		BotIdentity:  sharedTestVerifier(t),
	}
	deps.Sessions.Assertions = deps.Assertions
	if wire != nil {
		wire(&deps)
	}
	m := server.NewMux()
	api.Register(m, deps)
	if mount != nil {
		mount(m, deps)
	}
	return m, deps
}

// newTestAPIWired fills Deps fields before registration; handlers hold Deps by value.
func newTestAPIWired(t *testing.T, wire func(*api.Deps)) (http.Handler, api.Deps) {
	t.Helper()
	return newTestAPIFull(t, nil, wire)
}

// seedInvite mints an ordinary invite with max_uses uses and no admin grant,
// and returns the plain code.
func seedInvite(t *testing.T, d api.Deps, maxUses uint64) string {
	t.Helper()
	return seedInviteAdmin(t, d, maxUses, 0)
}

// seedAdminInvite mints the one-use bootstrap invite `dillad init` writes:
// grants_admin = 1, which is the only thing that ever sets
// store.UserFlagInstanceAdmin.
func seedAdminInvite(t *testing.T, d api.Deps) string {
	t.Helper()
	return seedInviteAdmin(t, d, 1, 1)
}

func seedInviteAdmin(t *testing.T, d api.Deps, maxUses uint64, grantsAdmin uint8) string {
	t.Helper()
	code, hash := auth.NewInviteCode()
	now := d.Clock.Now().Unix()
	row := store.InviteRow{
		ID: id.New(), CodeHash: hash, GrantsAdmin: grantsAdmin, MaxUses: maxUses,
		Created: now, ExpiresAt: now + int64(24*time.Hour/time.Second),
	}
	if err := d.Repo.CreateInvite(context.Background(), row); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	return code
}

// uniqueAPITestDSK keeps managed Bot fixtures independent of legacy all-zero
// Human device keys. These authorization fixtures do not validate credentials.
func uniqueAPITestDSK(t *testing.T) []byte {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub
}

// seedAPIDevice creates a user and one native device and returns both rows and
// the device's Ed25519 signing key.
func seedAPIDevice(t *testing.T, d api.Deps) (store.UserRow, store.DeviceRow, ed25519.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	now := d.Clock.Now().Unix()
	umkPub, sskPub, sigUMKSSK := testAccountKeys()
	u := store.UserRow{
		ID: id.New(), Username: "user" + id.New().String()[:8], Display: "User",
		UMKPub: umkPub, SSKPub: sskPub, SigUMKSSK: sigUMKSSK,
		Created: now,
	}
	if err := d.Repo.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	verified := now
	dev := store.DeviceRow{
		ID: id.New(), UserID: u.ID, DSKPub: pub, Tier: 0, SignerTier: 0,
		CredentialBlob: []byte{1}, VerifiedAt: &verified, LastSeen: now, Created: now,
	}
	if err := d.Repo.CreateDevice(ctx, dev); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return u, dev, priv
}

// seedAPISession creates a user, a device and one live enrolled session, and
// returns the bearer token. The session is established through the real
// challenge-and-signature path, so nothing here can pass a test the wire could
// not.
func seedAPISession(t *testing.T, d api.Deps) (store.UserRow, store.DeviceRow, string) {
	t.Helper()
	ctx := context.Background()
	u, dev, priv := seedAPIDevice(t, d)
	nonce, _, err := d.Sessions.Challenge(ctx, dev.ID)
	if err != nil {
		t.Fatalf("Challenge: %v", err)
	}
	pre := auth.SessionPreimage(d.Sessions.InstanceID(), dev.ID, nonce, auth.PurposeSession)
	tok, err := d.Sessions.Establish(ctx, auth.EstablishRequest{
		DeviceID: dev.ID, Nonce: nonce, Purpose: auth.PurposeSession, Sig: ed25519.Sign(priv, pre),
	})
	if err != nil {
		t.Fatalf("Establish: %v", err)
	}
	return u, dev, tok.Token
}

// seedSession is seedAPISession under the name the accounts tests use.
func seedSession(t *testing.T, d api.Deps) (store.UserRow, store.DeviceRow, string) {
	t.Helper()
	return seedAPISession(t, d)
}

// post sends body as application/cbor. It never calls t.Fatal, because the
// concurrent-redemption test calls it from 64 goroutines and a Fatal outside
// the test goroutine is a silent Goexit.
func post(_ *testing.T, h http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	return postCBOR(h, path, body)
}

// postCBOR posts an unauthenticated CBOR body.
func postCBOR(h http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	return postCBORAuth(h, path, body, "")
}

// postCBORAuth is postCBOR with an Authorization: Bearer header.
func postCBORAuth(h http.Handler, path string, body []byte, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/cbor")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// testLister is the device-list gate the api tests run behind: a user it has no entries for has
// published no list (the no-list clause, so every seeded device establishes enrolled, exactly as
// web-1's fixtures always did), and a test lists a user's (device_id, dsk_pub) pairs to drive the
// listed and unlisted branches of protocol/02 § Device sessions item 4.
type testLister struct {
	mu      sync.Mutex
	entries map[id.ID][]auth.ListedDevice
}

func (l *testLister) ListedDevices(_ context.Context, user id.ID) ([]auth.ListedDevice, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries, ok := l.entries[user]
	if !ok {
		return nil, auth.ErrNoDeviceList
	}
	return entries, nil
}

// list makes rows the user's newest verified list: each row's id and key, one entry each.
func (l *testLister) list(user id.ID, rows ...store.DeviceRow) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := make([]auth.ListedDevice, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, auth.ListedDevice{DeviceID: row.ID, DSKPub: row.DSKPub})
	}
	l.entries[user] = entries
}

func listerOf(t *testing.T, d api.Deps) *testLister {
	t.Helper()
	l, ok := d.Sessions.DeviceLists.(*testLister)
	if !ok {
		t.Fatalf("Sessions.DeviceLists is %T, want the harness's *testLister", d.Sessions.DeviceLists)
	}
	return l
}

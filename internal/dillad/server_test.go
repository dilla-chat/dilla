package dillad_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/build"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus"
	_ "modernc.org/sqlite"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/dillad"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/obs"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/jonasthim/dilla/internal/store/sqlite/migrations"
	"github.com/jonasthim/dilla/internal/web"
)

// testConfig writes a dilla.toml under t.TempDir(), migrates the database it
// names and inserts the instance row and one bootstrap invite, exactly as
// `dillad init` does, and returns the loaded config. newInstance is testConfig
// plus dillad.New, returning the server, its handler and the invite code;
// newInstanceWith takes a mutator that runs on the config before New.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, _ := testConfigInvite(t)
	return cfg
}

// testConfigInvite is testConfig plus the plain bootstrap invite code, which is
// printed once by `dillad init` and is not recoverable from the database.
func testConfigInvite(t *testing.T) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()

	c := config.Default()
	c.Instance.Domain = "dilla.test"
	c.Instance.PublicIP = netip.MustParseAddr("203.0.113.7")
	c.Instance.DataDir = dir
	c.DB.Path = filepath.Join(dir, "dilla.db")
	c.Blobs.Dir = filepath.Join(dir, "blobs")
	c.TLS.Agreed = true
	c.TURN.SharedSecretFile = writeTestSecret(t, dir, "turn.secret")
	c.TURN.RelayIP = c.Instance.PublicIP.String()
	c.LiveKit.APISecretFile = writeTestSecret(t, dir, "livekit.secret")
	c.Derive()

	write, err := sqlite.OpenWrite(c.DB.Path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
	if err != nil {
		write.Close()
		t.Fatalf("goose provider: %v", err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		write.Close()
		t.Fatalf("goose up: %v", err)
	}
	read, err := sqlite.OpenRead(c.DB.Path)
	if err != nil {
		write.Close()
		t.Fatalf("OpenRead: %v", err)
	}
	repo := sqlite.New(write, read)

	now := time.Now().Unix()
	senderKeyID, frankingKeyID := id.New(), id.New()
	instance := store.InstanceRow{
		InstanceID: id.New(), ExternalSenderKeyID: senderKeyID,
		KeyHistory:    testKeyHistory(t, senderKeyID, frankingKeyID, now),
		FrankingKeyID: frankingKeyID, Generation: 1, PolicyVersion: 1, Created: now,
	}
	code, hash := auth.NewInviteCode()
	invite := store.InviteRow{
		ID: id.New(), CodeHash: hash, GrantsAdmin: 1, MaxUses: 1,
		Created: now, ExpiresAt: now + int64(24*time.Hour/time.Second),
	}
	if err := repo.Tx(context.Background(), func(tx store.Repository) error {
		if err := tx.CreateInstance(context.Background(), instance); err != nil {
			return err
		}
		return tx.CreateInvite(context.Background(), invite)
	}); err != nil {
		repo.Close()
		t.Fatalf("bootstrap: %v", err)
	}
	// The composition root opens its own pools from the config, exactly as
	// `dillad serve` does, so the seeding pools are closed here.
	if err := repo.Close(); err != nil {
		t.Fatalf("close seeding repository: %v", err)
	}

	cfgPath := filepath.Join(dir, "dilla.toml")
	f, err := os.OpenFile(cfgPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create dilla.toml: %v", err)
	}
	if err := c.WriteConfig(f); err != nil {
		f.Close()
		t.Fatalf("write dilla.toml: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close dilla.toml: %v", err)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return loaded, code
}

// testKeyHistory is instances.key_history exactly as `dillad init` writes it
// (protocol/03-identity.md § Instance keys): the composition root reads the
// external-sender key and K_frank out of it, and refuses an instance whose
// history does not name both.
func testKeyHistory(t *testing.T, senderKeyID, frankingKeyID id.ID, now int64) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	frank := make([]byte, 32)
	if _, err := rand.Read(frank); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b, err := cborx.Marshal([]any{uint64(1), []any{
		[]any{uint64(0), senderKeyID, []byte(pub), priv.Seed(), uint64(now), nil},
		[]any{uint64(1), frankingKeyID, []byte{}, frank, uint64(now), nil},
	}})
	if err != nil {
		t.Fatalf("encode key history: %v", err)
	}
	return b
}

// writeTestSecret writes 32 CSPRNG bytes as hex at 0600: config.Validate refuses
// a secret file that is missing, group-readable or under 32 bytes.
func writeTestSecret(t *testing.T, dir, name string) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(buf)), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func newInstance(t *testing.T) (*dillad.Server, http.Handler, string) {
	t.Helper()
	return newInstanceWith(t, nil)
}

func newInstanceWith(t *testing.T, tune func(*config.Config)) (*dillad.Server, http.Handler, string) {
	t.Helper()
	srv, h, code, _ := newInstanceTuned(t, tune)
	return srv, h, code
}

// newInstanceTuned is newInstanceWith plus the config the instance was built
// from, which is what the instance document and the limits array are asserted
// against: every one of their elements is either a config value or a constant
// of protocol/09-http-api.md, and a test that only counts the elements would
// pass on a transposed array or an inverted enum.
func newInstanceTuned(t *testing.T, tune func(*config.Config)) (*dillad.Server, http.Handler, string, *config.Config) {
	t.Helper()
	cfg, code := testConfigInvite(t)
	if tune != nil {
		tune(cfg)
	}
	srv, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t),
	})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	return srv, srv.Handler(), code, cfg
}

// uints reads a decoded CBOR array of unsigned integers. cborx decodes a
// nested array into []any, so the enum elements need converting before they can
// be compared.
func uints(t *testing.T, v any, what string) []uint64 {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is %T, not an array", what, v)
	}
	out := make([]uint64, 0, len(raw))
	for i, e := range raw {
		n, ok := e.(uint64)
		if !ok {
			t.Fatalf("%s[%d] is %T, not an unsigned integer", what, i, e)
		}
		out = append(out, n)
	}
	return out
}

// testDevice is one client device's identity: the 16-byte id the registration
// body carries and the Ed25519 key the session ceremony proves possession of.
type testDevice struct {
	ID   id.ID
	Priv ed25519.PrivateKey
	UMK  ed25519.PrivateKey
	SSK  ed25519.PrivateKey
}

func newTestDevice(t *testing.T) testDevice {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	deviceID := id.New()
	seed := func(label string) []byte { sum := sha256.Sum256([]byte(label + deviceID.String())); return sum[:] }
	return testDevice{ID: deviceID, Priv: priv, UMK: ed25519.NewKeyFromSeed(seed("human-umk-")), SSK: ed25519.NewKeyFromSeed(seed("human-ssk-"))}
}

// newAccountRequest is a fully signed Human POST /v1/accounts fixture.
func newAccountRequest(code, username string, dev testDevice) []any {
	umkPub := dev.UMK.Public().(ed25519.PublicKey)
	sskPub := dev.SSK.Public().(ed25519.PublicKey)
	dskPub := dev.Priv.Public().(ed25519.PublicKey)
	sigUMKSSK := ed25519.Sign(dev.UMK, append([]byte("dilla ssk v1"), sskPub...))
	dskMessage := append([]byte("dilla dsk v1"), dev.ID[:]...)
	dskMessage = append(dskMessage, dskPub...)
	dskMessage = append(dskMessage, 0, 0, 0)
	sigSSKDev := ed25519.Sign(dev.SSK, dskMessage)
	credential, _ := cborx.Marshal([]any{uint64(1), umkPub, make([]byte, 16), dev.ID[:], uint64(0), uint64(0), uint64(0), sskPub, sigUMKSSK, sigSSKDev})
	return []any{
		code, username, "Jonas",
		umkPub, sskPub, sigUMKSSK,
		nil,
		[]any{
			dev.ID, dskPub,
			uint64(0), uint64(0), credential,
		},
	}
}

func post(t *testing.T, h http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/cbor")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// establishSession runs the real two-leg session ceremony against the handler
// and returns the establish response.
func establishSession(t *testing.T, h http.Handler, srv *dillad.Server, dev testDevice) *httptest.ResponseRecorder {
	t.Helper()
	res := post(t, h, "/v1/devices/"+dev.ID.String()+"/sessions/challenge", nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("POST sessions/challenge = %d", res.Code)
	}
	var challenge []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &challenge); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	nonce, ok := challenge[0].([]byte)
	if !ok {
		t.Fatalf("challenge element 0 is %T, want a byte string", challenge[0])
	}
	pre := auth.SessionPreimage(srv.Sessions().InstanceID(), dev.ID, nonce, auth.PurposeSession)
	body, err := cborx.Marshal([]any{nonce, uint64(auth.PurposeSession), ed25519.Sign(dev.Priv, pre), nil, nil})
	if err != nil {
		t.Fatalf("marshal establish: %v", err)
	}
	return post(t, h, "/v1/devices/"+dev.ID.String()+"/sessions", body)
}

func TestRegisterEstablishAndReadTheInstanceDocument(t *testing.T) {
	// migrated SQLite, one bootstrap invite. The two retention windows both
	// default to 30 days, so a transposed pair would be invisible in the limits
	// array; they are given distinct values here.
	srv, h, code, cfg := newInstanceTuned(t, func(c *config.Config) {
		c.Retention.HandshakeDays = 14
		c.Retention.CiphertextDays = 90
	})
	defer srv.Shutdown(context.Background())

	// 1. redeem the invite and create the account, which also mints the first
	//    device session.
	dev := newTestDevice(t)
	body, _ := cborx.Marshal(newAccountRequest(code, "jonas", dev))
	res := post(t, h, "/v1/accounts", body)
	if res.Code != http.StatusOK {
		t.Fatalf("POST /v1/accounts = %d", res.Code)
	}
	var account []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &account); err != nil {
		t.Fatalf("decode: %v", err)
	}
	token, ok := account[2].(string)
	if !ok || token == "" {
		t.Fatalf("no session token in the account response: %#v", account)
	}

	// 2. the instance document, with and without a session.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/instance", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/instance = %d with discovery = public", rec.Code)
	}
	var doc []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode instance: %v", err)
	}
	if len(doc) != 11 {
		t.Fatalf("the instance document has %d elements, want 11", len(doc))
	}
	// Every element, in the ORDER protocol/09-http-api.md § Instance fixes: a
	// transposed pair or an inverted enum passes a length check. wire_versions is the hello's
	// [4,3,2] since wire 4 (Task S1 part 2: the account-kind attestation).
	for i, want := range [][]uint64{{5, 4, 3, 2}, {1}, {1}} {
		names := []string{"wire_versions", "e2ee_versions", "media_versions"}
		if got := uints(t, doc[i], names[i]); !slices.Equal(got, want) {
			t.Fatalf("%s = %v, want %v", names[i], got, want)
		}
	}
	wantID := srv.Sessions().InstanceID()
	gotID, ok := doc[3].([]byte)
	if !ok {
		t.Fatalf("instance_id is %T, not a byte string", doc[3])
	}
	if !bytes.Equal(gotID, wantID[:]) {
		t.Fatalf("instance_id = %x, want the seeded row's %x", gotID, wantID[:])
	}
	if doc[4] != uint64(1) {
		t.Fatalf("generation = %v, want 1 on a fresh instance", doc[4])
	}
	if doc[5] != cfg.Instance.Domain {
		t.Fatalf("domain = %v, want %q", doc[5], cfg.Instance.Domain)
	}
	// registration_mode: 0 invite-only, 1 open, 2 closed. The default is
	// registration.mode = "invite", so it is the 0 whose meaning the document
	// fixes and not a zero value that happens to be there.
	if doc[6] != uint64(0) {
		t.Fatalf("registration_mode = %v with registration.mode = %q, want 0", doc[6], cfg.Registration.Mode)
	}
	// auth_methods: 0 password, 1 totp, 2 passkey, 3 oidc, ascending and with
	// no duplicates. The default auth.methods is password, totp, passkey.
	if got := uints(t, doc[7], "auth_methods"); !slices.Equal(got, []uint64{0, 1, 2}) {
		t.Fatalf("auth_methods = %v with auth.methods = %v, want [0 1 2]", got, cfg.Auth.Methods)
	}
	if doc[8] != uint64(1) {
		t.Fatalf("policy_version = %v, want the seeded row's 1", doc[8])
	}
	// external_sender_key_id and external_sender_pub: the current kind-0 entry of the key
	// history, the key every text and call group's external_senders extension must carry.
	row, err := srv.Repo().GetInstance(t.Context())
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got, _ := doc[9].([]byte); !bytes.Equal(got, row.ExternalSenderKeyID[:]) {
		t.Fatalf("external_sender_key_id = %x, want the instance row's %x", got, row.ExternalSenderKeyID[:])
	}
	var keys []any
	if err := cborx.Unmarshal(row.KeyHistory, &keys); err != nil {
		t.Fatalf("decode key_history: %v", err)
	}
	entries, _ := keys[1].([]any)
	var wantPub []byte
	for _, e := range entries {
		entry, _ := e.([]any)
		if keyID, _ := entry[1].([]byte); entry[0] == uint64(0) && bytes.Equal(keyID, row.ExternalSenderKeyID[:]) {
			wantPub, _ = entry[2].([]byte)
		}
	}
	if got, _ := doc[10].([]byte); len(wantPub) != 32 || !bytes.Equal(got, wantPub) {
		t.Fatalf("external_sender_pub = %x, want the key history's current public key %x", got, wantPub)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/instance/limits", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/instance/limits = %d", rec.Code)
	}
	var limits []uint64
	if err := cborx.Unmarshal(rec.Body.Bytes(), &limits); err != nil {
		t.Fatalf("decode limits: %v", err)
	}
	if len(limits) != 11 {
		t.Fatalf("the limits document has %d elements, want 11", len(limits))
	}
	// All eleven, in order, each against the config value or protocol constant
	// it comes from: heartbeat_ms, the two retention windows and max_blob_bytes
	// could otherwise be in any order.
	for _, want := range []struct {
		i    int
		name string
		want uint64
	}{
		{0, "max_ciphertext_bytes", uint64(cfg.Limits.MaxCiphertextBytes)},
		{1, "max_blob_bytes", uint64(cfg.Blobs.MaxBlobBytes)},
		{2, "max_attachments", 4},
		{3, "max_previews", 2},
		{4, "max_keypackages_per_device", uint64(cfg.Limits.MaxKeypackagesPerDevice)},
		{5, "keypackage_refill_threshold", uint64(cfg.Limits.KeypackageRefillThreshold)},
		{6, "quota_bytes_per_user", uint64(cfg.Blobs.QuotaBytesPerUser)},
		{7, "heartbeat_ms", uint64(cfg.Gateway.HeartbeatInterval.Value() / time.Millisecond)},
		{8, "max_frame_bytes", cfg.MaxFrameBytes()},
		{9, "retention_handshake_days", uint64(cfg.Retention.HandshakeDays)},
		{10, "retention_ciphertext_days", uint64(cfg.Retention.CiphertextDays)},
	} {
		if limits[want.i] != want.want {
			t.Fatalf("limits[%d] (%s) = %d, want %d", want.i, want.name, limits[want.i], want.want)
		}
	}
	// The wave-wide ciphertext cap and the derived frame size, spelled out so
	// a config default that drifts is caught here and not in a client.
	if limits[0] != 131072 {
		t.Fatalf("max_ciphertext_bytes = %d, want 131072", limits[0])
	}
	if limits[8] != limits[0]+512 {
		t.Fatalf("max_frame_bytes = %d, want max_ciphertext_bytes + 512 = %d", limits[8], limits[0]+512)
	}

	// The session response's element 6 and the X-Dilla-Generation header carry
	// the same number, so a client that compares them sees a restore and not a
	// bug (R29, D12). POST /v1/accounts answers the FOUR-element array
	// interfaces.md §5.1 fixes and carries no generation element, so the
	// comparison is made on the session ceremony's own seven-element answer.
	if got := res.Header().Get("X-Dilla-Generation"); got != "1" {
		t.Fatalf("X-Dilla-Generation = %q on the registration response, want \"1\"", got)
	}
	sres := establishSession(t, h, srv, dev)
	if sres.Code != http.StatusCreated {
		t.Fatalf("POST /v1/devices/{device_id}/sessions = %d", sres.Code)
	}
	var session []any
	if err := cborx.Unmarshal(sres.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if len(session) != 7 {
		t.Fatalf("the session response has %d elements, want 7", len(session))
	}
	if session[6] != uint64(1) {
		t.Fatalf("the session response's generation = %v, want 1 on a fresh instance", session[6])
	}
	if got := sres.Header().Get("X-Dilla-Generation"); got != "1" {
		t.Fatalf("X-Dilla-Generation = %q on the same response, want \"1\"", got)
	}

	// 3. an authenticated read.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/accounts/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/accounts/me = %d", rec.Code)
	}
}

// protocol/09-http-api.md § Instance requires auth_methods "ascending, no
// duplicates", and config.Validate constrains neither the order an operator
// writes auth.methods in nor a repeated entry, so the document does the sorting
// and the de-duplication itself.
func TestAuthMethodsAreSortedAndDeduplicated(t *testing.T) {
	srv, h, _, _ := newInstanceTuned(t, func(c *config.Config) {
		c.Auth.Methods = []string{"passkey", "password", "passkey"}
	})
	defer srv.Shutdown(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/instance", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/instance = %d", rec.Code)
	}
	var doc []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode instance: %v", err)
	}
	if got := uints(t, doc[7], "auth_methods"); !slices.Equal(got, []uint64{0, 2}) {
		t.Fatalf("auth_methods = %v for [passkey password passkey], want [0 2]", got)
	}
}

func TestEveryResponseCarriesTheGenerationHeader(t *testing.T) {
	srv, h, _ := newInstance(t)
	defer srv.Shutdown(context.Background())
	for _, path := range []string{"/v1/instance", "/v1/instance/limits", "/healthz", "/v1/accounts/me"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Header().Get("X-Dilla-Generation") == "" {
			t.Fatalf("%s carries no X-Dilla-Generation header (status %d)", path, rec.Code)
		}
	}
}

func TestDiscoverySessionMakesTheInstanceDocumentAuthenticated(t *testing.T) {
	srv, h, _ := newInstanceWith(t, func(c *config.Config) { c.Instance.Discovery = "session" })
	defer srv.Shutdown(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/instance", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/instance = %d with discovery = session, want 401", rec.Code)
	}
}

func TestShutdownClosesTheListenerWithinTheGrace(t *testing.T) {
	srv, _, _ := newInstance(t)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(context.Background(), ln)
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("Shutdown took %s, longer than shutdown_grace", elapsed)
	}
	if _, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(t.Context(), "tcp", ln.Addr().String()); err == nil {
		t.Fatal("the listener is still accepting connections after Shutdown")
	}
}

// facts-http-gateway §4.1 is a recorded CORRECTION: h2c is stdlib in Go 1.27
// (Server.Protocols + SetUnencryptedHTTP2), not x/net/http2/h2c. Without the
// field a front proxy speaking unencrypted HTTP/2 cannot reach the API in
// behind_proxy mode, and the one fact the facts file went out of its way to
// correct goes silently unused. The direct-TLS listener (Plan 2 task 16) speaks
// h2 over TLS instead, and never unencrypted HTTP/2.
func TestTheServerSpeaksUnencryptedHTTP2AndHTTP1(t *testing.T) {
	srv, _, _ := newInstanceWith(t, func(c *config.Config) {
		c.TLS.Mode = config.TLSModeBehindProxy
		c.Server.TrustedProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	})
	defer srv.Shutdown(context.Background())
	p := srv.Protocols()
	if p == nil {
		t.Fatal("http.Server.Protocols is unset; the listener is HTTP/1 only")
	}
	if !p.UnencryptedHTTP2() {
		t.Fatal("unencrypted HTTP/2 is not enabled")
	}
	if !p.HTTP1() {
		t.Fatal("HTTP/1 is not enabled; the /gateway route needs it for the WebSocket upgrade")
	}

	direct, _, _ := newInstance(t) // acme_tls_alpn: the listener behind the 443 demux
	defer direct.Shutdown(context.Background())
	if p := direct.Protocols(); p == nil || p.UnencryptedHTTP2() || !p.HTTP2() || !p.HTTP1() {
		t.Fatal("the direct-TLS listener must speak HTTP/1.1 and h2, and never unencrypted HTTP/2")
	}
}

// interfaces.md §7.3 requires the test harness to reach the repository, the
// delivery service and the gateway, and Options must default what it is not
// given: part 1b's harness calls New with the config, a clock and the wasi core
// alone, and New opens the store, compiles the runtime and builds the gateway
// and the delivery service itself (task 27a).
func TestNewDefaultsTheCollaboratorsItIsNotGiven(t *testing.T) {
	cfg := testConfig(t) // writes dilla.toml, migrates the database, runs init's bootstrap
	srv, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.System(), CorePath: testCorePath(t),
	})
	if err != nil {
		t.Fatalf("New with only Config, Clock and the core: %v", err)
	}
	defer srv.Shutdown(context.Background())
	if srv.Repo() == nil {
		t.Fatal("Server.Repo() is nil after New opened the store itself")
	}
	if srv.Mux() == nil {
		t.Fatal("Server.Mux() is nil; parts 1b and 2 mount their routes through it")
	}
	if srv.DS() == nil || srv.Gateway() == nil {
		t.Fatal("DS() and Gateway() must be what New built")
	}
	// protocol/02 § Device sessions item 4: a nil device-list gate refuses every establish, which
	// no handler test would notice, so the composition root's wiring is asserted here.
	if srv.Sessions().DeviceLists == nil {
		t.Fatal("Sessions().DeviceLists is nil: every no-login establish would answer 401")
	}
}

// Options.Extra is the registrar hook parts 1b and 2 mount through.
func TestExtraRegistrarsAreMounted(t *testing.T) {
	cfg := testConfig(t)
	srv, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t),
		Extra: []func(*server.Mux){func(m *server.Mux) {
			m.HandleFunc("GET /v1/extra", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTeapot)
			})
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Shutdown(context.Background())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/extra", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("an Extra registrar's route answered %d; it was never mounted", rec.Code)
	}
}

// metrics.require_admin is on by DEFAULT and Options.ScrapeToken is a plain
// string, so the composition root must not hand obs.Metrics.Handler an empty
// token: it compares the Authorization header against it in constant time, and
// a request with no header compares equal to "". Mounting /metrics that way
// would publish every counter of a default instance to anyone who asks.
func TestMetricsIsNotOpenWhenNoScrapeTokenWasSupplied(t *testing.T) {
	srv, h, _ := newInstance(t)
	defer srv.Shutdown(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /metrics with no Authorization header = %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "dilla_http_requests_total") {
		t.Fatal("an unauthenticated scrape read the registry")
	}
}

// goToolPath resolves the `go` command to shell out to. It must NOT be a
// literal absolute path: the toolchain lives at /home/thim/.local/go on the
// development box and under /opt/hostedtoolcache/go/<version>/x64 on the CI
// runner actions/setup-go provisions, so a hard-coded path makes exec.Command
// fail to start in CI ("fork/exec …: no such file or directory") and turns the
// fail-closed `go` gate red on a machine where nothing is actually wrong.
//
// build.Default.GOROOT is $GOROOT when it is set and otherwise the GOROOT of
// the toolchain that built this test binary — the one whose `go list` already
// agrees with go.mod's `go 1.27.3`, so it is preferred over PATH, which may
// hold an older go that would try to download a toolchain instead.
func goToolPath(t *testing.T) string {
	t.Helper()
	if root := build.Default.GOROOT; root != "" {
		p := filepath.Join(root, "bin", "go")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	p, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go toolchain: GOROOT %q holds no bin/go and PATH has none: %v", build.Default.GOROOT, err)
	}
	return p
}

func TestTheReleaseBinaryDoesNotLinkTheTestHelpers(t *testing.T) {
	// The MODULE path, not "./cmd/dillad": the test's working directory is
	// internal/dillad, where that relative path does not exist and go list exits
	// non-zero on every run.
	out, err := exec.CommandContext(t.Context(), goToolPath(t), "list", "-deps",
		"github.com/jonasthim/dilla/cmd/dillad").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	// Two seeding helpers exist, and neither may ship: dilladtest seeds a whole instance, and
	// internal/ds/dstest writes a group past every check of the registration route (hardening G).
	for _, helper := range []string{"dilladtest", "internal/ds/dstest"} {
		if strings.Contains(string(out), helper) {
			t.Fatalf("cmd/dillad depends on %s; the seeding helpers must never ship", helper)
		}
	}
}

// server.RequestLog logs and calls observe AFTER next.ServeHTTP returns, with
// no defer, so whichever middleware sits INSIDE it is the one whose panics it
// can still account for. With Recover as the outermost wrapper the unwinding
// skips both: a panicking request produces no msg=http access-log line and no
// dilla_http_requests_total / dilla_http_request_duration_seconds sample, so
// handler panics never reach the 5xx rate an operator alerts on. Wiring the
// middlewares is this package's one job, so the order is asserted here.
func TestAPanickingHandlerIsStillLoggedAndCounted(t *testing.T) {
	cfg := testConfig(t)
	var logs bytes.Buffer
	reg := prometheus.NewRegistry()
	srv, err := dillad.New(context.Background(), dillad.Options{
		Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t),
		Log:         obs.NewLogger(cfg.Log, &logs),
		Metrics:     obs.NewMetrics(reg, reg),
		ScrapeToken: "scrape-me",
		Extra: []func(*server.Mux){func(m *server.Mux) {
			m.HandleFunc("GET /v1/panic", func(http.ResponseWriter, *http.Request) {
				panic("boom")
			})
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Shutdown(context.Background())
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/panic", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /v1/panic = %d, want 500 from server.Recover", rec.Code)
	}

	var access string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"msg":"http"`) && strings.Contains(line, `"route":"GET /v1/panic"`) {
			access = line
		}
	}
	if access == "" {
		t.Fatalf("the panicking request produced no msg=http access-log line; the log held:\n%s", logs.String())
	}
	if !strings.Contains(access, `"status":500`) {
		t.Fatalf("the access-log line records the wrong status: %s", access)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, cfg.Metrics.Path, nil)
	req.Header.Set("Authorization", "Bearer scrape-me")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s with the scrape token = %d", cfg.Metrics.Path, rec.Code)
	}
	want := `dilla_http_requests_total{code="5xx",method="GET",route="GET /v1/panic"} 1`
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("the panicking request was never counted; %q is absent from the scrape", want)
	}
	if !strings.Contains(rec.Body.String(), `dilla_http_request_duration_seconds_count{route="GET /v1/panic"} 1`) {
		t.Fatal("the panicking request contributed no duration sample")
	}
}

// webTree is a client tree with the manifest task 22's writer would emit for it.
func webTree(t *testing.T, files map[string]string) fstest.MapFS {
	t.Helper()
	type entry struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Size   int    `json:"size"`
	}
	doc := struct {
		V     int     `json:"v"`
		Files []entry `json:"files"`
	}{V: 1, Files: []entry{}}
	fsys := fstest.MapFS{}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		sum := sha256.Sum256([]byte(files[p]))
		doc.Files = append(doc.Files, entry{Path: p, SHA256: hex.EncodeToString(sum[:]), Size: len(files[p])})
		fsys[p] = &fstest.MapFile{Data: []byte(files[p])}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal the manifest: %v", err)
	}
	fsys[web.ManifestName] = &fstest.MapFile{Data: append(raw, '\n')}
	return fsys
}

func wantCSP(host string) string {
	return "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; img-src 'self' blob:; " +
		"font-src 'self'; connect-src 'self' ws://" + host + " wss://" + host + "; worker-src 'self'; " +
		"media-src blob:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}

func serveOne(t *testing.T, h http.Handler, method, path, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	if host != "" {
		req.Host = host
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// C9: the instance serves the client from its own origin; a binary built without one serves the
// committed placeholder, through the whole handler chain.
func TestTheEmbeddedWebClientIsServedAtTheRoot(t *testing.T) {
	srv, h, _ := newInstance(t)
	defer srv.Shutdown(context.Background())
	for _, path := range []string{"/", "/welcome", "/c/0123456789abcdef0123456789abcdef"} {
		rec := serveOne(t, h, http.MethodGet, path, "dilla.test")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<meta name="dilla-web" content="placeholder">`) {
			t.Fatalf("GET %s = %d %q, want the placeholder", path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP("dilla.test") {
			t.Errorf("GET %s: CSP %q", path, got)
		}
		if rec.Header().Get("X-Dilla-Generation") == "" {
			t.Errorf("GET %s carries no X-Dilla-Generation", path)
		}
	}
}

// The catch-all never answers for the API: every reserved or routed path answers exactly as it did
// before the client was mounted (the mux's 404 body, its 405 with the route's Allow, the upgrade's
// 426), and never with the page.
func TestTheAPIAnswersAsBeforeBesideTheWebClient(t *testing.T) {
	srv, h, _ := newInstance(t)
	defer srv.Shutdown(context.Background())
	message := "/v1/groups/" + id.New().String() + "/message"
	for _, c := range []struct {
		method, path string
		status       int
		allow, body  string
	}{
		{http.MethodGet, "/v1/nope", http.StatusNotFound, "", "404 page not found\n"},
		{http.MethodPost, "/v1/nope", http.StatusNotFound, "", "404 page not found\n"},
		{http.MethodGet, "/v1", http.StatusNotFound, "", "404 page not found\n"},
		{http.MethodGet, message, http.StatusMethodNotAllowed, "POST", ""},
		{http.MethodPost, "/", http.StatusMethodNotAllowed, "GET, HEAD", "method not allowed\n"},
		{http.MethodPost, "/gateway", http.StatusMethodNotAllowed, "GET, HEAD", ""},
		{http.MethodGet, "/gateway", http.StatusUpgradeRequired, "", ""},
		{http.MethodGet, "/rtc/validate", http.StatusNotFound, "", "404 page not found\n"},
		{http.MethodPost, "/debug/sfu/token", http.StatusNotFound, "", "404 page not found\n"},
		{http.MethodGet, "/healthz", http.StatusOK, "", ""},
	} {
		rec := serveOne(t, h, c.method, c.path, "")
		if rec.Code != c.status {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.status)
		}
		if c.allow != "" && rec.Header().Get("Allow") != c.allow {
			t.Errorf("%s %s: Allow %q, want %q", c.method, c.path, rec.Header().Get("Allow"), c.allow)
		}
		if c.body != "" && rec.Body.String() != c.body {
			t.Errorf("%s %s: body %q, want %q", c.method, c.path, rec.Body.String(), c.body)
		}
		if strings.Contains(rec.Body.String(), "dilla-web") {
			t.Errorf("%s %s answered with the web client", c.method, c.path)
		}
		if rec.Header().Get("X-Dilla-Generation") == "" {
			t.Errorf("%s %s carries no X-Dilla-Generation", c.method, c.path)
		}
	}
}

// With an SFU, New mounts /rtc and /rtc/ without a method (routes.go:241-242). The client must be
// served beside them: a "GET /" pattern would make this New panic (ruling 32).
func TestTheWebClientAndTheSignallingProxyServeSideBySide(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()
	srv, err := dillad.New(context.Background(), dillad.Options{
		Config: testConfig(t), Clock: clock.System(), Wasm: sharedRuntime(t), SFU: upstreamSFU{url: upstream.URL},
	})
	if err != nil {
		t.Fatalf("dillad.New with an SFU: %v", err)
	}
	defer srv.Shutdown(context.Background())
	if rec := serveOne(t, srv.Handler(), http.MethodGet, "/", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `content="placeholder"`) {
		t.Fatalf("GET / beside the SFU = %d %q", rec.Code, rec.Body.String())
	}
	if rec := serveOne(t, srv.Handler(), http.MethodGet, "/rtc/validate?access_token=x", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("GET /rtc/validate = %d, want the join gate's 403", rec.Code)
	}
	if rec := serveOne(t, srv.Handler(), http.MethodPost, "/rtc", ""); rec.Code != http.StatusMethodNotAllowed ||
		rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST /rtc = %d (Allow %q), want the proxy's own 405 with Allow GET", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestOptionsWebReplacesTheEmbeddedClient(t *testing.T) {
	const page = "<!doctype html><title>built</title>\n"
	srv, err := dillad.New(context.Background(), dillad.Options{
		Config: testConfig(t), Clock: clock.System(), Wasm: sharedRuntime(t),
		Web: webTree(t, map[string]string{"index.html": page}),
	})
	if err != nil {
		t.Fatalf("dillad.New: %v", err)
	}
	defer srv.Shutdown(context.Background())
	if rec := serveOne(t, srv.Handler(), http.MethodGet, "/", ""); rec.Body.String() != page {
		t.Fatalf("GET / = %q, want the tree passed in Options.Web", rec.Body.String())
	}
}

// Ruling 12: a client tree its manifest does not describe stops the instance from starting.
func TestNewRefusesAWebClientItsManifestDoesNotDescribe(t *testing.T) {
	const page = "<!doctype html><title>built</title>\n"
	tampered := webTree(t, map[string]string{"index.html": page})
	tampered["index.html"] = &fstest.MapFile{Data: []byte(strings.ToUpper(page))}
	srv, err := dillad.New(context.Background(), dillad.Options{
		Config: testConfig(t), Clock: clock.System(), Wasm: sharedRuntime(t), Web: tampered,
	})
	if err == nil {
		_ = srv.Shutdown(context.Background())
		t.Fatal("dillad.New accepted a client whose index.html does not match its manifest")
	}
	if !strings.HasPrefix(err.Error(), "dillad: web client: ") ||
		!strings.Contains(err.Error(), "index.html does not match its manifest sha256") {
		t.Fatalf("dillad.New = %v", err)
	}
}

// gateEntry is one entry of protocol/03-identity.md's device list:
// [device_id, dsk_pub, tier, added_at, revoked_at|null].
type gateEntry struct {
	_         struct{} `cbor:",toarray"`
	DeviceID  []byte
	DSKPub    []byte
	Tier      uint64
	AddedAt   uint64
	RevokedAt *uint64
}

// gateListUnsigned is the five-element array the SSK signs.
type gateListUnsigned struct {
	_        struct{} `cbor:",toarray"`
	V        uint64
	UserID   []byte
	Version  uint64
	PrevHash []byte
	Entries  []gateEntry
}

// gateListSigned is the six-element list the instance stores and serves.
type gateListSigned struct {
	_        struct{} `cbor:",toarray"`
	V        uint64
	UserID   []byte
	Version  uint64
	PrevHash []byte
	Entries  []gateEntry
	SigSSK   []byte
}

// signedDeviceList builds version `version` of user's list, chained to prevBlob (nil: 32 zero
// bytes, the genesis rule), signed by ssk under "dilla devices v1" exactly as a client signs it,
// and returns the PUT body [version, blob, ssk_signature, prev_hash] and the blob, whose SHA-256
// the next version carries.
func signedDeviceList(t *testing.T, ssk ed25519.PrivateKey, user id.ID, version uint64, prevBlob []byte, entries []gateEntry) ([]any, []byte) {
	t.Helper()
	prev := make([]byte, 32)
	if prevBlob != nil {
		sum := sha256.Sum256(prevBlob)
		prev = sum[:]
	}
	unsigned, err := cborx.Marshal(gateListUnsigned{V: 1, UserID: user[:], Version: version, PrevHash: prev, Entries: entries})
	if err != nil {
		t.Fatalf("encode the unsigned list: %v", err)
	}
	sig := ed25519.Sign(ssk, append([]byte("dilla devices v1"), unsigned...))
	blob, err := cborx.Marshal(gateListSigned{V: 1, UserID: user[:], Version: version, PrevHash: prev,
		Entries: entries, SigSSK: sig})
	if err != nil {
		t.Fatalf("encode the signed list: %v", err)
	}
	return []any{version, blob, sig, prev}, blob
}

func pubOf(dev testDevice) []byte { return []byte(dev.Priv.Public().(ed25519.PublicKey)) }

func entryFor(dev testDevice, tier uint64, revokedAt *uint64) gateEntry {
	return gateEntry{DeviceID: dev.ID[:], DSKPub: pubOf(dev), Tier: tier, AddedAt: 1, RevokedAt: revokedAt}
}

// accountWithKeys registers username with a password and with ssk's public half as users.ssk_pub,
// which is what the instance verifies the user's lists against; dev is the first (native) device.
func accountWithKeys(t *testing.T, h http.Handler, code, username, password string, dev testDevice, ssk ed25519.PrivateKey) (id.ID, string) {
	t.Helper()
	umkPub := dev.UMK.Public().(ed25519.PublicKey)
	sskPub := ssk.Public().(ed25519.PublicKey)
	dskPub := pubOf(dev)
	sigUMKSSK := ed25519.Sign(dev.UMK, append([]byte("dilla ssk v1"), sskPub...))
	dskMessage := append([]byte("dilla dsk v1"), dev.ID[:]...)
	dskMessage = append(dskMessage, dskPub...)
	dskMessage = append(dskMessage, 0, 0, 0)
	sigSSKDev := ed25519.Sign(ssk, dskMessage)
	credential, err := cborx.Marshal([]any{uint64(1), umkPub, make([]byte, 16), dev.ID[:], uint64(0), uint64(0), uint64(0), sskPub, sigUMKSSK, sigSSKDev})
	if err != nil {
		t.Fatalf("encode account Human credential: %v", err)
	}
	rec := call(t, h, http.MethodPost, "/v1/accounts", "", []any{
		code, username, "Test", umkPub, sskPub, sigUMKSSK,
		password, []any{dev.ID, dskPub, uint64(0), uint64(0), credential},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/accounts %s = %d %q", username, rec.Code, errorCode(rec))
	}
	out := decodeArray(t, rec)
	token, _ := out[2].(string)
	return idOf(t, out[0]), token
}

// mintInvite writes one more one-use invite, as an admin's invite would.
func mintInvite(t *testing.T, srv *dillad.Server) string {
	t.Helper()
	code, hash := auth.NewInviteCode()
	now := srv.Now().Unix()
	if err := srv.Repo().CreateInvite(context.Background(), store.InviteRow{ID: id.New(), CodeHash: hash,
		MaxUses: 1, Created: now, ExpiresAt: now + int64(24*time.Hour/time.Second)}); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	return code
}

// loginAssertion runs the password ceremony and returns the enrolment assertion.
func loginAssertion(t *testing.T, h http.Handler, username, password string) string {
	t.Helper()
	rec := call(t, h, http.MethodPost, "/v1/auth/password/login", "", []any{username, password})
	if rec.Code != http.StatusOK {
		t.Fatalf("password login %s = %d %q", username, rec.Code, errorCode(rec))
	}
	out := decodeArray(t, rec)
	assertion, _ := out[0].(string)
	if assertion == "" || out[1] != uint64(0) {
		t.Fatalf("password login answered %v, want [assertion, 0]", out)
	}
	return assertion
}

// establishAs runs challenge and establish for dev with element 3 and login as given (nil → null).
func establishAs(t *testing.T, h http.Handler, srv *dillad.Server, dev testDevice, element3 any, login []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := post(t, h, "/v1/devices/"+dev.ID.String()+"/sessions/challenge", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("challenge = %d", rec.Code)
	}
	nonce, _ := decodeArray(t, rec)[0].([]byte)
	pre := auth.SessionPreimage(srv.Sessions().InstanceID(), dev.ID, nonce, auth.PurposeSession)
	var l any
	if login != nil {
		l = login
	}
	return call(t, h, http.MethodPost, "/v1/devices/"+dev.ID.String()+"/sessions", "",
		[]any{nonce, uint64(auth.PurposeSession), ed25519.Sign(dev.Priv, pre), element3, l})
}

// scopeOf requires a 201 and returns its scope and token.
func scopeOf(t *testing.T, rec *httptest.ResponseRecorder) (uint64, string) {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("establish = %d %q (%x), want 201", rec.Code, errorCode(rec), rec.Body.Bytes())
	}
	out := decodeArray(t, rec)
	scope, _ := out[1].(uint64)
	token, _ := out[0].(string)
	return scope, token
}

// The gate through the composition root: the real wasm verifier behind the real lister adapter,
// the real password ceremony, the real assertion store. Attacker statements as in
// internal/auth: a stolen password registers nothing for a no-list account and only a pending
// device for a listed one.
func TestTheDeviceListGateRunsThroughTheCompositionRoot(t *testing.T) {
	srv, h, code := newInstance(t)
	defer srv.Shutdown(context.Background())
	const password = "correct horse battery staple"
	ssk := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x5a}, 32))
	d0 := newTestDevice(t)
	alice, tok0 := accountWithKeys(t, h, code, "alice", password, d0, ssk)
	own := "/v1/users/" + alice.String() + "/device-list"

	if scope, _ := scopeOf(t, establishAs(t, h, srv, d0, nil, nil)); scope != 0 {
		t.Fatalf("no list yet: scope %d, want 0 (the no-list clause)", scope)
	}
	body1, blob1 := signedDeviceList(t, ssk, alice, 1, nil, []gateEntry{entryFor(d0, 0, nil)})
	if rec := call(t, h, http.MethodPut, own, tok0, body1); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT v1 = %d %q", rec.Code, errorCode(rec))
	}
	if scope, _ := scopeOf(t, establishAs(t, h, srv, d0, nil, nil)); scope != 0 {
		t.Fatalf("listed: scope %d, want 0", scope)
	}

	d1 := newTestDevice(t)
	reg := []any{d1.ID, pubOf(d1), uint64(1), uint64(1), bytes.Repeat([]byte{0}, 10)}
	scope, p1 := scopeOf(t, establishAs(t, h, srv, d1, reg, []byte(loginAssertion(t, h, "alice", password))))
	if scope != 1 {
		t.Fatalf("a host-login registration: scope %d, want 1 (pending)", scope)
	}
	if rec := call(t, h, http.MethodGet, own, p1, nil); rec.Code != http.StatusOK {
		t.Fatalf("pending GET own list = %d %q, want 200", rec.Code, errorCode(rec))
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/v1/keypackages"},
		{http.MethodPost, "/v1/devices"},
		{http.MethodGet, "/v1/communities"},
	} {
		if rec := call(t, h, r.method, r.path, p1, []any{}); rec.Code != http.StatusForbidden || errorCode(rec) != "E_FORBIDDEN" {
			t.Errorf("pending %s %s = %d %q, want 403 E_FORBIDDEN", r.method, r.path, rec.Code, errorCode(rec))
		}
	}
	if scope, _ := scopeOf(t, establishAs(t, h, srv, d1, nil, nil)); scope != 1 {
		t.Fatalf("an unlisted device of a listed user re-establishing: scope %d, want 1", scope)
	}
	body2, _ := signedDeviceList(t, ssk, alice, 2, blob1, []gateEntry{entryFor(d0, 0, nil), entryFor(d1, 1, nil)})
	if rec := call(t, h, http.MethodPut, own, tok0, body2); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT v2 = %d %q", rec.Code, errorCode(rec))
	}
	if scope, _ := scopeOf(t, establishAs(t, h, srv, d1, nil, nil)); scope != 0 {
		t.Fatalf("once listed: scope %d, want 0", scope)
	}

	// A stolen password for an account that never published a list.
	bobSSK := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x6b}, 32))
	accountWithKeys(t, h, mintInvite(t, srv), "bob", password, newTestDevice(t), bobSSK)
	d2 := newTestDevice(t)
	reg2 := []any{d2.ID, pubOf(d2), uint64(1), uint64(1), bytes.Repeat([]byte{0}, 10)}
	rec := establishAs(t, h, srv, d2, reg2, []byte(loginAssertion(t, h, "bob", password)))
	if rec.Code != http.StatusUnauthorized || errorCode(rec) != "E_UNAUTHENTICATED" {
		t.Fatalf("registration for a no-list account = %d %q, want 401 E_UNAUTHENTICATED", rec.Code, errorCode(rec))
	}
	if _, err := srv.Repo().GetDevice(context.Background(), d2.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a stolen password created a device row: %v", err)
	}
}

// Boundary ruling 4 (task 7 scan): only an absent list is "no list". A stored list that exists but
// cannot be decoded answers 401 E_UNAUTHENTICATED, as a list failing verification does, through
// the real verifier behind the composition root's lister: never the no-list clause's enrolled
// scope, and never 503, which stays for instance faults. Attacker: whoever could put unreadable
// bytes in device_lists (a damaged row, a restore gone wrong) would otherwise make every device of
// the user enrolled, an unlisted one included.
func TestAStoredListThatCannotBeDecodedIsRefused(t *testing.T) {
	srv, h, _ := newInstance(t)
	defer srv.Shutdown(context.Background())
	const password = "correct horse battery staple"
	for i, garbage := range map[string][]byte{
		"bytes that are not CBOR":         bytes.Repeat([]byte{0xff}, 96),
		"a CBOR array of the wrong shape": {0x83, 0x01, 0x02, 0x03},
		"a truncated list":                {0x86, 0x01, 0x50},
	} {
		t.Run(i, func(t *testing.T) {
			ssk := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(len(i))}, 32))
			d0 := newTestDevice(t)
			name := "u" + id.New().String()[:12]
			user, _ := accountWithKeys(t, h, mintInvite(t, srv), name, password, d0, ssk)
			if err := srv.Repo().PutDeviceList(context.Background(), store.DeviceListRow{
				UserID: user, Version: 1, Blob: garbage, SSKSignature: make([]byte, 64),
				PrevHash: make([]byte, 32), Created: srv.Now().Unix(),
			}); err != nil {
				t.Fatalf("PutDeviceList: %v", err)
			}
			rec := establishAs(t, h, srv, d0, nil, nil)
			if rec.Code != http.StatusUnauthorized || errorCode(rec) != "E_UNAUTHENTICATED" {
				t.Fatalf("establish under an undecodable list = %d %q, want 401 E_UNAUTHENTICATED", rec.Code, errorCode(rec))
			}
			d1 := newTestDevice(t)
			reg := []any{d1.ID, pubOf(d1), uint64(1), uint64(1), bytes.Repeat([]byte{0}, 10)}
			rec = establishAs(t, h, srv, d1, reg, []byte(loginAssertion(t, h, name, password)))
			if rec.Code != http.StatusUnauthorized || errorCode(rec) != "E_UNAUTHENTICATED" {
				t.Fatalf("registration under an undecodable list = %d %q, want 401 E_UNAUTHENTICATED", rec.Code, errorCode(rec))
			}
			if _, err := srv.Repo().GetDevice(context.Background(), d1.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("a registration under an undecodable list created a device row: %v", err)
			}
		})
	}
}

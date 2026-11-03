package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// The rendered pkg.go.dev summary for SessionData is wrong: it shows
// Challenge []byte and an AllowCredentials field. The source at v0.18.2 has
// Challenge string (base64url) and AllowedCredentialIDs [][]byte. Anyone who
// wrote the ceremony column types from the docs would get them wrong, so this
// is a compile-time assertion against the real struct.
var (
	_ string   = webauthn.SessionData{}.Challenge
	_ [][]byte = webauthn.SessionData{}.AllowedCredentialIDs
)

// passkeyRPID is the relying party every test in this file registers against.
// A second spelling appears only where a test needs a credential that belongs
// to somebody else's RP.
const passkeyRPID = "chat.example"

// newPasskeys returns a Passkeys over a migrated temporary database, that
// database's repository, and the fake clock both of them read.
func newPasskeys(t *testing.T) (*auth.Passkeys, store.Repository, *clock.Fake) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "passkeys.db")
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
	clk := clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))

	cfg := config.Default().Auth.WebAuthn
	cfg.RPID = passkeyRPID
	cfg.RPDisplayName = "dilla test"
	cfg.RPOrigins = []string{"https://" + passkeyRPID}
	pk, err := auth.NewPasskeys(auth.ConfigFromDilla(cfg), repo, clk)
	if err != nil {
		t.Fatalf("NewPasskeys: %v", err)
	}
	return pk, repo, clk
}

// seedAuthUser inserts one account and returns its id.
func seedAuthUser(t *testing.T, repo store.Repository) id.ID {
	t.Helper()
	u := store.UserRow{ID: id.New(), Username: "passkey" + id.New().String()[:8], Display: "Passkey",
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: 1}
	if err := repo.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u.ID
}

// handleFor reads the webauthn_users row for one account and relying party.
func handleFor(t *testing.T, repo store.Repository, user id.ID, rpID string) []byte {
	t.Helper()
	handle, err := repo.GetWebauthnUserHandle(context.Background(), rpID, user)
	if err != nil {
		t.Fatalf("GetWebauthnUserHandle: %v", err)
	}
	return handle
}

func TestSessionDataFieldTypesAreTheSourcesNotTheDocs(t *testing.T) {
	st := reflect.TypeOf(webauthn.SessionData{})
	ch, ok := st.FieldByName("Challenge")
	if !ok || ch.Type.Kind() != reflect.String {
		t.Fatalf("SessionData.Challenge is %v, want string", ch.Type)
	}
	allow, ok := st.FieldByName("AllowedCredentialIDs")
	if !ok || allow.Type.String() != "[][]uint8" {
		t.Fatalf("SessionData.AllowedCredentialIDs is %v, want [][]byte", allow.Type)
	}
}

func TestCeremonyIsSingleUseAndExpires(t *testing.T) {
	p, repo, clk := newPasskeys(t)
	ctx := context.Background()
	user := seedAuthUser(t, repo)

	ceremonyID, options, err := p.BeginRegistration(ctx, user)
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	var creation map[string]any
	if err := json.Unmarshal(options, &creation); err != nil {
		t.Fatalf("options are not JSON: %v", err)
	}
	// The ceremony row holds the session server-side; the client never sees it.
	row, err := repo.TakeCeremony(ctx, ceremonyID, clk.Now().Unix())
	if err != nil {
		t.Fatalf("TakeCeremony: %v", err)
	}
	// SessionJSON, not SessionJson: the sqlc spelling belongs to
	// sqlitedb.WebauthnCeremonies and appears only inside the adapter's
	// conversion; store.CeremonyRow declares SessionJSON.
	if !strings.Contains(row.SessionJSON, "challenge") {
		t.Fatalf("the ceremony row does not hold SessionData as JSON: %q", row.SessionJSON)
	}
	if _, err := repo.TakeCeremony(ctx, ceremonyID, clk.Now().Unix()); err == nil {
		t.Fatal("a ceremony was taken twice")
	}

	ceremonyID, _, err = p.BeginRegistration(ctx, user)
	if err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	clk.Advance(2 * time.Hour)
	if _, err := repo.TakeCeremony(ctx, ceremonyID, clk.Now().Unix()); err == nil {
		t.Fatal("an expired ceremony was still takeable")
	}
}

func TestUserHandleIsSixtyFourRandomBytesAndNotTheUserID(t *testing.T) {
	p, repo, _ := newPasskeys(t)
	ctx := context.Background()
	user := seedAuthUser(t, repo)
	if _, _, err := p.BeginRegistration(ctx, user); err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	handle := handleFor(t, repo, user, passkeyRPID)
	if len(handle) != 64 {
		t.Fatalf("user handle is %d bytes, want 64", len(handle))
	}
	if strings.HasPrefix(string(handle), string(user[:])) {
		t.Fatal("the user handle is derived from the user id; it must be independent random bytes")
	}
	back, err := repo.GetWebauthnUserByHandle(ctx, passkeyRPID, handle)
	if err != nil || back != user {
		t.Fatalf("GetWebauthnUserByHandle = %s, %v", back, err)
	}
}

// The handle is minted once and never rotated. The authenticator keeps the
// handle it saw at registration, so a handle that changes on the next ceremony
// makes GetWebauthnUserByHandle miss and breaks discoverable login for that
// account for good — and a single-ceremony test cannot see it.
func TestTheUserHandleIsStableAcrossCeremonies(t *testing.T) {
	p, repo, _ := newPasskeys(t)
	ctx := context.Background()
	user := seedAuthUser(t, repo)
	if _, _, err := p.BeginRegistration(ctx, user); err != nil {
		t.Fatalf("BeginRegistration: %v", err)
	}
	first := handleFor(t, repo, user, passkeyRPID)
	for i := 0; i < 3; i++ {
		if _, _, err := p.BeginRegistration(ctx, user); err != nil {
			t.Fatalf("BeginRegistration %d: %v", i, err)
		}
		if got := handleFor(t, repo, user, passkeyRPID); !bytes.Equal(got, first) {
			t.Fatalf("the user handle rotated on ceremony %d: %x -> %x", i+2, first, got)
		}
	}
	if _, _, err := p.BeginLogin(ctx); err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	if got := handleFor(t, repo, user, passkeyRPID); !bytes.Equal(got, first) {
		t.Fatalf("the user handle rotated during login: %x -> %x", first, got)
	}
}

// Flags, transports, attestation format and extensions are stored, so they must
// come back: CredentialFlags.BackupEligible is the one the library documents as
// "This should NEVER change", and it cannot be enforced against a record whose
// flags are always zero.
func TestCredentialFlagsAndTransportsSurviveTheRoundTrip(t *testing.T) {
	p, repo, _ := newPasskeys(t)
	ctx := context.Background()
	user := seedAuthUser(t, repo)
	cred := store.WebauthnCredentialRow{
		CredID: []byte("cred-c"), RPID: passkeyRPID, UserID: user, PublicKey: []byte{1},
		AttestationType: "none", AttestationFormat: "packed", Transports: "usb,nfc",
		// {"rk":true}, not {"credProps":{"rk":true}}: at v0.18.2
		// Credential.Extensions is the typed CredentialExtensions struct
		// (webauthn/credential.go:146,151-186) whose discoverability field is
		// the top-level `rk`, not the raw credProps client output. A fixture in
		// the client's spelling would decode to a zero struct WITHOUT an error
		// and this test would pass against a read path that dropped everything.
		Flags: []byte{0x0F}, ExtensionsJSON: `{"rk":true}`, Created: 1,
	}
	if err := repo.PutWebauthnCredential(ctx, cred); err != nil {
		t.Fatalf("PutWebauthnCredential: %v", err)
	}
	loaded, err := p.CredentialsForTest(ctx, user)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("%d credentials loaded, want 1", len(loaded))
	}
	c := loaded[0]
	if !c.Flags.BackupEligible || !c.Flags.BackupState || !c.Flags.UserPresent || !c.Flags.UserVerified {
		t.Fatalf("flags lost in the round trip: %+v", c.Flags)
	}
	if c.AttestationFormat != "packed" {
		t.Fatalf("attestation format = %q", c.AttestationFormat)
	}
	if len(c.Transport) != 2 || string(c.Transport[0]) != "usb" || string(c.Transport[1]) != "nfc" {
		t.Fatalf("transports = %v", c.Transport)
	}
	// CredentialExtensions is a struct, not a map, so there is no len() to take:
	// RK is the field the stored JSON carries and the one a dropped column
	// leaves nil.
	if c.Extensions.RK == nil || !*c.Extensions.RK {
		t.Fatalf("extensions were dropped: %+v", c.Extensions)
	}
}

func TestACredentialForAnotherRPIDIsRefused(t *testing.T) {
	_, repo, _ := newPasskeys(t)
	ctx := context.Background()
	user := seedAuthUser(t, repo)
	cred := store.WebauthnCredentialRow{
		CredID: []byte("cred-a"), RPID: "other.example", UserID: user,
		PublicKey: []byte{1}, Flags: []byte{0}, Created: 1,
	}
	if err := repo.PutWebauthnCredential(ctx, cred); err != nil {
		t.Fatalf("PutWebauthnCredential: %v", err)
	}
	list, err := repo.ListWebauthnCredentials(ctx, user, passkeyRPID)
	if err != nil {
		t.Fatalf("ListWebauthnCredentials: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("a credential registered for other.example was listed for chat.example")
	}
}

func TestSignCountAndFlagsAreWrittenBack(t *testing.T) {
	_, repo, _ := newPasskeys(t)
	ctx := context.Background()
	user := seedAuthUser(t, repo)
	cred := store.WebauthnCredentialRow{CredID: []byte("cred-b"), RPID: passkeyRPID,
		UserID: user, PublicKey: []byte{1}, SignCount: 3, Flags: []byte{0x01}, Created: 1}
	if err := repo.PutWebauthnCredential(ctx, cred); err != nil {
		t.Fatalf("PutWebauthnCredential: %v", err)
	}
	if err := repo.UpdateWebauthnCredential(ctx, cred.CredID, 9, []byte{0x11}, 100); err != nil {
		t.Fatalf("UpdateWebauthnCredential: %v", err)
	}
	got, err := repo.GetWebauthnCredential(ctx, cred.CredID)
	if err != nil {
		t.Fatalf("GetWebauthnCredential: %v", err)
	}
	if got.SignCount != 9 || got.Flags[0] != 0x11 || got.LastUsed == nil || *got.LastUsed != 100 {
		t.Fatalf("write-back lost data: %+v", got)
	}
}

func TestTheMessagePackDecodersAreNeverReached(t *testing.T) {
	// The generated UnmarshalMsg/DecodeMsg size their allocations from the
	// wire's length prefixes before reading the payload, so a handful of
	// malformed bytes can make the process allocate gigabytes. dillad stores
	// SessionData as JSON and must never call them.
	body, err := os.ReadFile(filepath.Join("passkey.go"))
	if err != nil {
		t.Fatalf("read passkey.go: %v", err)
	}
	for _, banned := range []string{"UnmarshalMsg", "DecodeMsg", "MarshalMsg", "EncodeMsg"} {
		if strings.Contains(string(body), banned) {
			t.Fatalf("passkey.go calls %s", banned)
		}
	}
	if !strings.Contains(string(body), "json.Marshal") {
		t.Fatal("passkey.go does not serialise SessionData as JSON")
	}
}

func TestConfigFromDillaRefusesNoOrigins(t *testing.T) {
	c := config.Default().Auth.WebAuthn
	c.RPID = passkeyRPID
	c.RPOrigins = nil
	if _, err := webauthn.New(auth.ConfigFromDilla(c)); err == nil {
		t.Fatal("go-webauthn accepted a config with no RPOrigins; config.Validate must have caught this first")
	}
}

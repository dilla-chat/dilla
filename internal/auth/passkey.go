package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// ceremonyTTL is how long a Begin* result is good for. Short, because the whole
// ceremony is one user gesture.
const ceremonyTTL = 5 * 60 // seconds

// discoverableTTL is an unauthenticated (discoverable) login ceremony's life (wave B task 3 security review): as long as
// the browser's get() is allowed to run (LoginTimeout, sent in the options as `timeout`) and no longer, so rows a
// distributed attacker opens leave the table sooner. Step-ups and registrations keep ceremonyTTL.
const discoverableTTL = 120 // seconds

// LoginTimeout is the WebAuthn login timeout the options carry (go-webauthn's default is 5 minutes).
const LoginTimeout = time.Duration(discoverableTTL) * time.Second

// The default caps on open ceremonies (security review I1/B): discoverable logins instance-wide, and a user's own
// (registrations and step-ups). Past either, the oldest of that scope is evicted: a fresh begin always works.
const (
	DefaultMaxOpenLogins  = 10000
	DefaultMaxOpenPerUser = 5
)

// webauthn_ceremonies.kind is CHECK (kind IN (0, 1)) (schema/003_auth.sql), so a step-up has no
// kind of its own: it is a ceremonyLogin row WITH user_id set, where a discoverable login's is
// NULL. FinishLogin refuses a row whose user is set and FinishStepUp one whose user is not the
// caller, so neither ceremony can be spent as the other.
const (
	ceremonyRegister uint8 = 0
	ceremonyLogin    uint8 = 1
)

// ErrPasskeyUnknown is FinishLogin's answer for an assertion whose credential id, or whose user
// handle, is not registered for this instance's rp_id: a passkey the instance does not hold. The
// route answers it 401 E_PASSKEY_UNKNOWN, distinct from every other failure, so the client can
// say "this passkey is not registered here" without reading a detail.
var ErrPasskeyUnknown = errors.New("auth: the passkey is not registered on this instance")

// ErrNoPasskey is BeginStepUp's answer for an account that holds no passkey for this rp_id.
var ErrNoPasskey = errors.New("auth: the account holds no passkey")

// ErrNotYourCeremony is FinishStepUp's answer for a ceremony that is not a step-up of the caller:
// a discoverable login's, or another account's step-up.
var ErrNotYourCeremony = errors.New("auth: the ceremony is not a step-up of this account")

// ConfigFromDilla builds go-webauthn's config from dilla.toml.
//
// EncodeUserIDAsString stays false: its own doc warns that a user.id which is
// not base64url makes PublicKeyCredential.parseCreationOptionsFromJSON throw,
// and that a coincidentally-valid one decodes to DIFFERENT bytes with nothing
// reporting it.
func ConfigFromDilla(c config.WebAuthn) *webauthn.Config {
	return &webauthn.Config{
		RPID:                 c.RPID,
		RPDisplayName:        c.RPDisplayName,
		RPOrigins:            c.RPOrigins,
		EncodeUserIDAsString: false,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.UserVerificationRequirement(c.UserVerification),
		},
		// The browser's get() may run as long as a discoverable login ceremony lives (discoverableTTL), no longer.
		Timeouts: webauthn.TimeoutsConfig{Login: webauthn.TimeoutConfig{Timeout: LoginTimeout, TimeoutUVD: LoginTimeout}},
	}
}

// Passkeys runs the four WebAuthn ceremonies with the session stored
// server-side, keyed by an opaque ceremony id.
type Passkeys struct {
	wa   *webauthn.WebAuthn
	repo store.Repository
	clk  clock.Clock
	rpID string
	// The caps PutCeremonyCapped enforces: discoverable logins instance-wide, and each user's own ceremonies.
	maxOpenLogins  int
	maxOpenPerUser int
}

func NewPasskeys(cfg *webauthn.Config, repo store.Repository, clk clock.Clock) (*Passkeys, error) {
	wa, err := webauthn.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("auth: webauthn config: %w", err)
	}
	return &Passkeys{wa: wa, repo: repo, clk: clk, rpID: cfg.RPID,
		maxOpenLogins: DefaultMaxOpenLogins, maxOpenPerUser: DefaultMaxOpenPerUser}, nil
}

// SetLimits sets the caps on open ceremonies (auth.webauthn.max_open_logins and max_open_per_user). A value below 1
// keeps the default.
func (p *Passkeys) SetLimits(maxOpenLogins, maxOpenPerUser int) {
	if maxOpenLogins > 0 {
		p.maxOpenLogins = maxOpenLogins
	}
	if maxOpenPerUser > 0 {
		p.maxOpenPerUser = maxOpenPerUser
	}
}

// dillaUser is go-webauthn's User over dilla's rows. There are exactly four
// methods: the rendered docs list a fifth, WebAuthnIcon, which the source at
// v0.18.2 does not have.
type dillaUser struct {
	handle  []byte
	name    string
	display string
	creds   []webauthn.Credential
}

func (u dillaUser) WebAuthnID() []byte                         { return u.handle }
func (u dillaUser) WebAuthnName() string                       { return u.name }
func (u dillaUser) WebAuthnDisplayName() string                { return u.display }
func (u dillaUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

var _ webauthn.User = dillaUser{}

// user loads the WebAuthn view of a dilla account, minting the 64-byte handle
// on first use. The handle is fresh random bytes, NOT the user id: the id is
// 16 bytes and is used elsewhere, and the spec's own advice is that the handle
// "uses the entire 64 bytes".
func (p *Passkeys) user(ctx context.Context, userID id.ID) (dillaUser, error) {
	row, err := p.repo.GetUser(ctx, userID)
	if err != nil {
		return dillaUser{}, err
	}
	handle, err := p.handle(ctx, userID)
	if err != nil {
		return dillaUser{}, err
	}
	creds, err := p.credentials(ctx, userID)
	if err != nil {
		return dillaUser{}, err
	}
	return dillaUser{handle: handle, name: row.Username, display: row.Display, creds: creds}, nil
}

// handle reads the account's WebAuthn user handle and mints one only when none
// exists. It MUST be idempotent: the authenticator stores the handle it saw at
// registration, and user() is called again by FinishRegistration and by
// FinishLogin's DiscoverableUserHandler, so a handle minted on every call would
// rotate the row out from under the authenticator and make
// GetWebauthnUserByHandle miss for ever. PutWebauthnUser's SQL is
// ON CONFLICT (rp_id, user_id) DO NOTHING for the same reason, so two
// concurrent ceremonies converge on the first handle rather than the last.
func (p *Passkeys) handle(ctx context.Context, userID id.ID) ([]byte, error) {
	existing, err := p.repo.GetWebauthnUserHandle(ctx, p.rpID, userID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	fresh := make([]byte, 64)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	if err := p.repo.PutWebauthnUser(ctx, userID, p.rpID, fresh, p.clk.Now().Unix()); err != nil {
		return nil, err
	}
	// Re-read: a concurrent ceremony may have won the insert, and the handle the
	// authenticator will see is whichever one is in the row.
	return p.repo.GetWebauthnUserHandle(ctx, p.rpID, userID)
}

func (p *Passkeys) credentials(ctx context.Context, userID id.ID) ([]webauthn.Credential, error) {
	rows, err := p.repo.ListWebauthnCredentials(ctx, userID, p.rpID)
	if err != nil {
		return nil, err
	}
	out := make([]webauthn.Credential, 0, len(rows))
	for _, r := range rows {
		// Every stored column is round-tripped, not just the four the library
		// needs to verify a signature. CredentialFlags carries BackupEligible
		// ("This should NEVER change"), BackupState and the latched UserVerified
		// (webauthn@v0.18.2 webauthn/credential.go:446-465); validation cannot
		// enforce the BE-never-changes invariant against a record whose flags
		// are always zero. Extensions matter because
		// Config.ExtensionsUnsolicitedOutputPolicy defaults to reject.
		c := webauthn.Credential{
			ID:                r.CredID,
			PublicKey:         r.PublicKey,
			AttestationType:   r.AttestationType,
			AttestationFormat: r.AttestationFormat,
			Transport:         splitTransports(r.Transports),
			Flags:             flagsFromByte(r.Flags),
			Authenticator:     webauthn.Authenticator{SignCount: uint32(r.SignCount)}, //nolint:gosec // G115: the WebAuthn signature counter is a uint32 by the specification; the row stores it widened
		}
		if r.ExtensionsJSON != "" {
			if err := json.Unmarshal([]byte(r.ExtensionsJSON), &c.Extensions); err != nil {
				return nil, fmt.Errorf("auth: decode credential extensions: %w", err)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// splitTransports is joinTransports' inverse.
func splitTransports(s string) []protocol.AuthenticatorTransport {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]protocol.AuthenticatorTransport, 0, len(parts))
	for _, p := range parts {
		out = append(out, protocol.AuthenticatorTransport(p))
	}
	return out
}

// flagsFromByte is flagBytes' inverse.
func flagsFromByte(b []byte) webauthn.CredentialFlags {
	if len(b) == 0 {
		return webauthn.CredentialFlags{}
	}
	return webauthn.CredentialFlags{
		UserPresent:    b[0]&(1<<0) != 0,
		UserVerified:   b[0]&(1<<1) != 0,
		BackupEligible: b[0]&(1<<2) != 0,
		BackupState:    b[0]&(1<<3) != 0,
	}
}

// storeSession persists SessionData as JSON. It is never sent to the client and
// the MessagePack codecs are never used on it.
func (p *Passkeys) storeSession(ctx context.Context, kind uint8, userID *id.ID, s *webauthn.SessionData) (id.ID, error) {
	body, err := json.Marshal(s)
	if err != nil {
		return id.ID{}, fmt.Errorf("auth: encode session: %w", err)
	}
	now := p.clk.Now().Unix()
	ceremonyID := id.New()
	ttl, limit := int64(ceremonyTTL), p.maxOpenPerUser
	if userID == nil {
		ttl, limit = discoverableTTL, p.maxOpenLogins
	}
	row := store.CeremonyRow{
		ID: ceremonyID, Kind: kind, UserID: userID,
		SessionJSON: string(body), Created: now, Expires: now + ttl,
	}
	// The write and the eviction of the oldest beyond the cap are one transaction (security review I1/B).
	if _, err := p.repo.PutCeremonyCapped(ctx, row, limit, now); err != nil {
		return id.ID{}, err
	}
	return ceremonyID, nil
}

func (p *Passkeys) takeSession(ctx context.Context, ceremonyID id.ID, want uint8) (webauthn.SessionData, *id.ID, error) {
	row, err := p.repo.TakeCeremony(ctx, ceremonyID, p.clk.Now().Unix())
	if err != nil {
		return webauthn.SessionData{}, nil, err
	}
	if row.Kind != want {
		return webauthn.SessionData{}, nil, errors.New("auth: ceremony kind mismatch")
	}
	var s webauthn.SessionData
	if err := json.Unmarshal([]byte(row.SessionJSON), &s); err != nil {
		return webauthn.SessionData{}, nil, fmt.Errorf("auth: decode session: %w", err)
	}
	return s, row.UserID, nil
}

func (p *Passkeys) BeginRegistration(ctx context.Context, userID id.ID) (id.ID, []byte, error) {
	u, err := p.user(ctx, userID)
	if err != nil {
		return id.ID{}, nil, err
	}
	// The account's own credentials are excluded, so an authenticator that already holds one
	// answers InvalidStateError in the browser instead of minting a second credential for it.
	creation, session, err := p.wa.BeginRegistration(u,
		webauthn.WithExclusions(webauthn.Credentials(u.creds).CredentialDescriptors()))
	if err != nil {
		return id.ID{}, nil, fmt.Errorf("auth: begin registration: %w", err)
	}
	ceremonyID, err := p.storeSession(ctx, ceremonyRegister, &userID, session)
	if err != nil {
		return id.ID{}, nil, err
	}
	options, err := json.Marshal(creation)
	if err != nil {
		return id.ID{}, nil, err
	}
	return ceremonyID, options, nil
}

// FinishRegistration takes the ceremony and verifies the response, and returns the credential
// row it would store, named `name`, WITHOUT storing it: the route writes the row in the same
// transaction that re-checks the step-up rule under the user lock and clears factor_owed, so a
// credential is never on the account without the rest of that write. The row's user is the one
// the ceremony was begun for.
func (p *Passkeys) FinishRegistration(ctx context.Context, ceremonyID id.ID, response []byte, name string) (store.WebauthnCredentialRow, error) {
	session, userID, err := p.takeSession(ctx, ceremonyID, ceremonyRegister)
	if err != nil {
		return store.WebauthnCredentialRow{}, err
	}
	if userID == nil {
		return store.WebauthnCredentialRow{}, errors.New("auth: registration ceremony without a user")
	}
	u, err := p.user(ctx, *userID)
	if err != nil {
		return store.WebauthnCredentialRow{}, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(strings.NewReader(string(response)))
	if err != nil {
		return store.WebauthnCredentialRow{}, fmt.Errorf("auth: parse registration response: %w", err)
	}
	cred, err := p.wa.CreateCredential(u, session, parsed)
	if err != nil {
		return store.WebauthnCredentialRow{}, fmt.Errorf("auth: create credential: %w", err)
	}
	extensions, err := marshalExtensions(cred.Extensions)
	if err != nil {
		return store.WebauthnCredentialRow{}, err
	}
	return store.WebauthnCredentialRow{
		CredID: cred.ID, RPID: p.rpID, UserID: *userID, PublicKey: cred.PublicKey,
		SignCount: int64(cred.Authenticator.SignCount), AttestationType: cred.AttestationType,
		AttestationFormat: cred.AttestationFormat, Transports: joinTransports(cred.Transport),
		Flags: flagBytes(cred.Flags), ExtensionsJSON: extensions, Name: name, Created: p.clk.Now().Unix(),
	}, nil
}

// marshalExtensions is credentials()' inverse: the curated CredentialExtensions
// struct as JSON, or "" when the authenticator reported nothing worth keeping.
// Writing "{}" instead would make every row carry an object the read path then
// has to decode for no result.
func marshalExtensions(e webauthn.CredentialExtensions) (string, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("auth: encode credential extensions: %w", err)
	}
	if string(body) == "{}" {
		return "", nil
	}
	return string(body), nil
}

func (p *Passkeys) BeginLogin(ctx context.Context) (id.ID, []byte, error) {
	assertion, session, err := p.wa.BeginDiscoverableLogin()
	if err != nil {
		return id.ID{}, nil, fmt.Errorf("auth: begin discoverable login: %w", err)
	}
	ceremonyID, err := p.storeSession(ctx, ceremonyLogin, nil, session)
	if err != nil {
		return id.ID{}, nil, err
	}
	options, err := json.Marshal(assertion)
	if err != nil {
		return id.ID{}, nil, err
	}
	return ceremonyID, options, nil
}

// ErrCeremonyGone is FinishLogin's answer when the ceremony row is absent: expired, evicted at the cap, already spent
// or never issued. The route refuses it like any failed login but does not count it against the address (wave B task 3
// security re-check): a person who picked a passkey after the ceremony's 120 s guessed nothing.
var ErrCeremonyGone = errors.New("auth: the ceremony is expired, evicted or unknown")

// FinishLogin uses ValidatePasskeyLogin, which hands back the resolved user as
// well as the credential, so dillad never has to ask for a handle first. The
// plain discoverable variant returns only the credential.
//
// uv is THIS assertion's authenticator-data UV flag, read from the parsed response. The
// credential ValidatePasskeyLogin returns is no source for it: at go-webauthn v0.18.2 its
// Flags.UserVerified is the STORED credential's, latched (CredentialFlags.Update keeps the
// stored UV and drops the assertion's; login.go:416-422), so a credential registered with UV
// would report UV on every later presence-only assertion.
//
// A ceremony with a user set is a step-up (BeginStepUp), never a login, and is refused. An
// assertion naming a credential id or a user handle this rp_id does not hold is ErrPasskeyUnknown. An absent
// ceremony row is ErrCeremonyGone.
func (p *Passkeys) FinishLogin(ctx context.Context, ceremonyID id.ID, response []byte) (id.ID, bool, error) {
	session, owner, err := p.takeSession(ctx, ceremonyID, ceremonyLogin)
	if errors.Is(err, store.ErrNotFound) {
		return id.ID{}, false, ErrCeremonyGone
	}
	if err != nil {
		return id.ID{}, false, err
	}
	if owner != nil {
		return id.ID{}, false, errors.New("auth: a step-up ceremony is not a login")
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(strings.NewReader(string(response)))
	if err != nil {
		return id.ID{}, false, fmt.Errorf("auth: parse assertion: %w", err)
	}
	if err := p.known(ctx, parsed); err != nil {
		return id.ID{}, false, err
	}
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		userID, err := p.repo.GetWebauthnUserByHandle(ctx, p.rpID, userHandle)
		if err != nil {
			return nil, err
		}
		u, err := p.user(ctx, userID)
		if err != nil {
			return nil, err
		}
		return u, nil
	}
	_, cred, err := p.wa.ValidatePasskeyLogin(handler, session, parsed)
	if err != nil {
		return id.ID{}, false, fmt.Errorf("auth: validate passkey login: %w", err)
	}
	row, err := p.repo.GetWebauthnCredential(ctx, cred.ID)
	if err != nil {
		return id.ID{}, false, err
	}
	if row.RPID != p.rpID {
		return id.ID{}, false, errors.New("auth: credential belongs to another relying party")
	}
	if err := p.repo.UpdateWebauthnCredential(ctx, cred.ID, int64(cred.Authenticator.SignCount),
		flagBytes(cred.Flags), p.clk.Now().Unix()); err != nil {
		return id.ID{}, false, err
	}
	return row.UserID, parsed.Response.AuthenticatorData.Flags.HasUserVerified(), nil
}

// known refuses, with ErrPasskeyUnknown, an assertion whose credential id or user handle is not
// registered for this rp_id. It runs after the ceremony is taken, so a probe costs a ceremony
// like any other failure.
func (p *Passkeys) known(ctx context.Context, parsed *protocol.ParsedCredentialAssertionData) error {
	row, err := p.repo.GetWebauthnCredential(ctx, parsed.RawID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrPasskeyUnknown
	case err != nil:
		return err
	case row.RPID != p.rpID:
		return ErrPasskeyUnknown
	}
	if _, err := p.repo.GetWebauthnUserByHandle(ctx, p.rpID, parsed.Response.UserHandle); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrPasskeyUnknown
		}
		return err
	}
	return nil
}

// BeginStepUp starts a passkey step-up for an account already signed in: an assertion over this
// account's credentials only, user verification required. Its ceremony row is kind 1 with the
// user set (see ceremonyLogin).
func (p *Passkeys) BeginStepUp(ctx context.Context, userID id.ID) (id.ID, []byte, error) {
	u, err := p.user(ctx, userID)
	if err != nil {
		return id.ID{}, nil, err
	}
	if len(u.creds) == 0 {
		return id.ID{}, nil, ErrNoPasskey
	}
	assertion, session, err := p.wa.BeginLogin(u, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return id.ID{}, nil, fmt.Errorf("auth: begin step-up: %w", err)
	}
	ceremonyID, err := p.storeSession(ctx, ceremonyLogin, &userID, session)
	if err != nil {
		return id.ID{}, nil, err
	}
	options, err := json.Marshal(assertion)
	if err != nil {
		return id.ID{}, nil, err
	}
	return ceremonyID, options, nil
}

// FinishStepUp verifies a step-up assertion for caller. The ceremony must be a step-up begun for
// caller (ErrNotYourCeremony otherwise: a discoverable login's ceremony, or another account's),
// the assertion must verify against caller's credentials for this rp_id, and THIS assertion must
// carry UV.
func (p *Passkeys) FinishStepUp(ctx context.Context, ceremonyID id.ID, response []byte, caller id.ID) error {
	session, owner, err := p.takeSession(ctx, ceremonyID, ceremonyLogin)
	if err != nil {
		return err
	}
	if owner == nil || *owner != caller {
		return ErrNotYourCeremony
	}
	u, err := p.user(ctx, caller)
	if err != nil {
		return err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(strings.NewReader(string(response)))
	if err != nil {
		return fmt.Errorf("auth: parse step-up assertion: %w", err)
	}
	cred, err := p.wa.ValidateLogin(u, session, parsed)
	if err != nil {
		return fmt.Errorf("auth: validate step-up: %w", err)
	}
	if !parsed.Response.AuthenticatorData.Flags.HasUserVerified() {
		return errors.New("auth: the step-up assertion did not verify the user")
	}
	return p.repo.UpdateWebauthnCredential(ctx, cred.ID, int64(cred.Authenticator.SignCount),
		flagBytes(cred.Flags), p.clk.Now().Unix())
}

// PruneCeremonies drops expired rows. It is also what the upgrade path calls:
// SessionData.Extensions changes shape across go-webauthn releases, so
// in-flight ceremonies must be dropped on a version bump rather than carried.
func (p *Passkeys) PruneCeremonies(ctx context.Context) (int64, error) {
	return p.repo.PruneCeremonies(ctx, p.clk.Now().Unix())
}

// OpenLogins counts the unexpired discoverable login ceremonies (the instance-wide cap's scope).
func (p *Passkeys) OpenLogins(ctx context.Context) (int64, error) {
	return p.repo.CountOpenCeremonies(ctx, nil, p.clk.Now().Unix())
}

// CredentialsForTest exposes the stored-to-library conversion so a test can
// assert the round trip. It reads nothing the ceremony paths do not already
// read, and it is the only exported seam in this file.
func (p *Passkeys) CredentialsForTest(ctx context.Context, userID id.ID) ([]webauthn.Credential, error) {
	return p.credentials(ctx, userID)
}

func joinTransports(ts []protocol.AuthenticatorTransport) string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, string(t))
	}
	return strings.Join(out, ",")
}

func flagBytes(f webauthn.CredentialFlags) []byte {
	var b byte
	if f.UserPresent {
		b |= 1 << 0
	}
	if f.UserVerified {
		b |= 1 << 1
	}
	if f.BackupEligible {
		b |= 1 << 2
	}
	if f.BackupState {
		b |= 1 << 3
	}
	return []byte{b}
}

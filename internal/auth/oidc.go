package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
)

// OIDC is the optional host-login path. Discovery is LAZY: oidc.NewProvider
// fetches the issuer's well-known document, and doing that at start-up means a
// down IdP keeps dillad from serving anything at all — including the endpoints
// that would let an operator turn OIDC off.
type OIDC struct {
	cfg    config.OIDC
	secret string
	clk    clock.Clock

	discMu   sync.Mutex
	provider *oidc.Provider // nil until a discovery SUCCEEDS; a failure is never cached

	mu      sync.Mutex
	pending map[string]pendingLogin
}

// Pending is the half of a login the start leg keeps and the callback leg
// spends: the nonce the id_token must carry, and the PKCE verifier the token
// endpoint must be given. The `state` is not a field because state IS the key
// these are filed under, and the browser carries it in the state cookie.
type Pending struct{ Nonce, Verifier, DesktopRequest string }

type pendingLogin struct {
	p       Pending
	expires time.Time
}

// Stash files one in-flight login under its state. The map lives here rather
// than in internal/api because api.Deps is copied by value per route and is
// declared to gain exactly one field for OIDC; a per-process map behind a
// pointer is the only place both routes can reach the same rows.
func (o *OIDC) Stash(state string, p Pending, ttl time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pending == nil {
		o.pending = map[string]pendingLogin{}
	}
	o.sweepLocked()
	o.pending[state] = pendingLogin{p: p, expires: o.clk.Now().Add(ttl)}
}

// Spend removes and returns the login filed under state. It deletes BEFORE it
// checks the expiry, exactly as an enrolment assertion does, so a replay of an
// expired login cannot be told from a replay of a live one and neither can be
// retried — and so the callback handler can delete the row before it spends the
// authorization code.
func (o *OIDC) Spend(state string) (Pending, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	row, ok := o.pending[state]
	if !ok {
		return Pending{}, false
	}
	delete(o.pending, state)
	if o.clk.Now().After(row.expires) {
		return Pending{}, false
	}
	return row.p, true
}

// PendingLenForTest reports how many in-flight logins are filed. It exists for
// the same reason Passkeys.CredentialsForTest does: the table is unexported
// state, and "the row was DELETED, not merely refused" and "the sweep dropped
// the abandoned login" are the two properties a test cannot otherwise see —
// both Spend paths answer false either way.
func (o *OIDC) PendingLenForTest() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pending)
}

func (o *OIDC) sweepLocked() {
	now := o.clk.Now()
	for k, v := range o.pending {
		if now.After(v.expires) {
			delete(o.pending, k)
		}
	}
}

func NewOIDC(c config.OIDC, clientSecret string, clk clock.Clock) *OIDC {
	return &OIDC{cfg: c, secret: clientSecret, clk: clk}
}

// discoveryTimeout bounds one discovery attempt, and every JWKS fetch the provider makes later.
const discoveryTimeout = 30 * time.Second

// discover fetches the issuer's discovery document once it can, and keeps only a SUCCESS: a
// sync.Once cached the first answer for the life of the process, so one IdP outage, or one login
// whose request was cancelled mid-discovery, disabled OIDC until a restart. The provider keeps the
// context it was built with for its later JWKS fetches, so it is built on one that no request's
// cancellation reaches, with a client timeout in place of the request's deadline.
func (o *OIDC) discover(ctx context.Context) (*oidc.Provider, error) {
	o.discMu.Lock()
	defer o.discMu.Unlock()
	if o.provider != nil {
		return o.provider, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("auth: oidc discovery for %s: %w", o.cfg.Issuer, err)
	}
	base := oidc.ClientContext(context.WithoutCancel(ctx), &http.Client{Timeout: discoveryTimeout})
	p, err := oidc.NewProvider(base, o.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: oidc discovery for %s: %w", o.cfg.Issuer, err)
	}
	o.provider = p
	return p, nil
}

func (o *OIDC) oauthConfig(p *oidc.Provider) oauth2.Config {
	return oauth2.Config{
		ClientID: o.cfg.ClientID, ClientSecret: o.secret, RedirectURL: o.cfg.RedirectURL,
		Endpoint: p.Endpoint(), Scopes: o.cfg.Scopes,
	}
}

// NewVerifierAndState mints the three one-time values a login needs.
func NewVerifierAndState() (string, string, string) {
	return oauth2.GenerateVerifier(), oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
}

// AuthURL builds the authorization URL with PKCE S256 and the nonce.
func (o *OIDC) AuthURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	p, err := o.discover(ctx)
	if err != nil {
		return "", err
	}
	cfg := o.oauthConfig(p)
	return cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), nil
}

// AuthURLOffline builds the same URL from the configured authorization endpoint
// without discovery, for tests and for an operator-supplied endpoint override.
func (o *OIDC) AuthURLOffline(state, nonce, verifier string) (string, error) {
	if o.cfg.Issuer == "" {
		return "", errors.New("auth: oidc issuer is empty")
	}
	cfg := oauth2.Config{
		ClientID: o.cfg.ClientID, ClientSecret: o.secret, RedirectURL: o.cfg.RedirectURL,
		Endpoint: oauth2.Endpoint{AuthURL: o.cfg.Issuer + "/authorize", TokenURL: o.cfg.Issuer + "/token"},
		Scopes:   o.cfg.Scopes,
	}
	return cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), nil
}

// Exchange spends the code and the verifier and verifies the id_token. Three
// checks are the caller's, not the library's: state (done by the handler),
// nonce (below) and the single use of the verifier (the handler deletes the
// state row before calling this).
func (o *OIDC) Exchange(ctx context.Context, code, verifier, nonce string) (string, string, string, error) {
	p, err := o.discover(ctx)
	if err != nil {
		return "", "", "", err
	}
	cfg := o.oauthConfig(p)
	tok, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return "", "", "", fmt.Errorf("auth: oidc exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return "", "", "", errors.New("auth: oidc response carried no id_token")
	}
	idToken, err := p.Verifier(&oidc.Config{ClientID: o.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return "", "", "", fmt.Errorf("auth: verify id_token: %w", err)
	}
	if idToken.Nonce != nonce {
		return "", "", "", errors.New("auth: id_token nonce mismatch")
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "", "", "", fmt.Errorf("auth: id_token claims: %w", err)
	}
	return idToken.Issuer, idToken.Subject, claims.Email, nil
}

// StateCookie is the one cookie dillad sets, and it exists only between the
// redirect and the callback.
//
// The prefix is __Secure-, not __Host-. RFC 6265bis §4.1.3.2 requires a
// __Host--prefixed cookie to have Path=/ and no Domain, and a browser REJECTS
// the Set-Cookie outright otherwise — so __Host- together with the narrow path
// below would be a cookie no client ever stores, and every OIDC login would
// fail at the callback with no state to compare. __Secure- keeps the part that
// matters here (the cookie is only ever sent over HTTPS) and leaves the path
// free, and the value is single-use and server-verified regardless.
// Deviation ID10.
func StateCookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/v1/auth/oidc/callback",
		MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}
}

// DesktopOIDCStateCookie uses the browser's default path so an instance hosted
// under a reverse-proxy prefix keeps the proof on its own OIDC directory. The
// callback matches it only to a pending desktop ceremony, separately from the
// legacy browser cookie.
func DesktopOIDCStateCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: "__Secure-dilla-desktop-oidc", Value: value, MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

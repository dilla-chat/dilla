package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/pressly/goose/v3"
	"golang.org/x/oauth2"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/jonasthim/dilla/internal/store/sqlite/migrations"
)

// newAuthRepo returns a repository over a migrated temporary database. It is
// the same shape newPasskeys builds its store half from, without the ceremony
// runner: the OIDC mapping test needs nothing but the two identity queries.
func newAuthRepo(t *testing.T) store.Repository {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oidc.db")
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
	return repo
}

// fakeIDP serves a discovery document, a JWKS and a token endpoint, so the whole
// flow is exercised without a network. The token endpoint switches on the `code`
// form value, so one server serves the good case and the three refusals.
func fakeIDP(t *testing.T) *httptest.Server {
	t.Helper()
	srv, _ := fakeIDPCounted(t)
	return srv
}

// idpHits counts what the provider was actually asked for. Laziness cannot be
// asserted by timing: an issuer that refuses the connection returns instantly,
// so an EAGER constructor also finishes inside any deadline a test can set.
// Counting the discovery request is the only assertion that tells the two
// apart.
type idpHits struct{ discovery, token atomic.Int64 }

// fakeIDPCounted is fakeIDP with those counters.
func fakeIDPCounted(t *testing.T) (*httptest.Server, *idpHits) {
	t.Helper()
	hits := &idpHits{}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sign := func(t *testing.T, issuer, audience, nonce string) string {
		t.Helper()
		now := time.Now()
		claims := map[string]any{
			"iss": issuer, "aud": audience, "sub": "sub-123", "email": "jonas@example",
			"nonce": nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		}
		body, err := json.Marshal(claims)
		if err != nil {
			t.Fatalf("marshal claims: %v", err)
		}
		signed, err := signer.Sign(body)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		raw, err := signed.CompactSerialize()
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		return raw
	}

	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		hits.discovery.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q,`+
			`"id_token_signing_alg_values_supported":["RS256"]}`,
			srv.URL, srv.URL+"/authorize", srv.URL+"/token", srv.URL+"/jwks")
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: key.Public(), KeyID: "test", Algorithm: "RS256", Use: "sig"},
		}}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(set); err != nil {
			t.Errorf("encode jwks: %v", err)
		}
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		hits.token.Add(1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		var idToken string
		switch r.Form.Get("code") {
		case "code-good":
			idToken = sign(t, srv.URL, "dilla", "nonce")
		case "code-wrong-aud":
			idToken = sign(t, srv.URL, "someone-else", "nonce")
		case "code-wrong-iss":
			idToken = sign(t, "https://evil.example", "dilla", "nonce")
		case "code-wrong-nonce":
			idToken = sign(t, srv.URL, "dilla", "not-the-nonce")
		case "code-private-jwk", "code-oversized-jwk":
			parts := strings.Split(sign(t, srv.URL, "dilla", "nonce"), ".")
			header := map[string]any{"alg": "RS256", "kid": "test", "typ": "JWT"}
			jwk := map[string]string{"kty": "RSA", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}
			if r.Form.Get("code") == "code-private-jwk" {
				prime := make([]byte, 2048)
				prime[0], prime[len(prime)-1] = 0x80, 1
				jwk["p"] = base64.RawURLEncoding.EncodeToString(prime)
			} else {
				modulus := make([]byte, 513) // 4097 bits: just beyond the safe parser bound.
				modulus[0] = 1
				jwk["n"] = base64.RawURLEncoding.EncodeToString(modulus)
			}
			header["jwk"] = jwk
			rawHeader, err := json.Marshal(header)
			if err != nil {
				t.Fatal(err)
			}
			input := base64.RawURLEncoding.EncodeToString(rawHeader) + "." + parts[1]
			digest := sha256.Sum256([]byte(input))
			// Sign directly: the fixed JOSE signer correctly refuses these unsafe headers too.
			sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			idToken = input + "." + base64.RawURLEncoding.EncodeToString(sig)
		default:
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"at","token_type":"Bearer","expires_in":3600,"id_token":%q}`, idToken)
	})
	return srv, hits
}

func TestPKCEIsAlwaysS256AndTheVerifierIsSingleUse(t *testing.T) {
	c := config.Default().Auth.OIDC
	c.Enabled = true
	c.Issuer = "https://idp.example/app"
	c.ClientID = "dilla"
	c.RedirectURL = "https://chat.example/v1/auth/oidc/callback"
	o := auth.NewOIDC(c, "secret", clock.System())
	state, nonce, verifier := auth.NewVerifierAndState()
	if len(verifier) < 43 {
		t.Fatalf("verifier is %d characters; oauth2.GenerateVerifier produces 32 octets of randomness", len(verifier))
	}
	raw, err := o.AuthURLOffline(state, nonce, verifier) // no discovery needed
	if err != nil {
		t.Fatalf("AuthURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge") == verifier {
		t.Fatal("the challenge is missing or is the verifier in the clear")
	}
	if q.Get("state") != state || q.Get("nonce") != nonce {
		t.Fatal("state or nonce is missing from the authorization URL")
	}
	if want := oauth2.S256ChallengeFromVerifier(verifier); q.Get("code_challenge") != want {
		t.Fatalf("challenge = %q, want %q", q.Get("code_challenge"), want)
	}
}

func TestStateCookieIsSecurePrefixedAndScopedToTheCallback(t *testing.T) {
	// __Secure-, not __Host-: RFC 6265bis §4.1.3.2 makes a browser REJECT a
	// __Host- cookie whose Path is not exactly "/", so __Host- plus a narrow
	// path is a cookie no client ever stores and an OIDC login that can never
	// complete. The narrow path is the property worth keeping (deviation ID10).
	c := auth.StateCookie("__Secure-dilla-oidc", "value", 300)
	if !strings.HasPrefix(c.Name, "__Secure-") {
		t.Fatalf("cookie name %q is not __Secure- prefixed", c.Name)
	}
	if strings.HasPrefix(c.Name, "__Host-") {
		t.Fatal("a __Host- cookie with a path other than / is rejected by every browser")
	}
	if !c.Secure {
		t.Fatal("the state cookie is not Secure; __Secure- requires it")
	}
	if c.Domain != "" {
		t.Fatal("the state cookie sets a Domain")
	}
	if c.Path != "/v1/auth/oidc/callback" {
		t.Fatalf("cookie path = %q; the state cookie is scoped to the callback route only", c.Path)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags: HttpOnly=%v SameSite=%v; the callback is a cross-site top-level navigation, so Lax is the tightest that works",
			c.HttpOnly, c.SameSite)
	}
}

func TestIssuerAndAudienceMismatchesAreRefused(t *testing.T) {
	idp := fakeIDP(t)
	defer idp.Close()
	c := config.Default().Auth.OIDC
	c.Enabled = true
	c.Issuer = idp.URL
	c.ClientID = "dilla"
	c.RedirectURL = "https://chat.example/v1/auth/oidc/callback"
	o := auth.NewOIDC(c, "secret", clock.System())
	ctx := context.Background()

	if _, _, _, err := o.Exchange(ctx, "code-wrong-aud", "verifier", "nonce"); err == nil {
		t.Fatal("an id_token for another audience was accepted")
	}
	if _, _, _, err := o.Exchange(ctx, "code-wrong-iss", "verifier", "nonce"); err == nil {
		t.Fatal("an id_token from another issuer was accepted")
	}
	if _, _, _, err := o.Exchange(ctx, "code-wrong-nonce", "verifier", "nonce"); err == nil {
		t.Fatal("an id_token with a different nonce was accepted; Verify does not check the nonce for you")
	}
	issuer, subject, email, err := o.Exchange(ctx, "code-good", "verifier", "nonce")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if issuer != idp.URL || subject != "sub-123" || email != "jonas@example" {
		t.Fatalf("claims = %q/%q/%q", issuer, subject, email)
	}
}

func TestOIDCRejectsUnsafeEmbeddedRSAKeys(t *testing.T) {
	idp := fakeIDP(t)
	c := config.Default().Auth.OIDC
	c.Enabled, c.Issuer, c.ClientID = true, idp.URL, "dilla"
	c.RedirectURL = "https://chat.example/v1/auth/oidc/callback"
	o := auth.NewOIDC(c, "secret", clock.System())
	for _, tc := range []struct{ code, reason string }{
		{"code-private-jwk", "private parameters"},
		{"code-oversized-jwk", "modulus too large"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			if _, _, _, err := o.Exchange(context.Background(), tc.code, "verifier", "nonce"); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("unsafe embedded RSA key must be refused by the parser (%s), got %v", tc.reason, err)
			}
		})
	}
	if _, subject, _, err := o.Exchange(context.Background(), "code-good", "verifier", "nonce"); err != nil || subject != "sub-123" {
		t.Fatalf("ordinary signed login after refusals: subject=%q, error=%v", subject, err)
	}
}

func TestMappingIsBySubjectSoAChangedEmailDoesNotRemap(t *testing.T) {
	repo := newAuthRepo(t)
	ctx := context.Background()
	user := seedAuthUser(t, repo)
	if err := repo.PutOIDCIdentity(ctx, "https://idp.example/app", "sub-123", user, 1); err != nil {
		t.Fatalf("PutOIDCIdentity: %v", err)
	}
	got, err := repo.GetOIDCIdentity(ctx, "https://idp.example/app", "sub-123")
	if err != nil || got != user {
		t.Fatalf("GetOIDCIdentity = %s, %v", got, err)
	}
	// A different subject is a different identity even with the same email.
	if _, err := repo.GetOIDCIdentity(ctx, "https://idp.example/app", "sub-456"); err == nil {
		t.Fatal("a different subject resolved to an existing account")
	}
}

func TestDiscoveryIsLazySoADownIdPDoesNotBlockStartUp(t *testing.T) {
	// The assertion is a COUNT, not a deadline. `https://127.0.0.1:1/...` refuses
	// the connection instantly, so an eager NewOIDC returns far inside any
	// timeout and a timing test passes against the very constructor it exists
	// to catch. What LAZY means is that NewOIDC asks the identity provider
	// nothing at all.
	idp, hits := fakeIDPCounted(t)
	c := config.Default().Auth.OIDC
	c.Enabled = true
	c.Issuer = idp.URL
	c.ClientID = "dilla"
	c.RedirectURL = "https://chat.example/v1/auth/oidc/callback"
	ctx := context.Background()

	o := auth.NewOIDC(c, "secret", clock.System())
	if n := hits.discovery.Load(); n != 0 {
		t.Fatalf("NewOIDC fetched the discovery document %d times; construction must touch no network", n)
	}
	if _, err := o.AuthURL(ctx, "state", "nonce", "verifier"); err != nil {
		t.Fatalf("AuthURL: %v", err)
	}
	if n := hits.discovery.Load(); n != 1 {
		t.Fatalf("discovery ran %d times on first use, want exactly 1", n)
	}
	// And once for the life of the provider, not once per call: the cached provider
	// is the other half of the same property.
	if _, _, _, err := o.Exchange(ctx, "code-good", "verifier", "nonce"); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if n := hits.discovery.Load(); n != 1 {
		t.Fatalf("discovery ran %d times across two calls, want exactly 1", n)
	}

	// The original shape of this test, kept: an issuer nothing answers on must
	// not hold the constructor either.
	down := c
	down.Issuer = "https://127.0.0.1:1/does-not-exist"
	done := make(chan struct{})
	go func() {
		auth.NewOIDC(down, "secret", clock.System())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("NewOIDC blocked on discovery; it must be lazy")
	}
}

// A FAILED discovery is not cached: the first login's request was cancelled mid-discovery, and the
// next login still discovers and completes. With a sync.Once the first failure disabled OIDC until
// a restart. The provider the second call builds must also outlive the request that built it: its
// JWKS fetch during Exchange runs after that request's context is gone.
func TestAFailedDiscoveryIsRetriedOnTheNextLogin(t *testing.T) {
	idp, hits := fakeIDPCounted(t)
	c := config.Default().Auth.OIDC
	c.Enabled = true
	c.Issuer = idp.URL
	c.ClientID = "dilla"
	c.RedirectURL = "https://chat.example/v1/auth/oidc/callback"
	o := auth.NewOIDC(c, "secret", clock.System())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := o.AuthURL(cancelled, "state", "nonce", "verifier"); err == nil {
		t.Fatal("a discovery under a cancelled request succeeded")
	}

	first, done := context.WithCancel(context.Background())
	if _, err := o.AuthURL(first, "state", "nonce", "verifier"); err != nil {
		t.Fatalf("the next login's discovery: %v — a failed discovery was cached", err)
	}
	done() // the request that discovered is over
	if _, _, _, err := o.Exchange(context.Background(), "code-good", "verifier", "nonce"); err != nil {
		t.Fatalf("Exchange after the discovering request ended: %v", err)
	}
	if n := hits.discovery.Load(); n != 1 {
		t.Fatalf("discovery ran %d times, want once: a success is cached", n)
	}
}

// The pending-login table is what the callback's anti-replay property rests on:
// the row is spent once, it is deleted BEFORE its expiry is examined, and an
// expired row cannot be told from one that was never there.
func TestAPendingLoginIsSpentOnceAndExpires(t *testing.T) {
	const ttl = 5 * time.Minute
	clk := clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	c := config.Default().Auth.OIDC
	c.Enabled = true
	c.Issuer = "https://idp.example/app"
	c.ClientID = "dilla"
	o := auth.NewOIDC(c, "secret", clk)

	o.Stash("state-a", auth.Pending{Nonce: "n-a", Verifier: "v-a"}, ttl)
	got, ok := o.Spend("state-a")
	if !ok || got.Nonce != "n-a" || got.Verifier != "v-a" {
		t.Fatalf("Spend(state-a) = %+v, %v; want the stashed nonce and verifier", got, ok)
	}
	if n := o.PendingLenForTest(); n != 0 {
		t.Fatalf("%d rows left after the login was spent, want 0", n)
	}
	// The replay. A callback that arrives twice must find nothing the second
	// time, which is what keeps a captured authorization code from being
	// exchanged again.
	if got, ok := o.Spend("state-a"); ok {
		t.Fatalf("a replayed state was spent a second time: %+v", got)
	}
	if _, ok := o.Spend("never-stashed"); ok {
		t.Fatal("a state that was never stashed was spent")
	}

	// The expiry, and the delete that precedes it. Winding the clock back is
	// not something a caller can do, but it is the only way to SEE that the
	// expired row was removed rather than merely refused: if Spend checked the
	// expiry before deleting, the row would still be filed and would come back
	// to life the moment the deadline moved.
	o.Stash("state-b", auth.Pending{Nonce: "n-b", Verifier: "v-b"}, ttl)
	clk.Advance(ttl + time.Second)
	if _, ok := o.Spend("state-b"); ok {
		t.Fatal("an expired pending login was spent")
	}
	if n := o.PendingLenForTest(); n != 0 {
		t.Fatalf("%d rows left after an expired login was refused, want 0", n)
	}
	clk.Advance(-(ttl + time.Second))
	if _, ok := o.Spend("state-b"); ok {
		t.Fatal("an expired pending login survived the Spend that refused it")
	}

	// And the sweep: a login nobody ever came back for is not kept for the life
	// of the process, it goes on the next Stash.
	o.Stash("state-c", auth.Pending{Nonce: "n-c", Verifier: "v-c"}, ttl)
	clk.Advance(ttl + time.Second)
	o.Stash("state-d", auth.Pending{Nonce: "n-d", Verifier: "v-d"}, ttl)
	if n := o.PendingLenForTest(); n != 1 {
		t.Fatalf("%d rows filed, want 1: the abandoned login was not swept", n)
	}
	if _, ok := o.Spend("state-c"); ok {
		t.Fatal("the swept login was still spendable")
	}
	if got, ok := o.Spend("state-d"); !ok || got.Nonce != "n-d" {
		t.Fatalf("the sweep took the live login too: %+v, %v", got, ok)
	}
}

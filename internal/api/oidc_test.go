package api_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/server"
)

// oidcStateCookieName is internal/api's own constant, repeated here because the
// browser is the only thing that ever sees it and a test that guessed a
// different name would be testing nothing.
const oidcStateCookieName = "__Secure-dilla-oidc"

// oidcIDP is a fake identity provider: discovery, JWKS and a token endpoint,
// signing its own id_tokens. The nonce is settable because the start leg mints
// it server-side — the test reads it back off the authorization redirect and
// tells the provider to echo it, exactly as a real provider echoes the nonce it
// was given. `token` counts the exchanges, which is how the replay test proves
// a second callback never reaches the token endpoint at all.
type oidcIDP struct {
	*httptest.Server
	mu      sync.Mutex
	nonce   string
	subject string
	token   atomic.Int64
}

func (i *oidcIDP) setNonce(n string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.nonce = n
}

func (i *oidcIDP) setSubject(s string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.subject = s
}

func (i *oidcIDP) claims() (nonce, subject string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.nonce, i.subject
}

func (i *oidcIDP) exchanges() int64 { return i.token.Load() }

func fakeOIDCProvider(t *testing.T) *oidcIDP {
	t.Helper()
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
	idp := &oidcIDP{subject: "sub-123"}
	mux := http.NewServeMux()
	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Close)

	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q,`+
			`"id_token_signing_alg_values_supported":["RS256"]}`,
			idp.URL, idp.URL+"/authorize", idp.URL+"/token", idp.URL+"/jwks")
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: key.Public(), KeyID: "test", Algorithm: "RS256", Use: "sig"},
		}}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(set); err != nil {
			t.Errorf("encode jwks: %v", err)
		}
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		idp.token.Add(1)
		if err := r.ParseForm(); err != nil {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		if r.Form.Get("code") != "code-good" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		nonce, subject := idp.claims()
		now := time.Now()
		body, err := json.Marshal(map[string]any{
			"iss": idp.URL, "aud": "dilla", "sub": subject, "email": "jonas@example",
			"nonce": nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		})
		if err != nil {
			t.Errorf("marshal claims: %v", err)
			return
		}
		signed, err := signer.Sign(body)
		if err != nil {
			t.Errorf("sign: %v", err)
			return
		}
		raw, err := signed.CompactSerialize()
		if err != nil {
			t.Errorf("serialize: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"at","token_type":"Bearer","expires_in":3600,"id_token":%q}`, raw)
	})
	return idp
}

// newOIDCAPI is newTestAPI with an identity provider wired in. It rebuilds the
// mux rather than reaching into the one newTestAPI made, for the reason
// newPasskeyAPI does: Deps is copied per route, so a field set afterwards would
// reach no handler.
func newOIDCAPI(t *testing.T, autoCreate bool) (http.Handler, api.Deps, *oidcIDP) {
	t.Helper()
	idp := fakeOIDCProvider(t)
	_, d := newTestAPIWithConfig(t, func(c *config.Config) {
		c.Auth.OIDC.Enabled = true
		c.Auth.OIDC.Issuer = idp.URL
		c.Auth.OIDC.ClientID = "dilla"
		c.Auth.OIDC.RedirectURL = "https://" + c.Instance.Domain + "/v1/auth/oidc/callback"
		c.Auth.OIDC.AutoCreate = autoCreate
		// Several requests per test on one address; the buckets are somebody
		// else's test's subject.
		c.Limits.Rate.LoginBurst = 100
		c.Limits.Rate.LoginFailedBurst = 100
	})
	d.OIDC = auth.NewOIDC(d.Config.Auth.OIDC, "secret", d.Clock)
	m := server.NewMux()
	api.Register(m, d)
	return m, d, idp
}

// getWithCookies drives a browser navigation: these two routes are the only
// ones in the tree that are not CBOR calls.
func getWithCookies(h http.Handler, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func setCookie(t *testing.T, res *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: res.Header()}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie in %v", name, res.Header().Values("Set-Cookie"))
	return nil
}

// startOIDC runs the start leg and returns the state cookie the browser would
// keep and the state and nonce the provider was given.
func startOIDC(t *testing.T, h http.Handler) (cookie *http.Cookie, state, nonce string) {
	t.Helper()
	res := getWithCookies(h, "/v1/auth/oidc/start")
	if res.Code != http.StatusFound {
		t.Fatalf("start answered %d, want 302; body %x", res.Code, res.Body.Bytes())
	}
	loc, err := url.Parse(res.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	q := loc.Query()
	return setCookie(t, res, oidcStateCookieName), q.Get("state"), q.Get("nonce")
}

func errorCode(t *testing.T, res *httptest.ResponseRecorder) string {
	t.Helper()
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if len(out) < 1 {
		t.Fatalf("error body has %d elements", len(out))
	}
	code, ok := out[0].(string)
	if !ok {
		t.Fatalf("error code is %T, not a string", out[0])
	}
	return code
}

// The start leg hands the browser one opaque value and keeps everything else.
// The cookie and the `state` query parameter are the SAME value, because that
// is what the callback compares, and neither the nonce nor the PKCE verifier
// may leave the server.
func TestTheOIDCStartLegSetsAStateCookieMatchingTheAuthorizationRequest(t *testing.T) {
	h, _, _ := newOIDCAPI(t, false)
	res := getWithCookies(h, "/v1/auth/oidc/start")
	if res.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body %x", res.Code, res.Body.Bytes())
	}
	loc, err := url.Parse(res.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" {
		t.Fatalf("authorization request is missing a challenge, a nonce or a state: %v", q)
	}
	c := setCookie(t, res, oidcStateCookieName)
	if c.Value != q.Get("state") {
		t.Fatalf("cookie %q and state %q differ; the callback compares them", c.Value, q.Get("state"))
	}
	if c.Path != "/v1/auth/oidc/callback" || !c.Secure || !c.HttpOnly ||
		c.SameSite != http.SameSiteLaxMode || c.Domain != "" {
		t.Fatalf("state cookie flags: %+v", c)
	}
	// The two secrets stay behind. A verifier in the cookie jar would make PKCE
	// ceremonial, and a nonce there would let a replayed id_token pass.
	jar := strings.Join(res.Header().Values("Set-Cookie"), " ")
	if strings.Contains(jar, q.Get("nonce")) || strings.Contains(jar, q.Get("code_challenge")) {
		t.Fatalf("the nonce or the challenge reached the browser: %s", jar)
	}
}

// The whole ceremony, and then the replay. The second callback carries exactly
// what the first one did — same cookie, same state, same code — and must be
// refused WITHOUT reaching the token endpoint, because the pending row is
// deleted before the code is spent.
func TestAnOIDCCallbackIsSpentOnceAndAReplayNeverReachesTheTokenEndpoint(t *testing.T) {
	h, d, idp := newOIDCAPI(t, false)
	ctx := context.Background()
	user, _, _ := seedAPIDevice(t, d)
	if err := d.Repo.PutOIDCIdentity(ctx, idp.URL, "sub-123", user.ID, d.Clock.Now().Unix()); err != nil {
		t.Fatalf("PutOIDCIdentity: %v", err)
	}
	cookie, state, nonce := startOIDC(t, h)
	idp.setNonce(nonce)

	target := "/v1/auth/oidc/callback?code=code-good&state=" + url.QueryEscape(state)
	res := getWithCookies(h, target, cookie)
	if res.Code != http.StatusFound {
		t.Fatalf("callback answered %d, want 302; body %x", res.Code, res.Body.Bytes())
	}
	const want = "https://dilla.test/#assertion="
	loc := res.Header().Get("Location")
	if !strings.HasPrefix(loc, want) {
		t.Fatalf("Location = %q, want the assertion in the fragment of %q", loc, want)
	}
	// The assertion is the login: it must name the mapped account.
	got, needs2FA, ok := d.Assertions.Spend(strings.TrimPrefix(loc, want))
	if !ok || got != user.ID {
		t.Fatalf("the assertion resolved to %s, %v; want %s", got, ok, user.ID)
	}
	if needs2FA {
		t.Fatal("this account has no confirmed second factor, so none is owed")
	}
	if n := idp.exchanges(); n != 1 {
		t.Fatalf("the token endpoint was called %d times for one login, want 1", n)
	}
	// The cookie is cleared whatever happens, so nothing is left to replay.
	if c := setCookie(t, res, oidcStateCookieName); c.Value != "" || c.MaxAge > 0 {
		t.Fatalf("the state cookie survived the callback: %+v", c)
	}

	res = getWithCookies(h, target, cookie)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("the replayed callback answered %d, want 401", res.Code)
	}
	if n := idp.exchanges(); n != 1 {
		t.Fatalf("the replayed callback reached the token endpoint (%d exchanges); the pending row must be gone", n)
	}
}

// A callback whose `state` is not the cookie's is a forgery — the login it
// claims to finish was started somewhere else — and is refused before anything
// is spent.
func TestAnOIDCCallbackWhoseStateDoesNotMatchTheCookieIsRefused(t *testing.T) {
	h, _, idp := newOIDCAPI(t, false)
	cookie, state, nonce := startOIDC(t, h)
	idp.setNonce(nonce)

	res := getWithCookies(h, "/v1/auth/oidc/callback?code=code-good&state=not-the-cookie", cookie)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("a forged state answered %d, want 401", res.Code)
	}
	// No cookie at all is the same refusal: a callback nobody started.
	res = getWithCookies(h, "/v1/auth/oidc/callback?code=code-good&state="+url.QueryEscape(state))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("a callback with no state cookie answered %d, want 401", res.Code)
	}
	// An identity provider that says the user said no is refused the same way.
	res = getWithCookies(h, "/v1/auth/oidc/callback?error=access_denied&state="+url.QueryEscape(state), cookie)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("a cancelled consent answered %d, want 401", res.Code)
	}

	// The forgery the pending table alone does NOT catch, and the reason the
	// cookie comparison exists: login CSRF. The attacker starts a login of
	// their own, so its state IS filed and IS spendable, and then walks the
	// victim's browser onto the callback carrying it. Only the comparison
	// against the victim's own cookie stands between that browser and an
	// assertion for the attacker's identity.
	attackerCookie, attackerState, attackerNonce := startOIDC(t, h)
	res = getWithCookies(h,
		"/v1/auth/oidc/callback?code=code-good&state="+url.QueryEscape(attackerState), cookie)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("the attacker's live state answered %d in the victim's browser, want 401", res.Code)
	}
	if n := idp.exchanges(); n != 0 {
		t.Fatalf("a refused callback spent the code anyway (%d exchanges)", n)
	}
	// And the refusal consumed nothing: both logins are still theirs to finish
	// from the browser that started them. (Neither subject is mapped to an
	// account here, so each ends at 403 — after a real exchange.)
	idp.setNonce(attackerNonce)
	if res := getWithCookies(h,
		"/v1/auth/oidc/callback?code=code-good&state="+url.QueryEscape(attackerState),
		attackerCookie); res.Code != http.StatusForbidden {
		t.Fatalf("the attacker's own callback answered %d, want 403; the refusal ate their row", res.Code)
	}
	idp.setNonce(nonce)
	if res := getWithCookies(h,
		"/v1/auth/oidc/callback?code=code-good&state="+url.QueryEscape(state),
		cookie); res.Code != http.StatusForbidden {
		t.Fatalf("the genuine callback answered %d, want 403; the forgeries ate its row", res.Code)
	}
	if n := idp.exchanges(); n != 2 {
		t.Fatalf("%d exchanges, want 2: one per login that was actually finished", n)
	}
}

// An identity provider that authenticates somebody dillad has no account for is
// E_FORBIDDEN, and it is E_FORBIDDEN whatever `auth.oidc.auto_create` says:
// users.umk_pub, ssk_pub and sig_umk_ssk are NOT NULL and are key material only
// the client can generate, so no route can conjure an account from an id_token.
// auto_create changes the detail and what is logged, nothing else. Deviation
// ID19 and ruling 40 record that as the shipped behaviour and the key as
// reserved; if the registration leg they name is ever built, this is the test
// that must be rewritten with it.
func TestAnOIDCLoginWithNoMappedAccountIsForbidden(t *testing.T) {
	for _, autoCreate := range []bool{false, true} {
		t.Run(fmt.Sprintf("auto_create=%v", autoCreate), func(t *testing.T) {
			h, _, idp := newOIDCAPI(t, autoCreate)
			idp.setSubject("sub-nobody")
			cookie, state, nonce := startOIDC(t, h)
			idp.setNonce(nonce)
			res := getWithCookies(h, "/v1/auth/oidc/callback?code=code-good&state="+url.QueryEscape(state), cookie)
			if res.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body %x", res.Code, res.Body.Bytes())
			}
			if code := errorCode(t, res); code != "E_FORBIDDEN" {
				t.Fatalf("error code = %s, want E_FORBIDDEN", code)
			}
			if n := idp.exchanges(); n != 1 {
				t.Fatalf("the callback made %d exchanges, want 1", n)
			}
		})
	}
}

// An instance with no identity provider still MOUNTS both routes — protocol/09
// declares them — and answers 501 rather than panicking on a nil *auth.OIDC. A
// 404 here would mean the routes went missing from Register.
func TestTheOIDCRoutesRefuseWhenNoIdentityProviderIsConfigured(t *testing.T) {
	h, deps := newTestAPI(t)
	if deps.OIDC != nil {
		t.Fatal("newTestAPI wired an identity provider; this test is about the instance that has none")
	}
	for _, path := range []string{"/v1/auth/oidc/start", "/v1/auth/oidc/callback"} {
		res := getWithCookies(h, path)
		if res.Code != http.StatusNotImplemented {
			t.Fatalf("%s answered %d, want 501", path, res.Code)
		}
	}
}

func TestDesktopOIDCCallbackCompletesOnlyThePKCEInitiator(t *testing.T) {
	h, d, idp := newOIDCAPI(t, false)
	user, _, _ := seedAPIDevice(t, d)
	if err := d.Repo.PutOIDCIdentity(context.Background(), idp.URL, "sub-123", user.ID, d.Clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	verifier := strings.Repeat("a", 43)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	post := func(path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), "POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://dilla.test")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	created := post("/desktop-auth/request", `{"kind":"oidc","options":"","challenge":"`+challenge+`"}`, nil)
	var request struct {
		ID string `json:"id"`
	}
	if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &request) != nil {
		t.Fatal(created.Code, created.Body)
	}
	page := getWithCookies(h, "/desktop-auth/"+request.ID)
	cookie := setCookie(t, page, "__Secure-dilla-desktop")
	launch := post("/desktop-auth/"+request.ID+"/oidc", `{}`, cookie)
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	if launch.Code != 200 || json.Unmarshal(launch.Body.Bytes(), &ticket) != nil {
		t.Fatal(launch.Code, launch.Body)
	}
	startURL := "/v1/auth/oidc/start?desktop_request=" + request.ID + "&desktop_ticket=" + ticket.Ticket
	start := getWithCookies(h, startURL)
	if start.Code != 302 {
		t.Fatal(start.Code, start.Body)
	}
	loc, _ := url.Parse(start.Header().Get("Location"))
	q := loc.Query()
	idp.setNonce(q.Get("nonce"))
	if replay := getWithCookies(h, startURL); replay.Code != 401 {
		t.Fatal("browser ticket replay", replay.Code)
	}
	callback := getWithCookies(h, "/v1/auth/oidc/callback?code=code-good&state="+q.Get("state"), setCookie(t, start, oidcStateCookieName))
	if callback.Code != 302 || callback.Header().Get("Location") != "/desktop-auth/"+request.ID+"?done=1" {
		t.Fatal(callback.Code, callback.Header())
	}
	if strings.Contains(callback.Header().Get("Location"), "assertion") {
		t.Fatal("assertion in browser navigation")
	}
	result := post("/desktop-auth/"+request.ID+"/consume", `{"verifier":"`+verifier+`"}`, nil)
	var completed struct {
		Assertion string `json:"assertion"`
		Needs     uint8  `json:"needs"`
	}
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &completed) != nil {
		t.Fatal(result.Code, result.Body)
	}
	got, owes, ok := d.Assertions.Spend(completed.Assertion)
	if !ok || got != user.ID || owes || completed.Needs != 0 {
		t.Fatal(got, owes, ok, completed)
	}
	if replay := post("/desktop-auth/"+request.ID+"/consume", `{"verifier":"`+verifier+`"}`, nil); replay.Code != 404 {
		t.Fatal("completion replay", replay.Code)
	}
}

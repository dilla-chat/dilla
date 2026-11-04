package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// newPasskeyAPI is newTestAPI with the relying party the composition root will
// derive from instance.domain, so the four ceremony routes are live. It rebuilds
// the mux rather than reaching into the one newTestAPI made, because Deps is
// copied per route and a field set afterwards would reach no handler.
func newPasskeyAPI(t *testing.T) (http.Handler, api.Deps) {
	t.Helper()
	h, d, _ := newPasskeyAPIFull(t, "", false)
	return h, d
}

// newPasskeyAPIUV is newPasskeyAPI on an instance whose
// auth.webauthn.user_verification is uv ("required" is the default).
func newPasskeyAPIUV(t *testing.T, uv string) (http.Handler, api.Deps) {
	t.Helper()
	h, d, _ := newPasskeyAPIFull(t, uv, false)
	return h, d
}

// newPasskeyAPIHooked is newPasskeyAPI whose handlers reach the store through a
// beforeTx, for the in-transaction re-check tests.
func newPasskeyAPIHooked(t *testing.T) (http.Handler, api.Deps, *beforeTx) {
	t.Helper()
	return newPasskeyAPIFull(t, "", true)
}

// newPasskeyAPIFull writes the relying party into the shared Config too, so the
// handlers' "holds a factor" reads the rp_id the ceremonies file credentials
// under, exactly as the composition root's defaulting does.
func newPasskeyAPIFull(t *testing.T, uv string, hooked bool) (http.Handler, api.Deps, *beforeTx) {
	t.Helper()
	return newPasskeyAPITuned(t, uv, hooked, nil)
}

// newPasskeyAPITuned is newPasskeyAPIFull with a hook into the configuration, as
// newTestAPIWithConfig has.
func newPasskeyAPITuned(t *testing.T, uv string, hooked bool, tune func(*config.Config)) (http.Handler, api.Deps, *beforeTx) {
	t.Helper()
	_, d := newTestAPIWithConfig(t, tune)
	c := &d.Config.Auth.WebAuthn
	c.RPID = d.Domain
	c.RPDisplayName = d.Domain
	c.RPOrigins = []string{"https://" + d.Domain}
	if uv != "" {
		c.UserVerification = uv
	}
	pk, err := auth.NewPasskeys(auth.ConfigFromDilla(*c), d.Repo, d.Clock)
	if err != nil {
		t.Fatalf("NewPasskeys: %v", err)
	}
	pk.SetLimits(c.MaxOpenLogins, c.MaxOpenPerUser)
	d.Passkeys = pk
	var hook *beforeTx
	if hooked {
		hook = &beforeTx{Repository: d.Repo}
		d.Repo = hook
	}
	m := server.NewMux()
	api.Register(m, d)
	return m, d, hook
}

// An instance with no relying party configured still MOUNTS the four routes —
// protocol/09 declares them — and answers 501 rather than panicking on a nil
// *auth.Passkeys. A 404 here would mean the routes went missing from Register.
func TestThePasskeyRoutesRefuseWhenNoRelyingPartyIsConfigured(t *testing.T) {
	h, deps := newTestAPI(t)
	if deps.Passkeys != nil {
		t.Fatal("newTestAPI wired a relying party; this test is about the instance that has none")
	}
	_, _, token := seedAPISession(t, deps)
	empty, _ := cborx.Marshal([]any{})
	finish, _ := cborx.Marshal([]any{id.New(), "{}"})
	for _, tc := range []struct {
		path  string
		body  []byte
		token string
	}{
		{"/v1/auth/passkey/register/begin", empty, token},
		{"/v1/auth/passkey/register/finish", finish, token},
		{"/v1/auth/passkey/login/begin", empty, ""},
		{"/v1/auth/passkey/login/finish", finish, ""},
	} {
		res := postCBORAuth(h, tc.path, tc.body, tc.token)
		if res.Code != http.StatusNotImplemented {
			t.Fatalf("%s answered %d, want 501", tc.path, res.Code)
		}
	}
}

// register/begin is an enrolled route: it adds a credential to an account that
// already exists, so an anonymous caller must not be able to start a ceremony
// against somebody else's account.
func TestPasskeyRegisterBeginNeedsASessionAndReturnsCreationOptions(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	empty, _ := cborx.Marshal([]any{nil})
	if res := postCBOR(h, "/v1/auth/passkey/register/begin", empty); res.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated register/begin answered %d, want 401", res.Code)
	}

	_, _, token := seedAPISession(t, deps)
	res := postCBORAuth(h, "/v1/auth/passkey/register/begin", empty, token)
	if res.Code != http.StatusOK {
		t.Fatalf("register/begin answered %d: %q", res.Code, res.Body.String())
	}
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("register/begin returned %d elements, want [ceremony_id, options]", len(out))
	}
	ceremony, ok := out[0].([]byte)
	if !ok || len(ceremony) != id.Size {
		t.Fatalf("ceremony_id = %#v, want a 16-byte string", out[0])
	}
	// options is an opaque tstr: the library's JSON, not re-encoded as CBOR.
	options, ok := out[1].(string)
	if !ok {
		t.Fatalf("options = %#v, want a text string", out[1])
	}
	var creation struct {
		PublicKey struct {
			RP struct {
				ID string `json:"id"`
			} `json:"rp"`
			User struct {
				ID string `json:"id"`
			} `json:"user"`
			AuthenticatorSelection struct {
				ResidentKey string `json:"residentKey"`
			} `json:"authenticatorSelection"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(options), &creation); err != nil {
		t.Fatalf("options are not the library's JSON: %v (%q)", err, options)
	}
	if creation.PublicKey.RP.ID != deps.Domain {
		t.Fatalf("rp.id = %q, want %q", creation.PublicKey.RP.ID, deps.Domain)
	}
	if creation.PublicKey.User.ID == "" {
		t.Fatal("the creation options carry no user handle")
	}
	// ResidentKey "required" is what makes the credential discoverable, which is
	// the whole premise of the username-less login route below.
	if creation.PublicKey.AuthenticatorSelection.ResidentKey != "required" {
		t.Fatalf("residentKey = %q, want \"required\"",
			creation.PublicKey.AuthenticatorSelection.ResidentKey)
	}
}

// login/begin is unauthenticated and names no account: the options it returns
// must carry no allowCredentials list, or the route would answer "these are the
// credentials that exist" to anyone who asked.
func TestPasskeyLoginBeginIsUnauthenticatedAndNamesNoAccount(t *testing.T) {
	h, _ := newPasskeyAPI(t)
	empty, _ := cborx.Marshal([]any{})
	res := postCBOR(h, "/v1/auth/passkey/login/begin", empty)
	if res.Code != http.StatusOK {
		t.Fatalf("login/begin answered %d: %q", res.Code, res.Body.String())
	}
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("login/begin returned %d elements, want [ceremony_id, options]", len(out))
	}
	options, ok := out[1].(string)
	if !ok {
		t.Fatalf("options = %#v, want a text string", out[1])
	}
	var assertion struct {
		PublicKey struct {
			Challenge        string `json:"challenge"`
			AllowCredentials []any  `json:"allowCredentials"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal([]byte(options), &assertion); err != nil {
		t.Fatalf("options are not the library's JSON: %v (%q)", err, options)
	}
	if assertion.PublicKey.Challenge == "" {
		t.Fatal("the assertion options carry no challenge")
	}
	if len(assertion.PublicKey.AllowCredentials) != 0 {
		t.Fatalf("a discoverable login listed %d credentials", len(assertion.PublicKey.AllowCredentials))
	}
}

// A finish leg is refused whatever is wrong with it — an unknown ceremony, an
// expired one, a replayed one, a response the authenticator got wrong — and the
// login leg says only E_UNAUTHENTICATED, because a discoverable ceremony names
// no account and therefore has nothing account-specific to report.
func TestAPasskeyFinishWithAnUnknownCeremonyIsRefused(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	_, _, token := seedAPISession(t, deps)
	body, _ := cborx.Marshal([]any{id.New(), "{}", "name"})
	login, _ := cborx.Marshal([]any{id.New(), "{}"})

	res := postCBORAuth(h, "/v1/auth/passkey/register/finish", body, token)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("register/finish on an unknown ceremony answered %d, want 400", res.Code)
	}
	res = postCBOR(h, "/v1/auth/passkey/login/finish", login)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("login/finish on an unknown ceremony answered %d, want 401", res.Code)
	}
}

// The ceremony is single use in the store, so the replay of a body that was
// once valid finds no row. Beginning twice and finishing the FIRST ceremony
// after the second has been taken proves the row is consumed by the take and
// not by the outcome.
func TestAPasskeyCeremonyIsSpentOnceItIsTaken(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	_, _, token := seedAPISession(t, deps)
	empty, _ := cborx.Marshal([]any{nil})
	res := postCBORAuth(h, "/v1/auth/passkey/register/begin", empty, token)
	if res.Code != http.StatusOK {
		t.Fatalf("register/begin answered %d", res.Code)
	}
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ceremony := out[0].([]byte)
	var cid id.ID
	copy(cid[:], ceremony)
	body, _ := cborx.Marshal([]any{cid, "{}", "name"})
	first := postCBORAuth(h, "/v1/auth/passkey/register/finish", body, token)
	second := postCBORAuth(h, "/v1/auth/passkey/register/finish", body, token)
	if first.Code != http.StatusBadRequest || second.Code != http.StatusBadRequest {
		t.Fatalf("finish answered %d then %d, want 400 both times", first.Code, second.Code)
	}
	// And the row really is gone: PruneCeremonies has nothing left to drop.
	n, err := deps.Passkeys.PruneCeremonies(t.Context())
	if err != nil {
		t.Fatalf("PruneCeremonies: %v", err)
	}
	if n != 0 {
		t.Fatalf("PruneCeremonies dropped %d rows; the finish did not consume the ceremony", n)
	}
}

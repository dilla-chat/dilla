package api_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/jonasthim/dilla/internal/config"
)

// Wave B task 3: a password login that owes a second factor carries a third element, 1 when the account
// holds a passkey for this rp_id (so "Use a passkey" on the code step would work), else 0. A login owing
// nothing carries none, and so does every refusal: the flag is never read by anyone who has not passed
// the password.
func TestAPasswordLoginOwingAFactorSaysWhetherAPasskeyWouldWork(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "", false, func(c *config.Config) { c.Limits.Rate.LoginBurst = 100 })

	// No factor: two elements, no flag.
	plain, _, _ := seedAPISession(t, deps)
	plainPw := seedPassword(t, deps, plain, "correct horse battery staple")
	if status, out := login(t, h, plain.Username, plainPw); status != http.StatusOK || len(out) != 2 || out[1] != uint64(0) {
		t.Fatalf("a login owing nothing = %d %#v, want [assertion, 0] with no passkey element", status, out)
	}

	// A passkey and no app: needs 2, passkey 1.
	keyed, _, keyedToken := seedAPISession(t, deps)
	keyedPw := seedPassword(t, deps, keyed, "correct horse battery staple")
	device := newVirtualAuthenticator(t, deps.Domain, true)
	credID := enrolPasskey(t, h, keyedToken, device)
	if status, out := login(t, h, keyed.Username, keyedPw); status != http.StatusOK || len(out) != 3 || out[1] != uint64(2) || out[2] != uint64(1) {
		t.Fatalf("a passkey-only account's login = %d %#v, want [assertion, 2, 1]", status, out)
	}

	// An app and no passkey: needs 1, passkey 0.
	app, _, appToken := seedAPISession(t, deps)
	appPw := seedPassword(t, deps, app, "correct horse battery staple")
	enrolTOTP(t, h, deps, appToken)
	if status, out := login(t, h, app.Username, appPw); status != http.StatusOK || len(out) != 3 || out[1] != uint64(1) || out[2] != uint64(0) {
		t.Fatalf("an app-only account's login = %d %#v, want [assertion, 1, 0]", status, out)
	}

	// The same app account once it also holds a passkey under this rp_id: needs 1, passkey 1.
	row, err := deps.Repo.GetWebauthnCredential(t.Context(), credID)
	if err != nil {
		t.Fatalf("GetWebauthnCredential: %v", err)
	}
	if _, err := deps.Repo.DeleteWebauthnCredential(t.Context(), keyed.ID, credID); err != nil {
		t.Fatalf("DeleteWebauthnCredential: %v", err)
	}
	row.UserID = app.ID
	if err := deps.Repo.PutWebauthnCredential(t.Context(), row); err != nil {
		t.Fatalf("PutWebauthnCredential: %v", err)
	}
	if status, out := login(t, h, app.Username, appPw); status != http.StatusOK || len(out) != 3 || out[1] != uint64(1) || out[2] != uint64(1) {
		t.Fatalf("an app account with a passkey = %d %#v, want [assertion, 1, 1]", status, out)
	}

	// A passkey under another relying party would not work here: passkey 0.
	if _, err := deps.Repo.DeleteWebauthnCredential(t.Context(), app.ID, credID); err != nil {
		t.Fatalf("DeleteWebauthnCredential: %v", err)
	}
	row.RPID = "other.example"
	if err := deps.Repo.PutWebauthnCredential(t.Context(), row); err != nil {
		t.Fatalf("PutWebauthnCredential: %v", err)
	}
	if status, out := login(t, h, app.Username, appPw); status != http.StatusOK || len(out) != 3 || out[2] != uint64(0) {
		t.Fatalf("an app account whose passkey is under other.example = %d %#v, want [assertion, 1, 0]", status, out)
	}
}

// The security pin: before the password verifies, an account with a passkey and one without answer the
// same bytes on the same status, and neither answer carries a passkey element. A wrong password must not
// say whether a passkey exists.
func TestAWrongPasswordNeverSaysWhetherTheAccountHasAPasskey(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "", false, func(c *config.Config) { c.Limits.Rate.LoginBurst = 100 })

	keyed, _, keyedToken := seedAPISession(t, deps)
	seedPassword(t, deps, keyed, "correct horse battery staple")
	enrolPasskey(t, h, keyedToken, newVirtualAuthenticator(t, deps.Domain, true))

	bare, _, _ := seedAPISession(t, deps)
	seedPassword(t, deps, bare, "correct horse battery staple")

	withKey := loginRaw(h, keyed.Username, "wrong password entirely")
	without := loginRaw(h, bare.Username, "wrong password entirely")
	unknown := loginRaw(h, "nobody-here", "wrong password entirely")
	for name, res := range map[string]int{"passkey": withKey.Code, "no passkey": without.Code, "unknown": unknown.Code} {
		if res != http.StatusUnauthorized {
			t.Fatalf("a wrong password (%s) = %d, want 401", name, res)
		}
	}
	if !bytes.Equal(withKey.Body.Bytes(), without.Body.Bytes()) || !bytes.Equal(withKey.Body.Bytes(), unknown.Body.Bytes()) {
		t.Fatalf("wrong-password answers differ:\n passkey    %x\n no passkey %x\n unknown    %x",
			withKey.Body.Bytes(), without.Body.Bytes(), unknown.Body.Bytes())
	}
}

// Wave B task 3 fix round 1 (security review item 8): "Use a passkey" on the code step. An account that owes the app
// code after its password (needs 1, passkey 1) signs in with a user-verified passkey instead: the passkey login is a
// login of its own and its assertion owes nothing, which is what the worker accepts in signin-totp.
func TestAPasskeyStandsInForTheCodeAfterAPasswordLoginOwingIt(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "", false, func(c *config.Config) { c.Limits.Rate.LoginBurst = 100 })
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	enrolTOTP(t, h, deps, token)
	device := newVirtualAuthenticator(t, deps.Domain, true)
	enrolPasskeyWith(t, h, token, device, stepUp(t, h, token, stepUpPassword, pw))

	status, out := login(t, h, u.Username, pw)
	if status != http.StatusOK || len(out) != 3 || out[1] != uint64(1) || out[2] != uint64(1) {
		t.Fatalf("the password login = %d %#v, want [assertion, 1, 1]", status, out)
	}
	assertion, needs := passkeyLoginNeeds(t, h, device)
	if needs != 0 {
		t.Fatalf("a user-verified passkey after the password: needs = %d, want 0", needs)
	}
	if got, owes := spendAssertion(t, deps, assertion); got != u.ID || owes {
		t.Fatalf("the passkey's assertion = %s owing %v, want %s owing nothing", got, owes, u.ID)
	}
}

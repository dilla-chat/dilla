package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/clock"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

// seedPassword gives a seeded user a password credential, hashed with the same
// testHasher the Deps carries, and returns the plaintext.
func seedPassword(t *testing.T, d api.Deps, u store.UserRow, pw string) string {
	t.Helper()
	phc, err := d.Hasher.Hash(context.Background(), pw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := d.Repo.PutPasswordCredential(context.Background(), u.ID, phc, d.Clock.Now().Unix()); err != nil {
		t.Fatalf("PutPasswordCredential: %v", err)
	}
	return pw
}

// login posts the password login and returns the status and the decoded body.
func login(t *testing.T, h http.Handler, username, pw string) (int, []any) {
	t.Helper()
	body, err := cborx.Marshal([]any{username, pw})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res := postCBOR(h, "/v1/auth/password/login", body)
	var out []any
	if res.Code == http.StatusOK {
		if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode login body: %v", err)
		}
	}
	return res.Code, out
}

// enrolTOTP walks the two enrolled routes and returns the base32 secret and the
// recovery codes the confirm hands back.
func enrolTOTP(t *testing.T, h http.Handler, d api.Deps, token string) (string, []string) {
	t.Helper()
	secret, codes := enrolTOTPWith(t, h, d, token, nil)
	if len(codes) != 10 {
		t.Fatalf("confirm returned %d recovery codes, want 10", len(codes))
	}
	return secret, codes
}

// enrolTOTPWith is enrolTOTP with a step-up token (nil sends null); it returns
// whatever backup codes the confirm answers, which is none when the account
// already held unused ones.
func enrolTOTPWith(t *testing.T, h http.Handler, d api.Deps, token string, stepup any) (string, []string) {
	t.Helper()
	body, _ := cborx.Marshal([]any{stepup})
	res := postCBORAuth(h, "/v1/auth/totp/enroll", body, token)
	if res.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, body %x", res.Code, res.Body.Bytes())
	}
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode enroll: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("enroll returned %d elements, want [secret, otpauth_url]", len(out))
	}
	secret, ok := out[0].(string)
	if !ok || secret == "" {
		t.Fatalf("enroll secret = %#v", out[0])
	}
	if url, ok := out[1].(string); !ok || url == "" {
		t.Fatalf("enroll otpauth_url = %#v", out[1])
	}

	code, err := totp.GenerateCode(secret, d.Clock.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	body, _ = cborx.Marshal([]any{code})
	res = postCBORAuth(h, "/v1/auth/totp/confirm", body, token)
	if res.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, body %x", res.Code, res.Body.Bytes())
	}
	var confirmed []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &confirmed); err != nil {
		t.Fatalf("decode confirm: %v", err)
	}
	if len(confirmed) != 1 {
		t.Fatalf("confirm returned %d elements, want [recovery_codes]", len(confirmed))
	}
	raw, ok := confirmed[0].([]any)
	if !ok {
		t.Fatalf("confirm recovery_codes = %#v", confirmed[0])
	}
	codes := make([]string, 0, len(raw))
	for _, c := range raw {
		s, ok := c.(string)
		if !ok {
			t.Fatalf("recovery code %#v is not a string", c)
		}
		codes = append(codes, s)
	}
	// The confirm consumed its own counter, so the code that proved the
	// enrolment can never also clear a login. Step one period on so the next
	// code the test mints is a counter the store has not seen.
	advance(t, d, 30*time.Second)
	return secret, codes
}

// advance moves the harness's fake clock.
func advance(t *testing.T, d api.Deps, by time.Duration) {
	t.Helper()
	f, ok := d.Clock.(*clock.Fake)
	if !ok {
		t.Fatalf("the harness clock is %T, not a *clock.Fake", d.Clock)
	}
	f.Advance(by)
}

func TestPasswordLoginReturnsAnAssertionAndNeedsTOTP(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")

	status, out := login(t, h, u.Username, pw)
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want 200", status)
	}
	if len(out) != 2 {
		t.Fatalf("login returned %d elements, want [assertion, needs_totp]", len(out))
	}
	assertion, ok := out[0].(string)
	if !ok || assertion == "" {
		t.Fatalf("assertion = %#v", out[0])
	}
	if needs, ok := out[1].(uint64); !ok || needs != 0 {
		t.Fatalf("needs_totp = %#v, want 0 before TOTP is enrolled", out[1])
	}

	// Once TOTP is confirmed the same login says so.
	enrolTOTP(t, h, deps, token)
	status, out = login(t, h, u.Username, pw)
	if status != http.StatusOK {
		t.Fatalf("login status after enrolment = %d, want 200", status)
	}
	if needs, ok := out[1].(uint64); !ok || needs != 1 {
		t.Fatalf("needs_totp = %#v, want 1 once TOTP is confirmed", out[1])
	}
}

// loginRaw is login without the decode, for the tests that compare two whole
// responses byte for byte.
func loginRaw(h http.Handler, username, pw string) *httptest.ResponseRecorder {
	body, err := cborx.Marshal([]any{username, pw})
	if err != nil {
		panic("marshal login body: " + err.Error())
	}
	return postCBOR(h, "/v1/auth/password/login", body)
}

// One attempt against each proves indistinguishability only for the FREE
// attempts, which were never the hard part. The divergence this route has to
// not have appears once one of the two crosses free_attempts: if the lockout is
// reported for handles that exist and not for handles that do not, the fifth
// wrong password answers 429 with a retry_after_ms for a real account and 401
// with a null one for an imaginary one — on the status line AND in the body —
// and the dummy-hash timing equaliser two layers down is paid for nothing.
func TestAWrongPasswordAndAnUnknownAccountAreTheSameRefusal(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) {
		// Both buckets out of the way: they refuse identically for the two
		// handles, so leaving them in would hide the divergence under a 429
		// that has nothing to do with the lockout.
		c.Limits.Rate.LoginBurst = 100
		c.Limits.Rate.LoginFailedBurst = 100
	})
	u, _, _ := seedAPISession(t, deps)
	seedPassword(t, deps, u, "correct horse battery staple")

	locked := false
	for i := 1; i <= deps.Config.Auth.Lockout.FreeAttempts+2; i++ {
		known := loginRaw(h, u.Username, "not the password")
		unknown := loginRaw(h, "nosuchaccount", "not the password")
		if known.Code != unknown.Code {
			t.Fatalf("attempt %d: a wrong password answered %d and an unknown handle %d",
				i, known.Code, unknown.Code)
		}
		if !bytes.Equal(known.Body.Bytes(), unknown.Body.Bytes()) {
			t.Fatalf("attempt %d: the two refusals carry different bodies: %x and %x",
				i, known.Body.Bytes(), unknown.Body.Bytes())
		}
		if a, b := known.Header().Get("Retry-After"), unknown.Header().Get("Retry-After"); a != b {
			t.Fatalf("attempt %d: Retry-After = %q for a real handle and %q for an unknown one", i, a, b)
		}
		if known.Code == http.StatusTooManyRequests {
			locked = true
		} else if known.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401 or 429", i, known.Code)
		}
	}
	// Without this the loop above would prove only that seven identical 401s
	// are identical, which is what the one-attempt version of this test proved.
	if !locked {
		t.Fatalf("%d wrong passwords never produced a lockout; the loop proved nothing about the 429",
			deps.Config.Auth.Lockout.FreeAttempts+2)
	}
}

// Reporting a lockout is not enforcing one. The gate has to stand BEFORE the
// credential is checked, or the attacker whose sixth guess happens to be right
// walks through the lockout holding an assertion.
func TestACorrectPasswordIsRefusedWhileTheAccountIsLockedOut(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) {
		c.Limits.Rate.LoginBurst = 100
		c.Limits.Rate.LoginFailedBurst = 100
	})
	u, _, _ := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")

	for i := 0; i <= deps.Config.Auth.Lockout.FreeAttempts; i++ {
		if status, _ := login(t, h, u.Username, "not the password"); status == http.StatusOK {
			t.Fatalf("wrong password %d logged in", i+1)
		}
	}
	if status, _ := login(t, h, u.Username, pw); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: the CORRECT password must not walk through a locked account", status)
	}

	// The lockout runs from the LAST failure, so waiting it out reopens the
	// account: one that never reopens is a denial of service on its owner.
	advance(t, deps, deps.Config.Auth.Lockout.FirstLockout.Value()+time.Second)
	if status, _ := login(t, h, u.Username, pw); status != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the lockout expired", status)
	}
}

func TestTheAssertionIsSpentExactlyOnce(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	secret, _ := enrolTOTP(t, h, deps, token)

	_, out := login(t, h, u.Username, pw)
	assertion := out[0].(string)

	code, err := totp.GenerateCode(secret, deps.Clock.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	body, _ := cborx.Marshal([]any{assertion, code})
	if res := postCBOR(h, "/v1/auth/totp/verify", body); res.Code != http.StatusOK {
		t.Fatalf("first verify status = %d, body %x", res.Code, res.Body.Bytes())
	}

	// Replay the SPENT assertion with a FRESH code, one period on. Nothing but
	// the assertion can refuse this: the code has never been presented, so its
	// counter is still ahead of the stored one.
	advance(t, deps, 30*time.Second)
	fresh, err := totp.GenerateCode(secret, deps.Clock.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	replay, _ := cborx.Marshal([]any{assertion, fresh})
	if res := postCBOR(h, "/v1/auth/totp/verify", replay); res.Code == http.StatusOK {
		t.Fatal("a spent assertion was accepted a second time")
	}
	// And the fresh code itself is still good behind a fresh assertion, which is
	// what proves the refusal above was the assertion and not the code.
	_, out = login(t, h, u.Username, pw)
	good, _ := cborx.Marshal([]any{out[0].(string), fresh})
	if res := postCBOR(h, "/v1/auth/totp/verify", good); res.Code != http.StatusOK {
		t.Fatalf("the fresh code was refused behind a fresh assertion: %d", res.Code)
	}
}

func TestAReplayedTOTPCounterIsRefusedInsideTheSkewWindow(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	secret, _ := enrolTOTP(t, h, deps, token)

	code, err := totp.GenerateCode(secret, deps.Clock.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}

	_, out := login(t, h, u.Username, pw)
	body, _ := cborx.Marshal([]any{out[0].(string), code})
	res := postCBOR(h, "/v1/auth/totp/verify", body)
	if res.Code != http.StatusOK {
		t.Fatalf("first verify status = %d, body %x", res.Code, res.Body.Bytes())
	}
	var verified []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &verified); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if len(verified) != 1 {
		t.Fatalf("verify returned %d elements, want [assertion]", len(verified))
	}
	if s, ok := verified[0].(string); !ok || s == "" {
		t.Fatalf("verify assertion = %#v", verified[0])
	}

	// A FRESH assertion and the SAME code, at the same instant: the code is
	// still inside the skew window, and the only thing that refuses it is the
	// stored counter.
	_, out = login(t, h, u.Username, pw)
	body, _ = cborx.Marshal([]any{out[0].(string), code})
	if res := postCBOR(h, "/v1/auth/totp/verify", body); res.Code == http.StatusOK {
		t.Fatal("a TOTP code was accepted twice inside the skew window; the counter is not consumed")
	}
}

func TestARecoveryCodeWorksOnceAndThenDoesNot(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	_, codes := enrolTOTP(t, h, deps, token)

	_, out := login(t, h, u.Username, pw)
	body, _ := cborx.Marshal([]any{out[0].(string), codes[3]})
	res := postCBOR(h, "/v1/auth/recovery/verify", body)
	if res.Code != http.StatusOK {
		t.Fatalf("recovery verify status = %d, body %x", res.Code, res.Body.Bytes())
	}
	var verified []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &verified); err != nil {
		t.Fatalf("decode recovery verify: %v", err)
	}
	if s, ok := verified[0].(string); !ok || s == "" {
		t.Fatalf("recovery assertion = %#v", verified[0])
	}

	_, out = login(t, h, u.Username, pw)
	body, _ = cborx.Marshal([]any{out[0].(string), codes[3]})
	if res := postCBOR(h, "/v1/auth/recovery/verify", body); res.Code == http.StatusOK {
		t.Fatal("a recovery code was spent twice")
	}

	// A different, untouched code still works.
	_, out = login(t, h, u.Username, pw)
	body, _ = cborx.Marshal([]any{out[0].(string), codes[4]})
	if res := postCBOR(h, "/v1/auth/recovery/verify", body); res.Code != http.StatusOK {
		t.Fatalf("an unspent recovery code was refused: %d", res.Code)
	}
}

func TestEnrollingOverAConfirmedAuthenticatorIsRefused(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	enrolTOTP(t, h, deps, token)

	body, _ := cborx.Marshal([]any{nil})
	res := postCBORAuth(h, "/v1/auth/totp/enroll", body, token)
	if res.Code != http.StatusForbidden {
		t.Fatalf("a second enrolment answered %d, want 403: overwriting the row would clear confirmed_at", res.Code)
	}
	// And the account's second factor is still required.
	_, out := login(t, h, u.Username, pw)
	if needs, ok := out[1].(uint64); !ok || needs != 1 {
		t.Fatalf("needs_totp = %#v after a refused re-enrolment, want 1", out[1])
	}
}

func TestPasswordChangeNeedsTheOldPasswordAndReplacesTheCredential(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")

	body, _ := cborx.Marshal([]any{"wrong old password", "a brand new password"})
	if res := postCBORAuth(h, "/v1/auth/password", body, token); res.Code == http.StatusNoContent {
		t.Fatal("the password changed without the old one")
	}
	body, _ = cborx.Marshal([]any{pw, "a brand new password"})
	if res := postCBORAuth(h, "/v1/auth/password", body, token); res.Code != http.StatusNoContent {
		t.Fatalf("password change status = %d, want 204", res.Code)
	}
	if status, _ := login(t, h, u.Username, pw); status == http.StatusOK {
		t.Fatal("the old password still logs in")
	}
	if status, _ := login(t, h, u.Username, "a brand new password"); status != http.StatusOK {
		t.Fatalf("the new password does not log in: %d", status)
	}
}

// Task 11 (Jonas, Q7): the factors read says when the password was last changed: unknown (null) for a password set before the
// change was recorded, and the change's time once it is changed.
func TestFactorsSayWhenThePasswordWasChanged(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	if got := getFactors(t, h, token)[5]; got != nil {
		t.Fatalf("password_changed = %#v before any change, want null", got)
	}
	body, _ := cborx.Marshal([]any{pw, "a brand new password"})
	if res := postCBORAuth(h, "/v1/auth/password", body, token); res.Code != http.StatusNoContent {
		t.Fatalf("password change status = %d, want 204", res.Code)
	}
	want := uint64(deps.Clock.Now().Unix())
	if got, ok := getFactors(t, h, token)[5].(uint64); !ok || got != want {
		t.Fatalf("password_changed = %#v after the change, want %d", getFactors(t, h, token)[5], want)
	}
}

// protocol/09-http-api.md marks POST /v1/auth/password "E (step-up)". With a
// credential on the account the old password IS the step-up; on an account
// registered through a passkey or OIDC there is no credential to re-present, so
// the step-up falls back to the session's own freshness — the same one DELETE
// /v1/accounts/me enforces. Without it a stolen session token plants a password
// credential, which is a second login path that outlives revoking the passkey.
func TestSettingAFirstPasswordNeedsAFreshSession(t *testing.T) {
	h, deps := newTestAPI(t)
	stale, _, staleToken := seedAPISession(t, deps)
	advance(t, deps, deps.Config.Auth.Session.ReauthWindow.Value()+time.Second)

	body, err := cborx.Marshal([]any{nil, "a brand new password"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if res := postCBORAuth(h, "/v1/auth/password", body, staleToken); res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a stale session must not set a first password", res.Code)
	}
	if status, _ := login(t, h, stale.Username, "a brand new password"); status == http.StatusOK {
		t.Fatal("the refused request planted a credential anyway")
	}

	// A session established inside the window may.
	fresh, _, freshToken := seedAPISession(t, deps)
	if res := postCBORAuth(h, "/v1/auth/password", body, freshToken); res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: a fresh session may set a first password (body %x)",
			res.Code, res.Body.Bytes())
	}
	if status, _ := login(t, h, fresh.Username, "a brand new password"); status != http.StatusOK {
		t.Fatalf("the password a fresh session set does not log in: %d", status)
	}
}

// web3.6 review M7: on an account that holds a second factor, a fresh session is not enough to set
// a first password. A browser can establish a fresh session from its device key at will, so the
// window alone would let a stolen browser plant a password, step up with it (method 0) and add or
// remove factors the passkey step-up was meant to guard. The first password takes a step-up
// token, [old = null, new, stepup], minted by a factor the account holds.
func TestAFirstPasswordOnAnAccountWithAFactorNeedsAStepUp(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	_, codes := enrolTOTP(t, h, deps, token)

	refused := func(name string, body []any) {
		t.Helper()
		b, err := cborx.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		res := postCBORAuth(h, "/v1/auth/password", b, token)
		if res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
			t.Fatalf("%s: status = %d (%x), want 403 E_STEP_UP", name, res.Code, res.Body.Bytes())
		}
		if _, err := deps.Repo.GetPasswordCredential(context.Background(), u.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: the refused request planted a credential anyway (%v)", name, err)
		}
	}
	refused("no step-up element", []any{nil, "a brand new password"})
	refused("a null step-up", []any{nil, "a brand new password", nil})
	refused("an unknown step-up", []any{nil, "a brand new password", "not-a-token"})

	stepup := stepUp(t, h, token, stepUpBackup, codes[0])
	b, err := cborx.Marshal([]any{nil, "a brand new password", stepup})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if res := postCBORAuth(h, "/v1/auth/password", b, token); res.Code != http.StatusNoContent {
		t.Fatalf("with a step-up: status = %d (%x), want 204", res.Code, res.Body.Bytes())
	}
	if status, _ := login(t, h, u.Username, "a brand new password"); status != http.StatusOK {
		t.Fatalf("the password set with a step-up does not log in: %d", status)
	}
}

// A first password with a step-up still needs a fresh session: the token does not stand in for
// the window, and a refusal by the window leaves the token unspent.
func TestAFirstPasswordWithAStepUpStillNeedsAFreshSession(t *testing.T) {
	h, deps := newTestAPI(t)
	_, _, token := seedAPISession(t, deps)
	_, codes := enrolTOTP(t, h, deps, token)
	advance(t, deps, deps.Config.Auth.Session.ReauthWindow.Value()+time.Second)
	stepup := stepUp(t, h, token, stepUpBackup, codes[0])
	b, err := cborx.Marshal([]any{nil, "a brand new password", stepup})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if res := postCBORAuth(h, "/v1/auth/password", b, token); res.Code != http.StatusForbidden || errorCode(t, res) != "E_FORBIDDEN" {
		t.Fatalf("a stale session with a step-up = %d (%x), want 403 E_FORBIDDEN", res.Code, res.Body.Bytes())
	}
	// The token is unspent: the regenerate route takes it.
	if res := newCodes(h, token, stepup); res.Code != http.StatusOK {
		t.Fatalf("the token after the window's refusal = %d, want it unspent (200)", res.Code)
	}
}

// The `login_failed` bucket is the per-ADDRESS half of throttling, and the
// reason it exists is the attacker who spreads guesses over many handles: the
// `login` bucket is keyed by address AND by handle spelling, so a thousand
// guesses against a thousand handles never empties a single handle's bucket.
// RecordFailure is the one writer of login_failed; this route only PEEKS at it,
// so a correct password never spends the failure budget — but it is refused
// while the budget an attacker sharing the address burnt is still empty.
func TestFailuresFromOneAddressThrottleEveryAccountBehindIt(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) {
		// Take the per-attempt `login` bucket out of the picture: what is under
		// test is the failure budget, not the arrival rate.
		c.Limits.Rate.LoginBurst = 100
	})
	victim, _, _ := seedAPISession(t, deps)
	pw := seedPassword(t, deps, victim, "correct horse battery staple")

	// Spend the address's whole failure budget on OTHER accounts. Each one is a
	// single failure, well inside free_attempts, so no per-account lockout fires.
	for i := 0; i < deps.Config.Limits.Rate.LoginFailedBurst; i++ {
		other, _, _ := seedAPISession(t, deps)
		seedPassword(t, deps, other, "correct horse battery staple")
		if status, _ := login(t, h, other.Username, "not the password"); status != http.StatusUnauthorized {
			t.Fatalf("failure %d against a fresh account answered %d, want 401", i+1, status)
		}
	}

	if status, _ := login(t, h, victim.Username, pw); status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: this address spent its whole login_failed budget on %d other accounts",
			status, deps.Config.Limits.Rate.LoginFailedBurst)
	}

	// One token's worth of refill later the same request is allowed, which is
	// what proves the refusal was the failure bucket and not something durable.
	advance(t, deps, time.Duration(float64(time.Second)/deps.Config.Limits.Rate.LoginFailedPerSecond)+time.Second)
	if status, _ := login(t, h, victim.Username, pw); status != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the failure budget refilled", status)
	}
}

// --- web3.6 S2: second-factor logins, factor routes and step-up ---

// The methods of POST /v1/auth/stepup.
const (
	stepUpPassword uint64 = 0
	stepUpTOTP     uint64 = 1
	stepUpBackup   uint64 = 2
)

// stepUpRaw posts POST /v1/auth/stepup [method, secret] for the session token holds.
func stepUpRaw(h http.Handler, token string, method uint64, secret string) *httptest.ResponseRecorder {
	body, err := cborx.Marshal([]any{method, secret})
	if err != nil {
		panic("marshal step-up body: " + err.Error())
	}
	return postCBORAuth(h, "/v1/auth/stepup", body, token)
}

// stepUp mints a step-up token and fails the test unless the route answers 201
// [stepup(tstr), expires(uint)].
func stepUp(t *testing.T, h http.Handler, token string, method uint64, secret string) string {
	t.Helper()
	res := stepUpRaw(h, token, method, secret)
	return stepUpToken(t, res)
}

// stepUpToken decodes a 201 [stepup, expires].
func stepUpToken(t *testing.T, res *httptest.ResponseRecorder) string {
	t.Helper()
	if res.Code != http.StatusCreated {
		t.Fatalf("step-up answered %d (%x), want 201", res.Code, res.Body.Bytes())
	}
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode step-up: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("step-up returned %d elements, want [stepup, expires]", len(out))
	}
	tok, ok := out[0].(string)
	if !ok || tok == "" {
		t.Fatalf("stepup = %#v, want a text string", out[0])
	}
	if exp, ok := out[1].(uint64); !ok || exp == 0 {
		t.Fatalf("expires = %#v, want a uint", out[1])
	}
	return tok
}

// getAuth sends an authenticated GET.
func getAuth(h http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// getFactors reads GET /v1/auth/factors.
func getFactors(t *testing.T, h http.Handler, token string) []any {
	t.Helper()
	res := getAuth(h, "/v1/auth/factors", token)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /v1/auth/factors answered %d (%x)", res.Code, res.Body.Bytes())
	}
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode factors: %v", err)
	}
	if len(out) != 6 {
		t.Fatalf("factors returned %d elements, want 6", len(out))
	}
	return out
}

// removeFactor posts POST /v1/auth/factors/remove [stepup, kind, cred_id].
func removeFactor(h http.Handler, token, stepup string, kind uint64, credID []byte) *httptest.ResponseRecorder {
	var cred any
	if credID != nil {
		cred = credID
	}
	body, err := cborx.Marshal([]any{stepup, kind, cred})
	if err != nil {
		panic("marshal remove body: " + err.Error())
	}
	return postCBORAuth(h, "/v1/auth/factors/remove", body, token)
}

// newCodes posts POST /v1/auth/recovery/codes [stepup].
func newCodes(h http.Handler, token, stepup string) *httptest.ResponseRecorder {
	body, err := cborx.Marshal([]any{stepup})
	if err != nil {
		panic("marshal codes body: " + err.Error())
	}
	return postCBORAuth(h, "/v1/auth/recovery/codes", body, token)
}

// codesOf decodes [backup_codes([tstr])].
func codesOf(t *testing.T, res *httptest.ResponseRecorder) []string {
	t.Helper()
	if res.Code != http.StatusOK {
		t.Fatalf("answered %d (%x), want 200", res.Code, res.Body.Bytes())
	}
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode codes: %v", err)
	}
	raw, ok := out[0].([]any)
	if len(out) != 1 || !ok {
		t.Fatalf("codes body = %#v, want [backup_codes]", out)
	}
	codes := make([]string, 0, len(raw))
	for _, c := range raw {
		codes = append(codes, c.(string))
	}
	return codes
}

// sessionFor establishes one more enrolled session for an existing account, on
// a device of its own.
func secondSession(t *testing.T, d api.Deps, u store.UserRow) string {
	t.Helper()
	ctx := context.Background()
	now := d.Clock.Now().Unix()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	verified := now
	dev := store.DeviceRow{
		ID: id.New(), UserID: u.ID, DSKPub: pub, CredentialBlob: []byte{1},
		VerifiedAt: &verified, LastSeen: now, Created: now,
	}
	if err := d.Repo.CreateDevice(ctx, dev); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
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
	return tok.Token
}

// owe marks the account as owing a second factor, the state every sign-up starts in.
func owe(t *testing.T, d api.Deps, uid id.ID) {
	t.Helper()
	if err := d.Repo.SetFactorOwed(context.Background(), uid, true); err != nil {
		t.Fatalf("SetFactorOwed: %v", err)
	}
}

// needsOf reads a password login's `needs`.
func needsOf(t *testing.T, h http.Handler, username, pw string) (string, uint64) {
	t.Helper()
	status, out := login(t, h, username, pw)
	if status != http.StatusOK || len(out) != 2 {
		t.Fatalf("login = %d %#v, want 200 [assertion, needs]", status, out)
	}
	needs, ok := out[1].(uint64)
	if !ok {
		t.Fatalf("needs = %#v, want a uint", out[1])
	}
	return out[0].(string), needs
}

// The login matrix: what each kind of account still owes after its password.
func TestAPasswordLoginStatesWhatTheAccountOwes(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	const pw = "correct horse battery staple"

	owed, _, _ := seedAPISession(t, deps)
	owe(t, deps, owed.ID)
	seedPassword(t, deps, owed, pw)
	if _, needs := needsOf(t, h, owed.Username, pw); needs != 0 {
		t.Fatalf("an owed account: needs = %d, want 0", needs)
	}

	withTOTP, _, totpToken := seedAPISession(t, deps)
	seedPassword(t, deps, withTOTP, pw)
	enrolTOTP(t, h, deps, totpToken)
	if _, needs := needsOf(t, h, withTOTP.Username, pw); needs != 1 {
		t.Fatalf("a TOTP account: needs = %d, want 1", needs)
	}

	passkeyOnly, _, pkToken := seedAPISession(t, deps)
	seedPassword(t, deps, passkeyOnly, pw)
	enrolPasskey(t, h, pkToken, newVirtualAuthenticator(t, deps.Domain, true))
	if _, needs := needsOf(t, h, passkeyOnly.Username, pw); needs != 2 {
		t.Fatalf("a passkey-only account: needs = %d, want 2", needs)
	}
}

// A `needs 2` assertion is not one /v1/auth/totp/verify accepts, even once the
// account has a confirmed TOTP and the code is right; a backup code clears it.
func TestANeedsTwoAssertionTakesABackupCodeAndNotATOTPCode(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	_, codes := enrolPasskeyWith(t, h, token, newVirtualAuthenticator(t, deps.Domain, true), nil)
	if len(codes) != 10 {
		t.Fatalf("the first factor minted %d backup codes, want 10", len(codes))
	}
	refused, needs := needsOf(t, h, u.Username, pw)
	control, needs2 := needsOf(t, h, u.Username, pw)
	if needs != 2 || needs2 != 2 {
		t.Fatalf("needs = %d, %d; want 2", needs, needs2)
	}

	// The control: a backup code clears a needs-2 assertion, and the assertion
	// it hands back establishes (owes nothing).
	body, _ := cborx.Marshal([]any{control, codes[0].(string)})
	res := postCBOR(h, "/v1/auth/recovery/verify", body)
	if res.Code != http.StatusOK {
		t.Fatalf("recovery/verify on a needs-2 assertion = %d, want 200", res.Code)
	}
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, owes := spendAssertion(t, deps, out[0].(string)); got != u.ID || owes {
		t.Fatalf("the cleared assertion = %s owing %v, want %s owing nothing", got, owes, u.ID)
	}

	// The account confirms a TOTP after the login; its right code still does
	// not clear the needs-2 assertion.
	secret, _ := enrolTOTPWith(t, h, deps, token, stepUp(t, h, token, stepUpPassword, pw))
	code, err := totp.GenerateCode(secret, deps.Clock.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	body, _ = cborx.Marshal([]any{refused, code})
	if res := postCBOR(h, "/v1/auth/totp/verify", body); res.Code != http.StatusUnauthorized {
		t.Fatalf("totp/verify on a needs-2 assertion = %d, want 401", res.Code)
	}
}

// An account holding a factor adds another only behind a step-up.
func TestEnrollingWhileAFactorExistsNeedsAStepUp(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	enrolPasskey(t, h, token, newVirtualAuthenticator(t, deps.Domain, true))

	body, _ := cborx.Marshal([]any{nil})
	res := postCBORAuth(h, "/v1/auth/totp/enroll", body, token)
	if res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
		t.Fatalf("enroll without a step-up = %d, want 403 E_STEP_UP", res.Code)
	}
	body, _ = cborx.Marshal([]any{stepUp(t, h, token, stepUpPassword, pw)})
	if res := postCBORAuth(h, "/v1/auth/totp/enroll", body, token); res.Code != http.StatusOK {
		t.Fatalf("enroll with a valid step-up = %d (%x), want 200", res.Code, res.Body.Bytes())
	}
}

// A step-up token is single-use.
func TestAStepUpTokenIsSpentOnce(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	enrolTOTP(t, h, deps, token)
	tok := stepUp(t, h, token, stepUpPassword, pw)
	codesOf(t, newCodes(h, token, tok))
	res := newCodes(h, token, tok)
	if res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
		t.Fatalf("the same token twice = %d, want 403 E_STEP_UP", res.Code)
	}
}

// A step-up token is bound to the session that minted it.
func TestAStepUpTokenIsBoundToItsSession(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	other := secondSession(t, deps, u)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	enrolTOTP(t, h, deps, token)
	tok := stepUp(t, h, token, stepUpPassword, pw)

	res := newCodes(h, other, tok)
	if res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
		t.Fatalf("another session's token = %d, want 403 E_STEP_UP", res.Code)
	}
	if got := codesOf(t, newCodes(h, token, tok)); len(got) != 10 {
		t.Fatalf("the minting session's token made %d codes, want 10", len(got))
	}
}

// The first factor mints ten backup codes and clears the debt; a second
// factor's confirm mints none, so printed codes keep working.
func TestTheFirstFactorMintsBackupCodesAndClearsTheDebt(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	u, _, token := seedAPISession(t, deps)
	owe(t, deps, u.ID)
	pw := seedPassword(t, deps, u, "correct horse battery staple")

	_, codes := enrolTOTP(t, h, deps, token)
	if len(codes) != 10 {
		t.Fatalf("the first confirm minted %d codes, want 10", len(codes))
	}
	if row, err := deps.Repo.GetUser(t.Context(), u.ID); err != nil || row.FactorOwed {
		t.Fatalf("after the first factor: %+v, %v; want factor_owed cleared", row, err)
	}

	_, second := enrolPasskeyWith(t, h, token, newVirtualAuthenticator(t, deps.Domain, true),
		stepUp(t, h, token, stepUpPassword, pw))
	if len(second) != 0 {
		t.Fatalf("a second factor's finish minted %d codes, want []", len(second))
	}
	if n, err := deps.Repo.CountRecoveryCodes(t.Context(), u.ID); err != nil || n != 10 {
		t.Fatalf("unused codes = %d, %v; the first set must survive", n, err)
	}
}

// The begin/confirm window: an owed account enrolls without a step-up (it holds
// nothing), another device confirms a passkey in between, and the confirm is
// refused under the lock with nothing written.
func TestConfirmRechecksTheStepUpUnderTheLock(t *testing.T) {
	h, deps, hook := newPasskeyAPIHooked(t)

	confirm := func(token, secret string) *httptest.ResponseRecorder {
		code, err := totp.GenerateCode(secret, deps.Clock.Now())
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}
		body, _ := cborx.Marshal([]any{code})
		return postCBORAuth(h, "/v1/auth/totp/confirm", body, token)
	}
	enroll := func(token string) string {
		body, _ := cborx.Marshal([]any{nil})
		res := postCBORAuth(h, "/v1/auth/totp/enroll", body, token)
		if res.Code != http.StatusOK {
			t.Fatalf("enroll = %d", res.Code)
		}
		var out []any
		if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out[0].(string)
	}

	u, _, token := seedAPISession(t, deps)
	owe(t, deps, u.ID)
	secret := enroll(token)
	hook.arm(func() {
		if err := deps.Repo.PutWebauthnCredential(context.Background(), store.WebauthnCredentialRow{
			CredID: bytes.Repeat([]byte{0x51}, 32), RPID: deps.Domain, UserID: u.ID, PublicKey: []byte{1},
			Flags: []byte{1}, Name: "elsewhere", Created: deps.Clock.Now().Unix(),
		}); err != nil {
			t.Errorf("PutWebauthnCredential: %v", err)
		}
	})
	res := confirm(token, secret)
	if res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
		t.Fatalf("confirm after a factor appeared = %d, want 403 E_STEP_UP", res.Code)
	}
	row, err := deps.Repo.GetTOTP(t.Context(), u.ID)
	if err != nil || row.ConfirmedAt != nil {
		t.Fatalf("the TOTP row = %+v, %v; want it still unconfirmed", row, err)
	}
	if n, _ := deps.Repo.CountRecoveryCodes(t.Context(), u.ID); n != 0 {
		t.Fatalf("the refused confirm minted %d codes", n)
	}
	if got, _ := deps.Repo.GetUser(t.Context(), u.ID); !got.FactorOwed {
		t.Fatal("the refused confirm cleared factor_owed")
	}

	// The control: the same steps without the hook.
	v, _, vToken := seedAPISession(t, deps)
	owe(t, deps, v.ID)
	if res := confirm(vToken, enroll(vToken)); res.Code != http.StatusOK {
		t.Fatalf("the undisturbed confirm = %d, want 200", res.Code)
	}
}

// The same window on the passkey leg: a confirmed TOTP appears between begin
// and finish, and the finish stores nothing.
func TestPasskeyFinishRechecksTheStepUpUnderTheLock(t *testing.T) {
	h, deps, hook := newPasskeyAPIHooked(t)
	finish := func(token string, device *virtualAuthenticator, arm func()) *httptest.ResponseRecorder {
		begin, _ := cborx.Marshal([]any{nil})
		res := postCBORAuth(h, "/v1/auth/passkey/register/begin", begin, token)
		if res.Code != http.StatusOK {
			t.Fatalf("register/begin = %d", res.Code)
		}
		ceremony, options := ceremonyAndOptions(t, res.Body.Bytes())
		body, _ := cborx.Marshal([]any{ceremony, device.register(t, options), "Laptop"})
		if arm != nil {
			hook.arm(arm)
		}
		return postCBORAuth(h, "/v1/auth/passkey/register/finish", body, token)
	}

	u, _, token := seedAPISession(t, deps)
	owe(t, deps, u.ID)
	device := newVirtualAuthenticator(t, deps.Domain, true)
	res := finish(token, device, func() {
		confirmed := deps.Clock.Now().Unix()
		if err := deps.Repo.PutTOTP(context.Background(), store.TOTPRow{UserID: u.ID,
			Secret: []byte("JBSWY3DPEHPK3PXP"), Digits: 6, Period: 30, Algorithm: "SHA1",
			ConfirmedAt: &confirmed, Created: confirmed}); err != nil {
			t.Errorf("PutTOTP: %v", err)
		}
	})
	if res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
		t.Fatalf("finish after a factor appeared = %d, want 403 E_STEP_UP", res.Code)
	}
	if _, err := deps.Repo.GetWebauthnCredential(t.Context(), device.credID); err == nil {
		t.Fatal("the refused finish stored the credential")
	}

	v, _, vToken := seedAPISession(t, deps)
	owe(t, deps, v.ID)
	if res := finish(vToken, newVirtualAuthenticator(t, deps.Domain, true), nil); res.Code != http.StatusOK {
		t.Fatalf("the undisturbed finish = %d (%x), want 200", res.Code, res.Body.Bytes())
	}
}

// The last factor cannot be removed; one of two can.
func TestRemovingTheLastFactorIsRefused(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	enrolTOTP(t, h, deps, token)
	res := removeFactor(h, token, stepUp(t, h, token, stepUpPassword, pw), 0, nil)
	if res.Code != http.StatusConflict || errorCode(t, res) != "E_INVALID_REQUEST" {
		t.Fatalf("removing the only factor = %d, want 409 E_INVALID_REQUEST", res.Code)
	}

	credID, _ := enrolPasskeyWith(t, h, token, newVirtualAuthenticator(t, deps.Domain, true),
		stepUp(t, h, token, stepUpPassword, pw))
	if res := removeFactor(h, token, stepUp(t, h, token, stepUpPassword, pw), 1, credID); res.Code != http.StatusNoContent {
		t.Fatalf("removing one of two = %d (%x), want 204", res.Code, res.Body.Bytes())
	}
	f := getFactors(t, h, token)
	if f[1] == nil || len(f[2].([]any)) != 0 {
		t.Fatalf("factors after the remove = %#v, want the TOTP and no passkey", f)
	}
}

// Two devices removing the two last factors at once: the second remove commits
// between the first's checks and its transaction, and the first is refused
// under the user lock, leaving exactly one factor.
func TestTwoRemovesCannotTakeTheLastTwoFactors(t *testing.T) {
	h, deps, hook := newPasskeyAPIHooked(t)
	setup := func() (store.UserRow, string, []byte, string) {
		u, _, token := seedAPISession(t, deps)
		pw := seedPassword(t, deps, u, "correct horse battery staple")
		enrolTOTP(t, h, deps, token)
		credID, _ := enrolPasskeyWith(t, h, token, newVirtualAuthenticator(t, deps.Domain, true),
			stepUp(t, h, token, stepUpPassword, pw))
		return u, token, credID, pw
	}

	u, token, credID, pw := setup()
	first, second := stepUp(t, h, token, stepUpPassword, pw), stepUp(t, h, token, stepUpPassword, pw)
	hook.arm(func() {
		if res := removeFactor(h, token, second, 1, credID); res.Code != http.StatusNoContent {
			t.Errorf("the concurrent remove = %d, want 204", res.Code)
		}
	})
	res := removeFactor(h, token, first, 0, nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("the first remove = %d, want 409", res.Code)
	}
	if ok, err := deps.Repo.GetTOTP(t.Context(), u.ID); err != nil || ok.ConfirmedAt == nil {
		t.Fatalf("the TOTP = %+v, %v; want it kept", ok, err)
	}
	if creds, _ := deps.Repo.ListWebauthnCredentials(t.Context(), u.ID, deps.Domain); len(creds) != 0 {
		t.Fatalf("%d passkeys left, want 0: exactly one factor remains", len(creds))
	}

	_, vToken, _, vpw := setup()
	if res := removeFactor(h, vToken, stepUp(t, h, vToken, stepUpPassword, vpw), 0, nil); res.Code != http.StatusNoContent {
		t.Fatalf("the undisturbed remove = %d, want 204", res.Code)
	}
}

// A regenerate retires every earlier code.
func TestRegeneratingBackupCodesRetiresTheOldOnes(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	_, old := enrolTOTP(t, h, deps, token)
	fresh := codesOf(t, newCodes(h, token, stepUp(t, h, token, stepUpPassword, pw)))
	if len(fresh) != 10 {
		t.Fatalf("regenerate made %d codes, want 10", len(fresh))
	}
	verify := func(code string) int {
		assertion, _ := needsOf(t, h, u.Username, pw)
		body, _ := cborx.Marshal([]any{assertion, code})
		return postCBOR(h, "/v1/auth/recovery/verify", body).Code
	}
	if got := verify(old[0]); got != http.StatusUnauthorized {
		t.Fatalf("an old code after the regenerate = %d, want 401", got)
	}
	if got := verify(fresh[0]); got != http.StatusOK {
		t.Fatalf("a new code = %d, want 200", got)
	}
}

// postFrom is postCBORAuth from a given client address.
func postLoginFrom(h http.Handler, path string, body []byte, addr string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/cbor")
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Step-up failures lock the step-up ledger, never the account's password
// ledger: a thief guessing the password behind a stolen session cannot lock
// the owner out of signing in.
func TestStepUpFailuresLockOnlyTheStepUpLedger(t *testing.T) {
	// The address failure budget out of the way: what is under test is the step-up ledger
	// (TestAStepUpRespectsTheAddressFailureBudget covers the budget).
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) { c.Limits.Rate.LoginFailedBurst = 100 })
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	free := deps.Config.Auth.Lockout.FreeAttempts
	for i := 1; i <= free+1; i++ {
		res := stepUpRaw(h, token, stepUpPassword, "not the password")
		want, code := http.StatusForbidden, "E_STEP_UP"
		if i == free+1 {
			want, code = http.StatusTooManyRequests, "E_RATE_LIMITED"
		}
		if res.Code != want || errorCode(t, res) != code {
			t.Fatalf("wrong step-up %d = %d %s, want %d %s", i, res.Code, errorCode(t, res), want, code)
		}
	}
	if locked := deps.Throttle.LockedFor(u.ID); locked != 0 {
		t.Fatalf("the account ledger is locked for %s after step-up failures", locked)
	}
	body, _ := cborx.Marshal([]any{u.Username, pw})
	if res := postLoginFrom(h, "/v1/auth/password/login", body, "198.51.100.7:4000"); res.Code != http.StatusOK {
		t.Fatalf("a password login after the step-up lockout = %d, want 200", res.Code)
	}
}

// A step-up by TOTP consumes the counter.
func TestAStepUpTOTPCodeIsSpentOnce(t *testing.T) {
	h, deps := newTestAPI(t)
	_, _, token := seedAPISession(t, deps)
	secret, _ := enrolTOTP(t, h, deps, token)
	code, _ := totp.GenerateCode(secret, deps.Clock.Now())
	stepUp(t, h, token, stepUpTOTP, code)
	res := stepUpRaw(h, token, stepUpTOTP, code)
	if res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
		t.Fatalf("the same code twice = %d, want 403 E_STEP_UP", res.Code)
	}
	advance(t, deps, 30*time.Second)
	next, _ := totp.GenerateCode(secret, deps.Clock.Now())
	stepUp(t, h, token, stepUpTOTP, next)
}

// A step-up by backup code spends it.
func TestAStepUpBackupCodeIsSpentOnce(t *testing.T) {
	h, deps := newTestAPI(t)
	_, _, token := seedAPISession(t, deps)
	_, codes := enrolTOTP(t, h, deps, token)
	stepUp(t, h, token, stepUpBackup, codes[0])
	if res := stepUpRaw(h, token, stepUpBackup, codes[0]); res.Code != http.StatusForbidden {
		t.Fatalf("a spent backup code = %d, want 403", res.Code)
	}
	stepUp(t, h, token, stepUpBackup, codes[1])
}

// GET /v1/auth/factors describes the account.
func TestTheFactorsListDescribesTheAccount(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	u, _, token := seedAPISession(t, deps)
	owe(t, deps, u.ID)
	f := getFactors(t, h, token)
	if f[0] != uint64(1) || f[1] != nil || len(f[2].([]any)) != 0 || f[3] != uint64(0) || f[4] != uint64(0) {
		t.Fatalf("an owed account's factors = %#v, want [1, null, [], 0, 0]", f)
	}

	pw := seedPassword(t, deps, u, "correct horse battery staple")
	enrolTOTP(t, h, deps, token)
	credID, _ := enrolPasskeyWith(t, h, token, newVirtualAuthenticator(t, deps.Domain, true),
		stepUp(t, h, token, stepUpPassword, pw))
	f = getFactors(t, h, token)
	if f[0] != uint64(0) || f[1] == nil || f[3] != uint64(10) || f[4] != uint64(1) {
		t.Fatalf("factors = %#v, want [0, confirmed_at, [one], 10, 1]", f)
	}
	keys := f[2].([]any)
	if len(keys) != 1 {
		t.Fatalf("passkeys = %#v, want one", keys)
	}
	k := keys[0].([]any)
	if !bytes.Equal(k[0].([]byte), credID) || k[1] != "My passkey" || k[2] == nil || k[3] != nil {
		t.Fatalf("passkey row = %#v, want [cred_id, name, created, null]", k)
	}
}

// web3.6 review I2: an account marked as owing a factor that holds a usable one (an rp_id changed
// and put back, before the restart that reconciles it) is settled by reading its factors: the
// list answers owed 0 and the mark is gone, so the forced set-up finishes without the step-up
// the owed account could not get. One that holds none stays owed.
func TestReadingTheFactorsOfAnOwedHolderSettlesIt(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	u, _, token := seedAPISession(t, deps)
	enrolTOTP(t, h, deps, token)
	owe(t, deps, u.ID)
	if f := getFactors(t, h, token); f[0] != uint64(0) || f[1] == nil {
		t.Fatalf("an owed account holding a confirmed TOTP lists %#v, want owed 0 and the TOTP", f)
	}
	if got, err := deps.Repo.GetUser(context.Background(), u.ID); err != nil || got.FactorOwed {
		t.Fatalf("after the read, factor_owed = %v (%v), want false", got.FactorOwed, err)
	}
	if res := getAuth(h, "/v1/devices", token); res.Code != http.StatusOK {
		t.Fatalf("GET /v1/devices after the read = %d (%x), want 200", res.Code, res.Body.Bytes())
	}

	bare, _, bareToken := seedAPISession(t, deps)
	owe(t, deps, bare.ID)
	if f := getFactors(t, h, bareToken); f[0] != uint64(1) {
		t.Fatalf("an owed account with no factor lists owed %#v, want 1", f[0])
	}
}

// Under instance.discovery = "session" the discovery document is `E, F`: a
// client setting up a factor reads auth_methods.
func TestAnOwedSessionReadsTheInstanceDocumentUnderSessionDiscovery(t *testing.T) {
	h, deps := newTestAPIFull(t, func(c *config.Config) { c.Instance.Discovery = "session" },
		func(d *api.Deps) { d.ExternalSenderPub = make([]byte, 32) })
	u, _, token := seedAPISession(t, deps)
	owe(t, deps, u.ID)
	if res := getAuth(h, "/v1/instance", token); res.Code != http.StatusOK {
		t.Fatalf("GET /v1/instance as an owed session = %d, want 200", res.Code)
	}
	// The control: the session really is owed.
	if res := getAuth(h, "/v1/devices", token); res.Code != http.StatusForbidden || errorCode(t, res) != "E_FACTOR_REQUIRED" {
		t.Fatalf("GET /v1/devices as an owed session = %d, want 403 E_FACTOR_REQUIRED", res.Code)
	}
}

// --- web3.6 S2 review fix round 1 ---

// stepUpFrom posts POST /v1/auth/stepup from a given client address.
func stepUpFrom(h http.Handler, token string, method uint64, secret, addr string) *httptest.ResponseRecorder {
	body, err := cborx.Marshal([]any{method, secret})
	if err != nil {
		panic("marshal step-up body: " + err.Error())
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/auth/stepup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/cbor")
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// I1 (a): a step-up token lives StepUpTTL. The control is a token spent inside
// it; the two differ only in how long they waited.
func TestAStepUpTokenExpires(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	enrolTOTP(t, h, deps, token)

	late := stepUp(t, h, token, stepUpPassword, pw)
	advance(t, deps, api.StepUpTTL+time.Second)
	res := newCodes(h, token, late)
	if res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
		t.Fatalf("a token spent %s after it was minted = %d, want 403 E_STEP_UP", api.StepUpTTL+time.Second, res.Code)
	}

	inTime := stepUp(t, h, token, stepUpPassword, pw)
	advance(t, deps, api.StepUpTTL-time.Second)
	if got := codesOf(t, newCodes(h, token, inTime)); len(got) != 10 {
		t.Fatalf("a token spent inside its lifetime made %d codes, want 10", len(got))
	}
}

// I1 (c): a confirm over an already-confirmed row is 404 and mints nothing; the
// control is the first confirm (enrolTOTP), which answered 200 with ten codes.
func TestConfirmingAConfirmedAuthenticatorIsNotFound(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	secret, codes := enrolTOTP(t, h, deps, token)
	// Spend one code, so a re-mint would show as a count of 10 again.
	_ = stepUp(t, h, token, stepUpBackup, codes[0])

	code, err := totp.GenerateCode(secret, deps.Clock.Now())
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	confirm, _ := cborx.Marshal([]any{code})
	res := postCBORAuth(h, "/v1/auth/totp/confirm", confirm, token)
	if res.Code != http.StatusNotFound || errorCode(t, res) != "E_NOT_FOUND" {
		t.Fatalf("a confirm over a confirmed authenticator = %d, want 404 E_NOT_FOUND", res.Code)
	}
	if n, err := deps.Repo.CountRecoveryCodes(t.Context(), u.ID); err != nil || n != 9 {
		t.Fatalf("unused codes after the refused confirm = %d, %v; want 9 (nothing minted)", n, err)
	}
}

// I1 (e): removing a passkey the caller does not hold is 404 and leaves it in
// place; the control removes the caller's own.
func TestRemovingAnotherAccountsPasskeyIsNotFound(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	a, _, aToken := seedAPISession(t, deps)
	pw := seedPassword(t, deps, a, "correct horse battery staple")
	enrolTOTP(t, h, deps, aToken)
	own, _ := enrolPasskeyWith(t, h, aToken, newVirtualAuthenticator(t, deps.Domain, true),
		stepUp(t, h, aToken, stepUpPassword, pw))
	b, _, bToken := seedAPISession(t, deps)
	theirs := enrolPasskey(t, h, bToken, newVirtualAuthenticator(t, deps.Domain, true))

	res := removeFactor(h, aToken, stepUp(t, h, aToken, stepUpPassword, pw), 1, theirs)
	if res.Code != http.StatusNotFound || errorCode(t, res) != "E_NOT_FOUND" {
		t.Fatalf("removing another account's passkey = %d, want 404 E_NOT_FOUND", res.Code)
	}
	if row, err := deps.Repo.GetWebauthnCredential(t.Context(), theirs); err != nil || row.UserID != b.ID {
		t.Fatalf("the other account's passkey = %+v, %v; want it untouched", row, err)
	}
	if res := removeFactor(h, aToken, stepUp(t, h, aToken, stepUpPassword, pw), 1, own); res.Code != http.StatusNoContent {
		t.Fatalf("removing the caller's own passkey = %d, want 204", res.Code)
	}
}

// M1: the TOTP enrolment record is bound to the secret its enroll wrote. A
// racing second enrolment that replaced the secret between this enroll and its
// confirm (the hook) does not inherit this enroll's step-up: the confirm
// re-checks, finds a factor, and writes nothing. The control confirms the
// secret this enroll wrote.
func TestAConfirmInheritsAStepUpOnlyForItsOwnSecret(t *testing.T) {
	h, deps, hook := newPasskeyAPIHooked(t)
	const other = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	setup := func() (store.UserRow, string, string) {
		u, _, token := seedAPISession(t, deps)
		pw := seedPassword(t, deps, u, "correct horse battery staple")
		enrolPasskey(t, h, token, newVirtualAuthenticator(t, deps.Domain, true))
		body, _ := cborx.Marshal([]any{stepUp(t, h, token, stepUpPassword, pw)})
		res := postCBORAuth(h, "/v1/auth/totp/enroll", body, token)
		if res.Code != http.StatusOK {
			t.Fatalf("a stepped-up enroll = %d", res.Code)
		}
		var out []any
		if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return u, token, out[0].(string)
	}
	confirm := func(token, secret string) *httptest.ResponseRecorder {
		code, err := totp.GenerateCode(secret, deps.Clock.Now())
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}
		body, _ := cborx.Marshal([]any{code})
		return postCBORAuth(h, "/v1/auth/totp/confirm", body, token)
	}

	u, token, _ := setup()
	hook.arm(func() {
		if err := deps.Repo.PutTOTP(context.Background(), store.TOTPRow{UserID: u.ID, Secret: []byte(other),
			Digits: 6, Period: 30, Algorithm: "SHA1", Created: deps.Clock.Now().Unix()}); err != nil {
			t.Errorf("PutTOTP: %v", err)
		}
	})
	res := confirm(token, other)
	if res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
		t.Fatalf("confirming a secret another enrolment wrote = %d, want 403 E_STEP_UP", res.Code)
	}
	if row, err := deps.Repo.GetTOTP(t.Context(), u.ID); err != nil || row.ConfirmedAt != nil {
		t.Fatalf("the TOTP row = %+v, %v; want it unconfirmed", row, err)
	}

	_, vToken, vSecret := setup()
	if res := confirm(vToken, vSecret); res.Code != http.StatusOK {
		t.Fatalf("confirming the stepped-up enroll's own secret = %d, want 200", res.Code)
	}
}

// M2: the step-up ledger is per account and per client address, so the owner
// stepping up from elsewhere does not hand a thief a fresh set of free
// attempts. The control is the owner on the thief's own address, whose success
// does clear that address's count.
func TestAnOwnersStepUpElsewhereDoesNotResetAThiefsCount(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) { c.Limits.Rate.LoginFailedBurst = 100 })
	const thiefAddr, ownerAddr = "203.0.113.9:4000", "198.51.100.7:4000"
	free := deps.Config.Auth.Lockout.FreeAttempts
	// run: free-1 thief failures, the owner's success from ownerFrom, then `more` thief failures;
	// it answers the last one.
	run := func(ownerFrom string, more int) int {
		u, _, thief := seedAPISession(t, deps)
		owner := secondSession(t, deps, u)
		pw := seedPassword(t, deps, u, "correct horse battery staple")
		for i := 1; i < free; i++ {
			if res := stepUpFrom(h, thief, stepUpPassword, "not it", thiefAddr); res.Code != http.StatusForbidden {
				t.Fatalf("thief failure %d = %d, want 403", i, res.Code)
			}
		}
		stepUpToken(t, stepUpFrom(h, owner, stepUpPassword, pw, ownerFrom))
		code := 0
		for range more {
			code = stepUpFrom(h, thief, stepUpPassword, "not it", thiefAddr).Code
		}
		return code
	}
	if got := run(ownerAddr, 2); got != http.StatusTooManyRequests {
		t.Fatalf("the thief's failure past free_attempts after the owner stepped up elsewhere = %d, want 429", got)
	}
	// The owner's success on the thief's own address resets nothing account-wide either.
	if got := run(thiefAddr, 2); got != http.StatusTooManyRequests {
		t.Fatalf("the thief's failure past free_attempts after the owner stepped up on its address = %d, want 429", got)
	}
	// The control: one failure fewer is still inside free_attempts.
	if got := run(ownerAddr, 1); got != http.StatusForbidden {
		t.Fatalf("the control (free_attempts failures) = %d, want 403", got)
	}
}

// M2: a step-up respects the address failure budget the password login peeks:
// an address that spent it on failed logins is refused 429 even with the right
// password; the control steps up from another address.
func TestAStepUpRespectsTheAddressFailureBudget(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) { c.Limits.Rate.LoginBurst = 100 })
	const spent, fresh = "203.0.113.9:4000", "198.51.100.7:4000"
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	for i := 0; i < deps.Config.Limits.Rate.LoginFailedBurst; i++ {
		other, _, _ := seedAPISession(t, deps)
		seedPassword(t, deps, other, "correct horse battery staple")
		body, _ := cborx.Marshal([]any{other.Username, "not the password"})
		if res := postLoginFrom(h, "/v1/auth/password/login", body, spent); res.Code != http.StatusUnauthorized {
			t.Fatalf("failed login %d = %d, want 401", i+1, res.Code)
		}
	}
	if res := stepUpFrom(h, token, stepUpPassword, pw, spent); res.Code != http.StatusTooManyRequests {
		t.Fatalf("a step-up from an address with no failure budget = %d, want 429", res.Code)
	}
	stepUpToken(t, stepUpFrom(h, token, stepUpPassword, pw, fresh))
}

// Wire 7: an account holds at most 1024 passkeys (the client's decode bound).
// register/begin at the cap is 409 E_INVALID_REQUEST; the control is one below.
func TestAnAccountHoldsAtMost1024Passkeys(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	seedCredentials(t, deps, u.ID, 1024)
	begin := func() *httptest.ResponseRecorder {
		body, _ := cborx.Marshal([]any{stepUp(t, h, token, stepUpPassword, pw)})
		return postCBORAuth(h, "/v1/auth/passkey/register/begin", body, token)
	}
	res := begin()
	if res.Code != http.StatusConflict || errorCode(t, res) != "E_INVALID_REQUEST" {
		t.Fatalf("register/begin at 1024 passkeys = %d, want 409 E_INVALID_REQUEST", res.Code)
	}
	if n, err := deps.Repo.DeleteWebauthnCredential(t.Context(), u.ID, credIDFor(u.ID, 0)); err != nil || n != 1 {
		t.Fatalf("DeleteWebauthnCredential = %d, %v", n, err)
	}
	if res := begin(); res.Code != http.StatusOK {
		t.Fatalf("register/begin at 1023 passkeys = %d, want 200", res.Code)
	}
}

// Wire 7, under the lock: a passkey that reached the cap between begin and
// finish (the hook) refuses the finish with 409; the control finishes at 1023.
func TestThePasskeyCapIsRecheckedUnderTheLock(t *testing.T) {
	h, deps, hook := newPasskeyAPIHooked(t)
	finish := func(arm func(store.UserRow)) (store.UserRow, *httptest.ResponseRecorder) {
		u, _, token := seedAPISession(t, deps)
		pw := seedPassword(t, deps, u, "correct horse battery staple")
		seedCredentials(t, deps, u.ID, 1023)
		body, _ := cborx.Marshal([]any{stepUp(t, h, token, stepUpPassword, pw)})
		res := postCBORAuth(h, "/v1/auth/passkey/register/begin", body, token)
		if res.Code != http.StatusOK {
			t.Fatalf("register/begin = %d", res.Code)
		}
		ceremony, options := ceremonyAndOptions(t, res.Body.Bytes())
		device := newVirtualAuthenticator(t, deps.Domain, true)
		body, _ = cborx.Marshal([]any{ceremony, device.register(t, options), "One more"})
		if arm != nil {
			hook.arm(func() { arm(u) })
		}
		return u, postCBORAuth(h, "/v1/auth/passkey/register/finish", body, token)
	}
	_, res := finish(func(u store.UserRow) {
		seedCredentialAt(t, deps, u.ID, 5000)
	})
	if res.Code != http.StatusConflict || errorCode(t, res) != "E_INVALID_REQUEST" {
		t.Fatalf("finish after the account reached 1024 passkeys = %d, want 409 E_INVALID_REQUEST", res.Code)
	}
	if _, res := finish(nil); res.Code != http.StatusOK {
		t.Fatalf("the undisturbed finish at 1023 = %d, want 200", res.Code)
	}
}

// credIDFor is the account's i-th seeded credential id.
func credIDFor(uid id.ID, i int) []byte {
	return append([]byte{0xc0, byte(i >> 8), byte(i)}, uid[:]...)
}

func seedCredentialAt(t *testing.T, d api.Deps, uid id.ID, i int) {
	t.Helper()
	cred := credIDFor(uid, i)
	if err := d.Repo.PutWebauthnCredential(context.Background(), store.WebauthnCredentialRow{
		CredID: cred, RPID: d.Domain, UserID: uid, PublicKey: []byte{1}, Flags: []byte{1},
		Name: "seeded", Created: d.Clock.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutWebauthnCredential %d: %v", i, err)
	}
}

// seedCredentials gives the account n passkey rows under the instance's rp_id.
func seedCredentials(t *testing.T, d api.Deps, uid id.ID, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		seedCredentialAt(t, d, uid, i)
	}
}

// The throttle fix (item 1): step-up failures are capped per account, whatever the address. From
// several addresses, free_attempts wrong TOTP codes each; the CORRECT code from a fresh address
// is then refused 429 before it is checked. The control is the correct code on a fresh account.
func TestStepUpTOTPGuessesAreCappedPerAccountAcrossAddresses(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) { c.Limits.Rate.LoginFailedBurst = 1000 })
	free := deps.Config.Auth.Lockout.FreeAttempts
	_, _, token := seedAPISession(t, deps)
	secret, _ := enrolTOTP(t, h, deps, token)
	for a := 1; a <= 3; a++ {
		for range free {
			stepUpFrom(h, token, stepUpTOTP, "000000", fmt.Sprintf("203.0.113.%d:4000", a))
		}
	}
	right, _ := totp.GenerateCode(secret, deps.Clock.Now())
	if res := stepUpFrom(h, token, stepUpTOTP, right, "198.51.100.200:4000"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("the correct code after %d wrong ones over 3 addresses = %d, want 429", 3*free, res.Code)
	}

	_, _, fresh := seedAPISession(t, deps)
	fSecret, _ := enrolTOTP(t, h, deps, fresh)
	fRight, _ := totp.GenerateCode(fSecret, deps.Clock.Now())
	stepUpToken(t, stepUpFrom(h, fresh, stepUpTOTP, fRight, "198.51.100.201:4000"))
}

// The throttle fix (item 2): the old password is POST /v1/auth/password's step-up, and guessing
// it behind a stolen session is capped like any step-up. Past free_attempts the RIGHT old password
// is refused 429 and the password is unchanged; the guesses never lock the owner's own login. The
// control is the right old password first, on a fresh account.
func TestChangePasswordOldPasswordIsThrottled(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) { c.Limits.Rate.LoginFailedBurst = 1000 })
	const pw = "correct horse battery staple"
	u, _, token := seedAPISession(t, deps)
	seedPassword(t, deps, u, pw)
	change := func(token, old string) int {
		body, _ := cborx.Marshal([]any{old, "the thief's password"})
		return postCBORAuth(h, "/v1/auth/password", body, token).Code
	}
	for i := 0; i <= deps.Config.Auth.Lockout.FreeAttempts; i++ {
		change(token, "not the password")
	}
	if got := change(token, pw); got != http.StatusTooManyRequests {
		t.Fatalf("the right old password after free_attempts+1 wrong ones = %d, want 429", got)
	}
	if locked := deps.Throttle.LockedFor(u.ID); locked != 0 {
		t.Fatalf("the guesses locked the owner's login for %s", locked)
	}
	if status, _ := login(t, h, u.Username, pw); status != http.StatusOK {
		t.Fatalf("the owner's password after the refused change = %d, want 200 (unchanged)", status)
	}

	v, _, vToken := seedAPISession(t, deps)
	seedPassword(t, deps, v, pw)
	if got := change(vToken, pw); got != http.StatusNoContent {
		t.Fatalf("the control (the right old password first) = %d, want 204", got)
	}
}

// slowHasher is testHasher that takes a while, so concurrent requests overlap inside Verify, and
// counts the guesses that reached it.
type slowHasher struct {
	testHasher
	checked *atomic.Int64
}

func (h slowHasher) Verify(ctx context.Context, pw, phc string) (bool, bool, error) {
	h.checked.Add(1)
	time.Sleep(150 * time.Millisecond)
	return h.testHasher.Verify(ctx, pw, phc)
}

// The throttle fix (item 4): concurrent step-ups are counted before they are checked, so a burst
// in flight cannot all pass the gate ahead of its own failures. Twenty wrong passwords at once from
// one address: at most free_attempts+1 reach the hasher.
func TestConcurrentStepUpGuessesAreCountedBeforeTheyAreChecked(t *testing.T) {
	checked := &atomic.Int64{}
	h, deps := newTestAPIFull(t, func(c *config.Config) { c.Limits.Rate.LoginFailedBurst = 1000 },
		func(d *api.Deps) { d.Hasher = slowHasher{checked: checked} })
	u, _, token := seedAPISession(t, deps)
	seedPassword(t, deps, u, "correct horse battery staple")
	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stepUpRaw(h, token, stepUpPassword, "not the password")
		}()
	}
	wg.Wait()
	limit := int64(deps.Config.Auth.Lockout.FreeAttempts + 1)
	if got := checked.Load(); got > limit || got == 0 {
		t.Fatalf("%d of %d concurrent wrong step-ups reached the password check, want 1..%d", got, n, limit)
	}
}

// --- second-factor verify reserves its guess (re-review round) ---

// wrongTOTP is a six-digit code that matches the secret in none of the windows the skew admits.
func wrongTOTP(t *testing.T, secret string, now time.Time) string {
	t.Helper()
	near := map[string]bool{}
	for _, off := range []time.Duration{-60 * time.Second, -30 * time.Second, 0, 30 * time.Second, 60 * time.Second} {
		c, err := totp.GenerateCode(secret, now.Add(off))
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}
		near[c] = true
	}
	for i := 0; ; i++ {
		c := fmt.Sprintf("%06d", (i*7919)%1000000)
		if !near[c] {
			return c
		}
	}
}

// totpCountRepo counts the TOTP secret reads (one per code comparison) while armed, and stalls
// each, so concurrent verifies overlap inside the check.
type totpCountRepo struct {
	store.Repository
	armed *atomic.Bool
	n     *atomic.Int64
}

func (c totpCountRepo) GetTOTP(ctx context.Context, u id.ID) (store.TOTPRow, error) {
	if c.armed.Load() {
		c.n.Add(1)
		time.Sleep(30 * time.Millisecond)
	}
	return c.Repository.GetTOTP(ctx, u)
}

// I1: a burst of needs-1 assertions minted while the account was unlocked, spent at once on
// wrong codes: each guess is counted before its code is compared, so at most free_attempts+1
// reach the comparison.
func TestConcurrentSecondFactorGuessesAreCountedBeforeTheyAreChecked(t *testing.T) {
	armed, compared := &atomic.Bool{}, &atomic.Int64{}
	h, deps := newTestAPIFull(t, func(c *config.Config) {
		c.Limits.Rate.LoginBurst = 1000
		c.Limits.Rate.LoginFailedBurst = 1000
	}, func(d *api.Deps) { d.Repo = totpCountRepo{Repository: d.Repo, armed: armed, n: compared} })
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	secret, _ := enrolTOTP(t, h, deps, token)

	const k = 20
	assertions := make([]string, 0, k)
	for range k {
		a, needs := needsOf(t, h, u.Username, pw)
		if needs != 1 {
			t.Fatalf("needs = %d, want 1", needs)
		}
		assertions = append(assertions, a)
	}
	wrong := wrongTOTP(t, secret, deps.Clock.Now())
	armed.Store(true)
	var wg sync.WaitGroup
	for _, a := range assertions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _ := cborx.Marshal([]any{a, wrong})
			postCBOR(h, "/v1/auth/totp/verify", body)
		}()
	}
	wg.Wait()
	limit := int64(deps.Config.Auth.Lockout.FreeAttempts + 1)
	if got := compared.Load(); got > limit || got == 0 {
		t.Fatalf("%d of %d concurrent wrong codes reached the comparison, want 1..%d", got, k, limit)
	}
}

// I2: an assertion minted while the account was unlocked does not outlive a lockout that lands
// after it: the correct code is refused 429. The control spends the same kind of assertion on an
// account that was not locked in between.
func TestTheVerifyRefusesALockedAccountEvenForAnEarlierAssertion(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) {
		c.Limits.Rate.LoginBurst = 1000
		c.Limits.Rate.LoginFailedBurst = 1000
	})
	run := func(lock bool) int {
		u, _, token := seedAPISession(t, deps)
		pw := seedPassword(t, deps, u, "correct horse battery staple")
		secret, _ := enrolTOTP(t, h, deps, token)
		assertion, _ := needsOf(t, h, u.Username, pw)
		if lock {
			for i := 0; i <= deps.Config.Auth.Lockout.FreeAttempts; i++ {
				login(t, h, u.Username, "not the password")
			}
			if deps.Throttle.LockedFor(u.ID) == 0 {
				t.Fatal("free_attempts+1 wrong passwords did not lock the account")
			}
		}
		code, _ := totp.GenerateCode(secret, deps.Clock.Now())
		body, _ := cborx.Marshal([]any{assertion, code})
		return postCBOR(h, "/v1/auth/totp/verify", body).Code
	}
	if got := run(true); got != http.StatusTooManyRequests {
		t.Fatalf("the correct code on an earlier assertion of a now-locked account = %d, want 429", got)
	}
	if got := run(false); got != http.StatusOK {
		t.Fatalf("the control (no lock in between) = %d, want 200", got)
	}
}

// failingHasher answers E_RATE_LIMITED (its concurrency bound) while fail is set: a request that
// ends before its guess is decided.
type failingHasher struct {
	testHasher
	fail *atomic.Bool
}

func (h failingHasher) Verify(ctx context.Context, pw, phc string) (bool, bool, error) {
	if h.fail.Load() {
		return false, false, server.Errorf(server.CodeRateLimited, "the hasher is busy")
	}
	return h.testHasher.Verify(ctx, pw, phc)
}

// M1 (a): a step-up that ends before its guess is decided (the hasher refused) is not counted,
// and it refunds no one else's guess: the wrong guesses on either side of it add up exactly.
func TestAnUndecidedStepUpNeitherCountsNorRefunds(t *testing.T) {
	fail := &atomic.Bool{}
	h, deps := newTestAPIFull(t, func(c *config.Config) { c.Limits.Rate.LoginFailedBurst = 1000 },
		func(d *api.Deps) { d.Hasher = failingHasher{fail: fail} })
	u, _, token := seedAPISession(t, deps)
	seedPassword(t, deps, u, "correct horse battery staple")
	free := deps.Config.Auth.Lockout.FreeAttempts
	for i := 1; i < free; i++ {
		if res := stepUpRaw(h, token, stepUpPassword, "not it"); res.Code != http.StatusForbidden {
			t.Fatalf("wrong guess %d = %d, want 403", i, res.Code)
		}
	}
	fail.Store(true)
	for i := 0; i < 3; i++ {
		if res := stepUpRaw(h, token, stepUpPassword, "not it"); res.Code != http.StatusTooManyRequests {
			t.Fatalf("undecided step-up %d = %d, want the hasher's 429", i, res.Code)
		}
	}
	fail.Store(false)
	if res := stepUpRaw(h, token, stepUpPassword, "not it"); res.Code != http.StatusForbidden {
		t.Fatalf("wrong guess %d (the undecided ones not counted) = %d, want 403", free, res.Code)
	}
	if res := stepUpRaw(h, token, stepUpPassword, "not it"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("wrong guess %d (none of the earlier ones refunded) = %d, want 429", free+1, res.Code)
	}
}

// M1 (b): a password login that still owes a code does not clear the account ledger, so wrong
// codes after correct passwords accumulate: past free_attempts the right password and the right
// code are refused. The control stops one round short.
func TestWrongCodesAfterTheRightPasswordAccumulate(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) {
		c.Limits.Rate.LoginBurst = 1000
		c.Limits.Rate.LoginFailedBurst = 1000
	})
	free := deps.Config.Auth.Lockout.FreeAttempts
	run := func(rounds int) int {
		u, _, token := seedAPISession(t, deps)
		pw := seedPassword(t, deps, u, "correct horse battery staple")
		secret, _ := enrolTOTP(t, h, deps, token)
		wrong := wrongTOTP(t, secret, deps.Clock.Now())
		for range rounds {
			assertion, _ := needsOf(t, h, u.Username, pw)
			body, _ := cborx.Marshal([]any{assertion, wrong})
			postCBOR(h, "/v1/auth/totp/verify", body)
		}
		status, out := login(t, h, u.Username, pw)
		if status != http.StatusOK {
			return status
		}
		code, _ := totp.GenerateCode(secret, deps.Clock.Now())
		body, _ := cborx.Marshal([]any{out[0].(string), code})
		return postCBOR(h, "/v1/auth/totp/verify", body).Code
	}
	if got := run(free + 1); got != http.StatusTooManyRequests {
		t.Fatalf("the right password and code after %d wrong codes = %d, want 429", free+1, got)
	}
	if got := run(free - 1); got != http.StatusOK {
		t.Fatalf("the control (%d wrong codes) = %d, want 200", free-1, got)
	}
}

// --- web3.7 3f: the web3.6 security cards ---

// countingHasher is testHasher that counts its Hash calls and fails the next one while failNext is
// set (once: the flag is cleared by the failure).
type countingHasher struct {
	testHasher
	hashed   *atomic.Int64
	failNext *atomic.Bool
}

func (h countingHasher) Hash(ctx context.Context, pw string) (string, error) {
	h.hashed.Add(1)
	if h.failNext.CompareAndSwap(true, false) {
		return "", server.Errorf(server.CodeInternal, "the hasher failed")
	}
	return h.testHasher.Hash(ctx, pw)
}

func newHasherAPI(t *testing.T) (http.Handler, api.Deps, countingHasher) {
	t.Helper()
	hasher := countingHasher{hashed: &atomic.Int64{}, failNext: &atomic.Bool{}}
	h, deps := newTestAPIFull(t, nil, func(d *api.Deps) { d.Hasher = hasher })
	return h, deps, hasher
}

func firstPassword(t *testing.T, h http.Handler, token string, body []any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := cborx.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return postCBORAuth(h, "/v1/auth/password", b, token)
}

// R33: a first password whose hash fails leaves its step-up token unspent, so the retry with the
// same token sets the password.
func TestAFailedHashDoesNotSpendTheStepUp(t *testing.T) {
	h, deps, hasher := newHasherAPI(t)
	u, _, token := seedAPISession(t, deps)
	_, codes := enrolTOTP(t, h, deps, token)
	stepup := stepUp(t, h, token, stepUpBackup, codes[0])

	hasher.failNext.Store(true)
	if res := firstPassword(t, h, token, []any{nil, "a brand new password", stepup}); res.Code < 500 {
		t.Fatalf("a failed hash = %d (%x), want 5xx", res.Code, res.Body.Bytes())
	}
	if res := firstPassword(t, h, token, []any{nil, "a brand new password", stepup}); res.Code != http.StatusNoContent {
		t.Fatalf("the retry with the same token = %d (%x), want 204: the failed hash spent it", res.Code, res.Body.Bytes())
	}
	if status, _ := login(t, h, u.Username, "a brand new password"); status != http.StatusOK {
		t.Fatalf("the password the retry set does not log in: %d", status)
	}
	// Spent by the success: the same token is refused afterwards.
	if res := newCodes(h, token, stepup); res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
		t.Fatalf("the token after the set = %d, want 403 E_STEP_UP (spent)", res.Code)
	}
}

// R33: a missing or bad step-up token is refused before any Argon2 work; the control with a valid
// token costs exactly one hash.
func TestABadStepUpCostsNoHash(t *testing.T) {
	h, deps, hasher := newHasherAPI(t)
	_, _, token := seedAPISession(t, deps)
	_, codes := enrolTOTP(t, h, deps, token)

	for _, c := range []struct {
		name string
		body []any
	}{
		{"no step-up element", []any{nil, "a brand new password"}},
		{"a null step-up", []any{nil, "a brand new password", nil}},
		{"an unknown step-up", []any{nil, "a brand new password", "not-a-token"}},
	} {
		res := firstPassword(t, h, token, c.body)
		if res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
			t.Fatalf("%s: status = %d (%x), want 403 E_STEP_UP", c.name, res.Code, res.Body.Bytes())
		}
		if got := hasher.hashed.Load(); got != 0 {
			t.Fatalf("%s: the refusal cost %d hashes, want 0", c.name, got)
		}
	}
	stepup := stepUp(t, h, token, stepUpBackup, codes[0])
	if res := firstPassword(t, h, token, []any{nil, "a brand new password", stepup}); res.Code != http.StatusNoContent {
		t.Fatalf("the control with a valid token = %d (%x), want 204", res.Code, res.Body.Bytes())
	}
	if got := hasher.hashed.Load(); got != 1 {
		t.Fatalf("the control cost %d hashes, want 1", got)
	}
}

// Valid is Spend's lookup without the spend.
func TestValidDoesNotSpend(t *testing.T) {
	s := api.NewStepUps(clock.NewFake(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)), api.StepUpTTL)
	uid, sess := id.New(), []byte("the session's token hash")
	tok, _ := s.Issue(uid, sess)
	for i := range 2 {
		if got, ok := s.Valid(tok, sess); !ok || got != uid {
			t.Fatalf("Valid #%d = %s, %v; want %s, true", i+1, got, ok, uid)
		}
	}
	if got, ok := s.Spend(tok, sess); !ok || got != uid {
		t.Fatalf("Spend after two Valids = %s, %v; want %s, true", got, ok, uid)
	}
	if _, ok := s.Valid(tok, sess); ok {
		t.Fatal("Valid after the Spend = true; want false")
	}
}

// failOnceTOTPRepo fails the next GetTOTP while armed (once: the flag is cleared by the failure).
type failOnceTOTPRepo struct {
	store.Repository
	armed *atomic.Bool
}

func (r failOnceTOTPRepo) GetTOTP(ctx context.Context, u id.ID) (store.TOTPRow, error) {
	if r.armed.CompareAndSwap(true, false) {
		return store.TOTPRow{}, errors.New("the store is down")
	}
	return r.Repository.GetTOTP(ctx, u)
}

// verifyWrong posts one wrong code behind a fresh needs-1 assertion.
func verifyWrong(t *testing.T, h http.Handler, u store.UserRow, pw, wrong string) int {
	t.Helper()
	assertion, _ := needsOf(t, h, u.Username, pw)
	body, _ := cborx.Marshal([]any{assertion, wrong})
	return postCBOR(h, "/v1/auth/totp/verify", body).Code
}

// A store failure during a verify is not a guess: it refunds its own attempt, so the lockout comes
// after exactly as many wrong codes as on an account where nothing failed.
func TestAStoreErrorDuringAVerifyRefundsTheAttempt(t *testing.T) {
	armed := &atomic.Bool{}
	h, deps := newTestAPIFull(t, func(c *config.Config) {
		c.Limits.Rate.LoginBurst = 1000
		c.Limits.Rate.LoginFailedBurst = 1000
	}, func(d *api.Deps) { d.Repo = failOnceTOTPRepo{Repository: d.Repo, armed: armed} })
	free := deps.Config.Auth.Lockout.FreeAttempts
	// firstLock answers the number of the wrong code that is refused 429.
	firstLock := func(storeFails bool) int {
		u, _, token := seedAPISession(t, deps)
		pw := seedPassword(t, deps, u, "correct horse battery staple")
		secret, _ := enrolTOTP(t, h, deps, token)
		wrong := wrongTOTP(t, secret, deps.Clock.Now())
		if storeFails {
			assertion, _ := needsOf(t, h, u.Username, pw)
			code, _ := totp.GenerateCode(secret, deps.Clock.Now())
			body, _ := cborx.Marshal([]any{assertion, code})
			armed.Store(true)
			if res := postCBOR(h, "/v1/auth/totp/verify", body); res.Code < 500 {
				t.Fatalf("a verify whose store read fails = %d, want 5xx", res.Code)
			}
		}
		for i := 1; i <= free+2; i++ {
			switch got := verifyWrong(t, h, u, pw, wrong); got {
			case http.StatusTooManyRequests:
				return i
			case http.StatusUnauthorized:
			default:
				t.Fatalf("wrong code %d = %d, want 401 or 429", i, got)
			}
		}
		t.Fatalf("%d wrong codes never locked the account", free+2)
		return 0
	}
	control, failed := firstLock(false), firstLock(true)
	if control != free+1 {
		t.Fatalf("the control locked at wrong code %d, want %d", control, free+1)
	}
	if failed != control {
		t.Fatalf("after a store failure the lockout came at wrong code %d, want %d (the failure was counted)", failed, control)
	}
}

// A complete second-factor login clears the account ledger: four wrong codes, the right code, then
// four more wrong codes do not lock the account.
func TestASuccessClearsTheAccountLedger(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) {
		c.Limits.Rate.LoginBurst = 1000
		c.Limits.Rate.LoginFailedBurst = 1000
	})
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	secret, _ := enrolTOTP(t, h, deps, token)
	wrong := wrongTOTP(t, secret, deps.Clock.Now())
	for i := 1; i <= 4; i++ {
		if got := verifyWrong(t, h, u, pw, wrong); got != http.StatusUnauthorized {
			t.Fatalf("wrong code %d = %d, want 401", i, got)
		}
	}
	assertion, _ := needsOf(t, h, u.Username, pw)
	code, _ := totp.GenerateCode(secret, deps.Clock.Now())
	body, _ := cborx.Marshal([]any{assertion, code})
	if res := postCBOR(h, "/v1/auth/totp/verify", body); res.Code != http.StatusOK {
		t.Fatalf("the right code = %d, want 200", res.Code)
	}
	for i := 1; i <= 4; i++ {
		if got := verifyWrong(t, h, u, pw, wrong); got != http.StatusUnauthorized {
			t.Fatalf("wrong code %d after the success = %d, want 401 (the success cleared the ledger)", i, got)
		}
	}
	if locked := deps.Throttle.LockedFor(u.ID); locked != 0 {
		t.Fatalf("the account is locked for %s, want not locked", locked)
	}
}

// R20: one step-up token spent by sixteen goroutines at once is spent exactly once.
func TestOneStepUpTokenIsSpentOnce(t *testing.T) {
	s := api.NewStepUps(clock.NewFake(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)), api.StepUpTTL)
	uid, sess := id.New(), []byte("the session's token hash")
	tok, _ := s.Issue(uid, sess)
	const n = 16
	var spent atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := s.Spend(tok, sess); ok {
				spent.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := spent.Load(); got != 1 {
		t.Fatalf("%d of %d concurrent spends of one token succeeded, want exactly 1", got, n)
	}
}

// R20: two concurrent POST /v1/auth/recovery/verify with the same backup code sign in once.
func TestOneBackupCodeSignsInOnce(t *testing.T) {
	h, deps := newTestAPIWithConfig(t, func(c *config.Config) {
		c.Limits.Rate.LoginBurst = 1000
		c.Limits.Rate.LoginFailedBurst = 1000
	})
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	_, codes := enrolTOTP(t, h, deps, token)
	first, _ := needsOf(t, h, u.Username, pw)
	second, _ := needsOf(t, h, u.Username, pw)

	var ok atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, a := range []string{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _ := cborx.Marshal([]any{a, codes[0]})
			<-start
			if postCBOR(h, "/v1/auth/recovery/verify", body).Code == http.StatusOK {
				ok.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := ok.Load(); got != 1 {
		t.Fatalf("%d of 2 concurrent verifies with one backup code answered 200, want exactly 1", got)
	}
}

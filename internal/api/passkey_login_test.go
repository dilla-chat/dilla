package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pquerna/otp/totp"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/id"
)

// The whole ceremony, both legs, on the real routes: register a discoverable
// credential and then log in with it, username-less.
//
// This is the first test in the task that reaches CreateCredential,
// ValidatePasskeyLogin and the store write-back at all. It is also the
// end-to-end check on the flag codec: validateLogin refuses a login whose
// BackupEligible flag differs from the STORED one (webauthn@v0.18.2
// webauthn/login.go:402-405), so the login below can only succeed if the BE bit
// flagBytes wrote is the bit flagsFromByte reads.
func TestAPasskeyRegistersAndThenLogsInWithoutAUsername(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	user, _, token := seedAPISession(t, deps)
	device := newVirtualAuthenticator(t, deps.Domain, true)

	credID := enrolPasskey(t, h, token, device)
	if len(credID) == 0 {
		t.Fatal("register/finish reported no credential id")
	}

	// The stored row is what a later login is validated against, so every
	// column FinishRegistration writes is worth an assertion here.
	stored, err := deps.Repo.GetWebauthnCredential(t.Context(), credID)
	if err != nil {
		t.Fatalf("GetWebauthnCredential: %v", err)
	}
	if stored.UserID != user.ID {
		t.Fatalf("the credential is owned by %s, want %s", stored.UserID, user.ID)
	}
	if stored.RPID != deps.Domain {
		t.Fatalf("rp_id = %q, want %q", stored.RPID, deps.Domain)
	}
	if stored.Transports != "internal,hybrid" {
		t.Fatalf("transports = %q, want the two the authenticator reported", stored.Transports)
	}
	if stored.AttestationFormat != "none" {
		t.Fatalf("attestation_format = %q, want %q", stored.AttestationFormat, "none")
	}
	if len(stored.Flags) != 1 || stored.Flags[0] == 0 {
		t.Fatalf("flags = %#v; the ceremony's UP/UV/BE/BS were not persisted", stored.Flags)
	}

	creds, err := deps.Passkeys.CredentialsForTest(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("CredentialsForTest: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("the account holds %d credentials, want 1", len(creds))
	}
	flags := creds[0].Flags
	if !flags.UserPresent || !flags.UserVerified || !flags.BackupEligible || !flags.BackupState {
		t.Fatalf("the stored flags read back as %+v, not the ones the authenticator set", flags)
	}

	assertion, needs := passkeyLoginNeeds(t, h, device)
	if needs != 0 {
		t.Fatalf("a UV passkey login: needs = %d, want 0", needs)
	}
	got, needsSecondFactor := spendAssertion(t, deps, assertion)
	if got != user.ID {
		t.Fatalf("the passkey login named %s, want %s", got, user.ID)
	}
	// No second factor is enrolled on this account, so the passkey is the whole
	// login and the assertion is complete.
	if needsSecondFactor {
		t.Fatal("an account with no second factor was asked for one")
	}

	// The sign count the authenticator reported is written back; without it a
	// cloned authenticator could replay for ever undetected.
	after, err := deps.Repo.GetWebauthnCredential(t.Context(), credID)
	if err != nil {
		t.Fatalf("GetWebauthnCredential: %v", err)
	}
	if after.SignCount != 1 {
		t.Fatalf("sign_count = %d after one login, want 1", after.SignCount)
	}
}

// A passkey counts alone only when THIS assertion verified the user. On an
// instance that set user_verification = "preferred", a credential registered
// with UV and later used with presence only owes a code: the stored credential's
// UV flag is latched (webauthn@v0.18.2 CredentialFlags.Update), so it must not be
// what decides. The control is the same credential with UV.
func TestAPossessionOnlyPasskeyLoginOwesACode(t *testing.T) {
	h, deps := newPasskeyAPIUV(t, "preferred")
	user, _, token := seedAPISession(t, deps)
	device := newVirtualAuthenticator(t, deps.Domain, true)
	enrolPasskey(t, h, token, device)

	if _, needs := passkeyLoginNeeds(t, h, device); needs != 0 {
		t.Fatalf("a UV passkey login: needs = %d, want 0", needs)
	}
	device.userVerified = false
	assertion, needs := passkeyLoginNeeds(t, h, device)
	if needs != 1 {
		t.Fatalf("a presence-only passkey login: needs = %d, want 1", needs)
	}
	if got, owes := spendAssertion(t, deps, assertion); got != user.ID || !owes {
		t.Fatalf("the presence-only assertion = %s owing %v, want %s owing a factor", got, owes, user.ID)
	}
}

// Under the default user_verification = "required" a presence-only assertion
// is refused outright; the UV control logs in owing nothing.
func TestARequiredInstanceRefusesAPossessionOnlyPasskey(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	if deps.Config.Auth.WebAuthn.UserVerification != "required" {
		t.Fatalf("user_verification defaults to %q, want \"required\"", deps.Config.Auth.WebAuthn.UserVerification)
	}
	_, _, token := seedAPISession(t, deps)
	device := newVirtualAuthenticator(t, deps.Domain, true)
	enrolPasskey(t, h, token, device)

	device.userVerified = false
	if res := passkeyLoginRaw(t, h, device); res.Code != http.StatusUnauthorized {
		t.Fatalf("a presence-only login on a required instance = %d, want 401", res.Code)
	}
	device.userVerified = true
	if _, needs := passkeyLoginNeeds(t, h, device); needs != 0 {
		t.Fatalf("the UV control: needs = %d, want 0", needs)
	}
}

// A passkey this instance does not hold is E_PASSKEY_UNKNOWN, so the client can
// say "this passkey is not registered here" rather than "it failed".
func TestADeletedPasskeyIsUnknown(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	user, _, token := seedAPISession(t, deps)
	device := newVirtualAuthenticator(t, deps.Domain, true)
	credID := enrolPasskey(t, h, token, device)

	if res := passkeyLoginRaw(t, h, device); res.Code != http.StatusOK {
		t.Fatalf("the credential before the delete = %d, want 200", res.Code)
	}
	if n, err := deps.Repo.DeleteWebauthnCredential(t.Context(), user.ID, credID); err != nil || n != 1 {
		t.Fatalf("DeleteWebauthnCredential = %d, %v", n, err)
	}
	res := passkeyLoginRaw(t, h, device)
	if res.Code != http.StatusUnauthorized || errorCode(t, res) != "E_PASSKEY_UNKNOWN" {
		t.Fatalf("a deleted passkey = %d %s, want 401 E_PASSKEY_UNKNOWN", res.Code, errorCode(t, res))
	}
}

// stepUpCeremony begins a passkey step-up for the session token holds.
func stepUpCeremony(t *testing.T, h http.Handler, token string) (id.ID, string) {
	t.Helper()
	empty, _ := cborx.Marshal([]any{})
	res := postCBORAuth(h, "/v1/auth/stepup/passkey/begin", empty, token)
	if res.Code != http.StatusOK {
		t.Fatalf("stepup/passkey/begin = %d (%x)", res.Code, res.Body.Bytes())
	}
	return ceremonyAndOptions(t, res.Body.Bytes())
}

// A step-up ceremony is kind 1 with its user set: login/finish refuses it, and
// the step-up finish refuses a login ceremony and another user's step-up.
func TestAStepUpCeremonyIsNotALoginAndALoginIsNotAStepUp(t *testing.T) {
	// The address failure budget out of the way: three refusals below charge it, and this test is
	// about which ceremony each route takes, not about the budget.
	h, deps, _ := newPasskeyAPITuned(t, "", false, func(c *config.Config) { c.Limits.Rate.LoginFailedBurst = 100 })
	_, _, token := seedAPISession(t, deps)
	device := newVirtualAuthenticator(t, deps.Domain, true)
	enrolPasskey(t, h, token, device)

	// A step-up ceremony sent to login/finish.
	ceremony, options := stepUpCeremony(t, h, token)
	body, _ := cborx.Marshal([]any{ceremony, device.login(t, options)})
	if res := postCBOR(h, "/v1/auth/passkey/login/finish", body); res.Code != http.StatusUnauthorized {
		t.Fatalf("a step-up ceremony at login/finish = %d, want 401", res.Code)
	}
	// Its control: a login ceremony there.
	if res := passkeyLoginRaw(t, h, device); res.Code != http.StatusOK {
		t.Fatalf("a login ceremony at login/finish = %d, want 200", res.Code)
	}

	// A login ceremony sent to stepup/passkey/finish.
	empty, _ := cborx.Marshal([]any{})
	res := postCBOR(h, "/v1/auth/passkey/login/begin", empty)
	ceremony, options = ceremonyAndOptions(t, res.Body.Bytes())
	body, _ = cborx.Marshal([]any{ceremony, device.login(t, options)})
	res = postCBORAuth(h, "/v1/auth/stepup/passkey/finish", body, token)
	if res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
		t.Fatalf("a login ceremony at stepup/passkey/finish = %d, want 403 E_STEP_UP", res.Code)
	}

	// Another user's step-up ceremony, finished from this session.
	_, _, otherToken := seedAPISession(t, deps)
	otherDevice := newVirtualAuthenticator(t, deps.Domain, true)
	enrolPasskey(t, h, otherToken, otherDevice)
	ceremony, options = stepUpCeremony(t, h, otherToken)
	body, _ = cborx.Marshal([]any{ceremony, otherDevice.login(t, options)})
	res = postCBORAuth(h, "/v1/auth/stepup/passkey/finish", body, token)
	if res.Code != http.StatusForbidden || errorCode(t, res) != "E_STEP_UP" {
		t.Fatalf("another user's step-up ceremony = %d, want 403 E_STEP_UP", res.Code)
	}

	// The control: this user's own step-up ceremony mints a token.
	ceremony, options = stepUpCeremony(t, h, token)
	body, _ = cborx.Marshal([]any{ceremony, device.login(t, options)})
	stepUpToken(t, postCBORAuth(h, "/v1/auth/stepup/passkey/finish", body, token))
}

// A passkey's name is 1-64 bytes of UTF-8 without control characters. The
// refusals do not spend the ceremony, so the control finishes the same one.
func TestAPasskeyNameIsOneToSixtyFourBytes(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	_, _, token := seedAPISession(t, deps)
	device := newVirtualAuthenticator(t, deps.Domain, true)
	begin, _ := cborx.Marshal([]any{nil})
	res := postCBORAuth(h, "/v1/auth/passkey/register/begin", begin, token)
	if res.Code != http.StatusOK {
		t.Fatalf("register/begin = %d", res.Code)
	}
	ceremony, options := ceremonyAndOptions(t, res.Body.Bytes())
	response := device.register(t, options)
	finish := func(name string) int {
		body, _ := cborx.Marshal([]any{ceremony, response, name})
		return postCBORAuth(h, "/v1/auth/passkey/register/finish", body, token).Code
	}
	for _, bad := range []string{"", strings.Repeat("a", 65), "tab\there", "\xff\xfe"} {
		if got := finish(bad); got != http.StatusBadRequest {
			t.Fatalf("name %q = %d, want 400", bad, got)
		}
	}
	if got := finish(strings.Repeat("a", 64)); got != http.StatusOK {
		t.Fatalf("a 64-byte name = %d, want 200", got)
	}
}

// I1 (b): a registration ceremony finished from another account's session is
// refused (400) and NOT spent: the account that began it finishes the same
// ceremony, with the same response, as the control.
func TestRegisterFinishRefusesAnotherAccountsCeremony(t *testing.T) {
	h, deps := newPasskeyAPI(t)
	a, _, aToken := seedAPISession(t, deps)
	_, _, bToken := seedAPISession(t, deps)
	begin, _ := cborx.Marshal([]any{nil})
	res := postCBORAuth(h, "/v1/auth/passkey/register/begin", begin, aToken)
	if res.Code != http.StatusOK {
		t.Fatalf("register/begin = %d", res.Code)
	}
	ceremony, options := ceremonyAndOptions(t, res.Body.Bytes())
	device := newVirtualAuthenticator(t, deps.Domain, true)
	body, _ := cborx.Marshal([]any{ceremony, device.register(t, options), "Laptop"})

	if res := postCBORAuth(h, "/v1/auth/passkey/register/finish", body, bToken); res.Code != http.StatusBadRequest {
		t.Fatalf("finishing another account's ceremony = %d, want 400", res.Code)
	}
	if _, err := deps.Repo.GetWebauthnCredential(t.Context(), device.credID); err == nil {
		t.Fatal("the refused finish stored a credential")
	}
	if res := postCBORAuth(h, "/v1/auth/passkey/register/finish", body, aToken); res.Code != http.StatusOK {
		t.Fatalf("the account that began it finishing it = %d (%x), want 200", res.Code, res.Body.Bytes())
	}
	if row, err := deps.Repo.GetWebauthnCredential(t.Context(), device.credID); err != nil || row.UserID != a.ID {
		t.Fatalf("the credential = %+v, %v; want it filed to the account that began the ceremony", row, err)
	}
}

// I1 (d): a passkey registered under another rp_id is unknown at login and is
// not a factor. The control is the same credential under this instance's rp_id.
func TestAPasskeyUnderAnotherRelyingPartyIsNeitherALoginNorAFactor(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "", false, func(c *config.Config) { c.Limits.Rate.LoginBurst = 100 })
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	device := newVirtualAuthenticator(t, deps.Domain, true)
	credID := enrolPasskey(t, h, token, device)

	if res := passkeyLoginRaw(t, h, device); res.Code != http.StatusOK {
		t.Fatalf("the control (this rp_id) = %d, want 200", res.Code)
	}
	if _, needs := needsOf(t, h, u.Username, pw); needs != 2 {
		t.Fatalf("the control: a password login owes %d, want 2", needs)
	}

	row, err := deps.Repo.GetWebauthnCredential(t.Context(), credID)
	if err != nil {
		t.Fatalf("GetWebauthnCredential: %v", err)
	}
	if _, err := deps.Repo.DeleteWebauthnCredential(t.Context(), u.ID, credID); err != nil {
		t.Fatalf("DeleteWebauthnCredential: %v", err)
	}
	row.RPID = "other.example"
	if err := deps.Repo.PutWebauthnCredential(t.Context(), row); err != nil {
		t.Fatalf("PutWebauthnCredential: %v", err)
	}

	res := passkeyLoginRaw(t, h, device)
	if res.Code != http.StatusUnauthorized || errorCode(t, res) != "E_PASSKEY_UNKNOWN" {
		t.Fatalf("a passkey under other.example = %d %s, want 401 E_PASSKEY_UNKNOWN", res.Code, errorCode(t, res))
	}
	if _, needs := needsOf(t, h, u.Username, pw); needs != 0 {
		t.Fatalf("a password login of an account whose only passkey is under other.example owes %d, want 0", needs)
	}
	if f := getFactors(t, h, token); len(f[2].([]any)) != 0 {
		t.Fatalf("factors list %d passkeys, want none under this rp_id", len(f[2].([]any)))
	}
}

// M3: on an instance where a presence-only passkey login is possible
// ("preferred"), it owes a code, and it must not clear the account's
// failed-login lockout before that code is given. The control is the same
// passkey with UV, which is the whole login and does clear it.
func TestAPresenceOnlyPasskeyLoginDoesNotClearTheLockout(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "preferred", false, func(c *config.Config) {
		c.Limits.Rate.LoginBurst = 100
		c.Limits.Rate.LoginFailedBurst = 100
	})
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	enrolTOTP(t, h, deps, token)
	device := newVirtualAuthenticator(t, deps.Domain, true)
	enrolPasskeyWith(t, h, token, device, stepUp(t, h, token, stepUpPassword, pw))

	for i := 0; i <= deps.Config.Auth.Lockout.FreeAttempts; i++ {
		login(t, h, u.Username, "not the password")
	}
	if deps.Throttle.LockedFor(u.ID) == 0 {
		t.Fatal("the account is not locked after free_attempts+1 wrong passwords")
	}

	device.userVerified = false
	// While the account is locked a presence-only login, which would owe a code, is refused
	// outright, and the lockout stays.
	if res := passkeyLoginRaw(t, h, device); res.Code != http.StatusTooManyRequests {
		t.Fatalf("a presence-only login of a locked account = %d, want 429", res.Code)
	}
	if deps.Throttle.LockedFor(u.ID) == 0 {
		t.Fatal("a presence-only passkey login, still owing a code, cleared the account's lockout")
	}

	device.userVerified = true
	if _, needs := passkeyLoginNeeds(t, h, device); needs != 0 {
		t.Fatalf("a UV login owes %d, want 0", needs)
	}
	if locked := deps.Throttle.LockedFor(u.ID); locked != 0 {
		t.Fatalf("the control: a complete passkey login left the account locked for %s", locked)
	}
}

// The throttle fix (item 3): someone holding a PIN-less key on a "preferred" instance logs in
// presence-only (needs 1) and guesses the TOTP at /totp/verify. Those guesses charge the account
// ledger, and once it is locked neither the presence-only login nor the verify lets the correct
// code through. The control is the same correct code on an account with no prior failures.
func TestAPresenceOnlyPasskeyLoginCannotOutguessTheLockout(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "preferred", false, func(c *config.Config) {
		c.Limits.Rate.LoginBurst = 1000
		c.Limits.Rate.LoginFailedBurst = 1000
	})
	setup := func() (string, *virtualAuthenticator) {
		u, _, token := seedAPISession(t, deps)
		pw := seedPassword(t, deps, u, "correct horse battery staple")
		secret, _ := enrolTOTP(t, h, deps, token)
		device := newVirtualAuthenticator(t, deps.Domain, true)
		enrolPasskeyWith(t, h, token, device, stepUp(t, h, token, stepUpPassword, pw))
		device.userVerified = false
		return secret, device
	}
	// attempt is a presence-only login and a code; it answers the first refusal, or the verify's status.
	attempt := func(device *virtualAuthenticator, code string) int {
		res := passkeyLoginRaw(t, h, device)
		if res.Code != http.StatusOK {
			return res.Code
		}
		var out []any
		if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		body, _ := cborx.Marshal([]any{out[0].(string), code})
		return postCBOR(h, "/v1/auth/totp/verify", body).Code
	}

	secret, device := setup()
	for i := 0; i <= deps.Config.Auth.Lockout.FreeAttempts; i++ {
		attempt(device, "000000")
	}
	right, _ := totp.GenerateCode(secret, deps.Clock.Now())
	if got := attempt(device, right); got != http.StatusTooManyRequests {
		t.Fatalf("the correct code after free_attempts+1 wrong ones = %d, want 429", got)
	}

	cSecret, cDevice := setup()
	cRight, _ := totp.GenerateCode(cSecret, deps.Clock.Now())
	if got := attempt(cDevice, cRight); got != http.StatusOK {
		t.Fatalf("the control (no prior failures) = %d, want 200", got)
	}
}

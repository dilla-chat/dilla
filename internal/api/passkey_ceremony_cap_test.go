package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// Wave B task 3, security review round 6. A: past auth.webauthn.max_open_logins the oldest open discoverable login
// ceremony is evicted, so a fresh begin always works; a network bucket sits beside the address's. B: step-ups and
// registrations are not counted there and are capped per user. C: a passkey finish bound to a password login's
// assertion refuses a passkey of another account.

func beginCeremony(t *testing.T, h http.Handler) (id.ID, string) {
	t.Helper()
	empty, _ := cborx.Marshal([]any{})
	res := postCBOR(h, "/v1/auth/passkey/login/begin", empty)
	if res.Code != http.StatusOK {
		t.Fatalf("login/begin = %d %q", res.Code, res.Body.String())
	}
	return ceremonyAndOptions(t, res.Body.Bytes())
}

func finishCeremony(h http.Handler, ceremony id.ID, response string, bound ...string) (int, []any) {
	parts := []any{ceremony, response}
	for _, b := range bound {
		parts = append(parts, b)
	}
	body, _ := cborx.Marshal(parts)
	res := postCBOR(h, "/v1/auth/passkey/login/finish", body)
	var out []any
	if res.Code == http.StatusOK {
		_ = cborx.Unmarshal(res.Body.Bytes(), &out)
	} else {
		var e []any
		if cborx.Unmarshal(res.Body.Bytes(), &e) == nil && len(e) > 0 {
			out = e
		}
	}
	return res.Code, out
}

func TestABeginAtTheCapEvictsTheOldestAndStillWorks(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "", false, func(c *config.Config) { c.Auth.WebAuthn.MaxOpenLogins = 3 })
	_, _, token := seedAPISession(t, deps)
	device := newVirtualAuthenticator(t, deps.Domain, true)
	enrolPasskey(t, h, token, device)

	// "Oldest" is by creation second (ties fall in id order), so the begins are a second apart.
	oldest, oldestOptions := beginCeremony(t, h)
	for i := 0; i < 3; i++ {
		advance(t, deps, time.Second)
		beginCeremony(t, h)
	}
	if open, err := deps.Passkeys.OpenLogins(t.Context()); err != nil || open != 3 {
		t.Fatalf("open discoverable ceremonies = %d, %v; want the cap, 3", open, err)
	}
	// The evicted ceremony fails as an expired one does: a plain 401, never E_PASSKEY_UNKNOWN.
	if code, _ := finishCeremony(h, oldest, device.login(t, oldestOptions)); code != http.StatusUnauthorized {
		t.Fatalf("a finish on the evicted ceremony = %d, want 401", code)
	}
	// A fresh begin at the cap works, and so does its finish.
	fresh, options := beginCeremony(t, h)
	if code, _ := finishCeremony(h, fresh, device.login(t, options)); code != http.StatusOK {
		t.Fatalf("a finish on a fresh ceremony at the cap = %d, want 200", code)
	}
}

func TestPasskeyBeginsAreLimitedPerNetwork(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "", false, func(c *config.Config) {
		c.Limits.Rate.PasskeyBeginNetBurst = 4
		c.Limits.Rate.PasskeyBeginBurst = 100
	})
	_ = deps
	empty, _ := cborx.Marshal([]any{})
	for i, addr := range []string{"198.51.100.1:1", "198.51.100.2:1", "198.51.100.3:1", "198.51.100.4:1"} {
		if res := postLoginFrom(h, "/v1/auth/passkey/login/begin", empty, addr); res.Code != http.StatusOK {
			t.Fatalf("begin %d from %s = %d, want 200", i+1, addr, res.Code)
		}
	}
	if res := postLoginFrom(h, "/v1/auth/passkey/login/begin", empty, "198.51.100.250:1"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("a fifth begin from the same /24 = %d, want 429", res.Code)
	}
	if res := postLoginFrom(h, "/v1/auth/passkey/login/begin", empty, "203.0.113.9:1"); res.Code != http.StatusOK {
		t.Fatalf("a begin from another /24 = %d, want 200", res.Code)
	}
	// IPv6 by /48: two /64s of one /48 share the bucket.
	for i, addr := range []string{"[2001:db8:1:1::1]:1", "[2001:db8:1:2::1]:1", "[2001:db8:1:3::1]:1", "[2001:db8:1:4::1]:1"} {
		if res := postLoginFrom(h, "/v1/auth/passkey/login/begin", empty, addr); res.Code != http.StatusOK {
			t.Fatalf("v6 begin %d from %s = %d, want 200", i+1, addr, res.Code)
		}
	}
	if res := postLoginFrom(h, "/v1/auth/passkey/login/begin", empty, "[2001:db8:1:ff::1]:1"); res.Code != http.StatusTooManyRequests {
		t.Fatalf("a fifth begin from the same /48 = %d, want 429", res.Code)
	}
}

func TestStepUpCeremoniesAreCappedPerUserAndNotCountedAsLogins(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "", false, func(c *config.Config) {
		c.Auth.WebAuthn.MaxOpenLogins = 2
		c.Auth.WebAuthn.MaxOpenPerUser = 2
	})
	u, _, token := seedAPISession(t, deps)
	enrolPasskey(t, h, token, newVirtualAuthenticator(t, deps.Domain, true))
	empty, _ := cborx.Marshal([]any{})
	for i := 0; i < 5; i++ {
		if res := postCBORAuth(h, "/v1/auth/stepup/passkey/begin", empty, token); res.Code != http.StatusOK {
			t.Fatalf("step-up begin %d = %d", i+1, res.Code)
		}
	}
	now := deps.Clock.Now().Unix()
	if n, err := deps.Repo.CountOpenCeremonies(t.Context(), &u.ID, now); err != nil || n != 2 {
		t.Fatalf("the user's open ceremonies after 5 step-up begins = %d, %v; want the per-user cap, 2", n, err)
	}
	if n, _ := deps.Passkeys.OpenLogins(t.Context()); n != 0 {
		t.Fatalf("step-ups counted as discoverable logins: %d", n)
	}
	// The login cap still has room for its own two.
	beginCeremony(t, h)
	beginCeremony(t, h)
	if n, _ := deps.Passkeys.OpenLogins(t.Context()); n != 2 {
		t.Fatalf("open discoverable logins = %d, want 2", n)
	}
}

func TestABoundPasskeyFinishRefusesAnotherAccountsPasskey(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "", false, func(c *config.Config) { c.Limits.Rate.LoginBurst = 100 })
	// Ada: password and app (her login owes a code). Bo: a passkey.
	ada, _, adaToken := seedAPISession(t, deps)
	adaPw := seedPassword(t, deps, ada, "correct horse battery staple")
	enrolTOTP(t, h, deps, adaToken)
	_, _, boToken := seedAPISession(t, deps)
	boDevice := newVirtualAuthenticator(t, deps.Domain, true)
	enrolPasskey(t, h, boToken, boDevice)

	status, out := login(t, h, ada.Username, adaPw)
	if status != http.StatusOK || out[1] != uint64(1) {
		t.Fatalf("Ada's password login = %d %#v, want owing a code", status, out)
	}
	held, _ := out[0].(string)
	ceremony, options := beginCeremony(t, h)
	code, body := finishCeremony(h, ceremony, boDevice.login(t, options), held)
	if code != http.StatusForbidden || len(body) == 0 || body[0] != "E_PASSKEY_OTHER_ACCOUNT" {
		t.Fatalf("Bo's passkey bound to Ada's login = %d %#v, want 403 E_PASSKEY_OTHER_ACCOUNT", code, body)
	}
	// Without the binding (the login step) the same passkey signs Bo in, unchanged.
	ceremony, options = beginCeremony(t, h)
	if code, _ := finishCeremony(h, ceremony, boDevice.login(t, options)); code != http.StatusOK {
		t.Fatalf("Bo's passkey unbound = %d, want 200", code)
	}
}

func TestABoundPasskeyFinishWithItsOwnAccountSignsInAndSpendsThePasswordAssertion(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "", false, func(c *config.Config) { c.Limits.Rate.LoginBurst = 100 })
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	enrolTOTP(t, h, deps, token)
	device := newVirtualAuthenticator(t, deps.Domain, true)
	enrolPasskeyWith(t, h, token, device, stepUp(t, h, token, stepUpPassword, pw))

	_, out := login(t, h, u.Username, pw)
	held, _ := out[0].(string)
	ceremony, options := beginCeremony(t, h)
	code, body := finishCeremony(h, ceremony, device.login(t, options), held)
	if code != http.StatusOK || body[1] != uint64(0) {
		t.Fatalf("her own passkey bound to her login = %d %#v, want 200 needs 0", code, body)
	}
	if _, _, ok := deps.Assertions.SpendNeeds(held); ok {
		t.Fatal("the password login's assertion was left live after the passkey superseded it")
	}
}

// The binding token says nothing when it is not a live first-factor login's assertion owing a factor: unknown, expired,
// complete and a passkey login's own are all the same plain 401, before the ceremony runs.
func TestABadBindingIsRefusedGenerically(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "", false, func(c *config.Config) {
		c.Limits.Rate.LoginBurst = 100
		c.Limits.Rate.LoginFailedBurst = 100
	})
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	device := newVirtualAuthenticator(t, deps.Domain, true)
	enrolPasskeyWith(t, h, token, device, stepUp(t, h, token, stepUpPassword, pw))

	// A password login now owes a passkey (needs 2): a live binding, used as the control below.
	login(t, h, u.Username, pw)
	complete := deps.Assertions.Issue(u.ID, 0)
	passkeyOwn := deps.Assertions.Issue(u.ID, 1)
	expired := deps.Assertions.IssueFirstFactor(u.ID, 1)
	advance(t, deps, 6*time.Minute)
	// `live` expired too with the clock: mint a fresh live one after the advance.
	_, out := login(t, h, u.Username, pw)
	live, _ := out[0].(string)

	var bodies [][]byte
	for name, bad := range map[string]string{"unknown": "not-a-token", "expired": expired, "complete": complete, "a passkey login's": passkeyOwn} {
		ceremony, options := beginCeremony(t, h)
		parts, _ := cborx.Marshal([]any{ceremony, device.login(t, options), bad})
		res := postCBOR(h, "/v1/auth/passkey/login/finish", parts)
		if res.Code != http.StatusUnauthorized || errorCode(t, res) != "E_UNAUTHENTICATED" {
			t.Fatalf("a %s binding = %d %s, want 401 E_UNAUTHENTICATED", name, res.Code, errorCode(t, res))
		}
		bodies = append(bodies, res.Body.Bytes())
	}
	for i := 1; i < len(bodies); i++ {
		if string(bodies[i]) != string(bodies[0]) {
			t.Fatalf("bad bindings answer different bodies: %x vs %x", bodies[0], bodies[i])
		}
	}
	ceremony, options := beginCeremony(t, h)
	if code, _ := finishCeremony(h, ceremony, device.login(t, options), live); code != http.StatusOK {
		t.Fatalf("the control, a live binding of the same account = %d, want 200", code)
	}
}

// Re-check M3: a bad binding is refused before the ceremony runs, so the same ceremony and the same response then work
// with the right binding.
func TestABadBindingLeavesTheCeremonyUnspent(t *testing.T) {
	h, deps, _ := newPasskeyAPITuned(t, "", false, func(c *config.Config) { c.Limits.Rate.LoginBurst = 100 })
	u, _, token := seedAPISession(t, deps)
	pw := seedPassword(t, deps, u, "correct horse battery staple")
	enrolTOTP(t, h, deps, token)
	device := newVirtualAuthenticator(t, deps.Domain, true)
	enrolPasskeyWith(t, h, token, device, stepUp(t, h, token, stepUpPassword, pw))
	_, out := login(t, h, u.Username, pw)
	held, _ := out[0].(string)

	ceremony, options := beginCeremony(t, h)
	response := device.login(t, options)
	if code, _ := finishCeremony(h, ceremony, response, "not-a-token"); code != http.StatusUnauthorized {
		t.Fatalf("a bad binding = %d, want 401", code)
	}
	if code, body := finishCeremony(h, ceremony, response, held); code != http.StatusOK {
		t.Fatalf("the same ceremony and response with the right binding = %d %#v, want 200", code, body)
	}
}

// Re-check M3: the cap holds under concurrent writes on both engines (Postgres when DILLA_TEST_PG is set, as in CI).
func TestPutCeremonyCappedHoldsUnderConcurrencyOnBothEngines(t *testing.T) {
	for name, repo := range policyEngines(t) {
		t.Run(name, func(t *testing.T) {
			const limit, writers = 5, 40
			now := int64(1_800_000_000)
			var wg sync.WaitGroup
			errs := make(chan error, writers)
			for i := 0; i < writers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					row := store.CeremonyRow{ID: id.New(), Kind: 1, SessionJSON: "{}", Created: now, Expires: now + 120}
					if _, err := repo.PutCeremonyCapped(context.Background(), row, limit, now); err != nil {
						errs <- err
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatalf("PutCeremonyCapped: %v", err)
			}
			if n, err := repo.CountOpenCeremonies(context.Background(), nil, now); err != nil || n != limit {
				t.Fatalf("open discoverable ceremonies after %d concurrent writes = %d, %v; want %d", writers, n, err, limit)
			}
		})
	}
}

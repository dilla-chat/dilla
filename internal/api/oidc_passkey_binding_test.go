package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/server"
)

// Desktop merge security review I1 and M3. An OIDC login is a first factor exactly as a password is: the passkey
// that finishes its second step must belong to the account the identity provider named. And a desktop OIDC callback
// whose request expired leaves no live assertion behind.

// newOIDCPasskeyAPI is newOIDCAPI with the relying party newPasskeyAPITuned wires, so one instance runs both.
func newOIDCPasskeyAPI(t *testing.T) (http.Handler, api.Deps, *oidcIDP) {
	t.Helper()
	idp := fakeOIDCProvider(t)
	_, d := newTestAPIWithConfig(t, func(c *config.Config) {
		c.Auth.OIDC.Enabled = true
		c.Auth.OIDC.Issuer = idp.URL
		c.Auth.OIDC.ClientID = "dilla"
		c.Auth.OIDC.RedirectURL = "https://" + c.Instance.Domain + "/v1/auth/oidc/callback"
		c.Limits.Rate.LoginBurst = 100
		c.Limits.Rate.LoginFailedBurst = 100
	})
	d.OIDC = auth.NewOIDC(d.Config.Auth.OIDC, "secret", d.Clock)
	c := &d.Config.Auth.WebAuthn
	c.RPID = d.Domain
	c.RPDisplayName = d.Domain
	c.RPOrigins = []string{"https://" + d.Domain}
	pk, err := auth.NewPasskeys(auth.ConfigFromDilla(*c), d.Repo, d.Clock)
	if err != nil {
		t.Fatalf("NewPasskeys: %v", err)
	}
	pk.SetLimits(c.MaxOpenLogins, c.MaxOpenPerUser)
	d.Passkeys = pk
	m := server.NewMux()
	api.Register(m, d)
	return m, d, idp
}

// desktopOIDC runs the desktop OIDC login up to the callback, moving the clock by lag first, and returns the callback's
// answer and a consume that hands the desktop its result.
func desktopOIDC(t *testing.T, h http.Handler, d api.Deps, idp *oidcIDP, lag time.Duration) (*httptest.ResponseRecorder, func() (string, uint8)) {
	t.Helper()
	start := startOIDC(t, h)
	idp.setNonce(start.nonce)
	advance(t, d, lag)
	callback := getWithCookies(h, "/v1/auth/oidc/callback?code=code-good&state="+url.QueryEscape(start.state), start.cookie)
	return callback, start.consume
}

// The desktop flow is the only OIDC flow since Task 12 refused the browser one (TestTheBrowserOIDCFlowIsRefused).
func TestOIDCAssertionRejectsAnotherAccountsPasskey(t *testing.T) {
	h, d, idp := newOIDCPasskeyAPI(t)
	// Ada: signs in through the provider and holds an app (her login owes a code). Bo: a passkey.
	ada, _, adaToken := seedAPISession(t, d)
	if err := d.Repo.PutOIDCIdentity(context.Background(), idp.URL, "sub-123", ada.ID, d.Clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	enrolTOTP(t, h, d, adaToken)
	_, _, boToken := seedAPISession(t, d)
	boDevice := newVirtualAuthenticator(t, d.Domain, true)
	enrolPasskey(t, h, boToken, boDevice)

	callback, consume := desktopOIDC(t, h, d, idp, 0)
	if callback.Code != http.StatusFound {
		t.Fatal(callback.Code, callback.Body)
	}
	held, needs := consume()
	if needs != 1 {
		t.Fatalf("Ada's desktop OIDC login owes %d, want a code (1)", needs)
	}
	ceremony, options := beginCeremony(t, h)
	code, body := finishCeremony(h, ceremony, boDevice.login(t, options), held)
	if code != http.StatusForbidden || len(body) == 0 || body[0] != "E_PASSKEY_OTHER_ACCOUNT" {
		t.Fatalf("Bo's passkey bound to Ada's OIDC login = %d %#v, want 403 E_PASSKEY_OTHER_ACCOUNT", code, body)
	}
	// Ada's OIDC assertion is still hers: refused bindings do not spend it.
	if got, _, ok := d.Assertions.SpendNeeds(held); !ok || got != ada.ID {
		t.Fatalf("Ada's OIDC assertion after the refusal = %s, %v; want live and hers", got, ok)
	}
}

// Review M3: a desktop request that expired between start and callback cannot take the assertion, and the callback
// leaves none behind for it.
func TestADesktopOIDCCallbackWhoseRequestExpiredLeavesNoAssertion(t *testing.T) {
	h, d, idp := newOIDCPasskeyAPI(t)
	user, _, _ := seedAPIDevice(t, d)
	if err := d.Repo.PutOIDCIdentity(context.Background(), idp.URL, "sub-123", user.ID, d.Clock.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	// Past the broker's two minutes, within the OIDC state's five.
	callback, _ := desktopOIDC(t, h, d, idp, 121*time.Second)
	if callback.Code != http.StatusUnauthorized {
		t.Fatalf("the callback for an expired desktop request = %d, want 401", callback.Code)
	}
	if n := d.Assertions.LiveForTest(); n != 0 {
		t.Fatalf("%d assertion(s) left live after the desktop request could not take it, want 0", n)
	}
}

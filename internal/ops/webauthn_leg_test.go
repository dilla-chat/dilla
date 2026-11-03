package ops_test

import (
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/ops"
)

func webauthnConfig(rpID string, origins ...string) config.Config {
	c := config.Default()
	c.Instance.Domain = "chat.example"
	c.Auth.WebAuthn.RPID = rpID
	c.Auth.WebAuthn.RPOrigins = origins
	c.Derive()
	return *c
}

func TestTheWebAuthnLegFlagsAnIPRelyingPartyAndAForeignOrigin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		want    ops.Status
		contain string
	}{
		{"control: the instance's own origin, with its port", webauthnConfig("chat.example", "https://chat.example:8443"), ops.Green, ""},
		{"control: a subdomain of rp_id", webauthnConfig("chat.example", "https://app.chat.example"), ops.Green, ""},
		{"control: the desktop shell's custom scheme is not a web origin", webauthnConfig("chat.example", "https://chat.example", "dilla://app"), ops.Green, ""},
		{"an IPv4 rp_id", webauthnConfig("10.0.0.1", "https://10.0.0.1"), ops.Red, "IP address"},
		{"an IPv6 rp_id", webauthnConfig("::1", "https://[::1]"), ops.Red, "IP address"},
		{"an origin on another domain", webauthnConfig("chat.example", "https://chat.example", "https://other.example"), ops.Red, "other.example"},
		{"a look-alike suffix is not a subdomain", webauthnConfig("chat.example", "https://notchat.example"), ops.Red, "notchat.example"},
		// Hosts are case-insensitive: the browser lowercases the origin's host before it compares.
		{"control: an origin host in another case", webauthnConfig("chat.example", "https://Chat.Example"), ops.Green, ""},
		{"control: a subdomain in another case", webauthnConfig("Chat.Example", "https://APP.chat.example"), ops.Green, ""},
		{"another domain stays foreign in any case", webauthnConfig("chat.example", "https://Other.Example"), ops.Red, "Other.Example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leg := ops.WebAuthnLeg(tc.cfg)
			if leg.Name != "webauthn" || leg.Status != tc.want {
				t.Fatalf("leg = %+v, want status %v", leg, tc.want)
			}
			if !strings.Contains(leg.Detail, tc.contain) {
				t.Fatalf("detail %q does not mention %q", leg.Detail, tc.contain)
			}
			if tc.want != ops.Green && leg.Fix == "" {
				t.Fatal("a failing leg must say how to fix it")
			}
		})
	}
}

func TestTheWebAuthnLegWarnsBehindAProxyAndWithoutTOTP(t *testing.T) {
	behind := webauthnConfig("chat.example", "https://chat.example:8443")
	behind.TLS.Mode = config.TLSModeBehindProxy
	leg := ops.WebAuthnLeg(behind)
	if leg.Status != ops.Yellow || !strings.Contains(leg.Detail, behindWant) {
		t.Fatalf("behind_proxy leg = %+v", leg)
	}

	passkeyOnly := webauthnConfig("chat.example", "https://chat.example")
	passkeyOnly.Auth.Methods = []string{"password", "passkey"}
	leg = ops.WebAuthnLeg(passkeyOnly)
	if leg.Status != ops.Yellow || !strings.Contains(leg.Detail, "passkeys are the only second factor") {
		t.Fatalf("no-totp leg = %+v", leg)
	}

	// A failure outranks a warning, and both are said.
	both := webauthnConfig("10.0.0.1", "https://10.0.0.1")
	both.Auth.Methods = []string{"password", "passkey"}
	leg = ops.WebAuthnLeg(both)
	if leg.Status != ops.Red || !strings.Contains(leg.Detail, "IP address") || !strings.Contains(leg.Detail, "passkeys are the only second factor") {
		t.Fatalf("combined leg = %+v", leg)
	}
}

const behindWant = "tls.mode is behind_proxy and auth.webauthn.rp_origins lacks the public origin https://chat.example; add the origin the browser shows"

func TestTheWebAuthnLegWarnsBehindAProxyOnlyWhenTheOriginIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		behind bool
		origin string
		want   ops.Status
	}{
		{"the public origin is listed", true, "https://chat.example", ops.Green},
		{"only a subdomain is listed", true, "https://other.chat.example", ops.Yellow},
		{"not behind a proxy, defaults", false, "https://chat.example", ops.Green},
		{"the origin in another case", true, "https://Chat.Example", ops.Green},
		{"the origin with a foreign port", true, "https://chat.example:8443", ops.Yellow},
		{"the origin with port 443", true, "https://chat.example:443", ops.Green},
		{"the origin over http", true, "http://chat.example", ops.Yellow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := webauthnConfig("chat.example", tc.origin)
			if tc.behind {
				cfg.TLS.Mode = config.TLSModeBehindProxy
			}
			leg := ops.WebAuthnLeg(cfg)
			if leg.Status != tc.want {
				t.Fatalf("leg = %+v, want %v", leg, tc.want)
			}
			if tc.want == ops.Yellow && leg.Detail != behindWant {
				t.Fatalf("detail = %q, want %q", leg.Detail, behindWant)
			}
		})
	}
}

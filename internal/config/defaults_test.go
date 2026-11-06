package config_test

import (
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/config"
)

// The mobile app's origins are CORS and gateway origins only: a passkey ceremony from
// capacitor://localhost or https://localhost must never verify against this instance.
func TestMobileOriginsNotWebAuthn(t *testing.T) {
	c := config.Default()
	c.Instance.Domain = "chat.example"
	c.Derive()
	want := []string{"capacitor://localhost", "https://localhost"}
	if !slices.Equal(c.HTTP.MobileOrigins, want) {
		t.Fatalf("http.mobile_origins = %v, want %v", c.HTTP.MobileOrigins, want)
	}
	for _, origin := range want {
		if slices.Contains(c.Auth.WebAuthn.RPOrigins, origin) {
			t.Fatalf("auth.webauthn.rp_origins %v contains the mobile origin %q", c.Auth.WebAuthn.RPOrigins, origin)
		}
	}
}

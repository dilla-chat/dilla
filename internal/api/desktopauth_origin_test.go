package api

import "testing"

func TestDesktopBrowserOriginUsesOnlyExplicitBrowserRelyingPartyOrigins(t *testing.T) {
	for _, tt := range []struct {
		name    string
		origins []string
		want    string
	}{
		{"default", nil, "https://chat.test"},
		{"native origin is not a browser", []string{"dilla://app"}, "https://chat.test"},
		{"configured browser", []string{"https://browser.chat.test"}, "https://browser.chat.test"},
		{"explicit development loopback", []string{"http://localhost:8497"}, "http://localhost:8497"},
		{"remote cleartext refused", []string{"http://remote.test"}, "https://chat.test"},
		{"path is not an origin", []string{"https://chat.test/untrusted"}, "https://chat.test"},
		{"credentials refused", []string{"https://user@remote.test"}, "https://chat.test"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := desktopBrowserOrigin("chat.test", tt.origins); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

package dilladtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The mobile app (Capacitor) runs at capacitor://localhost on iOS and https://localhost on Android
// and calls a user-chosen instance cross-origin: the gateway's upgrade accepts exactly those two
// origins by default, scheme included, and nothing beside them.
func TestMobileOriginsGatewayUpgrade(t *testing.T) {
	h, err := NewHost(t.Context(), HostOptions{DataDir: t.TempDir(), CorePath: corePath(t)})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(t.Context()) })
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(ts.Close)
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/gateway"

	for _, tc := range []struct {
		origin string
		ok     bool
	}{
		{"capacitor://localhost", true},
		{"https://localhost", true},
		{"dilla://app", true},
		{"https://evil.example", false},
		{"http://localhost", false},
		{"https://localhost:8443", false},
		{"capacitor://evil.example", false},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{ //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
				Subprotocols: []string{"dilla.v1"},
				HTTPHeader:   http.Header{"Origin": {tc.origin}},
			})
			if tc.ok {
				if err != nil {
					t.Fatalf("dial from %s: %v", tc.origin, err)
				}
				_ = c.CloseNow()
				return
			}
			if err == nil {
				_ = c.CloseNow()
				t.Fatalf("dial from %s was accepted", tc.origin)
			}
			if resp == nil || resp.StatusCode != http.StatusForbidden {
				t.Fatalf("dial from %s: %v (response %v), want 403", tc.origin, err, resp)
			}
		})
	}
}

// The instance's /v1/ and /i/ routes answer the app's preflight; other paths do not. /desktop-auth/
// stays strict on purpose (task 4 review ruling): internal/desktopauth refuses any foreign Origin,
// so the mobile app calls it through native HTTP with no Origin header, as the desktop main
// process does, and a WebView preflight there gets no Allow-* header.
func TestMobileOriginsCORSRoutes(t *testing.T) {
	h, err := NewHost(t.Context(), HostOptions{DataDir: t.TempDir(), CorePath: corePath(t)})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(t.Context()) })
	for _, tc := range []struct {
		path string
		cors bool
	}{
		{"/v1/auth/login", true},
		{"/v1/instance", true},
		{"/i/abc", true},
		{"/desktop-auth/start", false},
		{"/healthz", false},
		{"/", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, tc.path, nil)
			r.Header.Set("Origin", "capacitor://localhost")
			r.Header.Set("Access-Control-Request-Method", "POST")
			r.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
			w := httptest.NewRecorder()
			h.Handler().ServeHTTP(w, r)
			got := w.Header().Get("Access-Control-Allow-Origin")
			if tc.cors && (w.Code != http.StatusNoContent || got != "capacitor://localhost") {
				t.Fatalf("preflight %s = %d, Allow-Origin %q", tc.path, w.Code, got)
			}
			if !tc.cors {
				for name := range w.Header() {
					if strings.HasPrefix(name, "Access-Control-Allow-") {
						t.Fatalf("preflight %s got %s: %q", tc.path, name, w.Header().Get(name))
					}
				}
			}
		})
	}
}

// Every response under the CORS prefixes varies on Origin, a request without one included, so a
// shared cache never hands a same-origin response to the app or the reverse.
func TestMobileOriginsVaryOnOrigin(t *testing.T) {
	h, err := NewHost(t.Context(), HostOptions{DataDir: t.TempDir(), CorePath: corePath(t)})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(t.Context()) })
	for _, origin := range []string{"", "capacitor://localhost", "https://evil.example"} {
		t.Run("origin="+origin, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/instance", nil)
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
			w := httptest.NewRecorder()
			h.Handler().ServeHTTP(w, r)
			if !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "Origin") {
				t.Fatalf("GET /v1/instance from %q: Vary = %q, want Origin", origin, w.Header().Values("Vary"))
			}
		})
	}
}

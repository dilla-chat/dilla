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

// The instance's /v1/, /i/ and /desktop-auth/ routes answer the app's preflight; other paths do not.
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
		{"/desktop-auth/start", true},
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
			if !tc.cors && got != "" {
				t.Fatalf("preflight %s got Allow-Origin %q", tc.path, got)
			}
		})
	}
}

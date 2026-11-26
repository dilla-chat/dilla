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

// The gateway's upgrade accepts only an Origin that names the request's own host or one of
// http.trusted_origins (default dilla://app), scheme included. The retired Capacitor origins are
// refused like any other foreign origin.
func TestGatewayOriginCheck(t *testing.T) {
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
		{"dilla://app", true},
		{"https://evil.example", false},
		{"http://localhost", false},
		{"capacitor://localhost", false},
		{"https://localhost", false},
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

// No route answers a cross-origin preflight: an OPTIONS request with an Origin header gets no
// Access-Control-* header on /v1/, /i/ or /desktop-auth/.
func TestPreflightGetsNoCORSHeaders(t *testing.T) {
	h, err := NewHost(t.Context(), HostOptions{DataDir: t.TempDir(), CorePath: corePath(t)})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(t.Context()) })
	for _, path := range []string{"/v1/auth/login", "/v1/instance", "/i/abc", "/desktop-auth/start", "/desktop-auth/"} {
		for _, origin := range []string{"capacitor://localhost", "https://localhost", "https://evil.example"} {
			t.Run(path+" from "+origin, func(t *testing.T) {
				r := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, path, nil)
				r.Header.Set("Origin", origin)
				r.Header.Set("Access-Control-Request-Method", "POST")
				r.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
				w := httptest.NewRecorder()
				h.Handler().ServeHTTP(w, r)
				for name := range w.Header() {
					if strings.HasPrefix(name, "Access-Control-") {
						t.Fatalf("preflight %s got %s: %q", path, name, w.Header().Get(name))
					}
				}
			})
		}
	}
}

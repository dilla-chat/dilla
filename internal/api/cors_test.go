package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
)

var mobileOrigins = []string{"capacitor://localhost", "https://localhost"}

// corsTarget is a handler behind CORS that records whether the request reached it.
func corsTarget(reached *bool) http.Handler {
	return api.CORS(mobileOrigins)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	}))
}

func allowHeaders(h http.Header) []string {
	var out []string
	for name := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "Access-Control-Allow-") {
			out = append(out, name)
		}
	}
	return out
}

func TestCORSPreflightAllowedOrigin(t *testing.T) {
	for _, origin := range mobileOrigins {
		t.Run(origin, func(t *testing.T) {
			var reached bool
			r := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/auth/login", nil)
			r.Header.Set("Origin", origin)
			r.Header.Set("Access-Control-Request-Method", "POST")
			r.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
			w := httptest.NewRecorder()
			corsTarget(&reached).ServeHTTP(w, r)

			if w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", w.Code)
			}
			if reached {
				t.Fatal("a preflight reached the handler")
			}
			h := w.Header()
			if got := h.Get("Access-Control-Allow-Origin"); got != origin {
				t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, origin)
			}
			allowed := strings.ToLower(h.Get("Access-Control-Allow-Headers"))
			for _, want := range []string{"authorization", "content-type"} {
				if !strings.Contains(allowed, want) {
					t.Fatalf("Access-Control-Allow-Headers = %q, missing %q", allowed, want)
				}
			}
			if got := h.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
				t.Fatalf("Access-Control-Allow-Methods = %q, missing POST", got)
			}
			if got := h.Get("Access-Control-Max-Age"); got != "600" {
				t.Fatalf("Access-Control-Max-Age = %q, want 600", got)
			}
			if !strings.Contains(strings.Join(h.Values("Vary"), ","), "Origin") {
				t.Fatalf("Vary = %q, missing Origin", h.Values("Vary"))
			}
			if _, ok := h["Access-Control-Allow-Credentials"]; ok {
				t.Fatal("Access-Control-Allow-Credentials is set; the app sends bearer tokens, never cookies")
			}
		})
	}
}

func TestCORSActualRequestAllowedOrigin(t *testing.T) {
	var reached bool
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/instance", nil)
	r.Header.Set("Origin", "capacitor://localhost")
	w := httptest.NewRecorder()
	corsTarget(&reached).ServeHTTP(w, r)
	if !reached || w.Code != http.StatusOK {
		t.Fatalf("reached = %v, status = %d", reached, w.Code)
	}
	h := w.Header()
	if got := h.Get("Access-Control-Allow-Origin"); got != "capacitor://localhost" {
		t.Fatalf("Access-Control-Allow-Origin = %q", got)
	}
	exposed := strings.ToLower(h.Get("Access-Control-Expose-Headers"))
	for _, want := range []string{"x-dilla-generation", "retry-after"} {
		if !strings.Contains(exposed, want) {
			t.Fatalf("Access-Control-Expose-Headers = %q, missing %q", exposed, want)
		}
	}
	if _, ok := h["Access-Control-Allow-Credentials"]; ok {
		t.Fatal("Access-Control-Allow-Credentials is set")
	}
}

func TestCORSUnknownOrigin(t *testing.T) {
	// A neighbour of an allowed origin (same host, other scheme or port) is unknown too.
	for _, origin := range []string{"https://evil.example", "http://localhost", "https://localhost:8443", "capacitor://evil.example"} {
		for _, method := range []string{http.MethodGet, http.MethodOptions} {
			t.Run(origin+" "+method, func(t *testing.T) {
				var reached bool
				r := httptest.NewRequestWithContext(t.Context(), method, "/v1/instance", nil)
				r.Header.Set("Origin", origin)
				r.Header.Set("Access-Control-Request-Method", "POST")
				w := httptest.NewRecorder()
				corsTarget(&reached).ServeHTTP(w, r)
				if got := allowHeaders(w.Header()); len(got) != 0 {
					t.Fatalf("unknown origin got %v", got)
				}
				if !reached {
					t.Fatal("the request did not reach the handler")
				}
			})
		}
	}
}

func TestCORSNoOrigin(t *testing.T) {
	var reached bool
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/instance", nil)
	w := httptest.NewRecorder()
	corsTarget(&reached).ServeHTTP(w, r)
	if !reached || w.Code != http.StatusOK {
		t.Fatalf("reached = %v, status = %d", reached, w.Code)
	}
	// Only Vary: Origin is added, so a cache keeps the same-origin and cross-origin answers apart.
	if got := w.Header().Values("Vary"); len(got) != 1 || got[0] != "Origin" {
		t.Fatalf("Vary = %q, want [Origin]", got)
	}
	if got := allowHeaders(w.Header()); len(got) != 0 {
		t.Fatalf("a same-origin request gained %v", got)
	}
	if len(w.Header()) != 1 {
		t.Fatalf("a same-origin request gained headers: %v", w.Header())
	}
}

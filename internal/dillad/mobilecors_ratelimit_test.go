package dillad_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/dillad"
)

// A preflight is answered by the CORS middleware with a fixed, empty 204 and never reaches a
// handler, so it spends no rate-limit token. That is not a bypass: the limiter meters handlers
// (server.RateLimiter.Allow inside each route), and a preflight does no handler work — no body
// read, no database, no Argon2id — and returns no data, so it is constant-cost and cannot amplify.
// The actual request that follows is metered as before. This pins both halves on GET /i/{code},
// metered on the invite bucket (burst 5): many preflights first, then the bucket still holds its
// full burst for real requests, and once real requests drain it a preflight is still a plain 204.
func TestMobileOriginsPreflightIsConstantCostAndUnmetered(t *testing.T) {
	cfg := testConfig(t)
	srv, err := dillad.New(t.Context(), dillad.Options{Config: cfg, Clock: clock.System(), Wasm: sharedRuntime(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	h := srv.Handler()
	if !cfg.Limits.Rate.Enabled {
		t.Fatal("rate limiting is off in the test config; this test needs it on")
	}
	preflight := func() *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/i/abc", nil)
		r.Header.Set("Origin", "capacitor://localhost")
		r.Header.Set("Access-Control-Request-Method", "GET")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	get := func() int {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/i/abc", nil)
		r.Header.Set("Origin", "capacitor://localhost")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := range 50 {
		if w := preflight(); w.Code != http.StatusNoContent || w.Body.Len() != 0 {
			t.Fatalf("preflight %d = %d with %d body bytes, want an empty 204", i, w.Code, w.Body.Len())
		}
	}
	burst := cfg.Limits.Rate.InviteBurst
	for i := range burst {
		if code := get(); code == http.StatusTooManyRequests {
			t.Fatalf("GET %d after 50 preflights was rate limited: preflights spent invite tokens", i)
		}
	}
	if code := get(); code != http.StatusTooManyRequests {
		t.Fatalf("GET past the invite burst = %d, want 429 (the actual request is still metered)", code)
	}
	if w := preflight(); w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("preflight after the bucket drained = %d, want an empty 204", w.Code)
	}
}

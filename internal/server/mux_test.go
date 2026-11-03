package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/server"
)

func TestConflictingPatternsPanicAtRegistration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("two conflicting patterns registered without a panic")
		}
	}()
	m := server.NewMux()
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	m.Handle("GET /v1/groups/{id}", h)
	m.Handle("GET /v1/groups/{other}", h)
}

// Patterns lists what was registered through the Mux and through its Wrapped views alike, so the
// route sweep sees the routes a handler group mounts behind the session middleware.
func TestPatternsListsTheMuxAndItsWrappedViews(t *testing.T) {
	m := server.NewMux()
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	m.Handle("GET /v1/a", h)
	m.Wrapped(func(_ string, h http.Handler) http.Handler { return h }).Handle("POST /v1/b/{id}", h)
	got := m.Patterns()
	if len(got) != 2 || got[0] != "GET /v1/a" || got[1] != "POST /v1/b/{id}" {
		t.Fatalf("Patterns = %q, want both registrations in order", got)
	}
}

func TestAutomatic405CarriesAllow(t *testing.T) {
	m := server.NewMux()
	m.Handle("POST /v1/accounts", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv := httptest.NewServer(m)
	defer srv.Close()
	res, err := httpGet(t, srv.URL+"/v1/accounts")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.StatusCode)
	}
	_ = m.Mux() // the extension point 1b and 2 register through must exist
	if res.Header.Get("Allow") == "" {
		t.Fatal("the automatic 405 carries no Allow header")
	}
}

// httpGet is http.Get bound to the test's context.
func httpGet(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return http.DefaultClient.Do(req)
}

// Wrapped is how the composition root mounts a handler group that registers bare: every pattern
// registered through the view reaches the one ServeMux wrapped, the wrapper sees the pattern, and a
// pattern registered on the plain mux is left alone.
func TestAWrappedViewWrapsEveryRouteItRegisters(t *testing.T) {
	m := server.NewMux()
	var seen []string
	view := m.Wrapped(func(pattern string, h http.Handler) http.Handler {
		seen = append(seen, pattern)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Wrapped", pattern)
			h.ServeHTTP(w, r)
		})
	})
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	view.HandleFunc("GET /v1/wrapped", ok)
	view.Handle("POST /v1/wrapped", http.HandlerFunc(ok))
	m.HandleFunc("GET /v1/plain", ok)
	if len(seen) != 2 || seen[0] != "GET /v1/wrapped" || seen[1] != "POST /v1/wrapped" {
		t.Fatalf("the wrapper saw %q", seen)
	}
	for _, c := range []struct{ method, path, want string }{
		{http.MethodGet, "/v1/wrapped", "GET /v1/wrapped"},
		{http.MethodPost, "/v1/wrapped", "POST /v1/wrapped"},
		{http.MethodGet, "/v1/plain", ""},
	} {
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), c.method, c.path, nil))
		if rec.Code != http.StatusNoContent || rec.Header().Get("X-Wrapped") != c.want {
			t.Errorf("%s %s = %d, X-Wrapped %q, want 204 and %q", c.method, c.path, rec.Code,
				rec.Header().Get("X-Wrapped"), c.want)
		}
	}
}

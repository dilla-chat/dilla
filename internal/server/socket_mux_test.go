package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/server"
)

// A socket route is registered on the socket-only ServeMux through the same wrap as an HTTP route,
// and the HTTP mux never answers it.
func TestSocketRoutesAreOnlyOnTheSocketMux(t *testing.T) {
	m := server.NewMux()
	wrapped := 0
	view := m.Wrapped(func(_ string, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wrapped++
			h.ServeHTTP(w, r)
		})
	})
	view.Socket("GET /v1/socket/{id}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/socket/1", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("the HTTP mux answered a socket route with %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	m.SocketMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/socket/1", nil))
	if rec.Code != http.StatusTeapot || wrapped != 1 {
		t.Fatalf("the socket mux answered %d, wrap ran %d times", rec.Code, wrapped)
	}
	if got := m.SocketPatterns(); len(got) != 1 || got[0] != "GET /v1/socket/{id}" {
		t.Fatalf("SocketPatterns = %v", got)
	}
	for _, p := range m.Patterns() {
		if p == "GET /v1/socket/{id}" {
			t.Fatal("Patterns lists a socket route")
		}
	}
	if m.SocketCap("GET /v1/socket/{id}") != server.DefaultSocketBodyCap {
		t.Fatalf("default socket cap = %d", m.SocketCap("GET /v1/socket/{id}"))
	}
	view.SocketWithCap("POST /v1/socket/small", 64, http.NotFoundHandler())
	if m.SocketCap("POST /v1/socket/small") != 64 {
		t.Fatal("SocketWithCap did not record its cap")
	}
}

func TestConflictingSocketPatternsPanicAtRegistration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("two conflicting socket patterns registered without a panic")
		}
	}()
	m := server.NewMux()
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	m.Socket("GET /v1/x/{id}", h)
	m.Socket("GET /v1/x/{other}", h)
}

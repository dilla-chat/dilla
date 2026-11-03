package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/server"
)

// registerInstance already guards `d.Config != nil` when it decides whether
// GET /v1/instance is enrolled-only, so a Deps without a Config is a shape the
// package's own routing table admits — and three tests in this package build
// exactly that. Both handlers then read d.Config.Auth, d.Config.Instance and
// d.Config.Limits unconditionally, which is a nil dereference: in production
// server.Recover turns it into a 500, but internal/api mounts no Recover, so
// the panic takes the whole test binary down instead of failing one test.
//
// The precedent is TestDeleteMeWithoutAConfigRefusesRatherThanSkippingTheStepUp
// in accounts_delete_test.go: a missing config fails CLOSED with a 500.
func TestTheInstanceRoutesWithoutAConfigFailClosed(t *testing.T) {
	_, deps := newTestAPI(t)
	unwired := deps
	unwired.Config = nil
	m := server.NewMux()
	api.Register(m, unwired)

	for _, path := range []string{"/v1/instance", "/v1/instance/limits"} {
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("GET %s = %d with no config, want 500: a missing config must fail closed", path, rec.Code)
		}
	}
}

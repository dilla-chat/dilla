package api_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/config"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
)

// The Bot session wrapper owns rate metering. A single request must spend only
// one token, including when the resource handler refuses it after authentication.
func TestBotInteractionRoutesSpendOneRateTokenPerRequest(t *testing.T) {
	for _, route := range []struct{ method, suffix string }{
		{http.MethodGet, "/info"},
		{http.MethodGet, "/handshakes"},
		{http.MethodGet, "/messages"},
		{http.MethodPost, "/cursor"},
	} {
		t.Run(route.suffix, func(t *testing.T) {
			handler, deps := newTestAPIWithConfig(t, func(c *config.Config) {
				c.Limits.Rate.ReadBurst, c.Limits.Rate.WriteBurst = 1, 1
				c.Limits.Rate.ReadPerSecond, c.Limits.Rate.WritePerSecond = 0.001, 0.001
			})
			mux := handler.(*server.Mux)
			groups := &api.Groups{Limiter: deps.Limiter}
			groups.Register(mux, deps.Sessions)
			_, _, token := seedAPISession(t, deps)
			path := "/v1/bot-runtime/installations/" + id.New().String() + "/interaction-groups/" + id.New().String() + route.suffix
			call := func() int {
				request := httptest.NewRequestWithContext(t.Context(), route.method, path, bytes.NewReader(mustCBOR(t, []any{uint64(0), uint64(1)})))
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("Content-Type", "application/cbor")
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, request)
				return response.Code
			}
			if status := call(); status != http.StatusServiceUnavailable {
				t.Fatalf("first request = %d, want handler's 503 after spending one token", status)
			}
			if status := call(); status != http.StatusTooManyRequests {
				t.Fatalf("second request = %d, want 429 after exhausting the burst", status)
			}
		})
	}
}

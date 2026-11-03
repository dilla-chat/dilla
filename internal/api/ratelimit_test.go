package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

// registrationBody is a fresh, well-formed POST /v1/accounts body. Every call
// mints a new username and device id, so nothing but the bucket can refuse the
// second and later requests.
func registrationBody(t *testing.T, code string) []byte {
	t.Helper()
	name := "user" + id.New().String()[:8]
	return validHumanSignupBody(t, code, name, name, nil)
}

// refusalCode decodes the first element of the CBOR error array.
func refusalCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refusal: %v (body %x)", err, rec.Body.Bytes())
	}
	if len(body) < 1 {
		t.Fatalf("refusal body has %d elements", len(body))
	}
	s, _ := body[0].(string)
	return s
}

// The three unauthenticated routes are the only /v1 surface a caller reaches
// without a bearer token, so each one is metered on its own bucket keyed by the
// client address. Without this, dilla.toml's register_* and invite_* knobs
// configure nothing and the registration path is free to hammer.
func TestTheUnauthenticatedRoutesCarryTheirOwnBuckets(t *testing.T) {
	t.Run("POST /v1/accounts is on the register bucket", func(t *testing.T) {
		h, deps := newTestAPI(t)
		code := seedInvite(t, deps, 1000)
		burst := deps.Config.Limits.Rate.RegisterBurst
		for i := 0; i < burst; i++ {
			if rec := post(t, h, "/v1/accounts", registrationBody(t, code)); rec.Code == http.StatusTooManyRequests {
				t.Fatalf("registration %d of %d was refused inside the burst", i+1, burst)
			}
		}
		rec := post(t, h, "/v1/accounts", registrationBody(t, code))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("registration %d = %d, want 429: the register bucket is not applied", burst+1, rec.Code)
		}
		if got := refusalCode(t, rec); got != "E_RATE_LIMITED" {
			t.Fatalf("refusal code = %q, want E_RATE_LIMITED", got)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Fatal("a rate refusal carries no Retry-After header")
		}
	})

	t.Run("POST /v1/invites/redeem is on the invite bucket", func(t *testing.T) {
		h, deps := newTestAPI(t)
		code := seedInvite(t, deps, 1000)
		body, err := cborx.Marshal([]any{code})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		burst := deps.Config.Limits.Rate.InviteBurst
		for i := 0; i < burst; i++ {
			if rec := post(t, h, "/v1/invites/redeem", body); rec.Code == http.StatusTooManyRequests {
				t.Fatalf("redemption %d of %d was refused inside the burst", i+1, burst)
			}
		}
		rec := post(t, h, "/v1/invites/redeem", body)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("redemption %d = %d, want 429: the invite bucket is not applied", burst+1, rec.Code)
		}
		if got := refusalCode(t, rec); got != "E_RATE_LIMITED" {
			t.Fatalf("refusal code = %q, want E_RATE_LIMITED", got)
		}
	})

	t.Run("GET /i/{code} is on the invite bucket", func(t *testing.T) {
		h, deps := newTestAPI(t)
		code := seedInvite(t, deps, 1000)
		get := func() *httptest.ResponseRecorder {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/i/"+code, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec
		}
		burst := deps.Config.Limits.Rate.InviteBurst
		for i := 0; i < burst; i++ {
			if rec := get(); rec.Code == http.StatusTooManyRequests {
				t.Fatalf("landing fetch %d of %d was refused inside the burst", i+1, burst)
			}
		}
		if rec := get(); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("landing fetch %d = %d, want 429: the invite bucket is not applied", burst+1, rec.Code)
		}
	})
}

// A Deps built without a limiter must refuse rather than serve the metered
// routes unmetered: a missing dependency that silently removes a defence is the
// one failure mode a test cannot see afterwards.
func TestAnUnwiredLimiterRefusesRatherThanServingUnmetered(t *testing.T) {
	_, deps := newTestAPI(t)
	code := seedInvite(t, deps, 1000)

	unwired := deps
	unwired.Limiter = nil
	m := server.NewMux()
	api.Register(m, unwired)

	rec := post(t, m, "/v1/accounts", registrationBody(t, code))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: a nil limiter must fail closed, not open", rec.Code)
	}
	if got := refusalCode(t, rec); got != "E_INTERNAL" {
		t.Fatalf("refusal code = %q, want E_INTERNAL", got)
	}
}

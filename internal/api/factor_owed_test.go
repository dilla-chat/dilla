package api_test

// factor_owed_test.go: web3.6 S1 at the api package's mux. An account registered through
// POST /v1/accounts owes a second factor: its session reaches the `E, F` routes and is refused
// E_FACTOR_REQUIRED elsewhere, until the flag is cleared. satisfyFactor is the helper later tasks'
// Go tests use when they register through the real route and are about something else.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// satisfyFactor clears the account's owed second factor through the repository. It stands in for
// the factor enrolment in a test about something else; there is no production seam for it.
func satisfyFactor(t *testing.T, repo store.Repository, uid id.ID) {
	t.Helper()
	if err := repo.SetFactorOwed(context.Background(), uid, false); err != nil {
		t.Fatalf("SetFactorOwed(%s, false): %v", uid, err)
	}
}

func TestASignUpOwesASecondFactorUntilItIsSatisfied(t *testing.T) {
	h, deps := newTestAPI(t)
	code := seedInvite(t, deps, 1)
	body := validHumanSignupBody(t, code, "owing", "Owing", "correct horse battery staple")
	res := post(t, h, "/v1/accounts", body)
	if res.Code != http.StatusOK {
		t.Fatalf("POST /v1/accounts = %d (%x)", res.Code, res.Body.Bytes())
	}
	var out []any
	if err := cborx.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, _ := out[0].([]byte)
	uid := id.ID(raw)
	token, _ := out[2].(string)
	if u, err := deps.Repo.GetUser(context.Background(), uid); err != nil || !u.FactorOwed {
		t.Fatalf("the new account = %+v, %v; want it to owe a second factor", u, err)
	}

	get := func(path string) (int, string) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var refusal []any
		_ = cborx.Unmarshal(rec.Body.Bytes(), &refusal)
		code := ""
		if rec.Code >= 400 && len(refusal) > 0 {
			code, _ = refusal[0].(string)
		}
		return rec.Code, code
	}
	if status, code := get("/v1/accounts/me"); status != http.StatusOK {
		t.Fatalf("GET /v1/accounts/me as an owed session = %d %s, want 200", status, code)
	}
	if status, code := get("/v1/devices"); status != http.StatusForbidden || code != "E_FACTOR_REQUIRED" {
		t.Fatalf("GET /v1/devices as an owed session = %d %s, want 403 E_FACTOR_REQUIRED", status, code)
	}

	satisfyFactor(t, deps.Repo, uid)
	if status, code := get("/v1/devices"); status != http.StatusOK {
		t.Fatalf("GET /v1/devices once satisfied = %d %s, want 200", status, code)
	}
}

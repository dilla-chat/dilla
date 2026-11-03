package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/server"
)

// deleteMe sends DELETE /v1/accounts/me with the bearer token.
func deleteMe(h http.Handler, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/v1/accounts/me", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// DELETE /v1/accounts/me tombstones the account keeping its username (R36) and
// takes every session of every device with it.
func TestDeletingAnAccountTombstonesItAndRevokesEverySession(t *testing.T) {
	h, deps := newTestAPI(t)
	ctx := context.Background()
	u, dev, token := seedSession(t, deps)

	if rec := deleteMe(h, token); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /v1/accounts/me = %d, want 204 (body %x)", rec.Code, rec.Body.Bytes())
	}
	row, err := deps.Repo.GetUserByUsername(ctx, u.Username)
	if err != nil {
		t.Fatalf("the username was not kept reserved: %v", err)
	}
	if row.DeletedAt == nil {
		t.Fatal("the account was not tombstoned")
	}
	if row.DisabledAt == nil {
		t.Fatal("the tombstoned account is still enabled")
	}
	n, err := deps.Repo.CountSessionsByDevice(ctx, dev.ID)
	if err != nil {
		t.Fatalf("CountSessionsByDevice: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d sessions survived the deletion", n)
	}
}

// The route is marked "E (step-up)". The step-up this part of the plan can
// enforce is the session's own freshness, so a session older than
// auth.session.reauth_window must be refused.
func TestAStaleSessionCannotDeleteTheAccount(t *testing.T) {
	h, deps := newTestAPI(t)
	ctx := context.Background()
	u, _, token := seedSession(t, deps)

	fake, ok := deps.Clock.(*clock.Fake)
	if !ok {
		t.Fatalf("the test clock is %T, not *clock.Fake", deps.Clock)
	}
	fake.Advance(deps.Config.Auth.Session.ReauthWindow.Value() + time.Second)

	rec := deleteMe(h, token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a stale session must not be able to delete the account", rec.Code)
	}
	row, err := deps.Repo.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if row.DeletedAt != nil {
		t.Fatal("the refused request tombstoned the account anyway")
	}
}

// A Deps without a Config must not silently drop the freshness check: a missing
// dependency that removes the only gate this route has is fail-open, and a
// handler test that built Deps without a Config would exercise no gate at all.
func TestDeleteMeWithoutAConfigRefusesRatherThanSkippingTheStepUp(t *testing.T) {
	_, deps := newTestAPI(t)
	ctx := context.Background()
	u, _, token := seedSession(t, deps)

	unwired := deps
	unwired.Config = nil
	m := server.NewMux()
	api.Register(m, unwired)

	rec := deleteMe(m, token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: a missing config must fail closed", rec.Code)
	}
	row, err := deps.Repo.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if row.DeletedAt != nil {
		t.Fatal("the account was tombstoned with no step-up enforced at all")
	}
}

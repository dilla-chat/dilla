package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/store"
)

// Wave B task 3b (Jonas, 2026-10-30): a password set at registration or by a change has at least 12 characters
// (code points, as the web client counts them). Eleven is refused 400 before anything is stored or hashed; twelve,
// including twelve multi-byte characters, is taken.
func TestARegistrationPasswordHasTwelveCharacters(t *testing.T) {
	h, deps := newTestAPI(t)
	code := seedInvite(t, deps, 3)

	res := post(t, h, "/v1/accounts", validHumanSignupBody(t, code, "eleven", "", "elevenchars"))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("an 11-character password: status = %d, want 400", res.Code)
	}
	if _, err := deps.Repo.GetUserByUsername(context.Background(), "eleven"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused registration left an account (err %v)", err)
	}
	if res := post(t, h, "/v1/accounts", validHumanSignupBody(t, code, "twelve", "", "twelve chars")); res.Code != http.StatusOK {
		t.Fatalf("a 12-character password: status = %d, want 200", res.Code)
	}
	// Twelve code points of two bytes each: counted as 12, not 24 and not refused.
	if res := post(t, h, "/v1/accounts", validHumanSignupBody(t, code, "wide", "", "åäöåäöåäöåäö")); res.Code != http.StatusOK {
		t.Fatalf("12 two-byte characters: status = %d, want 200", res.Code)
	}
	if status, _ := login(t, h, "twelve", "twelve chars"); status != http.StatusOK {
		t.Fatalf("the 12-character password does not log in: %d", status)
	}
}

// No password (a passkey-only registration) is not measured.
func TestARegistrationWithoutAPasswordIsNotMeasured(t *testing.T) {
	h, deps := newTestAPI(t)
	code := seedInvite(t, deps, 1)
	if res := post(t, h, "/v1/accounts", validHumanSignupBody(t, code, "nopass", "", nil)); res.Code != http.StatusOK {
		t.Fatalf("no password: status = %d, want 200", res.Code)
	}
}

// A change to a password below the minimum is refused before the old password is checked (no guess is spent) and the
// stored one stays; a password set before the minimum, shorter than 12, still signs in and can be changed to a long one.
func TestAChangedPasswordHasTwelveCharactersButAnOldShortOneStillSignsIn(t *testing.T) {
	h, deps := newTestAPI(t)
	u, _, token := seedAPISession(t, deps)
	old := seedPassword(t, deps, u, "short pw") // 8 characters: set before the minimum

	if status, _ := login(t, h, u.Username, old); status != http.StatusOK {
		t.Fatalf("an existing 8-character password does not sign in: %d", status)
	}
	body, _ := cborx.Marshal([]any{old, "elevenchars"})
	if res := postCBORAuth(h, "/v1/auth/password", body, token); res.Code != http.StatusBadRequest {
		t.Fatalf("a change to 11 characters: status = %d, want 400", res.Code)
	}
	// A wrong old password with a short new one: still the length refusal, so the old password was never compared.
	body, _ = cborx.Marshal([]any{"not the old one", "short"})
	if res := postCBORAuth(h, "/v1/auth/password", body, token); res.Code != http.StatusBadRequest {
		t.Fatalf("a short new password with a wrong old one: status = %d, want 400", res.Code)
	}
	if status, _ := login(t, h, u.Username, old); status != http.StatusOK {
		t.Fatalf("a refused change replaced the password: %d", status)
	}
	body, _ = cborx.Marshal([]any{old, "twelve chars"})
	if res := postCBORAuth(h, "/v1/auth/password", body, token); res.Code != http.StatusNoContent {
		t.Fatalf("a change to 12 characters: status = %d, want 204", res.Code)
	}
	if status, _ := login(t, h, u.Username, "twelve chars"); status != http.StatusOK {
		t.Fatalf("the new password does not sign in: %d", status)
	}
}

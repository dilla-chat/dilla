package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

// A display name stored before the rule existed (an invisible character in it) is kept: it still reads back, and a
// status-only PATCH works. Only a changed display name is checked.
func TestALegacyDisplayNameIsKeptAndDoesNotBlockOtherFields(t *testing.T) {
	h, deps := newTestAPI(t)
	user, _, token := seedSession(t, deps)
	ctx := context.Background()
	legacy := "Ada" + string(rune(0x200b)) + "L"
	if _, err := deps.Repo.UpdateProfile(ctx, user.ID, profileStr(legacy), nil, nil); err != nil {
		t.Fatal(err)
	}
	var me []cbor.RawMessage
	if err := cbor.Unmarshal(doAuth(h, http.MethodGet, "/v1/accounts/me", token).Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	var shown string
	if err := cbor.Unmarshal(me[2], &shown); err != nil || shown != legacy {
		t.Fatalf("GET /me display = %q, %v; want the stored name", shown, err)
	}
	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{nil, "still works", nil}); rec.Code != http.StatusNoContent {
		t.Fatalf("status-only patch = %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := deps.Repo.GetUser(ctx, user.ID)
	if got.Display != legacy || got.StatusMsg != "still works" {
		t.Fatalf("stored %q and %q", got.Display, got.StatusMsg)
	}
	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{legacy, nil, nil}); rec.Code != http.StatusBadRequest {
		t.Fatalf("sending the legacy name back = %d, want 400 (a changed name is checked)", rec.Code)
	}
}

// The server trims the display name and refuses a whitespace-only one.
func TestPatchMeTrimsTheDisplayNameAndRefusesABlankOne(t *testing.T) {
	h, deps := newTestAPI(t)
	user, _, token := seedSession(t, deps)
	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{"  Ada  ", nil, nil}); rec.Code != http.StatusNoContent {
		t.Fatalf("patch = %d", rec.Code)
	}
	if got, _ := deps.Repo.GetUser(context.Background(), user.ID); got.Display != "Ada" {
		t.Fatalf("stored %q, want it trimmed", got.Display)
	}
	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{"   ", nil, nil}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a whitespace-only name = %d, want 400", rec.Code)
	}
}

// The scripts that need a joiner can still set their names and texts.
func TestPatchMeAcceptsPersianAndSinhala(t *testing.T) {
	h, deps := newTestAPI(t)
	user, _, token := seedSession(t, deps)
	persian := string([]rune{0x645, 0x6cc, 0x200c, 0x62e, 0x648, 0x627, 0x647, 0x645})
	sinhala := string([]rune{0xdc1, 0xdca, 0x200d, 0xdbb, 0xdd3})
	if rec := cborCall(t, h, http.MethodPatch, "/v1/accounts/me", token, []any{persian, sinhala, nil}); rec.Code != http.StatusNoContent {
		t.Fatalf("patch = %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := deps.Repo.GetUser(context.Background(), user.ID); got.Display != persian || got.StatusMsg != sinhala {
		t.Fatalf("stored %q and %q", got.Display, got.StatusMsg)
	}
}

package api_test

// device_label_test.go covers PATCH /v1/devices/{device_id} (EOD E7): the owner names a device,
// GET /v1/devices carries the name, and nobody else's session reaches it.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// seedDeviceFor adds a second device row to user and returns its id.
func seedDeviceFor(t *testing.T, deps api.Deps, user id.ID) id.ID {
	t.Helper()
	now := deps.Clock.Now().Unix()
	row := store.DeviceRow{ID: id.New(), UserID: user, DSKPub: make([]byte, 32), CredentialBlob: []byte{1},
		LastSeen: now, Created: now}
	if err := deps.Repo.CreateDevice(t.Context(), row); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return row.ID
}

func listedLabels(t *testing.T, h http.Handler, token string) map[id.ID]any {
	t.Helper()
	rec := cborCall(t, h, http.MethodGet, "/v1/devices", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/devices = %d %x", rec.Code, rec.Body.Bytes())
	}
	var rows [][]any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode the device list: %v", err)
	}
	out := map[id.ID]any{}
	for _, row := range rows {
		if len(row) != 7 {
			t.Fatalf("device row = %v, want 7 elements ending in the label", row)
		}
		raw, _ := row[0].([]byte)
		out[id.ID(raw)] = row[6]
	}
	return out
}

func TestTheOwnerNamesAndUnnamesADevice(t *testing.T) {
	h, deps := newTestAPI(t)
	_, dev, token := seedAPISession(t, deps)
	path := "/v1/devices/" + dev.ID.String()
	if got := listedLabels(t, h, token)[dev.ID]; got != nil {
		t.Fatalf("a new device's label = %v, want null", got)
	}
	if rec := cborCall(t, h, http.MethodPatch, path, token, []any{"work laptop"}); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	if got := listedLabels(t, h, token)[dev.ID]; got != "work laptop" {
		t.Fatalf("label after PATCH = %v, want work laptop", got)
	}
	if rec := cborCall(t, h, http.MethodPatch, path, token, []any{nil}); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH null = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	if got := listedLabels(t, h, token)[dev.ID]; got != nil {
		t.Fatalf("label after PATCH null = %v, want null", got)
	}
}

func TestADeviceNamesAnotherDeviceOfTheSameUser(t *testing.T) {
	h, deps := newTestAPI(t)
	user, _, token := seedAPISession(t, deps)
	sibling := seedDeviceFor(t, deps, user.ID)
	if rec := cborCall(t, h, http.MethodPatch, "/v1/devices/"+sibling.String(), token, []any{"phone"}); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH of a sibling device = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	if got := listedLabels(t, h, token)[sibling]; got != "phone" {
		t.Fatalf("sibling label = %v, want phone", got)
	}
}

func TestNobodyNamesAnotherUsersDevice(t *testing.T) {
	h, deps := newTestAPI(t)
	_, victim, victimToken := seedAPISession(t, deps)
	_, _, attackerToken := seedAPISession(t, deps)
	rec := cborCall(t, h, http.MethodPatch, "/v1/devices/"+victim.ID.String(), attackerToken, []any{"pwned"})
	if rec.Code != http.StatusNotFound || refusalCode(t, rec) != "E_NOT_FOUND" {
		t.Fatalf("another user's PATCH = %d %q, want 404 E_NOT_FOUND", rec.Code, refusalCode(t, rec))
	}
	if got := listedLabels(t, h, victimToken)[victim.ID]; got != nil {
		t.Fatalf("label after a foreign PATCH = %v, want null", got)
	}
	unknown := "/v1/devices/" + id.New().String()
	if rec := cborCall(t, h, http.MethodPatch, unknown, attackerToken, []any{"x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH of an unknown device = %d, want 404 like another user's", rec.Code)
	}
}

func TestADeviceLabelIsValidated(t *testing.T) {
	for name, body := range map[string]any{
		"empty":                                []any{""},
		"over 64 characters":                   []any{strings.Repeat("x", 65)},
		"a control byte":                       []any{"bad\x00name"},
		"a newline":                            []any{"two\nlines"},
		"no element":                           []any{},
		"two elements":                         []any{"a", "b"},
		"a number":                             []any{7},
		"whitespace only":                      []any{"   "},
		"a right-to-left override":             []any{"phone\u202eexe"},
		"a bidi isolate":                       []any{"a\u2066b"},
		"a right-to-left mark":                 []any{"a\u200fb"},
		"a left-to-right mark":                 []any{"a\u200eb"},
		"a line separator":                     []any{"a\u2028b"},
		"an arabic letter mark":                []any{"a\u061cb"},
		"a zero-width space":                   []any{"a\u200bb"},
		"a zero-width joiner":                  []any{"a\u200db"},
		"a byte order mark":                    []any{"a\ufeffb"},
		"a word joiner":                        []any{"a\u2060b"},
		"an invisible operator":                []any{"a\u2061b"},
		"the last invisible operator":          []any{"a\u2065b"},
		"a deprecated format character":        []any{"a\u206ab"},
		"the last deprecated format character": []any{"a\u206fb"},
		"a next-line control at the edge":      []any{"phone\u0085"},
		"a byte order mark at the edge":        []any{"\ufeffphone\ufeff"},
		"a tag character":                      []any{"a\U000E0041b"},
		"a variation selector after a letter":  []any{"a\ufe0fb"},
		"a braille blank":                      []any{"a\u2800b"},
	} {
		// One instance per case: the write bucket is small and a refusal must not hide behind a 429.
		h, deps := newTestAPI(t)
		_, dev, token := seedAPISession(t, deps)
		path := "/v1/devices/" + dev.ID.String()
		if rec := cborCall(t, h, http.MethodPatch, path, token, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: PATCH = %d, want 400", name, rec.Code)
		}
		if got := listedLabels(t, h, token)[dev.ID]; got != nil {
			t.Errorf("%s: label after the refused PATCH = %v, want null", name, got)
		}
	}
}

func TestADeviceLabelOf64CharactersIsAccepted(t *testing.T) {
	h, deps := newTestAPI(t)
	_, dev, token := seedAPISession(t, deps)
	path := "/v1/devices/" + dev.ID.String()
	// 64 characters, counted as characters and not bytes, is the limit.
	edge := strings.Repeat("é", 64)
	if rec := cborCall(t, h, http.MethodPatch, path, token, []any{edge}); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH of 64 two-byte characters = %d, want 204", rec.Code)
	}
}

// The stored form is the trimmed NFC one, the same normalisation display names get.
func TestADeviceLabelIsStoredTrimmedAndNormalised(t *testing.T) {
	h, deps := newTestAPI(t)
	_, dev, token := seedAPISession(t, deps)
	path := "/v1/devices/" + dev.ID.String()
	if rec := cborCall(t, h, http.MethodPatch, path, token, []any{"  caf\u0065\u0301  "}); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH = %d %x, want 204", rec.Code, rec.Body.Bytes())
	}
	if got := listedLabels(t, h, token)[dev.ID]; got != "caf\u00e9" {
		t.Fatalf("label = %q, want the trimmed NFC %q", got, "caf\u00e9")
	}
}

func TestARevokedDeviceCannotBeNamed(t *testing.T) {
	h, deps := newTestAPI(t)
	user, _, token := seedAPISession(t, deps)
	sibling := seedDeviceFor(t, deps, user.ID)
	path := "/v1/devices/" + sibling.String()
	if rec := cborCall(t, h, http.MethodPatch, path, token, []any{"old phone"}); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH of a live sibling = %d, want 204", rec.Code)
	}
	if err := deps.Repo.RevokeDevice(t.Context(), sibling, deps.Clock.Now().Unix()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	rec := cborCall(t, h, http.MethodPatch, path, token, []any{"new name"})
	if rec.Code != http.StatusNotFound || refusalCode(t, rec) != "E_NOT_FOUND" {
		t.Fatalf("PATCH of a revoked device = %d %q, want 404 E_NOT_FOUND", rec.Code, refusalCode(t, rec))
	}
	if got := listedLabels(t, h, token)[sibling]; got != "old phone" {
		t.Fatalf("label of the revoked device = %v, want old phone kept", got)
	}
}

// The route is mounted enrolled, not owedOK: a session that still owes a second factor names nothing.
func TestASessionOwingASecondFactorCannotNameADevice(t *testing.T) {
	h, deps := newTestAPI(t)
	user, dev, token := seedAPISession(t, deps)
	if err := deps.Repo.SetFactorOwed(t.Context(), user.ID, true); err != nil {
		t.Fatalf("SetFactorOwed: %v", err)
	}
	rec := cborCall(t, h, http.MethodPatch, "/v1/devices/"+dev.ID.String(), token, []any{"x"})
	if rec.Code != http.StatusForbidden || refusalCode(t, rec) != "E_FACTOR_REQUIRED" {
		t.Fatalf("PATCH as an owed session = %d %q, want 403 E_FACTOR_REQUIRED", rec.Code, refusalCode(t, rec))
	}
	if err := deps.Repo.SetFactorOwed(t.Context(), user.ID, false); err != nil {
		t.Fatalf("SetFactorOwed: %v", err)
	}
	if got := listedLabels(t, h, token)[dev.ID]; got != nil {
		t.Fatalf("label after the refused PATCH = %v, want null", got)
	}
}

func TestAPendingSessionCannotNameADevice(t *testing.T) {
	h, deps := newTestAPI(t)
	owner, dev, _ := seedAPIDevice(t, deps)
	listerOf(t, deps).list(owner.ID, dev)
	_, pending := pendingSession(t, h, deps, owner.ID)
	rec := cborCall(t, h, http.MethodPatch, "/v1/devices/"+dev.ID.String(), pending, []any{"x"})
	if rec.Code != http.StatusForbidden || refusalCode(t, rec) != "E_FORBIDDEN" {
		t.Fatalf("pending PATCH = %d %q, want 403 E_FORBIDDEN", rec.Code, refusalCode(t, rec))
	}
}

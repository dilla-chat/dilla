package api

import (
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/server"
)

// Bot v2 Task 1: a group the session's wire may not reach is a plain three-field 426
// E_UPGRADE_REQUIRED; there is no older wire left to list.
func TestGroupCapabilityRefusalMapsToUpgradeRequired(t *testing.T) {
	rec := httptest.NewRecorder()
	server.WriteError(rec, dsError(&capability.UpgradeRequired{}))
	if rec.Code != 426 {
		t.Fatalf("status = %d, want 426", rec.Code)
	}
	var body []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body) != 3 || body[0] != "E_UPGRADE_REQUIRED" || body[2] != nil {
		t.Fatalf("response = %#v, want three-field E_UPGRADE_REQUIRED with null retry slot", body)
	}
}

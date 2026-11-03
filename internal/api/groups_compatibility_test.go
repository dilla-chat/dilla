package api

import (
	"net/http/httptest"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/server"
)

func TestGroupCapabilityRefusalMapsToExactVersionResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	server.WriteError(rec, dsError(&capability.UpgradeRequired{}))
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body) != 6 || body[0] != "E_VERSION" || body[2] != nil {
		t.Fatalf("response = %#v, want six-field E_VERSION with null retry slot", body)
	}
	for i, want := range [][]uint64{{3}, {1}, {1}} {
		got, ok := body[i+3].([]any)
		if !ok || len(got) != len(want) {
			t.Fatalf("supported version field %d = %#v, want %v", i, body[i+3], want)
		}
		for j, version := range want {
			if got[j] != version {
				t.Fatalf("supported version field %d[%d] = %#v, want %d", i, j, got[j], version)
			}
		}
	}
}

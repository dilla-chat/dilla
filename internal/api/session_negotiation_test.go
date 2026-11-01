package api_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/server"
)

func TestSessionNegotiationSelectsHighestCommonVersionsPerLayer(t *testing.T) {
	h, deps := newTestAPI(t)
	_, _, token := seedAPISession(t, deps)
	body, err := cborx.Marshal([]any{
		[]any{uint64(2), uint64(3)},
		[]any{uint64(1)},
		[]any{uint64(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := postCBORAuth(h, "/v1/sessions/negotiate", body, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("negotiate = %d, want 200 (%q)", rec.Code, rec.Body.Bytes())
	}
	var selected []uint64
	if err := cborx.Unmarshal(rec.Body.Bytes(), &selected); err != nil {
		t.Fatal(err)
	}
	if len(selected) != 3 || selected[0] != 3 || selected[1] != 1 || selected[2] != 1 {
		t.Fatalf("selected versions = %v, want [3 1 1]", selected)
	}
}

func TestSessionNegotiationRejectsDuplicateVersionAndLeavesPriorSelectionUntouched(t *testing.T) {
	h, deps := newTestAPI(t)
	_, _, token := seedAPISession(t, deps)
	valid, _ := cborx.Marshal([]any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}})
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", valid, token); rec.Code != http.StatusOK {
		t.Fatalf("initial negotiation = %d, want 200", rec.Code)
	}
	invalid, _ := cborx.Marshal([]any{[]any{uint64(3), uint64(3)}, []any{uint64(1)}, []any{uint64(1)}})
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", invalid, token); rec.Code != http.StatusBadRequest || refusalCode(t, rec) != "E_INVALID_REQUEST" {
		t.Fatalf("duplicate version = %d %q, want 400 E_INVALID_REQUEST", rec.Code, refusalCode(t, rec))
	}
	s, err := deps.Sessions.Resolve(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if s.WireVersion != 3 || s.E2EEVersion != 1 || s.MediaVersion != 1 {
		t.Fatalf("stored versions after invalid request = [%d %d %d], want [3 1 1]", s.WireVersion, s.E2EEVersion, s.MediaVersion)
	}
}

func TestSessionNegotiationIsBearerScopedAndDowngradesTheSameBearer(t *testing.T) {
	h, deps := newTestAPI(t)
	user, _, tokenA := seedAPISession(t, deps)
	tokenB := secondSession(t, deps, user)
	selectVersions := func(token string, body []any) []uint64 {
		t.Helper()
		encoded, err := cborx.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := postCBORAuth(h, "/v1/sessions/negotiate", encoded, token)
		if rec.Code != http.StatusOK {
			t.Fatalf("negotiate = %d, want 200 (%q)", rec.Code, rec.Body.Bytes())
		}
		var selected []uint64
		if err := cborx.Unmarshal(rec.Body.Bytes(), &selected); err != nil {
			t.Fatal(err)
		}
		return selected
	}
	if got := selectVersions(tokenA, []any{[]any{uint64(2), uint64(3)}, []any{uint64(1)}, []any{uint64(1)}}); got[0] != 3 {
		t.Fatalf("initial wire selection = %v, want 3", got)
	}
	if got := selectVersions(tokenA, []any{[]any{uint64(2)}, []any{uint64(1)}, []any{uint64(1)}}); got[0] != 2 {
		t.Fatalf("downgrade selection = %v, want wire 2", got)
	}
	a, err := deps.Sessions.Resolve(context.Background(), tokenA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := deps.Sessions.Resolve(context.Background(), tokenB)
	if err != nil {
		t.Fatal(err)
	}
	if a.WireVersion != 2 || b.WireVersion != 0 {
		t.Fatalf("same-device bearer selections = %d and %d, want 2 and 0", a.WireVersion, b.WireVersion)
	}
}

func TestSessionNegotiationRejectsUnsupportedWithoutReplacingSelection(t *testing.T) {
	h, deps := newTestAPI(t)
	_, _, token := seedAPISession(t, deps)
	valid, _ := cborx.Marshal([]any{[]any{uint64(3), uint64(2)}, []any{uint64(1)}, []any{uint64(1)}})
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", valid, token); rec.Code != http.StatusOK {
		t.Fatalf("initial negotiation = %d, want 200", rec.Code)
	}
	unsupported, _ := cborx.Marshal([]any{[]any{uint64(1)}, []any{uint64(1)}, []any{uint64(1)}})
	rec := postCBORAuth(h, "/v1/sessions/negotiate", unsupported, token)
	if rec.Code != http.StatusBadRequest || refusalCode(t, rec) != "E_VERSION" {
		t.Fatalf("wire 1 negotiation = %d %q, want 400 E_VERSION", rec.Code, refusalCode(t, rec))
	}
	assertNegotiationVersionError(t, rec, [][]uint64{{5, 4, 3, 2}, {1}, {1}})
	s, err := deps.Sessions.Resolve(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if s.WireVersion != 3 {
		t.Fatalf("failed negotiation changed selected wire to %d, want 3", s.WireVersion)
	}
}

func TestSessionNegotiationUsesTheConfiguredWireOffer(t *testing.T) {
	h, deps := newTestAPIWired(t, func(d *api.Deps) { d.WireVersions = []uint64{2} })
	_, _, token := seedAPISession(t, deps)
	body, _ := cborx.Marshal([]any{[]any{uint64(3), uint64(2)}, []any{uint64(1)}, []any{uint64(1)}})
	rec := postCBORAuth(h, "/v1/sessions/negotiate", body, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("configured wire 2 negotiation = %d, want 200", rec.Code)
	}
	var selected []uint64
	if err := cborx.Unmarshal(rec.Body.Bytes(), &selected); err != nil {
		t.Fatal(err)
	}
	if len(selected) != 3 || selected[0] != 2 {
		t.Fatalf("selected = %v, want wire 2", selected)
	}
	unsupported, _ := cborx.Marshal([]any{[]uint64{3}, []uint64{1}, []uint64{1}})
	refused := postCBORAuth(h, "/v1/sessions/negotiate", unsupported, token)
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("unsupported configured offer = %d, want 400", refused.Code)
	}
	assertNegotiationVersionError(t, refused, [][]uint64{{2}, {1}, {1}})
}

func TestSessionNegotiationRequiresThreeNonemptyLists(t *testing.T) {
	h, deps := newTestAPI(t)
	_, _, token := seedAPISession(t, deps)
	for name, body := range map[string][]any{
		"wrong outer length": {[]any{uint64(3)}, []any{uint64(1)}},
		"empty list":         {[]any{}, []any{uint64(1)}, []any{uint64(1)}},
		"too many versions":  {[]any{uint64(3), uint64(2), uint64(4), uint64(5), uint64(6), uint64(7), uint64(8), uint64(9), uint64(10)}, []any{uint64(1)}, []any{uint64(1)}},
	} {
		encoded, err := cborx.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := postCBORAuth(h, "/v1/sessions/negotiate", encoded, token)
		if rec.Code != http.StatusBadRequest || refusalCode(t, rec) != "E_INVALID_REQUEST" {
			t.Errorf("%s = %d %q, want 400 E_INVALID_REQUEST", name, rec.Code, refusalCode(t, rec))
		}
	}
}

func TestDeletingTheBearerSessionDeletesItsNegotiatedSelection(t *testing.T) {
	h, deps := newTestAPI(t)
	_, device, token := seedAPISession(t, deps)
	body, _ := cborx.Marshal([]any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}})
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", body, token); rec.Code != http.StatusOK {
		t.Fatalf("negotiate = %d, want 200", rec.Code)
	}
	if _, err := deps.Sessions.DeleteDeviceSessions(context.Background(), device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Sessions.Resolve(context.Background(), token); err == nil {
		t.Fatal("deleted bearer still resolved with its capability selection")
	}
}

func TestSessionNegotiationRequiresBearerAndCannotBeGrantedByHeaders(t *testing.T) {
	h, deps := newTestAPI(t)
	rec := postCBOR(h, "/v1/sessions/negotiate", []byte{0x83, 0x81, 0x03, 0x81, 0x01, 0x81, 0x01})
	if rec.Code != http.StatusUnauthorized || refusalCode(t, rec) != "E_UNAUTHENTICATED" {
		t.Fatalf("headerless negotiation = %d %q, want 401 E_UNAUTHENTICATED", rec.Code, refusalCode(t, rec))
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/sessions/negotiate", bytes.NewReader([]byte{0x83, 0x81, 0x03, 0x81, 0x01, 0x81, 0x01}))
	request.Header.Set("Content-Type", "application/cbor")
	request.Header.Set("X-Dilla-Wire-Version", "3")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || refusalCode(t, response) != "E_UNAUTHENTICATED" {
		t.Fatalf("header negotiation = %d %q, want 401 E_UNAUTHENTICATED", response.Code, refusalCode(t, response))
	}
	_, _, token := seedAPISession(t, deps)
	request = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/accounts/me", bytes.NewReader([]byte{0x80}))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/cbor")
	request.Header.Set("X-Dilla-Wire-Version", "3")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	s, err := deps.Sessions.Resolve(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if s.WireVersion != 0 {
		t.Fatalf("header changed the stored wire version to %d", s.WireVersion)
	}
}

func TestBotSessionRoutePreservesEnrollmentAuthAndDefersResourceWireGate(t *testing.T) {
	h, deps := newTestAPI(t)
	_, _, token := seedAPISession(t, deps)
	mux := server.NewMux()
	guard := api.BotSessionRoute(deps.Sessions, deps.Limiter)
	mux.Wrapped(guard).Handle("POST /v1/bot/capability-check", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/bot/capability-check", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	rec := call()
	if rec.Code != http.StatusNoContent {
		t.Fatalf("bot route wrapper blocked handler before resource authorization: %d", rec.Code)
	}
	body, _ := cborx.Marshal([]any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}})
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", body, token); rec.Code != http.StatusOK {
		t.Fatalf("negotiate wire 3 = %d, want 200", rec.Code)
	}
	if rec := call(); rec.Code != http.StatusNoContent {
		t.Fatalf("negotiated bot guard = %d, want 204", rec.Code)
	}
}

// E_VERSION has three supported-version lists after the ordinary refusal fields
// (protocol/02). Clients must not have to parse the human-readable detail.
func assertNegotiationVersionError(t *testing.T, rec *httptest.ResponseRecorder, want [][]uint64) {
	t.Helper()
	var fields []any
	if err := cborx.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 6 || fields[0] != "E_VERSION" || fields[2] != nil {
		t.Fatalf("version refusal has %d fields, want code/detail/null and three version lists", len(fields))
	}
	got, err := cborx.Marshal(fields[3:])
	if err != nil {
		t.Fatal(err)
	}
	expected, err := cborx.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, expected) {
		t.Fatalf("supported version lists = %x, want %x", got, expected)
	}
}

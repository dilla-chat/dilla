package server_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
)

func asServerError(err error, target **server.Error) bool { return errors.As(err, target) }

func TestDecodeBodyRefusesTheWrongContentType(t *testing.T) {
	body, _ := cborx.Marshal([]uint64{1})
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/x", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	var out []uint64
	err := server.DecodeBody(rec, r, 4096, &out)
	if err == nil {
		t.Fatal("a JSON content type was accepted")
	}
	server.WriteError(rec, err)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
}

func TestDecodeBodyRefusesABodyOverTheCap(t *testing.T) {
	big, _ := cborx.Marshal(strings.Repeat("a", 5000))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/x", bytes.NewReader(big))
	r.Header.Set("Content-Type", "application/cbor")
	rec := httptest.NewRecorder()
	var out string
	err := server.DecodeBody(rec, r, 1024, &out)
	if err == nil {
		t.Fatal("a body over the cap was accepted")
	}
	var se *server.Error
	if !asServerError(err, &se) || se.Code != server.CodeTooLarge {
		t.Fatalf("error = %v, want E_TOO_LARGE", err)
	}
}

func TestDecodeBodyRefusesNonDeterministicCBOR(t *testing.T) {
	// 0x18 0x01 is the non-minimal encoding of the integer 1.
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/x", bytes.NewReader([]byte{0x81, 0x18, 0x01}))
	r.Header.Set("Content-Type", "application/cbor")
	rec := httptest.NewRecorder()
	var out []uint64
	err := server.DecodeBody(rec, r, 4096, &out)
	var se *server.Error
	if !asServerError(err, &se) || se.Code != server.CodeInvalidRequest {
		t.Fatalf("error = %v, want E_INVALID_REQUEST", err)
	}
}

func TestPathIDRejectsMalformedAndAcceptsWellFormed(t *testing.T) {
	want := id.New()
	mux := server.NewMux()
	mux.Handle("GET /v1/groups/{id}/info", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := server.PathID(r, "id")
		if err != nil {
			server.WriteError(w, err)
			return
		}
		if got != want {
			t.Errorf("PathID = %s, want %s", got, want)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := httpGet(t, srv.URL+"/v1/groups/"+want.String()+"/info")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("well-formed id gave %d", res.StatusCode)
	}
	res, err = httpGet(t, srv.URL+"/v1/groups/NOTHEX/info")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed id gave %d, want 400", res.StatusCode)
	}
}

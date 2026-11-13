package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
)

// parityFixtureMark is the twelfth element dilla-testhost appends to GET /v1/instance under -parity-fixture
// (client-core's PARITY_FIXTURE_MARK, Task 6c). The web client then shows deterministic diagnostics (no live round trips,
// no key-package report), so a reference shot of the Diagnostics section is the same on every run. It exists only in this
// test host: dillad never serves it.
const parityFixtureMark = "dilla-testhost/parity-fixture"

// parityFixture appends the marker to a 200 GET /v1/instance answer and passes everything else through.
func parityFixture(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/instance" {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		if rec.Code == http.StatusOK {
			if marked, ok := withMark(body); ok {
				body = marked
			}
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.Code)
		_, _ = w.Write(body)
	})
}

func withMark(body []byte) ([]byte, bool) {
	var items []cbor.RawMessage
	if err := cborx.Unmarshal(body, &items); err != nil {
		return nil, false
	}
	mark, err := cborx.Marshal(parityFixtureMark)
	if err != nil {
		return nil, false
	}
	out, err := cborx.Marshal(append(items, cborx.Raw(mark)))
	if err != nil {
		return nil, false
	}
	return out, true
}

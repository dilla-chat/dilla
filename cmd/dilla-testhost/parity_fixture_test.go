package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
)

// Task 6c: under -parity-fixture the host appends its marker to GET /v1/instance (client-core's PARITY_FIXTURE_MARK), so
// the web client's diagnostics are deterministic in a reference shot. Without the flag nothing changes, and dillad itself
// never serves the marker.
func TestTheParityFixtureFlagReachesTheHostOptions(t *testing.T) {
	o, err := parseFlags([]string{"-parity-fixture"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !o.parityFixture {
		t.Fatal("-parity-fixture did not set the option")
	}
	o, err = parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if o.parityFixture {
		t.Fatal("the option is set without the flag")
	}
}

func instanceDoc(t *testing.T) []byte {
	t.Helper()
	b, err := cborx.Marshal([]any{[]uint64{1}, []uint64{1}, []uint64{1}, make([]byte, 16), uint64(1), "dilla.test", uint64(0),
		[]uint64{0}, uint64(1), make([]byte, 16), make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheParityFixtureMarksOnlyTheInstanceDocument(t *testing.T) {
	doc := instanceDoc(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/cbor")
		_, _ = w.Write(doc)
	})
	get := func(h http.Handler, method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, nil))
		return rec
	}
	rec := get(parityFixture(inner), http.MethodGet, "/v1/instance")
	var items []cbor.RawMessage
	if err := cborx.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 12 {
		t.Fatalf("%d elements, want 12", len(items))
	}
	var mark string
	if err := cborx.Unmarshal(items[11], &mark); err != nil || mark != parityFixtureMark {
		t.Fatalf("twelfth element %q (%v), want %q", mark, err, parityFixtureMark)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/cbor" {
		t.Fatalf("Content-Type %q", got)
	}
	// Any other route and any other method pass through untouched.
	for _, c := range []struct{ method, path string }{{http.MethodGet, "/v1/instance/limits"}, {http.MethodPost, "/v1/instance"}} {
		if body := get(parityFixture(inner), c.method, c.path).Body.Bytes(); string(body) != string(doc) {
			t.Fatalf("%s %s was changed", c.method, c.path)
		}
	}
}

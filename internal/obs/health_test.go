package obs_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/obs"
)

func TestReadyzIsUnreadyUntilEveryGateIsGreen(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	h := obs.NewHealth(clk)
	names := []string{"db", "schema", "wasi", "livekit", "tls", "heal"}
	gates := map[string]*obs.Gate{}
	for _, n := range names {
		gates[n] = h.Gate(n)
	}
	rec := httptest.NewRecorder()
	h.Readiness().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d before any gate is green, want 503", rec.Code)
	}
	for _, n := range names[:len(names)-1] {
		gates[n].Set(true, "")
	}
	rec = httptest.NewRecorder()
	h.Readiness().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d with one gate red, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "heal") {
		t.Fatalf("/readyz did not name the failed gate: %s", body)
	}
	for _, n := range names {
		if n == "heal" {
			continue
		}
		if strings.Contains(body, n) {
			t.Fatalf("/readyz named a healthy gate %q: %s", n, body)
		}
	}
	gates["heal"].Set(true, "")
	rec = httptest.NewRecorder()
	h.Readiness().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d with every gate green, want 200", rec.Code)
	}
}

func TestHealthzFailsWhenAWorkerIsPastItsHardLimit(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	clk := clock.NewFake(start)
	h := obs.NewHealth(clk)
	h.MarkWorker("retention", start.Add(time.Minute))
	rec := httptest.NewRecorder()
	h.Liveness().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d while the worker is inside its deadline, want 200", rec.Code)
	}
	clk.Advance(2 * time.Minute)
	rec = httptest.NewRecorder()
	h.Liveness().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d past the worker's hard limit, want 500", rec.Code)
	}
}

func TestDrainMakesReadyzUnready(t *testing.T) {
	h := obs.NewHealth(clock.System())
	for _, n := range []string{"db", "schema", "wasi", "livekit", "tls", "heal"} {
		h.Gate(n).Set(true, "")
	}
	h.Drain()
	rec := httptest.NewRecorder()
	h.Readiness().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatal("a draining instance still reported ready; a load balancer would keep sending it work")
	}
}

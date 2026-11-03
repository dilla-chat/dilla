package obs_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dilla-chat/dilla/internal/obs"
)

// A wrapped Registerer is not a Gatherer, and Handler must not assume it is:
// NewMetrics takes the gatherer as its own argument (deviation-free, but the
// panic it prevents would happen inside dillad.New).
func TestHandlerDoesNotPanicOnAWrappedRegisterer(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := obs.NewMetrics(prometheus.WrapRegistererWithPrefix("x_", reg), reg)
	rec := httptest.NewRecorder()
	m.Handler(false, "").ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d over a wrapped registerer", rec.Code)
	}
}

func TestNoMetricCarriesAnIdentifierLabel(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := obs.NewMetrics(reg, reg)
	m.ObserveHTTP("/v1/instance", http.MethodGet, 200, time.Millisecond)
	m.RateLimited("login")
	m.RelayBytes(true, 10)
	m.StatsReport(true, 40, 1)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	banned := map[string]bool{"group_id": true, "channel_id": true, "user_id": true, "device_id": true}
	for _, f := range families {
		for _, metric := range f.GetMetric() {
			for _, label := range metric.GetLabel() {
				if banned[label.GetName()] {
					t.Fatalf("metric %s carries the label %s", f.GetName(), label.GetName())
				}
			}
		}
	}
	if len(families) == 0 {
		t.Fatal("no metrics were registered at all")
	}
}

func TestMetricsEndpointIsGuarded(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := obs.NewMetrics(reg, reg)
	h := m.Handler(true, "scrapetoken")

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated scrape gave %d, want 401", rec.Code)
	}

	r = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	r.Header.Set("Authorization", "Bearer scrapetoken")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated scrape gave %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dilla_") {
		t.Fatalf("no dilla metrics in the scrape: %s", rec.Body.String())
	}
}

func TestMetricNamesAreTheDocumentedSet(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	obs.NewMetrics(reg, reg)
	want := []string{
		"dilla_http_requests_total", "dilla_http_request_duration_seconds",
		"dilla_gateway_connections", "dilla_gateway_frames_total",
		"dilla_gateway_queue_overflow_total", "dilla_ds_commits_total",
		"dilla_ds_commit_duration_seconds", "dilla_ds_proposals_outstanding",
		"dilla_mls_pending_removal_age_seconds", "dilla_ds_frozen_groups",
		"dilla_ds_election_rounds_total", "dilla_wasi_calls_total",
		"dilla_wasi_call_duration_seconds", "dilla_store_tx_duration_seconds",
		"dilla_blob_bytes_total", "dilla_rate_limited_total",
		"dilla_blob_gc_runs_total", "dilla_blob_gc_deleted_total", "dilla_blob_gc_bytes_total",
		"dilla_blob_refs_expired_total", "dilla_blob_purges_total",
		"dilla_cert_renewal_failures_total", "dilla_clock_skew_seconds",
		"dilla_call_full_total", "dilla_call_share_refusals_total", "dilla_call_cuts_total",
		"dilla_call_grant_retries_total", "dilla_call_grant_repairs_pending",
		"dilla_call_stats_reports_total", "dilla_call_relay_reports_total", "dilla_call_decrypt_failures_total",
		"dilla_call_rtt_seconds", "dilla_turn_allocations", "dilla_turn_quota_refusals_total", "dilla_turn_relay_bytes_total",
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	have := map[string]bool{}
	for _, f := range families {
		have[f.GetName()] = true
	}
	// A metric with no observations is still registered; Gather reports it only
	// once it has a child, so unregistered names are checked by Describe.
	descs := make(chan *prometheus.Desc, 64)
	fresh := prometheus.NewPedanticRegistry()
	obs.NewMetrics(fresh, fresh).Describe(descs)
	close(descs)
	for d := range descs {
		have[nameOf(d)] = true
	}
	for _, name := range want {
		if !have[name] {
			t.Errorf("metric %s is not registered", name)
		}
	}
}

// nameOf parses the metric name out of (*prometheus.Desc).String(), whose
// format is `Desc{fqName: "name", help: "...", ...}`.
func nameOf(d *prometheus.Desc) string {
	s := d.String()
	const key = `fqName: "`
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// The blob recorders are nil-safe: the sweeper and the admin handler are built
// without metrics in tests and CLI verbs.
func TestTheBlobRecordersAreNilSafe(t *testing.T) {
	var m *obs.Metrics
	m.BlobSweep(nil)
	m.BlobCollected(1)
	m.BlobRefExpired("retention")
	m.BlobPurged()
}

// counterValue reads one label-free counter out of a registry. It gathers rather than calling
// prometheus/testutil, a module this go.mod does not require (internal/gateway's metrics_test.go
// reads its gauge the same way).
func counterValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			if len(f.GetMetric()) != 1 {
				t.Fatalf("%s has %d series, want 1", name, len(f.GetMetric()))
			}
			return f.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("%s is not in the registry", name)
	return 0
}

// gaugeValue reads one label-free gauge out of a registry.
func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name && len(f.GetMetric()) == 1 {
			return f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("%s is not in the registry", name)
	return 0
}

func TestTheCallCountersCount(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := obs.NewMetrics(reg, reg)
	m.CallFull()
	m.ShareRefused()
	m.ShareRefused()
	if got := counterValue(t, reg, "dilla_call_full_total"); got != 1 {
		t.Errorf("dilla_call_full_total = %v", got)
	}
	if got := counterValue(t, reg, "dilla_call_share_refusals_total"); got != 2 {
		t.Errorf("dilla_call_share_refusals_total = %v", got)
	}
	m.CallCut()
	if got := counterValue(t, reg, "dilla_call_cuts_total"); got != 1 {
		t.Errorf("dilla_call_cuts_total = %v", got)
	}
	m.CallGrantRetry()
	if got := counterValue(t, reg, "dilla_call_grant_retries_total"); got != 1 {
		t.Errorf("dilla_call_grant_retries_total = %v", got)
	}
	m.CallRepairsPending(3)
	if got := gaugeValue(t, reg, "dilla_call_grant_repairs_pending"); got != 3 {
		t.Errorf("dilla_call_grant_repairs_pending = %v", got)
	}
	m.CallsLive(3)
	if got := gaugeValue(t, reg, "dilla_call_live"); got != 3 {
		t.Errorf("dilla_call_live = %v", got)
	}
	var none *obs.Metrics
	none.CallFull()
	none.ShareRefused()
	none.CallCut()
	none.CallGrantRetry()
	none.CallRepairsPending(1)
	none.CallsLive(1)
}

// histogramValue reads a label-free histogram's sample count and sum.
func histogramValue(t *testing.T, reg *prometheus.Registry, name string) (uint64, float64) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name && len(f.GetMetric()) == 1 {
			h := f.GetMetric()[0].GetHistogram()
			return h.GetSampleCount(), h.GetSampleSum()
		}
	}
	t.Fatalf("no histogram %s", name)
	return 0, 0
}

// labelledCounter reads the series of a counter family whose one label has the given value.
func labelledCounter(t *testing.T, reg *prometheus.Registry, name, value string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, metric := range f.GetMetric() {
			if l := metric.GetLabel(); len(l) == 1 && l[0].GetValue() == value {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("%s{%q} is not in the registry", name, value)
	return 0
}

func TestTheTURNAndStatsRecordersCount(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := obs.NewMetrics(reg, reg)
	m.StatsReport(true, 80, 3)
	m.StatsReport(false, 20, 0)
	m.QuotaRefused()
	m.Allocations(4)
	m.RelayBytes(true, 100)
	m.RelayBytes(false, 30)
	if got := counterValue(t, reg, "dilla_call_stats_reports_total"); got != 2 {
		t.Errorf("reports = %v", got)
	}
	if got := counterValue(t, reg, "dilla_call_relay_reports_total"); got != 1 {
		t.Errorf("relay reports = %v", got)
	}
	if got := counterValue(t, reg, "dilla_call_decrypt_failures_total"); got != 3 {
		t.Errorf("decrypt failures = %v", got)
	}
	if got := counterValue(t, reg, "dilla_turn_quota_refusals_total"); got != 1 {
		t.Errorf("quota refusals = %v", got)
	}
	if got := gaugeValue(t, reg, "dilla_turn_allocations"); got != 4 {
		t.Errorf("allocations = %v", got)
	}
	if got := labelledCounter(t, reg, "dilla_turn_relay_bytes_total", "to_client"); got != 100 {
		t.Errorf("to_client = %v", got)
	}
	if got := labelledCounter(t, reg, "dilla_turn_relay_bytes_total", "to_peer"); got != 30 {
		t.Errorf("to_peer = %v", got)
	}
	if live, refused := m.TURNState(); live != 4 || refused != 1 {
		t.Errorf("TURNState = %d, %d; want 4, 1", live, refused)
	}
	// Review M7: the round-trip histogram is in seconds, Prometheus's base unit.
	if count, sum := histogramValue(t, reg, "dilla_call_rtt_seconds"); count != 2 || sum < 0.0999 || sum > 0.1001 {
		t.Errorf("dilla_call_rtt_seconds count %d, sum %v; want 2 and 0.1 (80 ms + 20 ms)", count, sum)
	}
	var none *obs.Metrics
	none.StatsReport(true, 1, 1)
	none.QuotaRefused()
	none.RelayBytes(true, 1)
	none.Allocations(1)
	if live, refused := none.TURNState(); live != 0 || refused != 0 {
		t.Error("a nil *Metrics reported relay state")
	}
}

package gateway

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/obs"
)

// gaugeValue reads one gauge out of a registry. It gathers rather than calling
// prometheus/testutil, which is a module this go.mod does not require and which would be a
// dependency change on a fix commit.
func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		metrics := f.GetMetric()
		if len(metrics) != 1 {
			t.Fatalf("%s has %d series, want 1", name, len(metrics))
		}
		return metrics[0].GetGauge().GetValue()
	}
	// A gauge that has never moved is still gathered, so an absent family is a real failure.
	t.Fatalf("%s is not in the registry", name)
	return 0
}

// dilla_gateway_connections is a GAUGE of OPEN connections (internal/obs/metrics.go, "Open gateway
// connections."), so every path that takes a connection out of the live registry decrements it.
// An Inc with no matching Dec makes the gauge a lifetime counter that climbs from the first
// reconnect onwards, and it is the one operational signal for gateway load.
func TestTheOpenConnectionsGaugeCountsOpenConnections(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(h *harness, c *conn)
	}{
		{"suspend", func(h *harness, c *conn) { h.gw.suspend(c) }},
		{"close_device", func(h *harness, c *conn) {
			h.gw.CloseDevice(c.deviceID, CloseSessionRevoked, "revoked")
		}},
		{"shutdown", func(h *harness, _ *conn) { _ = h.gw.Shutdown(context.Background()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			reg := prometheus.NewRegistry()
			h.gw.opts.Metrics = obs.NewMetrics(reg, reg)

			c := h.connect(t, id.New())
			if got := gaugeValue(t, reg, "dilla_gateway_connections"); got != 1 {
				t.Fatalf("gauge = %v after one connection, want 1", got)
			}

			tc.close(h, c)
			if got := gaugeValue(t, reg, "dilla_gateway_connections"); got != 0 {
				t.Fatalf("gauge = %v after the connection ended, want 0", got)
			}

			// The deferred suspend readLoop runs after a close must not drive the gauge negative:
			// the connection is already out of the registry, so the second removal counts nothing.
			h.gw.suspend(c)
			if got := gaugeValue(t, reg, "dilla_gateway_connections"); got != 0 {
				t.Fatalf("gauge = %v after a second removal of the same connection, want 0", got)
			}
		})
	}
}

// A connection whose sendReady fails never reached the registry, so it must not be counted.
func TestAConnectionThatNeverGotReadyIsNotCounted(t *testing.T) {
	h := newHarness(t)
	reg := prometheus.NewRegistry()
	h.gw.opts.Metrics = obs.NewMetrics(reg, reg)
	h.gw.opts.Store = failingStore{}

	if _, err := h.gw.register(context.Background(), session(id.New()), newRecordingSink(4, false)); err == nil {
		t.Fatal("register must fail when the store does")
	}
	if got := gaugeValue(t, reg, "dilla_gateway_connections"); got != 0 {
		t.Fatalf("gauge = %v after a failed registration, want 0", got)
	}
}

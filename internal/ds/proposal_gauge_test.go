package ds_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/obs"
	"github.com/jonasthim/dilla/internal/store"
)

// restartWithMetrics replaces the harness DS with a fresh process's, one that reports metrics.
func (h *dsHarness) restartWithMetrics() *obs.Metrics {
	h.t.Helper()
	if err := h.ds.Shutdown(context.Background()); err != nil {
		h.t.Fatalf("Shutdown: %v", err)
	}
	reg := prometheus.NewRegistry()
	m := obs.NewMetrics(reg, reg)
	d, err := ds.New(ds.Options{
		Store: h.repo, Wasm: h.wasm, Gateway: h.gw, Clock: h.clk,
		Keys: testInstanceKeys(h.t), Policy: ds.DefaultPolicy(), Channels: h.channels,
		ACL: h.acl, Metrics: m,
	})
	if err != nil {
		h.t.Fatalf("ds.New: %v", err)
	}
	h.t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	h.ds = d
	return m
}

// gaugeValue reads one label of dilla_ds_proposals_outstanding.
func gaugeValue(t *testing.T, m *obs.Metrics, label string) float64 {
	t.Helper()
	var out dto.Metric
	if err := m.DSProposals.WithLabelValues(label).Write(&out); err != nil {
		t.Fatalf("reading the gauge: %v", err)
	}
	return out.GetGauge().GetValue()
}

func (h *dsHarness) putProposalRow(t *testing.T, groupID id.ID, epoch uint64, origin uint8, issued int64, ttl uint64) []byte {
	t.Helper()
	ref := id.New()
	leaf := uint32(1)
	if err := h.repo.PutProposal(context.Background(), store.ProposalRow{
		GroupID: groupID, Ref: ref[:], Epoch: epoch, Kind: 3, TargetLeaf: &leaf, Origin: origin,
		ActionID: id.New(), IssuedAt: issued, TTL: ttl,
	}); err != nil {
		t.Fatalf("PutProposal: %v", err)
	}
	return ref[:]
}

// dilla_ds_proposals_outstanding used to start at zero in every process, so the first sweep's voids
// of rows an earlier process issued — the closed call groups' backlog on CT 144 — drove it
// negative. Start seeds it from SQL (live instance proposals, every group), each decrement is
// clamped at zero, and the sweep ends with it equal to the open groups' live instance proposals.
func TestTheProposalGaugeIsSeededAndNeverGoesNegative(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	long := h.clk.Now().Add(-14 * 24 * time.Hour).Unix()

	// The backlog a previous process left: a closed group with three instance Removes long past
	// their 30 s TTL and one member Remove, which the gauge never counted.
	closed := h.bareGroup(t)
	for range 3 {
		h.putProposalRow(t, closed, 0, 0, long, 30)
	}
	h.putProposalRow(t, closed, 0, 1, long, 30)
	if err := h.repo.CloseGroup(ctx, closed, long); err != nil {
		t.Fatalf("CloseGroup: %v", err)
	}
	// The open group: two live instance Removes, one expired one, and a member Remove.
	h.putProposalRow(t, reg.GroupID, 6, 0, h.clk.Now().Unix(), 86400)
	h.putProposalRow(t, reg.GroupID, 6, 0, h.clk.Now().Unix(), 86400)
	h.putProposalRow(t, reg.GroupID, 6, 0, long, 86400)
	member := h.putProposalRow(t, reg.GroupID, 6, 1, long, 30)

	m := h.restartWithMetrics()
	removes := func() float64 { return gaugeValue(t, m, "remove") }
	if err := h.ds.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := removes(); got != 6 {
		t.Fatalf("gauge after Start = %v, want 6 (seeded from SQL: 3 closed + 3 open instance Removes)", got)
	}

	// A void of a member proposal, and a second void of the same row, never decrement.
	if err := h.ds.Void(ctx, reg.GroupID, member); err != nil {
		t.Fatalf("Void: %v", err)
	}
	if err := h.ds.Void(ctx, reg.GroupID, member); err != nil {
		t.Fatalf("Void again: %v", err)
	}
	if got := removes(); got != 6 {
		t.Fatalf("gauge after voiding a member proposal twice = %v, want 6", got)
	}

	if _, err := h.ds.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	open := 0
	rows, err := h.repo.ListProposals(ctx, reg.GroupID, 6, false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	for _, r := range rows {
		if r.Origin == 0 {
			open++
		}
	}
	if open != 2 {
		t.Fatalf("%d live instance proposals in the open group after the sweep, want 2", open)
	}
	if got := removes(); got != float64(open) {
		t.Fatalf("gauge after the sweep = %v, want %d (the open group's live instance proposals)", got, open)
	}
	for _, l := range []string{"add", "update", "other"} {
		if got := gaugeValue(t, m, l); got != 0 {
			t.Errorf("gauge{kind=%q} = %v, want 0", l, got)
		}
	}
}

// Without a seed (a store error at Start, say), a decrement below zero is clamped: the gauge reads
// 0, never a negative count, until the sweep's re-seed.
func TestAProposalGaugeDecrementBelowZeroIsClamped(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	m := h.restartWithMetrics() // not started: nothing seeded
	ref := h.putProposalRow(t, reg.GroupID, 6, 0, h.clk.Now().Unix(), 86400)
	if err := h.ds.Void(ctx, reg.GroupID, ref); err != nil {
		t.Fatalf("Void: %v", err)
	}
	if got := gaugeValue(t, m, "remove"); got != 0 {
		t.Fatalf("gauge = %v after an unseeded decrement, want 0", got)
	}
}

func TestVoidingAnInstanceProposalTwiceDecrementsTheGaugeOnlyOnce(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	ref := h.putProposalRow(t, reg.GroupID, 6, 0, h.clk.Now().Unix(), 86400)
	h.putProposalRow(t, reg.GroupID, 6, 0, h.clk.Now().Unix(), 86400)
	m := h.restartWithMetrics()
	if err := h.ds.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := gaugeValue(t, m, "remove"); got != 2 {
		t.Fatalf("gauge after Start = %v, want 2", got)
	}
	for i := range 2 {
		if err := h.ds.Void(ctx, reg.GroupID, ref); err != nil {
			t.Fatalf("Void: %v", err)
		}
		if got := gaugeValue(t, m, "remove"); got != 1 {
			t.Fatalf("gauge after void %d = %v, want 1", i+1, got)
		}
	}
}

func TestAStrandedOldEpochProposalDoesNotRaiseTheGaugeAfterSweep(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.putProposalRow(t, reg.GroupID, 5, 0, h.clk.Now().Add(-time.Hour).Unix(), 30)
	h.putProposalRow(t, reg.GroupID, 6, 0, h.clk.Now().Unix(), 86400)
	m := h.restartWithMetrics()
	if err := h.ds.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := gaugeValue(t, m, "remove"); got != 1 {
		t.Errorf("gauge after Start = %v, want 1", got)
	}
	for range 2 {
		if _, err := h.ds.Sweep(ctx); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		if got := gaugeValue(t, m, "remove"); got != 1 {
			t.Fatalf("gauge after Sweep = %v, want 1", got)
		}
	}
}

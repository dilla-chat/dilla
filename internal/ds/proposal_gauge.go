package ds

import (
	"context"
	"sync"
)

// proposalGauge keeps dilla_ds_proposals_outstanding{kind}: the non-void instance proposals
// (origin 0), at open groups' current epoch and every epoch of closed groups. The count lives here, under one mutex, and the gauge is Set from it, so
// a decrement can never take it below zero.
//
// A bare Inc/Dec gauge started at zero on every process start: a void of a proposal issued before
// the restart (voidExpired, the closed-group void) then drove it negative — on CT 144 the closed
// call groups' backlog alone would have left it at about -63. Start seeds it from SQL
// (CountLiveInstanceProposals), and Sweep re-seeds it every tick, which also corrects the paths
// that end a proposal without decrementing (a commit deleting the refs it applied, a re-issue of a
// live row).
type proposalGauge struct {
	mu sync.Mutex
	n  map[string]float64
}

// proposalLabels is every label proposalLabel returns, so a seed zeroes a kind SQL no longer holds.
var proposalLabels = []string{"add", "remove", "update", "other"}

func (d *DS) gaugeSet(byLabel map[string]float64) {
	if d.opts.Metrics == nil {
		return
	}
	d.gauge.mu.Lock()
	defer d.gauge.mu.Unlock()
	d.gauge.n = map[string]float64{}
	for _, l := range proposalLabels {
		d.gauge.n[l] = byLabel[l]
		d.opts.Metrics.DSProposals.WithLabelValues(l).Set(d.gauge.n[l])
	}
}

func (d *DS) gaugeAdd(kind uint8, delta float64) {
	if d.opts.Metrics == nil {
		return
	}
	l := proposalLabel(kind)
	d.gauge.mu.Lock()
	defer d.gauge.mu.Unlock()
	if d.gauge.n == nil {
		d.gauge.n = map[string]float64{}
	}
	d.gauge.n[l] = max(0, d.gauge.n[l]+delta)
	d.opts.Metrics.DSProposals.WithLabelValues(l).Set(d.gauge.n[l])
}

// gaugeInc counts a fresh instance proposal.
func (d *DS) gaugeInc(kind uint8) { d.gaugeAdd(kind, 1) }

// gaugeDec uncounts an instance proposal that was live and is now void or gone. Callers pass only
// rows with origin 0 that were non-void: a member proposal was never counted.
func (d *DS) gaugeDec(kind uint8) { d.gaugeAdd(kind, -1) }

// seedProposalGauge sets the gauge from SQL.
func (d *DS) seedProposalGauge(ctx context.Context) error {
	if d.opts.Metrics == nil {
		return nil
	}
	counts, err := d.opts.Store.CountLiveInstanceProposals(ctx)
	if err != nil {
		return err
	}
	byLabel := map[string]float64{}
	for kind, n := range counts {
		byLabel[proposalLabel(kind)] += float64(n)
	}
	d.gaugeSet(byLabel)
	return nil
}

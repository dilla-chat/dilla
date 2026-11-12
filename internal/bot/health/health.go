// Package health is the Bot's honest health signal (Bot v2 Task 17): it asks the Bot's own command path whether it can
// answer, and tells dillad the answer, the load, and since when commands fail. Nothing in it can only say "fine": a
// probe that does not return is a failure, and dillad marks the Bot offline when the reports stop.
package health

import (
	"context"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
)

// The spec's timings.
const (
	// ProbePeriod is how often the Bot probes its own command path.
	ProbePeriod = 60 * time.Second
	// ProbeTimeout is how long a probe may take (the sealed-request timeout): a reply that has not come by then is a
	// failure.
	ProbeTimeout = 5 * time.Second
	// ReportEvery is how often the Bot reports to dillad. It is far below the 150 s dillad waits before it marks the
	// Bot offline and gives its 60 s queue-growth rule several reports to compare.
	ReportEvery = 15 * time.Second
	// MinProbeGap is the least time between two probes, however often ProbeNow is asked: a flapping helper must not
	// make the Bot probe in a storm. A probe asked for sooner waits for the gap.
	MinProbeGap = 5 * time.Second
)

// Prober asks the Bot's command path to answer one probe: a sealed OpProbe to the Bot's own command key as its own
// device, through POST …/bot/commands, answered [0]. nil means it answered in time.
type Prober interface {
	Probe(ctx context.Context) error
}

// ProberFunc is a Prober from a function.
type ProberFunc func(ctx context.Context) error

// Probe implements Prober.
func (f ProberFunc) Probe(ctx context.Context) error { return f(ctx) }

// Report is PUT /v1/bot-runtime/health's body, [probe_ok, probe_at, queue_depth, oldest_wait_ms, music_start_p95_ms,
// failing_since|null] (the Bot sends the booleans as uint 0 or 1).
type Report struct {
	ProbeOK       bool
	ProbeAt       time.Time
	QueueDepth    uint64
	OldestWaitMS  uint64
	MusicStartP95 uint64
	FailingSince  *time.Time
	ReportedAt    time.Time
	// FailedCommands is the commands that failed since the previous report (v0.1.0-bot2.2).
	FailedCommands uint32
}

// Reporter sends one report to dillad.
type Reporter interface {
	Report(ctx context.Context, r Report) error
}

// ReporterFunc is a Reporter from a function.
type ReporterFunc func(ctx context.Context, r Report) error

// Report implements Reporter.
func (f ReporterFunc) Report(ctx context.Context, r Report) error { return f(ctx, r) }

// Load is what the Bot knows about its own backlog and music starts, read at each report.
type Load struct {
	// QueueDepth is the commands accepted and not yet answered.
	QueueDepth uint64
	// OldestWait is how long the oldest of them has waited.
	OldestWait time.Duration
	// MusicStartP95 is the 95th percentile of the first-frame time of the last 20 track starts, zero without data.
	MusicStartP95 time.Duration
	// FailedCommands is the commands that failed since the last read: the function that returns it resets it, so a
	// failure is reported once.
	FailedCommands uint32
}

// Monitor probes and reports.
type Monitor struct {
	probe  Prober
	report Reporter
	clk    clock.Clock

	mu        sync.Mutex
	load      func() Load
	unsent    func(uint32)
	probed    bool
	lastProbe time.Time
	probeOK   bool
	failing   time.Time // when commands began failing, zero while the last probe succeeded
	degraded  time.Time // when the event poll began failing repeatedly (v0.1.0-bot2.4), zero while it works
	probeNow  bool      // ProbeNow was called: the next tick probes whatever the period
	wake      chan struct{}
}

// New builds a monitor. It does nothing until Run (or Tick) is called.
func New(probe Prober, report Reporter, clk clock.Clock) *Monitor {
	if clk == nil {
		clk = clock.System()
	}
	return &Monitor{probe: probe, report: report, clk: clk, wake: make(chan struct{}, 1)}
}

// SetLoad sets where the report's load comes from. Without one the load is zero.
func (m *Monitor) SetLoad(load func() Load) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load = load
}

// SetUnsent sets where the failed commands of a report that could not be sent go back to, so the next report carries
// them. Without one they are lost with the report.
func (m *Monitor) SetUnsent(f func(uint32)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unsent = f
}

// SetDegraded marks the installation degraded since the given time: its event poll failed repeatedly. The report's
// failing_since carries it beside the probe's. A second mark keeps the first time.
func (m *Monitor) SetDegraded(since time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.degraded.IsZero() {
		m.degraded = since
	}
}

// ClearDegraded ends the mark SetDegraded made.
func (m *Monitor) ClearDegraded() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.degraded = time.Time{}
}

// FailingSince is when commands began failing, as the last probe saw it, or when the event poll began failing
// repeatedly, whichever came first. ok is false while both work (or before the first probe).
func (m *Monitor) FailingSince() (since time.Time, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	since = m.failingSinceLocked()
	return since, !since.IsZero()
}

func (m *Monitor) failingSinceLocked() time.Time {
	switch {
	case m.failing.IsZero():
		return m.degraded
	case m.degraded.IsZero() || m.failing.Before(m.degraded):
		return m.failing
	}
	return m.degraded
}

// ProbeNow asks for a probe at once instead of at the next period: the installation's helper was replaced or is gone
// (v0.1.0-bot2.2). A running monitor ticks immediately; one driven by Tick probes on its next tick.
func (m *Monitor) ProbeNow() {
	m.mu.Lock()
	m.probeNow = true
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run ticks every ReportEvery until ctx ends, the first after one period: a Bot that has only just started is not yet
// ready for commands, and reporting that as a failed probe would be a startup artefact, not a fault.
func (m *Monitor) Run(ctx context.Context) {
	for {
		timer := m.clk.NewTimer(ReportEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		case <-m.wake:
			timer.Stop()
		}
		m.Tick(ctx)
	}
}

// Tick is one step: probe when ProbePeriod has passed since the last probe (the first tick always does), then report.
func (m *Monitor) Tick(ctx context.Context) {
	now := m.clk.Now()
	m.mu.Lock()
	due := !m.probed || now.Sub(m.lastProbe) >= ProbePeriod || (m.probeNow && now.Sub(m.lastProbe) >= MinProbeGap)
	if due {
		m.probeNow = false // an ask made inside the gap stays pending until the gap has passed
	}
	m.mu.Unlock()
	if due {
		ok := m.runProbe(ctx)
		m.mu.Lock()
		m.probed, m.lastProbe, m.probeOK = true, now, ok
		switch {
		case ok:
			m.failing = time.Time{}
		case m.failing.IsZero():
			m.failing = now
		}
		m.mu.Unlock()
	}
	m.send(ctx, now)
}

// runProbe returns whether the probe answered within ProbeTimeout. A probe that never returns is a failure, and its
// context is cancelled.
func (m *Monitor) runProbe(ctx context.Context) bool {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.probe.Probe(probeCtx) }()
	timer := m.clk.NewTimer(ProbeTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err == nil
	case <-timer.C():
		return false
	case <-ctx.Done():
		return false
	}
}

func (m *Monitor) send(ctx context.Context, now time.Time) {
	m.mu.Lock()
	r := Report{ProbeOK: m.probeOK, ProbeAt: m.lastProbe, ReportedAt: now}
	if failing := m.failingSinceLocked(); !failing.IsZero() {
		r.FailingSince = &failing
	}
	load, unsent := m.load, m.unsent
	m.mu.Unlock()
	if load != nil {
		l := load()
		r.QueueDepth, r.OldestWaitMS, r.MusicStartP95, r.FailedCommands = l.QueueDepth, millis(l.OldestWait), millis(l.MusicStartP95), l.FailedCommands
	}
	// A report that fails is not retried: the next one is ReportEvery away, and dillad marks the Bot offline when none
	// arrives for 150 s.
	if err := m.report.Report(ctx, r); err != nil && r.FailedCommands > 0 && unsent != nil {
		unsent(r.FailedCommands)
	}
}

// millis is d in whole milliseconds, never negative.
func millis(d time.Duration) uint64 {
	if d <= 0 {
		return 0
	}
	return uint64(d.Milliseconds()) //nolint:gosec // d is positive
}

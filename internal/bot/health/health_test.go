package health

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
)

type recorder struct {
	mu      sync.Mutex
	reports []Report
}

func (r *recorder) Report(_ context.Context, rep Report) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, rep)
	return nil
}

func (r *recorder) last(t *testing.T) Report {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reports) == 0 {
		t.Fatal("no report was sent")
	}
	return r.reports[len(r.reports)-1]
}

// run advances the fake clock one report period at a time, ticking at each step, for d.
func run(m *Monitor, clk *clock.Fake, d time.Duration) {
	for elapsed := time.Duration(0); elapsed < d; elapsed += ReportEvery {
		clk.Advance(ReportEvery)
		m.Tick(context.Background())
	}
}

// countingClock counts the timers armed on it, so a test knows the monitor is waiting.
type countingClock struct {
	*clock.Fake
	timers atomic.Int32
}

func (c *countingClock) NewTimer(d time.Duration) clock.Timer {
	c.timers.Add(1)
	return c.Fake.NewTimer(d)
}

func TestProbeEvery60s(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	probes := 0
	rec := &recorder{}
	m := New(ProberFunc(func(context.Context) error { probes++; return nil }), rec, clk)
	m.Tick(context.Background()) // t = 0: the first tick probes
	if probes != 1 {
		t.Fatalf("%d probes at start, want 1", probes)
	}
	run(m, clk, 45*time.Second) // t = 15, 30, 45: reports only
	if probes != 1 || len(rec.reports) != 4 {
		t.Fatalf("after 45 s: probes=%d reports=%d, want 1 probe and a report per 15 s", probes, len(rec.reports))
	}
	run(m, clk, 15*time.Second) // t = 60
	if probes != 2 {
		t.Fatalf("%d probes at t=60s, want 2", probes)
	}
	run(m, clk, 5*time.Minute)
	if probes != 7 {
		t.Fatalf("%d probes after 6 minutes, want one a minute: 7", probes)
	}
}

func TestHealthyOnlyWhenProbeSucceeds(t *testing.T) {
	clk := &countingClock{Fake: clock.NewFake(time.Unix(1_800_000_000, 0))}
	rec := &recorder{}
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	// The reply never arrives.
	m := New(ProberFunc(func(ctx context.Context) error {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return ctx.Err()
	}), rec, clk)
	done := make(chan struct{})
	go func() { m.Tick(context.Background()); close(done) }()
	// The probe has not answered: wait until the monitor armed its timeout, then let the timeout pass.
	deadline := time.After(2 * time.Second)
	for clk.timers.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the probe's timeout was never armed")
		case <-time.After(time.Millisecond):
		}
	}
	clk.Advance(ProbeTimeout)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a probe whose reply never arrives must fail at the timeout")
	}
	got := rec.last(t)
	if got.ProbeOK {
		t.Fatal("probe_ok is true although the probe never got a reply")
	}
	if got.FailingSince == nil || !got.FailingSince.Equal(time.Unix(1_800_000_000, 0)) {
		t.Fatalf("failing_since = %v, want the time of the first failed probe", got.FailingSince)
	}
}

func TestFailingSinceKeepsTheFirstFailureAndClearsOnSuccess(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	rec := &recorder{}
	fail := true
	m := New(ProberFunc(func(context.Context) error {
		if fail {
			return errors.New("no answer")
		}
		return nil
	}), rec, clk)
	first := clk.Now()
	m.Tick(context.Background())
	run(m, clk, 2*ProbePeriod)
	if got := rec.last(t); got.ProbeOK || got.FailingSince == nil || !got.FailingSince.Equal(first) {
		t.Fatalf("report %+v, want probe_ok=false and failing_since the first failure", got)
	}
	if since, ok := m.FailingSince(); !ok || !since.Equal(first) {
		t.Fatalf("FailingSince = %v %v", since, ok)
	}
	fail = false
	run(m, clk, ProbePeriod)
	if got := rec.last(t); !got.ProbeOK || got.FailingSince != nil {
		t.Fatalf("report %+v after recovery, want ok and no failing_since", got)
	}
	if _, ok := m.FailingSince(); ok {
		t.Fatal("FailingSince still set after a successful probe")
	}
}

func TestReportCarriesTheLoad(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	rec := &recorder{}
	m := New(ProberFunc(func(context.Context) error { return nil }), rec, clk)
	m.SetLoad(func() Load {
		return Load{QueueDepth: 7, OldestWait: 1500 * time.Millisecond, MusicStartP95: 900 * time.Millisecond}
	})
	m.Tick(context.Background())
	got := rec.last(t)
	if got.QueueDepth != 7 || got.OldestWaitMS != 1500 || got.MusicStartP95 != 900 || !got.ProbeOK || !got.ProbeAt.Equal(clk.Now()) {
		t.Fatalf("report %+v", got)
	}
}

// v0.1.0-bot2.4: a degraded mark from the event poll reaches failing_since beside the probe's, and clears alone.
func TestDegradedMarkReachesFailingSince(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	rec := &recorder{}
	m := New(ProberFunc(func(context.Context) error { return nil }), rec, clk)
	m.Tick(context.Background())
	if got := rec.last(t); got.FailingSince != nil {
		t.Fatalf("report %+v before the mark", got)
	}
	since := clk.Now()
	m.SetDegraded(since)
	m.SetDegraded(since.Add(time.Hour)) // a second mark keeps the first time
	clk.Advance(ReportEvery)
	m.Tick(context.Background())
	if got := rec.last(t); !got.ProbeOK || got.FailingSince == nil || !got.FailingSince.Equal(since) {
		t.Fatalf("report %+v, want probe_ok and failing_since at the mark", got)
	}
	if got, ok := m.FailingSince(); !ok || !got.Equal(since) {
		t.Fatalf("FailingSince = %v %v", got, ok)
	}
	m.ClearDegraded()
	clk.Advance(ReportEvery)
	m.Tick(context.Background())
	if got := rec.last(t); got.FailingSince != nil {
		t.Fatalf("report %+v after the clear", got)
	}
}

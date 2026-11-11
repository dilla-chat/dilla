package health

import (
	"context"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
)

// v0.1.0-bot2.2 (F7): ProbeNow makes the next tick probe whatever the period, and a running monitor ticks at once.
func TestProbeNowProbesAtOnce(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	probes := 0
	m := New(ProberFunc(func(context.Context) error { probes++; return nil }), &recorder{}, clk)
	m.Tick(context.Background())
	clk.Advance(ReportEvery)
	m.Tick(context.Background())
	if probes != 1 {
		t.Fatalf("%d probes 15 s in, want 1", probes)
	}
	m.ProbeNow()
	clk.Advance(ReportEvery)
	m.Tick(context.Background())
	if probes != 2 {
		t.Fatalf("%d probes after ProbeNow and one tick, want 2", probes)
	}
	clk.Advance(ReportEvery)
	m.Tick(context.Background())
	if probes != 2 {
		t.Fatalf("%d probes: ProbeNow asked for one probe, got another before the period", probes)
	}
}

// A flapping helper cannot make the Bot probe in a storm: ProbeNow waits for MinProbeGap since the last probe.
func TestProbeNowKeepsAMinimumGap(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	probes := 0
	m := New(ProberFunc(func(context.Context) error { probes++; return nil }), &recorder{}, clk)
	m.Tick(context.Background()) // probe 1 at t=0
	clk.Advance(time.Second)
	m.ProbeNow()
	m.Tick(context.Background()) // 1 s after the last probe: too soon
	if probes != 1 {
		t.Fatalf("%d probes 1 s after the last, want 1", probes)
	}
	clk.Advance(MinProbeGap)
	m.Tick(context.Background()) // the asked-for probe runs once the gap has passed
	if probes != 2 {
		t.Fatalf("%d probes after the gap, want 2", probes)
	}
}

// A report that could not be sent gives its failed commands back, so the next one carries them.
func TestUnsentReportReturnsItsFailedCommands(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	fail := true
	var got []uint32
	m := New(ProberFunc(func(context.Context) error { return nil }), ReporterFunc(func(_ context.Context, r Report) error {
		if fail {
			fail = false
			return context.DeadlineExceeded
		}
		got = append(got, r.FailedCommands)
		return nil
	}), clk)
	pending := uint32(2)
	m.SetLoad(func() Load { l := Load{FailedCommands: pending}; pending = 0; return l })
	m.SetUnsent(func(n uint32) { pending += n })
	m.Tick(context.Background()) // the send fails
	clk.Advance(ReportEvery)
	m.Tick(context.Background())
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("reports delivered %v, want one carrying the 2 failures", got)
	}
}

func TestProbeNowWakesARunningMonitor(t *testing.T) {
	clk := &countingClock{Fake: clock.NewFake(time.Unix(1_800_000_000, 0))}
	probed := make(chan struct{}, 4)
	m := New(ProberFunc(func(context.Context) error { probed <- struct{}{}; return nil }), &recorder{}, clk)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.After(2 * time.Second)
	for clk.timers.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the monitor never armed its timer")
		case <-time.After(time.Millisecond):
		}
	}
	m.ProbeNow() // no clock advance: the monitor ticks at once, and the first tick probes
	select {
	case <-probed:
	case <-time.After(2 * time.Second):
		t.Fatal("ProbeNow did not make the running monitor probe")
	}
}

func TestFailedCommandsAreReported(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	rec := &recorder{}
	m := New(ProberFunc(func(context.Context) error { return nil }), rec, clk)
	failed := uint32(2)
	m.SetLoad(func() Load { l := Load{FailedCommands: failed}; failed = 0; return l })
	m.Tick(context.Background())
	if got := rec.last(t).FailedCommands; got != 2 {
		t.Fatalf("FailedCommands = %d, want 2", got)
	}
	clk.Advance(ReportEvery)
	m.Tick(context.Background())
	if got := rec.last(t).FailedCommands; got != 0 {
		t.Fatalf("FailedCommands = %d on the next report, want 0", got)
	}
}

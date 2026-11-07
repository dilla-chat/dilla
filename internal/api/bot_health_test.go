package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/bot/health"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 17: dillad judges the Bot's health reports. State 0 is ready, 1 degraded, 2 offline.
const (
	healthReady    = uint64(0)
	healthDegraded = uint64(1)
	healthOffline  = uint64(2)
)

func healthReportAt(ok bool, probeAt time.Time, depth, waitMS uint64) api.BotHealthReport {
	return api.BotHealthReport{ProbeOK: ok, ProbeAt: uint64(probeAt.Unix()), QueueDepth: depth, OldestWaitMS: waitMS}
}

// The fault test: intake stops at t = 0. The Bot's real monitor keeps probing and reporting, its probes fail, and
// dillad shows the Bot degraded by t <= 120 s (two probe periods).
func TestDegradedWithinTwoProbePeriodsWhenIntakeStops(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	tracker := api.NewBotHealth(clk)
	community := id.New()
	intakeRunning := true
	monitor := health.New(
		health.ProberFunc(func(context.Context) error {
			if !intakeRunning {
				return context.DeadlineExceeded
			}
			return nil
		}),
		health.ReporterFunc(func(_ context.Context, r health.Report) error {
			tracker.Record(community, api.BotHealthReport{ProbeOK: r.ProbeOK, ProbeAt: uint64(r.ProbeAt.Unix()), QueueDepth: r.QueueDepth, OldestWaitMS: r.OldestWaitMS})
			return nil
		}), clk)
	start := clk.Now()
	monitor.Tick(context.Background()) // t = 0: the probe still passes
	intakeRunning = false
	if state, _ := tracker.Status(community); state != healthReady {
		t.Fatalf("state %d at t=0, want ready", state)
	}
	degradedAt := time.Duration(-1)
	for elapsed := health.ReportEvery; elapsed <= 120*time.Second; elapsed += health.ReportEvery {
		clk.Advance(health.ReportEvery)
		monitor.Tick(context.Background())
		if state, _ := tracker.Status(community); state == healthDegraded && degradedAt < 0 {
			degradedAt = clk.Now().Sub(start)
		}
	}
	if degradedAt < 0 || degradedAt > 120*time.Second {
		t.Fatalf("degraded at %v, want by t=120s", degradedAt)
	}
	// One failed probe is not enough: at t=60 s there had been only one failure.
	if degradedAt <= 60*time.Second {
		t.Fatalf("degraded at %v after one failed probe, want two", degradedAt)
	}
}

func TestDegradedWhenARequestWaitsOver10s(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	tracker := api.NewBotHealth(clk)
	community := id.New()
	tracker.Record(community, healthReportAt(true, clk.Now(), 1, 9_999))
	if state, _ := tracker.Status(community); state != healthReady {
		t.Fatalf("state %d for a 9.999 s wait, want ready", state)
	}
	clk.Advance(15 * time.Second)
	tracker.Record(community, healthReportAt(true, clk.Now(), 1, 10_001))
	if state, since := tracker.Status(community); state != healthDegraded || since != uint64(clk.Now().Unix()) {
		t.Fatalf("state %d since %d for a 10.001 s wait, want degraded since now", state, since)
	}
	clk.Advance(15 * time.Second)
	tracker.Record(community, healthReportAt(true, clk.Now(), 0, 20))
	if state, _ := tracker.Status(community); state != healthReady {
		t.Fatalf("state %d once the wait cleared, want ready", state)
	}
}

func TestDegradedWhenQueueGrowsFor60s(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	tracker := api.NewBotHealth(clk)
	community := id.New()
	// Reports every 15 s with a queue that grows each time.
	for i := 0; i < 5; i++ {
		tracker.Record(community, healthReportAt(true, clk.Now(), uint64(i+1), 100))
		state, _ := tracker.Status(community)
		if i < 4 && state != healthReady {
			t.Fatalf("report %d (t=%ds): state %d, want ready before a full minute of growth", i, i*15, state)
		}
		if i == 4 && state != healthDegraded {
			t.Fatalf("after 60 s of growth: state %d, want degraded", state)
		}
		clk.Advance(15 * time.Second)
	}
	// A queue that is large but steady, or that drops once, is not growing.
	steady := id.New()
	for i := 0; i < 6; i++ {
		tracker.Record(steady, healthReportAt(true, clk.Now(), 50, 100))
		clk.Advance(15 * time.Second)
	}
	if state, _ := tracker.Status(steady); state != healthReady {
		t.Fatalf("steady queue: state %d, want ready", state)
	}
	dipping := id.New()
	for i, depth := range []uint64{1, 2, 3, 2, 3, 4} {
		tracker.Record(dipping, healthReportAt(true, clk.Now(), depth, 100))
		clk.Advance(15 * time.Second)
		_ = i
	}
	if state, _ := tracker.Status(dipping); state != healthReady {
		t.Fatalf("a queue that dropped within the minute: state %d, want ready", state)
	}
}

func TestOfflineWithoutReportFor150s(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	tracker := api.NewBotHealth(clk)
	community := id.New()
	reported := clk.Now()
	tracker.Record(community, healthReportAt(true, clk.Now(), 0, 0))
	clk.Advance(150 * time.Second)
	if state, _ := tracker.Status(community); state != healthReady {
		t.Fatalf("state %d at exactly 150 s of silence, want still ready", state)
	}
	clk.Advance(time.Second)
	if state, since := tracker.Status(community); state != healthOffline || since != uint64(reported.Unix()) {
		t.Fatalf("state %d since %d after 151 s of silence, want offline since the last report", state, since)
	}
	// A report brings it back.
	tracker.Record(community, healthReportAt(true, clk.Now(), 0, 0))
	if state, _ := tracker.Status(community); state != healthReady {
		t.Fatalf("state %d after a new report, want ready", state)
	}
	// A Bot that never reports is offline 150 s after dillad started, not ready for ever.
	never := id.New()
	clk.Advance(151 * time.Second)
	if state, _ := tracker.Status(never); state != healthOffline {
		t.Fatalf("a Bot that never reported: state %d, want offline", state)
	}
}

func TestRepeatedReportsOfOneFailedProbeCountOnce(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	tracker := api.NewBotHealth(clk)
	community := id.New()
	probe := clk.Now()
	for i := 0; i < 4; i++ { // four reports of the same failed probe
		tracker.Record(community, healthReportAt(false, probe, 0, 0))
		clk.Advance(15 * time.Second)
	}
	if state, _ := tracker.Status(community); state != healthReady {
		t.Fatalf("state %d after one failed probe reported four times, want ready", state)
	}
	tracker.Record(community, healthReportAt(false, clk.Now(), 0, 0)) // a second probe fails
	if state, _ := tracker.Status(community); state != healthDegraded {
		t.Fatalf("state %d after two failed probes, want degraded", state)
	}
	tracker.Record(community, healthReportAt(true, clk.Now().Add(time.Second), 0, 0)) // a probe passes
	if state, _ := tracker.Status(community); state != healthReady {
		t.Fatalf("state %d after a passing probe, want ready", state)
	}
}

// The route: only the community's ready Bot reports, over the socket; the catalog's status reads the judgement.
func TestHealthRouteFeedsTheCatalogStatus(t *testing.T) {
	f := newCatalogFixture(t)
	clk, ok := f.deps.Clock.(*clock.Fake)
	if !ok {
		t.Skip("the fixture clock is not a fake")
	}
	put := func(token string, body []byte) int {
		return doCBORAuth(f.h, http.MethodPut, "/v1/bot-runtime/health", body, token).Code
	}
	status := func() (uint64, uint64) {
		resp := f.getCatalog(t, f.memberTok)
		var state, since uint64
		if err := cborx.Unmarshal(resp.Status[0], &state); err != nil {
			t.Fatal(err)
		}
		if resp.Status[1][0] != 0xf6 {
			if err := cborx.Unmarshal(resp.Status[1], &since); err != nil {
				t.Fatal(err)
			}
		}
		return state, since
	}
	if state, _ := status(); state != healthReady {
		t.Fatalf("a fresh ready Bot: state %d, want ready", state)
	}
	// A wrong body is refused; a human member is not the Bot.
	if code := put(f.botTok, mustCBOR(t, []any{uint64(2), uint64(1), uint64(0), uint64(0), uint64(0), nil})); code != http.StatusBadRequest {
		t.Fatalf("probe_ok 2 = %d, want 400", code)
	}
	if code := put(f.memberTok, mustCBOR(t, []any{uint64(1), uint64(1), uint64(0), uint64(0), uint64(0), nil})); code != http.StatusForbidden {
		t.Fatalf("a member's report = %d, want 403", code)
	}
	probeAt := uint64(clk.Now().Unix())
	if code := put(f.botTok, mustCBOR(t, []any{uint64(1), probeAt, uint64(0), uint64(12_000), uint64(0), nil})); code != http.StatusNoContent {
		t.Fatalf("report = %d, want 204", code)
	}
	if state, since := status(); state != healthDegraded || since != probeAt {
		t.Fatalf("after a 12 s wait: state %d since %d, want degraded since %d", state, since, probeAt)
	}
	clk.Advance(151 * time.Second)
	if state, _ := status(); state != healthOffline {
		t.Fatalf("after 151 s of silence: state %d, want offline", state)
	}
}

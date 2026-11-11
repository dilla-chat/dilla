package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/bot/health"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// v0.1.0-bot2.2 (F7): one report that carries failed commands makes the Bot degraded at once and writes bot-health; it
// clears only on a report with no failures and a passing probe.
func TestOneFailedReportIsDegraded(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	path := filepath.Join(t.TempDir(), api.BotHealthFile)
	tracker := api.NewBotHealth(clk)
	tracker.Persist(path, nil)
	community := id.New()
	tracker.Record(community, healthReportAt(true, clk.Now(), 0, 0))
	if state, _ := tracker.Status(community); state != healthReady {
		t.Fatalf("state %d, want ready", state)
	}
	clk.Advance(health.ReportEvery)
	failed := healthReportAt(true, time.Unix(1_800_000_000, 0), 0, 0)
	failed.FailedCommands = 2
	tracker.Record(community, failed)
	if state, _ := tracker.Status(community); state != healthDegraded {
		t.Fatalf("state %d after a report with failed commands, want degraded", state)
	}
	got, err := api.ReadBotHealthStates(path)
	if err != nil || got[community].FailedCommands != 2 || got[community].Since.IsZero() {
		t.Fatalf("bot-health = %+v, %v; want 2 failed commands for %s", got, err, community)
	}
	// A clean report still needs a passing probe: a failed probe keeps it degraded.
	clk.Advance(health.ReportEvery)
	tracker.Record(community, healthReportAt(false, clk.Now(), 0, 0))
	if state, _ := tracker.Status(community); state != healthDegraded {
		t.Fatalf("state %d after a clean report with a failing probe, want degraded", state)
	}
	// More failures add to the count.
	clk.Advance(health.ReportEvery)
	again := healthReportAt(true, clk.Now(), 0, 0)
	again.FailedCommands = 3
	tracker.Record(community, again)
	if got, _ := api.ReadBotHealthStates(path); got[community].FailedCommands != 5 {
		t.Fatalf("count = %d, want 5", got[community].FailedCommands)
	}
	// A clean report with a passing probe clears it, and the file.
	clk.Advance(health.ReportEvery)
	tracker.Record(community, healthReportAt(true, clk.Now(), 0, 0))
	if state, _ := tracker.Status(community); state != healthReady {
		t.Fatalf("state %d after a clean report and a passing probe, want ready", state)
	}
	if got, err := api.ReadBotHealthStates(path); err != nil || len(got) != 0 {
		t.Fatalf("bot-health after recovery = %v, %v; want empty", got, err)
	}
}

// A line without a count (an older dillad's) reads as zero.
func TestBotHealthFileCountIsAdditive(t *testing.T) {
	path := filepath.Join(t.TempDir(), api.BotHealthFile)
	community := id.New()
	if err := os.WriteFile(path, []byte(community.String()+" 1800000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := api.ReadBotHealthStates(path)
	if err != nil || got[community].FailedCommands != 0 || got[community].Since.Unix() != 1_800_000_000 {
		t.Fatalf("states = %+v, %v", got, err)
	}
}

// The report's failed_commands is a seventh element: a report without it (bot2, bot2.1) still has six, and is read as
// zero failures; one with it makes the Bot degraded.
func TestHealthRouteAcceptsFailedCommands(t *testing.T) {
	f := newCatalogFixture(t)
	if _, ok := f.deps.Clock.(*clock.Fake); !ok {
		t.Skip("the fixture clock is not a fake")
	}
	put := func(body []byte) int {
		return doCBORAuth(f.h, http.MethodPut, "/v1/bot-runtime/health", body, f.botTok).Code
	}
	state := func() uint64 {
		resp := f.getCatalog(t, f.memberTok)
		var s uint64
		if err := cborx.Unmarshal(resp.Status[0], &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if code := put(mustCBOR(t, []any{uint64(1), uint64(1), uint64(0), uint64(0), uint64(0), nil})); code != http.StatusNoContent || state() != healthReady {
		t.Fatalf("a six-element report = %d, state %d; want 204 and ready", code, state())
	}
	if code := put(mustCBOR(t, []any{uint64(1), uint64(1), uint64(0), uint64(0), uint64(0), nil, "x"})); code != http.StatusBadRequest {
		t.Fatalf("a non-uint failed_commands = %d, want 400", code)
	}
	if code := put(mustCBOR(t, []any{uint64(1), uint64(1), uint64(0), uint64(0), uint64(0), nil, uint64(2)})); code != http.StatusNoContent || state() != healthDegraded {
		t.Fatalf("a report with failed_commands 2 = %d, state %d; want 204 and degraded", code, state())
	}
}

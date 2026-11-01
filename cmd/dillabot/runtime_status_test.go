package main

import (
	"context"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

type statusReporterFake struct {
	calls    int
	version  *string
	handled  uint64
	lastSeen id.ID
}

func (f *statusReporterFake) ReportStatus(_ context.Context, installation id.ID, version *string, handled uint64) error {
	f.calls++
	f.version, f.handled, f.lastSeen = version, handled, installation
	return nil
}

// Task 10c: a fresh process counts any pending restart as done (starting is the restart), honours
// each later request once, and reports its version and the last request it finished only when
// the server's copy differs. An older server (no widened installation) is never asked anything.
func TestRuntimeStatusTrackerHonoursEachRestartOnceAndReportsWhenStale(t *testing.T) {
	tracker := newRuntimeStatusTracker("0.4.2")
	reporter := &statusReporterFake{}
	ctx := context.Background()
	inst := client.Installation{ID: id.New(), Widened: true, RestartRequested: 2, RestartHandled: 1}
	if tracker.Observe(inst) {
		t.Fatal("first sight restarted again; the process start is the restart")
	}
	if err := tracker.Report(ctx, reporter, inst); err != nil || reporter.calls != 1 || reporter.handled != 2 || reporter.version == nil || *reporter.version != "0.4.2" {
		t.Fatalf("first report = %d calls, handled %d, version %v", reporter.calls, reporter.handled, reporter.version)
	}
	v := "0.4.2"
	inst.RestartHandled, inst.RuntimeVersion = 2, &v
	if tracker.Observe(inst) {
		t.Fatal("no new request but restarted")
	}
	if err := tracker.Report(ctx, reporter, inst); err != nil || reporter.calls != 1 {
		t.Fatalf("steady state reported again: %d calls", reporter.calls)
	}
	// A manager clicks Restart (twice, coalesced by the server into one request).
	inst.RestartRequested = 3
	if !tracker.Observe(inst) {
		t.Fatal("a new restart request was not honoured")
	}
	if tracker.Observe(inst) {
		t.Fatal("the same request was honoured twice")
	}
	if err := tracker.Report(ctx, reporter, inst); err != nil || reporter.calls != 2 || reporter.handled != 3 {
		t.Fatalf("report after restart = %d calls, handled %d", reporter.calls, reporter.handled)
	}
	// An older server: no widened fields, no restart, no report.
	old := client.Installation{ID: id.New(), RestartRequested: 9}
	if tracker.Observe(old) {
		t.Fatal("restarted for a server that cannot ask")
	}
	if err := tracker.Report(ctx, reporter, old); err != nil || reporter.calls != 2 {
		t.Fatal("reported to a server that does not take reports")
	}
	// Forgotten installations start over as a fresh sight.
	tracker.RetainOnly(nil)
	inst.RestartRequested = 4
	if tracker.Observe(inst) {
		t.Fatal("a forgotten installation restarted on first sight")
	}
}

// Fix round 1 (M3): a build without a version reports once (its restart count) and then stays
// quiet, though the server's version stays null.
func TestRuntimeStatusTrackerWithoutAVersionDoesNotRepeatReports(t *testing.T) {
	tracker := newRuntimeStatusTracker("")
	reporter := &statusReporterFake{}
	ctx := context.Background()
	inst := client.Installation{ID: id.New(), Widened: true, RestartRequested: 1, RestartHandled: 0}
	tracker.Observe(inst)
	if err := tracker.Report(ctx, reporter, inst); err != nil || reporter.calls != 1 || reporter.version != nil || reporter.handled != 1 {
		t.Fatalf("first report = %d calls, version %v, handled %d", reporter.calls, reporter.version, reporter.handled)
	}
	inst.RestartHandled = 1
	for range 3 {
		tracker.Observe(inst)
		if err := tracker.Report(ctx, reporter, inst); err != nil || reporter.calls != 1 {
			t.Fatalf("a versionless runtime reported again: %d calls", reporter.calls)
		}
	}
}

// The Bot enforces the Music page's options itself: anything else, or no report, is today's
// behaviour (100, 30 minutes).
func TestMusicDefaultsAreEnforcedByTheBot(t *testing.T) {
	registry := newMusicQueueRegistry()
	installation := id.New()
	if registry.DefaultVolume(installation) != 100 || registry.IdleLeave(installation) != 30*time.Minute {
		t.Fatal("unknown installation is not on today's defaults")
	}
	registry.SetMusicDefaults(installation, true, 50, 15)
	if registry.DefaultVolume(installation) != 50 || registry.IdleLeave(installation) != 15*time.Minute {
		t.Fatal("reported settings not applied")
	}
	for _, bad := range [][2]uint64{{60, 15}, {50, 10}, {300, 15}, {50, 1 << 40}} {
		registry.SetMusicDefaults(installation, true, bad[0], bad[1])
		vol, idle := registry.DefaultVolume(installation), registry.IdleLeave(installation)
		if bad[0] != 50 && vol != 100 || bad[1] != 15 && idle != 30*time.Minute {
			t.Fatalf("out-of-range %v applied: volume %d idle %v", bad, vol, idle)
		}
	}
	registry.SetMusicDefaults(installation, false, 25, 5)
	if registry.DefaultVolume(installation) != 100 || registry.IdleLeave(installation) != 30*time.Minute {
		t.Fatal("an older server's missing settings did not fall back to today's defaults")
	}
}

// gateCreation wires the Polls and Reminders bits to their own create handlers.
func TestUtilityCreationGateReadsTheRightBits(t *testing.T) {
	u := &utilityRuntime{}
	caps := uint16(0)
	u.gateCreation(func(context.Context) (uint16, error) { return caps, nil })
	check := func(wantPoll, wantRemind bool) {
		t.Helper()
		poll, _ := u.polls.CreateEnabled(context.Background())
		remind, _ := u.reminders.CreateEnabled(context.Background())
		if poll != wantPoll || remind != wantRemind {
			t.Fatalf("caps %#x: polls %v reminders %v, want %v %v", caps, poll, remind, wantPoll, wantRemind)
		}
	}
	check(false, false)
	caps = service.CommandCapPolls
	check(true, false)
	caps = service.CommandCapReminders | 0xff
	check(false, true)
}

//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

func backgroundAPI(t *testing.T, active ...id.ID) *client.Client {
	t.Helper()
	instance := id.New()
	document, err := cborx.Marshal([]any{[]any{uint64(6)}, []any{uint64(1)}, []any{uint64(1)}, instance, uint64(1), "probe", uint64(0), []any{uint64(0)}, uint64(1), id.New(), make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]any, 0, len(active))
	for _, installation := range active {
		rows = append(rows, []any{installation, id.New(), uint64(3), uint64(1), id.New(), id.New(), uint64(1), uint64(1), uint64(0), uint64(0), nil, uint64(100), uint64(5)})
	}
	inventory, err := cborx.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := cborx.Marshal([]any{"E_RATE_LIMITED", "try later", nil})
	if err != nil {
		t.Fatal(err)
	}
	api, err := client.New(client.Config{BaseURL: "https://example.invalid", InstanceID: instance,
		RateLimits: client.RateLimits{ReadPerSecond: 1e6, ReadBurst: 1000000},
		HTTPClient: &http.Client{Transport: runtimeRoundTripper(func(r *http.Request) (*http.Response, error) {
			body, status := document, http.StatusOK
			if r.URL.Path == "/v1/bot-runtime/installations" {
				body = inventory
			} else if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/status") {
				// Defer dependency rebuilding after retained maintenance has run.
				body, status = deferred, http.StatusTooManyRequests
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/cbor"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
		})}})
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func backgroundProbe(t *testing.T) func(context.Context) error {
	t.Helper()
	api := backgroundAPI(t)
	return func(ctx context.Context) error { _, err := api.Discover(ctx); return err }
}

func TestStaleMaintenanceDoesNotSkipRecoveredRuntime(t *testing.T) {
	r := newBotGatewayRuntime(t.Context(), nil, false, nil)
	installation := id.New()
	probe := backgroundProbe(t)
	live := false
	runs := 0
	task := func(ctx context.Context) error {
		if err := probe(ctx); err != nil {
			return err
		}
		runs++
		return nil
	}
	r.maintenance = map[id.ID]backgroundMaintenance{installation: {current: func() bool { return live }, run: func(ctx context.Context) error { return runBackgroundMaintenance(ctx, task, task) }}}
	done := make(map[id.ID]bool)
	if err := r.maintainOne(t.Context(), installation, done); err != nil {
		t.Fatal(err)
	}
	live = true
	r.maintenance[installation] = backgroundMaintenance{run: func(ctx context.Context) error { return runBackgroundMaintenance(ctx, task, task) }}
	if err := r.maintainOne(t.Context(), installation, done); err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Fatalf("recovered helper skipped RunDue/maintain: %d runs", runs)
	}
}

func TestMaintenanceErrorsCarryInstallationID(t *testing.T) {
	r := newBotGatewayRuntime(t.Context(), nil, false, nil)
	installation := id.New()
	failure := errors.New("announcement publication failed")
	r.maintenance = map[id.ID]backgroundMaintenance{installation: {run: func(context.Context) error { return failure }}}
	err := r.maintain(t.Context(), make(map[id.ID]bool))
	var scoped *installationMaintenanceError
	if !errors.As(err, &scoped) || scoped.installation != installation || !errors.Is(err, failure) || !strings.Contains(err.Error(), installation.String()) {
		t.Fatalf("maintenance error lost installation or cause: %v", err)
	}
}

func TestMaintenanceRunsOncePerPassAfterTaskFailure(t *testing.T) {
	r := newBotGatewayRuntime(t.Context(), nil, false, nil)
	installation := id.New()
	runs := 0
	failure := errors.New("announcement publication failed")
	r.SetMaintenance(installation, backgroundMaintenance{run: func(context.Context) error {
		runs++
		return failure
	}})
	for range 2 {
		done := make(map[id.ID]bool)
		if err := r.maintain(t.Context(), done); !errors.Is(err, failure) {
			t.Fatalf("maintenance error=%v", err)
		}
		// Rebinding during this pass must not run a retained task twice.
		if err := r.maintainOne(t.Context(), installation, done); err != nil {
			t.Fatal(err)
		}
	}
	if runs != 2 {
		t.Fatalf("maintenance ran %d times in two passes, want 2", runs)
	}
}

func TestMaintenanceRunsEveryFiveSeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		installation := id.New()
		var announcements, utilities []time.Time
		// These are the same two tasks bound to scheduler.RunDue and utilities.maintain in reconcile.
		probe := backgroundProbe(t)
		task := func(times *[]time.Time) func(context.Context) error {
			return func(ctx context.Context) error {
				if err := probe(ctx); err != nil {
					return err
				}
				*times = append(*times, time.Now())
				return nil
			}
		}
		api := backgroundAPI(t, installation)
		if _, err := api.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		runtime, err := api.Runtime("runtime-credential")
		if err != nil {
			t.Fatal(err)
		}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		r := newBotGatewayRuntime(t.Context(), api, false, logger)
		registry := newMusicQueueRegistry()
		defer func() { _ = registry.Close() }()
		cfg := config{StateDir: t.TempDir()}
		statuses := newRuntimeStatusTracker("test")
		r.maintenance = map[id.ID]backgroundMaintenance{installation: {run: func(ctx context.Context) error {
			return runBackgroundMaintenance(ctx, task(&announcements), task(&utilities))
		}}}
		defer r.Close()
		for elapsed := time.Duration(0); elapsed <= 125*time.Second; elapsed += 5 * time.Second {
			if err := reconcile(t.Context(), cfg, api, runtime, nil, registry, r, statuses, logger, nil); err != nil {
				t.Fatalf("reconciliation stopped: %v", err)
			}
			time.Sleep(5 * time.Second)
		}
		for _, times := range [][]time.Time{announcements, utilities} {
			if len(times) != 26 {
				t.Fatalf("maintenance did not run each pass: %d runs", len(times))
			}
			last := now
			for _, at := range times {
				if at.Sub(last) > 60*time.Second {
					t.Fatalf("maintenance gap %s", at.Sub(last))
				}
				last = at
			}
		}
	})
}

func TestRemovedInstallationDoesNotRunMaintenance(t *testing.T) {
	api := backgroundAPI(t)
	if _, err := api.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	runtime, err := api.Runtime("runtime-credential")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := newBotGatewayRuntime(t.Context(), api, false, logger)
	installation := id.New()
	runs := 0
	task := func(ctx context.Context) error {
		if _, err := api.Discover(ctx); err != nil {
			return err
		}
		runs++
		return nil
	}
	r.maintenance = map[id.ID]backgroundMaintenance{installation: {run: func(ctx context.Context) error { return runBackgroundMaintenance(ctx, task, task) }}}
	registry := newMusicQueueRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	err = reconcile(t.Context(), config{StateDir: t.TempDir()}, api, runtime, nil, registry, r, newRuntimeStatusTracker("test"), logger, nil)
	if runs != 0 {
		t.Fatalf("removed installation ran RunDue/maintain %d times", runs)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := r.maintenance[installation]; exists {
		t.Fatal("removed installation retained maintenance")
	}
}

// A helper that died after a good pass must be replaced: maintenance meeting a closed helper asks for a pass of that
// installation, and only that one (v0.1.0-bot2 on CT 144 logged "helper client is closed" every 10 s forever).
func TestMaintenanceMeetingAClosedHelperAsksForAPassOfThatInstallation(t *testing.T) {
	r := newBotGatewayRuntime(context.Background(), nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	dead, healthy := id.New(), id.New()
	r.SetMaintenance(dead, backgroundMaintenance{run: func(context.Context) error {
		return fmt.Errorf("reminder tick: %w", helper.ErrClosed)
	}})
	r.SetMaintenance(healthy, backgroundMaintenance{run: func(context.Context) error { return errors.New("transient") }})
	if err := r.maintainDue(context.Background()); !errors.Is(err, helper.ErrClosed) {
		t.Fatalf("maintainDue = %v, want the closed helper reported", err)
	}
	select {
	case <-r.Pass():
	default:
		t.Fatal("no pass was requested for the installation whose helper is closed")
	}
	targets, _ := r.takePassTargets()
	if !targets[dead] || targets[healthy] || len(targets) != 1 {
		t.Fatalf("pass targets = %v, want only the dead helper's installation", targets)
	}
}

// Fix round, I-1 (a) and (c): the maintenance at the start of a pass, which runs before the pass replaces unusable
// helpers, asks for no pass when it meets a gone helper, does not fail the pass, and leaves the installation's own run
// on the replacement to the pass. The installation's own run on a replacement that is gone fails that installation.
func TestPassStartMaintenanceMeetingAGoneHelperAsksForNothing(t *testing.T) {
	r := newBotGatewayRuntime(t.Context(), nil, true, quietLogger())
	defer r.Close()
	installation := id.New()
	r.SetMaintenance(installation, backgroundMaintenance{run: func(context.Context) error {
		return fmt.Errorf("reminder tick: %w", helper.ErrClosed)
	}})
	done := map[id.ID]bool{}
	if err := r.maintain(t.Context(), done); err != nil {
		t.Fatalf("pass-start maintenance = %v, want a gone helper left to the pass, not an error of it", err)
	}
	select {
	case <-r.Pass():
		t.Fatal("the pass-start maintenance asked for another pass from inside the pass")
	default:
	}
	if done[installation] {
		t.Fatal("the gone helper's installation is marked done: its replacement's run would be skipped")
	}
	runs := 0
	r.SetMaintenance(installation, backgroundMaintenance{run: func(context.Context) error { runs++; return nil }})
	if err := r.maintainOne(t.Context(), installation, done); err != nil || runs != 1 {
		t.Fatalf("replacement's run in the pass: err=%v runs=%d, want one run", err, runs)
	}
	// A replacement that is gone in its own run fails the installation (the loop retries it on instFail), asking for
	// no pass itself.
	r.SetMaintenance(installation, backgroundMaintenance{run: func(context.Context) error { return nil }, usable: func() bool { return false }})
	if err := r.maintainOne(t.Context(), installation, map[id.ID]bool{}); !errors.Is(err, helper.ErrClosed) {
		t.Fatalf("in-pass run on an unusable helper = %v, want ErrClosed", err)
	}
	select {
	case <-r.Pass():
		t.Fatal("the in-pass run asked for a pass")
	default:
	}
}

// Fix round, I-1 (b): the due tick asks for a pass at once the first time a helper is gone, then on a doubling backoff
// up to helperRespawnCap; the ticks in between ask for nothing; a replacement that serves a run resets it.
func TestHelperRespawnBackoffDoublesAndResetsWhenAReplacementServes(t *testing.T) {
	var delays []time.Duration
	var scheduled []func()
	defer func(f func(time.Duration, func())) { helperRespawnAfter = f }(helperRespawnAfter)
	helperRespawnAfter = func(d time.Duration, f func()) { delays = append(delays, d); scheduled = append(scheduled, f) }
	r := newBotGatewayRuntime(t.Context(), nil, true, quietLogger())
	defer r.Close()
	installation := id.New()
	gone := backgroundMaintenance{run: func(context.Context) error { return helper.ErrClosed }}
	asked := func() bool {
		targets, _ := r.takePassTargets()
		return targets[installation]
	}
	r.SetMaintenance(installation, gone)
	_ = r.maintainDue(t.Context())
	if !asked() || len(delays) != 0 {
		t.Fatalf("first gone helper: asked=false or delayed %v, want a pass at once", delays)
	}
	for range 8 {
		// Two ticks while an ask is scheduled schedule one ask.
		_ = r.maintainDue(t.Context())
		_ = r.maintainDue(t.Context())
		if asked() {
			t.Fatal("a tick during the backoff asked for a pass at once")
		}
		scheduled[len(scheduled)-1]()
		if !asked() {
			t.Fatal("the scheduled ask asked for no pass")
		}
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 300 * time.Second, 300 * time.Second, 300 * time.Second}
	if !slices.Equal(delays, want) {
		t.Fatalf("respawn delays %v, want %v", delays, want)
	}
	// v0.1.0-bot2.2: a replacement resets the backoff only once it has stayed usable for helperServedFor.
	now := time.Now()
	r.now = func() time.Time { return now }
	r.SetMaintenance(installation, backgroundMaintenance{run: func(context.Context) error { return nil }})
	_ = r.maintainDue(t.Context())
	now = now.Add(helperServedFor)
	_ = r.maintainDue(t.Context())
	r.SetMaintenance(installation, gone)
	delays = nil
	_ = r.maintainDue(t.Context())
	if !asked() || len(delays) != 0 {
		t.Fatalf("after a replacement served, the next gone helper waited %v, want a pass at once", delays)
	}
}

// Fix round, I-1: a helper that dies on its first maintenance call after every respawn. Before, the pass-start
// maintenance of each replacing pass asked for the next pass, and the helper was respawned every ~2 s for good. Now
// the Bot's loop runs passes for it no faster than the respawn backoff: at once, then 10 s doubling to 5 min.
func TestAHelperThatDiesAfterEveryRespawnIsNotRespawnedFasterThanTheBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		r := newBotGatewayRuntime(ctx, nil, true, quietLogger())
		installation := id.New()
		respawns := 0
		alwaysGone := func() backgroundMaintenance {
			return backgroundMaintenance{run: func(context.Context) error { return fmt.Errorf("reminder tick: %w", helper.ErrClosed) }}
		}
		var passes []time.Time
		start := time.Now()
		loop := botLoop{clock: clock.System(), log: quietLogger(), passes: r.Pass(), take: r.takePassTargets,
			reload: make(chan os.Signal), wake: make(chan struct{}), poll: func(context.Context) {},
			maintain: r.maintainDue, due: dueInterval,
			pass: func(passCtx context.Context, targets map[id.ID]bool) error {
				passes = append(passes, time.Now())
				// As reconcile: maintenance first, then each covered installation; its helper is gone, so the pass
				// starts another, which will die on its first maintenance call too.
				done := map[id.ID]bool{}
				if err := r.maintain(passCtx, done); err != nil {
					return err
				}
				if targets == nil || targets[installation] {
					respawns++
					r.SetMaintenance(installation, alwaysGone())
				}
				return nil
			}}
		finished := make(chan struct{})
		go func() { _ = loop.run(ctx); close(finished) }()
		time.Sleep(30 * time.Minute)
		cancel()
		<-finished
		r.Close()
		var gaps []time.Duration
		for i := 1; i < len(passes); i++ {
			gaps = append(gaps, passes[i].Sub(passes[i-1]))
		}
		// At once, then 10, 20, 40, 80, 160 s and 5 min apart, shifted by up to a 10 s tick: about 11 in 30 min.
		// Without the backoff it would be one every 10 s (180); with the pass-start ask, one every 2 s (about 900).
		if len(passes) > 13 {
			t.Fatalf("%d passes (%d respawns) in 30 min, want at most 13 on the respawn backoff", len(passes), respawns)
		}
		t.Logf("%d passes in 30 min, %d respawns, gaps %v", len(passes), respawns, gaps)
		if len(gaps) < 3 || gaps[len(gaps)-1] < helperRespawnCap || gaps[len(gaps)-2] < helperRespawnCap {
			t.Fatalf("pass gaps %v, want the backoff at its %v cap by the end", gaps, helperRespawnCap)
		}
		if time.Since(start) < 30*time.Minute {
			t.Fatal("fake time did not advance")
		}
	})
}

// captureRespawns records the respawn backoff's scheduled asks instead of running them on a timer.
func captureRespawns(t *testing.T) (delays func() []time.Duration, fire func()) {
	t.Helper()
	var d []time.Duration
	var f []func()
	old := helperRespawnAfter
	helperRespawnAfter = func(delay time.Duration, ask func()) { d, f = append(d, delay), append(f, ask) }
	t.Cleanup(func() { helperRespawnAfter = old })
	return func() []time.Duration { return slices.Clone(d) }, func() { f[len(f)-1]() }
}

// F4: the text poller bound to a helper that closed is not run against it, and says nothing per tick; the pass that
// replaces the helper binds a new poller.
func TestTextPollerSkipsClosedHelper(t *testing.T) {
	installation, community := id.New(), id.New()
	registry := newMusicQueueRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	n := newUsableTestNative(installation, community)
	if err := registry.RetainInteractions(installation, community, n); err != nil {
		t.Fatal(err)
	}
	logs := &lockedLog{}
	r := newBotGatewayRuntime(t.Context(), nil, true, slog.New(slog.NewTextHandler(logs, nil)))
	defer r.Close()
	polls := 0
	r.SetTextPoller(installation, helperBoundPoller(registry, installation, community, n, func(context.Context) error {
		polls++
		if !n.Usable() {
			return helper.ErrClosed
		}
		return nil
	}))
	r.PollText(t.Context())
	if polls != 1 {
		t.Fatalf("the poller on a usable helper ran %d times, want 1", polls)
	}
	n.dead.Store(true)
	for range 3 {
		r.PollText(t.Context())
	}
	if polls != 1 || logs.String() != "" {
		t.Fatalf("the poller ran %d times on a closed helper; logs:\n%s", polls, logs)
	}
}

// F8: the due tick met the same closed client every 10 s and logged an invented "helper client is closed" each time.
// Now it calls nothing on it, logs "bot native helper gone" once per client and one line per respawn ask, which follow
// the backoff.
func TestDueTickLogsGoneOncePerClient(t *testing.T) {
	delays, fire := captureRespawns(t)
	logs := &lockedLog{}
	r := newBotGatewayRuntime(t.Context(), nil, true, slog.New(slog.NewTextHandler(logs, nil)))
	defer r.Close()
	installation := id.New()
	runs := 0
	dead := func(client *native.Client) backgroundMaintenance {
		return backgroundMaintenance{client: client, usable: func() bool { return false }, run: func(context.Context) error { runs++; return nil }}
	}
	first := new(native.Client)
	r.SetMaintenance(installation, dead(first))
	asked := func() bool { targets, _ := r.takePassTargets(); return targets[installation] }
	for tick := range 3 {
		if err := r.maintainDue(t.Context()); err != nil {
			t.Fatalf("tick %d = %v, want no invented error", tick, err)
		}
		if got := asked(); got != (tick == 0) {
			t.Fatalf("tick %d asked=%v", tick, got)
		}
	}
	fire()
	if !asked() {
		t.Fatal("the scheduled ask asked for no pass")
	}
	_ = r.maintainDue(t.Context())
	if !slices.Equal(delays(), []time.Duration{10 * time.Second, 20 * time.Second}) {
		t.Fatalf("respawn delays %v, want [10s 20s]", delays())
	}
	if runs != 0 {
		t.Fatalf("maintenance ran %d times on a closed client", runs)
	}
	if c := logs.count("bot native helper gone"); c != 1 || logs.count("installation_id="+installation.String()) < 1 {
		t.Fatalf("%d gone lines, want 1:\n%s", c, logs)
	}
	if c := logs.count("bot native helper respawn requested"); c != 2 {
		t.Fatalf("%d respawn lines, want one per ask (2):\n%s", c, logs)
	}
	if strings.Contains(logs.String(), "helper client is closed") {
		t.Fatalf("an invented close error was logged:\n%s", logs)
	}
	// Another client that is gone is logged once too.
	r.SetMaintenance(installation, dead(new(native.Client)))
	_ = r.maintainDue(t.Context())
	_ = r.maintainDue(t.Context())
	if c := logs.count("bot native helper gone"); c != 2 {
		t.Fatalf("%d gone lines after a second dead client, want 2:\n%s", c, logs)
	}
}

// CT 144 (bot2.1) respawned 10/10 s, 20/10 s, 40/30 s apart: a replacement that served one due tick reset the backoff,
// so a helper dying on the same work after every respawn was replaced at once each time. Only a client that stayed
// usable for helperServedFor resets it.
func TestBackoffNotResetByShortLivedReplacement(t *testing.T) {
	delays, fire := captureRespawns(t)
	r := newBotGatewayRuntime(t.Context(), nil, true, quietLogger())
	defer r.Close()
	now := time.Now()
	r.now = func() time.Time { return now }
	installation := id.New()
	asked := func() bool { targets, _ := r.takePassTargets(); return targets[installation] }
	tick := func() { now = now.Add(dueInterval); _ = r.maintainDue(t.Context()) }
	use := func() func() {
		usable := true
		client := new(native.Client)
		r.SetMaintenance(installation, backgroundMaintenance{client: client, usable: func() bool { return usable }, run: func(context.Context) error { return nil }})
		return func() { usable = false }
	}
	die := use()
	die()
	tick() // at once
	tick() // scheduled after 10 s
	fire()
	if !asked() || !slices.Equal(delays(), []time.Duration{10 * time.Second}) {
		t.Fatalf("delays %v", delays())
	}
	die = use()
	tick() // the replacement serves one tick
	die()
	tick()
	if asked() {
		t.Fatal("a replacement that served one tick reset the backoff: asked at once")
	}
	if !slices.Equal(delays(), []time.Duration{10 * time.Second, 20 * time.Second}) {
		t.Fatalf("delays %v, want the next ask after 20 s", delays())
	}
	fire()
	_ = asked()
	die = use()
	for range int(helperServedFor/dueInterval) + 1 {
		tick()
	}
	die()
	tick()
	if !asked() || len(delays()) != 2 {
		t.Fatalf("a replacement usable for %v did not reset the backoff (delays %v)", helperServedFor, delays())
	}
}

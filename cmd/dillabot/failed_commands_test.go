//go:build linux

package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/health"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// failedNow is the failed commands the next health report would carry (and resets them, as the report does).
func failedNow(e *sealedEntry) uint32 {
	return e.healthLoad(time.Now(), nil).FailedCommands
}

// v0.1.0-bot2.2 (F7): a command whose 200 answer could not be sealed was counted as answered; it is a failed command,
// and the next report carries it once. The answer is the command's own (not a refusal, which is counted on its own): the
// helper's seal fails with a closed helper and no replacement arrives within the park.
func TestSealFailureCountsAsFailed(t *testing.T) {
	shortHelperPark(t, 20*time.Millisecond)
	w := newLoadWorld(t, 3, true)
	logs := &lockedLog{}
	reg := &registeredHelper{}
	a := w.helper
	reg.set(a)
	followRegistry(w, reg, slog.New(slog.NewTextHandler(logs, nil)))
	a.sealErr = helper.ErrClosed // dies at the seal of the handler's answer
	w.entry.in.Handle(t.Context(), w.commands[0])
	if logs.count("bot command answer not sealed") != 1 || logs.count("bot command refused") != 0 {
		t.Fatalf("want the command's own answer unsealed, not a refusal:\n%s", logs)
	}
	if got := failedNow(w.entry); got != 1 {
		t.Fatalf("failed commands = %d, want 1", got)
	}
	if got := failedNow(w.entry); got != 0 {
		t.Fatalf("the next report still carries %d failures, want 0", got)
	}
}

// An open that fails (no usable helper) is a failed command.
func TestOpenFailureCountsAsFailed(t *testing.T) {
	shortHelperPark(t, 20*time.Millisecond)
	w := newLoadWorld(t, 3, true)
	reg := &registeredHelper{}
	a := w.helper
	reg.set(a)
	followRegistry(w, reg, slog.New(slog.NewTextHandler(&lockedLog{}, nil)))
	a.close()
	w.entry.in.Handle(t.Context(), w.commands[0])
	if got := failedNow(w.entry); got != 1 {
		t.Fatalf("failed commands = %d, want 1", got)
	}
}

// The retryable refusal of a missing helper (helper_gone) is a failure for the member even though its answer is sealed
// on the replacement; it counts once.
func TestOfflineRefusalCountsOnce(t *testing.T) {
	shortHelperPark(t, 5*time.Second)
	w := newLoadWorld(t, 3, true)
	logs := &lockedLog{}
	reg := &registeredHelper{}
	a, b := w.helper, &loadHelper{requests: w.helper.requests}
	reg.set(a)
	followRegistry(w, reg, slog.New(slog.NewTextHandler(logs, nil)))
	a.onOpen = a.close
	done := make(chan struct{})
	go func() { w.handleNext(t.Context()); close(done) }()
	waitUntil(t, func() bool { return logs.count("bot command answer waiting for a replacement helper") == 1 })
	reg.set(b)
	<-done
	answer, _ := w.lastAnswer()
	if answer.Status != intake.StatusOffline {
		t.Fatalf("answer %+v, want the offline refusal", answer)
	}
	if got := failedNow(w.entry); got != 1 {
		t.Fatalf("failed commands = %d, want 1 for the offline refusal", got)
	}
}

// A start-up refusal (the feed is not ready yet, with a usable helper) is not a failed command.
func TestStartupRefusalIsNotAFailedCommand(t *testing.T) {
	w := newLoadWorld(t, 3, true)
	logs := &lockedLog{}
	w.entry.in.SetLogger(slog.New(slog.NewTextHandler(logs, nil)))
	w.entry.in.SetReady(func() bool { return false })
	w.entry.in.SetParkWindow(0)
	w.entry.in.Handle(t.Context(), w.commands[0])
	answer, _ := w.lastAnswer()
	if answer.Status != intake.StatusOffline || logs.count("class=not_ready") != 1 {
		t.Fatalf("answer %+v; logs:\n%s; want the not_ready refusal", answer, logs)
	}
	if got := failedNow(w.entry); got != 0 {
		t.Fatalf("failed commands = %d for a start-up refusal, want 0", got)
	}
}

// A command that is answered does not count.
func TestAnsweredCommandIsNotFailed(t *testing.T) {
	w := newLoadWorld(t, 3, true)
	w.handleNext(t.Context())
	if answer, n := w.lastAnswer(); n == 0 || answer.Status >= 400 {
		t.Fatalf("answer %+v (%d), want the command's answer", answer, n)
	}
	if got := failedNow(w.entry); got != 0 {
		t.Fatalf("failed commands = %d, want 0", got)
	}
}

// A failure is counted once per queued command however many answers fail, and the bookkeeping ends with the command:
// a failure after (or without) its handling leaves nothing behind.
func TestFailureCountsOncePerCommand(t *testing.T) {
	var l commandLoad
	request := id.New()
	l.enqueue(request, time.Now())
	l.fail(request)
	l.fail(request)
	l.answered(request)
	l.fail(request) // after the handling ended: counted, and leaves no entry
	l.failOpen()
	if got := l.takeFailed(); got != 3 {
		t.Fatalf("failures = %d, want 3", got)
	}
	if len(l.failedBy) != 0 {
		t.Fatalf("%d entries left behind", len(l.failedBy))
	}
}

// The Bot's own probe has its own signal (probe_ok): its failure is not a failed command.
func TestProbeIsNotAFailedCommand(t *testing.T) {
	w := newLoadWorld(t, 3, true)
	request := id.New()
	_, forget := w.entry.probes.begin(request)
	defer forget()
	w.entry.failCommand(request)
	if got := failedNow(w.entry); got != 0 {
		t.Fatalf("failed commands = %d for a probe, want 0", got)
	}
	w.entry.failCommand(id.New())
	if got := failedNow(w.entry); got != 1 {
		t.Fatalf("failed commands = %d for a command, want 1", got)
	}
}

// A report that could not be sent gives its failures back: the next successful one carries them.
type flakySession struct {
	sealedSession
	mu    sync.Mutex
	fails int
	last  uint32
}

func (f *flakySession) ReportHealth(_ context.Context, r client.HealthReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return errors.New("dillad unreachable")
	}
	f.last = r.FailedCommands
	return nil
}

func TestFailedReportKeepsItsCount(t *testing.T) {
	w := newLoadWorld(t, 3, true)
	session := &flakySession{sealedSession: w.session, fails: 1}
	w.entry.mu.Lock()
	w.entry.cfg.session = session
	w.entry.mu.Unlock()
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	monitor := w.entry.newHealthMonitor(nil, clk)
	w.entry.load.failOpen()
	w.entry.load.failOpen()
	monitor.Tick(t.Context()) // the send fails
	clk.Advance(health.ReportEvery)
	monitor.Tick(t.Context())
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.last != 2 {
		t.Fatalf("the next report carried %d failures, want the 2 the failed one held", session.last)
	}
}

// Both parks share one budget measured from when the command was taken: a command that already waited does not wait the
// whole limit again.
func TestParkBudgetIsPerCommand(t *testing.T) {
	shortHelperPark(t, 300*time.Millisecond)
	w := newLoadWorld(t, 3, true)
	reg := &registeredHelper{}
	followRegistry(w, reg, slog.New(slog.NewTextHandler(&lockedLog{}, nil)))
	ctx := withCommandStart(t.Context(), time.Now().Add(-280*time.Millisecond))
	start := time.Now()
	if h := w.entry.awaitHelper(ctx, nil, "bot command waiting for a replacement helper"); h != nil {
		t.Fatal("a helper appeared from nowhere")
	}
	if waited := time.Since(start); waited > 150*time.Millisecond {
		t.Fatalf("waited %v with 20 ms of the budget left, want the rest of it only", waited)
	}
}

// v0.1.0-bot2.2 (F7): an installation's helper going away or being replaced probes at once, not at the next 60 s.
func TestHelperChangeProbesAtOnce(t *testing.T) {
	delays, fire := captureRespawns(t)
	_, _ = delays, fire
	r := newBotGatewayRuntime(t.Context(), nil, true, quietLogger())
	defer r.Close()
	installation, community := id.New(), id.New()
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	var probes atomic.Int32
	e := r.sealed.entry(installation, community)
	e.mu.Lock()
	e.monitor = health.New(health.ProberFunc(func(context.Context) error { probes.Add(1); return nil }),
		health.ReporterFunc(func(context.Context, health.Report) error { return nil }), clk)
	e.mu.Unlock()
	tick := func() { clk.Advance(health.ReportEvery); e.monitor.Tick(t.Context()) }
	tick() // the first probe
	tick()
	if got := probes.Load(); got != 1 {
		t.Fatalf("%d probes, want 1", got)
	}
	usable := true
	first := new(native.Client)
	r.SetMaintenance(installation, backgroundMaintenance{client: first, usable: func() bool { return usable }, run: func(context.Context) error { return nil }})
	_ = r.maintainDue(t.Context()) // the helper serves: no change yet
	tick()
	if got := probes.Load(); got != 1 {
		t.Fatalf("%d probes while the helper serves, want 1", got)
	}
	usable = false
	_ = r.maintainDue(t.Context()) // the due tick finds it gone
	tick()
	if got := probes.Load(); got != 2 {
		t.Fatalf("%d probes after the helper went, want 2 (within one tick)", got)
	}
	second := new(native.Client)
	r.SetMaintenance(installation, backgroundMaintenance{client: second, usable: func() bool { return true }, run: func(context.Context) error { return nil }})
	_ = r.maintainDue(t.Context()) // the replacement serves its first run
	tick()
	if got := probes.Load(); got != 3 {
		t.Fatalf("%d probes after the replacement registered, want 3 (within one tick)", got)
	}
	_ = r.maintainDue(t.Context())
	tick()
	if got := probes.Load(); got != 3 {
		t.Fatalf("%d probes while the replacement keeps serving, want 3", got)
	}
}

// Task 3 carry-over: the boundary of the backoff reset. A replacement that serves for 50 s (under helperServedFor) and
// then dies keeps the doubled delay: the next ask waits 20 s, it is not made at once.
func TestBackoffKeptWhenReplacementServes50s(t *testing.T) {
	delays, fire := captureRespawns(t)
	r := newBotGatewayRuntime(t.Context(), nil, true, quietLogger())
	defer r.Close()
	now := time.Now()
	r.now = func() time.Time { return now }
	installation := id.New()
	asked := func() bool { targets, _ := r.takePassTargets(); return targets[installation] }
	tick := func() { now = now.Add(dueInterval); _ = r.maintainDue(t.Context()) }
	usable := true
	use := func() {
		usable = true
		r.SetMaintenance(installation, backgroundMaintenance{client: new(native.Client), usable: func() bool { return usable }, run: func(context.Context) error { return nil }})
	}
	use()
	usable = false
	tick() // at once
	tick() // scheduled after 10 s
	fire()
	_ = asked()
	use()
	for range int((helperServedFor - dueInterval) / dueInterval) {
		tick() // serves at +0 s .. +40 s
	}
	tick() // +50 s: still under helperServedFor
	usable = false
	tick()
	if asked() {
		t.Fatal("a replacement that served 50 s reset the backoff: asked at once")
	}
	if !slices.Equal(delays(), []time.Duration{10 * time.Second, 20 * time.Second}) {
		t.Fatalf("delays %v, want 10 s then 20 s", delays())
	}
}

// F4 carry-over: parked commands give up under the client's 5 s first-answer window, so the 503 E_BOT_OFFLINE that
// triggers its automatic resend arrives before "The Bot didn't answer".
func TestHelperParkLimitIsUnderTheClientFirstAnswerWindow(t *testing.T) {
	if helperParkLimit != 4*time.Second {
		t.Fatalf("helperParkLimit = %v, want 4s (the client's BOT_FIRST_ANSWER_MS is 5 s)", helperParkLimit)
	}
}

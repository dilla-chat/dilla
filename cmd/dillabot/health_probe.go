//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/bot/health"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 17: the Bot's own health. Every 60 s it sends itself an OpProbe = 90 command along the whole command path
// and reports every 15 s whether the probe was answered, with the backlog and the oldest command's wait.
//
// Fix round 2 (C1, I1): the probe is the real thing. The helper seals an OpProbe to the community's current command key
// as the Bot's own device (op 25) and keeps the reply secret. The Bot POSTs it to …/bot/commands like a member's device;
// dillad attaches the Bot's own session and relays it back as op 64; the delivery loop hands it to the intake with the
// feed barrier; a worker opens it in the helper (op 22: the Bot's own device list, as dillad's feed pushed it), checks
// Held and readiness and answers through the helper's seal (op 23) and POST /v1/bot-runtime/replies; dillad delivers
// op 62 to the Bot's own connection, and the helper opens it (op 26). A helper that errors or hangs, a stuck feed, a
// wedged delivery loop, a full queue, workers that do not run, and a broken relay or reply route all fail it, because
// each is on its path. Nothing short-circuits: a probe is a command among the others.

// probeAnswerLimit bounds how long the probe waits for its answer (a variable for tests).
var probeAnswerLimit = health.ProbeTimeout

// probeHelper is the helper's half of the probe: op 25 seals it, op 26 opens its answers.
type probeHelper interface {
	SealProbe(ctx context.Context, community, request id.ID, now time.Time) ([]byte, uint32, error)
	OpenProbeReply(ctx context.Context, handle, seq uint32, sealed []byte, forget bool) ([]byte, error)
}

// probeSender sends the sealed probe through dillad's relay (client.Session.SendCommand).
type probeSender interface {
	SendCommand(ctx context.Context, community, request id.ID, sealed []byte) error
}

// probeTable is the probes waiting for their answer (op 62), by request id. The zero value is ready.
type probeTable struct {
	mu      sync.Mutex
	pending map[id.ID]chan gatewayclient.BotReply
}

func (p *probeTable) begin(request id.ID) (<-chan gatewayclient.BotReply, func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = map[id.ID]chan gatewayclient.BotReply{}
	}
	ch := make(chan gatewayclient.BotReply, intake.MaxAnswers)
	p.pending[request] = ch
	return ch, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		delete(p.pending, request)
	}
}

// deliver hands an op 62 to the probe waiting for it, and reports whether one was.
func (p *probeTable) deliver(reply gatewayclient.BotReply) bool {
	p.mu.Lock()
	ch := p.pending[reply.RequestID]
	p.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- reply:
	default:
	}
	return true
}

// probeReply routes an op 62 that reached the installation's connection to its probe. The Bot sends no other command,
// so any other answer is dropped.
func (r *botGatewayRuntime) probeReply(installation id.ID, reply *gatewayclient.BotReply) {
	if reply == nil {
		return
	}
	if e := r.sealed.lookup(installation); e == nil || !e.probes.deliver(*reply) {
		r.log.Debug("bot reply for no waiting probe dropped", "installation_id", installation.String())
	}
}

// Probe sends one OpProbe along the command path and returns nil when it was answered 200 within probeAnswerLimit.
func (e *sealedEntry) Probe(ctx context.Context) error {
	cfg, _ := e.current()
	// The installation's current helper (v0.1.0-bot2.2, F4); a closed one is not called.
	h, okHelper := e.helperNow().(probeHelper)
	sender, okSender := cfg.session.(probeSender)
	if !okHelper || !okSender || cfg.botUser.IsZero() {
		return errSealedNotConfigured
	}
	clk := e.probeClock()
	timeout := clk.NewTimer(probeAnswerLimit)
	defer timeout.Stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-timeout.C():
			cancel()
		case <-ctx.Done():
		}
	}()
	request := id.New()
	answers, forget := e.probes.begin(request)
	defer forget()
	sealed, handle, err := h.SealProbe(ctx, e.community, request, clk.Now())
	if err != nil {
		return probeFailure(ctx, "seal", err)
	}
	if err := sender.SendCommand(ctx, e.community, request, sealed); err != nil {
		return probeFailure(ctx, "relay", err)
	}
	for {
		select {
		case reply := <-answers:
			body, err := h.OpenProbeReply(ctx, handle, reply.Seq, reply.Sealed, reply.Final)
			if err != nil {
				return probeFailure(ctx, "open answer", err)
			}
			answer, err := intake.DecodeAnswer(body)
			if err != nil {
				return fmt.Errorf("bot probe answer: %w", err)
			}
			if !answer.Final {
				continue
			}
			if answer.Status != intake.StatusOK {
				return fmt.Errorf("bot intake refused the probe: %d %s", answer.Status, answer.Body)
			}
			return nil
		case <-ctx.Done():
			return probeFailure(ctx, "answer", ctx.Err())
		}
	}
}

// probeFailure names the step that failed; one cut off by the answer limit is a deadline failure.
func probeFailure(ctx context.Context, step string, err error) error {
	if ctx.Err() != nil && !errors.Is(err, context.DeadlineExceeded) {
		err = errors.Join(context.DeadlineExceeded, err)
	}
	return fmt.Errorf("bot probe %s: %w", step, err)
}

// probeClock is the clock the probe's answer deadline runs on (the system's unless a test set one).
func (e *sealedEntry) probeClock() clock.Clock {
	if e.clk != nil {
		return e.clk
	}
	return clock.System()
}

// commandLoad tracks the commands relayed to an installation and not yet answered (fix round 2, M3: a command a worker
// took but has not finished, inside a hanging helper call or a blocked handler, still counts). The zero value is ready.
type commandLoad struct {
	mu     sync.Mutex
	queued map[id.ID]time.Time
	// failed is the commands that failed since the last report, and failedBy the requests already counted: a command
	// is counted once, however many of its steps fail, until its handling ends (answered).
	failed   uint32
	failedBy map[id.ID]struct{}
}

func (l *commandLoad) enqueue(request id.ID, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.queued == nil {
		l.queued = map[id.ID]time.Time{}
	}
	if _, ok := l.queued[request]; !ok {
		l.queued[request] = at
	}
}

// answered drops a command whose handling ended (its last answer sent, or dropped).
func (l *commandLoad) answered(request id.ID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.queued, request)
	delete(l.failedBy, request)
}

// fail counts request as a failed command, once: its answer could not be sealed or delivered, or was the retryable
// offline refusal.
func (l *commandLoad) fail(request id.ID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, counted := l.failedBy[request]; counted {
		return
	}
	if l.failedBy == nil {
		l.failedBy = map[id.ID]struct{}{}
	}
	l.failedBy[request] = struct{}{}
	l.failed++
}

// failOpen counts a command whose open failed. The open does not know the request (its id is inside the seal), and a
// command is opened once.
func (l *commandLoad) failOpen() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed++
}

// takeFailed is the failed commands since the last call, and starts the count again.
func (l *commandLoad) takeFailed() uint32 {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.failed
	l.failed = 0
	return n
}

// snapshot is the commands not yet answered and how long the oldest has waited.
func (l *commandLoad) snapshot(now time.Time) (depth uint64, oldest time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, at := range l.queued {
		if wait := now.Sub(at); wait > oldest {
			oldest = wait
		}
	}
	return uint64(len(l.queued)), oldest
}

// healthReporter is the part of the Bot session that reports health.
type healthReporter interface {
	ReportHealth(ctx context.Context, r client.HealthReport) error
}

// startHealth (called with e.mu held) starts the installation's health monitor, once: it probes the command path every
// 60 s and reports every 15 s until ctx ends. musicP95 reads the installation's music start timings (zero without any).
func (e *sealedEntry) startHealth(ctx context.Context, musicP95 func() time.Duration, clk clock.Clock) {
	reporter := health.ReporterFunc(func(reportCtx context.Context, r health.Report) error {
		cfg, _ := e.current()
		if cfg.healthDir != "" {
			state := healthState{ProbeOK: r.ProbeOK, ProbeAt: r.ProbeAt, ReportedAt: r.ReportedAt}
			if r.FailingSince != nil {
				state.FailingSince = *r.FailingSince
			}
			_ = writeHealthState(cfg.healthDir, state) // doctor's view; a failed write loses only that
		}
		session, ok := cfg.session.(healthReporter)
		if !ok {
			return nil
		}
		report := client.HealthReport{ProbeOK: r.ProbeOK, ProbeAt: r.ProbeAt, QueueDepth: r.QueueDepth, OldestWaitMS: r.OldestWaitMS, MusicStartP95: r.MusicStartP95, FailingSince: r.FailingSince, FailedCommands: r.FailedCommands}
		return session.ReportHealth(reportCtx, report)
	})
	monitor := health.New(health.ProberFunc(e.Probe), reporter, clk)
	monitor.SetLoad(func() health.Load { return e.healthLoad(clk.Now(), musicP95) })
	// The caller (SetSealedIntake) holds e.mu.
	e.monitor = monitor
	go monitor.Run(ctx)
}

// healthLoad is the installation's load for one report: its backlog, its music start timings, and the commands that
// failed since the last report (which this read resets).
func (e *sealedEntry) healthLoad(now time.Time, musicP95 func() time.Duration) health.Load {
	depth, oldest := e.load.snapshot(now)
	var p95 time.Duration
	if musicP95 != nil {
		p95 = musicP95()
	}
	return health.Load{QueueDepth: depth, OldestWait: oldest, MusicStartP95: p95, FailedCommands: e.load.takeFailed()}
}

// FailingSince is when the installation's commands began failing, as its last probe saw it (doctor).
func (e *sealedEntry) FailingSince() (time.Time, bool) {
	e.mu.Lock()
	monitor := e.monitor
	e.mu.Unlock()
	if monitor == nil {
		return time.Time{}, false
	}
	return monitor.FailingSince()
}

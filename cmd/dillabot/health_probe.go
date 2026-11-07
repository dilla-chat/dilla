//go:build linux

package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/health"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 17: the Bot's own health. Every 60 s it sends itself an OpProbe = 90 command through the real intake and
// reports every 15 s whether the probe was answered, with the queue's depth and the oldest command's wait.
//
// The probe is a command the Bot's own user "sends" from its own installation: it enters the same bounded queue as a
// member's command, waits for one of the same workers, and is opened, checked for Held and readiness, answered and
// replied to by the same intake code (internal/bot/intake). A wedged worker pool, a full queue, an intake that is not
// ready (no feed snapshot) or a user held by a failed device-list ingest all fail it. Only the cryptographic seal is
// local: the probe's bytes are a marker the entry recognises by a request id nobody else knows, and its answer goes to
// the waiting probe, not to dillad (dillad would 404 a reply to a request it never relayed). dillad's own check is the
// report: no report for 150 s and it marks the Bot offline.

// probeMarker prefixes the sealed bytes of a probe; the request id follows.
var probeMarker = []byte("dilla bot probe v1\x00")

// probeAnswerLimit bounds how long the probe waits for its answer (a variable for tests).
var probeAnswerLimit = health.ProbeTimeout

var errProbeQueueFull = errors.New("bot intake queue full")

// probeTable is the probes waiting for their answer, by request id. The zero value is ready.
type probeTable struct {
	mu      sync.Mutex
	pending map[id.ID]chan intake.Answer
}

func (p *probeTable) begin(request id.ID) (<-chan intake.Answer, func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = map[id.ID]chan intake.Answer{}
	}
	ch := make(chan intake.Answer, intake.MaxAnswers)
	p.pending[request] = ch
	return ch, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		delete(p.pending, request)
	}
}

func (p *probeTable) has(request id.ID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.pending[request]
	return ok
}

// probeBytes is the sealed field of a probe command.
func probeBytes(request id.ID) []byte {
	return append(append([]byte(nil), probeMarker...), request[:]...)
}

// probeRequest is the request id in sealed when it is a probe this table is waiting for.
func (p *probeTable) probeRequest(sealed []byte) (id.ID, bool) {
	var request id.ID
	if len(sealed) != len(probeMarker)+len(request) || string(sealed[:len(probeMarker)]) != string(probeMarker) {
		return request, false
	}
	copy(request[:], sealed[len(probeMarker):])
	return request, p.has(request)
}

// deliver hands the answer a probe's intake sealed (EncodeAnswer's bytes, unsealed) to its waiter.
func (p *probeTable) deliver(request id.ID, encoded []byte) {
	var fields []any
	if cborx.Unmarshal(encoded, &fields) != nil || len(fields) != 3 {
		return
	}
	status, _ := fields[0].(uint64)
	body, _ := fields[1].([]byte)
	final, _ := fields[2].(uint64)
	p.mu.Lock()
	ch := p.pending[request]
	p.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- intake.Answer{Status: uint16(min(status, 65535)),
		Body: body, Final: final == 1}:
	default:
	}
}

// openProbe is the opened command of a probe: what op 22 would answer for a verified OpProbe from the Bot's own user.
func openProbe(request, community, user, device id.ID, now time.Time) helper.OpenedCommand {
	return helper.OpenedCommand{Request: protocol.SealedRequest{
		RequestID: request, CommunityID: community, Operation: uint64(protocol.OpProbe),
		SentAt: uint64(max(now.Unix(), 0)), ReplyKey: [32]byte{1}, UserID: user, DeviceID: device,
	}}
}

// Probe sends one OpProbe through the installation's intake and returns nil when it was answered 200 within
// probeAnswerLimit. A queue with no room, an intake that refuses it, and no answer are all failures.
func (e *sealedEntry) Probe(ctx context.Context) error {
	cfg, _ := e.current()
	if cfg.helper == nil || cfg.botUser.IsZero() {
		return errSealedNotConfigured
	}
	request := id.New()
	answers, forget := e.probes.begin(request)
	defer forget()
	cmd := intake.RelayedCommand{RequestID: request, CommunityID: e.community, UserID: cfg.botUser, DeviceID: e.installation, Sealed: probeBytes(request)}
	select {
	case e.queue <- cmd:
	default:
		return errProbeQueueFull
	}
	timeout := e.probeClock().NewTimer(probeAnswerLimit)
	defer timeout.Stop()
	for {
		select {
		case a := <-answers:
			if !a.Final {
				continue
			}
			if a.Status != intake.StatusOK {
				return errors.New("bot intake refused the probe: " + string(a.Body))
			}
			return nil
		case <-timeout.C():
			return context.DeadlineExceeded
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// probeClock is the clock the probe's answer deadline runs on (the system's unless a test set one).
func (e *sealedEntry) probeClock() clock.Clock {
	if e.clk != nil {
		return e.clk
	}
	return clock.System()
}

// commandLoad tracks the commands relayed to an installation and not yet started by a worker. The zero value is ready.
type commandLoad struct {
	mu     sync.Mutex
	queued map[id.ID]time.Time
}

func (l *commandLoad) enqueue(request id.ID, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.queued == nil {
		l.queued = map[id.ID]time.Time{}
	}
	l.queued[request] = at
}

// started drops a command a worker took.
func (l *commandLoad) started(request id.ID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.queued, request)
}

// snapshot is the commands still waiting for a worker and how long the oldest has waited.
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

// startHealth (called with e.mu held) starts the installation's health monitor, once: it probes the intake every 60 s and reports every 15 s
// until ctx ends. musicP95 reads the installation's music start timings (zero without any).
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
		report := client.HealthReport{ProbeOK: r.ProbeOK, ProbeAt: r.ProbeAt, QueueDepth: r.QueueDepth, OldestWaitMS: r.OldestWaitMS, MusicStartP95: r.MusicStartP95, FailingSince: r.FailingSince}
		return session.ReportHealth(reportCtx, report)
	})
	monitor := health.New(health.ProberFunc(e.Probe), reporter, clk)
	monitor.SetLoad(func() health.Load {
		depth, oldest := e.load.snapshot(clk.Now())
		var p95 time.Duration
		if musicP95 != nil {
			p95 = musicP95()
		}
		return health.Load{QueueDepth: depth, OldestWait: oldest, MusicStartP95: p95}
	})
	// The caller (SetSealedIntake) holds e.mu.
	e.monitor = monitor
	go monitor.Run(ctx)
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

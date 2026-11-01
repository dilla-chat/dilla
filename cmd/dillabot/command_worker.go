//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/id"
)

const commandWorkerCap = 4
const commandRetirementGrace = 30 * time.Second

type commandTurn struct {
	work     interactionWork
	stop     atomic.Bool
	retired  atomic.Bool
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	finished bool
	grace    *time.Timer
}

func (t *commandTurn) requestStop() { t.stop.Store(true) }

// Retirement alone may cancel transport work, after bounded time for an active
// helper transaction/HTTP exchange to finish. Ordinary preemption never does.
func (t *commandTurn) retire() {
	t.retired.Store(true)
	t.requestStop()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.finished && t.grace == nil {
		t.grace = time.AfterFunc(commandRetirementGrace, t.cancel)
	}
}
func (t *commandTurn) finish() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.finished = true
	if t.grace != nil {
		t.grace.Stop()
	}
	t.cancel()
	close(t.done)
}

type commandJob struct {
	work       interactionWork
	ctx        context.Context
	cancel     context.CancelFunc // finishes a completed turn; never used for preemption
	poll       func(context.Context, id.ID) error
	background bool
	version    uint64
	turn       *commandTurn
}
type commandLogState struct {
	next    time.Time
	attempt uint8
}
type commandFailureState struct {
	attempt uint8
	logs    map[string]commandLogState
}
type commandWorkers struct {
	ctx          context.Context
	cancel       context.CancelFunc
	jobs         chan commandJob
	finished     chan struct{}
	wg           sync.WaitGroup
	active       map[id.ID]context.CancelFunc // pendingMu; cooperative stop functions
	activeJobs   map[id.ID]*commandTurn
	background   map[id.ID]bool
	retry        map[interactionWork]time.Time
	failures     map[interactionWork]*commandFailureState
	blockedLease map[interactionWork]uint64
}

// StartCommandWorkers runs ONE turn at a time per installation, including its
// different groups: the durable event ACK cursor is shared and the native helper
// is serial. Independent installations share a global cap of four turns. A
// gateway hint stops background catch-up cooperatively, never by cancelling IPC.
func (r *botGatewayRuntime) StartCommandWorkers() {
	r.pendingMu.Lock()
	if r.commands != nil || r.commandsClosed {
		r.pendingMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	pool := &commandWorkers{ctx: ctx, cancel: cancel, jobs: make(chan commandJob, commandWorkerCap), finished: make(chan struct{}, commandWorkerCap), active: make(map[id.ID]context.CancelFunc), background: make(map[id.ID]bool), retry: make(map[interactionWork]time.Time)}
	r.commands = pool
	pool.wg.Add(commandWorkerCap + 1)
	r.pendingMu.Unlock()
	for range commandWorkerCap {
		go r.commandWorker(pool)
	}
	go r.commandDriver(pool)
	r.signalCommands()
}
func (r *botGatewayRuntime) stopCommandWorkers() {
	r.pendingMu.Lock()
	r.commandsClosed = true
	pool := r.commands
	var turns []*commandTurn
	if pool != nil {
		// Stop reservations before snapshotting: every call that could already be
		// in transport must receive its own bounded retirement grace.
		pool.cancel()
		for _, turn := range pool.activeJobs {
			turns = append(turns, turn)
		}
	}
	r.pendingMu.Unlock()
	if pool == nil {
		return
	}
	for _, turn := range turns {
		turn.retire()
	}
	pool.wg.Wait()
}
func (r *botGatewayRuntime) commandDriver(pool *commandWorkers) {
	defer pool.wg.Done()
	defer close(pool.jobs)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		if pool.ctx.Err() != nil {
			return
		}
		jobs, delay, pending := r.commandBatch(pool, time.Now())
		// Reservations include queued jobs; at most four exist, so this buffer has
		// room for the entire batch even if shutdown starts between reservations.
		for _, job := range jobs {
			pool.jobs <- job
		}
		var wake <-chan time.Time
		if pending {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(max(delay, time.Millisecond))
			wake = timer.C
		}
		select {
		case <-pool.ctx.Done():
			return
		case <-r.interaction:
		case <-pool.finished:
		case <-wake:
		}
	}
}
func (pool *commandWorkers) initState() {
	if pool.activeJobs == nil {
		pool.activeJobs = make(map[id.ID]*commandTurn)
	}
	if pool.failures == nil {
		pool.failures = make(map[interactionWork]*commandFailureState)
	}
	if pool.blockedLease == nil {
		pool.blockedLease = make(map[interactionWork]uint64)
	}
}
func (r *botGatewayRuntime) commandBatch(pool *commandWorkers, now time.Time) ([]commandJob, time.Duration, bool) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	pool.initState()
	var jobs []commandJob
	var next time.Duration
	pending := false
	remaining := r.pendingOrder[:0]
	for _, work := range r.pendingOrder {
		if !r.pending[work.installation][work.group] {
			continue
		}
		poll := r.interactionPollers[work.installation]
		if poll == nil || pool.active[work.installation] != nil || len(pool.active) >= commandWorkerCap {
			remaining = append(remaining, work)
			continue
		}
		if version, blocked := pool.blockedLease[work]; blocked && version == r.pollerVersions[work.installation] {
			remaining = append(remaining, work)
			continue
		}
		delay := max(r.interactionDelayLocked(work.installation, work.group, now), pool.retry[work].Sub(now))
		if delay > 0 {
			remaining = append(remaining, work)
			if !pending || delay < next {
				next = delay
			}
			pending = true
			continue
		}
		background := r.backgroundPending[work]
		// Parent cancellation requests a safe stop. It must not close a helper socket
		// while GroupApply is executing; retirement supplies a separate grace timer.
		ctx := client.WithBackground(context.WithoutCancel(pool.ctx))
		if !background {
			ctx = client.WithInteractive(ctx)
		}
		ctx, cancel := context.WithCancel(ctx)
		turn := &commandTurn{work: work, cancel: cancel, done: make(chan struct{})}
		ctx = client.WithCooperativeStop(ctx, func() bool { return turn.stop.Load() || pool.ctx.Err() != nil })
		pool.active[work.installation] = turn.requestStop
		pool.activeJobs[work.installation] = turn
		pool.background[work.installation] = background
		r.budgetLocked(work.installation, work.group).spend(now, 1)
		delete(r.pending[work.installation], work.group)
		if len(r.pending[work.installation]) == 0 {
			delete(r.pending, work.installation)
		}
		delete(pool.retry, work)
		delete(pool.blockedLease, work)
		delete(r.backgroundPending, work)
		jobs = append(jobs, commandJob{work: work, ctx: ctx, cancel: turn.finish, poll: poll, background: background, version: r.pollerVersions[work.installation], turn: turn})
	}
	r.pendingOrder = remaining
	for _, job := range jobs {
		r.lastInteractionPoll[job.work.installation] = now
	}
	return jobs, next, pending
}

type commandPanicError struct {
	value string
	stack []byte
}

func (e *commandPanicError) Error() string { return "command handler panic: " + e.value }
func runCommandJob(job commandJob) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = &commandPanicError{value: fmt.Sprint(p), stack: debug.Stack()}
		}
	}()
	if err = client.CooperativeCheckpoint(job.ctx); err != nil {
		return err
	}
	return job.poll(job.ctx, job.work.group)
}
func commandRetryDelay(attempt uint8) time.Duration {
	if attempt > 6 {
		return 30 * time.Second
	}
	return min(interactionRefill*time.Duration(1<<attempt), 30*time.Second)
}
func (r *botGatewayRuntime) commandWorker(pool *commandWorkers) {
	defer pool.wg.Done()
	for job := range pool.jobs {
		err := runCommandJob(job)
		job.cancel() // all helper calls and HTTP exchanges have returned
		r.finishCommandJob(pool, job, err, time.Now())
		select {
		case pool.finished <- struct{}{}:
		default:
		}
	}
}
func (r *botGatewayRuntime) finishCommandJob(pool *commandWorkers, job commandJob, err error, now time.Time) {
	r.pendingMu.Lock()
	pool.initState()
	delete(pool.active, job.work.installation)
	delete(pool.activeJobs, job.work.installation)
	delete(pool.background, job.work.installation)
	retry := err != nil && pool.ctx.Err() == nil && r.interactionPollers[job.work.installation] != nil && (job.turn == nil || !job.turn.retired.Load())
	warn := false
	rateInfo := false
	var delay time.Duration
	if err == nil {
		delete(pool.failures, job.work)
		delete(pool.blockedLease, job.work)
	}
	if retry {
		switch {
		case errors.Is(err, client.ErrCooperativeStop), errors.Is(err, errCommandWorkRemaining):
			// Successful bounded progress and requested hand-offs are not failures.
		case errors.Is(err, errNativeLeaseUnavailable):
			pool.blockedLease[job.work] = job.version
			warn = true
		default:
			if refusal := client.RateLimitError(err); refusal != nil {
				delay = time.Second
				if refusal.RetryAfterMS != nil {
					delay = time.Duration(min(*refusal.RetryAfterMS, uint64(30000))) * time.Millisecond
				}
				delay = min(max(delay, interactionSpacing), 30*time.Second)
				if r.deferred == nil {
					r.deferred = make(map[id.ID]time.Time)
				}
				r.deferred[job.work.installation] = now.Add(delay)
				rateInfo = true
			} else {
				state := pool.failures[job.work]
				if state == nil {
					state = &commandFailureState{logs: make(map[string]commandLogState)}
					pool.failures[job.work] = state
				}
				delay = commandRetryDelay(state.attempt)
				if state.attempt < 7 {
					state.attempt++
				}
				key := fmt.Sprintf("%T:%s", err, err.Error())
				logged := state.logs[key]
				if !now.Before(logged.next) {
					warn = true
					logged.next = now.Add(commandRetryDelay(logged.attempt))
					if logged.attempt < 7 {
						logged.attempt++
					}
					if len(state.logs) >= 32 {
						clear(state.logs)
					}
					state.logs[key] = logged
				}
			}
		}
		pool.retry[job.work] = now.Add(delay)
		if job.background {
			r.queueBackgroundInteractionLocked(job.work.installation, job.work.group)
		} else {
			r.queueInteractionLocked(job.work.installation, job.work.group)
		}
	}
	// Retirement suppresses retries, not the diagnostic for a recovered panic.
	var panicErr *commandPanicError
	isPanic := errors.As(err, &panicErr)
	if isPanic && !retry {
		warn = true
	}
	r.pendingMu.Unlock()
	if warn && r.log != nil {
		attrs := []any{"installation_id", job.work.installation.String(), "group_id", job.work.group.String(), "error", err.Error(), "retry_after", delay}
		if isPanic {
			attrs = append(attrs, "stack", string(panicErr.stack))
		}
		r.log.WarnContext(r.ctx, "bot interaction command intake failed", attrs...)
	}
	if rateInfo && r.log != nil {
		r.log.InfoContext(r.ctx, "bot interaction command intake rate limited", "installation_id", job.work.installation.String(), "group_id", job.work.group.String(), "retry_after", delay)
	}
	if retry {
		r.signalCommands()
	}
}

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/bot/catalog"
	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/health"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
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
	failed := false
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
				failed = true
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
		var trustErr *service.TrustPhaseError
		if errors.As(err, &trustErr) {
			attrs = append(attrs, "phase", trustErr.Phase.String(), "cause_class", trustErr.CauseClass())
		}
		var membershipErr *service.MembershipPhaseError
		if errors.As(err, &membershipErr) {
			attrs = append(attrs, "membership_phase", membershipErr.Phase.String())
			attrs = append(attrs, safeMembershipErrorAttrs(membershipErr)...)
		}
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
	if failed {
		// A failed intake turn (a source the pass has not synced or healed, a membership the pass has not
		// reconciled) asks for one reconciliation pass, as the periodic pass used to provide (Bot v2 task 14): the
		// worker's own retry backoff spaces the asks, and a pass is never requested while intake succeeds.
		r.requestPass(job.work.installation)
	}
}

// ---- Bot v2 Task 15: sealed command intake ----
//
// Each installation has one sealed intake that outlives passes and gateway connections:
//
//   - its Authz is fed by the installation's feed (sealedFeed tees member, role, community and snapshot events
//     to it), so a command is authorised from pushed state only;
//   - op 64 is decoded on the delivery loop (relayCommand) and queued; sealedWorkers goroutines run
//     intake.Handle, so one slow command ("Searching…") never holds the others, and nothing waits on a clock;
//   - a pass hands it the current helper, session, capabilities and handlers (SetSealedIntake); the intake's
//     answer cache (5 min, for a Retry) and the Authz survive the swap;
//   - its catalog.Publisher publishes the catalog and command key on start, on a capability change, every hour
//     (the helper rotates the key; a request is sent only when the bytes changed) and when a command named a key
//     the helper no longer holds.
//
// A command costs one helper op 22, one op 23 per answer, and one POST /v1/bot-runtime/replies per answer: no
// device-list read, no member read (TestCommandCostIsFlatAt200Members600Devices).

const (
	// sealedWorkers is each installation's concurrent commands; the helper serialises its own ops.
	sealedWorkers = 4
	// sealedQueue bounds the commands waiting for a worker. A command over it is dropped (logged): the client
	// times out after 5 s and its Retry is relayed again.
	sealedQueue = 256
)

// sealedHelper is the installation's native helper: ops 21, 22 and 23.
type sealedHelper interface {
	intake.Helper
	catalog.KeySource
}

// sealedSession is the installation's Bot session: replies, the catalog and the command key.
type sealedSession interface {
	intake.ReplySender
	catalog.Session
}

// sealedIntakeConfig is what a pass hands an installation's sealed intake.
type sealedIntakeConfig struct {
	installation, community, botUser id.ID
	capabilities                     uint64
	helper                           sealedHelper
	session                          sealedSession
	// handlers is the pass's feature handler map (the same one the interaction dispatcher serves).
	handlers map[protocol.Operation]service.InteractionHandlerSpec
	// reminders, when set, is served from the feed: membership from Authz, no capability read per create
	// (the catalog only offers reminder operations while the capability is on).
	reminders *service.ReminderInteractions
	held      intake.HeldChecker
	log       *slog.Logger
	// musicStartP95 reads the installation's music start p95 for the health report (Bot v2 Task 17); nil is zero.
	musicStartP95 func() time.Duration
	// healthDir is where the health monitor leaves its last report for `dillabot doctor` (health_state.go); empty
	// writes none.
	healthDir string
}

// sealedEntry is one installation's sealed intake. It is the intake's helper, dispatcher and reply sender and the
// publisher's key source and session, each forwarding to the pass's current dependencies.
type sealedEntry struct {
	installation, community id.ID
	authz                   *intake.Authz
	queue                   chan intake.RelayedCommand

	mu         sync.Mutex
	cfg        sealedIntakeConfig
	dispatcher *service.SealedDispatcher
	operations map[uint64]catalog.Rule
	in         *intake.Intake
	publisher  *catalog.Publisher
	cancel     context.CancelFunc
	started    bool

	// Bot v2 Task 17 (health_probe.go): the Bot's probes of its own intake, the commands waiting for a worker, and the
	// health monitor that reports both.
	probes  probeTable
	load    commandLoad
	monitor *health.Monitor
	clk     clock.Clock // the probe's clock; nil is the system's
}

func (e *sealedEntry) current() (sealedIntakeConfig, *service.SealedDispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg, e.dispatcher
}

var errSealedNotConfigured = errors.New("bot sealed intake: no pass has configured the installation")

func (e *sealedEntry) OpenCommand(ctx context.Context, sealed []byte, user, device id.ID, now time.Time) (helper.OpenedCommand, error) {
	if request, ok := e.probes.probeRequest(sealed); ok {
		return openProbe(request, e.community, user, device, now), nil
	}
	cfg, _ := e.current()
	if cfg.helper == nil {
		return helper.OpenedCommand{}, errSealedNotConfigured
	}
	return cfg.helper.OpenCommand(ctx, sealed, user, device, now)
}

func (e *sealedEntry) SealReply(ctx context.Context, key [32]byte, request id.ID, seq uint32, body []byte) ([]byte, error) {
	if e.probes.has(request) {
		return append([]byte{}, body...), nil // a probe's answer stays in the Bot
	}
	cfg, _ := e.current()
	if cfg.helper == nil {
		return nil, errSealedNotConfigured
	}
	return cfg.helper.SealReply(ctx, key, request, seq, body)
}

func (e *sealedEntry) CommandKey(ctx context.Context, community id.ID, now time.Time) ([]byte, error) {
	cfg, _ := e.current()
	if cfg.helper == nil {
		return nil, errSealedNotConfigured
	}
	return cfg.helper.CommandKey(ctx, community, now)
}

func (e *sealedEntry) Reply(ctx context.Context, device, request id.ID, seq uint32, sealed []byte, final bool) error {
	if e.probes.has(request) {
		e.probes.deliver(request, sealed)
		return nil
	}
	cfg, _ := e.current()
	if cfg.session == nil {
		return errSealedNotConfigured
	}
	return cfg.session.Reply(ctx, device, request, seq, sealed, final)
}

func (e *sealedEntry) PublishCatalog(ctx context.Context, community id.ID, commands []byte) (uint64, error) {
	cfg, _ := e.current()
	if cfg.session == nil {
		return 0, errSealedNotConfigured
	}
	return cfg.session.PublishCatalog(ctx, community, commands)
}

func (e *sealedEntry) PublishCommandKey(ctx context.Context, community id.ID, publication []byte) error {
	cfg, _ := e.current()
	if cfg.session == nil {
		return errSealedNotConfigured
	}
	return cfg.session.PublishCommandKey(ctx, community, publication)
}

func (e *sealedEntry) Dispatch(ctx context.Context, req protocol.SealedRequest, reply func(intake.Answer)) {
	_, d := e.current()
	d.Dispatch(ctx, req, reply)
}

func (e *sealedEntry) Held(user id.ID) bool {
	cfg, _ := e.current()
	return cfg.held != nil && cfg.held.Held(user)
}

func (e *sealedEntry) features() []catalog.Feature {
	cfg, _ := e.current()
	return catalog.FeaturesOf(cfg.capabilities)
}

func (e *sealedEntry) operation(op uint64) (intake.OperationRule, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rule, ok := e.operations[op]
	return intake.OperationRule{Permission: rule.Permission, Read: rule.Read, ChannelScoped: rule.ChannelScoped, ChannelArgs: rule.ChannelArgs}, ok
}

// sealedRegistry is the runtime's sealed intakes by installation. Its zero value is ready.
type sealedRegistry struct {
	mu      sync.Mutex
	entries map[id.ID]*sealedEntry
}

// entry returns the installation's entry, creating it (and its Authz) on first use; a community change replaces it.
func (s *sealedRegistry) entry(installation, community id.ID) *sealedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[id.ID]*sealedEntry{}
	}
	e := s.entries[installation]
	if e == nil || e.community != community {
		if e != nil {
			e.stop()
		}
		e = &sealedEntry{installation: installation, community: community, authz: intake.NewAuthz(community), queue: make(chan intake.RelayedCommand, sealedQueue)}
		s.entries[installation] = e
	}
	return e
}

func (s *sealedRegistry) lookup(installation id.ID) *sealedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[installation]
}

// retain stops and forgets every installation not in keep.
func (s *sealedRegistry) retain(keep map[id.ID]bool) {
	s.mu.Lock()
	var stopped []*sealedEntry
	for installation, e := range s.entries {
		if !keep[installation] {
			stopped = append(stopped, e)
			delete(s.entries, installation)
		}
	}
	s.mu.Unlock()
	for _, e := range stopped {
		e.stop()
	}
}

func (e *sealedEntry) stop() {
	e.mu.Lock()
	cancel := e.cancel
	e.cancel = nil
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// feed tees the installation's feed handler to its Authz: what it returns is what the feed consumer applies.
func (s *sealedRegistry) feed(installation, community id.ID, inner *installationFeed) Feed {
	if inner == nil {
		return nil
	}
	return sealedFeed{Feed: inner, authz: s.entry(installation, community).authz}
}

// sealedFeed applies each event to the feed handler and to the intake's Authz, in feed order.
type sealedFeed struct {
	Feed
	authz *intake.Authz
}

func (f sealedFeed) SnapshotBegin(ctx context.Context) {
	f.authz.SnapshotBegin()
	f.Feed.SnapshotBegin(ctx)
}

func (f sealedFeed) Member(ctx context.Context, ev MemberEvent) {
	f.authz.Member(ev.User, ev.Joined, ev.Roles, ev.Snapshot)
	f.Feed.Member(ctx, ev)
}

func (f sealedFeed) Role(ctx context.Context, ev RoleEvent) {
	f.authz.Role(ev.Role, ev.Allow, ev.Deny, ev.Position, ev.Deleted, ev.FromSnapshot)
	f.Feed.Role(ctx, ev)
}

func (f sealedFeed) Community(ctx context.Context, community, owner id.ID, snapshot bool) {
	f.authz.Owner(owner, snapshot)
	f.Feed.Community(ctx, community, owner, snapshot)
}

// ChannelOverwrites and MemberTimeout are kinds 9 and 10 (feed_authz.go): only the Authz keeps them.
func (f sealedFeed) ChannelOverwrites(_ context.Context, _, channel id.ID, overwrites []intake.Overwrite, deleted, snapshot bool) {
	f.authz.ChannelOverwrites(channel, overwrites, deleted, snapshot)
}

func (f sealedFeed) MemberTimeout(_ context.Context, _, user id.ID, until uint64, snapshot bool) {
	f.authz.MemberTimeout(user, until, snapshot)
}

func (f sealedFeed) SnapshotDone(ctx context.Context) {
	f.authz.SnapshotDone()
	f.Feed.SnapshotDone(ctx)
}

// sealedReminderAuthority answers the reminder handlers' membership question from the feed.
type sealedReminderAuthority struct{ authz *intake.Authz }

func (a sealedReminderAuthority) ReminderMember(_ context.Context, user id.ID) (bool, error) {
	return a.authz.IsMember(user), nil
}

// sealedHandlers is the pass's handler map with the reminders served from the feed.
func sealedHandlers(cfg sealedIntakeConfig, authz *intake.Authz) map[protocol.Operation]service.InteractionHandlerSpec {
	out := make(map[protocol.Operation]service.InteractionHandlerSpec, len(cfg.handlers)+5)
	for op, h := range cfg.handlers {
		out[op] = h
	}
	if cfg.reminders != nil {
		rem := *cfg.reminders
		rem.Authority = sealedReminderAuthority{authz}
		rem.CreateEnabled = nil
		for op, h := range rem.Handlers() {
			out[op] = h
		}
	}
	return out
}

// SetSealedIntake configures the installation's sealed intake from a pass and starts it on first use: its
// workers and its catalog publisher. A capability change republishes the catalog at once.
func (r *botGatewayRuntime) SetSealedIntake(cfg sealedIntakeConfig) {
	if cfg.installation.IsZero() || cfg.community.IsZero() || cfg.helper == nil || cfg.session == nil {
		return
	}
	if cfg.log == nil {
		cfg.log = r.log.With("installation_id", cfg.installation.String())
	}
	e := r.sealed.entry(cfg.installation, cfg.community)
	features := catalog.FeaturesOf(cfg.capabilities)
	e.mu.Lock()
	changed := e.started && e.cfg.capabilities != cfg.capabilities
	// A new Bot session (fix round 1 M-2): dillad may hold an older publication, or none after a restore, so the key
	// and the catalog are sent again whatever the publisher believes it sent.
	renewed := e.started && e.cfg.session != cfg.session
	e.cfg = cfg
	e.dispatcher = &service.SealedDispatcher{Community: cfg.community, Handlers: sealedHandlers(cfg, e.authz)}
	e.operations = catalog.Operations(features)
	start := !e.started
	if start {
		e.started = true
		e.in = intake.New(e, e.authz, e, e, clock.System())
		e.in.SetHeld(e)
		e.in.SetOperations(e.operation)
		e.in.SetLogger(cfg.log)
		e.publisher = catalog.NewPublisher(e, e, clock.System(), e.features)
		e.publisher.SetLogger(cfg.log)
		// A command sealed to a key the helper does not hold: send the current key and catalog again even when their
		// bytes are unchanged, so the client's refetch finds them (fix round 1 M-2).
		publisher := e.publisher
		e.in.SetKeyStale(func() { publisher.Forget(); publisher.Changed() })
		// Fail closed until the installation's feed exists (Held is false for an installation with no feed) and has
		// applied a snapshot (the Authz's own readiness): 503 E_BOT_OFFLINE, retryable.
		installation := cfg.installation
		e.in.SetReady(func() bool { return r.feedHandler(installation) != nil })
		ctx, cancel := context.WithCancel(r.ctx)
		e.cancel = cancel
		for range sealedWorkers {
			go e.work(ctx)
		}
		go e.publisher.Run(ctx, cfg.community)
		e.startHealth(ctx, func() time.Duration {
			current, _ := e.current()
			if current.musicStartP95 == nil {
				return 0
			}
			return current.musicStartP95()
		}, clock.System())
	}
	in, publisher := e.in, e.publisher
	e.mu.Unlock()
	in.SetBotUser(cfg.botUser)
	if renewed {
		publisher.Forget()
	}
	if changed || renewed {
		publisher.Changed()
	}
}

func (e *sealedEntry) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-e.queue:
			e.load.started(cmd.RequestID)
			e.mu.Lock()
			in := e.in
			e.mu.Unlock()
			in.Handle(ctx, cmd)
		}
	}
}

// decodeRelayedCommand reads op 64, [request_id, community_id, user_id, device_id, sealed] (gateway.BotCommandPayload).
func decodeRelayedCommand(payload []cbor.RawMessage) (intake.RelayedCommand, error) {
	var cmd intake.RelayedCommand
	if len(payload) != 5 {
		return cmd, errFeedShape
	}
	for i, dst := range []*id.ID{&cmd.RequestID, &cmd.CommunityID, &cmd.UserID, &cmd.DeviceID} {
		v, err := feedField[id.ID](payload[i])
		if err != nil || v.IsZero() {
			return intake.RelayedCommand{}, errFeedShape
		}
		*dst = v
	}
	sealed, err := feedField[[]byte](payload[4])
	if err != nil || len(sealed) == 0 {
		return intake.RelayedCommand{}, errFeedShape
	}
	cmd.Sealed = sealed
	return cmd, nil
}

// relayCommand queues one op 64 for the installation's sealed intake. It never blocks the delivery loop.
func (r *botGatewayRuntime) relayCommand(installation id.ID, payload []cbor.RawMessage, ready func(context.Context) bool) {
	cmd, err := decodeRelayedCommand(payload)
	// The feed barrier (fix round 1 I-3): the command is opened and authorised only after the events dillad sent
	// before it are applied.
	cmd.Ready = ready
	e := r.sealed.lookup(installation)
	switch {
	case err != nil:
		r.log.Warn("bot command relay malformed", "installation_id", installation.String())
	case e == nil || cmd.CommunityID != e.community:
		r.log.Warn("bot command relay for no sealed intake", "installation_id", installation.String())
	default:
		e.load.enqueue(cmd.RequestID, time.Now())
		select {
		case e.queue <- cmd:
		default:
			e.load.started(cmd.RequestID)
			r.log.Warn("bot command intake queue full; command dropped", "installation_id", installation.String())
		}
	}
}

// installationHeld is the runtime's held-user query for one installation (Task 14's botGatewayRuntime.Held).
type installationHeld struct {
	r            *botGatewayRuntime
	installation id.ID
}

func (h installationHeld) Held(user id.ID) bool { return h.r.Held(h.installation, user) }

// newSealedIntakeConfig is a pass's sealed intake configuration: the pass's feature handlers with the utilities'
// polls and reminders (as the interaction dispatcher serves them, without its capability discovery).
func newSealedIntakeConfig(r *botGatewayRuntime, installation, community, botUser id.ID, capabilities uint64, h sealedHelper, session sealedSession,
	handlers map[protocol.Operation]service.InteractionHandlerSpec, utilities *utilityRuntime) sealedIntakeConfig {
	cfg := sealedIntakeConfig{installation: installation, community: community, botUser: botUser, capabilities: capabilities,
		helper: h, session: session, handlers: assembleUtilityInteractionHandlers(handlers, utilities, nil, community, installation, botUser, 0),
		held: installationHeld{r, installation}}
	if utilities != nil {
		reminders := utilities.reminders
		cfg.reminders = &reminders
	}
	return cfg
}

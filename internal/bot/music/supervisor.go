package music

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/id"
)

var ErrSupervisorClosed = errors.New("music supervisor is closed")

// Playback is the owned lifetime of one provider/encoder/LiveKit track.
type Playback interface {
	Done() <-chan struct{}
	Wait() error
	Close() error
}

// TrackStarter composes provider PCM, the bounded Opus encoder, call credentials,
// MLS epoch source and encrypted LiveKit publisher for one track.
type TrackStarter interface {
	Start(context.Context, Track) (Playback, error)
}

type QueueStore interface {
	Load(context.Context, id.ID) (State, uint64, error)
	Save(context.Context, id.ID, uint64, State) (uint64, error)
}

// Supervisor owns the persisted queue and at most one active track for a voice
// channel. Loading restores metadata only; audio begins only on OpResume.
type Supervisor struct {
	mu                         sync.Mutex
	ctx                        context.Context
	channel                    id.ID
	store                      QueueStore
	starter                    TrackStarter
	state                      State
	revision                   uint64
	playback                   Playback
	closed                     bool
	changed                    chan struct{}
	emptyTimer                 Timer
	grace                      time.Duration
	afterFunc                  func(time.Duration, func()) Timer
	onEmpty                    func()
	stopLifetime               func() bool
	onAutomaticPlaybackFailure func(context.Context, error)
	onSkipped                  func(context.Context, []SkippedTrack)
	starting                   *startRun
	startRequest               uint64
	logger                     *slog.Logger
	// Task 10c: leave the voice room after nothing has played for idleFor() (read when the
	// supervisor goes idle, so a changed setting applies to the next idle period).
	idleFor   func() time.Duration
	idleTimer Timer
	idleGen   uint64
}

// IdleLeaver is a TrackStarter that can leave its retained voice room while nothing plays.
type IdleLeaver interface{ LeaveIdle() error }

// WithIdleLeave makes the supervisor leave the voice room once nothing has played for idleFor()
// (paused, stopped or the queue finished). Playing again cancels the timer; the next explicit
// play reconnects. A non-positive duration disables it.
func WithIdleLeave(idleFor func() time.Duration, afterFunc func(time.Duration, func()) Timer) SupervisorOption {
	return func(s *Supervisor) {
		s.idleFor = idleFor
		if afterFunc != nil {
			s.afterFunc = afterFunc
		}
	}
}

// idleCheckLocked starts the idle-leave timer when nothing plays and stops it when something
// does. It runs after every state change.
func (s *Supervisor) idleCheckLocked() {
	if s.idleFor == nil {
		return
	}
	if s.playback != nil || s.starting != nil || s.closed {
		if s.idleTimer != nil {
			s.idleTimer.Stop()
			s.idleTimer = nil
		}
		return
	}
	if s.idleTimer != nil {
		return
	}
	d := s.idleFor()
	if d <= 0 {
		return
	}
	s.idleGen++
	gen := s.idleGen
	s.idleTimer = s.afterFunc(d, func() { s.leaveIdle(gen) })
}

func (s *Supervisor) leaveIdle(gen uint64) {
	s.mu.Lock()
	if s.closed || s.idleTimer == nil || s.idleGen != gen || s.playback != nil || s.starting != nil {
		s.mu.Unlock()
		return
	}
	s.idleTimer = nil
	leaver, ok := s.starter.(IdleLeaver)
	s.mu.Unlock()
	if ok {
		_ = leaver.LeaveIdle()
	}
}

type Timer interface{ Stop() bool }

type SupervisorOption func(*Supervisor)

// WithAutomaticPlaybackFailure reports startup failures during EOF advancement.
// Explicit starts return their errors to the interaction handler for logging.
func WithAutomaticPlaybackFailure(report func(context.Context, error)) SupervisorOption {
	return func(s *Supervisor) { s.onAutomaticPlaybackFailure = report }
}

// WithEmptyRoomGrace schedules onEmpty after grace. onEmpty is only a callback;
// the caller must perform the actual call leave operation.
func WithEmptyRoomGrace(grace time.Duration, afterFunc func(time.Duration, func()) Timer, onEmpty func()) SupervisorOption {
	return func(s *Supervisor) {
		if grace > 0 {
			s.grace = grace
		}
		if afterFunc != nil {
			s.afterFunc = afterFunc
		}
		s.onEmpty = onEmpty
	}
}

func NewSupervisor(ctx context.Context, channel id.ID, store QueueStore, starter TrackStarter, options ...SupervisorOption) (*Supervisor, error) {
	if ctx == nil || channel.IsZero() || store == nil || starter == nil {
		return nil, errors.New("invalid music supervisor configuration")
	}
	ctx = client.WithoutCooperativeStop(client.WithBackground(ctx))
	state, revision, err := store.Load(ctx, channel)
	if err != nil {
		return nil, err
	}
	s := &Supervisor{ctx: ctx, channel: channel, store: store, starter: starter, state: state, revision: revision, changed: make(chan struct{}), grace: 5 * time.Minute, afterFunc: func(d time.Duration, fn func()) Timer { return time.AfterFunc(d, fn) }}
	for _, option := range options {
		if option != nil {
			option(s)
		}
	}
	// Retained call resources belong to this supervisor even while playback is paused.
	s.mu.Lock()
	s.stopLifetime = context.AfterFunc(ctx, func() { _ = s.Close() })
	s.mu.Unlock()
	return s, nil
}

func (s *Supervisor) State() State { s.mu.Lock(); defer s.mu.Unlock(); return cloneState(s.state) }

// Apply persists a revision-checked operation before starting or stopping
// resources. Resume is the only operation that can start restored state.
func (s *Supervisor) Apply(ctx context.Context, expected uint64, op Operation) (State, error) {
	if ctx == nil {
		return State{}, errors.New("nil music operation context")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked(ctx, expected, op, false)
}

// ApplyImmediate serializes a pause or skip with automatic queue advancement.
// The caller has already authorized this action against the current call.
func (s *Supervisor) ApplyImmediate(ctx context.Context, op Operation) (State, error) {
	if ctx == nil || (op.Kind != OpPause && op.Kind != OpSkip) {
		return State{}, ErrInvalidOperation
	}
	if _, _, err := s.RefreshState(ctx); err != nil {
		return State{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked(ctx, s.revision, op, true)
}

func (s *Supervisor) applyLocked(ctx context.Context, expected uint64, op Operation, async bool) (State, error) {
	var skipped []SkippedTrack
	defer func() {
		if len(skipped) > 0 {
			s.mu.Unlock()
			s.reportSkipped(skipped)
			s.mu.Lock()
		}
	}()
	if s.closed {
		return State{}, ErrSupervisorClosed
	}
	if expected != s.state.Revision {
		return cloneState(s.state), ErrRevisionConflict
	}
	if op.Kind == OpSkip && op.ExpectedCurrentItemID != nil {
		currentID := ""
		if s.state.Current != nil {
			currentID = s.state.Current.ID
		}
		if currentID != *op.ExpectedCurrentItemID {
			return cloneState(s.state), nil
		}
	}
	if op.NowMS == 0 {
		op.NowMS = time.Now().UnixMilli()
	}
	next, err := Apply(s.state, expected, op)
	if err != nil {
		return cloneState(s.state), err
	}
	revision, err := s.store.Save(ctx, s.channel, s.revision, next)
	if err != nil {
		return cloneState(s.state), err
	}
	s.state, s.revision = next, revision
	if op.Kind == OpResume {
		s.startRequest++
	}
	if op.Kind == OpPause || op.Kind == OpStop || op.Kind == OpClear || op.Kind == OpSkip {
		s.closePlaybackLocked()
	}
	if !s.state.Paused && s.state.Current != nil && s.playback == nil && async {
		s.scheduleStartLocked()
	} else if !s.state.Paused && s.state.Current != nil && s.playback == nil {
		var startErr error
		skipped, startErr = s.startLocked(ctx, op.Kind == OpResume || op.Kind == OpSkip, nil)
		if err := startErr; err != nil {
			_ = s.pauseLocked(ctx)
			s.signalLocked()
			return cloneState(s.state), err
		}
	}
	s.idleCheckLocked()
	s.signalLocked()
	return cloneState(s.state), nil
}

// ApplyPersistedSnapshot adopts a revision committed by another authorized
// component. It never writes that mutation again. startPlayback must be true
// only for a live, explicit Resume or a Skip from an already-playing queue;
// restored or metadata-only snapshots cannot start audio. A failed start is
// skipped for a provider failure, or durably paused at a new CAS revision so the persisted queue cannot claim
// playback is active. Because the external mutation is already committed, the
// request context does not own runtime adoption; bounded reads and playback
// use the supervisor's lifetime context instead.
func (s *Supervisor) ApplyPersistedSnapshot(ctx context.Context, committed State, expected uint64, startPlayback bool) (uint64, error) {
	if ctx == nil {
		return 0, errors.New("nil persisted music snapshot context")
	}
	loadCtx, cancelLoad := context.WithTimeout(s.ctx, 5*time.Second)
	stored, revision, err := s.store.Load(loadCtx, s.channel)
	cancelLoad()
	s.mu.Lock()
	var skipped []SkippedTrack
	defer func() { s.mu.Unlock(); s.reportSkipped(skipped) }()
	if s.closed {
		return s.revision, ErrSupervisorClosed
	}
	if err != nil {
		return s.revision, err
	}
	if committed.Revision != expected {
		return s.revision, ErrRevisionConflict
	}
	if revision != expected {
		if revision > s.revision {
			s.adoptLocked(stored, revision)
		}
		return s.revision, nil
	}
	if expected <= s.revision {
		if expected == s.revision && !startPlayback {
			return s.revision, nil
		}
		return s.revision, ErrRevisionConflict
	}
	state := cloneState(committed)
	if state.Paused || state.Current == nil || s.state.Current == nil || state.Current.ID != s.state.Current.ID {
		s.closePlaybackLocked()
	}
	s.state, s.revision = cloneState(state), revision
	if startPlayback {
		s.startRequest++
	}
	if startPlayback && !state.Paused && state.Current != nil && s.playback == nil {
		var startErr error
		skipped, startErr = s.startLocked(s.ctx, true, nil)
		if err := startErr; err != nil {
			pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
			_ = s.pauseLocked(pauseCtx)
			cancel()
			s.signalLocked()
			return s.revision, err
		}
	}
	s.idleCheckLocked()
	s.signalLocked()
	return s.revision, nil
}

// RefreshState repairs a missed hand-off from durable state without starting audio.
// The native read runs outside the state mutex; a newer runtime revision wins a racing read.
func (s *Supervisor) RefreshState(ctx context.Context) (State, uint64, error) {
	stored, revision, err := s.store.Load(ctx, s.channel)
	if err != nil {
		return State{}, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return State{}, s.revision, ErrSupervisorClosed
	}
	if revision > s.revision {
		s.adoptLocked(stored, revision)
	}
	return cloneState(s.state), s.revision, nil
}

func (s *Supervisor) adoptLocked(state State, revision uint64) {
	state = cloneState(state)
	if state.Current != nil && s.state.Current != nil && state.Current.ID == s.state.Current.ID {
		// Durable reads are restart-safe (paused), not a live playback command.
		// Metadata adoption must retain both a live track and an in-flight start.
		state.Paused = s.state.Paused
		if state.Paused {
			s.closePlaybackLocked()
		}
	} else {
		s.closePlaybackLocked()
		state.Paused = true
	}
	s.state, s.revision = state, revision
	s.idleCheckLocked()
	s.signalLocked()
}

// SchedulePersistedSnapshot adopts first, then schedules provider work off the
// interaction drain. A later pause/skip invalidates the scheduled start.
func (s *Supervisor) SchedulePersistedSnapshot(ctx context.Context, state State, revision uint64, start bool) (uint64, error) {
	adopted, err := s.ApplyPersistedSnapshot(ctx, state, revision, false)
	if err != nil || !start {
		return adopted, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.revision, ErrSupervisorClosed
	}
	if s.revision == revision && !state.Paused && state.Current != nil {
		// A concurrent read may already have adopted this revision from the
		// restart-safe store view (paused). This explicit, committed Resume
		// still owns its hand-off; a newer Pause/Skip revision would fail the
		// revision check above instead of reviving an obsolete start.
		if s.state.Paused && s.playback == nil {
			s.state = cloneState(state)
			s.signalLocked()
		}
		if !s.state.Paused && s.state.Current != nil && s.playback == nil {
			s.scheduleStartLocked()
		}
	}
	return s.revision, nil
}

func (s *Supervisor) scheduleStartLocked() {
	s.startRequest++
	request := s.startRequest
	go func() {
		s.mu.Lock()
		if s.closed || request != s.startRequest || s.state.Paused || s.state.Current == nil {
			s.mu.Unlock()
			return
		}
		skips, err := s.startLocked(s.ctx, true, nil)
		if err != nil && request == s.startRequest && !s.closed {
			pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
			_ = s.pauseLocked(pauseCtx)
			cancel()
		}
		s.idleCheckLocked()
		s.signalLocked()
		s.mu.Unlock()
		s.reportSkipped(skips)
		if err != nil && s.onAutomaticPlaybackFailure != nil {
			s.onAutomaticPlaybackFailure(s.ctx, err)
		}
	}()
}

// WithSkippedTrack reports a failed item only after its removal is durable.
func WithSkippedTrack(report func(context.Context, []SkippedTrack)) SupervisorOption {
	return func(s *Supervisor) { s.onSkipped = report }
}
func WithPlaybackLog(logger *slog.Logger) SupervisorOption {
	return func(s *Supervisor) { s.logger = logger }
}

const maxConsecutiveStartSkips = 2
const maxTotalStartSkips = 5
const maxStartDuration = 45 * time.Second

type startRun struct {
	request     uint64
	interrupted bool
	cancel      context.CancelFunc
	done        chan struct{}
}
type startBudget struct {
	skipped, consecutive int
	deadline             time.Time
}

func (b *startBudget) succeeded() { b.consecutive = 0 }

func (b *startBudget) exhausted(now time.Time) bool {
	return b.consecutive >= maxConsecutiveStartSkips || b.skipped >= maxTotalStartSkips || !now.Before(b.deadline)
}

func (s *Supervisor) reportSkipped(skips []SkippedTrack) {
	if len(skips) > 0 && s.onSkipped != nil {
		s.onSkipped(s.ctx, skips)
	}
}

// startLocked returns with s.mu held, but provider startup runs without it.
// State changes cancel the pending run; no stale result can publish into it.
func (s *Supervisor) startLocked(ctx context.Context, explicit bool, budget *startBudget) ([]SkippedTrack, error) {
	for s.starting != nil {
		pending := s.starting.done
		s.mu.Unlock()
		select {
		case <-pending:
		case <-ctx.Done():
		}
		s.mu.Lock()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if s.closed || s.state.Paused || s.state.Current == nil || s.playback != nil {
			return nil, nil
		}
	}
	runCtx, cancelCause := context.WithCancelCause(ctx)
	cancel := func() { cancelCause(context.Canceled) }
	timeout := time.AfterFunc(maxStartDuration, func() { cancelCause(context.DeadlineExceeded) })
	successful := false
	run := &startRun{request: s.startRequest, cancel: cancel, done: make(chan struct{})}
	s.starting = run
	defer func() {
		timeout.Stop()
		if !successful {
			cancel()
		}
		if s.starting == run {
			s.starting = nil
		}
		close(run.done)
	}()
	if budget == nil {
		budget = &startBudget{}
	}
	// EOF successors share skip totals with their playing chain. Each startup
	// callback gets its own deadline; time spent successfully playing is free.
	budget.deadline = time.Now().Add(maxStartDuration)
	var skips []SkippedTrack
	for !s.closed && !s.state.Paused && s.state.Current != nil {
		if err := runCtx.Err(); err != nil {
			if run.interrupted || run.request != s.startRequest {
				return skips, nil
			}
			return skips, context.Cause(runCtx)
		}
		if budget.exhausted(time.Now()) {
			return skips, s.pauseLocked(ctx)
		}
		track := *s.state.Current
		s.mu.Unlock()
		var p Playback
		var err error
		if starter, ok := s.starter.(interface {
			StartExplicit(context.Context, Track) (Playback, error)
		}); explicit && ok {
			p, err = starter.StartExplicit(runCtx, track)
		} else {
			p, err = s.starter.Start(runCtx, track)
		}
		s.mu.Lock()
		if run.interrupted || s.closed || s.state.Paused || s.state.Current == nil || s.state.Current.ID != track.ID {
			if p != nil {
				s.mu.Unlock()
				_ = p.Close()
				s.mu.Lock()
			}
			return skips, nil
		}
		if runCtx.Err() != nil {
			if p != nil {
				s.mu.Unlock()
				_ = p.Close()
				s.mu.Lock()
			}
			if run.interrupted || run.request != s.startRequest || s.closed || s.state.Paused || s.state.Current == nil || s.state.Current.ID != track.ID {
				return skips, nil
			}
			return skips, context.Cause(runCtx)
		}
		if err != nil {
			if run.request != s.startRequest {
				return skips, nil
			}
			var permanent interface{ ProviderFailure() bool }
			if !errors.As(err, &permanent) || !permanent.ProviderFailure() {
				return skips, err
			}
			originalRepeat := s.state.Repeat
			nextBase := cloneState(s.state)
			nextBase.Repeat = RepeatOff
			next, applyErr := Apply(nextBase, s.state.Revision, Operation{Kind: OpSkip, NowMS: time.Now().UnixMilli()})
			if applyErr != nil {
				return skips, applyErr
			}
			next.Repeat = originalRepeat
			revision, saveErr := s.store.Save(runCtx, s.channel, s.revision, next)
			if saveErr != nil {
				return skips, saveErr
			}
			s.state, s.revision = next, revision
			skips = append(skips, SkippedTrack{Track: track, Failure: err})
			budget.skipped++
			budget.consecutive++
			if budget.exhausted(time.Now()) && s.state.Current != nil {
				return skips, s.pauseLocked(ctx)
			}
			continue
		}
		if p == nil || p.Done() == nil {
			if p != nil {
				_ = p.Close()
			}
			return skips, errors.New("music track starter returned invalid playback")
		}
		budget.succeeded()
		successful = true
		timeout.Stop()
		go func() { <-p.Done(); cancel() }()
		s.playback = p
		s.state.PositionMS, s.state.PositionAtMS = 0, time.Now().UnixMilli()
		go s.monitor(p, budget)
		return skips, nil
	}
	return skips, nil
}

func (s *Supervisor) monitor(p Playback, budget *startBudget) {
	<-p.Done()
	err := p.Wait()
	s.mu.Lock()
	var skipped []SkippedTrack
	defer func() { s.mu.Unlock(); s.reportSkipped(skipped) }()
	if s.closed || s.playback != p {
		return
	}
	s.playback = nil
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	if err != nil {
		logger.WarnContext(s.ctx, "bot music playback ended", "class", FailureClass(err))
	} else {
		logger.InfoContext(s.ctx, "bot music playback ended", "class", "eof")
	}
	defer s.idleCheckLocked()
	if err != nil {
		_ = s.pauseLocked(s.ctx)
		s.signalLocked()
		return
	}
	next, applyErr := Apply(s.state, s.state.Revision, Operation{Kind: OpComplete, NowMS: time.Now().UnixMilli()})
	if applyErr != nil {
		_ = s.pauseLocked(s.ctx)
		s.signalLocked()
		return
	}
	revision, saveErr := s.store.Save(s.ctx, s.channel, s.revision, next)
	if saveErr != nil {
		_ = s.pauseLocked(s.ctx)
		s.signalLocked()
		return
	}
	s.state, s.revision = next, revision
	if !s.state.Paused && s.state.Current != nil {
		var startErr error
		skipped, startErr = s.startLocked(s.ctx, false, budget)
		if err := startErr; err != nil {
			if s.onAutomaticPlaybackFailure != nil {
				s.onAutomaticPlaybackFailure(s.ctx, err)
			}
			_ = s.pauseLocked(s.ctx)
		}
	}
	s.signalLocked()
}

func (s *Supervisor) pauseLocked(ctx context.Context) error {
	next, err := Apply(s.state, s.state.Revision, Operation{Kind: OpPause, NowMS: time.Now().UnixMilli()})
	if err != nil {
		s.state.Paused = true
		return err
	}
	revision, err := s.store.Save(ctx, s.channel, s.revision, next)
	if err != nil {
		s.state.Paused = true
		return err
	}
	s.state, s.revision = next, revision
	return nil
}

// EmptyRoom pauses the queue, closes the per-track provider, and schedules the
// caller's leave action after the configured grace. Position remains the last
// persisted value; this supervisor is not ready for production until providers
// can pause and report the current position accurately.
func (s *Supervisor) EmptyRoom(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil music operation context")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSupervisorClosed
	}
	next, err := Apply(s.state, s.state.Revision, Operation{Kind: OpPause, NowMS: time.Now().UnixMilli()})
	if err != nil {
		return err
	}
	revision, err := s.store.Save(ctx, s.channel, s.revision, next)
	if err != nil {
		return err
	}
	s.state, s.revision = next, revision
	s.closePlaybackLocked()
	if s.emptyTimer != nil {
		s.emptyTimer.Stop()
	}
	if s.onEmpty != nil {
		s.emptyTimer = s.afterFunc(s.grace, s.onEmpty)
	}
	s.idleCheckLocked()
	s.signalLocked()
	return nil
}

// RoomOccupied cancels a pending leave callback when someone returns.
func (s *Supervisor) RoomOccupied() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.emptyTimer != nil {
		s.emptyTimer.Stop()
		s.emptyTimer = nil
	}
}

func (s *Supervisor) closePlaybackLocked() {
	s.startRequest++
	if s.starting != nil {
		s.starting.interrupted = true
		s.starting.cancel()
	}
	if s.playback != nil {
		p := s.playback
		s.playback = nil
		_ = p.Close()
	}
}
func (s *Supervisor) signalLocked() { close(s.changed); s.changed = make(chan struct{}) }

// WaitForChange waits until persisted state or playback status changes.
func (s *Supervisor) WaitForChange(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil music wait context")
	}
	s.mu.Lock()
	changed := s.changed
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	}
}

func (s *Supervisor) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.stopLifetime != nil {
		s.stopLifetime()
	}
	if s.emptyTimer != nil {
		s.emptyTimer.Stop()
		s.emptyTimer = nil
	}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.closePlaybackLocked()
	var pending <-chan struct{}
	if s.starting != nil {
		pending = s.starting.done
	}
	s.mu.Unlock()
	if pending != nil {
		<-pending
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if closer, ok := s.starter.(io.Closer); ok {
		err = closer.Close()
	}
	s.signalLocked()
	return err
}

package music

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/id"
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
	pausedPlayback             bool
	pauseTimer                 Timer
	pauseGen                   uint64
	pauseAfter                 func(time.Duration, func()) Timer
	// Task 10c: leave the voice room after nothing has played for idleFor() (read when the
	// supervisor goes idle, so a changed setting applies to the next idle period).
	idleFor   func() time.Duration
	idleTimer Timer
	idleGen   uint64
	// Bot v2 task 18: stage timings of the last 20 starts, and the timer that pre-buffers the next track.
	timings       []StartTiming
	prefetch      *prefetchPlan
	prefetchAfter func(time.Duration, func()) Timer
	// v0.1.0-bot2.2: the queue items loaded from storage when the supervisor was made (restored=true in the logs; at
	// most QueueLimit), and the item of the playback now published.
	restored    map[string]bool
	playingItem string
	// onRemoveFailed is told when a failed item's removal could not be saved (final review I1): the queue is stored on
	// the helper, so a helper that died under the track cannot save its removal, and the owner removes it on the
	// replacement (RemoveFailedItem).
	onRemoveFailed func(ctx context.Context, item string, err error)
}

// TrackRemovedError is an explicit start whose track failed and was removed from the queue and from persistence
// (v0.1.0-bot2.2): the waiting command answers that it was removed, so no channel notice is sent for it. Playing is
// the queued track that started in its place, or nil.
type TrackRemovedError struct {
	ItemID  string
	Playing *Track
	Cause   error
}

func (e *TrackRemovedError) Error() string {
	return "music: the track failed to start and was removed from the queue"
}
func (e *TrackRemovedError) Unwrap() error { return e.Cause }

// trackFailed reports an error that is the track's own failure, so its item leaves the queue (v0.1.0-bot2.2, owner
// rule "failed track is removed"; controller ruling, fix round 1). A track is removed when its own playback broke:
// an item-specific provider refusal, a helper closed under it (the production wrong-track case), a media epoch still
// without the Bot's leaf after the one rejoin, failed call-group checks, an encoder or publish failure. It is kept,
// paused, when there is nobody to play to (ErrNeedsExplicitPlay), when the provider as a whole is unavailable
// (ErrPlayerUnavailable) or when the connection merely ended or expired (ErrConnectionEnded: a server-ended call,
// an idle leave, a transport death, the room's lifetime). A user's pause or stop never reaches this check.
//
// Final review I2: it is also kept when the Bot was not admitted to publish (ErrNotAdmitted: the installation not
// ready, a Bot permission change, a held user, trust not available yet) and when a dillad request failed for a
// passing reason: a retry-safe refusal or cooperative stop (client.IsDeferred), or a 5xx, 409 or 429 answer.
//
// Residual round: a helper failure (helper.ErrClosed, helper.ErrOutcomeUnknown) anywhere in the error wins over every
// keep reason, so a dead helper joined with a dillad 503 or an admission refusal still removes the track. A failed
// call-group check during playback (ErrCallCheckFailed) is kept only when the Bot was not admitted (ruling R7d): the
// dillad keep reasons apply to start stages only, and keeping a track whose checks fail would bring back the loop
// where it stops, stays paused and the next /play resumes it.
func trackFailed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, helper.ErrClosed) || errors.Is(err, helper.ErrOutcomeUnknown) {
		return true
	}
	if errors.Is(err, ErrNeedsExplicitPlay) || errors.Is(err, ErrPlayerUnavailable) || errors.Is(err, ErrConnectionEnded) || errors.Is(err, ErrNotAdmitted) {
		return false
	}
	if errors.Is(err, ErrCallCheckFailed) {
		return true
	}
	return !client.IsDeferred(err) && !transientServerError(err)
}

// transientServerError is a dillad answer that says nothing about the track: a server error, a conflict, a rate limit.
func transientServerError(err error) bool {
	var api *client.APIError
	if !errors.As(err, &api) {
		return false
	}
	return api.Status >= 500 || api.Status == 409 || api.Status == 429
}

// startFailed reports a start that failed at one of its stages for the track's own reason.
func startFailed(err error) bool {
	var stage *StartError
	return errors.As(err, &stage) && trackFailed(err)
}

// removeCurrentLocked durably drops the current item after its failure. Repeat never queues it again; the next item
// becomes current (paused only when the queue was paused).
func (s *Supervisor) removeCurrentLocked(ctx context.Context) error {
	originalRepeat := s.state.Repeat
	base := cloneState(s.state)
	base.Repeat = RepeatOff
	next, err := Apply(base, s.state.Revision, Operation{Kind: OpSkip, NowMS: time.Now().UnixMilli()})
	if err != nil {
		return err
	}
	next.Repeat = originalRepeat
	revision, err := s.store.Save(ctx, s.channel, s.revision, next)
	if err != nil {
		if s.onRemoveFailed != nil && s.state.Current != nil {
			s.onRemoveFailed(ctx, s.state.Current.ID, err)
		}
		return err
	}
	s.state, s.revision = next, revision
	return nil
}

// WithRemoveFailed reports a failed item whose removal could not be saved (final review I1). report runs with the
// supervisor's lock held: it must not call back into the supervisor or take a lock held while calling it.
func WithRemoveFailed(report func(ctx context.Context, item string, err error)) SupervisorOption {
	return func(s *Supervisor) { s.onRemoveFailed = report }
}

// ErrFailedItemBusy is RemoveFailedItem on an item that is current but playing or starting (residual round, item 3).
var ErrFailedItemBusy = errors.New("music: the failed item is playing or starting")

// RemoveFailedItem removes item, a track that failed under a helper that could not save its removal, when it is still
// the current track and nothing plays or starts (final review I1). It is removed as a failed start removes it: repeat
// never queues it again, and the next item becomes current, paused when the queue is paused. removed is false when item
// is no longer current (nothing is changed then). While item is current but playing or starting, nothing is changed
// and the error is ErrFailedItemBusy: the caller keeps it and looks again later.
func (s *Supervisor) RemoveFailedItem(ctx context.Context, item string) (removed bool, err error) {
	if ctx == nil || item == "" {
		return false, errors.New("invalid failed item removal")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, ErrSupervisorClosed
	}
	if s.state.Current == nil || s.state.Current.ID != item {
		return false, nil
	}
	if s.playback != nil || s.starting != nil {
		return false, ErrFailedItemBusy
	}
	if err := s.removeCurrentLocked(ctx); err != nil {
		return false, err
	}
	s.signalLocked()
	return true, nil
}

// withoutItem takes item's skip out of skips (its command answers for it).
func withoutItem(skips []SkippedTrack, item string) ([]SkippedTrack, *SkippedTrack) {
	for i := range skips {
		if skips[i].Track.ID == item {
			removed := skips[i]
			return append(skips[:i:i], skips[i+1:]...), &removed
		}
	}
	return skips, nil
}

// IdleLeaver is a TrackStarter that can leave its retained voice room while nothing plays.
type IdleLeaver interface{ LeaveIdle() error }

// CallWarmer is a TrackStarter that can join its voice room ahead of the first start (Bot v2 Task 19): an authorised
// /play begins the call-group join and the room connection while its track resolves. WarmCall must not block; the
// start that follows uses what it prepared.
type CallWarmer interface{ WarmCall(context.Context) }

// WarmCall asks the starter to join the voice room for a /play that was just opened, unless a track is already
// playing, paused in place or starting (the room is joined then) or the supervisor is closed. group is the room's
// current call group as the /play saw it (PlaybackGroup of the warm's context). It never starts audio and never
// blocks.
func (s *Supervisor) WarmCall(group id.ID) {
	s.mu.Lock()
	warmer, ok := s.starter.(CallWarmer)
	busy := s.closed || s.playback != nil || s.starting != nil
	ctx := s.ctx
	s.mu.Unlock()
	if ok && !busy {
		warmer.WarmCall(WithPlaybackGroup(ctx, group))
	}
}

// PlayAbandoned tells the starter that a /play whose warm it began ended without starting a track (Bot v2 Task 19
// fix round 1, M3): the call prepared for it is dropped and the room, if nothing plays in it, is left.
func (s *Supervisor) PlayAbandoned() {
	s.mu.Lock()
	abandoner, ok := s.starter.(interface{ AbandonWarm() })
	busy := s.closed || s.playback != nil || s.starting != nil
	s.mu.Unlock()
	if ok && !busy {
		abandoner.AbandonWarm()
	}
}

// Playing reports whether a track is playing now (published and not paused).
func (s *Supervisor) Playing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && s.playback != nil && !s.pausedPlayback
}

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
	s.prefetchCheckLocked()
	if s.idleFor == nil {
		return
	}
	if (s.playback != nil && !s.pausedPlayback) || s.starting != nil || s.closed {
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
	if s.closed || s.idleTimer == nil || s.idleGen != gen || (s.playback != nil && !s.pausedPlayback) || s.starting != nil {
		s.mu.Unlock()
		return
	}
	s.idleTimer = nil
	s.closePlaybackLocked()
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
	s.restored = make(map[string]bool, len(state.Queue)+1)
	if state.Current != nil {
		s.restored[state.Current.ID] = true
	}
	for _, track := range state.Queue {
		s.restored[track.ID] = true
	}
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
	before := cloneState(s.state)
	wasPaused := s.pausedPlayback
	if op.Kind == OpPause {
		if s.canHoldPlaybackLocked() {
			s.pausePlaybackLocked()
		}
	}
	base := cloneState(s.state)
	next, err := Apply(base, expected, op)
	if err != nil {
		var rollbackErr error
		skipped, rollbackErr = s.rollbackPauseLocked(before, wasPaused, async)
		return cloneState(s.state), errors.Join(err, rollbackErr)
	}
	revision, err := s.store.Save(ctx, s.channel, s.revision, next)
	if err != nil {
		var rollbackErr error
		skipped, rollbackErr = s.rollbackPauseLocked(before, wasPaused, async)
		return cloneState(s.state), errors.Join(err, rollbackErr)
	}
	s.state, s.revision = next, revision
	if op.Kind == OpResume {
		s.startRequest++
	}
	switch op.Kind {
	case OpPause:
		s.pausePlaybackLocked()
	case OpStop, OpClear, OpSkip:
		s.closePlaybackLocked()
	case OpResume:
		s.resumePlaybackLocked()
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
	if state.Current == nil || s.state.Current == nil || state.Current.ID != s.state.Current.ID {
		s.closePlaybackLocked()
	}
	s.state, s.revision = cloneState(state), revision
	if state.Paused {
		s.pausePlaybackLocked()
	}
	if startPlayback {
		s.resumePlaybackLocked()
		s.startRequest++
	}
	if startPlayback && !state.Paused && state.Current != nil && s.playback == nil {
		requested := state.Current.ID
		var startErr error
		skipped, startErr = s.startLocked(s.ctx, true, nil)
		// The requested track failed and left the queue: its command answers for it, not a channel notice.
		var removed *SkippedTrack
		skipped, removed = withoutItem(skipped, requested)
		if err := startErr; err != nil {
			pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
			_ = s.pauseLocked(pauseCtx)
			cancel()
			s.signalLocked()
			if removed != nil {
				return s.revision, &TrackRemovedError{ItemID: requested, Cause: removed.Failure}
			}
			return s.revision, err
		}
		if removed != nil {
			var playing *Track
			if s.playback != nil && s.state.Current != nil {
				current := *s.state.Current
				playing = &current
			}
			s.idleCheckLocked()
			s.signalLocked()
			return s.revision, &TrackRemovedError{ItemID: requested, Playing: playing, Cause: removed.Failure}
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
		if !state.Paused && s.playback != nil {
			// Load clears a playing snapshot's stale playhead for restart safety.
			// A live metadata refresh must keep this process's playback position.
			state.PositionMS = s.state.PositionMS
			state.PositionAtMS = s.state.PositionAtMS
		}
		if state.Paused && !s.pausedPlayback {
			s.pausePlaybackLocked()
		}
		if s.pausedPlayback {
			state.PositionMS = s.state.PositionMS
			state.PositionAtMS = 0
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
		if s.state.Paused {
			s.state = cloneState(state)
			s.signalLocked()
		}
		s.resumePlaybackLocked()
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
	if explicit {
		ctx = client.WithInteractive(ctx)
	} else {
		ctx = client.WithBackground(ctx)
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
		if s.atTrackEndLocked() {
			// A cold start at the end produces no audio: the track has finished.
			next, applyErr := Apply(cloneState(s.state), s.state.Revision, Operation{Kind: OpComplete, NowMS: time.Now().UnixMilli()})
			if applyErr != nil {
				return skips, applyErr
			}
			revision, saveErr := s.store.Save(runCtx, s.channel, s.revision, next)
			if saveErr != nil {
				return skips, saveErr
			}
			s.state, s.revision = next, revision
			continue
		}
		track := *s.state.Current
		startCtx, startTimer := withStartTimer(WithPlaybackItem(WithStartAtMS(runCtx, s.state.PositionMS), track.ID, s.restored[track.ID]))
		startBegan := time.Now()
		s.mu.Unlock()
		var p Playback
		var err error
		if starter, ok := s.starter.(interface {
			StartExplicit(context.Context, Track) (Playback, error)
		}); explicit && ok {
			p, err = starter.StartExplicit(startCtx, track)
		} else {
			p, err = s.starter.Start(startCtx, track)
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
			// v0.1.0-bot2.2: a track that failed to start leaves the queue and persistence (not only a permanent
			// provider failure), so a later /play never resumes it; the next queued track starts.
			if !startFailed(err) {
				return skips, err
			}
			if removeErr := s.removeCurrentLocked(runCtx); removeErr != nil {
				return skips, removeErr
			}
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
		s.recordTimingLocked(startTimer.finish(track.ID, startBegan, time.Now()))
		budget.succeeded()
		successful = true
		timeout.Stop()
		go func() { <-p.Done(); cancel() }()
		s.playback = p
		s.playingItem = track.ID
		s.state.PositionAtMS = time.Now().UnixMilli()
		go s.monitor(p, budget)
		return skips, nil
	}
	return skips, nil
}

// endTolerance is how close to the known duration an unclassified stream end still counts as the track's own end.
const endTolerance = 3 * time.Second

// endDetail is the fixed verdict logged on a playback end: eof, cancelled, connection, helper or invalid.
func endDetail(err error) string {
	switch {
	case err == nil:
		return "eof"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, ErrConnectionEnded), errors.Is(err, ErrNotAdmitted), errors.Is(err, ErrPlayerUnavailable), errors.Is(err, ErrNeedsExplicitPlay):
		return "connection"
	case errors.Is(err, helper.ErrClosed), errors.Is(err, helper.ErrOutcomeUnknown), errors.Is(err, helper.ErrLaunch):
		return "helper"
	}
	return "invalid"
}

// endedAtDurationLocked reports whether the playing track ran to within endTolerance of its known duration.
func (s *Supervisor) endedAtDurationLocked(p Playback) bool {
	c := s.state.Current
	if c == nil || c.ID != s.playingItem || c.DurationMS <= 0 {
		return false
	}
	var elapsed int64
	if source, ok := p.(Pausable); ok {
		elapsed = source.PositionMS()
	} else if s.state.PositionAtMS > 0 {
		elapsed = s.state.PositionMS + time.Now().UnixMilli() - s.state.PositionAtMS
	} else {
		return false
	}
	return elapsed >= c.DurationMS-endTolerance.Milliseconds()
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
	wasPaused := s.pausedPlayback
	if wasPaused {
		s.cancelPauseLocked()
		if err != nil {
			s.signalLocked()
			s.idleCheckLocked()
			return
		}
	}
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("item_id", s.playingItem, "restored", s.restored[s.playingItem])
	detail := endDetail(err)
	if err != nil && detail == "invalid" && s.endedAtDurationLocked(p) {
		// The stream stopped without its closing page at the track's own end: a normal end, not a failure.
		// The Bot decides this from the known duration and logs only the verdict, never the duration.
		logger.InfoContext(s.ctx, "bot music playback ended", "class", "eof", "detail", "eof", "error_class", helper.Class(err))
		err = nil
	} else if err != nil {
		logger.WarnContext(s.ctx, "bot music playback ended", "class", FailureClass(err), "detail", detail, "error_class", helper.Class(err))
	} else {
		logger.InfoContext(s.ctx, "bot music playback ended", "class", "eof", "detail", detail)
	}
	defer s.idleCheckLocked()
	if err != nil {
		if errors.Is(err, ErrSourceGone) && s.state.Current != nil && s.state.Current.Provider == ProviderYouTube {
			if source, ok := p.(Pausable); ok {
				s.state.PositionMS = s.playbackPositionLocked(source)
				s.state.PositionAtMS = 0
				var startErr error
				skipped, startErr = s.startLocked(s.ctx, false, budget)
				if startErr != nil {
					_ = s.pauseLocked(s.ctx)
					if s.onAutomaticPlaybackFailure != nil {
						s.onAutomaticPlaybackFailure(s.ctx, startErr)
					}
				}
				s.signalLocked()
				return
			}
		}
		// v0.1.0-bot2.2: a track that ended on an error leaves the queue (a channel notice says so; no command waits)
		// and the next queued one starts. A user's pause or stop never reaches here.
		if trackFailed(err) && s.state.Current != nil && s.state.Current.ID == s.playingItem {
			track := *s.state.Current
			if removeErr := s.removeCurrentLocked(s.ctx); removeErr == nil {
				skipped = append(skipped, SkippedTrack{Track: track, Failure: err})
				if !s.state.Paused && s.state.Current != nil {
					var skips []SkippedTrack
					var startErr error
					skips, startErr = s.startLocked(s.ctx, false, budget)
					skipped = append(skipped, skips...)
					if startErr != nil {
						if s.onAutomaticPlaybackFailure != nil {
							s.onAutomaticPlaybackFailure(s.ctx, startErr)
						}
						_ = s.pauseLocked(s.ctx)
					}
				}
				s.signalLocked()
				return
			}
		}
		_ = s.pauseLocked(s.ctx)
		s.signalLocked()
		return
	}
	// A clean EOF that won the race with Pause still owns its advance.
	// Complete against a playing base, then keep the successor paused.
	base := cloneState(s.state)
	if wasPaused {
		base.Paused = false
	}
	next, applyErr := Apply(base, s.state.Revision, Operation{Kind: OpComplete, NowMS: time.Now().UnixMilli()})
	if applyErr != nil {
		_ = s.pauseLocked(s.ctx)
		s.signalLocked()
		return
	}
	if wasPaused {
		next.Paused = true
		next.PositionAtMS = 0
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
	s.cancelPauseLocked()
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
	s.cancelPrefetchLocked()
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

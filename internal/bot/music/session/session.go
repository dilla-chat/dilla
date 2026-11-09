// Package session composes the per-call provider, encoder, and LiveKit playback lifecycle.
package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/livekit"
	musicruntime "github.com/dilla-chat/dilla/internal/bot/music/runtime"
	"github.com/dilla-chat/dilla/internal/bot/music/youtube"
	"github.com/dilla-chat/dilla/internal/id"
)

var ErrInvalid = errors.New("music session: invalid configuration")

type PCMStarter interface {
	Start(context.Context, string) (io.ReadCloser, error)
}
type Encoder interface {
	Done() <-chan struct{}
	Wait() error
	Close() error
}
type EncoderStarter interface {
	Start(context.Context, io.ReadCloser) (Encoder, error)
}
type Publisher interface {
	Done() <-chan struct{}
	Wait() error
	Close() error
}
type PublisherStarter interface {
	Start(context.Context, client.Call, id.ID, livekit.EpochSource, Encoder) (Publisher, error)
}

type Options struct {
	Community, Channel id.ID
	Provider           music.Provider
	Track              string
	Call               client.Call
	// PrepareCall runs only after provider PCM and encoder have started.
	PrepareCall func(context.Context) (client.Call, error)
	Group       id.ID
	PCM         PCMStarter
	Encoder     EncoderStarter
	Publisher   PublisherStarter
	Epochs      livekit.EpochSource
}

// OptionsForTrack supplies call credentials, provider PCM and epoch-bound
// publisher dependencies for a queue track.
type OptionsForTrack func(context.Context, music.Track) (Options, error)

type explicitStartKey struct{}

// WithExplicitStart marks ctx as a start a member asked for, as StartExplicit does.
func WithExplicitStart(ctx context.Context) context.Context {
	return context.WithValue(ctx, explicitStartKey{}, true)
}

// ExplicitStart reports whether ctx is a start a member asked for (StartExplicit: a /play, a resume, a skip), not the
// supervisor's automatic advance to the next queued track.
func ExplicitStart(ctx context.Context) bool {
	explicit, _ := ctx.Value(explicitStartKey{}).(bool)
	return explicit
}

type QueueStarter struct {
	LogContext func(context.Context) context.Context
	Logger     *slog.Logger
	OnSkipped  func(context.Context, []music.SkippedTrack)
	// OnCallEnded clears call-scoped registrations after a successful room leave.
	OnCallEnded func()
	// SetCallEnded installs call cleanup in the retained room lifecycle reporter.
	SetCallEnded func(func())

	OptionsFor OptionsForTrack
	// ResetConnection is reserved for an explicit playback action. Automatic EOF
	// advancement cannot clear a disconnected room's terminal state.
	ResetConnection func() error
	CloseConnection func() error
	// LeaveConnection disconnects an idle retained room (Task 10c idle-leave); nil does nothing.
	LeaveConnection func() error
	// IdleLeaveAfter is the installation's current "Leave voice when idle" time; the supervisor
	// that owns this starter leaves the room once nothing has played for that long. Nil disables.
	IdleLeaveAfter func() time.Duration
	// OnStateChange is called after the channel's queue state was persisted by the playback supervisor: the playing
	// track or the queue changed on its own (a track ended and the next began). Bot v2 Task 17 sends a call hint from
	// it. Nil does nothing.
	OnStateChange func(context.Context)
	// Warm begins the voice room's call-group join, call token and room connection for an opened /play, while its
	// track resolves (Bot v2 Task 19). It must not block; the next start's PrepareCall uses its result. Nil warms
	// nothing.
	Warm func(context.Context)
	// OnStartFailed is told of every failed start attempt: what was prepared for the call is not reused (Bot v2 Task 19
	// fix round 1, I2), so a retry reads membership, credentials and the epoch again. Nil does nothing.
	OnStartFailed func(error)
	// Abandon is a /play that ended without starting: a call its warm prepared is dropped and the idle room left (M3).
	Abandon func()
}

// AbandonWarm implements music.CallWarmer's abandon.
func (s QueueStarter) AbandonWarm() {
	if s.Abandon != nil {
		s.Abandon()
	}
}

// WarmCall implements music.CallWarmer.
func (s QueueStarter) WarmCall(ctx context.Context) {
	if s.Warm != nil && ctx != nil {
		s.Warm(ctx)
	}
}

// LeaveIdle leaves the retained room when nothing is playing.
func (s QueueStarter) LeaveIdle() error {
	if s.LeaveConnection != nil {
		if err := s.LeaveConnection(); err != nil {
			return err
		}
		if s.OnCallEnded != nil {
			s.OnCallEnded()
		}
	}
	return nil
}

func (s QueueStarter) StartExplicit(ctx context.Context, track music.Track) (music.Playback, error) {
	if ctx == nil || s.OptionsFor == nil {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.ResetConnection != nil {
		if err := s.ResetConnection(); err != nil {
			return nil, err
		}
	}
	ctx = context.WithValue(client.WithInteractive(ctx), explicitStartKey{}, true)
	playback, err := s.Start(ctx, track)
	if !errors.Is(err, livekit.ErrBinding) || ctx.Err() != nil || s.ResetConnection == nil {
		return playback, err
	}
	// A fresh authorized call can replace a retained room before its delayed
	// disconnect callback arrives. acquireRoom marks that old binding dead.
	// Only an explicit action may tear it down and reacquire current options.
	if resetErr := s.ResetConnection(); resetErr != nil {
		return nil, resetErr
	}
	return s.Start(ctx, track)
}

// Close releases the retained connection when its queue owner is closed.
func (s QueueStarter) Close() error {
	if s.CloseConnection != nil {
		if err := s.CloseConnection(); err != nil {
			return err
		}
		if s.OnCallEnded != nil {
			s.OnCallEnded()
		}
	}
	return nil
}

func (s QueueStarter) Start(ctx context.Context, track music.Track) (music.Playback, error) {
	if ctx == nil || s.OptionsFor == nil {
		return nil, ErrInvalid
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		logger := s.Logger
		if logger == nil {
			logger = music.PlaybackLogger(ctx)
		}
		explicit, _ := ctx.Value(explicitStartKey{}).(bool)
		attemptCtx := music.WithPlaybackLogger(ctx, logger.With("provider", string(track.Provider), "attempt", attempt+1, "explicit", explicit))
		if s.LogContext != nil {
			attemptCtx = s.LogContext(attemptCtx)
		}
		playback, err := s.startOnce(attemptCtx, track)
		if err != nil && s.OnStartFailed != nil {
			s.OnStartFailed(err)
		}
		// A previous publisher's departure can remove the replacement during
		// its handshake. No source was published successfully: close resources
		// and re-read membership, call credentials and epoch on a bounded retry.
		if err == nil || ctx.Err() != nil || attempt >= 2 ||
			(!errors.Is(err, context.Canceled) && !errors.Is(err, livekit.ErrEpochChanged) && !errors.Is(err, livekit.ErrRoomConnection)) {
			return playback, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s QueueStarter) startOnce(ctx context.Context, track music.Track) (music.Playback, error) {
	if s.OptionsFor == nil {
		return nil, ErrInvalid
	}
	o, err := s.OptionsFor(ctx, track)
	if err != nil {
		var stage *music.StartError
		if errors.As(err, &stage) {
			return nil, music.StartStage(ctx, stage.Stage, stage.Cause)
		}
		return nil, providerStage(ctx, "provider_check", err)
	}
	if track.Provider != music.ProviderYouTube ||
		(o.Provider != "" && o.Provider != track.Provider) {
		return nil, music.StartStage(ctx, "provider_check", ErrInvalid)
	}
	o.Provider = track.Provider
	o.Track = track.CanonicalID
	return Start(ctx, o)
}

// Prefetch pre-resolves and pre-buffers track, so the Start that follows when
// the current track ends has its audio ready. It never joins a call or touches
// the room: only the provider is asked. A provider that cannot pre-buffer, or
// a track it refuses, simply starts cold later.
func (s QueueStarter) Prefetch(ctx context.Context, track music.Track) error {
	if ctx == nil || s.OptionsFor == nil || track.Provider != music.ProviderYouTube {
		return ErrInvalid
	}
	o, err := s.OptionsFor(ctx, track)
	if err != nil {
		return err
	}
	prefetcher, ok := o.PCM.(interface {
		Prefetch(context.Context, string) error
	})
	if !ok {
		return nil
	}
	return prefetcher.Prefetch(ctx, track.CanonicalID)
}

// Start owns every resource through publisher completion.
func Start(ctx context.Context, o Options) (*Session, error) {
	if ctx == nil {
		return nil, ErrInvalid
	}
	provider := o.Provider
	if provider == "" {
		provider = music.ProviderYouTube
	}
	if o.Community.IsZero() || o.Channel.IsZero() || o.Track == "" || o.PCM == nil || o.Encoder == nil || o.Publisher == nil || o.Epochs == nil || (o.PrepareCall == nil && (o.Call.CallID.IsZero() || o.Group.IsZero() || o.Call.GroupID != o.Group)) || provider != music.ProviderYouTube {
		return nil, music.StartStage(ctx, "provider_check", ErrInvalid)
	}
	began := time.Now()
	ctx = music.WithCallJoinTrace(client.WithoutCooperativeStop(ctx))
	if !o.Group.IsZero() {
		ctx = music.WithPlaybackGroup(ctx, o.Group)
	}
	if checker, ok := o.PCM.(interface{ Check(context.Context) error }); ok {
		if err := checker.Check(ctx); err != nil {
			return nil, providerStage(ctx, "provider_check", err)
		}
	}
	_ = music.StartStage(ctx, "provider_check", nil)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pcm, err := o.PCM.Start(ctx, o.Track)
	if err == nil && pcm == nil {
		err = ErrInvalid
	}
	if err = providerStage(ctx, "provider_pcm", err); err != nil {
		return nil, err
	}
	pcm = &ownedPCM{ReadCloser: pcm}
	encoder, err := o.Encoder.Start(ctx, pcm)
	if err == nil && encoder == nil {
		err = ErrInvalid
	}
	if err = music.StartStage(ctx, "encoder", err); err != nil {
		_ = pcm.Close()
		return nil, err
	}
	if o.PrepareCall != nil {
		o.Call, err = o.PrepareCall(ctx)
		if err != nil {
			_ = encoder.Close()
			_ = pcm.Close()
			return nil, err
		}
		o.Group = o.Call.GroupID
	}
	ctx = music.WithPlaybackGroup(ctx, o.Group)
	if err = ctx.Err(); err != nil {
		_ = encoder.Close()
		_ = pcm.Close()
		return nil, err
	}
	pub, err := o.Publisher.Start(ctx, o.Call, o.Group, o.Epochs, encoder)
	if err != nil {
		_ = encoder.Close()
		_ = pcm.Close()
		return nil, err
	}
	_ = music.StartStage(ctx, "published", nil)
	music.MarkFirstFrame(ctx, time.Since(began))
	s := &Session{startAtMS: music.StartAtMS(ctx), pcm: pcm, encoder: encoder, publisher: pub, done: make(chan struct{}), stop: make(chan struct{})}
	go s.monitor(ctx)
	return s, nil
}

type Session struct {
	pcm        io.ReadCloser
	encoder    Encoder
	publisher  Publisher
	done       chan struct{}
	stop       chan struct{}
	once       sync.Once
	mu         sync.RWMutex
	err        error
	paused     bool
	sourceGone bool
	startAtMS  int64
	resumedAt  time.Time
}

const sourceRecoveryWindow = 5 * time.Second

func (s *Session) monitor(ctx context.Context) {
	select {
	case <-ctx.Done():
		s.finish(ctx.Err())
	case <-s.publisher.Done():
		s.finish(s.resumeSourceError(s.publisher.Wait()))
	case <-s.encoder.Done():
		s.mu.Lock()
		paused := s.paused
		s.sourceGone = true
		s.mu.Unlock()
		if paused { // Keep the published silence until Resume or the pause bound closes it.
			select {
			case <-ctx.Done():
				s.finish(ctx.Err())
			case <-s.publisher.Done():
				s.finish(s.publisher.Wait())
			case <-s.stop:
			}
		} else {
			s.finish(s.resumeSourceError(s.encoder.Wait()))
		}
	case <-s.stop:
	}
	close(s.done)
}

// A blocked source can reveal its failure only once reads resume. The publisher
// can observe that encoder failure first, so both terminal paths classify it.
// Offer one cold replacement; a new session has no resume window.
func (s *Session) resumeSourceError(err error) error {
	if err == nil {
		return nil
	}
	select {
	case <-s.encoder.Done():
	default:
		return err
	}
	sourceErr := s.encoder.Wait()
	if sourceErr == nil || !errors.Is(err, sourceErr) ||
		errors.Is(sourceErr, context.Canceled) || errors.Is(sourceErr, context.DeadlineExceeded) {
		return err
	}
	s.mu.RLock()
	paused, resumedAt := s.paused, s.resumedAt
	s.mu.RUnlock()
	if !paused && !resumedAt.IsZero() && time.Since(resumedAt) <= sourceRecoveryWindow {
		return errors.Join(music.ErrSourceGone, err)
	}
	return err
}

func (s *Session) finish(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		_ = s.publisher.Close()
		_ = s.encoder.Close()
		_ = s.pcm.Close()
		close(s.stop)
	})
}
func (s *Session) Done() <-chan struct{} { return s.done }
func (s *Session) Wait() error           { <-s.done; s.mu.RLock(); defer s.mu.RUnlock(); return s.err }
func (s *Session) Close() error {
	s.finish(context.Canceled)
	<-s.done
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

// ownedPCM lets encoder and session cleanup share one close without losing the
// provider's PCM format.
type ownedPCM struct {
	io.ReadCloser
	failed atomic.Bool
	once   sync.Once
	err    error
}

func (p *ownedPCM) Close() error { p.once.Do(func() { p.err = p.ReadCloser.Close() }); return p.err }

// OggOpus forwards the provider's marker that its stream is already Opus.
func (p *ownedPCM) OggOpus() bool { return isOggOpus(p.ReadCloser) }
func (p *ownedPCM) PCMFormat() musicruntime.PCMFormat {
	if formatted, ok := p.ReadCloser.(interface{ PCMFormat() musicruntime.PCMFormat }); ok {
		return formatted.PCMFormat()
	}
	return musicruntime.PCMFormat{SampleRate: youtubePCMRate, Channels: 2}
}

// providerStage retains provider-specific, bounded diagnostic classes.
func providerStage(ctx context.Context, stage string, err error) error {
	if err != nil {
		class := youtube.FailureClass(err)
		if class != "unknown" {
			err = &providerFailureClass{error: err, class: class, permanent: youtube.PermanentPlaybackFailure(err)}
		}
	}
	return music.StartStage(ctx, stage, err)
}

type providerFailureClass struct {
	error
	class     string
	permanent bool
}

func (e *providerFailureClass) Unwrap() error        { return e.error }
func (e *providerFailureClass) FailureClass() string { return e.class }

func (e *providerFailureClass) PermanentPlaybackFailure() bool { return e.permanent }

func (p *ownedPCM) Read(b []byte) (int, error) {
	n, err := p.ReadCloser.Read(b)
	if err != nil {
		p.failed.Store(true)
	}
	return n, err
}
func (p *ownedPCM) SourceGone() bool {
	if p.failed.Load() {
		return true
	}
	if src, ok := p.ReadCloser.(interface{ SourceGone() bool }); ok {
		return src.SourceGone()
	}
	return false
}
func (s *Session) Pause() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = true
	if p, ok := s.pcm.(interface{ SetPaused(bool) }); ok {
		p.SetPaused(true)
	}
	if p, ok := s.encoder.(music.Pausable); ok {
		p.Pause()
	}
}
func (s *Session) PositionMS() int64 {
	if p, ok := s.encoder.(music.Pausable); ok {
		return s.startAtMS + p.PositionMS()
	}
	return s.startAtMS
}
func (s *Session) Resume() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sourceGone {
		return music.ErrSourceGone
	}
	select {
	case <-s.encoder.Done():
		return music.ErrSourceGone
	default:
	}
	select {
	case <-s.done:
		return music.ErrSourceGone
	default:
	}
	if p, ok := s.pcm.(interface{ SourceGone() bool }); ok && p.SourceGone() {
		return music.ErrSourceGone
	}
	if p, ok := s.encoder.(music.Pausable); ok {
		if err := p.Resume(); err != nil {
			return err
		}
	}
	if p, ok := s.pcm.(interface{ SetPaused(bool) }); ok {
		p.SetPaused(false)
	}
	s.paused = false
	s.resumedAt = time.Now()
	return nil
}

var _ music.Pausable = (*Session)(nil)

func (p *ownedPCM) SetPaused(paused bool) {
	if source, ok := p.ReadCloser.(interface{ SetPaused(bool) }); ok {
		source.SetPaused(paused)
	}
}

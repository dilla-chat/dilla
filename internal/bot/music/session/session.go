// Package session composes the installation-wide Spotify slot with one
// per-call Spotify, encoder, and LiveKit playback lifecycle.
package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/livekit"
	musicruntime "github.com/jonasthim/dilla/internal/bot/music/runtime"
	"github.com/jonasthim/dilla/internal/bot/music/spotify"
	"github.com/jonasthim/dilla/internal/bot/music/youtube"
	"github.com/jonasthim/dilla/internal/id"
)

var ErrInvalid = errors.New("music session: invalid configuration")

type Lease interface{ Release() error }
type LeaseAcquire func(id.ID, id.ID) (Lease, error)
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
	Acquire     LeaseAcquire
	PCM         PCMStarter
	Encoder     EncoderStarter
	Publisher   PublisherStarter
	Epochs      livekit.EpochSource
}

// OptionsForTrack supplies call credentials, provider PCM and epoch-bound
// publisher dependencies for a queue track. Spotify options include Acquire;
// YouTube options may omit the installation-wide lease.
type OptionsForTrack func(context.Context, music.Track) (Options, error)

type explicitStartKey struct{}

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
	ctx = context.WithValue(ctx, explicitStartKey{}, true)
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
	if (track.Provider != music.ProviderSpotify && track.Provider != music.ProviderYouTube) ||
		(o.Provider != "" && o.Provider != track.Provider) {
		return nil, music.StartStage(ctx, "provider_check", ErrInvalid)
	}
	o.Provider = track.Provider
	o.Track = track.CanonicalID
	return Start(ctx, o)
}

// Start acquires the global lease before starting any Spotify audio. The
// returned session owns every resource through publisher completion.
func Start(ctx context.Context, o Options) (*Session, error) {
	if ctx == nil {
		return nil, ErrInvalid
	}
	provider := o.Provider
	if provider == "" {
		provider = music.ProviderSpotify
	}
	if o.Community.IsZero() || o.Channel.IsZero() || o.Track == "" || o.PCM == nil || o.Encoder == nil || o.Publisher == nil || o.Epochs == nil || (o.PrepareCall == nil && (o.Call.CallID.IsZero() || o.Group.IsZero() || o.Call.GroupID != o.Group)) || (provider != music.ProviderSpotify && provider != music.ProviderYouTube) || (provider == music.ProviderSpotify && o.Acquire == nil) {
		return nil, music.StartStage(ctx, "provider_check", ErrInvalid)
	}
	ctx = client.WithoutCooperativeStop(client.WithBackground(ctx))
	if !o.Group.IsZero() {
		ctx = music.WithPlaybackGroup(ctx, o.Group)
	}
	if checker, ok := o.PCM.(interface{ Check(context.Context) error }); ok {
		if err := checker.Check(ctx); err != nil {
			return nil, providerStage(ctx, "provider_check", err)
		}
	}
	_ = music.StartStage(ctx, "provider_check", nil)
	var lease Lease
	var err error
	if provider == music.ProviderSpotify {
		lease, err = o.Acquire(o.Community, o.Channel)
		if err == nil && lease == nil {
			err = ErrInvalid
		}
		if err = music.StartStage(ctx, "spotify_lease", err); err != nil {
			return nil, err
		}
	}
	failed := true
	defer func() {
		if failed {
			if lease != nil {
				_ = lease.Release()
			}
		}
	}()
	if err = ctx.Err(); err != nil {
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
	s := &Session{lease: lease, pcm: pcm, encoder: encoder, publisher: pub, done: make(chan struct{}), stop: make(chan struct{})}
	failed = false
	go s.monitor(ctx)
	return s, nil
}

type Session struct {
	pcm       io.ReadCloser
	lease     Lease
	encoder   Encoder
	publisher Publisher
	done      chan struct{}
	stop      chan struct{}
	once      sync.Once
	mu        sync.RWMutex
	err       error
}

func (s *Session) monitor(ctx context.Context) {
	select {
	case <-ctx.Done():
		s.finish(ctx.Err())
	case <-s.publisher.Done():
		s.finish(s.publisher.Wait())
	case <-s.encoder.Done():
		s.finish(s.encoder.Wait())
	case <-s.stop:
	}
	close(s.done)
}
func (s *Session) finish(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		_ = s.publisher.Close()
		_ = s.encoder.Close()
		_ = s.pcm.Close()
		if s.lease != nil {
			_ = s.lease.Release()
		}
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

// AcquireSpotify adapts the concrete global lease manager for Options.
func AcquireSpotify(m *music.SpotifyLeaseManager) LeaseAcquire {
	return func(c, ch id.ID) (Lease, error) { return m.Acquire(c, ch) }
}

// ownedPCM lets encoder and session cleanup share one close without losing the
// provider's PCM format (Spotify is 48 kHz; YouTube is 44.1 kHz).
type ownedPCM struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (p *ownedPCM) Close() error { p.once.Do(func() { p.err = p.ReadCloser.Close() }); return p.err }
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
		if spotifyClass := spotify.FailureClass(err); spotifyClass != "unknown" {
			class = spotifyClass
		}
		if class != "unknown" {
			err = &providerFailureClass{error: err, class: class, permanent: youtube.PermanentPlaybackFailure(err) || spotify.PermanentPlaybackFailure(err)}
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

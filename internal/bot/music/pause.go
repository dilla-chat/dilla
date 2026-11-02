package music

import (
	"context"
	"errors"
	"time"
)

// Pausable is a published playback whose source and playhead can be retained.
type Pausable interface {
	Pause()
	Resume() error
	PositionMS() int64
}

var ErrSourceGone = errors.New("music: paused source is gone")

type startAtKey struct{}

// WithStartAtMS carries an internal provider offset, never a persisted track field.
func WithStartAtMS(ctx context.Context, ms int64) context.Context {
	return context.WithValue(ctx, startAtKey{}, ms)
}
func StartAtMS(ctx context.Context) int64 { ms, _ := ctx.Value(startAtKey{}).(int64); return ms }

// WithPauseClock injects the pause bound's scheduler.
func WithPauseClock(after func(time.Duration, func()) Timer) SupervisorOption {
	return func(s *Supervisor) { s.pauseAfter = after }
}

const maxPausedDuration = 10 * time.Minute

func (s *Supervisor) canHoldPlaybackLocked() bool {
	_, ok := s.playback.(Pausable)
	return ok && s.state.Current != nil && s.state.Current.Provider == ProviderYouTube
}

func (s *Supervisor) playbackPositionLocked(p Pausable) int64 {
	pos := max(int64(0), p.PositionMS())
	if s.state.Current != nil {
		if d := s.state.Current.DurationMS; d > 0 && pos > d {
			pos = d
		}
	}
	return pos
}

// rollbackPauseLocked restores the pre-operation state as well as the live gate.
// A source lost during the failed write needs a replacement at the captured position.
func (s *Supervisor) rollbackPauseLocked(before State, wasPaused bool) ([]SkippedTrack, error) {
	position := s.state.PositionMS
	s.state = before
	if wasPaused || !s.pausedPlayback {
		return nil, nil
	}
	s.resumePlaybackLocked()
	s.state = before
	if s.playback != nil || s.state.Paused || s.state.Current == nil {
		return nil, nil
	}
	s.state.PositionMS, s.state.PositionAtMS = position, 0
	skipped, err := s.startLocked(s.ctx, false, nil)
	if err != nil {
		_ = s.pauseLocked(s.ctx)
	}
	s.idleCheckLocked()
	s.signalLocked()
	return skipped, err
}

func (s *Supervisor) cancelPauseLocked() {
	s.pauseGen++
	if s.pauseTimer != nil {
		s.pauseTimer.Stop()
		s.pauseTimer = nil
	}
	s.pausedPlayback = false
}
func (s *Supervisor) pausePlaybackLocked() {
	p, ok := s.playback.(Pausable)
	if !ok || !s.canHoldPlaybackLocked() {
		s.closePlaybackLocked()
		return
	}
	if s.pausedPlayback {
		return
	}
	p.Pause()
	s.state.PositionMS = s.playbackPositionLocked(p)
	s.state.PositionAtMS = 0
	s.pausedPlayback = true
	s.pauseGen++
	gen := s.pauseGen
	after := s.pauseAfter
	if after == nil {
		after = s.afterFunc
	}
	s.pauseTimer = after(maxPausedDuration, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed || !s.pausedPlayback || gen != s.pauseGen {
			return
		}
		s.closePlaybackLocked()
		s.idleCheckLocked()
		s.signalLocked()
	})
}
func (s *Supervisor) resumePlaybackLocked() {
	if !s.pausedPlayback {
		return
	}
	p, ok := s.playback.(Pausable)
	if !ok || p.Resume() != nil {
		s.closePlaybackLocked()
		return
	}
	s.cancelPauseLocked()
	s.state.PositionAtMS = time.Now().UnixMilli()
	s.idleCheckLocked()
}

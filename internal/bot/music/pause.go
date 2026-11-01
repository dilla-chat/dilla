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
	if !ok {
		s.closePlaybackLocked()
		return
	}
	if s.pausedPlayback {
		return
	}
	p.Pause()
	s.state.PositionMS = p.PositionMS()
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

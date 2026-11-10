package music

import (
	"context"
	"time"
)

// PrefetchLead is how long before the current track ends the next one is
// pre-resolved and pre-buffered: at most this early, never earlier.
const PrefetchLead = 30 * time.Second

// prefetchSlack absorbs the drift between the playhead and the timer.
const prefetchSlack = 2 * time.Second

// Prefetcher is an optional TrackStarter capability. Prefetch resolves track
// and starts buffering its audio, so the Start that follows finds it ready.
// It must be cheap to abandon: nothing is published and the buffer is bounded.
type Prefetcher interface {
	Prefetch(context.Context, Track) error
}

// WithPrefetch injects the scheduler of the pre-buffer timer.
func WithPrefetch(after func(time.Duration, func()) Timer) SupervisorOption {
	return func(s *Supervisor) { s.prefetchAfter = after }
}

type prefetchPlan struct {
	timer    Timer
	playback Playback
	current  string
	next     string
	fireAt   time.Time
	fired    bool
}

// nextTrackLocked is the track that will start when the current one ends.
func (s *Supervisor) nextTrackLocked() *Track {
	if s.state.Current == nil {
		return nil
	}
	if s.state.Repeat == RepeatCurrent {
		track := *s.state.Current
		return &track
	}
	if len(s.state.Queue) > 0 {
		track := s.state.Queue[0]
		return &track
	}
	if s.state.Repeat == RepeatQueue {
		track := *s.state.Current
		return &track
	}
	return nil
}

func (s *Supervisor) cancelPrefetchLocked() {
	if s.prefetch != nil && s.prefetch.timer != nil {
		s.prefetch.timer.Stop()
	}
	s.prefetch = nil
}

// prefetchCheckLocked keeps exactly one pre-buffer timer aligned with the
// playhead: 30 s before the playing track ends, for the track queued next. It
// runs after every state change, so a pause, skip, seek or a new successor
// replaces or cancels the timer.
func (s *Supervisor) prefetchCheckLocked() {
	prefetcher, ok := s.starter.(Prefetcher)
	if !ok || s.closed || s.playback == nil || s.pausedPlayback || s.state.Paused || s.state.Current == nil || s.state.Current.DurationMS <= 0 {
		s.cancelPrefetchLocked()
		return
	}
	next := s.nextTrackLocked()
	if next == nil {
		s.cancelPrefetchLocked()
		return
	}
	now := time.Now()
	remaining := time.Duration(s.state.Current.DurationMS-PlayheadMS(s.state, now.UnixMilli())) * time.Millisecond
	delay := max(remaining-PrefetchLead, 0)
	fireAt := now.Add(delay)
	if plan := s.prefetch; plan != nil && plan.playback == s.playback && plan.current == s.state.Current.ID && plan.next == next.ID {
		if plan.fired || absDuration(fireAt.Sub(plan.fireAt)) <= prefetchSlack {
			return
		}
	}
	s.cancelPrefetchLocked()
	plan := &prefetchPlan{playback: s.playback, current: s.state.Current.ID, next: next.ID, fireAt: fireAt}
	s.prefetch = plan
	after := s.prefetchAfter
	if after == nil {
		after = func(d time.Duration, fn func()) Timer { return time.AfterFunc(d, fn) }
	}
	target := *next
	plan.timer = after(delay, func() {
		s.mu.Lock()
		if s.closed || s.prefetch != plan || plan.fired || s.playback != plan.playback || s.pausedPlayback || s.state.Paused {
			s.mu.Unlock()
			return
		}
		plan.fired = true
		ctx := s.ctx
		s.mu.Unlock()
		// A pre-buffer that fails costs nothing: the track then starts cold.
		_ = prefetcher.Prefetch(ctx, target)
	})
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

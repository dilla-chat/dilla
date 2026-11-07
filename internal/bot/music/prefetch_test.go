package music_test

import (
	"context"
	"sync"
	"testing"
	"time"

	music "github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/native"
)

// prefetchStarter records which tracks the supervisor asked it to pre-buffer.
type prefetchStarter struct {
	idleStarter
	pmu        sync.Mutex
	prefetched []string
}

func (s *prefetchStarter) Prefetch(_ context.Context, t music.Track) error {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.prefetched = append(s.prefetched, t.ID)
	return nil
}

func (s *prefetchStarter) prefetchedSnapshot() []string {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return append([]string(nil), s.prefetched...)
}

func newPrefetchSupervisor(t *testing.T, starter music.TrackStarter, options ...music.SupervisorOption) *music.Supervisor {
	t.Helper()
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	s, err := music.NewSupervisor(context.Background(), tid(4), supervisorStore{store}, starter, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func youtubeTrack(id, canonical string, duration time.Duration) music.Track {
	return music.Track{ID: id, Provider: music.ProviderYouTube, CanonicalID: canonical, Title: id, RequesterID: "member", DurationMS: duration.Milliseconds()}
}

func TestNextTrackPrebufferedNoEarlierThan30sBeforeEnd(t *testing.T) {
	starter := &prefetchStarter{}
	var timers []*fakeTimer
	var delays []time.Duration
	s := newPrefetchSupervisor(t, starter, music.WithPrefetch(func(d time.Duration, fn func()) music.Timer {
		timer := &fakeTimer{fn: fn}
		timers, delays = append(timers, timer), append(delays, d)
		return timer
	}))
	first := youtubeTrack("a", "abcdefghijk", 200*time.Second)
	second := youtubeTrack("b", "bcdefghijkl", 180*time.Second)
	state, err := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{first, second}})
	if err != nil {
		t.Fatal(err)
	}
	if len(timers) != 0 {
		t.Fatal("a paused queue scheduled a pre-buffer")
	}
	if _, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	if len(timers) == 0 {
		t.Fatal("playing a track with a queued successor scheduled no pre-buffer")
	}
	last := len(timers) - 1
	// 200 s track at 0 s: the lead is 30 s, so the pre-buffer fires 170 s in, never sooner.
	if delays[last] > 170*time.Second || delays[last] < 169*time.Second {
		t.Fatalf("pre-buffer delay = %v, want 170 s (30 s before a 200 s track ends)", delays[last])
	}
	if got := starter.prefetchedSnapshot(); len(got) != 0 {
		t.Fatalf("pre-buffered %v before the 30 s lead", got)
	}
	timers[last].Fire()
	if got := starter.prefetchedSnapshot(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("pre-buffered %v at the lead, want only the next queued track", got)
	}
}

func TestShortTrackPrebuffersTheNextTrackImmediately(t *testing.T) {
	starter := &prefetchStarter{}
	var delays []time.Duration
	var timers []*fakeTimer
	s := newPrefetchSupervisor(t, starter, music.WithPrefetch(func(d time.Duration, fn func()) music.Timer {
		timer := &fakeTimer{fn: fn}
		timers, delays = append(timers, timer), append(delays, d)
		return timer
	}))
	state, err := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{
		youtubeTrack("a", "abcdefghijk", 20*time.Second), youtubeTrack("b", "bcdefghijkl", 20*time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	if len(delays) == 0 || delays[len(delays)-1] != 0 {
		t.Fatalf("a 20 s track must pre-buffer at once, delays=%v", delays)
	}
}

func TestPauseCancelsThePrebufferAndNothingIsQueuedMeansNothingToPrebuffer(t *testing.T) {
	starter := &prefetchStarter{}
	var timers []*fakeTimer
	s := newPrefetchSupervisor(t, starter, music.WithPrefetch(func(d time.Duration, fn func()) music.Timer {
		timer := &fakeTimer{fn: fn}
		timers = append(timers, timer)
		return timer
	}))
	state, err := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{
		youtubeTrack("a", "abcdefghijk", 200*time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	if state, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	if len(timers) != 0 {
		t.Fatal("the last track in the queue scheduled a pre-buffer of a successor that does not exist")
	}
	more, err := s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{
		youtubeTrack("b", "bcdefghijkl", 200*time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(timers) == 0 {
		t.Fatal("queueing a successor behind a playing track scheduled no pre-buffer")
	}
	if _, err = s.Apply(context.Background(), more.Revision, music.Operation{Kind: music.OpStop}); err != nil {
		t.Fatal(err)
	}
	for _, timer := range timers {
		timer.Fire()
	}
	if got := starter.prefetchedSnapshot(); len(got) != 0 {
		t.Fatalf("a stopped queue pre-buffered %v", got)
	}
}

func TestStaysJoinedWhileQueueNonEmpty(t *testing.T) {
	starter := &prefetchStarter{}
	var idle []*fakeTimer
	var idleMu sync.Mutex
	s := newPrefetchSupervisor(t, starter,
		music.WithPrefetch(func(time.Duration, func()) music.Timer { return &fakeTimer{} }),
		music.WithIdleLeave(func() time.Duration { return time.Minute }, func(d time.Duration, fn func()) music.Timer {
			idleMu.Lock()
			defer idleMu.Unlock()
			timer := &fakeTimer{fn: fn}
			idle = append(idle, timer)
			return timer
		}))
	state, err := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{
		youtubeTrack("a", "abcdefghijk", 5*time.Second), youtubeTrack("b", "bcdefghijkl", 5*time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	idleMu.Lock()
	for _, timer := range idle {
		timer.stopped = true // the pre-resume idle timer is cancelled by playing
	}
	before := len(idle)
	idleMu.Unlock()
	// Track A ends: B must start with no leave, no idle timer, no change of room.
	starter.current.Close()
	deadline := time.After(2 * time.Second)
	for len(starter.startsSnapshot()) < 2 {
		select {
		case <-deadline:
			t.Fatal("the queue did not advance to the next track")
		case <-time.After(time.Millisecond):
		}
	}
	idleMu.Lock()
	armed := len(idle) - before
	idleMu.Unlock()
	if starter.left != 0 || armed != 0 {
		t.Fatalf("left voice %d times and armed %d idle timers between two queued tracks, want none", starter.left, armed)
	}
	if starter.closed.Load() != 0 {
		t.Fatal("the retained connection was closed between tracks")
	}
	// Only the end of the queue starts the idle period.
	starter.current.Close()
	deadline = time.After(2 * time.Second)
	for {
		idleMu.Lock()
		n := len(idle) - before
		idleMu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("an empty queue never armed the idle-leave timer")
		case <-time.After(time.Millisecond):
		}
	}
}

package music_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/native"
)

type warmPlayback struct {
	supervisorPlayback
	pauses, resumes int
	gone            bool
	position        int64
	paused          bool
	waitGate        chan struct{}
	waiting         chan struct{}
}

func (p *warmPlayback) Pause() { p.pauses++; p.paused = true }
func (p *warmPlayback) Resume() error {
	p.resumes++
	if p.gone {
		return music.ErrSourceGone
	}
	p.paused = false
	return nil
}
func (p *warmPlayback) PositionMS() int64 {
	if p.position != 0 {
		return p.position
	}
	return 42000
}
func (p *warmPlayback) Wait() error {
	if p.waitGate != nil {
		close(p.waiting)
		<-p.waitGate
	}
	return p.err
}

type warmStarter struct {
	starts  int
	offsets []int64
	current *warmPlayback
	left    int
}

func (s *warmStarter) Start(ctx context.Context, _ music.Track) (music.Playback, error) {
	s.starts++
	s.offsets = append(s.offsets, music.StartAtMS(ctx))
	s.current = &warmPlayback{supervisorPlayback: supervisorPlayback{done: make(chan struct{})}}
	return s.current, nil
}
func (s *warmStarter) LeaveIdle() error { s.left++; return nil }

type pauseClock struct {
	now    time.Duration
	timers []*pauseTimer
}
type pauseTimer struct {
	mu      sync.Mutex
	at      time.Duration
	fn      func()
	stopped bool
}

func (x *pauseTimer) Stop() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	was := !x.stopped
	x.stopped = true
	return was
}
func (c *pauseClock) after(d time.Duration, fn func()) music.Timer {
	x := &pauseTimer{at: c.now + d, fn: fn}
	c.timers = append(c.timers, x)
	return x
}
func (c *pauseClock) advance(d time.Duration) {
	c.now += d
	for _, x := range c.timers {
		if x.at <= c.now && x.Stop() {
			x.fn()
		}
	}
}
func warmFixture(t *testing.T, extra ...music.SupervisorOption) (*music.Supervisor, *warmStarter, supervisorStore, *pauseClock) {
	t.Helper()
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	st := &warmStarter{}
	clock := &pauseClock{}
	opts := append([]music.SupervisorOption{music.WithPauseClock(clock.after)}, extra...)
	s, err := music.NewSupervisor(t.Context(), tid(4), supervisorStore{store}, st, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	state, err := s.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	return s, st, supervisorStore{store}, clock
}
func persistWarmResume(t *testing.T, s *music.Supervisor, store supervisorStore) {
	t.Helper()
	state, err := music.Apply(s.State(), s.State().Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.Save(t.Context(), tid(4), s.State().Revision, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SchedulePersistedSnapshot(t.Context(), state, revision, true); err != nil {
		t.Fatal(err)
	}
}
func TestPauseKeepsThePlaybackAndResumeContinuesIt(t *testing.T) {
	s, st, store, _ := warmFixture(t)
	p := st.current
	state, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause})
	if err != nil {
		t.Fatal(err)
	}
	if p.closed != 0 || p.pauses != 1 || state.PositionMS != 42000 {
		t.Fatalf("pause closed=%d pauses=%d position=%d", p.closed, p.pauses, state.PositionMS)
	}
	persistWarmResume(t, s, store)
	if st.starts != 1 || p.resumes != 1 || s.State().PositionMS != 42000 {
		t.Fatalf("starts=%d resumes=%d state=%+v", st.starts, p.resumes, s.State())
	}
}
func TestPausedPlaybackIsClosedAfterTenMinutes(t *testing.T) {
	s, st, _, clock := warmFixture(t)
	p := st.current
	if _, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	clock.advance(10*time.Minute - time.Second)
	if p.closed != 0 {
		t.Fatal("closed before ten minutes")
	}
	clock.advance(2 * time.Second)
	if p.closed != 1 {
		t.Fatalf("closed=%d", p.closed)
	}
	if _, err := s.Apply(t.Context(), s.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	if st.starts != 2 || st.offsets[1] != 42000 {
		t.Fatalf("starts=%d offsets=%v", st.starts, st.offsets)
	}
}
func TestResumeAfterSourceGoneColdStartsAtThePausedPosition(t *testing.T) {
	s, st, _, _ := warmFixture(t)
	p := st.current
	if _, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	p.gone = true
	if _, err := s.Apply(t.Context(), s.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	if p.closed != 1 || st.starts != 2 || st.offsets[1] != 42000 {
		t.Fatalf("closed=%d starts=%d offsets=%v", p.closed, st.starts, st.offsets)
	}
}
func TestQuickPauseResumePairsKeepOnePlayback(t *testing.T) {
	s, st, store, clock := warmFixture(t)
	p := st.current
	for range 2 {
		if _, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
			t.Fatal(err)
		}
		persistWarmResume(t, s, store)
	}
	// A callback already dispatched when Stop ran must not close a resumed playback.
	for _, timer := range clock.timers {
		timer.fn()
	}
	if st.starts != 1 || p.closed != 0 || p.pauses != 2 || p.resumes != 2 {
		t.Fatalf("starts=%d playback=%+v", st.starts, p)
	}
	// Race the active bound and Resume under the supervisor mutex.
	if _, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); clock.timers[len(clock.timers)-1].fn() }()
	_, err := s.Apply(t.Context(), s.State().Revision, music.Operation{Kind: music.OpResume})
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if st.starts > 2 || (st.starts == 2 && p.closed != 1) {
		t.Fatalf("overlapping playback: starts=%d old closes=%d", st.starts, p.closed)
	}
}
func TestPausedPlaybackCountsAsIdle(t *testing.T) {
	var idle func()
	s, st, _, _ := warmFixture(t, music.WithIdleLeave(func() time.Duration { return time.Minute }, func(_ time.Duration, fn func()) music.Timer { idle = fn; return &fakeTimer{fn: fn} }))
	if _, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	if idle == nil {
		t.Fatal("paused playback did not arm idle leave")
	}
	idle()
	if st.left != 1 || st.current.closed != 1 {
		t.Fatalf("idle leave=%d playback closes=%d", st.left, st.current.closed)
	}
}
func TestPersistedSnapshotResumeAfterRestartStartsAtPositionMS(t *testing.T) {
	s, st, store, _ := warmFixture(t)
	if _, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	restarted, err := music.NewSupervisor(t.Context(), tid(4), store, st)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	state, err := music.Apply(restarted.State(), restarted.State().Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := store.Save(t.Context(), tid(4), restarted.State().Revision, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.ApplyPersistedSnapshot(t.Context(), state, rev, true); err != nil {
		t.Fatal(err)
	}
	if st.starts != 2 || st.offsets[1] != 42000 {
		t.Fatalf("offsets=%v", st.offsets)
	}
}

func TestPauseDuringStartStillCloses(t *testing.T) {
	starter := &reviewLiveStarter{entered: make(chan struct{}), release: make(chan struct{})}
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	s, err := music.NewSupervisor(t.Context(), tid(4), supervisorStore{store}, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, err := s.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "m"}}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume})
		done <- err
	}()
	<-starter.entered
	if _, err = s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	close(starter.release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	starter.mu.Lock()
	defer starter.mu.Unlock()
	if len(starter.playbacks) != 0 || !s.State().Paused {
		t.Fatal("half-started playback survived pause")
	}
}

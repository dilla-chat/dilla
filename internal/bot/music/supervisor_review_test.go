package music_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/music/spotify"
	"github.com/jonasthim/dilla/internal/bot/music/youtube"
	"github.com/jonasthim/dilla/internal/bot/native"
)

type reviewPermanentFailure struct{}

func (reviewPermanentFailure) Error() string                  { return "invalid track" }
func (reviewPermanentFailure) PermanentPlaybackFailure() bool { return true }

type reviewStarter struct {
	mu               sync.Mutex
	calls            int
	failure          error
	entered, release chan struct{}
}

func (r *reviewStarter) Start(ctx context.Context, _ music.Track) (music.Playback, error) {
	r.mu.Lock()
	r.calls++
	first := r.calls == 1
	r.mu.Unlock()
	if first && r.entered != nil {
		close(r.entered)
		<-r.release
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, r.failure
}
func reviewSupervisor(t *testing.T, count int, starter music.TrackStarter, options ...music.SupervisorOption) (*music.Supervisor, music.State) {
	t.Helper()
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	s, err := music.NewSupervisor(t.Context(), tid(4), supervisorStore{store}, starter, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	tracks := make([]music.Track, count)
	for i := range tracks {
		tracks[i] = music.Track{ID: fmt.Sprint(i), Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "private", RequesterID: "member"}
	}
	state, err := s.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
	if err != nil {
		t.Fatal(err)
	}
	return s, state
}
func TestSupervisorTransientFailureKeepsFiveHundredItems(t *testing.T) {
	starter := &reviewStarter{failure: &music.StartError{Stage: "provider_pcm", Cause: youtube.ErrSandboxUnavailable}}
	s, state := reviewSupervisor(t, 500, starter)
	got, err := s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume})
	if !errors.Is(err, youtube.ErrSandboxUnavailable) || got.Current == nil || got.Current.ID != "0" || len(got.Queue) != 499 || !got.Paused || starter.calls != 1 {
		t.Fatalf("state=%+v calls=%d error=%v", got, starter.calls, err)
	}
}
func TestSupervisorNotConfiguredSpotifySkipsThenCaps(t *testing.T) {
	starter := &reviewStarter{failure: &music.StartError{Stage: "provider_check", Cause: spotify.ErrProviderUnavailable}}
	s, state := reviewSupervisor(t, 6, starter)
	got, err := s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil || starter.calls != 2 || got.Current == nil || got.Current.ID != "2" || len(got.Queue) != 3 || !got.Paused {
		t.Fatalf("state=%+v calls=%d error=%v", got, starter.calls, err)
	}
}
func TestSupervisorStopDoesNotWaitForProvider(t *testing.T) {
	starter := &reviewStarter{failure: &music.StartError{Stage: "provider_pcm", Cause: reviewPermanentFailure{}}, entered: make(chan struct{}), release: make(chan struct{})}
	s, state := reviewSupervisor(t, 3, starter)
	resumeDone := make(chan error, 1)
	go func() {
		_, err := s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume})
		resumeDone <- err
	}()
	<-starter.entered
	stopDone := make(chan error, 1)
	go func() {
		_, err := s.Apply(context.Background(), state.Revision+1, music.Operation{Kind: music.OpStop})
		stopDone <- err
	}()
	prompt := false
	var stopErr error
	select {
	case stopErr = <-stopDone:
		prompt = true
	case <-time.After(100 * time.Millisecond):
	}
	close(starter.release)
	<-resumeDone
	if !prompt {
		stopErr = <-stopDone
	}
	if !prompt || stopErr != nil {
		t.Fatalf("stop waited behind provider: prompt=%v error=%v", prompt, stopErr)
	}
	if got := s.State(); got.Current != nil || !got.Paused || starter.calls != 1 {
		t.Fatalf("stop state=%+v calls=%d", got, starter.calls)
	}
}
func TestSupervisorSkipNotificationsRunOutsideStateLock(t *testing.T) {
	starter := &reviewStarter{failure: &music.StartError{Stage: "provider_pcm", Cause: reviewPermanentFailure{}}}
	var s *music.Supervisor
	readable := true
	report := func(context.Context, []music.SkippedTrack) {
		done := make(chan struct{})
		go func() { _ = s.State(); close(done) }()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
			readable = false
		}
	}
	var state music.State
	s, state = reviewSupervisor(t, 1, starter, music.WithSkippedTrack(report))
	_, err := s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil || !readable {
		t.Fatalf("notice held state lock: readable=%v error=%v", readable, err)
	}
}

type reviewLiveStarter struct {
	mu               sync.Mutex
	calls            int
	entered, release chan struct{}
	playbacks        []*supervisorPlayback
}

func (r *reviewLiveStarter) Start(ctx context.Context, _ music.Track) (music.Playback, error) {
	r.mu.Lock()
	r.calls++
	first := r.calls == 1
	r.mu.Unlock()
	if first {
		close(r.entered)
		<-r.release
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := &supervisorPlayback{done: make(chan struct{})}
	r.mu.Lock()
	r.playbacks = append(r.playbacks, p)
	r.mu.Unlock()
	return p, nil
}
func (r *reviewLiveStarter) closeAll() {
	r.mu.Lock()
	ps := append([]*supervisorPlayback(nil), r.playbacks...)
	r.mu.Unlock()
	for _, p := range ps {
		_ = p.Close()
	}
}
func awaitReviewRevision(t *testing.T, s *music.Supervisor, revision uint64) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if s.State().Revision >= revision {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("concurrent operation did not commit")
}
func TestSupervisorConcurrentResumeStartsOnce(t *testing.T) {
	starter := &reviewLiveStarter{entered: make(chan struct{}), release: make(chan struct{})}
	s, state := reviewSupervisor(t, 1, starter)
	defer func() { _ = s.Close(); starter.closeAll() }()
	done := make(chan error, 2)
	go func() {
		_, err := s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume})
		done <- err
	}()
	<-starter.entered
	go func() {
		_, err := s.Apply(context.Background(), state.Revision+1, music.Operation{Kind: music.OpResume})
		done <- err
	}()
	awaitReviewRevision(t, s, state.Revision+2)
	close(starter.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if starter.calls != 1 || s.State().Paused {
		t.Fatalf("starts=%d state=%+v", starter.calls, s.State())
	}
}
func TestSupervisorCancelledStartCannotPauseNewResume(t *testing.T) {
	starter := &reviewLiveStarter{entered: make(chan struct{}), release: make(chan struct{})}
	s, state := reviewSupervisor(t, 1, starter)
	defer func() { _ = s.Close(); starter.closeAll() }()
	done := make(chan error, 2)
	go func() {
		_, err := s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume})
		done <- err
	}()
	<-starter.entered
	paused, err := s.Apply(t.Context(), state.Revision+1, music.Operation{Kind: music.OpPause})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := s.Apply(context.Background(), paused.Revision, music.Operation{Kind: music.OpResume})
		done <- err
	}()
	awaitReviewRevision(t, s, paused.Revision+1)
	close(starter.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if starter.calls != 2 || s.State().Paused {
		t.Fatalf("starts=%d state=%+v", starter.calls, s.State())
	}
}

type reviewCleanupPlayback struct {
	done, closing, release chan struct{}
	once                   sync.Once
}

func (p *reviewCleanupPlayback) Done() <-chan struct{} { return p.done }
func (p *reviewCleanupPlayback) Wait() error           { <-p.done; return nil }
func (p *reviewCleanupPlayback) Close() error {
	p.once.Do(func() { close(p.closing); <-p.release; close(p.done) })
	return nil
}

type reviewCleanupStarter struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	old     *reviewCleanupPlayback
	current *supervisorPlayback
}

func (r *reviewCleanupStarter) Start(ctx context.Context, _ music.Track) (music.Playback, error) {
	r.mu.Lock()
	r.calls++
	first := r.calls == 1
	r.mu.Unlock()
	if first {
		close(r.entered)
		<-ctx.Done()
		return r.old, nil
	}
	r.current = &supervisorPlayback{done: make(chan struct{})}
	return r.current, nil
}
func TestSupervisorCancelledCleanupCannotPauseNewResume(t *testing.T) {
	old := &reviewCleanupPlayback{done: make(chan struct{}), closing: make(chan struct{}), release: make(chan struct{})}
	starter := &reviewCleanupStarter{entered: make(chan struct{}), old: old}
	s, state := reviewSupervisor(t, 1, starter)
	defer func() { _ = s.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 2)
	go func() { _, err := s.Apply(ctx, state.Revision, music.Operation{Kind: music.OpResume}); done <- err }()
	<-starter.entered
	cancel()
	<-old.closing
	paused, err := s.Apply(t.Context(), state.Revision+1, music.Operation{Kind: music.OpPause})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := s.Apply(context.Background(), paused.Revision, music.Operation{Kind: music.OpResume})
		done <- err
	}()
	awaitReviewRevision(t, s, paused.Revision+1)
	close(old.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if starter.calls != 2 || s.State().Paused {
		t.Fatalf("starts=%d state=%+v", starter.calls, s.State())
	}
}

type chainStart struct {
	track    music.Track
	playback *supervisorPlayback
}

type chainStarter struct {
	failed  map[string]bool
	started chan chainStart
}

func (s *chainStarter) Start(_ context.Context, track music.Track) (music.Playback, error) {
	if s.failed[track.ID] {
		return nil, &music.StartError{Stage: "provider_pcm", Cause: reviewPermanentFailure{}}
	}
	p := &supervisorPlayback{done: make(chan struct{})}
	s.started <- chainStart{track: track, playback: p}
	return p, nil
}

func TestSupervisorAutomaticStartChainCapsFiveTotalSkipsAcrossSuccesses(t *testing.T) {
	starter := &chainStarter{failed: map[string]bool{"0": true, "2": true, "4": true, "6": true, "8": true}, started: make(chan chainStart, 12)}
	s, state := reviewSupervisor(t, 12, starter)
	if _, err := s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1", "3", "5", "7"} {
		select {
		case start := <-starter.started:
			if start.track.ID != want {
				t.Fatalf("started %s, want %s", start.track.ID, want)
			}
			close(start.playback.done)
		case <-time.After(time.Second):
			t.Fatalf("did not start %s: state=%+v", want, s.State())
		}
	}
	deadline := time.Now().Add(time.Second)
	for !s.State().Paused && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := s.State()
	if !got.Paused || got.Current == nil || got.Current.ID != "9" || len(got.Queue) != 2 {
		t.Fatalf("fifth total skip did not pause chain: %+v", got)
	}
	select {
	case start := <-starter.started:
		t.Fatalf("started %s beyond total skip cap", start.track.ID)
	default:
	}
	// A new explicit resume gets a fresh budget, even on the same current item.
	if _, err := s.Apply(t.Context(), got.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	select {
	case start := <-starter.started:
		if start.track.ID != "9" {
			t.Fatalf("fresh resume started %s, want 9", start.track.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("explicit resume retained exhausted chain budget")
	}
}

func TestSupervisorAutomaticStartChainStillCapsTwoConsecutiveSkips(t *testing.T) {
	starter := &chainStarter{failed: map[string]bool{"0": true, "2": true, "3": true}, started: make(chan chainStart, 6)}
	s, state := reviewSupervisor(t, 6, starter)
	if _, err := s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	select {
	case start := <-starter.started:
		if start.track.ID != "1" {
			t.Fatalf("started %s, want 1", start.track.ID)
		}
		close(start.playback.done)
	case <-time.After(time.Second):
		t.Fatal("first successful playback did not start")
	}
	deadline := time.Now().Add(time.Second)
	for !s.State().Paused && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := s.State()
	if !got.Paused || got.Current == nil || got.Current.ID != "4" || len(got.Queue) != 1 {
		t.Fatalf("consecutive skip cap after successful playback: %+v", got)
	}
}

package music_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	music "github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/native"
)

// gatedPlayback models the pool's draining slot (livekit/draining.go): each started track holds its first sample
// until the playback started before it is done, then sends that sample at once, like the publisher's write loop does
// when its HoldFirstSample gate opens. A stop ends the hold. Closing a playback returns only once the track started
// after it has acted on the opened gate (sent its first sample, or seen its own stop), so a successor that is still
// live when its predecessor closes always leaks its sample, deterministically.
type gatedPlayback struct {
	starter                       *gatedStarter
	index, samples, tail          int
	gate, inputDone, done, stop   chan struct{}
	settled, resumed              chan struct{}
	stopOnce, doneOnce, settleOnc sync.Once
	pauseMu                       sync.Mutex
	paused                        bool
}

func (p *gatedPlayback) finish()                    { p.doneOnce.Do(func() { close(p.done) }) }
func (p *gatedPlayback) settle()                    { p.settleOnc.Do(func() { close(p.settled) }) }
func (p *gatedPlayback) Done() <-chan struct{}      { return p.done }
func (p *gatedPlayback) Wait() error                { <-p.done; return nil }
func (p *gatedPlayback) InputDone() <-chan struct{} { return p.inputDone }
func (p *gatedPlayback) InputErr() error            { return nil }
func (p *gatedPlayback) PositionMS() int64          { return 0 }
func (p *gatedPlayback) Pause() {
	p.pauseMu.Lock()
	defer p.pauseMu.Unlock()
	if !p.paused {
		p.paused, p.resumed = true, make(chan struct{})
	}
}
func (p *gatedPlayback) Resume() error {
	p.pauseMu.Lock()
	defer p.pauseMu.Unlock()
	if p.paused {
		p.paused = false
		close(p.resumed)
	}
	return nil
}

// holdWhilePaused blocks while the playback is paused; it reports false on a stop.
func (p *gatedPlayback) holdWhilePaused() bool {
	for {
		p.pauseMu.Lock()
		paused, resumed := p.paused, p.resumed
		p.pauseMu.Unlock()
		if !paused {
			return true
		}
		p.settle() // a held successor acted on its gate too
		select {
		case <-resumed:
		case <-p.stop:
			return false
		}
	}
}

func (p *gatedPlayback) Close() error {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
	if next := p.starter.after(p); next != nil {
		<-next.settled
	}
	return nil
}

func (p *gatedPlayback) run() {
	defer p.finish()
	defer p.settle()
	select {
	case <-p.gate:
	case <-p.stop:
		return
	}
	for i := range p.samples {
		select {
		case <-p.stop:
			return
		default:
		}
		if !p.holdWhilePaused() {
			return
		}
		p.starter.stamp(p.index)
		p.settle()
		if p.samples-i-1 == p.tail {
			close(p.inputDone) // the encoder wrote its last packet; tail packets are still queued
		}
		select {
		case <-p.stop:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type gatedSample struct {
	track int
	at    time.Time
}

type gatedStarter struct {
	mu      sync.Mutex
	starts  []*gatedPlayback
	samples []gatedSample
}

func (s *gatedStarter) stamp(track int) {
	s.mu.Lock()
	s.samples = append(s.samples, gatedSample{track, time.Now()})
	s.mu.Unlock()
}

func (s *gatedStarter) after(p *gatedPlayback) *gatedPlayback {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.index+1 < len(s.starts) {
		return s.starts[p.index+1]
	}
	return nil
}

func (s *gatedStarter) Start(ctx context.Context, _ music.Track) (music.Playback, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	gate := make(chan struct{})
	if n := len(s.starts); n > 0 {
		gate = s.starts[n-1].done
	} else {
		close(gate)
	}
	p := &gatedPlayback{
		starter: s, index: len(s.starts), samples: 100, tail: 50, gate: gate,
		inputDone: make(chan struct{}), done: make(chan struct{}), stop: make(chan struct{}), settled: make(chan struct{}),
	}
	s.starts = append(s.starts, p)
	s.mu.Unlock()
	go p.run()
	return p, nil
}

func (s *gatedStarter) snapshot() (starts int, samples []gatedSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.starts), append([]gatedSample(nil), s.samples...)
}

// drainingFixture starts three 2 s tracks and returns 1.5 s in: the first track plays its queued tail in the draining
// slot and the second is started, holding its first sample.
func drainingFixture(t *testing.T) (*music.Supervisor, *gatedStarter, uint64) {
	t.Helper()
	starter := &gatedStarter{}
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	s, err := music.NewSupervisor(t.Context(), tid(4), supervisorStore{store}, starter)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	tracks := []music.Track{
		{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", RequesterID: "member"},
		{ID: "b", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", RequesterID: "member"},
		{ID: "c", Provider: music.ProviderYouTube, CanonicalID: "cdefghijklm", RequesterID: "member"},
	}
	state, err := s.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	synctest.Wait()
	starts, samples := starter.snapshot()
	if starts != 2 {
		t.Fatalf("starts 1.5 s in = %d, want 2 (the second track started beside the first one's tail)", starts)
	}
	for _, sample := range samples {
		if sample.track != 0 {
			t.Fatalf("track %d sent a sample while the first one still drained", sample.track)
		}
	}
	return s, starter, s.State().Revision
}

func samplesOf(samples []gatedSample, track int, since time.Time) int {
	n := 0
	for _, sample := range samples {
		if sample.track == track && !sample.at.Before(since) {
			n++
		}
	}
	return n
}

func TestStopDuringDrainPublishesNothingOfTheNextTrack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, starter, revision := drainingFixture(t)
		stoppedAt := time.Now()
		if _, err := s.Apply(t.Context(), revision, music.Operation{Kind: music.OpStop}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		starts, samples := starter.snapshot()
		if n := samplesOf(samples, 1, time.Time{}); n != 0 {
			t.Fatalf("the next track sent %d samples after /stop during the drain, want none", n)
		}
		if n := samplesOf(samples, 0, stoppedAt.Add(time.Nanosecond)); n != 0 {
			t.Fatalf("the draining track sent %d samples after /stop, want its tail to stop at once", n)
		}
		if starts != 2 {
			t.Fatalf("starts after /stop = %d, want 2", starts)
		}
	})
}

func TestSkipDuringDrainStartsTheNextTrackAndEndsTheTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, starter, revision := drainingFixture(t)
		skippedAt := time.Now()
		if _, err := s.Apply(t.Context(), revision, music.Operation{Kind: music.OpSkip}); err != nil {
			t.Fatal(err)
		}
		if cur := s.State().Current; cur == nil || cur.ID != "c" {
			t.Fatalf("current after /skip = %+v, want c", cur)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		starts, samples := starter.snapshot()
		if n := samplesOf(samples, 1, time.Time{}); n != 0 {
			t.Fatalf("the skipped track sent %d samples, want none", n)
		}
		if n := samplesOf(samples, 0, skippedAt.Add(time.Nanosecond)); n != 0 {
			t.Fatalf("the draining track sent %d samples after /skip, want its tail to stop at once", n)
		}
		if starts != 3 {
			t.Fatalf("starts after /skip = %d, want 3 (the track after the skipped one)", starts)
		}
		if n := samplesOf(samples, 2, skippedAt); n == 0 {
			t.Fatal("the track after the skipped one sent no samples within a second of /skip")
		}
	})
}

func TestPauseDuringDrainHoldsTheNextTrack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, starter, revision := drainingFixture(t)
		pausedAt := time.Now()
		if _, err := s.Apply(t.Context(), revision, music.Operation{Kind: music.OpPause}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		_, samples := starter.snapshot()
		if n := samplesOf(samples, 1, time.Time{}); n != 0 {
			t.Fatalf("the next track sent %d samples after /pause during the drain, want none", n)
		}
		if n := samplesOf(samples, 0, pausedAt.Add(time.Nanosecond)); n != 0 {
			t.Fatalf("the draining track sent %d samples after /pause, want its tail to stop at once", n)
		}
	})
}

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
	"github.com/dilla-chat/dilla/internal/id"
)

// drainSpeaker is a fake publisher: it plays the started tracks one after the other, one 20 ms sample at a time, and
// stamps each sample. A track's next sample waits for the one before it, so tracks never overlap; a track that is not
// started by the time the one before it drained leaves a gap in the stamps.
type drainSpeaker struct {
	mu     sync.Mutex
	stamps []time.Time
	queue  chan *drainPlayback
}

func (s *drainSpeaker) run() {
	for p := range s.queue {
		for i := range p.samples {
			select {
			case <-p.stop:
			case <-time.After(20 * time.Millisecond):
			}
			select {
			case <-p.stop:
				p.finish()
				goto next
			default:
			}
			s.mu.Lock()
			s.stamps = append(s.stamps, time.Now())
			s.mu.Unlock()
			if p.samples-i-1 == p.tail {
				close(p.inputDone) // the encoder wrote its last packet; tail packets are still queued
			}
		}
		p.finish()
	next:
	}
}

type drainPlayback struct {
	samples, tail int
	inputDone     chan struct{}
	done          chan struct{}
	stop          chan struct{}
	stopOnce      sync.Once
	doneOnce      sync.Once
}

func (p *drainPlayback) finish()                    { p.doneOnce.Do(func() { close(p.done) }) }
func (p *drainPlayback) Done() <-chan struct{}      { return p.done }
func (p *drainPlayback) Wait() error                { <-p.done; return nil }
func (p *drainPlayback) InputDone() <-chan struct{} { return p.inputDone }
func (p *drainPlayback) InputErr() error            { return nil }
func (p *drainPlayback) Close() error {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
	return nil
}

type drainStarter struct {
	speaker *drainSpeaker
	mu      sync.Mutex
	starts  []*drainPlayback
	// beforeDone records, for each start after the first, whether the playback before it was still playing its tail.
	beforeDone []bool
}

func (s *drainStarter) Start(ctx context.Context, _ music.Track) (music.Playback, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := &drainPlayback{samples: 100, tail: 50, inputDone: make(chan struct{}), done: make(chan struct{}), stop: make(chan struct{})}
	s.mu.Lock()
	if n := len(s.starts); n > 0 {
		select {
		case <-s.starts[n-1].done:
			s.beforeDone = append(s.beforeDone, false)
		default:
			s.beforeDone = append(s.beforeDone, true)
		}
	}
	s.starts = append(s.starts, p)
	s.mu.Unlock()
	s.speaker.queue <- p
	return p, nil
}

// slowSaveStore's durable write takes saveTime: CI's 410 ms helper Save on the switch.
type slowSaveStore struct {
	supervisorStore
	saveTime time.Duration
}

func (s slowSaveStore) Save(ctx context.Context, channel id.ID, expected uint64, state music.State) (uint64, error) {
	time.Sleep(s.saveTime)
	return s.supervisorStore.Save(ctx, channel, expected, state)
}

func TestAdvanceStartsOnInputEOF(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		speaker := &drainSpeaker{queue: make(chan *drainPlayback, 4)}
		go speaker.run()
		defer close(speaker.queue)
		starter := &drainStarter{speaker: speaker}
		n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
		store, err := persistence.New(n, tid(2))
		if err != nil {
			t.Fatal(err)
		}
		s, err := music.NewSupervisor(t.Context(), tid(4), slowSaveStore{supervisorStore{store}, 400 * time.Millisecond}, starter)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		tracks := []music.Track{
			{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", RequesterID: "member"},
			{ID: "b", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", RequesterID: "member"},
		}
		state, err := s.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Second) // both 2 s tracks, the slow saves and any stray start
		synctest.Wait()

		starter.mu.Lock()
		starts, beforeDone := len(starter.starts), append([]bool(nil), starter.beforeDone...)
		starter.mu.Unlock()
		if starts != 2 {
			t.Fatalf("starts = %d, want 2 (the last item ends with no extra start)", starts)
		}
		if !beforeDone[0] {
			t.Fatal("the next track started only after the previous playback's Done, not on its input EOF")
		}
		speaker.mu.Lock()
		stamps := append([]time.Time(nil), speaker.stamps...)
		speaker.mu.Unlock()
		if len(stamps) != 200 {
			t.Fatalf("samples played = %d, want both tracks' 200 (every track plays to its end)", len(stamps))
		}
		var worst time.Duration
		for i := 1; i < len(stamps); i++ {
			worst = max(worst, stamps[i].Sub(stamps[i-1]))
		}
		if worst > 40*time.Millisecond {
			t.Fatalf("the widest step between samples across the switch = %v, want at most 40 ms", worst)
		}
		if cur := s.State().Current; cur != nil {
			t.Fatalf("current after the last item ended = %q, want none", cur.ID)
		}
	})
}

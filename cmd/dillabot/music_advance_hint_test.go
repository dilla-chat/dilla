//go:build linux

package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

// hintPlayback ends when the test says so.
type hintPlayback struct{ done chan struct{} }

func (p *hintPlayback) Done() <-chan struct{} { return p.done }
func (p *hintPlayback) Wait() error           { return nil }
func (p *hintPlayback) Close() error {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	return nil
}

// heldStarter starts any track; a start of an item in hold waits for its release.
type heldStarter struct {
	mu        sync.Mutex
	playbacks map[string]*hintPlayback
	hold      map[string]chan struct{}
	entered   chan string
}

func (s *heldStarter) Start(_ context.Context, t music.Track) (music.Playback, error) {
	if release, ok := s.hold[t.ID]; ok {
		s.entered <- t.ID
		<-release
	}
	p := &hintPlayback{done: make(chan struct{})}
	s.mu.Lock()
	s.playbacks[t.ID] = p
	s.mu.Unlock()
	return p, nil
}

func (s *heldStarter) playing(item string) *hintPlayback {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.playbacks[item]
}

type neverTimer struct{}

func (neverTimer) Stop() bool { return true }

// CI run 44790568832: notifyingMusicQueue's hint after an advance's save set off the listeners' music-state reads
// inside the next track's start. The hint of an advance now waits for that start (music.DeferNotice); a revert of the
// queue's deferral fails here.
func TestTheQueuesAdvanceHintWaitsForTheNextTracksStart(t *testing.T) {
	community, channel := id.New(), id.New()
	n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()}, rows: map[string]native.StateValue{}}
	stateStore, err := persistence.New(n, community)
	if err != nil {
		t.Fatal(err)
	}
	var hints atomic.Int32
	queue := notifyingMusicQueue{persistedMusicQueue: persistedMusicQueue{store: stateStore}, changed: func(context.Context) { hints.Add(1) }}
	track := func(item string) music.Track {
		return music.Track{ID: item, Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "T", RequesterID: "member"}
	}
	seed := music.InitialState()
	first := track("first")
	seed.Current, seed.Queue, seed.Revision = &first, []music.Track{track("second")}, 1
	if _, err := queue.Save(context.Background(), channel, 0, seed); err != nil {
		t.Fatal(err)
	}
	starter := &heldStarter{playbacks: map[string]*hintPlayback{}, hold: map[string]chan struct{}{"second": make(chan struct{})}, entered: make(chan string, 1)}
	sup, err := music.NewSupervisor(t.Context(), channel, queue, starter,
		music.WithAdvanceNoticeClock(func(time.Duration, func()) music.Timer { return neverTimer{} }))
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() { close(starter.hold["second"]) })
	t.Cleanup(func() { release(); _ = sup.Close() }) // a failed test must not leave the start held
	if _, err := sup.Apply(t.Context(), sup.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	if starter.playing("first") == nil {
		t.Fatal("the first track did not start")
	}
	hints.Store(0)

	_ = starter.playing("first").Close() // the track ends
	if item := <-starter.entered; item != "second" {
		t.Fatalf("the advance started %s", item)
	}
	time.Sleep(20 * time.Millisecond) // an undeferred hint is sent synchronously by the save, before this start
	if got := hints.Load(); got != 0 {
		t.Fatalf("%d hints went out while the next track was starting, want none until it started", got)
	}
	release()
	deadline := time.Now().Add(5 * time.Second)
	for hints.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the advance's hint never went out")
		}
		time.Sleep(time.Millisecond)
	}
	if got := hints.Load(); got != 1 {
		t.Fatalf("the advance sent %d hints, want 1", got)
	}
}

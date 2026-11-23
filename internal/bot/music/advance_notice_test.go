package music_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	music "github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/id"
)

// events is the order of the starts and the state-change notices of one supervisor.
type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) add(v string) { e.mu.Lock(); e.list = append(e.list, v); e.mu.Unlock() }
func (e *events) reset()       { e.mu.Lock(); e.list = nil; e.mu.Unlock() }
func (e *events) get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.list)
}

// noticeStore notices each successful save as cmd/dillabot's notifyingMusicQueue does: held while an advance starts
// its next track.
type noticeStore struct {
	supervisorStore
	events *events
}

func (s noticeStore) Save(ctx context.Context, channel id.ID, expected uint64, state music.State) (uint64, error) {
	revision, err := s.supervisorStore.Save(ctx, channel, expected, state)
	if err == nil {
		notice := func() { s.events.add("notice") }
		if !music.DeferNotice(ctx, notice) {
			notice()
		}
	}
	return revision, err
}

// orderedStarter records "started <id>" when a start returns; a start of an item in hold waits for its release first.
type orderedStarter struct {
	*itemStarter
	events  *events
	hold    map[string]chan struct{}
	entered chan string
}

func (s orderedStarter) Start(ctx context.Context, t music.Track) (music.Playback, error) {
	if release, ok := s.hold[t.ID]; ok {
		s.entered <- t.ID
		<-release
	}
	p, err := s.itemStarter.Start(ctx, t)
	s.events.add("started " + t.ID)
	return p, err
}

// manualClock is an advance-notice clock a test fires by hand.
type manualClock struct {
	mu    sync.Mutex
	fired []func()
}

type manualTimer struct{}

func (manualTimer) Stop() bool { return true }

func (c *manualClock) after(_ time.Duration, fn func()) music.Timer {
	c.mu.Lock()
	c.fired = append(c.fired, fn)
	c.mu.Unlock()
	return manualTimer{}
}

func (c *manualClock) fire(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	fns := c.fired
	c.fired = nil
	c.mu.Unlock()
	if len(fns) == 0 {
		t.Fatal("no advance notice limit was scheduled")
	}
	for _, fn := range fns {
		fn()
	}
}

type advanceRig struct {
	sup     *music.Supervisor
	starter orderedStarter
	events  *events
	clock   *manualClock
	ctx     context.Context
}

func newAdvanceRig(t *testing.T, hold ...string) *advanceRig {
	t.Helper()
	seed := music.InitialState()
	first, second := fixtureTrack("first"), fixtureTrack("second")
	seed.Current = &first
	seed.Queue = []music.Track{second}
	r := newFailedTrackRig(t, seed)
	ev := &events{}
	starter := orderedStarter{itemStarter: newItemStarter(), events: ev, hold: map[string]chan struct{}{}, entered: make(chan string, 1)}
	for _, item := range hold {
		starter.hold[item] = make(chan struct{})
	}
	clock := &manualClock{}
	sup, err := music.NewSupervisor(r.ctx, r.channel, noticeStore{supervisorStore: r.store, events: ev}, starter, music.WithAdvanceNoticeClock(clock.after))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, release := range starter.hold {
			select {
			case <-release:
			default:
				close(release)
			}
		}
		_ = sup.Close()
	})
	if _, err := sup.Apply(r.ctx, sup.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first track", func() bool { return starter.startCount("first") == 1 })
	ev.reset()
	return &advanceRig{sup: sup, starter: starter, events: ev, clock: clock, ctx: r.ctx}
}

// CI run 44790568832: the advance's call hint went out before the next track's start, so the listeners' music-state
// reads (each a durable command open on the Bot's helper) ran inside the start and opened a 550 ms gap. The notice of
// an advance now follows the next track's start, once it has returned.
func TestAnAdvanceNoticesItsStateAfterTheNextTrackStarted(t *testing.T) {
	r := newAdvanceRig(t)
	r.starter.end(t, "first", nil)
	waitFor(t, "the advance's notice", func() bool { return slices.Contains(r.events.get(), "notice") })
	if got, want := r.events.get(), []string{"started second", "notice"}; !slices.Equal(got, want) {
		t.Fatalf("the advance's events are %v, want %v: its notice must not reach the listeners before the next track starts", got, want)
	}
}

// Ruling R36: a start that takes long (a queued YouTube item resolving, up to maxStartDuration) does not hold the
// listeners' now-playing: at the limit the notice goes out while the start goes on, and only once.
func TestASlowStartNoticesTheAdvanceAtTheLimit(t *testing.T) {
	r := newAdvanceRig(t, "second")
	r.starter.end(t, "first", nil)
	if item := <-r.starter.entered; item != "second" {
		t.Fatalf("the advance started %s", item)
	}
	if got := r.events.get(); len(got) != 0 {
		t.Fatalf("before the limit the events are %v, want none", got)
	}
	r.clock.fire(t)
	if got, want := r.events.get(), []string{"notice"}; !slices.Equal(got, want) {
		t.Fatalf("at the limit the events are %v, want %v while the start goes on", got, want)
	}
	close(r.starter.hold["second"])
	waitFor(t, "the next track", func() bool { return slices.Contains(r.events.get(), "started second") })
	waitFor(t, "the advance to settle", func() bool { return r.sup.Playing() })
	if got, want := r.events.get(), []string{"notice", "started second"}; !slices.Equal(got, want) {
		t.Fatalf("the advance's events are %v, want %v: one notice, at the limit", got, want)
	}
}

// A save outside an advance (a person's operation through the supervisor) is noticed at once, before any start it
// leads to.
func TestASaveOutsideAnAdvanceIsNoticedAtOnce(t *testing.T) {
	r := newAdvanceRig(t)
	state, err := r.sup.Apply(r.ctx, r.sup.State().Revision, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{fixtureTrack("third")}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.events.get(), []string{"notice"}; !slices.Equal(got, want) {
		t.Fatalf("after an enqueue (revision %d) the events are %v, want %v at once", state.Revision, got, want)
	}
}

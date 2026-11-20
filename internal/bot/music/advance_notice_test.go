package music_test

import (
	"context"
	"slices"
	"sync"
	"testing"

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

type orderedStarter struct {
	*itemStarter
	events *events
}

func (s orderedStarter) Start(ctx context.Context, t music.Track) (music.Playback, error) {
	s.events.add("start " + t.ID)
	return s.itemStarter.Start(ctx, t)
}

// CI run 44790568832: the advance's call hint went out before the next track's start, so the listeners' music-state
// reads (each a durable command open on the Bot's helper) ran inside the start and opened a 550 ms gap. The notice of
// an advance now follows the next track's start.
func TestAnAdvanceNoticesItsStateAfterTheNextTrackStarted(t *testing.T) {
	seed := music.InitialState()
	first, second := fixtureTrack("first"), fixtureTrack("second")
	seed.Current = &first
	seed.Queue = []music.Track{second}
	r := newFailedTrackRig(t, seed)
	ev := &events{}
	starter := orderedStarter{itemStarter: newItemStarter(), events: ev}
	sup, err := music.NewSupervisor(r.ctx, r.channel, noticeStore{supervisorStore: r.store, events: ev}, starter)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	if _, err := sup.Apply(r.ctx, sup.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first track", func() bool { return starter.startCount("first") == 1 })
	ev.reset()

	starter.end(t, "first", nil)
	waitFor(t, "the advance's notice", func() bool { return slices.Contains(ev.get(), "notice") })
	if got, want := ev.get(), []string{"start second", "notice"}; !slices.Equal(got, want) {
		t.Fatalf("the advance's events are %v, want %v: its notice must not reach the listeners before the next track starts", got, want)
	}
}

// A save outside an advance (a person's operation through the supervisor) is noticed at once.
func TestASaveOutsideAnAdvanceIsNoticedAtOnce(t *testing.T) {
	ev := &events{}
	if music.DeferNotice(context.Background(), func() { ev.add("notice") }) {
		t.Fatal("DeferNotice held a notice outside an advance")
	}
	if len(ev.get()) != 0 {
		t.Fatalf("DeferNotice ran the notice itself: %v", ev.get())
	}
}

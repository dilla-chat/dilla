package music_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	music "github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/livekit"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

// v0.1.0-bot2.2 Task 7: failed tracks leave the queue, and every playback line names its queue item.

const fixtureVideo = "dQw4w9WgXcQ"
const fixtureTitle = "Fixture Title Never Logged"

func fixtureTrack(item string) music.Track {
	return music.Track{ID: item, Provider: music.ProviderYouTube, CanonicalID: fixtureVideo, Title: fixtureTitle, RequesterID: "member"}
}

// lockedLog is a JSON log sink safe for the supervisor's monitor goroutine.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}
func (l *lockedLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.String() }
func (l *lockedLog) rows(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if row["msg"] == msg {
			out = append(out, row)
		}
	}
	return out
}

// itemStarter publishes every track except those it is told to fail, logging its stage lines as the session does.
type itemStarter struct {
	mu        sync.Mutex
	fail      map[string]error
	starts    []string
	playbacks map[string]*supervisorPlayback
}

func newItemStarter() *itemStarter {
	return &itemStarter{fail: map[string]error{}, playbacks: map[string]*supervisorPlayback{}}
}

func (s *itemStarter) Start(ctx context.Context, t music.Track) (music.Playback, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts = append(s.starts, t.ID)
	if err := s.fail[t.ID]; err != nil {
		return nil, music.StartStage(ctx, "provider_pcm", err)
	}
	_ = music.StartStage(ctx, "published", nil)
	p := &supervisorPlayback{done: make(chan struct{})}
	s.playbacks[t.ID] = p
	return p, nil
}

func (s *itemStarter) startCount(item string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, started := range s.starts {
		if started == item {
			n++
		}
	}
	return n
}

// end finishes item's playback with err (nil: the track reached its end).
func (s *itemStarter) end(t *testing.T, item string, err error) {
	t.Helper()
	s.mu.Lock()
	p := s.playbacks[item]
	s.mu.Unlock()
	if p == nil {
		t.Fatalf("%s is not playing", item)
	}
	p.err = err
	close(p.done)
}

type failedTrackRig struct {
	store   supervisorStore
	sup     *music.Supervisor
	starter *itemStarter
	log     *lockedLog
	ctx     context.Context
	channel id.ID
	mu      sync.Mutex
	skipped []string
}

func newFailedTrackRig(t *testing.T, seed music.State) *failedTrackRig {
	t.Helper()
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	inner, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	r := &failedTrackRig{store: supervisorStore{inner: inner}, starter: newItemStarter(), log: &lockedLog{}, channel: tid(4)}
	logger := slog.New(slog.NewJSONHandler(r.log, nil))
	r.ctx = music.WithPlaybackLogger(t.Context(), logger)
	if seed.Revision != 0 || seed.Current != nil || len(seed.Queue) > 0 {
		seed.Revision = 1
		if _, err := r.store.Save(r.ctx, r.channel, 0, seed); err != nil {
			t.Fatal(err)
		}
	}
	r.sup, err = music.NewSupervisor(r.ctx, r.channel, r.store, r.starter, music.WithPlaybackLog(logger),
		music.WithSkippedTrack(func(_ context.Context, skips []music.SkippedTrack) {
			r.mu.Lock()
			defer r.mu.Unlock()
			for _, skip := range skips {
				r.skipped = append(r.skipped, skip.Track.ID)
			}
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.sup.Close() })
	return r
}

func (r *failedTrackRig) resume(t *testing.T) {
	t.Helper()
	if _, err := r.sup.Apply(r.ctx, r.sup.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
}

func (r *failedTrackRig) stored(t *testing.T) music.State {
	t.Helper()
	state, _, err := r.store.Load(r.ctx, r.channel)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func (r *failedTrackRig) skippedItems() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.skipped...)
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func queueIDs(state music.State) []string {
	ids := []string{}
	for _, track := range state.Queue {
		ids = append(ids, track.ID)
	}
	return ids
}

func TestPlaybackLinesCarryItemID(t *testing.T) {
	seed := music.InitialState()
	restored := fixtureTrack("restored-item")
	seed.Current = &restored
	r := newFailedTrackRig(t, seed)
	r.resume(t)
	r.starter.end(t, restored.ID, nil)
	waitFor(t, "the restored track's end", func() bool { return len(r.log.rows(t, "bot music playback ended")) == 1 })

	// A live /play: committed by the handler, then handed over.
	live := fixtureTrack("live-item")
	base := r.sup.State()
	queued, err := music.Apply(base, base.Revision, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{live}})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := music.Apply(queued, queued.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	resumed.Revision = queued.Revision
	revision, err := r.store.Save(r.ctx, r.channel, base.Revision, resumed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.sup.ApplyPersistedSnapshot(r.ctx, resumed, revision, true); err != nil {
		t.Fatal(err)
	}
	r.starter.end(t, live.ID, errors.New("encoder stopped"))
	waitFor(t, "the live track's end", func() bool { return len(r.log.rows(t, "bot music playback ended")) == 2 })

	want := map[string]bool{restored.ID: true, live.ID: false}
	starts := r.log.rows(t, "bot music playback start")
	if len(starts) != 2 {
		t.Fatalf("start lines = %v", starts)
	}
	for _, rows := range [][]map[string]any{starts, r.log.rows(t, "bot music playback ended")} {
		for i, row := range rows {
			item, _ := row["item_id"].(string)
			restoredFlag, ok := want[item]
			if !ok || row["restored"] != restoredFlag {
				t.Fatalf("line %d %v: want item_id of a queue item and restored=%v", i, row, restoredFlag)
			}
		}
	}
	for _, secret := range []string{fixtureVideo, fixtureTitle} {
		if strings.Contains(r.log.String(), secret) {
			t.Fatalf("a playback line carries track content %q: %s", secret, r.log.String())
		}
	}
}

func TestFailedStartRemovesItem(t *testing.T) {
	seed := music.InitialState()
	failing, next := fixtureTrack("failing-item"), fixtureTrack("next-item")
	seed.Current, seed.Queue = &failing, []music.Track{next}
	r := newFailedTrackRig(t, seed)
	r.starter.fail[failing.ID] = errors.New("provider refused: http 403")

	r.resume(t)

	state := r.sup.State()
	if state.Current == nil || state.Current.ID != next.ID || state.Paused || len(state.Queue) != 0 {
		t.Fatalf("state = %+v, want the failed item gone and the next one playing", state)
	}
	stored := r.stored(t)
	if stored.Current == nil || stored.Current.ID != next.ID || len(stored.Queue) != 0 {
		t.Fatalf("stored = %+v, want the failed item gone from persistence", stored)
	}
	if got := r.skippedItems(); len(got) != 1 || got[0] != failing.ID {
		t.Fatalf("skipped = %v, want the failed item reported once", got)
	}
}

func TestErrorEndRemovesItemButUserPauseKeeps(t *testing.T) {
	seed := music.InitialState()
	a, b, c := fixtureTrack("item-a"), fixtureTrack("item-b"), fixtureTrack("item-c")
	seed.Current, seed.Queue = &a, []music.Track{b, c}
	r := newFailedTrackRig(t, seed)
	r.resume(t)

	// A ends on a cancelled context (the 6 s end seen in production): removed, B starts.
	r.starter.end(t, a.ID, fmt.Errorf("publisher: %w", context.Canceled))
	waitFor(t, "B to start", func() bool { return r.starter.startCount(b.ID) == 1 })
	if state := r.stored(t); state.Current == nil || state.Current.ID != b.ID || strings.Join(queueIDs(state), ",") != c.ID {
		t.Fatalf("stored after A's error = %+v", state)
	}

	// B ends because its call group could not be caught up: removed, C starts.
	r.starter.end(t, b.ID, fmt.Errorf("%w: %w", livekit.ErrCallBehind, errors.New("catch-up failed")))
	waitFor(t, "C to start", func() bool { return r.starter.startCount(c.ID) == 1 })
	if got := r.skippedItems(); strings.Join(got, ",") != a.ID+","+b.ID {
		t.Fatalf("skipped = %v, want A and B reported", got)
	}

	// The user pauses C: it stays, and resumes as today.
	if _, err := r.sup.Apply(r.ctx, r.sup.State().Revision, music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	if state := r.stored(t); state.Current == nil || state.Current.ID != c.ID || !state.Paused {
		t.Fatalf("stored after the pause = %+v, want C kept, paused", state)
	}
	r.resume(t)
	if n := r.starter.startCount(c.ID); n != 2 {
		t.Fatalf("C starts = %d, want the paused item resumed", n)
	}
	if got := r.skippedItems(); len(got) != 2 {
		t.Fatalf("skipped = %v, a user pause removed nothing", got)
	}
}

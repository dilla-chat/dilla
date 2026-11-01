package music_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	music "github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/livekit"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

type supervisorNative struct {
	mu      sync.Mutex
	rows    map[string]native.StateValue
	binding native.Binding
}

type supervisorStore struct{ inner *persistence.Store }

type failingStore struct {
	supervisorStore
	saves      int
	failOnSave int
}

func (s *failingStore) Save(ctx context.Context, channel id.ID, expected uint64, state music.State) (uint64, error) {
	s.saves++
	if s.saves == s.failOnSave {
		return 0, errors.New("save failed")
	}
	return s.supervisorStore.Save(ctx, channel, expected, state)
}

func (s supervisorStore) Load(ctx context.Context, channel id.ID) (music.State, uint64, error) {
	x, err := s.inner.Load(ctx, channel)
	return x.State, x.Revision, err
}
func (s supervisorStore) Save(ctx context.Context, channel id.ID, expected uint64, state music.State) (uint64, error) {
	x, err := s.inner.Save(ctx, channel, expected, state)
	return x.Revision, err
}

func (n *supervisorNative) Binding() native.Binding { return n.binding }
func (n *supervisorNative) StateGet(ctx context.Context, area uint64, key []byte) (native.StateValue, error) {
	if err := ctx.Err(); err != nil {
		return native.StateValue{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.rows[string(key)], nil
}
func (n *supervisorNative) StateTransaction(ctx context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	c := changes[0]
	old := n.rows[string(c.Key)]
	if old.Revision != c.ExpectedRevision {
		return nil, persistence.ErrConflict
	}
	n.rows[string(c.Key)] = native.StateValue{Revision: old.Revision + 1, Value: append([]byte(nil), c.Value...)}
	return []uint64{old.Revision + 1}, nil
}

type supervisorPlayback struct {
	done   chan struct{}
	closed int
	err    error
}

func (p *supervisorPlayback) Done() <-chan struct{} { return p.done }
func (p *supervisorPlayback) Wait() error           { return p.err }
func (p *supervisorPlayback) Close() error {
	p.closed++
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	return nil
}

type supervisorStarter struct {
	explicitStarts int
	closed         atomic.Int32
	starts         []music.Track
	current        *supervisorPlayback
	fail           bool
}

type fakeTimer struct {
	fn      func()
	stopped bool
}

func (t *fakeTimer) Stop() bool { was := !t.stopped; t.stopped = true; return was }
func (t *fakeTimer) Fire() {
	if !t.stopped {
		t.fn()
	}
}

func (s *supervisorStarter) Start(ctx context.Context, t music.Track) (music.Playback, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.fail {
		return nil, errors.New("start failed")
	}
	s.starts = append(s.starts, t)
	s.current = &supervisorPlayback{done: make(chan struct{})}
	return s.current, nil
}
func (s *supervisorStarter) StartExplicit(ctx context.Context, track music.Track) (music.Playback, error) {
	s.explicitStarts++
	return s.Start(ctx, track)
}
func (s *supervisorStarter) Close() error { s.closed.Add(1); return nil }

func TestSupervisorCancellationClosesRetainedConnectionOnce(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	starter := &supervisorStarter{}
	supervisor, err := music.NewSupervisor(ctx, tid(4), supervisorStore{store}, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	cancel()
	deadline := time.After(time.Second)
	for starter.closed.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("cancellation did not close retained connection")
		case <-time.After(time.Millisecond):
		}
	}
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
	if starter.closed.Load() != 1 {
		t.Fatal("retained connection closed more than once")
	}
}

func tid(v byte) (x id.ID) { x[15] = v; return }

func TestSupervisorPersistsQueueBeforePlaybackAndNeverAutoplaysRestore(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	starter := &supervisorStarter{}
	s, err := music.NewSupervisor(context.Background(), tid(4), supervisorStore{store}, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(starter.starts) != 0 {
		t.Fatal("restore started audio")
	}
	track := music.Track{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}
	state, err := s.Apply(context.Background(), s.State().Revision, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track}})
	if err != nil {
		t.Fatal(err)
	}
	if len(starter.starts) != 0 || state.Current != nil {
		t.Fatal("enqueue started audio")
	}
	state, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	if len(starter.starts) != 1 || state.Current == nil || state.Paused {
		t.Fatalf("resume state=%+v starts=%d", state, len(starter.starts))
	}
}

func TestSupervisorRestoresPersistedCurrentPausedWithoutStartingAudio(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	state := music.InitialState()
	track := music.Track{ID: "restored", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "Restored", RequesterID: "member"}
	state, err = music.Apply(state, 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Save(context.Background(), tid(4), 0, state)
	if err != nil {
		t.Fatal(err)
	}
	state, err = music.Apply(first.State, first.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(context.Background(), tid(4), first.Revision, state); err != nil {
		t.Fatal(err)
	}
	starter := &supervisorStarter{}
	s, err := music.NewSupervisor(context.Background(), tid(4), supervisorStore{store}, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := s.State()
	if !got.Paused || got.Current == nil || got.Current.ID != track.ID || len(starter.starts) != 0 {
		t.Fatalf("restore state=%+v starts=%d", got, len(starter.starts))
	}
}

func TestSupervisorAppliesCommittedRevisionWithoutSecondMutationWrite(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	channel := tid(4)
	starter := &supervisorStarter{}
	s, err := music.NewSupervisor(context.Background(), channel, supervisorStore{store}, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	track := music.Track{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}
	nextTrack := music.Track{ID: "b", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "B", RequesterID: "member"}
	queued, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track, nextTrack}})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(context.Background(), channel, 0, queued)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyPersistedSnapshot(context.Background(), saved.State, saved.Revision, false); err != nil {
		t.Fatal(err)
	}
	if len(starter.starts) != 0 || s.State().Revision != saved.Revision {
		t.Fatalf("non-resume apply state=%+v starts=%d", s.State(), len(starter.starts))
	}
	playing, err := music.Apply(saved.State, saved.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	saved, err = store.Save(context.Background(), channel, saved.Revision, playing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyPersistedSnapshot(context.Background(), saved.State, saved.Revision, true); err != nil {
		t.Fatal(err)
	}
	if len(starter.starts) != 1 || starter.starts[0].ID != track.ID {
		t.Fatalf("committed resume starts=%v, want one start of %s", starter.starts, track.ID)
	}
	skipped, err := music.Apply(saved.State, saved.Revision, music.Operation{Kind: music.OpSkip})
	if err != nil {
		t.Fatal(err)
	}
	saved, err = store.Save(context.Background(), channel, saved.Revision, skipped)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyPersistedSnapshot(context.Background(), saved.State, saved.Revision, true); err != nil {
		t.Fatal(err)
	}
	if starter.explicitStarts != 2 {
		t.Fatalf("committed resume and Skip must authorize their own fresh admission: %d", starter.explicitStarts)
	}
	if len(starter.starts) != 2 || starter.starts[1].ID != nextTrack.ID || starter.current == nil {
		t.Fatalf("committed active Skip starts=%v, want playback of %s", starter.starts, nextTrack.ID)
	}
	loaded, err := store.Load(context.Background(), channel)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != saved.Revision {
		t.Fatalf("successful handoff wrote revision %d, want only committed revision %d", loaded.Revision, saved.Revision)
	}
}

func TestSupervisorAppliesCommittedSkipAfterRequestContextCancellation(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	channel := tid(4)
	starter := &supervisorStarter{}
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()
	s, err := music.NewSupervisor(lifetime, channel, supervisorStore{store}, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first := music.Track{ID: "first", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "First", RequesterID: "member"}
	second := music.Track{ID: "second", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "Second", RequesterID: "member"}
	queued, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{first, second}})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(context.Background(), channel, 0, queued)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyPersistedSnapshot(context.Background(), saved.State, saved.Revision, false); err != nil {
		t.Fatal(err)
	}
	playing, err := music.Apply(saved.State, saved.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	saved, err = store.Save(context.Background(), channel, saved.Revision, playing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyPersistedSnapshot(context.Background(), saved.State, saved.Revision, true); err != nil {
		t.Fatal(err)
	}
	skipped, err := music.Apply(saved.State, saved.Revision, music.Operation{Kind: music.OpSkip})
	if err != nil {
		t.Fatal(err)
	}
	saved, err = store.Save(context.Background(), channel, saved.Revision, skipped)
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()
	if _, err := s.ApplyPersistedSnapshot(requestCtx, saved.State, saved.Revision, true); err != nil {
		t.Fatalf("committed Skip handoff with canceled request context: %v", err)
	}
	if got := s.State(); got.Current == nil || got.Current.ID != second.ID || got.Paused {
		t.Fatalf("supervisor state after canceled request context = %+v, want active Second", got)
	}
	if len(starter.starts) != 2 || starter.starts[1].ID != second.ID || starter.current == nil {
		t.Fatalf("playback starts after canceled request context = %v, want Second", starter.starts)
	}
}

func TestSupervisorCommittedResumeFailurePersistsPausedQueue(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	channel := tid(4)
	starter := &supervisorStarter{fail: true}
	s, err := music.NewSupervisor(context.Background(), channel, supervisorStore{store}, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	track := music.Track{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}
	queued, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track}})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(context.Background(), channel, 0, queued)
	if err != nil {
		t.Fatal(err)
	}
	playing, err := music.Apply(saved.State, saved.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	saved, err = store.Save(context.Background(), channel, saved.Revision, playing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyPersistedSnapshot(context.Background(), saved.State, saved.Revision, true); err == nil {
		t.Fatal("provider failure was ignored")
	}
	loaded, err := store.Load(context.Background(), channel)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.State.Paused || loaded.Revision != saved.Revision+1 {
		t.Fatalf("provider failure state=%+v revision=%d, want a persisted pause after the committed resume", loaded.State, loaded.Revision)
	}
}

func TestSupervisorPausesPersistedQueueWhenPublisherLosesEpoch(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	channel := tid(4)
	starter := &supervisorStarter{}
	s, err := music.NewSupervisor(context.Background(), channel, supervisorStore{store}, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	track := music.Track{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}
	queued, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track}})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(context.Background(), channel, 0, queued)
	if err != nil {
		t.Fatal(err)
	}
	playing, err := music.Apply(saved.State, saved.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	saved, err = store.Save(context.Background(), channel, saved.Revision, playing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyPersistedSnapshot(context.Background(), saved.State, saved.Revision, true); err != nil {
		t.Fatal(err)
	}
	playback := starter.current
	playback.err = livekit.ErrEpochChanged
	close(playback.done)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state := s.State()
		if state.Paused && state.Revision == saved.Revision+1 {
			persisted, loadErr := store.Load(context.Background(), channel)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if !persisted.State.Paused || persisted.Revision != state.Revision {
				t.Fatalf("persisted epoch-loss state=%+v revision=%d", persisted.State, persisted.Revision)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("publisher epoch loss did not pause queue: %+v", s.State())
}

func TestSupervisorFailedStartLeavesStatePausedWhenPauseSaveFails(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, _ := persistence.New(n, tid(2))
	wrapped := &failingStore{supervisorStore: supervisorStore{store}}
	starter := &supervisorStarter{}
	s, err := music.NewSupervisor(context.Background(), tid(4), wrapped, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	track := music.Track{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}
	state, err := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track}})
	if err != nil {
		t.Fatal(err)
	}
	state, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	state, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpSeek, PositionMS: 1234})
	if err != nil {
		t.Fatal(err)
	}
	state, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpPause})
	if err != nil {
		t.Fatal(err)
	}
	starter.fail = true
	wrapped.failOnSave = wrapped.saves + 2 // Resume persists; compensating pause fails.
	state, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume})
	if err == nil {
		t.Fatal("expected provider start failure")
	}
	if !state.Paused || state.Current == nil || state.Current.ID != track.ID || !nearPlayhead(state.PositionMS, 0) || wrapped.saves != wrapped.failOnSave {
		t.Fatalf("failed start left unsafe state: %+v", state)
	}
}

func TestSupervisorPauseAndEmptyRoomClosePlayback(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, _ := persistence.New(n, tid(2))
	starter := &supervisorStarter{}
	var scheduled *fakeTimer
	left := false
	s, _ := music.NewSupervisor(context.Background(), tid(4), supervisorStore{store}, starter, music.WithEmptyRoomGrace(5*time.Minute, func(_ time.Duration, fn func()) music.Timer { scheduled = &fakeTimer{fn: fn}; return scheduled }, func() { left = true }))
	defer s.Close()
	track := music.Track{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}
	queued := music.Track{ID: "b", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "B", RequesterID: "member"}
	state, _ := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track, queued}})
	state, err := s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	p := starter.current
	state, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpSeek, PositionMS: 1234})
	if err != nil {
		t.Fatal(err)
	}
	state, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpPause})
	if err != nil {
		t.Fatal(err)
	}
	// Fix round 2: the seek is not applied to the audio, so the paused playhead is what played since the start.
	if p.closed != 1 || !state.Paused || !nearPlayhead(state.PositionMS, 0) {
		t.Fatalf("pause failed: closed=%d state=%+v", p.closed, state)
	}
	_, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	p = starter.current
	if err := s.EmptyRoom(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.closed != 1 || s.State().Current == nil || s.State().Current.ID != "a" || !s.State().Paused || !nearPlayhead(s.State().PositionMS, 0) || len(s.State().Queue) != 1 || s.State().Queue[0].ID != "b" {
		t.Fatal("empty room did not pause while preserving current track and queue")
	}
	if left || scheduled == nil {
		t.Fatal("empty-room callback ran before grace expiry")
	}
	scheduled.Fire()
	if !left {
		t.Fatal("expiry callback was not invoked")
	}
	left = false
	if err := s.EmptyRoom(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.RoomOccupied()
	scheduled.Fire()
	if left {
		t.Fatal("returning member did not cancel leave callback")
	}
}

func TestSupervisorAdvancesAfterTrackEOF(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, _ := persistence.New(n, tid(2))
	starter := &supervisorStarter{}
	s, _ := music.NewSupervisor(context.Background(), tid(4), supervisorStore{store}, starter)
	defer s.Close()
	tracks := []music.Track{{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}, {ID: "b", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "B", RequesterID: "member"}}
	state, _ := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
	_, err := s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	close(starter.current.done)
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		current := s.State()
		if current.Current != nil && current.Current.ID == "b" {
			break
		}
		select {
		case <-waitCtx.Done():
			t.Fatal(waitCtx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if starter.explicitStarts != 1 {
		t.Fatalf("EOF must not authorize a room reconnect: explicit starts=%d", starter.explicitStarts)
	}
	if len(starter.starts) != 2 || s.State().Current == nil || s.State().Current.ID != "b" {
		t.Fatalf("did not advance: starts=%v state=%+v", starter.starts, s.State())
	}
}

func TestSupervisorEOFFailuresLeaveQueuePaused(t *testing.T) {
	for _, phase := range []string{"save completion", "start successor"} {
		t.Run(phase, func(t *testing.T) {
			n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
			store, err := persistence.New(n, tid(2))
			if err != nil {
				t.Fatal(err)
			}
			wrapped := &failingStore{supervisorStore: supervisorStore{store}}
			starter := &supervisorStarter{}
			s, err := music.NewSupervisor(context.Background(), tid(4), wrapped, starter)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			tracks := []music.Track{{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}, {ID: "b", Provider: music.ProviderYouTube, CanonicalID: "bcdefghijkl", Title: "B", RequesterID: "member"}}
			state, err := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume})
			if err != nil {
				t.Fatal(err)
			}
			if phase == "save completion" {
				wrapped.failOnSave = wrapped.saves + 1
			} else {
				starter.fail = true
			}
			close(starter.current.done)
			deadline := time.Now().Add(time.Second)
			for !s.State().Paused && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			got := s.State()
			wantCurrent, wantQueued := "a", 1
			if phase == "start successor" {
				wantCurrent, wantQueued = "b", 0
			}
			if !got.Paused || got.Current == nil || got.Current.ID != wantCurrent || len(got.Queue) != wantQueued {
				t.Fatalf("EOF %s left unsafe state: %+v", phase, got)
			}
		})
	}
}

func TestSupervisorResumeWhilePlayingKeepsCurrentPublication(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	starter := &supervisorStarter{}
	s, err := music.NewSupervisor(context.Background(), tid(4), supervisorStore{store}, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	track := music.Track{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}
	if _, err := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.Apply(context.Background(), s.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
			t.Fatal(err)
		}
	}
	if len(starter.starts) != 1 || s.State().Paused {
		t.Fatalf("repeated Resume replaced a live track: starts=%d state=%+v", len(starter.starts), s.State())
	}
}

// nearPlayhead allows the few milliseconds a playing track advances between a test's resume and its pause (Ruling AI:
// a pause settles the playhead the track reached).
func nearPlayhead(got, want int64) bool { return got >= want && got < want+1000 }

func TestSupervisorAdoptsStoredSnapshotOnHandoffRevisionMismatchWithoutPlayback(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	channel := tid(4)
	starter := &supervisorStarter{}
	s, err := music.NewSupervisor(t.Context(), channel, supervisorStore{store}, starter)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	queued, err := music.Apply(music.InitialState(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Save(t.Context(), channel, 0, queued)
	if err != nil {
		t.Fatal(err)
	}
	playing, err := music.Apply(first.State, first.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := store.Save(t.Context(), channel, first.Revision, playing)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := s.ApplyPersistedSnapshot(t.Context(), first.State, first.Revision, true)
	if err != nil || revision != latest.Revision || s.State().Revision != latest.Revision || !s.State().Paused || len(starter.starts) != 0 {
		t.Fatalf("adoption revision=%d state=%+v starts=%d err=%v", revision, s.State(), len(starter.starts), err)
	}
}

type handoffLoadFailureStore struct {
	supervisorStore
	fail bool
}

func (s *handoffLoadFailureStore) Load(ctx context.Context, channel id.ID) (music.State, uint64, error) {
	if s.fail {
		return music.State{}, 0, errors.New("handoff read failed")
	}
	return s.supervisorStore.Load(ctx, channel)
}

func TestSupervisorEnqueueAdoptionPreservesCurrentPlayback(t *testing.T) {
	for _, handoff := range []string{"pending handoff", "failed handoff", "stale handoff"} {
		t.Run(handoff, func(t *testing.T) {
			n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
			store, err := persistence.New(n, tid(2))
			if err != nil {
				t.Fatal(err)
			}
			wrapped := &handoffLoadFailureStore{supervisorStore: supervisorStore{store}}
			starter := &supervisorStarter{}
			s, err := music.NewSupervisor(t.Context(), tid(4), wrapped, starter)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			track := music.Track{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", RequesterID: "member"}
			state, err := s.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track}})
			if err != nil {
				t.Fatal(err)
			}
			state, err = s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume})
			if err != nil {
				t.Fatal(err)
			}
			playing := starter.current
			track.ID = "b"
			committed, err := music.Apply(state, state.Revision, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Save(t.Context(), tid(4), state.Revision, committed); err != nil {
				t.Fatal(err)
			}
			if handoff == "failed handoff" {
				wrapped.fail = true
				_, err := s.SchedulePersistedSnapshot(t.Context(), committed, committed.Revision, false)
				wrapped.fail = false
				if err == nil {
					t.Fatal("handoff did not fail")
				}
			}
			if handoff == "stale handoff" {
				// A delayed older handoff reaches the same adoption path as a state read.
				if _, err := s.ApplyPersistedSnapshot(t.Context(), state, state.Revision, false); err != nil {
					t.Fatal(err)
				}
			}
			got, revision, err := s.RefreshState(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if got.Paused || got.Current == nil || got.Current.ID != "a" || len(got.Queue) != 1 || got.Queue[0].ID != "b" || revision != committed.Revision {
				t.Fatalf("enqueue adoption lost live state: state=%+v revision=%d", got, revision)
			}
			select {
			case <-playing.done:
				t.Fatal("enqueue adoption closed current playback")
			default:
			}
			if _, err := s.SchedulePersistedSnapshot(t.Context(), committed, committed.Revision, false); err != nil {
				t.Fatal(err)
			}
			if len(starter.starts) != 1 || s.State().Paused {
				t.Fatalf("late handoff changed playback: starts=%d state=%+v", len(starter.starts), s.State())
			}
		})
	}
}

func TestSupervisorImmediateSkipGuardsCurrentItem(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	starter := &supervisorStarter{}
	s, state := reviewSupervisorWithStore(t, store, starter)
	item := "a"
	op := music.Operation{Kind: music.OpSkip, ExpectedCurrentItemID: &item}
	// Leave playback paused, so advancing the queue needs no asynchronous start.
	type result struct {
		state music.State
		err   error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			got, err := s.ApplyImmediate(t.Context(), op)
			results <- result{got, err}
		}()
	}
	var first music.State
	for range 2 {
		got := <-results
		if got.err != nil || got.state.Revision != state.Revision+1 || got.state.Current == nil || got.state.Current.ID != "b" {
			t.Fatalf("racing guarded skip advanced twice: state=%+v err=%v", got.state, got.err)
		}
		first = got.state
	}
	stored, err := store.Load(t.Context(), tid(4))
	if err != nil || stored.Revision != first.Revision {
		t.Fatalf("duplicate skip wrote durable state: revision=%d err=%v", stored.Revision, err)
	}
	item = ""
	if got, err := s.ApplyImmediate(t.Context(), op); err != nil || got.Revision != first.Revision {
		t.Fatalf("empty expected current skipped a track: state=%+v err=%v", got, err)
	}
	// Older clients omit the guard and retain unconditional skip behavior.
	legacy, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpSkip})
	if err != nil || legacy.Current == nil || legacy.Current.ID != "c" || legacy.Revision != state.Revision+2 {
		t.Fatalf("legacy skip state=%+v err=%v", legacy, err)
	}
}

func TestSupervisorReadAdoptionRetainsPauseOrClosesChangedCurrent(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "paused metadata", true: "changed playing current"}[changed], func(t *testing.T) {
			n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
			store, err := persistence.New(n, tid(2))
			if err != nil {
				t.Fatal(err)
			}
			starter := &supervisorStarter{}
			s, state := reviewSupervisorWithStore(t, store, starter)
			op := music.Operation{Kind: music.OpSetVolume, Volume: 50}
			wantCurrent := "a"
			if changed {
				state, err = s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume})
				if err != nil {
					t.Fatal(err)
				}
				op.Kind = music.OpSkip
				wantCurrent = "b"
			}
			old := starter.current
			committed, err := music.Apply(state, state.Revision, op)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Save(t.Context(), tid(4), state.Revision, committed); err != nil {
				t.Fatal(err)
			}
			got, revision, err := s.RefreshState(t.Context())
			if err != nil || !got.Paused || got.Current == nil || got.Current.ID != wantCurrent || revision != committed.Revision {
				t.Fatalf("read adoption state=%+v revision=%d err=%v", got, revision, err)
			}
			select {
			case <-old.done:
			default:
				t.Fatal("changed current retained old playback")
			}
		})
	}
}

func reviewSupervisorWithStore(t *testing.T, store *persistence.Store, starter music.TrackStarter) (*music.Supervisor, music.State) {
	t.Helper()
	s, err := music.NewSupervisor(t.Context(), tid(4), supervisorStore{store}, starter)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	tracks := make([]music.Track, 3)
	for i, item := range []string{"a", "b", "c"} {
		tracks[i] = music.Track{ID: item, Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", RequesterID: "member"}
	}
	state, err := s.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
	if err != nil {
		t.Fatal(err)
	}
	state, err = s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	state, err = s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpPause})
	if err != nil {
		t.Fatal(err)
	}
	return s, state
}

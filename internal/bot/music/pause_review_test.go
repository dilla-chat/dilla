package music_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/persistence"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

type pauseReviewStore struct {
	supervisorStore
	fail          bool
	beforeFailure func()
}

func (s *pauseReviewStore) Save(ctx context.Context, channel id.ID, expected uint64, state music.State) (uint64, error) {
	if s.fail {
		if s.beforeFailure != nil {
			s.beforeFailure()
		}
		return 0, errors.New("pause save failed")
	}
	return s.supervisorStore.Save(ctx, channel, expected, state)
}
func pauseReviewFixture(t *testing.T, duration int64, opts ...music.SupervisorOption) (*music.Supervisor, *warmStarter, *pauseReviewStore) {
	t.Helper()
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	inner, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	store := &pauseReviewStore{supervisorStore: supervisorStore{inner}}
	st := &warmStarter{}
	s, err := music.NewSupervisor(t.Context(), tid(4), store, st, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	canonicalA, canonicalB := "abcdefghijk", "bcdefghijkl"
	state, err := s.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{
		{ID: "a", Provider: music.ProviderYouTube, CanonicalID: canonicalA, Title: "A", RequesterID: "member", DurationMS: duration},
		{ID: "b", Provider: music.ProviderYouTube, CanonicalID: canonicalB, Title: "B", RequesterID: "member"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	return s, st, store
}
func TestLatePauseClampsPositionAndLaterControlsWork(t *testing.T) {
	s, st, store := pauseReviewFixture(t, 41000)
	state, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause})
	if err != nil {
		t.Fatal(err)
	}
	if !state.Paused || state.PositionMS != 41000 {
		t.Fatalf("pause=%+v", state)
	}
	stored, _, err := store.Load(t.Context(), tid(4))
	if err != nil || stored.PositionMS != 41000 {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if _, err = s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	state, err = s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpSkip})
	if err != nil || state.Current == nil || state.Current.ID != "b" || st.current.closed != 1 {
		t.Fatalf("skip=%+v err=%v", state, err)
	}
}
func TestPauseApplyFailureRestoresStateAndResumesPlayback(t *testing.T) {
	s, st, _ := pauseReviewFixture(t, 60000)
	before := s.State()
	_, err := s.Apply(t.Context(), before.Revision, music.Operation{Kind: music.OpPause, NowMS: -1})
	if !errors.Is(err, music.ErrInvalidOperation) {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(s.State(), before) || st.current.paused || st.current.pauses != 1 || st.current.resumes != 1 {
		t.Fatalf("before=%+v after=%+v playback=%+v", before, s.State(), st.current)
	}
}
func TestPauseSaveFailureRestoresStateAndResumesPlayback(t *testing.T) {
	s, st, store := pauseReviewFixture(t, 60000)
	before := s.State()
	store.fail = true
	_, err := s.Apply(t.Context(), before.Revision, music.Operation{Kind: music.OpPause})
	if err == nil {
		t.Fatal("save succeeded")
	}
	if !reflect.DeepEqual(s.State(), before) || st.current.paused || st.current.resumes != 1 {
		t.Fatalf("before=%+v after=%+v playback=%+v", before, s.State(), st.current)
	}
}
func TestPauseSaveFailureWithDeadSourceColdStartsAtPosition(t *testing.T) {
	s, st, store := pauseReviewFixture(t, 60000)
	before := s.State()
	p := st.current
	store.fail = true
	store.beforeFailure = func() { p.gone = true }
	_, err := s.Apply(t.Context(), before.Revision, music.Operation{Kind: music.OpPause})
	if err == nil {
		t.Fatal("save succeeded")
	}
	if st.starts != 2 || st.offsets[1] != 42000 || p.closed != 1 || s.State().Paused || s.State().Revision != before.Revision {
		t.Fatalf("state=%+v starts=%d offsets=%v closes=%d", s.State(), st.starts, st.offsets, p.closed)
	}
}
func TestPauseRacingCleanEOFAdvancesAndLeavesNextTrackPaused(t *testing.T) {
	s, st, store := pauseReviewFixture(t, 60000)
	p := st.current
	p.waitGate, p.waiting = make(chan struct{}), make(chan struct{})
	close(p.done)
	<-p.waiting
	state, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause})
	close(p.waitGate)
	if err != nil {
		t.Fatal(err)
	}
	awaitReviewRevision(t, s, state.Revision+1)
	state = s.State()
	stored, _, err := store.Load(t.Context(), tid(4))
	if err != nil || !state.Paused || state.Current == nil || state.Current.ID != "b" || state.PositionMS != 0 || st.starts != 1 || stored.Current.ID != "b" {
		t.Fatalf("state=%+v stored=%+v starts=%d err=%v", state, stored, st.starts, err)
	}
	if _, err = s.Apply(t.Context(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	if st.starts != 2 || st.offsets[1] != 0 {
		t.Fatalf("starts=%d offsets=%v", st.starts, st.offsets)
	}
}
func TestSourceDiesAfterWarmResumeColdStartsAtPosition(t *testing.T) {
	s, st, _ := pauseReviewFixture(t, 60000)
	if _, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(t.Context(), s.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	p := st.current
	p.position = 43000
	p.err = errors.Join(music.ErrSourceGone, errors.New("source failed after resume"))
	close(p.done)
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		state := s.State()
		if state.PositionMS == 43000 && state.PositionAtMS > 0 && !state.Paused {
			if st.starts != 2 || st.offsets[1] != 43000 {
				t.Fatalf("starts=%d offsets=%v", st.starts, st.offsets)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("source recovery left state=%+v", s.State())
}
func TestRestartWhilePlayingDiscardsStalePausePosition(t *testing.T) {
	s, st, store := pauseReviewFixture(t, 60000)
	if _, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(t.Context(), s.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	restarted, err := music.NewSupervisor(t.Context(), tid(4), store, st)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.Apply(t.Context(), restarted.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	if st.starts != 2 || st.offsets[1] != 0 {
		t.Fatalf("offsets=%v", st.offsets)
	}
}
func TestPlayingMetadataRefreshPreservesLivePosition(t *testing.T) {
	s, _, store := pauseReviewFixture(t, 60000)
	if _, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(t.Context(), s.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	before := s.State()
	metadata, err := music.Apply(before, before.Revision, music.Operation{Kind: music.OpSetVolume, Volume: 50})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), tid(4), before.Revision, metadata); err != nil {
		t.Fatal(err)
	}
	state, _, err := s.RefreshState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Paused || state.PositionMS != 42000 || state.PositionAtMS != before.PositionAtMS || state.Volume != 50 {
		t.Fatalf("state=%+v before=%+v", state, before)
	}
}

func awaitReviewState(t *testing.T, s *music.Supervisor, ok func(music.State) bool) music.State {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		if state := s.State(); ok(state) {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("state never settled: %+v", s.State())
	return music.State{}
}
func TestResumeAfterBoundAtTrackEndAdvancesToNextTrack(t *testing.T) {
	clock := &pauseClock{}
	s, st, _ := pauseReviewFixture(t, 41000, music.WithPauseClock(clock.after))
	state, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause})
	if err != nil || state.PositionMS != 41000 {
		t.Fatalf("pause=%+v err=%v", state, err)
	}
	clock.advance(11 * time.Minute)
	state, err = s.Apply(t.Context(), s.State().Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	if state.Current == nil || state.Current.ID != "b" || state.Paused || st.starts != 2 || st.offsets[1] != 0 {
		t.Fatalf("state=%+v starts=%d offsets=%v", state, st.starts, st.offsets)
	}
}
func TestRecoveryColdStartAtTrackEndAdvances(t *testing.T) {
	s, st, _ := pauseReviewFixture(t, 41000)
	if _, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(t.Context(), s.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	p := st.current
	p.position = 40500
	p.err = errors.Join(music.ErrSourceGone, errors.New("source failed after resume"))
	close(p.done)
	state := awaitReviewState(t, s, func(x music.State) bool { return x.Current != nil && x.Current.ID == "b" && x.PositionAtMS > 0 })
	if state.Paused || st.starts != 2 || st.offsets[1] != 0 {
		t.Fatalf("state=%+v starts=%d offsets=%v", state, st.starts, st.offsets)
	}
}
func TestUnknownDurationIsNeverTreatedAsFinished(t *testing.T) {
	s, st, _ := pauseReviewFixture(t, 0)
	p := st.current
	if _, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	p.gone = true
	state, err := s.Apply(t.Context(), s.State().Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	if state.Current == nil || state.Current.ID != "a" || st.starts != 2 || st.offsets[1] != 42000 {
		t.Fatalf("state=%+v starts=%d offsets=%v", state, st.starts, st.offsets)
	}
}
func TestPauseRollbackColdStartDoesNotBlockImmediateControl(t *testing.T) {
	s, st, store := pauseReviewFixture(t, 60000)
	st.block = make(chan struct{})
	t.Cleanup(func() { close(st.block) })
	p := st.current
	store.fail = true
	store.beforeFailure = func() { p.gone = true }
	done := make(chan error, 1)
	go func() {
		_, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("save succeeded")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ApplyImmediate blocked on the rollback cold start")
	}
}

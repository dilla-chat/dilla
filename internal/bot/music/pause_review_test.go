package music_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
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
func pauseReviewFixture(t *testing.T, provider music.Provider, duration int64) (*music.Supervisor, *warmStarter, *pauseReviewStore) {
	t.Helper()
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	inner, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	store := &pauseReviewStore{supervisorStore: supervisorStore{inner}}
	st := &warmStarter{}
	s, err := music.NewSupervisor(t.Context(), tid(4), store, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	canonicalA, canonicalB := "abcdefghijk", "bcdefghijkl"
	if provider == music.ProviderSpotify {
		canonicalA, canonicalB = "spotify:track:1234567890123456789012", "spotify:track:2345678901234567890123"
	}
	state, err := s.Apply(t.Context(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{
		{ID: "a", Provider: provider, CanonicalID: canonicalA, Title: "A", RequesterID: "member", DurationMS: duration},
		{ID: "b", Provider: provider, CanonicalID: canonicalB, Title: "B", RequesterID: "member"},
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
	s, st, store := pauseReviewFixture(t, music.ProviderYouTube, 41000)
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
	s, st, _ := pauseReviewFixture(t, music.ProviderYouTube, 60000)
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
	s, st, store := pauseReviewFixture(t, music.ProviderYouTube, 60000)
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
	s, st, store := pauseReviewFixture(t, music.ProviderYouTube, 60000)
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
	s, st, store := pauseReviewFixture(t, music.ProviderYouTube, 60000)
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
	s, st, _ := pauseReviewFixture(t, music.ProviderYouTube, 60000)
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
	s, st, store := pauseReviewFixture(t, music.ProviderYouTube, 60000)
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
func TestSpotifyPauseClosesAndResumeStartsFromZero(t *testing.T) {
	s, st, _ := pauseReviewFixture(t, music.ProviderSpotify, 60000)
	p := st.current
	if _, err := s.ApplyImmediate(t.Context(), music.Operation{Kind: music.OpPause}); err != nil {
		t.Fatal(err)
	}
	if p.closed != 1 || p.pauses != 0 {
		t.Fatalf("closed=%d gate pauses=%d", p.closed, p.pauses)
	}
	if _, err := s.Apply(t.Context(), s.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	if st.starts != 2 || st.offsets[1] != 0 || s.State().PositionMS != 0 {
		t.Fatalf("starts=%d offsets=%v state=%+v", st.starts, st.offsets, s.State())
	}
}

func TestPlayingMetadataRefreshPreservesLivePosition(t *testing.T) {
	s, _, store := pauseReviewFixture(t, music.ProviderYouTube, 60000)
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

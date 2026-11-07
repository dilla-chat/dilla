package music_test

import (
	"context"
	"testing"
	"time"

	music "github.com/dilla-chat/dilla/internal/bot/music"
)

// markingStarter reports the stage timings a real provider reports through the context.
type markingStarter struct {
	supervisorStarter
	n int
}

func (s *markingStarter) Start(ctx context.Context, t music.Track) (music.Playback, error) {
	s.n++
	music.MarkResolve(ctx, time.Duration(s.n)*time.Millisecond, s.n%2 == 1)
	music.MarkFirstByte(ctx, time.Duration(s.n)*2*time.Millisecond)
	music.MarkFirstFrame(ctx, time.Duration(s.n)*3*time.Millisecond)
	return s.supervisorStarter.Start(ctx, t)
}

func (s *markingStarter) StartExplicit(ctx context.Context, t music.Track) (music.Playback, error) {
	return s.Start(ctx, t)
}

func TestTimingsKeepLast20(t *testing.T) {
	starter := &markingStarter{}
	s := newPrefetchSupervisor(t, starter)
	tracks := make([]music.Track, 0, 25)
	for i := 0; i < 25; i++ {
		id := string(rune('a'+i%26)) + "bcdefghijk"
		tracks = append(tracks, youtubeTrack("t"+string(rune('a'+i)), id, 200*time.Second))
	}
	state, err := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: tracks})
	if err != nil {
		t.Fatal(err)
	}
	if state, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 24; i++ {
		if state, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpSkip}); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Timings()
	if len(got) != 20 {
		t.Fatalf("kept %d timings, want the last 20", len(got))
	}
	if got[0].Track != "tf" || got[19].Track != "ty" {
		t.Fatalf("kept %s..%s, want tf..ty (oldest first)", got[0].Track, got[19].Track)
	}
	last := got[19]
	if last.Resolve != 25*time.Millisecond || last.FirstByte != 50*time.Millisecond || last.FirstFrame != 75*time.Millisecond || !last.Cold || last.At.IsZero() {
		t.Fatalf("last timing = %+v", last)
	}
	if p95 := music.StartP95(got); p95 != 72*time.Millisecond && p95 != 75*time.Millisecond {
		t.Fatalf("p95 first-frame = %v, want the 19th of 20 ordered values (72 ms)", p95)
	}
}

func TestStartTimingFirstFrameDefaultsToTheWholeStart(t *testing.T) {
	starter := &supervisorStarter{}
	s := newPrefetchSupervisor(t, starter)
	state, err := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{youtubeTrack("a", "abcdefghijk", 200*time.Second)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume}); err != nil {
		t.Fatal(err)
	}
	got := s.Timings()
	if len(got) != 1 || got[0].FirstFrame <= 0 || got[0].Cold {
		t.Fatalf("timings %+v, want one start with a measured first frame and no cold lookup", got)
	}
}

func TestStartP95OfNothingIsZero(t *testing.T) {
	if music.StartP95(nil) != 0 {
		t.Fatal("p95 of no starts is not zero")
	}
}

package music_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	music "github.com/dilla-chat/dilla/internal/bot/music"
)

// v0.1.0-bot2.4: the playback-ended line carries a fixed verdict decided inside the Bot, never the duration.

// positionPlayback is a pausable playback that reports a fixed position at its end.
type positionPlayback struct {
	supervisorPlayback
	positionMS int64
}

func (p *positionPlayback) Pause()            {}
func (p *positionPlayback) Resume() error     { return nil }
func (p *positionPlayback) PositionMS() int64 { return p.positionMS }

type positionStarter struct {
	itemStarter
	positionMS int64
}

func (s *positionStarter) Start(ctx context.Context, t music.Track) (music.Playback, error) {
	_ = music.StartStage(ctx, "published", nil)
	p := &positionPlayback{supervisorPlayback: supervisorPlayback{done: make(chan struct{})}, positionMS: s.positionMS}
	s.mu.Lock()
	s.playbacks[t.ID] = &p.supervisorPlayback
	s.mu.Unlock()
	return p, nil
}

func TestPlaybackEndVerdicts(t *testing.T) {
	const duration = int64(98000)
	cases := []struct {
		name     string
		position int64
		err      error
		detail   string
		advances bool
	}{
		{"a stream cut at the duration minus 1 s is the end", duration - 1000, errors.New("encoder stopped"), "eof", true},
		{"a clean end is eof", duration - 1000, nil, "eof", true},
		{"an early error is invalid", duration / 2, errors.New("encoder stopped"), "invalid", false},
		{"a cancelled end", duration / 2, context.Canceled, "cancelled", false},
		{"a connection end", duration / 2, music.ErrConnectionEnded, "connection", false},
		{"a helper end", duration / 2, helper.ErrClosed, "helper", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			track := fixtureTrack("first")
			track.DurationMS = duration
			next := fixtureTrack("second")
			seed := music.InitialState()
			seed.Current = &track
			seed.Queue = []music.Track{next}
			r := newFailedTrackRig(t, seed)
			starter := &positionStarter{itemStarter: *newItemStarter(), positionMS: c.position}
			sup, err := music.NewSupervisor(r.ctx, r.channel, r.store, starter, music.WithPlaybackLog(slog.New(slog.NewJSONHandler(r.log, nil))),
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
			t.Cleanup(func() { _ = sup.Close() })
			if _, err := sup.Apply(r.ctx, sup.State().Revision, music.Operation{Kind: music.OpResume}); err != nil {
				t.Fatal(err)
			}
			starter.end(t, "first", c.err)
			waitFor(t, "the end line", func() bool { return len(r.log.rows(t, "bot music playback ended")) == 1 })
			row := r.log.rows(t, "bot music playback ended")[0]
			if row["detail"] != c.detail {
				t.Fatalf("detail = %v, want %s (%v)", row["detail"], c.detail, row)
			}
			if c.advances {
				waitFor(t, "the successor", func() bool { s := sup.State(); return s.Current != nil && s.Current.ID == "second" })
				if skipped := r.skippedItems(); len(skipped) != 0 {
					t.Fatalf("a normal end removed %v as failed", skipped)
				}
			}
			for key, value := range row {
				lower := strings.ToLower(key)
				if strings.Contains(lower, "duration") || strings.Contains(lower, "elapsed") || strings.Contains(lower, "position") {
					t.Fatalf("the end line carries %s=%v", key, value)
				}
			}
			for _, secret := range []string{"98000", "97000", "49000"} {
				if strings.Contains(r.log.String(), secret) {
					t.Fatalf("a log line carries the duration %s: %s", secret, r.log.String())
				}
			}
		})
	}
}

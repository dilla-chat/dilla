package music_test

import (
	"context"
	"testing"
	"time"

	music "github.com/jonasthim/dilla/internal/bot/music"
	"github.com/jonasthim/dilla/internal/bot/music/persistence"
	"github.com/jonasthim/dilla/internal/bot/native"
)

// idleStarter is a supervisorStarter that can leave its voice room (Task 10c idle-leave).
type idleStarter struct {
	supervisorStarter
	left int
}

func (s *idleStarter) LeaveIdle() error { s.left++; return nil }

// Task 10c: the supervisor's idle-leave timer, with a fake timer and a fake starter (no room). An
// enqueue while paused starts a timer for the configured time; resuming stops it, and firing the
// stopped timer calls nothing; a stop starts a new timer that reads the setting again (5 minutes
// now); firing it calls the starter's LeaveIdle once, and firing it again does not call it twice.
// Leaving the actual room is pool_idle_test.go's.
func TestSupervisorLeavesVoiceAfterTheIdleTime(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, err := persistence.New(n, tid(2))
	if err != nil {
		t.Fatal(err)
	}
	starter := &idleStarter{}
	var timers []*fakeTimer
	var delays []time.Duration
	idle := 15 * time.Minute
	s, err := music.NewSupervisor(context.Background(), tid(4), supervisorStore{store}, starter,
		music.WithIdleLeave(func() time.Duration { return idle }, func(d time.Duration, fn func()) music.Timer {
			timer := &fakeTimer{fn: fn}
			timers, delays = append(timers, timer), append(delays, d)
			return timer
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	track := music.Track{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}
	state, err := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track}})
	if err != nil {
		t.Fatal(err)
	}
	// Enqueued but paused: idle, so the timer runs.
	if len(timers) != 1 || delays[0] != 15*time.Minute {
		t.Fatalf("timers=%d delays=%v after enqueue, want one 15-minute timer", len(timers), delays)
	}
	state, err = s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpResume})
	if err != nil {
		t.Fatal(err)
	}
	if !timers[0].stopped {
		t.Fatal("playing did not cancel the idle timer")
	}
	timers[0].Fire()
	if starter.left != 0 {
		t.Fatal("a cancelled idle timer left voice while music played")
	}
	// A changed setting applies to the next idle period.
	idle = 5 * time.Minute
	if _, err := s.Apply(context.Background(), state.Revision, music.Operation{Kind: music.OpStop}); err != nil {
		t.Fatal(err)
	}
	if len(timers) != 2 || delays[1] != 5*time.Minute {
		t.Fatalf("after stop timers=%d delays=%v, want a second, 5-minute timer", len(timers), delays)
	}
	timers[1].Fire()
	if starter.left != 1 {
		t.Fatalf("left voice %d times after the idle time, want once", starter.left)
	}
	// Firing again (a stale timer) does nothing.
	timers[1].Fire()
	if starter.left != 1 {
		t.Fatal("a fired idle timer left voice twice")
	}
}

// Without the option, or with a non-positive time, the supervisor never leaves on its own.
func TestSupervisorWithoutIdleLeaveStays(t *testing.T) {
	n := &supervisorNative{rows: map[string]native.StateValue{}, binding: native.Binding{Instance: tid(1), Community: tid(2), Installation: tid(3)}}
	store, _ := persistence.New(n, tid(2))
	starter := &idleStarter{}
	scheduled := 0
	s, _ := music.NewSupervisor(context.Background(), tid(4), supervisorStore{store}, starter,
		music.WithIdleLeave(func() time.Duration { return 0 }, func(time.Duration, func()) music.Timer { scheduled++; return &fakeTimer{} }))
	defer s.Close()
	track := music.Track{ID: "a", Provider: music.ProviderYouTube, CanonicalID: "abcdefghijk", Title: "A", RequesterID: "member"}
	if _, err := s.Apply(context.Background(), 0, music.Operation{Kind: music.OpEnqueue, Tracks: []music.Track{track}}); err != nil {
		t.Fatal(err)
	}
	if scheduled != 0 || starter.left != 0 {
		t.Fatalf("scheduled=%d left=%d, want none", scheduled, starter.left)
	}
}

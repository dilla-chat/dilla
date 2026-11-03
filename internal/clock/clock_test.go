package clock_test

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
)

func TestFakeAdvanceMovesNowAndFiresTimer(t *testing.T) {
	start := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	f := clock.NewFake(start)
	if !f.Now().Equal(start) {
		t.Fatalf("Now = %s, want %s", f.Now(), start)
	}
	timer := f.NewTimer(5 * time.Second)
	select {
	case <-timer.C():
		t.Fatal("timer fired before Advance")
	default:
	}
	f.Advance(5 * time.Second)
	select {
	case at := <-timer.C():
		if !at.Equal(start.Add(5 * time.Second)) {
			t.Fatalf("timer fired at %s, want %s", at, start.Add(5*time.Second))
		}
	case <-time.After(time.Second):
		t.Fatal("timer did not fire after Advance")
	}
	if got := f.Since(start); got != 5*time.Second {
		t.Fatalf("Since = %s, want 5s", got)
	}
}

func TestSystemClockAdvances(t *testing.T) {
	c := clock.System()
	first := c.Now()
	c.Sleep(2 * time.Millisecond)
	if !c.Now().After(first) {
		t.Fatal("system clock did not advance")
	}
}

// Stop and Reset must work on a timer from EITHER clock. A hand-built
// time.Timer panics in both since Go 1.23, which is why Clock.NewTimer returns
// a clock.Timer interface rather than *time.Timer (deviation ID8).
func TestTimerStopAndResetAreSafeOnBothClocks(t *testing.T) {
	for _, tc := range []struct {
		name string
		clk  clock.Clock
	}{
		{"system", clock.System()},
		{"fake", clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			timer := tc.clk.NewTimer(time.Hour)
			if !timer.Stop() {
				t.Fatal("Stop reported the timer was not pending")
			}
			if timer.Stop() {
				t.Fatal("a second Stop reported the timer was still pending")
			}
			if timer.Reset(time.Hour) {
				t.Fatal("Reset reported a stopped timer as pending")
			}
			timer.Stop()
		})
	}
}

func TestFakeTimerStopPreventsFiring(t *testing.T) {
	start := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	f := clock.NewFake(start)
	timer := f.NewTimer(time.Second)
	timer.Stop()
	f.Advance(2 * time.Second)
	select {
	case <-timer.C():
		t.Fatal("a stopped fake timer fired")
	default:
	}
}

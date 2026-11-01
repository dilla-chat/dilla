package music

import (
	"testing"
	"time"
)

func TestStartBudgetConsecutiveTotalAndDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		name                 string
		skipped, consecutive int
		elapsed              time.Duration
		want                 bool
	}{
		{"two consecutive", 2, 2, 0, true},
		{"five total independent of streak", 5, 0, 0, true},
		{"thirty seconds", 0, 0, 30 * time.Second, true},
		{"within all bounds", 1, 1, 29 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := startBudget{skipped: tc.skipped, consecutive: tc.consecutive, deadline: now.Add(30 * time.Second)}
			if got := budget.exhausted(now.Add(tc.elapsed)); got != tc.want {
				t.Fatalf("exhausted=%v want%v", got, tc.want)
			}
		})
	}
}

func TestStartBudgetSuccessfulStartResetsConsecutiveSkips(t *testing.T) {
	b := startBudget{skipped: 1, consecutive: 1, deadline: time.Now().Add(maxStartDuration)}
	b.succeeded()
	if b.consecutive != 0 || b.skipped != 1 {
		t.Fatalf("success budget = %+v", b)
	}
	if maxStartDuration != 45*time.Second {
		t.Fatalf("start duration = %v, want 45s beyond Spotify's 35s connect timeout", maxStartDuration)
	}
}

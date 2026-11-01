package service

import (
	"context"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
)

func TestGroupEventRateLimitDoesNotAgeOrAcknowledge(t *testing.T) {
	q := ageTestQueue()
	retry := uint64(1000)
	handler := groupEventHandlerFunc(func(context.Context, native.GroupEvent) error {
		return &client.APIError{Status: 429, Code: "E_RATE_LIMITED", Outcome: client.OutcomeDefinitive, RetryAfterMS: &retry}
	})
	start := time.Unix(1800000000, 0)
	for pass := range 4 {
		result, err := pollAt(t, t.Context(), q, start.Add(time.Duration(pass)*25*time.Hour), handler)
		if err != nil || q.acked != 0 || len(q.state) != 0 || !result.More {
			t.Fatalf("pass %d: result=%+v err=%v ack=%d state=%v", pass, result, err, q.acked, q.state)
		}
	}
	result, err := pollAt(t, t.Context(), q, start.Add(100*time.Hour), groupEventHandlerFunc(func(context.Context, native.GroupEvent) error { return nil }))
	if err != nil || result.Handled != 2 || q.acked != 2 {
		t.Fatalf("resume result=%+v err=%v ack=%d", result, err, q.acked)
	}
}

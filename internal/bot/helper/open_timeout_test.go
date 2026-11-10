package helper

import (
	"context"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/prioritygate"
)

type timeoutRecorder struct {
	seen map[protocol.HelperOperation]time.Duration
}

func (r *timeoutRecorder) request(ctx context.Context, requestID id.ID, operation protocol.HelperOperation, _ []any, timeout time.Duration) (protocol.HelperResponse, error) {
	r.seen[operation] = timeout
	if d, ok := ctx.Deadline(); ok && time.Until(d) < timeout-time.Second {
		r.seen[operation] = time.Until(d)
	}
	return protocol.HelperResponse{RequestID: requestID}, nil
}

func (r *timeoutRecorder) stop(time.Duration) error { return nil }

// Opening an upgraded v1 store runs one-off cleanup in the helper. Under the ordinary 15 s call budget the helper
// was killed mid-upgrade on every reconcile pass (v0.1.0-bot2 on an upgraded instance), so Open has its own budget
// while every other call keeps the short one.
func TestOpenHasItsOwnLongerBudgetAndCallsKeepTheShortOne(t *testing.T) {
	r := &timeoutRecorder{seen: map[protocol.HelperOperation]time.Duration{}}
	c := &Client{gate: prioritygate.New(1), transport: r, callTime: 15 * time.Second, openTime: 10 * time.Minute, stopTime: time.Second}
	key := make([]byte, 32)
	if _, err := c.Open(context.Background(), id.New(), id.New(), id.New(), key); err != nil {
		t.Fatal(err)
	}
	if got := r.seen[protocol.HelperOpen]; got < 9*time.Minute {
		t.Fatalf("Open ran under %v, want the 10 min open budget", got)
	}
	if _, err := c.request(context.Background(), protocol.HelperOperation(2), nil); err != nil {
		t.Fatal(err)
	}
	if got := r.seen[protocol.HelperOperation(2)]; got != 15*time.Second {
		t.Fatalf("an ordinary call ran under %v, want 15s", got)
	}
}

func TestStartDefaultsTheOpenBudgetToTenMinutes(t *testing.T) {
	o := Options{}
	applyDefaults(&o)
	if o.OpenTimeout != 10*time.Minute || o.CallTimeout != 15*time.Second {
		t.Fatalf("defaults: open %v call %v", o.OpenTimeout, o.CallTimeout)
	}
}

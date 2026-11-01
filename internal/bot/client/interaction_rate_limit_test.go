package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/jonasthim/dilla/internal/id"
)

// Eleven private groups need 22 proof POSTs per sweep. Cover the default burst
// and repeated sweeps against the live server's larger burst, both at 2/second.
func TestInteractionProofRefreshElevenGroupsUnderProductionWriteLimit(t *testing.T) {
	for _, tc := range []struct {
		name          string
		burst, passes int
	}{
		{"default burst", 20, 1}, {"live burst", 40, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limiter := rate.NewLimiter(2, tc.burst)
			want := cbor(t, []any{[]byte{1}, make([]byte, 64)})
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				if !limiter.Allow() {
					reply(w, http.StatusTooManyRequests, []any{"E_RATE_LIMITED", "", uint64(500)})
					return
				}
				w.Header().Set("Content-Type", "application/cbor")
				_, _ = w.Write(want)
			}, 0)
			s, _ := c.Session("bot-session")
			for range tc.passes {
				for group := byte(10); group < 21; group++ {
					requestID := id.New()
					for _, snapshot := range []bool{false, true} {
						got, err := interactionProofCall(snapshot)(s, t.Context(), testID(1), requestID, testID(group), testID(2), testID(3))
						if err != nil || string(got) != string(want) {
							t.Fatalf("group %d snapshot %v: proof = %x, %v", group, snapshot, got, err)
						}
					}
				}
			}
		})
	}
}

func TestInteractionProofRefusalRetriesAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     []any
		attempts int
	}{
		{"persistent rate limit", 429, []any{"E_RATE_LIMITED", "", uint64(100)}, 0},
		{"long delay", 429, []any{"E_RATE_LIMITED", "", uint64(5001)}, 1},
		{"overflow delay", 429, []any{"E_RATE_LIMITED", "", ^uint64(0)}, 1},
		{"authorization", 403, []any{"E_FORBIDDEN", "", nil}, 1},
		{"unknown outcome", 503, []any{"E_UNAVAILABLE", "", uint64(1)}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				calls++
				reply(w, tc.status, tc.body)
			}, 0)
			s, _ := c.Session("bot-session")
			ctx, cancel := context.WithTimeout(t.Context(), 450*time.Millisecond)
			defer cancel()
			_, err := s.IssueBotInteractionAdmission(ctx, testID(1), testID(2), testID(3), testID(4), testID(5))
			if err == nil || tc.attempts > 0 && calls != tc.attempts || tc.attempts == 0 && calls < 4 {
				t.Fatalf("refusal = %v after %d attempts; want %d", err, calls, tc.attempts)
			}
		})
	}
}

func TestInteractionProofRecoversFromWriteBurstLimit(t *testing.T) {
	for _, contextSnapshot := range []bool{false, true} {
		t.Run(map[bool]string{false: "admission", true: "context"}[contextSnapshot], func(t *testing.T) {
			var calls int
			var firstBody string
			var refusedAt time.Time
			want := cbor(t, []any{[]byte{1}, make([]byte, 64)})
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				body, _ := io.ReadAll(r.Body)
				calls++
				if calls == 1 {
					firstBody = string(body)
					refusedAt = time.Now()
					reply(w, http.StatusTooManyRequests, []any{"E_RATE_LIMITED", "", uint64(30)})
					return
				}
				if string(body) != firstBody || time.Since(refusedAt) < 30*time.Millisecond {
					t.Error("proof retry changed the binding or ignored the server delay")
				}
				w.Header().Set("Content-Type", "application/cbor")
				_, _ = w.Write(want)
			}, 0)
			s, _ := c.Session("bot-session")
			got, err := interactionProofCall(contextSnapshot)(s, context.Background(), testID(1), testID(2), testID(3), testID(4), testID(5))
			if err != nil || string(got) != string(want) || calls != 2 {
				t.Fatalf("proof = %x, %v (%d attempts); want exact signed response after retry", got, err, calls)
			}
		})
	}
}

func TestInteractionProofRateLimitWaitCanBeCanceled(t *testing.T) {
	for _, contextSnapshot := range []bool{false, true} {
		t.Run(map[bool]string{false: "admission", true: "context"}[contextSnapshot], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			calls := 0
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, http.StatusOK, discovery(testID(99)))
					return
				}
				calls++
				reply(w, http.StatusTooManyRequests, []any{"E_RATE_LIMITED", "", uint64(5000)})
			}, 0)
			s, _ := c.Session("bot-session")
			_, err := interactionProofCall(contextSnapshot)(s, ctx, testID(1), testID(2), testID(3), testID(4), testID(5))
			if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
				t.Fatalf("proof cancellation = %v (%d attempts)", err, calls)
			}
		})
	}
}

func interactionProofCall(snapshot bool) func(*Session, context.Context, id.ID, id.ID, id.ID, id.ID, id.ID) ([]byte, error) {
	if snapshot {
		return (*Session).BotInteractionContextSnapshot
	}
	return (*Session).IssueBotInteractionAdmission
}

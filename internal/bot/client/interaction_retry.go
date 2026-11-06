package client

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"time"
)

// RateLimitError recognizes definitive rate refusals. Call start can also return
// 429 after writes and is retry-safe; future in-handler 429s must be audited. Retrying them
// preserves the exact request binding; ambiguous outcomes keep their old behavior.
func RateLimitError(err error) *APIError {
	var refusal *APIError
	var transient func(error) bool
	transient = func(err error) bool {
		if err == nil {
			return true
		}
		if capacity, ok := err.(*rateCacheCapacityError); ok { //nolint:errorlint // Inspect this tree node; errors.As would hide mixed permanent joins.
			refusal = capacity.api
			return true
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, cause := range joined.Unwrap() {
				if !transient(cause) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			return transient(wrapped.Unwrap())
		}
		var api *APIError
		if errors.As(err, &api) && api.Status == http.StatusTooManyRequests && api.Code == "E_RATE_LIMITED" && api.Outcome == OutcomeDefinitive {
			refusal = api
			return true
		}
		return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	}
	if transient(err) {
		return refusal
	}
	return nil
}

// IsDeferred recognizes only trees of retry-safe refusals or cooperative
// stops, a socket route with no gateway yet (ErrNoGateway) and their cancellation causes, never a joined permanent
// failure.
func IsDeferred(err error) bool {
	found := false
	var transient func(error) bool
	transient = func(err error) bool {
		if err == nil {
			return true
		}
		if _, ok := err.(*rateCacheCapacityError); ok { //nolint:errorlint // Inspect this tree node; errors.As would hide mixed permanent joins.
			found = true
			return true
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, cause := range joined.Unwrap() {
				if !transient(cause) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			return transient(wrapped.Unwrap())
		}
		if errors.Is(err, ErrCooperativeStop) || errors.Is(err, ErrNoGateway) || RateLimitError(err) != nil {
			found = true
			return true
		}
		return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	}
	return transient(err) && found
}

func retryDelay(refusal *APIError, attempt int) time.Duration {
	if refusal.RetryAfterMS != nil {
		// Clamp before converting untrusted uint64 milliseconds to a signed duration.
		const maxMS = uint64((1<<63 - 1) / int64(time.Millisecond))
		return time.Duration(min(*refusal.RetryAfterMS, maxMS)) * time.Millisecond
	}
	base := min(250*time.Millisecond*time.Duration(1<<min(attempt, 6)), 10*time.Second)
	jitter, _ := rand.Int(rand.Reader, big.NewInt(int64(base/4)+1))
	if jitter != nil {
		base += time.Duration(jitter.Int64())
	}
	return min(base, 10*time.Second)
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	// The previous exchange has completed: waiting for Retry-After is a safe
	// boundary, and stopping here must leave the request context alive.
	if err := CooperativeCheckpoint(ctx); err != nil {
		return err
	}
	delay = max(delay, time.Millisecond)
	if deadline, ok := ctx.Deadline(); ok && delay > time.Until(deadline) {
		return context.DeadlineExceeded
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	var stopped <-chan time.Time
	if stop, _ := ctx.Value(cooperativeStopKey{}).(func() bool); stop != nil {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		stopped = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return CooperativeCheckpoint(ctx)
		case <-stopped:
			if err := CooperativeCheckpoint(ctx); err != nil {
				return err
			}
		}
	}
}

func (s *Session) interactionProofRequest(ctx context.Context, path string, request []byte, maxBytes int64, mutation bool) ([]byte, error) {
	raw, _, err := s.do(ctx, http.MethodPost, path, nil, request, maxBytes, mutation)
	return raw, err
}

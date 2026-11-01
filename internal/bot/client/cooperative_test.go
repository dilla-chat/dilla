package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func cooperativeTestClient(t *testing.T, transport http.RoundTripper) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: "https://example.invalid", InstanceID: testID(99), HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	c.discovered.Store(true)
	return c
}

func TestCooperativeStopFinishesInFlightHTTPThenStopsNextRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stop atomic.Bool
		entered, release := make(chan struct{}), make(chan struct{})
		calls := 0
		c := cooperativeTestClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				close(entered)
				<-release
			}
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}))
		ctx := WithCooperativeStop(t.Context(), stop.Load)
		done := make(chan error, 1)
		request := func() error {
			_, _, err := c.do(ctx, "bot", http.MethodPost, "groups/"+testID(1).String()+"/cursor", nil, nil, MaxSmallBody, true)
			return err
		}
		go func() { done <- request() }()
		<-entered
		stop.Store(true)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("in-flight HTTP interrupted: %v", err)
		default:
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("in-flight HTTP outcome=%v", err)
		}
		if ctx.Err() != nil {
			t.Fatalf("cooperative stop cancelled request context: %v", ctx.Err())
		}
		if err := request(); !errors.Is(err, ErrCooperativeStop) {
			t.Fatalf("next request=%v", err)
		}
		if calls != 1 {
			t.Fatalf("transport calls=%d, want 1", calls)
		}
	})
}

func TestCooperativeStopLeavesLimiterWaitWithoutSpendingToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newPriorityLimiter(.1, 1)
		if !limiter.Allow() {
			t.Fatal("missing initial token")
		}
		var stop atomic.Bool
		ctx := WithCooperativeStop(t.Context(), stop.Load)
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- waitLimiter(ctx, limiter) }()
		synctest.Wait()
		stop.Store(true)
		if err := <-done; !errors.Is(err, ErrCooperativeStop) || RateLimitError(err) != nil {
			t.Fatalf("limiter stop=%v", err)
		}
		if elapsed := time.Since(start); elapsed > 25*time.Millisecond {
			t.Fatalf("stop waited %v for a token", elapsed)
		}
		if ctx.Err() != nil || limiter.background != 0 {
			t.Fatalf("stop left cancellation/waiter: ctx=%v background=%d", ctx.Err(), limiter.background)
		}
		time.Sleep(10 * time.Second)
		if !limiter.Allow() {
			t.Fatal("stopped admission spent the next token")
		}
	})
}

func TestCooperativeStopInterruptsRetryDelayAtSafeBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stop atomic.Bool
		ctx := WithCooperativeStop(t.Context(), stop.Load)
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- waitRetry(ctx, 10*time.Second) }()
		synctest.Wait()
		stop.Store(true)
		if err := <-done; !errors.Is(err, ErrCooperativeStop) {
			t.Fatalf("retry wait=%v", err)
		}
		if elapsed := time.Since(start); elapsed > 25*time.Millisecond {
			t.Fatalf("stop waited through Retry-After: %v", elapsed)
		}
		if ctx.Err() != nil {
			t.Fatalf("retry stop cancelled context: %v", ctx.Err())
		}
	})
}

func TestCooperativeStopAfterRateRefusalAvoidsRetryAndPreservesDeferral(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stop atomic.Bool
		calls := 0
		refusal := cbor(t, []any{"E_RATE_LIMITED", "", uint64(10000)})
		c := cooperativeTestClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			stop.Store(true)
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Content-Type": []string{"application/cbor"}}, Body: io.NopCloser(strings.NewReader(string(refusal))), Request: r}, nil
		}))
		ctx := WithCooperativeStop(t.Context(), stop.Load)
		start := time.Now()
		_, _, err := c.do(ctx, "bot", http.MethodPost, "groups/"+testID(1).String()+"/cursor", nil, nil, MaxSmallBody, true)
		if !errors.Is(err, ErrCooperativeStop) || !IsDeferred(err) || calls != 1 {
			t.Fatalf("refusal stop=%v deferred=%v calls=%d", err, IsDeferred(err), calls)
		}
		var api *APIError
		if !errors.As(err, &api) || api.Code != "E_RATE_LIMITED" {
			t.Fatalf("stop lost definitive refusal: %v", err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("already stopped turn waited %v before returning", elapsed)
		}
	})
}

func TestBackgroundLifetimeClearsStopAndPriorityWhileRetainingCancellation(t *testing.T) {
	type lifetimeValueKey struct{}
	parent, cancel := context.WithTimeout(context.WithValue(t.Context(), lifetimeValueKey{}, "installation"), time.Hour)
	defer cancel()
	turn := WithCooperativeStop(WithImmediateControl(parent), func() bool { return true })
	background := WithBackground(turn)
	if RequestPriority(background) != 0 || !errors.Is(CooperativeCheckpoint(background), ErrCooperativeStop) {
		t.Fatal("background priority clearing lost the safe stop")
	}
	lifetime := WithoutCooperativeStop(background)
	if err := CooperativeCheckpoint(lifetime); err != nil {
		t.Fatalf("lifetime inherited completed turn stop: %v", err)
	}
	if lifetime.Value(lifetimeValueKey{}) != "installation" {
		t.Fatal("lifetime lost installation context")
	}
	if deadline, ok := lifetime.Deadline(); !ok {
		t.Fatal("lifetime lost real deadline")
	} else if want, _ := parent.Deadline(); !deadline.Equal(want) {
		t.Fatal("lifetime changed real deadline")
	}
	if RequestPriority(WithoutCooperativeStop(turn)) != 2 {
		t.Fatal("clearing stop also cleared priority")
	}
	cancel()
	if !errors.Is(CooperativeCheckpoint(lifetime), context.Canceled) {
		t.Fatal("lifetime detached real cancellation")
	}
}

func TestDeferredClassificationPreservesCooperativeStopAndUnrelatedFailures(t *testing.T) {
	refusal := &APIError{Status: http.StatusTooManyRequests, Code: "E_RATE_LIMITED", Outcome: OutcomeDefinitive}
	for _, err := range []error{ErrCooperativeStop, fmt.Errorf("phase: %w", ErrCooperativeStop), errors.Join(ErrCooperativeStop, context.Canceled), errors.Join(refusal, ErrCooperativeStop)} {
		if !IsDeferred(err) {
			t.Fatalf("safe stop not deferred: %v", err)
		}
	}
	for _, err := range []error{nil, context.Canceled, errors.Join(ErrCooperativeStop, errors.New("durable state write failed")), errors.Join(refusal, ErrCooperativeStop, errors.New("handler failed"))} {
		if IsDeferred(err) {
			t.Fatalf("unrelated failure hidden by deferral: %v", err)
		}
	}
	if RateLimitError(ErrCooperativeStop) != nil {
		t.Fatal("cooperative stop masqueraded as server refusal")
	}
}

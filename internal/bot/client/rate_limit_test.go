package client

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonasthim/dilla/internal/config"
)

func TestClientRateLimitRetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retry      any
		minimum    time.Duration
		status     int
		code       string
		persistent bool
	}{
		{"retry after", uint64(40), 40 * time.Millisecond, 429, "E_RATE_LIMITED", false},
		{"missing retry after", nil, 250 * time.Millisecond, 429, "E_RATE_LIMITED", false},
		{"retry after header", nil, time.Second, 429, "E_RATE_LIMITED", false},
		{"deadline retains refusal", uint64(1000), 0, 429, "E_RATE_LIMITED", true},
		{"non rate limit", nil, 0, 403, "E_FORBIDDEN", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/instance" {
					reply(w, 200, discovery(testID(99)))
					return
				}
				calls++
				if calls == 1 || tc.persistent {
					if tc.name == "retry after header" {
						w.Header().Set("Retry-After", "1")
					}
					reply(w, tc.status, []any{tc.code, "", tc.retry})
					return
				}
				w.WriteHeader(204)
			}, 0)
			timeout := 700 * time.Millisecond
			if tc.name == "retry after header" {
				timeout = 2 * time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			start := time.Now()
			_, _, err := c.do(ctx, "session", http.MethodPost, "groups/"+testID(1).String()+"/cursor", nil, nil, MaxSmallBody, true)
			if tc.persistent {
				var api *APIError
				if !errors.As(err, &api) || api.Code != tc.code || calls != 1 {
					t.Fatalf("error=%v calls=%d", err, calls)
				}
			} else if err != nil || calls != 2 || time.Since(start) < tc.minimum {
				t.Fatalf("error=%v calls=%d elapsed=%s", err, calls, time.Since(start))
			}
		})
	}
}

func TestDeviceLimiterWritePacingAndIndependentClasses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := New(Config{BaseURL: "https://example.invalid", InstanceID: testID(99), HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		})}})
		if err != nil {
			t.Fatal(err)
		}
		c.discovered.Store(true)
		start := time.Now()
		for range 100 {
			if _, _, err := c.do(t.Context(), "device", http.MethodPost, "groups/"+testID(1).String()+"/cursor", nil, nil, MaxSmallBody, true); err != nil {
				t.Fatal(err)
			}
		}
		if elapsed := time.Since(start); elapsed < 80*time.Second*2/3 {
			t.Fatalf("100 writes took %s", elapsed)
		}
		start = time.Now()
		for range 40 {
			if err := c.waitRate(t.Context(), "device", http.MethodGet, "groups/x/info"); err != nil {
				t.Fatal(err)
			}
		}
		for range 10 {
			if err := c.waitRate(t.Context(), "device", http.MethodPost, "groups/x/message"); err != nil {
				t.Fatal(err)
			}
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("write traffic depleted other classes: %s", elapsed)
		}
	})
}

func TestRequestRateClassesMirrorServer(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{"GET", "bot-runtime/installations/x/interaction-groups/y/info", "read"},
		{"HEAD", "channels/x/messages", "read"},
		{"POST", "groups/x/message", "message"},
		{"POST", "channels/x/messages", "message"},
		{"POST", "groups/x/bot/messages", "write"},
		{"POST", "bot-runtime/installations/x/interaction-admissions/context", "write"},
		{"POST", "communities/x/bot/receipts/y/resolve", "write"},
		{"POST", "bot-runtime/installations/x/interaction-groups/y/cursor", "write"},
		{"PUT", "devices/x/credential", "write"},
		{"POST", "groups/x/commit", "commit"},
		{"POST", "groups/x/resync", "commit"},
		{"POST", "groups/x/heal", "commit"},
		{"POST", "groups/x/proposal", "proposal"},
		{"GET", "devices/x/keypackage", "keypackage"},
		{"POST", "bot-runtime/installations/x/interaction-groups/y/heal", "commit"},
	} {
		if got := requestClass(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s class=%s want=%s", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestDeviceLimiterSurvivesSessionRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := New(Config{BaseURL: "https://example.invalid", InstanceID: testID(99)})
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range []string{"old"} {
			if _, err := c.SessionForDevice(token, testID(1)); err != nil {
				t.Fatal(err)
			}
		}
		for range 20 {
			if err := c.waitRate(t.Context(), "old", "POST", "groups/x/cursor"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := c.SessionForDevice("renewed", testID(1)); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := c.waitRate(t.Context(), "renewed", "POST", "groups/x/cursor"); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) < time.Second*2/3 {
			t.Fatal("session renewal reset the device write burst")
		}
		if _, err := c.SessionForDevice("other-device", testID(2)); err != nil {
			t.Fatal(err)
		}
		start = time.Now()
		if err := c.waitRate(t.Context(), "other-device", "POST", "groups/x/cursor"); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 0 {
			t.Fatal("another device shared the depleted bucket")
		}
	})
}

func TestDeviceLimiterDeadlineIsTransient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := New(Config{BaseURL: "https://example.invalid", InstanceID: testID(99)})
		if err != nil {
			t.Fatal(err)
		}
		for range 20 {
			if err := c.waitRate(t.Context(), "device", "POST", "groups/x/cursor"); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		defer cancel()
		if err := c.waitRate(ctx, "device", "POST", "groups/x/cursor"); RateLimitError(err) == nil {
			t.Fatalf("local pacing deadline must leave delivery pending: %v", err)
		}
	})
}

func TestRateLimitClassificationPreservesJoinedFailures(t *testing.T) {
	refusal := &APIError{Status: 429, Code: "E_RATE_LIMITED", Outcome: OutcomeDefinitive}
	if RateLimitError(errors.Join(refusal, context.DeadlineExceeded)) == nil {
		t.Fatal("retry deadline lost refusal")
	}
	if RateLimitError(errors.Join(refusal, errors.New("native state failure"))) != nil {
		t.Fatal("rate refusal hid unrelated failure")
	}
}

func TestRateLimitBackoffBounds(t *testing.T) {
	refusal := &APIError{}
	for attempt := range 20 {
		base := min(250*time.Millisecond*time.Duration(1<<min(attempt, 6)), 10*time.Second)
		delay := retryDelay(refusal, attempt)
		if delay < base || delay > min(base+base/4, 10*time.Second) {
			t.Fatalf("attempt=%d delay=%s base=%s", attempt, delay, base)
		}
	}
}

func TestRateLimitConfigDefaultsAndOverrides(t *testing.T) {
	defaults, err := (RateLimits{}).defaults()
	if err != nil || defaults.WritePerSecond != 1.5 || defaults.WriteBurst != 20 || defaults.ReadPerSecond != 8 || defaults.ReadBurst != 40 || defaults.MessagePerSecond != .8 || defaults.MessageBurst != 10 {
		t.Fatalf("defaults=%+v error=%v", defaults, err)
	}
	custom, err := (RateLimits{WritePerSecond: 1, ReadBurst: 5}).defaults()
	if err != nil || custom.WritePerSecond != 1 || custom.ReadBurst != 5 {
		t.Fatalf("override=%+v error=%v", custom, err)
	}
	for _, bad := range []RateLimits{{WritePerSecond: -1}, {ReadBurst: -1}, {MessagePerSecond: math.Inf(1)}, {ReadPerSecond: math.NaN()}} {
		if _, err := bad.defaults(); err == nil {
			t.Fatalf("accepted invalid limits %+v", bad)
		}
	}
}

func TestRetryBeyondDeadlineReturnsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		start := time.Now()
		if err := waitRetry(ctx, 2*time.Second); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error=%v", err)
		}
		if time.Since(start) != 0 {
			t.Fatal("slept despite impossible retry")
		}
	})
}

func TestRenewalDropsRetiredTokenMaps(t *testing.T) {
	c, err := New(Config{BaseURL: "https://example.invalid", InstanceID: testID(99)})
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"old", "renewed", "latest"} {
		if _, err := c.SessionForDevice(token, testID(1)); err != nil {
			t.Fatal(err)
		}
		if err := c.waitRate(t.Context(), token, "POST", "groups/x/cursor"); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.devices) != 1 || len(c.buckets) != 1 {
		t.Fatalf("devices=%d buckets=%d", len(c.devices), len(c.buckets))
	}
	if c.buckets["device/"+testID(1).String()].write.Tokens() > 18 {
		t.Fatal("renewal reset budget")
	}
}

// Pin both sides of the table: specialized routes are explicitly metered in the
// server registrations, rather than handled by routeClass's read/write fallback.
func TestSpecializedRateTableMatchesServerRegistrations(t *testing.T) {
	for _, tc := range []struct{ file, route, meter, method, path, want string }{
		{"groups.go", "POST /v1/groups/{id}/commit", "dsClassCommit", "POST", "groups/x/commit", "commit"},
		{"groups.go", "POST /v1/groups/{id}/resync", "dsClassCommit", "POST", "groups/x/resync", "commit"},
		{"groups.go", "POST /v1/groups/{id}/proposal", "dsClassProposal", "POST", "groups/x/proposal", "proposal"},
		{"heal.go", "POST /v1/groups/{id}/heal", "dsClassCommit", "POST", "groups/x/heal", "commit"},
		{"bot_interaction_group_heal.go", "mux.Handle(\"POST \"+base", "dsClassCommit", "POST", "bot-runtime/installations/x/interaction-groups/y/heal", "commit"},
	} {
		raw, err := os.ReadFile("../../api/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		needle := tc.route
		if strings.HasPrefix(needle, "POST ") {
			needle = "\"" + needle + "\""
		}
		_, registration, ok := strings.Cut(string(raw), needle)
		end := strings.Index(registration, "mux.Handle(")
		if end >= 0 {
			registration = registration[:end]
		}
		if !ok || !strings.Contains(registration, tc.meter) {
			t.Fatalf("server registration changed: %s", tc.route)
		}
		if got := requestClass(tc.method, tc.path); got != tc.want {
			t.Fatalf("%s: %s", tc.path, got)
		}
	}
	raw, err := os.ReadFile("../../api/keypackages.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "allowDS(h.Limiter, dsClassKeyPackage, keyPackageBucket(session.DeviceID, deviceID))") {
		t.Fatal("server keypackage meter changed")
	}
	defaults, _ := (RateLimits{}).defaults()
	server := config.Default().Limits.Rate
	for _, pair := range [][2]float64{{defaults.CommitPerSecond, server.CommitPerSecond}, {defaults.ProposalPerSecond, server.ProposalPerSecond}, {defaults.KeyPackagePerSecond, server.ProposalPerSecond}} {
		if pair[0] <= 0 || pair[0] >= pair[1] {
			t.Fatalf("client rate must stay below server: %v", pair)
		}
	}
}

func TestSpecializedBudgetsAreIndependentAndKeyPackagesArePerTarget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := New(Config{BaseURL: "https://example.invalid", InstanceID: testID(99)})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		for _, tc := range []struct {
			method, path string
			n            int
		}{
			{"POST", "groups/x/cursor", 20}, {"POST", "groups/x/commit", 30}, {"POST", "groups/x/proposal", 8},
			{"GET", "devices/a/keypackage", 8}, {"GET", "devices/b/keypackage", 8},
		} {
			for range tc.n {
				if err := c.waitRate(t.Context(), "token", tc.method, tc.path); err != nil {
					t.Fatal(err)
				}
			}
		}
		if time.Since(start) != 0 {
			t.Fatal("independent budgets competed")
		}
		if err := c.waitRate(t.Context(), "token", "GET", "devices/a/keypackage"); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) < 2500*time.Millisecond {
			t.Fatal("keypackage target limit ignored")
		}
	})
}

func TestRetiredSessionDoesNotRecreateTokenBuckets(t *testing.T) {
	c, err := New(Config{BaseURL: "https://example.invalid", InstanceID: testID(99), HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	c.discovered.Store(true)
	retired, err := c.SessionForDevice("old", testID(1))
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"renewed", "latest"} {
		current, err := c.SessionForDevice(token, testID(1))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []*Session{retired, current} {
			if err := s.AdvanceCursor(t.Context(), testID(2), 1, 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(c.devices) != 1 || len(c.buckets) != 1 {
		t.Fatalf("devices=%d buckets=%d", len(c.devices), len(c.buckets))
	}
	if c.buckets["device/"+testID(1).String()].write.Tokens() > 17 {
		t.Fatal("overlapping sessions reset budget")
	}
}

// Waiting resume/start requests must not reserve tokens ahead of pause/skip.
func TestImmediateControlTakesNextTokenBeforeInteractiveAndBackground(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newPriorityLimiter(1, 1)
		if !limiter.Allow() {
			t.Fatal("missing initial burst")
		}
		admitted := make(chan string, 3)
		start := time.Now()
		for _, waiter := range []struct {
			name string
			ctx  context.Context
		}{
			{"background", t.Context()},
			{"interactive", WithInteractive(t.Context())},
		} {
			go func() {
				if err := limiter.Wait(waiter.ctx); err != nil {
					t.Error(err)
					return
				}
				admitted <- waiter.name
			}()
		}
		synctest.Wait()
		go func() {
			if err := limiter.Wait(WithImmediateControl(t.Context())); err != nil {
				t.Error(err)
				return
			}
			admitted <- "control"
		}()
		synctest.Wait()
		for i, want := range []string{"control", "interactive", "background"} {
			if got := <-admitted; got != want {
				t.Fatalf("admission %d=%s, want %s", i, got, want)
			}
			if elapsed := time.Since(start); elapsed != time.Duration(i+1)*time.Second {
				t.Fatalf("admission %d after %s, rate cap changed", i, elapsed)
			}
		}
	})
}

func TestImmediateControlCancellationAndBackgroundLifetimeClearPriority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newPriorityLimiter(1, 1)
		limiter.Allow()
		ctx, cancel := context.WithCancel(WithImmediateControl(t.Context()))
		done := make(chan error, 1)
		go func() { done <- limiter.Wait(ctx) }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if limiter.immediate != 0 || limiter.interactive != 0 {
			t.Fatal("cancelled control waiter retained priority")
		}
		lifetime := WithBackground(WithImmediateControl(t.Context()))
		if immediate, _ := lifetime.Value(immediateControlContextKey{}).(bool); immediate {
			t.Fatal("background lifetime inherited immediate control")
		}
		if interactive, _ := lifetime.Value(interactiveContextKey{}).(bool); interactive {
			t.Fatal("background lifetime inherited interactive priority")
		}
	})
}

// Sustained ordinary command demand must leave a bounded share of the same
// device's tokens for maintenance, without consuming any token early.
func TestBackgroundProgressDuringSustainedInteractiveDemand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newPriorityLimiter(1, 1)
		limiter.Allow()
		admitted := make(chan string, 17)
		start := time.Now()
		go func() {
			if err := limiter.Wait(t.Context()); err != nil {
				admitted <- "error: " + err.Error()
				return
			}
			admitted <- "background"
		}()
		for range 16 {
			go func() {
				if err := limiter.Wait(WithInteractive(t.Context())); err != nil {
					admitted <- "error: " + err.Error()
					return
				}
				admitted <- "interactive"
			}()
		}
		synctest.Wait()
		for i := range 17 {
			want := "interactive"
			if i == 8 {
				want = "background"
			}
			if got := <-admitted; got != want {
				t.Fatalf("admission %d=%s, want %s", i+1, got, want)
			}
			if elapsed := time.Since(start); elapsed != time.Duration(i+1)*time.Second {
				t.Fatalf("admission %d after %s, rate cap changed", i+1, elapsed)
			}
		}
	})
}

func TestImmediateControlPrecedesReservedBackgroundToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newPriorityLimiter(1, 1)
		limiter.Allow()
		admitted := make(chan string, 11)
		go func() {
			if err := limiter.Wait(t.Context()); err != nil {
				admitted <- "error: " + err.Error()
				return
			}
			admitted <- "background"
		}()
		for range 9 {
			go func() {
				if err := limiter.Wait(WithInteractive(t.Context())); err != nil {
					admitted <- "error: " + err.Error()
					return
				}
				admitted <- "interactive"
			}()
		}
		synctest.Wait()
		for range 8 {
			if got := <-admitted; got != "interactive" {
				t.Fatalf("ordinary command burst interrupted by %s", got)
			}
		}
		go func() {
			if err := limiter.Wait(WithImmediateControl(t.Context())); err != nil {
				admitted <- "error: " + err.Error()
				return
			}
			admitted <- "control"
		}()
		synctest.Wait()
		for _, want := range []string{"control", "background", "interactive"} {
			if got := <-admitted; got != want {
				t.Fatalf("admission=%s, want %s", got, want)
			}
		}
	})
}

package server_test

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/server"
)

// Every test here drives a clock.Fake and never sleeps: the limiter passes
// l.clk.Now() into AllowN and TokensAt, so its behaviour is a pure function of
// the fake's time.
func newLimiter(t *testing.T, maxKeys int) (*server.RateLimiter, *clock.Fake) {
	t.Helper()
	c := config.Default().Limits.Rate
	c.MaxKeys = maxKeys
	clk := clock.NewFake(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	return server.NewRateLimiter(c, clk), clk
}

func TestARefusedCallConsumesNoToken(t *testing.T) {
	l, clk := newLimiter(t, 10)
	class := server.Class{Name: "login", PerSecond: 1, Burst: 1}
	if ok, _ := l.Allow(class, "a"); !ok {
		t.Fatal("first call refused")
	}
	ok, retry := l.Allow(class, "a")
	if ok {
		t.Fatal("second call allowed; burst is 1")
	}
	if retry != time.Second {
		t.Fatalf("retry = %s, want 1s: the bucket is a whole token short at 1/s", retry)
	}
	// Reserve() would have consumed a token here and pushed the next allowance
	// out; AllowN() must not (gap-49 verdicts 9 and 10).
	_, retryAgain := l.Allow(class, "a")
	if retryAgain != retry {
		t.Fatalf("a refused call consumed a token: retry went %s -> %s", retry, retryAgain)
	}
	// §5.3: the delay shrinks with the deficit. Nine tenths of the way back, the
	// answer is a tenth of a second, not another whole one.
	clk.Advance(900 * time.Millisecond)
	if ok, retryLate := l.Allow(class, "a"); ok {
		t.Fatal("the bucket refilled a whole token in 900ms at 1/s")
	} else if retryLate != 100*time.Millisecond {
		t.Fatalf("retry = %s 900ms in, want 100ms: the delay must follow the deficit", retryLate)
	}
	clk.Advance(100 * time.Millisecond)
	if ok, _ := l.Allow(class, "a"); !ok {
		t.Fatal("the bucket did not refill after the delay it named")
	}
}

func TestRetryAfterIsRoundedUpAndCappedByMaxRetryAfter(t *testing.T) {
	l, _ := newLimiter(t, 10)
	// 3/s: one token is 333.33ms, which must round UP to 334ms.
	class := server.Class{Name: "read", PerSecond: 3, Burst: 1}
	l.Allow(class, "a")
	_, retry := l.Allow(class, "a")
	if retry != 334*time.Millisecond {
		t.Fatalf("retry = %s, want 334ms (1/3s rounded up to the next millisecond)", retry)
	}
	// A very slow class is capped by limits.rate.max_retry_after.
	slow := server.Class{Name: "slow", PerSecond: 0.001, Burst: 1}
	l.Allow(slow, "a")
	_, capped := l.Allow(slow, "a")
	if capped > config.Default().Limits.Rate.MaxRetryAfter.Value() {
		t.Fatalf("retry = %s, over max_retry_after %s", capped, config.Default().Limits.Rate.MaxRetryAfter.Value())
	}
}

func TestSweepEvictsFullBucketsAndMaxKeysCaps(t *testing.T) {
	l, clk := newLimiter(t, 4)
	class := server.Class{Name: "read", PerSecond: 1000, Burst: 10}
	for _, key := range []string{"a", "b", "c", "d", "e", "f"} {
		l.Allow(class, key)
	}
	if l.Len() > 4 {
		t.Fatalf("limiter map holds %d keys, want at most max_keys = 4", l.Len())
	}
	// 10 tokens; a 1000/s refill restores them within 10 ms. Advance exactly
	// that, deterministically.
	clk.Advance(10 * time.Millisecond)
	l.Sweep()
	if l.Len() != 0 {
		t.Fatalf("Sweep left %d full buckets behind", l.Len())
	}
}

// A max_keys of 0 or less must never mean "no limiting". Taken literally,
// len(buckets) >= 0 is true for every arrival, so every key evicts the one
// resident bucket and is handed a brand-new full one: alternating two keys is
// then allowed for ever. config.Validate refuses a zero per_second and a burst
// below 1, but it says nothing about max_keys, so an operator who writes
// max_keys = 0 meaning "no cap on keys" would ship an instance with no login,
// register or invite throttling at all. The limiter clamps it to the documented
// default instead.
func TestNonPositiveMaxKeysStillLimits(t *testing.T) {
	for _, maxKeys := range []int{0, -1} {
		l, _ := newLimiter(t, maxKeys)
		class := server.Class{Name: "login", PerSecond: 1, Burst: 1}
		if ok, _ := l.Allow(class, "a"); !ok {
			t.Fatalf("max_keys = %d: first call for a refused", maxKeys)
		}
		if ok, _ := l.Allow(class, "b"); !ok {
			t.Fatalf("max_keys = %d: first call for b refused", maxKeys)
		}
		if ok, _ := l.Allow(class, "a"); ok {
			t.Fatalf("max_keys = %d: a's second call allowed; its bucket was evicted by b, so the limiter is off", maxKeys)
		}
		if ok, _ := l.Allow(class, "b"); ok {
			t.Fatalf("max_keys = %d: b's second call allowed; the limiter is off", maxKeys)
		}
		if l.Len() != 2 {
			t.Fatalf("max_keys = %d: limiter holds %d keys, want both buckets resident", maxKeys, l.Len())
		}
	}
}

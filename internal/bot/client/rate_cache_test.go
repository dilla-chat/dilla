package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/time/rate"

	"github.com/dilla-chat/dilla/internal/id"
)

func TestRateCacheDeviceChurnHasABound(t *testing.T) {
	c, err := New(Config{BaseURL: "https://example.test", InstanceID: testID(1)})
	if err != nil {
		t.Fatal(err)
	}
	deferred := 0
	for i := 0; i < 1030; i++ {
		if _, err = c.SessionForDevice(fmt.Sprintf("token-%d", i), id.New()); err != nil {
			deferred++
		}
	}
	if len(c.devices) > 1024 || len(c.buckets) > 1024 || deferred != 6 {
		t.Fatalf("unbounded identity churn: aliases=%d buckets=%d deferred=%d", len(c.devices), len(c.buckets), deferred)
	}
}

func cacheClient(t *testing.T, limits RateLimits) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: "https://example.test", InstanceID: testID(99), RateLimits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestRateCacheNeverEvictsSpentBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cacheClient(t, RateLimits{DeviceCacheLimit: 1, WriteBurst: 1, WritePerSecond: .001})
		if _, err := c.SessionForDevice("old", testID(1)); err != nil {
			t.Fatal(err)
		}
		if err := c.waitRate(t.Context(), "old", "POST", "groups/x/cursor"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Minute)
		if _, err := c.SessionForDevice("new", testID(2)); !errors.Is(err, ErrRateCacheCapacity) {
			t.Fatalf("spent bucket evicted: %v", err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		defer cancel()
		if err := c.waitRate(ctx, "old", "POST", "groups/x/cursor"); err == nil {
			t.Fatal("spent burst reset")
		}
		time.Sleep(20 * time.Minute)
		if _, err := c.SessionForDevice("new", testID(2)); err != nil {
			t.Fatal(err)
		}
		stats := c.RateCacheStats()
		if stats.DeviceBuckets != 1 || stats.TokenAliases != 1 || stats.Evictions != 1 {
			t.Fatalf("cleanup=%+v", stats)
		}
	})
}

func TestRateCacheRenewalPreservesWaitingPrioritiesAndSpentBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cacheClient(t, RateLimits{DeviceCacheLimit: 1, WriteBurst: 1, WritePerSecond: 1})
		old, err := c.SessionForDevice("old", testID(1))
		if err != nil {
			t.Fatal(err)
		}
		if err = c.waitRate(old.rateContext(t.Context()), "old", "POST", "groups/x/cursor"); err != nil {
			t.Fatal(err)
		}
		admitted := make(chan string, 3)
		for _, waiter := range []struct {
			name string
			ctx  context.Context
		}{
			{"background", t.Context()},
			{"interactive", WithInteractive(t.Context())},
		} {
			go func() {
				if waitErr := c.waitRate(old.rateContext(waiter.ctx), "old", "POST", "groups/x/cursor"); waitErr != nil {
					admitted <- waitErr.Error()
					return
				}
				admitted <- waiter.name
			}()
		}
		synctest.Wait()
		current, err := c.SessionForDevice("renewed", testID(1))
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			if waitErr := c.waitRate(current.rateContext(WithImmediateControl(t.Context())), "renewed", "POST", "groups/x/cursor"); waitErr != nil {
				admitted <- waitErr.Error()
				return
			}
			admitted <- "control"
		}()
		synctest.Wait()
		if _, err = c.SessionForDevice("unrelated", testID(2)); !errors.Is(err, ErrRateCacheCapacity) {
			t.Fatalf("cache replaced a waiting device: %v", err)
		}
		start := time.Now()
		for i, want := range []string{"control", "interactive", "background"} {
			if got := <-admitted; got != want {
				t.Fatalf("admission %d=%s, want %s", i, got, want)
			}
			if elapsed := time.Since(start); elapsed != time.Duration(i+1)*time.Second {
				t.Fatalf("renewal refreshed the shared budget: admission %d at %s", i, elapsed)
			}
		}
	})
}

func TestRateCacheTargetLimitsAndIdleEviction(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(fmt.Sprint(global), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				limits := RateLimits{DeviceCacheLimit: 4, TargetCacheLimit: 1, GlobalTargetCacheLimit: 8}
				if global {
					limits.TargetCacheLimit = 4
					limits.GlobalTargetCacheLimit = 1
				}
				c := cacheClient(t, limits)
				if err := c.waitRate(t.Context(), "token-a", "GET", "devices/first/keypackage"); err != nil {
					t.Fatal(err)
				}
				token := "token-a"
				if global {
					token = "token-b"
				}
				if err := c.waitRate(t.Context(), token, "GET", "devices/second/keypackage"); !errors.Is(err, ErrRateCacheCapacity) {
					t.Fatalf("unbounded targets: %v", err)
				}
				if c.RateCacheStats().TargetBuckets != 1 {
					t.Fatal("target cap exceeded")
				}
				time.Sleep(5 * time.Minute)
				if err := c.waitRate(t.Context(), token, "GET", "devices/second/keypackage"); err != nil {
					t.Fatal(err)
				}
				if c.RateCacheStats().TargetBuckets != 1 || c.RateCacheStats().Evictions == 0 {
					t.Fatalf("idle target not evicted: %+v", c.RateCacheStats())
				}
			})
		})
	}
}
func TestRateCachePinsActiveWaitersEvenAfterRefill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cacheClient(t, RateLimits{DeviceCacheLimit: 1, WriteBurst: 1, WritePerSecond: .001})
		if err := c.waitRate(t.Context(), "old", "POST", "x"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- c.waitRate(ctx, "old", "POST", "x") }()
		synctest.Wait()
		// Refill without waking the waiter's already-armed timer, exercising the
		// interval in which tokens are full but a request still owns this bucket.
		c.buckets[tokenKey("old")].write.SetLimit(rate.Inf)
		time.Sleep(5 * time.Minute)
		if err := c.waitRate(t.Context(), "new", "POST", "x"); !errors.Is(err, ErrRateCacheCapacity) {
			t.Fatalf("active waiter evicted: %v", err)
		}
		cancel()
		<-done
		time.Sleep(5 * time.Minute)
		if err := c.waitRate(t.Context(), "new", "POST", "x"); err != nil {
			t.Fatal(err)
		}
	})
}
func TestRateCacheBindingMergesSpentAnonymousBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cacheClient(t, RateLimits{DeviceCacheLimit: 2, WriteBurst: 2, WritePerSecond: .001})
		old, err := c.SessionForDevice("old", testID(1))
		if err != nil {
			t.Fatal(err)
		}
		if err = c.waitRate(t.Context(), "old", "POST", "x"); err != nil {
			t.Fatal(err)
		}
		if err = c.waitRate(t.Context(), "new", "POST", "x"); err != nil {
			t.Fatal(err)
		}
		current, err := c.SessionForDevice("new", testID(1))
		if err != nil {
			t.Fatal(err)
		}
		if old.deviceRateKey != current.deviceRateKey {
			t.Fatal("rotation split device budget")
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		defer cancel()
		if err = c.waitRate(ctx, "new", "POST", "x"); err == nil {
			t.Fatal("binding discarded anonymous spend")
		}
		if stats := c.RateCacheStats(); stats.DeviceBuckets != 1 || stats.TokenAliases != 1 {
			t.Fatalf("binding leaked: %+v", stats)
		}
	})
}
func TestRateCacheClockRollbackDoesNotMakeEntriesIdle(t *testing.T) {
	c := cacheClient(t, RateLimits{DeviceCacheLimit: 1})
	if _, err := c.SessionForDevice("old", testID(1)); err != nil {
		t.Fatal(err)
	}
	c.rateMu.Lock()
	b := c.buckets["device/"+testID(1).String()]
	b.lastUsed = time.Now().Add(time.Hour)
	c.rateMu.Unlock()
	if _, err := c.SessionForDevice("new", testID(2)); !errors.Is(err, ErrRateCacheCapacity) {
		t.Fatalf("future timestamp evicted: %v", err)
	}
}
func TestRateCacheConcurrentAdmissionRemainsBounded(t *testing.T) {
	c := cacheClient(t, RateLimits{DeviceCacheLimit: 8, TargetCacheLimit: 2, GlobalTargetCacheLimit: 8})
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Go(func() {
			token := id.New().String()
			session, err := c.SessionForDevice(token, id.New())
			if err == nil {
				if session == nil {
					t.Error("missing admitted session")
				}
				_ = c.waitRate(t.Context(), token, "GET", "devices/target/keypackage")
			}
			_ = c.RateCacheStats()
		})
	}
	wg.Wait()
	s := c.RateCacheStats()
	if s.DeviceBuckets > 8 || s.TokenAliases > 8 || s.TargetBuckets > 8 {
		t.Fatalf("concurrent bound violated: %+v", s)
	}
}
func TestRateCacheConfigurationBounds(t *testing.T) {
	defaults, err := (RateLimits{}).defaults()
	if err != nil || defaults.DeviceCacheLimit != 1024 || defaults.TargetCacheLimit != 2048 || defaults.GlobalTargetCacheLimit != 16384 {
		t.Fatalf("defaults=%+v %v", defaults, err)
	}
	for _, limits := range []RateLimits{{DeviceCacheLimit: -1}, {TargetCacheLimit: -1}, {GlobalTargetCacheLimit: -1}, {DeviceCacheLimit: 1 << 21}} {
		if _, err := New(Config{BaseURL: "https://example.test", InstanceID: testID(1), RateLimits: limits}); err == nil {
			t.Fatalf("bad cache limits accepted: %+v", limits)
		}
	}
}

func TestRateCacheCapacityDefersBeforeAllocatingAnonymousIdentity(t *testing.T) {
	c := cacheClient(t, RateLimits{DeviceCacheLimit: 4, GlobalTargetCacheLimit: 1})
	if err := c.waitRate(t.Context(), "first", "GET", "devices/a/keypackage"); err != nil {
		t.Fatal(err)
	}
	if err := c.waitRate(t.Context(), "second", "GET", "devices/b/keypackage"); !errors.Is(err, ErrRateCacheCapacity) {
		t.Fatal(err)
	}
	if s := c.RateCacheStats(); s.DeviceBuckets != 1 || s.TargetBuckets != 1 {
		t.Fatalf("failed admission allocated state: %+v", s)
	}
}

func TestRateCacheBindingMergesTargetSpendAndCounts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cacheClient(t, RateLimits{DeviceCacheLimit: 2, TargetCacheLimit: 2, GlobalTargetCacheLimit: 2, KeyPackageBurst: 2, KeyPackagePerSecond: .001})
		if _, err := c.SessionForDevice("old", testID(1)); err != nil {
			t.Fatal(err)
		}
		for _, token := range []string{"old", "new"} {
			if err := c.waitRate(t.Context(), token, "GET", "devices/same/keypackage"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := c.SessionForDevice("new", testID(1)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		defer cancel()
		if err := c.waitRate(ctx, "new", "GET", "devices/same/keypackage"); err == nil {
			t.Fatal("binding reset target spend")
		}
		if stats := c.RateCacheStats(); stats.TargetBuckets != 1 || stats.DeviceBuckets != 1 || stats.TokenAliases != 1 {
			t.Fatalf("target merge leaked: %+v", stats)
		}
	})
}
func TestRateCacheCannotRebindKnownTokenToAnotherDevice(t *testing.T) {
	c := cacheClient(t, RateLimits{})
	if _, err := c.SessionForDevice("bound", testID(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SessionForDevice("bound", testID(2)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("known token rebound: %v", err)
	}
}

func TestRateCacheBindingCanProgressAfterIdleTargetCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cacheClient(t, RateLimits{DeviceCacheLimit: 2, TargetCacheLimit: 1, GlobalTargetCacheLimit: 2})
		if _, err := c.SessionForDevice("old", testID(1)); err != nil {
			t.Fatal(err)
		}
		if err := c.waitRate(t.Context(), "old", "GET", "devices/a/keypackage"); err != nil {
			t.Fatal(err)
		}
		if err := c.waitRate(t.Context(), "new", "GET", "devices/b/keypackage"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.SessionForDevice("new", testID(1)); !errors.Is(err, ErrRateCacheCapacity) {
			t.Fatalf("recent target capacity bypassed: %v", err)
		}
		time.Sleep(5 * time.Minute)
		if _, err := c.SessionForDevice("new", testID(1)); err != nil {
			t.Fatalf("idle binding cannot progress: %v", err)
		}
		if s := c.RateCacheStats(); s.DeviceBuckets != 1 || s.TargetBuckets > 1 {
			t.Fatalf("bad merge counts: %+v", s)
		}
	})
}

func TestRateCacheConcurrentIdleCleanupPreservesBounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cacheClient(t, RateLimits{DeviceCacheLimit: 8, TargetCacheLimit: 1, GlobalTargetCacheLimit: 8})
		for i := 0; i < 8; i++ {
			token := fmt.Sprint(i)
			if _, err := c.SessionForDevice(token, id.New()); err != nil {
				t.Fatal(err)
			}
			if err := c.waitRate(t.Context(), token, "GET", "devices/a/keypackage"); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(5 * time.Minute)
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Go(func() {
				token := id.New().String()
				if _, err := c.SessionForDevice(token, id.New()); err == nil {
					_ = c.waitRate(t.Context(), token, "GET", "devices/b/keypackage")
				}
			})
		}
		wg.Wait()
		s := c.RateCacheStats()
		if s.DeviceBuckets > 8 || s.TargetBuckets > 8 || s.TokenAliases > 8 || s.Evictions < 8 {
			t.Fatalf("concurrent cleanup violated bounds: %+v", s)
		}
	})
}

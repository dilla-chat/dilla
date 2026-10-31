package client

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

const rateCacheIdle = 5 * time.Minute

var ErrRateCacheCapacity = errors.New("bot limiter cache capacity reached")

// The local admission refusal is definitive before HTTP execution. Its concrete
// type keeps an unrelated server-side 503 from becoming a retry-safe deferral.
type rateCacheCapacityError struct{ api *APIError }

func (e *rateCacheCapacityError) Error() string {
	return ErrRateCacheCapacity.Error() + ": " + e.api.Error()
}
func (e *rateCacheCapacityError) Unwrap() error        { return e.api }
func (e *rateCacheCapacityError) Is(target error) bool { return target == ErrRateCacheCapacity }

type RateCacheStats struct {
	DeviceBuckets, TargetBuckets, TokenAliases int
	Evictions, CapacityDeferrals               uint64
}

// RateCacheStats exposes aggregate lifecycle counts, never identities or tokens.
func (c *Client) RateCacheStats() RateCacheStats {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	return RateCacheStats{DeviceBuckets: len(c.buckets), TargetBuckets: c.targetBuckets, TokenAliases: len(c.devices), Evictions: c.cacheEvictions, CapacityDeferrals: c.cacheDeferrals}
}
func (c *Client) rateCapacityLocked() error {
	c.cacheDeferrals++
	// Log the first refusal and exponential milestones rather than flooding logs.
	if c.cacheDeferrals&(c.cacheDeferrals-1) == 0 {
		slog.Warn("bot limiter cache admission deferred", "capacity_deferrals", c.cacheDeferrals, "device_buckets", len(c.buckets), "target_buckets", c.targetBuckets, "evictions", c.cacheEvictions)
	}
	retry := uint64(1000)
	return &rateCacheCapacityError{api: &APIError{Status: http.StatusServiceUnavailable, Code: "E_UNAVAILABLE", Outcome: OutcomeDefinitive, RetryAfterMS: &retry}}
}
func fullLimiter(l *priorityLimiter, now time.Time) bool {
	return l.TokensAt(now) >= float64(l.Burst())
}
func (b *deviceLimiter) limiters() []*priorityLimiter {
	return []*priorityLimiter{b.write, b.read, b.message, b.commit, b.proposal}
}
func (b *deviceLimiter) idleFull(now time.Time) bool {
	if b.inUse != 0 || now.Sub(b.lastUsed) < rateCacheIdle {
		return false
	}
	for _, l := range b.limiters() {
		if !fullLimiter(l, now) {
			return false
		}
	}
	for _, l := range b.keypackages {
		if l.inUse != 0 || !fullLimiter(l, now) {
			return false
		}
	}
	return true
}
func (c *Client) removeDeviceLocked(key string, b *deviceLimiter) {
	delete(c.buckets, key)
	c.targetBuckets -= len(b.keypackages)
	for token, owner := range c.devices {
		if owner == key {
			delete(c.devices, token)
		}
	}
	c.cacheEvictions++
}
func (c *Client) evictDeviceLocked(now time.Time) bool {
	var victim string
	var oldest time.Time
	for key, b := range c.buckets {
		if !b.idleFull(now) {
			continue
		}
		if victim == "" || b.lastUsed.Before(oldest) || (b.lastUsed.Equal(oldest) && key < victim) {
			victim, oldest = key, b.lastUsed
		}
	}
	if victim == "" {
		return false
	}
	c.removeDeviceLocked(victim, c.buckets[victim])
	return true
}
func (c *Client) deviceBucketLocked(key string, now time.Time) (*deviceLimiter, error) {
	if b := c.buckets[key]; b != nil {
		return b, nil
	}
	if len(c.buckets) >= c.rates.DeviceCacheLimit && !c.evictDeviceLocked(now) {
		return nil, c.rateCapacityLocked()
	}
	b := &deviceLimiter{write: newPriorityLimiter(rate.Limit(c.rates.WritePerSecond), c.rates.WriteBurst), read: newPriorityLimiter(rate.Limit(c.rates.ReadPerSecond), c.rates.ReadBurst), message: newPriorityLimiter(rate.Limit(c.rates.MessagePerSecond), c.rates.MessageBurst), commit: newPriorityLimiter(rate.Limit(c.rates.CommitPerSecond), c.rates.CommitBurst), proposal: newPriorityLimiter(rate.Limit(c.rates.ProposalPerSecond), c.rates.ProposalBurst), keypackages: map[string]*priorityLimiter{}, lastUsed: now}
	c.buckets[key] = b
	return b, nil
}
func targetIdle(l *priorityLimiter, now time.Time) bool {
	return l.inUse == 0 && now.Sub(l.lastUsed) >= rateCacheIdle && fullLimiter(l, now)
}
func (c *Client) evictTargetLocked(only *deviceLimiter, now time.Time) bool {
	var victim string
	var owner *deviceLimiter
	var oldest time.Time
	for _, b := range c.buckets {
		if only != nil && only != b {
			continue
		}
		for target, l := range b.keypackages {
			if !targetIdle(l, now) {
				continue
			}
			if owner == nil || l.lastUsed.Before(oldest) {
				victim, owner, oldest = target, b, l.lastUsed
			}
		}
	}
	if owner == nil {
		return false
	}
	delete(owner.keypackages, victim)
	c.targetBuckets--
	c.cacheEvictions++
	return true
}
func (c *Client) targetBucketLocked(b *deviceLimiter, target string, now time.Time) (*priorityLimiter, error) {
	if l := b.keypackages[target]; l != nil {
		return l, nil
	}
	if len(b.keypackages) >= c.rates.TargetCacheLimit && !c.evictTargetLocked(b, now) {
		return nil, c.rateCapacityLocked()
	}
	if c.targetBuckets >= c.rates.GlobalTargetCacheLimit && !c.evictTargetLocked(nil, now) {
		return nil, c.rateCapacityLocked()
	}
	l := newPriorityLimiter(rate.Limit(c.rates.KeyPackagePerSecond), c.rates.KeyPackageBurst)
	l.lastUsed = now
	b.keypackages[target] = l
	c.targetBuckets++
	return l, nil
}
func (c *Client) canMergeTargetsLocked(dst, src *deviceLimiter, now time.Time) bool {
	n := len(dst.keypackages)
	for key := range src.keypackages {
		if dst.keypackages[key] == nil {
			n++
		}
	}
	if n <= c.rates.TargetCacheLimit {
		return true
	}
	// A failed binding must become admissible once old targets are safe to
	// evict, without discarding either identity's partially spent class budget.
	for target, l := range src.keypackages {
		if dst.keypackages[target] == nil && targetIdle(l, now) {
			delete(src.keypackages, target)
			c.targetBuckets--
			c.cacheEvictions++
			n--
			if n <= c.rates.TargetCacheLimit {
				return true
			}
		}
	}
	for target, l := range dst.keypackages {
		if src.keypackages[target] == nil && targetIdle(l, now) {
			delete(dst.keypackages, target)
			c.targetBuckets--
			c.cacheEvictions++
			n--
			if n <= c.rates.TargetCacheLimit {
				return true
			}
		}
	}
	return false
}
func mergeLimiter(dst, src *priorityLimiter, now time.Time) {
	// Binding a previously anonymous token charges its already-spent allowance
	// against the device too. This reservation intentionally carries debt forward;
	// Wait itself never reserves future tokens or grants another initial burst.
	spent := int(math.Ceil(float64(src.Burst()) - src.TokensAt(now)))
	if spent > 0 {
		dst.ReserveN(now, min(spent, dst.Burst()))
	}
}
func (c *Client) mergeBucketsLocked(dst, src *deviceLimiter, now time.Time) {
	destination, source := dst.limiters(), src.limiters()
	for i, l := range source {
		mergeLimiter(destination[i], l, now)
	}
	for key, l := range src.keypackages {
		if existing := dst.keypackages[key]; existing != nil {
			mergeLimiter(existing, l, now)
			c.targetBuckets--
		} else {
			dst.keypackages[key] = l
		}
	}
}

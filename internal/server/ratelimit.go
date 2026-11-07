package server

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
)

// Class is one metered thing: a name, a rate and a burst.
type Class struct {
	Name      string
	PerSecond float64
	Burst     int
}

// The incoming-webhook buckets (Bot v2 task 13). POST /hooks/{webhook_id}/{secret} is unauthenticated:
//   - WebhookAddressAllClass is spent by every request of a client address (RateKey of RealIP) and bounds the work
//     any one address causes.
//   - WebhookAddressClass and WebhookPrefixClass count failures only (fix round 2 S1): a request whose id or secret
//     did not match, or that failed before the check. An address whose failure bucket is empty is refused before any
//     work; a network's failure bucket only turns its failures' 401 into 429. A request with the right secret never
//     touches either, so junk from a network cannot throttle a valid sender on it. Guessing a random 128-bit id with
//     a 256-bit secret is hopeless at any of these rates.
//   - WebhookClass is keyed by the webhook id and spent only once the secret matched: 30 a minute with a burst of 5
//     (spec G5).
var (
	// WebhookAddressClass: one address's failures, 2 a second with a burst of 20.
	WebhookAddressClass = Class{Name: "webhook_address", PerSecond: 2, Burst: 20}
	// WebhookPrefixAllClass is spent by every webhook request of a network (RatePrefixKey: IPv4 /24, IPv6 /48),
	// before any work (fix round 3 S4/R2-1): a flood spread over a network's addresses meets one ceiling. 50 a
	// second with a burst of 250 is far above what valid senders on one network send to webhooks at 0.5 a second
	// each, so junk must be very heavy before it can throttle them.
	WebhookPrefixAllClass = Class{Name: "webhook_prefix_all", PerSecond: 50, Burst: 250}
	// WebhookAddressAllClass is spent by every webhook request of an address, whatever its outcome (fix round 2 S1):
	// 10 a second with a burst of 50 bounds the work one address causes, valid secrets included, well above what a
	// host feeding even a dozen webhooks at their own 0.5 a second sends.
	WebhookAddressAllClass = Class{Name: "webhook_address_all", PerSecond: 10, Burst: 50}
	// WebhookPrefixClass is keyed by RatePrefixKey (IPv4 /24, IPv6 /48) and counts the network's failures
	// (fix round 1 M-3): one network spreading attempts over its 65 536 /64s still meets one bucket.
	WebhookPrefixClass = Class{Name: "webhook_prefix", PerSecond: 20, Burst: 100}
	// WebhookAvatarClass (per webhook) and WebhookAvatarGlobalClass (the instance) meter avatar fetches (fix round
	// 1): a webhook starts at most one fetch per 30 s after a burst of 2, and the instance at most 2 a second
	// after a burst of 10. A refused fetch posts without an avatar.
	WebhookAvatarClass       = Class{Name: "webhook_avatar", PerSecond: 1.0 / 30, Burst: 2}
	WebhookAvatarGlobalClass = Class{Name: "webhook_avatar_all", PerSecond: 2, Burst: 10}
	// WebhookAvatarCommunityClass (fix round 2 N-2) is one community's share: one fetch per 5 s after a burst of 5,
	// so a community's 50 webhooks cannot fill the instance's 2 a second by themselves.
	WebhookAvatarCommunityClass = Class{Name: "webhook_avatar_community", PerSecond: 0.2, Burst: 5}
	WebhookClass                = Class{Name: "webhook", PerSecond: 0.5, Burst: 5}
)

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// RateLimiter is an in-process token-bucket map keyed by (class, subject).
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	cfg     config.Rate
	clk     clock.Clock
	maxWait time.Duration
}

// defaultMaxKeys is config's documented limits.rate.max_keys default, repeated
// here as the clamp for a non-positive value.
const defaultMaxKeys = 100000

func NewRateLimiter(c config.Rate, clk clock.Clock) *RateLimiter {
	// A max_keys of 0 or less would read as "every arrival is over the cap":
	// each new key evicts the one resident bucket and gets a full new one, so
	// alternating keys are never refused and rate limiting is silently OFF.
	// config.Validate refuses a zero per_second and a burst below 1 but says
	// nothing about max_keys, so an operator writing max_keys = 0 to mean "no
	// cap on keys" must not disarm login, register and invite throttling.
	if c.MaxKeys < 1 {
		c.MaxKeys = defaultMaxKeys
	}
	return &RateLimiter{buckets: make(map[string]*bucket), cfg: c, clk: clk, maxWait: c.MaxRetryAfter.Value()}
}

// Allow refuses with AllowN(now, 1), never with Reserve(): a denied AllowN
// mutates nothing, while Reserve consumes a token even when the caller intends
// to refuse, so a client that is already being throttled gets throttled harder
// for asking (gap-49 verdicts 9 and 10).
//
// Every call passes l.clk.Now() rather than letting x/time/rate read the wall
// clock: a limiter that ignores the injected clock makes its own tests
// timing-dependent, which is the whole reason the seam exists.
func (l *RateLimiter) Allow(class Class, key string) (bool, time.Duration) {
	if !l.cfg.Enabled {
		return true, 0
	}
	now := l.clk.Now()
	b := l.bucketFor(class, key, now)
	if b.lim.AllowN(now, 1) {
		return true, 0
	}
	return false, l.retryAfter(b.lim.TokensAt(now), class)
}

func (l *RateLimiter) bucketFor(class Class, key string, now time.Time) *bucket {
	k := class.Name + "\x00" + key
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[k]
	if !ok {
		if len(l.buckets) >= l.cfg.MaxKeys {
			l.evictOldestLocked()
		}
		b = &bucket{lim: rate.NewLimiter(rate.Limit(class.PerSecond), class.Burst)}
		l.buckets[k] = b
	}
	b.seen = now
	return b
}

// Take is Allow for a token the caller may give back: it spends one token of (class, key) now and, when it did,
// returns refund, which restores it (once). A refused Take spends nothing, as a refused Allow does. It is how a
// failure bucket is reserved before work whose outcome is not yet known and refunded when the work succeeded
// (webhook fix round 3 R2-2): requests running at the same time each hold a token, so they cannot all pass a check
// that only one token would have allowed.
func (l *RateLimiter) Take(class Class, key string) (bool, time.Duration, func()) {
	if !l.cfg.Enabled {
		return true, 0, func() {}
	}
	now := l.clk.Now()
	b := l.bucketFor(class, key, now)
	r := b.lim.ReserveN(now, 1)
	if !r.OK() || r.DelayFrom(now) > 0 {
		r.CancelAt(now)
		return false, l.retryAfter(b.lim.TokensAt(now), class), nil
	}
	var once sync.Once
	return true, 0, func() { once.Do(func() { l.refund(b, class) }) }
}

// refund gives one token back to b, never past its burst. Reservation.CancelAt cannot do it: it restores nothing
// once its time to act has passed, which for an immediately granted reservation is the instant it was taken, so a
// refund after any work at all would be a silent no-op. AllowN with -1 adds one token at the current time; the
// burst check and the add run under l.mu so two refunds cannot both pass the check. A concurrent Allow only takes
// tokens, so it cannot push the bucket over its burst either.
func (l *RateLimiter) refund(b *bucket, class Class) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clk.Now()
	if b.lim.TokensAt(now) > float64(class.Burst)-1 {
		return
	}
	b.lim.AllowN(now, -1)
}

// retryAfter is §5.3's number: computed from the bucket's DEFICIT and rounded
// up, not from the constant 1/rate. The deficit is how far short of one token
// the bucket is right now: a caller refused the instant the bucket emptied
// waits a full 1/rate, one refused 90% of the way back waits a tenth of it, and
// telling both the same number sends the second one away for nine times longer
// than it needed to.
func (l *RateLimiter) retryAfter(tokens float64, class Class) time.Duration {
	deficit := 1 - tokens
	if deficit < 0 {
		deficit = 0
	}
	ms := math.Ceil(deficit / class.PerSecond * 1000) // rounded UP to the next millisecond
	if ms < 1 {
		ms = 1
	}
	wait := time.Duration(ms) * time.Millisecond
	if l.maxWait > 0 && wait > l.maxWait {
		wait = l.maxWait
	}
	return wait
}

// Tokens reports how much of (class, key)'s bucket is left WITHOUT spending any
// of it, so a caller may gate on a bucket some other code path is the writer of.
// A key with no bucket yet, and every key while rate limiting is disabled, has
// its full burst.
func (l *RateLimiter) Tokens(class Class, key string) float64 {
	if !l.cfg.Enabled {
		return float64(class.Burst)
	}
	l.mu.Lock()
	b, ok := l.buckets[class.Name+"\x00"+key]
	l.mu.Unlock()
	if !ok {
		return float64(class.Burst)
	}
	return b.lim.TokensAt(l.clk.Now())
}

// Peek is Allow's answer without Allow's token: the same (allowed, retry-after)
// pair, computed from Tokens. It creates no bucket and mutates nothing, so
// peeking a hundred times reads the same as peeking once.
func (l *RateLimiter) Peek(class Class, key string) (bool, time.Duration) {
	tokens := l.Tokens(class, key)
	if tokens >= 1 {
		return true, 0
	}
	return false, l.retryAfter(tokens, class)
}

// Sweep drops every bucket that is full, which is behaviour-preserving: a full
// bucket is indistinguishable from a fresh one.
func (l *RateLimiter) Sweep() {
	now := l.clk.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.lim.TokensAt(now) >= float64(b.lim.Burst()) {
			delete(l.buckets, k)
		}
	}
}

// Config returns the rate configuration this limiter was built with, so a
// caller can look its own class up without carrying the config twice.
func (l *RateLimiter) Config() config.Rate { return l.cfg }

func (l *RateLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

func (l *RateLimiter) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, b := range l.buckets {
		if oldest.IsZero() || b.seen.Before(oldest) {
			oldestKey, oldest = k, b.seen
		}
	}
	delete(l.buckets, oldestKey)
}

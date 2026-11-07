package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/dilla-chat/dilla/internal/id"
)

// RateLimits may be overridden by [rate_limits] in bot.toml. Zero fields use
// conservative defaults below the instance's default per-device allowances.
type RateLimits struct {
	DeviceCacheLimit       int     `toml:"device_cache_limit"`
	TargetCacheLimit       int     `toml:"target_cache_limit"`
	GlobalTargetCacheLimit int     `toml:"global_target_cache_limit"`
	WritePerSecond         float64 `toml:"write_per_second"`
	WriteBurst             int     `toml:"write_burst"`
	ReadPerSecond          float64 `toml:"read_per_second"`
	ReadBurst              int     `toml:"read_burst"`
	MessagePerSecond       float64 `toml:"message_per_second"`
	MessageBurst           int     `toml:"message_burst"`
	CommitPerSecond        float64 `toml:"commit_per_second"`
	CommitBurst            int     `toml:"commit_burst"`
	ProposalPerSecond      float64 `toml:"proposal_per_second"`
	ProposalBurst          int     `toml:"proposal_burst"`
	KeyPackagePerSecond    float64 `toml:"keypackage_per_second"`
	KeyPackageBurst        int     `toml:"keypackage_burst"`
}

// Validate checks operator overrides without constructing a client or contacting an instance.
func (r RateLimits) Validate() error { _, err := r.defaults(); return err }

func (r RateLimits) defaults() (RateLimits, error) {
	for _, limit := range []struct {
		value         *int
		fallback, max int
	}{
		{&r.DeviceCacheLimit, 1024, 1 << 20}, {&r.TargetCacheLimit, 2048, 1 << 20}, {&r.GlobalTargetCacheLimit, 16384, 1 << 24},
	} {
		if *limit.value == 0 {
			*limit.value = limit.fallback
		}
		if *limit.value < 1 || *limit.value > limit.max {
			return r, ErrInvalidConfig
		}
	}
	for _, v := range []struct {
		rate         *float64
		burst        *int
		defaultRate  float64
		defaultBurst int
	}{
		{&r.CommitPerSecond, &r.CommitBurst, 1.5, 30}, {&r.ProposalPerSecond, &r.ProposalBurst, .4, 8}, {&r.KeyPackagePerSecond, &r.KeyPackageBurst, .4, 8},
		{&r.WritePerSecond, &r.WriteBurst, 1.5, 20}, {&r.ReadPerSecond, &r.ReadBurst, 8, 40}, {&r.MessagePerSecond, &r.MessageBurst, .8, 10},
	} {
		if *v.rate == 0 {
			*v.rate = v.defaultRate
		}
		if *v.burst == 0 {
			*v.burst = v.defaultBurst
		}
		if *v.rate < 0 || math.IsNaN(*v.rate) || math.IsInf(*v.rate, 0) || *v.burst < 1 {
			return r, ErrInvalidConfig
		}
	}
	return r, nil
}

type interactiveContextKey struct{}
type immediateControlContextKey struct{}

// WithInteractive marks a person's command and its downstream requests.
func WithInteractive(ctx context.Context) context.Context {
	return context.WithValue(ctx, interactiveContextKey{}, true)
}

// WithImmediateControl gives pause and skip requests first use of the next token.
// It preserves all rate caps and also marks downstream requests as interactive.
func WithImmediateControl(ctx context.Context) context.Context {
	return context.WithValue(WithInteractive(ctx), immediateControlContextKey{}, true)
}

// RequestPriority is the scheduling class shared with serialized helper work.
// It does not bypass any HTTP rate cap.
func RequestPriority(ctx context.Context) int {
	if immediate, _ := ctx.Value(immediateControlContextKey{}).(bool); immediate {
		return 2
	}
	if interactive, _ := ctx.Value(interactiveContextKey{}).(bool); interactive {
		return 1
	}
	return 0
}

// priorityLimiter never reserves future tokens. Immediate control waiters take
// the next token first. Ordinary interactive requests precede background work,
// with one background grant after eight ordinary grants while background waits.
// All three priorities spend the same bucket, including its initial burst.
type priorityLimiter struct {
	*rate.Limiter
	mu             sync.Mutex
	interactive    int
	immediate      int
	background     int
	interactiveRun int
	changed        chan struct{}
	// Cache lifecycle fields are protected by Client.rateMu.
	lastUsed time.Time
	inUse    int
}

func newPriorityLimiter(limit rate.Limit, burst int) *priorityLimiter {
	return &priorityLimiter{Limiter: rate.NewLimiter(limit, burst), changed: make(chan struct{})}
}

func (l *priorityLimiter) notifyLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *priorityLimiter) Wait(ctx context.Context) error {
	interactive, _ := ctx.Value(interactiveContextKey{}).(bool)
	immediate, _ := ctx.Value(immediateControlContextKey{}).(bool)
	l.mu.Lock()
	if interactive {
		l.interactive++
		if immediate {
			l.immediate++
		}
	} else {
		l.background++
	}
	l.notifyLocked()
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if interactive {
			l.interactive--
			if immediate {
				l.immediate--
			}
		} else {
			l.background--
			if l.background == 0 {
				l.interactiveRun = 0
			}
		}
		l.notifyLocked()
		l.mu.Unlock()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := CooperativeCheckpoint(ctx); err != nil {
			return err
		}
		l.mu.Lock()
		backgroundTurn := l.background > 0 && l.interactiveRun >= 8
		eligible := immediate || l.immediate == 0 &&
			(interactive && !backgroundTurn || !interactive && (l.interactive == 0 || backgroundTurn))
		if eligible && l.Allow() {
			if !interactive {
				l.interactiveRun = 0
			} else if !immediate && l.background > 0 {
				l.interactiveRun++
			}
			l.mu.Unlock()
			return nil
		}
		changed := l.changed
		delay := time.Duration(math.Ceil(max(0, 1-l.Tokens()) / float64(l.Limit()) * float64(time.Second)))
		// A background waiter with available tokens still yields to interactive demand.
		if delay <= 0 {
			delay = time.Millisecond
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < delay {
			l.mu.Unlock()
			return context.DeadlineExceeded
		}
		// A waiting admission is a safe boundary; observe cooperative stops promptly.
		if ready, _ := ctx.Value(cooperativeStopKey{}).(func() bool); ready != nil {
			delay = min(delay, 25*time.Millisecond)
		}
		l.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

type deviceLimiter struct {
	write, read, message, commit, proposal *priorityLimiter
	keypackages                            map[string]*priorityLimiter
	lastUsed                               time.Time
	inUse                                  int
}

func tokenKey(token string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(token))) }

// SessionForDevice keeps one budget through session renewal and restoration.
// Session is still available for callers without a known device identity.
func (c *Client) SessionForDevice(token string, device id.ID) (*Session, error) {
	s, err := c.Session(token)
	if err != nil {
		return nil, err
	}
	if device.IsZero() {
		return nil, ErrInvalidConfig
	}
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	key, deviceKey := tokenKey(token), "device/"+device.String()
	if owner, ok := c.devices[key]; ok && owner != deviceKey {
		return nil, ErrInvalidConfig
	}
	now := time.Now()
	fallback, deviceBucket := c.buckets[key], c.buckets[deviceKey]
	if fallback != nil && deviceBucket != nil && fallback != deviceBucket {
		if fallback.inUse > 0 || !c.canMergeTargetsLocked(deviceBucket, fallback, now) {
			return nil, c.rateCapacityLocked()
		}
		c.mergeBucketsLocked(deviceBucket, fallback, now)
		delete(c.buckets, key)
	} else if deviceBucket == nil && fallback != nil {
		c.buckets[deviceKey] = fallback
		delete(c.buckets, key)
	} else if deviceBucket == nil {
		if _, err := c.deviceBucketLocked(deviceKey, now); err != nil {
			return nil, err
		}
	}
	c.buckets[deviceKey].lastUsed = now
	for retired, owner := range c.devices {
		if owner == deviceKey && retired != key {
			delete(c.devices, retired)
			delete(c.buckets, retired)
		}
	}
	c.devices[key] = deviceKey
	s.deviceRateKey = deviceKey
	return s, nil
}

// Mirror internal/api/dsmeter.go:routeClass and the explicit upload routes in
// internal/api/messages.go and internal/api/bot_message.go. Bot uploads are WRITE,
// ordinary ciphertext /message and readable channel /messages uploads are MESSAGE.
// Specialized sequencer routes use their explicit server classes. KeyPackage
// fetches spend both read and per-(requester,target) budgets.
func requestClass(method, path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if method == http.MethodGet && len(parts) == 3 && parts[0] == "devices" && parts[2] == "keypackage" {
		return "keypackage"
	}
	if method == http.MethodPost {
		if len(parts) == 3 && parts[0] == "groups" {
			switch parts[2] {
			case "commit", "resync", "heal":
				return "commit"
			case "proposal":
				return "proposal"
			}
		}
		if len(parts) == 6 && parts[0] == "bot-runtime" && parts[1] == "installations" && parts[3] == "interaction-groups" && parts[5] == "heal" {
			return "commit"
		}
		// dillad meters the Bot's sealed answers and state hints as bot_runtime at the read rate (dsmeter.go): one
		// answer per command must not wait on the write bucket (Bot v2 Task 15).
		if len(parts) == 2 && parts[0] == "bot-runtime" && (parts[1] == "replies" || parts[1] == "state-hints") {
			return "read"
		}
	}
	if method == http.MethodGet || method == http.MethodHead {
		return "read"
	}
	if method == http.MethodPost && len(parts) == 3 && ((parts[0] == "groups" && parts[2] == "message") || (parts[0] == "channels" && parts[2] == "messages")) {
		return "message"
	}
	return "write"
}

func (c *Client) waitRate(ctx context.Context, token, method, path string) error {
	if err := CooperativeCheckpoint(ctx); err != nil {
		return err
	}
	key := tokenKey(token)
	c.rateMu.Lock()
	if device, ok := ctx.Value(deviceRateContextKey{}).(string); ok {
		key = device
	} else if device, ok := c.devices[key]; ok {
		key = device
	}
	now := time.Now()
	class := requestClass(method, path)
	// Do not allocate a new anonymous identity when its first target request
	// cannot be admitted. Known device bindings already own their bounded slot.
	if c.buckets[key] == nil && class == "keypackage" && c.targetBuckets >= c.rates.GlobalTargetCacheLimit && !c.evictTargetLocked(nil, now) {
		err := c.rateCapacityLocked()
		c.rateMu.Unlock()
		return err
	}
	b, err := c.deviceBucketLocked(key, now)
	if err != nil {
		c.rateMu.Unlock()
		return err
	}
	var keypackage *priorityLimiter
	if class == "keypackage" {
		target := strings.Split(strings.Trim(path, "/"), "/")[1]
		keypackage, err = c.targetBucketLocked(b, target, now)
		if err != nil {
			c.rateMu.Unlock()
			return err
		}
		keypackage.inUse++
		keypackage.lastUsed = now
	}
	b.inUse++
	b.lastUsed = now
	c.rateMu.Unlock()
	defer func() {
		c.rateMu.Lock()
		defer c.rateMu.Unlock()
		b.inUse--
		b.lastUsed = time.Now()
		if keypackage != nil {
			keypackage.inUse--
			keypackage.lastUsed = b.lastUsed
		}
	}()
	limiter := b.write
	switch class {
	case "commit":
		limiter = b.commit
	case "proposal":
		limiter = b.proposal
	case "keypackage", "read":
		limiter = b.read
	case "message":
		limiter = b.message
	}
	if err := waitLimiter(ctx, limiter); err != nil {
		return err
	}
	// Read admission must finish before spending the scarcer per-target token.
	if keypackage != nil {
		return waitLimiter(ctx, keypackage)
	}
	return nil
}

func waitLimiter(ctx context.Context, limiter *priorityLimiter) error {
	if err := limiter.Wait(ctx); err != nil {
		if errors.Is(err, ErrCooperativeStop) {
			return err
		}
		if deficit := 1 - limiter.Tokens(); deficit > 0 {
			retry := uint64(math.Ceil(deficit / float64(limiter.Limit()) * 1000))
			cause := ctx.Err()
			if cause == nil {
				cause = context.DeadlineExceeded
			}
			return errors.Join(&APIError{Status: http.StatusTooManyRequests, Code: "E_RATE_LIMITED", Outcome: OutcomeDefinitive, RetryAfterMS: &retry}, cause)
		}
		return err
	}
	return nil
}

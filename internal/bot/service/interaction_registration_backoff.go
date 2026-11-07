package service

import (
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

const (
	interactionRegistrationBackoffBase = 30 * time.Second
	interactionRegistrationBackoffMax  = time.Hour
)

type interactionRegistrationBackoffKey struct {
	installation, user, episode, device id.ID
}

type interactionRegistrationBackoffEntry struct {
	failures  uint64
	notBefore time.Time
}

// InteractionRegistrationBackoff holds devices that have no KeyPackage to take
// out of the registration passes until their retry time. It is in memory so the
// durable queue keeps its wire format; share one across the per-pass
// coordinators so backoff survives them. A restart resets it, which costs one
// extra attempt and one repeat warning per device. The retry times carry Go's
// monotonic reading, so a wall-clock jump cannot shorten or stretch them.
type InteractionRegistrationBackoff struct {
	mu      sync.Mutex
	entries map[interactionRegistrationBackoffKey]interactionRegistrationBackoffEntry
}

func NewInteractionRegistrationBackoff() *InteractionRegistrationBackoff {
	return &InteractionRegistrationBackoff{entries: map[interactionRegistrationBackoffKey]interactionRegistrationBackoffEntry{}}
}

func interactionRegistrationBackoffDelay(failures uint64) time.Duration {
	delay := interactionRegistrationBackoffBase
	for ; failures > 1 && delay < interactionRegistrationBackoffMax; failures-- {
		delay *= 2
	}
	return min(delay, interactionRegistrationBackoffMax)
}

// due reports whether the device may be attempted at now.
func (b *InteractionRegistrationBackoff) due(key interactionRegistrationBackoffKey, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[key]
	return !ok || !now.Before(entry.notBefore)
}

// fail records one more attempt that found no KeyPackage and reports whether
// the device just entered backoff, so the caller warns once.
func (b *InteractionRegistrationBackoff) fail(key interactionRegistrationBackoffKey, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[key]
	entry.failures++
	entry.notBefore = now.Add(interactionRegistrationBackoffDelay(entry.failures))
	b.entries[key] = entry
	return !ok
}

func (b *InteractionRegistrationBackoff) clear(key interactionRegistrationBackoffKey) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, key)
}

// retain drops the job's entries for devices that are no longer candidates.
func (b *InteractionRegistrationBackoff) retain(installation, user, episode id.ID, devices []id.ID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.entries {
		if key.installation != installation || key.user != user || key.episode != episode {
			continue
		}
		listed := false
		for _, device := range devices {
			listed = listed || device == key.device
		}
		if !listed {
			delete(b.entries, key)
		}
	}
}

func (b *InteractionRegistrationBackoff) dropJob(installation, user, episode id.ID) {
	b.retain(installation, user, episode, nil)
}

// NextDue is the earliest time a backed-off device of installation is due again, and whether one is backed off
// (Bot v2 task 14 fix round 1, M-3: the Bot retries a stuck registration on this per-device backoff, not on a timer).
func (b *InteractionRegistrationBackoff) NextDue(installation id.ID) (time.Time, bool) {
	if b == nil {
		return time.Time{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var next time.Time
	found := false
	for key, entry := range b.entries {
		if key.installation != installation {
			continue
		}
		if !found || entry.notBefore.Before(next) {
			next, found = entry.notBefore, true
		}
	}
	return next, found
}

func (b *InteractionRegistrationBackoff) has(key interactionRegistrationBackoffKey) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.entries[key]
	return ok
}

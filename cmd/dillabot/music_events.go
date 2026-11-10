//go:build linux

package main

import (
	"sync"

	"github.com/dilla-chat/dilla/internal/id"
)

// musicEvents carries the Bot's pushed signals the music path acts on (Bot v2 Task 19 fix round 1):
//   - op 54 (the community, its settings or the Bot's own installation changed): a generation per installation,
//     so the music path's reused installation row and policy version are read again after it (M1, M2) instead of
//     after a time window;
//   - op 16/18 (a handshake or a new epoch in a group): a nudge to the room playing in that call group, so the Bot
//     applies the commit and re-keys its sender at once (C1); the room's monitor tick catches anything a lost hint
//     missed.
type musicEvents struct {
	mu          sync.Mutex
	generations map[id.ID]uint64
	watchers    map[id.ID]map[id.ID]map[*musicWatch]struct{}
}

type musicWatch struct{ nudge func() }

var musicSignals = &musicEvents{}

// communityChanged records an op 54 for installation.
func (m *musicEvents) communityChanged(installation id.ID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generations == nil {
		m.generations = map[id.ID]uint64{}
	}
	m.generations[installation]++
}

// generation is installation's op 54 count: a value read under an older generation is stale.
func (m *musicEvents) generation(installation id.ID) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generations[installation]
}

// watchCallGroup calls nudge whenever group gets a handshake or epoch hint, until the returned stop is called.
func (m *musicEvents) watchCallGroup(installation, group id.ID, nudge func()) (stop func()) {
	w := &musicWatch{nudge: nudge}
	m.mu.Lock()
	if m.watchers == nil {
		m.watchers = map[id.ID]map[id.ID]map[*musicWatch]struct{}{}
	}
	groups := m.watchers[installation]
	if groups == nil {
		groups = map[id.ID]map[*musicWatch]struct{}{}
		m.watchers[installation] = groups
	}
	if groups[group] == nil {
		groups[group] = map[*musicWatch]struct{}{}
	}
	groups[group][w] = struct{}{}
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if set := m.watchers[installation][group]; set != nil {
				delete(set, w)
				if len(set) == 0 {
					delete(m.watchers[installation], group)
				}
			}
		})
	}
}

// callGroupChanged nudges every room watching group (a handshake or epoch hint arrived for it).
func (m *musicEvents) callGroupChanged(installation, group id.ID) {
	m.mu.Lock()
	var nudges []func()
	for w := range m.watchers[installation][group] {
		nudges = append(nudges, w.nudge)
	}
	m.mu.Unlock()
	for _, nudge := range nudges {
		nudge()
	}
}

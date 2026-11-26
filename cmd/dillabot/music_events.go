//go:build linux

package main

import (
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// musicEvents carries the Bot's pushed signals the music path acts on (Bot v2 Task 19 fix round 1):
//   - op 54 (the community, its settings or the Bot's own installation changed): a generation per installation,
//     so the music path's reused installation row and policy version are read again after it (M1, M2) instead of
//     after a time window;
//   - op 16/18 (a handshake or a new epoch in a group): a nudge to the room playing in that call group, so the Bot
//     applies the commit and re-keys its sender at once (C1); the room's monitor tick catches anything a lost hint
//     missed.
//   - op 16/18 and op 19 (a handshake, an epoch, a message in a group): the group's next sequence as of that frame,
//     per gateway connection (hintedHead), so a music start's catch-up need not read dillad's head (Task 3).
type musicEvents struct {
	mu          sync.Mutex
	generations map[id.ID]uint64
	watchers    map[id.ID]map[id.ID]map[*musicWatch]struct{}
	heads       map[id.ID]map[id.ID]hintedHead
}

// hintedHead is a group's next sequence as the hints of one gateway connection showed it: the newest hinted frame's
// sequence plus one, or a remote read's head while that connection stayed up. at is when it was last confirmed.
type hintedHead struct {
	next, conn uint64
	at         time.Time
}

// musicHintedHeadMaxAge bounds how long a hinted head stands for a start without a newer hint or a read (the monitor
// tick's, every 2 s while a track plays) confirming it. A hint can be lost with the connection up (dillad drops a group
// fan-out whose scope check fails), so only starts use the head; the tick always reads.
const musicHintedHeadMaxAge = 10 * time.Second

// groupHinted records a frame of group at next-1 that arrived on gateway connection conn. A head of an older connection
// is replaced: what a dropped connection showed says nothing about what came after it.
func (m *musicEvents) groupHinted(installation, group id.ID, next, conn uint64, at time.Time) {
	if next == 0 || conn == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.heads == nil {
		m.heads = map[id.ID]map[id.ID]hintedHead{}
	}
	groups := m.heads[installation]
	if groups == nil {
		groups = map[id.ID]hintedHead{}
		m.heads[installation] = groups
	}
	head := groups[group]
	if head.conn != conn {
		head = hintedHead{conn: conn}
	}
	head.next, head.at = max(head.next, next), at
	groups[group] = head
}

// groupRead records a remote read of group's head, made while gateway connection conn was live from its start to its
// answer. It renews (and may raise) a head conn's hints made; it never makes one, so a head always rests on a hint of
// the live connection.
func (m *musicEvents) groupRead(installation, group id.ID, next, conn uint64, at time.Time) {
	if next == 0 || conn == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	head, ok := m.heads[installation][group]
	if !ok || head.conn != conn {
		return
	}
	head.next, head.at = max(head.next, next), at
	m.heads[installation][group] = head
}

// hintedHead is group's hinted next sequence while it stands: conn is the gateway's live connection (not 0), the head
// came from that connection, and it was confirmed within musicHintedHeadMaxAge of now.
func (m *musicEvents) hintedHead(installation, group id.ID, conn uint64, now time.Time) (uint64, bool) {
	if conn == 0 {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	head, ok := m.heads[installation][group]
	if !ok || head.conn != conn || now.Sub(head.at) > musicHintedHeadMaxAge || now.Before(head.at) {
		return 0, false
	}
	return head.next, true
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

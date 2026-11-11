package api

import (
	"context"
	"slices"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// BeginCommitForTest takes the device's one commit slot, as a commit upload in flight does.
func (h *Groups) BeginCommitForTest(device id.ID) (func(), error) { return h.beginCommit(device) }

// HandleReadLiveForTest is Handle's work for a participant_joined or a track_published whose call
// Handle read live as row: a test ends the call between that read and this work (#37 CI).
func (c *CallEvents) HandleReadLiveForTest(ctx context.Context, ev *livekit.WebhookEvent, row store.VoiceSessionRow) {
	p := ev.GetParticipant()
	switch ev.GetEvent() {
	case webhook.EventParticipantJoined:
		c.joined(ctx, row, p.GetIdentity(), p.GetSid(), joinedAt(p))
	case webhook.EventTrackPublished:
		c.trackPublished(ctx, row, p.GetIdentity(), ev.GetTrack())
	}
}

// SetStartHookForTest makes start call f at its named points ("mint", "respond").
func (h *Calls) SetStartHookForTest(f func(stage string)) { h.startHook = f }

// SetEndHookForTest makes endCall call f at its named points ("closed": the call group is closed and
// the voice session not yet ended).
func (h *Calls) SetEndHookForTest(f func(stage string)) { h.endHook = f }

// DeferredLeavesForTest is how many leaves the call events hold back for a newer session.
func (c *CallEvents) DeferredLeavesForTest() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.deferred)
}

// TakeOverVoiceForTest runs the voice takeover for dev of user, reported joined to room.
func (h *Calls) TakeOverVoiceForTest(ctx context.Context, user, dev id.ID, room string) {
	h.takeOverVoice(ctx, user, dev, room)
}

// ProcessQueueForTest runs one pass of the retry loop's work queue on the caller's goroutine.
func (h *Calls) ProcessQueueForTest(ctx context.Context) { h.processQueue(ctx) }

// ProcessTeardownsForTest runs the work queue's teardown pass alone on the caller's goroutine.
func (h *Calls) ProcessTeardownsForTest(ctx context.Context) { h.processTeardowns(ctx) }

// QueuedForTest is how many entries the work queue holds, and whether a request did not fit.
func (h *Calls) QueuedForTest() (int, bool) {
	h.cutMu.Lock()
	defer h.cutMu.Unlock()
	return h.queuedLocked(), h.resyncAll
}

// LockCallForTest takes call's lock and returns its release, for a test that makes a call busy.
func (h *Calls) LockCallForTest(ctx context.Context, call id.ID) (func(), error) {
	return h.leases.lockCall(ctx, call, time.Second)
}

// FlushAnnouncements waits, up to five seconds, until every queued voice_state was delivered.
func (c *CallEvents) FlushAnnouncements() {
	deadline := time.Now().Add(5 * time.Second)
	for !c.announcementsIdle() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

// SaturatingAddForTest is the stats route's saturating sum.
func SaturatingAddForTest(a, b uint64) uint64 { return saturatingAdd(a, b) }

// PendingRepairs is how many grant repairs are outstanding across every call.
func (h *Calls) PendingRepairs() int { return h.leases.pendingCount() }

// MarkPending records a pending repair of dev in call's room, as a failed cut would.
func (h *Calls) MarkPending(call, dev id.ID, room string) {
	h.leases.markPending(call, dev.String(), pendingRepair{Room: room, DropSlot: true})
}

// SetSweepEvery sets the retry loop's room-sweep cadence, for a test that runs the loop.
func (h *Calls) SetSweepEvery(d time.Duration) { h.sweepEvery = d }

// CutPasses is how many retry-loop passes took a non-empty cut-request set.
func (h *Calls) CutPasses() int64 { return h.cutPasses.Load() }

// SharersOf is the devices holding a sharing slot of call, for the lease-invariant tests in
// package api_test.
func (h *Calls) SharersOf(call id.ID) []id.ID {
	h.leases.mu.Lock()
	defer h.leases.mu.Unlock()
	out := make([]id.ID, 0, len(h.leases.byCall[call]))
	for dev := range h.leases.byCall[call] {
		out = append(out, dev)
	}
	slices.SortFunc(out, func(a, b id.ID) int { return slices.Compare(a[:], b[:]) })
	return out
}

// AdmittedAsForTest is the leaf and added epoch of the membership dev was admitted to room with for
// the session LiveKit reports joined at joinedAt (zero: the latest), and whether there is one.
func (h *Calls) AdmittedAsForTest(room string, dev id.ID, joinedAt time.Time) (leaf uint32, added uint64, ok bool) {
	m, ok := h.admittedAs(room, dev, joinedAt)
	return m.leaf, m.added, ok
}

// SetMemberPageForTest lowers the notifier's member page size to n and returns the restore.
func SetMemberPageForTest(n int32) func() {
	old := memberPage
	memberPage = n
	return func() { memberPage = old }
}

// SetAFKHookForTest makes the AFK paths call f at their named points ("cut": the sweep's cut has
// landed and the mark is not yet decided; "sweep-pending": the sweep recorded pending 1 and has not
// yet asked the SFU to cut; "away-decided": PUT …/afk/away has decided and not yet marked;
// "away-pending": the PUT marked the user away and recorded every pending 2, and cuts next).
func (h *Calls) SetAFKHookForTest(f func(stage string)) { h.afkHook = f }

// AwayLockFreeForTest reports whether awayMu is free right now (a clear must not hold it while it
// resolves the op 55 audience).
func (h *Calls) AwayLockFreeForTest() bool {
	if h.awayMu.TryLock() {
		h.awayMu.Unlock()
		return true
	}
	return false
}

// MarkAwayForTest marks user away in cid as a sweep or a PUT would (connection checked, decided now).
func (h *Calls) MarkAwayForTest(ctx context.Context, cid, user id.ID) {
	_, _ = h.markAway(ctx, cid, user, h.afkDecision(), true)
}

// AwaySeededForTest reports whether the away index has been seeded from the store.
func (h *Calls) AwaySeededForTest() bool { return h.awaySeededNow() }

// FlushAFKAwayForTest waits, up to five seconds, until every queued op 55 was sent.
func (h *Calls) FlushAFKAwayForTest() { h.flushAway(5 * time.Second) }

// AFKOfflineWaitersForTest is how many goroutines wait out a user's offline grace now.
func (h *Calls) AFKOfflineWaitersForTest() int { return int(h.awayOffWaiters.Load()) }

// AFKAdmissionStateForTest reads, in one afkMu section, dev's pending reason and the number of its
// latest recorded admission (0 for none).
func (h *Calls) AFKAdmissionStateForTest(user, dev id.ID) (pending, admitSeq uint64) {
	h.afkMu.Lock()
	defer h.afkMu.Unlock()
	return h.afkPending[dev].reason, h.afkAdmits[user][dev].seq
}

// LiveForTest is how many unexpired assertions the store holds.
func (a *Assertions) LiveForTest() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked()
	return len(a.items)
}

// ReplayWaitersForTest is how many duplicates are parked on a running replay entry.
func (d *SocketDispatcher) ReplayWaitersForTest() int64 { return d.waiters.Load() }

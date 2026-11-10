package api

import (
	"context"
	"errors"
	"maps"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// The call work queue (server-half fix wave, lens calls): everything a request, the delivery
// service's commit path or a webhook would otherwise do to a live call on its own goroutine — a cut
// (CutDevice, CutUser), a grant sync after a membership or permission change (RequestSync), the
// eviction of the devices a commit removed from a call group (requestEviction) and the teardown of a
// call a channel or community delete ended (requestTeardown) — is recorded here instead, in a small
// bounded and de-duplicated set, and the retry loop (StartRetries) is woken to do it at once on its
// own goroutine. Recording never takes a call's lock and never calls the SFU, so no request and no
// commit waits on a slow or hung SFU. Meanwhile every gate re-reads what it decides on: the /rtc gate,
// start and share refuse a device that is barred, lost view_channel or connect, or is no current leaf
// of the call group, so a queued cut is only the in-room half of a refusal that already holds.
//
// The set is bounded by maxCutRequests entries across its kinds. A request that does not fit is not
// dropped silently: it raises resyncAll, and the loop's next pass — at once, it is woken — runs a
// room sweep (SweepRooms), which applies every one of these rules to every participant of every
// live call room and deletes every room of a call that has ended. So the worst-case delay of a
// queued cut, sync, eviction or teardown is the sweep's own bound: one pass right after the
// overflow, then one pass per RoomSweepInterval, each covering maxRoomsPerSweep rooms.

// maxCutRequests bounds the work queue: cuts, syncs, evicted devices and teardowns together. A request
// beyond it raises resyncAll (above).
const maxCutRequests = 1024

// syncKey is one queued grant sync: SyncCallGrants' scope. A zero user is every user and a zero
// channel every channel of the community; a DM's sync has a zero community and names its channel.
type syncKey struct {
	community, user, channel id.ID
}

// queuedEviction is the devices a commit took out of one live call's group, for that call's room.
type queuedEviction struct {
	row     store.VoiceSessionRow
	devices map[id.ID]bool
}

// queuedTeardown is a call already ended in the record whose room, slots and call group are still to
// be closed (requestTeardown).
type queuedTeardown struct {
	row    store.VoiceSessionRow
	reason string
}

// queuedLocked is how many entries the work queue holds. The caller holds cutMu.
func (h *Calls) queuedLocked() int {
	n := len(h.cutDevices) + len(h.cutUsers) + len(h.syncs) + len(h.teardowns)
	for _, ev := range h.evictions {
		n += len(ev.devices)
	}
	return n
}

// initQueueLocked makes the queue's maps. The caller holds cutMu.
func (h *Calls) initQueueLocked() {
	if h.cutDevices == nil {
		h.cutDevices, h.cutUsers = map[id.ID]bool{}, map[id.ID]bool{}
	}
	if h.syncs == nil {
		h.syncs = map[syncKey]bool{}
		h.evictions = map[string]*queuedEviction{}
		h.teardowns = map[string]queuedTeardown{}
	}
}

// wake wakes the retry loop; one pending wake is enough, the loop takes the whole queue.
func (h *Calls) wake() {
	select {
	case h.cutWake <- struct{}{}:
	default:
	}
}

// overflowLocked records that a request did not fit: the loop's next pass sweeps every room. The
// caller holds cutMu.
func (h *Calls) overflowLocked() {
	h.resyncAll = true
}

// warnOverflow logs a request the queue had no room for.
func (h *Calls) warnOverflow(ctx context.Context, what string, args ...any) {
	h.log.WarnContext(ctx, "the call work queue is full; the room sweep, run at once, covers this "+what, args...)
}

// RequestSync queues SyncCallGrants for communityID, scoped to userID and channelID when they are
// set, and wakes the retry loop, which runs it at once on its own goroutine: what every membership,
// role, overwrite and visibility change calls after its commit, so the change's request never waits
// on the SFU or a call's lock. Requests coalesce: a sync already queued with the same scope, or with
// the whole community's, takes the new one; a community-wide request takes every narrower one of its
// community. A request that does not fit raises the queue's resync-all flag. A nil h, or an h without
// an SFU, has no live call to sync.
func (h *Calls) RequestSync(ctx context.Context, communityID id.ID, userID, channelID *id.ID) {
	if h == nil || h.sfu == nil {
		return
	}
	k := syncKey{community: communityID}
	if userID != nil {
		k.user = *userID
	}
	if channelID != nil {
		k.channel = *channelID
	}
	wide := syncKey{community: communityID}
	communityWide := !communityID.IsZero()
	full := false
	h.cutMu.Lock()
	h.initQueueLocked()
	switch {
	case h.syncs[k], communityWide && h.syncs[wide]:
	case h.queuedLocked() >= maxCutRequests:
		h.overflowLocked()
		full = true
	default:
		if communityWide && k == wide {
			for q := range h.syncs {
				if q.community == communityID {
					delete(h.syncs, q)
				}
			}
		}
		h.syncs[k] = true
	}
	h.cutMu.Unlock()
	if full {
		h.warnOverflow(ctx, "grant sync", "community", communityID)
	}
	h.wake()
}

// requestEviction queues the removal of devices from row's room — the delivery service's evictor
// (CallEvents.Evict) — and wakes the retry loop. The relay is cut first, here and synchronously: it
// closes sockets and writes a map. The devices are no longer leaves of the call group, so the /rtc
// gate already refuses them; what the loop does is the in-room removal (evictDevices).
func (h *Calls) requestEviction(ctx context.Context, row store.VoiceSessionRow, devices []id.ID) {
	for _, dev := range devices {
		h.revokeRelay(dev)
	}
	if h.sfu == nil || len(devices) == 0 {
		return
	}
	full := false
	h.cutMu.Lock()
	h.initQueueLocked()
	ev := h.evictions[row.LivekitRoom]
	if ev == nil {
		ev = &queuedEviction{row: row, devices: map[id.ID]bool{}}
	}
	for _, dev := range devices {
		if ev.devices[dev] {
			continue
		}
		if h.queuedLocked() >= maxCutRequests {
			h.overflowLocked()
			full = true
			break
		}
		ev.devices[dev] = true
		h.evictions[row.LivekitRoom] = ev
	}
	h.cutMu.Unlock()
	if full {
		h.warnOverflow(ctx, "eviction", "room", row.LivekitRoom)
	}
	h.wake()
}

// requestTeardown queues the teardown of a call already ended in the record (teardown) and wakes the
// retry loop. An instance without an SFU runs no retry loop and has no room to close: the teardown
// then runs here.
func (h *Calls) requestTeardown(ctx context.Context, row store.VoiceSessionRow, reason string) {
	if h.sfu == nil {
		h.teardown(ctx, row, reason)
		return
	}
	full := false
	h.cutMu.Lock()
	h.initQueueLocked()
	if _, ok := h.teardowns[row.LivekitRoom]; !ok {
		if h.queuedLocked() >= maxCutRequests {
			h.overflowLocked()
			full = true
		} else {
			h.teardowns[row.LivekitRoom] = queuedTeardown{row: row, reason: reason}
		}
	}
	h.cutMu.Unlock()
	if full {
		h.warnOverflow(ctx, "call teardown", "room", row.LivekitRoom)
	}
	h.wake()
}

// queuedWork reports whether a cut, an eviction or a grant sync is waiting: every kind that takes a
// device's access away in the room. A long pass — a retry pass, a sweep, the teardowns — checks it
// between its steps (serveQueued), so a revocation, a kick or a role change is not left behind it
// (N1).
func (h *Calls) queuedWork() bool {
	h.cutMu.Lock()
	defer h.cutMu.Unlock()
	return len(h.cutDevices)+len(h.cutUsers)+len(h.evictions)+len(h.syncs) > 0
}

// serveQueued runs the queued cuts, evictions and grant syncs now, between two steps of a long pass.
func (h *Calls) serveQueued(ctx context.Context) {
	if !h.queuedWork() {
		return
	}
	h.processCuts(ctx)
	h.processEvictions(ctx)
	h.processSyncs(ctx)
}

// processQueue is one pass over the work queue, on the retry loop: cuts first, then evictions,
// teardowns and grant syncs, then — when a request did not fit — a room sweep, and last the
// membership-bound Removes a whole-room reconcile decided (flushLeafRemovals).
func (h *Calls) processQueue(ctx context.Context) {
	h.processCuts(ctx)
	h.processEvictions(ctx)
	h.processTeardowns(ctx)
	h.processSyncs(ctx)
	h.cutMu.Lock()
	resync := h.resyncAll
	h.resyncAll = false
	h.cutMu.Unlock()
	if resync {
		h.SweepRooms(ctx)
	}
	h.flushLeafRemovals(ctx)
}

// processEvictions removes every queued evicted device from its call's room (evictDevices: under the
// call's lock, a removal the SFU does not take or a lock that cannot be taken is a pending cut).
func (h *Calls) processEvictions(ctx context.Context) {
	h.cutMu.Lock()
	evictions := h.evictions
	h.evictions = map[string]*queuedEviction{}
	h.cutMu.Unlock()
	if len(evictions) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, cutDeviceBudget)
	defer cancel()
	for _, ev := range evictions {
		devices := make([]id.ID, 0, len(ev.devices))
		for dev := range ev.devices {
			devices = append(devices, dev)
		}
		if err := h.evictDevices(ctx, ev.row, devices); err != nil {
			h.log.WarnContext(ctx, "evicting removed devices from their call's room did not land; it stays pending",
				"room", ev.row.LivekitRoom, "err", err)
		}
	}
}

// teardownTimeout bounds one teardown: its lock wait (callLockWait), its room close
// (roomCloseTimeout) and its call group's close through the delivery service.
const teardownTimeout = callHoldBackground

// processTeardowns closes the room, slots and call group of every queued ended call (N3): within
// cutDeviceBudget, each teardown within teardownTimeout, serving the queued cuts, evictions and
// syncs between two of them. The teardowns the budget does not reach are put back for the next pass.
func (h *Calls) processTeardowns(ctx context.Context) {
	h.cutMu.Lock()
	teardowns := h.teardowns
	h.teardowns = map[string]queuedTeardown{}
	h.cutMu.Unlock()
	if len(teardowns) == 0 {
		return
	}
	bctx, cancel := context.WithTimeout(ctx, cutDeviceBudget)
	defer cancel()
	left := maps.Clone(teardowns)
	for room, td := range teardowns {
		if bctx.Err() != nil {
			break
		}
		h.serveQueued(bctx)
		tctx, tcancel := context.WithTimeout(bctx, teardownTimeout)
		h.teardown(tctx, td.row, td.reason)
		tcancel()
		delete(left, room)
	}
	if len(left) == 0 || ctx.Err() != nil {
		return // stopping: the sweep closes an ended call's room, the retry pass drops its state
	}
	h.cutMu.Lock()
	h.initQueueLocked()
	for room, td := range left {
		if _, ok := h.teardowns[room]; ok {
			continue
		}
		if h.queuedLocked() >= maxCutRequests {
			h.overflowLocked()
			break
		}
		h.teardowns[room] = td
	}
	h.cutMu.Unlock()
	h.wake()
}

// processSyncs runs the queued grant syncs within cutDeviceBudget, serving a cut or an eviction
// queued meanwhile between two of them. A sync the budget cuts short is not put back: SyncCallGrants
// leaves every live call it did not reach as a pending room resync, which the next retry pass (at
// most CallRetryInterval later) drives (N1).
func (h *Calls) processSyncs(ctx context.Context) {
	h.cutMu.Lock()
	syncs := h.syncs
	h.syncs = map[syncKey]bool{}
	h.cutMu.Unlock()
	if len(syncs) == 0 {
		return
	}
	bctx, cancel := context.WithTimeout(ctx, cutDeviceBudget)
	defer cancel()
	for k := range syncs {
		h.cutMu.Lock()
		cuts := len(h.cutDevices)+len(h.cutUsers)+len(h.evictions) > 0
		h.cutMu.Unlock()
		if cuts {
			h.processCuts(bctx)
			h.processEvictions(bctx)
		}
		var user, channel *id.ID
		if !k.user.IsZero() {
			user = &k.user
		}
		if !k.channel.IsZero() {
			channel = &k.channel
		}
		if err := SyncCallGrants(bctx, h.repo, h.res, h.sfu, h, k.community, user, channel); err != nil {
			h.log.WarnContext(ctx, "a queued call grant sync did not finish; the retry loop and the room sweep converge it",
				"community", k.community, "err", err)
		}
	}
	h.reportPending()
}

// endRow ends row's call in the record if it is still that call — live, in row's room — reading and
// writing in one transaction, so an end never closes the call a start has reopened since under the
// same call id (R9). It reports whether it ended it.
func (h *Calls) endRow(ctx context.Context, row store.VoiceSessionRow) (bool, error) {
	ended := false
	err := h.repo.Tx(ctx, func(tx store.Repository) error {
		cur, err := tx.GetVoiceSession(ctx, row.CallID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if cur.Ended != nil || cur.LivekitRoom != row.LivekitRoom {
			return nil
		}
		if err := tx.EndVoiceSession(ctx, row.CallID, h.clk.Now().Unix()); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		ended = true
		return nil
	})
	return ended, err
}

// endQueued ends row's call in the record at once and queues the rest of its end (requestTeardown):
// the path of a call whose channel is gone or whose group is closed, found by a delete, the sweep, a
// cut, a retry, an eviction or a sync. Every gate refuses an ended call's room from the moment the
// record says so.
func (h *Calls) endQueued(ctx context.Context, row store.VoiceSessionRow, reason string) error {
	ended, err := h.endRow(ctx, row)
	if err != nil {
		return err
	}
	if ended {
		// L-GW-33: the call ended, so every ring of it stops now (state 0), and a restart of the same
		// DM — the same call id — rings afresh (task-3 review I1).
		h.ringer.End(row.CallID)
		h.log.InfoContext(ctx, "call ended", "call_id", row.CallID.String(), "reason", reason)
		h.requestTeardown(ctx, row, reason)
	}
	return nil
}

// endLocked is endQueued for a caller that holds the call's lock: it also forgets the room's slots,
// repairs and F11 penalties at once.
func (h *Calls) endLocked(ctx context.Context, row store.VoiceSessionRow, reason string) error {
	if err := h.endQueued(ctx, row, reason); err != nil {
		return err
	}
	h.leases.dropRoom(row.CallID, row.LivekitRoom)
	h.forgetMembers(row.CallID, row.LivekitRoom)
	return nil
}

// teardown is what follows a call's end in the record: its room's slots, repairs and penalties go
// under the call's lock (a busy lock leaves them to the retry pass, which drops an ended call's),
// every device the call events saw in it is announced out, then its room is closed and its call
// group closed (through the delivery service, after the lock). It touches only row's room, so a call reopened
// since under the same call id keeps its own state.
func (h *Calls) teardown(ctx context.Context, row store.VoiceSessionRow, reason string) {
	h.forgetMembers(row.CallID, row.LivekitRoom)
	if h.sfu != nil { // without an SFU no slot, repair or penalty was ever recorded
		if _, unlock, err := h.lock(ctx, row.CallID, callLockWait, callHoldBackground); err == nil {
			h.leases.dropRoom(row.CallID, row.LivekitRoom)
			unlock()
		}
	}
	// Announced before the room closes (see endCall): the room_finished the close makes forgets
	// the room's state.
	if h.events != nil {
		h.events.ended(ctx, row)
	}
	h.closeRoom(ctx, row.LivekitRoom)
	if row.GroupID != nil {
		h.closeCallGroup(ctx, *row.GroupID)
	}
	h.log.InfoContext(ctx, "an ended call's room and group are closed", "call_id", row.CallID.String(), "reason", reason)
}

// EndChannelCalls ends every live call of channels — the channels a channel or community delete has
// just tombstoned, after their groups were closed — in the record at once, and queues each one's
// teardown (its room deleted, everyone in it disconnected, its slots and repairs dropped, voice_state
// 0 announced). A failure is logged: the room sweep ends a live call whose channel is gone, and
// closes the room of an ended one. A nil h touches nothing.
func (h *Calls) EndChannelCalls(ctx context.Context, channels []id.ID, reason string) {
	if h == nil {
		return
	}
	for _, ch := range channels {
		rows, err := h.repo.ListLiveVoiceSessions(ctx, ch)
		if err != nil {
			h.log.ErrorContext(ctx, "listing a deleted channel's live calls failed; the room sweep ends them",
				"channel_id", ch.String(), "err", err)
			continue
		}
		for _, row := range rows {
			if err := h.endQueued(ctx, row, reason); err != nil {
				h.log.ErrorContext(ctx, "ending a deleted channel's call failed; the room sweep ends it",
					"call_id", row.CallID.String(), "err", err)
			}
		}
	}
}

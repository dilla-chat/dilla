package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/livekit/protocol/livekit"

	botaccess "github.com/jonasthim/dilla/internal/bot/access"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// The call group's members against the room (server-half fix wave, integration round): which
// membership — leaf and AddedEpoch — a device was admitted to a call's room with, and the sweep's
// removal of a call-group leaf whose device stays out of the room (ruling (a) of the calls
// re-review). A call group's members are "every device present in the call" (protocol/01): a device
// that left without its leave report reaching the instance, or that was cut and whose leave report
// was lost, would otherwise keep deriving the call's media keys for as long as the call lasts.

const (
	// admissionJoinWindow is how long after its admission (a start's token or the /rtc gate) a device
	// may still be absent from the room without its leaf being removed: the time a client takes from
	// its token to LiveKit's join, a reconnect included. The sweep never removes a leaf inside it.
	admissionJoinWindow = 30 * time.Second
	// maxAdmissionsPerDevice bounds the admissions remembered per device and room: enough to tell
	// a late event's session from the ones after it.
	maxAdmissionsPerDevice = 4
	// leafRemovalTimeout bounds one membership-bound Remove the sweep proposes.
	leafRemovalTimeout = 10 * time.Second
)

// membership is one call-group leaf of a device: its index and the epoch the device took it in.
type membership struct {
	leaf  uint32
	added uint64
}

// admission is a membership a device was admitted to a room with, and when: at by the instance
// clock (the join window, the sweep), sfuAt by the SFU's (what a session's join time is compared
// with: LiveKit stamps it with its own clock, which the instance clock need not match, and reports it
// in whole milliseconds, so sfuAt is truncated to the millisecond). resume marks a LiveKit signal
// resume through /rtc: the session already in the room again, never a newer one.
type admission struct {
	membership
	at, sfuAt time.Time
	resume    bool
}

type admitKey struct {
	room string
	dev  id.ID
}

// memberKey is one membership of one call's group.
type memberKey struct {
	call, dev id.ID
	membership
}

// leafRemoval is a Remove the sweep decided under a call's lock, proposed after it is released.
type leafRemoval struct {
	group id.ID
	key   memberKey
}

// noteAdmission records the membership dev holds in row's call group now, as it is admitted to row's
// room: what a leave of the session it opens is bound to (DS-7, N2), and what opens its join window.
// A store failure records nothing: the leave then proposes nothing and the sweep decides. resume is
// a signal resume of the session already in the room.
func (h *Calls) noteAdmission(ctx context.Context, row store.VoiceSessionRow, dev id.ID, resume bool) {
	if row.GroupID == nil {
		return
	}
	members, err := h.repo.ListMembers(ctx, *row.GroupID)
	if err != nil {
		h.log.WarnContext(ctx, "reading a call group's members at admission failed", "group_id", row.GroupID.String(), "err", err)
		return
	}
	for _, m := range members {
		if m.DeviceID != dev || m.RemovedEpoch != nil {
			continue
		}
		k := admitKey{room: row.LivekitRoom, dev: dev}
		a := admission{membership: membership{leaf: m.LeafIndex, added: m.AddedEpoch}, at: h.clk.Now(), sfuAt: h.sfuNow().Truncate(time.Millisecond), resume: resume}
		h.memMu.Lock()
		if h.admissions == nil {
			h.admissions = map[admitKey][]admission{}
		}
		list := append(h.admissions[k], a)
		if len(list) > maxAdmissionsPerDevice {
			list = list[len(list)-maxAdmissionsPerDevice:]
		}
		h.admissions[k] = list
		h.memMu.Unlock()
		return
	}
}

// admittedAs is the admission dev was admitted to room with for the session LiveKit reports joined
// at joinedAt (the SFU's clock): the latest admission at or before it by that clock (a session is
// admitted before it joins), or the latest of all when the event names no join time.
func (h *Calls) admittedAs(room string, dev id.ID, joinedAt time.Time) (admission, bool) {
	h.memMu.Lock()
	defer h.memMu.Unlock()
	list := h.admissions[admitKey{room: room, dev: dev}]
	for i := len(list) - 1; i >= 0; i-- {
		if joinedAt.IsZero() || !list[i].sfuAt.After(joinedAt) {
			return list[i], true
		}
	}
	return admission{}, false
}

// latestFreshAdmission is dev's latest admission to room by a fresh join (a signal resume is no new
// session), and whether one is remembered.
func (h *Calls) latestFreshAdmission(room string, dev id.ID) (admission, bool) {
	h.memMu.Lock()
	defer h.memMu.Unlock()
	list := h.admissions[admitKey{room: room, dev: dev}]
	for i := len(list) - 1; i >= 0; i-- {
		if !list[i].resume {
			return list[i], true
		}
	}
	return admission{}, false
}

// newerAdmission reports whether dev was admitted to room again after than (the SFU's clock): a
// fresh join's admission whose sfuAt is after it. A leave of the session admitted at than is then
// of an earlier session (a page reload and rejoin), and the newer session's own events decide its
// membership. A signal resume is the same session again, so it is never newer. A zero than is no
// admission, so nothing is newer than it. Only admissions recorded after the one stamped than count
// (when it is still remembered): one recorded before it under another clock reading — a clock that
// stepped back — is never taken for a newer session.
func (h *Calls) newerAdmission(room string, dev id.ID, than time.Time) bool {
	if than.IsZero() {
		return false
	}
	h.memMu.Lock()
	defer h.memMu.Unlock()
	list := h.admissions[admitKey{room: room, dev: dev}]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].sfuAt.Equal(than) {
			list = list[i+1:]
			break
		}
	}
	for _, a := range list {
		if !a.resume && a.sfuAt.After(than) {
			return true
		}
	}
	return false
}

// currentMembershipOut is the membership dev holds now in row's call group, when a leave's bound
// membership (gone) was refused and the device may be removed at the one it holds instead (M-4): a
// current leaf of an open, epoch-known group that is not gone itself, whose device the SFU lists out
// of the room and that was not admitted within admissionJoinWindow — the rules the sweep removes a
// leaf under. When the SFU cannot list the room, or anything else is unknown, it answers false.
func (h *Calls) currentMembershipOut(ctx context.Context, row store.VoiceSessionRow, dev id.ID, gone admission) (membership, bool) {
	if row.GroupID == nil || h.sfu == nil {
		return membership{}, false
	}
	g, err := h.repo.GetGroup(ctx, *row.GroupID)
	if err != nil || g.ClosedAt != nil || g.EpochUnknown {
		return membership{}, false
	}
	members, err := h.repo.ListMembers(ctx, g.GroupID)
	if err != nil {
		return membership{}, false
	}
	var cur membership
	found := false
	for _, m := range members {
		if m.DeviceID == dev && m.RemovedEpoch == nil && m.AddedEpoch <= g.Epoch {
			cur, found = membership{leaf: m.LeafIndex, added: m.AddedEpoch}, true
		}
	}
	if !found || cur == gone.membership {
		return membership{}, false
	}
	lctx, cancel := sfuCtx(ctx)
	parts, err := h.sfu.Participants(lctx, row.LivekitRoom)
	cancel()
	if err != nil {
		return membership{}, false
	}
	for _, p := range parts {
		if base, _, _ := strings.Cut(p.GetIdentity(), "#"); base == dev.String() {
			return membership{}, false
		}
	}
	h.memMu.Lock()
	list := h.admissions[admitKey{room: row.LivekitRoom, dev: dev}]
	recent := len(list) > 0 && h.clk.Now().Sub(list[len(list)-1].at) < admissionJoinWindow
	h.memMu.Unlock()
	if recent {
		return membership{}, false
	}
	return cur, true
}

// forgetMembers drops what is remembered for call's room: its admissions and the absences its
// sweep tracked. The room's call has ended or moved on.
func (h *Calls) forgetMembers(call id.ID, room string) {
	h.memMu.Lock()
	defer h.memMu.Unlock()
	for k := range h.admissions {
		if k.room == room {
			delete(h.admissions, k)
		}
	}
	delete(h.botOnlySince, call)
	for k := range h.absentSince {
		if k.call == call {
			delete(h.absentSince, k)
		}
	}
}

// resetBotOnly drops the first bot-only observation when a reconciliation cannot establish
// the room's state. call zero resets every room after a failed SFU room listing.
func (h *Calls) resetBotOnly(call id.ID) {
	h.memMu.Lock()
	defer h.memMu.Unlock()
	if call == (id.ID{}) {
		clear(h.botOnlySince)
	} else {
		delete(h.botOnlySince, call)
	}
}

// trackAbsentLocked is ruling (a), run by a whole-room reconcile under the call's lock: it compares
// the call group's current leaves with the room's participants. A leaf whose device is not in the
// room — or was cut from it by this reconcile (cut) — is remembered with the time it was first seen
// so. When the same membership (leaf, AddedEpoch) is still absent a sweep period or more later, and
// its device was not admitted within admissionJoinWindow, its Remove is queued for after the lock
// (flushLeafRemovals), bound to that membership, so a device that rejoined since is never removed. A
// leaf seen present again is forgotten. Without a delivery service, or for a closed, gone or
// epoch-unknown group, nothing is tracked: the end of the call or a heal decides those leaves.
// It reports a bot-only call after two consecutive whole-room observations at least sweepEvery
// apart: no human leaf is present or inside its admission window, and at least one managed-bot leaf
// remains. While waiting it queues no Removes: bots cannot commit the human's Remove, so the caller
// must end the call under its existing call lock instead. Uncertain observations reset the wait.
func (h *Calls) trackAbsentLocked(ctx context.Context, row store.VoiceSessionRow, parts []*livekit.ParticipantInfo, cut map[string]bool) (bool, error) {
	if row.GroupID == nil {
		h.resetBotOnly(row.CallID)
		return false, nil
	}
	g, err := h.repo.GetGroup(ctx, *row.GroupID)
	if err != nil || g.ClosedAt != nil || g.EpochUnknown {
		h.resetBotOnly(row.CallID)
		return false, err
	}
	members, err := h.repo.ListMembers(ctx, g.GroupID)
	if err != nil {
		h.resetBotOnly(row.CallID)
		h.log.WarnContext(ctx, "reading a call group's members for the sweep failed", "group_id", g.GroupID.String(), "err", err)
		return false, err
	}
	bots := make(map[id.ID]bool)
	unknown := make(map[id.ID]bool)
	var lookupErr error
	for _, m := range members {
		if m.RemovedEpoch != nil || m.AddedEpoch > g.Epoch {
			continue
		}
		if _, known := bots[m.UserID]; !known && !unknown[m.UserID] {
			bot, err := botaccess.IsBotUser(ctx, h.repo, m.UserID)
			if err != nil {
				unknown[m.UserID] = true
				lookupErr = errors.Join(lookupErr, err)
				h.log.WarnContext(ctx, "reading a call member's account kind failed; skipping its absence and keeping the call live", "group_id", g.GroupID.String(), "user_id", m.UserID.String(), "err", err)
				continue
			}
			bots[m.UserID] = bot
		}
	}
	present := make(map[string]bool, len(parts))
	for _, p := range parts {
		if !cut[p.GetIdentity()] {
			present[p.GetIdentity()] = true
		}
	}
	now := h.clk.Now()
	h.memMu.Lock()
	defer h.memMu.Unlock()
	hasBot, humanActive := false, false
	for _, m := range members {
		if m.RemovedEpoch != nil || m.AddedEpoch > g.Epoch {
			continue
		}
		if bots[m.UserID] {
			hasBot = true
			continue
		}
		list := h.admissions[admitKey{room: row.LivekitRoom, dev: m.DeviceID}]
		if present[m.DeviceID.String()] || (len(list) > 0 && now.Sub(list[len(list)-1].at) < admissionJoinWindow) {
			humanActive = true
		}
	}
	if hasBot && !humanActive && lookupErr == nil {
		if h.botOnlySince == nil {
			h.botOnlySince = make(map[id.ID]time.Time)
		}
		first, seen := h.botOnlySince[row.CallID]
		if !seen {
			h.botOnlySince[row.CallID] = now
		}
		return seen && now.Sub(first) >= h.sweepEvery, nil
	}
	delete(h.botOnlySince, row.CallID)
	if h.dsvc == nil {
		return false, lookupErr
	}
	if h.absentSince == nil {
		h.absentSince, h.leafProposed = map[memberKey]time.Time{}, map[memberKey]time.Time{}
	}
	current := map[memberKey]bool{}
	for _, m := range members {
		if m.RemovedEpoch != nil || m.AddedEpoch > g.Epoch || unknown[m.UserID] {
			continue
		}
		k := memberKey{call: row.CallID, dev: m.DeviceID, membership: membership{leaf: m.LeafIndex, added: m.AddedEpoch}}
		current[k] = true
		if present[m.DeviceID.String()] {
			delete(h.absentSince, k)
			continue
		}
		first, seen := h.absentSince[k]
		if !seen {
			h.absentSince[k] = now
			continue
		}
		if now.Sub(first) < h.sweepEvery {
			continue
		}
		if list := h.admissions[admitKey{room: row.LivekitRoom, dev: m.DeviceID}]; len(list) > 0 &&
			now.Sub(list[len(list)-1].at) < admissionJoinWindow {
			continue
		}
		if at, ok := h.leafProposed[k]; ok && now.Sub(at) < proposeWindow {
			continue
		}
		h.leafProposed[k] = now
		h.leafRemovals = append(h.leafRemovals, leafRemoval{group: g.GroupID, key: k})
	}
	for k := range h.absentSince {
		if k.call == row.CallID && !current[k] {
			delete(h.absentSince, k)
		}
	}
	for k, at := range h.leafProposed {
		if now.Sub(at) >= proposeWindow {
			delete(h.leafProposed, k)
		}
	}
	return false, lookupErr
}

// flushLeafRemovals proposes the Removes trackAbsentLocked queued, each bound to its membership
// (ds.ProposeRemoveOfMember). It runs on the retry loop with no call's lock held: nothing under a
// call's lock calls the delivery service. A membership that is gone by now (ds.ErrRemoveTargetGone)
// is dropped; any other failure is logged and the next sweep decides again.
func (h *Calls) flushLeafRemovals(ctx context.Context) {
	h.memMu.Lock()
	removals := h.leafRemovals
	h.leafRemovals = nil
	h.memMu.Unlock()
	for _, r := range removals {
		if h.dsvc == nil || ctx.Err() != nil {
			return
		}
		dctx, cancel := context.WithTimeout(ctx, leafRemovalTimeout)
		err := h.dsvc.ProposeRemoveOfMember(dctx, r.group, r.key.leaf, r.key.dev, r.key.added, id.New())
		cancel()
		switch {
		case errors.Is(err, ds.ErrRemoveTargetGone):
		case err != nil:
			h.log.WarnContext(ctx, "removing the leaf of a device that stayed out of its call failed; the next sweep retries",
				"group_id", r.group.String(), "device_id", r.key.dev.String(), "err", err)
		}
	}
}

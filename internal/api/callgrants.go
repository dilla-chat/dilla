package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"

	botaccess "github.com/jonasthim/dilla/internal/bot/access"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/sfu"
	"github.com/jonasthim/dilla/internal/store"
)

// grantDeps is what a grant reconciliation reads and drives: the store, the resolver over it and
// the SFU. The routes use their own; SyncCallGrants is handed the caller's.
type grantDeps struct {
	repo   store.Repository
	res    *Resolver
	tokens CallTokens
}

func (h *Calls) deps() grantDeps { return grantDeps{repo: h.repo, res: h.res, tokens: h.sfu} }

// SyncCallGrants re-pushes the live call grants a role or overwrite change may have moved (gap G26).
// It runs after the commit, beside the six materialisers of roles.go, with the scope of the change:
// one user of a community (grant, revoke), every member of one channel (an overwrite), or every
// member of every channel (a role's bits or its deletion). For each live call in scope it takes the
// call's lock, re-reads the call, drives the call's pending repairs, and reconciles each participant
// in scope from bits resolved under the lock (reconcile). LiveKit's MatchesPermission makes an
// unchanged permission a no-op, so over-calling is safe.
func SyncCallGrants(ctx context.Context, repo store.Repository, res *Resolver, tokens CallTokens, h *Calls,
	communityID id.ID, userID *id.ID, channelID *id.ID) error {
	if tokens == nil || h == nil {
		return nil
	}
	// The store reads go on past ctx's end (N1): a sync whose budget ran out still finds every live
	// call in scope, and leaves each one it does not reach as a pending room resync for the retry
	// loop's next pass instead of skipping it.
	rctx := context.WithoutCancel(ctx)
	var channels []store.ChannelRow
	if channelID != nil {
		ch, err := repo.GetChannel(rctx, *channelID)
		if errors.Is(err, store.ErrNotFound) {
			// A deleted channel has no call to sync: one still live is ended (CALLS-1).
			return h.endCallsOfGoneChannel(rctx, *channelID)
		}
		if err != nil {
			return err
		}
		channels = []store.ChannelRow{ch}
	} else {
		all, err := repo.ListChannels(rctx, communityID)
		if err != nil {
			return err
		}
		channels = all
	}
	d := grantDeps{repo: repo, res: res, tokens: tokens}
	var errs []error
	for _, ch := range channels {
		if !CallGroupAllowed(ch) {
			continue
		}
		rows, err := repo.ListLiveVoiceSessions(rctx, ch.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				h.leases.markPending(row.CallID, roomResync, pendingRepair{Room: row.LivekitRoom})
				continue
			}
			if err := h.syncRoomGrants(ctx, d, ch, row, userID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	h.reportPending()
	return errors.Join(errs...)
}

// endCallsOfGoneChannel ends every live call of a channel that is deleted (endQueued): what every path
// that finds a call's channel gone does instead of skipping its room.
func (h *Calls) endCallsOfGoneChannel(ctx context.Context, channelID id.ID) error {
	rows, err := h.repo.ListLiveVoiceSessions(ctx, channelID)
	if err != nil {
		return err
	}
	var errs []error
	for _, row := range rows {
		errs = append(errs, h.endQueued(ctx, row, "channel_deleted"))
	}
	return errors.Join(errs...)
}

// syncAfterMembership queues SyncCallGrants scoped to userID after a membership change has committed
// — a kick, a leave, a ban, a group-DM removal — so the user's connected SFU sessions are cut
// immediately after, on the retry loop (RequestSync), never within the request; the MLS Remove of
// their leaves still follows through the delivery service. channelID scopes it to one channel (a DM
// has no community); nil walks every channel of communityID. Meanwhile the gates refuse the user's
// devices. A nil h (no SFU) touches nothing.
func syncAfterMembership(ctx context.Context, h *Calls, communityID id.ID, userID id.ID, channelID *id.ID) {
	h.RequestSync(ctx, communityID, &userID, channelID)
	// A community membership that went took the user's away row with it (the store's cascade): the
	// remaining audience hears op 55 since 0 (AW2 fix round 1, M6).
	if channelID == nil {
		h.AFKMemberRemoved(ctx, communityID, userID)
	}
}

// roomResync is the pending-repair key for "reconcile every participant of the room": a grant sync
// that could not take the call's lock, could not list the room, or ran out of its hold leaves it for
// the retry loop.
const roomResync = "*"

// syncRoomGrants is SyncCallGrants' visit of one live call. It runs on the retry loop (RequestSync),
// so it waits for the call's lock no longer than a request does: a busy call is left as a pending
// room resync, which the loop's next retry pass drives, rather than holding up the queue behind it.
func (h *Calls) syncRoomGrants(ctx context.Context, d grantDeps, ch store.ChannelRow, row store.VoiceSessionRow, userID *id.ID) (result error) {
	ctx, unlock, err := h.lock(ctx, row.CallID, callLockWait, callHoldBackground)
	if err != nil {
		h.resetBotOnly(row.CallID)
		// The change is committed; the room converges on the retry loop instead.
		h.leases.markPending(row.CallID, roomResync, pendingRepair{Room: row.LivekitRoom})
		h.reportPending()
		return fmt.Errorf("sync the grants of %s: %w", row.LivekitRoom, err)
	}
	defer unlock()
	defer func() {
		if result != nil {
			h.resetBotOnly(row.CallID)
		}
	}()
	cur, live, err := h.liveRow(ctx, d.repo, row)
	if err != nil || !live {
		return err
	}
	retried, _ := h.drainPending(ctx, d, ch, cur, maxRepairsPerTick, "")
	return h.reconcileRoom(ctx, d, ch, cur, userID, retried, nil)
}

// reconcileRoom reconciles every participant of row's room in scope — userID's devices when it is
// set, the identities match accepts when it is set — skipping the identities a drain already did.
// Over the whole room (neither set) it also frees the slot of every holder the room no longer lists
// (CALLS-5): a participant_left the webhook lost leaves it held otherwise, for the room's life. The
// release goes through reconcile with the slot dropped, so a holder the SFU still holds after all is
// demoted first. A room that cannot be listed, or a hold that runs out before every participant was
// reached, is left as a pending room resync. The caller holds the call's lock; ctx carries the hold's
// deadline.
func (h *Calls) reconcileRoom(ctx context.Context, d grantDeps, ch store.ChannelRow, row store.VoiceSessionRow,
	userID *id.ID, done map[string]bool, match func(string) bool) (result error) {
	defer func() {
		if result != nil {
			h.resetBotOnly(row.CallID)
		}
	}()
	h.leases.dropStale(row.CallID, row.LivekitRoom)
	lctx, cancel := sfuCtx(ctx)
	parts, err := d.tokens.Participants(lctx, row.LivekitRoom)
	cancel()
	if err != nil {
		h.leases.markPending(row.CallID, roomResync, pendingRepair{Room: row.LivekitRoom})
		return err
	}
	var errs []error
	whole := userID == nil && match == nil
	if whole {
		h.leases.clearPending(row.CallID, roomResync)
		present := make(map[string]bool, len(parts))
		for _, p := range parts {
			present[p.GetIdentity()] = true
		}
		pending := h.leases.pendingOf(row.CallID)
		for _, dev := range h.leases.holders(row.CallID, row.LivekitRoom) {
			identity := dev.String()
			if _, ok := pending[identity]; ok || present[identity] || done[identity] {
				continue
			}
			errs = append(errs, h.reconcile(ctx, d, ch, row, identity, nil, true))
		}
	}
	cut := map[string]bool{}
	complete := true
	for _, p := range parts {
		identity := p.GetIdentity()
		if done[identity] || (match != nil && !match(identity)) {
			continue
		}
		if err := ctx.Err(); err != nil {
			h.leases.markPending(row.CallID, roomResync, pendingRepair{Room: row.LivekitRoom})
			errs = append(errs, fmt.Errorf("the hold on %s ran out: %w", row.LivekitRoom, err))
			complete = false
			break
		}
		if whole && h.strikeListed(ctx, d, row, p) {
			cut[identity] = true // removed for a repeated F11 offence
			continue
		}
		wasCut, err := h.reconcileOutcome(ctx, d, ch, row, identity, userID, false)
		if wasCut {
			cut[identity] = true
		}
		errs = append(errs, err)
	}
	if whole && complete {
		botsOnly, err := h.trackAbsentLocked(ctx, row, parts, cut)
		errs = append(errs, err)
		if botsOnly && errors.Join(errs...) == nil {
			errs = append(errs, h.endOver(ctx, row, "bots_only"))
		}
	}
	return errors.Join(errs...)
}

// strikeListed is ruling (b): F11 checked from the room's participant list, so a publication whose
// track_published webhook was shed or lost is still penalised. Each listed track that is not
// dilla-sframe/1 or not of its source's kind counts one strike for its track sid — the webhook's
// strike for the same sid and this one are the same strike, whichever came first. A first strike is
// enforced by the reconcile that follows (it honours the penalty: a listen-only push); a repeat
// removes the device from the room here, and strikeListed reports true. The caller holds the call's
// lock.
func (h *Calls) strikeListed(ctx context.Context, d grantDeps, row store.VoiceSessionRow, p *livekit.ParticipantInfo) bool {
	dev, err := id.Parse(p.GetIdentity())
	if err != nil {
		return false
	}
	repeat := false
	for _, t := range p.GetTracks() {
		if !notDillaMedia(t) {
			continue
		}
		n, counted := h.leases.strikeTrack(row.CallID, row.LivekitRoom, dev, t.GetSid())
		if !counted {
			continue // the webhook, or an earlier sweep, already counted this publication
		}
		h.log.WarnContext(ctx, "the room sweep found a published track that is not dilla-sframe/1 or not of its declared kind",
			"room", row.LivekitRoom, "device_id", dev.String(), "track", t.GetSid(), "strikes", n)
		repeat = repeat || n > 1
	}
	if !repeat {
		return false
	}
	if err := h.removeDevice(ctx, d, row, dev, true); err != nil {
		h.log.WarnContext(ctx, "removing a repeat F11 offender did not land; it stays pending", "room", row.LivekitRoom, "err", err)
	}
	return true
}

// reconcile brings one participant of row's room within what it is entitled to now; the caller holds
// the call's lock, so the bits it resolves and the push it makes are one step against every share,
// unshare and other sync of the call. It fails closed (DEV-44, the spec's "the SFU session is cut
// immediately on kick"):
//
//   - an identity that is no device of this instance (a "#" shadow, a foreign name, an id naming no
//     device) is removed by its exact identity, whatever userID scopes;
//   - a device whose lookup or resolution fails is pushed the no-publish grant;
//   - a barred device (revoked, quarantined, or of a disabled or deleted user), a device whose
//     user lost view_channel or connect, and a device that is no current leaf of the call group
//     (a Remove of its leaf has been committed: the delivery service's evictor, G29) are cut from
//     the room at once with every "#" shadow of it (the leaf Remove still follows through the
//     delivery service where it has not already);
//   - otherwise the device is pushed its complete permission, the camera and screen sources only
//     while it holds a slot of this room and dropSlot is false, and nothing at all while it is
//     penalised for publishing media that is not dilla-sframe/1 (F11); a device that keeps neither
//     video nor screen_share (or dropSlot, or a penalty) loses its slot once the push landed.
//
// A cut or push that does not land is recorded as a pending repair and the error returned; the slot
// stays held while it is pending, and the token mint and the /rtc gate refuse the device for the
// call. One that lands clears the repair. A participant the SFU no longer holds needs nothing and
// loses its slot.
func (h *Calls) reconcile(ctx context.Context, d grantDeps, ch store.ChannelRow, row store.VoiceSessionRow,
	identity string, userID *id.ID, dropSlot bool) error {
	_, err := h.reconcileOutcome(ctx, d, ch, row, identity, userID, dropSlot)
	return err
}

// reconcileOutcome is reconcile that also reports whether the participant was cut from the room
// (removed, or its removal left pending) rather than kept with a permission.
func (h *Calls) reconcileOutcome(ctx context.Context, d grantDeps, ch store.ChannelRow, row store.VoiceSessionRow,
	identity string, userID *id.ID, dropSlot bool) (bool, error) {
	pending := func(err error, drop bool) error {
		h.leases.markPending(row.CallID, identity, pendingRepair{Room: row.LivekitRoom, DropSlot: drop})
		return err
	}
	dev, err := id.Parse(identity)
	if err != nil {
		return true, h.cutIdentity(ctx, d, row, identity, pending)
	}
	dv, err := d.repo.GetDevice(ctx, dev)
	if errors.Is(err, store.ErrNotFound) {
		return true, h.cutIdentity(ctx, d, row, identity, pending)
	}
	if err != nil {
		return false, pending(errors.Join(fmt.Errorf("device %s: %w", dev, err), h.pushNone(ctx, d, row, dev)), dropSlot)
	}
	_, wasPending := h.leases.pendingOf(row.CallID)[identity]
	if userID != nil && dv.UserID != *userID && !wasPending {
		return false, nil
	}
	bar, err := barred(ctx, d.repo, dv)
	if err != nil {
		return false, pending(errors.Join(fmt.Errorf("device %s: %w", dev, err), h.pushNone(ctx, d, row, dev)), dropSlot)
	}
	var bits Bits
	leaf := false
	if !bar {
		if bits, err = d.res.Resolve(ctx, dv.UserID, ch); err != nil {
			return false, pending(errors.Join(fmt.Errorf("device %s: %w", dev, err), h.pushNone(ctx, d, row, dev)), dropSlot)
		}
		if leaf, err = leafOfCall(ctx, d.repo, row, dev); err != nil {
			return false, pending(errors.Join(fmt.Errorf("device %s: %w", dev, err), h.pushNone(ctx, d, row, dev)), dropSlot)
		}
	}
	if bar || !leaf || !bits.Has(PermViewChannel) || !bits.Has(PermConnect) {
		return true, h.removeDevice(ctx, d, row, dev, false)
	}
	// R34: a call of the built-in AFK channel is listen-only on this push too, as a penalty is.
	penalised := h.leases.penalised(row.CallID, row.LivekitRoom, dev) || h.inAFK(ctx, row)
	leased := h.leases.held(row.CallID, row.LivekitRoom, dev) && !dropSlot && !penalised
	keepsSlot := (bits.Has(PermVideo) || bits.Has(PermScreenShare)) && !dropSlot && !penalised
	perm := sfu.PublishGrant(bits.Has(PermSpeak) && !penalised, bits.Has(PermVideo) && leased, bits.Has(PermScreenShare) && leased)
	perm, err = restrictBotCallGrant(ctx, d.repo, dev, perm)
	if err != nil {
		return false, pending(errors.Join(fmt.Errorf("device %s: %w", dev, err), h.pushNone(ctx, d, row, dev)), dropSlot)
	}
	keepsSlot = keepsSlot && (slices.Contains(perm.CanPublishSources, livekit.TrackSource_CAMERA) ||
		slices.Contains(perm.CanPublishSources, livekit.TrackSource_SCREEN_SHARE))
	pctx, cancel := sfuCtx(ctx)
	err = d.tokens.UpdatePermission(pctx, row.LivekitRoom, identity, perm)
	cancel()
	if err != nil {
		if !errors.Is(err, sfu.ErrNoParticipant) {
			return false, pending(fmt.Errorf("push the permission of %s: %w", dev, err), dropSlot || penalised)
		}
		keepsSlot = false // the room no longer holds it: it publishes nothing
	}
	if !keepsSlot {
		h.leases.release(row.CallID, dev)
	}
	h.leases.clearPending(row.CallID, identity)
	return false, nil
}

// removeDevice disconnects dev and every "#" shadow of it from row's room, frees its slot and clears
// its repair. A removal the SFU does not take falls back to the no-publish push and is a pending
// repair that drops the slot; cut marks it as a removal a retry repeats whatever the device's
// entitlement (an eviction, F11's repeat). The caller holds the call's lock.
func (h *Calls) removeDevice(ctx context.Context, d grantDeps, row store.VoiceSessionRow, dev id.ID, cut bool) error {
	// Every cut from a room also cuts the device from the relay as of now (review I1, and the
	// commit review's parity finding): barred, no longer a leaf, lost view_channel or connect,
	// evicted, or disconnected for media that is not dilla's. A device that may still take part
	// in a call gets a credential issued after the cut from its next start, which the relay admits.
	h.revokeRelay(dev)
	rctx, cancel := sfuCtx(ctx)
	err := d.tokens.RemoveParticipants(rctx, row.LivekitRoom, dev)
	cancel()
	if err != nil {
		h.leases.markPending(row.CallID, dev.String(), pendingRepair{Room: row.LivekitRoom, DropSlot: true, Cut: cut})
		return errors.Join(fmt.Errorf("cut device %s: %w", dev, err), h.pushNone(ctx, d, row, dev))
	}
	h.leases.release(row.CallID, dev)
	h.leases.clearPending(row.CallID, dev.String())
	if h.counters != nil {
		h.counters.CallCut()
	}
	return nil
}

// leafOfCall reports whether dev holds a leaf of the call group row's call was opened on in the
// group's current epoch — requireCurrentLeaf's rule (leafIn), so the room keeps exactly the devices
// the /rtc gate would admit. A call with no group, a group that is gone, a closed group and an
// epoch-unknown group hold no leaf.
func leafOfCall(ctx context.Context, repo store.Repository, row store.VoiceSessionRow, dev id.ID) (bool, error) {
	if row.GroupID == nil {
		return false, nil
	}
	group, err := repo.GetGroup(ctx, *row.GroupID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return leafIn(ctx, repo, group, dev)
}

// cutIdentity removes a participant that is no device of this instance by its exact identity.
func (h *Calls) cutIdentity(ctx context.Context, d grantDeps, row store.VoiceSessionRow, identity string,
	pending func(error, bool) error) error {
	rctx, cancel := sfuCtx(ctx)
	err := d.tokens.RemoveParticipant(rctx, row.LivekitRoom, identity)
	cancel()
	if err != nil {
		return pending(fmt.Errorf("remove %q: %w", identity, err), true)
	}
	h.leases.clearPending(row.CallID, identity)
	if h.counters != nil {
		h.counters.CallCut()
	}
	return nil
}

// pushNone pushes dev the no-publish grant: the fallback of a cut or a lookup that failed. Its own
// failure is reported; the caller records the repair either way.
func (h *Calls) pushNone(ctx context.Context, d grantDeps, row store.VoiceSessionRow, dev id.ID) error {
	perm, lookupErr := restrictBotCallGrant(ctx, d.repo, dev, sfu.PublishGrant(false, false, false))
	if lookupErr != nil {
		// An unknown account may be a bot. Never restore receive rights during a failure.
		perm = &livekit.ParticipantPermission{}
	}
	pctx, cancel := sfuCtx(ctx)
	err := d.tokens.UpdatePermission(pctx, row.LivekitRoom, dev.String(), perm)
	cancel()
	if err != nil && !errors.Is(err, sfu.ErrNoParticipant) {
		return errors.Join(lookupErr, fmt.Errorf("fail-closed demotion of %s: %w", dev, err))
	}
	return lookupErr
}

// drainPending retries at most limit pending repairs of row's call — only identity only's when only
// is set, which is what a request drives — and answers the identities it retried and how many
// repairs it attempted. It stops when ctx (the hold's deadline) ends. The caller holds the call's
// lock. Repairs recorded for an older room of the call are dropped: that room is closed.
func (h *Calls) drainPending(ctx context.Context, d grantDeps, ch store.ChannelRow, row store.VoiceSessionRow,
	limit int, only string) (map[string]bool, int) {
	retried := map[string]bool{}
	if d.tokens == nil {
		return retried, 0
	}
	n := 0
	for identity, r := range h.leases.pendingOf(row.CallID) {
		if r.Room != row.LivekitRoom {
			h.leases.clearPending(row.CallID, identity)
			continue
		}
		if only != "" && identity != only {
			continue
		}
		if n >= limit || ctx.Err() != nil {
			break
		}
		n++
		if h.counters != nil {
			h.counters.CallGrantRetry()
		}
		h.log.WarnContext(ctx, "retrying a call grant repair", "device", identity, "room", r.Room, "drop_slot", r.DropSlot)
		var err error
		dev, perr := id.Parse(identity)
		switch {
		case identity == roomResync:
			err = h.reconcileRoom(ctx, d, ch, row, nil, retried, nil)
		case r.Cut && perr == nil:
			retried[identity] = true
			err = h.removeDevice(ctx, d, row, dev, true)
		default:
			retried[identity] = true
			err = h.reconcile(ctx, d, ch, row, identity, nil, r.DropSlot)
		}
		if err != nil {
			h.log.WarnContext(ctx, "a call grant repair did not land; it stays pending", "device", identity, "room", r.Room, "err", err)
		}
	}
	return retried, n
}

// RetryPending drives the pending repairs of every call once, at most maxRepairsPerTick of them, and
// forgets the slots and repairs of every call that has ended (its room is closed) and the slots of
// an older room of a live call. The retry loop (StartRetries) runs it, so a cut or demotion the SFU
// refused converges without another event in the call; a call whose lock is busy is left for the
// next pass. Each call's hold is bounded by callHoldBackground, and the whole pass by retryPassBudget
// (CALLS-M2: calls that hold only slots spend no repair budget, and each waits up to callLockWait for
// its lock). A cut or an eviction queued while the pass runs is served between two calls, not after
// the pass. After the repairs, and never on their budget, it completes at most maxDeferredPerTick
// deferred leaves whose newer session never joined, within deferredPassBudget
// (CallEvents.completeDeferredLeaves; review S1: a slow SFU must not starve a pending cut).
func (h *Calls) RetryPending(ctx context.Context) {
	h.retryRepairs(ctx)
	if h.events != nil {
		dctx, cancel := context.WithTimeout(ctx, deferredPassBudget)
		h.events.completeDeferredLeaves(dctx, maxDeferredPerTick)
		cancel()
	}
}

// retryRepairs is RetryPending's repair pass.
func (h *Calls) retryRepairs(ctx context.Context) {
	budget := maxRepairsPerTick
	pctx, cancel := context.WithTimeout(ctx, retryPassBudget)
	defer cancel()
	defer h.flushLeafRemovals(ctx) // a room resync's whole-room reconcile may have decided some
	for _, call := range h.leases.trackedCalls() {
		if budget <= 0 || pctx.Err() != nil {
			return
		}
		h.serveQueued(ctx)
		n, err := h.retryTracked(pctx, call, budget)
		if err != nil {
			h.log.WarnContext(ctx, "a call's grant repairs were skipped this pass", "call", call, "err", err)
		}
		budget -= n
	}
}

// retryOwn drives dev's own pending repair in call, if it has one: what a start or the /rtc gate does
// before it decides about dev. It waits at most callLockWait for the call's lock and holds it at most
// callHoldRequest. An error is a lock that could not be taken (a *server.Error, 503) or a store
// failure.
func (h *Calls) retryOwn(ctx context.Context, call, dev id.ID) error {
	if !h.leases.isPending(call, dev) {
		return nil
	}
	ctx, unlock, err := h.lock(ctx, call, callLockWait, callHoldRequest)
	if err != nil {
		return err
	}
	defer unlock()
	row, err := h.repo.GetVoiceSession(ctx, call)
	if errors.Is(err, store.ErrNotFound) || (err == nil && row.Ended != nil) {
		h.leases.dropCall(call)
		return nil
	}
	if err != nil || h.sfu == nil {
		return err
	}
	ch, err := h.repo.GetChannel(ctx, row.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		return h.endLocked(ctx, row, "channel_deleted") // the channel is gone, so is its call (CALLS-1)
	}
	if err != nil {
		return err
	}
	h.drainPending(ctx, h.deps(), ch, row, 1, dev.String())
	return nil
}

// retryTracked is the retry loop's visit of one call with a repair outstanding or a slot held: it
// forgets everything of a call that ended (its DELETE could not take the lock) and the slots of an
// older room, and drives at most limit repairs, answering how many it attempted.
func (h *Calls) retryTracked(ctx context.Context, call id.ID, limit int) (int, error) {
	ctx, unlock, err := h.lock(ctx, call, callLockWait, callHoldBackground)
	if err != nil {
		return 0, err
	}
	defer unlock()
	row, err := h.repo.GetVoiceSession(ctx, call)
	if errors.Is(err, store.ErrNotFound) || (err == nil && row.Ended != nil) {
		h.leases.dropCall(call)
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	h.leases.dropStale(call, row.LivekitRoom)
	if len(h.leases.pendingOf(call)) == 0 || h.sfu == nil {
		return 0, nil
	}
	ch, err := h.repo.GetChannel(ctx, row.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, h.endLocked(ctx, row, "channel_deleted") // the channel is gone, so is its call (CALLS-1)
	}
	if err != nil {
		return 0, err
	}
	_, n := h.drainPending(ctx, h.deps(), ch, row, limit, "")
	return n, nil
}

// SweepRooms is the room sweep: it lists the SFU's rooms and, for each, under that call's lock,
// reconciles every participant of a live call's room (reconcileRoom: a barred device, or one that
// lost view_channel or connect, is cut; an identity that is no device is removed) and deletes a room
// that belongs to no call, to an ended call or to an older room of a live call — which is also the
// retry of a DeleteRoom that failed when the call ended. It is how a change written by another
// process (`dillad admin user disable`, an admin device revoke), a membership change whose own sync
// failed and a device admitted at /rtc just before it lost access converge without an event in the
// call. A pass visits at most maxRoomsPerSweep rooms within sweepPassBudget, going on from where the
// last pass stopped; each room's hold is bounded by callHoldBackground, and a busy call is left for
// the next pass. Passes never overlap.
func (h *Calls) SweepRooms(ctx context.Context) {
	if h.sfu == nil {
		return
	}
	h.sweepMu.Lock()
	defer h.sweepMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, sweepPassBudget)
	defer cancel()
	rooms, err := h.listRooms(ctx)
	if err != nil {
		h.resetBotOnly(id.ID{})
		h.log.WarnContext(ctx, "listing the SFU's rooms for the sweep failed", "err", err)
		return
	}
	if len(rooms) == 0 {
		return
	}
	slices.Sort(rooms)
	start := sort.SearchStrings(rooms, h.sweepCursor)
	if start < len(rooms) && rooms[start] == h.sweepCursor {
		start++
	}
	for i := 0; i < len(rooms) && i < maxRoomsPerSweep; i++ {
		if ctx.Err() != nil {
			return
		}
		room := rooms[(start+i)%len(rooms)]
		h.sweepCursor = room
		h.serveQueued(ctx)
		if err := h.sweepRoom(ctx, room, nil); err != nil {
			h.log.WarnContext(ctx, "sweeping a call room did not finish; the next pass retries it", "room", room, "err", err)
		}
		h.flushLeafRemovals(ctx) // the room's lock is released: its decided Removes go to the DS now
	}
}

// CutDevice requests that device, and every "#" shadow of it, be cut from every live call room the
// SFU holds it in. It never calls the SFU, takes no lock but the request set's and returns at once:
// the in-process revocation and quarantine paths call it after their commit, on request paths and
// next to the delivery service, which a slow SFU must not stall. The device id joins a small,
// bounded, de-duplicated set, and the retry loop is woken to cut it on its own goroutine at once
// (falling back to its tick). Meanwhile the device is barred in the database, so the /rtc gate,
// start and share refuse it. The cut runs the same reconcile as the sweep for those identities
// only: a barred device is removed with its slot freed (pending when the SFU refuses), one that is
// not barred keeps exactly its current permission. A request that finds the work queue full
// (callqueue.go) raises its resync-all flag with a WARN: the loop, woken all the same, runs a room
// sweep at once, which cuts every barred participant of every live call room (CALLS-M4: one user who
// logs many devices out cannot crowd another user's cut past the sweep's bound).
func (h *Calls) CutDevice(ctx context.Context, device id.ID) {
	h.requestCut(ctx, device, false)
}

// CutUser is CutDevice for every device of userID, resolved on the retry loop: the in-process admin
// disable calls it.
func (h *Calls) CutUser(ctx context.Context, userID id.ID) {
	h.requestCut(ctx, userID, true)
}

func (h *Calls) requestCut(ctx context.Context, who id.ID, user bool) {
	// The relay first, synchronously and whatever happens below (review I1): it closes sockets and
	// writes a map, nothing more. A user's devices are resolved on the retry loop (processCuts);
	// the in-process paths that disable a user also cut each device, and the relay's barred lookup
	// covers the rest.
	if !user {
		h.revokeRelay(who)
	}
	if h.sfu == nil {
		return
	}
	h.cutMu.Lock()
	h.initQueueLocked()
	set := h.cutDevices
	if user {
		set = h.cutUsers
	}
	full := !set[who] && h.queuedLocked() >= maxCutRequests
	if full {
		h.overflowLocked()
	} else {
		set[who] = true
	}
	h.cutMu.Unlock()
	if full {
		h.warnOverflow(ctx, "cut", "id", who, "user", user)
	}
	h.wake() // a wake already pending is enough: the loop takes the whole set
}

// processCuts takes the whole cut-request set and cuts its devices (and the devices of its users)
// from every live call room in one pass bounded by cutDeviceBudget. It runs on the retry loop.
func (h *Calls) processCuts(ctx context.Context) {
	h.cutMu.Lock()
	devices, users := h.cutDevices, h.cutUsers
	h.cutDevices, h.cutUsers = nil, nil
	h.cutMu.Unlock()
	if len(devices) == 0 && len(users) == 0 {
		return
	}
	h.cutPasses.Add(1)
	match := make(map[string]bool, len(devices))
	for d := range devices {
		match[d.String()] = true
	}
	for u := range users {
		rows, err := h.repo.ListDevicesByUser(ctx, u)
		if err != nil {
			h.log.WarnContext(ctx, "listing a user's devices to cut them from calls failed; the room sweep covers it",
				"user", u, "err", err)
			continue
		}
		for _, d := range rows {
			match[d.ID.String()] = true
			h.revokeRelay(d.ID)
		}
	}
	h.cutMatching(ctx, func(identity string) bool {
		base, _, _ := strings.Cut(identity, "#")
		return match[base]
	})
}

func (h *Calls) cutMatching(ctx context.Context, match func(string) bool) {
	ctx, cancel := context.WithTimeout(ctx, cutDeviceBudget)
	defer cancel()
	rooms, err := h.listRooms(ctx)
	if err != nil {
		h.log.WarnContext(ctx, "listing the SFU's rooms for a cut failed; the room sweep covers it", "err", err)
		return
	}
	for _, room := range rooms {
		if ctx.Err() != nil {
			return
		}
		if err := h.sweepRoom(ctx, room, match); err != nil {
			h.log.WarnContext(ctx, "cutting a device from a call room did not finish; the room sweep retries it",
				"room", room, "err", err)
		}
	}
}

func (h *Calls) listRooms(ctx context.Context) ([]string, error) {
	lctx, cancel := sfuCtx(ctx)
	defer cancel()
	return h.sfu.Rooms(lctx)
}

// sweepRoom is one room of the sweep (match nil) or of a cut (match set). A room named for no call
// is deleted by the sweep and left alone by a cut. A room of a call that has ended or moved to a newer
// room is deleted by both: everyone in it is cut, and nobody belongs there. A live call whose channel
// is gone (deleted) or whose call group is closed or gone is over (CALLS-1, CALLS-2): it is ended
// and its room deleted, never skipped. Any other live call's room is reconciled under the call's
// lock, every participant (sweep) or those match accepts (cut).
func (h *Calls) sweepRoom(ctx context.Context, room string, match func(string) bool) (result error) {
	callHex, _, ok := strings.Cut(room, "-")
	callID, perr := id.Parse(callHex)
	if !ok || perr != nil {
		if match != nil {
			return nil
		}
		return h.deleteRoom(ctx, room)
	}
	ctx, unlock, err := h.lock(ctx, callID, callLockWait, callHoldBackground)
	if err != nil {
		h.resetBotOnly(callID)
		return err
	}
	defer unlock()
	defer func() {
		if result != nil {
			h.resetBotOnly(callID)
		}
	}()

	row, err := h.repo.GetVoiceSession(ctx, callID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return h.deleteRoom(ctx, room)
	case err != nil:
		return err
	case row.Ended != nil || row.LivekitRoom != room:
		if row.Ended != nil {
			h.leases.dropCall(callID)
		} else {
			h.leases.dropStale(callID, row.LivekitRoom)
		}
		h.forgetMembers(callID, room)
		return h.deleteRoom(ctx, room)
	}
	ch, err := h.repo.GetChannel(ctx, row.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		return h.endOver(ctx, row, "channel_deleted")
	}
	if err != nil {
		return err
	}
	if over, err := h.groupOver(ctx, row); err != nil {
		return err
	} else if over {
		return h.endOver(ctx, row, "closed_group")
	}
	return h.reconcileRoom(ctx, h.deps(), ch, row, nil, nil, match)
}

// groupOver reports whether row's live call is on a call group that is closed or gone, which no
// commit can change any more: such a call cannot go on.
func (h *Calls) groupOver(ctx context.Context, row store.VoiceSessionRow) (bool, error) {
	if row.GroupID == nil {
		return false, nil
	}
	g, err := h.repo.GetGroup(ctx, *row.GroupID)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return g.ClosedAt != nil, nil
}

// endOver ends a live call the sweep or a cut found over, under the call's lock the caller holds, and
// deletes its room at once; the rest of its end (its group, voice_state) follows on the queue.
func (h *Calls) endOver(ctx context.Context, row store.VoiceSessionRow, reason string) error {
	if err := h.endLocked(ctx, row, reason); err != nil {
		return err
	}
	return h.deleteRoom(ctx, row.LivekitRoom)
}

// deleteRoom closes a room that belongs to no live call: everyone in it is disconnected, and with
// room.auto_create false nobody can rejoin it.
func (h *Calls) deleteRoom(ctx context.Context, room string) error {
	dctx, cancel := sfuCtx(ctx)
	defer cancel()
	if err := h.sfu.DeleteRoom(dctx, room); err != nil {
		return fmt.Errorf("delete the orphan room %s: %w", room, err)
	}
	h.log.InfoContext(ctx, "the room sweep closed a room that belongs to no live call", "room", room)
	return nil
}

// StartRetries runs the retry loop: as soon as work is queued (callqueue.go: a cut, an eviction, a
// teardown, a grant sync) it does it (processQueue), and every `every` it also drives the calls'
// pending repairs (RetryPending) and, at most every RoomSweepInterval, sweeps the SFU's rooms
// (SweepRooms), all on its own goroutine, so a slow or hung SFU never delays a request or another
// maintenance duty. A pass never overlaps the next — a tick that comes while one runs is dropped —
// each SFU call is bounded by sfuCallTimeout, each hold of a call's lock by callHoldBackground, each
// pass by its budget and the sweep's own bounds. stop ends the loop and waits for the pass in
// flight, which its cancelled context cuts short; the composition root calls it before the SFU stops.
func (h *Calls) StartRetries(every time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	sweepEvery := h.sweepEvery
	go func() {
		defer close(done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		var lastSweep time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.cutWake:
				h.processQueue(ctx)
				h.reportPending()
			case <-tick.C:
				h.processQueue(ctx)
				h.RetryPending(ctx)
				h.SweepAFK(ctx)
				if time.Since(lastSweep) >= sweepEvery {
					h.SweepRooms(ctx)
					lastSweep = time.Now()
				}
				h.reportPending()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// restrictBotCallGrant uses authoritative account identity for every grant seam. Managed
// bots can send microphone audio when permitted, but can never receive tracks or send video.
// Callers must propagate lookup failures; demotion uses a fully closed fallback instead.
func restrictBotCallGrant(ctx context.Context, repo store.Repository, dev id.ID, perm *livekit.ParticipantPermission) (*livekit.ParticipantPermission, error) {
	device, err := repo.GetDevice(ctx, dev)
	if err != nil {
		return nil, err
	}
	bot, err := botaccess.IsBotUser(ctx, repo, device.UserID)
	if err != nil {
		return nil, err
	}
	if !bot {
		return perm, nil
	}
	grant := sfu.PublishGrant(perm.CanPublish && slices.Contains(perm.CanPublishSources, livekit.TrackSource_MICROPHONE), false, false)
	grant.CanSubscribe = false
	return grant, nil
}

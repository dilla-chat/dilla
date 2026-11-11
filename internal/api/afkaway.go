package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/livekit/protocol/livekit"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// AFK means disconnected (AW2): a member who is away is listed under the community's AFK channel and
// has no voice connection. The instance stores the away roster per (community, user)
// (community_afk_away), announces every change with op 55 afk_away to the AFK channel's audience, and
// never hosts a call in the AFK channel (R-AW1).
//
// Away and calls never overlap (R-AW2): any admission of any device of the user clears every row of
// that user. A row is set by the sweep's cut when no other device of the user is in a call (R-AW3)
// and by PUT …/afk/away, which cuts every device of the user first. It is cleared by an admission,
// by a message or a typing op in that community (Q1), by the user holding no gateway connection for
// 30 continuous seconds (Q2, revised: a reload or a short blip keeps away), and by leaving, a kick or
// a ban (the store's cascade, announced by AFKMemberRemoved). No client route clears it (Q3).
//
// Locks (fix rounds 1 and 2). awayMu serialises the roster's writes, and nothing holds it while it
// waits on a send or on an audience read. awaySet, the in-memory index of who may be away, sits
// behind its own RWMutex (awayIdxMu): every typing op, upload, readable post and admission of a user
// with no entry is a read-locked map lookup with no store call, and never waits on a mark or a clear,
// whose write lock is held only for a map update. The index is a superset of the stored rows (a
// cascade leaves a stale entry, which the next clear drops), exact while one process serves the
// database, and seeded at boot (retried with backoff until it lands); until then every user may be
// away and the clears go to the store. A writer appends its op 55 notes, in row order, to a FIFO
// under awayMu (an append only); one drainer goroutine sends them with no lock held, resolving each
// audience as it goes, so frames keep the order the rows changed. StopAFK ends the drainer and the
// maintenance passes and waits for them.
//
// The decision to mark is checked under afkMu against the admissions recorded since it was taken
// (afkAdmitSeq), and an admission records itself under afkMu BEFORE it takes awayMu to clear: so
// either the mark sees the admission and is skipped, or the admission's clear runs after the mark's
// write. The admission wins either way (R-AW7). Lock order: awayMu, then afkMu or awaySendMu.

const (
	// afkAwaySnapshotMax caps the snapshot's away list (GET /v1/communities/{id}/afk, 5th element).
	afkAwaySnapshotMax = 1024
	// afkBootGrace is how long after a start the rows of users with no gateway connection are kept,
	// so reconnecting clients keep theirs (R-AW5).
	afkBootGrace = 60 * time.Second
	// afkAdmitMemory is how long a device's latest admission is remembered for the mark's checks.
	afkAdmitMemory = 10 * time.Minute
	// afkOfflineGrace is how long a user must hold no gateway connection, without a break, before
	// their away rows are cleared (Q2, as revised by Jonas on 2026-10-21): a reload or a short
	// network blip keeps a person away.
	afkOfflineGrace = 30 * time.Second
	// afkRecoverWindow is how far back an admission makes a device one that may still be recovering
	// its call after the SFU dropped it (the web client retries for 120 s): PUT …/afk/away records
	// pending 2 for such a device even though no room lists it (R3-M1).
	afkRecoverWindow = 2 * time.Minute
)

// afkAdmit is a device's latest admission: its number and when (the instance clock).
type afkAdmit struct {
	seq uint64
	at  time.Time
}

// afkAwayEntry is one element of the snapshot's away list: [user_id, since_ts].
type afkAwayEntry struct {
	_       struct{} `cbor:",toarray"`
	UserID  id.ID
	SinceTS uint64
}

// awayNote is one op 55 to send: [community, user, since].
type awayNote struct {
	community, user id.ID
	since           uint64
}

// AFKActivity is what the message routes report a user's activity in a channel to (Q1).
type AFKActivity interface {
	AFKActiveInChannel(ctx context.Context, user, channel id.ID)
}

// WithAFKAway sets where op 55 goes (the gateway's DeliverUser) and how to tell whether a user holds
// a live gateway connection, starts the op 55 drainer, and returns h. A nil connected counts every
// user as connected.
func (h *Calls) WithAFKAway(gw VoiceGateway, connected func(id.ID) bool) *Calls {
	h.awayMu.Lock()
	h.awayGW, h.awayConnected = gw, connected
	h.awayMu.Unlock()
	if gw != nil {
		h.startDrainer()
	}
	return h
}

// WithAFKDraining sets how to tell that the gateway is shutting down (R3): while it drains every
// user looks disconnected, so the post-boot clear does not run. It returns h.
func (h *Calls) WithAFKDraining(draining func() bool) *Calls {
	h.awayMu.Lock()
	defer h.awayMu.Unlock()
	h.awayDraining = draining
	return h
}

func (h *Calls) afkStage(stage string) {
	if f := h.afkHook; f != nil {
		f(stage)
	}
}

// connectedLocked reports whether user holds a gateway connection. The caller holds awayMu.
func (h *Calls) connectedLocked(user id.ID) bool {
	return h.awayConnected == nil || h.awayConnected(user)
}

// draining reports whether the gateway is shutting down.
func (h *Calls) draining() bool {
	h.awayMu.Lock()
	f := h.awayDraining
	h.awayMu.Unlock()
	return f != nil && f()
}

// idxMayBeAway reports whether user may hold an away row (in cid, when it is set). Before the index
// is seeded everyone may. It takes the index's read lock alone.
func (h *Calls) idxMayBeAway(user id.ID, cid *id.ID) bool {
	h.awayIdxMu.RLock()
	defer h.awayIdxMu.RUnlock()
	if !h.awaySeeded {
		return true
	}
	cs := h.awaySet[user]
	if cid == nil {
		return len(cs) > 0
	}
	_, ok := cs[*cid]
	return ok
}

// idxKnows reports whether the seeded index holds user in cid; false before the seed (R5: a user the
// seed never loaded is not announced).
func (h *Calls) idxKnows(user, cid id.ID) bool {
	h.awayIdxMu.RLock()
	defer h.awayIdxMu.RUnlock()
	_, ok := h.awaySet[user][cid]
	return h.awaySeeded && ok
}

// idxHas reports whether the index holds user (in cid, when it is set), seeded or not. Unseeded, it
// holds the entries of the marks made since the start, an in-flight one's included: a mark notes its
// entry before its checks (A-N1).
func (h *Calls) idxHas(user id.ID, cid *id.ID) bool {
	h.awayIdxMu.RLock()
	defer h.awayIdxMu.RUnlock()
	cs := h.awaySet[user]
	if cid == nil {
		return len(cs) > 0
	}
	_, ok := cs[*cid]
	return ok
}

// mayBeAway is the lookup of the activity paths: a read lock, never awayMu.
func (h *Calls) mayBeAway(user id.ID) bool { return h.idxMayBeAway(user, nil) }

func (h *Calls) awaySeededNow() bool {
	h.awayIdxMu.RLock()
	defer h.awayIdxMu.RUnlock()
	return h.awaySeeded
}

// idxNote adds user in cid to the index and reports whether it was there already.
func (h *Calls) idxNote(user, cid id.ID) bool {
	h.awayIdxMu.Lock()
	defer h.awayIdxMu.Unlock()
	if h.awaySet == nil {
		h.awaySet = make(map[id.ID]map[id.ID]struct{})
	}
	if h.awaySet[user] == nil {
		h.awaySet[user] = make(map[id.ID]struct{})
	}
	_, had := h.awaySet[user][cid]
	h.awaySet[user][cid] = struct{}{}
	return had
}

func (h *Calls) idxDrop(user, cid id.ID) {
	h.awayIdxMu.Lock()
	defer h.awayIdxMu.Unlock()
	if cs := h.awaySet[user]; cs != nil {
		delete(cs, cid)
		if len(cs) == 0 {
			delete(h.awaySet, user)
		}
	}
}

func (h *Calls) idxDropUser(user id.ID) {
	h.awayIdxMu.Lock()
	defer h.awayIdxMu.Unlock()
	delete(h.awaySet, user)
}

// releaseAway queues notes, in row order, for the drainer and lets awayMu go. The caller holds
// awayMu; it is released on return. Nothing here waits on a send.
func (h *Calls) releaseAway(_ context.Context, notes []awayNote) {
	if len(notes) > 0 && h.awayGW != nil {
		h.awayQMu.Lock()
		h.awayQueue = append(h.awayQueue, notes...)
		h.awayQMu.Unlock()
		select {
		case h.awayWake <- struct{}{}:
		default:
		}
	}
	h.awayMu.Unlock()
}

// startDrainer starts the op 55 drainer once, unless StopAFK already ran.
func (h *Calls) startDrainer() {
	h.afkLifeMu.Lock()
	defer h.afkLifeMu.Unlock()
	if h.drainerOn || h.afkStopped {
		return
	}
	h.drainerOn = true
	h.afkWG.Add(1)
	go func() {
		defer h.afkWG.Done()
		h.drain(h.afkCtx)
	}()
}

// drain is the drainer: it sends the queued notes in order, holding no lock while it resolves an
// audience or delivers, until ctx ends.
func (h *Calls) drain(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.awayWake:
		}
		for {
			h.awayQMu.Lock()
			batch := h.awayQueue
			h.awayQueue = nil
			h.awaySending = len(batch) > 0
			h.awayQMu.Unlock()
			if len(batch) == 0 {
				break
			}
			h.awayMu.Lock()
			gw := h.awayGW
			h.awayMu.Unlock()
			// One audience per community per batch, and the final state per (community, user).
			audiences := make(map[id.ID][]id.ID)
			for _, n := range coalesceAway(batch) {
				if ctx.Err() != nil {
					break
				}
				audience, ok := audiences[n.community]
				if !ok {
					audience = h.awayAudience(ctx, n.community)
					audiences[n.community] = audience
				}
				sendAway(gw, n, audience)
			}
			h.awayQMu.Lock()
			h.awaySending = false
			h.awayQMu.Unlock()
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// flushAway waits, up to d, until the queue is empty and no batch is being sent.
func (h *Calls) flushAway(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		h.awayQMu.Lock()
		idle := len(h.awayQueue) == 0 && !h.awaySending
		h.awayQMu.Unlock()
		if idle {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// StopAFK ends the op 55 drainer and every maintenance pass (their context is cancelled) and waits
// for them (R3). After it, no new pass starts. Queued op 55 that were not sent are dropped: the
// snapshot is the record.
func (h *Calls) StopAFK() {
	h.afkLifeMu.Lock()
	h.afkStopped = true
	h.afkLifeMu.Unlock()
	h.afkCancel()
	h.afkWG.Wait()
}

// GoAFKMaintenance runs AFKMaintenance on its own tracked goroutine under the AFK context, unless
// StopAFK ran (R3). dillad calls it from the maintenance tick.
func (h *Calls) GoAFKMaintenance() {
	h.afkLifeMu.Lock()
	defer h.afkLifeMu.Unlock()
	if h.afkStopped {
		return
	}
	h.afkWG.Add(1)
	go func() {
		defer h.afkWG.Done()
		h.AFKMaintenance(h.afkCtx)
	}()
}

// awayAudience is every user who may view the community's AFK channel (its EligibleUsers, the
// voice_state pattern). A community with no AFK channel, or a failure to read its audience, answers
// nobody: the snapshot is the record.
func (h *Calls) awayAudience(ctx context.Context, cid id.ID) []id.ID {
	cfg, err := h.repo.GetCommunityAFK(ctx, cid)
	if err != nil || cfg.ChannelID == nil {
		return nil
	}
	ch, err := h.repo.GetChannel(ctx, *cfg.ChannelID)
	if err != nil {
		return nil
	}
	audience, err := EligibleUsers(ctx, h.repo, ch)
	if err != nil {
		if ctx.Err() == nil {
			h.log.WarnContext(ctx, "resolving the AFK channel's audience for afk_away failed", "community", cid, "err", err)
		}
		return nil
	}
	return audience
}

// sendAway delivers one op 55 to audience.
func sendAway(gw VoiceGateway, n awayNote, audience []id.ID) {
	payload, err := gateway.AFKAwayPayload(n.community, n.user, n.since)
	if err != nil {
		return
	}
	f := gateway.Frame{Op: gateway.OpAFKAway, Payload: payload}
	for _, u := range audience {
		gw.DeliverUser(u, f)
	}
}

// coalesceAway keeps, of the notes for one (community, user) in a batch, only the last: the final
// state is what a client must end on, and an earlier one would be superseded within the same send.
// The kept notes stay in their order (N-C).
func coalesceAway(batch []awayNote) []awayNote {
	type key struct{ community, user id.ID }
	seen := make(map[key]bool, len(batch))
	keep := make([]awayNote, 0, len(batch))
	for i := len(batch) - 1; i >= 0; i-- {
		k := key{batch[i].community, batch[i].user}
		if seen[k] {
			continue
		}
		seen[k] = true
		keep = append(keep, batch[i])
	}
	for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
		keep[i], keep[j] = keep[j], keep[i]
	}
	return keep
}

// noteAFKAdmissionLocked records an admission of dev of user. The caller holds afkMu.
func (h *Calls) noteAFKAdmissionLocked(user, dev id.ID) {
	h.afkAdmitSeq++
	if h.afkAdmits == nil {
		h.afkAdmits = make(map[id.ID]map[id.ID]afkAdmit)
	}
	if h.afkAdmits[user] == nil {
		h.afkAdmits[user] = make(map[id.ID]afkAdmit)
	}
	h.afkAdmits[user][dev] = afkAdmit{seq: h.afkAdmitSeq, at: h.clk.Now()}
}

// latestAdmitSeqLocked is the number of the user's latest admission, 0 for none. The caller holds
// afkMu.
func (h *Calls) latestAdmitSeqLocked(user id.ID) uint64 {
	var seq uint64
	for _, a := range h.afkAdmits[user] {
		seq = max(seq, a.seq)
	}
	return seq
}

// pruneAFKAdmitsLocked forgets admissions older than afkAdmitMemory. The caller holds afkMu.
func (h *Calls) pruneAFKAdmitsLocked() {
	for u, devs := range h.afkAdmits {
		for d, a := range devs {
			if h.clk.Since(a.at) >= afkAdmitMemory {
				delete(devs, d)
			}
		}
		if len(devs) == 0 {
			delete(h.afkAdmits, u)
		}
	}
	for u, devs := range h.voiceFresh {
		for d, a := range devs {
			if h.clk.Since(a.at) >= afkAdmitMemory {
				delete(devs, d)
			}
		}
		if len(devs) == 0 {
			delete(h.voiceFresh, u)
		}
	}
}

// afkDecision is the admission number a mark is decided at: an admission after it wins.
func (h *Calls) afkDecision() uint64 {
	h.afkMu.Lock()
	defer h.afkMu.Unlock()
	return h.afkAdmitSeq
}

// clearAwayUser clears every away row of user and announces each. A user not in the index is a
// read-locked lookup.
func (h *Calls) clearAwayUser(ctx context.Context, user id.ID) {
	if !h.idxMayBeAway(user, nil) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	// Unseeded, every user may be away: a read-only lookup first, so only a user with a row takes
	// the lock and runs the DELETE (N-B). The lookup runs outside awayMu, so a mark in flight (noted,
	// its row not yet written) is seen through its index entry, read after the store: the clear then
	// waits for it on awayMu and runs after its write (A-N1). A mark noted after this read was decided
	// after the clear's own event, which its checks then see.
	if !h.awaySeededNow() {
		if has, err := h.repo.AFKAwayExistsUser(ctx, user); err == nil && !has && !h.idxHas(user, nil) {
			return
		}
	}
	h.awayMu.Lock()
	h.clearAwayUserLocked(ctx, user)
}

// clearAwayUserLocked deletes every away row of user and queues their op 55. The caller holds awayMu;
// it is released on return.
func (h *Calls) clearAwayUserLocked(ctx context.Context, user id.ID) {
	cids, err := h.repo.ClearAFKAwayUser(ctx, user)
	if err != nil {
		h.awayMu.Unlock()
		h.log.WarnContext(ctx, "clearing a user's away rows failed", "user", user, "err", err)
		return
	}
	h.idxDropUser(user)
	notes := make([]awayNote, 0, len(cids))
	for _, cid := range cids {
		notes = append(notes, awayNote{community: cid, user: user})
	}
	h.releaseAway(ctx, notes)
}

// clearAwayIn clears user's away row in community cid, announcing it when there was one.
func (h *Calls) clearAwayIn(ctx context.Context, cid, user id.ID) {
	if !h.idxMayBeAway(user, &cid) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if !h.awaySeededNow() {
		// As in clearAwayUser: an in-flight mark's index entry keeps the clear (A-N1).
		if has, err := h.repo.AFKAwayExists(ctx, cid, user); err == nil && !has && !h.idxHas(user, &cid) {
			return
		}
	}
	h.awayMu.Lock()
	cleared, err := h.repo.ClearAFKAway(ctx, cid, user)
	if err != nil {
		h.awayMu.Unlock()
		h.log.WarnContext(ctx, "clearing an away row failed", "community", cid, "user", user, "err", err)
		return
	}
	h.idxDrop(user, cid)
	var notes []awayNote
	if cleared {
		notes = []awayNote{{community: cid, user: user}}
	}
	h.releaseAway(ctx, notes)
}

// AFKActiveInChannel is activity of user in channel (a readable post, a typing op, Q1): it clears the
// user's away row in the channel's community. A user who is not away is a map lookup.
func (h *Calls) AFKActiveInChannel(ctx context.Context, user, channel id.ID) {
	if !h.mayBeAway(user) {
		return
	}
	ch, err := h.repo.GetChannel(ctx, channel)
	if err != nil || ch.CommunityID == nil {
		return
	}
	h.clearAwayIn(ctx, *ch.CommunityID, user)
}

// AFKActiveInGroup is an end-to-end encrypted upload by user into group (Q1): an upload into a
// community channel's text group clears the user's away row in that community; a DM's, a call
// group's or any other group's clears nothing. A user who is not away is a map lookup.
func (h *Calls) AFKActiveInGroup(ctx context.Context, user, group id.ID) {
	if !h.mayBeAway(user) {
		return
	}
	g, err := h.repo.GetGroup(ctx, group)
	if err != nil || g.Kind != GroupText || g.CommunityID == nil {
		return
	}
	h.clearAwayIn(ctx, *g.CommunityID, user)
}

// awayOffline is a user whose last gateway connection closed: their away rows are cleared at
// deadline unless a connection came back. AFKOffline moves the deadline and re-arms t; the one
// goroutine waiting on t never re-arms it.
type awayOffline struct {
	deadline time.Time
	t        clock.Timer
}

// AFKOffline is the gateway's hook for a user's last live connection closing (Q2, as revised): the
// user's away rows are cleared once the user has held no gateway connection for afkOfflineGrace
// continuous seconds. A reconnect within it keeps them (a reload, a short network blip); a later
// close starts the grace again. A user has one entry and one timer however often they reconnect, so
// a flapping connection leaks nothing; StopAFK ends the waits, and no clear runs while the gateway
// drains.
func (h *Calls) AFKOffline(_ context.Context, user id.ID) {
	h.awayOffMu.Lock()
	defer h.awayOffMu.Unlock()
	deadline := h.clk.Now().Add(afkOfflineGrace)
	if e := h.awayOff[user]; e != nil {
		e.deadline = deadline
		e.t.Reset(afkOfflineGrace)
		return
	}
	h.afkLifeMu.Lock()
	if h.afkStopped {
		h.afkLifeMu.Unlock()
		return
	}
	h.afkWG.Add(1)
	h.afkLifeMu.Unlock()
	if h.awayOff == nil {
		h.awayOff = make(map[id.ID]*awayOffline)
	}
	e := &awayOffline{deadline: deadline, t: h.clk.NewTimer(afkOfflineGrace)}
	h.awayOff[user] = e
	h.awayOffWaiters.Add(1)
	go h.awaitOffline(user, e)
}

// awaitOffline waits out e's grace for user, tracked by afkWG, and ends with StopAFK.
func (h *Calls) awaitOffline(user id.ID, e *awayOffline) {
	defer h.afkWG.Done()
	defer h.awayOffWaiters.Add(-1)
	ctx := h.afkCtx
	for {
		select {
		case <-ctx.Done():
			e.t.Stop()
			h.awayOffMu.Lock()
			if h.awayOff[user] == e {
				delete(h.awayOff, user)
			}
			h.awayOffMu.Unlock()
			return
		case <-e.t.C():
		}
		if h.offlineDue(ctx, user, e) {
			return
		}
	}
}

// offlineDue is the end of user's grace: it answers false when a later close moved the deadline (the
// timer is re-armed already), and otherwise drops the entry and clears every away row of the user,
// unless a connection came back, the instance stops or the gateway drains. The deadline, the
// connection and the rows are read under awayMu, where every mark checks the connection and writes:
// a mark of a user who reconnected cannot fall between this check and the DELETE.
func (h *Calls) offlineDue(ctx context.Context, user id.ID, e *awayOffline) bool {
	h.awayMu.Lock()
	h.awayOffMu.Lock()
	if h.awayOff[user] != e {
		h.awayOffMu.Unlock()
		h.awayMu.Unlock()
		return true
	}
	if h.clk.Now().Before(e.deadline) {
		h.awayOffMu.Unlock()
		h.awayMu.Unlock()
		return false
	}
	delete(h.awayOff, user)
	h.awayOffMu.Unlock()
	if ctx.Err() != nil || (h.awayDraining != nil && h.awayDraining()) || h.connectedLocked(user) ||
		!h.idxMayBeAway(user, nil) {
		h.awayMu.Unlock()
		return true
	}
	h.clearAwayUserLocked(context.WithoutCancel(ctx), user)
	return true
}

// afkAdmissionClears is the admission's clear (R-AW2, R-AW4): Admitted records the admission under
// afkMu first (noteAFKAdmissionLocked) and calls this after releasing it; every row of the user goes.
func (h *Calls) afkAdmissionClears(ctx context.Context, user id.ID) {
	h.clearAwayUser(ctx, user)
}

// AFKMemberRemoved runs after a leave, a kick or a ban (M6): the membership's cascade dropped the
// user's away row in cid, and the remaining audience is told with op 55 since 0. The membership is
// read under awayMu (R2), where every mark writes, so a rejoin and a mark cannot fall between the
// read and the index drop. A user who is still a member, whom the index does not hold, or whom an
// unseeded index never loaded (R5) sends nothing.
func (h *Calls) AFKMemberRemoved(ctx context.Context, cid, user id.ID) {
	if h == nil || !h.idxKnows(user, cid) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	h.awayMu.Lock()
	if _, err := h.repo.GetMember(ctx, cid, user); err == nil || !errors.Is(err, store.ErrNotFound) {
		h.awayMu.Unlock()
		return
	}
	if !h.idxKnows(user, cid) {
		h.awayMu.Unlock()
		return
	}
	h.idxDrop(user, cid)
	h.releaseAway(ctx, []awayNote{{community: cid, user: user}})
}

// otherDeviceInCall reports whether a device of user other than except is in a call: admitted
// within the join window (the SFU may not list it yet), or listed by the SFU in any room.
func (h *Calls) otherDeviceInCall(ctx context.Context, user, except id.ID) (bool, error) {
	h.afkMu.Lock()
	joining := false
	for dev, a := range h.afkAdmits[user] {
		if dev != except && h.clk.Since(a.at) < admissionJoinWindow {
			joining = true
			break
		}
	}
	h.afkMu.Unlock()
	if joining {
		return true, nil
	}
	if h.sfu == nil {
		return false, nil
	}
	devices, err := h.repo.ListDevicesByUser(ctx, user)
	if err != nil {
		return false, err
	}
	mine := make(map[string]bool, len(devices))
	for _, d := range devices {
		if d.ID != except {
			mine[d.ID.String()] = true
		}
	}
	if len(mine) == 0 {
		return false, nil
	}
	found := false
	err = h.eachUserParticipant(ctx, mine, func(string, string) bool {
		found = true
		return false
	})
	return found, err
}

// eachUserParticipant calls f(room, device) for every participant of every SFU room whose identity
// (before any "#" shadow suffix) is in devices, until f answers false.
func (h *Calls) eachUserParticipant(ctx context.Context, devices map[string]bool, f func(room, device string) bool) error {
	rooms, err := h.listRooms(ctx)
	if err != nil {
		return err
	}
	for _, room := range rooms {
		pctx, cancel := sfuCtx(ctx)
		parts, err := h.sfu.Participants(pctx, room)
		cancel()
		if err != nil {
			return err
		}
		for _, p := range parts {
			if p.GetState() == livekit.ParticipantInfo_DISCONNECTED {
				continue
			}
			base, _, _ := strings.Cut(p.GetIdentity(), "#")
			if devices[base] && !f(room, base) {
				return nil
			}
		}
	}
	return nil
}

// markAway marks user away in community cid since now, unless an admission of the user came after
// decided (R-AW7) or, with needConn, the user holds no gateway connection (Q2), and announces a row
// it wrote with the since it stored. It answers whether the user holds the row afterwards (written
// now, or kept from an earlier mark); false when the mark was skipped. ErrNotFound when the user is
// no member (a racing kick). Only the boot marks without needConn: nobody is connected yet, and
// the boot grace decides.
func (h *Calls) markAway(ctx context.Context, cid, user id.ID, decided uint64, needConn bool) (bool, error) {
	ctx = context.WithoutCancel(ctx)
	h.awayMu.Lock()
	// The index entry goes in BEFORE the checks: a clear whose lock-free lookup comes after this
	// point waits for the mark on awayMu and then clears; one whose lookup came before it saw its
	// own event (an admission's number, a closed last connection) land before the checks below,
	// which then skip the mark. A skipped mark takes back an entry it added.
	had := h.idxNote(user, cid)
	h.afkStage("mark-noted")
	undo := func() {
		if !had {
			h.idxDrop(user, cid)
		}
	}
	h.afkMu.Lock()
	newer := h.latestAdmitSeqLocked(user) > decided
	h.afkMu.Unlock()
	if newer || (needConn && !h.connectedLocked(user)) {
		undo()
		h.awayMu.Unlock()
		return false, nil
	}
	h.afkStage("mark-checked")
	now := h.clk.Now().Unix()
	set, err := h.repo.SetAFKAway(ctx, cid, user, now)
	if err != nil {
		undo()
		h.awayMu.Unlock()
		return false, err
	}
	var notes []awayNote
	if set {
		notes = []awayNote{{community: cid, user: user, since: uint64(now)}} //nolint:gosec // G115: the instance clock is after 1970
	}
	h.releaseAway(ctx, notes)
	// SetAFKAway answers false for a row that was already there, which keeps its earlier since: the
	// user is away either way.
	return true, nil
}

// sweepMarkAway is the sweep's mark after its cut of dev (R-AW3): only when no other device of the
// user is in a call. A failure leaves the user unmarked: the cut stands, and away never overlaps a
// call (R-AW2).
func (h *Calls) sweepMarkAway(ctx context.Context, tracked afkTracked, dev id.ID, decided uint64) {
	h.afkStage("cut")
	busy, err := h.otherDeviceInCall(ctx, tracked.user, dev)
	if err != nil {
		h.log.WarnContext(ctx, "checking a swept user's other devices failed; not marked away", "user", tracked.user, "err", err)
		return
	}
	if busy {
		return
	}
	if _, err := h.markAway(ctx, tracked.community, tracked.user, decided, true); err != nil && !errors.Is(err, store.ErrNotFound) {
		h.log.WarnContext(ctx, "marking a swept user away failed", "user", tracked.user, "err", err)
	}
}

// afkAway is PUT /v1/communities/{id}/afk/away `[]` (R-AW3, by choosing AFK): member-only. It marks
// the caller's user away in the community first, keeping an earlier since, and then cuts every
// device of the user from every SFU room (cutUserEverywhere). A cut that fails is logged and the
// request still answers 204 (R3-n2): the mark stands, every device carries its reason, and the next
// AFK maintenance passes retry the cut while the user is away (final review I3). A skipped mark cuts
// nothing (M1). There is no DELETE (Q3).
func (h *Calls) afkAway(w http.ResponseWriter, r *http.Request) {
	cid, user, dev, err := h.afkMember(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	var body []cbor.RawMessage
	if err := server.DecodeBody(w, r, maxCBORBody, &body); err != nil {
		server.WriteError(w, err)
		return
	}
	if len(body) != 0 {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "the body is []"))
		return
	}
	cfg, err := h.afkDestination(r.Context(), cid, user)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if cfg.ChannelID == nil {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "this community has no AFK channel"))
		return
	}
	decided := h.afkDecision()
	h.afkStage("away-decided")
	away, err := h.markAway(r.Context(), cid, user, decided, true)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	// Final review M1: a skipped mark (a newer admission, or no gateway connection) leaves the user
	// not away, so nothing is cut: a call admitted meanwhile wins (R-AW7).
	if away && !h.cutUserEverywhere(r.Context(), user, dev) {
		h.noteCutRetry(user)
	}
	w.WriteHeader(http.StatusNoContent)
}

// afkCutRetryMax bounds the maintenance passes that retry one user's unfinished cut (I3).
const afkCutRetryMax = 5

// noteCutRetry records that user's chosen-AFK cut did not finish; the next AFKMaintenance retries it.
// Every device it listed already carries its pending reason (setAFKPendingLocked).
func (h *Calls) noteCutRetry(user id.ID) {
	h.afkMu.Lock()
	defer h.afkMu.Unlock()
	if h.afkCutRetries == nil {
		h.afkCutRetries = make(map[id.ID]int)
	}
	if _, ok := h.afkCutRetries[user]; !ok {
		h.afkCutRetries[user] = 0
	}
}

// retryAFKCuts is AFKMaintenance's retry of the unfinished chosen-AFK cuts (final review I3): for
// each recorded user still away, it lists and cuts the user's devices again, and drops the record
// once a pass went through, once the user is no longer away, or after afkCutRetryMax failed passes.
func (h *Calls) retryAFKCuts(ctx context.Context) {
	h.afkMu.Lock()
	users := make(map[id.ID]int, len(h.afkCutRetries))
	for u, n := range h.afkCutRetries {
		users[u] = n
	}
	h.afkMu.Unlock()
	for user, n := range users {
		if ctx.Err() != nil || h.draining() {
			return
		}
		if !h.idxHas(user, nil) {
			h.dropCutRetry(user)
			continue
		}
		if h.cutUserEverywhere(ctx, user, id.ID{}) {
			h.dropCutRetry(user)
			h.log.InfoContext(ctx, "retried an unfinished AFK cut; it went through", "user", user, "attempt", n+1)
			continue
		}
		if n+1 >= afkCutRetryMax {
			h.dropCutRetry(user)
			h.log.ErrorContext(ctx, "an AFK cut failed on every retry; giving up, a device may stay in a call while its user is away",
				"user", user, "attempts", n+1)
			continue
		}
		h.afkMu.Lock()
		if _, ok := h.afkCutRetries[user]; ok {
			h.afkCutRetries[user] = n + 1
		}
		h.afkMu.Unlock()
		h.log.WarnContext(ctx, "retrying an unfinished AFK cut failed; the next maintenance pass tries again", "user", user, "attempt", n+1)
	}
}

func (h *Calls) dropCutRetry(user id.ID) {
	h.afkMu.Lock()
	delete(h.afkCutRetries, user)
	h.afkMu.Unlock()
}

// cutUserEverywhere removes every device of user, with its "#" shadows, from every SFU room it is
// in, revokes its relay and stops its AFK timer. Every device it cuts gets its pending reason 2
// (chosen AFK) BEFORE the removal (R3-n1), so the cut client's first read already says why; so does
// every device of the user other than caller that was admitted to a call within afkRecoverWindow
// and that no room lists now (R3-M1): a call the SFU dropped, whose recovery then reads 2 and does
// not rejoin. A failed room listing or removal is logged, and the device keeps its reason (R3-n2).
// It answers whether the cut finished: the devices and rooms were listed and every removal went
// through. A zero caller (a maintenance retry) leaves out no device.
func (h *Calls) cutUserEverywhere(ctx context.Context, user, caller id.ID) bool {
	complete := true
	type cut struct {
		room string
		dev  id.ID
	}
	var cuts []cut
	listed := map[id.ID]bool{}
	if h.sfu != nil {
		devices, err := h.repo.ListDevicesByUser(ctx, user)
		if err != nil {
			h.log.ErrorContext(ctx, "listing a user's devices for AFK failed; none is cut", "user", user, "err", err)
			complete = false
		}
		mine := make(map[string]bool, len(devices))
		for _, d := range devices {
			mine[d.ID.String()] = true
		}
		seen := map[cut]bool{}
		if len(mine) > 0 {
			if err := h.eachUserParticipant(ctx, mine, func(room, d string) bool {
				dev, err := id.Parse(d)
				if err != nil {
					return true
				}
				if c := (cut{room, dev}); !seen[c] {
					seen[c] = true
					cuts = append(cuts, c)
					listed[dev] = true
				}
				return true
			}); err != nil {
				h.log.ErrorContext(ctx, "listing the SFU rooms for AFK failed; the devices it listed are cut", "user", user, "err", err)
				complete = false
			}
		}
	}
	h.afkMu.Lock()
	// The cut device's client learns from any community's snapshot that it left because its user
	// chose AFK (pending 2), wherever it was: a community call or a DM call.
	for _, c := range cuts {
		h.setAFKPendingLocked(c.dev, afkPendingChose)
	}
	for dev, a := range h.afkAdmits[user] {
		if dev != caller && !listed[dev] && h.clk.Since(a.at) < afkRecoverWindow {
			h.setAFKPendingLocked(dev, afkPendingChose)
		}
	}
	h.afkMu.Unlock()
	h.afkStage("away-pending")
	for _, c := range cuts {
		rctx, cancel := sfuCtx(context.WithoutCancel(ctx))
		err := h.sfu.RemoveParticipants(rctx, c.room, c.dev)
		cancel()
		if err != nil {
			h.log.ErrorContext(ctx, "cutting a device for AFK failed; it keeps its pending reason, and maintenance retries the cut",
				"device", c.dev, "room", c.room, "err", err)
			complete = false
			continue
		}
		h.revokeRelay(c.dev)
		h.afkMu.Lock()
		delete(h.afkTracked, c.dev)
		h.afkMu.Unlock()
	}
	return complete
}

// eachCommunity calls f for every live community, a page at a time.
func (h *Calls) eachCommunity(ctx context.Context, f func(store.CommunityRow)) error {
	var after id.ID
	for {
		page, err := h.repo.ListCommunities(ctx, after, afkBackfillPage)
		if err != nil {
			return err
		}
		for _, com := range page {
			f(com)
		}
		if len(page) < afkBackfillPage {
			return nil
		}
		after = page[len(page)-1].ID
	}
}

// SeedAFKAway loads the in-memory away index from the stored roster (I1), one community at a time
// and without holding a lock across the walk. It merges into what marks wrote meanwhile; a clear
// that raced it can leave a stale entry, which is safe (the index is a superset). Until it succeeds
// every user may be away and the clears go to the store; AFKMaintenance retries it with backoff (R5).
func (h *Calls) SeedAFKAway(ctx context.Context) error {
	seen := make(map[id.ID]map[id.ID]struct{})
	var failed error
	if err := h.eachCommunity(ctx, func(com store.CommunityRow) {
		if failed != nil {
			return
		}
		rows, err := h.repo.ListAFKAway(ctx, com.ID)
		if err != nil {
			failed = err
			return
		}
		for _, r := range rows {
			if seen[r.UserID] == nil {
				seen[r.UserID] = make(map[id.ID]struct{})
			}
			seen[r.UserID][com.ID] = struct{}{}
		}
	}); err != nil {
		return err
	}
	if failed != nil {
		return failed
	}
	// The merge runs under awayMu (N-A): a mark notes its entry and takes it back on a skip under
	// awayMu, so a merge either lands before the mark (which then finds the entry and keeps it) or
	// after its undo (which then puts back the entry of the stored row).
	h.awayMu.Lock()
	defer h.awayMu.Unlock()
	h.awayIdxMu.Lock()
	defer h.awayIdxMu.Unlock()
	if h.awaySet == nil {
		h.awaySet = make(map[id.ID]map[id.ID]struct{})
	}
	for u, cs := range seen {
		if h.awaySet[u] == nil {
			h.awaySet[u] = make(map[id.ID]struct{})
		}
		for cid := range cs {
			h.awaySet[u][cid] = struct{}{}
		}
	}
	h.awaySeeded = true
	return nil
}

// Seed retry backoff (R5): 5 s, doubling, at most 5 min.
const (
	afkSeedBackoffMin = 5 * time.Second
	afkSeedBackoffMax = 5 * time.Minute
)

// seedFailedLocked schedules the next seed attempt. The caller holds awayMu.
func (h *Calls) seedFailedLocked() {
	if h.seedBackoff == 0 {
		h.seedBackoff = afkSeedBackoffMin
	} else {
		h.seedBackoff = min(2*h.seedBackoff, afkSeedBackoffMax)
	}
	h.seedNextAt = h.clk.Now().Add(h.seedBackoff)
}

// trySeed seeds the index when it is not seeded and its backoff has passed.
func (h *Calls) trySeed(ctx context.Context) {
	if h.awaySeededNow() {
		return
	}
	h.awayMu.Lock()
	due := !h.clk.Now().Before(h.seedNextAt)
	h.awayMu.Unlock()
	if !due {
		return
	}
	err := h.SeedAFKAway(ctx)
	h.awayMu.Lock()
	defer h.awayMu.Unlock()
	if err != nil {
		h.seedFailedLocked()
		if ctx.Err() != nil {
			h.log.DebugContext(ctx, "loading the away roster stopped with the instance", "err", err)
			return
		}
		h.log.ErrorContext(ctx, "loading the away roster failed; every clear reads the store until a retry lands",
			"err", err, "retry_in", h.seedBackoff.String())
		return
	}
	h.seedBackoff = 0
}

// EndAFKCalls is the boot step after EnsureAFKChannels (release note, R-AW1): it seeds the away
// index, ends every live call in a community's AFK channel, and marks every user with a device in its
// call group away there, since now. It also starts the boot grace (R-AW5) that AFKMaintenance waits
// out. It answers how many calls it ended. A call whose end fails is logged and left for
// AFKMaintenance to retry on the next tick (M4); the others go on.
func (h *Calls) EndAFKCalls(ctx context.Context) (int, error) {
	h.awayMu.Lock()
	h.awayGraceUntil, h.awayGraceDone = h.clk.Now().Add(afkBootGrace), false
	h.awayMu.Unlock()
	h.trySeed(ctx)
	n, failed, err := h.endAFKCallsOnce(ctx, true)
	h.awayEndPending.Store(failed > 0 || err != nil)
	if n > 0 {
		h.log.InfoContext(ctx, "ended the calls in AFK channels; their users are away", "calls", n)
	}
	return n, err
}

// endAFKCallsOnce is one walk of EndAFKCalls: the calls it ended and how many ends failed. At boot
// (boot) the users are marked with no connection check, as nobody is connected yet and the
// in-process SFU restarted; on a retry (R4) a user is marked only as a sweep would mark them: with a
// gateway connection and no device in another call.
func (h *Calls) endAFKCallsOnce(ctx context.Context, boot bool) (int, int, error) {
	n, failed := 0, 0
	err := h.eachCommunity(ctx, func(com store.CommunityRow) {
		if ctx.Err() != nil {
			return
		}
		cfg, err := h.repo.GetCommunityAFK(ctx, com.ID)
		if err != nil || cfg.ChannelID == nil {
			if err != nil {
				failed++
			}
			return
		}
		live, err := h.repo.ListLiveVoiceSessions(ctx, *cfg.ChannelID)
		if err != nil {
			failed++
			h.log.ErrorContext(ctx, "listing the AFK channel's calls failed", "community", com.ID, "err", err)
			return
		}
		for _, row := range live {
			users := h.callUsers(ctx, row)
			if err := h.endCall(context.WithoutCancel(ctx), row.CallID, row.LivekitRoom, "afk_channel"); err != nil {
				failed++
				h.log.ErrorContext(ctx, "ending a call in the AFK channel failed; retried on the next tick", "call_id", row.CallID, "err", err)
				continue
			}
			n++
			for _, u := range users {
				decided := h.afkDecision()
				if !boot {
					busy, err := h.otherDeviceInCall(ctx, u, id.ID{})
					if err != nil || busy {
						continue
					}
				}
				if _, err := h.markAway(ctx, com.ID, u, decided, !boot); err != nil && !errors.Is(err, store.ErrNotFound) {
					h.log.WarnContext(ctx, "marking an AFK call's user away failed", "user", u, "err", err)
				}
			}
		}
	})
	return n, failed, err
}

// callUsers is the users with a current leaf in row's call group, each once.
func (h *Calls) callUsers(ctx context.Context, row store.VoiceSessionRow) []id.ID {
	if row.GroupID == nil {
		return nil
	}
	members, err := h.repo.ListMembers(ctx, *row.GroupID)
	if err != nil {
		h.log.WarnContext(ctx, "reading an AFK call's members failed", "call_id", row.CallID, "err", err)
		return nil
	}
	var out []id.ID
	seen := map[id.ID]bool{}
	for _, m := range members {
		if m.RemovedEpoch == nil && !seen[m.UserID] {
			seen[m.UserID] = true
			out = append(out, m.UserID)
		}
	}
	return out
}

// AFKMaintenance is one maintenance pass (dillad runs it through GoAFKMaintenance, one pass at a
// time): it retries a failed seed with backoff (R5) and the AFK-call ends the boot could not finish
// (M4, marking as a sweep would, R4), and once the boot grace has passed (R-AW5) it clears, once, the
// away rows of every user who holds no gateway connection, announcing each. The walk holds awayMu
// for one row at a time, never across it (M5); it stops when ctx ends, and it does not run while the
// gateway drains, when every user looks disconnected (R3).
func (h *Calls) AFKMaintenance(ctx context.Context) {
	if !h.awayMaintaining.CompareAndSwap(false, true) {
		return
	}
	defer h.awayMaintaining.Store(false)
	h.trySeed(ctx)
	if h.awayEndPending.Load() && ctx.Err() == nil {
		n, failed, err := h.endAFKCallsOnce(ctx, false)
		h.awayEndPending.Store(failed > 0 || err != nil)
		if n > 0 {
			h.log.InfoContext(ctx, "ended the calls in AFK channels on a retry", "calls", n)
		}
	}
	if ctx.Err() != nil || h.draining() {
		return
	}
	h.retryAFKCuts(ctx)
	h.retryTakeovers(ctx)
	if ctx.Err() != nil || h.draining() {
		return
	}
	h.awayMu.Lock()
	due := !h.awayGraceUntil.IsZero() && !h.awayGraceDone && !h.clk.Now().Before(h.awayGraceUntil)
	if due {
		h.awayGraceDone = true
	}
	connected := h.awayConnected
	h.awayMu.Unlock()
	if !due || connected == nil {
		return
	}
	if err := h.eachCommunity(ctx, func(com store.CommunityRow) {
		if ctx.Err() != nil || h.draining() {
			return
		}
		rows, err := h.repo.ListAFKAway(ctx, com.ID)
		if err != nil {
			h.log.WarnContext(ctx, "reading an away roster after the boot grace failed", "community", com.ID, "err", err)
			return
		}
		for _, r := range rows {
			if ctx.Err() != nil || h.draining() {
				return
			}
			if !connected(r.UserID) {
				h.clearAwayIn(ctx, com.ID, r.UserID)
			}
		}
	}); err != nil {
		h.log.WarnContext(ctx, "listing the communities after the boot grace failed", "err", err)
	}
}

// awaySnapshot is the snapshot's away list of community cid, at most afkAwaySnapshotMax entries,
// bounded in the query (M3).
func (h *Calls) awaySnapshot(ctx context.Context, cid id.ID) ([]afkAwayEntry, error) {
	rows, err := h.repo.ListAFKAwayFirst(ctx, cid, afkAwaySnapshotMax)
	if err != nil {
		return nil, err
	}
	out := make([]afkAwayEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, afkAwayEntry{UserID: r.UserID, SinceTS: uint64(r.Since)}) //nolint:gosec // G115: a unix second this instance wrote
	}
	return out, nil
}

// isAFKChannel reports whether ch is its community's AFK channel (R-AW1). A pointer that cannot be
// read answers false: the listen-only grant (inAFK) stays the backstop.
func (h *Calls) isAFKChannel(ctx context.Context, ch store.ChannelRow) bool {
	if ch.CommunityID == nil {
		return false
	}
	cfg, err := h.repo.GetCommunityAFK(ctx, *ch.CommunityID)
	return err == nil && cfg.ChannelID != nil && *cfg.ChannelID == ch.ID
}

// errAFKCall is R-AW1's refusal.
func errAFKCall() error {
	return server.Errorf(server.CodeForbidden, "nobody joins a call in the AFK channel")
}

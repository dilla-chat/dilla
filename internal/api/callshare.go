package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/auth"
	botaccess "github.com/jonasthim/dilla/internal/bot/access"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/sfu"
	"github.com/jonasthim/dilla/internal/store"
)

// shareLeases is the publisher lease (DEV-02, ruling F1, MD-3) and the per-call state that goes with
// it: per call, the devices holding a sharing slot and when they took it, the participants whose
// required cut or demotion has not landed in the SFU yet (pending repairs), and the per-call lock.
// A slot covers camera, screen and screen audio together; screen audio never counts alone and the
// microphone never counts. It lives in memory because the SFU is in-process: a restart drops every
// call, and every lease and repair with it.
//
// The per-call lock (lockCall) serialises every lease transition of a call together with the SFU
// push that goes with it: no path pushes a permission for a call, takes or frees a slot, or records
// or clears a repair without holding it. So at every instant the devices whose pushed permission
// allows a camera or screen source are a subset of the slot holders, and never more than
// livekit.max_publishers. mu only guards the maps themselves.
type shareLeases struct {
	mu      sync.Mutex
	byCall  map[id.ID]map[id.ID]slot
	pending map[id.ID]map[string]pendingRepair
	locks   map[id.ID]*callLock
	// penalties is ruling F11's per-call state (dilla-media task 12): the devices of a call's room
	// that published media that is not dilla-sframe/1, with their strike count. A penalised device's
	// permission is listen-only wherever one is computed — reconcile, grantFor, the token mint and
	// the /rtc gate — so neither the room sweep nor a grant sync re-grants it, and it lasts as long
	// as the room.
	penalties map[id.ID]map[id.ID]penalty
}

// penalty is one device's F11 strikes in the room of a call they were counted in, and the track sids
// already counted (ruling (b): one publication is one strike, whether the track_published webhook or
// the room sweep saw it first).
type penalty struct {
	room    string
	strikes int
	tracks  map[string]bool
}

// callLock is one call's lock: a one-slot channel, so taking it can give up when the caller's
// context ends or the wait bound passes. It is reference-counted so an idle call holds none.
type callLock struct {
	ch   chan struct{}
	refs int
}

// slot is one device's sharing slot: the room of the call it was taken in, and when. A slot belongs
// to that room only: the next call of the same call id opens a fresh room, and a slot whose room is
// not the live call's is dropped wherever it is met (take, held, the retry loop), so it can never
// carry a reduced cap or a camera grant into a later call.
type slot struct {
	room  string
	since int64
}

// pendingRepair is a cut or a demotion of one participant of a call's room that did not land: the
// SFU refused or failed the removal or the push. Until it lands the participant's slot stays held,
// and the token mint and the /rtc gate refuse the device for that call.
type pendingRepair struct {
	Room string
	// DropSlot is set when the repair must also free the device's sharing slot (an unshare or a
	// cut), so a retry never pushes the video sources back.
	DropSlot bool
	// Cut is set when the repair is a removal from the room whatever the device's entitlement: the
	// delivery service's eviction of a removed device and F11's removal of a repeat offender
	// (dilla-media task 12), which a retry must not turn into a permission push.
	Cut bool
}

func newShareLeases() *shareLeases {
	return &shareLeases{
		byCall:    map[id.ID]map[id.ID]slot{},
		pending:   map[id.ID]map[string]pendingRepair{},
		locks:     map[id.ID]*callLock{},
		penalties: map[id.ID]map[id.ID]penalty{},
	}
}

// strikeTrack counts one F11 violation of dev in call's room for the publication track (its track
// sid) and answers its strikes there, and whether this call counted it: a sid already counted adds
// nothing, and an empty sid is always counted. Strikes of another room of the call are forgotten
// first. The caller holds call's lock.
func (l *shareLeases) strikeTrack(call id.ID, room string, dev id.ID, track string) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.penalties[call]
	if m == nil {
		m = map[id.ID]penalty{}
		l.penalties[call] = m
	}
	p := m[dev]
	if p.room != room {
		p = penalty{room: room}
	}
	if track != "" && p.tracks[track] {
		return p.strikes, false
	}
	if track != "" {
		if p.tracks == nil {
			p.tracks = map[string]bool{}
		}
		p.tracks[track] = true
	}
	p.strikes++
	m[dev] = p
	return p.strikes, true
}

// penalised reports whether dev was struck in call's room (F11): its permission there is
// listen-only.
func (l *shareLeases) penalised(call id.ID, room string, dev id.ID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.penalties[call][dev]
	return ok && p.room == room && p.strikes > 0
}

// errCallBusy is a call lock that could not be taken within its bound or before the caller's
// context ended.
var errCallBusy = errors.New("api: the call is busy")

// lockCall takes call's lock and returns its release; it gives up with errCallBusy once wait has
// passed or ctx has ended, so no caller queues behind a stuck holder for longer than wait.
func (l *shareLeases) lockCall(ctx context.Context, call id.ID, wait time.Duration) (func(), error) {
	l.mu.Lock()
	cl := l.locks[call]
	if cl == nil {
		cl = &callLock{ch: make(chan struct{}, 1)}
		l.locks[call] = cl
	}
	cl.refs++
	l.mu.Unlock()
	deref := func() {
		l.mu.Lock()
		cl.refs--
		if cl.refs == 0 {
			delete(l.locks, call)
		}
		l.mu.Unlock()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case cl.ch <- struct{}{}:
		return func() { <-cl.ch; deref() }, nil
	case <-ctx.Done():
	case <-timer.C:
	}
	deref()
	return nil, errCallBusy
}

// pendingCount is how many repairs are outstanding across every call.
func (l *shareLeases) pendingCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.pending {
		n += len(m)
	}
	return n
}

// take gives dev a slot of call's room unless maxSlots are held there. A device that already holds
// one keeps it (already true): a repeated request is idempotent. Slots taken in another room of the
// call are dropped first. The caller holds call's lock.
func (l *shareLeases) take(call id.ID, room string, dev id.ID, maxSlots int, now int64) (taken, already bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropStaleLocked(call, room)
	held := l.byCall[call]
	if _, ok := held[dev]; ok {
		return true, true
	}
	if len(held) >= maxSlots {
		return false, false
	}
	if held == nil {
		held = map[id.ID]slot{}
		l.byCall[call] = held
	}
	held[dev] = slot{room: room, since: now}
	return true, false
}

// held reports whether dev holds a slot of call taken in room.
func (l *shareLeases) held(call id.ID, room string, dev id.ID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.byCall[call][dev]
	return ok && s.room == room
}

// dropStale forgets every slot of call taken in a room other than room, the live call's. The caller
// holds call's lock.
func (l *shareLeases) dropStale(call id.ID, room string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropStaleLocked(call, room)
}

func (l *shareLeases) dropStaleLocked(call id.ID, room string) {
	for dev, s := range l.byCall[call] {
		if s.room != room {
			delete(l.byCall[call], dev)
		}
	}
	if len(l.byCall[call]) == 0 {
		delete(l.byCall, call)
	}
	for dev, p := range l.penalties[call] {
		if p.room != room {
			delete(l.penalties[call], dev)
		}
	}
	if len(l.penalties[call]) == 0 {
		delete(l.penalties, call)
	}
}

// release frees dev's slot of call. The caller holds call's lock.
func (l *shareLeases) release(call, dev id.ID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byCall[call], dev)
	if len(l.byCall[call]) == 0 {
		delete(l.byCall, call)
	}
}

// markPending records that identity's repair in call did not land; a DropSlot or Cut already
// recorded for the same room stays. The caller holds call's lock.
func (l *shareLeases) markPending(call id.ID, identity string, r pendingRepair) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.pending[call]
	if m == nil {
		m = map[string]pendingRepair{}
		l.pending[call] = m
	}
	if prev, ok := m[identity]; ok && prev.Room == r.Room {
		r.DropSlot = r.DropSlot || prev.DropSlot
		r.Cut = r.Cut || prev.Cut
	}
	m[identity] = r
}

// clearPending forgets identity's repair in call. The caller holds call's lock.
func (l *shareLeases) clearPending(call id.ID, identity string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pending[call], identity)
	if len(l.pending[call]) == 0 {
		delete(l.pending, call)
	}
}

// pendingOf is a copy of call's repairs.
func (l *shareLeases) pendingOf(call id.ID) map[string]pendingRepair {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]pendingRepair, len(l.pending[call]))
	for k, v := range l.pending[call] {
		out[k] = v
	}
	return out
}

// isPending reports whether dev has a repair outstanding in call.
func (l *shareLeases) isPending(call, dev id.ID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.pending[call][dev.String()]
	return ok
}

// trackedCalls is every call with a repair outstanding, a slot held or an F11 penalty recorded: the
// retry loop drives the first and drops all three for a call that has ended.
func (l *shareLeases) trackedCalls() []id.ID {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]id.ID, 0, len(l.pending)+len(l.byCall)+len(l.penalties))
	seen := make(map[id.ID]bool, cap(out))
	for _, m := range []map[id.ID]bool{keysOf(l.pending), keysOf(l.byCall), keysOf(l.penalties)} {
		for call := range m {
			if !seen[call] {
				seen[call] = true
				out = append(out, call)
			}
		}
	}
	return out
}

func keysOf[V any](m map[id.ID]V) map[id.ID]bool {
	out := make(map[id.ID]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// dropCall forgets every slot, repair and F11 penalty of call: the call is over and its room
// closed. The caller holds call's lock.
func (l *shareLeases) dropCall(call id.ID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byCall, call)
	delete(l.pending, call)
	delete(l.penalties, call)
}

// dropRoom forgets every slot, repair and F11 penalty of call recorded for room: that room's call
// has ended. A later call of the same call id, in another room, keeps its own. The caller holds
// call's lock.
func (l *shareLeases) dropRoom(call id.ID, room string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for dev, s := range l.byCall[call] {
		if s.room == room {
			delete(l.byCall[call], dev)
		}
	}
	if len(l.byCall[call]) == 0 {
		delete(l.byCall, call)
	}
	for identity, r := range l.pending[call] {
		if r.Room == room {
			delete(l.pending[call], identity)
		}
	}
	if len(l.pending[call]) == 0 {
		delete(l.pending, call)
	}
	for dev, p := range l.penalties[call] {
		if p.room == room {
			delete(l.penalties[call], dev)
		}
	}
	if len(l.penalties[call]) == 0 {
		delete(l.penalties, call)
	}
}

// holders is the devices holding a slot of call taken in room.
func (l *shareLeases) holders(call id.ID, room string) []id.ID {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []id.ID
	for dev, s := range l.byCall[call] {
		if s.room == room {
			out = append(out, dev)
		}
	}
	return out
}

func (h *Calls) maxPublishers() int {
	if h.cfg.MaxPublishers > 0 {
		return h.cfg.MaxPublishers
	}
	return defaultMaxSharers
}

// callOf is the {call_id} of a share, unshare or stats request: a call in a channel the caller can
// see (404 otherwise, as for an unknown one), with the caller's bits there. The call may have ended;
// each route decides what that means. The bits are a first look only: a route re-reads them under
// the call's lock before it pushes anything.
func (h *Calls) callOf(r *http.Request) (auth.Session, store.VoiceSessionRow, store.ChannelRow, Bits, error) {
	s, err := enrolledSession(r)
	if err != nil {
		return s, store.VoiceSessionRow{}, store.ChannelRow{}, 0, err
	}
	callID, err := server.PathID(r, "call_id")
	if err != nil {
		return s, store.VoiceSessionRow{}, store.ChannelRow{}, 0, err
	}
	return h.callForSession(r.Context(), s, callID)
}

// callForSession is the common authority path for HTTP and connection-derived telemetry.
func (h *Calls) callForSession(ctx context.Context, s auth.Session, callID id.ID) (auth.Session, store.VoiceSessionRow, store.ChannelRow, Bits, error) {
	row, err := h.repo.GetVoiceSession(ctx, callID)
	if err != nil {
		return s, store.VoiceSessionRow{}, store.ChannelRow{}, 0, notFound(err)
	}
	ch, err := h.repo.GetChannel(ctx, row.ChannelID)
	if err != nil {
		return s, store.VoiceSessionRow{}, store.ChannelRow{}, 0, notFound(err)
	}
	bits, err := h.res.Resolve(ctx, s.UserID, ch)
	if err != nil {
		return s, store.VoiceSessionRow{}, store.ChannelRow{}, 0, err
	}
	if err := h.botCanUseSession(ctx, s, ch); err != nil {
		return s, store.VoiceSessionRow{}, store.ChannelRow{}, 0, err
	}
	if !bits.Has(PermViewChannel) {
		return s, store.VoiceSessionRow{}, store.ChannelRow{}, 0, server.Errorf(server.CodeNotFound, "no such object")
	}
	return s, row, ch, bits, nil
}

// requireLeafOfCall is the leaf gate on the group row's call was opened on.
func (h *Calls) requireLeafOfCall(ctx context.Context, row store.VoiceSessionRow, dev id.ID) error {
	if row.GroupID == nil {
		return server.Errorf(server.CodeLeafNotCurrent, "the call has no group to be a leaf of")
	}
	group, err := h.repo.GetGroup(ctx, *row.GroupID)
	if err != nil {
		return notFound(err)
	}
	return h.requireCurrentLeaf(ctx, group, dev)
}

// liveRow re-reads row's call under its lock: the same call, still live, in the same room. false
// when it ended or a later call of the same call id replaced it.
func (h *Calls) liveRow(ctx context.Context, repo store.Repository, row store.VoiceSessionRow) (store.VoiceSessionRow, bool, error) {
	cur, err := repo.GetVoiceSession(ctx, row.CallID)
	if errors.Is(err, store.ErrNotFound) {
		return cur, false, nil
	}
	if err != nil {
		return cur, false, err
	}
	return cur, cur.Ended == nil && cur.LivekitRoom == row.LivekitRoom, nil
}

// share is POST /v1/calls/{call_id}/share `[]`: 204 once the device holds a sharing slot and the SFU
// holds its complete promoted permission. Everything that decides the push — the call still live,
// the caller's bits, the leaf, the slot — is read again under the call's lock, and the push happens
// under it too, so of two devices racing for the last slot exactly one wins and no unshare or grant
// sync can interleave with the promotion.
func (h *Calls) share(w http.ResponseWriter, r *http.Request) {
	s, row, ch, bits, err := h.callOf(r)
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
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "the body is the empty array"))
		return
	}
	if row.Ended != nil {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "the call has ended"))
		return
	}
	// R3: the AFK channel is listen-only, so it holds no sharing slot; refused before any lease.
	if h.inAFK(r.Context(), row) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "nobody shares in the AFK channel"))
		return
	}
	if !bits.Has(PermVideo) && !bits.Has(PermScreenShare) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "missing permission"))
		return
	}
	if err := h.requireLeafOfCall(r.Context(), row, s.DeviceID); err != nil {
		server.WriteError(w, err)
		return
	}
	if err := h.refuseBarred(r.Context(), h.repo, s.DeviceID); err != nil {
		server.WriteError(w, err)
		return
	}
	if h.leases.penalised(row.CallID, row.LivekitRoom, s.DeviceID) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "your device published media that is not dilla-sframe/1 in this call"))
		return
	}
	if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, ch.CommunityID); err != nil {
		server.WriteError(w, err)
		return
	}
	if h.sfu == nil {
		server.WriteError(w, notImplemented("this instance runs no SFU (livekit.enabled is false)"))
		return
	}
	ctx, unlock, err := h.lock(r.Context(), row.CallID, callLockWait, callHoldRequest)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	defer unlock()
	cur, live, err := h.liveRow(ctx, h.repo, row)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if !live {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "the call has ended"))
		return
	}
	// A request drives only its own device's repair; every other one is the retry loop's.
	h.drainPending(ctx, h.deps(), ch, cur, 1, s.DeviceID.String())
	if h.leases.isPending(cur.CallID, s.DeviceID) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "your device's access to this call is being revoked"))
		return
	}
	if err := h.refuseBarred(ctx, h.repo, s.DeviceID); err != nil {
		server.WriteError(w, err)
		return
	}
	bits, err = h.res.Resolve(ctx, s.UserID, ch)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	switch {
	case !bits.Has(PermViewChannel):
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such object"))
		return
	case !bits.Has(PermConnect) || (!bits.Has(PermVideo) && !bits.Has(PermScreenShare)):
		server.WriteError(w, server.Errorf(server.CodeForbidden, "missing permission"))
		return
	}
	if err := h.requireLeafOfCall(ctx, cur, s.DeviceID); err != nil {
		server.WriteError(w, err)
		return
	}
	// F11: a device that published media that is not dilla-sframe/1 in this room is listen-only
	// for as long as the room lasts.
	if h.leases.penalised(cur.CallID, cur.LivekitRoom, s.DeviceID) {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "your device published media that is not dilla-sframe/1 in this call"))
		return
	}
	bot, err := botaccess.IsBotUser(ctx, h.repo, s.UserID)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if bot {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "managed bots cannot share camera or screen"))
		return
	}
	limit := h.maxPublishers()
	taken, already := h.leases.take(cur.CallID, cur.LivekitRoom, s.DeviceID, limit, h.clk.Now().Unix())
	if !taken {
		if h.counters != nil {
			h.counters.ShareRefused()
		}
		server.WriteError(w, server.Errorf(server.CodeCallSharersFull, "%d devices of this call are already sharing", limit))
		return
	}
	perm, err := h.grantFor(ctx, bits, cur, s.DeviceID)
	if err != nil {
		if !already {
			h.leases.release(cur.CallID, s.DeviceID)
		}
		server.WriteError(w, err)
		return
	}
	pctx, cancel := sfuCtx(ctx)
	err = h.sfu.UpdatePermission(pctx, cur.LivekitRoom, s.DeviceID.String(), perm)
	cancel()
	if err != nil {
		if errors.Is(err, sfu.ErrNoParticipant) {
			// A device the room does not hold publishes nothing, so its slot (new or old) goes.
			h.leases.release(cur.CallID, s.DeviceID)
			server.WriteError(w, server.Errorf(server.CodeNotFound, "your device is not in the call's room"))
			return
		}
		// Whether the SFU applied the promotion is unknown, so the slot stays held: a device that may
		// publish video keeps counting against the cap. The device frees it with DELETE …/share.
		h.log.ErrorContext(ctx, "promoting a sharer failed; the slot stays held", "room", cur.LivekitRoom,
			"device", s.DeviceID, "already", already, "err", err)
		server.WriteError(w, server.Errorf(server.CodeInternal, "the SFU could not update the permission"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// unshare is DELETE /v1/calls/{call_id}/share: 204, also when the device held no slot or the call
// has ended.
func (h *Calls) unshare(w http.ResponseWriter, r *http.Request) {
	s, row, ch, _, err := h.callOf(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if row.Ended == nil {
		if err := requireCommunityWire(r.Context(), h.repo, s.WireVersion, ch.CommunityID); err != nil {
			server.WriteError(w, err)
			return
		}
		if err := h.releaseShare(r.Context(), ch, row, s.DeviceID); err != nil {
			server.WriteError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// releaseShare frees dev's slot of row's call under the call's lock: the demotion — computed from
// the bits read now, under the lock — is pushed FIRST, LiveKit unpublishes the video tracks, and the
// slot is freed after. A device the room no longer holds needs no demotion. A demotion that fails
// keeps the slot and is recorded as a pending repair, which the retry loop and the device's own next
// request drive until it lands.
func (h *Calls) releaseShare(ctx context.Context, ch store.ChannelRow, row store.VoiceSessionRow, dev id.ID) error {
	ctx, unlock, err := h.lock(ctx, row.CallID, callLockWait, callHoldRequest)
	if err != nil {
		return err
	}
	defer unlock()
	cur, live, err := h.liveRow(ctx, h.repo, row)
	if err != nil {
		return err
	}
	if !live {
		return nil
	}
	d := h.deps()
	h.drainPending(ctx, d, ch, cur, 1, dev.String())
	if !h.leases.held(cur.CallID, cur.LivekitRoom, dev) {
		return nil
	}
	if h.sfu == nil {
		h.leases.release(cur.CallID, dev)
		return nil
	}
	if err := h.reconcile(ctx, d, ch, cur, dev.String(), nil, true); err != nil {
		h.log.ErrorContext(ctx, "demoting a sharer failed; the slot stays held until the repair lands",
			"room", cur.LivekitRoom, "device", dev, "err", err)
		return server.Errorf(server.CodeInternal, "the SFU could not update the permission")
	}
	return nil
}

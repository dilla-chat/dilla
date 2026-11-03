package api

import (
	"container/list"
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// The DM call ring (web-3, L-GW-33): a start that opens a call in a DM or group DM rings every other
// participant with op 53 call_ring until one of that user's devices joins, the user declines, the
// call ends, or RingSeconds pass on the instance clock. Rings live in memory only: they are never
// persisted or replayed, and reach only the devices with an open gateway session.

const (
	// RingSeconds is how long a ring lasts (founder decision R5): expires_ts is the ring's start
	// plus this many seconds.
	RingSeconds = 30
	// ringPairGap and ringPairPerHour are the pair rate rule (RV-4): a (caller, recipient) pair is
	// rung at most once per ringPairGap and at most ringPairPerHour times per rolling ringPairWindow.
	ringPairGap     = 15 * time.Second
	ringPairPerHour = 20
	ringPairWindow  = time.Hour
	// maxRingPairs bounds the pair state. A pair whose window has passed is dropped first; only a
	// table full of live pairs evicts the oldest live one (the residual: filling it takes at least
	// maxRingPairs / maxLivePairsPerCaller = 64 callers at their full share).
	maxRingPairs = 65536
	// maxLivePairsPerCaller is one caller's share of the pair table (task-3 review M1): a caller
	// holding this many live pairs rings no new recipient until one of its pairs' windows passes,
	// so no single caller can push a still-limited pair out of the table.
	maxLivePairsPerCaller = 1024
	// maxRingingCallsPerCaller is how many calls one caller may have ringing at once (RV-4's "8
	// concurrent rings per client", task-3 review I2): a start beyond it rings nobody, for that
	// caller only.
	maxRingingCallsPerCaller = 8
	// maxRingingCalls is the memory bound on calls with a ring outstanding; with the per-caller cap,
	// reaching it takes at least 512 callers.
	maxRingingCalls = 4096
)

// Ringer is the in-memory ring state: per call, the users being rung, each with its own timer, and
// the pair rate rule's history. One mutex guards both; every frame is delivered outside it.
type Ringer struct {
	gw  VoiceGateway
	clk clock.Clock
	log *slog.Logger
	// maxUsers is livekit.max_voice_participants: a start with more recipients rings nobody. 0 sets
	// no bound.
	maxUsers int
	// base is the clock reading the pair rule measures from: offsets from it use the monotonic
	// reading when the clock has one (review M5), so a wall-clock step neither frees nor holds a pair.
	base time.Time

	mu       sync.Mutex
	calls    map[id.ID]*ringCall
	byCaller map[id.ID]map[id.ID]int // caller → its ringing calls → how many of their rings are its
	pairs    map[ringPair]*pairState
	perPair  map[id.ID]int // caller → how many pairs it holds in the table
	order    *list.List    // of *pairState, the pair rung least recently first
}

// ringCall is one call's outstanding rings.
type ringCall struct {
	channel id.ID
	users   map[id.ID]*ring
}

// ring is one user's ring: who rang, when it ends, and its timer.
type ring struct {
	caller  id.ID
	expires uint64
	timer   clock.Timer
	stop    chan struct{}
}

// ringPair is the rate rule's key: the caller user and the recipient user, never the recipient
// alone, so no third party can spend a recipient's budget and silence honest callers.
type ringPair struct{ caller, recipient id.ID }

// pairState is one pair's rings within the rolling window (offsets from Ringer.base, oldest first)
// and when a suppressed ring of the pair was last logged.
type pairState struct {
	key    ringPair
	rings  []time.Duration
	logged time.Duration
	seen   bool // logged holds a value
	elem   *list.Element
}

// NewRinger builds the ring over the gateway the frames go through and the instance clock.
func NewRinger(gw VoiceGateway, clk clock.Clock, log *slog.Logger) *Ringer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Ringer{gw: gw, clk: clk, log: log, base: clk.Now(), calls: map[id.ID]*ringCall{},
		byCaller: map[id.ID]map[id.ID]int{}, pairs: map[ringPair]*pairState{}, perPair: map[id.ID]int{},
		order: list.New()}
}

// WithMaxUsers sets the per-call recipient bound (livekit.max_voice_participants) and returns r.
func (r *Ringer) WithMaxUsers(n int) *Ringer {
	r.maxUsers = n
	return r
}

// Start rings recipients for call, which caller has just opened (a 201) in ch, a DM or group DM:
// each of them gets state 1 on every online device, unless that user is already being rung for the
// call (at most one ring per (call, user)) or the pair rate rule holds the ring back. Any other kind
// of channel rings nobody. It answers the users it rang (WB product walk I6: the caller is told, so a
// ring the pair rule or a cap held back is never shown as ringing).
//
// Attacker (review I2): one caller starting call after call to fill the ringing-call table and
// silence everyone else's rings. A caller holds at most maxRingingCallsPerCaller ringing calls; its
// next start rings nobody while every other caller still rings.
func (r *Ringer) Start(ch store.ChannelRow, call, caller id.ID, recipients []id.ID) []id.ID {
	if r == nil || (ch.Kind != ChannelDM && ch.Kind != ChannelGroupDM) {
		return nil
	}
	now := r.clk.Now()
	expires := uint64(now.Unix()) + RingSeconds //nolint:gosec // G115: the instance clock is after 1970
	users := make([]id.ID, 0, len(recipients))
	seen := map[id.ID]bool{caller: true}
	for _, u := range recipients {
		if !seen[u] {
			seen[u] = true
			users = append(users, u)
		}
	}
	if r.maxUsers > 0 && len(users) > r.maxUsers {
		r.log.Warn("a call has more recipients than a call holds; it rings nobody", "call_id", call.String(), "recipients", len(users))
		return nil
	}
	var started, suppressed []id.ID
	capped := false
	r.mu.Lock()
	mine := r.byCaller[caller]
	if mine[call] == 0 && len(mine) >= maxRingingCallsPerCaller {
		r.mu.Unlock()
		r.log.Warn("this caller has too many calls ringing; this call rings nobody", "caller", caller.String(), "call_id", call.String())
		return nil
	}
	rc := r.calls[call]
	if rc == nil {
		if len(r.calls) >= maxRingingCalls {
			r.mu.Unlock()
			r.log.Warn("too many calls are ringing; this call rings nobody", "call_id", call.String())
			return nil
		}
		rc = &ringCall{channel: ch.ID, users: map[id.ID]*ring{}}
	}
	at := now.Sub(r.base)
	for _, u := range users {
		if rc.users[u] != nil {
			continue
		}
		allowed, logIt, full := r.allowLocked(caller, u, at)
		capped = capped || full
		if !allowed {
			if logIt {
				suppressed = append(suppressed, u)
			}
			continue
		}
		rg := &ring{caller: caller, expires: expires, timer: r.clk.NewTimer(RingSeconds * time.Second), stop: make(chan struct{})}
		rc.users[u] = rg
		if r.byCaller[caller] == nil {
			r.byCaller[caller] = map[id.ID]int{}
		}
		r.byCaller[caller][call]++
		started = append(started, u)
		go r.expire(call, u, rg)
	}
	if len(rc.users) > 0 {
		r.calls[call] = rc
	}
	r.mu.Unlock()
	for _, u := range suppressed {
		r.log.Info("a ring was held back by the pair rate rule", "caller", caller.String(), "recipient", u.String())
	}
	if capped {
		r.log.Warn("this caller holds its full share of ring pairs; new recipients are not rung", "caller", caller.String())
	}
	for _, u := range started {
		r.deliver(u, call, ch.ID, caller, 1, expires)
	}
	return started
}

// allowLocked applies the pair rate rule to one ring of recipient by caller at offset at and, when
// it is allowed, records it. logIt is true for a held-back ring whose pair was not logged within
// the window (one INFO per suppressed pair per hour); full reports a new pair refused because the
// caller holds its whole share of the table. r.mu is held.
//
// Attacker (review M1): ringing fresh users until the table evicts a pair that is still limited.
// Pairs whose window has passed go first; a caller holds at most maxLivePairsPerCaller pairs; and
// only a table full of live pairs (64 callers at their full share) evicts the oldest live pair.
func (r *Ringer) allowLocked(caller, recipient id.ID, at time.Duration) (allowed, logIt, full bool) {
	r.expirePairsLocked(at)
	k := ringPair{caller: caller, recipient: recipient}
	ps := r.pairs[k]
	if ps == nil {
		if r.perPair[caller] >= maxLivePairsPerCaller {
			return false, false, true
		}
		if len(r.pairs) >= maxRingPairs {
			if old, ok := r.order.Front().Value.(*pairState); ok {
				r.log.Warn("the ring pair table is full of live pairs; the oldest is evicted",
					"caller", old.key.caller.String(), "recipient", old.key.recipient.String())
				r.dropPairLocked(old)
			}
		}
		ps = &pairState{key: k}
		ps.elem = r.order.PushBack(ps)
		r.pairs[k] = ps
		r.perPair[caller]++
	}
	keep := 0
	for keep < len(ps.rings) && at-ps.rings[keep] >= ringPairWindow {
		keep++
	}
	ps.rings = ps.rings[keep:]
	if n := len(ps.rings); n >= ringPairPerHour || (n > 0 && at-ps.rings[n-1] < ringPairGap) {
		if !ps.seen || at-ps.logged >= ringPairWindow {
			ps.logged, ps.seen = at, true
			logIt = true
		}
		return false, logIt, false
	}
	ps.rings = append(ps.rings, at)
	r.order.MoveToBack(ps.elem)
	return true, false, false
}

// expirePairsLocked drops every pair whose newest ring is a whole window old. The order list is by
// newest ring, so they are all at its front. r.mu is held.
func (r *Ringer) expirePairsLocked(at time.Duration) {
	for e := r.order.Front(); e != nil; e = r.order.Front() {
		ps, ok := e.Value.(*pairState)
		if !ok || (len(ps.rings) > 0 && at-ps.rings[len(ps.rings)-1] < ringPairWindow) {
			return
		}
		r.dropPairLocked(ps)
	}
}

// dropPairLocked removes ps from the table. r.mu is held.
func (r *Ringer) dropPairLocked(ps *pairState) {
	r.order.Remove(ps.elem)
	delete(r.pairs, ps.key)
	if r.perPair[ps.key.caller]--; r.perPair[ps.key.caller] <= 0 {
		delete(r.perPair, ps.key.caller)
	}
}

// expire ends user's ring when its timer fires, unless the ring was answered, declined or ended
// first (stop closed) or replaced.
func (r *Ringer) expire(call, user id.ID, rg *ring) {
	select {
	case <-rg.timer.C():
	case <-rg.stop:
		return
	}
	r.mu.Lock()
	rc := r.calls[call]
	if rc == nil || rc.users[user] != rg {
		r.mu.Unlock()
		return
	}
	r.dropLocked(call, rc, user)
	channel := rc.channel
	r.mu.Unlock()
	r.deliver(user, call, channel, rg.caller, 0, rg.expires)
}

// take removes user's ring of call and stops its timer, answering it and the call's channel; nil
// when the user is not being rung for the call.
func (r *Ringer) take(call, user id.ID, match func(*ring) bool) (*ring, id.ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rc := r.calls[call]
	if rc == nil {
		return nil, id.Zero
	}
	rg := rc.users[user]
	if rg == nil || !match(rg) {
		return nil, id.Zero
	}
	r.dropLocked(call, rc, user)
	return rg, rc.channel
}

// dropLocked forgets user's ring of call and stops its timer. r.mu is held.
func (r *Ringer) dropLocked(call id.ID, rc *ringCall, user id.ID) {
	if rg := rc.users[user]; rg != nil {
		rg.timer.Stop()
		select {
		case <-rg.stop:
		default:
			close(rg.stop)
		}
		if mine := r.byCaller[rg.caller]; mine != nil {
			if mine[call]--; mine[call] <= 0 {
				delete(mine, call)
			}
			if len(mine) == 0 {
				delete(r.byCaller, rg.caller)
			}
		}
	}
	delete(rc.users, user)
	if len(rc.users) == 0 {
		delete(r.calls, call)
	}
}

// Answered stops user's ring of call because one of the user's devices was admitted to the call's
// room: state 0 to every device of the user.
func (r *Ringer) Answered(call, user id.ID) {
	if r == nil {
		return
	}
	if rg, channel := r.take(call, user, func(*ring) bool { return true }); rg != nil {
		r.deliver(user, call, channel, rg.caller, 0, rg.expires)
	}
}

// Declined stops user's ring of call when expires is that ring's expires_ts — state 0 to every
// device of the user and state 2, naming the user, to every device of the caller — and answers the
// caller. Any other expires_ts (a late decline of an earlier ring of the same DM: a DM's call id is
// its channel id), or no ring, changes nothing.
func (r *Ringer) Declined(call, user id.ID, expires uint64) (caller id.ID, ok bool) {
	if r == nil {
		return id.Zero, false
	}
	rg, channel := r.take(call, user, func(rg *ring) bool { return rg.expires == expires })
	if rg == nil {
		return id.Zero, false
	}
	r.deliver(user, call, channel, rg.caller, 0, rg.expires)
	r.deliver(rg.caller, call, channel, user, 2, rg.expires)
	return rg.caller, true
}

// End stops every ring of call because the call ended: state 0 to every device of each user.
func (r *Ringer) End(call id.ID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	rc := r.calls[call]
	if rc == nil {
		r.mu.Unlock()
		return
	}
	rings := make(map[id.ID]*ring, len(rc.users))
	for u, rg := range rc.users {
		rings[u] = rg
		r.dropLocked(call, rc, u)
	}
	r.mu.Unlock()
	for u, rg := range rings {
		r.deliver(u, call, rc.channel, rg.caller, 0, rg.expires)
	}
}

// deliver sends one call_ring to every live connection of to.
func (r *Ringer) deliver(to, call, channel, user id.ID, state, expires uint64) {
	if r.gw == nil {
		return
	}
	p, err := gateway.CallRingPayload(call, channel, user, state, expires)
	if err != nil {
		r.log.Error("encoding a call_ring failed", "err", err)
		return
	}
	r.gw.DeliverUser(to, gateway.Frame{
		Op: gateway.OpCallRing, Payload: p,
		Scope: gateway.FrameScope{Channels: []id.ID{channel}},
	})
}

// --- the Calls side: the start's ring and the decline route. ---

// ring rings the other participants of a DM or group-DM call caller has just opened: every
// participant user but the caller who has no device in the call's room. A community channel rings
// nobody.
func (h *Calls) ring(ctx context.Context, ch store.ChannelRow, row store.VoiceSessionRow, caller id.ID) []id.ID {
	if h.ringer == nil || (ch.Kind != ChannelDM && ch.Kind != ChannelGroupDM) {
		return nil
	}
	members, err := h.repo.ListChannelMembers(ctx, ch.ID)
	if err != nil {
		h.log.WarnContext(ctx, "reading a DM's participants to ring them failed; nobody is rung", "err", err)
		return nil
	}
	inCall := map[id.ID]bool{caller: true}
	if h.events != nil {
		for _, p := range h.events.participantsIn(row.CallID, row.LivekitRoom) {
			inCall[p.User] = true
		}
	}
	recipients := make([]id.ID, 0, len(members))
	for _, u := range members {
		if !inCall[u] {
			recipients = append(recipients, u)
		}
	}
	return h.ringer.Start(ch, row.CallID, caller, recipients)
}

// decline is POST /v1/calls/{call_id}/decline `[expires_ts]` (L-HTTP-104): 204 whether or not the
// caller's user was still being rung by the ring that expires_ts names.
//
// Attacker: a non-participant who learnt a call id can neither stop a ring nor confirm the call
// exists — an unknown call, an ended call and a call of a DM the caller is no participant of all
// answer the same 404 — and a participant's late decline of an earlier ring of the same DM (the call
// id is the channel id) names another expires_ts, so it stops nothing and tells nobody "declined".
func (h *Calls) decline(w http.ResponseWriter, r *http.Request) {
	s, err := enrolledSession(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	callID, err := server.PathID(r, "call_id")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	row, err := h.repo.GetVoiceSession(r.Context(), callID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	ch, err := h.repo.GetChannel(r.Context(), row.ChannelID)
	if err != nil {
		server.WriteError(w, notFound(err))
		return
	}
	// The DM participant check: Resolve grants view_channel in a DM or group DM to its participants
	// only (resolveDM), and in a community channel by roles and overwrites.
	bits, err := h.res.Resolve(r.Context(), s.UserID, ch)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	// WB security m2: a deleted channel answers like an unknown id, as the read routes do
	// (viewableCallChannel), whether or not the resolver already refused it.
	if !bits.Has(PermViewChannel) || row.Ended != nil || ch.DeletedAt != nil {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no such object"))
		return
	}
	if ch.Kind != ChannelDM && ch.Kind != ChannelGroupDM {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "only a DM or group-DM call rings"))
		return
	}
	var body []cbor.RawMessage
	if err := server.DecodeBody(w, r, maxCBORBody, &body); err != nil {
		server.WriteError(w, err)
		return
	}
	var expires uint64
	if len(body) != 1 || cborx.Unmarshal(body[0], &expires) != nil {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "the body is [expires_ts]"))
		return
	}
	h.ringer.Declined(callID, s.UserID, expires)
	w.WriteHeader(http.StatusNoContent)
}

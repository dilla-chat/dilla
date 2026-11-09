//go:build linux

package main

// The Bot's event feed (Bot v2 task 14): dillad pushes op 65 bot_event on the Bot's gateway connection (Task 12) and
// the Bot applies it here, instead of polling. Kinds and payloads, exactly as internal/gateway (codec.go) encodes them:
//
//	1 device_list   [user_id, push(bstr)]          the user's whole signed history from version 1 (helper op 24's body)
//	2 member        [community_id, user_id, joined(uint 0|1), [role_id…]]
//	3 role          [community_id, role_id, [allow, deny, position], deleted(uint 0|1)]
//	4 channel_group [community_id, channel_id, group_id, epoch]
//	5 bot_channel   [community_id, channel_id, marked(uint 0|1)]
//	6 snapshot_begin, 7 snapshot_end  [community_id]
//	8 community     [community_id, owner_user_id]
//
// Acknowledgement. The gateway client acknowledges ringed frames strictly in n order (Delivery.Ack), and a resume
// replays from the last acknowledged n. A ringed op 65 is acknowledged only once its handler has run (orderedAcks):
// every ringed frame after it waits behind it, so the acknowledged prefix is always applied, and a dropped socket
// replays exactly what was not.
//
// Snapshot. A cold start streams snapshot_begin and its body with n = 0 (never ringed), then a ringed snapshot_end.
// The body is buffered and applied as one job once snapshot_end arrives; only snapshot_end is acknowledged, after the
// whole snapshot ran. A drop before snapshot_end means the next connection identifies afresh and gets a new snapshot
// (dillad refuses a resume below snapshot_end), so a half-streamed snapshot is never applied.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// The op 65 kinds (internal/gateway codec.go).
const (
	feedDeviceList    uint64 = 1
	feedMember        uint64 = 2
	feedRole          uint64 = 3
	feedChannelGroup  uint64 = 4
	feedBotChannel    uint64 = 5
	feedSnapshotBegin uint64 = 6
	feedSnapshotEnd   uint64 = 7
	feedCommunity     uint64 = 8
)

// MemberEvent is a member kind: Joined false is a leave, kick or ban. Snapshot marks an event of a cold-start snapshot,
// which states the current membership rather than a join.
type MemberEvent struct {
	Community, User id.ID
	Joined          bool
	Roles           []id.ID
	Snapshot        bool
}

// RoleEvent is a role kind: the role's allow and deny bits and its position (perm.go's Resolve applies deny before
// allow in position order), or its deletion.
type RoleEvent struct {
	Community, Role       id.ID
	Allow, Deny, Position uint64
	Deleted, FromSnapshot bool
}

// ChannelGroupEvent is a channel_group kind: a Bot channel's text group or a call group the Bot is in, at epoch.
type ChannelGroupEvent struct {
	Community, Channel, Group id.ID
	Epoch                     uint64
	Snapshot                  bool
}

// Feed is what the feed's events are dispatched to (feed.go), one installation's. Every method runs on the feed's one
// worker, in feed order, and returns once the event is applied: the feed acknowledges it then. A failure is the
// handler's to log; the event counts as applied (a malformed or refused event can never apply later, and holding the
// acknowledgement would only stall every frame behind it).
//
// The task brief's DeviceList(user, version, body) is DeviceList(user, push) by ruling: kind 1 carries the whole
// helper op 24 push, which the handler hands on unchanged. Community (kind 8, the owner) is added beside the brief's
// methods, and every method takes the worker's context.
type Feed interface {
	// DeviceList returns an error only for a transient failure (fix round 1, I-2): the feed then holds the event,
	// unacknowledged with everything behind it, and applies it again on a backoff. A definitive refusal is applied.
	DeviceList(ctx context.Context, user id.ID, push []byte, snapshot bool) error
	// SnapshotBegin starts a snapshot: the handler stages the snapshot's state, and SnapshotDone replaces its state
	// with it (fix round 1, I-1). A snapshot cut off before snapshot_end never reaches SnapshotDone.
	SnapshotBegin(ctx context.Context)
	Member(ctx context.Context, ev MemberEvent)
	Role(ctx context.Context, ev RoleEvent)
	Community(ctx context.Context, community, owner id.ID, snapshot bool)
	ChannelGroup(ctx context.Context, ev ChannelGroupEvent)
	BotChannel(ctx context.Context, community, channel id.ID, marked, snapshot bool)
	SnapshotDone(ctx context.Context)
}

// feedEvent is one decoded op 65.
type feedEvent struct {
	kind         uint64
	community    id.ID
	user         id.ID
	push         []byte
	member       MemberEvent
	role         RoleEvent
	group        ChannelGroupEvent
	channel      id.ID
	marked       bool
	owner        id.ID
	fromSnapshot bool
	authz        authzFeedEvent // kinds 9 and 10 (Task 15, feed_authz.go)
}

var errFeedShape = errors.New("bot feed: malformed bot_event")

func feedField[T any](raw cbor.RawMessage) (T, error) {
	var v T
	if err := cborx.Unmarshal(raw, &v); err != nil {
		return v, errFeedShape
	}
	return v, nil
}

func feedFlag(raw cbor.RawMessage) (bool, error) {
	v, err := feedField[uint64](raw)
	if err != nil || v > 1 {
		return false, errFeedShape
	}
	return v == 1, nil
}

// decodeFeedEvent reads an op 65 payload [kind, body] strictly: the body's shape is the kind's, every id is a 16-byte
// string, every flag is uint 0 or 1.
func decodeFeedEvent(payload []cbor.RawMessage) (feedEvent, error) {
	var ev feedEvent
	if len(payload) != 2 {
		return ev, errFeedShape
	}
	kind, err := feedField[uint64](payload[0])
	if err != nil {
		return ev, err
	}
	var body []cbor.RawMessage
	if cborx.ExpectMajor(payload[1], cborx.MajorArray) != nil || cborx.Unmarshal(payload[1], &body) != nil {
		return ev, errFeedShape
	}
	if kind == feedChannelOverwrites || kind == feedMemberTimeout {
		return decodeAuthzFeedEvent(kind, body) // Task 15, feed_authz.go
	}
	want := map[uint64]int{feedDeviceList: 2, feedMember: 4, feedRole: 4, feedChannelGroup: 4, feedBotChannel: 3,
		feedSnapshotBegin: 1, feedSnapshotEnd: 1, feedCommunity: 2}[kind]
	if want == 0 || len(body) != want {
		return ev, fmt.Errorf("%w: kind %d with %d elements", errFeedShape, kind, len(body))
	}
	ev.kind = kind
	ids := func(raws ...cbor.RawMessage) ([]id.ID, error) {
		out := make([]id.ID, len(raws))
		for i, r := range raws {
			v, err := feedField[id.ID](r)
			if err != nil || v.IsZero() {
				return nil, errFeedShape
			}
			out[i] = v
		}
		return out, nil
	}
	switch kind {
	case feedDeviceList:
		u, err := ids(body[0])
		if err != nil {
			return ev, err
		}
		push, err := feedField[[]byte](body[1])
		if err != nil || len(push) == 0 {
			return ev, errFeedShape
		}
		ev.user, ev.push = u[0], push
		return ev, nil
	case feedMember:
		v, err := ids(body[0], body[1])
		if err != nil {
			return ev, err
		}
		joined, err := feedFlag(body[2])
		if err != nil {
			return ev, err
		}
		roles, err := feedField[[]id.ID](body[3])
		if err != nil {
			return ev, err
		}
		ev.community = v[0]
		ev.member = MemberEvent{Community: v[0], User: v[1], Joined: joined, Roles: roles}
	case feedRole:
		v, err := ids(body[0], body[1])
		if err != nil {
			return ev, err
		}
		bits, err := feedField[[]uint64](body[2])
		if err != nil || len(bits) != 3 {
			return ev, errFeedShape
		}
		deleted, err := feedFlag(body[3])
		if err != nil {
			return ev, err
		}
		ev.community = v[0]
		ev.role = RoleEvent{Community: v[0], Role: v[1], Allow: bits[0], Deny: bits[1], Position: bits[2], Deleted: deleted}
	case feedChannelGroup:
		v, err := ids(body[0], body[1], body[2])
		if err != nil {
			return ev, err
		}
		epoch, err := feedField[uint64](body[3])
		if err != nil {
			return ev, err
		}
		ev.community = v[0]
		ev.group = ChannelGroupEvent{Community: v[0], Channel: v[1], Group: v[2], Epoch: epoch}
	case feedBotChannel:
		v, err := ids(body[0], body[1])
		if err != nil {
			return ev, err
		}
		marked, err := feedFlag(body[2])
		if err != nil {
			return ev, err
		}
		ev.community, ev.channel, ev.marked = v[0], v[1], marked
	case feedSnapshotBegin, feedSnapshotEnd:
		v, err := ids(body[0])
		if err != nil {
			return ev, err
		}
		ev.community = v[0]
	case feedCommunity:
		v, err := ids(body[0], body[1])
		if err != nil {
			return ev, err
		}
		ev.community, ev.owner = v[0], v[1]
	}
	return ev, nil
}

// apply dispatches one event to h.
func (ev feedEvent) apply(ctx context.Context, h Feed) error {
	switch ev.kind {
	case feedDeviceList:
		return h.DeviceList(ctx, ev.user, ev.push, ev.fromSnapshot)
	case feedMember:
		m := ev.member
		m.Snapshot = ev.fromSnapshot
		h.Member(ctx, m)
	case feedRole:
		r := ev.role
		r.FromSnapshot = ev.fromSnapshot
		h.Role(ctx, r)
	case feedChannelGroup:
		g := ev.group
		g.Snapshot = ev.fromSnapshot
		h.ChannelGroup(ctx, g)
	case feedBotChannel:
		h.BotChannel(ctx, ev.community, ev.channel, ev.marked, ev.fromSnapshot)
	case feedCommunity:
		h.Community(ctx, ev.community, ev.owner, ev.fromSnapshot)
	case feedChannelOverwrites, feedMemberTimeout:
		ev.applyAuthz(ctx, h) // Task 15, feed_authz.go
	}
	return nil
}

// ackSlot is one ringed delivery waiting for its turn to be acknowledged.
type ackSlot struct {
	d    gatewayclient.Delivery
	gen  uint64
	done bool
}

// orderedAcks acknowledges a connection's ringed deliveries strictly in arrival (n) order, each once it is marked done:
// the gateway client accepts only the next n, and the acknowledged prefix is what a resume will not replay.
type orderedAcks struct {
	mu    sync.Mutex
	gen   uint64
	slots []*ackSlot
	log   *slog.Logger
}

// track queues a ringed delivery; a delivery with n = 0 has nothing to acknowledge and gets no slot.
func (a *orderedAcks) track(d gatewayclient.Delivery) *ackSlot {
	if d.Frame.N == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := &ackSlot{d: d, gen: a.gen}
	a.slots = append(a.slots, s)
	return s
}

// done marks s applied and acknowledges every leading slot that is.
func (a *orderedAcks) done(s *ackSlot) {
	if s == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.gen != a.gen {
		return // a slot of a stream the client has replaced; its ack would be stale
	}
	s.done = true
	for len(a.slots) > 0 && a.slots[0].done {
		head := a.slots[0]
		a.slots = a.slots[1:]
		if err := head.d.Ack(); err != nil && !errors.Is(err, gatewayclient.ErrStaleDelivery) && a.log != nil {
			a.log.Warn("bot gateway acknowledgement deferred", "op", uint64(head.d.Frame.Op), "error", err.Error())
		}
	}
}

// reset forgets every slot: a new identify started a new stream (its ready is n = 1 again).
func (a *orderedAcks) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gen++
	a.slots = nil
}

// feedJob is one unit of the worker: a live event, or a whole snapshot, and the slot acknowledged after it.
type feedJob struct {
	events   []feedEvent
	snapshot bool
	slot     *ackSlot
	// frames is the op 65 deliveries the job settles (a snapshot's begin, body and end), for the command barrier;
	// gen is the stream it was queued on: a job of an older stream settles nothing (fix round 2, N-1).
	frames uint64
	gen    uint64
}

// feedConsumer is one gateway connection's feed: the delivery loop hands it every op 65 (deliver) and never waits on
// it; one worker (run) applies the jobs in order once a handler is set (setFeed), and acknowledges each after it ran.
type feedConsumer struct {
	community id.ID
	acks      *orderedAcks
	log       *slog.Logger

	mu       sync.Mutex
	handler  Feed
	jobs     []feedJob
	snapshot []feedEvent // the body of the snapshot streaming now; nil outside one
	inSnap   bool
	// afterSnap is the live events seen inside the streaming snapshot: applied after it (fix round 2, N-4).
	afterSnap []feedJob
	wake      chan struct{}

	ready     chan struct{} // closed once a snapshot has been applied
	readyOnce sync.Once

	// The command barrier (Task 15, fix round 1 I-3): posted counts the stream's op 65 deliveries handed to deliver,
	// settled those applied or dropped, in delivery order; settledCh is closed and replaced whenever settled moves or
	// the stream changes. A sealed command waits until settled reaches the posted count at its arrival, so it is
	// authorised only after every event dillad sent before it. gen is the stream (fix round 2, N-1): a fresh identify
	// starts a new one with both counts at zero, and a job of an older stream settles nothing. snapFrames counts the
	// snapshot streaming now.
	//
	// A command of an older stream, or of a connection that ended (ended), waits for the next stream's snapshot
	// instead (Task 21b fix round 2): that snapshot supersedes every event sent before the command, and answering it
	// offline would lose it for its member, whose client resends only an unanswered command. snapped is the gen+1 of
	// the last stream whose snapshot was applied (0: none yet); successor is the installation's connection after
	// this one ended (nil until there is one).
	gen             uint64
	posted, settled uint64
	snapFrames      uint64
	settledCh       chan struct{}
	snapped         uint64
	ended           bool
	successor       func() *feedConsumer
}

func newFeedConsumer(community id.ID, acks *orderedAcks, log *slog.Logger) *feedConsumer {
	return &feedConsumer{community: community, acks: acks, log: log, wake: make(chan struct{}, 1), ready: make(chan struct{}),
		settledCh: make(chan struct{})}
}

// feedBarrierBound is how long a command of the current stream waits for the feed to apply the events delivered
// before it; past it the command is answered offline (retryable). A command whose stream ended or was replaced waits
// on the next stream's snapshot instead (feedNextStreamBound).
var feedBarrierBound = 2 * time.Second

// feedNextStreamBound is how long a command of an older stream or an ended connection waits for the next stream's
// snapshot: the spec's ready budget (a Bot restart is ready within 5 s). Past it the command is answered offline.
var feedNextStreamBound = readyBudget

// feedSuccessorPoll is how often a command of an ended connection looks for the installation's next one.
const feedSuccessorPoll = 50 * time.Millisecond

// settleLocked marks n more deliveries settled and wakes the commands waiting on them. f.mu is held.
func (f *feedConsumer) settleLocked(n uint64) {
	if n == 0 {
		return
	}
	f.settled = min(f.settled+n, f.posted)
	close(f.settledCh)
	f.settledCh = make(chan struct{})
}

func (f *feedConsumer) settle(n uint64) {
	f.mu.Lock()
	f.settleLocked(n)
	f.mu.Unlock()
}

// settleJob settles a finished job's frames, unless a fresh identify replaced its stream meanwhile (fix round 2, N-1).
func (f *feedConsumer) settleJob(job *feedJob) {
	f.mu.Lock()
	if job.gen == f.gen {
		if job.snapshot {
			f.snapped = job.gen + 1
		}
		f.settleLocked(job.frames)
	}
	f.mu.Unlock()
}

// end marks the connection over (its gateway client returned): the commands waiting on its stream look for the
// installation's next connection, which successor names once it exists.
func (f *feedConsumer) end(successor func() *feedConsumer) {
	f.mu.Lock()
	f.ended, f.successor = true, successor
	close(f.settledCh)
	f.settledCh = make(chan struct{})
	f.mu.Unlock()
}

// awaitNextStream is the barrier of a command whose stream was replaced (a fresh identify on this connection) or
// whose connection ended: true once a later stream's snapshot is applied, on this connection or a successor; false
// past feedNextStreamBound or when ctx ends.
func (f *feedConsumer) awaitNextStream(ctx context.Context, gen uint64) bool {
	t := time.NewTimer(feedNextStreamBound)
	defer t.Stop()
	cur := f
	for {
		cur.mu.Lock()
		ended, snapped, curGen, ch, successor := cur.ended, cur.snapped, cur.gen, cur.settledCh, cur.successor
		cur.mu.Unlock()
		if !ended && (cur != f || curGen != gen) && snapped == curGen+1 {
			return true
		}
		var poll *time.Timer
		var wake <-chan time.Time
		if ended {
			if successor != nil {
				if next := successor(); next != nil && next != cur {
					cur = next
					continue
				}
			}
			poll = time.NewTimer(feedSuccessorPoll)
			wake = poll.C
		}
		expired := false
		select {
		case <-ch:
		case <-wake:
		case <-t.C:
			expired = true
		case <-ctx.Done():
			expired = true
		}
		if poll != nil {
			poll.Stop()
		}
		if expired {
			return false
		}
	}
}

// barrier is called on the delivery loop for an op 64: it returns the wait a sealed command makes before it is
// opened and authorised, true once every op 65 delivered before it on its stream has settled, false when the stream
// changed (a fresh identify), the bound passed or ctx ended.
func (f *feedConsumer) barrier() func(context.Context) bool {
	f.mu.Lock()
	gen, target := f.gen, f.posted
	f.mu.Unlock()
	return func(ctx context.Context) bool {
		t := time.NewTimer(feedBarrierBound)
		defer t.Stop()
		for {
			f.mu.Lock()
			stale, done, ch := f.gen != gen || f.ended, f.settled >= target, f.settledCh
			f.mu.Unlock()
			if stale {
				return f.awaitNextStream(ctx, gen)
			}
			if done {
				return true
			}
			select {
			case <-ch:
			case <-t.C:
				return false
			case <-ctx.Done():
				return false
			}
		}
	}
}

func (f *feedConsumer) poke() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// setFeed sets the handler the worker applies events to; until one is set the worker holds every job (and so every
// acknowledgement behind it).
func (f *feedConsumer) setFeed(h Feed) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
	f.poke()
}

// resetStream drops a half-streamed snapshot and the jobs of the previous stream: a fresh identify brings a fresh
// snapshot that supersedes them, and their acknowledgements are stale.
func (f *feedConsumer) resetStream() {
	f.mu.Lock()
	f.jobs, f.snapshot, f.inSnap, f.afterSnap = nil, nil, false, nil
	// A new stream (fix round 2, N-1): nothing of the old one is marked applied. Its waiting commands see the
	// generation change and wait for this stream's snapshot (awaitNextStream, Task 21b fix round 2), answered offline
	// only past feedNextStreamBound; its counts start again at zero.
	f.gen++
	f.posted, f.settled, f.snapFrames = 0, 0, 0
	close(f.settledCh)
	f.settledCh = make(chan struct{})
	f.mu.Unlock()
}

// deliver takes one op 65 delivery and its slot (nil for n = 0). It never blocks on the handler.
func (f *feedConsumer) deliver(d gatewayclient.Delivery, slot *ackSlot) {
	f.mu.Lock()
	f.posted++
	f.mu.Unlock()
	ev, err := decodeFeedEvent(d.Frame.BotPayload)
	if err == nil && ev.community != (id.ID{}) && ev.community != f.community {
		err = fmt.Errorf("bot feed: an event of community %s on community %s's connection", ev.community, f.community)
	}
	if err != nil {
		// Nothing can ever apply it; acknowledging lets the frames behind it through.
		f.log.Warn("bot feed event dropped", "error", err.Error(), "n", d.Frame.N)
		f.acks.done(slot)
		f.settle(1)
		return
	}
	f.mu.Lock()
	switch {
	case ev.kind == feedSnapshotBegin:
		// A begin inside a streaming snapshot restarts it: the cut-off one's frames are settled (superseded).
		f.settleLocked(f.snapFrames)
		f.snapshot, f.inSnap, f.snapFrames = []feedEvent{}, true, 1
		f.mu.Unlock()
		f.acks.done(slot)
		return
	case ev.kind == feedSnapshotEnd:
		if !f.inSnap {
			f.settleLocked(1)
			f.mu.Unlock()
			f.log.Warn("bot feed snapshot_end without a snapshot_begin; ignored")
			f.acks.done(slot)
			return
		}
		f.jobs = append(f.jobs, feedJob{events: f.snapshot, snapshot: true, slot: slot, frames: f.snapFrames + 1, gen: f.gen})
		f.jobs = append(f.jobs, f.afterSnap...)
		f.snapshot, f.inSnap, f.afterSnap, f.snapFrames = nil, false, nil, 0
	case f.inSnap && d.Frame.N == 0:
		ev.fromSnapshot = true
		f.snapshot = append(f.snapshot, ev)
		f.snapFrames++
		f.mu.Unlock()
		return
	case f.inSnap:
		// A live event inside a snapshot (dillad sends none: the snapshot's reads supersede it) is applied after the
		// snapshot's swap, never before it, where the swap would undo it (fix round 2, N-4).
		f.afterSnap = append(f.afterSnap, feedJob{events: []feedEvent{ev}, slot: slot, frames: 1, gen: f.gen})
		f.mu.Unlock()
		return
	default:
		f.jobs = append(f.jobs, feedJob{events: []feedEvent{ev}, slot: slot, frames: 1, gen: f.gen})
	}
	f.mu.Unlock()
	f.poke()
}

// run applies jobs until ctx ends.
func (f *feedConsumer) run(ctx context.Context) {
	for {
		f.mu.Lock()
		h := f.handler
		var job *feedJob
		if h != nil && len(f.jobs) > 0 {
			j := f.jobs[0]
			f.jobs = f.jobs[1:]
			job = &j
		}
		f.mu.Unlock()
		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-f.wake:
			}
			continue
		}
		if job.snapshot {
			h.SnapshotBegin(ctx)
		}
		for _, ev := range job.events {
			if !f.applyHeld(ctx, h, ev) {
				return // unapplied: not acknowledged, so the next connection gets it again
			}
		}
		if job.snapshot {
			h.SnapshotDone(ctx)
			f.readyOnce.Do(func() { close(f.ready) })
		}
		f.acks.done(job.slot)
		f.settleJob(job)
	}
}

// feedRetryBase, feedRetryCap and feedRetryAttempts bound the retries of an event whose apply failed transiently: 7
// attempts with 6 waits of 1, 2, 4, 8, 16 and 30 s between them, about a minute (fix round 2, N-1).
var (
	feedRetryBase     = time.Second
	feedRetryAttempts = 7
)

const feedRetryCap = 30 * time.Second

// applyHeld applies ev, and while that fails transiently (a helper restart, a timeout), applies it again on a backoff
// (feedRetryBase doubling to feedRetryCap) without moving on: the event stays unacknowledged, and so does every frame
// behind it, so the order holds and a resume would replay it (fix round 1, I-2). The hold is bounded (fix round 2, N-1):
// after feedRetryAttempts the event is given up and counts as applied, so one user's list that never applies cannot
// stall the installation's feed (everyone else's revocations, the snapshot, op 64 acks). The handler keeps that user
// held, so their commands stay refused until a later ingest for them succeeds. It reports false when ctx ended first.
func (f *feedConsumer) applyHeld(ctx context.Context, h Feed, ev feedEvent) bool {
	delay := feedRetryBase
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return false
		}
		err := ev.apply(ctx, h)
		if err == nil {
			return true
		}
		if attempt >= feedRetryAttempts {
			f.log.Warn("bot feed event given up after its retries; its user stays held", "kind", ev.kind, "attempts", attempt, "error", err.Error())
			return true
		}
		f.log.Warn("bot feed event held; applying it again", "kind", ev.kind, "after", delay, "error", err.Error())
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
		delay = min(delay*2, feedRetryCap)
	}
}

// readyWithin waits until the feed has applied its first snapshot, the point from which the Bot answers commands from
// pushed state (the spec's "Bot restart ready within 5 s"), or budget passes.
func (f *feedConsumer) readyWithin(ctx context.Context, budget time.Duration) error {
	t := time.NewTimer(budget)
	defer t.Stop()
	select {
	case <-f.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return fmt.Errorf("bot feed: no snapshot applied within %v", budget)
	}
}

// readyBudget is the spec's "Bot restart ready within 5 s".
const readyBudget = 5 * time.Second

//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/bot/trust"
	"github.com/dilla-chat/dilla/internal/id"
)

// interactionRegistrationStageFunc queues a member's interaction registration under (user, episode).
type interactionRegistrationStageFunc func(context.Context, id.ID, id.ID) error

// interactionRegistrationEpisodeResolver resolves a member's current membership episode (client.CommunityMembers).
type interactionRegistrationEpisodeResolver interface {
	ResolveCurrentEpisode(context.Context, id.ID) (id.ID, bool, error)
}

// feedDeps is what an installation's feed handler acts through. A pass builds it from the installation's current
// helper and session (setDeps); the handler outlives passes and connections. ingest is bound as soon as the
// installation has a helper; the rest once it is membership-ready (fix round 1, M-2).
type feedDeps struct {
	// core is the installation's helper as membership trust uses it (op 24, pending leaves, revalidation): the
	// production binding. A new core (a fresh helper after a closed one) retries every held user at once.
	core trust.TrustCore
	// ingest is helper op 24 alone, for tests without MLS groups (no leaves to re-judge). core wins when both are set.
	ingest func(ctx context.Context, user id.ID, push []byte) (native.DeviceListIngest, error)
	// episodes resolves a member's current membership episode (the Bot-scoped episode route).
	episodes interactionRegistrationEpisodeResolver
	// stage queues a member's interaction registration for that episode (InteractionRegistrationCoordinator); the
	// next pass registers it.
	stage interactionRegistrationStageFunc
	// welcome runs a member-join episode through the welcome and join-role handler; nil when neither is enabled.
	welcome func(context.Context, protocol.Event) error
}

// feedState is what the Bot was told: the membership, the roles, the owner, the Bot channels and the channel groups
// (by group id, each at its highest epoch).
type feedState struct {
	members     map[id.ID]bool
	roles       map[id.ID]RoleEvent
	owner       id.ID
	botChannels map[id.ID]bool
	groups      map[id.ID]ChannelGroupEvent
}

func newFeedState() *feedState {
	return &feedState{members: map[id.ID]bool{}, roles: map[id.ID]RoleEvent{}, botChannels: map[id.ID]bool{}, groups: map[id.ID]ChannelGroupEvent{}}
}

// installationFeed is one installation's Feed: what the Bot learns from op 65 and what it does about it.
//
//   - device_list: membership trust (internal/bot/trust, Bot v2 Task 16) applies the push: helper op 24 unchanged,
//     then only that user's leaves in the Bot's groups are re-judged. A held or behind answer, or a transient failure,
//     makes the user Held, which the Bot's command intake refuses (fail closed). A transient failure (a closed helper,
//     a timeout, a busy, unavailable or internal status) is retried out of band on trust.DefaultRetry, per user: the
//     event is acknowledged in order, so a failing user never delays another user's events or the snapshot. A
//     definitive refusal is applied as it is. A live, changed list of a member also stages the member's interaction
//     registration, so a new device gets its own interaction group.
//   - member: a live join runs the welcome and join-role handler and stages the member's interaction registration;
//     a leave forgets the member. Either asks for a pass of this installation.
//   - role, community, channel_group: state only (Task 15 authorises commands from them, Task 16 joins Bot channels);
//     they ask for no pass (fix round 1, I-3). A channel group keeps its highest epoch (M-4). bot_channel asks for one.
//   - a snapshot (SnapshotBegin … SnapshotDone) is staged whole and replaces the state at SnapshotDone (fix round 1,
//     I-1): a role deleted or a channel unmarked while the Bot was away is gone after it, and a snapshot cut off before
//     snapshot_end leaves the state untouched. A later snapshot's members the Bot did not know joined while it was away:
//     each is welcomed and registered then.
type installationFeed struct {
	installation, community, botUser id.ID
	log                              *slog.Logger
	requestPass                      func()
	now                              func() time.Time

	mu          sync.Mutex
	deps        feedDeps
	state       *feedState
	staging     *feedState // non-nil between SnapshotBegin and SnapshotDone
	hadSnapshot bool
	// trust is the installation's membership trust: it applies device lists and keeps the held users.
	trust *trust.Trust
	// sweeping is set while a snapshot's trust sweep runs.
	sweeping atomic.Bool
	// left is the text groups the Bot has left (not a Bot channel), as the membership reconciler last reported them:
	// no trust work for them (fix round 1, M-5). needsReAdd is the Bot channels whose group the Bot cannot join on its
	// own (fix round 1, I-1).
	left, needsReAdd map[id.ID]bool
	// firstSeen is when a join episode was first heard: the welcome handler keys its pending state on the event's id
	// and time, so a retried episode must present the same ones.
	firstSeen map[id.ID]uint64
}

var _ Feed = (*installationFeed)(nil)

// feedTrustRetry is the out-of-band retry of a user's failed device list (tests shorten it).
var feedTrustRetry = trust.DefaultRetry

func newInstallationFeed(installation, community, botUser id.ID, log *slog.Logger, requestPass func()) *installationFeed {
	f := &installationFeed{installation: installation, community: community, botUser: botUser, log: log, requestPass: requestPass, now: time.Now,
		state: newFeedState(), firstSeen: map[id.ID]uint64{}}
	f.trust = trust.New(feedTrustCore{f}, feedTrustRetry).WithLogger(log).WithGroups(f.inTrustGroup)
	return f
}

// inTrustGroup reports whether membership trust works on group: not a group the Bot has left.
func (f *installationFeed) inTrustGroup(group id.ID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.left[group]
}

// close stops the feed's trust retries: the installation's feed is dropped (fix round 1, M-3).
func (f *installationFeed) close() { f.trust.Close() }

// NeedsReAdd is the Bot channels whose group the Bot cannot join on its own: a member's client must add the Bot again
// (the admin page shows them, Task 22).
func (f *installationFeed) NeedsReAdd() map[id.ID]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.needsReAdd)
}

// setDeps replaces the handler's dependencies with the current pass's. A dependency the pass did not build (nil)
// keeps the previous one. A new helper retries every user whose list is waiting for a retry (R2-3).
func (f *installationFeed) setDeps(d feedDeps) {
	f.mu.Lock()
	rebound := d.core != nil && !sameTrustCore(d.core, f.deps.core)
	if d.core != nil {
		f.deps.core = d.core
	}
	if d.ingest != nil {
		f.deps.ingest = d.ingest
	}
	if d.episodes != nil {
		f.deps.episodes = d.episodes
	}
	if d.stage != nil {
		f.deps.stage = d.stage
	}
	if d.welcome != nil {
		f.deps.welcome = d.welcome
	}
	f.mu.Unlock()
	if rebound {
		f.trust.Kick()
	}
}

// sameTrustCore reports whether a and b are the same helper; a core of an uncomparable type is never the same.
func sameTrustCore(a, b trust.TrustCore) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	return ta == tb && ta.Comparable() && a == b
}

// feedTrustCore is the feed's current helper as membership trust sees it: whatever the latest pass bound. Before any
// pass bound one, every call fails with errFeedNoHelper (transient: the user is held and retried).
type feedTrustCore struct{ f *installationFeed }

func (c feedTrustCore) IngestDeviceList(ctx context.Context, user id.ID, push []byte) (native.DeviceListIngest, error) {
	d := c.f.current()
	switch {
	case d.core != nil:
		return d.core.IngestDeviceList(ctx, user, push)
	case d.ingest != nil:
		return d.ingest(ctx, user, push)
	}
	return native.DeviceListIngest{}, errFeedNoHelper
}

func (c feedTrustCore) PendingLeaves(ctx context.Context) ([]native.GroupPendingLeaf, error) {
	d := c.f.current()
	switch {
	case d.core != nil:
		return d.core.PendingLeaves(ctx)
	case d.ingest != nil:
		return nil, nil // the test seam: no MLS groups
	}
	return nil, errFeedNoHelper
}

func (c feedTrustCore) RevalidateGroup(ctx context.Context, group id.ID, leaf, limit uint64) (uint32, error) {
	d := c.f.current()
	if d.core == nil {
		return 0, errFeedNoHelper
	}
	return d.core.RevalidateGroup(ctx, group, leaf, limit)
}

func (f *installationFeed) current() feedDeps {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deps
}

func (f *installationFeed) changed() {
	if f.requestPass != nil {
		f.requestPass()
	}
}

// Held reports whether user's commands are refused by membership trust: their latest device list failed to apply (a
// transient failure being retried out of band, or one whose retries ran out), or it answered held (a key change) or
// behind. A later push for the user that applies current releases them (their next push, or the next snapshot's).
// Command intake (Task 15) enforces it through botGatewayRuntime.Held.
func (f *installationFeed) Held(user id.ID) bool {
	return f.trust.Held(user)
}

// BotChannels is the community's Bot channels as the feed last applied them, and whether a snapshot has been applied
// (the membership reconciler narrows the Bot to these only once it has).
func (f *installationFeed) BotChannels() (map[id.ID]bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.state.botChannels), f.hadSnapshot
}

// target is the state an event writes: the staged snapshot's, or the live one.
func (f *installationFeed) target(snapshot bool) *feedState {
	if snapshot && f.staging != nil {
		return f.staging
	}
	return f.state
}

// errFeedNoHelper is a device list that arrived before any pass bound a helper: transient.
var errFeedNoHelper = errors.New("bot feed: no helper bound yet")

// DeviceList applies a pushed list through membership trust and never returns an error: a failure holds the user and
// is retried out of band (Bot v2 Task 16, R2-1), so the event is acknowledged in order and nothing behind it waits.
func (f *installationFeed) DeviceList(ctx context.Context, user id.ID, push []byte, snapshot bool) error {
	got, err := f.trust.Pushed(ctx, user, push)
	switch {
	case err == nil, errors.Is(err, trust.ErrHeld):
	case trust.Definitive(err):
		return nil // logged by trust; the helper keeps the list it had
	default:
		if errors.Is(err, helper.ErrClosed) || errors.Is(err, errFeedNoHelper) {
			// The helper went away (or none is bound yet): a pass binds a fresh one, and its binding retries now (R2-3).
			f.changed()
		}
		return nil
	}
	if snapshot || user == f.botUser {
		return nil
	}
	f.mu.Lock()
	member := f.state.members[user]
	f.mu.Unlock()
	if member && got.Changed {
		// A member's new device needs its own interaction group.
		f.stage(ctx, f.current(), user)
		f.changed()
	}
	return nil
}

// stage queues the member's interaction registration under its current episode.
func (f *installationFeed) stage(ctx context.Context, d feedDeps, user id.ID) (episode id.ID, ok bool) {
	if d.episodes == nil {
		return id.ID{}, false
	}
	episode, present, err := d.episodes.ResolveCurrentEpisode(ctx, user)
	if err != nil {
		f.log.Warn("bot feed member episode unavailable", "error", err.Error())
		return id.ID{}, false
	}
	if !present || episode.IsZero() {
		return id.ID{}, false
	}
	if d.stage != nil {
		if err := d.stage(ctx, user, episode); err != nil {
			f.log.Warn("bot feed interaction registration not staged", "error", err.Error())
		}
	}
	return episode, true
}

// joined handles a member join: the welcome and join role, then the interaction registration.
func (f *installationFeed) joined(ctx context.Context, user id.ID) {
	if user == f.botUser {
		return
	}
	d := f.current()
	episode, ok := f.stage(ctx, d, user)
	if !ok || d.welcome == nil {
		return
	}
	f.mu.Lock()
	at, seen := f.firstSeen[episode]
	if !seen {
		at = uint64(max(f.now().Unix(), 1))
		f.firstSeen[episode] = at
	}
	f.mu.Unlock()
	h := sha256.Sum256(append(append([]byte("dilla.bot.feed.member-join.v1\x00"), f.community[:]...), episode[:]...))
	var eventID id.ID
	copy(eventID[:], h[:id.Size])
	ev := protocol.Event{ID: eventID, CommunityID: f.community, Sequence: 1, Timestamp: at, Kind: protocol.EventMemberJoin, Data: []any{user, episode}}
	if err := d.welcome(ctx, ev); err != nil {
		f.log.Warn("bot feed member join not welcomed", "error", err.Error())
	}
}

func (f *installationFeed) Member(ctx context.Context, ev MemberEvent) {
	f.mu.Lock()
	if ev.Snapshot && f.staging != nil {
		if ev.Joined {
			f.staging.members[ev.User] = true
		}
		f.mu.Unlock()
		return
	}
	was := f.state.members[ev.User]
	if ev.Joined {
		f.state.members[ev.User] = true
	} else {
		delete(f.state.members, ev.User)
	}
	f.mu.Unlock()
	if ev.Joined && !was {
		f.joined(ctx, ev.User)
	}
	f.changed()
}

func (f *installationFeed) Role(_ context.Context, ev RoleEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.target(ev.FromSnapshot)
	if ev.Deleted {
		delete(s.roles, ev.Role)
	} else {
		s.roles[ev.Role] = ev
	}
}

func (f *installationFeed) Community(_ context.Context, _, owner id.ID, snapshot bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.target(snapshot).owner = owner
}

func (f *installationFeed) ChannelGroup(_ context.Context, ev ChannelGroupEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.target(ev.Snapshot)
	if old, ok := s.groups[ev.Group]; ok && old.Epoch >= ev.Epoch {
		return // an older epoch, queued before a newer one arrived, never goes back
	}
	s.groups[ev.Group] = ev
}

// BotChannel records a mark. A live mark asks for a pass, which joins the channel's group (applies its Welcome and syncs
// it); a live unmark asks for one too, and the Bot leaves the group: it is no longer synced (Bot v2 Task 16).
func (f *installationFeed) BotChannel(_ context.Context, _, channel id.ID, marked, snapshot bool) {
	f.mu.Lock()
	s := f.target(snapshot)
	was := s.botChannels[channel]
	if marked {
		s.botChannels[channel] = true
	} else {
		delete(s.botChannels, channel)
	}
	f.mu.Unlock()
	if snapshot {
		return
	}
	switch {
	case marked && !was:
		f.log.Info("bot joining Bot channel group", "channel_id", channel.String())
	case !marked && was:
		f.log.Info("bot left non-Bot channel group", "channel_id", channel.String(), "reason", "unmarked")
	}
	f.changed()
}

// SnapshotBegin starts staging: whatever an earlier, cut-off snapshot staged is dropped.
func (f *installationFeed) SnapshotBegin(context.Context) {
	f.mu.Lock()
	f.staging = newFeedState()
	f.mu.Unlock()
}

// SnapshotDone replaces the state with the staged snapshot. The members it names that the Bot did not know joined
// while it was away (after its first snapshot): each is welcomed and registered now.
func (f *installationFeed) SnapshotDone(ctx context.Context) {
	f.mu.Lock()
	snap := f.staging
	if snap == nil {
		f.mu.Unlock()
		return
	}
	var missed []id.ID
	if f.hadSnapshot {
		for u := range snap.members {
			if !f.state.members[u] {
				missed = append(missed, u)
			}
		}
	}
	f.state, f.staging, f.hadSnapshot = snap, nil, true
	f.mu.Unlock()
	for _, u := range missed {
		f.joined(ctx, u)
	}
	f.sweep(ctx)
	f.changed()
}

// sweep re-judges, once per applied snapshot and off the feed's worker (readiness never waits on it), every leaf whose
// list changed since it was judged: leaves left unjudged by a restart between op 24 and op 13, and on the first start
// under v2 every leaf once (Bot v2 Task 16). A sweep already running covers this snapshot too.
func (f *installationFeed) sweep(ctx context.Context) {
	if !f.sweeping.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer f.sweeping.Store(false)
		if err := f.trust.Sweep(context.WithoutCancel(ctx)); err != nil {
			f.log.Warn("bot trust snapshot sweep incomplete; the leaves are judged on their users' next push or snapshot", "error", err.Error())
		}
	}()
}

// snapshotOfState is a copy of the handler's state (tests).
func (f *installationFeed) snapshotOfState() feedState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return feedState{members: maps.Clone(f.state.members), roles: maps.Clone(f.state.roles), owner: f.state.owner,
		botChannels: maps.Clone(f.state.botChannels), groups: maps.Clone(f.state.groups)}
}

// Held is the query command intake (Task 15) refuses a user's command by: whether membership trust holds user on
// installation's feed (installationFeed.Held). An installation without a feed yet has applied nothing and holds no
// one; intake gates on the feed's readiness for that.
func (r *botGatewayRuntime) Held(installation, user id.ID) bool {
	f := r.feedHandler(installation)
	return f != nil && f.Held(user)
}

// BotChannels is the membership reconciler's view of installation's Bot channels (service.BotChannels): unknown until
// the installation's feed has applied a snapshot.
func (r *botGatewayRuntime) BotChannels(installation id.ID) service.BotChannels {
	return feedBotChannels{r: r, installation: installation}
}

// feedBotChannels links installation's membership reconciler to its feed (service.BotChannels).
type feedBotChannels struct {
	r            *botGatewayRuntime
	installation id.ID
}

func (b feedBotChannels) Marked() (map[id.ID]bool, bool) {
	f := b.r.feedHandler(b.installation)
	if f == nil {
		return nil, false
	}
	return f.BotChannels()
}

func (b feedBotChannels) Membership(left, needsReAdd map[id.ID]bool) {
	f := b.r.feedHandler(b.installation)
	if f == nil {
		return
	}
	f.mu.Lock()
	f.left, f.needsReAdd = maps.Clone(left), maps.Clone(needsReAdd)
	f.mu.Unlock()
}

// nativeRegistry is where a pass finds an installation's retained helper (musicQueueRegistry).
type nativeRegistry interface {
	Native(installation, community id.ID) musicRuntimeNative
	Disable(installation id.ID) error
}

// retainedNativeClient is installation's retained helper while it can still serve calls. A retained helper that
// closed (its process went away) or was invalidated is retired, so the pass starts a fresh one and the feed's ingest
// works again without a Bot restart (Bot v2 Task 16, R2-3).
func retainedNativeClient(reg nativeRegistry, installation, community id.ID) (*native.Client, bool) {
	n := reg.Native(installation, community)
	if u, ok := n.(interface{ Usable() bool }); ok && !u.Usable() {
		_ = reg.Disable(installation)
		return nil, false
	}
	c, ok := n.(*native.Client)
	return c, ok
}

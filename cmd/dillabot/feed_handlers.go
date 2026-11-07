//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
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
	// ingest is helper op 24 (native.Client.IngestDeviceList).
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
//   - device_list: the push goes to helper op 24 unchanged. A held or behind answer is logged; the helper refuses that
//     user's commands itself until a later push makes the list current. A definitive refusal (*helper.StatusError)
//     is applied as it is; any other failure is transient: the event is held and applied again (feed.applyHeld), and
//     until it applies the user is Held, which the Bot's command intake refuses (fail closed). A live, changed list of
//     a member also stages the member's interaction registration, so a new device gets its own interaction group.
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
	// held is the users whose latest device list failed transiently and is being applied again: fail closed.
	held map[id.ID]bool
	// firstSeen is when a join episode was first heard: the welcome handler keys its pending state on the event's id
	// and time, so a retried episode must present the same ones.
	firstSeen map[id.ID]uint64
}

var _ Feed = (*installationFeed)(nil)

func newInstallationFeed(installation, community, botUser id.ID, log *slog.Logger, requestPass func()) *installationFeed {
	return &installationFeed{installation: installation, community: community, botUser: botUser, log: log, requestPass: requestPass, now: time.Now,
		state: newFeedState(), held: map[id.ID]bool{}, firstSeen: map[id.ID]uint64{}}
}

// setDeps replaces the handler's dependencies with the current pass's. A dependency the pass did not build (nil)
// keeps the previous one.
func (f *installationFeed) setDeps(d feedDeps) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

// Held reports whether user's latest device list failed to apply (a transient failure being retried, or one whose
// retries ran out, fix round 2, N-1): the user's commands are refused until a later ingest for the user succeeds (their
// next push, or the next snapshot's). Command intake (Task 15) enforces it through botGatewayRuntime.Held.
func (f *installationFeed) Held(user id.ID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held[user]
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

func (f *installationFeed) DeviceList(ctx context.Context, user id.ID, push []byte, snapshot bool) error {
	d := f.current()
	if d.ingest == nil {
		f.hold(user)
		return errFeedNoHelper
	}
	got, err := d.ingest(ctx, user, push)
	var status *helper.StatusError
	switch {
	case errors.As(err, &status):
		f.release(user)
		f.log.Warn("bot feed device list refused by the helper", "helper_status", status.Status)
		return nil
	case err != nil:
		f.hold(user)
		if errors.Is(err, helper.ErrClosed) {
			// The helper went away: a pass binds a fresh one (fix round 2, N-1).
			f.changed()
		}
		return err
	case got.State == native.DeviceListHeld:
		f.log.Info("bot feed device list held: a key change is pending; the user's commands are refused until it is verified", "version", got.Version)
	case got.State == native.DeviceListBehind:
		f.log.Info("bot feed device list behind the pushed head; the user's commands are refused until a full history arrives", "version", got.Version)
	}
	f.release(user)
	if snapshot || user == f.botUser {
		return nil
	}
	f.mu.Lock()
	member := f.state.members[user]
	f.mu.Unlock()
	if member && got.Changed {
		// A member's new device needs its own interaction group.
		f.stage(ctx, d, user)
		f.changed()
	}
	return nil
}

func (f *installationFeed) hold(user id.ID) {
	f.mu.Lock()
	f.held[user] = true
	f.mu.Unlock()
}

func (f *installationFeed) release(user id.ID) {
	f.mu.Lock()
	delete(f.held, user)
	f.mu.Unlock()
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

func (f *installationFeed) BotChannel(_ context.Context, _, channel id.ID, marked, snapshot bool) {
	f.mu.Lock()
	s := f.target(snapshot)
	if marked {
		s.botChannels[channel] = true
	} else {
		delete(s.botChannels, channel)
	}
	f.mu.Unlock()
	if !snapshot {
		f.changed()
	}
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
	f.changed()
}

// snapshotOfState is a copy of the handler's state (tests).
func (f *installationFeed) snapshotOfState() feedState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return feedState{members: maps.Clone(f.state.members), roles: maps.Clone(f.state.roles), owner: f.state.owner,
		botChannels: maps.Clone(f.state.botChannels), groups: maps.Clone(f.state.groups)}
}

// Held is the query command intake (Task 15) refuses a user's command by: whether user's latest device list failed to
// apply on installation's feed and no later ingest for the user has succeeded since (fix round 2, N-1). An installation
// without a feed yet has applied nothing and holds no one; intake gates on the feed's readiness for that.
func (r *botGatewayRuntime) Held(installation, user id.ID) bool {
	f := r.feedHandler(installation)
	return f != nil && f.Held(user)
}

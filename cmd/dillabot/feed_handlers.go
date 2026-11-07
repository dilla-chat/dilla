//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
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
// helper and session (setDeps); the handler outlives passes and connections.
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

// installationFeed is one installation's Feed: what the Bot learns from op 65 and what it does about it.
//
//   - device_list: the push goes to helper op 24 unchanged. A held or behind answer is logged; the helper refuses that
//     user's commands itself until a later push makes the list current. A live list of a member also stages the
//     member's interaction registration, so a new device gets its own interaction group (the v1 device-list
//     structural event's job).
//   - member: a live join runs the welcome and join-role handler and stages the member's interaction registration;
//     a leave forgets the member. A snapshot states the membership: the first one is the baseline, and a later one's
//     members that the Bot did not know are joins it missed while it was away.
//   - role, community, bot_channel, channel_group: kept (Task 15 authorises commands from them, Task 16 joins and
//     leaves Bot channels from them).
//
// Every live change asks for one reconciliation pass (requestPass), which re-reads what the change affects and
// registers staged members; nothing runs on a timer.
type installationFeed struct {
	installation, community, botUser id.ID
	log                              *slog.Logger
	requestPass                      func()
	now                              func() time.Time

	mu          sync.Mutex
	deps        feedDeps
	members     map[id.ID]bool
	snapMembers map[id.ID]bool
	hadSnapshot bool
	roles       map[id.ID]RoleEvent
	owner       id.ID
	botChannels map[id.ID]bool
	groups      map[id.ID]ChannelGroupEvent // by channel
	// firstSeen is when a join episode was first heard: the welcome handler keys its pending state on the event's id
	// and time, so a retried episode must present the same ones.
	firstSeen map[id.ID]uint64
}

var _ Feed = (*installationFeed)(nil)

func newInstallationFeed(installation, community, botUser id.ID, log *slog.Logger, requestPass func()) *installationFeed {
	return &installationFeed{installation: installation, community: community, botUser: botUser, log: log, requestPass: requestPass, now: time.Now,
		members: map[id.ID]bool{}, roles: map[id.ID]RoleEvent{}, botChannels: map[id.ID]bool{}, groups: map[id.ID]ChannelGroupEvent{},
		firstSeen: map[id.ID]uint64{}}
}

// setDeps replaces the handler's dependencies with the current pass's.
func (f *installationFeed) setDeps(d feedDeps) {
	f.mu.Lock()
	f.deps = d
	f.mu.Unlock()
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

func (f *installationFeed) DeviceList(ctx context.Context, user id.ID, push []byte, snapshot bool) {
	d := f.current()
	if d.ingest == nil {
		f.log.Warn("bot feed device list not applied: no helper")
		return
	}
	got, err := d.ingest(ctx, user, push)
	switch {
	case err != nil:
		var status *helper.StatusError
		if errors.As(err, &status) {
			f.log.Warn("bot feed device list refused by the helper", "helper_status", status.Status)
		} else {
			f.log.Warn("bot feed device list not applied", "error", err.Error())
		}
		return
	case got.State == native.DeviceListHeld:
		f.log.Info("bot feed device list held: a key change is pending; the user's commands are refused until it is verified", "version", got.Version)
	case got.State == native.DeviceListBehind:
		f.log.Info("bot feed device list behind the pushed head; the user's commands are refused until a full history arrives", "version", got.Version)
	}
	if snapshot || user == f.botUser {
		return
	}
	f.mu.Lock()
	member := f.members[user]
	f.mu.Unlock()
	if member && got.Changed {
		// A member's new device needs its own interaction group.
		f.stage(ctx, d, user)
		f.changed()
	}
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
	if ev.Snapshot {
		if f.snapMembers == nil {
			f.snapMembers = map[id.ID]bool{}
		}
		f.snapMembers[ev.User] = true
		f.mu.Unlock()
		return
	}
	was := f.members[ev.User]
	if ev.Joined {
		f.members[ev.User] = true
	} else {
		delete(f.members, ev.User)
	}
	f.mu.Unlock()
	if ev.Joined && !was {
		f.joined(ctx, ev.User)
	}
	f.changed()
}

func (f *installationFeed) Role(_ context.Context, ev RoleEvent) {
	f.mu.Lock()
	if ev.Deleted {
		delete(f.roles, ev.Role)
	} else {
		f.roles[ev.Role] = ev
	}
	f.mu.Unlock()
	if !ev.FromSnapshot {
		f.changed()
	}
}

func (f *installationFeed) Community(_ context.Context, _, owner id.ID, snapshot bool) {
	f.mu.Lock()
	f.owner = owner
	f.mu.Unlock()
	if !snapshot {
		f.changed()
	}
}

func (f *installationFeed) ChannelGroup(_ context.Context, ev ChannelGroupEvent) {
	f.mu.Lock()
	f.groups[ev.Channel] = ev
	f.mu.Unlock()
	if !ev.Snapshot {
		f.changed()
	}
}

func (f *installationFeed) BotChannel(_ context.Context, _, channel id.ID, marked, snapshot bool) {
	f.mu.Lock()
	if marked {
		f.botChannels[channel] = true
	} else {
		delete(f.botChannels, channel)
	}
	f.mu.Unlock()
	if !snapshot {
		f.changed()
	}
}

// SnapshotDone makes the snapshot's membership the Bot's. The members a later snapshot names that the Bot did not know
// joined while it was away: each is welcomed and registered now.
func (f *installationFeed) SnapshotDone(ctx context.Context) {
	f.mu.Lock()
	snap := f.snapMembers
	if snap == nil {
		snap = map[id.ID]bool{}
	}
	var missed []id.ID
	if f.hadSnapshot {
		for u := range snap {
			if !f.members[u] {
				missed = append(missed, u)
			}
		}
	}
	f.members, f.snapMembers, f.hadSnapshot = snap, nil, true
	f.mu.Unlock()
	for _, u := range missed {
		f.joined(ctx, u)
	}
	f.changed()
}

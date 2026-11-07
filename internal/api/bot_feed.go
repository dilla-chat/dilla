package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// The Bot's event feed (Bot v2 task 12): op 65 bot_event on the Bot runtime's connection only. It
// replaces the Bot's polling of the structural-event routes, which are gone.
//
// What the Bot learns is bounded by its installation's one community:
//   - device_list: the signed list history of each current member of that community, never of
//     anyone else (feedBot.inScope);
//   - member, role, community: that community's members with their role ids, its roles' bits and
//     its owner;
//   - bot_channel: that community's Bot-channel marks;
//   - channel_group: the epoch of a group in that community that is a Bot channel's text group or a
//     call group the Bot's device is a member of;
//   - channel_overwrites (task 15): the permission overwrites of that community's Bot channels and voice
//     channels, and of no other channel, so the Bot authorises a command in its channel as dillad would;
//   - member_timeout (task 15): a member's timeout end, so the Bot refuses a timed-out member.
//
// Nothing about another community, and no message content. Every event is emitted after the change
// committed: the write paths call in after their transaction (PutDeviceList, StructureNotifier, the
// BotChannelEvents callers, ds.Options.OnEpoch), and the feed reads the committed state itself.
//
// Each Bot has one worker. A write path only marks work for it and wakes it (never blocking on the
// Bot's socket); the worker coalesces what accumulated, reads the current state, diffs it against
// what the Bot was last told and sends the difference. Because a device list is always read whole
// and current at send time, one user's pushes reach the Bot in version order.
//
// The cold start: on a Bot identify (not a resume) the gateway hands the feed the connection's
// BotStream, and the worker streams the snapshot (snapshot_begin; every member's device list; the
// members; the roles; the owner; the Bot channels; snapshot_end). That is one pass per Bot start,
// never per command or on a timer. Live work that arrives meanwhile is done after the snapshot.

// BotFeedMaxDeviceListBytes is the largest device-list push the feed sends: the Bot's helper frame
// (about 1 MiB) is the practical cap below protocol.MaxDeviceListPushBytes. A user whose push is
// larger is left out, logged and counted, and the rest of the feed goes on.
const BotFeedMaxDeviceListBytes = 1 << 20

// BotFeedGateway is what the feed needs of the gateway. *gateway.Gateway is one.
type BotFeedGateway interface {
	DeliverBot(deviceID id.ID, f gateway.Frame) bool
}

// BotFeedStream is a Bot connection's cold-start stream, *gateway.BotStream.
type BotFeedStream interface {
	Installation() id.ID
	Community() id.ID
	Device() id.ID
	Send(context.Context, gateway.Frame) bool
	Finish(gateway.Frame) bool
	Abort(reason string)
}

// BotFeedHooks is what the write paths tell the feed after their commit. Deps.BotFeed holds one; nil
// tells nobody.
type BotFeedHooks interface {
	DeviceListChanged(user id.ID)
	CommunityChanged(community id.ID)
}

// BotFeedOptions configures NewBotFeed.
type BotFeedOptions struct {
	Repo    store.Repository
	Gateway BotFeedGateway
	Log     *slog.Logger
	// Skipped counts a device-list push left out, by reason: too_large, too_many_rows, no_keys or
	// invalid. Nil counts nothing.
	Skipped func(reason string)
}

// BotFeed is the feed of every installed Bot of the instance.
type BotFeed struct {
	ctx    context.Context
	cancel context.CancelFunc
	o      BotFeedOptions
	wg     sync.WaitGroup
	mu     sync.Mutex
	bots   map[id.ID]*feedBot // by installation
}

// NewBotFeed starts an empty feed; its workers stop with ctx or Stop.
func NewBotFeed(ctx context.Context, o BotFeedOptions) *BotFeed {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	ctx, cancel := context.WithCancel(ctx)
	return &BotFeed{ctx: ctx, cancel: cancel, o: o, bots: map[id.ID]*feedBot{}}
}

// Stop ends every worker and waits for them, so no read is in flight when the store closes.
func (f *BotFeed) Stop() {
	f.cancel()
	f.wg.Wait()
}

// roleState is what the Bot was told of one role.
type roleState struct{ allow, deny, position uint64 }

// feedBot is one installation's feed: its pending work and what its Bot was last told.
type feedBot struct {
	installation, community, device id.ID
	wake                            chan struct{}
	// retired is closed when a newer device or community replaced this installation's feed.
	retired chan struct{}

	mu sync.Mutex
	// offline is set when no connection of the Bot took a frame: nothing is fed until its next identify.
	offline  bool
	snapshot BotFeedStream
	// snapshotting is set while the worker streams a snapshot: a device-list change then is queued
	// whoever it is for, since the snapshot may have read that user's list before the change.
	snapshotting bool
	// stream is the connection the last finished snapshot went out on: a read failure ends it, so the
	// Bot identifies again and is resynchronised by a fresh snapshot instead of missing a change.
	stream    BotFeedStream
	structure bool // members, roles or owner may have changed
	channels  bool // Bot-channel marks may have changed
	users     map[id.ID]struct{}
	groups    []ds.EpochChange
	barriers  []chan struct{}
	// members is the Bot's view of its community's membership (user -> role ids), read under mu by
	// the hooks for scope; roles, owner and marked are the rest of what it was told.
	members map[id.ID][]id.ID
	roles   map[id.ID]roleState
	owner   id.ID
	marked  map[id.ID]bool
	// overwrites is each Bot or voice channel's overwrites as told (task 15); timeouts each timed-out member's
	// end as told; timeoutUsers the members whose timeout changed since the last turn.
	overwrites   map[id.ID][]feedOverwrite
	timeouts     map[id.ID]uint64
	timeoutUsers map[id.ID]struct{}
}

// feedOverwrite is one channel_overwrites entry: target_kind 0 @everyone, 1 role, 2 member.
type feedOverwrite struct {
	kind        uint64
	target      id.ID
	allow, deny uint64
}

// bot returns the installation's feed, creating it and its worker on first use.
func (f *BotFeed) bot(installation, community, device id.ID) *feedBot {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.bots[installation]
	if b == nil || b.device != device || b.community != community {
		if b != nil {
			close(b.retired)
		}
		b = &feedBot{installation: installation, community: community, device: device, wake: make(chan struct{}, 1),
			retired: make(chan struct{}), users: map[id.ID]struct{}{}, timeoutUsers: map[id.ID]struct{}{}}
		f.bots[installation] = b
		f.wg.Add(1)
		go f.run(b)
	}
	return b
}

// Forget drops an installation's feed and stops its worker (fix round 1 M-4): its Bot was disabled or
// removed. A later identify of the same installation starts a new one.
func (f *BotFeed) Forget(installation id.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b := f.bots[installation]; b != nil {
		close(b.retired)
		delete(f.bots, installation)
	}
}

// each runs fn on every Bot feed of a community, or on every Bot feed when community is zero.
func (f *BotFeed) each(community id.ID, fn func(*feedBot)) {
	f.mu.Lock()
	bots := make([]*feedBot, 0, len(f.bots))
	for _, b := range f.bots {
		if community.IsZero() || b.community == community {
			bots = append(bots, b)
		}
	}
	f.mu.Unlock()
	for _, b := range bots {
		fn(b)
	}
}

func (b *feedBot) poke() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// BotIdentified starts a Bot's cold start: the snapshot goes out on s, and the Bot is fed again.
func (f *BotFeed) BotIdentified(s BotFeedStream) {
	b := f.bot(s.Installation(), s.Community(), s.Device())
	b.mu.Lock()
	if b.snapshot != nil {
		b.snapshot.Abort("a newer Bot connection identified")
	}
	b.snapshot, b.offline = s, false
	b.mu.Unlock()
	b.poke()
}

// DeviceListChanged queues the user's device list for every Bot whose community they are a member of.
func (f *BotFeed) DeviceListChanged(user id.ID) {
	f.each(id.ID{}, func(b *feedBot) {
		b.mu.Lock()
		_, member := b.members[user]
		queue := !b.offline && (member || b.structure || b.snapshot != nil || b.snapshotting)
		if queue {
			b.users[user] = struct{}{}
		}
		b.mu.Unlock()
		if queue {
			b.poke()
		}
	})
}

// CommunityChanged queues a members, roles and owner diff for the community's Bot.
func (f *BotFeed) CommunityChanged(community id.ID) {
	f.each(community, func(b *feedBot) {
		b.mu.Lock()
		queue := !b.offline
		b.structure = b.structure || queue
		b.mu.Unlock()
		if queue {
			b.poke()
		}
	})
}

// MemberTimeoutChanged queues the member's timeout for the community's Bot (task 15): the timeout routes call it
// after their commit, through StructureNotifier.ChangedUser.
func (f *BotFeed) MemberTimeoutChanged(community, user id.ID) {
	f.each(community, func(b *feedBot) {
		b.mu.Lock()
		queue := !b.offline
		if queue {
			b.timeoutUsers[user] = struct{}{}
		}
		b.mu.Unlock()
		if queue {
			b.poke()
		}
	})
}

// BotChannelMarked and BotChannelUnmarked are the feed's BotChannelEvents: they queue a Bot-channel diff.
func (f *BotFeed) BotChannelMarked(community, _ id.ID)   { f.channelsChanged(community) }
func (f *BotFeed) BotChannelUnmarked(community, _ id.ID) { f.channelsChanged(community) }

func (f *BotFeed) channelsChanged(community id.ID) {
	f.each(community, func(b *feedBot) {
		b.mu.Lock()
		queue := !b.offline
		b.channels = b.channels || queue
		b.mu.Unlock()
		if queue {
			b.poke()
		}
	})
}

// GroupEpoch is ds.Options.OnEpoch: an epoch of a Bot channel's text group, or of a call group the
// Bot's device is in, is queued for that community's Bot. It only queues: it runs under the group lock.
func (f *BotFeed) GroupEpoch(c ds.EpochChange) {
	if c.CommunityID == nil {
		return
	}
	f.each(*c.CommunityID, func(b *feedBot) {
		b.mu.Lock()
		queue := !b.offline
		if queue {
			// Coalesced per group: only a group's newest epoch is still news when the worker runs.
			i := slices.IndexFunc(b.groups, func(g ds.EpochChange) bool { return g.GroupID == c.GroupID })
			switch {
			case i < 0:
				b.groups = append(b.groups, c)
			case c.Epoch > b.groups[i].Epoch:
				b.groups[i] = c
			}
		}
		b.mu.Unlock()
		if queue {
			b.poke()
		}
	})
}

// Drain waits until every Bot's queued work is done, or ctx ends.
func (f *BotFeed) Drain(ctx context.Context) error {
	var waits []chan struct{}
	f.each(id.ID{}, func(b *feedBot) {
		done := make(chan struct{})
		b.mu.Lock()
		b.barriers = append(b.barriers, done)
		b.mu.Unlock()
		b.poke()
		waits = append(waits, done)
	})
	for _, w := range waits {
		select {
		case <-w:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// feedBatch is one turn of a worker: everything queued since the last.
type feedBatch struct {
	snapshot           BotFeedStream
	structure, channel bool
	users              []id.ID
	timeouts           []id.ID
	groups             []ds.EpochChange
	barriers           []chan struct{}
}

func (b *feedBot) take() (feedBatch, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	batch := feedBatch{snapshot: b.snapshot, structure: b.structure, channel: b.channels, groups: b.groups, barriers: b.barriers}
	b.snapshotting = batch.snapshot != nil
	// A snapshot taken now supersedes any failure before it (fix round 1 I-1): a goOffline from the
	// old connection's last send, landing after the Bot re-identified, must not leave the feed deaf for
	// the whole new snapshot. Changes committed from here are queued and follow snapshot_end.
	if batch.snapshot != nil {
		b.offline = false
	}
	for u := range b.users {
		batch.users = append(batch.users, u)
	}
	for u := range b.timeoutUsers {
		batch.timeouts = append(batch.timeouts, u)
	}
	b.snapshot, b.structure, b.channels, b.groups, b.barriers = nil, false, false, nil, nil
	clear(b.users)
	clear(b.timeoutUsers)
	empty := batch.snapshot == nil && !batch.structure && !batch.channel && len(batch.users) == 0 && len(batch.groups) == 0 &&
		len(batch.timeouts) == 0
	return batch, !empty || len(batch.barriers) > 0
}

func (f *BotFeed) run(b *feedBot) {
	defer f.wg.Done()
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-b.retired:
			return
		case <-b.wake:
		}
		for {
			batch, ok := b.take()
			if !ok {
				break
			}
			f.process(b, batch)
			for _, done := range batch.barriers {
				close(done)
			}
		}
	}
}

// errBotOffline ends a turn whose Bot no connection takes frames for.
var errBotOffline = errors.New("bot feed: the Bot is offline")

func (f *BotFeed) process(b *feedBot, batch feedBatch) {
	ctx := f.ctx
	if batch.snapshot != nil {
		if err := f.snapshot(ctx, b, batch.snapshot); err != nil {
			if !errors.Is(err, errBotOffline) {
				f.o.Log.Error("bot feed: the cold-start snapshot failed", "installation", b.installation.String(), "err", err)
				batch.snapshot.Abort("bot snapshot unavailable")
			}
			f.goOffline(b)
			return
		}
	}
	err := func() error {
		users := batch.users
		timeouts := batch.timeouts
		if batch.structure {
			joined, err := f.structure(ctx, b)
			if err != nil {
				return err
			}
			users = append(users, joined...)
			// A rejoining member may still be timed out.
			timeouts = append(timeouts, joined...)
		}
		if batch.channel {
			if err := f.botChannels(ctx, b); err != nil {
				return err
			}
		}
		// Overwrites change with a channel, a role, an overwrite (a community change) or a Bot-channel mark.
		if batch.structure || batch.channel {
			if err := f.channelOverwrites(ctx, b, f.liveSender(b)); err != nil {
				return err
			}
		}
		if err := f.memberTimeouts(ctx, b, timeouts, b.inScope, f.liveSender(b)); err != nil {
			return err
		}
		slices.SortFunc(users, compareIDs)
		for _, u := range slices.Compact(users) {
			if !b.inScope(u) {
				continue
			}
			if err := f.deviceList(ctx, b, u, f.liveSender(b)); err != nil {
				return err
			}
		}
		for _, c := range batch.groups {
			if err := f.channelGroup(b, c); err != nil {
				return err
			}
		}
		return nil
	}()
	switch {
	case errors.Is(err, errBotOffline):
		f.goOffline(b)
	case err != nil:
		// The change stays committed and the Bot's view is behind it, so its connection is ended: its
		// next identify streams a fresh snapshot, rather than the feed sending a partial diff later.
		f.o.Log.Error("bot feed: reading the change failed", "installation", b.installation.String(), "err", err)
		b.mu.Lock()
		stream := b.stream
		b.mu.Unlock()
		f.goOffline(b)
		if stream != nil {
			stream.Abort("bot feed unavailable")
		}
	}
}

// goOffline stops feeding the Bot until its next identify, which streams a fresh snapshot.
func (f *BotFeed) goOffline(b *feedBot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.offline, b.snapshotting = true, false
	b.structure, b.channels, b.groups = false, false, nil
	clear(b.users)
	clear(b.timeoutUsers)
}

// inScope is the device-list scope: a current member of the Bot's community, as last told.
func (b *feedBot) inScope(user id.ID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.members[user]
	return ok
}

// event encodes one op 65 frame.
func event(kind uint64, body any) (gateway.Frame, error) {
	p, err := gateway.BotEventPayload(kind, body)
	if err != nil {
		return gateway.Frame{}, err
	}
	return gateway.Frame{Op: gateway.OpBotEvent, Payload: p, Replay: true}, nil
}

// liveSender sends live events through the gateway.
func (f *BotFeed) liveSender(b *feedBot) func(gateway.Frame) error {
	return func(fr gateway.Frame) error {
		if !f.o.Gateway.DeliverBot(b.device, fr) {
			return errBotOffline
		}
		return nil
	}
}

// snapshot streams the cold start on s and resets what the Bot was told to the state it streamed.
func (f *BotFeed) snapshot(ctx context.Context, b *feedBot, s BotFeedStream) error {
	send := func(fr gateway.Frame) error {
		fr.Replay = false
		if !s.Send(ctx, fr) {
			return errBotOffline
		}
		return nil
	}
	begin, err := event(gateway.BotEventSnapshotBegin, []any{b.community})
	if err != nil {
		return err
	}
	if err := send(begin); err != nil {
		return err
	}
	community, err := f.o.Repo.GetCommunity(ctx, b.community)
	if err != nil {
		return err
	}
	members, err := f.readMembers(ctx, b.community)
	if err != nil {
		return err
	}
	roles, err := f.o.Repo.ListRoles(ctx, b.community)
	if err != nil {
		return err
	}
	marked, err := f.o.Repo.ListBotChannels(ctx, b.community)
	if err != nil {
		return err
	}
	users := make([]id.ID, 0, len(members))
	for u := range members {
		users = append(users, u)
	}
	slices.SortFunc(users, compareIDs)
	for _, u := range users {
		if err := f.deviceList(ctx, b, u, send); err != nil {
			return err
		}
	}
	for _, u := range users {
		fr, err := event(gateway.BotEventMember, []any{b.community, u, uint64(1), members[u]})
		if err != nil {
			return err
		}
		if err := send(fr); err != nil {
			return err
		}
	}
	told := map[id.ID]roleState{}
	for _, r := range roles {
		st := roleState{allow: r.Allow, deny: r.Deny, position: r.Position}
		fr, err := event(gateway.BotEventRole, []any{b.community, r.ID, []uint64{st.allow, st.deny, st.position}, uint64(0)})
		if err != nil {
			return err
		}
		if err := send(fr); err != nil {
			return err
		}
		told[r.ID] = st
	}
	fr, err := event(gateway.BotEventCommunity, []any{b.community, community.Owner})
	if err != nil {
		return err
	}
	if err := send(fr); err != nil {
		return err
	}
	channels := map[id.ID]bool{}
	for _, m := range marked {
		fr, err := event(gateway.BotEventBotChannel, []any{b.community, m.ChannelID, uint64(1)})
		if err != nil {
			return err
		}
		if err := send(fr); err != nil {
			return err
		}
		channels[m.ChannelID] = true
	}
	// The channel groups (Bot v2 task 14, Task 12 review N-1): a live op 65 is dropped while the snapshot is owed, so
	// an epoch committed before these reads reaches the Bot only here. One committed after them is queued by GroupEpoch
	// and sent once snapshot_end has gone out.
	if err := f.snapshotChannelGroups(ctx, b, channels, send); err != nil {
		return err
	}
	// Task 15: the Bot and voice channels' overwrites, and every timed-out member, from scratch.
	b.mu.Lock()
	b.overwrites, b.timeouts = nil, nil
	b.mu.Unlock()
	if err := f.channelOverwritesOf(ctx, b, roles, channels, send); err != nil {
		return err
	}
	if err := f.memberTimeouts(ctx, b, users, func(u id.ID) bool { _, ok := members[u]; return ok }, send); err != nil {
		return err
	}
	end, err := event(gateway.BotEventSnapshotEnd, []any{b.community})
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.members, b.roles, b.owner, b.marked = members, told, community.Owner, channels
	b.mu.Unlock()
	if !s.Finish(end) {
		return errBotOffline
	}
	// Fed again: whatever a failure before this identify left behind is behind the snapshot now.
	b.mu.Lock()
	b.offline, b.snapshotting, b.stream = false, false, s
	b.mu.Unlock()
	return nil
}

// readMembers is the community's members with their sorted role ids.
func (f *BotFeed) readMembers(ctx context.Context, community id.ID) (map[id.ID][]id.ID, error) {
	ids, err := communityMemberIDs(ctx, f.o.Repo, community)
	if err != nil {
		return nil, err
	}
	// One read for every member's roles (fix round 1 M-3), not one per member.
	held, err := f.o.Repo.ListCommunityMemberRoles(ctx, community)
	if err != nil {
		return nil, err
	}
	out := make(map[id.ID][]id.ID, len(ids))
	for _, u := range ids {
		roles := held[u]
		if roles == nil {
			roles = []id.ID{}
		}
		slices.SortFunc(roles, compareIDs)
		out[u] = roles
	}
	return out, nil
}

// structure sends the members, roles and owner diff and reports the users who joined.
func (f *BotFeed) structure(ctx context.Context, b *feedBot) ([]id.ID, error) {
	send := f.liveSender(b)
	community, err := f.o.Repo.GetCommunity(ctx, b.community)
	if errors.Is(err, store.ErrNotFound) {
		// The community is gone; its Bot is removed with it.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	members, err := f.readMembers(ctx, b.community)
	if err != nil {
		return nil, err
	}
	roles, err := f.o.Repo.ListRoles(ctx, b.community)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	oldMembers, oldRoles, oldOwner := b.members, b.roles, b.owner
	b.mu.Unlock()

	// Roles first: a member event may name a new role.
	nowRoles := map[id.ID]roleState{}
	for _, r := range roles {
		st := roleState{allow: r.Allow, deny: r.Deny, position: r.Position}
		nowRoles[r.ID] = st
		if old, ok := oldRoles[r.ID]; ok && old == st {
			continue
		}
		fr, err := event(gateway.BotEventRole, []any{b.community, r.ID, []uint64{st.allow, st.deny, st.position}, uint64(0)})
		if err != nil {
			return nil, err
		}
		if err := send(fr); err != nil {
			return nil, err
		}
	}
	for _, rid := range sortedKeys(oldRoles) {
		if _, ok := nowRoles[rid]; ok {
			continue
		}
		st := oldRoles[rid]
		fr, err := event(gateway.BotEventRole, []any{b.community, rid, []uint64{st.allow, st.deny, st.position}, uint64(1)})
		if err != nil {
			return nil, err
		}
		if err := send(fr); err != nil {
			return nil, err
		}
	}
	if community.Owner != oldOwner {
		fr, err := event(gateway.BotEventCommunity, []any{b.community, community.Owner})
		if err != nil {
			return nil, err
		}
		if err := send(fr); err != nil {
			return nil, err
		}
	}
	var joined []id.ID
	for _, u := range sortedKeys(members) {
		old, was := oldMembers[u]
		if was && slices.Equal(old, members[u]) {
			continue
		}
		if !was {
			joined = append(joined, u)
		}
		fr, err := event(gateway.BotEventMember, []any{b.community, u, uint64(1), members[u]})
		if err != nil {
			return nil, err
		}
		if err := send(fr); err != nil {
			return nil, err
		}
	}
	for _, u := range sortedKeys(oldMembers) {
		if _, ok := members[u]; ok {
			continue
		}
		fr, err := event(gateway.BotEventMember, []any{b.community, u, uint64(0), []id.ID{}})
		if err != nil {
			return nil, err
		}
		if err := send(fr); err != nil {
			return nil, err
		}
	}
	b.mu.Lock()
	b.members, b.roles, b.owner = members, nowRoles, community.Owner
	b.mu.Unlock()
	return joined, nil
}

// botChannels sends the Bot-channel marks diff.
func (f *BotFeed) botChannels(ctx context.Context, b *feedBot) error {
	send := f.liveSender(b)
	rows, err := f.o.Repo.ListBotChannels(ctx, b.community)
	if err != nil {
		return err
	}
	now := map[id.ID]bool{}
	for _, r := range rows {
		now[r.ChannelID] = true
	}
	b.mu.Lock()
	old := b.marked
	b.mu.Unlock()
	for _, ch := range sortedKeys(now) {
		if old[ch] {
			continue
		}
		fr, err := event(gateway.BotEventBotChannel, []any{b.community, ch, uint64(1)})
		if err != nil {
			return err
		}
		if err := send(fr); err != nil {
			return err
		}
	}
	for _, ch := range sortedKeys(old) {
		if now[ch] {
			continue
		}
		fr, err := event(gateway.BotEventBotChannel, []any{b.community, ch, uint64(0)})
		if err != nil {
			return err
		}
		if err := send(fr); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.marked = now
	b.mu.Unlock()
	return nil
}

// channelGroup sends one group epoch when the group is a Bot channel's text group or a call the
// Bot's device is in.
func (f *BotFeed) channelGroup(b *feedBot, c ds.EpochChange) error {
	b.mu.Lock()
	marked := b.marked[c.TargetID]
	b.mu.Unlock()
	fed := (c.Kind == groupKindText && marked) || (c.Kind == groupKindCall && slices.Contains(c.Devices, b.device))
	if !fed {
		return nil
	}
	fr, err := event(gateway.BotEventChannelGroup, []any{b.community, c.TargetID, c.GroupID, c.Epoch})
	if err != nil {
		return err
	}
	return f.liveSender(b)(fr)
}

// snapshotChannelGroups sends, inside a snapshot, the channel_group of everything channelGroup feeds live: each marked
// Bot channel's open text group, and each open call group of the Bot's community that the Bot's device is in.
func (f *BotFeed) snapshotChannelGroups(ctx context.Context, b *feedBot, marked map[id.ID]bool, send func(gateway.Frame) error) error {
	emit := func(g store.GroupRow) error {
		fr, err := event(gateway.BotEventChannelGroup, []any{b.community, g.TargetID, g.GroupID, g.Epoch})
		if err != nil {
			return err
		}
		return send(fr)
	}
	for _, ch := range sortedKeys(marked) {
		groups, err := f.o.Repo.GroupsForTarget(ctx, ch, groupKindText)
		if err != nil {
			return err
		}
		for _, g := range groups {
			if g.ClosedAt != nil || g.CommunityID == nil || *g.CommunityID != b.community {
				continue
			}
			if err := emit(g); err != nil {
				return err
			}
		}
	}
	ids, err := f.o.Repo.GroupsForDevice(ctx, b.device)
	if err != nil {
		return err
	}
	slices.SortFunc(ids, compareIDs)
	for _, gid := range slices.Compact(ids) {
		g, err := f.o.Repo.GetGroup(ctx, gid)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if g.Kind != groupKindCall || g.ClosedAt != nil || g.CommunityID == nil || *g.CommunityID != b.community {
			continue
		}
		if err := emit(g); err != nil {
			return err
		}
	}
	return nil
}

// The group kinds the feed reads from an EpochChange (protocol/01 § Group kinds).
const (
	groupKindText uint8 = 0
	groupKindCall uint8 = 1
)

// deviceList sends the user's whole signed device-list history, from version 1, as one push. A user
// with no list, no usable account keys, more than protocol.MaxDeviceListPushRows versions or a push
// over BotFeedMaxDeviceListBytes is left out: logged, counted, and the feed goes on.
func (f *BotFeed) deviceList(ctx context.Context, b *feedBot, user id.ID, send func(gateway.Frame) error) error {
	push, reason, err := f.devicePush(ctx, user)
	if err != nil {
		return err
	}
	if reason != "" {
		if reason != "none" {
			f.o.Log.Warn("bot feed: a device list is left out", "user", user.String(), "reason", reason,
				"installation", b.installation.String())
			if f.o.Skipped != nil {
				f.o.Skipped(reason)
			}
		}
		return nil
	}
	fr, err := event(gateway.BotEventDeviceList, []any{user, push})
	if err != nil {
		return err
	}
	return send(fr)
}

// devicePush is the user's device_list_push body, or a reason it is left out ("none" for a user with
// no list or account, which is not an anomaly).
func (f *BotFeed) devicePush(ctx context.Context, user id.ID) ([]byte, string, error) {
	u, err := f.o.Repo.GetUser(ctx, user)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "none", nil
	}
	if err != nil {
		return nil, "", err
	}
	if u.DeletedAt != nil {
		return nil, "none", nil
	}
	rows, err := f.o.Repo.ListDeviceListsAfter(ctx, user, 0, protocol.MaxDeviceListPushRows+1)
	if err != nil {
		return nil, "", err
	}
	if len(rows) == 0 {
		return nil, "none", nil
	}
	if len(rows) > protocol.MaxDeviceListPushRows {
		return nil, "too_many_rows", nil
	}
	if len(u.UMKPub) != 32 || len(u.SSKPub) != 32 || len(u.SigUMKSSK) != 64 {
		return nil, "no_keys", nil
	}
	p := protocol.DeviceListPush{History: make([]protocol.DeviceListRow, 0, len(rows))}
	copy(p.UMK[:], u.UMKPub)
	copy(p.SSK[:], u.SSKPub)
	copy(p.SigUMKSSK[:], u.SigUMKSSK)
	size := 0
	for _, r := range rows {
		if len(r.SSKSignature) != 64 || len(r.PrevHash) != 32 {
			return nil, "invalid", nil
		}
		size += len(r.Blob)
		if size > BotFeedMaxDeviceListBytes {
			return nil, "too_large", nil
		}
		row := protocol.DeviceListRow{Version: r.Version, Blob: r.Blob}
		copy(row.SSKSignature[:], r.SSKSignature)
		copy(row.PrevHash[:], r.PrevHash)
		p.History = append(p.History, row)
	}
	body, err := protocol.EncodeDeviceListPush(p)
	if err != nil {
		if allZero32(p.UMK) || allZero32(p.SSK) {
			return nil, "no_keys", nil
		}
		return nil, "invalid", nil
	}
	if len(body) > BotFeedMaxDeviceListBytes {
		return nil, "too_large", nil
	}
	return body, "", nil
}

func allZero32(b [32]byte) bool { return b == [32]byte{} }

func compareIDs(a, b id.ID) int { return bytes.Compare(a[:], b[:]) }

// sortedKeys is a map's keys in id order, so a diff's events have a fixed order.
func sortedKeys[V any](m map[id.ID]V) []id.ID {
	out := make([]id.ID, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, compareIDs)
	return out
}

// channelOverwrites sends the diff of the Bot and voice channels' overwrites (task 15).
func (f *BotFeed) channelOverwrites(ctx context.Context, b *feedBot, send func(gateway.Frame) error) error {
	roles, err := f.o.Repo.ListRoles(ctx, b.community)
	if err != nil {
		return err
	}
	rows, err := f.o.Repo.ListBotChannels(ctx, b.community)
	if err != nil {
		return err
	}
	marked := map[id.ID]bool{}
	for _, r := range rows {
		marked[r.ChannelID] = true
	}
	return f.channelOverwritesOf(ctx, b, roles, marked, send)
}

// channelOverwritesOf sends, for every Bot channel (marked) and voice channel of the community, its overwrites when
// they differ from what the Bot was told, and deleted for a channel it was told of that is no longer one of them. It
// names no other channel. Role overwrites of the @everyone role (the first role at position 0) are target_kind 0.
func (f *BotFeed) channelOverwritesOf(ctx context.Context, b *feedBot, roles []store.RoleRow, marked map[id.ID]bool, send func(gateway.Frame) error) error {
	channels, err := f.o.Repo.ListChannels(ctx, b.community)
	if err != nil {
		return err
	}
	sorted := slices.Clone(roles)
	slices.SortFunc(sorted, byPositionThenID)
	var everyone id.ID
	if len(sorted) > 0 && sorted[0].Position == 0 {
		everyone = sorted[0].ID
	}
	b.mu.Lock()
	old := b.overwrites
	b.mu.Unlock()
	now := map[id.ID][]feedOverwrite{}
	for _, ch := range channels {
		if ch.DeletedAt != nil || !marked[ch.ID] && ch.Kind != ChannelVoice {
			continue
		}
		rows, err := f.o.Repo.ListOverwrites(ctx, ch.ID)
		if err != nil {
			return err
		}
		list := make([]feedOverwrite, 0, len(rows))
		for _, o := range rows {
			kind := uint64(2)
			if o.TargetKind == overwriteRole {
				kind = 1
				if o.TargetID == everyone {
					kind = 0
				}
			}
			list = append(list, feedOverwrite{kind: kind, target: o.TargetID, allow: o.Allow & uint64(PermAll), deny: o.Deny & uint64(PermAll)})
		}
		slices.SortFunc(list, func(x, y feedOverwrite) int {
			if c := cmpUint(x.kind, y.kind); c != 0 {
				return c
			}
			return compareIDs(x.target, y.target)
		})
		now[ch.ID] = list
	}
	for _, ch := range sortedKeys(now) {
		if was, ok := old[ch]; ok && slices.Equal(was, now[ch]) {
			continue
		}
		entries := make([]any, 0, len(now[ch]))
		for _, o := range now[ch] {
			entries = append(entries, []any{o.kind, o.target, o.allow, o.deny})
		}
		fr, err := event(gateway.BotEventChannelOverwrites, []any{b.community, ch, entries, uint64(0)})
		if err != nil {
			return err
		}
		if err := send(fr); err != nil {
			return err
		}
	}
	for _, ch := range sortedKeys(old) {
		if _, ok := now[ch]; ok {
			continue
		}
		fr, err := event(gateway.BotEventChannelOverwrites, []any{b.community, ch, []any{}, uint64(1)})
		if err != nil {
			return err
		}
		if err := send(fr); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.overwrites = now
	b.mu.Unlock()
	return nil
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// memberTimeouts sends each of users' timeout when it differs from what the Bot was told (task 15): the end in
// unix seconds, or 0 once it is cleared. A member who left is forgotten without an event (their member event says so).
func (f *BotFeed) memberTimeouts(ctx context.Context, b *feedBot, users []id.ID, inScope func(id.ID) bool, send func(gateway.Frame) error) error {
	users = slices.Clone(users)
	slices.SortFunc(users, compareIDs)
	now := time.Now().Unix()
	for _, u := range slices.Compact(users) {
		if !inScope(u) {
			b.mu.Lock()
			delete(b.timeouts, u)
			b.mu.Unlock()
			continue
		}
		var until uint64
		row, err := f.o.Repo.GetMemberTimeout(ctx, b.community, u, now)
		switch {
		case err == nil && row.Expires > 0:
			until = uint64(row.Expires)
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return err
		}
		b.mu.Lock()
		told := b.timeouts[u]
		b.mu.Unlock()
		if told == until {
			continue
		}
		fr, err := event(gateway.BotEventMemberTimeout, []any{b.community, u, until})
		if err != nil {
			return err
		}
		if err := send(fr); err != nil {
			return err
		}
		b.mu.Lock()
		if b.timeouts == nil {
			b.timeouts = map[id.ID]uint64{}
		}
		if until == 0 {
			delete(b.timeouts, u)
		} else {
			b.timeouts[u] = until
		}
		b.mu.Unlock()
	}
	return nil
}

package intake

import (
	"bytes"
	"cmp"
	"slices"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// Permission bits, mirrored from internal/api/perm.go (the Bot does not import the server). A catalog
// command's permission_bit is a mask of these; dillad refuses any other bit in a catalog.
const (
	PermViewChannel     uint64 = 1 << 0
	PermSendMessages    uint64 = 1 << 1
	PermManageMessages  uint64 = 1 << 2
	PermKickMembers     uint64 = 1 << 12
	PermBanMembers      uint64 = 1 << 13
	PermManageRoles     uint64 = 1 << 15
	PermManageCommunity uint64 = 1 << 16
	PermAdministrator   uint64 = 1 << 20
	PermModerateMembers uint64 = 1 << 24
	PermControlMusic    uint64 = 1 << 25
	// PermAll is every defined bit (bits 0..25), as dillad's PermAll: a role's stored bits are masked
	// with it, so a bit a newer dillad defines is never granted here by accident.
	PermAll uint64 = 1<<26 - 1
	// communityBits are dillad's: the permissions a channel overwrite may neither grant nor remove.
	communityBits = PermKickMembers | PermBanMembers | PermManageCommunity | PermManageRoles | 1<<17 | PermAdministrator |
		1<<23 | PermModerateMembers
)

// OverwriteKind is a channel overwrite's target in the feed's kind 9 (channel_overwrites).
type OverwriteKind uint8

const (
	OverwriteEveryone OverwriteKind = 0
	OverwriteRole     OverwriteKind = 1
	OverwriteMember   OverwriteKind = 2
)

// Overwrite is one entry of a channel's overwrites: @everyone (Target is the @everyone role), a role, or a
// member.
type Overwrite struct {
	Kind        OverwriteKind
	Target      id.ID
	Allow, Deny uint64
}

// authzChannel is one Bot or voice channel's overwrites: by role (the @everyone role included) and by member.
type authzChannel struct {
	roles   map[id.ID]Overwrite
	members map[id.ID]Overwrite
}

type authzRole struct {
	id                    id.ID
	allow, deny, position uint64
}

// authzState is what the feed told the Bot: the owner, the roles and each member's held roles.
type authzState struct {
	owner   id.ID
	roles   map[id.ID]authzRole
	members map[id.ID][]id.ID
	// sorted is roles by (position, id), rebuilt on a role change; everyone is the @everyone role:
	// the first role at position 0, as dillad's loadCommunitySnapshotBase picks it.
	sorted   []authzRole
	everyone id.ID
	// channels holds the overwrites of the Bot channels and voice channels (feed kind 9); a channel not here
	// is unknown, and only permission_bit 0 is allowed in it. timeouts is each timed-out member's end in unix
	// seconds (kind 10).
	channels map[id.ID]*authzChannel
	timeouts map[id.ID]uint64
}

func newAuthzState() *authzState {
	return &authzState{roles: map[id.ID]authzRole{}, members: map[id.ID][]id.ID{}, channels: map[id.ID]*authzChannel{}, timeouts: map[id.ID]uint64{}}
}

func (s *authzState) reindex() {
	s.sorted = s.sorted[:0]
	for _, r := range s.roles {
		s.sorted = append(s.sorted, r)
	}
	slices.SortFunc(s.sorted, func(a, b authzRole) int {
		if c := cmp.Compare(a.position, b.position); c != 0 {
			return c
		}
		return bytes.Compare(a.id[:], b.id[:])
	})
	s.everyone = id.ID{}
	if len(s.sorted) > 0 && s.sorted[0].position == 0 {
		s.everyone = s.sorted[0].id
	}
}

// Authz is the Bot's permission cache for one community, built only from the feed's pushed member, role
// and community events (Bot v2 Task 12 kinds 2, 3 and 8): no request is ever made to authorise a command.
// A snapshot is staged and replaces the state at its end, as the feed handler's own state does; a
// snapshot cut off before its end changes nothing. Until the first snapshot is applied the cache is not
// Ready and every command is answered "offline" (retryable), never "forbidden".
//
// The resolution is dillad's community-wide one (internal/api/perm.go Resolve): the owner holds every
// bit; otherwise @everyone and each held role apply in ascending (position, id) order, deny before
// allow; administrator is every bit; a member without view_channel holds none. Channel overwrites are
// in the feed for the Bot and voice channels (kind 9), so a command's permission_bit is checked in the
// channel it names, and member timeouts too (kind 10).
type Authz struct {
	community id.ID
	mu        sync.RWMutex
	live      *authzState
	staging   *authzState
	ready     bool
}

// NewAuthz is an empty, not-ready cache for community.
func NewAuthz(community id.ID) *Authz {
	return &Authz{community: community, live: newAuthzState()}
}

// Community is the community the cache belongs to.
func (a *Authz) Community() id.ID { return a.community }

func (a *Authz) target(snapshot bool) *authzState {
	if snapshot && a.staging != nil {
		return a.staging
	}
	return a.live
}

// SnapshotBegin starts staging a snapshot; whatever an earlier, cut-off one staged is dropped. Until it ends the cache
// is not ready (fix round 1, I-2): a new snapshot means the Bot was away, and the state it holds may predate changes
// made meanwhile, so commands are answered offline (retryable) rather than authorised against it.
func (a *Authz) SnapshotBegin() {
	a.mu.Lock()
	a.staging = newAuthzState()
	a.ready = false
	a.mu.Unlock()
}

// SnapshotDone replaces the state with the staged snapshot and makes the cache ready.
func (a *Authz) SnapshotDone() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.staging == nil {
		return
	}
	a.staging.reindex()
	a.live, a.staging, a.ready = a.staging, nil, true
}

// Member records a member event: joined with the role ids it holds, or gone (left, kicked, banned).
func (a *Authz) Member(user id.ID, joined bool, roles []id.ID, snapshot bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.target(snapshot)
	if joined {
		s.members[user] = append([]id.ID(nil), roles...)
	} else {
		delete(s.members, user)
	}
}

// Role records a role's allow, deny and position, or its deletion.
func (a *Authz) Role(role id.ID, allow, deny, position uint64, deleted, snapshot bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.target(snapshot)
	if deleted {
		delete(s.roles, role)
	} else {
		s.roles[role] = authzRole{id: role, allow: allow & PermAll, deny: deny & PermAll, position: position}
	}
	if s == a.live {
		s.reindex()
	}
}

// Owner records the community's owner (kind 8).
func (a *Authz) Owner(owner id.ID, snapshot bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.target(snapshot).owner = owner
}

// ChannelOverwrites records a Bot or voice channel's overwrites (kind 9), or forgets the channel (deleted:
// removed, or no longer a Bot or voice channel).
func (a *Authz) ChannelOverwrites(channel id.ID, overwrites []Overwrite, deleted, snapshot bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.target(snapshot)
	if deleted {
		delete(s.channels, channel)
		return
	}
	c := &authzChannel{roles: map[id.ID]Overwrite{}, members: map[id.ID]Overwrite{}}
	for _, o := range overwrites {
		o.Allow, o.Deny = o.Allow&PermAll, o.Deny&PermAll
		switch o.Kind {
		case OverwriteEveryone, OverwriteRole:
			c.roles[o.Target] = o
		case OverwriteMember:
			c.members[o.Target] = o
		}
	}
	s.channels[channel] = c
}

// MemberTimeout records a member's timeout end in unix seconds (kind 10); 0 clears it.
func (a *Authz) MemberTimeout(user id.ID, until uint64, snapshot bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.target(snapshot)
	if until == 0 {
		delete(s.timeouts, user)
	} else {
		s.timeouts[user] = until
	}
}

// TimedOut reports whether user's timeout runs at now.
func (a *Authz) TimedOut(user id.ID, now time.Time) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	until, ok := a.live.timeouts[user]
	if !ok {
		return false
	}
	secs := now.Unix()
	return secs < 0 || uint64(secs) < until
}

// Ready reports whether a snapshot has been applied.
func (a *Authz) Ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ready
}

// IsMember reports whether user is a current member.
func (a *Authz) IsMember(user id.ID) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.live.members[user]
	return ok
}

// Permissions is user's community-wide permission bits, 0 for a non-member.
func (a *Authz) Permissions(user id.ID) uint64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	bits, _ := a.resolveLocked(user)
	return bits
}

func (a *Authz) resolveLocked(user id.ID) (uint64, bool) {
	s := a.live
	held, member := s.members[user]
	if !member {
		return 0, false
	}
	if !s.owner.IsZero() && user == s.owner {
		return PermAll, true
	}
	var bits uint64
	for _, r := range s.sorted {
		if r.id != s.everyone && !slices.Contains(held, r.id) {
			continue
		}
		bits &^= r.deny
		bits |= r.allow
	}
	if bits&PermAdministrator != 0 {
		return PermAll, true
	}
	if bits&PermViewChannel == 0 {
		return 0, true
	}
	return bits, true
}

// resolveChannelLocked is dillad's resolveChannel: the community-wide bits, then the overwrites of
// @everyone and each held role in ascending (position, id) order, then the member's own, deny before allow;
// community-scoped bits restored from the base; administrator is everything, no view_channel nothing.
// (dillad's AFK-channel grant of view and connect is not mirrored: the feed does not name the AFK channel,
// so the Bot fails closed there for a non-zero bit a member holds only through that grant.)
func (a *Authz) resolveChannelLocked(user id.ID, c *authzChannel) uint64 {
	s := a.live
	base, _ := a.resolveLocked(user)
	if base == PermAll || base == 0 {
		return base
	}
	held := s.members[user]
	bits := base
	for _, r := range s.sorted {
		if r.id != s.everyone && !slices.Contains(held, r.id) {
			continue
		}
		if o, ok := c.roles[r.id]; ok {
			bits &^= o.Deny
			bits |= o.Allow
		}
	}
	if o, ok := c.members[user]; ok {
		bits &^= o.Deny
		bits |= o.Allow
	}
	bits = bits&^communityBits | base&communityBits
	if bits&PermAdministrator != 0 {
		return PermAll
	}
	if bits&PermViewChannel == 0 {
		return 0
	}
	return bits
}

// ChannelPermissions is user's permission bits in channel, from the feed state only (Task 21b fix round 1: the poll
// handlers ask it instead of dillad). A channel the feed told the Bot nothing about, or a non-member, holds nothing.
func (a *Authz) ChannelPermissions(user, channel id.ID) uint64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if _, member := a.resolveLocked(user); !member {
		return 0
	}
	c, ok := a.live.channels[channel]
	if !ok {
		return 0
	}
	return a.resolveChannelLocked(user, c)
}

// Allowed reports whether user, a current member of community, holds every bit of permissionBit (0:
// membership alone) in channel, from the feed state only. A non-zero bit in a channel the feed told the Bot
// nothing about is refused (fail closed). With a zero channel a non-zero bit is resolved community-wide only
// for a communityScoped operation; for any other it is refused, since the sender chooses the channel.
func (a *Authz) Allowed(community, channel, user id.ID, permissionBit uint64, communityScoped bool) bool {
	if community != a.community || permissionBit&^PermAll != 0 {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	bits, member := a.resolveLocked(user)
	if !member {
		return false
	}
	if permissionBit == 0 {
		return true
	}
	if channel.IsZero() && !communityScoped {
		return false
	}
	if !channel.IsZero() {
		c, ok := a.live.channels[channel]
		if !ok {
			return false
		}
		bits = a.resolveChannelLocked(user, c)
	}
	return bits&permissionBit == permissionBit
}

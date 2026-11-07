package intake

import (
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

func authzFixture(t *testing.T) (*Authz, id.ID, id.ID, id.ID) {
	t.Helper()
	community, everyone, mods := id.New(), id.New(), id.New()
	a := NewAuthz(community)
	a.SnapshotBegin()
	a.Owner(id.New(), true)
	a.Role(everyone, PermViewChannel|PermSendMessages, 0, 0, false, true)
	a.Role(mods, PermManageCommunity, 0, 5, false, true)
	a.SnapshotDone()
	return a, community, everyone, mods
}

func TestAuthzResolvesLikeDillad(t *testing.T) {
	a, community, everyone, mods := authzFixture(t)
	member, mod, outsider := id.New(), id.New(), id.New()
	a.Member(member, true, nil, false)
	a.Member(mod, true, []id.ID{mods}, false)
	if !a.Allowed(community, id.ID{}, member, 0, true) {
		t.Fatal("permission_bit 0 is every member's")
	}
	if a.Allowed(community, id.ID{}, outsider, 0, true) {
		t.Fatal("a non-member is refused even for permission_bit 0")
	}
	if a.Allowed(id.New(), id.ID{}, member, 0, true) {
		t.Fatal("another community is refused")
	}
	if a.Allowed(community, id.ID{}, member, PermManageCommunity, true) || !a.Allowed(community, id.ID{}, mod, PermManageCommunity, true) {
		t.Fatal("a held role grants its allow bits, and only to its holders")
	}
	// Deny before allow, in ascending position: a higher role's allow wins over a lower role's deny.
	a.Role(everyone, PermViewChannel, PermManageCommunity, 0, false, false)
	if !a.Allowed(community, id.ID{}, mod, PermManageCommunity, true) {
		t.Fatal("the higher role's allow is applied after the lower role's deny")
	}
	a.Role(mods, 0, PermManageCommunity, 5, false, false)
	if a.Allowed(community, id.ID{}, mod, PermManageCommunity, true) {
		t.Fatal("a held role's deny removes the bit")
	}
	// Administrator is everything; no view_channel is nothing.
	a.Role(mods, PermAdministrator, 0, 5, false, false)
	if !a.Allowed(community, id.ID{}, mod, PermControlMusic|PermBanMembers, true) {
		t.Fatal("administrator holds every bit")
	}
	a.Role(everyone, 0, PermViewChannel, 0, false, false)
	if a.Allowed(community, id.ID{}, member, PermSendMessages, true) {
		t.Fatal("a member without view_channel holds no bit")
	}
	if !a.Allowed(community, id.ID{}, member, 0, true) {
		t.Fatal("permission_bit 0 needs membership only")
	}
}

func TestAuthzOwnerHoldsEverything(t *testing.T) {
	community, owner := id.New(), id.New()
	a := NewAuthz(community)
	a.SnapshotBegin()
	a.Owner(owner, true)
	a.Member(owner, true, nil, true)
	a.SnapshotDone()
	if !a.Allowed(community, id.ID{}, owner, PermAdministrator|PermManageRoles, true) {
		t.Fatal("the owner holds every bit")
	}
}

func TestAuthzSnapshotReplacesAndACutSnapshotChangesNothing(t *testing.T) {
	a, community, _, mods := authzFixture(t)
	mod := id.New()
	a.Member(mod, true, []id.ID{mods}, false)
	if !a.Ready() || !a.Allowed(community, id.ID{}, mod, PermManageCommunity, true) {
		t.Fatal("ready after the first snapshot")
	}
	// A snapshot cut off before its end leaves the state untouched.
	a.SnapshotBegin()
	a.Member(id.New(), true, nil, true)
	if !a.Allowed(community, id.ID{}, mod, PermManageCommunity, true) {
		t.Fatal("a staged snapshot is not applied before its end")
	}
	// A later snapshot that omits the role and the member replaces the state.
	a.SnapshotBegin()
	a.Role(id.New(), PermViewChannel, 0, 0, false, true)
	a.SnapshotDone()
	if a.IsMember(mod) || a.Allowed(community, id.ID{}, mod, 0, true) {
		t.Fatal("a member the snapshot omits is gone")
	}
}

func TestAuthzLeaveAndRoleDeletion(t *testing.T) {
	a, community, _, mods := authzFixture(t)
	mod := id.New()
	a.Member(mod, true, []id.ID{mods}, false)
	a.Role(mods, 0, 0, 5, true, false)
	if a.Allowed(community, id.ID{}, mod, PermManageCommunity, true) {
		t.Fatal("a deleted role grants nothing")
	}
	a.Member(mod, false, nil, false)
	if a.Allowed(community, id.ID{}, mod, 0, true) {
		t.Fatal("a member who left is refused")
	}
}

func TestAuthzAllowedDoesNotAllocate(t *testing.T) {
	a, community, _, mods := authzFixture(t)
	mod := id.New()
	a.Member(mod, true, []id.ID{mods}, false)
	if n := testing.AllocsPerRun(100, func() { a.Allowed(community, id.ID{}, mod, PermManageCommunity, true) }); n != 0 {
		t.Fatalf("Allowed allocates %v times", n)
	}
	ch := id.New()
	a.ChannelOverwrites(ch, []Overwrite{{Kind: OverwriteRole, Target: mods, Allow: PermSendMessages}}, false, false)
	if n := testing.AllocsPerRun(100, func() { a.Allowed(community, ch, mod, PermSendMessages, false) }); n != 0 {
		t.Fatalf("Allowed in a channel allocates %v times", n)
	}
}

// Ruling (security review): a command is authorised in its channel, as dillad's ResolveChannel does: @everyone's
// overwrite, then the held roles' in position order, then the member's own; community-scoped bits are untouched.
func TestAuthzChannelDenyOverwriteRefusesInThatChannelOnly(t *testing.T) {
	a, community, everyone, mods := authzFixture(t)
	member, mod := id.New(), id.New()
	a.Member(member, true, nil, false)
	a.Member(mod, true, []id.ID{mods}, false)
	music, other := id.New(), id.New()
	a.ChannelOverwrites(music, []Overwrite{{Kind: OverwriteEveryone, Target: everyone, Deny: PermSendMessages}}, false, false)
	a.ChannelOverwrites(other, nil, false, false)
	if a.Allowed(community, music, member, PermSendMessages, false) || !a.Allowed(community, other, member, PermSendMessages, false) {
		t.Fatal("an @everyone deny refuses in its channel only")
	}
	if !a.Allowed(community, id.ID{}, member, PermSendMessages, true) {
		t.Fatal("community-wide is unchanged")
	}
	// A role's allow overwrite, applied after @everyone's deny, gives it back to the role's holders.
	a.ChannelOverwrites(music, []Overwrite{{Kind: OverwriteEveryone, Target: everyone, Deny: PermSendMessages}, {Kind: OverwriteRole, Target: mods, Allow: PermSendMessages}}, false, false)
	if !a.Allowed(community, music, mod, PermSendMessages, false) || a.Allowed(community, music, member, PermSendMessages, false) {
		t.Fatal("a held role's allow overwrite follows @everyone's deny")
	}
	// The member's own overwrite comes last.
	a.ChannelOverwrites(music, []Overwrite{{Kind: OverwriteRole, Target: mods, Allow: PermSendMessages}, {Kind: OverwriteMember, Target: mod, Deny: PermSendMessages}}, false, false)
	if a.Allowed(community, music, mod, PermSendMessages, false) {
		t.Fatal("the member's own deny wins")
	}
	// Community-scoped bits cannot be granted or removed by an overwrite; losing view_channel is losing everything.
	a.ChannelOverwrites(music, []Overwrite{{Kind: OverwriteMember, Target: mod, Deny: PermManageCommunity}}, false, false)
	if !a.Allowed(community, music, mod, PermManageCommunity, false) {
		t.Fatal("an overwrite removed a community-scoped bit")
	}
	a.ChannelOverwrites(music, []Overwrite{{Kind: OverwriteMember, Target: member, Deny: PermViewChannel}}, false, false)
	if a.Allowed(community, music, member, PermSendMessages, false) {
		t.Fatal("a member who cannot view the channel holds no bit in it")
	}
	// A deleted channel is unknown again.
	a.ChannelOverwrites(music, nil, true, false)
	if a.Allowed(community, music, mod, PermSendMessages, false) {
		t.Fatal("a deleted channel's state is gone")
	}
}

func TestAuthzUnknownChannelWithANonZeroBitIsRefused(t *testing.T) {
	a, community, _, _ := authzFixture(t)
	member := id.New()
	a.Member(member, true, nil, false)
	unknown := id.New()
	if a.Allowed(community, unknown, member, PermSendMessages, false) {
		t.Fatal("a channel the Bot holds no overwrite state for was allowed a non-zero bit")
	}
	if !a.Allowed(community, unknown, member, 0, false) {
		t.Fatal("permission_bit 0 needs membership only")
	}
}

func TestAuthzTimeoutsAndSnapshotStaging(t *testing.T) {
	a, community, _, _ := authzFixture(t)
	member := id.New()
	a.Member(member, true, nil, false)
	now := time.Unix(1_800_000_000, 0)
	a.MemberTimeout(member, uint64(now.Unix())+60, false)
	if !a.TimedOut(member, now) || a.TimedOut(member, now.Add(time.Minute)) {
		t.Fatal("a timeout holds until its end")
	}
	a.MemberTimeout(member, 0, false)
	if a.TimedOut(member, now) {
		t.Fatal("a cleared timeout")
	}
	// A snapshot replaces timeouts and channel overwrites with what it names.
	ch := id.New()
	a.ChannelOverwrites(ch, nil, false, false)
	a.MemberTimeout(member, uint64(now.Unix())+60, false)
	a.SnapshotBegin()
	a.Member(member, true, nil, true)
	a.Role(id.New(), PermViewChannel|PermSendMessages, 0, 0, false, true)
	a.SnapshotDone()
	if a.TimedOut(member, now) || a.Allowed(community, ch, member, PermSendMessages, false) {
		t.Fatal("the snapshot did not replace the timeouts and channels")
	}
}

// Review ruling: the sender picks channel_id, so a zero channel must not skip a channel deny. A non-zero bit with no
// channel holds only for a community-scoped operation.
func TestAuthzZeroChannelHoldsOnlyForCommunityScopedOperations(t *testing.T) {
	a, community, _, _ := authzFixture(t)
	member := id.New()
	a.Member(member, true, nil, false)
	if a.Allowed(community, id.ID{}, member, PermSendMessages, false) {
		t.Fatal("a channel-scoped bit was allowed with no channel")
	}
	if !a.Allowed(community, id.ID{}, member, PermSendMessages, true) {
		t.Fatal("a community-scoped bit without a channel")
	}
	if !a.Allowed(community, id.ID{}, member, 0, false) {
		t.Fatal("permission_bit 0 needs membership only")
	}
}

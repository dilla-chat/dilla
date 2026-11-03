package api_test

import (
	"bytes"
	"cmp"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// fixture builds a snapshot by hand so the resolver is tested as a pure
// function, with no database and no HTTP.
type roleSpec struct {
	name     string
	position uint64
	allow    api.Bits
	deny     api.Bits
	held     bool
}

// owSpec names a role overwrite by the POSITION of the role it targets, because
// the fixture mints role ids itself. It is a separate field rather than an
// overload of OverwriteRow.Allow: reusing Allow as a marker left the marker in
// the row as real permission bits.
type owSpec struct {
	kind    uint8  // 0 role, 1 user
	rolePos uint64 // kind 0 only: the position of the target role
	userID  id.ID  // kind 1 only
	allow   api.Bits
	deny    api.Bits
}

func snapshot(t *testing.T, owner, user id.ID, roles []roleSpec, ows []owSpec) api.Snapshot {
	t.Helper()
	s := api.Snapshot{Owner: owner, Held: map[id.ID]bool{}}
	byPos := map[uint64]id.ID{}
	for _, spec := range roles {
		r := store.RoleRow{
			ID: id.New(), Name: spec.name, Position: spec.position,
			Allow: uint64(spec.allow), Deny: uint64(spec.deny),
		}
		s.Roles = append(s.Roles, r)
		byPos[spec.position] = r.ID
		if spec.position == 0 && s.Everyone == (id.ID{}) {
			// The FIRST role declared at position 0 is @everyone: it is identified
			// by id, not by position, exactly as LoadSnapshot does. A later role
			// at position 0 is the "smuggled" case, which must not replace it.
			s.Everyone = r.ID
		}
		if spec.held {
			s.Held[r.ID] = true
		}
	}
	// Roles must be (position, id)-ordered before Resolve sees them; LoadSnapshot
	// sorts with this comparator and Resolve sorts defensively with the same one,
	// so the fixture uses it too and the invariant is stated in one place.
	slices.SortFunc(s.Roles, func(a, b store.RoleRow) int {
		if c := cmp.Compare(a.Position, b.Position); c != 0 {
			return c
		}
		return bytes.Compare(a.ID[:], b.ID[:])
	})
	for _, o := range ows {
		row := store.OverwriteRow{TargetKind: o.kind, Allow: uint64(o.allow), Deny: uint64(o.deny)}
		if o.kind == 0 {
			row.TargetID = byPos[o.rolePos]
		} else {
			row.TargetID = o.userID
		}
		s.Overwrite = append(s.Overwrite, row)
	}
	return s
}

func TestResolveTable(t *testing.T) {
	owner, user := id.New(), id.New()
	const (
		view  = api.PermViewChannel
		send  = api.PermSendMessages
		mm    = api.PermManageMessages
		pin   = api.PermPinMessages
		admin = api.PermAdministrator
		kick  = api.PermKickMembers
	)
	base := []roleSpec{{name: "@everyone", position: 0, allow: view | send}}

	cases := []struct {
		name  string
		who   id.ID
		roles []roleSpec
		ow    []owSpec
		want  api.Bits
	}{
		{"owner short-circuits everything", owner, nil, nil, api.PermAll},
		{"owner beats an explicit deny", owner,
			[]roleSpec{{name: "@everyone", position: 0, deny: api.PermAll}}, nil, api.PermAll},
		{"administrator short-circuits", user,
			append(base, roleSpec{name: "admin", position: 5, allow: admin, held: true}), nil, api.PermAll},
		// PermAdministrator is a COMMUNITY bit: it is in communityBits, so a
		// channel overwrite can neither grant nor remove it. A channel-scoped
		// administrator would be a channel-scoped escalation to PermAll.
		{"administrator cannot be granted by a channel overwrite", user, base,
			[]owSpec{{kind: 1, userID: user, allow: admin}}, view | send},
		{"base only", user, base, nil, view | send},
		{"unheld role does not apply", user,
			append(base, roleSpec{name: "mod", position: 5, allow: mm}), nil, view | send},
		{"held role adds", user,
			append(base, roleSpec{name: "mod", position: 5, allow: mm, held: true}), nil, view | send | mm},
		{"held role denies", user,
			append(base, roleSpec{name: "muted", position: 5, deny: send, held: true}), nil, view},
		{"higher role re-allows what a lower one denied", user,
			append(base,
				roleSpec{name: "muted", position: 5, deny: send, held: true},
				roleSpec{name: "mod", position: 6, allow: send, held: true}), nil, view | send},
		// Declaration order is deliberately the reverse of position order here:
		// the fixture sorts, so this case proves the resolver reads position and
		// not slice order.
		{"lower role cannot undo a higher allow", user,
			append(base,
				roleSpec{name: "mod", position: 6, allow: send, held: true},
				roleSpec{name: "muted", position: 5, deny: send, held: true}), nil, view | send},
		{"deny and allow in one role: allow wins", user,
			append(base, roleSpec{name: "odd", position: 5, allow: mm, deny: mm, held: true}), nil, view | send | mm},
		{"role overwrite denies", user,
			append(base, roleSpec{name: "mod", position: 5, allow: mm, held: true}),
			[]owSpec{{kind: 0, rolePos: 5, deny: mm}}, view | send},
		{"role overwrite allows", user,
			append(base, roleSpec{name: "mod", position: 5, held: true}),
			[]owSpec{{kind: 0, rolePos: 5, allow: mm}}, view | send | mm},
		{"user overwrite beats every role overwrite", user,
			append(base, roleSpec{name: "mod", position: 5, allow: mm, held: true}),
			[]owSpec{
				{kind: 0, rolePos: 5, deny: mm},
				{kind: 1, userID: user, allow: mm},
			}, view | send | mm},
		{"user overwrite denies what a role allowed", user,
			append(base, roleSpec{name: "mod", position: 5, allow: mm | pin, held: true}),
			[]owSpec{{kind: 1, userID: user, deny: pin}}, view | send | mm},
		{"an overwrite for another user is ignored", user,
			base, []owSpec{{kind: 1, userID: id.New(), deny: send}}, view | send},
		{"no view means no bits at all", user,
			base, []owSpec{{kind: 1, userID: user, deny: view}}, 0},
		{"no view in the base means no bits", user,
			[]roleSpec{{name: "@everyone", position: 0, allow: send}}, nil, 0},
		{"unknown bits in the row are masked off", user,
			[]roleSpec{{name: "@everyone", position: 0, allow: view | send | api.Bits(1)<<40}}, nil, view | send},
		{"kick is a community bit and survives a channel overwrite", user,
			append(base, roleSpec{name: "mod", position: 5, allow: kick, held: true}),
			[]owSpec{{kind: 1, userID: user, deny: kick}}, view | send | kick},
		{"a role at position 0 that is not @everyone is not implicitly held", user,
			append(base, roleSpec{name: "smuggled", position: 0, allow: admin}), nil, view | send},
		{"equal positions break on role id, deterministically", user,
			append(base,
				roleSpec{name: "a", position: 5, allow: mm, held: true},
				roleSpec{name: "b", position: 5, deny: mm, held: true}), nil, 0 /* filled below */},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot(t, owner, user, tc.roles, tc.ow)
			got := s.ResolveChannel(tc.who)
			if tc.name == "equal positions break on role id, deterministically" {
				// Whichever of the two ids sorts last wins; assert only that the
				// answer is stable across repeated resolutions.
				for i := 0; i < 8; i++ {
					if s.ResolveChannel(tc.who) != got {
						t.Fatal("ResolveChannel is not deterministic for equal positions")
					}
				}
				return
			}
			if got != tc.want {
				t.Fatalf("ResolveChannel = %#x, want %#x", got, tc.want)
			}
		})
	}
}

func TestKickIsNotChannelScoped(t *testing.T) {
	owner, user := id.New(), id.New()
	s := snapshot(t, owner, user, []roleSpec{
		{name: "@everyone", position: 0, allow: api.PermViewChannel},
		{name: "mod", position: 5, allow: api.PermBanMembers, held: true},
	}, []owSpec{{kind: 1, userID: user, deny: api.PermBanMembers}})
	if !s.Resolve(user).Has(api.PermBanMembers) {
		t.Fatal("Resolve dropped a community bit")
	}
	if !s.ResolveChannel(user).Has(api.PermBanMembers) {
		t.Fatal("a channel overwrite must not remove a community-scoped bit")
	}
}

func TestModerateMembersIsCommunityScopedAndOwnerHasIt(t *testing.T) {
	owner, user := id.New(), id.New()
	const moderateMembers api.Bits = 1 << 24
	if !api.PermAll.Has(moderateMembers) {
		t.Fatal("owner does not hold moderate_members")
	}
	s := snapshot(t, owner, user, []roleSpec{
		{name: "@everyone", position: 0, allow: api.PermViewChannel},
		{name: "mod", position: 5, allow: moderateMembers, held: true},
	}, []owSpec{
		{kind: 1, userID: user, deny: moderateMembers},
	})
	if !s.Resolve(user).Has(moderateMembers) {
		t.Fatal("role did not grant moderate_members community-wide")
	}
	if !s.ResolveChannel(user).Has(moderateMembers) {
		t.Fatal("channel overwrite removed moderate_members")
	}

	withoutRole := snapshot(t, owner, user, []roleSpec{
		{name: "@everyone", position: 0, allow: api.PermViewChannel},
	}, []owSpec{{kind: 1, userID: user, allow: moderateMembers}})
	if withoutRole.ResolveChannel(user).Has(moderateMembers) {
		t.Fatal("channel overwrite granted moderate_members")
	}
}

func TestBotPermissionsClampPreexistingRoleAndOverwriteModerationGrants(t *testing.T) {
	owner, user := id.New(), id.New()
	s := snapshot(t, owner, user, []roleSpec{
		{name: "@everyone", position: 0, allow: api.PermAll},
		{name: "older moderator", position: 3, allow: api.PermAll, held: true},
	}, []owSpec{{kind: 1, userID: user, allow: api.PermAll}})
	s.Bot = true
	if got := s.ResolveChannel(user); got.Has(api.PermAdministrator | api.PermManageRoles | api.PermManageCommunity) {
		t.Fatalf("bot inherited administrator/community management: %b", got)
	}
	for _, bit := range []api.Bits{api.PermManageMessages, api.PermKickMembers, api.PermBanMembers, api.PermModerateMembers} {
		if s.ResolveChannel(user).Has(bit) {
			t.Errorf("joining bot inherited unapproved moderation bit %b", bit)
		}
	}
	s.BotModerator = api.PermManageMessages
	got := s.ResolveChannel(user)
	if !got.Has(api.PermManageMessages) {
		t.Fatal("Ready bot with approved removal capability lacks manage_messages")
	}
	for _, bit := range []api.Bits{api.PermKickMembers, api.PermBanMembers, api.PermModerateMembers, api.PermAdministrator, api.PermManageRoles, api.PermManageCommunity} {
		if got.Has(bit) {
			t.Errorf("bot with removal only inherited unapproved bit %b", bit)
		}
	}
}

func TestPermissionBitsFitSigned64(t *testing.T) {
	if api.PermAll >= 1<<62 {
		t.Fatalf("PermAll = %#x reaches bit 62 or above; Postgres stores allow/deny in a signed BIGINT", api.PermAll)
	}
}

func BenchmarkResolveChannel(b *testing.B) {
	owner, user := id.New(), id.New()
	roles := make([]store.RoleRow, 0, 32)
	held := map[id.ID]bool{}
	for i := range 32 {
		r := store.RoleRow{ID: id.New(), Position: uint64(i), Allow: uint64(api.PermSendMessages)}
		roles = append(roles, r)
		if i%2 == 0 {
			held[r.ID] = true
		}
	}
	s := api.Snapshot{Owner: owner, Roles: roles, Everyone: roles[0].ID, Held: held,
		Overwrite: []store.OverwriteRow{{TargetKind: 1, TargetID: user, Allow: uint64(api.PermPinMessages)}}}
	b.ReportAllocs()
	for b.Loop() {
		_ = s.ResolveChannel(user)
	}
}

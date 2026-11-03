package api_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Plan 2 task 3 step 8: the owner checks of tasks 1 and 2 become resolver calls.
// A member holding the right bit passes where only the owner did, and a member
// whose view of a channel an overwrite removed no longer sees it.

func TestManageChannelsIsARoleNotOwnership(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	member, memberTok := e.NewUser("builder")
	joinCommunity(t, e, cid, memberTok)

	// Without the bit: 403 on create, patch and delete.
	if _, _, status := newChannel(t, e, cid, memberTok, 0, 1, 0, "nope"); status != http.StatusForbidden {
		t.Fatalf("create without PermManageChannels = %d, want 403", status)
	}
	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 1, 0, "general")
	if status, _ := e.Do(http.MethodPatch, "/v1/channels/"+ch.String(), memberTok,
		[]any{"renamed", nil, nil, nil, nil, nil, nil}); status != http.StatusForbidden {
		t.Fatalf("patch without PermManageChannels = %d, want 403", status)
	}

	builders := createRole(t, e, cid, ownerTok, "builders", 10, api.PermViewChannel|api.PermManageChannels)
	grant(t, e, cid, ownerTok, member, builders, http.StatusNoContent)

	if _, _, status := newChannel(t, e, cid, memberTok, 0, 1, 0, "built"); status != http.StatusCreated {
		t.Fatalf("create with PermManageChannels = %d, want 201", status)
	}
	if status, body := e.Do(http.MethodPatch, "/v1/channels/"+ch.String(), memberTok,
		[]any{"renamed", nil, nil, nil, nil, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("patch with PermManageChannels = %d (%x)", status, body)
	}

	// A channel overwrite can take PermManageChannels away in one channel.
	if status, _ := e.Do(http.MethodPut,
		"/v1/channels/"+ch.String()+"/overwrites/1/"+member.String(), ownerTok,
		[]any{uint64(0), uint64(api.PermManageChannels)}); status != http.StatusNoContent {
		t.Fatal("PUT overwrite failed")
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/channels/"+ch.String(), memberTok, nil); status != http.StatusForbidden {
		t.Fatalf("delete with PermManageChannels denied here = %d, want 403", status)
	}
}

func TestAnOverwriteThatRemovesViewHidesTheChannel(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "staff-only")
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)

	if status, _ := e.Do(http.MethodGet, "/v1/channels/"+ch.String(), memberTok, nil); status != http.StatusOK {
		t.Fatalf("GET before the overwrite = %d, want 200", status)
	}
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	everyone := roles[0].ID
	if status, body := e.Do(http.MethodPut,
		"/v1/channels/"+ch.String()+"/overwrites/0/"+everyone.String(), ownerTok,
		[]any{uint64(0), uint64(api.PermViewChannel)}); status != http.StatusNoContent {
		t.Fatalf("PUT @everyone overwrite = %d (%x)", status, body)
	}
	// Not 403: a channel the caller cannot view does not exist for them.
	status, body := e.Do(http.MethodGet, "/v1/channels/"+ch.String(), memberTok, nil)
	if status != http.StatusNotFound || e.ErrCode(body) != "E_NOT_FOUND" {
		t.Fatalf("GET after the overwrite = %d (%x), want 404", status, body)
	}
	// The owner still sees it.
	if status, _ := e.Do(http.MethodGet, "/v1/channels/"+ch.String(), ownerTok, nil); status != http.StatusOK {
		t.Fatalf("owner GET = %d, want 200", status)
	}
	// A user overwrite re-opens it for one member.
	if status, _ := e.Do(http.MethodPut,
		"/v1/channels/"+ch.String()+"/overwrites/1/"+member.String(), ownerTok,
		[]any{uint64(api.PermViewChannel), uint64(0)}); status != http.StatusNoContent {
		t.Fatal("PUT user overwrite failed")
	}
	if status, _ := e.Do(http.MethodGet, "/v1/channels/"+ch.String(), memberTok, nil); status != http.StatusOK {
		t.Fatalf("GET with a user overwrite = %d, want 200", status)
	}
}

func TestManageCommunityMayPatchButNotDelete(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	member, memberTok := e.NewUser("steward")
	joinCommunity(t, e, cid, memberTok)
	base := "/v1/communities/" + cid.String()

	if status, _ := e.Do(http.MethodPatch, base, memberTok, []any{"x", nil, nil, nil}); status != http.StatusForbidden {
		t.Fatalf("PATCH without PermManageCommunity = %d, want 403", status)
	}
	stewards := createRole(t, e, cid, ownerTok, "stewards", 10, api.PermViewChannel|api.PermManageCommunity)
	grant(t, e, cid, ownerTok, member, stewards, http.StatusNoContent)
	if status, body := e.Do(http.MethodPatch, base, memberTok, []any{"renamed", nil, nil, nil}); status != http.StatusOK {
		t.Fatalf("PATCH with PermManageCommunity = %d (%x)", status, body)
	}
	if status, _ := e.Do(http.MethodDelete, base, memberTok, nil); status != http.StatusForbidden {
		t.Fatalf("DELETE by a non-owner = %d, want 403", status)
	}
}

// The overwrite routes refuse what a role create refuses, and a few things of
// their own.
func TestOverwriteRouteRefusals(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 1, 0, "general")
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	staff := createRole(t, e, cid, ownerTok, "staff", 10, api.PermViewChannel|api.PermManageRoles)
	grant(t, e, cid, ownerTok, member, staff, http.StatusNoContent)
	path := func(kind string, target id.ID) string {
		return "/v1/channels/" + ch.String() + "/overwrites/" + kind + "/" + target.String()
	}
	stranger, _ := e.NewUser("stranger")
	otherCid := createCommunity(t, e, ownerTok)
	foreignRole := createRole(t, e, otherCid, ownerTok, "foreign", 3, api.PermViewChannel)
	plain, plainTok := e.NewUser("plain")
	joinCommunity(t, e, cid, plainTok)
	com, err := e.Repo.GetCommunity(t.Context(), cid)
	if err != nil {
		t.Fatalf("GetCommunity: %v", err)
	}

	for _, c := range []struct {
		name   string
		tok    string
		path   string
		body   []any
		status int
		code   string
	}{
		{"kind 2", ownerTok, path("2", member), []any{uint64(0), uint64(0)}, 400, "E_INVALID_REQUEST"},
		{"the same bit allowed and denied", ownerTok, path("1", member),
			[]any{uint64(api.PermSendMessages), uint64(api.PermSendMessages)}, 400, "E_INVALID_REQUEST"},
		{"an undefined bit", ownerTok, path("1", member), []any{uint64(1) << 40, uint64(0)}, 400, "E_INVALID_REQUEST"},
		{"a community-wide bit", ownerTok, path("1", member),
			[]any{uint64(0), uint64(api.PermKickMembers)}, 400, "E_INVALID_REQUEST"},
		{"a user who is not a member", ownerTok, path("1", stranger), []any{uint64(0), uint64(0)}, 404, "E_NOT_FOUND"},
		{"a role of another community", ownerTok, path("0", foreignRole), []any{uint64(0), uint64(0)}, 404, "E_NOT_FOUND"},
		{"a bit the staffer does not hold", memberTok, path("1", plain),
			[]any{uint64(api.PermManageMessages), uint64(0)}, 403, "E_FORBIDDEN"},
		// The rank rule: only a role, or a member whose highest role is, strictly
		// below the actor's highest.
		{"the staffer's own role", memberTok, path("0", staff), []any{uint64(0), uint64(0)}, 403, "E_FORBIDDEN"},
		{"the staffer themself", memberTok, path("1", member), []any{uint64(0), uint64(0)}, 403, "E_FORBIDDEN"},
		{"the owner", memberTok, path("1", com.Owner),
			[]any{uint64(0), uint64(api.PermSendMessages)}, 403, "E_FORBIDDEN"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := e.Do(http.MethodPut, c.path, c.tok, c.body)
			if status != c.status {
				t.Fatalf("= %d (%x), want %d %s", status, body, c.status, c.code)
			}
			if code := e.ErrCode(body); code != c.code {
				t.Fatalf("code = %s, want %s", code, c.code)
			}
		})
	}
	// A plain member (no PermManageRoles) is 403; the staffer, within their own
	// bits and below their rank, succeeds; deleting what is not there is 404.
	if status, _ := e.Do(http.MethodPut, path("1", plain), plainTok, []any{uint64(0), uint64(0)}); status != http.StatusForbidden {
		t.Fatalf("a member without PermManageRoles = %d, want 403", status)
	}
	if status, body := e.Do(http.MethodPut, path("1", plain), memberTok,
		[]any{uint64(0), uint64(api.PermSendMessages)}); status != http.StatusNoContent {
		t.Fatalf("the staffer within their own bits = %d (%x)", status, body)
	}
	if status, _ := e.Do(http.MethodDelete, path("1", member), ownerTok, nil); status != http.StatusNotFound {
		t.Fatalf("deleting an absent overwrite = %d, want 404", status)
	}
}

func TestRoleDeleteAndRevoke(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	staff := createRole(t, e, cid, ownerTok, "staff", 10, api.PermViewChannel|api.PermManageRoles)
	peer := createRole(t, e, cid, ownerTok, "peer", 10, api.PermViewChannel)
	low := createRole(t, e, cid, ownerTok, "low", 5, api.PermAddReactions)
	grant(t, e, cid, ownerTok, member, staff, http.StatusNoContent)
	grant(t, e, cid, ownerTok, member, low, http.StatusNoContent)

	roles, _ := e.Repo.ListRoles(t.Context(), cid)
	everyone := roles[0].ID
	if status, _ := e.Do(http.MethodDelete, "/v1/roles/"+everyone.String(), ownerTok, nil); status != http.StatusBadRequest {
		t.Fatalf("deleting @everyone = %d, want 400", status)
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/roles/"+peer.String(), memberTok, nil); status != http.StatusForbidden {
		t.Fatalf("deleting a role at the staffer's own position = %d, want 403", status)
	}
	// Revoke is the mirror of grant: the staffer may revoke the low role.
	revoke := "/v1/communities/" + cid.String() + "/members/" + member.String() + "/roles/" + low.String()
	if status, _ := e.Do(http.MethodDelete, revoke, memberTok, nil); status != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", status)
	}
	if status, _ := e.Do(http.MethodDelete, revoke, memberTok, nil); status != http.StatusNotFound {
		t.Fatalf("a second revoke = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodDelete, "/v1/roles/"+low.String(), memberTok, nil); status != http.StatusNoContent {
		t.Fatalf("deleting a role below the staffer = %d, want 204", status)
	}
	if _, err := e.Repo.GetRole(t.Context(), low); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRole after delete = %v", err)
	}
	// A role another staffer placed below this one, but carrying a bit this one
	// does not hold, is not theirs to hand out.
	loaded := createRole(t, e, cid, ownerTok, "loaded", 4, api.PermBanMembers)
	grant(t, e, cid, memberTok, member, loaded, http.StatusForbidden)
}

// invariant 4's Add clause: the delivery service's ACL is the resolver.
func TestResolverACLAnswersInvariantFour(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	ctx := context.Background()
	text, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
	voice, _, _ := newChannel(t, e, cid, ownerTok, 1, 0, 0, "call")
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	outsider, _ := e.NewUser("outsider")

	group := func(kind uint8, community *id.ID, target id.ID) id.ID {
		g := store.GroupRow{
			GroupID: id.New(), Binding: []byte{0x80}, Kind: kind, CommunityID: community, TargetID: target,
			Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1,
			PolicyVersion: 1, Created: 1,
		}
		if err := e.Repo.CreateGroup(ctx, g); err != nil {
			t.Fatalf("CreateGroup: %v", err)
		}
		return g.GroupID
	}
	textGroup := group(0, &cid, text)
	callGroup := group(1, &cid, voice)
	dmGroup := group(0, nil, id.New())

	acl := api.ResolverACL{Repo: e.Repo}
	var _ ds.ACL = acl
	eligible := func(g, u id.ID) bool {
		t.Helper()
		ok, err := acl.Eligible(ctx, g, u)
		if err != nil {
			t.Fatalf("Eligible: %v", err)
		}
		return ok
	}
	if !eligible(textGroup, member) || !eligible(callGroup, member) {
		t.Fatal("a member who may view the channels was refused")
	}
	if eligible(textGroup, outsider) || eligible(callGroup, outsider) {
		t.Fatal("a non-member was admitted")
	}
	if eligible(id.New(), member) {
		t.Fatal("an unknown group admitted someone")
	}
	// A DM-shaped group whose target is no DM channel keeps Plan 1's rule: only a
	// user already in it. (A DM channel's participants: TestDMParticipantsMayRegisterAndBeAdded.)
	if eligible(dmGroup, member) {
		t.Fatal("a DM group admitted a user who is not in it")
	}

	// Take Connect away in the voice channel and View away in the text channel.
	put := func(ch id.ID, deny api.Bits) {
		if status, body := e.Do(http.MethodPut, "/v1/channels/"+ch.String()+"/overwrites/1/"+member.String(),
			ownerTok, []any{uint64(0), uint64(deny)}); status != http.StatusNoContent {
			t.Fatalf("PUT overwrite = %d (%x)", status, body)
		}
	}
	put(voice, api.PermConnect)
	put(text, api.PermViewChannel)
	if eligible(textGroup, member) {
		t.Fatal("a member who cannot view the channel is eligible for its text group")
	}
	if eligible(callGroup, member) {
		t.Fatal("a member who cannot connect is eligible for the call group")
	}
	// The owner is always eligible.
	owner, err := e.Repo.GetCommunity(ctx, cid)
	if err != nil {
		t.Fatalf("GetCommunity: %v", err)
	}
	if !eligible(textGroup, owner.Owner) {
		t.Fatal("the owner was refused")
	}
}

// The registration ACL reads the resolver too: a member who cannot view a
// channel may not register its group.
func TestMayRegisterNeedsViewOfTheChannel(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	ctx := context.Background()
	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	src := api.StructureChannels{Repo: e.Repo}
	if err := src.MayRegister(ctx, member, binding(&cid, ch, 0)); err != nil {
		t.Fatalf("a member who may view the channel was refused: %v", err)
	}
	if status, _ := e.Do(http.MethodPut, "/v1/channels/"+ch.String()+"/overwrites/1/"+member.String(),
		ownerTok, []any{uint64(0), uint64(api.PermViewChannel)}); status != http.StatusNoContent {
		t.Fatal("PUT overwrite failed")
	}
	if err := src.MayRegister(ctx, member, binding(&cid, ch, 0)); !errors.Is(err, ds.ErrNotEligible) {
		t.Fatalf("a member who cannot view the channel = %v, want ds.ErrNotEligible", err)
	}
}

// A community call group is bound to its voice channel (protocol/01 dilla_binding,
// R9: target_id is the channel id; the call id is a companion column). When that
// channel is gone the group is refused exactly as a text group is: falling back
// to community-wide bits would drop the channel's overwrites and hand a deleted
// PRIVATE voice channel's call group, its GroupInfo and its tree to every member.
func TestResolverACLRefusesACallGroupWhoseChannelIsGone(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	ctx := context.Background()
	voice, _, _ := newChannel(t, e, cid, ownerTok, 1, 0, 0, "staff-call")
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)

	// Private: @everyone may not view the voice channel.
	roles, err := e.Repo.ListRoles(ctx, cid)
	if err != nil || len(roles) == 0 {
		t.Fatalf("ListRoles: %v", err)
	}
	everyone := roles[0].ID
	if status, body := e.Do(http.MethodPut, "/v1/channels/"+voice.String()+"/overwrites/0/"+everyone.String(),
		ownerTok, []any{uint64(0), uint64(api.PermViewChannel)}); status != http.StatusNoContent {
		t.Fatalf("PUT @everyone overwrite = %d (%x)", status, body)
	}
	g := store.GroupRow{
		GroupID: id.New(), Binding: []byte{0x80}, Kind: 1, CommunityID: &cid, TargetID: voice,
		Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1,
		PolicyVersion: 1, Created: 1,
	}
	if err := e.Repo.CreateGroup(ctx, g); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	// The member holds view|connect community-wide through @everyone.
	snap, err := api.LoadSnapshot(ctx, e.Repo, cid, member, nil)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if !snap.Resolve(member).Has(api.PermViewChannel | api.PermConnect) {
		t.Fatal("fixture: the member must hold view|connect community-wide")
	}

	acl := api.ResolverACL{Repo: e.Repo}
	eligible := func() bool {
		t.Helper()
		ok, err := acl.Eligible(ctx, g.GroupID, member)
		if err != nil {
			t.Fatalf("Eligible: %v", err)
		}
		return ok
	}
	if eligible() {
		t.Fatal("a member the private voice channel hides is eligible for its call group")
	}
	if status, body := e.Do(http.MethodDelete, "/v1/channels/"+voice.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE channel = %d (%x)", status, body)
	}
	if eligible() {
		t.Fatal("deleting the private voice channel made its call group eligible community-wide")
	}
	// The registration ACL agrees: a community call group names a live voice channel.
	src := api.StructureChannels{Repo: e.Repo}
	if err := src.MayRegister(ctx, member, binding(&cid, voice, 1)); !errors.Is(err, ds.ErrBindingTarget) {
		t.Fatalf("registering a call group on the deleted channel = %v, want ds.ErrBindingTarget", err)
	}
}

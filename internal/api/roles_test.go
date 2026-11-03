package api_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestARoleCannotBeGrantedAboveTheGrantersHighest(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)

	// The owner makes a "staff" role at position 10 that may manage roles, and
	// an "admin" role at position 50 that may not be handed out by staff.
	staff := createRole(t, e, cid, ownerTok, "staff", 10, api.PermViewChannel|api.PermManageRoles)
	high := createRole(t, e, cid, ownerTok, "high", 50, api.PermManageMessages)

	member, memberTok := e.NewUser("staffer")
	joinCommunity(t, e, cid, memberTok)
	grant(t, e, cid, ownerTok, member, staff, http.StatusNoContent)

	// The staffer may grant a role below their own highest…
	low := createRole(t, e, cid, ownerTok, "low", 5, api.PermAddReactions)
	victim, victimTok := e.NewUser("victim")
	joinCommunity(t, e, cid, victimTok)
	grant(t, e, cid, memberTok, victim, low, http.StatusNoContent)

	// …but not one at or above it.
	grant(t, e, cid, memberTok, victim, high, http.StatusForbidden)
	grant(t, e, cid, memberTok, victim, staff, http.StatusForbidden)
}

func TestManualRoleRoutesPreserveOtherGrantSources(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	role := createRole(t, e, cid, ownerTok, "member", 5, api.PermViewChannel)
	target, targetTok := e.NewUser("target")
	joinCommunity(t, e, cid, targetTok)
	path := "/v1/communities/" + cid.String() + "/members/" + target.String() + "/roles/" + role.String()

	if err := e.Repo.AddMemberRoleGrant(t.Context(), cid, target, role, store.RoleGrantSelf); err != nil {
		t.Fatalf("add self grant: %v", err)
	}
	if status, body := e.Do(http.MethodPut, path, ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("manual grant = %d (%x), want 204", status, body)
	}
	if err := e.Repo.RemoveMemberRoleGrant(t.Context(), cid, target, role, store.RoleGrantSelf); err != nil {
		t.Fatalf("remove self grant: %v", err)
	}
	held, err := e.Repo.ListMemberRoles(t.Context(), cid, target)
	if err != nil || !slices.Contains(held, role) {
		t.Fatalf("manual route grant after self removal = %v, %v; want role retained", held, err)
	}

	if err := e.Repo.AddMemberRoleGrant(t.Context(), cid, target, role, store.RoleGrantSelf); err != nil {
		t.Fatalf("add self grant again: %v", err)
	}
	if err := e.Repo.AddMemberRoleGrant(t.Context(), cid, target, role, store.RoleGrantReaction); err != nil {
		t.Fatalf("add reaction grant: %v", err)
	}
	if status, body := e.Do(http.MethodDelete, path, ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("manual revoke = %d (%x), want 204", status, body)
	}
	held, err = e.Repo.ListMemberRoles(t.Context(), cid, target)
	if err != nil || slices.Contains(held, role) {
		t.Fatalf("all sources after manual revoke = %v, %v; want role absent", held, err)
	}
}

func TestOverwriteRoutesRoundTrip(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 1, 0, "general")
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)

	path := "/v1/channels/" + ch.String() + "/overwrites/1/" + member.String()
	if status, _ := e.Do(http.MethodPut, path, ownerTok,
		[]any{uint64(0), uint64(api.PermSendMessages)}); status != http.StatusNoContent {
		t.Fatal("PUT overwrite failed")
	}
	ow, err := e.Repo.ListOverwrites(t.Context(), ch)
	if err != nil {
		t.Fatalf("ListOverwrites: %v", err)
	}
	if len(ow) != 1 || ow[0].TargetKind != 1 || ow[0].TargetID != member ||
		ow[0].Deny != uint64(api.PermSendMessages) {
		t.Fatalf("overwrite = %+v", ow)
	}

	got, err := api.NewResolver(e.Repo).Resolve(t.Context(), member, mustChannel(t, e, ch))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Has(api.PermSendMessages) {
		t.Fatal("the deny overwrite did not take effect")
	}

	if status, _ := e.Do(http.MethodDelete, path, ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE overwrite failed")
	}
	if ow, _ := e.Repo.ListOverwrites(t.Context(), ch); len(ow) != 0 {
		t.Fatalf("overwrite survived the delete: %+v", ow)
	}
	for _, body := range [][2]uint64{{uint64(api.PermModerateMembers), 0}, {0, uint64(api.PermModerateMembers)}} {
		if status, reply := e.Do(http.MethodPut, path, ownerTok, []any{body[0], body[1]}); status != http.StatusBadRequest {
			t.Fatalf("PUT moderate_members overwrite %x/%x = %d (%x), want 400", body[0], body[1], status, reply)
		}
	}
	if ow, _ := e.Repo.ListOverwrites(t.Context(), ch); len(ow) != 0 {
		t.Fatalf("refused community-bit overwrites were stored: %+v", ow)
	}
}

func TestModerateMembersRoleHTTPRoundTrip(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	const want = api.PermModerateMembers
	roleID := createRole(t, e, cid, ownerTok, "moderator", 5, want)
	type roleRow struct {
		_           struct{} `cbor:",toarray"`
		ID          id.ID
		Name        string
		Color       uint64
		Position    uint64
		Allow       uint64
		Deny        uint64
		Hoist       uint64
		Mentionable uint64
	}
	status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/roles", ownerTok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET roles = %d (%x)", status, body)
	}
	var rows []roleRow
	mustUnmarshal(t, body, &rows)
	for _, row := range rows {
		if row.ID == roleID {
			if row.Allow != uint64(want) {
				t.Fatalf("role allow = %#x, want %#x", row.Allow, uint64(want))
			}
			return
		}
	}
	t.Fatalf("GET roles omitted created role %s", roleID)
}

// The two escalation paths a position bound alone does not close: a staffer who
// may manage roles must not be able to MINT authority they do not hold, nor to
// hand it to the whole community by parking a role at position 0.
func TestAStafferCannotMintARoleCarryingBitsTheyDoNotHold(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	staff := createRole(t, e, cid, ownerTok, "staff", 10,
		api.PermViewChannel|api.PermSendMessages|api.PermManageRoles)
	member, memberTok := e.NewUser("staffer")
	joinCommunity(t, e, cid, memberTok)
	grant(t, e, cid, ownerTok, member, staff, http.StatusNoContent)

	// Below their own highest role, so the position bound passes — but the bits
	// are not theirs.
	status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/roles", memberTok,
		[]any{"sneaky", uint64(0), uint64(5), uint64(api.PermAdministrator), uint64(0), uint64(0), uint64(0)})
	if status != http.StatusForbidden {
		t.Fatalf("minting an administrator role = %d, want 403 (%x)", status, body)
	}
	if code := e.ErrCode(body); code != "E_FORBIDDEN" {
		t.Fatalf("code = %s", code)
	}
	// The same request for bits the staffer does hold succeeds.
	if status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/roles", memberTok,
		[]any{"fine", uint64(0), uint64(5), uint64(api.PermSendMessages), uint64(0), uint64(0), uint64(0)}); status != http.StatusCreated {
		t.Fatalf("minting a role within their own bits = %d (%x)", status, body)
	}
	// And a PATCH cannot smuggle the bit in afterwards.
	low := createRole(t, e, cid, ownerTok, "low", 5, api.PermAddReactions)
	if status, _ := e.Do(http.MethodPatch, "/v1/roles/"+low.String(), memberTok,
		[]any{nil, nil, nil, uint64(api.PermAdministrator), nil, nil, nil}); status != http.StatusForbidden {
		t.Fatalf("patching in an administrator bit = %d, want 403", status)
	}
}

func TestManagerCannotGrantModerateMembersWithoutHoldingIt(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	staff := createRole(t, e, cid, ownerTok, "staff", 10, api.PermViewChannel|api.PermManageRoles)
	moderator := createRole(t, e, cid, ownerTok, "moderator", 5, api.Bits(1<<24))
	member, memberTok := e.NewUser("staffer")
	joinCommunity(t, e, cid, memberTok)
	grant(t, e, cid, ownerTok, member, staff, http.StatusNoContent)
	target, targetTok := e.NewUser("target")
	joinCommunity(t, e, cid, targetTok)

	status, body := e.Do(http.MethodPut, "/v1/communities/"+cid.String()+"/members/"+target.String()+"/roles/"+moderator.String(), memberTok, nil)
	if status != http.StatusForbidden {
		t.Fatalf("granting moderate_members without holding it = %d, want 403 (%x)", status, body)
	}
	if code := e.ErrCode(body); code != "E_FORBIDDEN" {
		t.Fatalf("code = %s, want E_FORBIDDEN", code)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), cid, target); err != nil || slices.Contains(held, moderator) {
		t.Fatalf("refused grant changed target roles: %v (%v)", held, err)
	}
}

func TestModerateMembersRoleRequiresTargetSecondFactor(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	moderator := createRole(t, e, cid, ownerTok, "moderator", 5, api.Bits(1<<24))
	actorRole := createRole(t, e, cid, ownerTok, "staff", 10, api.PermViewChannel|api.PermManageRoles|api.Bits(1<<24))
	actor, actorTok := e.NewUser("staffer")
	joinCommunity(t, e, cid, actorTok)
	grant(t, e, cid, ownerTok, actor, actorRole, http.StatusNoContent)
	target, targetTok := e.NewUser("target")
	joinCommunity(t, e, cid, targetTok)
	community, err := e.Repo.GetCommunity(t.Context(), cid)
	if err != nil {
		t.Fatalf("GetCommunity: %v", err)
	}
	if err := e.Repo.UpdateCommunityMeta(t.Context(), cid, community.Name, community.MinAccountAgeSeconds, 1); err != nil {
		t.Fatalf("enable require_mod_2fa: %v", err)
	}

	status, body := e.Do(http.MethodPut, "/v1/communities/"+cid.String()+"/members/"+target.String()+"/roles/"+moderator.String(), actorTok, nil)
	if code := e.ErrCode(body); status != http.StatusForbidden || code != "E_TARGET_NO_FACTOR" {
		t.Fatalf("granting moderate_members without target factor = %d %s, want 403 E_TARGET_NO_FACTOR", status, code)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), cid, target); err != nil || slices.Contains(held, moderator) {
		t.Fatalf("refused grant changed target roles: %v (%v)", held, err)
	}
}

func TestBotKindAloneDoesNotBypassModeratorSecondFactor(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	moderator := createRole(t, e, cid, ownerTok, "moderator", 5, api.PermModerateMembers)
	actorRole := createRole(t, e, cid, ownerTok, "staff", 10,
		api.PermViewChannel|api.PermManageRoles|api.PermModerateMembers)
	actor, actorTok := e.NewUser("staffer")
	joinCommunity(t, e, cid, actorTok)
	grant(t, e, cid, ownerTok, actor, actorRole, http.StatusNoContent)

	botID := id.New()
	bot := store.UserRow{ID: botID, Username: "bot" + botID.String()[:8], Display: "Unmanaged bot",
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64),
		Created: e.Clk.Now().Unix(), Kind: 1, FactorOwed: true}
	if err := e.Repo.CreateUser(t.Context(), bot); err != nil {
		t.Fatalf("CreateUser unmanaged bot: %v", err)
	}
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: cid, UserID: botID, Joined: e.Clk.Now().Unix()}); err != nil {
		t.Fatalf("PutMember unmanaged bot: %v", err)
	}
	community, err := e.Repo.GetCommunity(t.Context(), cid)
	if err != nil {
		t.Fatalf("GetCommunity: %v", err)
	}
	if err := e.Repo.UpdateCommunityMeta(t.Context(), cid, community.Name, community.MinAccountAgeSeconds, 1); err != nil {
		t.Fatalf("enable require_mod_2fa: %v", err)
	}

	path := "/v1/communities/" + cid.String() + "/members/" + botID.String() + "/roles/" + moderator.String()
	status, body := e.Do(http.MethodPut, path, actorTok, nil)
	if code := e.ErrCode(body); status != http.StatusForbidden || code != "E_TARGET_NO_FACTOR" {
		t.Fatalf("granting moderator role to Bot-kind user without a managed installation = %d %s, want 403 E_TARGET_NO_FACTOR", status, code)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), cid, botID); err != nil || slices.Contains(held, moderator) {
		t.Fatalf("refused bot-kind grant changed target roles: %v (%v)", held, err)
	}
}

func TestManagedRoleRoutesBindAuthorityToExactBotInstallation(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	role := createRole(t, e, cid, ownerTok, "managed moderator", 5, api.PermModerateMembers)
	community, err := e.Repo.GetCommunity(t.Context(), cid)
	if err != nil {
		t.Fatal(err)
	}
	now := e.Clk.Now().Unix()
	botID, deviceID, runtimeID := id.New(), id.New(), id.New()
	bot := store.UserRow{ID: botID, Username: "bot" + botID.String()[:8], Display: "Managed bot",
		Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}
	if err := e.Repo.CreateUser(t.Context(), bot); err != nil {
		t.Fatalf("CreateUser bot: %v", err)
	}
	verified := now
	if err := e.Repo.CreateDevice(t.Context(), store.DeviceRow{ID: deviceID, UserID: botID, DSKPub: uniqueAPITestDSK(t),
		CredentialBlob: []byte{1}, Tier: 0, SignerTier: 0, VerifiedAt: &verified, Created: now}); err != nil {
		t.Fatalf("CreateDevice bot: %v", err)
	}
	if err := e.Repo.CreateBotRuntime(t.Context(), store.BotRuntimeRow{ID: runtimeID, TokenHash: make([]byte, 32), Created: now, Rotated: now}); err != nil {
		t.Fatalf("CreateBotRuntime: %v", err)
	}
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: cid, UserID: botID, Joined: now}); err != nil {
		t.Fatalf("PutMember bot: %v", err)
	}
	credentialVerified := now
	installationID := id.New()
	if err := e.Repo.CreateBotInstallation(t.Context(), store.BotInstallationRow{
		ID: installationID, CommunityID: cid, RuntimeID: &runtimeID, UserID: &botID, DeviceID: &deviceID, RoleID: &role,
		State: store.BotInstallationReady, Revision: 1, Capabilities: 1 << 4, ApprovedBy: community.Owner,
		ApprovedAt: now, CredentialVerifiedAt: &credentialVerified, Created: now, Updated: now,
	}); err != nil {
		t.Fatalf("CreateBotInstallation: %v", err)
	}
	setWireVersion(e, ownerTok, 3)

	human, humanTok := e.NewUser("human-target")
	setWireVersion(e, humanTok, 3)
	joinCommunity(t, e, cid, humanTok)
	if err := e.Repo.UpdateCommunityMeta(t.Context(), cid, community.Name, community.MinAccountAgeSeconds, 1); err != nil {
		t.Fatalf("enable require_mod_2fa: %v", err)
	}
	grantPath := "/v1/communities/" + cid.String() + "/members/%s/roles/" + role.String()
	if status, body := e.Do(http.MethodPut, fmt.Sprintf(grantPath, human.String()), ownerTok, nil); status != http.StatusForbidden {
		t.Fatalf("grant managed role to human = %d, want 403 (%x)", status, body)
	}
	patchPath := "/v1/roles/" + role.String()
	if status, body := e.Do(http.MethodPatch, patchPath, ownerTok,
		[]any{nil, nil, nil, uint64(api.PermAdministrator), nil, nil, nil}); status != http.StatusForbidden {
		t.Fatalf("patch managed role permissions = %d, want 403 (%x)", status, body)
	}
	if status, body := e.Do(http.MethodPatch, patchPath, ownerTok,
		[]any{nil, nil, uint64(6), nil, nil, nil, nil}); status != http.StatusForbidden {
		t.Fatalf("patch managed role rank = %d, want 403 (%x)", status, body)
	}
	if status, body := e.Do(http.MethodDelete, patchPath, ownerTok, nil); status != http.StatusForbidden {
		t.Fatalf("delete managed role = %d, want 403 (%x)", status, body)
	}
	grant(t, e, cid, ownerTok, botID, role, http.StatusNoContent)
	if held, err := e.Repo.ListMemberRoles(t.Context(), cid, botID); err != nil || !slices.Contains(held, role) {
		t.Fatalf("exact Ready bot role grant missing: roles=%v err=%v", held, err)
	}
}

func TestNoRoleCanBeMovedToPositionZero(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	role := createRole(t, e, cid, ownerTok, "staff", 10, api.PermViewChannel|api.PermManageRoles)

	// Position 0 is @everyone's and is implicitly held by every member, so a role
	// parked there would hand its bits to the whole community. Even the owner is
	// refused, because there is no legitimate reason to have two.
	status, body := e.Do(http.MethodPatch, "/v1/roles/"+role.String(), ownerTok,
		[]any{nil, nil, uint64(0), nil, nil, nil, nil})
	if status != http.StatusBadRequest {
		t.Fatalf("PATCH position=0 = %d, want 400 (%x)", status, body)
	}
	if code := e.ErrCode(body); code != "E_INVALID_REQUEST" {
		t.Fatalf("code = %s", code)
	}
	if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/roles", ownerTok,
		[]any{"also no", uint64(0), uint64(0), uint64(0), uint64(0), uint64(0), uint64(0)}); status != http.StatusBadRequest {
		t.Fatalf("POST position=0 = %d, want 400", status)
	}
	// The @everyone role itself may still be edited — just not moved.
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	everyone := roles[0]
	if everyone.Position != 0 {
		t.Fatalf("roles[0] is not @everyone: %+v", everyone)
	}
	if status, _ := e.Do(http.MethodPatch, "/v1/roles/"+everyone.ID.String(), ownerTok,
		[]any{nil, nil, nil, uint64(api.PermViewChannel), nil, nil, nil}); status != http.StatusNoContent {
		t.Fatal("the owner could not edit @everyone's permissions")
	}
	if status, _ := e.Do(http.MethodPatch, "/v1/roles/"+everyone.ID.String(), ownerTok,
		[]any{nil, nil, uint64(7), nil, nil, nil, nil}); status != http.StatusBadRequest {
		t.Fatal("@everyone was moved off position 0")
	}
}

func TestRemovingARoleThatOpenedAPrivateChannelSchedulesRemoves(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 0 /* e2ee */, 0 /* private */, "secret")
	role := createRole(t, e, cid, ownerTok, "insiders", 10, api.PermViewChannel|api.PermSendMessages)
	if status, _ := e.Do(http.MethodPut,
		"/v1/channels/"+ch.String()+"/overwrites/0/"+role.String(), ownerTok,
		[]any{uint64(api.PermViewChannel), uint64(0)}); status != http.StatusNoContent {
		t.Fatal("PUT role overwrite failed")
	}
	// The channel is private: @everyone, which every new community grants
	// PermViewChannel, is denied it here, so the role is what opens it.
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil || len(roles) == 0 || roles[0].Position != 0 {
		t.Fatalf("ListRoles = %+v, %v; want @everyone first", roles, err)
	}
	if status, _ := e.Do(http.MethodPut,
		"/v1/channels/"+ch.String()+"/overwrites/0/"+roles[0].ID.String(), ownerTok,
		[]any{uint64(0), uint64(api.PermViewChannel)}); status != http.StatusNoContent {
		t.Fatal("PUT @everyone overwrite failed")
	}
	member, memberTok := e.NewUser("insider")
	joinCommunity(t, e, cid, memberTok)
	grant(t, e, cid, ownerTok, member, role, http.StatusNoContent)

	members, err := listChannelMembers(t, e, ch)
	if err != nil {
		t.Fatalf("ListChannelMembers: %v", err)
	}
	if !slices.Contains(members, member) {
		t.Fatal("the grant did not materialise channel_members")
	}

	// Revoking the role drops the materialised eligibility, which is what task 7
	// turns into delivery-service Removes.
	if status, _ := e.Do(http.MethodDelete,
		"/v1/communities/"+cid.String()+"/members/"+member.String()+"/roles/"+role.String(),
		ownerTok, nil); status != http.StatusNoContent {
		t.Fatal("DELETE role grant failed")
	}
	members, _ = listChannelMembers(t, e, ch)
	if slices.Contains(members, member) {
		t.Fatal("channel_members still lists a user whose role was revoked")
	}
}

// I1 (fix wave), protocol/09 § Roles "No new authority": a role's channel overwrites are
// authority too. A manage_roles holder may not grant (or revoke) a lower role whose overwrite in
// some channel carries a bit the holder does not have THERE, or they could open a private channel
// they cannot see to themself: bits 0 community-wide, 0x8ff3 in the channel after a self-grant.
func TestARoleGrantCannotHandOutChannelAccessTheGranterLacks(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	private, _, _ := newChannel(t, e, cid, ownerTok, 0, 0 /* e2ee */, 0 /* private */, "secret")
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil || len(roles) == 0 || roles[0].Position != 0 {
		t.Fatalf("ListRoles = %+v, %v; want @everyone first", roles, err)
	}
	if status, _ := e.Do(http.MethodPut,
		"/v1/channels/"+private.String()+"/overwrites/0/"+roles[0].ID.String(), ownerTok,
		[]any{uint64(0), uint64(api.PermViewChannel)}); status != http.StatusNoContent {
		t.Fatal("PUT @everyone deny failed")
	}
	// "insiders" carries nothing community-wide; its overwrite is what opens the channel.
	insiders := createRole(t, e, cid, ownerTok, "insiders", 5, 0)
	if status, _ := e.Do(http.MethodPut,
		"/v1/channels/"+private.String()+"/overwrites/0/"+insiders.String(), ownerTok,
		[]any{uint64(api.PermViewChannel | api.PermSendMessages), uint64(0)}); status != http.StatusNoContent {
		t.Fatal("PUT insiders overwrite failed")
	}
	// A role whose overwrite touches only a channel the staffer sees, with bits they hold there.
	open, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "open")
	helpers := createRole(t, e, cid, ownerTok, "helpers", 4, 0)
	if status, _ := e.Do(http.MethodPut,
		"/v1/channels/"+open.String()+"/overwrites/0/"+helpers.String(), ownerTok,
		[]any{uint64(api.PermSendMessages), uint64(0)}); status != http.StatusNoContent {
		t.Fatal("PUT helpers overwrite failed")
	}

	staff := createRole(t, e, cid, ownerTok, "staff", 10, api.PermViewChannel|api.PermSendMessages|api.PermManageRoles)
	staffer, stafferTok := e.NewUser("staffer")
	joinCommunity(t, e, cid, stafferTok)
	grant(t, e, cid, ownerTok, staffer, staff, http.StatusNoContent)
	if bits, _ := api.NewResolver(e.Repo).Resolve(t.Context(), staffer, mustChannel(t, e, private)); bits.Has(api.PermViewChannel) {
		t.Fatal("the staffer already sees the private channel; the test proves nothing")
	}

	grant(t, e, cid, stafferTok, staffer, insiders, http.StatusForbidden)
	if bits, _ := api.NewResolver(e.Repo).Resolve(t.Context(), staffer, mustChannel(t, e, private)); bits.Has(api.PermViewChannel) {
		t.Fatal("the refused self-grant opened the private channel anyway")
	}
	// The revoke is the same authority: a staffer who could not grant it may not take it away.
	victim, victimTok := e.NewUser("victim")
	joinCommunity(t, e, cid, victimTok)
	grant(t, e, cid, ownerTok, victim, insiders, http.StatusNoContent)
	path := "/v1/communities/" + cid.String() + "/members/" + victim.String() + "/roles/" + insiders.String()
	if status, _ := e.Do(http.MethodDelete, path, stafferTok, nil); status != http.StatusForbidden {
		t.Fatalf("a staffer revoking a role that opens a channel they cannot see = %d, want 403", status)
	}
	// The positive control.
	grant(t, e, cid, stafferTok, victim, helpers, http.StatusNoContent)
}

// I2 (fix wave): a user overwrite is written for a member and ends with the membership. After a
// kick or a leave, rejoining must not find the private channel the overwrite opened; and an
// overwrite that outlived its member (written before this fix) can still be deleted.
func TestAUserOverwriteEndsWithTheMembership(t *testing.T) {
	for _, how := range []string{"kick", "leave"} {
		t.Run(how, func(t *testing.T) {
			e, cid, ownerTok := channelEnv(t)
			api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
			private, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
			roles, _ := e.Repo.ListRoles(t.Context(), cid)
			if status, _ := e.Do(http.MethodPut,
				"/v1/channels/"+private.String()+"/overwrites/0/"+roles[0].ID.String(), ownerTok,
				[]any{uint64(0), uint64(api.PermViewChannel)}); status != http.StatusNoContent {
				t.Fatal("PUT @everyone deny failed")
			}
			user, userTok := e.NewUser("insider")
			joinCommunity(t, e, cid, userTok)
			owPath := "/v1/channels/" + private.String() + "/overwrites/1/" + user.String()
			if status, _ := e.Do(http.MethodPut, owPath, ownerTok,
				[]any{uint64(api.PermViewChannel), uint64(0)}); status != http.StatusNoContent {
				t.Fatal("PUT user overwrite failed")
			}
			if status, _ := e.Do(http.MethodGet, "/v1/channels/"+private.String(), userTok, nil); status != http.StatusOK {
				t.Fatalf("the insider's GET of the private channel = %d, want 200", status)
			}

			switch how {
			case "kick":
				if status, _ := e.Do(http.MethodDelete, "/v1/communities/"+cid.String()+"/members/"+user.String(), ownerTok, nil); status != http.StatusNoContent {
					t.Fatal("kick failed")
				}
			case "leave":
				if status, _ := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/leave", userTok, []any{}); status != http.StatusNoContent {
					t.Fatal("leave failed")
				}
			}
			if ow, _ := e.Repo.ListOverwrites(t.Context(), private); len(ow) != 1 {
				t.Fatalf("after the %s the channel's overwrites are %+v, want only @everyone's", how, ow)
			}
			joinCommunity(t, e, cid, userTok)
			if status, _ := e.Do(http.MethodGet, "/v1/channels/"+private.String(), userTok, nil); status != http.StatusNotFound {
				t.Fatalf("after the %s and a rejoin the private channel = %d, want 404", how, status)
			}
		})
	}

	// A stale overwrite from before the fix: its target is not a member, and the owner may still
	// delete it (the target's rank is taken as 0). A PUT for a non-member stays 404.
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	private, _, _ := newChannel(t, e, cid, ownerTok, 0, 0, 0, "secret")
	gone := id.New()
	if err := e.Repo.PutOverwrite(t.Context(), store.OverwriteRow{
		ChannelID: private, TargetKind: 1, TargetID: gone, Allow: uint64(api.PermViewChannel),
	}); err != nil {
		t.Fatalf("PutOverwrite: %v", err)
	}
	owPath := "/v1/channels/" + private.String() + "/overwrites/1/" + gone.String()
	if status, _ := e.Do(http.MethodPut, owPath, ownerTok, []any{uint64(0), uint64(0)}); status != http.StatusNotFound {
		t.Fatalf("PUT for a non-member = %d, want 404", status)
	}
	if status, body := e.Do(http.MethodDelete, owPath, ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE of a stale user overwrite = %d %s, want 204", status, e.ErrCode(body))
	}
}

// I4 (fix wave): a call group needs view_channel AND connect (protocol/02 invariants 1 and 4), so
// a user who keeps view but loses connect must lose their call-group leaf too, through an
// overwrite and through a role revoke alike. The view-only channel_members set is not enough.
func TestLosingConnectRemovesTheCallLeaf(t *testing.T) {
	setup := func(t *testing.T) (*env, id.ID, string, id.ID, id.ID, string, id.ID) {
		e, cid, ownerTok := channelEnv(t)
		api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).WithDS(e.DS).Register(e.Mux)
		voice, _, status := newChannel(t, e, cid, ownerTok, 1, 0, 0, "call")
		if status != http.StatusCreated {
			t.Fatalf("voice channel = %d", status)
		}
		member, memberTok := e.NewUser("speaker")
		joinCommunity(t, e, cid, memberTok)
		cg := seedGroupOfKind(t, e, voice, cid, 1)
		seedMember(t, e, cg, member, 4)
		roles, _ := e.Repo.ListRoles(t.Context(), cid)
		return e, cid, ownerTok, voice, member, memberTok, roles[0].ID
	}
	removedLeaf4 := func(e *env) bool {
		for _, r := range e.DS.removes() {
			if r.Leaf == 4 {
				return true
			}
		}
		return false
	}

	t.Run("overwrite", func(t *testing.T) {
		e, _, ownerTok, voice, member, _, everyone := setup(t)
		e.DS.Reset()
		if status, _ := e.Do(http.MethodPut, "/v1/channels/"+voice.String()+"/overwrites/0/"+everyone.String(), ownerTok,
			[]any{uint64(0), uint64(api.PermConnect)}); status != http.StatusNoContent {
			t.Fatal("PUT @everyone deny-connect failed")
		}
		bits, _ := api.NewResolver(e.Repo).Resolve(t.Context(), member, mustChannel(t, e, voice))
		if !bits.Has(api.PermViewChannel) || bits.Has(api.PermConnect) {
			t.Fatalf("bits = %#x, want view without connect", uint64(bits))
		}
		if !removedLeaf4(e) {
			t.Fatalf("Removes issued = %+v, want one for the call leaf 4", e.DS.removes())
		}
	})

	t.Run("role revoke", func(t *testing.T) {
		e, cid, ownerTok, voice, member, _, everyone := setup(t)
		if status, _ := e.Do(http.MethodPut, "/v1/channels/"+voice.String()+"/overwrites/0/"+everyone.String(), ownerTok,
			[]any{uint64(0), uint64(api.PermConnect)}); status != http.StatusNoContent {
			t.Fatal("PUT @everyone deny-connect failed")
		}
		speakers := createRole(t, e, cid, ownerTok, "speakers", 5, 0)
		if status, _ := e.Do(http.MethodPut, "/v1/channels/"+voice.String()+"/overwrites/0/"+speakers.String(), ownerTok,
			[]any{uint64(api.PermConnect), uint64(0)}); status != http.StatusNoContent {
			t.Fatal("PUT speakers allow-connect failed")
		}
		grant(t, e, cid, ownerTok, member, speakers, http.StatusNoContent)
		e.DS.Reset()
		path := "/v1/communities/" + cid.String() + "/members/" + member.String() + "/roles/" + speakers.String()
		if status, _ := e.Do(http.MethodDelete, path, ownerTok, nil); status != http.StatusNoContent {
			t.Fatal("revoke failed")
		}
		if !removedLeaf4(e) {
			t.Fatalf("Removes issued after the revoke = %+v, want one for the call leaf 4", e.DS.removes())
		}
	})
}

// createRole posts one role to /v1/communities/{id}/roles as tok and returns its
// id. Its body is [name, color, position, allow, deny, hoist, mentionable].
func createRole(t *testing.T, e *env, cid id.ID, tok, name string, position uint64, allow api.Bits) id.ID {
	t.Helper()
	status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/roles", tok,
		[]any{name, uint64(0), position, uint64(allow), uint64(0), uint64(0), uint64(0)})
	if status != http.StatusCreated {
		t.Fatalf("create role %q = %d (%x)", name, status, body)
	}
	var out []id.ID
	mustUnmarshal(t, body, &out)
	if len(out) != 1 {
		t.Fatalf("create role response = %v, want [role_id]", out)
	}
	return out[0]
}

func TestMembersReadTheRolesList(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	type roleRow struct {
		_           struct{} `cbor:",toarray"`
		ID          id.ID
		Name        string
		Color       uint64
		Position    uint64
		Allow       uint64
		Deny        uint64
		Hoist       uint64
		Mentionable uint64
	}
	list := func(tok string) []roleRow {
		t.Helper()
		status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/roles", tok, nil)
		if status != http.StatusOK {
			t.Fatalf("GET roles = %d (%x)", status, body)
		}
		var rows []roleRow
		mustUnmarshal(t, body, &rows)
		return rows
	}

	// A plain member reads the list; a new community has only @everyone.
	_, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil || len(roles) != 1 {
		t.Fatalf("ListRoles = %v, %v", roles, err)
	}
	everyone := roles[0]
	rows := list(memberTok)
	want := roleRow{ID: everyone.ID, Name: "@everyone", Allow: uint64(api.DefaultEveryoneAllow)}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("new community roles = %+v, want [%+v]", rows, want)
	}

	// A role at position 3 follows @everyone.
	three := createRole(t, e, cid, ownerTok, "three", 3, api.PermPinMessages)
	rows = list(memberTok)
	if len(rows) != 2 || rows[0].ID != everyone.ID || rows[1].ID != three || rows[1].Position != 3 || rows[1].Name != "three" {
		t.Fatalf("roles after create = %+v", rows)
	}

	// A stored bit outside the vocabulary is cleared in the answer.
	if err := e.Repo.PutRole(t.Context(), store.RoleRow{ID: three, CommunityID: cid, Name: "three", Position: 3,
		Allow: uint64(api.PermPinMessages) | 1<<40, Deny: 1 << 41, Created: e.Clk.Now().Unix()}); err != nil {
		t.Fatalf("PutRole: %v", err)
	}
	rows = list(ownerTok)
	if rows[1].Allow != uint64(api.PermPinMessages) || rows[1].Deny != 0 {
		t.Fatalf("masked row = %+v, want allow=PermPinMessages deny=0", rows[1])
	}

	_, strangerTok := e.NewUser("stranger")
	if status, _ := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/roles", strangerTok, nil); status != http.StatusNotFound {
		t.Errorf("non-member = %d, want 404", status)
	}
}

// joinCommunity joins cid as tok.
func joinCommunity(t *testing.T, e *env, cid id.ID, tok string) {
	t.Helper()
	if status, body := e.Do(http.MethodPost, "/v1/communities/"+cid.String()+"/join", tok, []any{nil}); status != http.StatusOK {
		t.Fatalf("join = %d (%x)", status, body)
	}
}

// grant PUTs role to user in cid as tok and asserts the status.
func grant(t *testing.T, e *env, cid id.ID, tok string, user, role id.ID, want int) {
	t.Helper()
	path := "/v1/communities/" + cid.String() + "/members/" + user.String() + "/roles/" + role.String()
	if status, body := e.Do(http.MethodPut, path, tok, nil); status != want {
		t.Fatalf("grant %s to %s = %d, want %d (%x)", role, user, status, want, body)
	}
}

// mustChannel reads one live channel row.
func mustChannel(t *testing.T, e *env, ch id.ID) store.ChannelRow {
	t.Helper()
	row, err := e.Repo.GetChannel(t.Context(), ch)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	return row
}

// listChannelMembers reads channel_members through the store (task 6's table,
// which task 7's materialiser writes for a community channel).
func listChannelMembers(t *testing.T, e *env, ch id.ID) ([]id.ID, error) {
	t.Helper()
	lister, ok := e.Repo.(interface {
		ListChannelMembers(ctx context.Context, channelID id.ID) ([]id.ID, error)
	})
	if !ok {
		t.Fatal("store.Repository has no ListChannelMembers yet (task 7)")
	}
	return lister.ListChannelMembers(t.Context(), ch)
}

type overwriteRow struct {
	_         struct{} `cbor:",toarray"`
	ChannelID id.ID
	Kind      uint64
	TargetID  id.ID
	Allow     uint64
	Deny      uint64
}

func listOverwriteRows(t *testing.T, e *env, cid id.ID, tok string) []overwriteRow {
	t.Helper()
	status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/overwrites", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("GET overwrites = %d (%x)", status, body)
	}
	var rows []overwriteRow
	mustUnmarshal(t, body, &rows)
	if rows == nil {
		t.Fatalf("GET overwrites answered null, want an empty array (%x)", body)
	}
	return rows
}

func TestMembersReadTheOverwritesTheyCanSee(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)

	// A fresh community has no overwrites: an empty array, never null.
	if rows := listOverwriteRows(t, e, cid, ownerTok); len(rows) != 0 {
		t.Fatalf("fresh community overwrites = %+v", rows)
	}

	ch, _, _ := newChannel(t, e, cid, ownerTok, 0, 1, 0, "general")
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil || len(roles) != 1 {
		t.Fatalf("ListRoles = %v, %v", roles, err)
	}
	everyone := roles[0].ID
	roleA := createRole(t, e, cid, ownerTok, "a", 3, api.PermViewChannel)

	put := func(kind uint64, target id.ID, allow, deny api.Bits) {
		t.Helper()
		path := "/v1/channels/" + ch.String() + "/overwrites/" + strconv.FormatUint(kind, 10) + "/" + target.String()
		if status, body := e.Do(http.MethodPut, path, ownerTok, []any{uint64(allow), uint64(deny)}); status != http.StatusNoContent {
			t.Fatalf("PUT overwrite = %d (%x)", status, body)
		}
	}
	put(0, everyone, 0, api.PermViewChannel)
	put(0, roleA, api.PermViewChannel, 0)

	want := []overwriteRow{
		{ChannelID: ch, Kind: 0, TargetID: everyone, Deny: uint64(api.PermViewChannel)},
		{ChannelID: ch, Kind: 0, TargetID: roleA, Allow: uint64(api.PermViewChannel)},
	}
	if bytes.Compare(want[0].TargetID[:], want[1].TargetID[:]) > 0 {
		want[0], want[1] = want[1], want[0]
	}
	if got := listOverwriteRows(t, e, cid, ownerTok); !slices.Equal(got, want) {
		t.Fatalf("owner overwrites = %+v, want %+v", got, want)
	}

	// B cannot view #general, so it sees none of its overwrites.
	if got := listOverwriteRows(t, e, cid, memberTok); len(got) != 0 {
		t.Fatalf("member without the role sees %+v", got)
	}
	grant(t, e, cid, ownerTok, member, roleA, http.StatusNoContent)
	if got := listOverwriteRows(t, e, cid, memberTok); !slices.Equal(got, want) {
		t.Fatalf("member with the role sees %+v, want %+v", got, want)
	}

	// Bits outside the vocabulary are cleared in the answer.
	if err := e.Repo.PutOverwrite(t.Context(), store.OverwriteRow{ChannelID: ch, TargetKind: 0, TargetID: roleA,
		Allow: 1<<40 | 1, Deny: 0}); err != nil {
		t.Fatalf("PutOverwrite: %v", err)
	}
	for _, row := range listOverwriteRows(t, e, cid, ownerTok) {
		if row.TargetID == roleA && row.Allow != 1 {
			t.Fatalf("allow = %#x, want 1", row.Allow)
		}
	}

	// An overwrite on a category is listed to a member who can view it.
	cat, _, _ := newChannel(t, e, cid, ownerTok, uint64(api.ChannelCategory), 1, 0, "cat")
	if err := e.Repo.PutOverwrite(t.Context(), store.OverwriteRow{ChannelID: cat, TargetKind: 1, TargetID: member,
		Allow: uint64(api.PermSendMessages)}); err != nil {
		t.Fatalf("PutOverwrite: %v", err)
	}
	found := false
	for _, row := range listOverwriteRows(t, e, cid, memberTok) {
		found = found || (row.ChannelID == cat && row.TargetID == member)
	}
	if !found {
		t.Fatal("the category overwrite is not listed to a member who can view the category")
	}

	// A non-member and an unknown community both answer 404.
	_, strangerTok := e.NewUser("stranger")
	if status, _ := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/overwrites", strangerTok, nil); status != http.StatusNotFound {
		t.Errorf("non-member = %d, want 404", status)
	}
	if status, _ := e.Do(http.MethodGet, "/v1/communities/"+id.New().String()+"/overwrites", ownerTok, nil); status != http.StatusNotFound {
		t.Errorf("unknown community = %d, want 404", status)
	}
}

// M-1: the overwrites list is ordered by channel_id, kind, target_id across channels and kinds,
// not in the store's order. The channel with the lower id is put at the higher position, so
// ListChannels (position, id) reads the channels in the opposite order to the answer's.
func TestTheOverwritesListIsInFullOrder(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	member, memberTok := e.NewUser("member")
	joinCommunity(t, e, cid, memberTok)
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	everyone := roles[0].ID
	roleA := createRole(t, e, cid, ownerTok, "a", 3, api.PermSendMessages)
	x, _, _ := newChannel(t, e, cid, ownerTok, 0, 1, 0, "x")
	y, _, _ := newChannel(t, e, cid, ownerTok, 0, 1, 0, "y")
	byID := func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) }
	chans := []id.ID{x, y}
	slices.SortFunc(chans, byID)
	low, high := chans[0], chans[1]
	if status, body := e.Do(http.MethodPatch, "/v1/channels/"+low.String(), ownerTok,
		[]any{nil, nil, nil, nil, nil, uint64(10), nil}); status != http.StatusNoContent {
		t.Fatalf("PATCH position = %d (%x)", status, body)
	}
	if stored, _ := e.Repo.ListChannels(t.Context(), cid); len(withoutBuiltinAFK(stored)) != 2 || stored[0].ID != high {
		t.Fatalf("the store does not read the higher id first: %+v", stored)
	}
	targets := []id.ID{everyone, roleA}
	slices.SortFunc(targets, byID)
	var want []overwriteRow
	for _, ch := range []id.ID{low, high} {
		for _, target := range targets {
			want = append(want, overwriteRow{ChannelID: ch, Kind: 0, TargetID: target, Deny: uint64(api.PermSendMessages)})
		}
		want = append(want, overwriteRow{ChannelID: ch, Kind: 1, TargetID: member, Deny: uint64(api.PermAddReactions)})
	}
	// Written in the reverse of the wanted order.
	for i := len(want) - 1; i >= 0; i-- {
		row := want[i]
		path := "/v1/channels/" + row.ChannelID.String() + "/overwrites/" + strconv.FormatUint(row.Kind, 10) + "/" + row.TargetID.String()
		if status, body := e.Do(http.MethodPut, path, ownerTok, []any{row.Allow, row.Deny}); status != http.StatusNoContent {
			t.Fatalf("PUT %s = %d (%x)", path, status, body)
		}
	}
	if got := listOverwriteRows(t, e, cid, ownerTok); !slices.Equal(got, want) {
		t.Fatalf("overwrites = %+v\nwant %+v", got, want)
	}
}

// S2: an overwrite PUT or DELETE that would hide the channel from its own author is refused,
// unless the author is the owner or holds administrator. 403 keeps its precedence, and there is
// no self-target exception on PUT.
func TestNoOneLocksThemselfOutOfAChannel(t *testing.T) {
	f := privateEnv(t)
	owPath := func(ch id.ID, kind string, target id.ID) string {
		return "/v1/channels/" + ch.String() + "/overwrites/" + kind + "/" + target.String()
	}
	do := func(name, method, path, tok string, body any, want int, detail string) {
		t.Helper()
		status, resp := f.e.Do(method, path, tok, body)
		if status != want {
			t.Fatalf("%s = %d, want %d (%x)", name, status, want, resp)
		}
		if detail != "" && errDetail(t, resp) != detail {
			t.Fatalf("%s detail = %q, want %q", name, errDetail(t, resp), detail)
		}
	}
	denyView := []any{uint64(0), uint64(api.PermViewChannel)}

	// PUT @everyone deny view by a mod with no other access: refused; by the owner: done.
	ch, _, _ := newChannel(t, f.e, f.cid, f.ownerTok, 0, 0, 0, "allmänt")
	everyone := owPath(ch, "0", f.everyone)
	do("mod PUT that hides", http.MethodPut, everyone, f.mTok, denyView, http.StatusBadRequest, "you would lose this channel")
	do("mod PUT with a bit it lacks", http.MethodPut, everyone, f.mTok,
		[]any{uint64(api.PermPinMessages), uint64(api.PermViewChannel)}, http.StatusForbidden, "")
	do("mod PUT on itself", http.MethodPut, owPath(ch, "1", f.mod), f.mTok,
		[]any{uint64(api.PermViewChannel), uint64(0)}, http.StatusForbidden, "")
	if ow, _ := f.e.Repo.ListOverwrites(t.Context(), ch); len(ow) != 0 {
		t.Fatalf("a refused PUT wrote %+v", ow)
	}
	do("owner PUT", http.MethodPut, everyone, f.ownerTok, denyView, http.StatusNoContent, "")

	// An administrator is exempt like the owner.
	admin := createRole(t, f.e, f.cid, f.ownerTok, "admin", 3, api.PermAdministrator)
	grant(t, f.e, f.cid, f.ownerTok, f.b, admin, http.StatusNoContent)
	other, _, _ := newChannel(t, f.e, f.cid, f.ownerTok, 0, 0, 0, "annat")
	do("admin PUT", http.MethodPut, owPath(other, "0", f.everyone), f.bTok, denyView, http.StatusNoContent, "")

	// With @everyone denied, a lower role's overwrite is what gives the mod the channel. The mod
	// may change @everyone again, but may not DELETE that role overwrite; the owner may.
	viewers := createRole(t, f.e, f.cid, f.ownerTok, "viewers", 2, 0)
	grant(t, f.e, f.cid, f.ownerTok, f.mod, viewers, http.StatusNoContent)
	do("owner opens to viewers", http.MethodPut, owPath(ch, "0", viewers), f.ownerTok,
		[]any{uint64(api.PermViewChannel), uint64(0)}, http.StatusNoContent, "")
	do("mod PUT kept by viewers", http.MethodPut, everyone, f.mTok,
		[]any{uint64(0), uint64(api.PermViewChannel | api.PermSendMessages)}, http.StatusNoContent, "")
	do("mod PUT that replaces viewers' allow", http.MethodPut, owPath(ch, "0", viewers), f.mTok,
		[]any{uint64(0), uint64(0)}, http.StatusBadRequest, "you would lose this channel")
	do("mod DELETE that hides", http.MethodDelete, owPath(ch, "0", viewers), f.mTok, nil,
		http.StatusBadRequest, "you would lose this channel")
	if ow, _ := f.e.Repo.ListOverwrites(t.Context(), ch); len(ow) != 2 {
		t.Fatalf("a refused change touched the overwrites: %+v", ow)
	}
	do("owner DELETE", http.MethodDelete, owPath(ch, "0", viewers), f.ownerTok, nil, http.StatusNoContent, "")
}

// S2: the self-lockout rule is decided inside the transaction against the overwrites as they
// stand. The mod sees a private channel through two roles; while its DELETE of one of them is
// between the checks and the transaction, the owner removes the other. The DELETE is refused.
func TestTheSelfLockoutRuleRechecksUnderTheLock(t *testing.T) {
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	hooked := &beforeTx{Repository: e.Repo}
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	api.NewChannels(e.Repo, e.DS, e.Clk, 10, log).Register(e.Mux)
	api.NewRoles(hooked, e.Clk, "dilla.example", log).Register(e.Mux)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	mod, modTok := joinAs(t, e, cid, "mod")
	roles, err := e.Repo.ListRoles(t.Context(), cid)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	modRole := createRole(t, e, cid, ownerTok, "mod", 5, api.PermViewChannel|api.PermManageRoles)
	viewers := createRole(t, e, cid, ownerTok, "viewers", 2, 0)
	helpers := createRole(t, e, cid, ownerTok, "helpers", 3, 0)
	for _, r := range []id.ID{modRole, viewers, helpers} {
		grant(t, e, cid, ownerTok, mod, r, http.StatusNoContent)
	}
	var ch id.ID
	path := func(target id.ID) string {
		return "/v1/channels/" + ch.String() + "/overwrites/0/" + target.String()
	}
	private := func(name string) {
		t.Helper()
		ch, _, _ = newChannel(t, e, cid, ownerTok, 0, 0, 0, name)
		for target, pair := range map[id.ID][]any{
			roles[0].ID: {uint64(0), uint64(api.PermViewChannel)},
			viewers:     {uint64(api.PermViewChannel), uint64(0)},
			helpers:     {uint64(api.PermViewChannel), uint64(0)},
		} {
			if status, body := e.Do(http.MethodPut, path(target), ownerTok, pair); status != http.StatusNoContent {
				t.Fatalf("owner PUT = %d (%x)", status, body)
			}
		}
	}
	// Control: undisturbed, the DELETE passes, viewers still showing the channel to the mod, so
	// the refusal below could only come from the check inside the transaction.
	private("kontroll")
	if status, body := e.Do(http.MethodDelete, path(helpers), modTok, nil); status != http.StatusNoContent {
		t.Fatalf("mod DELETE undisturbed = %d (%x), want 204", status, body)
	}
	private("hemligt")

	hooked.arm(func() {
		if err := e.Repo.DeleteOverwrite(t.Context(), ch, 0, viewers); err != nil {
			t.Errorf("concurrent delete: %v", err)
		}
	})
	status, body := e.Do(http.MethodDelete, path(helpers), modTok, nil)
	if status != http.StatusBadRequest || errDetail(t, body) != "you would lose this channel" {
		t.Fatalf("mod DELETE raced to lockout = %d (%x), want 400", status, body)
	}
	if ow, _ := e.Repo.ListOverwrites(t.Context(), ch); len(ow) != 2 {
		t.Fatalf("overwrites = %+v, want @everyone and helpers", ow)
	}
}

// I-2: every role route takes its decision again inside its transaction under the community row
// lock. For each route, a moderator demoted after the route's checks and before its transaction
// is refused 403 and nothing is written; the same request undisturbed (the control) succeeds, so
// the 403 can only come from the check inside the transaction.
func TestTheRoleRoutesRecheckUnderTheLock(t *testing.T) {
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	hooked := &beforeTx{Repository: e.Repo}
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	api.NewRoles(hooked, e.Clk, "dilla.example", log).Register(e.Mux)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	mod, modTok := joinAs(t, e, cid, "mod")
	target, _ := joinAs(t, e, cid, "target")
	modRole := createRole(t, e, cid, ownerTok, "mod", 5, api.PermViewChannel|api.PermManageRoles)
	grant(t, e, cid, ownerTok, mod, modRole, http.StatusNoContent)
	low := createRole(t, e, cid, ownerTok, "low", 2, 0)

	roleCount := func() int {
		roles, err := e.Repo.ListRoles(t.Context(), cid)
		if err != nil {
			t.Fatalf("ListRoles: %v", err)
		}
		return len(roles)
	}
	holdsLow := func() bool {
		held, err := e.Repo.ListMemberRoles(t.Context(), cid, target)
		if err != nil {
			t.Fatalf("ListMemberRoles: %v", err)
		}
		return slices.Contains(held, low)
	}
	lowName := func() string {
		role, err := e.Repo.GetRole(t.Context(), low)
		if err != nil {
			return "<gone>"
		}
		return role.Name
	}
	grantPath := "/v1/communities/" + cid.String() + "/members/" + target.String() + "/roles/" + low.String()
	cases := []struct {
		name, method, path string
		body               any
		ok                 int
		unchanged          func() bool // true while the refused request has written nothing
	}{
		{"create", http.MethodPost, "/v1/communities/" + cid.String() + "/roles",
			[]any{"ny", uint64(0), uint64(3), uint64(0), uint64(0), uint64(0), uint64(0)}, http.StatusCreated,
			func() bool { return roleCount() == 3 }},
		{"patch", http.MethodPatch, "/v1/roles/" + low.String(),
			[]any{"lägre", nil, nil, nil, nil, nil, nil}, http.StatusNoContent,
			func() bool { return lowName() == "low" }},
		{"grant", http.MethodPut, grantPath, nil, http.StatusNoContent,
			func() bool { return !holdsLow() }},
		{"revoke", http.MethodDelete, grantPath, nil, http.StatusNoContent,
			func() bool { return holdsLow() }},
		{"delete", http.MethodDelete, "/v1/roles/" + low.String(), nil, http.StatusNoContent,
			func() bool { return lowName() == "lägre" }},
	}
	for _, c := range cases {
		hooked.arm(func() {
			if err := e.Repo.DeleteMemberRole(t.Context(), cid, mod, modRole); err != nil {
				t.Errorf("demote: %v", err)
			}
		})
		status, body := e.Do(c.method, c.path, modTok, c.body)
		if status != http.StatusForbidden {
			t.Fatalf("%s by a mod demoted before the transaction = %d (%x), want 403", c.name, status, body)
		}
		if !c.unchanged() {
			t.Fatalf("the refused %s wrote", c.name)
		}
		if err := e.Repo.PutMemberRole(t.Context(), cid, mod, modRole); err != nil {
			t.Fatalf("re-promote: %v", err)
		}
		if status, body := e.Do(c.method, c.path, modTok, c.body); status != c.ok {
			t.Fatalf("control: %s undisturbed = %d (%x), want %d", c.name, status, body, c.ok)
		}
		if c.unchanged() {
			t.Fatalf("control: the %s wrote nothing", c.name)
		}
	}
}

// S2 round 2: the in-transaction re-check covers more than the actor's own roles. (a) A grant
// whose community turns on require_mod_2fa, and whose role gains a moderator bit, between the
// checks and the transaction is refused for a target with no second factor. (b) A patch or delete
// of a role moved to or above the actor's highest role in that window is refused. Nothing is
// written, and each request undisturbed (the control) succeeds.
func TestTheRoleRoutesRecheckTheRoleAndTheCommunityUnderTheLock(t *testing.T) {
	e := newEnv(t)
	log := slog.New(slog.DiscardHandler)
	hooked := &beforeTx{Repository: e.Repo}
	api.NewCommunities(e.Repo, e.DS, e.Clk, log).Register(e.Mux)
	api.NewRoles(hooked, e.Clk, "dilla.example", log).Register(e.Mux)
	_, ownerTok := e.NewUser("owner")
	cid := createCommunity(t, e, ownerTok)
	mod, modTok := joinAs(t, e, cid, "mod")
	target, _ := joinAs(t, e, cid, "target")
	// The mod holds manage_messages, so the role gaining it passes checkBits and only the
	// second-factor rule can refuse the grant.
	modRole := createRole(t, e, cid, ownerTok, "mod", 5,
		api.PermViewChannel|api.PermManageRoles|api.PermManageMessages)
	grant(t, e, cid, ownerTok, mod, modRole, http.StatusNoContent)
	low := createRole(t, e, cid, ownerTok, "low", 2, 0)
	role := func() (store.RoleRow, bool) {
		r, err := e.Repo.GetRole(t.Context(), low)
		return r, err == nil
	}
	setRole := func(position uint64, allow api.Bits) {
		r, ok := role()
		if !ok {
			t.Fatal("low is gone")
		}
		r.Position, r.Allow = position, uint64(allow)
		if err := e.Repo.PutRole(t.Context(), r); err != nil {
			t.Errorf("PutRole: %v", err)
		}
	}
	setMod2FA := func(v uint8) {
		com, err := e.Repo.GetCommunity(t.Context(), cid)
		if err != nil {
			t.Fatalf("GetCommunity: %v", err)
		}
		if err := e.Repo.UpdateCommunityMeta(t.Context(), cid, com.Name, com.MinAccountAgeSeconds, v); err != nil {
			t.Errorf("UpdateCommunityMeta: %v", err)
		}
	}

	// (a) grant.
	grantPath := "/v1/communities/" + cid.String() + "/members/" + target.String() + "/roles/" + low.String()
	hooked.arm(func() {
		setMod2FA(1)
		setRole(2, api.PermManageMessages)
	})
	status, body := e.Do(http.MethodPut, grantPath, modTok, nil)
	if code := e.ErrCode(body); status != http.StatusForbidden || code != "E_TARGET_NO_FACTOR" {
		t.Fatalf("grant after require_mod_2fa and a moderator bit = %d %s, want 403 E_TARGET_NO_FACTOR",
			status, code)
	}
	if held, _ := e.Repo.ListMemberRoles(t.Context(), cid, target); slices.Contains(held, low) {
		t.Fatal("the refused grant wrote")
	}
	setMod2FA(0)
	setRole(2, 0)
	if status, body := e.Do(http.MethodPut, grantPath, modTok, nil); status != http.StatusNoContent {
		t.Fatalf("control: grant undisturbed = %d (%x)", status, body)
	}

	// (b) patch and delete of a role moved to, and above, the mod's own position.
	for _, position := range []uint64{5, 6} {
		hooked.arm(func() { setRole(position, 0) })
		status, body := e.Do(http.MethodPatch, "/v1/roles/"+low.String(), modTok, []any{"bytt", nil, nil, nil, nil, nil, nil})
		if status != http.StatusForbidden && status != http.StatusConflict {
			t.Fatalf("patch of a role moved to %d = %d (%x), want 403 or 409", position, status, body)
		}
		if r, _ := role(); r.Name != "low" {
			t.Fatalf("the refused patch wrote %q", r.Name)
		}
		setRole(2, 0)
		hooked.arm(func() { setRole(position, 0) })
		if status, body := e.Do(http.MethodDelete, "/v1/roles/"+low.String(), modTok, nil); status != http.StatusForbidden {
			t.Fatalf("delete of a role moved to %d = %d (%x), want 403", position, status, body)
		}
		if _, ok := role(); !ok {
			t.Fatal("the refused delete wrote")
		}
		setRole(2, 0)
	}
	if status, body := e.Do(http.MethodPatch, "/v1/roles/"+low.String(), modTok, []any{"bytt", nil, nil, nil, nil, nil, nil}); status != http.StatusNoContent {
		t.Fatalf("control: patch undisturbed = %d (%x)", status, body)
	}
	if status, body := e.Do(http.MethodDelete, "/v1/roles/"+low.String(), modTok, nil); status != http.StatusNoContent {
		t.Fatalf("control: delete undisturbed = %d (%x)", status, body)
	}
	if _, ok := role(); ok {
		t.Fatal("control: the delete wrote nothing")
	}
}

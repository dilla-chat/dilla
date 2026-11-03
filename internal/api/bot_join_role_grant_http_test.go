package api_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestBotJoinRoleGrantRejectsHumanSession(t *testing.T) {
	h := newGroupsAPI(t)
	path := "/v1/communities/" + id.New().String() + "/bot/join-role-grants"
	resp := h.do(t, http.MethodPost, path, h.session, mustCBOR(t, []any{id.New(), id.New(), id.New()}))
	if resp.Code != http.StatusForbidden {
		t.Fatalf("human session POST join-role grant = %d (%x), want 403", resp.Code, resp.Body.Bytes())
	}
}

func TestBotMemberEpisodeIsCapabilityNeutralButInstallationScoped(t *testing.T) {
	handler, deps := newTestAPI(t)
	ctx := context.Background()
	owner, _, _ := seedAPISession(t, deps)
	now := deps.Clock.Now().Unix()
	cid, otherCommunity := id.New(), id.New()
	for _, community := range []id.ID{cid, otherCommunity} {
		if err := deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: community, Owner: owner.ID, Name: "episode-read", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
			t.Fatal(err)
		}
	}
	bot, target, device, installation, runtime := id.New(), id.New(), id.New(), id.New(), id.New()
	for _, user := range []store.UserRow{
		{ID: bot, Username: "episode-bot", Display: "Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now},
		newAPITestUser(target, "episode-target", now),
	} {
		if err := deps.Repo.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []id.ID{bot, target} {
		if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	verified := now
	if err := deps.Repo.CreateDevice(ctx, store.DeviceRow{ID: device, UserID: bot, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtime, TokenHash: bytes.Repeat([]byte{0x31}, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: cid, RuntimeID: &runtime, UserID: &bot, DeviceID: &device, State: store.BotInstallationReady, Revision: 1, Capabilities: 1 << 0, ApprovedBy: owner.ID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	token := "episode-music-bot-session"
	if err := deps.Repo.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(token), DeviceID: device, UserID: bot, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(token), InstallationID: installation, UserID: bot, DeviceID: device, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(token), 3, 1, 1); err != nil {
		t.Fatal(err)
	}

	path := "/v1/communities/" + cid.String() + "/bot/members/" + target.String() + "/episode"
	resp := getAuth(handler, path, token)
	var reply []any
	member, err := deps.Repo.GetMember(ctx, cid, target)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Code != http.StatusOK || cborx.Unmarshal(resp.Body.Bytes(), &reply) != nil || len(reply) != 2 || !bytes.Equal(reply[0].([]byte), target[:]) || !bytes.Equal(reply[1].([]byte), member.EpisodeID[:]) {
		t.Fatalf("Music-only member episode = %d %#v, want [%s,%s]", resp.Code, reply, target, member.EpisodeID)
	}

	grant := postCBORAuth(handler, "/v1/communities/"+cid.String()+"/bot/join-role-grants", mustCBOR(t, []any{id.New(), target, member.EpisodeID}), token)
	if grant.Code != http.StatusForbidden {
		t.Fatalf("Music-only join-role grant = %d (%x), want 403", grant.Code, grant.Body.Bytes())
	}
	wrongCommunity := getAuth(handler, "/v1/communities/"+otherCommunity.String()+"/bot/members/"+target.String()+"/episode", token)
	if wrongCommunity.Code == http.StatusOK {
		t.Fatal("episode lookup for a community outside the bound installation succeeded")
	}
	missingMember := getAuth(handler, "/v1/communities/"+cid.String()+"/bot/members/"+id.New().String()+"/episode", token)
	if missingMember.Code == http.StatusOK {
		t.Fatal("episode lookup for a nonmember succeeded")
	}

	current, err := deps.Repo.GetBotInstallation(ctx, installation)
	if err != nil {
		t.Fatal(err)
	}
	current.State = store.BotInstallationDisabled
	current.Revision++
	current.Updated++
	if err := deps.Repo.UpdateBotInstallation(ctx, installation, current.Revision-1, current); err != nil {
		t.Fatal(err)
	}
	disabled := getAuth(handler, path, token)
	if disabled.Code == http.StatusOK {
		t.Fatal("episode lookup for a disabled installation succeeded")
	}

}

func TestBotJoinRoleGrantBindsEpisodeAndRechecksApprover(t *testing.T) {
	grantNotifications := 0
	var notifiedCommunity, notifiedMember id.ID
	handler, deps := newTestAPIWired(t, func(d *api.Deps) {
		d.BotJoinRoleGranted = func(_ context.Context, community, member id.ID) {
			grantNotifications++
			notifiedCommunity, notifiedMember = community, member
		}
	})
	ctx := context.Background()
	owner, _, _ := seedAPISession(t, deps)
	now := deps.Clock.Now().Unix()
	cid := id.New()
	if err := deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: cid, Owner: owner.ID, Name: "join-grant", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	manager, target, staleTarget, bot, device, installation, runtime := id.New(), id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	for _, user := range []store.UserRow{
		newAPITestUser(manager, "grant-manager", now), newAPITestUser(target, "grant-target", now),
		newAPITestUser(staleTarget, "grant-stale", now),
		{ID: bot, Username: "grant-bot", Display: "Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now},
	} {
		if err := deps.Repo.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []id.ID{owner.ID, manager, target, staleTarget, bot} {
		if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	everyone := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "@everyone", Position: 0, Allow: uint64(api.PermViewChannel), Created: now}
	approvedRole := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "joiner", Position: 1, Allow: uint64(api.PermViewChannel), Created: now}
	managerRole := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "role-manager", Position: 10, Allow: uint64(api.PermManageRoles), Created: now}
	for _, role := range []store.RoleRow{everyone, approvedRole, managerRole} {
		if err := deps.Repo.PutRole(ctx, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := deps.Repo.PutMemberRole(ctx, cid, manager, managerRole.ID); err != nil {
		t.Fatal(err)
	}
	verified := now
	if err := deps.Repo.CreateDevice(ctx, store.DeviceRow{ID: device, UserID: bot, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtime, TokenHash: bytes.Repeat([]byte{0x27}, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: cid, RuntimeID: &runtime, UserID: &bot, DeviceID: &device, State: store.BotInstallationReady, Revision: 1, Capabilities: 1 << 2, ApprovedBy: owner.ID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockCommunity(ctx, cid); err != nil {
			return err
		}
		return tx.(store.BotJoinRolePolicy).SetBotJoinRolePolicy(ctx, installation, approvedRole.ID, manager, now)
	}); err != nil {
		t.Fatal(err)
	}
	token := "join-grant-bot-session"
	if err := deps.Repo.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(token), DeviceID: device, UserID: bot, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(token), InstallationID: installation, UserID: bot, DeviceID: device, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(token), 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + cid.String() + "/bot/join-role-grants"
	current, err := deps.Repo.GetMember(ctx, cid, target)
	if err != nil {
		t.Fatal(err)
	}
	requestID := id.New()
	body, _ := cborx.Marshal([]any{requestID, target, current.EpisodeID})
	first := postCBORAuth(handler, path, body, token)
	if first.Code != http.StatusOK {
		t.Fatalf("grant = %d (%x), want 200", first.Code, first.Body.Bytes())
	}
	if grantNotifications != 1 || notifiedCommunity != cid || notifiedMember != target {
		t.Fatalf("first grant notifications = %d for %s/%s, want once for %s/%s", grantNotifications, notifiedCommunity, notifiedMember, cid, target)
	}
	var receipt []any
	if err := cborx.Unmarshal(first.Body.Bytes(), &receipt); err != nil || len(receipt) != 6 {
		t.Fatalf("receipt = %#v, %v", receipt, err)
	}
	episodePath := "/v1/communities/" + cid.String() + "/bot/members/" + target.String() + "/episode"
	episodeResp := getAuth(handler, episodePath, token)
	var episodeReply []any
	if episodeResp.Code != http.StatusOK || cborx.Unmarshal(episodeResp.Body.Bytes(), &episodeReply) != nil || len(episodeReply) != 2 || !bytes.Equal(episodeReply[0].([]byte), target[:]) || !bytes.Equal(episodeReply[1].([]byte), current.EpisodeID[:]) {
		t.Fatalf("member episode = %d %#v, want [%s,%s]", episodeResp.Code, episodeReply, target, current.EpisodeID)
	}
	retry := postCBORAuth(handler, path, body, token)
	if retry.Code != http.StatusOK || !bytes.Equal(retry.Body.Bytes(), first.Body.Bytes()) {
		t.Fatalf("retry = %d (%x), want stable receipt %x", retry.Code, retry.Body.Bytes(), first.Body.Bytes())
	}
	if grantNotifications != 2 {
		t.Fatalf("receipt retry produced %d grant notifications, want two for repair", grantNotifications)
	}
	oldEpisode := mustMemberEpisode(t, deps.Repo, cid, staleTarget)
	if err := deps.Repo.DeleteMember(ctx, cid, staleTarget); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: staleTarget, Joined: now + 1}); err != nil {
		t.Fatal(err)
	}
	stale := postCBORAuth(handler, path, mustCBOR(t, []any{id.New(), staleTarget, oldEpisode}), token)
	if stale.Code != http.StatusNotFound {
		t.Fatalf("stale episode grant = %d (%x), want 404", stale.Code, stale.Body.Bytes())
	}
	if err := deps.Repo.DeleteMemberRole(ctx, cid, manager, managerRole.ID); err != nil {
		t.Fatal(err)
	}
	lost := postCBORAuth(handler, path, mustCBOR(t, []any{id.New(), staleTarget, mustMemberEpisode(t, deps.Repo, cid, staleTarget)}), token)
	if lost.Code != http.StatusForbidden {
		t.Fatalf("grant after approver authority loss = %d (%x), want 403", lost.Code, lost.Body.Bytes())
	}
	if grantNotifications != 2 {
		t.Fatalf("refused grants produced %d notifications, want two", grantNotifications)
	}
	held, err := deps.Repo.ListMemberRoles(ctx, cid, staleTarget)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range held {
		if role == approvedRole.ID {
			t.Fatal("authority failure left an effective join-role grant")
		}
	}
}

func mustMemberEpisode(t *testing.T, repo store.Repository, cid, user id.ID) id.ID {
	t.Helper()
	row, err := repo.GetMember(t.Context(), cid, user)
	if err != nil {
		t.Fatal(err)
	}
	return row.EpisodeID
}

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

func TestBotActorAuthorityIsBoundToReadyCommunityAndCurrentMember(t *testing.T) {
	h, deps := newTestAPI(t)
	ctx := context.Background()
	owner, _, humanToken := seedAPISession(t, deps)
	now := deps.Clock.Now().Unix()
	cid := id.New()
	if err := deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: cid, Owner: owner.ID, Name: "authority", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	target, bot, device, installation, runtime := id.New(), id.New(), id.New(), id.New(), id.New()
	for _, u := range []store.UserRow{
		newAPITestUser(owner.ID, "authority-owner", now),
		newAPITestUser(target, "authority-target", now),
		{ID: bot, Username: "authority-bot", Display: "Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now},
	} {
		if u.ID == owner.ID {
			continue
		}
		if err := deps.Repo.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	verified := now
	if err := deps.Repo.CreateDevice(ctx, store.DeviceRow{ID: device, UserID: bot, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []id.ID{owner.ID, target, bot} {
		if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	everyone := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "@everyone", Position: 0, Allow: uint64(api.PermViewChannel | api.PermReadHistory), Created: now}
	manager := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "manager", Position: 1, Allow: uint64(api.PermManageCommunity | api.PermManageMessages), Created: now}
	for _, role := range []store.RoleRow{everyone, manager} {
		if err := deps.Repo.PutRole(ctx, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := deps.Repo.PutMemberRole(ctx, cid, target, manager.ID); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtime, TokenHash: bytes.Repeat([]byte{0x22}, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: cid, RuntimeID: &runtime, UserID: &bot, DeviceID: &device, State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner.ID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	token := "actor-authority-bot-session"
	if err := deps.Repo.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(token), DeviceID: device, UserID: bot, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(token), InstallationID: installation, UserID: bot, DeviceID: device, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(token), 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + cid.String() + "/bot/actors/" + target.String() + "/authority"
	resp := getAuth(h, path, token)
	if resp.Code != http.StatusOK {
		t.Fatalf("actor authority = %d %x", resp.Code, resp.Body.Bytes())
	}
	var got []any
	if err := cborx.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	roleValues, rolesOK := got[3].([]any)
	roleBytes, roleOK := func() ([]byte, bool) {
		if !rolesOK || len(roleValues) != 1 {
			return nil, false
		}
		value, ok := roleValues[0].([]byte)
		return value, ok
	}()
	if len(got) != 4 || got[0] != uint64(api.PermViewChannel|api.PermReadHistory|api.PermManageCommunity|api.PermManageMessages) || got[1] != uint64(0) || got[2] != uint64(0) || !roleOK || !bytes.Equal(roleBytes, manager.ID[:]) {
		t.Fatalf("authority tuple = %#v", got)
	}
	ownerAuthority := getAuth(h, "/v1/communities/"+cid.String()+"/bot/actors/"+owner.ID.String()+"/authority", token)
	var ownerTuple []any
	if ownerAuthority.Code != http.StatusOK || cborx.Unmarshal(ownerAuthority.Body.Bytes(), &ownerTuple) != nil || len(ownerTuple) != 4 ||
		ownerTuple[0] != uint64(api.PermAll) || ownerTuple[1] != uint64(1) || ownerTuple[2] != uint64(1) {
		t.Fatalf("owner authority tuple = %d %#v", ownerAuthority.Code, ownerTuple)
	}
	adminUser := id.New()
	if err := deps.Repo.CreateUser(ctx, newAPITestUser(adminUser, "authority-admin", now)); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: adminUser, Joined: now}); err != nil {
		t.Fatal(err)
	}
	adminRole := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "administrator", Position: 2, Allow: uint64(api.PermAdministrator), Created: now}
	if err := deps.Repo.PutRole(ctx, adminRole); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMemberRole(ctx, cid, adminUser, adminRole.ID); err != nil {
		t.Fatal(err)
	}
	adminAuthority := getAuth(h, "/v1/communities/"+cid.String()+"/bot/actors/"+adminUser.String()+"/authority", token)
	var adminTuple []any
	if adminAuthority.Code != http.StatusOK || cborx.Unmarshal(adminAuthority.Body.Bytes(), &adminTuple) != nil || len(adminTuple) != 4 ||
		adminTuple[0] != uint64(api.PermAll) || adminTuple[1] != uint64(0) || adminTuple[2] != uint64(1) {
		t.Fatalf("admin authority tuple = %d %#v", adminAuthority.Code, adminTuple)
	}
	if denied := getAuth(h, path, humanToken); denied.Code != http.StatusForbidden {
		t.Fatalf("human session authority = %d, want 403", denied.Code)
	}
	foreignCommunityPath := "/v1/communities/" + id.New().String() + "/bot/actors/" + target.String() + "/authority"
	if denied := getAuth(h, foreignCommunityPath, token); denied.Code != http.StatusForbidden {
		t.Fatalf("foreign community authority = %d, want 403", denied.Code)
	}
	foreign := id.New()
	if err := deps.Repo.CreateUser(ctx, newAPITestUser(foreign, "authority-foreign", now)); err != nil {
		t.Fatal(err)
	}
	foreignCID := id.New()
	if err := deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: foreignCID, Owner: owner.ID, Name: "authority-foreign", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: foreignCID, UserID: foreign, Joined: now}); err != nil {
		t.Fatal(err)
	}
	if got := getAuth(h, "/v1/communities/"+cid.String()+"/bot/actors/"+foreign.String()+"/authority", token); got.Code != http.StatusNotFound {
		t.Fatalf("foreign actor authority = %d, want 404", got.Code)
	}
	if err := deps.Repo.Tx(ctx, func(tx store.Repository) error { return tx.DeleteMember(ctx, cid, target) }); err != nil {
		t.Fatal(err)
	}
	if got := getAuth(h, path, token); got.Code != http.StatusNotFound {
		t.Fatalf("departed actor authority = %d, want 404", got.Code)
	}
	if err := deps.Repo.Tx(ctx, func(tx store.Repository) error {
		current, err := tx.GetActiveBotInstallationByCommunity(ctx, cid)
		if err != nil {
			return err
		}
		current.State = store.BotInstallationPending
		current.Revision++
		current.Updated = now + 1
		return tx.UpdateBotInstallation(ctx, current.ID, current.Revision-1, current)
	}); err != nil {
		t.Fatal(err)
	}
	if got := getAuth(h, path, token); got.Code != http.StatusForbidden && got.Code != http.StatusUnauthorized {
		t.Fatalf("non-ready bot authority = %d, want refusal", got.Code)
	}
}

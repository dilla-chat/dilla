package api_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// botModerationFixture is a ready Bot installation with a moderation role, its session, and a text group with one
// member message: the receipt tests' world (the automatic action route it once served is gone, Bot v2 Task 23).
type botModerationFixture struct {
	h                       http.Handler
	deps                    api.Deps
	community, installation id.ID
	bot, device, target     id.ID
	group, channel          id.ID
	token                   string
}

func newBotModerationFixture(t *testing.T, sourceAgeSeconds int64, wire ...func(*api.Deps)) botModerationFixture {
	t.Helper()
	h, deps := newTestAPIWired(t, func(d *api.Deps) {
		for _, w := range wire {
			w(d)
		}
	})
	ctx := context.Background()
	owner, _, _ := seedAPISession(t, deps)
	cid := seedBotCommunity(t, deps, owner.ID)
	now := deps.Clock.Now().Unix()
	target, targetDevice, _ := seedAPIDevice(t, deps)
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: target.ID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	bot, device, runtime, installation, role := id.New(), id.New(), id.New(), id.New(), id.New()
	verified := now
	if err := deps.Repo.CreateUser(ctx, store.UserRow{ID: bot, Username: "moderator" + bot.String()[:8], Display: "Moderator", Kind: 1,
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateDevice(ctx, store.DeviceRow{ID: device, UserID: bot, DSKPub: bytes.Repeat([]byte{0x55}, 32),
		CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: bot, Joined: now}); err != nil {
		t.Fatal(err)
	}
	roles, err := deps.Repo.ListRoles(ctx, cid)
	if err != nil || len(roles) == 0 {
		t.Fatalf("load default role: roles=%d err=%v", len(roles), err)
	}
	defaultRole := roles[0]
	defaultRole.Allow |= uint64(api.PermViewChannel | api.PermReadHistory)
	if err := deps.Repo.PutRole(ctx, defaultRole); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtime, TokenHash: bytes.Repeat([]byte{0x66}, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutRole(ctx, store.RoleRow{ID: role, CommunityID: cid, Name: "moderators", Position: 1,
		Allow: uint64(api.PermModerateMembers), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMemberRole(ctx, cid, bot, role); err != nil {
		t.Fatal(err)
	}
	roleID := role
	if err := deps.Repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: cid, RuntimeID: &runtime,
		UserID: &bot, DeviceID: &device, RoleID: &roleID, State: store.BotInstallationReady, Revision: 1,
		Capabilities: 1 << 4, ApprovedBy: owner.ID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	token := "moderation-session-" + installation.String()
	if err := deps.Repo.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(token), DeviceID: device, UserID: bot,
		Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(token), InstallationID: installation,
		UserID: bot, DeviceID: device, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(token), 6, 1, 1); err != nil {
		t.Fatal(err)
	}
	channel, group := id.New(), id.New()
	if err := deps.Repo.CreateChannel(ctx, store.ChannelRow{ID: channel, CommunityID: &cid, Kind: api.ChannelText, Mode: api.ModeE2EE,
		Name: "moderated", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateGroup(ctx, store.GroupRow{GroupID: group, Binding: []byte{0x80}, Kind: api.GroupText,
		CommunityID: &cid, TargetID: channel, Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1,
		MediaVersion: 1, PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutAppMessage(ctx, store.AppMessageRow{GroupID: group, Seq: 1, UploaderDevice: targetDevice.ID,
		Blob: []byte("ciphertext"), FrankingTag: make([]byte, 32), Created: now - sourceAgeSeconds, FrankingKeyID: deps.Instance.FrankingKeyID}); err != nil {
		t.Fatal(err)
	}
	return botModerationFixture{h: h, deps: deps, community: cid, installation: installation, bot: bot, device: device,
		target: target.ID, group: group, channel: channel, token: token}
}

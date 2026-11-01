package api_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/moderation/operationid"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

type botModerationFixture struct {
	h                       http.Handler
	deps                    api.Deps
	community, installation id.ID
	bot, device, target     id.ID
	group, channel          id.ID
	token                   string
}

func newBotModerationFixture(t *testing.T, sourceAgeSeconds int64) botModerationFixture {
	t.Helper()
	h, deps := newTestAPI(t)
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
	if err := deps.Repo.CreateUser(ctx, store.UserRow{ID: bot, Username: "moderator" + bot.String()[:8], Display: "Auto Moderator", Kind: 1,
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
	if err := deps.Repo.PutRole(ctx, store.RoleRow{ID: role, CommunityID: cid, Name: "automod", Position: 1,
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
	token := "automod-session-" + installation.String()
	if err := deps.Repo.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(token), DeviceID: device, UserID: bot,
		Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(token), InstallationID: installation,
		UserID: bot, DeviceID: device, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(token), 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	channel, group := id.New(), id.New()
	if err := deps.Repo.CreateChannel(ctx, store.ChannelRow{ID: channel, CommunityID: &cid, Kind: api.ChannelText, Mode: api.ModeE2EE,
		Name: "automod", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}); err != nil {
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

func (f botModerationFixture) submit(t *testing.T, action string, seq, expires, timeoutMillis uint64, pathInstallation id.ID) *httptest.ResponseRecorder {
	t.Helper()
	request := operationid.Automatic(f.group, seq)
	body, err := cborx.Marshal([]any{request, f.group, seq, action, []any{"message_rate"}, timeoutMillis, expires})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/bot-runtime/installations/" + pathInstallation.String() + "/moderation/actions"
	return doCBORAuth(f.h, http.MethodPost, path, body, f.token)
}

func TestBotAutomaticModerationRejectsInvalidAuthorityAndSource(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, botModerationFixture)
	}{
		{name: "wrong installation", run: func(t *testing.T, f botModerationFixture) {
			got := f.submit(t, "warn", 1, uint64(f.deps.Clock.Now().Unix()+20), 0, id.New())
			if got.Code == http.StatusOK {
				t.Fatalf("wrong installation accepted: %x", got.Body.Bytes())
			}
		}},
		{name: "removed capability", run: func(t *testing.T, f botModerationFixture) {
			if err := f.deps.Repo.Tx(t.Context(), func(tx store.Repository) error {
				row, err := tx.GetActiveBotInstallationByCommunity(t.Context(), f.community)
				if err != nil {
					return err
				}
				row.Capabilities = 0
				row.Revision++
				row.Updated++
				return tx.UpdateBotInstallation(t.Context(), row.ID, row.Revision-1, row)
			}); err == nil {
				got := f.submit(t, "warn", 1, uint64(f.deps.Clock.Now().Unix()+20), 0, f.installation)
				if got.Code != http.StatusForbidden {
					t.Fatalf("removed capability = %d %x, want 403", got.Code, got.Body.Bytes())
				}
			} else {
				t.Fatalf("remove capability: %v", err)
			}
		}},
		{name: "stale source message", run: func(t *testing.T, f botModerationFixture) {
			stale := newBotModerationFixture(t, 16)
			got := stale.submit(t, "warn", 1, uint64(stale.deps.Clock.Now().Unix()+20), 0, stale.installation)
			if got.Code != http.StatusBadRequest || errorCode(t, got) != "E_INVALID_REQUEST" {
				t.Fatalf("stale source = %d %x", got.Code, got.Body.Bytes())
			}
		}},
		{name: "forged source sequence", run: func(t *testing.T, f botModerationFixture) {
			got := f.submit(t, "warn", 2, uint64(f.deps.Clock.Now().Unix()+20), 0, f.installation)
			if got.Code != http.StatusNotFound {
				t.Fatalf("missing source = %d %x", got.Code, got.Body.Bytes())
			}
		}},
		{name: "expired request", run: func(t *testing.T, f botModerationFixture) {
			got := f.submit(t, "warn", 1, uint64(f.deps.Clock.Now().Unix()-1), 0, f.installation)
			if got.Code != http.StatusBadRequest || errorCode(t, got) != "E_INVALID_REQUEST" {
				t.Fatalf("expired request = %d %x", got.Code, got.Body.Bytes())
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newBotModerationFixture(t, 0)) })
	}
}

func TestBotAutomaticTimeoutReceiptIsIdempotentAndBlocksClassOne(t *testing.T) {
	f := newBotModerationFixture(t, 0)
	now := uint64(f.deps.Clock.Now().Unix())
	requestExpires := now + 20
	first := f.submit(t, "timeout", 1, requestExpires, 10*60*1000, f.installation)
	if first.Code != http.StatusOK {
		t.Fatalf("timeout action = %d %x", first.Code, first.Body.Bytes())
	}
	replay := f.submit(t, "timeout", 1, requestExpires, 10*60*1000, f.installation)
	if replay.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatalf("timeout replay = %d %x, first %d %x", replay.Code, replay.Body.Bytes(), first.Code, first.Body.Bytes())
	}
	active, err := f.deps.Repo.GetMemberTimeout(t.Context(), f.community, f.target, int64(now+1))
	if err != nil || active.ByUser != f.bot {
		t.Fatalf("timeout row = %+v, %v", active, err)
	}
	group, err := f.deps.Repo.GetGroup(t.Context(), f.group)
	if err != nil {
		t.Fatal(err)
	}
	err = (api.PostPolicy{Repo: f.deps.Repo}).Check(t.Context(), group, f.target, 1, int64(now+1))
	var apiErr *server.Error
	if !errors.As(err, &apiErr) || apiErr.Code != server.CodeForbidden {
		t.Fatalf("class-1 upload under timeout = %v, want forbidden", err)
	}
}

func TestBotAutomaticWarningUsesWarningEventAndStableReceipt(t *testing.T) {
	f := newBotModerationFixture(t, 0)
	expires := uint64(f.deps.Clock.Now().Unix() + 20)
	first := f.submit(t, "warn", 1, expires, 0, f.installation)
	replay := f.submit(t, "warn", 1, expires, 0, f.installation)
	if first.Code != http.StatusOK || replay.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatalf("warning receipt first=%d %x replay=%d %x", first.Code, first.Body.Bytes(), replay.Code, replay.Body.Bytes())
	}
	page, err := f.deps.Repo.ListBotStructuralEvents(t.Context(), f.community, 0, 10)
	if err != nil || len(page.Events) != 1 || page.Events[0].Kind != 6 || page.Events[0].Data[7] != uint64(0) {
		t.Fatalf("warning event page=%+v err=%v", page, err)
	}
}

func TestBotModerationReceiptOnlyCannotCreateAnEffect(t *testing.T) {
	f := newBotModerationFixture(t, 0)
	expires := uint64(f.deps.Clock.Now().Unix() + 20)
	request := operationid.Automatic(f.group, 1)
	body, _ := cborx.Marshal([]any{request, f.group, uint64(1), "warn", []any{"message_rate"}, uint64(0), expires})
	path := "/v1/bot-runtime/installations/" + f.installation.String() + "/moderation/actions?receipt_only=1"
	absent := doCBORAuth(f.h, http.MethodPost, path, body, f.token)
	if absent.Code != 404 {
		t.Fatalf("receipt lookup created effect: status=%d", absent.Code)
	}
	accepted := f.submit(t, "warn", 1, expires, 0, f.installation)
	prior := doCBORAuth(f.h, http.MethodPost, path, body, f.token)
	if prior.Code != 200 || !bytes.Equal(accepted.Body.Bytes(), prior.Body.Bytes()) {
		t.Fatalf("receipt lookup=%d", prior.Code)
	}
}

func TestBotModerationAcceptedReceiptSurvivesCapabilityRevocation(t *testing.T) {
	for _, action := range []string{"warn", "timeout"} {
		t.Run(action, func(t *testing.T) {
			f := newBotModerationFixture(t, 0)
			expires := uint64(f.deps.Clock.Now().Unix() + 20)
			timeout := uint64(0)
			if action == "timeout" {
				timeout = 60_000
			}
			accepted := f.submit(t, action, 1, expires, timeout, f.installation)
			if accepted.Code != 200 {
				t.Fatalf("accept=%d %x", accepted.Code, accepted.Body.Bytes())
			}
			if err := f.deps.Repo.Tx(t.Context(), func(tx store.Repository) error {
				row, err := tx.GetActiveBotInstallationByCommunity(t.Context(), f.community)
				if err != nil {
					return err
				}
				row.Capabilities = 0
				row.Revision++
				row.Updated++
				return tx.UpdateBotInstallation(t.Context(), row.ID, row.Revision-1, row)
			}); err != nil {
				t.Fatal(err)
			}
			body, _ := cborx.Marshal([]any{operationid.Automatic(f.group, 1), f.group, uint64(1), action, []any{"message_rate"}, timeout, expires})
			path := "/v1/bot-runtime/installations/" + f.installation.String() + "/moderation/actions"
			for _, suffix := range []string{"", "?receipt_only=1"} {
				prior := doCBORAuth(f.h, http.MethodPost, path+suffix, body, f.token)
				if prior.Code != 200 || !bytes.Equal(accepted.Body.Bytes(), prior.Body.Bytes()) {
					t.Fatalf("accepted receipt after revocation (%s)=%d %x", suffix, prior.Code, prior.Body.Bytes())
				}
			}
			fresh := f.submit(t, action, 2, expires, timeout, f.installation)
			if fresh.Code != 403 {
				t.Fatalf("new effect after revocation=%d", fresh.Code)
			}
		})
	}
}

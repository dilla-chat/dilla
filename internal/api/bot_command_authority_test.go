package api_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

func TestBotActorChannelAuthorityReturnsHumanBitsAndAppliesFreshChannelPolicy(t *testing.T) {
	h, deps := newTestAPI(t)
	ctx := context.Background()
	owner, _, _ := seedAPISession(t, deps)
	now := deps.Clock.Now().Unix()
	cid := id.New()
	if err := deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: cid, Owner: owner.ID, Name: "command-authority", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	actor, bot, device, installation, runtime, channel := id.New(), id.New(), id.New(), id.New(), id.New(), id.New()
	for _, user := range []store.UserRow{
		newAPITestUser(actor, "command-authority-actor", now),
		{ID: bot, Username: "command-authority-bot", Display: "Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now},
	} {
		if err := deps.Repo.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	verified := now
	if err := deps.Repo.CreateDevice(ctx, store.DeviceRow{ID: device, UserID: bot, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []id.ID{owner.ID, actor, bot} {
		if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: user, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	everyone := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "@everyone", Position: 0,
		Allow: uint64(api.PermViewChannel | api.PermReadHistory | api.PermSendMessages | api.PermConnect), Created: now}
	if err := deps.Repo.PutRole(ctx, everyone); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateChannel(ctx, store.ChannelRow{ID: channel, CommunityID: &cid, Kind: api.ChannelText, Mode: api.ModeE2EE, Visibility: api.VisPrivate, Name: "private", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutOverwrite(ctx, store.OverwriteRow{ChannelID: channel, TargetKind: 1, TargetID: actor, Deny: uint64(api.PermSendMessages | api.PermManageMessages)}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtime, TokenHash: bytes.Repeat([]byte{0x33}, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: cid, RuntimeID: &runtime, UserID: &bot, DeviceID: &device, State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner.ID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	token := "command-authority-bot-session"
	if err := deps.Repo.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(token), DeviceID: device, UserID: bot, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(token), InstallationID: installation, UserID: bot, DeviceID: device, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(token), 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + cid.String() + "/bot/actors/" + actor.String() + "/channels/" + channel.String() + "/authority"
	if denied := getAuth(h, "/v1/communities/"+cid.String()+"/bot/command-status", token); denied.Code != http.StatusForbidden {
		t.Fatalf("managed bot session on human command status route = %d, want 403", denied.Code)
	}
	response := getAuth(h, path, token)
	var got []any
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || cborx.Unmarshal(response.Body.Bytes(), &got) != nil || len(got) != 4 {
		t.Fatalf("channel authority = %d cache=%q tuple=%#v body=%x", response.Code, response.Header().Get("Cache-Control"), got, response.Body.Bytes())
	}
	communityBytes, communityOK := got[0].([]byte)
	actorBytes, actorOK := got[1].([]byte)
	channelBytes, channelOK := got[2].([]byte)
	wantBits := uint64(api.PermViewChannel | api.PermReadHistory | api.PermConnect)
	if !communityOK || !bytes.Equal(communityBytes, cid[:]) || !actorOK || !bytes.Equal(actorBytes, actor[:]) ||
		!channelOK || !bytes.Equal(channelBytes, channel[:]) || got[3] != wantBits {
		t.Fatalf("channel authority tuple = %#v, want human bits %d", got, wantBits)
	}
	// Task 10c: the Bot's DJ check reads control_music from this route, so a channel overwrite
	// that grants it reaches the Bot, and one that denies it is honoured on the next read.
	for _, tc := range []struct {
		name         string
		allow, deny  api.Bits
		wantControls bool
	}{
		{name: "overwrite grants control_music", allow: api.PermControlMusic, deny: api.PermSendMessages | api.PermManageMessages, wantControls: true},
		{name: "overwrite denies control_music", deny: api.PermSendMessages | api.PermManageMessages | api.PermControlMusic},
	} {
		if err := deps.Repo.PutOverwrite(ctx, store.OverwriteRow{ChannelID: channel, TargetKind: 1, TargetID: actor, Allow: uint64(tc.allow), Deny: uint64(tc.deny)}); err != nil {
			t.Fatal(err)
		}
		var tuple []any
		res := getAuth(h, path, token)
		if res.Code != http.StatusOK || cborx.Unmarshal(res.Body.Bytes(), &tuple) != nil || len(tuple) != 4 {
			t.Fatalf("%s: channel authority = %d %x", tc.name, res.Code, res.Body.Bytes())
		}
		bits, _ := tuple[3].(uint64)
		if got := api.Bits(bits).Has(api.PermControlMusic); got != tc.wantControls {
			t.Fatalf("%s: control_music = %v, want %v (bits %#x)", tc.name, got, tc.wantControls, bits)
		}
	}
	if err := deps.Repo.PutOverwrite(ctx, store.OverwriteRow{ChannelID: channel, TargetKind: 1, TargetID: actor, Deny: uint64(api.PermSendMessages | api.PermManageMessages)}); err != nil {
		t.Fatal(err)
	}

	// The bot itself cannot see a channel hidden by a role overwrite, even when
	// the target member's own overwrite grants it back.
	privateChannel := id.New()
	if err := deps.Repo.CreateChannel(ctx, store.ChannelRow{ID: privateChannel, CommunityID: &cid, Kind: api.ChannelText, Mode: api.ModeE2EE, Visibility: api.VisPrivate, Name: "bot-hidden", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutOverwrite(ctx, store.OverwriteRow{ChannelID: privateChannel, TargetKind: 0, TargetID: everyone.ID, Deny: uint64(api.PermViewChannel)}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutOverwrite(ctx, store.OverwriteRow{ChannelID: privateChannel, TargetKind: 1, TargetID: actor, Allow: uint64(api.PermViewChannel)}); err != nil {
		t.Fatal(err)
	}
	privatePath := "/v1/communities/" + cid.String() + "/bot/actors/" + actor.String() + "/channels/" + privateChannel.String() + "/authority"
	if denied := getAuth(h, privatePath, token); denied.Code != http.StatusNotFound {
		t.Fatalf("private channel authority = %d, want hidden 404", denied.Code)
	}
	if err := deps.Repo.PutMemberTimeout(ctx, store.MemberTimeoutRow{CommunityID: cid, UserID: actor, ByUser: owner.ID, Created: now, Expires: now + 60}); err != nil {
		t.Fatal(err)
	}
	if denied := getAuth(h, path, token); denied.Code != http.StatusForbidden {
		t.Fatalf("timed-out actor authority = %d, want 403", denied.Code)
	}
	if err := deps.Repo.Tx(ctx, func(tx store.Repository) error { return tx.DeleteMember(ctx, cid, actor) }); err != nil {
		t.Fatal(err)
	}
	if denied := getAuth(h, path, token); denied.Code != http.StatusNotFound {
		t.Fatalf("nonmember authority = %d, want 404", denied.Code)
	}
	if denied := getAuth(h, "/v1/communities/"+id.New().String()+"/bot/actors/"+actor.String()+"/channels/"+channel.String()+"/authority", token); denied.Code != http.StatusForbidden && denied.Code != http.StatusNotFound {
		t.Fatalf("wrong installation community = %d, want 403 or hidden 404", denied.Code)
	}
	if err := deps.Repo.Tx(ctx, func(tx store.Repository) error {
		current, err := tx.GetActiveBotInstallationByCommunity(ctx, cid)
		if err != nil {
			return err
		}
		current.State = store.BotInstallationDisabled
		current.Revision++
		current.Updated = now + 1
		return tx.UpdateBotInstallation(ctx, current.ID, current.Revision-1, current)
	}); err != nil {
		t.Fatal(err)
	}
	if denied := getAuth(h, path, token); denied.Code != http.StatusForbidden && denied.Code != http.StatusUnauthorized {
		t.Fatalf("disabled bot session authority = %d, want refusal", denied.Code)
	}
}

package dilladtest

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// rosterSession is the real bot session with only the call roster stubbed: a live call needs the
// SFU and an MLS call group, which this host does not run. Every authority read (the actor's
// community authority, their per-channel bits and the Bot's own bits) goes to the real dillad.
type rosterSession struct {
	*client.Session
	call client.ChannelCallState
}

func (r rosterSession) ChannelCall(context.Context, id.ID) (client.ChannelCallState, error) {
	return r.call, nil
}

// Task 10c fix round 1 (S-H): the Bot's decoders rejected bit 25 (Control music), so every DJ and
// every owner or administrator (PermAll) failed CanDJ and every owner/admin Bot command. This runs
// the production authority against a real dillad: a DJ and the owner may DJ, the owner may
// emergency-stop, a plain member may not DJ, and a channel overwrite denying Control music wins.
func TestControlMusicAuthorityAgainstARealDillad(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, HostOptions{DataDir: t.TempDir(), CorePath: corePath(t), ProductionACL: true})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	repo := h.Server().Repo()
	now := h.Server().Now().Unix()

	owner, dj, member, bot := id.New(), id.New(), id.New(), id.New()
	device, installation, runtime, voice, community := id.New(), id.New(), id.New(), id.New(), id.New()
	for _, u := range []store.UserRow{testUser(owner, "owner", now), testUser(dj, "dj", now), testUser(member, "member", now),
		{ID: bot, Username: "glenn-" + bot.String()[:8], Display: "Glenn", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}} {
		if err := repo.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	verified := now
	if err := repo.CreateDevice(ctx, store.DeviceRow{ID: device, UserID: bot, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateCommunity(ctx, store.CommunityRow{ID: community, Owner: owner, Name: "midgard", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []id.ID{owner, dj, member, bot} {
		if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: u, Joined: now}); err != nil {
			t.Fatal(err)
		}
	}
	everyone := store.RoleRow{ID: id.New(), CommunityID: community, Name: "@everyone", Position: 0,
		Allow: uint64(api.PermViewChannel | api.PermReadHistory | api.PermConnect | api.PermSpeak), Created: now}
	djRole := store.RoleRow{ID: id.New(), CommunityID: community, Name: "DJ", Position: 1, Allow: uint64(api.PermControlMusic), Created: now}
	for _, r := range []store.RoleRow{everyone, djRole} {
		if err := repo.PutRole(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.PutMemberRole(ctx, community, dj, djRole.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateChannel(ctx, store.ChannelRow{ID: voice, CommunityID: &community, Kind: api.ChannelVoice, Mode: api.ModeE2EE, Visibility: api.VisPrivate, Name: "lounge", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtime, TokenHash: bytes.Repeat([]byte{0x33}, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: community, RuntimeID: &runtime, UserID: &bot, DeviceID: &device,
		State: store.BotInstallationReady, Revision: 1, Capabilities: 1, ApprovedBy: owner, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	token := "control-music-bot-session"
	if err := repo.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(token), DeviceID: device, UserID: bot, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(token), InstallationID: installation, UserID: bot, DeviceID: device, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.Server().Sessions().RecordCapabilities(ctx, auth.TokenHash(token), 3, 1, 1); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	c, err := client.New(client.Config{BaseURL: srv.URL, InstanceID: instanceID(t, srv.URL), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Discover(ctx); err != nil {
		t.Fatalf("discover: %v", err)
	}
	session, err := c.Session(token)
	if err != nil {
		t.Fatal(err)
	}
	callID := id.New()
	group := id.New()
	roster := rosterSession{Session: session, call: client.ChannelCallState{GroupID: &group, CallID: &callID,
		Participants: []client.CallParticipant{{UserID: owner}, {UserID: dj}, {UserID: member}}}}
	authority := &service.CommunityMusicInteractionAuthority{Session: roster, Store: repo, InstallationID: installation, CommunityID: community}
	call := service.MusicCallBinding{CallID: callID, ChannelID: voice, Active: true}

	canDJ := func(user id.ID) bool {
		t.Helper()
		ok, err := authority.CanDJ(ctx, service.InteractionActor{User: user}, call)
		if err != nil {
			t.Fatalf("CanDJ(%s): %v", user, err)
		}
		return ok
	}
	if !canDJ(dj) {
		t.Fatal("a member holding Control music cannot DJ")
	}
	if !canDJ(owner) {
		t.Fatal("the owner (PermAll) cannot DJ")
	}
	if canDJ(member) {
		t.Fatal("a member without Control music can DJ")
	}
	stop, err := authority.CanEmergencyStop(ctx, service.InteractionActor{User: owner}, call)
	if err != nil || !stop {
		t.Fatalf("owner emergency stop = %v, %v", stop, err)
	}
	if stop, err := authority.CanEmergencyStop(ctx, service.InteractionActor{User: dj}, call); err != nil || stop {
		t.Fatalf("DJ emergency stop = %v, %v; want false", stop, err)
	}
	// A channel overwrite denying Control music to the DJ role wins in this channel.
	if err := repo.PutOverwrite(ctx, store.OverwriteRow{ChannelID: voice, TargetKind: 0, TargetID: djRole.ID, Deny: uint64(api.PermControlMusic)}); err != nil {
		t.Fatal(err)
	}
	if canDJ(dj) {
		t.Fatal("a channel overwrite denying Control music did not apply")
	}
	if !canDJ(owner) {
		t.Fatal("the owner lost DJ to a role overwrite")
	}
}

func testUser(uid id.ID, name string, created int64) store.UserRow {
	return store.UserRow{ID: uid, Username: name + "-" + uid.String()[:8], Display: name,
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: created}
}

// instanceID reads the instance id from GET /v1/instance (field 3), which the bot client pins.
func instanceID(t *testing.T, base string) id.ID {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/instance", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var doc []any
	if err := cborx.Unmarshal(body, &doc); err != nil || len(doc) < 4 {
		t.Fatalf("instance document: %v", err)
	}
	raw, ok := doc[3].([]byte)
	if !ok || len(raw) != len(id.ID{}) {
		t.Fatalf("instance id = %#v", doc[3])
	}
	var out id.ID
	copy(out[:], raw)
	return out
}

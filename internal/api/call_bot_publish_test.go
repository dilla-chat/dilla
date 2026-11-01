package api_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"

	"github.com/jonasthim/dilla/internal/api"
	dillaauth "github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/sfu"
	"github.com/jonasthim/dilla/internal/store"
)

func readyCallBot(t *testing.T, e *env, ch, group id.ID, ownerToken string) (id.ID, id.ID, string) {
	t.Helper()
	cid := ownerCommunityOf(t, e, ch)
	installation := createReadyDJBot(t, e, cid, e.sess[ownerToken].UserID)
	runtime := id.New()
	now := e.Clk.Now().Unix()
	if err := e.Repo.CreateBotRuntime(t.Context(), store.BotRuntimeRow{ID: runtime, TokenHash: make([]byte, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	installation.RuntimeID = &runtime
	installation.Revision++
	if err := e.Repo.UpdateBotInstallation(t.Context(), installation.ID, installation.Revision-1, installation); err != nil {
		t.Fatal(err)
	}
	user, dev := *installation.UserID, *installation.DeviceID
	token := "publish-only-bot"
	e.sess[token] = dillaauth.Session{UserID: user, DeviceID: dev, Scope: dillaauth.ScopeEnrolled, WireVersion: 3, E2EEVersion: 1, MediaVersion: 1, Bot: &dillaauth.BotSessionBinding{InstallationID: installation.ID, CommunityID: cid, State: store.BotInstallationReady}}
	seedLeaf(t, e, group, dev, 3, nil)
	if err := e.Repo.PutOverwrite(t.Context(), store.OverwriteRow{ChannelID: ch, TargetKind: 1, TargetID: user, Allow: uint64(api.PermViewChannel | api.PermConnect | api.PermSpeak | api.PermVideo | api.PermScreenShare)}); err != nil {
		t.Fatal(err)
	}
	return user, dev, token
}

func assertBotPublishOnly(t *testing.T, p *livekit.ParticipantPermission, speak bool) {
	t.Helper()
	var sources []livekit.TrackSource
	if speak {
		sources = []livekit.TrackSource{livekit.TrackSource_MICROPHONE}
	}
	if p == nil || p.GetCanSubscribe() || p.GetCanPublishData() || p.GetCanPublish() != speak || !slices.Equal(p.GetCanPublishSources(), sources) {
		t.Fatalf("bot permission = %v, want subscribe/data disabled and microphone=%v", p, speak)
	}
}

func TestManagedBotCallGrantsArePublishOnly(t *testing.T) {
	e, ch, ownerToken, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL, MaxPublishers: 1})
	user, dev, token := readyCallBot(t, e, ch, group, ownerToken)
	status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", token, []any{})
	if status != http.StatusCreated {
		t.Fatalf("bot start=%d %s", status, e.ErrCode(body))
	}
	out := decodeCall(t, body)
	room := stub.lastMint().Room
	t.Run("token", func(t *testing.T) { assertBotPublishOnly(t, stub.lastMint().Perm, true) })
	t.Run("admission rejects old subscribe token", func(t *testing.T) {
		p, err := calls.AdmitRoom(t.Context(), room, dev)
		if err != nil {
			t.Fatal(err)
		}
		assertBotPublishOnly(t, p, true)
		v := &auth.VideoGrant{Room: room, RoomJoin: true}
		v.UpdateFromPermission(sfu.PublishGrant(true, false, false))
		for _, withSources := range []bool{true, false} {
			if err := sfu.TokenWithin(&auth.ClaimGrants{Video: v}, p, withSources); err == nil {
				t.Fatalf("old subscribing bot token admitted (withSources=%v)", withSources)
			}
		}
	})
	share := "/v1/calls/" + out.CallID.String() + "/share"
	t.Run("share is unavailable without consuming capacity", func(t *testing.T) {
		status, body := e.Do(http.MethodPost, share, token, []any{})
		if status != http.StatusForbidden {
			t.Fatalf("bot share=%d %x, want 403", status, body)
		}
		ownerSession := e.sess[ownerToken]
		ownerSession.WireVersion, ownerSession.E2EEVersion, ownerSession.MediaVersion = 3, 1, 1
		e.sess[ownerToken] = ownerSession
		ownerDev := deviceOf(t, e, ownerToken)
		seedLeaf(t, e, group, ownerDev, 3, nil)
		status, body = e.Do(http.MethodPost, share, ownerToken, []any{})
		if status != http.StatusNoContent {
			t.Fatalf("human share=%d %s, want available slot", status, e.ErrCode(body))
		}
		p := stub.lastPerms()[ownerDev.String()]
		if !p.GetCanSubscribe() || !hasCamera(p) {
			t.Fatalf("human share permission changed: %v", p)
		}
	})
	stub.setPresent(room, &livekit.ParticipantInfo{Identity: dev.String()})
	t.Run("reconcile", func(t *testing.T) {
		if err := api.SyncCallGrants(t.Context(), e.Repo, api.NewResolver(e.Repo), stub, calls, ownerCommunityOf(t, e, ch), &user, &ch); err != nil {
			t.Fatal(err)
		}
		assertBotPublishOnly(t, stub.lastPerms()[dev.String()], true)
	})
	t.Run("speak revoked", func(t *testing.T) {
		denyInChannel(t, e, ch, user, api.PermSpeak)
		if err := api.SyncCallGrants(t.Context(), e.Repo, api.NewResolver(e.Repo), stub, calls, ownerCommunityOf(t, e, ch), &user, &ch); err != nil {
			t.Fatal(err)
		}
		assertBotPublishOnly(t, stub.lastPerms()[dev.String()], false)
		p, err := calls.AdmitRoom(t.Context(), room, dev)
		if err != nil {
			t.Fatal(err)
		}
		assertBotPublishOnly(t, p, false)
	})
}

type failingCallUserRepo struct {
	store.Repository
	user id.ID
}

func (r failingCallUserRepo) GetUser(ctx context.Context, user id.ID) (store.UserRow, error) {
	if user == r.user {
		return store.UserRow{}, errors.New("user lookup unavailable")
	}
	return r.Repository.GetUser(ctx, user)
}

func TestManagedBotCallDemotionNeverEnablesSubscribe(t *testing.T) {
	for _, failure := range []string{"device", "user", "removal"} {
		t.Run(failure, func(t *testing.T) {
			e, ch, ownerToken, group, stub, calls := callEnvCalls(t, api.CallsConfig{LiveKitURL: testLiveKitURL})
			user, dev, token := readyCallBot(t, e, ch, group, ownerToken)
			status, body := e.Do(http.MethodPost, "/v1/channels/"+ch.String()+"/calls", token, []any{})
			if status != http.StatusCreated {
				t.Fatalf("start=%d %s", status, e.ErrCode(body))
			}
			room := stub.lastMint().Room
			stub.setPresent(room, &livekit.ParticipantInfo{Identity: dev.String()})
			repo := e.Repo
			switch failure {
			case "device":
				repo = failingDeviceRepo{Repository: e.Repo, dev: dev}
			case "user":
				repo = failingCallUserRepo{Repository: e.Repo, user: user}
			case "removal":
				denyInChannel(t, e, ch, user, api.PermConnect)
				stub.failRemovals = 1
			}
			if err := api.SyncCallGrants(t.Context(), repo, api.NewResolver(repo), stub, calls, ownerCommunityOf(t, e, ch), nil, &ch); err == nil {
				t.Fatal("expected repair error")
			}
			assertBotPublishOnly(t, stub.lastPerms()[dev.String()], false)
		})
	}
}
func createReadyDJBot(t *testing.T, e *env, community, owner id.ID) store.BotInstallationRow {
	t.Helper()
	now := e.Clk.Now().Unix()
	botID, deviceID := id.New(), id.New()
	if err := e.Repo.CreateUser(t.Context(), store.UserRow{ID: botID, Username: "dj-policy-" + botID.String()[:8], Display: "DJ Policy Bot", Kind: 1, UMKPub: append(botID[:], make([]byte, 16)...), SSKPub: append(deviceID[:], make([]byte, 16)...), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: community, UserID: botID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	verified := now
	if err := e.Repo.CreateDevice(t.Context(), store.DeviceRow{ID: deviceID, UserID: botID, DSKPub: append(deviceID[:], make([]byte, 16)...), CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, UserID: &botID, DeviceID: &deviceID, State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}
	if err := e.Repo.CreateBotInstallation(t.Context(), installation); err != nil {
		t.Fatal(err)
	}
	return installation
}

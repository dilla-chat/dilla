package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"

	"github.com/jonasthim/dilla/internal/api"
	botruntime "github.com/jonasthim/dilla/internal/bot/runtime"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

func TestBotSpotifyAuthorizationFailsClosedWithoutRegisteredOAuthConfig(t *testing.T) {
	h, deps := newTestAPI(t)
	owner, _, ownerToken := seedAPISession(t, deps)
	member, _, memberToken := seedAPISession(t, deps)
	cid := seedBotCommunity(t, deps, owner.ID)
	if err := deps.Repo.PutMember(context.Background(), store.MemberOfCommunityRow{CommunityID: cid, UserID: member.ID, Joined: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", mustCBOR(t, []any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}}), ownerToken); rec.Code != http.StatusOK {
		t.Fatalf("negotiate owner: %d", rec.Code)
	}
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", mustCBOR(t, []any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}}), memberToken); rec.Code != http.StatusOK {
		t.Fatalf("negotiate member: %d", rec.Code)
	}
	base := "/v1/communities/" + cid.String() + "/bot/spotify/attempts"
	config := "/v1/communities/" + cid.String() + "/bot/spotify/config"
	if rec := doBotCBOR(h, http.MethodGet, config, nil, "Bearer "+memberToken); rec.Code != http.StatusForbidden {
		t.Fatalf("member config=%d, want 403", rec.Code)
	}
	body := mustCBOR(t, []any{"unused-step-up", "publicclient"})
	for _, tc := range []struct {
		token string
		want  int
	}{{"", http.StatusUnauthorized}, {memberToken, http.StatusForbidden}, {ownerToken, http.StatusForbidden}} {
		rec := doBotCBOR(h, http.MethodPost, base, body, "Bearer "+tc.token)
		if rec.Code != tc.want {
			t.Fatalf("authorize with token=%q: %d (%x), want %d", tc.token, rec.Code, rec.Body.Bytes(), tc.want)
		}
	}
	// Unknown attempts cannot be used to smuggle callback values or report success.
	callback := mustCBOR(t, []any{"state", "provider-code"})
	rec := doBotCBOR(h, http.MethodPost, base+"/"+id.New().String()+"/callback", callback, "Bearer "+ownerToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown callback = %d (%x), want 404", rec.Code, rec.Body.Bytes())
	}
	var response []any
	if cborx.Unmarshal(rec.Body.Bytes(), &response) != nil || len(response) == 0 {
		t.Fatalf("callback response malformed: %x", rec.Body.Bytes())
	}
}

func TestBotSpotifyPKCECallbackIsSessionBoundAndRuntimeClaimIsRetryable(t *testing.T) {
	var manager *botruntime.Manager
	h, deps := newTestAPIFull(t, nil, func(d *api.Deps) {
		manager = botruntime.New(d.Repo, d.Clock.Now)
		d.BotRuntime = manager
		d.StepUps = api.NewStepUps(d.Clock, api.StepUpTTL)
		d.BotSpotifyClientID = ""
		d.BotSpotifyRedirectURL = ""
	})
	owner, _, ownerToken := seedAPISession(t, deps)
	other, _, otherToken := seedAPISession(t, deps)
	cid := seedBotCommunity(t, deps, owner.ID)
	ctx := context.Background()
	now := deps.Clock.Now().Unix()
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: other.ID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	managerRole := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "music manager", Position: 10, Allow: uint64(api.PermManageCommunity | api.PermViewChannel | api.PermReadHistory | api.PermSendMessages | api.PermAddReactions), Created: now}
	if err := deps.Repo.PutRole(ctx, managerRole); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMemberRole(ctx, cid, other.ID, managerRole.ID); err != nil {
		t.Fatal(err)
	}
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", mustCBOR(t, []any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}}), ownerToken); rec.Code != http.StatusOK {
		t.Fatalf("negotiate owner: %d", rec.Code)
	}
	if rec := postCBORAuth(h, "/v1/sessions/negotiate", mustCBOR(t, []any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}}), otherToken); rec.Code != http.StatusOK {
		t.Fatalf("negotiate other: %d", rec.Code)
	}
	runtimeID := id.New()
	var runtimeToken string
	if _, err := manager.Create(ctx, func(got id.ID, token string) error { runtimeID = got; runtimeToken = token; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: id.New(), CommunityID: cid, RuntimeID: &runtimeID, State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner.ID, ApprovedAt: now, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	configPath := "/v1/communities/" + cid.String() + "/bot/spotify/config"
	configBefore := doBotCBOR(h, http.MethodGet, configPath, nil, "Bearer "+ownerToken)
	if configBefore.Code != http.StatusOK {
		t.Fatalf("config before begin=%d (%x)", configBefore.Code, configBefore.Body.Bytes())
	}
	var before []any
	if err := cborx.Unmarshal(configBefore.Body.Bytes(), &before); err != nil || len(before) != 2 || before[0] != "" || before[1] != "https://dilla.test/settings/bot/music/spotify/callback" {
		t.Fatalf("unexpected initial config: %v %x", err, configBefore.Body.Bytes())
	}
	sess, err := deps.Sessions.Resolve(ctx, ownerToken)
	if err != nil {
		t.Fatal(err)
	}
	step, _ := deps.StepUps.Issue(owner.ID, sess.TokenHash)
	base := "/v1/communities/" + cid.String() + "/bot/spotify/attempts"
	start := doBotCBOR(h, http.MethodPost, base, mustCBOR(t, []any{step, "publicclient"}), "Bearer "+ownerToken)
	if start.Code != http.StatusOK {
		t.Fatalf("begin = %d (%x)", start.Code, start.Body.Bytes())
	}
	var started []any
	if err := cborx.Unmarshal(start.Body.Bytes(), &started); err != nil || len(started) != 3 {
		t.Fatalf("decode begin: %v %x", err, start.Body.Bytes())
	}
	attempt := mustBotID(t, started[0])
	authorize, err := url.Parse(started[1].(string))
	if err != nil {
		t.Fatal(err)
	}
	q := authorize.Query()
	if authorize.Host != "accounts.spotify.com" || q.Get("client_id") != "publicclient" || q.Get("redirect_uri") != "https://dilla.test/settings/bot/music/spotify/callback" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("unexpected authorization URL: %s", authorize)
	}
	configAfter := doBotCBOR(h, http.MethodGet, configPath, nil, "Bearer "+ownerToken)
	var after []any
	if configAfter.Code != http.StatusOK || cborx.Unmarshal(configAfter.Body.Bytes(), &after) != nil || len(after) != 2 || after[0] != "publicclient" {
		t.Fatalf("setting not saved: %d %x", configAfter.Code, configAfter.Body.Bytes())
	}
	inst, err := deps.Repo.GetActiveBotInstallationByCommunity(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig := doBotCBOR(h, http.MethodGet, "/v1/bot-runtime/installations/"+inst.ID.String()+"/spotify/config", nil, "Bearer "+runtimeToken)
	var botConfig []any
	if runtimeConfig.Code != http.StatusOK || cborx.Unmarshal(runtimeConfig.Body.Bytes(), &botConfig) != nil || len(botConfig) != 2 || botConfig[0] != "publicclient" || botConfig[1] != before[1] {
		t.Fatalf("runtime did not receive setting: %d %x", runtimeConfig.Code, runtimeConfig.Body.Bytes())
	}
	wrong := doBotCBOR(h, http.MethodPost, base+"/"+attempt.String()+"/callback", mustCBOR(t, []any{q.Get("state"), "authorization-code"}), "Bearer "+otherToken)
	if wrong.Code != http.StatusNotFound {
		t.Fatalf("other manager callback=%d (%x), want 404", wrong.Code, wrong.Body.Bytes())
	}
	callback := doBotCBOR(h, http.MethodPost, base+"/"+attempt.String()+"/callback", mustCBOR(t, []any{q.Get("state"), "authorization-code"}), "Bearer "+ownerToken)
	if callback.Code != http.StatusAccepted {
		t.Fatalf("callback=%d (%x)", callback.Code, callback.Body.Bytes())
	}
	retryCallback := doBotCBOR(h, http.MethodPost, base+"/"+attempt.String()+"/callback", mustCBOR(t, []any{q.Get("state"), "authorization-code"}), "Bearer "+ownerToken)
	if retryCallback.Code != http.StatusAccepted {
		t.Fatalf("identical callback retry=%d (%x), want 202", retryCallback.Code, retryCallback.Body.Bytes())
	}
	var accepted []any
	if err := cborx.Unmarshal(callback.Body.Bytes(), &accepted); err != nil || len(accepted) != 1 || accepted[0] != uint64(1) {
		t.Fatalf("callback claimed connection: %v %x", err, callback.Body.Bytes())
	}
	claimPath := "/v1/bot-runtime/installations/" + inst.ID.String() + "/spotify/claim"
	var wrongRuntimeToken string
	if _, err := manager.Create(ctx, func(_ id.ID, token string) error { wrongRuntimeToken = token; return nil }); err != nil {
		t.Fatal(err)
	}
	wrongRuntimeConfig := doBotCBOR(h, http.MethodGet, "/v1/bot-runtime/installations/"+inst.ID.String()+"/spotify/config", nil, "Bearer "+wrongRuntimeToken)
	if wrongRuntimeConfig.Code != http.StatusForbidden {
		t.Fatalf("unassigned runtime config=%d, want 403", wrongRuntimeConfig.Code)
	}
	wrongRuntime := doBotCBOR(h, http.MethodPost, claimPath, nil, "Bearer "+wrongRuntimeToken)
	if wrongRuntime.Code != http.StatusForbidden {
		t.Fatalf("unassigned runtime claim=%d, want 403", wrongRuntime.Code)
	}
	claim := doBotCBOR(h, http.MethodPost, claimPath, nil, "Bearer "+runtimeToken)
	if claim.Code != http.StatusOK {
		t.Fatalf("runtime claim=%d (%x)", claim.Code, claim.Body.Bytes())
	}
	var claimed []any
	if err := cborx.Unmarshal(claim.Body.Bytes(), &claimed); err != nil || len(claimed) != 3 || claimed[1] != "authorization-code" {
		t.Fatalf("bad runtime credential handoff: %v %x", err, claim.Body.Bytes())
	}
	verifier, ok := claimed[2].(string)
	if !ok || q.Get("code_challenge") != func() string {
		sum := sha256.Sum256([]byte(verifier))
		return base64.RawURLEncoding.EncodeToString(sum[:])
	}() {
		t.Fatal("runtime verifier does not match advertised PKCE challenge")
	}
	replay := doBotCBOR(h, http.MethodPost, claimPath, nil, "Bearer "+runtimeToken)
	if replay.Code != http.StatusOK || !bytes.Equal(replay.Body.Bytes(), claim.Body.Bytes()) {
		t.Fatalf("runtime claim after lost response=%d, want same 200 receipt", replay.Code)
	}
	afterClaim := doBotCBOR(h, http.MethodPost, base+"/"+attempt.String()+"/callback", mustCBOR(t, []any{q.Get("state"), "authorization-code"}), "Bearer "+ownerToken)
	if afterClaim.Code != http.StatusAccepted {
		t.Fatalf("identical callback after runtime claim=%d (%x), want 202", afterClaim.Code, afterClaim.Body.Bytes())
	}
}

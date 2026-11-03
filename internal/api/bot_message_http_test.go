package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/http"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestMountedBotSendRouteRejectsMalformedBodyAndHumanActor(t *testing.T) {
	h := newGroupsAPI(t)
	path := "/v1/groups/" + h.groupID.String() + "/bot/messages"
	bad := h.do(t, http.MethodPost, path, h.session, []byte{0x9f, 0xff})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("malformed mounted Bot send body = %d, want 400", bad.Code)
	}

	body, err := cborx.Marshal([]any{id.New(), id.New(), nil, uint64(1), uint64(1), bytes.Repeat([]byte{0x44}, 40), uint64(1)})
	if err != nil {
		t.Fatal(err)
	}
	resp := h.do(t, http.MethodPost, path, h.session, body)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("human session reached managed Bot send = %d, want 403", resp.Code)
	}
}

func TestMountedReadyBotSendReceiptAndResolveReachScopedDS(t *testing.T) {
	h := newGroupsAPI(t)
	ctx := context.Background()
	now := h.deps.Clock.Now().Unix()
	owner, _, _ := seedAPIDevice(t, h.deps)
	communityID := id.New()
	if err := h.deps.Repo.CreateCommunity(ctx, store.CommunityRow{ID: communityID, Owner: owner.ID, Name: "mounted-bot-send", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	botID, deviceID, installationID, runtimeID := id.New(), id.New(), id.New(), id.New()
	verified := now
	if err := h.deps.Repo.CreateUser(ctx, store.UserRow{ID: botID, Username: "bot" + botID.String()[:8], Display: "Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateDevice(ctx, store.DeviceRow{ID: deviceID, UserID: botID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: communityID, UserID: botID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtimeID, TokenHash: bytes.Repeat([]byte{0x27}, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installationID, CommunityID: communityID, RuntimeID: &runtimeID, UserID: &botID, DeviceID: &deviceID, State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner.ID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	groupID, targetID := id.New(), id.New()
	if err := h.deps.Repo.CreateGroup(ctx, store.GroupRow{GroupID: groupID, Binding: []byte{0x80}, Kind: 0, CommunityID: &communityID, TargetID: targetID, Ciphersuite: 1, Epoch: 4, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	token := "mounted-ready-bot-session"
	if err := h.deps.Repo.CreateSession(ctx, store.SessionRow{TokenHash: auth.TokenHash(token), DeviceID: deviceID, UserID: botID, Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{TokenHash: auth.TokenHash(token), InstallationID: installationID, UserID: botID, DeviceID: deviceID, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(token), 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	instance, err := h.deps.Repo.GetInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	operationID, attemptID := id.New(), id.New()
	ciphertext := []byte{0x80}
	post := h.do(t, http.MethodPost, "/v1/groups/"+groupID.String()+"/bot/messages", token,
		mustCBOR(t, []any{operationID, attemptID, nil, instance.Generation, uint64(4), ciphertext, uint64(1)}))
	if post.Code != http.StatusForbidden || errorCode(t, post) != "E_LEAF_NOT_CURRENT" {
		t.Fatalf("ready Bot POST = %d %x, want DS current-leaf refusal", post.Code, post.Body.Bytes())
	}
	lookup := h.do(t, http.MethodGet, "/v1/communities/"+communityID.String()+"/bot/receipts/"+operationID.String()+"?attempt="+attemptID.String(), token, nil)
	if lookup.Code != http.StatusNotFound || errorCode(t, lookup) != "E_NOT_FOUND" {
		t.Fatalf("ready Bot receipt GET = %d %x, want scoped missing-receipt 404", lookup.Code, lookup.Body.Bytes())
	}
	ciphertextHash := sha256.Sum256(ciphertext)
	resolve := h.do(t, http.MethodPost, "/v1/communities/"+communityID.String()+"/bot/receipts/"+operationID.String()+"/resolve", token,
		mustCBOR(t, []any{attemptID, nil, groupID, instance.Generation, uint64(4), uint64(1), ciphertextHash[:]}))
	if resolve.Code != http.StatusForbidden || errorCode(t, resolve) != "E_LEAF_NOT_CURRENT" {
		t.Fatalf("ready Bot resolve POST = %d %x, want DS current-leaf refusal", resolve.Code, resolve.Body.Bytes())
	}
}

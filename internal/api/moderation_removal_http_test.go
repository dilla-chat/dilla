package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

func TestSignedModeratorRemovalTombstonesAndPagesDurably(t *testing.T) {
	notifications := 0
	h, deps := newTestAPIWired(t, func(d *api.Deps) {
		d.MessageDeleted = func(id.ID, uint64, int64) { notifications++ }
	})
	ctx := context.Background()
	actor, actorDevice, actorKey := seedAPIDevice(t, deps)
	nonce, _, err := deps.Sessions.Challenge(ctx, actorDevice.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := deps.Sessions.Establish(ctx, auth.EstablishRequest{DeviceID: actorDevice.ID, Nonce: nonce,
		Purpose: auth.PurposeSession, Sig: ed25519.Sign(actorKey, auth.SessionPreimage(deps.Sessions.InstanceID(), actorDevice.ID, nonce, auth.PurposeSession))})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(token.Token), 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	cid := seedBotCommunity(t, deps, actor.ID)
	target, targetDevice, _ := seedAPIDevice(t, deps)
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: target.ID, Joined: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	channel := id.New()
	group := id.New()
	if err := deps.Repo.CreateGroup(ctx, store.GroupRow{GroupID: group, Binding: []byte{0x80}, Kind: 0, CommunityID: &cid, TargetID: channel,
		Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.ReplaceMembers(ctx, group, 0, []store.MemberRow{{GroupID: group, UserID: actor.ID, DeviceID: actorDevice.ID, SignatureKey: actorDevice.DSKPub}}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutAppMessage(ctx, store.AppMessageRow{GroupID: group, Seq: 1, UploaderDevice: targetDevice.ID,
		Blob: []byte("ciphertext"), FrankingTag: make([]byte, 32), Created: deps.Clock.Now().Unix(), FrankingKeyID: deps.Instance.FrankingKeyID}); err != nil {
		t.Fatal(err)
	}
	proof := protocol.RemovalProof{RequestID: id.New(), ActorUser: actor.ID, ActorDevice: actorDevice.ID, CommunityID: cid,
		GroupID: group, TargetSequence: 1, Expires: uint64(deps.Clock.Now().Unix() + 20)}
	unsigned, err := protocol.RemovalPreimage(proof)
	if err != nil {
		t.Fatal(err)
	}
	proofBytes := unsigned[len("dilla moderation removal v1"):]
	signature := ed25519.Sign(actorKey, unsigned)
	postBody, _ := cborx.Marshal([]any{proofBytes, signature})
	path := "/v1/groups/" + group.String() + "/moderation/removals"
	badSignature := append([]byte(nil), signature...)
	badSignature[0] ^= 1
	badBody, _ := cborx.Marshal([]any{proofBytes, badSignature})
	refused := doCBORAuth(h, http.MethodPost, path, badBody, token.Token)
	if refused.Code != http.StatusUnauthorized {
		t.Fatalf("invalid signature = %d %x, want 401", refused.Code, refused.Body.Bytes())
	}
	if notifications != 0 {
		t.Fatalf("invalid signature sent %d deletion notifications", notifications)
	}
	lookup := doCBORAuth(h, http.MethodPost, path+"?receipt_only=1", postBody, token.Token)
	if lookup.Code != 404 || notifications != 0 {
		t.Fatalf("receipt lookup created removal: %d notifications=%d", lookup.Code, notifications)
	}
	accepted := doCBORAuth(h, http.MethodPost, path, postBody, token.Token)
	if accepted.Code != http.StatusOK {
		t.Fatalf("moderator removal = %d %x, want 200", accepted.Code, accepted.Body.Bytes())
	}
	lookup = doCBORAuth(h, http.MethodPost, path+"?receipt_only=1", postBody, token.Token)
	if lookup.Code != 200 || !bytes.Equal(lookup.Body.Bytes(), accepted.Body.Bytes()) {
		t.Fatalf("receipt lookup=%d", lookup.Code)
	}
	if notifications != 1 {
		t.Fatalf("new acceptance notifications = %d, want 1", notifications)
	}
	var result []any
	if err := cborx.Unmarshal(accepted.Body.Bytes(), &result); err != nil || len(result) != 2 || result[1] != uint64(1) {
		t.Fatalf("removal result = %v %#v", err, result)
	}
	message, err := deps.Repo.GetAppMessage(ctx, group, 1)
	if err != nil || message.DeletedAt == nil || len(message.Blob) != 0 {
		t.Fatalf("moderated message = %+v, %v", message, err)
	}
	// Exact replay is idempotent and keeps the original allocated revision.
	replayed := doCBORAuth(h, http.MethodPost, path, postBody, token.Token)
	if replayed.Code != http.StatusOK || replayed.Body.String() != accepted.Body.String() {
		t.Fatalf("replay = %d %x, original %d %x", replayed.Code, replayed.Body.Bytes(), accepted.Code, accepted.Body.Bytes())
	}
	if notifications != 1 {
		t.Fatalf("exact replay sent another notification; count=%d", notifications)
	}
	deletedProof := proof
	deletedProof.RequestID = id.New()
	deletedUnsigned, _ := protocol.RemovalPreimage(deletedProof)
	deletedBody, _ := cborx.Marshal([]any{deletedUnsigned[len("dilla moderation removal v1"):], ed25519.Sign(actorKey, deletedUnsigned)})
	deletedAgain := doCBORAuth(h, http.MethodPost, path, deletedBody, token.Token)
	if deletedAgain.Code != http.StatusNotFound {
		t.Fatalf("new removal of a tombstoned target = %d %x, want 404", deletedAgain.Code, deletedAgain.Body.Bytes())
	}
	expiredProof := proof
	expiredProof.RequestID = id.New()
	expiredProof.Expires = uint64(deps.Clock.Now().Unix() - 1)
	expiredUnsigned, _ := protocol.RemovalPreimage(expiredProof)
	expiredBody, _ := cborx.Marshal([]any{expiredUnsigned[len("dilla moderation removal v1"):], ed25519.Sign(actorKey, expiredUnsigned)})
	expired := doCBORAuth(h, http.MethodPost, path, expiredBody, token.Token)
	if expired.Code != http.StatusBadRequest || errorCode(t, expired) != "E_REMOVAL_PROOF_EXPIRED" {
		t.Fatalf("expired removal proof = %d %x, want 400 E_REMOVAL_PROOF_EXPIRED", expired.Code, expired.Body.Bytes())
	}
	page := doCBORAuth(h, http.MethodGet, path+"?after=0&limit=10", nil, token.Token)
	if page.Code != http.StatusOK {
		t.Fatalf("removal feed = %d %x", page.Code, page.Body.Bytes())
	}
	var feed []any
	if err := cborx.Unmarshal(page.Body.Bytes(), &feed); err != nil || len(feed) != 2 || feed[1] != uint64(1) {
		t.Fatalf("feed = %v %#v", err, feed)
	}
	rows, ok := feed[0].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("feed rows = %#v", feed[0])
	}
	row, ok := rows[0].([]any)
	if !ok || len(row) != 4 || row[0] != uint64(1) || string(row[1].([]byte)) != string(proofBytes) || string(row[2].([]byte)) != string(signature) {
		t.Fatalf("feed row = %#v", rows[0])
	}
	reader, readerDevice, readerToken := seedAPISession(t, deps)
	if err := deps.Repo.ReplaceMembers(ctx, group, 1, []store.MemberRow{
		{GroupID: group, LeafIndex: 0, UserID: actor.ID, DeviceID: actorDevice.ID, SignatureKey: actorDevice.DSKPub},
		{GroupID: group, LeafIndex: 1, UserID: reader.ID, DeviceID: readerDevice.ID, SignatureKey: readerDevice.DSKPub},
	}); err != nil {
		t.Fatal(err)
	}
	humanFeed := doCBORAuth(h, http.MethodGet, path+"?after=0&limit=10", nil, readerToken)
	if humanFeed.Code != http.StatusOK {
		t.Fatalf("human group member removal feed = %d %x, want 200", humanFeed.Code, humanFeed.Body.Bytes())
	}
	actions, err := deps.Repo.ListBotStructuralEvents(ctx, cid, 0, 10)
	if err != nil || len(actions.Events) != 1 || actions.Events[0].Kind != protocol.EventModeration || actions.Events[0].Data[7] != uint64(4) {
		t.Fatalf("moderation action event = %+v, %v", actions, err)
	}
	nonmember, _, outsiderToken := seedAPISession(t, deps)
	_ = nonmember
	if err := deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(outsiderToken), 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	deniedFeed := doCBORAuth(h, http.MethodGet, path, nil, outsiderToken)
	if deniedFeed.Code != http.StatusNotFound {
		t.Fatalf("nonmember removal feed = %d %x, want 404", deniedFeed.Code, deniedFeed.Body.Bytes())
	}
	// The message uploader at the same owner rank is protected by the strict
	// hierarchy rule even though the actor has every permission.
	if err := deps.Repo.PutAppMessage(ctx, store.AppMessageRow{GroupID: group, Seq: 2, UploaderDevice: actorDevice.ID,
		Blob: []byte("own ciphertext"), FrankingTag: make([]byte, 32), Created: deps.Clock.Now().Unix(), FrankingKeyID: deps.Instance.FrankingKeyID}); err != nil {
		t.Fatal(err)
	}
	equalProof := proof
	equalProof.RequestID = id.New()
	equalProof.TargetSequence = 2
	equalUnsigned, _ := protocol.RemovalPreimage(equalProof)
	equalBody, _ := cborx.Marshal([]any{equalUnsigned[len("dilla moderation removal v1"):], ed25519.Sign(actorKey, equalUnsigned)})
	equalRank := doCBORAuth(h, http.MethodPost, path, equalBody, token.Token)
	if equalRank.Code != http.StatusForbidden {
		t.Fatalf("equal-rank removal target = %d %x, want 403", equalRank.Code, equalRank.Body.Bytes())
	}
}

func TestSignedRemovalRequiresManageMessagesPermission(t *testing.T) {
	h, deps := newTestAPI(t)
	ctx := context.Background()
	owner, _, _ := seedAPIDevice(t, deps)
	actor, actorDevice, actorKey := seedAPIDevice(t, deps)
	cid := seedBotCommunity(t, deps, owner.ID)
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: cid, UserID: actor.ID, Joined: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	group := id.New()
	if err := deps.Repo.CreateGroup(ctx, store.GroupRow{GroupID: group, Binding: []byte{0x80}, Kind: 0, CommunityID: &cid, TargetID: id.New(),
		Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1, PolicyVersion: 1, Created: deps.Clock.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	nonce, _, err := deps.Sessions.Challenge(ctx, actorDevice.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := deps.Sessions.Establish(ctx, auth.EstablishRequest{DeviceID: actorDevice.ID, Nonce: nonce,
		Purpose: auth.PurposeSession, Sig: ed25519.Sign(actorKey, auth.SessionPreimage(deps.Sessions.InstanceID(), actorDevice.ID, nonce, auth.PurposeSession))})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(token.Token), 3, 1, 1); err != nil {
		t.Fatal(err)
	}
	proof := protocol.RemovalProof{RequestID: id.New(), ActorUser: actor.ID, ActorDevice: actorDevice.ID,
		CommunityID: cid, GroupID: group, TargetSequence: 1, Expires: uint64(deps.Clock.Now().Unix() + 20)}
	unsigned, _ := protocol.RemovalPreimage(proof)
	body, _ := cborx.Marshal([]any{unsigned[len("dilla moderation removal v1"):], ed25519.Sign(actorKey, unsigned)})
	got := doCBORAuth(h, http.MethodPost, "/v1/groups/"+group.String()+"/moderation/removals", body, token.Token)
	if got.Code != http.StatusForbidden {
		t.Fatalf("actor without manage_messages = %d %x, want 403", got.Code, got.Body.Bytes())
	}
}

func doCBORAuth(h http.Handler, method, path string, body []byte, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/cbor")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBotWithoutRemovalCapabilityCanReadMarkersButCannotRemove(t *testing.T) {
	h, deps := newTestAPI(t)
	ctx := t.Context()
	owner, _, _ := seedAPISession(t, deps)
	community := seedBotCommunity(t, deps, owner.ID)
	now := deps.Clock.Now().Unix()
	bot, device, installation, runtime := id.New(), id.New(), id.New(), id.New()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x43}, ed25519.SeedSize))
	verified := now
	if err := deps.Repo.CreateUser(ctx, store.UserRow{ID: bot, Username: "feedbot" + bot.String()[:8], Display: "Feed Bot", Kind: 1,
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateDevice(ctx, store.DeviceRow{ID: device, UserID: bot, DSKPub: key.Public().(ed25519.PublicKey), CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: bot, Joined: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotRuntime(ctx, store.BotRuntimeRow{ID: runtime, TokenHash: bytes.Repeat([]byte{0x28}, 32), Created: now, Rotated: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.CreateBotInstallation(ctx, store.BotInstallationRow{ID: installation, CommunityID: community, RuntimeID: &runtime,
		UserID: &bot, DeviceID: &device, State: store.BotInstallationReady, Revision: 1,
		ApprovedBy: owner.ID, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	token := "removal-feed-bot-session"
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
	group := id.New()
	if err := deps.Repo.CreateGroup(ctx, store.GroupRow{GroupID: group, Binding: []byte{0x80}, Kind: 0, CommunityID: &community,
		TargetID: id.New(), Ciphersuite: 1, ExternalSenderKeyID: id.New(), E2EEVersion: 1, MediaVersion: 1,
		PolicyVersion: 1, Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Repo.ReplaceMembers(ctx, group, 0, []store.MemberRow{{GroupID: group, UserID: bot, DeviceID: device,
		SignatureKey: key.Public().(ed25519.PublicKey)}}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/groups/" + group.String() + "/moderation/removals"
	if response := doCBORAuth(h, http.MethodGet, path+"?after=0&limit=1", nil, token); response.Code != http.StatusOK {
		t.Fatalf("bot without removal capability cannot read feed: %d %x", response.Code, response.Body.Bytes())
	}
	proof := protocol.RemovalProof{RequestID: id.New(), ActorUser: bot, ActorDevice: device, CommunityID: community,
		GroupID: group, TargetSequence: 1, Expires: uint64(now + 20)}
	preimage, err := protocol.RemovalPreimage(proof)
	if err != nil {
		t.Fatal(err)
	}
	postBody, _ := cborx.Marshal([]any{preimage[len("dilla moderation removal v1"):], ed25519.Sign(key, preimage)})
	if response := doCBORAuth(h, http.MethodPost, path, postBody, token); response.Code != http.StatusForbidden {
		t.Fatalf("bot without removal capability posted removal: %d %x", response.Code, response.Body.Bytes())
	}
}

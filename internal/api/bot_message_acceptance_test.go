package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"net/http"
	"os"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/ds/dstest"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestMountedReadyBotSendAcceptanceReceiptReplayAndResolve(t *testing.T) {
	h := newGroupsAPIWith(t, func(r store.Repository) ds.Channels { return api.StructureChannels{Repo: r} }, api.PostPolicy{})
	testMountedReadyBotSendAcceptance(t, h)
}

func TestMountedReadyBotSendAcceptanceReceiptReplayAndResolvePostgres(t *testing.T) {
	dsn := os.Getenv("DILLA_TEST_PG")
	if dsn == "" {
		t.Skip("DILLA_TEST_PG unset")
	}
	repo := botRegistrationPostgresRepo(t, dsn)
	h := newGroupsAPIWithRepository(t, repo, func(r store.Repository) ds.Channels { return api.StructureChannels{Repo: r} }, api.PostPolicy{})
	testMountedReadyBotSendAcceptance(t, h)
}

func testMountedReadyBotSendAcceptance(t *testing.T, h *groupsAPI) {
	t.Helper()
	ctx := context.Background()
	now := h.deps.Clock.Now().Unix()
	owner, _, _ := seedAPIDevice(t, h.deps)
	communityID, targetID := id.New(), id.New()
	if err := h.deps.Repo.CreateCommunity(ctx, store.CommunityRow{
		ID: communityID, Owner: owner.ID, Name: "mounted-bot-send-acceptance",
		PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateChannel(ctx, store.ChannelRow{
		ID: targetID, CommunityID: &communityID, Kind: api.ChannelText, Mode: api.ModeE2EE,
		Visibility: api.VisPrivate, SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: now,
	}); err != nil {
		t.Fatal(err)
	}
	binding, err := cborx.Marshal(ds.Binding{
		V: 1, InstanceID: h.keys.InstanceID, CommunityID: &communityID, TargetID: targetID,
		Kind: api.ChannelText, PolicyVersion: 1, E2EEVersion: 1, MediaVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dstest.SeedGroup(ctx, h.ds, h.deps.Repo, h.wasm, h.keys.ExternalSenderKeyID, now, dstest.Group{
		GroupID: h.groupID, Binding: binding, GroupInfo: h.fixture.groupInfo, RatchetTree: h.fixture.ratchetTree,
	}); err != nil {
		t.Fatalf("seed scoped public MLS group: %v", err)
	}
	group, err := h.deps.Repo.GetGroup(ctx, h.groupID)
	if err != nil {
		t.Fatal(err)
	}
	members, err := h.deps.Repo.ListMembers(ctx, h.groupID)
	if err != nil || len(members) == 0 {
		t.Fatalf("fixture group members = %d, err=%v", len(members), err)
	}
	// The committed application ciphertext was created by leaf 0 in this exact public group.
	botID, deviceID := members[0].UserID, members[0].DeviceID
	verified := now
	if err := h.deps.Repo.CreateUser(ctx, store.UserRow{
		ID: botID, Username: "acceptbot" + botID.String()[:8], Display: "Acceptance bot", Kind: 1,
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.CreateDevice(ctx, store.DeviceRow{
		ID: deviceID, UserID: botID, DSKPub: make([]byte, 32), CredentialBlob: []byte{1},
		VerifiedAt: &verified, Created: now, LastSeen: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: communityID, UserID: botID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: bytes.Repeat([]byte{0x27}, 32), Created: now, Rotated: now}
	if err := h.deps.Repo.CreateBotRuntime(ctx, runtime); err != nil {
		t.Fatal(err)
	}
	installation := store.BotInstallationRow{
		ID: id.New(), CommunityID: communityID, RuntimeID: &runtime.ID, UserID: &botID, DeviceID: &deviceID,
		State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner.ID, ApprovedAt: now,
		CredentialVerifiedAt: &verified, Created: now, Updated: now,
	}
	if err := h.deps.Repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	token := "mounted-ready-bot-send-acceptance"
	if err := h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.CreateSession(ctx, store.SessionRow{
			TokenHash: auth.TokenHash(token), DeviceID: deviceID, UserID: botID,
			Scope: uint8(auth.ScopeEnrolled), Created: now, Expires: now + 3600, IdleExpires: now + 3600,
		}); err != nil {
			return err
		}
		return tx.CreateBotSessionBinding(ctx, store.BotSessionBindingRow{
			TokenHash: auth.TokenHash(token), InstallationID: installation.ID, UserID: botID, DeviceID: deviceID, Created: now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.deps.Sessions.RecordCapabilities(ctx, auth.TokenHash(token), 6, 1, 1); err != nil {
		t.Fatal(err)
	}
	instance, err := h.deps.Repo.GetInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := apiFixtureFile(t, "application_message.mls")
	operationID, attemptID := id.New(), id.New()
	postPath := "/v1/groups/" + h.groupID.String() + "/bot/messages"
	postBody := mustCBOR(t, []any{operationID, attemptID, nil, instance.Generation, group.Epoch, ciphertext, uint64(1)})
	post := h.do(t, http.MethodPost, postPath, token, postBody)
	if post.Code != http.StatusOK || post.Header().Get("Content-Type") != "application/cbor" {
		t.Fatalf("ready Bot send = %d %x, want 200 application/cbor", post.Code, post.Body.Bytes())
	}
	assertAcceptedBotReceipt(t, post.Body.Bytes(), operationID, attemptID, communityID, h.groupID, instance.Generation, group.Epoch, ciphertext)
	firstReceipt := append([]byte(nil), post.Body.Bytes()...)
	rows, err := h.deps.Repo.ListAppMessages(ctx, h.groupID, 0, 10)
	if err != nil || len(rows) != 1 || rows[0].Seq != 1 || !bytes.Equal(rows[0].Blob, ciphertext) {
		t.Fatalf("accepted application rows = %+v, err=%v; want one exact ciphertext at seq 1", rows, err)
	}
	replay := h.do(t, http.MethodPost, postPath, token, postBody)
	if replay.Code != http.StatusOK || !bytes.Equal(replay.Body.Bytes(), firstReceipt) {
		t.Fatalf("exact retry = %d %x; want original accepted receipt %x", replay.Code, replay.Body.Bytes(), firstReceipt)
	}
	rows, err = h.deps.Repo.ListAppMessages(ctx, h.groupID, 0, 10)
	if err != nil || len(rows) != 1 || rows[0].Seq != 1 {
		t.Fatalf("rows after retry = %d, err=%v; want one row at seq 1", len(rows), err)
	}
	receiptPath := "/v1/communities/" + communityID.String() + "/bot/receipts/" + operationID.String() + "?attempt=" + attemptID.String()
	lookup := h.do(t, http.MethodGet, receiptPath, token, nil)
	if lookup.Code != http.StatusOK || !bytes.Equal(lookup.Body.Bytes(), firstReceipt) {
		t.Fatalf("receipt GET = %d %x; want exact accepted receipt %x", lookup.Code, lookup.Body.Bytes(), firstReceipt)
	}
	ciphertextHash := sha256.Sum256(ciphertext)
	resolvePath := "/v1/communities/" + communityID.String() + "/bot/receipts/" + operationID.String() + "/resolve"
	resolve := h.do(t, http.MethodPost, resolvePath, token, mustCBOR(t, []any{
		attemptID, nil, h.groupID, instance.Generation, group.Epoch, uint64(1), ciphertextHash[:],
	}))
	if resolve.Code != http.StatusOK || !bytes.Equal(resolve.Body.Bytes(), firstReceipt) {
		t.Fatalf("resolve = %d %x; want exact accepted receipt %x", resolve.Code, resolve.Body.Bytes(), firstReceipt)
	}
	foreign := id.New()
	foreignLookup := h.do(t, http.MethodGet, "/v1/communities/"+foreign.String()+"/bot/receipts/"+operationID.String()+"?attempt="+attemptID.String(), token, nil)
	if foreignLookup.Code != http.StatusForbidden || errorCode(t, foreignLookup) != "E_FORBIDDEN" {
		t.Fatalf("foreign community receipt lookup = %d %x; want scoped 403", foreignLookup.Code, foreignLookup.Body.Bytes())
	}
	if err := h.deps.Repo.Tx(ctx, func(tx store.Repository) error {
		return tx.ReplaceMembers(ctx, h.groupID, group.Epoch, members[1:])
	}); err != nil {
		t.Fatal(err)
	}
	revoked := h.do(t, http.MethodPost, postPath, token, mustCBOR(t, []any{
		id.New(), id.New(), nil, instance.Generation, group.Epoch, ciphertext, uint64(1),
	}))
	if revoked.Code != http.StatusForbidden || errorCode(t, revoked) != "E_LEAF_NOT_CURRENT" {
		t.Fatalf("send after bot leaf revocation = %d %x; want 403 E_LEAF_NOT_CURRENT", revoked.Code, revoked.Body.Bytes())
	}
	rows, err = h.deps.Repo.ListAppMessages(ctx, h.groupID, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows after revoked send = %d, err=%v; want no new row", len(rows), err)
	}
}

func assertAcceptedBotReceipt(t *testing.T, raw []byte, operation, attempt, community, group id.ID, generation, epoch uint64, ciphertext []byte) {
	t.Helper()
	var fields []any
	if err := cborx.Unmarshal(raw, &fields); err != nil || len(fields) != 12 {
		t.Fatalf("accepted receipt fields=%d err=%v; want exact 12-field CBOR tuple", len(fields), err)
	}
	receipt, err := protocol.DecodeReceipt(raw)
	if err != nil || receipt.Outcome != protocol.ReceiptAccepted || receipt.SendResponse == nil {
		t.Fatalf("accepted receipt decode = %+v, %v", receipt, err)
	}
	hash := sha256.Sum256(ciphertext)
	if receipt.OperationID != operation || receipt.AttemptID != attempt || receipt.PredecessorAttemptID != nil || receipt.CommunityID != community || receipt.GroupID != group ||
		receipt.ExpectedGeneration != generation || receipt.Epoch != epoch || receipt.Class != 1 || !bytes.Equal(receipt.CiphertextHash, hash[:]) ||
		receipt.SendResponse.Sequence != 1 || len(receipt.SendResponse.FrankingTag) != 32 || receipt.SendResponse.ReceiveTimestamp == 0 {
		t.Fatalf("accepted receipt contents = %+v", receipt)
	}
}

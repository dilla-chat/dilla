package ds_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/ds/dstest"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestManagedBotSendAcceptReplayAndResolveAreDurable(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	registered, _ := h.mustRegister(t)
	group, err := h.repo.GetGroup(ctx, registered.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	community, user, device := id.New(), id.New(), id.New()
	now := h.clk.Now().Unix()
	if err := h.repo.CreateUser(ctx, store.UserRow{
		ID: user, Username: "bot" + user.String(), Display: "managed bot", Kind: 1,
		UMKPub: bytes.Repeat([]byte{1}, 32), SSKPub: bytes.Repeat([]byte{2}, 32),
		SigUMKSSK: bytes.Repeat([]byte{3}, 64), Created: now,
	}); err != nil {
		t.Fatalf("CreateUser(bot): %v", err)
	}
	if err := h.repo.CreateDevice(ctx, store.DeviceRow{
		ID: device, UserID: user, DSKPub: bytes.Repeat([]byte{4}, 32),
		CredentialBlob: []byte{1}, Created: now, LastSeen: now,
	}); err != nil {
		t.Fatalf("CreateDevice(bot): %v", err)
	}
	if err := h.repo.CreateCommunity(ctx, store.CommunityRow{
		ID: community, Owner: user, Name: "managed-send", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now,
	}); err != nil {
		t.Fatalf("CreateCommunity: %v", err)
	}
	if err := h.repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: user, Joined: now}); err != nil {
		t.Fatalf("PutMember: %v", err)
	}
	if err := h.repo.CreateChannel(ctx, store.ChannelRow{
		ID: group.TargetID, CommunityID: &community, Kind: 0, Name: "managed-send", SettingsJSON: []byte(`{}`), Created: now,
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: bytes.Repeat([]byte{5}, 32), Created: now, Rotated: now}
	if err := h.repo.CreateBotRuntime(ctx, runtime); err != nil {
		t.Fatalf("CreateBotRuntime: %v", err)
	}
	verified := now
	installation := store.BotInstallationRow{
		ID: id.New(), CommunityID: community, RuntimeID: &runtime.ID, UserID: &user, DeviceID: &device,
		State: store.BotInstallationReady, Revision: 1, ApprovedBy: user, ApprovedAt: now,
		CredentialVerifiedAt: &verified, Created: now, Updated: now,
	}
	if err := h.repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatalf("CreateBotInstallation: %v", err)
	}
	// The fixture's public group has no account identities in its state blob. Bind one current SQL
	// leaf to the real bot account so this integration test exercises the production membership
	// and managed-installation checks without manufacturing a group state in the guest.
	members, err := h.repo.ListMembers(ctx, registered.GroupID)
	if err != nil || len(members) == 0 {
		t.Fatalf("ListMembers: count=%d err=%v", len(members), err)
	}
	members[0].UserID, members[0].DeviceID = user, device
	if err := h.repo.Tx(ctx, func(tx store.Repository) error {
		return tx.ReplaceMembers(ctx, registered.GroupID, group.Epoch, members)
	}); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
	h.repo.bindCommunity(registered.GroupID, community)
	delivery, err := ds.New(ds.Options{
		Store: h.repo, BotCapability: capability.New(h.repo), Wasm: h.wasm, Clock: h.clk,
		Keys: testInstanceKeys(t), Policy: ds.DefaultPolicy(), Channels: h.channels, ACL: h.acl,
		PostPolicy: api.PostPolicy{Repo: h.repo},
	})
	if err != nil {
		t.Fatalf("ds.New(transactional policy): %v", err)
	}
	t.Cleanup(func() { _ = delivery.Shutdown(context.Background()) })
	competitor, err := ds.New(ds.Options{
		Store: h.repo, BotCapability: capability.New(h.repo), Wasm: h.wasm, Clock: h.clk,
		Keys: testInstanceKeys(t), Policy: ds.DefaultPolicy(), Channels: h.channels, ACL: h.acl,
		PostPolicy: api.PostPolicy{Repo: h.repo},
	})
	if err != nil {
		t.Fatalf("ds.New(competing service): %v", err)
	}
	t.Cleanup(func() { _ = competitor.Shutdown(context.Background()) })
	session := auth.Session{
		UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled, WireVersion: 6,
		Bot: &auth.BotSessionBinding{InstallationID: installation.ID, CommunityID: community, State: store.BotInstallationReady},
	}
	instance, err := h.repo.GetInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := h.message(t, &dsMessageGroup{id: registered.GroupID, device: device}, group.Epoch)
	operation, attempt := id.New(), id.New()
	request := ds.BotMessageRequest{
		Session: session, InstallationID: installation.ID, OperationID: operation, AttemptID: attempt,
		CommunityID: community, GroupID: registered.GroupID, ExpectedGeneration: instance.Generation,
		Epoch: group.Epoch, Class: 1, Ciphertext: ciphertext,
	}
	accepted, err := delivery.AcceptBotMessage(ctx, request)
	if err != nil {
		t.Fatalf("AcceptBotMessage: %v", err)
	}
	if !accepted.NewlyAccepted {
		t.Fatal("first acceptance was not marked newly accepted")
	}
	receipt, err := protocol.DecodeReceipt(accepted.Receipt)
	if err != nil || receipt.Outcome != protocol.ReceiptAccepted || receipt.SendResponse == nil {
		t.Fatalf("accepted receipt = %+v, %v", receipt, err)
	}
	replayed, err := delivery.AcceptBotMessage(ctx, request)
	if err != nil {
		t.Fatalf("AcceptBotMessage(replay): %v", err)
	}
	if replayed.NewlyAccepted || !bytes.Equal(replayed.Receipt, accepted.Receipt) {
		t.Fatal("retry did not return the exact durable receipt without a second acceptance")
	}
	rows, err := h.repo.ListAppMessages(ctx, registered.GroupID, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("accepted application rows = %d, err=%v; want exactly one", len(rows), err)
	}
	resolved, err := delivery.ResolveBotMessage(ctx, ds.BotMessageResolveRequest{
		Session: session, InstallationID: installation.ID, OperationID: operation, AttemptID: attempt,
		CommunityID: community, GroupID: registered.GroupID, ExpectedGeneration: instance.Generation,
		Epoch: group.Epoch, Class: 1, CiphertextSHA256: sha256Bytes(ciphertext),
	})
	if err != nil || !bytes.Equal(resolved.Receipt, accepted.Receipt) {
		t.Fatalf("ResolveBotMessage(accepted): receipt equal=%v err=%v", bytes.Equal(resolved.Receipt, accepted.Receipt), err)
	}
	// Two separate DS objects remove the per-instance mutex from this race: the transaction locks
	// and durable attempt row must linearize accept against resolution at the database boundary.
	raceOperation, raceAttempt := id.New(), id.New()
	raceAccept := ds.BotMessageRequest{
		Session: session, InstallationID: installation.ID, OperationID: raceOperation, AttemptID: raceAttempt,
		CommunityID: community, GroupID: registered.GroupID, ExpectedGeneration: instance.Generation,
		Epoch: group.Epoch, Class: 1, Ciphertext: ciphertext,
	}
	raceResolve := ds.BotMessageResolveRequest{
		Session: session, InstallationID: installation.ID, OperationID: raceOperation, AttemptID: raceAttempt,
		CommunityID: community, GroupID: registered.GroupID, ExpectedGeneration: instance.Generation,
		Epoch: group.Epoch, Class: 1, CiphertextSHA256: sha256Bytes(ciphertext),
	}
	start := make(chan struct{})
	type acceptResult struct {
		result ds.BotMessageResult
		err    error
	}
	type resolveResult struct {
		result ds.BotMessageResult
		err    error
	}
	var wg sync.WaitGroup
	var acceptedRace acceptResult
	var resolvedRace resolveResult
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		acceptedRace.result, acceptedRace.err = delivery.AcceptBotMessage(ctx, raceAccept)
	}()
	go func() {
		defer wg.Done()
		<-start
		resolvedRace.result, resolvedRace.err = competitor.ResolveBotMessage(ctx, raceResolve)
	}()
	close(start)
	wg.Wait()
	if resolvedRace.err != nil {
		t.Fatalf("racing ResolveBotMessage: %v", resolvedRace.err)
	}
	raceReceipt, err := protocol.DecodeReceipt(resolvedRace.result.Receipt)
	if err != nil {
		t.Fatalf("racing resolve receipt: %v", err)
	}
	tracedRows, err := h.repo.ListAppMessages(ctx, registered.GroupID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if acceptedRace.err == nil {
		if raceReceipt.Outcome != protocol.ReceiptAccepted || !bytes.Equal(acceptedRace.result.Receipt, resolvedRace.result.Receipt) || len(tracedRows) != 2 {
			t.Fatalf("accept won race with resolve outcome=%d rows=%d", raceReceipt.Outcome, len(tracedRows))
		}
	} else {
		var dsErr *ds.Error
		if !errors.As(acceptedRace.err, &dsErr) || dsErr.Code != "E_CONFLICT" || raceReceipt.Outcome != protocol.ReceiptClosedNotAccepted || len(tracedRows) != 1 {
			t.Fatalf("resolve won race: accept err=%v, outcome=%d, rows=%d", acceptedRace.err, raceReceipt.Outcome, len(tracedRows))
		}
	}

	// A not-accepted first attempt can be closed and then superseded by one immutable successor.
	closedOp, closedAttempt, successor := id.New(), id.New(), id.New()
	closeRequest := ds.BotMessageResolveRequest{
		Session: session, InstallationID: installation.ID, OperationID: closedOp, AttemptID: closedAttempt,
		CommunityID: community, GroupID: registered.GroupID, ExpectedGeneration: instance.Generation,
		Epoch: group.Epoch, Class: 0, CiphertextSHA256: sha256Bytes(ciphertext),
	}
	closed, err := delivery.ResolveBotMessage(ctx, closeRequest)
	if err != nil {
		t.Fatalf("ResolveBotMessage(close): %v", err)
	}
	closedReceipt, err := protocol.DecodeReceipt(closed.Receipt)
	if err != nil || closedReceipt.Outcome != protocol.ReceiptClosedNotAccepted {
		t.Fatalf("closed receipt outcome=%v err=%v", closedReceipt.Outcome, err)
	}
	closeRequest.AttemptID = successor
	closeRequest.PredecessorAttempt = &closedAttempt
	next, err := delivery.ResolveBotMessage(ctx, closeRequest)
	if err != nil {
		t.Fatalf("ResolveBotMessage(successor): %v", err)
	}
	if got, err := protocol.DecodeReceipt(next.Receipt); err != nil || got.Outcome != protocol.ReceiptClosedNotAccepted {
		t.Fatalf("successor closed receipt outcome=%v err=%v", got.Outcome, err)
	}

	// A restore makes the outcome of a never-seen attempt unknowable. Resolve persists that
	// uncertainty as a terminal operation rather than allowing a retry to send into the new world.
	if err := delivery.OnRestore(ctx, instance.Generation+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	t.Run("stale MLS snapshot send is refused before group heal", func(t *testing.T) {
		// The snapshot's ciphertext and epoch are from before restore; only the
		// expected server generation is current. No application message may land.
		// The racing accept above is allowed to win, so compare against the actual
		// pre-call count instead of assuming only the first message was accepted.
		before, listErr := h.repo.ListAppMessages(ctx, registered.GroupID, 0, 10)
		if listErr != nil {
			t.Fatalf("application messages before blocked stale send: %v", listErr)
		}
		staleSend := request
		staleSend.OperationID, staleSend.AttemptID = id.New(), id.New()
		staleSend.ExpectedGeneration = instance.Generation + 1
		if _, sendErr := delivery.AcceptBotMessage(ctx, staleSend); sendErr == nil {
			t.Fatal("stale MLS snapshot send was accepted after restore")
		} else {
			var refusal *ds.Error
			if !errors.As(sendErr, &refusal) || refusal.Code != "E_LEAF_NOT_CURRENT" {
				t.Fatalf("stale MLS snapshot refusal = %v, want E_LEAF_NOT_CURRENT", sendErr)
			}
		}
		if rows, listErr := h.repo.ListAppMessages(ctx, registered.GroupID, 0, 10); listErr != nil || len(rows) != len(before) {
			t.Fatalf("application messages after blocked stale send = %d, err=%v; want unchanged from pre-call count %d", len(rows), listErr, len(before))
		}
	})
	orphan, predecessor, unknownOp := id.New(), id.New(), id.New()
	unknown, err := delivery.ResolveBotMessage(ctx, ds.BotMessageResolveRequest{
		Session: session, InstallationID: installation.ID, OperationID: unknownOp, AttemptID: orphan,
		PredecessorAttempt: &predecessor, CommunityID: community, GroupID: registered.GroupID,
		ExpectedGeneration: instance.Generation, Epoch: group.Epoch, Class: 0,
		CiphertextSHA256: sha256Bytes(ciphertext),
	})
	if err != nil {
		t.Fatalf("ResolveBotMessage(after restore): %v", err)
	}
	unknownReceipt, err := protocol.DecodeReceipt(unknown.Receipt)
	if err != nil || unknownReceipt.Outcome != protocol.ReceiptUnknownAfterRestore {
		t.Fatalf("unknown receipt outcome=%v err=%v", unknownReceipt.Outcome, err)
	}
}

func sha256Bytes(value []byte) []byte {
	sum := sha256.Sum256(value)
	return sum[:]
}

func TestManagedBotSendAcceptReplayOnPostgres(t *testing.T) {
	dsn := os.Getenv("DILLA_TEST_PG")
	if dsn == "" {
		t.Skip("DILLA_TEST_PG is unset")
	}
	ctx := context.Background()
	repo, _, _, seedDS, wasm, clk := newManagedPostgresDS(t, dsn)
	t.Cleanup(func() { _ = wasm.Close(context.Background()) })
	fixture := dsFixture(t)
	community := id.New()
	fixture.binding.CommunityID = &community
	binding, err := cborx.Marshal(fixture.binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := dstest.SeedGroup(ctx, seedDS, repo, wasm, testInstanceKeys(t).ExternalSenderKeyID, clk.Now().Unix(), dstest.Group{
		GroupID: fixture.groupID, Binding: binding, GroupInfo: fixture.groupInfo, RatchetTree: fixture.ratchetTree,
	}); err != nil {
		t.Fatalf("SeedGroup(Postgres): %v", err)
	}
	group, err := repo.GetGroup(ctx, fixture.groupID)
	if err != nil {
		t.Fatal(err)
	}
	members, err := repo.ListMembers(ctx, fixture.groupID)
	if err != nil || len(members) == 0 {
		t.Fatalf("ListMembers: count=%d err=%v", len(members), err)
	}
	user, device, now := members[0].UserID, members[0].DeviceID, clk.Now().Unix()
	if err := repo.CreateUser(ctx, store.UserRow{
		ID: user, Username: "pgbot" + user.String(), Display: "managed bot", Kind: 1,
		UMKPub: bytes.Repeat([]byte{1}, 32), SSKPub: bytes.Repeat([]byte{2}, 32), SigUMKSSK: bytes.Repeat([]byte{3}, 64), Created: now,
	}); err != nil {
		t.Fatalf("CreateUser(Postgres): %v", err)
	}
	if err := repo.CreateDevice(ctx, store.DeviceRow{ID: device, UserID: user, DSKPub: bytes.Repeat([]byte{4}, 32), CredentialBlob: []byte{1}, Created: now, LastSeen: now}); err != nil {
		t.Fatalf("CreateDevice(Postgres): %v", err)
	}
	if err := repo.CreateCommunity(ctx, store.CommunityRow{ID: community, Owner: user, Name: "pg-managed-send", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatalf("CreateCommunity(Postgres): %v", err)
	}
	if err := repo.PutMember(ctx, store.MemberOfCommunityRow{CommunityID: community, UserID: user, Joined: now}); err != nil {
		t.Fatalf("PutMember(Postgres): %v", err)
	}
	if err := repo.CreateChannel(ctx, store.ChannelRow{ID: group.TargetID, CommunityID: &community, Kind: 0, Name: "pg-managed-send", SettingsJSON: []byte(`{}`), Created: now}); err != nil {
		t.Fatalf("CreateChannel(Postgres): %v", err)
	}
	runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: bytes.Repeat([]byte{6}, 32), Created: now, Rotated: now}
	if err := repo.CreateBotRuntime(ctx, runtime); err != nil {
		t.Fatalf("CreateBotRuntime(Postgres): %v", err)
	}
	verified := now
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, RuntimeID: &runtime.ID, UserID: &user, DeviceID: &device, State: store.BotInstallationReady, Revision: 1, ApprovedBy: user, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}
	if err := repo.CreateBotInstallation(ctx, installation); err != nil {
		t.Fatalf("CreateBotInstallation(Postgres): %v", err)
	}
	delivery, err := ds.New(ds.Options{
		Store: repo, BotCapability: capability.New(repo), Wasm: wasm, Clock: clk,
		Keys: testInstanceKeys(t), Policy: ds.DefaultPolicy(), PostPolicy: api.PostPolicy{Repo: repo},
	})
	if err != nil {
		t.Fatalf("ds.New(Postgres transactional policy): %v", err)
	}
	t.Cleanup(func() { _ = delivery.Shutdown(context.Background()) })
	session := auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled, WireVersion: 6, Bot: &auth.BotSessionBinding{InstallationID: installation.ID, CommunityID: community, State: store.BotInstallationReady}}
	instance, err := repo.GetInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := privateMessage(t, 32, group.Epoch, 64)
	request := ds.BotMessageRequest{Session: session, InstallationID: installation.ID, OperationID: id.New(), AttemptID: id.New(), CommunityID: community, GroupID: fixture.groupID, ExpectedGeneration: instance.Generation, Epoch: group.Epoch, Class: 1, Ciphertext: ciphertext}
	first, err := delivery.AcceptBotMessage(ctx, request)
	if err != nil {
		t.Fatalf("AcceptBotMessage(Postgres): %v", err)
	}
	second, err := delivery.AcceptBotMessage(ctx, request)
	if err != nil {
		t.Fatalf("AcceptBotMessage(Postgres replay): %v", err)
	}
	if !first.NewlyAccepted || second.NewlyAccepted || !bytes.Equal(first.Receipt, second.Receipt) {
		t.Fatal("Postgres retry did not return the exact accepted receipt without a second acceptance")
	}
	rows, err := repo.ListAppMessages(ctx, fixture.groupID, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("Postgres application rows = %d err=%v; want exactly one", len(rows), err)
	}
	resolved, err := delivery.ResolveBotMessage(ctx, ds.BotMessageResolveRequest{Session: session, InstallationID: installation.ID, OperationID: request.OperationID, AttemptID: request.AttemptID, CommunityID: community, GroupID: fixture.groupID, ExpectedGeneration: instance.Generation, Epoch: group.Epoch, Class: 1, CiphertextSHA256: sha256Bytes(ciphertext)})
	if err != nil || !bytes.Equal(first.Receipt, resolved.Receipt) {
		t.Fatalf("ResolveBotMessage(Postgres accepted): receipt equal=%v err=%v", bytes.Equal(first.Receipt, resolved.Receipt), err)
	}
	if got, err := protocol.DecodeReceipt(resolved.Receipt); err != nil || got.Outcome != protocol.ReceiptAccepted {
		t.Fatalf("Postgres resolution outcome=%v err=%v", got.Outcome, err)
	}
}

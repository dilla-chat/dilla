package ds_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/ds/dstest"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestManagedBotSendReceiptQueryIsReadOnlyAndScoped(t *testing.T) {
	state := newSQLiteBotReceiptState(t)
	ctx := context.Background()

	acceptedReq, acceptedReceipt := state.send(t, "accepted")
	closedReq, closedReceipt := state.send(t, "closed")
	unknownReq, unknownReceipt := state.send(t, "unknown")
	for _, tc := range []struct {
		name string
		req  ds.BotMessageRequest
		want []byte
	}{
		{"accepted", acceptedReq, acceptedReceipt},
		{"closed", closedReq, closedReceipt},
		{"unknown", unknownReq, unknownReceipt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeMessages, err := state.repo.ListAppMessages(ctx, state.groupID, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			beforeOp, beforeAttempt := state.operationAndAttempt(t, tc.req)
			got, err := state.service.BotMessageReceipt(ctx, state.session, state.community, tc.req.OperationID, tc.req.AttemptID)
			if err != nil || !bytes.Equal(got.Receipt, tc.want) {
				t.Fatalf("BotMessageReceipt: exact=%v err=%v", bytes.Equal(got.Receipt, tc.want), err)
			}
			decoded, err := protocol.DecodeReceipt(got.Receipt)
			if err != nil {
				t.Fatalf("DecodeReceipt: %v", err)
			}
			switch tc.name {
			case "accepted":
				if decoded.Outcome != protocol.ReceiptAccepted || decoded.SendResponse == nil {
					t.Fatalf("accepted query returned %+v", decoded)
				}
			case "closed":
				if decoded.Outcome != protocol.ReceiptClosedNotAccepted || decoded.SendResponse != nil {
					t.Fatalf("closed query returned %+v", decoded)
				}
			case "unknown":
				if decoded.Outcome != protocol.ReceiptUnknownAfterRestore || decoded.SendResponse != nil {
					t.Fatalf("unknown query returned %+v", decoded)
				}
			}
			afterOp, afterAttempt := state.operationAndAttempt(t, tc.req)
			afterMessages, err := state.repo.ListAppMessages(ctx, state.groupID, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if beforeOp.CurrentAttempt != afterOp.CurrentAttempt || beforeOp.State != afterOp.State || !sameOptionalID(beforeOp.AcceptedAttempt, afterOp.AcceptedAttempt) || beforeAttempt.Outcome != afterAttempt.Outcome || !bytes.Equal(beforeAttempt.Receipt, afterAttempt.Receipt) || len(beforeMessages) != len(afterMessages) {
				t.Fatal("receipt lookup mutated durable send state")
			}
		})
	}

	for _, tc := range []struct {
		name      string
		session   auth.Session
		community id.ID
		operation id.ID
		attempt   id.ID
	}{
		{"unknown operation", state.session, state.community, id.New(), acceptedReq.AttemptID},
		{"unknown attempt", state.session, state.community, acceptedReq.OperationID, id.New()},
		{"foreign community", state.session, id.New(), acceptedReq.OperationID, acceptedReq.AttemptID},
		{"foreign installation", func() auth.Session {
			s := state.session
			binding := *s.Bot
			binding.InstallationID = id.New()
			s.Bot = &binding
			return s
		}(), state.community, acceptedReq.OperationID, acceptedReq.AttemptID},
		{"revoked scope", func() auth.Session {
			s := state.session
			binding := *s.Bot
			binding.State = store.BotInstallationDisabled
			s.Bot = &binding
			return s
		}(), state.community, acceptedReq.OperationID, acceptedReq.AttemptID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := state.service.BotMessageReceipt(ctx, tc.session, tc.community, tc.operation, tc.attempt)
			var dsErr *ds.Error
			if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
				t.Fatalf("scope mismatch error = %v; want indistinguishable E_NOT_FOUND", err)
			}
		})
	}
}

func TestManagedBotSendReceiptQueryOnPostgres(t *testing.T) {
	dsn := os.Getenv("DILLA_TEST_PG")
	if dsn == "" {
		t.Skip("DILLA_TEST_PG is unset")
	}
	state := newPostgresBotReceiptState(t, dsn)
	for _, outcome := range []string{"accepted", "closed", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			req, receipt := state.send(t, outcome)
			beforeOp, beforeAttempt := state.operationAndAttempt(t, req)
			beforeMessages, err := state.repo.ListAppMessages(context.Background(), state.groupID, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			got, err := state.service.BotMessageReceipt(context.Background(), state.session, state.community, req.OperationID, req.AttemptID)
			if err != nil || !bytes.Equal(got.Receipt, receipt) {
				t.Fatalf("Postgres BotMessageReceipt exact=%v err=%v", bytes.Equal(got.Receipt, receipt), err)
			}
			afterOp, afterAttempt := state.operationAndAttempt(t, req)
			afterMessages, err := state.repo.ListAppMessages(context.Background(), state.groupID, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if beforeOp.CurrentAttempt != afterOp.CurrentAttempt || beforeOp.State != afterOp.State || !sameOptionalID(beforeOp.AcceptedAttempt, afterOp.AcceptedAttempt) || beforeAttempt.Outcome != afterAttempt.Outcome || !bytes.Equal(beforeAttempt.Receipt, afterAttempt.Receipt) || len(beforeMessages) != len(afterMessages) {
				t.Fatal("Postgres receipt lookup mutated durable send state")
			}
		})
	}
	_, err := state.service.BotMessageReceipt(context.Background(), state.session, id.New(), id.New(), id.New())
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
		t.Fatalf("Postgres foreign scope error=%v; want E_NOT_FOUND", err)
	}
}

type botReceiptState struct {
	repo       store.Repository
	service    *ds.DS
	wasm       *mlswasi.Runtime
	clock      *clock.Fake
	session    auth.Session
	community  id.ID
	groupID    id.ID
	installID  id.ID
	userID     id.ID
	deviceID   id.ID
	generation uint64
	epoch      uint64
	group      store.GroupRow
	message    []byte
}

func newSQLiteBotReceiptState(t *testing.T) *botReceiptState {
	t.Helper()
	h := newDSHarness(t)
	registered, _ := h.mustRegister(t)
	group, err := h.repo.GetGroup(context.Background(), registered.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	community, user, device := id.New(), id.New(), id.New()
	state := setupBotReceiptIdentity(t, h.repo, h.wasm, h.clk, registered.GroupID, group, community, user, device, true)
	h.repo.bindCommunity(registered.GroupID, community)
	state.clock = h.clk
	state.wasm = h.wasm
	state.repo = h.repo
	state.groupID = registered.GroupID
	state.group = group
	state.message = h.message(t, &dsMessageGroup{id: registered.GroupID, device: device}, group.Epoch)
	return state
}

func newPostgresBotReceiptState(t *testing.T, dsn string) *botReceiptState {
	t.Helper()
	ctx := context.Background()
	repo, _, _, seedDS, wasm, clk := newManagedPostgresDS(t, dsn)
	fixture := dsFixture(t)
	community := id.New()
	fixture.binding.CommunityID = &community
	binding, err := cborx.Marshal(fixture.binding)
	if err != nil {
		t.Fatal(err)
	}
	keys := testInstanceKeys(t)
	if err := dstest.SeedGroup(ctx, seedDS, repo, wasm, keys.ExternalSenderKeyID, clk.Now().Unix(), dstest.Group{GroupID: fixture.groupID, Binding: binding, GroupInfo: fixture.groupInfo, RatchetTree: fixture.ratchetTree}); err != nil {
		t.Fatalf("SeedGroup(Postgres): %v", err)
	}
	group, err := repo.GetGroup(ctx, fixture.groupID)
	if err != nil {
		t.Fatal(err)
	}
	members, err := repo.ListMembers(ctx, fixture.groupID)
	if err != nil || len(members) == 0 {
		t.Fatalf("ListMembers(Postgres): count=%d err=%v", len(members), err)
	}
	state := setupBotReceiptIdentity(t, repo, wasm, clk, fixture.groupID, group, community, members[0].UserID, members[0].DeviceID, false)
	state.wasm = wasm
	state.repo = repo
	state.groupID = fixture.groupID
	state.group = group
	state.message = privateMessage(t, 32, group.Epoch, 64)
	return state
}

func setupBotReceiptIdentity(t *testing.T, repo store.Repository, wasm *mlswasi.Runtime, clk *clock.Fake, groupID id.ID, group store.GroupRow, community, user, device id.ID, rebind bool) *botReceiptState {
	t.Helper()
	now := clk.Now().Unix()
	if rebind {
		if err := repo.CreateUser(context.Background(), store.UserRow{ID: user, Username: "receipt" + user.String(), Display: "managed bot", Kind: 1, UMKPub: bytes.Repeat([]byte{1}, 32), SSKPub: bytes.Repeat([]byte{2}, 32), SigUMKSSK: bytes.Repeat([]byte{3}, 64), Created: now}); err != nil {
			t.Fatalf("CreateUser(bot): %v", err)
		}
		if err := repo.CreateDevice(context.Background(), store.DeviceRow{ID: device, UserID: user, DSKPub: bytes.Repeat([]byte{4}, 32), CredentialBlob: []byte{1}, Created: now, LastSeen: now}); err != nil {
			t.Fatalf("CreateDevice(bot): %v", err)
		}
		members, err := repo.ListMembers(context.Background(), groupID)
		if err != nil || len(members) == 0 {
			t.Fatalf("ListMembers: count=%d err=%v", len(members), err)
		}
		members[0].UserID, members[0].DeviceID = user, device
		if err := repo.Tx(context.Background(), func(tx store.Repository) error {
			return tx.ReplaceMembers(context.Background(), groupID, group.Epoch, members)
		}); err != nil {
			t.Fatalf("ReplaceMembers: %v", err)
		}
	} else {
		if err := repo.CreateUser(context.Background(), store.UserRow{ID: user, Username: "receipt" + user.String(), Display: "managed bot", Kind: 1, UMKPub: bytes.Repeat([]byte{1}, 32), SSKPub: bytes.Repeat([]byte{2}, 32), SigUMKSSK: bytes.Repeat([]byte{3}, 64), Created: now}); err != nil {
			t.Fatalf("CreateUser(Postgres): %v", err)
		}
		if err := repo.CreateDevice(context.Background(), store.DeviceRow{ID: device, UserID: user, DSKPub: bytes.Repeat([]byte{4}, 32), CredentialBlob: []byte{1}, Created: now, LastSeen: now}); err != nil {
			t.Fatalf("CreateDevice(Postgres): %v", err)
		}
	}
	if err := repo.CreateCommunity(context.Background(), store.CommunityRow{ID: community, Owner: user, Name: "receipt", PolicyJSON: []byte(`{}`), PolicyVersion: 1, Created: now}); err != nil {
		t.Fatalf("CreateCommunity: %v", err)
	}
	if err := repo.PutMember(context.Background(), store.MemberOfCommunityRow{CommunityID: community, UserID: user, Joined: now}); err != nil {
		t.Fatalf("PutMember: %v", err)
	}
	if err := repo.CreateChannel(context.Background(), store.ChannelRow{ID: group.TargetID, CommunityID: &community, Kind: 0, Name: "receipt", SettingsJSON: []byte(`{}`), Created: now}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	runtime := store.BotRuntimeRow{ID: id.New(), TokenHash: bytes.Repeat([]byte{5}, 32), Created: now, Rotated: now}
	if err := repo.CreateBotRuntime(context.Background(), runtime); err != nil {
		t.Fatalf("CreateBotRuntime: %v", err)
	}
	verified := now
	installation := store.BotInstallationRow{ID: id.New(), CommunityID: community, RuntimeID: &runtime.ID, UserID: &user, DeviceID: &device, State: store.BotInstallationReady, Revision: 1, ApprovedBy: user, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}
	if err := repo.CreateBotInstallation(context.Background(), installation); err != nil {
		t.Fatalf("CreateBotInstallation: %v", err)
	}
	service, err := ds.New(ds.Options{Store: repo, BotCapability: capability.New(repo), Wasm: wasm, Clock: clk, Keys: testInstanceKeys(t), Policy: ds.DefaultPolicy(), PostPolicy: api.PostPolicy{Repo: repo}})
	if err != nil {
		t.Fatalf("ds.New: %v", err)
	}
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	instance, err := repo.GetInstance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &botReceiptState{service: service, repo: repo, session: auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled, WireVersion: 6, Bot: &auth.BotSessionBinding{InstallationID: installation.ID, CommunityID: community, State: store.BotInstallationReady}}, community: community, installID: installation.ID, userID: user, deviceID: device, generation: instance.Generation, epoch: group.Epoch}
}

func (s *botReceiptState) send(t *testing.T, outcome string) (ds.BotMessageRequest, []byte) {
	t.Helper()
	ctx := context.Background()
	operation, attempt := id.New(), id.New()
	req := ds.BotMessageRequest{Session: s.session, InstallationID: s.installID, OperationID: operation, AttemptID: attempt, CommunityID: s.community, GroupID: s.groupID, ExpectedGeneration: s.generation, Epoch: s.epoch, Class: 1, Ciphertext: s.message}
	var result ds.BotMessageResult
	var err error
	switch outcome {
	case "accepted":
		result, err = s.service.AcceptBotMessage(ctx, req)
	case "closed":
		result, err = s.service.ResolveBotMessage(ctx, ds.BotMessageResolveRequest{Session: s.session, InstallationID: s.installID, OperationID: operation, AttemptID: attempt, CommunityID: s.community, GroupID: s.groupID, ExpectedGeneration: s.generation, Epoch: s.epoch, Class: 1, CiphertextSHA256: sha256Bytes(s.message)})
	case "unknown":
		if err := s.service.OnRestore(ctx, s.generation+1); err != nil {
			t.Fatalf("OnRestore: %v", err)
		}
		result, err = s.service.ResolveBotMessage(ctx, ds.BotMessageResolveRequest{Session: s.session, InstallationID: s.installID, OperationID: operation, AttemptID: attempt, PredecessorAttempt: ptrID(id.New()), CommunityID: s.community, GroupID: s.groupID, ExpectedGeneration: s.generation, Epoch: s.epoch, Class: 1, CiphertextSHA256: sha256Bytes(s.message)})
	default:
		t.Fatalf("unknown test receipt outcome %q", outcome)
	}
	if err != nil {
		t.Fatalf("create %s receipt: %v", outcome, err)
	}
	return req, result.Receipt
}

func (s *botReceiptState) operationAndAttempt(t *testing.T, req ds.BotMessageRequest) (store.BotSendOperationRow, store.BotSendAttemptRow) {
	t.Helper()
	var operation store.BotSendOperationRow
	var attempt store.BotSendAttemptRow
	err := s.repo.Tx(context.Background(), func(tx store.Repository) error {
		var err error
		operation, err = tx.LockBotSendOperation(context.Background(), s.installID, req.OperationID)
		if err != nil {
			return err
		}
		attempt, err = tx.GetBotSendAttempt(context.Background(), s.installID, req.OperationID, req.AttemptID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return operation, attempt
}

func ptrID(value id.ID) *id.ID { return &value }

func sameOptionalID(a, b *id.ID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

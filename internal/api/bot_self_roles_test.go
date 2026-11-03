package api_test

import (
	"log/slog"
	"net/http"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestSelfRolePolicyAndSelectionUseCurrentAllowlistAndOnlySelfSource(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	role := createRole(t, e, cid, ownerTok, "pickable", 2, api.PermViewChannel)
	member, memberTok := e.NewUser("picker")
	joinCommunity(t, e, cid, memberTok)

	policyPath := "/v1/communities/" + cid.String() + "/self-roles/policy"
	if status, body := e.Do(http.MethodPut, policyPath, ownerTok, []any{id.New(), []any{role}}); status != http.StatusOK {
		t.Fatalf("configure policy = %d (%x), want 200", status, body)
	}
	selectPath := "/v1/communities/" + cid.String() + "/self-roles/" + role.String()
	if err := e.Repo.PutMemberRole(t.Context(), cid, member, role); err != nil {
		t.Fatal(err)
	}
	if status, body := e.Do(http.MethodPut, selectPath, memberTok, []any{id.New(), uint64(0)}); status != http.StatusNoContent {
		t.Fatalf("disable absent self grant = %d (%x), want idempotent 204", status, body)
	}
	if status, body := e.Do(http.MethodGet, "/v1/communities/"+cid.String()+"/self-roles", memberTok, nil); status != http.StatusOK {
		t.Fatalf("list self roles = %d (%x), want 200", status, body)
	} else {
		var rows [][]any
		if err := cborx.Unmarshal(body, &rows); err != nil {
			t.Fatalf("decode self-role list: %v", err)
		}
		if len(rows) != 1 || rows[0][3] != uint64(0) {
			t.Fatalf("manual grant reported as self-selected: %#v", rows)
		}
	}
	if status, body := e.Do(http.MethodPut, selectPath, memberTok, []any{id.New(), uint64(1)}); status != http.StatusNoContent {
		t.Fatalf("self-select = %d (%x), want 204", status, body)
	}
	if status, body := e.Do(http.MethodPut, selectPath, memberTok, []any{id.New(), uint64(0)}); status != http.StatusNoContent {
		t.Fatalf("self-remove = %d (%x), want 204", status, body)
	}
	if status, body := e.Do(http.MethodPut, selectPath, memberTok, []any{id.New(), uint64(0)}); status != http.StatusNoContent {
		t.Fatalf("repeated self-remove = %d (%x), want idempotent 204", status, body)
	}
	held, err := e.Repo.ListMemberRoles(t.Context(), cid, member)
	if err != nil || len(held) != 1 || held[0] != role {
		t.Fatalf("manual grant after self removal = %v, %v", held, err)
	}
	if err := e.Repo.DeleteMemberRole(t.Context(), cid, member, role); err != nil {
		t.Fatal(err)
	}
	if status, body := e.Do(http.MethodPut, policyPath, ownerTok, []any{id.New(), []any{}}); status != http.StatusOK {
		t.Fatalf("disable policy = %d (%x), want 200", status, body)
	}
	if status, body := e.Do(http.MethodPut, selectPath, memberTok, []any{id.New(), uint64(1)}); status != http.StatusForbidden {
		t.Fatalf("selection after policy removal = %d (%x), want 403", status, body)
	}
	if _, err := e.Repo.GetMember(t.Context(), cid, member); err != nil {
		t.Fatal(err)
	}
}

func TestRemovingSelfRolePolicyRevokesOnlySelfGrants(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	role := createRole(t, e, cid, ownerTok, "temporary", 2, api.PermViewChannel)
	member, memberTok := e.NewUser("temporary-picker")
	joinCommunity(t, e, cid, memberTok)
	manualMember, manualTok := e.NewUser("manual-and-self-picker")
	joinCommunity(t, e, cid, manualTok)
	reactionMember, reactionTok := e.NewUser("reaction-and-self-picker")
	joinCommunity(t, e, cid, reactionTok)
	policyPath := "/v1/communities/" + cid.String() + "/self-roles/policy"
	selectionPath := "/v1/communities/" + cid.String() + "/self-roles/" + role.String()
	if status, body := e.Do(http.MethodPut, policyPath, ownerTok, []any{id.New(), []any{role}}); status != http.StatusOK {
		t.Fatalf("configure policy = %d (%x)", status, body)
	}
	for _, token := range []string{memberTok, manualTok, reactionTok} {
		if status, body := e.Do(http.MethodPut, selectionPath, token, []any{id.New(), uint64(1)}); status != http.StatusNoContent {
			t.Fatalf("select = %d (%x)", status, body)
		}
	}
	if err := e.Repo.PutMemberRole(t.Context(), cid, manualMember, role); err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.AddMemberRoleGrant(t.Context(), cid, reactionMember, role, "reaction"); err != nil {
		t.Fatal(err)
	}
	if status, body := e.Do(http.MethodPut, policyPath, ownerTok, []any{id.New(), []any{}}); status != http.StatusOK {
		t.Fatalf("remove policy = %d (%x)", status, body)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), cid, member); err != nil || len(held) != 0 {
		t.Fatalf("self-only grant after policy removal = %v, %v", held, err)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), cid, manualMember); err != nil || len(held) != 1 || held[0] != role {
		t.Fatalf("manual grant after policy removal = %v, %v", held, err)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), cid, reactionMember); err != nil || len(held) != 1 || held[0] != role {
		t.Fatalf("reaction grant after policy removal = %v, %v", held, err)
	}
	for _, user := range []id.ID{member, manualMember, reactionMember} {
		if selected, err := e.Repo.HasMemberRoleGrant(t.Context(), cid, user, role, "self"); err != nil || selected {
			t.Fatalf("self source after policy removal = %v, %v", selected, err)
		}
	}
}

func TestSelfRolePolicyRejectsEveryPermissionOutsideSafeCeiling(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler)).Register(e.Mux)
	allowed := api.PermViewChannel | api.PermSendMessages | api.PermAttachFiles | api.PermAddReactions |
		api.PermReadHistory | api.PermConnect | api.PermSpeak | api.PermVideo | api.PermScreenShare
	for bit := api.Bits(1); bit <= api.PermAll; bit <<= 1 {
		if api.PermAll&bit == 0 || allowed&bit != 0 {
			continue
		}
		role := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "unsafe", Position: 5, Allow: uint64(bit), Created: e.Clk.Now().Unix()}
		if err := e.Repo.PutRole(t.Context(), role); err != nil {
			t.Fatal(err)
		}
		path := "/v1/communities/" + cid.String() + "/self-roles/policy"
		status, body := e.Do(http.MethodPut, path, ownerTok, []any{id.New(), []any{role.ID}})
		if status != http.StatusForbidden {
			t.Fatalf("policy with permission bit %#x = %d (%x), want 403", bit, status, body)
		}
	}
	role := store.RoleRow{ID: id.New(), CommunityID: cid, Name: "unsafe-deny", Position: 5, Deny: uint64(api.PermManageNicknames), Created: e.Clk.Now().Unix()}
	if err := e.Repo.PutRole(t.Context(), role); err != nil {
		t.Fatal(err)
	}
	status, body := e.Do(http.MethodPut, "/v1/communities/"+cid.String()+"/self-roles/policy", ownerTok, []any{id.New(), []any{role.ID}})
	if status != http.StatusForbidden {
		t.Fatalf("policy with privileged deny = %d (%x), want 403", status, body)
	}
}

func TestRolePanelRegistrationRejectsHumanAuthoredLookalikeMessage(t *testing.T) {
	e, cid, ownerTok := channelEnv(t)
	roles := api.NewRoles(e.Repo, e.Clk, "dilla.example", slog.New(slog.DiscardHandler))
	roles.Register(e.Mux)
	role := createRole(t, e, cid, ownerTok, "panel-role", 2, api.PermViewChannel)
	if status, body := e.Do(http.MethodPut, "/v1/communities/"+cid.String()+"/self-roles/policy", ownerTok, []any{id.New(), []any{role}}); status != http.StatusOK {
		t.Fatalf("configure self-role policy = %d (%x)", status, body)
	}
	channel, _, status := newChannel(t, e, cid, ownerTok, 0, 0, 0, "role-panel")
	if status != http.StatusCreated {
		t.Fatalf("create channel = %d", status)
	}
	now := e.Clk.Now().Unix()
	owner := e.sess[ownerTok].UserID
	botID, deviceID, installationID := id.New(), id.New(), id.New()
	if err := e.Repo.CreateUser(t.Context(), store.UserRow{ID: botID, Username: "role-panel-bot", Display: "Role Panel Bot", Kind: 1, UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now}); err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.PutMember(t.Context(), store.MemberOfCommunityRow{CommunityID: cid, UserID: botID, Joined: now}); err != nil {
		t.Fatal(err)
	}
	verified := now
	if err := e.Repo.CreateDevice(t.Context(), store.DeviceRow{ID: deviceID, UserID: botID, DSKPub: append([]byte{1}, make([]byte, 31)...), CredentialBlob: []byte{1}, VerifiedAt: &verified, Created: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.CreateBotInstallation(t.Context(), store.BotInstallationRow{ID: installationID, CommunityID: cid, UserID: &botID, DeviceID: &deviceID, State: store.BotInstallationReady, Revision: 1, ApprovedBy: owner, ApprovedAt: now, CredentialVerifiedAt: &verified, Created: now, Updated: now}); err != nil {
		t.Fatal(err)
	}
	author := e.sess[ownerTok].UserID
	if _, err := e.Repo.PutReadableMessage(t.Context(), store.ReadableMessageRow{ChannelID: channel, ChannelHex: channel.String(), Seq: 7, Sender: author, Envelope: []byte{1}, Body: "Role menu", FrankingTag: make([]byte, 32), Created: e.Clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/communities/" + cid.String() + "/role-panels"
	foreignCID := createCommunity(t, e, ownerTok)
	foreignChannel := id.New()
	if err := e.Repo.CreateChannel(t.Context(), store.ChannelRow{ID: foreignChannel, CommunityID: &foreignCID, Kind: api.ChannelText,
		Mode: api.ModeE2EE, Name: "other-community", SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Created: e.Clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	status, body := e.Do(http.MethodPut, path, ownerTok, []any{id.New(), foreignChannel, uint64(9), []any{[]any{"✅", role}}})
	if status != http.StatusNotFound {
		t.Fatalf("register cross-community channel = %d (%x), want 404", status, body)
	}
	status, body = e.Do(http.MethodPut, path, ownerTok, []any{id.ID{}, channel, uint64(7), []any{[]any{"✅", role}}})
	if status != http.StatusBadRequest {
		t.Fatalf("register with zero operation id = %d (%x), want 400", status, body)
	}
	status, body = e.Do(http.MethodPut, path, ownerTok, []any{id.New(), channel, uint64(7), []any{[]any{"✅", role}}})
	if status != http.StatusForbidden {
		t.Fatalf("register human-authored lookalike = %d (%x), want 403", status, body)
	}
	if _, err := e.Repo.PutReadableMessage(t.Context(), store.ReadableMessageRow{ChannelID: channel, ChannelHex: channel.String(), Seq: 8, Sender: botID, UploaderDevice: id.New(), Envelope: []byte{1}, Body: "Lookalike", FrankingTag: make([]byte, 32), Created: e.Clk.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	status, body = e.Do(http.MethodPut, path, ownerTok, []any{id.New(), channel, uint64(8), []any{[]any{"✅", role}}})
	if status != http.StatusForbidden {
		t.Fatalf("register message attributed to a lookalike uploader = %d (%x), want 403", status, body)
	}
	groupID := seedTextGroup(t, e, channel, cid)
	requestID, attemptID := id.New(), id.New()
	seedAcceptedRolePanelBotSend(t, e, installationID, botID, deviceID, cid, groupID, requestID, attemptID, 9)
	status, body = e.Do(http.MethodPut, path, ownerTok, []any{requestID, channel, uint64(9), []any{[]any{"✅", role}}})
	if status != http.StatusForbidden {
		t.Fatalf("register panel without Roles capability = %d (%x), want 403", status, body)
	}
	installation, err := e.Repo.GetBotInstallation(t.Context(), installationID)
	if err != nil {
		t.Fatal(err)
	}
	installation.Capabilities = 1 << 2
	installation.Revision++
	installation.Updated = e.Clk.Now().Unix()
	if err := e.Repo.Tx(t.Context(), func(tx store.Repository) error {
		return tx.UpdateBotInstallation(t.Context(), installationID, installation.Revision-1, installation)
	}); err != nil {
		t.Fatal(err)
	}
	status, body = e.Do(http.MethodPut, path, ownerTok, []any{requestID, channel, uint64(9), []any{[]any{"✅", role}}})
	if status != http.StatusOK {
		t.Fatalf("register installation-authored panel = %d (%x), want 200", status, body)
	}
	ownerSession := e.sess[ownerTok]
	if err := e.Repo.ReplaceMembers(t.Context(), groupID, 0, []store.MemberRow{{GroupID: groupID, LeafIndex: 0,
		UserID: ownerSession.UserID, DeviceID: ownerSession.DeviceID, SignatureKey: make([]byte, 32)}}); err != nil {
		t.Fatal(err)
	}
	reactionClass := uint8(1)
	if err := e.Repo.PutAppMessage(t.Context(), store.AppMessageRow{GroupID: groupID, Seq: 1, UploaderDevice: ownerSession.DeviceID, Blob: []byte{1}, FrankingTag: make([]byte, 32), Created: now, Class: &reactionClass}); err != nil {
		t.Fatal(err)
	}
	reactionPath := path + "/" + channel.String() + "/9/reaction"
	status, body = e.Do(http.MethodPost, reactionPath, ownerTok, []any{groupID, uint64(1), "✅", uint64(1)})
	if status != http.StatusNoContent {
		t.Fatalf("accepted encrypted panel reaction = %d (%x), want 204", status, body)
	}
	held, err := e.Repo.ListMemberRoles(t.Context(), cid, ownerSession.UserID)
	if err != nil || !slices.Contains(held, role) {
		t.Fatalf("role after accepted panel action = %v, %v; want role", held, err)
	}
	if err := e.Repo.PutMemberRole(t.Context(), cid, ownerSession.UserID, role); err != nil {
		t.Fatal(err)
	}
	status, body = e.Do(http.MethodPost, reactionPath, ownerTok, []any{groupID, uint64(1), "✅", uint64(0)})
	if status != http.StatusNoContent {
		t.Fatalf("remove accepted encrypted panel reaction = %d (%x), want 204", status, body)
	}
	held, err = e.Repo.ListMemberRoles(t.Context(), cid, ownerSession.UserID)
	if err != nil || !slices.Contains(held, role) {
		t.Fatalf("manual grant after panel action removal = %v, %v; want role retained", held, err)
	}
	if err := e.Repo.DeleteMemberRole(t.Context(), cid, ownerSession.UserID, role); err != nil {
		t.Fatal(err)
	}
	held, err = e.Repo.ListMemberRoles(t.Context(), cid, ownerSession.UserID)
	if err != nil || slices.Contains(held, role) {
		t.Fatalf("role after both sources removed = %v, %v; want absent", held, err)
	}
	status, body = e.Do(http.MethodPut, path, ownerTok, []any{id.New(), channel, uint64(9), []any{[]any{"✅", role}}})
	if status != http.StatusForbidden {
		t.Fatalf("register with forged operation id = %d (%x), want 403", status, body)
	}
	otherChannel := id.New()
	if err := e.Repo.CreateChannel(t.Context(), store.ChannelRow{ID: otherChannel, CommunityID: &cid, Kind: api.ChannelText, Mode: api.ModeE2EE,
		Visibility: api.VisPrivate, SettingsJSON: []byte(`{}`), HostPolicyVersion: 1, Name: "role-panel-other", Created: now}); err != nil {
		t.Fatalf("create cross-channel target: %v", err)
	}
	seedTextGroup(t, e, otherChannel, cid)
	status, body = e.Do(http.MethodPut, path, ownerTok, []any{requestID, otherChannel, uint64(9), []any{[]any{"✅", role}}})
	if status != http.StatusForbidden {
		t.Fatalf("register receipt on another channel = %d (%x), want 403", status, body)
	}
	closedOperationID, closedAttemptID := id.New(), id.New()
	closedReceipt, err := protocol.EncodeReceipt(protocol.SendReceipt{
		OperationID: closedOperationID, AttemptID: closedAttemptID, CommunityID: cid, GroupID: groupID,
		ExpectedGeneration: 1, CiphertextHash: make([]byte, 32), Class: 0, Outcome: protocol.ReceiptClosedNotAccepted, Epoch: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.Tx(t.Context(), func(tx store.Repository) error {
		operation := store.BotSendOperationRow{InstallationID: installationID, OperationID: closedOperationID, UserID: botID, DeviceID: deviceID, CommunityID: cid, GroupID: groupID, CurrentAttempt: closedAttemptID, State: uint8(store.BotSendOperationActive)}
		if _, _, err := tx.CreateBotSendOperationIfAbsent(t.Context(), operation); err != nil {
			return err
		}
		return tx.InsertBotSendAttempt(t.Context(), store.BotSendAttemptRow{InstallationID: installationID, OperationID: closedOperationID, AttemptID: closedAttemptID, Generation: 1, Epoch: 0, Class: 0, CiphertextSHA256: make([]byte, 32), FingerprintSHA256: make([]byte, 32), Outcome: uint8(store.BotSendAttemptClosed), Receipt: closedReceipt})
	}); err != nil {
		t.Fatal(err)
	}
	status, body = e.Do(http.MethodPut, path, ownerTok, []any{closedOperationID, channel, uint64(9), []any{[]any{"✅", role}}})
	if status != http.StatusForbidden {
		t.Fatalf("register nonaccepted send receipt = %d (%x), want 403", status, body)
	}
	registered, err := e.Repo.GetRolePanel(t.Context(), channel, 9)
	if err != nil || registered.AuthorID != botID || registered.InstallationID != installationID || registered.SendOperationID != requestID {
		t.Fatalf("registered panel identity = %+v, %v", registered, err)
	}
	if err := e.Repo.AddReactionRoleGrant(t.Context(), store.ReactionRoleGrant{CommunityID: cid, UserID: owner, RoleID: role, ChannelID: channel, MessageSeq: 9, Reaction: "✅"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.Tx(t.Context(), func(tx store.Repository) error {
		if err := tx.LockGroup(t.Context(), groupID); err != nil {
			return err
		}
		return tx.CloseGroup(t.Context(), groupID, now+1)
	}); err != nil {
		t.Fatal(err)
	}
	status, body = e.Do(http.MethodPut, path, ownerTok, []any{requestID, channel, uint64(9), []any{[]any{"✅", role}}})
	if status != http.StatusForbidden {
		t.Fatalf("register against a closed text group = %d (%x), want 403", status, body)
	}
	ownerSession.WireVersion, ownerSession.E2EEVersion, ownerSession.MediaVersion = 3, 1, 1
	e.sess[ownerTok] = ownerSession
	if held, err := e.Repo.ListMemberRoles(t.Context(), cid, owner); err != nil || !slices.Contains(held, role) {
		t.Fatalf("role before channel deletion = %v, %v; want present", held, err)
	}
	if status, body := e.Do(http.MethodDelete, "/v1/channels/"+channel.String(), ownerTok, nil); status != http.StatusNoContent {
		t.Fatalf("delete role panel channel = %d (%x), want 204", status, body)
	}
	if held, err := e.Repo.ListMemberRoles(t.Context(), cid, owner); err != nil || slices.Contains(held, role) {
		t.Fatalf("role after channel deletion = %v, %v; want absent", held, err)
	}
	status, body = e.Do(http.MethodPut, path, ownerTok, []any{id.New(), channel, uint64(9), []any{[]any{"✅", role}}})
	if status != http.StatusNotFound {
		t.Fatalf("register panel in deleted channel = %d (%x), want 404", status, body)
	}
}

func seedAcceptedRolePanelBotSend(t *testing.T, e *env, installationID, botID, deviceID, cid, groupID, operationID, attemptID id.ID, seq uint64) {
	t.Helper()
	now := e.Clk.Now().Unix()
	responseBody, err := cborx.Marshal([]any{seq, make([]byte, 32), uint64(now)})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := protocol.EncodeReceipt(protocol.SendReceipt{
		OperationID: operationID, AttemptID: attemptID, CommunityID: cid, GroupID: groupID,
		ExpectedGeneration: 1, CiphertextHash: make([]byte, 32), Class: 0,
		Outcome: protocol.ReceiptAccepted, Epoch: 0,
		SendResponse: &protocol.OriginalSendResponse{Sequence: seq, FrankingTag: make([]byte, 32), ReceiveTimestamp: uint64(now), Body: responseBody},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.Tx(t.Context(), func(tx store.Repository) error {
		operation := store.BotSendOperationRow{InstallationID: installationID, OperationID: operationID, UserID: botID, DeviceID: deviceID, CommunityID: cid, GroupID: groupID, CurrentAttempt: attemptID, State: uint8(store.BotSendOperationActive)}
		if _, _, err := tx.CreateBotSendOperationIfAbsent(t.Context(), operation); err != nil {
			return err
		}
		if err := tx.InsertBotSendAttempt(t.Context(), store.BotSendAttemptRow{InstallationID: installationID, OperationID: operationID, AttemptID: attemptID, Generation: 1, Epoch: 0, Class: 0, CiphertextSHA256: make([]byte, 32), FingerprintSHA256: make([]byte, 32), Outcome: uint8(store.BotSendAttemptAccepted), Receipt: receipt}); err != nil {
			return err
		}
		return tx.SealBotSendOperation(t.Context(), installationID, operationID, attemptID, store.BotSendOperationAccepted)
	}); err != nil {
		t.Fatal(err)
	}
}

package api

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/jonasthim/dilla/internal/auth"
	botprotocol "github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

const selfRoleAllowedBits = PermViewChannel | PermSendMessages | PermAttachFiles |
	PermAddReactions | PermReadHistory | PermConnect | PermSpeak | PermVideo | PermScreenShare

type selfRolePolicyRequest struct {
	_         struct{} `cbor:",toarray"`
	RequestID id.ID
	Roles     []id.ID
}

type selfRoleSelectionRequest struct {
	_         struct{} `cbor:",toarray"`
	RequestID id.ID
	Enabled   uint8
}

type rolePanelOptionRequest struct {
	_     struct{} `cbor:",toarray"`
	Emoji string
	Role  id.ID
}

type rolePanelRegistrationRequest struct {
	_          struct{} `cbor:",toarray"`
	RequestID  id.ID
	ChannelID  id.ID
	MessageSeq uint64
	Options    []rolePanelOptionRequest
}

type rolePanelReactionRequest struct {
	_        struct{} `cbor:",toarray"`
	GroupID  id.ID
	EventSeq uint64
	Emoji    string
	Enabled  uint8
}

func (h *Roles) RegisterSelfRoles(mux *server.Mux) {
	mux.HandleFunc("GET /v1/communities/{id}/self-roles", h.listSelfRoles)
	mux.HandleFunc("PUT /v1/communities/{id}/self-roles/policy", h.configureSelfRoles)
	mux.HandleFunc("PUT /v1/communities/{id}/self-roles/{role_id}", h.selectSelfRole)
	mux.HandleFunc("PUT /v1/communities/{id}/role-panels", h.registerRolePanel)
	mux.HandleFunc("POST /v1/communities/{id}/role-panels/{channel_id}/{message_seq}/reaction", h.rolePanelReaction)
	mux.HandleFunc("PUT /v1/communities/{id}/bot-join-role", h.configureBotJoinRole)
	mux.HandleFunc("GET /v1/communities/{id}/bot-join-role", h.getBotJoinRole)
}

// rolePanelReaction records the authenticated member's explicit selection for
// a registered panel after the encrypted reaction message has been accepted by
// the delivery service. The server cannot inspect the MLS plaintext; the API
// action itself is constrained to the exact approved panel mapping.
func (h *Roles) rolePanelReaction(w http.ResponseWriter, r *http.Request) {
	var req rolePanelReactionRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		refuseCBOR(w, r, err)
		return
	}
	cid, actor, err := h.selfRoleCommunity(r, h.repo)
	if err != nil {
		h.fail(w, r, "apply role panel reaction", err)
		return
	}
	channelID, err := server.PathID(r, "channel_id")
	if err != nil {
		h.fail(w, r, "apply role panel reaction", err)
		return
	}
	seq, err := strconv.ParseUint(r.PathValue("message_seq"), 10, 64)
	if err != nil || seq == 0 || seq > math.MaxInt64 || req.GroupID.IsZero() || req.EventSeq == 0 || req.EventSeq > math.MaxInt64 ||
		req.Emoji == "" || len(req.Emoji) > 32 || !utf8.ValidString(req.Emoji) || req.Enabled > 1 {
		h.fail(w, r, "apply role panel reaction", server.Errorf(server.CodeInvalidRequest, "invalid panel reaction"))
		return
	}
	var changed bool
	err = h.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		channel, err := tx.GetChannel(r.Context(), channelID)
		if err != nil || channel.CommunityID == nil || *channel.CommunityID != cid || channel.Kind != ChannelText || channel.DeletedAt != nil {
			return server.Errorf(server.CodeNotFound, "channel not found")
		}
		if _, err := tx.GetMember(r.Context(), cid, actor.UserID); err != nil {
			return notFound(err)
		}
		group, err := tx.GetGroup(r.Context(), req.GroupID)
		if err != nil || group.Kind != 0 || group.CommunityID == nil || *group.CommunityID != cid || group.TargetID != channelID || group.ClosedAt != nil {
			return server.Errorf(server.CodeForbidden, "accepted reaction is not from this open channel group")
		}
		if err := tx.LockGroup(r.Context(), group.GroupID); err != nil {
			return err
		}
		group, err = tx.GetGroup(r.Context(), req.GroupID)
		if err != nil || group.Kind != 0 || group.CommunityID == nil || *group.CommunityID != cid || group.TargetID != channelID || group.ClosedAt != nil {
			return server.Errorf(server.CodeForbidden, "accepted reaction is not from this open channel group")
		}
		members, err := tx.ListMembers(r.Context(), req.GroupID)
		if err != nil {
			return err
		}
		memberDevice := false
		for _, member := range members {
			if member.UserID == actor.UserID && member.DeviceID == actor.DeviceID {
				memberDevice = true
				break
			}
		}
		if !memberDevice {
			return server.Errorf(server.CodeForbidden, "reaction sender is not a current channel device")
		}
		event, err := tx.GetAppMessage(r.Context(), req.GroupID, req.EventSeq)
		if err != nil || event.UploaderDevice != actor.DeviceID || event.DeletedAt != nil || event.Class == nil || *event.Class != 1 {
			return server.Errorf(server.CodeForbidden, "reaction event is not an accepted message from this device")
		}
		panel, err := tx.GetRolePanel(r.Context(), channelID, seq)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if panel.CommunityID != cid || panel.SendOperationID.IsZero() {
			return nil
		}
		options, err := tx.ListRolePanelOptions(r.Context(), channelID, seq)
		if err != nil {
			return err
		}
		var roleID id.ID
		for _, option := range options {
			if option.Emoji == req.Emoji {
				roleID = option.RoleID
				break
			}
		}
		if roleID.IsZero() {
			return server.Errorf(server.CodeForbidden, "emoji is not mapped by this panel")
		}
		source := store.ReactionRoleGrant{CommunityID: cid, UserID: actor.UserID, RoleID: roleID, ChannelID: channelID, MessageSeq: seq, Reaction: req.Emoji}
		if req.Enabled == 0 {
			err = tx.RemoveReactionRoleGrant(r.Context(), source)
			if errors.Is(err, store.ErrNotFound) {
				err = nil
			}
			changed = err == nil
			return err
		}
		installation, err := tx.GetBotInstallation(r.Context(), panel.InstallationID)
		if err != nil || installation.CommunityID != cid || installation.State != store.BotInstallationReady || installation.RemovedAt != nil || installation.UserID == nil || *installation.UserID != panel.AuthorID {
			return server.Errorf(server.CodeForbidden, "role panel is no longer active")
		}
		bot, err := tx.GetUser(r.Context(), panel.AuthorID)
		if err != nil || bot.Kind != 1 {
			return server.Errorf(server.CodeForbidden, "role panel author is not the installed bot")
		}
		if err := verifyRolePanelBotSend(r.Context(), tx, installation, bot.ID, channel, rolePanelRegistrationRequest{
			RequestID: panel.SendOperationID, ChannelID: channelID, MessageSeq: seq,
		}); err != nil {
			return err
		}
		policy, err := tx.ListSelfRolePolicy(r.Context(), cid)
		if err != nil {
			return err
		}
		approved := false
		var approvedBy id.ID
		for _, row := range policy {
			if row.RoleID == roleID {
				approved, approvedBy = true, row.ApprovedBy
				break
			}
		}
		if !approved {
			return server.Errorf(server.CodeForbidden, "panel role is no longer approved for self-service")
		}
		actorSnap, err := LoadSnapshot(r.Context(), tx, cid, actor.UserID, &channelID)
		if err != nil {
			return notFound(err)
		}
		actorBits := actorSnap.ResolveChannel(actor.UserID)
		if !actorBits.Has(PermViewChannel) || !actorBits.Has(PermAddReactions) {
			return server.Errorf(server.CodeForbidden, "view_channel and add_reactions required")
		}
		approverSnap, err := LoadSnapshot(r.Context(), tx, cid, approvedBy, nil)
		if err != nil || !approverSnap.Resolve(approvedBy).Has(PermManageRoles) || h.selfRoleCandidate(r, tx, cid, approvedBy, roleID, approverSnap.Resolve(approvedBy), approverSnap) != nil {
			return server.Errorf(server.CodeForbidden, "panel role approval is no longer valid")
		}
		err = tx.AddReactionRoleGrant(r.Context(), source)
		changed = err == nil
		return err
	})
	if err != nil {
		h.fail(w, r, "apply role panel reaction", err)
		return
	}
	if changed {
		h.reactionRoleChanged(r.Context(), cid, actor.UserID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Roles) registerRolePanel(w http.ResponseWriter, r *http.Request) {
	var req rolePanelRegistrationRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		refuseCBOR(w, r, err)
		return
	}
	cid, actor, err := h.selfRoleCommunity(r, h.repo)
	if err != nil {
		h.fail(w, r, "register role panel", err)
		return
	}
	if req.RequestID.IsZero() || req.ChannelID.IsZero() || req.MessageSeq == 0 || req.MessageSeq > math.MaxInt64 || len(req.Options) == 0 || len(req.Options) > 100 {
		h.fail(w, r, "register role panel", server.Errorf(server.CodeInvalidRequest, "invalid role panel"))
		return
	}
	seen := map[string]bool{}
	for _, option := range req.Options {
		if option.Emoji == "" || len(option.Emoji) > 32 || !utf8.ValidString(option.Emoji) || option.Role.IsZero() || seen[option.Emoji] {
			h.fail(w, r, "register role panel", server.Errorf(server.CodeInvalidRequest, "invalid or duplicate emoji mapping"))
			return
		}
		seen[option.Emoji] = true
	}
	var panel store.RolePanelRow
	var affected []id.ID
	options := make([]store.RolePanelOptionRow, len(req.Options))
	for i, option := range req.Options {
		options[i] = store.RolePanelOptionRow{Emoji: option.Emoji, RoleID: option.Role}
	}
	err = h.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		if _, _, err := h.selfRoleCommunity(r, tx); err != nil {
			return err
		}
		snap, err := LoadSnapshot(r.Context(), tx, cid, actor.UserID, nil)
		if err != nil {
			return notFound(err)
		}
		actorBits := snap.Resolve(actor.UserID)
		if !actorBits.Has(PermManageRoles) {
			return server.Errorf(server.CodeForbidden, "manage_roles required")
		}
		channel, err := tx.GetChannel(r.Context(), req.ChannelID)
		if err != nil || channel.CommunityID == nil || *channel.CommunityID != cid || channel.Kind != ChannelText || channel.DeletedAt != nil {
			return server.Errorf(server.CodeNotFound, "channel not found")
		}
		installation, err := tx.GetActiveBotInstallationByCommunity(r.Context(), cid)
		if err != nil || installation.State != store.BotInstallationReady || installation.UserID == nil || installation.DeviceID == nil {
			return server.Errorf(server.CodeForbidden, "ready community bot required")
		}
		if installation.Capabilities&uint16(botCapRoles) == 0 {
			return server.Errorf(server.CodeForbidden, "roles capability required")
		}
		botUser, err := tx.GetUser(r.Context(), *installation.UserID)
		if err != nil || botUser.Kind != 1 {
			return server.Errorf(server.CodeForbidden, "ready community bot required")
		}
		if _, err := tx.GetMember(r.Context(), cid, botUser.ID); err != nil {
			return server.Errorf(server.CodeForbidden, "bot must be a community member")
		}
		if err := verifyRolePanelBotSend(r.Context(), tx, installation, botUser.ID, channel, req); err != nil {
			return err
		}
		if channel.Mode == ModeReadable {
			messages, err := tx.ListReadableMessages(r.Context(), req.ChannelID, req.MessageSeq, 1)
			if err != nil {
				return err
			}
			if len(messages) > 0 && messages[0].Seq == req.MessageSeq && (messages[0].Sender != botUser.ID || messages[0].UploaderDevice != *installation.DeviceID || messages[0].Deleted != nil) {
				return server.Errorf(server.CodeForbidden, "panel must reference a live message authored by the installed bot")
			}
		}
		policy, err := tx.ListSelfRolePolicy(r.Context(), cid)
		if err != nil {
			return err
		}
		approved := make(map[id.ID]store.SelfRolePolicyRow, len(policy))
		for _, row := range policy {
			approved[row.RoleID] = row
		}
		for _, option := range req.Options {
			if err := h.selfRoleCandidate(r, tx, cid, actor.UserID, option.Role, actorBits, snap); err != nil {
				return err
			}
			row, ok := approved[option.Role]
			if !ok {
				return server.Errorf(server.CodeForbidden, "panel role is not approved for self-service")
			}
			approverSnap, err := LoadSnapshot(r.Context(), tx, cid, row.ApprovedBy, nil)
			if err != nil {
				return server.Errorf(server.CodeForbidden, "self-service approval is no longer valid")
			}
			approverBits := approverSnap.Resolve(row.ApprovedBy)
			if !approverBits.Has(PermManageRoles) || h.selfRoleCandidate(r, tx, cid, row.ApprovedBy, option.Role, approverBits, approverSnap) != nil {
				return server.Errorf(server.CodeForbidden, "panel role is no longer within the approved policy")
			}
		}
		panel = store.RolePanelRow{CommunityID: cid, ChannelID: req.ChannelID, MessageSeq: req.MessageSeq, AuthorID: botUser.ID, InstallationID: installation.ID, SendOperationID: req.RequestID}
		affected, err = tx.ListReactionRolePanelGrantUsers(r.Context(), cid, req.ChannelID, req.MessageSeq)
		if err != nil {
			return err
		}
		return tx.SetRolePanel(r.Context(), panel, options)
	})
	if err != nil {
		h.fail(w, r, "register role panel", err)
		return
	}
	for _, user := range affected {
		h.reactionRoleChanged(r.Context(), cid, user)
	}
	_ = server.WriteCBOR(w, http.StatusOK, []any{req.RequestID, uint64(len(options))})
}

// verifyRolePanelBotSend ties panel ownership to the durable accepted send
// receipt. MLS application messages can be pruned before the UI registers the
// panel, so the receipt is the durable authority for Bot authorship and seq.
func verifyRolePanelBotSend(ctx context.Context, tx store.Repository, installation store.BotInstallationRow, botUser id.ID, channel store.ChannelRow, req rolePanelRegistrationRequest) error {
	deny := func() error {
		return server.Errorf(server.CodeForbidden, "panel must reference an accepted send by the installed bot")
	}
	if installation.DeviceID == nil || channel.CommunityID == nil {
		return deny()
	}
	groups, err := tx.GroupsForTarget(ctx, channel.ID, 0)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return deny()
	}
	sends, ok := tx.(store.BotSendOperations)
	if !ok {
		return deny()
	}
	operation, err := sends.LockBotSendOperation(ctx, installation.ID, req.RequestID)
	if err != nil || operation.OperationID != req.RequestID || operation.InstallationID != installation.ID || operation.UserID != botUser || operation.DeviceID != *installation.DeviceID || operation.CommunityID != *channel.CommunityID || operation.State != uint8(store.BotSendOperationAccepted) || operation.AcceptedAttempt == nil || *operation.AcceptedAttempt != operation.CurrentAttempt {
		return deny()
	}
	var sendGroup *store.GroupRow
	for i := range groups {
		group := &groups[i]
		if group.GroupID == operation.GroupID && group.Kind == 0 && group.CommunityID != nil && *group.CommunityID == *channel.CommunityID && group.TargetID == channel.ID && group.ClosedAt == nil {
			sendGroup = group
			break
		}
	}
	if sendGroup == nil {
		return deny()
	}
	// A group may close concurrently on PostgreSQL. Take the standard group
	// lock after the community lock and re-read state before accepting the
	// receipt, so registration cannot race the close transaction.
	if err := tx.LockGroup(ctx, operation.GroupID); err != nil {
		return err
	}
	lockedGroup, err := tx.GetGroup(ctx, operation.GroupID)
	if err != nil {
		return err
	}
	if lockedGroup.Kind != 0 || lockedGroup.CommunityID == nil || *lockedGroup.CommunityID != *channel.CommunityID || lockedGroup.TargetID != channel.ID || lockedGroup.ClosedAt != nil {
		return deny()
	}
	attempt, err := sends.GetBotSendAttempt(ctx, installation.ID, req.RequestID, *operation.AcceptedAttempt)
	if err != nil || attempt.InstallationID != installation.ID || attempt.OperationID != req.RequestID || attempt.AttemptID != *operation.AcceptedAttempt || attempt.Outcome != uint8(store.BotSendAttemptAccepted) || attempt.Class != 0 {
		return deny()
	}
	receipt, err := botprotocol.DecodeReceipt(attempt.Receipt)
	if err != nil || receipt.OperationID != req.RequestID || receipt.AttemptID != attempt.AttemptID || receipt.CommunityID != *channel.CommunityID || receipt.GroupID != lockedGroup.GroupID || receipt.ExpectedGeneration != attempt.Generation || receipt.Epoch != attempt.Epoch || receipt.Class != uint64(attempt.Class) || receipt.Class != 0 || receipt.Outcome != botprotocol.ReceiptAccepted || receipt.SendResponse == nil || receipt.SendResponse.Sequence != req.MessageSeq || !bytes.Equal(receipt.CiphertextHash, attempt.CiphertextSHA256) || !sameOptionalID(receipt.PredecessorAttemptID, attempt.PredecessorID) {
		return deny()
	}
	if message, err := tx.GetAppMessage(ctx, lockedGroup.GroupID, req.MessageSeq); err == nil {
		if message.UploaderDevice != *installation.DeviceID || message.DeletedAt != nil || (message.Class != nil && *message.Class != 0) {
			return deny()
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

func sameOptionalID(a, b *id.ID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (h *Roles) selfRoleCommunity(r *http.Request, repo store.Repository) (id.ID, auth.Session, error) {
	s, ok := session(r)
	if !ok {
		return id.ID{}, auth.Session{}, server.Errorf(server.CodeUnauthenticated, "")
	}
	u, err := repo.GetUser(r.Context(), s.UserID)
	if err != nil {
		return id.ID{}, auth.Session{}, notFound(err)
	}
	if u.Kind != kindHuman {
		return id.ID{}, auth.Session{}, server.Errorf(server.CodeForbidden, "human session required")
	}
	cid, err := server.PathID(r, "id")
	if err != nil {
		return id.ID{}, auth.Session{}, err
	}
	if _, err := repo.GetMember(r.Context(), cid, s.UserID); err != nil {
		return id.ID{}, auth.Session{}, notFound(err)
	}
	if _, err := repo.GetCommunity(r.Context(), cid); err != nil {
		return id.ID{}, auth.Session{}, notFound(err)
	}
	return cid, s, nil
}

func (h *Roles) selfRoleCandidate(ctx *http.Request, repo store.Repository, cid, actor, roleID id.ID, have Bits, snap Snapshot) error {
	role, err := repo.GetRole(ctx.Context(), roleID)
	if err != nil || role.CommunityID != cid {
		return server.Errorf(server.CodeInvalidRequest, "role is unavailable for self-service")
	}
	if role.Position == 0 || Bits(role.Allow)&^selfRoleAllowedBits != 0 || Bits(role.Deny)&^selfRoleAllowedBits != 0 || Bits(role.Allow)&^have != 0 {
		return server.Errorf(server.CodeForbidden, "role exceeds self-service permission limits")
	}
	// A role must remain below the actor's highest held role. Owners can configure any
	// otherwise-safe role; an ordinary manager cannot approve a peer or superior role.
	if actor != snap.Owner {
		var highest uint64
		for _, held := range snap.Roles {
			if (snap.Held[held.ID] || held.ID == snap.Everyone) && held.Position > highest {
				highest = held.Position
			}
		}
		if role.Position >= highest {
			return server.Errorf(server.CodeForbidden, "role must be below your highest role")
		}
	}
	if inst, err := repo.GetActiveBotInstallationByCommunity(ctx.Context(), cid); err == nil && inst.RoleID != nil && *inst.RoleID == roleID {
		return server.Errorf(server.CodeForbidden, "bot-managed role cannot be self-assigned")
	}
	return nil
}

// applyRolePanelReaction adds or removes the authenticated reactor's own
// provenance only when reply_to names an exact, live, bot-authored panel in
// this channel. Unknown messages and non-panel reactions remain ordinary
// readable-channel reactions.
func (h *Roles) applyRolePanelReaction(r *http.Request, tx store.Repository, ch store.ChannelRow, actor auth.Session, env Envelope) (bool, error) {
	if (env.Type != EnvReactionAdd && env.Type != EnvReactionRemove) || ch.CommunityID == nil || env.ReplyTo == nil {
		return false, nil
	}
	seq, err := targetSeq(env)
	if err != nil || seq == 0 {
		return false, nil //nolint:nilerr // An invalid reply target is an ordinary reaction, not a panel.
	}
	if _, err := tx.GetMember(r.Context(), *ch.CommunityID, actor.UserID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	reactor, err := tx.GetUser(r.Context(), actor.UserID)
	if err != nil {
		return false, err
	}
	if reactor.Kind != kindHuman {
		return false, nil
	}
	panel, err := tx.GetRolePanel(r.Context(), ch.ID, seq)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if panel.CommunityID != *ch.CommunityID {
		return false, nil
	}
	installation, err := tx.GetBotInstallation(r.Context(), panel.InstallationID)
	if err != nil {
		return false, err
	}
	if installation.CommunityID != *ch.CommunityID || installation.State != store.BotInstallationReady || installation.RemovedAt != nil || installation.UserID == nil || installation.DeviceID == nil || *installation.UserID != panel.AuthorID {
		return false, nil
	}
	bot, err := tx.GetUser(r.Context(), panel.AuthorID)
	if err != nil {
		return false, err
	}
	if bot.Kind != 1 {
		return false, nil
	}
	rows, err := tx.ListReadableMessages(r.Context(), ch.ID, seq, 1)
	if err != nil {
		return false, err
	}
	hasReadableSource := len(rows) > 0 && rows[0].Seq == seq
	if hasReadableSource && (rows[0].Deleted != nil || rows[0].Sender != panel.AuthorID || rows[0].UploaderDevice != *installation.DeviceID) {
		return false, nil
	}
	if panel.SendOperationID.IsZero() {
		// Legacy readable panels have no durable operation link; they remain
		// usable only while their original readable message row is present.
		if !hasReadableSource {
			return false, nil
		}
	} else if err := verifyRolePanelBotSend(r.Context(), tx, installation, panel.AuthorID, ch, rolePanelRegistrationRequest{
		RequestID: panel.SendOperationID, ChannelID: ch.ID, MessageSeq: seq,
	}); err != nil {
		return false, nil //nolint:nilerr // Invalid panel provenance makes this reaction ordinary.
	}
	if _, err := tx.GetMember(r.Context(), *ch.CommunityID, panel.AuthorID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	options, err := tx.ListRolePanelOptions(r.Context(), ch.ID, seq)
	if err != nil {
		return false, err
	}
	var roleID id.ID
	for _, option := range options {
		if option.Emoji == env.Body {
			roleID = option.RoleID
			break
		}
	}
	if roleID.IsZero() {
		return false, nil
	}
	source := store.ReactionRoleGrant{CommunityID: *ch.CommunityID, UserID: actor.UserID, RoleID: roleID, ChannelID: ch.ID, MessageSeq: seq, Reaction: env.Body}
	if env.Type == EnvReactionRemove {
		err := tx.RemoveReactionRoleGrant(r.Context(), source)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	}
	policy, err := tx.ListSelfRolePolicy(r.Context(), *ch.CommunityID)
	if err != nil {
		return false, err
	}
	var approval *store.SelfRolePolicyRow
	for i := range policy {
		if policy[i].RoleID == roleID {
			approval = &policy[i]
			break
		}
	}
	if approval == nil {
		return false, nil
	}
	approverSnap, err := LoadSnapshot(r.Context(), tx, *ch.CommunityID, approval.ApprovedBy, nil)
	if err != nil {
		return false, nil //nolint:nilerr // A stale approval cannot grant a role.
	}
	approverBits := approverSnap.Resolve(approval.ApprovedBy)
	if !approverBits.Has(PermManageRoles) {
		return false, nil
	}
	if err := h.selfRoleCandidate(r, tx, *ch.CommunityID, approval.ApprovedBy, roleID, approverBits, approverSnap); err != nil {
		return false, nil //nolint:nilerr // A role that no longer meets approval constraints stays ungranted.
	}
	if err := tx.AddReactionRoleGrant(r.Context(), source); err != nil {
		return false, err
	}
	return true, nil
}

func (h *Roles) reactionRoleChanged(ctx context.Context, communityID, userID id.ID) {
	if err := syncChannelEligibility(ctx, h.repo, h.dsvc, communityID, userID, h.clk.Now().Unix()); err != nil {
		h.log.ErrorContext(ctx, "sync channel eligibility after reaction role", "community", communityID, "user", userID, "err", err)
	}
	h.syncCallGrants(ctx, communityID, &userID, nil)
	if h.notify != nil {
		h.notify.Changed(ctx, communityID)
	}
}

func (h *Roles) listSelfRoles(w http.ResponseWriter, r *http.Request) {
	cid, s, err := h.selfRoleCommunity(r, h.repo)
	if err != nil {
		h.fail(w, r, "list self roles", err)
		return
	}
	policy, err := h.repo.ListSelfRolePolicy(r.Context(), cid)
	if err != nil {
		h.fail(w, r, "list self roles", err)
		return
	}
	rows := make([][]any, 0, len(policy))
	for _, p := range policy {
		role, e := h.repo.GetRole(r.Context(), p.RoleID)
		if e != nil {
			continue
		}
		enabled, e := h.repo.HasMemberRoleGrant(r.Context(), cid, s.UserID, p.RoleID, store.RoleGrantSelf)
		if e != nil {
			h.fail(w, r, "list self roles", e)
			return
		}
		var selected uint64
		if enabled {
			selected = 1
		}
		rows = append(rows, []any{p.RoleID, role.Name, role.Position, selected})
	}
	_ = server.EncodeBody(w, http.StatusOK, rows)
}

func (h *Roles) configureSelfRoles(w http.ResponseWriter, r *http.Request) {
	var req selfRolePolicyRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		refuseCBOR(w, r, err)
		return
	}
	cid, s, err := h.selfRoleCommunity(r, h.repo)
	if err != nil {
		h.fail(w, r, "configure self roles", err)
		return
	}
	if len(req.Roles) > 250 {
		h.fail(w, r, "configure self roles", server.Errorf(server.CodeInvalidRequest, "at most 250 roles"))
		return
	}
	now := h.clk.Now().Unix()
	revoked := map[id.ID]bool{}
	err = h.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		if _, _, err := h.selfRoleCommunity(r, tx); err != nil {
			return err
		}
		snap, err := LoadSnapshot(r.Context(), tx, cid, s.UserID, nil)
		if err != nil {
			return notFound(err)
		}
		have := snap.Resolve(s.UserID)
		if !have.Has(PermManageRoles) {
			return server.Errorf(server.CodeForbidden, "manage_roles required")
		}
		seen := map[id.ID]bool{}
		for _, rid := range req.Roles {
			if seen[rid] {
				return server.Errorf(server.CodeInvalidRequest, "duplicate role")
			}
			seen[rid] = true
			if err := h.selfRoleCandidate(r, tx, cid, s.UserID, rid, have, snap); err != nil {
				return err
			}
		}
		previous, err := tx.ListSelfRolePolicy(r.Context(), cid)
		if err != nil {
			return err
		}
		for _, row := range previous {
			if seen[row.RoleID] {
				continue
			}
			users, err := tx.ListSelfRoleGrantUsers(r.Context(), cid, row.RoleID)
			if err != nil {
				return err
			}
			for _, user := range users {
				if err := tx.RemoveMemberRoleGrant(r.Context(), cid, user, row.RoleID, store.RoleGrantSelf); err != nil {
					return err
				}
				revoked[user] = true
			}
		}
		return tx.SetSelfRolePolicy(r.Context(), cid, s.UserID, req.Roles, now)
	})
	if err != nil {
		h.fail(w, r, "configure self roles", err)
		return
	}
	for user := range revoked {
		if err := syncChannelEligibility(afterCommit(r), h.repo, h.dsvc, cid, user, now); err != nil {
			h.log.ErrorContext(r.Context(), "sync channel eligibility after self role policy", "community", cid, "user", user, "err", err)
		}
		h.syncCallGrants(afterCommit(r), cid, &user, nil)
	}
	if len(revoked) > 0 {
		h.notify.Changed(afterCommit(r), cid)
	}
	_ = server.WriteCBOR(w, http.StatusOK, []any{req.RequestID, uint64(len(req.Roles))})
}

func (h *Roles) selectSelfRole(w http.ResponseWriter, r *http.Request) {
	var req selfRoleSelectionRequest
	if err := server.DecodeBody(w, r, maxCBORBody, &req); err != nil {
		refuseCBOR(w, r, err)
		return
	}
	cid, s, err := h.selfRoleCommunity(r, h.repo)
	if err != nil {
		h.fail(w, r, "select self role", err)
		return
	}
	rid, err := server.PathID(r, "role_id")
	if err != nil {
		h.fail(w, r, "select self role", err)
		return
	}
	if req.Enabled > 1 {
		h.fail(w, r, "select self role", server.Errorf(server.CodeInvalidRequest, "enabled must be 0 or 1"))
		return
	}
	var changed bool
	err = h.repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), cid); err != nil {
			return notFound(err)
		}
		if _, _, err := h.selfRoleCommunity(r, tx); err != nil {
			return err
		}
		policy, err := tx.ListSelfRolePolicy(r.Context(), cid)
		if err != nil {
			return err
		}
		var approval *store.SelfRolePolicyRow
		for i := range policy {
			if policy[i].RoleID == rid {
				approval = &policy[i]
				break
			}
		}
		if approval == nil {
			return server.Errorf(server.CodeForbidden, "role is not currently approved for self-service")
		}
		approverSnap, err := LoadSnapshot(r.Context(), tx, cid, approval.ApprovedBy, nil)
		if err != nil {
			return server.Errorf(server.CodeForbidden, "self-service approval is no longer valid")
		}
		approverBits := approverSnap.Resolve(approval.ApprovedBy)
		if !approverBits.Has(PermManageRoles) {
			return server.Errorf(server.CodeForbidden, "self-service approval is no longer valid")
		}
		if err := h.selfRoleCandidate(r, tx, cid, approval.ApprovedBy, rid, approverBits, approverSnap); err != nil {
			return server.Errorf(server.CodeForbidden, "role is no longer within the approved policy")
		}
		hasSelf, err := tx.HasMemberRoleGrant(r.Context(), cid, s.UserID, rid, store.RoleGrantSelf)
		if err != nil {
			return err
		}
		if req.Enabled == 1 && !hasSelf {
			err = tx.AddMemberRoleGrant(r.Context(), cid, s.UserID, rid, store.RoleGrantSelf)
		} else if req.Enabled == 0 && hasSelf {
			err = tx.RemoveMemberRoleGrant(r.Context(), cid, s.UserID, rid, store.RoleGrantSelf)
		}
		if err == nil && (req.Enabled == 1) != hasSelf {
			changed = true
		}
		return err
	})
	if err != nil {
		h.fail(w, r, "select self role", err)
		return
	}
	if changed {
		if err := syncChannelEligibility(afterCommit(r), h.repo, h.dsvc, cid, s.UserID, h.clk.Now().Unix()); err != nil {
			h.log.ErrorContext(r.Context(), "sync channel eligibility after self role", "community", cid, "user", s.UserID, "err", err)
		}
		h.syncCallGrants(afterCommit(r), cid, &s.UserID, nil)
		h.notify.Changed(afterCommit(r), cid)
	}
	w.WriteHeader(http.StatusNoContent)
}

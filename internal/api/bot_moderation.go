package api

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	automod "github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/moderation/operationid"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/server"
	"github.com/jonasthim/dilla/internal/store"
)

const (
	botModerationRequestLifetime = 30 * time.Second
	botModerationSourceLifetime  = 15 * time.Second
)

type botModerationActionRequest struct {
	_              struct{} `cbor:",toarray"`
	RequestID      id.ID
	GroupID        id.ID
	SourceSequence uint64
	Action         string
	Rules          []string
	TimeoutMillis  uint64
	Expires        uint64
}

type botModerationActionReceipt struct {
	ActionID id.ID
	Revision uint64
	Expires  *uint64
}

// AcceptBotModerationAction is the server boundary for automatic warn and
// timeout decisions. It independently binds each request to an actual recent
// text-group message and rechecks the live installation, capability, current
// permission, and hierarchy inside the transaction that records the result.
func (d Deps) AcceptBotModerationAction(w http.ResponseWriter, r *http.Request) {
	s, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	installationID, err := server.PathID(r, "installation")
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if s.Scope != auth.ScopeEnrolled || s.Bot == nil || s.Bot.State != store.BotInstallationReady ||
		s.Bot.InstallationID != installationID || s.Bot.CommunityID.IsZero() {
		server.WriteError(w, server.Errorf(server.CodeForbidden, "ready bot installation required"))
		return
	}
	if err := requireBotWire(s.WireVersion); err != nil {
		server.WriteError(w, err)
		return
	}
	var req botModerationActionRequest
	if err := server.DecodeBody(w, r, 4096, &req); err != nil {
		refuseCBOR(w, r, err)
		return
	}
	groupID, targetSequence, rules, err := validateBotModerationRequest(req)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	communityID := s.Bot.CommunityID
	digest, err := botModerationPayloadHash(req, rules)
	if err != nil {
		server.WriteError(w, server.Errorf(server.CodeInvalidRequest, "invalid moderation request"))
		return
	}
	var receipt botModerationActionReceipt
	err = d.Repo.Tx(r.Context(), func(tx store.Repository) error {
		if err := tx.LockCommunity(r.Context(), communityID); err != nil {
			return notFound(err)
		}
		installation, err := tx.GetActiveBotInstallationByCommunity(r.Context(), communityID)
		if err != nil || installation.ID != installationID || installation.State != store.BotInstallationReady ||
			installation.RemovedAt != nil || installation.UserID == nil || *installation.UserID != s.UserID ||
			installation.DeviceID == nil || *installation.DeviceID != s.DeviceID {
			return server.Errorf(server.CodeForbidden, "ready Bot installation required")
		}
		prior, priorErr := tx.GetBotOperation(r.Context(), communityID, req.RequestID)
		if priorErr == nil {
			if prior.InstallationID != installationID || prior.ActorKind != 2 || prior.ActorID != s.UserID ||
				prior.ActorDevice == nil || *prior.ActorDevice != s.DeviceID || prior.Operation != botOpModeration ||
				!bytes.Equal(prior.PayloadHash, digest) {
				return botConflict("moderation request id was used for a different operation")
			}
			return decodeBotModerationReceipt(prior.ResultCBOR, &receipt)
		}
		if !errors.Is(priorErr, store.ErrNotFound) {
			return priorErr
		}
		if r.URL.Query().Get("receipt_only") == "1" {
			return server.Errorf(server.CodeNotFound, "moderation receipt not found")
		}
		// A matching immutable receipt belongs to this installation even if the
		// capability for creating a new effect has since been revoked.
		if installation.Capabilities&uint16(botCapWarnings) == 0 || s.Bot.Capabilities&uint16(botCapWarnings) == 0 {
			return server.Errorf(server.CodeForbidden, "warning capability required")
		}
		now := d.Clock.Now().Unix()
		nowUnix, ok := checkedUint64(now)
		if !ok || now <= 0 || now > math.MaxInt64-int64(botModerationRequestLifetime/time.Second) ||
			req.Expires <= nowUnix || req.Expires-nowUnix > uint64(botModerationRequestLifetime/time.Second) {
			return server.Errorf(server.CodeInvalidRequest, "moderation request has expired or exceeds its acceptance window")
		}
		group, err := tx.GetGroup(r.Context(), groupID)
		if err != nil || group.ClosedAt != nil || group.Kind != GroupText || group.CommunityID == nil || *group.CommunityID != communityID {
			return notFound(store.ErrNotFound)
		}
		message, err := tx.GetAppMessage(r.Context(), groupID, targetSequence)
		if err != nil || message.DeletedAt != nil {
			return notFound(store.ErrNotFound)
		}
		if message.Created > now || now-message.Created > int64(botModerationSourceLifetime/time.Second) {
			return server.Errorf(server.CodeInvalidRequest, "moderation source message is stale")
		}
		uploader, err := tx.GetDevice(r.Context(), message.UploaderDevice)
		if err != nil {
			return notFound(err)
		}
		if _, err := tx.GetMember(r.Context(), communityID, uploader.UserID); err != nil {
			return notFound(err)
		}
		botUser, err := tx.GetUser(r.Context(), s.UserID)
		if err != nil || botUser.Kind != 1 {
			return server.Errorf(server.CodeForbidden, "managed bot account required")
		}
		m := moderation{community: communityID, target: uploader.UserID, session: s}
		m.snap, err = LoadSnapshot(r.Context(), tx, communityID, s.UserID, nil)
		if err != nil {
			return notFound(err)
		}
		if !m.snap.Resolve(s.UserID).Has(PermModerateMembers) {
			return server.Errorf(server.CodeForbidden, "moderate members permission required")
		}
		if err := outranks(r.Context(), tx, m.snap, s.UserID, uploader.UserID, communityID); err != nil {
			return err
		}
		var expires *uint64
		var eventExpires any
		moderationKind := uint64(0) // warning
		var expiresInt64 *int64
		if req.Action == string(automod.ActionTimeout) {
			seconds := (req.TimeoutMillis + 999) / 1000
			if seconds == 0 || seconds > uint64(automod.MaxTimeout/time.Second) || nowUnix > math.MaxInt64-seconds {
				return server.Errorf(server.CodeInvalidRequest, "timeout is outside the supported range")
			}
			until := nowUnix + seconds
			untilInt64, ok := checkedInt64(until)
			if !ok {
				return server.Errorf(server.CodeInvalidRequest, "timeout is outside the supported range")
			}
			expires = &until
			eventExpires = until
			expiresValue := untilInt64
			expiresInt64 = &expiresValue
			moderationKind = 1 // timeout; every upload class observes this row
		}
		actionID := id.New()
		event, err := tx.AppendBotStructuralEvent(r.Context(), protocol.Event{ID: actionID, CommunityID: communityID,
			Timestamp: nowUnix, Kind: protocol.EventModeration,
			Data: []any{actionID, req.RequestID, s.UserID, s.DeviceID, uploader.UserID, nil, nil, moderationKind, eventExpires}})
		if err != nil {
			return err
		}
		if expiresInt64 != nil {
			if err := tx.PutMemberTimeout(r.Context(), store.MemberTimeoutRow{CommunityID: communityID, UserID: uploader.UserID,
				ByUser: s.UserID, Created: now, Expires: *expiresInt64}); err != nil {
				return err
			}
		}
		name := "bot.automod.warn"
		if req.Action == string(automod.ActionTimeout) {
			name = "bot.automod.timeout"
		}
		if err := tx.Audit(r.Context(), store.AuditRow{Actor: &s.UserID, Action: name, Target: uploader.UserID.String(),
			Detail: fmt.Sprintf("%s %s %d %s %s", communityID, groupID, targetSequence, stringsJoin(rules), req.RequestID), At: now}); err != nil {
			return err
		}
		result, err := cborx.Marshal([]any{actionID, event.Sequence, expires})
		if err != nil {
			return err
		}
		if err := tx.CreateBotOperation(r.Context(), store.BotOperationRow{CommunityID: communityID, RequestID: req.RequestID,
			InstallationID: installationID, ActorKind: 2, ActorID: s.UserID, ActorDevice: &s.DeviceID,
			Operation: botOpModeration, PayloadHash: digest, ResultCBOR: result, Created: now}); err != nil {
			return err
		}
		receipt = botModerationActionReceipt{ActionID: actionID, Revision: event.Sequence, Expires: expires}
		return nil
	})
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	_ = server.WriteCBOR(w, http.StatusOK, []any{receipt.ActionID, receipt.Revision, receipt.Expires})
}

func validateBotModerationRequest(req botModerationActionRequest) (id.ID, uint64, []automod.Rule, error) {
	bad := server.Errorf(server.CodeInvalidRequest, "invalid automatic moderation request")
	if req.RequestID.IsZero() || req.GroupID.IsZero() || req.SourceSequence == 0 || req.SourceSequence > math.MaxInt64 ||
		req.RequestID != operationid.Automatic(req.GroupID, req.SourceSequence) || len(req.Rules) == 0 || len(req.Rules) > 3 {
		return id.ID{}, 0, nil, bad
	}
	if req.Action != string(automod.ActionWarn) && req.Action != string(automod.ActionTimeout) ||
		(req.Action == string(automod.ActionTimeout) && (req.TimeoutMillis == 0 || req.TimeoutMillis > uint64(automod.MaxTimeout/time.Millisecond))) ||
		(req.Action == string(automod.ActionWarn) && req.TimeoutMillis != 0) {
		return id.ID{}, 0, nil, bad
	}
	rules := make([]automod.Rule, 0, len(req.Rules))
	seen := make(map[automod.Rule]bool, len(req.Rules))
	for _, value := range req.Rules {
		rule := automod.Rule(value)
		if (rule != automod.RuleRate && rule != automod.RuleDuplicate && rule != automod.RuleMentions) || seen[rule] {
			return id.ID{}, 0, nil, bad
		}
		seen[rule] = true
		rules = append(rules, rule)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i] < rules[j] })
	return req.GroupID, req.SourceSequence, rules, nil
}

func botModerationPayloadHash(req botModerationActionRequest, rules []automod.Rule) ([]byte, error) {
	ruleValues := make([]any, len(rules))
	for i := range rules {
		ruleValues[i] = string(rules[i])
	}
	body, err := cborx.Marshal([]any{uint64(1), req.RequestID, req.GroupID, req.SourceSequence,
		req.Action, ruleValues, req.TimeoutMillis})
	if err != nil {
		return nil, err
	}
	return botPayloadHash([]any{[]byte("dilla:bot:automod:v1\x00"), body}), nil
}

func decodeBotModerationReceipt(raw []byte, out *botModerationActionReceipt) error {
	var tuple []any
	if cborx.Unmarshal(raw, &tuple) != nil || len(tuple) != 3 {
		return server.Errorf(server.CodeUnavailable, "moderation receipt is invalid")
	}
	action, actionOK := tuple[0].([]byte)
	revision, revisionOK := tuple[1].(uint64)
	if !actionOK || len(action) != 16 || !revisionOK || revision == 0 || revision > math.MaxInt64 {
		return server.Errorf(server.CodeUnavailable, "moderation receipt is invalid")
	}
	copy(out.ActionID[:], action)
	if tuple[2] != nil {
		expires, ok := tuple[2].(uint64)
		if !ok || expires <= 0 || expires > math.MaxInt64 {
			return server.Errorf(server.CodeUnavailable, "moderation receipt is invalid")
		}
		out.Expires = &expires
	}
	out.Revision = revision
	return nil
}

func stringsJoin(rules []automod.Rule) string {
	values := make([]string, len(rules))
	for i := range rules {
		values[i] = string(rules[i])
	}
	return fmt.Sprint(values)
}

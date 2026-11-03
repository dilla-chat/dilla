package service

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dilla-chat/dilla/internal/bot/moderation"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	moderationInteractionPageSize  = 8
	moderationInteractionDataLimit = 2400
	moderationInteractionCursorMax = 256
)

type moderationCaseListReader interface {
	List(context.Context, id.ID, []byte, uint64) ([]ModerationCase, []byte, error)
}

type moderationEvidenceListReader interface {
	List(context.Context, id.ID, []byte, uint64) ([]moderation.Evidence, []byte, error)
}

// AcceptedManualRemovalReceipt is returned only by the authenticated server
// lookup for an immutable, previously accepted moderator removal.
type AcceptedManualRemovalReceipt struct {
	ActionID, CommunityID, GroupID, ActorUserID, ActorDeviceID id.ID
	TargetSequence, Revision                                   uint64
	AcceptedAt                                                 int64
}

type manualRemovalReceiptLookup interface {
	AcceptedManualRemoval(context.Context, id.ID, id.ID) (AcceptedManualRemovalReceipt, error)
}

// AcceptedManualWarningReceipt is the authoritative, persisted metadata for a
// human warning action. It contains no case text.
type AcceptedManualWarningReceipt struct {
	RequestID, ActionID, CommunityID id.ID
	ActorUserID, ActorDeviceID       id.ID
	TargetUserID                     id.ID
	Revision                         uint64
	AcceptedAt                       int64
}

type manualWarningReceiptLookup interface {
	AcceptedManualWarning(context.Context, id.ID, id.ID) (AcceptedManualWarningReceipt, error)
}

type manualEvidenceWriter interface {
	BoundTo(id.ID, id.ID) bool
	Save(context.Context, moderation.EvidenceInput) error
}

// ModerationInteractions exposes bounded, freshly authorized encrypted case
// pages through the typed interaction dispatcher.
type ModerationInteractions struct {
	Reader              moderationCaseListReader
	CommunityID         id.ID
	InstallationID      id.ID
	ManualRemovalLookup manualRemovalReceiptLookup
	ManualWarningLookup manualWarningReceiptLookup
	EvidenceWriter      manualEvidenceWriter
	EvidenceReader      moderationEvidenceListReader
}

func (h ModerationInteractions) Handlers() map[protocol.Operation]InteractionHandlerSpec {
	return map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpModerationCases:        {RecomputeOnReplay: true, Handle: h.listCases},
		protocol.OpModerationEvidence:     {RevisionInHandler: true, Handle: h.saveManualEvidence},
		protocol.OpModerationEvidenceList: {RecomputeOnReplay: true, Handle: h.listManualEvidence},
	}
}

//nolint:nilerr // Invalid client input intentionally maps to an unavailable protocol result.
func (h ModerationInteractions) listManualEvidence(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	if ctx == nil || h.EvidenceReader == nil || h.CommunityID.IsZero() || h.InstallationID.IsZero() || request.CommunityID != h.CommunityID || actor.User.IsZero() || len(request.Args) != 1 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	after, ok := decodeEvidenceInteractionCursor(request.Args[0], h.CommunityID, h.InstallationID)
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	rows, next, err := h.EvidenceReader.List(ctx, actor.User, after, 1)
	if err != nil || len(rows) > 1 || len(next) > moderationCaseMaxCursor {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	page := make([]any, 0, len(rows))
	for _, row := range rows {
		createdAt := row.CreatedAt.Unix()
		if row.Rule != moderation.RuleManual || row.ID.IsZero() || !moderationActionAllowed(row.Action) || createdAt <= 0 ||
			len(row.Reason) == 0 || len(row.Reason) > moderation.MaxEvidenceReasonBytes || len(row.Excerpt) > moderation.ExcerptMaxBytes {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		page = append(page, []any{row.ID, string(row.Action), row.Reason, row.Excerpt, uint64(createdAt)})
	}
	var cursor []byte
	if len(next) > 0 {
		cursor, ok = encodeEvidenceInteractionCursor(h.CommunityID, h.InstallationID, next)
		if !ok {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
	}
	data, err := cborx.Marshal([]any{uint64(1), page, cursor})
	if err != nil || len(data) > moderationInteractionDataLimit {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	return InteractionResult{Status: 0, Data: data}, nil
}

func encodeEvidenceInteractionCursor(community, installation id.ID, after []byte) ([]byte, bool) {
	if community.IsZero() || installation.IsZero() || len(after) == 0 || len(after) > moderationCaseMaxCursor {
		return nil, false
	}
	cursor, err := cborx.Marshal([]any{uint64(1), community[:], installation[:], after})
	return cursor, err == nil && len(cursor) <= moderationInteractionCursorMax
}

func decodeEvidenceInteractionCursor(value any, community, installation id.ID) ([]byte, bool) {
	if value == nil {
		return nil, true
	}
	cursor, ok := value.([]byte)
	if !ok || len(cursor) == 0 || len(cursor) > moderationInteractionCursorMax || cborx.CheckDeterministic(cursor) != nil {
		return nil, false
	}
	var fields []any
	if cborx.Unmarshal(cursor, &fields) != nil || len(fields) != 4 || fields[0] != uint64(1) {
		return nil, false
	}
	c, cok := fields[1].([]byte)
	i, iok := fields[2].([]byte)
	after, aok := fields[3].([]byte)
	if !cok || !iok || !aok || len(c) != len(community) || len(i) != len(installation) || len(after) == 0 || len(after) > moderationCaseMaxCursor {
		return nil, false
	}
	if string(c) != string(community[:]) || string(i) != string(installation[:]) {
		return nil, false
	}
	return append([]byte(nil), after...), true
}

//nolint:nilerr // Invalid client input intentionally maps to an unavailable protocol result.
func (h ModerationInteractions) saveManualEvidence(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	if ctx == nil || h.CommunityID.IsZero() || request.CommunityID != h.CommunityID || actor.User.IsZero() || actor.Device.IsZero() ||
		h.InstallationID.IsZero() || h.EvidenceWriter == nil ||
		!h.EvidenceWriter.BoundTo(h.InstallationID, h.CommunityID) ||
		request.Revision != nil && *request.Revision != 0 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	var requestID id.ID
	var reason, excerpt string
	if len(request.Args) != 3 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	requestID, _ = request.Args[0].(id.ID)
	reason, _ = request.Args[1].(string)
	excerpt, _ = request.Args[2].(string)
	ok := !requestID.IsZero()
	reasonOK, excerptOK := true, true
	if !ok || requestID.IsZero() || !reasonOK || !excerptOK || !utf8.ValidString(reason) || len(reason) == 0 || len(reason) > moderation.MaxEvidenceReasonBytes ||
		strings.TrimSpace(reason) == "" || !utf8.ValidString(excerpt) || len(excerpt) > moderation.ExcerptMaxBytes {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	var removalReceipt AcceptedManualRemovalReceipt
	removalValid := false
	if h.ManualRemovalLookup != nil {
		receipt, err := h.ManualRemovalLookup.AcceptedManualRemoval(ctx, h.InstallationID, requestID)
		removalValid = err == nil && !receipt.ActionID.IsZero() && receipt.CommunityID == h.CommunityID && !receipt.GroupID.IsZero() &&
			receipt.ActorUserID == actor.User && receipt.ActorDeviceID == actor.Device && receipt.TargetSequence > 0 && receipt.TargetSequence <= math.MaxInt64 &&
			receipt.Revision > 0 && receipt.Revision <= math.MaxInt64 && receipt.AcceptedAt > 0 && receipt.AcceptedAt <= time.Now().Add(5*time.Minute).Unix()
		removalReceipt = receipt
	}
	var warningReceipt AcceptedManualWarningReceipt
	warningValid := false
	if excerpt == "" && h.ManualWarningLookup != nil {
		receipt, err := h.ManualWarningLookup.AcceptedManualWarning(ctx, h.InstallationID, requestID)
		warningValid = err == nil && receipt.RequestID == requestID && !receipt.ActionID.IsZero() && receipt.CommunityID == h.CommunityID &&
			receipt.ActorUserID == actor.User && receipt.ActorDeviceID == actor.Device && !receipt.TargetUserID.IsZero() &&
			receipt.Revision > 0 && receipt.Revision <= math.MaxInt64 && receipt.AcceptedAt > 0 && receipt.AcceptedAt <= time.Now().Add(5*time.Minute).Unix()
		warningReceipt = receipt
	}
	// The wire tuple has no action label; only a single unambiguous accepted
	// action receipt may authorize storing the evidence.
	if removalValid == warningValid {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if removalValid {
		err := h.EvidenceWriter.Save(ctx, moderation.EvidenceInput{
			OperationID: requestID, Action: moderation.ActionRemove, Rule: moderation.RuleManual,
			Reason: reason, Excerpt: excerpt, CreatedAt: time.Unix(removalReceipt.AcceptedAt, 0).UTC(),
			AcceptedActionID: removalReceipt.ActionID, AcceptedRevision: removalReceipt.Revision,
			ActorUserID: removalReceipt.ActorUserID, ActorDeviceID: removalReceipt.ActorDeviceID,
			GroupID: removalReceipt.GroupID, TargetSequence: removalReceipt.TargetSequence,
		})
		if err != nil {
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		return InteractionResult{Status: 0}, nil
	}
	err := h.EvidenceWriter.Save(ctx, moderation.EvidenceInput{
		OperationID: requestID, Action: moderation.ActionWarn, Rule: moderation.RuleManual,
		Reason: reason, CreatedAt: time.Unix(warningReceipt.AcceptedAt, 0).UTC(), AcceptedActionID: warningReceipt.ActionID,
		AcceptedRevision: warningReceipt.Revision, ActorUserID: warningReceipt.ActorUserID, ActorDeviceID: warningReceipt.ActorDeviceID,
		TargetUserID: warningReceipt.TargetUserID,
	})
	if err != nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	return InteractionResult{Status: 0}, nil
}

func (h ModerationInteractions) listCases(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	if h.Reader == nil || h.CommunityID.IsZero() || request.CommunityID != h.CommunityID || actor.User.IsZero() {
		return InteractionResult{}, errors.New("missing moderation interaction authority or context")
	}
	if len(request.Args) != 1 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	after, ok := decodeModerationInteractionCursor(request.Args[0], h.CommunityID)
	if !ok {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	rows, next, err := h.Reader.List(ctx, actor.User, after, moderationInteractionPageSize)
	if err != nil {
		return InteractionResult{}, err
	}
	if len(next) > moderationCaseMaxCursor {
		return InteractionResult{}, ErrModerationCaseState
	}
	page := make([]any, 0, len(rows))
	var data []byte
	for i, row := range rows {
		receivedAt := row.ReceivedAt.Unix()
		if row.SourceRevision > math.MaxInt64 || receivedAt <= 0 || !moderationActionAllowed(row.Action) || len(row.Excerpt) > moderation.ExcerptMaxBytes {
			return InteractionResult{}, ErrModerationCaseState
		}
		candidate := append(page, []any{row.Community, row.Group, row.Channel, row.Actor, row.Message,
			row.SourceRevision, row.Reason(), string(row.Action), row.Excerpt, uint64(receivedAt)})
		cursorAfter := next
		if i < len(rows)-1 {
			cursorAfter = moderationCaseKey(row.Message, row.SourceRevision)
		}
		var cursorToken []byte
		if len(cursorAfter) > 0 {
			cursorToken, ok = encodeModerationInteractionCursor(h.CommunityID, cursorAfter)
			if !ok {
				return InteractionResult{}, ErrModerationCaseState
			}
		}
		encoded, marshalErr := cborx.Marshal([]any{uint64(1), candidate, cursorToken})
		if marshalErr != nil {
			return InteractionResult{}, marshalErr
		}
		if len(encoded) > moderationInteractionDataLimit {
			if len(page) == 0 {
				return InteractionResult{}, ErrModerationCaseState
			}
			break
		}
		page, data = candidate, encoded
	}
	if data == nil {
		var cursorToken []byte
		if len(next) > 0 {
			cursorToken, ok = encodeModerationInteractionCursor(h.CommunityID, next)
			if !ok {
				return InteractionResult{}, ErrModerationCaseState
			}
		}
		data, err = cborx.Marshal([]any{uint64(1), []any{}, cursorToken})
		if err != nil {
			return InteractionResult{}, err
		}
	}
	return InteractionResult{Status: 0, Data: data}, nil
}

func encodeModerationInteractionCursor(community id.ID, after []byte) ([]byte, bool) {
	if community.IsZero() || len(after) == 0 || len(after) > moderationCaseMaxCursor {
		return nil, false
	}
	cursor, err := cborx.Marshal([]any{uint64(1), community[:], after})
	return cursor, err == nil && len(cursor) <= moderationInteractionCursorMax
}

func decodeModerationInteractionCursor(value any, community id.ID) ([]byte, bool) {
	if value == nil {
		return nil, true
	}
	cursor, ok := value.([]byte)
	if !ok || len(cursor) == 0 || len(cursor) > moderationInteractionCursorMax || cborx.CheckDeterministic(cursor) != nil {
		return nil, false
	}
	var fields []any
	if cborx.Unmarshal(cursor, &fields) != nil || len(fields) != 3 || fields[0] != uint64(1) {
		return nil, false
	}
	boundCommunityBytes, communityOK := fields[1].([]byte)
	after, afterOK := fields[2].([]byte)
	if !communityOK || len(boundCommunityBytes) != len(community) || !afterOK || len(after) == 0 || len(after) > moderationCaseMaxCursor {
		return nil, false
	}
	var boundCommunity id.ID
	copy(boundCommunity[:], boundCommunityBytes)
	if boundCommunity != community {
		return nil, false
	}
	return append([]byte(nil), after...), true
}

func moderationActionAllowed(action moderation.Action) bool {
	return action == moderation.ActionLog || action == moderation.ActionRemove || action == moderation.ActionWarn || action == moderation.ActionTimeout
}

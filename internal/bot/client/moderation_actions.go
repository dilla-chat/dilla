package client

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/jonasthim/dilla/internal/bot/moderation/operationid"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const MaxModerationActionBody = 8 << 10

type ModerationActionReceipt struct {
	ActionID id.ID
	Revision uint64
	Expires  *uint64
}

// AcceptBotModerationAction submits a bounded warning or timeout for an
// installation-bound source message. The server rechecks source and authority
// and returns the immutable receipt used for idempotent recovery.
func (s *Session) AcceptBotModerationAction(ctx context.Context, installation, request, group id.ID, sequence uint64,
	action string, rules []string, timeoutMillis, expires uint64) (ModerationActionReceipt, error) {
	return s.botModerationAction(ctx, installation, request, group, sequence, action, rules, timeoutMillis, expires, false)
}

// LookupBotModerationAction resolves an existing acceptance and cannot create an effect.
func (s *Session) LookupBotModerationAction(ctx context.Context, installation, request, group id.ID, sequence uint64,
	action string, rules []string, timeoutMillis, expires uint64) (ModerationActionReceipt, error) {
	return s.botModerationAction(ctx, installation, request, group, sequence, action, rules, timeoutMillis, expires, true)
}
func (s *Session) botModerationAction(ctx context.Context, installation, request, group id.ID, sequence uint64,
	action string, rules []string, timeoutMillis, expires uint64, receiptOnly bool) (ModerationActionReceipt, error) {
	if s == nil || s.c == nil || installation.IsZero() || group.IsZero() || sequence == 0 || sequence > math.MaxInt64 ||
		request != operationid.Automatic(group, sequence) || expires == 0 || expires > math.MaxInt64 ||
		len(rules) == 0 || len(rules) > 3 {
		return ModerationActionReceipt{}, ErrInvalidConfig
	}
	if action != "warn" && action != "timeout" ||
		(action == "timeout" && (timeoutMillis == 0 || timeoutMillis > uint64((28*24*time.Hour)/time.Millisecond))) ||
		(action == "warn" && timeoutMillis != 0) {
		return ModerationActionReceipt{}, ErrInvalidConfig
	}
	values := make([]any, len(rules))
	seen := make(map[string]bool, len(rules))
	for i, rule := range rules {
		if rule != "message_rate" && rule != "duplicate_body" && rule != "mention_count" || seen[rule] {
			return ModerationActionReceipt{}, ErrInvalidConfig
		}
		seen[rule] = true
		values[i] = rule
	}
	body, err := cborx.Marshal([]any{request, group, sequence, action, values, timeoutMillis, expires})
	if err != nil || len(body) > MaxModerationActionBody {
		return ModerationActionReceipt{}, ErrInvalidConfig
	}
	path := "bot-runtime/installations/" + installation.String() + "/moderation/actions"
	var query url.Values
	if receiptOnly {
		query = url.Values{"receipt_only": {"1"}}
	}
	raw, _, err := s.do(ctx, http.MethodPost, path, query, body, MaxModerationActionBody, true)
	if err != nil {
		return ModerationActionReceipt{}, err
	}
	result, err := decode(raw, 3)
	if err != nil {
		return ModerationActionReceipt{}, ErrProtocol
	}
	actionID, actionOK := asID(result[0])
	revision, revisionOK := asUint(result[1])
	if !actionOK || !revisionOK || revision == 0 || revision > math.MaxInt64 {
		return ModerationActionReceipt{}, ErrProtocol
	}
	receipt := ModerationActionReceipt{ActionID: actionID, Revision: revision}
	if result[2] != nil {
		until, ok := asUint(result[2])
		if !ok || until == 0 || until > math.MaxInt64 || action != "timeout" {
			return ModerationActionReceipt{}, ErrProtocol
		}
		receipt.Expires = &until
	} else if action == "timeout" {
		return ModerationActionReceipt{}, ErrProtocol
	}
	return receipt, nil
}

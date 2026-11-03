package service

import (
	"context"
	"errors"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/id"
)

const moderationViewAuditLogBit = uint64(1 << 17)

var ErrModerationCaseUnauthorized = errors.New("moderation cases unavailable")

type moderationCaseAuthority interface {
	BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error)
}

// ModerationCaseReader is the authorized read boundary for encrypted cases.
// It resolves the actor's current view_audit_log permission on every page.
type ModerationCaseReader struct {
	sink      *ModerationCaseSink
	authority moderationCaseAuthority
}

func NewModerationCaseReader(sink *ModerationCaseSink, authority moderationCaseAuthority) (*ModerationCaseReader, error) {
	if sink == nil || authority == nil || sink.binding.Community.IsZero() {
		return nil, ErrModerationCaseUnauthorized
	}
	return &ModerationCaseReader{sink: sink, authority: authority}, nil
}

// List checks current community permission before reading any encrypted row.
// Lookup errors, absent actors, and denied permissions share one public error.
func (r *ModerationCaseReader) List(ctx context.Context, actor id.ID, after []byte, limit uint64) ([]ModerationCase, []byte, error) {
	if r == nil || r.sink == nil || r.authority == nil || ctx == nil || actor.IsZero() {
		return nil, nil, ErrModerationCaseUnauthorized
	}
	resolved, present, err := r.authority.BotActorAuthority(ctx, r.sink.binding.Community, actor)
	if err != nil || !present || resolved.EffectiveBits&moderationViewAuditLogBit == 0 {
		return nil, nil, ErrModerationCaseUnauthorized
	}
	return r.sink.list(ctx, after, limit)
}

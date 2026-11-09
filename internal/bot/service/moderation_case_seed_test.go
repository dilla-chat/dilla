package service

import (
	"context"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/moderation"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// ModerationAction is an automatic moderation decision as an earlier version stored it, one case per source message
// revision. Bot v2 Task 23 removed the producer; the reader and the purge still read the rows it left, so the tests
// seed them through ApplyModerationAction, which writes a log-only case the way the producer did.
type ModerationAction struct {
	Community, Group, Channel, Actor, Message string
	SourceRevision                            uint64
	Decision                                  moderation.Decision
	Excerpt                                   string
	ReceivedAt                                time.Time
	runtimeGenerated                          bool
}

func (s *ModerationCaseSink) ApplyModerationAction(ctx context.Context, a ModerationAction) error {
	if !a.runtimeGenerated || a.Decision.Action != moderation.ActionLog || len(a.Decision.Rules) == 0 {
		return ErrModerationCaseState
	}
	rules := make([]any, len(a.Decision.Rules))
	for i, rule := range a.Decision.Rules {
		rules[i] = string(rule)
	}
	b := s.binding
	raw, err := cborx.Marshal([]any{moderationCaseVersion, b.Instance, b.Community, b.Installation, a.Community, a.Group, a.Channel, a.Actor, a.Message,
		a.SourceRevision, rules, string(moderation.ActionLog), uint64(0), moderation.Excerpt(a.Excerpt), uint64(a.ReceivedAt.UnixMilli()), id.ID{}, uint64(0), id.ID{}, uint64(0)})
	if err != nil {
		return err
	}
	if _, err := decodeModerationCase(raw, b); err != nil {
		return err
	}
	key := moderationCaseKey(a.Message, a.SourceRevision)
	current, err := s.native.StateGet(ctx, moderationCaseStateArea, key)
	if err != nil {
		return err
	}
	_, err = s.native.StateTransaction(ctx, id.New(), []native.StateChange{{Area: moderationCaseStateArea, Key: key, ExpectedRevision: current.Revision, Value: raw}})
	return err
}

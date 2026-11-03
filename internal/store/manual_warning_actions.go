package store

import (
	"context"

	"github.com/dilla-chat/dilla/internal/id"
)

// ManualWarningActionRow is the non-sensitive authoritative receipt for a human warning.
// It deliberately contains no reason or evidence bytes.
type ManualWarningActionRow struct {
	CommunityID, RequestID, ActionID id.ID
	ActorUserID, ActorDeviceID       id.ID
	TargetUserID                     id.ID
	Revision                         uint64
	AcceptedAt                       int64
}

// ManualWarningActions persists action receipts so retries and encrypted-case
// attachment can resolve the exact accepted warning.
type ManualWarningActions interface {
	PutManualWarningAction(context.Context, ManualWarningActionRow) error
	GetManualWarningAction(context.Context, id.ID, id.ID) (ManualWarningActionRow, error)
}

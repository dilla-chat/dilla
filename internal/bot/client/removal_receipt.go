package client

import (
	"context"
	"math"
	"net/http"

	"github.com/jonasthim/dilla/internal/id"
)

// AcceptedModerationRemovalReceipt is the immutable structural record of a
// human's signed, server-accepted message removal. It contains no message text.
type AcceptedModerationRemovalReceipt struct {
	ActionID       id.ID
	CommunityID    id.ID
	GroupID        id.ID
	ActorUserID    id.ID
	ActorDeviceID  id.ID
	TargetSequence uint64
	Revision       uint64
	AcceptedAt     uint64
}

func (s *Session) AcceptedModerationRemoval(ctx context.Context, installation, request id.ID) (AcceptedModerationRemovalReceipt, error) {
	var out AcceptedModerationRemovalReceipt
	if s == nil || s.c == nil || s.token == "" || installation.IsZero() || request.IsZero() {
		return out, ErrInvalidConfig
	}
	path := "bot-runtime/installations/" + installation.String() + "/moderation/removals/" + request.String()
	raw, _, err := s.do(ctx, http.MethodGet, path, nil, nil, MaxSmallBody, false)
	if err != nil {
		return out, err
	}
	values, err := decode(raw, 8)
	if err != nil {
		return out, ErrProtocol
	}
	ids := []*id.ID{&out.ActionID, &out.CommunityID, &out.GroupID, &out.ActorUserID, &out.ActorDeviceID}
	for i, destination := range ids {
		value, ok := asID(values[i])
		if !ok || value.IsZero() {
			return AcceptedModerationRemovalReceipt{}, ErrProtocol
		}
		*destination = value
	}
	for i, destination := range []*uint64{&out.TargetSequence, &out.Revision, &out.AcceptedAt} {
		value, ok := asUint(values[i+5])
		if !ok || value == 0 || value > math.MaxInt64 {
			return AcceptedModerationRemovalReceipt{}, ErrProtocol
		}
		*destination = value
	}
	return out, nil
}

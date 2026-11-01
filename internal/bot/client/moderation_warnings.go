package client

import (
	"context"
	"math"
	"net/http"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

// AcceptedManualWarningReceipt is immutable server metadata for the human
// action that an encrypted manual case must accompany.
type AcceptedManualWarningReceipt struct {
	RequestID, ActionID, CommunityID id.ID
	ActorUserID, ActorDeviceID       id.ID
	TargetUserID                     id.ID
	Revision                         uint64
	AcceptedAt                       int64
}

// AcceptedManualWarning resolves the authoritative receipt from the active
// community installation; it never returns the case reason.
func (s *Session) AcceptedManualWarning(ctx context.Context, installation, request id.ID) (AcceptedManualWarningReceipt, error) {
	if s == nil || s.c == nil || installation.IsZero() || request.IsZero() {
		return AcceptedManualWarningReceipt{}, ErrInvalidConfig
	}
	path := "bot-runtime/installations/" + installation.String() + "/moderation/warnings/" + request.String()
	raw, _, err := s.do(ctx, http.MethodGet, path, nil, nil, MaxSmallBody, false)
	if err != nil {
		return AcceptedManualWarningReceipt{}, err
	}
	var tuple []any
	if cborx.Unmarshal(raw, &tuple) != nil || len(tuple) != 8 {
		return AcceptedManualWarningReceipt{}, ErrProtocol
	}
	requestID, okRequest := asID(tuple[0])
	actionID, okAction := asID(tuple[1])
	communityID, okCommunity := asID(tuple[2])
	actorID, okActor := asID(tuple[3])
	deviceID, okDevice := asID(tuple[4])
	targetID, okTarget := asID(tuple[5])
	revision, okRevision := asUint(tuple[6])
	acceptedAt, okAcceptedAt := asUint(tuple[7])
	if !okRequest || requestID != request || !okAction || actionID.IsZero() || !okCommunity || communityID.IsZero() ||
		!okActor || actorID.IsZero() || !okDevice || deviceID.IsZero() || !okTarget || targetID.IsZero() ||
		!okRevision || revision == 0 || revision > math.MaxInt64 || !okAcceptedAt || acceptedAt == 0 || acceptedAt > math.MaxInt64 {
		return AcceptedManualWarningReceipt{}, ErrProtocol
	}
	return AcceptedManualWarningReceipt{RequestID: requestID, ActionID: actionID, CommunityID: communityID,
		ActorUserID: actorID, ActorDeviceID: deviceID, TargetUserID: targetID,
		Revision: revision, AcceptedAt: int64(acceptedAt)}, nil
}

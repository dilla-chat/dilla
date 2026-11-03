//go:build linux

package main

import (
	"context"
	"math"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

type manualRemovalLookup struct{ Session *client.Session }

func (r manualRemovalLookup) AcceptedManualRemoval(ctx context.Context, installation, request id.ID) (service.AcceptedManualRemovalReceipt, error) {
	if r.Session == nil {
		return service.AcceptedManualRemovalReceipt{}, client.ErrInvalidConfig
	}
	receipt, err := r.Session.AcceptedModerationRemoval(ctx, installation, request)
	if err != nil {
		return service.AcceptedManualRemovalReceipt{}, err
	}
	if receipt.AcceptedAt == 0 || receipt.AcceptedAt > math.MaxInt64 {
		return service.AcceptedManualRemovalReceipt{}, client.ErrProtocol
	}
	return service.AcceptedManualRemovalReceipt{
		ActionID: receipt.ActionID, CommunityID: receipt.CommunityID, GroupID: receipt.GroupID,
		ActorUserID: receipt.ActorUserID, ActorDeviceID: receipt.ActorDeviceID,
		TargetSequence: receipt.TargetSequence, Revision: receipt.Revision, AcceptedAt: int64(receipt.AcceptedAt),
	}, nil
}

type manualWarningLookup struct{ Session *client.Session }

func (r manualWarningLookup) AcceptedManualWarning(ctx context.Context, installation, request id.ID) (service.AcceptedManualWarningReceipt, error) {
	if r.Session == nil {
		return service.AcceptedManualWarningReceipt{}, client.ErrInvalidConfig
	}
	receipt, err := r.Session.AcceptedManualWarning(ctx, installation, request)
	if err != nil {
		return service.AcceptedManualWarningReceipt{}, err
	}
	return service.AcceptedManualWarningReceipt{
		RequestID: receipt.RequestID, ActionID: receipt.ActionID, CommunityID: receipt.CommunityID,
		ActorUserID: receipt.ActorUserID, ActorDeviceID: receipt.ActorDeviceID, TargetUserID: receipt.TargetUserID,
		Revision: receipt.Revision, AcceptedAt: receipt.AcceptedAt,
	}, nil
}

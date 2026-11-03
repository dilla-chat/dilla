//go:build linux

package main

import (
	"context"
	"errors"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

type autoModerationGrantSession interface {
	RedeemAutoModerationGrant(context.Context, id.ID, string, id.ID, id.ID, id.ID, uint64, uint64, id.ID, id.ID, [32]byte) (bool, id.ID, error)
}

type autoModerationGrantRedeemer struct {
	Session        autoModerationGrantSession
	InstallationID id.ID
}

func (r autoModerationGrantRedeemer) RedeemAutoModerationGrant(ctx context.Context, token string, binding service.AutoModerationGrantBinding) (bool, error) {
	if r.Session == nil || r.InstallationID.IsZero() || token == "" || binding.CommunityID.IsZero() || binding.Group.IsZero() || binding.RequestID.IsZero() || binding.ActorUser.IsZero() || binding.ActorDevice.IsZero() || binding.Operation != protocol.OpAutoModerationPut {
		return false, errors.New("invalid automatic moderation grant binding")
	}
	accepted, receipt, err := r.Session.RedeemAutoModerationGrant(ctx, r.InstallationID, token,
		binding.CommunityID, binding.Group, binding.RequestID, binding.ExpectedRevision, binding.RequestExpires,
		binding.ActorUser, binding.ActorDevice, binding.ConfigHash)
	if err != nil {
		return false, err
	}
	if accepted && receipt.IsZero() {
		return false, errors.New("automatic moderation grant receipt is invalid")
	}
	return accepted, nil
}

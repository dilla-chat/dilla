package api

import (
	"context"
	"errors"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// requireCommunityWire applies the community compatibility policy to the already-authorized
// resource. The current installation state is read for each request. Store failures fail closed
// as an availability refusal rather than making the community appear unenabled.
func requireCommunityWire(ctx context.Context, repo store.Repository, wireVersion uint64, communityID *id.ID) error {
	policy := capability.New(repo)
	err := policy.RequireCommunity(ctx, wireVersion, communityID)
	var upgrade *capability.UpgradeRequired
	if errors.As(err, &upgrade) {
		return server.Errorf(server.CodeUpgradeRequired, "this community needs wire version %d", auth.WireVersion)
	}
	if err != nil {
		return server.Unavailable(1000, "compatibility state unavailable")
	}
	return nil
}

// requireBotWire admits a managed Bot session only on the current wire (Bot v2: wire 6).
func requireBotWire(wireVersion uint64) error {
	if wireVersion == auth.WireVersion {
		return nil
	}
	return server.Errorf(server.CodeUpgradeRequired, "managed Bots negotiate wire version %d", auth.WireVersion)
}

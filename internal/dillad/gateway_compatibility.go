package dillad

import (
	"context"

	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// gatewayScopeResolver keeps the gateway's compatibility lookup narrow while sourcing the same
// committed group and channel bindings used by the HTTP and delivery-service paths.
type gatewayScopeResolver struct{ repo store.Repository }

func (r gatewayScopeResolver) GroupResource(ctx context.Context, groupID id.ID) (gateway.GroupResource, error) {
	group, err := r.repo.GetGroup(ctx, groupID)
	if err != nil {
		return gateway.GroupResource{}, err
	}
	return gateway.GroupResource{Kind: group.Kind, CommunityID: group.CommunityID}, nil
}

func (r gatewayScopeResolver) ChannelCommunity(ctx context.Context, channelID id.ID) (*id.ID, error) {
	channel, err := r.repo.GetChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	return channel.CommunityID, nil
}

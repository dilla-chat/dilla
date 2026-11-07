//go:build linux

package main

import (
	"context"
	"errors"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/community/welcome"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

func configuredWelcomeChannel(rows []client.Channel, community, channel id.ID) (client.Channel, error) {
	for _, row := range rows {
		if row.ID != channel {
			continue
		}
		if row.CommunityID != community || row.Kind != 0 || row.TextGroupID == nil || row.TextGroupID.IsZero() {
			return client.Channel{}, errors.New("configured welcome destination is not an accessible text channel")
		}
		return row, nil
	}
	return client.Channel{}, errors.New("configured welcome destination is unavailable")
}

// The installation's welcome and roles capability bits.
const (
	botWelcomeCapability uint64 = 1 << 1
	botRolesCapability   uint64 = 1 << 2
)

type welcomeRoleGranter struct {
	members      *client.CommunityMembers
	installation id.ID
}

func (g welcomeRoleGranter) GrantJoinRole(ctx context.Context, member, episode id.ID) error {
	_, _, err := g.members.GrantJoinRole(ctx, g.installation, member, episode)
	return err
}

// newWelcomeHandler is the member-join handler: the configured join role (Roles capability) and the welcome message
// (Welcome capability). Bot v2 task 14: the feed (feed_handlers.go) calls it for each member join it hears on op 65,
// with an event whose id and time are fixed per membership episode; it was a structural-event consumer until Task 12.
// With neither capability it is nil.
func newWelcomeHandler(n *native.Client, session *client.Session, authority *client.Client, community, installation id.ID, capabilities uint64) (func(context.Context, protocol.Event) error, error) {
	if capabilities&(botWelcomeCapability|botRolesCapability) == 0 {
		return nil, nil
	}
	if n == nil || session == nil || authority == nil || community.IsZero() || installation.IsZero() {
		return nil, errors.New("missing welcome runtime dependency")
	}
	members, err := session.CommunityMembers(community)
	if err != nil {
		return nil, err
	}
	coordinator, err := service.NewSendCoordinator(n, authority)
	if err != nil {
		return nil, err
	}
	destination := service.WelcomeDestinationResolver{Session: session, Native: n}
	var granter welcome.JoinRoleGranter
	if capabilities&botRolesCapability != 0 {
		granter = welcomeRoleGranter{members: members, installation: installation}
	}
	return welcome.ConfiguredEventHandlerWithCapabilities(n, granter, capabilities&botWelcomeCapability != 0, func(ctx context.Context, cfg welcome.Config) (*welcome.Processor, error) {
		channels, err := session.Channels(ctx, community)
		if err != nil {
			return nil, err
		}
		channel, err := configuredWelcomeChannel(channels, community, cfg.ChannelID)
		if err != nil {
			return nil, err
		}
		return welcome.New(n, welcome.Options{
			ChannelID: cfg.ChannelID, ExpectedGroupID: *channel.TextGroupID,
			Template: cfg.Template, ServerName: cfg.ServerName, ChannelName: channel.Name,
			Members: members, Destination: destination,
			Sender: service.WelcomeSender{Coordinator: coordinator, Authority: authority, Session: session, Native: n,
				CommunityID: community, InstallationID: installation, ExpectedGroupID: *channel.TextGroupID},
		})
	}), nil
}

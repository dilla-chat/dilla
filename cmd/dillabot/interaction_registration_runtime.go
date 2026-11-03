//go:build linux

package main

import (
	"context"
	"errors"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

type interactionRegistrationStageFunc func(context.Context, id.ID, id.ID) error
type interactionRegistrationEpisodeResolver interface {
	ResolveCurrentEpisode(context.Context, id.ID) (id.ID, bool, error)
}

// interactionRegistrationEventHandler stages one durable kind-3 registration
// per member-join episode. Admission and the signed context still decide
// eligibility; the structural event only supplies stable candidate IDs.
func interactionRegistrationEventHandler(installation id.ID, stage interactionRegistrationStageFunc) service.StructuralEventHandler {
	return func(ctx context.Context, event protocol.Event) error {
		if event.Kind != protocol.EventMemberJoin {
			return nil
		}
		if installation.IsZero() || stage == nil || len(event.Data) != 2 {
			return errors.New("invalid member-join interaction registration event")
		}
		user, userOK := event.Data[0].(id.ID)
		episode, episodeOK := event.Data[1].(id.ID)
		if !userOK || !episodeOK || user.IsZero() || episode.IsZero() || event.ID.IsZero() || event.CommunityID.IsZero() {
			return errors.New("member-join event lacks exact user, episode, or event identifiers")
		}
		return stage(ctx, user, episode)
	}
}

func newInteractionRegistrationHandler(coordinator *service.InteractionRegistrationCoordinator, installation id.ID, resolver interactionRegistrationEpisodeResolver) (service.StructuralEventHandler, error) {
	if coordinator == nil || installation.IsZero() || resolver == nil {
		return nil, errors.New("missing interaction registration coordinator")
	}
	stage := func(ctx context.Context, user, episode id.ID) error {
		return coordinator.QueueMemberJoin(ctx, user, episode)
	}
	return composeStructuralEventHandlers(
		interactionRegistrationEventHandler(installation, stage),
		interactionRegistrationDeviceListEventHandler(installation, resolver, stage),
	), nil
}

// interactionRegistrationDeviceListEventHandler turns a signed device-list
// publication into a registration retry only when the user still has a current
// membership episode in this community. The event carries no episode authority.
func interactionRegistrationDeviceListEventHandler(installation id.ID, resolver interactionRegistrationEpisodeResolver, stage interactionRegistrationStageFunc) service.StructuralEventHandler {
	return func(ctx context.Context, event protocol.Event) error {
		if event.Kind != protocol.EventDeviceList {
			return nil
		}
		if installation.IsZero() || resolver == nil || stage == nil || len(event.Data) != 2 {
			return errors.New("invalid device-list interaction registration event")
		}
		user, userOK := event.Data[0].(id.ID)
		version, versionOK := event.Data[1].(uint64)
		if !userOK || !versionOK || user.IsZero() || version == 0 || event.ID.IsZero() || event.CommunityID.IsZero() {
			return errors.New("device-list event lacks exact user, version, or event identifiers")
		}
		episode, present, err := resolver.ResolveCurrentEpisode(ctx, user)
		if err != nil || !present {
			return err
		}
		if episode.IsZero() {
			return errors.New("current member episode is empty")
		}
		return stage(ctx, user, episode)
	}
}

func composeStructuralEventHandlers(handlers ...service.StructuralEventHandler) service.StructuralEventHandler {
	return func(ctx context.Context, event protocol.Event) error {
		for _, handler := range handlers {
			if handler == nil {
				continue
			}
			if err := handler(ctx, event); err != nil {
				return err
			}
		}
		return nil
	}
}

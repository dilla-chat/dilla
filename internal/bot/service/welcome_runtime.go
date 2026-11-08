package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/community/welcome"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// WelcomeDestinationResolver checks the authenticated current channel view and
// the local MLS group binding before returning a text destination.
//
// Bot v2 Task 17: the channel must also be a Bot channel (CheckBotChannel). A feature aimed at any other channel posts
// nothing, is flagged needs_bot_channel under Feature, and gets an error matching ErrNeedsBotChannel. Welcome messages,
// scheduled announcements, role panels, polls and music cards all resolve their group here.
type WelcomeDestinationResolver struct {
	Session channelLister
	Native  interface {
		InspectGroup(context.Context, id.ID) (native.GroupInfo, error)
	}
	// Feature names the caller in the needs_bot_channel flag ("welcome", "announcements", "polls", "role_panels",
	// "music"); empty is "post".
	Feature string
}

type channelLister interface {
	Channels(context.Context, id.ID) ([]client.Channel, error)
}

func (r WelcomeDestinationResolver) ResolveTextGroup(ctx context.Context, community, channel id.ID) (id.ID, error) {
	if isNilSession(r.Session) || r.Native == nil || community.IsZero() || channel.IsZero() {
		return id.ID{}, errors.New("missing welcome destination dependency")
	}
	feature := r.Feature
	if feature == "" {
		feature = "post"
	}
	if err := CheckBotChannel(community, channel); err != nil {
		if errors.Is(err, ErrNeedsBotChannel) {
			FlagNeedsBotChannel(community, feature, channel)
		}
		logRefusal(ctx, feature, channel, err)
		return id.ID{}, err
	}
	ClearNeedsBotChannel(community, feature)
	channels, err := r.Session.Channels(ctx, community)
	if err != nil {
		return id.ID{}, err
	}
	for _, ch := range channels {
		if ch.ID != channel {
			continue
		}
		if ch.CommunityID != community || ch.Kind != 0 || ch.TextGroupID == nil || ch.TextGroupID.IsZero() {
			return id.ID{}, errors.New("welcome destination is not an active text channel")
		}
		group, err := r.Native.InspectGroup(ctx, *ch.TextGroupID)
		if err != nil {
			// Not (yet) in the Bot's native state: a Bot channel marked a moment ago is joined from its kept Welcome later.
			err = fmt.Errorf("%w: %w", ErrBotNotJoined, err)
			logRefusal(ctx, feature, channel, err)
			return id.ID{}, err
		}
		if group.ID != *ch.TextGroupID || group.State != 2 || group.Community == nil || *group.Community != community || group.Target != channel {
			err = errors.New("welcome destination MLS group is not authorized for this channel")
			if group.State != 2 {
				err = ErrBotNotJoined
			}
			logRefusal(ctx, feature, channel, err)
			return id.ID{}, err
		}
		return group.ID, nil
	}
	return id.ID{}, errors.New("configured welcome channel is unavailable")
}

// WelcomeSender sends through SendCoordinator, which persists the exact MLS
// ciphertext in the native outbox and resolves uncertain POST outcomes using
// the authenticated server receipt APIs.
type WelcomeSender struct {
	Coordinator *SendCoordinator
	Authority   interface {
		DiscoverObserved(context.Context) (client.Discovery, client.ResponseObservation, error)
	}
	Session *client.Session
	Native  interface {
		ConfirmSendReceipt(context.Context, id.ID, []byte) error
	}
	CommunityID, InstallationID, ExpectedGroupID id.ID
}

func (s WelcomeSender) SendWelcome(ctx context.Context, req welcome.SendRequest) (protocol.SendReceipt, error) {
	if s.Coordinator == nil || s.Authority == nil || s.Session == nil || req.OperationID.IsZero() || req.AttemptID.IsZero() || req.GroupID.IsZero() || req.CommunityID.IsZero() || req.InstallationID.IsZero() || req.Text == "" || req.CommunityID != s.CommunityID || req.InstallationID != s.InstallationID || req.GroupID != s.ExpectedGroupID {
		return protocol.SendReceipt{}, errors.New("missing welcome sender prerequisite")
	}
	discovery, _, err := s.Authority.DiscoverObserved(ctx)
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	// Security review (task 8b): text a member authors never carries the music player marker.
	body, err := cborx.Marshal([]any{uint64(0), nil, DefangMarkers(req.Text), []any{}, nil})
	if err != nil {
		return protocol.SendReceipt{}, err
	}
	return s.Coordinator.Send(ctx, s.Session, SendInput{OperationID: req.OperationID, AttemptID: req.AttemptID, GroupID: req.GroupID, RequestBody: body, OriginalNow: req.OriginalNow, ExpectedGeneration: discovery.Generation})
}

func (s WelcomeSender) ResolveWelcome(ctx context.Context, operation, attempt id.ID) (protocol.SendReceipt, bool, error) {
	if s.Session == nil || s.Native == nil || s.CommunityID.IsZero() || s.ExpectedGroupID.IsZero() || operation.IsZero() || attempt.IsZero() {
		return protocol.SendReceipt{}, false, errors.New("missing welcome receipt resolver prerequisite")
	}
	receipt, err := s.Session.BotMessageReceipt(ctx, s.CommunityID, operation, attempt, s.ExpectedGroupID)
	if errors.Is(err, client.ErrBotSendReceiptNotFound) {
		return protocol.SendReceipt{}, false, nil
	}
	if err != nil {
		return protocol.SendReceipt{}, false, err
	}
	body, err := protocol.EncodeReceipt(receipt)
	if err != nil {
		return protocol.SendReceipt{}, false, err
	}
	if err = s.Native.ConfirmSendReceipt(ctx, operation, body); err != nil {
		return protocol.SendReceipt{}, false, err
	}
	return receipt, true, nil
}

// isNilSession reports a missing session, including a typed nil *client.Session in the interface.
func isNilSession(s channelLister) bool {
	if s == nil {
		return true
	}
	session, ok := s.(*client.Session)
	return ok && session == nil
}

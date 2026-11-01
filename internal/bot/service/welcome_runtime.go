package service

import (
	"context"
	"errors"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/community/welcome"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

// WelcomeDestinationResolver checks the authenticated current channel view and
// the local MLS group binding before returning a text destination.
type WelcomeDestinationResolver struct {
	Session *client.Session
	Native  interface {
		InspectGroup(context.Context, id.ID) (native.GroupInfo, error)
	}
}

func (r WelcomeDestinationResolver) ResolveTextGroup(ctx context.Context, community, channel id.ID) (id.ID, error) {
	if r.Session == nil || r.Native == nil || community.IsZero() || channel.IsZero() {
		return id.ID{}, errors.New("missing welcome destination dependency")
	}
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
			return id.ID{}, err
		}
		if group.ID != *ch.TextGroupID || group.State != 2 || group.Community == nil || *group.Community != community || group.Target != channel {
			return id.ID{}, errors.New("welcome destination MLS group is not authorized for this channel")
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
	body, err := cborx.Marshal([]any{uint64(0), nil, NeutralizeBotMarkers(req.Text), []any{}, nil})
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

package service

import (
	"context"
	"errors"

	"github.com/jonasthim/dilla/internal/api"
	"github.com/jonasthim/dilla/internal/bot/announcements"
	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

type announcementPermissions interface {
	ChannelPermissions(context.Context, id.ID) (uint64, error)
}

// AnnouncementAuthorization checks the authenticated user's effective channel
// permissions at the time an announcement is considered for sending.
type AnnouncementAuthorization struct{ Session announcementPermissions }

func (a AnnouncementAuthorization) CanAnnounce(ctx context.Context, channel id.ID) (bool, error) {
	if a.Session == nil || channel.IsZero() {
		return false, errors.New("missing announcement authorization dependency")
	}
	bits, err := a.Session.ChannelPermissions(ctx, channel)
	if err != nil {
		return false, err
	}
	return api.Bits(bits).Has(api.PermSendMessages), nil
}

type announcementCoordinator interface {
	Send(context.Context, *client.Session, SendInput) (protocol.SendReceipt, error)
}

type announcementAuthority interface {
	DiscoverObserved(context.Context) (client.Discovery, client.ResponseObservation, error)
}

type announcementReceiptSession interface {
	BotMessageReceipt(context.Context, id.ID, id.ID, id.ID, id.ID) (protocol.SendReceipt, error)
}

type announcementReceiptNative interface {
	ConfirmSendReceipt(context.Context, id.ID, []byte) error
}

// AnnouncementSender adapts scheduled announcement requests to the durable
// send coordinator. It never logs announcement text.
type AnnouncementSender struct {
	Coordinator                 announcementCoordinator
	Authority                   announcementAuthority
	Session                     *client.Session
	CommunityID, InstallationID id.ID
}

func (s AnnouncementSender) SendAnnouncement(ctx context.Context, req announcements.SendRequest) (protocol.SendReceipt, error) {
	if s.Coordinator == nil || s.Authority == nil || s.Session == nil || req.OperationID.IsZero() || req.AttemptID.IsZero() || req.GroupID.IsZero() || req.CommunityID.IsZero() || req.InstallationID.IsZero() || req.CommunityID != s.CommunityID || req.InstallationID != s.InstallationID || req.Text == "" {
		return protocol.SendReceipt{}, errors.New("missing announcement sender prerequisite")
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
	if req.BeforeSend == nil || !req.BeforeSend() {
		return protocol.SendReceipt{}, errors.New("announcement send authorization changed")
	}
	return s.Coordinator.Send(ctx, s.Session, SendInput{OperationID: req.OperationID, AttemptID: req.AttemptID, PredecessorAttemptID: req.PredecessorAttemptID, GroupID: req.GroupID, RequestBody: body, OriginalNow: req.OriginalNow, ExpectedGeneration: discovery.Generation})
}

// AnnouncementReceiptLookup recovers an attempt receipt and confirms it in the
// native outbox without starting another send.
type AnnouncementReceiptLookup struct {
	Session announcementReceiptSession
	Native  announcementReceiptNative
}

func (l AnnouncementReceiptLookup) LookupAnnouncement(ctx context.Context, req announcements.SendRequest) (protocol.SendReceipt, bool, error) {
	if l.Session == nil || l.Native == nil || req.CommunityID.IsZero() || req.OperationID.IsZero() || req.AttemptID.IsZero() || req.GroupID.IsZero() {
		return protocol.SendReceipt{}, false, errors.New("missing announcement receipt dependency")
	}
	receipt, err := l.Session.BotMessageReceipt(ctx, req.CommunityID, req.OperationID, req.AttemptID, req.GroupID)
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
	if err = l.Native.ConfirmSendReceipt(ctx, req.OperationID, body); err != nil {
		return protocol.SendReceipt{}, false, err
	}
	return receipt, true, nil
}

// NewAnnouncementScheduler builds the runtime adapters used by cmd/dillabot.
func NewAnnouncementScheduler(n *native.Client, session *client.Session, authority *client.Client) (*announcements.Scheduler, error) {
	if n == nil || session == nil || authority == nil {
		return nil, errors.New("missing announcement runtime dependency")
	}
	coordinator, err := NewSendCoordinator(n, authority)
	if err != nil {
		return nil, err
	}
	binding := n.Binding()
	return announcements.NewScheduler(n, announcements.SchedulerOptions{
		Destination:   WelcomeDestinationResolver{Session: session, Native: n},
		Authorization: AnnouncementAuthorization{Session: session},
		Sender:        AnnouncementSender{Coordinator: coordinator, Authority: authority, Session: session, CommunityID: binding.Community, InstallationID: binding.Installation},
		ReceiptLookup: AnnouncementReceiptLookup{Session: session, Native: n},
	})
}

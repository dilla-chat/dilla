//go:build linux

package main

import (
	"context"
	"errors"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

type durableInteractionResponseSender struct {
	api    *client.Client
	outbox *service.InteractionResponseOutbox
}

func (s durableInteractionResponseSender) ResumeInteractionResponse(ctx context.Context, event native.GroupEvent, preflight func(context.Context) error) (bool, error) {
	if s.outbox == nil {
		return false, errors.New("missing interaction response outbox")
	}
	receipt, found, err := s.outbox.ResumeWithPreflight(ctx, event, preflight)
	if err != nil || !found {
		return false, err
	}
	if receipt.Outcome != protocol.ReceiptAccepted {
		return false, errInteractionResponseNotAccepted
	}
	return true, nil
}

func (s durableInteractionResponseSender) SendInteractionResponse(ctx context.Context, event native.GroupEvent, response string) (bool, error) {
	if s.api == nil || s.outbox == nil || response == "" {
		return false, errors.New("missing interaction response sender dependency")
	}
	discovery, _, err := s.api.DiscoverObserved(ctx)
	if err != nil {
		return false, err
	}
	receipt, err := s.outbox.Send(ctx, event, []byte(response), id.New(), id.New(), unixSecondsOrZero(time.Now()), discovery.Generation)
	if err != nil {
		return false, err
	}
	if receipt.Outcome != protocol.ReceiptAccepted {
		return false, errInteractionResponseNotAccepted
	}
	return true, nil
}

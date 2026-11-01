package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/protocol"
)

// Only a deterministic refusal may let the event handler acknowledge a saved read response as
// undeliverable (Jonas 2026-10-30). protocol/10-community-bot.md: 0 ok, 1 invalid, 2 forbidden,
// 3 expired, 4 conflict, 5 unavailable, 6 busy, 7 not_found, 8 upgrade_required, 9 internal.
func TestReadReplayReauthorizationRefusesDeterministicStatusesAndKeepsTheRestPending(t *testing.T) {
	refused := map[uint64]bool{1: true, 2: true, 3: true, 7: true, 8: true}
	for status := uint64(1); status <= 9; status++ {
		t.Run(protocolStatusName(status), func(t *testing.T) {
			d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
				protocol.OpOverviewGet: {RecomputeOnReplay: true, Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
					return InteractionResult{Status: status}, nil
				}},
			})
			event.Body = ptrString(interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil))
			err := d.ReauthorizeReadReplay(context.Background(), event)
			if refused[status] {
				if !errors.Is(err, ErrInteractionReadReplayNotAuthorized) || errors.Is(err, ErrInteractionReadReplayPending) {
					t.Fatalf("status %d read replay = %v; want a definitive refusal", status, err)
				}
				return
			}
			if errors.Is(err, ErrInteractionReadReplayNotAuthorized) || !errors.Is(err, ErrInteractionReadReplayPending) {
				t.Fatalf("status %d read replay = %v; a status that may clear must stay pending", status, err)
			}
		})
	}
}

func TestReadReplayReauthorizationKeepsHandlerAndDispatchErrorsPending(t *testing.T) {
	t.Run("handler error (answered as internal)", func(t *testing.T) {
		d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
			protocol.OpOverviewGet: {RecomputeOnReplay: true, Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
				return InteractionResult{}, errors.New("authority lookup failed")
			}},
		})
		event.Body = ptrString(interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil))
		err := d.ReauthorizeReadReplay(context.Background(), event)
		if errors.Is(err, ErrInteractionReadReplayNotAuthorized) || !errors.Is(err, ErrInteractionReadReplayPending) {
			t.Fatalf("handler error read replay = %v; want pending", err)
		}
	})
	t.Run("dispatch error", func(t *testing.T) {
		d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
			protocol.OpOverviewGet: {RecomputeOnReplay: true, Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
				t.Fatal("handler reached past a dispatch error")
				return InteractionResult{}, nil
			}},
		})
		event.Body = ptrString(interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil))
		event.SourceSequence = 0 // Dispatch refuses the event itself
		err := d.ReauthorizeReadReplay(context.Background(), event)
		if err == nil || errors.Is(err, ErrInteractionReadReplayNotAuthorized) {
			t.Fatalf("dispatch error read replay = %v; want a pending error", err)
		}
	})
}

func TestReadReplayReauthorizationKeepsCancelledContextPending(t *testing.T) {
	t.Run("cancelled before dispatch", func(t *testing.T) {
		calls := 0
		d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
			protocol.OpOverviewGet: {RecomputeOnReplay: true, Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
				calls++
				return InteractionResult{Status: interactionStatusForbidden}, nil
			}},
		})
		event.Body = ptrString(interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := d.ReauthorizeReadReplay(ctx, event)
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrInteractionReadReplayNotAuthorized) || calls != 0 {
			t.Fatalf("cancelled read replay = %v calls=%d; want pending cancellation", err, calls)
		}
	})
	t.Run("cancelled while the handler denies", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		d, event, _ := interactionFixture(t, map[protocol.Operation]InteractionHandlerSpec{
			protocol.OpOverviewGet: {RecomputeOnReplay: true, Handle: func(context.Context, InteractionActor, protocol.Request) (InteractionResult, error) {
				cancel() // e.g. an authority lookup cut short answers "forbidden"
				return InteractionResult{Status: interactionStatusForbidden}, nil
			}},
		})
		event.Body = ptrString(interactionBody(t, nil, interactionID(8), protocol.OpOverviewGet, nil, nil, nil))
		err := d.ReauthorizeReadReplay(ctx, event)
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrInteractionReadReplayNotAuthorized) {
			t.Fatalf("read replay denied under cancellation = %v; want pending cancellation", err)
		}
	})
}

func protocolStatusName(status uint64) string {
	return [...]string{"ok", "invalid", "forbidden", "expired", "conflict", "unavailable", "busy", "not_found", "upgrade_required", "internal"}[status]
}

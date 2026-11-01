//go:build linux

package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"regexp"
	"time"

	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

type automaticModerationEventHandler interface {
	Handle(context.Context, native.GroupEvent, bool, []string) (moderation.Decision, error)
}

type groupEventRouter struct {
	inspector             interactionEventInspection
	interactions          service.GroupEventHandler
	replyRecovery         service.GroupEventHandler
	automoderation        automaticModerationEventHandler
	replayThrough         uint64
	replayReceivedThrough uint64
}

var moderationMentionPattern = regexp.MustCompile(`<@([0-9a-f]{32})>`)

func newGroupEventRouter(inspector interactionEventInspection, interactions service.GroupEventHandler, automoderation automaticModerationEventHandler, replayThrough, replayReceivedThrough uint64, replyRecovery ...service.GroupEventHandler) service.GroupEventHandler {
	var recovery service.GroupEventHandler
	if len(replyRecovery) > 0 {
		recovery = replyRecovery[0]
	}
	return groupEventRouter{inspector: inspector, interactions: interactions, replyRecovery: recovery, automoderation: automoderation, replayThrough: replayThrough, replayReceivedThrough: replayReceivedThrough}
}

func (h groupEventRouter) HandleGroupEvent(ctx context.Context, event native.GroupEvent) error {
	if ctx == nil || h.inspector == nil || event.DeliveryID == 0 || event.Group.IsZero() {
		return errors.New("missing or invalid bot group event dependency")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	group, err := inspectInteractionEventGroup(ctx, h.inspector, event)
	if err != nil {
		return err
	}
	if group.ID != event.Group {
		return errors.New("native bot event group inspection mismatch")
	}
	switch group.Kind {
	case 0: // Native text group: only authenticated message and edit snapshots are eligible.
		if event.EnvelopeType != 0 && event.EnvelopeType != 1 {
			return nil
		}
		if h.automoderation == nil {
			return h.recoverReply(ctx, event, nil)
		}
		mentions := moderationMentionIDs(event.Body)
		now := time.Now().UnixMilli()
		// Server receipt time catches source pages synced into the journal after
		// the process-start delivery high-water. Future timestamps are uncertain
		// (for example host/server clock skew), so they are replay-only too.
		receivedAtTooLarge := event.ReceivedAt > uint64(math.MaxInt64/1000)
		receivedAtMS := uint64(0)
		if !receivedAtTooLarge {
			receivedAtMS = event.ReceivedAt * 1000
		}
		replay := event.DeliveryID <= h.replayThrough || event.ReceivedAt > 0 && (receivedAtTooLarge || receivedAtMS <= h.replayReceivedThrough) ||
			event.ReceivedAt > 0 && receivedAtMS > uint64(now)
		_, err := h.automoderation.Handle(ctx, event, replay, mentions)
		var stageErr service.AutoModerationStageError
		if errors.As(err, &stageErr) {
			sourceAgeSeconds := int64(0)
			nowSeconds := now / 1000
			if nowSeconds >= 0 && event.ReceivedAt <= uint64(nowSeconds) {
				ageSeconds := uint64(nowSeconds) - event.ReceivedAt
				if ageSeconds <= math.MaxInt64 {
					sourceAgeSeconds = int64(ageSeconds)
				}
			}
			slog.Warn("automatic moderation event handling failed", "stage", stageErr.Stage,
				"error_code", stageErr.Code, "http_status", stageErr.Status, "source_age_seconds", sourceAgeSeconds, "deferral_count", stageErr.Deferrals)
		}
		// Moderation decisions affect moderation only; text is never a command.
		return h.recoverReply(ctx, event, err)
	case 3: // Managed kind-3 interaction behavior retains its existing durable sender path.
		if h.interactions == nil {
			return errors.New("missing native interaction event handler")
		}
		return h.interactions.HandleGroupEvent(ctx, event)
	default:
		// The helper journal only emits snapshots from text and interaction groups.
		// Unknown future kinds are consumed without interpreting their body.
		return nil
	}
}

func moderationMentionIDs(body *string) []string {
	if body == nil {
		return nil
	}
	matches := moderationMentionPattern.FindAllStringSubmatch(*body, -1)
	if len(matches) == 0 {
		return nil
	}
	mentions := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		if _, err := id.Parse(match[1]); err == nil {
			mentions = append(mentions, match[1])
		}
	}
	return mentions
}

func (h groupEventRouter) recoverReply(ctx context.Context, event native.GroupEvent, prior error) error {
	if h.replyRecovery == nil {
		return prior
	}
	return errors.Join(prior, h.replyRecovery.HandleGroupEvent(ctx, event))
}

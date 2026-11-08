package gatewayclient

import (
	"context"

	"github.com/dilla-chat/dilla/internal/id"
)

// WebhookAck answers d, one op 66 bot_webhook, with op 67 bot_webhook_ack `[delivery_id, status]` on the connection d
// arrived on (Bot v2 Task 17). dillad takes the ack only from that connection, within 2 s, and answers the webhook's
// sender 204 for status 0 and 503 for any other status. Send it only once the message is posted.
//
// When that connection is gone (a resume or a reconnect happened while the message was being posted; fix round 2, M4)
// the ack is not sent on the new socket: dillad has already answered the sender 503, so the sender retries and the
// message may be posted twice. Delivery is at least once (protocol/10 "Webhook posts"). ErrStaleDelivery says so.
func (d Delivery) WebhookAck(ctx context.Context, delivery id.ID, status uint64) error {
	if d.client == nil || delivery.IsZero() {
		return ErrProtocol
	}
	return d.client.sendFrameOn(ctx, d.conn, OpBotWebhookAck, nil, []any{delivery[:], status})
}

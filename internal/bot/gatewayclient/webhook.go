package gatewayclient

import (
	"context"

	"github.com/dilla-chat/dilla/internal/id"
)

// WebhookAck answers one op 66 bot_webhook with op 67 bot_webhook_ack `[delivery_id, status]` on the connection now
// open (Bot v2 Task 17). dillad takes the ack only from the connection it sent the delivery on, within 2 s, and answers
// the webhook's sender 204 for status 0 and 503 for any other status. Send it only once the message is posted.
func (c *Client) WebhookAck(ctx context.Context, delivery id.ID, status uint64) error {
	if delivery.IsZero() {
		return ErrProtocol
	}
	return c.sendFrame(ctx, OpBotWebhookAck, nil, []any{delivery[:], status})
}

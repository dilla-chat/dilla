package native

import (
	"context"
	"errors"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

// CallJoinCheck applies the native media-key gate to a staged external call-group join.
// It must succeed before the external commit is posted to the instance.
func (c *Client) CallJoinCheck(ctx context.Context, call id.ID) error {
	if call.IsZero() {
		return errors.New("invalid native call ID")
	}
	_, err := c.call(ctx, protocol.HelperCallJoinCheck, []any{call}, true, voidResult)
	return err
}

// CallGroupForget removes a gone call-group record so a later call for the same channel can be joined.
func (c *Client) CallGroupForget(ctx context.Context, call id.ID) error {
	if call.IsZero() {
		return errors.New("invalid native call ID")
	}
	_, err := c.call(ctx, protocol.HelperCallGroupForget, []any{call}, true, voidResult)
	return err
}

// GroupForget drops a text group's local MLS state (helper op 27, Bot v2 Task 23): the Bot keeps none for a group whose
// channel is not a Bot channel, or that it was removed from. An unknown group is a no-op.
func (c *Client) GroupForget(ctx context.Context, group id.ID) error {
	if group.IsZero() {
		return errors.New("invalid group forget input")
	}
	_, err := c.call(ctx, protocol.HelperGroupForget, []any{group}, true, voidResult)
	return err
}

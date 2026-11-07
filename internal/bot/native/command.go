package native

import (
	"context"
	"errors"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

// The sealed-command ops (Bot v2 Task 8) on the installation's native helper, for the command intake and
// the catalog publisher (Task 15). They decode as helper.Client's wrappers do.

// CommandKey is op 21: the signed publication of community's current command key at now; the helper
// mints the first key and rotates a 7-day-old one.
func (c *Client) CommandKey(ctx context.Context, community id.ID, now time.Time) ([]byte, error) {
	seconds := now.Unix()
	if community.IsZero() || seconds <= 0 {
		return nil, errors.New("invalid command key request")
	}
	v, err := c.call(ctx, protocol.HelperCommandKey, []any{community, uint64(seconds)}, true, oneByteString("command key"))
	if err != nil {
		return nil, err
	}
	return resultAs[[]byte](v)
}

// OpenCommand is op 22: it opens and verifies a sealed command dillad attached to user's device.
func (c *Client) OpenCommand(ctx context.Context, sealed []byte, user, device id.ID, now time.Time) (helper.OpenedCommand, error) {
	payload, err := helper.OpenCommandPayload(sealed, user, device, now)
	if err != nil {
		return helper.OpenedCommand{}, err
	}
	v, err := c.call(ctx, protocol.HelperCommandOpen, payload, true, func(result []any) (any, error) {
		return helper.DecodeOpenedCommand(result, user, device)
	})
	if err != nil {
		return helper.OpenedCommand{}, err
	}
	return resultAs[helper.OpenedCommand](v)
}

// SealReply is op 23: answer number seq of requestID, sealed to the request's one-time replyKey.
func (c *Client) SealReply(ctx context.Context, replyKey [32]byte, requestID id.ID, seq uint32, body []byte) ([]byte, error) {
	if requestID.IsZero() || replyKey == [32]byte{} {
		return nil, errors.New("invalid reply seal request")
	}
	if body == nil {
		body = []byte{}
	}
	v, err := c.call(ctx, protocol.HelperReplySeal, []any{replyKey[:], requestID, uint64(seq), body}, false, oneByteString("reply seal"))
	if err != nil {
		return nil, err
	}
	return resultAs[[]byte](v)
}

func oneByteString(what string) func([]any) (any, error) {
	return func(result []any) (any, error) {
		if len(result) != 1 {
			return nil, errors.New(what + " result shape")
		}
		b, ok := result[0].([]byte)
		if !ok || len(b) == 0 {
			return nil, errors.New(what + " result")
		}
		return b, nil
	}
}

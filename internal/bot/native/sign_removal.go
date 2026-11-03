package native

import (
	"context"
	"errors"
	"math"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// SignRemoval asks the native identity to sign a short-lived moderator request.
// The returned body is the exact deterministic [proof,signature] payload for
// the server acceptance endpoint. No signing key is exposed to Go.
func (c *Client) SignRemoval(ctx context.Context, request, group id.ID, target, expires, now uint64) ([]byte, error) {
	if request.IsZero() || group.IsZero() || target == 0 || target > math.MaxInt64 || now == 0 ||
		now > math.MaxInt64 || expires <= now || expires > math.MaxInt64 || expires-now > 30 {
		return nil, errors.New("invalid removal signing input")
	}
	v, err := c.call(ctx, protocol.HelperSignRemoval, []any{request, group, target, expires, now}, true, func(a []any) (any, error) {
		if len(a) != 1 {
			return nil, errors.New("invalid signed removal result")
		}
		body, ok := a[0].([]byte)
		if !ok || len(body) == 0 || len(body) > 512 || cborx.CheckDeterministic(body) != nil {
			return nil, errors.New("invalid signed removal body")
		}
		var fields []any
		if cborx.Unmarshal(body, &fields) != nil || len(fields) != 2 {
			return nil, errors.New("invalid signed removal shape")
		}
		proofBody, ok := fields[0].([]byte)
		if !ok {
			return nil, errors.New("invalid removal proof")
		}
		signature, ok := fields[1].([]byte)
		if !ok || len(signature) != 64 {
			return nil, errors.New("invalid removal signature")
		}
		proof, err := protocol.DecodeRemovalProof(proofBody)
		if err != nil || proof.RequestID != request || proof.GroupID != group || proof.CommunityID != c.binding.Community ||
			proof.ActorUser.IsZero() || proof.ActorDevice.IsZero() || proof.TargetSequence != target || proof.Expires != expires {
			return nil, errors.New("signed removal proof has an invalid binding")
		}
		return append([]byte(nil), body...), nil
	})
	if err != nil {
		return nil, err
	}
	return resultAs[[]byte](v)
}

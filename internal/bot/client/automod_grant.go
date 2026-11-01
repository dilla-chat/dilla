package client

import (
	"context"
	"math"
	"net/http"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const maxAutoModerationGrantResponse = 4096

// RedeemAutoModerationGrant submits only a digest of the exact canonical config document.
// The plaintext remains inside the bot's MLS interaction handler.
func (s *Session) RedeemAutoModerationGrant(ctx context.Context, installation id.ID, token string, community, group, request id.ID, revision, expires uint64, actorUser, actorDevice id.ID, configHash [32]byte) (bool, id.ID, error) {
	if s == nil || s.c == nil || s.token == "" || token == "" || community.IsZero() || group.IsZero() || request.IsZero() || expires == 0 || expires > math.MaxInt64 || actorUser.IsZero() || actorDevice.IsZero() {
		return false, id.Zero, ErrInvalidConfig
	}
	inst, err := pathID(installation)
	if err != nil {
		return false, id.Zero, err
	}
	body, err := encode([]any{token, community, group, request, revision, expires, actorUser, actorDevice, configHash[:]})
	if err != nil {
		return false, id.Zero, err
	}
	raw, _, err := s.do(ctx, http.MethodPost, "bot-runtime/installations/"+inst+"/automod-grants/redeem", nil, body, maxAutoModerationGrantResponse, true)
	if err != nil {
		return false, id.Zero, err
	}
	var result []any
	if len(raw) > maxAutoModerationGrantResponse || cborx.Unmarshal(raw, &result) != nil || len(result) != 2 {
		return false, id.Zero, ErrProtocol
	}
	accepted, ok := asUint(result[0])
	receipt, okID := asID(result[1])
	if !ok || accepted != 1 || !okID || receipt.IsZero() {
		return false, id.Zero, ErrProtocol
	}
	return true, receipt, nil
}

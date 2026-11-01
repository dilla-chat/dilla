package client

import (
	"context"
	"crypto/sha256"
	"net/http"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

// JoinRoleGrantReceipt is the stable receipt returned for one installation,
// member, and membership episode.
type JoinRoleGrantReceipt struct {
	RequestID, InstallationID, CommunityID, MemberID, EpisodeID, RoleID id.ID
}

// GrantJoinRole asks the server to apply the community's current join-role
// policy to this exact membership episode. A 204 means no policy is configured.
func (s *Session) GrantJoinRole(ctx context.Context, community, installation, member, episode id.ID) (JoinRoleGrantReceipt, bool, error) {
	if s == nil || s.c == nil || community.IsZero() || installation.IsZero() || member.IsZero() || episode.IsZero() {
		return JoinRoleGrantReceipt{}, false, ErrInvalidConfig
	}
	requestID := joinRoleGrantRequestID(installation, member, episode)
	body, err := cborx.Marshal([]any{requestID, member, episode})
	if err != nil {
		return JoinRoleGrantReceipt{}, false, ErrProtocol
	}
	raw, status, err := s.do(ctx, http.MethodPost, "communities/"+community.String()+"/bot/join-role-grants", nil, body, MaxSmallBody, true)
	if err != nil {
		return JoinRoleGrantReceipt{}, false, err
	}
	if status == http.StatusNoContent {
		if len(raw) != 0 {
			return JoinRoleGrantReceipt{}, false, ErrProtocol
		}
		return JoinRoleGrantReceipt{}, false, nil
	}
	var row []any
	if cborx.Unmarshal(raw, &row) != nil || len(row) != 6 {
		return JoinRoleGrantReceipt{}, false, ErrProtocol
	}
	ids := make([]id.ID, 6)
	for i := range row {
		value, ok := asID(row[i])
		if !ok {
			return JoinRoleGrantReceipt{}, false, ErrProtocol
		}
		ids[i] = value
	}
	receipt := JoinRoleGrantReceipt{RequestID: ids[0], InstallationID: ids[1], CommunityID: ids[2], MemberID: ids[3], EpisodeID: ids[4], RoleID: ids[5]}
	if receipt.RequestID != requestID || receipt.InstallationID != installation || receipt.CommunityID != community || receipt.MemberID != member || receipt.EpisodeID != episode || receipt.RoleID.IsZero() {
		return JoinRoleGrantReceipt{}, false, ErrProtocol
	}
	return receipt, true, nil
}

func joinRoleGrantRequestID(installation, member, episode id.ID) id.ID {
	h := sha256.Sum256(append(append(append([]byte("dilla.bot.join-role-grant.v1\x00"), installation[:]...), member[:]...), episode[:]...))
	var out id.ID
	copy(out[:], h[:16])
	if out.IsZero() {
		out[15] = 1
	}
	return out
}

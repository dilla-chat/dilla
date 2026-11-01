package client

import (
	"context"
	"errors"
	"net/http"

	"github.com/jonasthim/dilla/internal/id"
)

// BotActorChannelAuthority fetches a human actor's current channel permission
// bits in the bot session's bound community. A 404 means the actor or channel
// is no longer available to this bot.
func (s *Session) BotActorChannelAuthority(ctx context.Context, community, actor, channel id.ID) (uint64, bool, error) {
	if s == nil || s.c == nil {
		return 0, false, ErrInvalidConfig
	}
	cid, err := pathID(community)
	if err != nil {
		return 0, false, err
	}
	uid, err := pathID(actor)
	if err != nil {
		return 0, false, err
	}
	chid, err := pathID(channel)
	if err != nil {
		return 0, false, err
	}
	path := "communities/" + cid + "/bot/actors/" + uid + "/channels/" + chid + "/authority"
	raw, status, err := s.do(ctx, http.MethodGet, path, nil, nil, MaxSmallBody, false)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && status == http.StatusNotFound {
			return 0, false, nil
		}
		return 0, false, err
	}
	a, err := decode(raw, 4)
	if err != nil {
		return 0, false, err
	}
	gotCommunity, ok := asID(a[0])
	if !ok || gotCommunity != community {
		return 0, false, ErrProtocol
	}
	gotActor, ok := asID(a[1])
	if !ok || gotActor != actor {
		return 0, false, ErrProtocol
	}
	gotChannel, ok := asID(a[2])
	if !ok || gotChannel != channel {
		return 0, false, ErrProtocol
	}
	bits, ok := asUint(a[3])
	if !ok || bits&^knownCommunityPermissionBits != 0 {
		return 0, false, ErrProtocol
	}
	return bits, true, nil
}

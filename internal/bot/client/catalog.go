package client

import (
	"context"
	"net/http"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 9: the Bot publishes its per-community command catalog and its command-key
// publication. Both are Bot runtime routes, on HTTPS until Task 14.

// maxCatalogBody is dillad's body cap on the catalog route (64 KiB).
const maxCatalogBody = 64 << 10

// PublishCatalog sends the community's commands list, deterministic CBOR [[name, description,
// operation, args_shape, permission_bit, needs_bot_channel, handler_kind]…], and answers the
// revision dillad stored it under (unchanged when the list is).
func (s *Session) PublishCatalog(ctx context.Context, community id.ID, commands []byte) (uint64, error) {
	if s == nil || s.c == nil || s.token == "" || len(commands) == 0 || len(commands) > maxCatalogBody {
		return 0, ErrInvalidConfig
	}
	c, err := pathID(community)
	if err != nil {
		return 0, err
	}
	raw, _, err := s.do(ctx, http.MethodPut, "bot-runtime/communities/"+c+"/catalog", nil, commands, MaxSmallBody, true)
	if err != nil {
		return 0, err
	}
	var result []uint64
	if cborx.Unmarshal(raw, &result) != nil || len(result) != 1 || result[0] == 0 {
		return 0, ErrProtocol
	}
	return result[0], nil
}

// PublishCommandKey sends the community's signed command-key publication (dilla_core::bot::command
// CommandKeyPublication, as the helper's op 21 returns it). dillad stores it after checking the
// signer, the community and not_after.
func (s *Session) PublishCommandKey(ctx context.Context, community id.ID, publication []byte) error {
	if s == nil || s.c == nil || s.token == "" || len(publication) == 0 || len(publication) > 256 {
		return ErrInvalidConfig
	}
	c, err := pathID(community)
	if err != nil {
		return err
	}
	_, _, err = s.do(ctx, http.MethodPut, "bot-runtime/communities/"+c+"/command-key", nil, publication, MaxSmallBody, true)
	return err
}

package client

import (
	"context"
	"crypto/sha256"
	"net/http"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bot v2 Task 10: the Bot answers a relayed sealed command, and tells members their Bot state
// changed. Both are Bot runtime routes, on HTTPS until Task 14.

const (
	// maxSealedReply is dilla_core::bot::command::MAX_SEALED_REPLY_BYTES, dillad's bound.
	maxSealedReply = 65536
	// Bot state scopes: a user, a text channel, or the call in a voice channel (the scope id is the
	// voice channel).
	StateScopeUser    uint8 = 0
	StateScopeChannel uint8 = 1
	StateScopeCall    uint8 = 2
)

// Reply sends answer number seq of a relayed command, sealed to the request's reply key, to the
// device that sent it. final marks the last answer. dillad answers 404 when it relayed no such
// request from that device in the last five minutes.
func (s *Session) Reply(ctx context.Context, device, request id.ID, seq uint32, sealed []byte, final bool) error {
	if s == nil || s.c == nil || s.token == "" || device.IsZero() || request.IsZero() || len(sealed) == 0 || len(sealed) > maxSealedReply {
		return ErrInvalidConfig
	}
	var f uint64
	if final {
		f = 1
	}
	body, err := cborx.Marshal([]any{device[:], request[:], uint64(seq), sealed, f})
	if err != nil {
		return err
	}
	_, _, err = s.do(ctx, http.MethodPost, "bot-runtime/replies", nil, body, MaxSmallBody, true)
	return err
}

// maxSealedCommand is dilla_core::bot::command::MAX_SEALED_COMMAND_BYTES, dillad's bound.
const maxSealedCommand = 16384

// SendCommand relays one sealed command to community's Bot through dillad, POST /v1/communities/{community}/bot/commands
// `[request_id, sealed]` (202). The Bot sends only its own health probe this way (Task 17 fix round 2): dillad attaches
// the Bot's own session, pushes it back to the Bot as op 64, and the answer returns as op 62 through the reply route, the
// path a member's command takes. The idempotency key is the SHA-256 of the sealed bytes (F3).
func (s *Session) SendCommand(ctx context.Context, community, request id.ID, sealed []byte) error {
	if s == nil || s.c == nil || s.token == "" || community.IsZero() || request.IsZero() || len(sealed) == 0 || len(sealed) > maxSealedCommand {
		return ErrInvalidConfig
	}
	body, err := cborx.Marshal([]any{request[:], sealed})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(sealed)
	raw, _, err := s.do(WithIdempotencyKey(ctx, digest[:]), http.MethodPost, "communities/"+community.String()+"/bot/commands", nil, body, MaxSmallBody, true)
	if err != nil {
		return err
	}
	var accepted []uint64
	if cborx.Unmarshal(raw, &accepted) != nil || len(accepted) != 1 || accepted[0] != 0 {
		return ErrProtocol
	}
	return nil
}

// StateHint raises one scope's Bot state revision and has dillad tell that scope's audience. It
// carries no content; it answers the new revision.
func (s *Session) StateHint(ctx context.Context, kind uint8, scope id.ID) (uint64, error) {
	if s == nil || s.c == nil || s.token == "" || kind > StateScopeCall || scope.IsZero() {
		return 0, ErrInvalidConfig
	}
	body, err := cborx.Marshal([]any{uint64(kind), scope[:]})
	if err != nil {
		return 0, err
	}
	raw, _, err := s.do(ctx, http.MethodPost, "bot-runtime/state-hints", nil, body, MaxSmallBody, true)
	if err != nil {
		return 0, err
	}
	var result []uint64
	if cborx.Unmarshal(raw, &result) != nil || len(result) != 1 || result[0] == 0 {
		return 0, ErrProtocol
	}
	return result[0], nil
}

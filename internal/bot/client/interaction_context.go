package client

import (
	"context"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

// MaxInteractionContextResponse bounds the signed server snapshot and its CBOR
// envelope. Rust verifies the snapshot signature and contents; this client only
// checks the response shape and returns the exact canonical wire bytes.
const MaxInteractionContextResponse = (160 << 10) + 256

// BotInteractionContextSnapshot asks the bound installation for a signed copy
// of the admission context. The returned bytes are the exact canonical CBOR
// [payload, signature] response; this method does not claim to authenticate it.
func (s *Session) BotInteractionContextSnapshot(ctx context.Context, installation, requestID, group, participant, participantDevice id.ID) ([]byte, error) {
	if s == nil || s.c == nil || s.token == "" || requestID.IsZero() || group.IsZero() || participant.IsZero() || participantDevice.IsZero() {
		return nil, ErrInvalidConfig
	}
	inst, err := pathID(installation)
	if err != nil {
		return nil, err
	}
	request, err := encode([]any{requestID, group, participant, participantDevice})
	if err != nil {
		return nil, err
	}
	raw, err := s.interactionProofRequest(ctx,
		"bot-runtime/installations/"+inst+"/interaction-admissions/context",
		request, MaxInteractionContextResponse, false)
	if err != nil {
		return nil, err
	}
	var response []any
	if len(raw) > MaxInteractionContextResponse || cborx.Unmarshal(raw, &response) != nil || len(response) != 2 {
		return nil, ErrProtocol
	}
	payload, ok := asBytes(response[0], -1)
	if !ok || len(payload) == 0 || len(payload) > 160<<10 {
		return nil, ErrProtocol
	}
	if _, ok := asBytes(response[1], 64); !ok {
		return nil, ErrProtocol
	}
	return append([]byte(nil), raw...), nil
}

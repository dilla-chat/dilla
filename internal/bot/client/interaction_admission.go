package client

import (
	"context"
	"net/http"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const maxInteractionAdmissionResponse = 4096

// IssueBotInteractionAdmission obtains the installation-bound, server-signed
// admission receipt. The exact CBOR [payload, signature] is returned for native
// verification; this HTTP client does not treat its contents as authority.
func (s *Session) IssueBotInteractionAdmission(ctx context.Context, installation, requestID, group, participant, participantDevice id.ID) ([]byte, error) {
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
		"bot-runtime/installations/"+inst+"/interaction-admissions",
		request, maxInteractionAdmissionResponse, true)
	if err != nil {
		return nil, err
	}
	var response []any
	if len(raw) > maxInteractionAdmissionResponse || cborx.Unmarshal(raw, &response) != nil || len(response) != 2 {
		return nil, unknown()
	}
	if payload, ok := asBytes(response[0], -1); !ok || len(payload) == 0 || len(payload) > maxInteractionAdmissionResponse-80 {
		return nil, unknown()
	}
	if _, ok := asBytes(response[1], 64); !ok {
		return nil, unknown()
	}
	return append([]byte(nil), raw...), nil
}

// BotInteractionGroupAbsent asks for an absence proof bound to an active,
// installation-scoped admission receipt. A malformed boolean is a protocol
// error; callers must never infer absence from an HTTP refusal.
func (s *Session) BotInteractionGroupAbsent(ctx context.Context, installation, requestID id.ID) (bool, error) {
	if s == nil || s.c == nil || s.token == "" || requestID.IsZero() {
		return false, ErrInvalidConfig
	}
	inst, err := pathID(installation)
	if err != nil {
		return false, err
	}
	request, err := pathID(requestID)
	if err != nil {
		return false, err
	}
	raw, _, err := s.do(ctx, http.MethodGet,
		"bot-runtime/installations/"+inst+"/interaction-admissions/"+request+"/group-absence",
		nil, nil, 16, false)
	if err != nil {
		return false, err
	}
	if len(raw) != 1 || raw[0] != 0xf4 && raw[0] != 0xf5 {
		return false, ErrProtocol
	}
	return raw[0] == 0xf5, nil
}

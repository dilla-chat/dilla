package client

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	MaxModerationRemovalPage = 250
	MaxModerationRemovalBody = 64 << 10
)

// ModerationRemoval is an accepted immutable moderation action. Signature
// verification requires the registered device list and is left to the caller.
type ModerationRemoval struct {
	Revision   uint64
	Proof      []byte
	Signature  []byte
	AcceptedAt uint64
}

type ModerationRemovalPage struct {
	Rows         []ModerationRemoval
	NextRevision uint64
}

// ModerationRemovals fetches the ordered deletion feed for one group. Revisions
// are contiguous, and every proof is structurally valid and bound to group.
func (s *Session) ModerationRemovals(ctx context.Context, group id.ID, after, limit uint64) (ModerationRemovalPage, error) {
	if s == nil || s.c == nil || group.IsZero() || after > math.MaxInt64 || limit == 0 || limit > MaxModerationRemovalPage {
		return ModerationRemovalPage{}, ErrInvalidConfig
	}
	q := url.Values{"after": {strconv.FormatUint(after, 10)}, "limit": {strconv.FormatUint(limit, 10)}}
	path := "groups/" + group.String() + "/moderation/removals"
	raw, _, err := s.do(ctx, http.MethodGet, path, q, nil, MaxModerationRemovalBody, false)
	if err != nil {
		return ModerationRemovalPage{}, err
	}
	var tuple []any
	if cborx.Unmarshal(raw, &tuple) != nil || len(tuple) != 2 {
		return ModerationRemovalPage{}, ErrProtocol
	}
	rows, ok := tuple[0].([]any)
	if !ok || uint64(len(rows)) > limit {
		return ModerationRemovalPage{}, ErrProtocol
	}
	next, ok := asUint(tuple[1])
	if !ok || next > math.MaxInt64 {
		return ModerationRemovalPage{}, ErrProtocol
	}
	page := ModerationRemovalPage{Rows: make([]ModerationRemoval, 0, len(rows)), NextRevision: next}
	last := after
	for _, value := range rows {
		row, ok := value.([]any)
		if !ok || len(row) != 4 {
			return ModerationRemovalPage{}, ErrProtocol
		}
		revision, okRevision := asUint(row[0])
		proof, okProof := asBytes(row[1], -1)
		signature, okSignature := asBytes(row[2], 64)
		acceptedAt, okAcceptedAt := asUint(row[3])
		if !okRevision || last >= math.MaxInt64 || revision != last+1 ||
			!okProof || !okSignature || !okAcceptedAt || acceptedAt > math.MaxInt64 {
			return ModerationRemovalPage{}, ErrProtocol
		}
		decodedProof, err := protocol.DecodeRemovalProof(proof)
		if err != nil || decodedProof.GroupID != group {
			return ModerationRemovalPage{}, ErrProtocol
		}
		page.Rows = append(page.Rows, ModerationRemoval{
			Revision: revision, Proof: append([]byte(nil), proof...),
			Signature: append([]byte(nil), signature...), AcceptedAt: acceptedAt,
		})
		last = revision
	}
	if next != last {
		return ModerationRemovalPage{}, ErrProtocol
	}
	return page, nil
}

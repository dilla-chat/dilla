package client

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"

	botprotocol "github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	MaxStructuralEventPage = 100
	MaxStructuralEventBody = 512 << 10
)

// StructuralEventPage is one bounded page from the community installation event stream.
// Events contain structural metadata only; their data is validated by bot/protocol.
type StructuralEventPage struct {
	Events       []botprotocol.Event
	NextSequence uint64
	HighWater    uint64
	Floor        uint64
}

// EventHistoryGapError means the requested cursor predates retained history. Floor and
// HighWater describe the server's current stream bounds; callers must reconcile state
// explicitly before choosing a new cursor.
type EventHistoryGapError struct {
	Floor, HighWater uint64
}

func (e *EventHistoryGapError) Error() string { return "bot client: structural event history gap" }

func (s *Session) StructuralEvents(ctx context.Context, community id.ID, after, limit uint64) (StructuralEventPage, error) {
	if s == nil || s.c == nil || community.IsZero() || after > math.MaxInt64 || limit == 0 || limit > MaxStructuralEventPage {
		return StructuralEventPage{}, ErrInvalidConfig
	}
	q := url.Values{"after": {formatUint(after)}, "limit": {formatUint(limit)}}
	path := "communities/" + community.String() + "/bot/events"
	raw, _, err := s.do(ctx, http.MethodGet, path, q, nil, MaxStructuralEventBody, false)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusGone && apiErr.Code == "E_PRUNED" && apiErr.EventFloor != 0 {
			return StructuralEventPage{}, &EventHistoryGapError{Floor: apiErr.EventFloor, HighWater: apiErr.EventHighWater}
		}
		return StructuralEventPage{}, err
	}
	var tuple []any
	if cborx.Unmarshal(raw, &tuple) != nil || len(tuple) != 4 {
		return StructuralEventPage{}, ErrProtocol
	}
	rows, ok := tuple[0].([]any)
	if !ok || uint64(len(rows)) > limit {
		return StructuralEventPage{}, ErrProtocol
	}
	next, okNext := asUint(tuple[1])
	high, okHigh := asUint(tuple[2])
	floor, okFloor := asUint(tuple[3])
	if !okNext || !okHigh || !okFloor || next > math.MaxInt64 || high > math.MaxInt64 || floor > high || next < after || next > high {
		return StructuralEventPage{}, ErrProtocol
	}
	if after < floor {
		return StructuralEventPage{}, &EventHistoryGapError{Floor: floor, HighWater: high}
	}
	page := StructuralEventPage{Events: make([]botprotocol.Event, 0, len(rows)), NextSequence: next, HighWater: high, Floor: floor}
	last := after
	for _, row := range rows {
		encoded, marshalErr := cborx.Marshal(row)
		if marshalErr != nil {
			return StructuralEventPage{}, ErrProtocol
		}
		event, decodeErr := botprotocol.DecodeEvent(encoded)
		if decodeErr != nil || botprotocol.ValidateEventContext(event, community) != nil || last >= math.MaxInt64 || event.Sequence != last+1 || event.Sequence > next {
			return StructuralEventPage{}, ErrProtocol
		}
		last = event.Sequence
		page.Events = append(page.Events, event)
	}
	if len(page.Events) == 0 && next != after || len(page.Events) > 0 && page.Events[len(page.Events)-1].Sequence != next || uint64(len(page.Events)) < limit && next != high {
		return StructuralEventPage{}, ErrProtocol
	}
	return page, nil
}

func (s *Session) AcknowledgeStructuralEvents(ctx context.Context, community id.ID, through uint64) error {
	if s == nil || s.c == nil || community.IsZero() || through == 0 || through > math.MaxInt64 {
		return ErrInvalidConfig
	}
	body, err := cborx.Marshal([]any{through})
	if err != nil {
		return ErrProtocol
	}
	path := "communities/" + community.String() + "/bot/events/ack"
	_, _, err = s.do(ctx, http.MethodPut, path, nil, body, MaxSmallBody, true)
	return err
}

func formatUint(n uint64) string {
	// strconv avoids locale or formatting ambiguity in cursor query parameters.
	return strconv.FormatUint(n, 10)
}

package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	botprotocol "github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

const (
	structuralEventStateArea = 7
	structuralEventStateKey  = "dilla.bot.structural-events.v1"
	defaultStructuralPage    = 100
	defaultStructuralBudget  = 2
)

var ErrStructuralEventReconciliationRequired = errors.New("bot structural event history requires explicit reconciliation")
var ErrStructuralEventUnhandled = errors.New("bot structural event has no configured handler")

// FailClosedStructuralEvent is the production fallback until a concrete adapter
// is configured for an event kind. Returning an error keeps the durable cursor
// and server acknowledgement before that event, so later events cannot be skipped.
func FailClosedStructuralEvent(_ context.Context, event botprotocol.Event) error {
	return fmt.Errorf("%w: kind %d sequence %d", ErrStructuralEventUnhandled, event.Kind, event.Sequence)
}

type structuralEventNative interface {
	Binding() native.Binding
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

type structuralEventSession interface {
	StructuralEvents(context.Context, id.ID, uint64, uint64) (client.StructuralEventPage, error)
	AcknowledgeStructuralEvents(context.Context, id.ID, uint64) error
}

type StructuralEventHandler func(context.Context, botprotocol.Event) error

type StructuralEventPollerOptions struct {
	PageLimit, PageBudget uint64
}

type StructuralEventPoller struct {
	mu         sync.Mutex
	native     structuralEventNative
	binding    native.Binding
	pageLimit  uint64
	pageBudget uint64
}

type StructuralEventPollResult struct {
	Processed, Pages, AcknowledgedThrough uint64
	More                                  bool
}

type StructuralEventGapError struct {
	Floor, HighWater uint64
}

func (e *StructuralEventGapError) Error() string {
	return fmt.Sprintf("%v (floor %d, high-water %d)", ErrStructuralEventReconciliationRequired, e.Floor, e.HighWater)
}
func (e *StructuralEventGapError) Unwrap() error { return ErrStructuralEventReconciliationRequired }

func NewStructuralEventPoller(n *native.Client, options StructuralEventPollerOptions) (*StructuralEventPoller, error) {
	if n == nil {
		return nil, errors.New("missing native client")
	}
	return newStructuralEventPoller(n, options)
}

func newStructuralEventPoller(n structuralEventNative, options StructuralEventPollerOptions) (*StructuralEventPoller, error) {
	if n == nil {
		return nil, errors.New("missing native client")
	}
	binding := n.Binding()
	if binding.Instance.IsZero() || binding.Community.IsZero() || binding.Installation.IsZero() {
		return nil, errors.New("invalid structural event binding")
	}
	if options.PageLimit == 0 {
		options.PageLimit = defaultStructuralPage
	}
	if options.PageBudget == 0 {
		options.PageBudget = defaultStructuralBudget
	}
	if options.PageLimit > defaultStructuralPage || options.PageBudget > 2 {
		return nil, errors.New("structural event page budget exceeds bound")
	}
	return &StructuralEventPoller{native: n, binding: binding, pageLimit: options.PageLimit, pageBudget: options.PageBudget}, nil
}

// PollOnce performs a bounded event catch-up pass. Call it from the installation's
// gateway wakeup path and a periodic fallback. Handler calls receive stable event IDs
// so a crash before local progress is saved can be retried idempotently.
func (p *StructuralEventPoller) PollOnce(ctx context.Context, session *client.Session, handler StructuralEventHandler) (StructuralEventPollResult, error) {
	if session == nil {
		return StructuralEventPollResult{}, errors.New("missing device session")
	}
	return p.pollOnce(ctx, session, handler)
}

// CompleteGapReconciliation advances the structural cursor only after the
// caller has durably staged registrations for the current member set. It first
// verifies that the fixed high-water remains readable; a newer retention gap
// must be reconciled before advancing.
func (p *StructuralEventPoller) CompleteGapReconciliation(ctx context.Context, session *client.Session, highWater uint64) error {
	if p == nil || session == nil || highWater > math.MaxInt64 {
		return errors.New("invalid structural event gap completion")
	}
	return p.completeGapReconciliation(ctx, session, highWater)
}

func (p *StructuralEventPoller) completeGapReconciliation(ctx context.Context, session structuralEventSession, highWater uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	cursor, revision, err := p.loadCursor(ctx)
	if err != nil {
		return err
	}
	if cursor.Sequence > highWater {
		return errors.New("structural gap high-water precedes durable cursor")
	}
	page, err := session.StructuralEvents(ctx, p.binding.Community, highWater, 1)
	if err != nil {
		var gap *client.EventHistoryGapError
		if errors.As(err, &gap) {
			return &StructuralEventGapError{Floor: gap.Floor, HighWater: gap.HighWater}
		}
		return err
	}
	if err = validateStructuralPage(page, p.binding.Community, highWater, 1); err != nil {
		return err
	}
	if cursor.Sequence < highWater {
		cursor = structuralEventCursor{Sequence: highWater}
		_, err = p.saveCursor(ctx, revision, cursor)
		if err != nil {
			return fmt.Errorf("persist structural reconciliation cursor: %w", err)
		}
	}
	if err = session.AcknowledgeStructuralEvents(ctx, p.binding.Community, highWater); err != nil {
		return err
	}
	return nil
}

func (p *StructuralEventPoller) pollOnce(ctx context.Context, session structuralEventSession, handler StructuralEventHandler) (StructuralEventPollResult, error) {
	var result StructuralEventPollResult
	if p == nil || session == nil || handler == nil {
		return result, errors.New("invalid structural event poll")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	cursor, revision, err := p.loadCursor(ctx)
	if err != nil {
		return result, err
	}
	after := cursor.Sequence
	var lastHighWater uint64
	for pageNo := uint64(0); pageNo < p.pageBudget; pageNo++ {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		page, pageErr := session.StructuralEvents(ctx, p.binding.Community, after, p.pageLimit)
		if pageErr != nil {
			var gap *client.EventHistoryGapError
			if errors.As(pageErr, &gap) {
				return result, &StructuralEventGapError{Floor: gap.Floor, HighWater: gap.HighWater}
			}
			return result, pageErr
		}
		if err = validateStructuralPage(page, p.binding.Community, after, p.pageLimit); err != nil {
			return result, err
		}
		result.Pages++
		lastHighWater = page.HighWater
		// The read proves that this persisted cursor is still usable. Retry an ack
		// whose earlier response may have been lost, but only after the read has
		// had a chance to surface a retention gap.
		if pageNo == 0 && cursor.Sequence > 0 {
			if err = session.AcknowledgeStructuralEvents(ctx, p.binding.Community, cursor.Sequence); err != nil {
				return result, err
			}
			result.AcknowledgedThrough = cursor.Sequence
		}
		for _, event := range page.Events {
			if err = ctx.Err(); err != nil {
				return result, err
			}
			if err = handler(ctx, event); err != nil {
				return result, err
			}
			cursor = structuralEventCursor{Sequence: event.Sequence, EventID: event.ID}
			revision, err = p.saveCursor(ctx, revision, cursor)
			if err != nil {
				return result, fmt.Errorf("persist structural event progress: %w", err)
			}
			if err = session.AcknowledgeStructuralEvents(ctx, p.binding.Community, cursor.Sequence); err != nil {
				return result, err
			}
			after = cursor.Sequence
			result.Processed++
			result.AcknowledgedThrough = cursor.Sequence
		}
		if page.NextSequence > cursor.Sequence {
			cursor.Sequence = page.NextSequence
			revision, err = p.saveCursor(ctx, revision, cursor)
			if err != nil {
				return result, fmt.Errorf("persist structural event scan cursor: %w", err)
			}
			if err = session.AcknowledgeStructuralEvents(ctx, p.binding.Community, cursor.Sequence); err != nil {
				return result, err
			}
			result.AcknowledgedThrough = cursor.Sequence
			after = cursor.Sequence
		}
		if after >= page.HighWater || page.NextSequence == after || uint64(len(page.Events)) < p.pageLimit {
			break
		}
	}
	result.More = after < lastHighWater
	return result, nil
}

type structuralEventCursor struct {
	Sequence uint64
	EventID  id.ID
}

func validateStructuralPage(page client.StructuralEventPage, community id.ID, after, limit uint64) error {
	if page.NextSequence > math.MaxInt64 || page.HighWater > math.MaxInt64 || page.Floor > page.HighWater || page.NextSequence < after || page.NextSequence > page.HighWater || uint64(len(page.Events)) > limit {
		return client.ErrProtocol
	}
	if after < page.Floor {
		return client.ErrProtocol
	}
	last := after
	for _, event := range page.Events {
		if last >= math.MaxInt64 || event.Sequence != last+1 || event.Sequence > page.NextSequence || botprotocol.ValidateEventContext(event, community) != nil {
			return client.ErrProtocol
		}
		if _, err := botprotocol.EncodeEvent(event); err != nil {
			return client.ErrProtocol
		}
		last = event.Sequence
	}
	if len(page.Events) == 0 && page.NextSequence != after || len(page.Events) > 0 && page.Events[len(page.Events)-1].Sequence != page.NextSequence || uint64(len(page.Events)) < limit && page.NextSequence != page.HighWater {
		return client.ErrProtocol
	}
	return nil
}

func (p *StructuralEventPoller) loadCursor(ctx context.Context) (structuralEventCursor, uint64, error) {
	value, err := p.native.StateGet(ctx, structuralEventStateArea, []byte(structuralEventStateKey))
	if err != nil {
		return structuralEventCursor{}, 0, err
	}
	if value.Tombstone {
		return structuralEventCursor{}, 0, errors.New("structural event cursor tombstoned")
	}
	if value.Revision == 0 {
		return structuralEventCursor{}, 0, nil
	}
	cursor, err := decodeStructuralEventCursor(value.Value)
	return cursor, value.Revision, err
}

func (p *StructuralEventPoller) saveCursor(ctx context.Context, revision uint64, cursor structuralEventCursor) (uint64, error) {
	value, err := cborx.Marshal([]any{uint64(1), cursor.Sequence, cursor.EventID})
	if err != nil {
		return revision, err
	}
	seed := append([]byte("dilla.bot.structural-events.state.tx.v1\x00"), value...)
	hash := sha256.Sum256(seed)
	tx := id.ID(hash[:16])
	if tx.IsZero() {
		return revision, errors.New("invalid structural event transaction id")
	}
	revisions, err := p.native.StateTransaction(ctx, tx, []native.StateChange{{Area: structuralEventStateArea, Key: []byte(structuralEventStateKey), ExpectedRevision: revision, Value: value}})
	if err != nil {
		return revision, err
	}
	if len(revisions) != 1 || revisions[0] != revision+1 {
		return revision, errors.New("invalid structural event state revision")
	}
	return revisions[0], nil
}

func decodeStructuralEventCursor(raw []byte) (structuralEventCursor, error) {
	var a []any
	if cborx.Unmarshal(raw, &a) != nil || len(a) != 3 || a[0] != uint64(1) {
		return structuralEventCursor{}, errors.New("invalid structural event cursor")
	}
	sequence, ok := a[1].(uint64)
	bytes, idOK := a[2].([]byte)
	if !ok || sequence > math.MaxInt64 || !idOK || len(bytes) != 16 {
		return structuralEventCursor{}, errors.New("invalid structural event cursor")
	}
	var eventID id.ID
	copy(eventID[:], bytes)
	if sequence == 0 && !eventID.IsZero() {
		return structuralEventCursor{}, errors.New("invalid structural event cursor")
	}
	return structuralEventCursor{Sequence: sequence, EventID: eventID}, nil
}

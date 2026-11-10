package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// TextEventMaxPendingAge bounds how long one event may fail at the head of the shared queue. After
// this long since its first failed handling attempt, the poller acknowledges it as undeliverable,
// whatever the cause, so a cause no handler classifies cannot stall the queue indefinitely. The
// first-failure time and the count of failed passes are durable in the helper's state store; a skip
// also needs at least groupEventMinFailedPasses failed passes.
const TextEventMaxPendingAge = 24 * time.Hour

const (
	groupEventFailureArea = uint64(8)
	groupEventFailureKey  = "dilla.bot.group-event-failure.v1/"
	// Version 3 added hold fields (HeldFirst, HeldAttempts), unused since Bot v2 Task 23 removed the
	// sender trust holds; the record keeps its shape. An older record is replaced by a fresh one,
	// which only restarts an age, never shortens it.
	groupEventFailureVersion = uint64(3)
	// groupEventMinFailedPasses failed passes are required besides the age limit, so a forward
	// clock jump alone can never skip an event.
	groupEventMinFailedPasses = 3
	// groupEventClockTolerance is how far in the future a stored first-failure time may be before
	// the clock is taken to have gone backwards and the record restarts at now.
	groupEventClockTolerance = 5 * time.Minute
)

// groupEventFailureState is the helper state store; *native.Client implements it. Without it the
// age limit cannot be proven and failing events simply stay pending.
type groupEventFailureState interface {
	StateGet(context.Context, uint64, []byte) (native.StateValue, error)
	StateTransaction(context.Context, id.ID, []native.StateChange) ([]uint64, error)
}

type groupEventNative interface {
	Binding() native.Binding
	ReadGroupEvents(context.Context, uint64, uint64) (native.GroupEventPage, error)
	AcknowledgeGroupEvents(context.Context, uint64) error
}

// GroupEventHandler processes one authenticated, decrypted native event.
// It must make externally visible effects idempotent by event identity because
// a lost acknowledgement causes the event to be delivered again.
type GroupEventHandler interface {
	HandleGroupEvent(context.Context, native.GroupEvent) error
}

var ErrGroupEventHandler = errors.New("group event handler failed")

// ErrGroupEventPollBusy leaves the shared cumulative cursor owned by its current drain.
var ErrGroupEventPollBusy = errors.New("group event polling is already active")

// GroupEventHandlerError keeps the failed delivery and its cause. It still matches
// ErrGroupEventHandler, so the event stays pending exactly as before.
type GroupEventHandlerError struct {
	DeliveryID uint64
	Err        error
}

func (e *GroupEventHandlerError) Error() string {
	return fmt.Sprintf("%s at delivery %d: %v", ErrGroupEventHandler.Error(), e.DeliveryID, e.Err)
}
func (e *GroupEventHandlerError) Unwrap() error        { return e.Err }
func (e *GroupEventHandlerError) Is(target error) bool { return target == ErrGroupEventHandler }

// UndeliverableGroupEvent is implemented by a handler error that proves the event can never be
// handled (for example, its group is gone or outside interaction scope, or its saved read was
// definitively denied). The poller acknowledges such an event instead of retrying it forever,
// because the queue is shared by every group and user: an unacknowledged head blocks all later
// events durably, across restarts.
//
// Only errors that are proven permanent may implement it. Acknowledging an event is
// irreversible: an undeliverable error for a transient failure (network, server 5xx, busy,
// rate limit, cancellation, a missing row that may still arrive) would drop a valid command.
type UndeliverableGroupEvent interface {
	error
	Undeliverable() bool
}

func isUndeliverableGroupEvent(err error) bool {
	var u UndeliverableGroupEvent
	return errors.As(err, &u) && u.Undeliverable()
}

// SkippedGroupEvent records one undeliverable event the poller acknowledged, for the caller's log.
// FirstFailure is set when the event was skipped by its age limit; Cause is then the
// last handler error.
type SkippedGroupEvent struct {
	DeliveryID   uint64
	FirstFailure time.Time
	Cause        error
}

// ErrGroupEventPendingTooLong marks an event acknowledged because it stayed pending past its age
// limit (InteractionEventMaxPendingAge or TextEventMaxPendingAge).
var ErrGroupEventPendingTooLong = errors.New("group event stayed pending past the age limit")

type GroupEventPollResult struct {
	Handled             uint64
	Skipped             []SkippedGroupEvent // undeliverable events acknowledged without handling
	AcknowledgedThrough uint64
	More                bool
	RateLimited         *client.APIError // pending work deferred without a failed handling pass
}

// GroupEventPoller drains one bounded page of the helper's durable event queue.
// The native helper, rather than this process, persists the acknowledgement.
type GroupEventPoller struct {
	active  atomic.Bool
	native  groupEventNative
	binding native.Binding
	now     func() time.Time
}

func NewGroupEventPoller(n groupEventNative) (*GroupEventPoller, error) {
	if n == nil {
		return nil, errors.New("missing group event source")
	}
	return newGroupEventPoller(n)
}

func newGroupEventPoller(n groupEventNative) (*GroupEventPoller, error) {
	if n == nil {
		return nil, errors.New("missing group event source")
	}
	b := n.Binding()
	if b.Instance.IsZero() || b.Community.IsZero() || b.Installation.IsZero() {
		return nil, errors.New("invalid group event binding")
	}
	return &GroupEventPoller{native: n, binding: b, now: time.Now}, nil
}

func (p *GroupEventPoller) PollOnce(ctx context.Context, handler GroupEventHandler) (GroupEventPollResult, error) {
	var result GroupEventPollResult
	if p == nil || handler == nil || ctx == nil {
		return result, errors.New("missing group event poll dependency")
	}
	if !p.active.CompareAndSwap(false, true) {
		return GroupEventPollResult{More: true}, ErrGroupEventPollBusy
	}
	defer p.active.Store(false)
	if err := client.CooperativeCheckpoint(ctx); err != nil {
		return result, err
	}
	page, err := p.native.ReadGroupEvents(ctx, 0, 100)
	if err != nil {
		return result, err
	}
	if len(page.Events) > 100 || page.Next > page.HighWater {
		return result, errors.New("invalid group event page")
	}
	for _, event := range page.Events {
		if err := client.CooperativeCheckpoint(ctx); err != nil {
			return result, err
		}
		if event.Community != p.binding.Community || event.DeliveryID <= result.AcknowledgedThrough || event.DeliveryID > page.Next {
			return result, errors.New("invalid group event binding or order")
		}
		// Once dispatch begins, finish this event and its durable acknowledgement.
		// A command hint only stops the turn before the next event.
		eventCtx := client.WithoutCooperativeStop(ctx)
		var skipped *SkippedGroupEvent
		handlingErr := handler.HandleGroupEvent(eventCtx, event)
		if errors.Is(handlingErr, client.ErrCooperativeStop) {
			result.More = true
			return result, &GroupEventHandlerError{DeliveryID: event.DeliveryID, Err: handlingErr}
		}
		if refusal := client.RateLimitError(handlingErr); refusal != nil {
			// A middleware refusal did not execute the operation. Leave the queue and
			// its durable failure/hold history untouched, even at the age limit.
			result.More = true
			result.RateLimited = refusal
			return result, nil // The refusal is reported as pending work in result.RateLimited, not a failed pass.
		}
		if err := handlingErr; err != nil {
			if ctx.Err() != nil {
				// Never skip, and never start an event's age, on a cancelled pass.
				return result, &GroupEventHandlerError{DeliveryID: event.DeliveryID, Err: err}
			}
			if isUndeliverableGroupEvent(err) {
				skipped = &SkippedGroupEvent{DeliveryID: event.DeliveryID, Cause: err}
			} else {
				first, aged, ageErr := p.failureAge(eventCtx, event)
				if ageErr != nil || !aged || ctx.Err() != nil {
					return result, &GroupEventHandlerError{DeliveryID: event.DeliveryID, Err: errors.Join(err, ageErr)}
				}
				skipped = &SkippedGroupEvent{DeliveryID: event.DeliveryID, FirstFailure: first,
					Cause: fmt.Errorf("%w: %w", ErrGroupEventPendingTooLong, err)}
			}
		}
		if err := p.native.AcknowledgeGroupEvents(eventCtx, event.DeliveryID); err != nil {
			return result, err
		}
		if skipped == nil {
			result.Handled++
		} else {
			result.Skipped = append(result.Skipped, *skipped)
		}
		result.AcknowledgedThrough = event.DeliveryID
	}
	if page.Next > result.AcknowledgedThrough {
		if err := p.native.AcknowledgeGroupEvents(ctx, page.Next); err != nil {
			return result, err
		}
		result.AcknowledgedThrough = page.Next
	}
	result.More = page.Next < page.HighWater
	return result, nil
}

// groupEventFailureRecord is the durable failure history of the current head event. Only the
// head can fail, so one record per installation suffices; it is bound to the event's delivery
// ID, group and source sequence, so a reused delivery ID never inherits an older event's age.
type groupEventFailureRecord struct {
	_        struct{} `cbor:",toarray"`
	Version  uint64
	Delivery uint64
	Group    id.ID
	Sequence uint64
	First    uint64 // unix seconds of the first failed pass
	Attempts uint64 // failed passes, at least 1
	// HeldFirst and HeldAttempts are always zero (the sender trust holds are gone, Bot v2 Task 23).
	HeldFirst    uint64
	HeldAttempts uint64
}

func (r groupEventFailureRecord) matches(event native.GroupEvent) bool {
	return r.Delivery == event.DeliveryID && r.Group == event.Group && r.Sequence == event.SourceSequence
}

// failureAge records one failed handling pass of event durably and reports whether the event may
// now be skipped: it needs BOTH its age limit (maxPendingAge) since the first failure AND at least
// groupEventMinFailedPasses failed passes, so a forward clock jump alone never skips an event,
// while real downtime still counts towards the age. A first-failure time more than
// groupEventClockTolerance in the future (the clock went backwards) restarts the record at now.
// A record that does not decode, or has an unknown version, is replaced by a fresh one. Every
// write carries a revision check; any state error keeps the event pending.
func (p *GroupEventPoller) failureAge(ctx context.Context, event native.GroupEvent) (time.Time, bool, error) {
	state, ok := p.native.(groupEventFailureState)
	if !ok {
		return time.Time{}, false, nil
	}
	now := p.now()
	nowUnix, ok := checkedUint64(now.Unix())
	if !ok || nowUnix == 0 || event.DeliveryID > math.MaxInt64 || event.SourceSequence > math.MaxInt64 {
		return time.Time{}, false, errors.New("invalid group event failure clock or event")
	}
	key := []byte(groupEventFailureKey + p.binding.Installation.String())
	current, err := state.StateGet(ctx, groupEventFailureArea, key)
	if err != nil {
		return time.Time{}, false, err
	}
	next := groupEventFailureRecord{Version: groupEventFailureVersion, Delivery: event.DeliveryID, Group: event.Group, Sequence: event.SourceSequence, First: nowUnix, Attempts: 1}
	if !current.Tombstone && len(current.Value) > 0 {
		var stored groupEventFailureRecord
		if cborx.Unmarshal(current.Value, &stored) == nil && stored.Version == groupEventFailureVersion &&
			stored.First > 0 && stored.Attempts > 0 && stored.Attempts < math.MaxInt64 && stored.matches(event) {
			if storedFirst, inRange := checkedInt64(stored.First); inRange &&
				time.Unix(storedFirst, 0).Before(now.Add(groupEventClockTolerance+time.Second)) {
				next.First, next.Attempts = stored.First, stored.Attempts+1
			}
		}
	}
	value, err := cborx.Marshal(next)
	if err != nil {
		return time.Time{}, false, err
	}
	if _, err := state.StateTransaction(ctx, id.New(), []native.StateChange{{Area: groupEventFailureArea, Key: key, ExpectedRevision: current.Revision, Value: value}}); err != nil {
		return time.Time{}, false, err
	}
	firstUnix, ok := checkedInt64(next.First)
	if !ok {
		return time.Time{}, false, errors.New("invalid group event first-failure time")
	}
	first := time.Unix(firstUnix, 0)
	return first, next.Attempts >= groupEventMinFailedPasses && now.Sub(first) >= TextEventMaxPendingAge, nil
}

var _ groupEventFailureState = (*native.Client)(nil)

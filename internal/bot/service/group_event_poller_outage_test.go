package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

// durableGroupEventQueue models the native helper's event journal: the ack cursor
// survives a process restart, and every read starts after it (ReadGroupEvents(0, 100)).
type durableGroupEventQueue struct {
	binding  native.Binding
	events   []native.GroupEvent
	acked    uint64
	ackFails int // the next ackFails acknowledgements fail
	state    map[string]native.StateValue
	stateErr error
	// interaction lists the groups the native descriptors prove are private interaction groups.
	interaction    []id.ID
	interactionErr error
	// conflicts makes the next state writes fail as if another writer had won the revision.
	conflicts int
}

func (q *durableGroupEventQueue) InteractionGroups(context.Context) ([]native.InteractionGroupDescriptor, error) {
	if q.interactionErr != nil {
		return nil, q.interactionErr
	}
	out := make([]native.InteractionGroupDescriptor, 0, len(q.interaction))
	for _, g := range q.interaction {
		out = append(out, native.InteractionGroupDescriptor{Group: g, State: 2})
	}
	return out, nil
}

// The helper state store, durable like the ack cursor.
func (q *durableGroupEventQueue) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	if q.stateErr != nil {
		return native.StateValue{}, q.stateErr
	}
	return q.state[fmt.Sprintf("%d/%s", area, key)], nil
}
func (q *durableGroupEventQueue) StateTransaction(_ context.Context, _ id.ID, changes []native.StateChange) ([]uint64, error) {
	if q.stateErr != nil {
		return nil, q.stateErr
	}
	if q.state == nil {
		q.state = map[string]native.StateValue{}
	}
	if q.conflicts > 0 {
		q.conflicts--
		return nil, errors.New("state revision conflict")
	}
	revs := make([]uint64, 0, len(changes))
	for _, ch := range changes {
		k := fmt.Sprintf("%d/%s", ch.Area, ch.Key)
		if q.state[k].Revision != ch.ExpectedRevision {
			return nil, errors.New("state revision conflict")
		}
		// The helper accepts only deterministic CBOR arrays (native.validStateValue).
		if cborx.ExpectMajor(ch.Value, cborx.MajorArray) != nil || cborx.CheckDeterministic(ch.Value) != nil {
			return nil, errors.New("state value is not a deterministic CBOR array")
		}
		q.state[k] = native.StateValue{Revision: ch.ExpectedRevision + 1, Value: append([]byte(nil), ch.Value...)}
		revs = append(revs, ch.ExpectedRevision+1)
	}
	return revs, nil
}

func (q *durableGroupEventQueue) Binding() native.Binding { return q.binding }
func (q *durableGroupEventQueue) ReadGroupEvents(_ context.Context, _, limit uint64) (native.GroupEventPage, error) {
	page := native.GroupEventPage{Next: q.acked, HighWater: q.acked}
	for _, e := range q.events {
		if e.DeliveryID > page.HighWater {
			page.HighWater = e.DeliveryID
		}
		if e.DeliveryID > q.acked && uint64(len(page.Events)) < limit {
			page.Events = append(page.Events, e)
			page.Next = e.DeliveryID
		}
	}
	return page, nil
}
func (q *durableGroupEventQueue) AcknowledgeGroupEvents(_ context.Context, through uint64) error {
	if q.ackFails > 0 {
		q.ackFails--
		return errors.New("helper acknowledgement unavailable")
	}
	if through > q.acked {
		q.acked = through
	}
	return nil
}

// goneGroupError is what a handler returns for an event whose group can never be answered
// again (the group left the native store). It is declared here, not imported, so this test
// compiles against the code before and after the fix.
type goneGroupError struct{}

func (goneGroupError) Error() string       { return "native interaction group is no longer active" }
func (goneGroupError) Undeliverable() bool { return true }

// Production outage (CT 144, 2026-10-25..): one event whose handler can never succeed sits at
// the head of the durable queue. Every later command, for every user, waits behind it, across
// reconciliation passes and process restarts, and the log only ever says "group event handler
// failed".
func TestGroupEventPollerUndeliverableHeadDoesNotBlockLaterEventsAcrossRestarts(t *testing.T) {
	community := id.New()
	q := &durableGroupEventQueue{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()},
		events: []native.GroupEvent{{DeliveryID: 1, Community: community, Group: id.New()}, {DeliveryID: 2, Community: community, Group: id.New()}}}
	var handled []uint64
	handler := groupEventHandlerFunc(func(_ context.Context, event native.GroupEvent) error {
		if event.DeliveryID == 1 {
			return goneGroupError{}
		}
		handled = append(handled, event.DeliveryID)
		return nil
	})
	var lastErr error
	for pass := 0; pass < 23; pass++ { // 23 logged "dispatch paused" passes, one restart in between
		p, err := newGroupEventPoller(q) // a fresh poller per pass, as reconcile() builds one
		if err != nil {
			t.Fatal(err)
		}
		var result GroupEventPollResult
		result, lastErr = p.PollOnce(context.Background(), handler)
		if pass == 0 && lastErr == nil {
			// The skip carries what the operator's log line needs: the delivery and its cause.
			var gone goneGroupError
			if len(result.Skipped) != 1 || result.Skipped[0].DeliveryID != 1 || !errors.As(result.Skipped[0].Cause, &gone) || result.Handled != 1 {
				t.Fatalf("skip record = %+v handled=%d; want delivery 1 with its cause", result.Skipped, result.Handled)
			}
		}
	}
	if len(handled) != 1 || handled[0] != 2 || q.acked != 2 {
		t.Fatalf("later event never dispatched behind an undeliverable head: handled=%v acked=%d lastErr=%v", handled, q.acked, lastErr)
	}
}

func TestGroupEventPollerKeepsTheHandlerCauseAndDeliveryID(t *testing.T) {
	community := id.New()
	q := &durableGroupEventQueue{binding: native.Binding{Instance: id.New(), Community: community, Installation: id.New()},
		events: []native.GroupEvent{{DeliveryID: 7, Community: community, Group: id.New()}}}
	cause := errors.New("read permission revoked")
	p, _ := newGroupEventPoller(q)
	_, err := p.PollOnce(context.Background(), groupEventHandlerFunc(func(context.Context, native.GroupEvent) error { return cause }))
	var handlerErr *GroupEventHandlerError
	if !errors.Is(err, ErrGroupEventHandler) || !errors.Is(err, cause) || !errors.As(err, &handlerErr) || handlerErr.DeliveryID != 7 || q.acked != 0 {
		t.Fatalf("PollOnce error = %v; the cause and delivery must survive and the event must stay pending", err)
	}
}

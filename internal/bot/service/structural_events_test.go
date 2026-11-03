package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	botprotocol "github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

type structuralEventState struct {
	binding native.Binding
	value   native.StateValue
	order   []string
}

func (n *structuralEventState) Binding() native.Binding { return n.binding }
func (n *structuralEventState) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	if area != structuralEventStateArea || string(key) != structuralEventStateKey {
		return native.StateValue{}, fmt.Errorf("unexpected state key")
	}
	return n.value, nil
}
func (n *structuralEventState) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if tx.IsZero() || len(changes) != 1 || changes[0].ExpectedRevision != n.value.Revision {
		return nil, fmt.Errorf("unexpected state transaction")
	}
	n.order = append(n.order, "persist")
	n.value = native.StateValue{Revision: n.value.Revision + 1, Value: append([]byte(nil), changes[0].Value...)}
	return []uint64{n.value.Revision}, nil
}

type fakeStructuralEventSession struct {
	page     client.StructuralEventPage
	err      error
	acks     []uint64
	order    *[]string
	requests []uint64
}

func (s *fakeStructuralEventSession) StructuralEvents(_ context.Context, _ id.ID, after, limit uint64) (client.StructuralEventPage, error) {
	s.requests = append(s.requests, after, limit)
	return s.page, s.err
}
func (s *fakeStructuralEventSession) AcknowledgeStructuralEvents(_ context.Context, _ id.ID, through uint64) error {
	if s.order != nil {
		*s.order = append(*s.order, "ack")
	}
	s.acks = append(s.acks, through)
	return nil
}

func TestStructuralEventPollerPersistsBeforeAckAndUsesStableEventIDs(t *testing.T) {
	community := testID(1)
	event := botprotocol.Event{ID: testID(2), CommunityID: community, Sequence: 1, Timestamp: 6, Kind: botprotocol.EventMemberJoin, Data: []any{testID(3), testID(4)}}
	n := &structuralEventState{binding: native.Binding{Instance: testID(7), Community: community, Installation: testID(8)}}
	s := &fakeStructuralEventSession{page: client.StructuralEventPage{Events: []botprotocol.Event{event}, NextSequence: 1, HighWater: 1, Floor: 0}, order: &n.order}
	p, err := newStructuralEventPoller(n, StructuralEventPollerOptions{PageLimit: 10, PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	var applied []id.ID
	result, err := p.pollOnce(context.Background(), s, func(_ context.Context, got botprotocol.Event) error {
		applied = append(applied, got.ID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0] != event.ID || result.AcknowledgedThrough != 1 {
		t.Fatalf("applied=%v result=%+v", applied, result)
	}
	if fmt.Sprint(n.order) != "[persist ack]" {
		t.Fatalf("order = %v", n.order)
	}
	if fmt.Sprint(s.acks) != "[1]" {
		t.Fatalf("acks = %v", s.acks)
	}
	var saved []any
	if err := cborx.Unmarshal(n.value.Value, &saved); err != nil || len(saved) != 3 || saved[0] != uint64(1) || saved[1] != uint64(1) {
		t.Fatalf("saved cursor = %#v, err=%v", saved, err)
	}
}

func TestStructuralEventGapRequiresReconciliationAndDoesNotAckOrApply(t *testing.T) {
	community := testID(1)
	n := &structuralEventState{binding: native.Binding{Instance: testID(7), Community: community, Installation: testID(8)}}
	s := &fakeStructuralEventSession{err: &client.EventHistoryGapError{Floor: 11, HighWater: 15}}
	p, err := newStructuralEventPoller(n, StructuralEventPollerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = p.pollOnce(context.Background(), s, func(context.Context, botprotocol.Event) error { called = true; return nil })
	var gap *StructuralEventGapError
	if !errors.As(err, &gap) || gap.Floor != 11 || gap.HighWater != 15 {
		t.Fatalf("error = %v", err)
	}
	if called || len(s.acks) != 0 || len(n.order) != 0 {
		t.Fatalf("handler=%t acks=%v state=%v", called, s.acks, n.order)
	}
}

func TestStructuralGapCompletionPersistsCursorBeforeAckAndVerifiesSnapshot(t *testing.T) {
	community := testID(1)
	n := &structuralEventState{binding: native.Binding{Instance: testID(7), Community: community, Installation: testID(8)}}
	event := botprotocol.Event{ID: testID(30), CommunityID: community, Sequence: 16, Timestamp: 1, Kind: botprotocol.EventMemberJoin, Data: []any{testID(3), testID(4)}}
	s := &fakeStructuralEventSession{page: client.StructuralEventPage{Events: []botprotocol.Event{event}, NextSequence: 16, HighWater: 18, Floor: 11}, order: &n.order}
	p, err := newStructuralEventPoller(n, StructuralEventPollerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.completeGapReconciliation(context.Background(), s, 15); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(n.order) != "[persist ack]" || fmt.Sprint(s.acks) != "[15]" || fmt.Sprint(s.requests) != "[15 1]" {
		t.Fatalf("order=%v acks=%v requests=%v", n.order, s.acks, s.requests)
	}
	cursor, _, err := p.loadCursor(context.Background())
	if err != nil || cursor.Sequence != 15 {
		t.Fatalf("cursor=%+v err=%v", cursor, err)
	}
}

func TestFailClosedStructuralEventLeavesCursorAndAckBeforeUnhandledEvent(t *testing.T) {
	community := testID(1)
	event := botprotocol.Event{ID: testID(2), CommunityID: community, Sequence: 1, Timestamp: 6, Kind: botprotocol.EventMemberJoin, Data: []any{testID(3), testID(4)}}
	n := &structuralEventState{binding: native.Binding{Instance: testID(7), Community: community, Installation: testID(8)}}
	s := &fakeStructuralEventSession{page: client.StructuralEventPage{Events: []botprotocol.Event{event}, NextSequence: 1, HighWater: 1}}
	p, err := newStructuralEventPoller(n, StructuralEventPollerOptions{PageLimit: 10, PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.pollOnce(context.Background(), s, FailClosedStructuralEvent)
	if !errors.Is(err, ErrStructuralEventUnhandled) {
		t.Fatalf("error = %v, want unhandled event", err)
	}
	if n.value.Revision != 0 || len(s.acks) != 0 {
		t.Fatalf("cursor revision=%d acknowledgements=%v", n.value.Revision, s.acks)
	}
}

func TestStructuralEventPollerRetriesAckFromDurableCursorBeforeReading(t *testing.T) {
	community := testID(1)
	n := &structuralEventState{binding: native.Binding{Instance: testID(7), Community: community, Installation: testID(8)}}
	n.value = native.StateValue{Revision: 1, Value: cbor(t, []any{uint64(1), uint64(4), testID(9)})}
	s := &fakeStructuralEventSession{page: client.StructuralEventPage{NextSequence: 4, HighWater: 4, Floor: 1}, order: &n.order}
	p, err := newStructuralEventPoller(n, StructuralEventPollerOptions{PageLimit: 10, PageBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.pollOnce(context.Background(), s, func(context.Context, botprotocol.Event) error { t.Fatal("unexpected event"); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(s.acks) != 1 || s.acks[0] != 4 || fmt.Sprint(s.requests) != "[4 10]" || fmt.Sprint(n.order) != "[ack]" {
		t.Fatalf("acks=%v requests=%v order=%v", s.acks, s.requests, n.order)
	}
}

func TestValidateStructuralPageAcceptsFreshZeroFloor(t *testing.T) {
	if err := validateStructuralPage(client.StructuralEventPage{NextSequence: 0, HighWater: 0, Floor: 0}, testID(1), 0, 10); err != nil {
		t.Fatalf("fresh page rejected: %v", err)
	}
}

func TestValidateStructuralPageRejectsSequenceGapAndMissingTail(t *testing.T) {
	community := testID(1)
	makeEvent := func(seq uint64) botprotocol.Event {
		return botprotocol.Event{ID: testID(byte(seq + 1)), CommunityID: community, Sequence: seq, Timestamp: 4, Kind: botprotocol.EventMemberJoin, Data: []any{testID(3), testID(4)}}
	}
	for _, tc := range []struct {
		name string
		page client.StructuralEventPage
	}{
		{"gap-in-page", client.StructuralEventPage{Events: []botprotocol.Event{makeEvent(3)}, NextSequence: 3, HighWater: 3, Floor: 1}},
		{"missing-tail", client.StructuralEventPage{Events: []botprotocol.Event{makeEvent(2)}, NextSequence: 2, HighWater: 3, Floor: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateStructuralPage(tc.page, community, 1, 10); !errors.Is(err, client.ErrProtocol) {
				t.Fatalf("error=%v, want ErrProtocol", err)
			}
		})
	}
}

func testID(n byte) id.ID { var v id.ID; v[15] = n; return v }
func cbor(t *testing.T, v any) []byte {
	t.Helper()
	b, err := cborx.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

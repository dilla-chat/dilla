package counter

import (
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/sframe"
)

type memoryState struct {
	values          map[string]native.StateValue
	failAfterCommit bool
}

func (m *memoryState) StateGet(_ context.Context, area uint64, key []byte) (native.StateValue, error) {
	return m.values[string(append([]byte{byte(area)}, key...))], nil
}

func (m *memoryState) StateTransaction(_ context.Context, tx id.ID, changes []native.StateChange) ([]uint64, error) {
	if tx.IsZero() || len(changes) != 1 {
		return nil, errors.New("invalid transaction")
	}
	c := changes[0]
	k := string(append([]byte{byte(c.Area)}, c.Key...))
	prior := m.values[k]
	if prior.Revision != c.ExpectedRevision {
		return nil, errors.New("revision conflict")
	}
	if m.values == nil {
		m.values = map[string]native.StateValue{}
	}
	m.values[k] = native.StateValue{Revision: prior.Revision + 1, Value: append([]byte(nil), c.Value...)}
	if m.failAfterCommit {
		m.failAfterCommit = false
		return nil, errors.New("lost response")
	}
	return []uint64{prior.Revision + 1}, nil
}

func TestReserveNeverReusesCounterRangeAcrossRestartOrLostResponse(t *testing.T) {
	ctx := context.Background()
	store := &memoryState{}
	group := id.New()
	first, err := Reserve(ctx, store, group, 5, 3)
	if err != nil || first != (Range{Start: 0, End: BlockSize}) {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := Reserve(ctx, store, group, 5, 3) // new service instance, same encrypted state
	if err != nil || second != (Range{Start: BlockSize, End: 2 * BlockSize}) {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	store.failAfterCommit = true
	if _, err := Reserve(ctx, store, group, 5, 3); err == nil {
		t.Fatal("lost response was reported as safe to publish")
	}
	afterCrash, err := Reserve(ctx, store, group, 5, 3)
	if err != nil || afterCrash != (Range{Start: 3 * BlockSize, End: 4 * BlockSize}) {
		t.Fatalf("after crash=%+v err=%v", afterCrash, err)
	}
	otherEpoch, err := Reserve(ctx, store, group, 6, 3)
	if err != nil || otherEpoch.Start != 0 {
		t.Fatalf("new epoch=%+v err=%v", otherEpoch, err)
	}
	otherGroup, err := Reserve(ctx, store, id.New(), 5, 3)
	if err != nil || otherGroup.Start != 0 {
		t.Fatalf("new group=%+v err=%v", otherGroup, err)
	}
}

func TestReserveFailsClosedOnMalformedOrExhaustedState(t *testing.T) {
	ctx := context.Background()
	group := id.New()
	store := &memoryState{values: map[string]native.StateValue{}}
	key := string(append([]byte{byte(stateArea)}, stateKey(group, 5, 3)...))
	store.values[key] = native.StateValue{Revision: 1, Value: []byte{0xff}}
	if _, err := Reserve(ctx, store, group, 5, 3); err == nil {
		t.Fatal("accepted malformed counter cursor")
	}
	value, err := cborx.Marshal([]any{uint64(1), sframe.MaxSeq + 1 - BlockSize + 1})
	if err != nil {
		t.Fatal(err)
	}
	store.values[key] = native.StateValue{Revision: 1, Value: value}
	if _, err := Reserve(ctx, store, group, 5, 3); err == nil {
		t.Fatal("reserved beyond MaxSeq")
	}
}

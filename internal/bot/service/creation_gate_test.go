package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/bot/polls"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/bot/reminders"
	"github.com/jonasthim/dilla/internal/cborx"
)

// Task 10c (Jonas, Q4 b): switching Reminders off refuses only creation. Reminders that exist can
// still be listed and cancelled; a gate error is not hidden as a refusal.
func TestRemindersOffRefusesOnlyCreation(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	h, _, _ := newReminderTest(&now)
	owner := reminderTestID(4)
	existing, err := h.Store.Create(ctx, reminderTestID(3), owner, [32]byte{1}, reminders.Reminder{Text: "x", Timezone: "UTC"}, time.Unix(4600, 0))
	if err != nil {
		t.Fatal(err)
	}
	enabled, gateErr, gateCalls := false, error(nil), 0
	h.CreateEnabled = func(context.Context) (bool, error) { gateCalls++; return enabled, gateErr }
	payload, _ := cborx.Marshal([]any{uint64(1), uint64(0), uint64(60), "call", "UTC", nil, nil})
	rev, expires := uint64(0), uint64(1100)
	create := protocol.Request{RequestID: reminderTestID(8), CommunityID: reminderTestID(1), Operation: protocol.OpReminderCreate, Revision: &rev, Expires: &expires, Args: []any{payload}}
	actor := InteractionActor{User: owner, Device: reminderTestID(5)}
	if result, err := h.create(ctx, actor, create); err != nil || result.Status != interactionStatusUnavailable {
		t.Fatalf("create with Reminders off = %+v, %v; want unavailable", result, err)
	}
	gateErr = errors.New("installation read failed")
	if _, err := h.create(ctx, actor, create); err == nil {
		t.Fatal("a failed capability read was reported as a plain refusal")
	}
	gateErr = nil
	list := protocol.Request{CommunityID: reminderTestID(1), Operation: protocol.OpReminderList, Args: []any{nil}}
	if result, err := h.list(ctx, actor, list); err != nil || result.Status != 0 {
		t.Fatalf("list with Reminders off = %+v, %v; want the member's reminders", result, err)
	}
	cancelRev := uint64(1)
	cancel := protocol.Request{RequestID: reminderTestID(9), CommunityID: reminderTestID(1), Operation: protocol.OpReminderCancel, Revision: &cancelRev, Expires: &expires, Args: []any{existing.ID}}
	if result, err := h.cancel(ctx, actor, cancel); err != nil || result.Status != 0 {
		t.Fatalf("cancel with Reminders off = %+v, %v; want cancelled", result, err)
	}
	enabled = true
	if result, err := h.create(ctx, actor, create); err != nil || result.Status != 0 {
		t.Fatalf("create with Reminders on = %+v, %v", result, err)
	}
	if gateCalls != 3 {
		t.Fatalf("capability read %d times, want once per create", gateCalls)
	}
}

// Switching Polls off refuses creating a poll before anything is stored or published.
func TestPollsOffRefusesCreation(t *testing.T) {
	state := &pollTestState{}
	store := polls.NewStore(state, pollServiceID(1))
	pub := &pollPublisherFake{}
	now := time.Unix(1000, 0)
	enabled := false
	h := PollInteractions{Store: store, Authority: &pollAuthorityFake{bits: 2, member: true}, CommunityID: pollServiceID(1),
		Now: func() time.Time { return now }, Publisher: pub, CreateEnabled: func(context.Context) (bool, error) { return enabled, nil }}
	ch := pollServiceID(3)
	payload, err := cborx.Marshal([]any{uint64(1), ch[:], nil, "Question", []any{"A", "B"}, uint64(0), uint64(3600)})
	if err != nil {
		t.Fatal(err)
	}
	rev, expires := uint64(0), uint64(1100)
	reqID := pollServiceID(30)
	r := protocol.Request{RequestID: reqID, CommunityID: pollServiceID(1), Operation: protocol.OpPollCreate, Revision: &rev, Expires: &expires, Args: []any{payload}}
	actor := InteractionActor{User: pollServiceID(2), Kind: 0}
	if result, err := h.create(context.Background(), actor, r); err != nil || result.Status != interactionStatusUnavailable {
		t.Fatalf("create with Polls off = %+v, %v", result, err)
	}
	if pub.calls != 0 {
		t.Fatal("a refused poll was published")
	}
	if _, err := store.Get(context.Background(), reqID); err == nil {
		t.Fatal("a refused poll was stored")
	}
	enabled = true
	if result, err := h.create(context.Background(), actor, r); err != nil || result.Status != 0 || pub.calls != 1 {
		t.Fatalf("create with Polls on = %+v, %v, publishes %d", result, err, pub.calls)
	}
}

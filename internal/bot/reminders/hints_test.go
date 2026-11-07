package reminders

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/id"
)

type hintLog struct {
	mu   sync.Mutex
	kind []uint8
	user []id.ID
}

func (h *hintLog) Hint(_ context.Context, kind uint8, scope id.ID) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kind = append(h.kind, kind)
	h.user = append(h.user, scope)
	return nil
}

// Bot v2 Task 17: a reminder that comes due tells its owner's clients with a user hint (the app then reads the due
// reminders with a sealed read). The Bot delivers nothing itself: there is no message and no op-84 push.
func TestReminderDueSendsUserHintNotDelivery(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	s := NewStore(&durableState{}, native.Binding{Community: tid(1), Installation: tid(2)}, func() time.Time { return now })
	hints := &hintLog{}
	s.Hints = hints
	owner, other := tid(4), tid(5)
	for i, who := range []id.ID{owner, owner, other} {
		if _, err := s.Create(ctx, tid(byte(10+i)), who, [32]byte{byte(i + 1)}, Reminder{Text: "stretch", Timezone: "UTC"}, time.Unix(1100, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Tick(ctx); err != nil || len(hints.user) != 0 {
		t.Fatalf("a tick before anything is due sent hints %v (err %v)", hints.user, err)
	}
	now = time.Unix(1100, 0)
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// One hint per owner, however many of their reminders came due, and always the user scope.
	if len(hints.user) != 2 || hints.user[0] != owner || hints.user[1] != other {
		t.Fatalf("hints = %v, want one user hint each for the two owners (in key order)", hints.user)
	}
	for _, kind := range hints.kind {
		if kind != client.StateScopeUser {
			t.Fatalf("hint kind %d, want the user scope", kind)
		}
	}
	// A reminder that is already due sends nothing again.
	if err := s.Tick(ctx); err != nil || len(hints.user) != 2 {
		t.Fatalf("second tick sent more hints: %v (err %v)", hints.user, err)
	}
}

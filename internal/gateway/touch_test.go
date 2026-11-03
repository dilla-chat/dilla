package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
)

// touchingStore is the harness's store double plus the one optional method the gateway calls when
// a connection reaches `ready`: TouchDevice, which records the device's last_seen.
type touchingStore struct {
	*harness
	mu      sync.Mutex
	touched map[id.ID]int64
}

func (s *touchingStore) TouchDevice(_ context.Context, device id.ID, lastSeen int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched[device] = lastSeen
	return nil
}

// protocol/01 § Cadence removes "a device that has not connected for 90 days", and the delivery
// service's sweep reads `devices.last_seen` for it (as does invariant 10's cursor eligibility). A
// connection that reaches `ready` IS the device connecting, so ready records it: without this
// last_seen stays at the device's creation and every device looks gone 90 days after it enrolled.
func TestReadyRecordsTheDeviceAsSeen(t *testing.T) {
	h := newHarness(t)
	store := &touchingStore{harness: h, touched: map[id.ID]int64{}}
	gw := New(Options{Clock: h.clk, Generation: 1, Store: store, Auth: h})
	t.Cleanup(func() { _ = gw.Shutdown(context.Background()) })

	h.clk.Advance(91 * 24 * time.Hour)
	device := id.New()
	if _, err := gw.register(context.Background(), session(device), newRecordingSink(64, false)); err != nil {
		t.Fatalf("register: %v", err)
	}
	store.mu.Lock()
	seen, ok := store.touched[device]
	store.mu.Unlock()
	if !ok {
		t.Fatal("ready did not record the device as seen")
	}
	if want := h.clk.Now().Unix(); seen != want {
		t.Fatalf("last_seen = %d, want the connection's own time %d", seen, want)
	}
}

// A store that cannot record it (the gateway's narrow Store interface does not require the
// method) still gets a working connection: the touch is best effort, never a refusal.
func TestReadyWithoutATouchingStoreStillConnects(t *testing.T) {
	h := newHarness(t)
	if c := h.connect(t, id.New()); c == nil {
		t.Fatal("no connection")
	}
}

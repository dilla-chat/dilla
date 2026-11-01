package livekit

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Task 10c: LeaveIdle leaves a retained room only while no track plays; the next start connects a
// fresh room, and a closed pool stays closed.
func TestPoolLeaveIdleDisconnectsOnlyAnIdleRoom(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{}
	p.backend = backend
	epochs := newPoolEpochs()
	call, group := poolCall(7, 8)
	if err := p.LeaveIdle(); err != nil {
		t.Fatalf("LeaveIdle on an empty pool = %v", err)
	}
	s, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.LeaveIdle(); err != nil {
		t.Fatal(err)
	}
	if _, disconnects, _ := backend.counts(); disconnects != 0 {
		t.Fatal("LeaveIdle disconnected a room with a playing track")
	}
	_ = s.Close()
	if err := p.LeaveIdle(); err != nil {
		t.Fatal(err)
	}
	if connects, disconnects, _ := backend.counts(); connects != 1 || disconnects != 1 {
		t.Fatalf("idle leave: connects=%d disconnects=%d, want 1 and 1", connects, disconnects)
	}
	s, err = p.startSource(context.Background(), call, group, epochs, newPoolSource())
	if err != nil {
		t.Fatalf("start after an idle leave = %v, want a fresh room", err)
	}
	if connects, _, _ := backend.counts(); connects != 2 {
		t.Fatalf("connects=%d after an idle leave, want a second room", connects)
	}
	_ = s.Close()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.LeaveIdle(); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("LeaveIdle after Close = %v, want ErrPoolClosed", err)
	}
}

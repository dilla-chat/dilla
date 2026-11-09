package livekit

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Bot v2 Task 19: a cold /play connects the room while its track resolves; the start then publishes into that room.
func TestWarmConnectsTheRoomAndTheStartPublishesIntoIt(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	backend := &poolBackendStub{}
	p.backend = backend
	call, group := poolCall(31, 32)
	if err := p.Warm(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if connects, _, _ := backend.counts(); connects != 1 || backend.publishes != 0 {
		t.Fatalf("warm: connects=%d publishes=%d, want a connected room and nothing published", connects, backend.publishes)
	}
	// A second warm while the room is connected changes nothing.
	if err := p.Warm(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	session, err := p.startSource(context.Background(), call, group, newPoolEpochs(), newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if connects, _, _ := backend.counts(); connects != 1 {
		t.Fatalf("the start connected again (%d rooms), want the warm room", connects)
	}
	// A warm while a track plays leaves the playing room alone.
	if err := p.Warm(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if connects, disconnects, _ := backend.counts(); connects != 1 || disconnects != 0 {
		t.Fatalf("warm during playback: connects=%d disconnects=%d", connects, disconnects)
	}
}

func TestWarmReplacesADeadOrEarlierCallRoom(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	backend := &poolBackendStub{}
	p.backend = backend
	first, _ := poolCall(41, 42)
	if err := p.Warm(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	// The call ended and a new one began: a new call group for the same channel.
	next, group := poolCall(41, 43)
	if err := p.Warm(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if connects, disconnects, _ := backend.counts(); connects != 2 || disconnects != 1 {
		t.Fatalf("rebind: connects=%d disconnects=%d, want the old room left and a new one connected", connects, disconnects)
	}
	session, err := p.startSource(context.Background(), next, group, newPoolEpochs(), newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Warm(context.Background(), next); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("warm on a closed pool = %v", err)
	}
}

// Fix round 1 (I2): a call kept without its token publishes only into the room already connected for it; it never
// connects a room (after an idle leave, a reset or a call end, only a member's play, with a fresh token, does).
func TestATokenlessCallNeverConnectsARoom(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	backend := &poolBackendStub{}
	p.backend = backend
	call, group := poolCall(61, 62)
	kept := call
	kept.Token = ""
	if _, err := p.startSource(context.Background(), kept, group, newPoolEpochs(), newPoolSource()); !errors.Is(err, ErrBinding) {
		t.Fatalf("a tokenless call on an empty pool = %v, want ErrBinding", err)
	}
	if connects, _, _ := backend.counts(); connects != 0 {
		t.Fatal("a tokenless call connected a room")
	}
	if err := p.Warm(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	session, err := p.startSource(context.Background(), kept, group, newPoolEpochs(), newPoolSource())
	if err != nil {
		t.Fatalf("a tokenless call into its connected room = %v", err)
	}
	_ = session.Close()
	if !p.Holds(kept) || p.Dead() {
		t.Fatal("the room is no longer held after the track")
	}
}

package livekit

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Fix round 1 (I4): an idle leave while a Start is publishing leaves the room alone; the track
// that start publishes keeps a live room.
func TestPoolLeaveIdleSkipsAStartThatIsPublishing(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	backend := &poolBackendStub{publishGate: make(chan struct{}), publishEntered: make(chan struct{}, 1)}
	p.backend = backend
	epochs := newPoolEpochs()
	call, group := poolCall(7, 8)
	type result struct {
		s   *Session
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
		done <- result{s, err}
	}()
	<-backend.publishEntered
	if err := p.LeaveIdle(); err != nil {
		t.Fatalf("LeaveIdle during a start = %v", err)
	}
	close(backend.publishGate)
	got := <-done
	if got.err != nil {
		t.Fatalf("start after a concurrent idle leave = %v", got.err)
	}
	if _, disconnects, _ := backend.counts(); disconnects != 0 {
		t.Fatalf("an idle leave during a start disconnected the room (%d)", disconnects)
	}
	_ = got.s.Close()
	_ = p.Close()
}

// Fix round 1 (I4): interleaving LeaveIdle with startSource never yields a started track on a dead
// room: either the start wins (the pool stays alive with that track) or the leave wins first and the
// start connects a fresh room. Run with -race.
func TestPoolLeaveIdleInterleavedWithStartNeverKillsAStartedTrack(t *testing.T) {
	for i := 0; i < 200; i++ {
		p, err := NewPool(time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		p.backend = &poolBackendStub{}
		epochs := newPoolEpochs()
		call, group := poolCall(7, 8)
		first, err := p.startSource(context.Background(), call, group, epochs, newPoolSource())
		if err != nil {
			t.Fatal(err)
		}
		_ = first.Close() // the room is retained and idle
		var wg sync.WaitGroup
		var started *Session
		var startErr error
		wg.Add(2)
		go func() { defer wg.Done(); _ = p.LeaveIdle() }()
		go func() {
			defer wg.Done()
			started, startErr = p.startSource(context.Background(), call, group, epochs, newPoolSource())
		}()
		wg.Wait()
		if startErr == nil {
			p.mu.Lock()
			dead, active := p.dead, p.active
			p.mu.Unlock()
			if dead || active != started {
				t.Fatalf("iteration %d: a started track sits on a dead room (dead=%v, active is started=%v)", i, dead, active == started)
			}
			_ = started.Close()
		}
		_ = p.Close()
	}
}

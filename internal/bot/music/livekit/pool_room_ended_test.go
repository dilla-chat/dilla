package livekit

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	lkproto "github.com/livekit/protocol/livekit"
)

func TestPoolRemoteDisconnectRetainsRegistrationAndIgnoresOldGeneration(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	backend := &poolBackendStub{}
	p.backend = backend
	var ended atomic.Int32
	p.SetRoomEndedReporter(func() {
		// A callback may inspect/change pool configuration; it must run outside p.mu.
		p.SetRTTReporter(nil)
		ended.Add(1)
	})
	call, group := poolCall(93, 94)
	first, err := p.startSource(context.Background(), call, group, newPoolEpochs(), newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	backend.disconnectRoom(0)
	backend.disconnectRoom(0)
	if got := ended.Load(); got != 0 {
		t.Fatalf("idle remote room-end reports=%d want 0", got)
	}
	if err := p.Reset(); err != nil {
		t.Fatal(err)
	}
	replacement, err := p.startSource(context.Background(), call, group, newPoolEpochs(), newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	backend.disconnectRoom(0)
	if got := ended.Load(); got != 0 {
		t.Fatalf("old generation cleared replacement: reports=%d", got)
	}
	backend.disconnectRoom(1)
	if got := ended.Load(); got != 0 {
		t.Fatalf("replacement room-end reports=%d want 0", got)
	}
}

func TestPoolReportsIdleLeaveOnce(t *testing.T) {
	p, err := NewPool(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	backend := &poolBackendStub{}
	p.backend = backend
	var ended atomic.Int32
	p.SetRoomEndedReporter(func() { p.SetRTTReporter(nil); ended.Add(1) })
	call, group := poolCall(95, 96)
	session, err := p.startSource(context.Background(), call, group, newPoolEpochs(), newPoolSource())
	if err != nil {
		t.Fatal(err)
	}
	_ = session.Close()
	if err := p.LeaveIdle(); err != nil {
		t.Fatal(err)
	}
	backend.disconnectRoom(0)
	if got := ended.Load(); got != 1 {
		t.Fatalf("idle leave reports=%d want 1", got)
	}
}

type reasonPoolRoom struct{ reason lkproto.DisconnectReason }

func (r reasonPoolRoom) Disconnect()                                {}
func (r reasonPoolRoom) DisconnectReason() lkproto.DisconnectReason { return r.reason }

func TestPoolReportsOnlyDefinitiveServerCallEnd(t *testing.T) {
	for _, tc := range []struct {
		reason lkproto.DisconnectReason
		ended  bool
	}{
		{lkproto.DisconnectReason_ROOM_DELETED, true},
		{lkproto.DisconnectReason_ROOM_CLOSED, true},
		{lkproto.DisconnectReason_SERVER_SHUTDOWN, false},
		{lkproto.DisconnectReason_MEDIA_FAILURE, false},
		{lkproto.DisconnectReason_SIGNAL_CLOSE, false},
		{lkproto.DisconnectReason_UNKNOWN_REASON, false},
	} {
		t.Run(tc.reason.String(), func(t *testing.T) {
			p, err := NewPool(time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			p.connection = &poolConnection{room: reasonPoolRoom{reason: tc.reason}}
			p.generation = 1
			var reports atomic.Int32
			p.SetRoomEndedReporter(func() { p.SetRTTReporter(nil); reports.Add(1) })
			p.markDead(1, ErrPoolDead)
			p.markDead(1, ErrPoolDead)
			want := int32(0)
			if tc.ended {
				want = 1
			}
			if reports.Load() != want || errors.Is(p.deadReason, ErrPoolCallEnded) != tc.ended {
				t.Fatalf("reason=%s reports=%d want=%d cause=%v", tc.reason, reports.Load(), want, p.deadReason)
			}
		})
	}
}

package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/id"
)

// A client that has read `ready` is online. The internal/ds election tests assert exactly that
// straight after reading the frame, and failed on the slower go-postgres runner ("a device that
// reached ready is not online"): registerVersions queued `ready` on a RUNNING writer and published
// the connection to the live registry only afterwards, so the frame could reach the wire first.
//
// The probe runs inside register, at every Clock.Now() call: whenever a frame has already been
// written it checks the online predicate at that very moment. Before the fix the writer wrote
// `ready` while register was still between its enqueue and addLive, and the probe saw the device
// offline; with the paused writer nothing reaches the sink until the connection is live.
func TestReadyNeverReachesTheWireBeforeTheDeviceIsOnline(t *testing.T) {
	h := newHarness(t)
	device := id.New()
	session := auth.Session{DeviceID: device, UserID: id.New(), Scope: auth.ScopeEnrolled}
	sink := newRecordingSink(16, false)

	var probes, offline atomic.Int64
	var probe func()
	probe = func() {
		probes.Add(1)
		select {
		case b := <-sink.frames:
			if !h.gw.Online(device) {
				offline.Add(1)
			}
			sink.frames <- b // leave it for the reader below
			return
		case <-time.After(50 * time.Millisecond):
		}
		h.clk.armNth(1, probe)
	}
	h.clk.armNth(1, probe)
	c, err := h.gw.register(context.Background(), session, sink)
	h.clk.disarm()
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	h.mu.Lock()
	h.sinks[c] = sink
	h.mu.Unlock()

	if probes.Load() == 0 {
		t.Fatal("the probe never ran inside register")
	}
	if offline.Load() != 0 {
		t.Fatal("`ready` reached the wire while the device was not yet online")
	}
	select {
	case <-sink.frames:
	case <-time.After(5 * time.Second):
		t.Fatal("`ready` never reached the wire")
	}
	if !h.gw.Online(device) {
		t.Fatal("a device whose `ready` is on the wire is not online")
	}
}

// The inbound frame bucket (facts-gateway-design §6.3; gateway.frame_burst and frame_burst_max):
// a client that sends more than FrameBurst frames without the clock moving is told
// E_RATE_LIMITED and closed 4008, which is resumable.
func TestAClientPastTheInboundFrameBurstIsClosedFourZeroZeroEight(t *testing.T) {
	h := newHarness(t)
	s := newRecordingSink(512, false)
	go h.gw.serve(context.Background(), s, "bearer")
	h.readFrom(t, s) // hello
	p, err := payload("bearer", uint64(1), uint64(1), uint64(1), uint64(0))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	s.feed(mustEncode(t, Frame{Op: OpIdentify, Payload: p, Replay: true}, 1))
	if in := h.readFrom(t, s); in.Op != OpReady {
		t.Fatalf("op %d, want ready", in.Op)
	}

	burst := h.gw.opts.FrameBurst
	beat, err := payload(uint64(0), uint64(1))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	for range burst + 1 {
		s.feed(mustEncode(t, Frame{Op: OpHeartbeat, Payload: beat, Replay: true}, 1))
	}
	if code := h.waitCloseOn(t, s); code != CloseRateLimited {
		t.Fatalf("close code = %d, want 4008", code)
	}
	if !CloseRateLimited.Resumable() {
		t.Fatal("4008 must be resumable")
	}
}

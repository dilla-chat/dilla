package gateway

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/id"
)

func statsInbound(t *testing.T, call id.ID, rtt uint32) Inbound {
	t.Helper()
	p, err := payload(call, CallStatsReport{RTTms: rtt})
	if err != nil {
		t.Fatal(err)
	}
	in, err := Decode(mustEncode(t, Frame{Op: OpCallStats, Payload: p}, 0), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestTelemetryPoolBoundsAndCoalescesWithoutBlockingHeartbeat(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{5}
	a := &activityAuth{capabilityRecordingAuth: &capabilityRecordingAuth{Authenticator: h}}
	h.gw.opts.Auth = a
	c := h.connect(t, id.New())
	h.drainReady(t, c)
	a.session = auth.Session{DeviceID: c.deviceID, UserID: c.userID, Scope: auth.ScopeEnrolled}
	entered := make(chan uint32, 8)
	release := make(chan struct{})
	var running atomic.Int32
	h.gw.WithCallStats(func(ctx context.Context, _ auth.Session, _ id.ID, report CallStatsReport) error {
		n := running.Add(1)
		defer running.Add(-1)
		if n > 8 {
			t.Error("more than eight workers")
		}
		entered <- report.RTTms
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	h.gw.handle(context.Background(), c, statsInbound(t, id.New(), 1))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	for i := uint32(2); i <= 20; i++ {
		h.gw.handle(context.Background(), c, statsInbound(t, id.New(), i))
	}
	if got := h.gw.TelemetryStats(); got.InFlight != 1 || got.Devices != 1 || got.Coalesced != 18 {
		t.Fatalf("slots=%+v", got)
	}
	// Even a stalled callback cannot prevent the same socket's heartbeat ACK.
	p, _ := payload(uint64(1), uint64(0))
	in, _ := Decode(mustEncode(t, Frame{Op: OpHeartbeat, Payload: p}, 0), 1<<20)
	h.gw.handle(context.Background(), c, in)
	if frame := h.waitFrame(t, c.deviceID); frame.Op != OpHeartbeatAck {
		t.Fatalf("heartbeat blocked: %+v", frame)
	}
	close(release)
	select {
	case latest := <-entered:
		if latest != 20 {
			t.Fatalf("latest=%d", latest)
		}
	case <-time.After(time.Second):
		t.Fatal("latest not processed")
	}
}

type telemetryAuth struct {
	*capabilityRecordingAuth
	h *harness
}

func (a *telemetryAuth) ResolveHash(ctx context.Context, hash []byte) (auth.Session, error) {
	if err := ctx.Err(); err != nil {
		return auth.Session{}, err
	}
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	for _, s := range a.h.tokens {
		if bytes.Equal(s.TokenHash, hash) {
			return s, nil
		}
	}
	return auth.Session{}, errNotEnrolledSession
}

func TestTelemetryPoolSaturationCancellationAndCleanup(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{5}
	h.gw.opts.Auth = &telemetryAuth{capabilityRecordingAuth: &capabilityRecordingAuth{Authenticator: h}, h: h}
	entered := make(chan id.ID, telemetryDeviceCap)
	release := make(chan struct{})
	h.gw.WithCallStats(func(_ context.Context, s auth.Session, _ id.ID, _ CallStatsReport) error {
		entered <- s.DeviceID
		<-release
		return nil
	})
	conns := make([]*conn, telemetryDeviceCap)
	for i := range conns {
		c := h.connect(t, id.New())
		h.drainReady(t, c)
		conns[i] = c
		h.gw.handle(context.Background(), c, statsInbound(t, id.New(), uint32(i)))
		if i < telemetryWorkers {
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("fixed worker did not start")
			}
		}
	}
	extra := h.connect(t, id.New())
	h.drainReady(t, extra)
	h.gw.handle(context.Background(), extra, statsInbound(t, id.New(), 1))
	snapshot := h.gw.TelemetryStats()
	if snapshot.Devices != 128 || snapshot.InFlight != 8 || snapshot.Dropped != 1 {
		t.Fatalf("bounds=%+v", snapshot)
	}
	// Cancel a queued socket and revoke another without closing it, as an external admin can.
	h.gw.removeLive(conns[100])
	h.mu.Lock()
	delete(h.tokens, h.tokenFor(conns[101].deviceID))
	h.mu.Unlock()
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for h.gw.TelemetryStats().Devices != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := h.gw.TelemetryStats(); got.Devices != 0 || got.InFlight != 0 {
		t.Fatalf("leaked slots=%+v", got)
	}
	close(entered)
	for device := range entered {
		if device == conns[100].deviceID || device == conns[101].deviceID || device == extra.deviceID {
			t.Fatal("cancelled/revoked/overflow sample reached acceptance")
		}
	}
}

func TestTelemetryDisconnectCancelsRunningWork(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{5}
	h.gw.opts.Auth = &telemetryAuth{capabilityRecordingAuth: &capabilityRecordingAuth{Authenticator: h}, h: h}
	c := h.connect(t, id.New())
	h.drainReady(t, c)
	entered, finished := make(chan struct{}), make(chan struct{})
	h.gw.WithCallStats(func(ctx context.Context, _ auth.Session, _ id.ID, _ CallStatsReport) error {
		close(entered)
		<-ctx.Done()
		close(finished)
		return ctx.Err()
	})
	h.gw.handle(context.Background(), c, statsInbound(t, id.New(), 1))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	h.gw.handle(context.Background(), c, statsInbound(t, id.New(), 2))
	h.gw.removeLive(c)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel work")
	}
}

func TestTelemetryRejectsLegacyAndMalformedBeforeQueueing(t *testing.T) {
	h := newHarness(t)
	h.gw.opts.WireVersions = []uint64{5}
	h.gw.opts.Auth = &telemetryAuth{capabilityRecordingAuth: &capabilityRecordingAuth{Authenticator: h}, h: h}
	c := h.connect(t, id.New())
	h.drainReady(t, c)
	h.gw.WithCallStats(func(context.Context, auth.Session, id.ID, CallStatsReport) error {
		t.Error("invalid telemetry reached acceptance")
		return nil
	})
	for _, kind := range []string{"legacy", "cid", "group", "oversize", "range"} {
		in := statsInbound(t, id.New(), 1)
		switch kind {
		case "legacy":
			c.wire = 4
		case "cid":
			in.CID = 1
		case "group":
			v := id.New()
			in.GroupID = &v
		case "oversize":
			in.Payload[1] = bytes.Repeat([]byte{0}, 1025)
		case "range":
			in = statsInbound(t, id.New(), 60001)
		}
		h.gw.handle(context.Background(), c, in)
		c.wire = 5
	}
	if got := h.gw.TelemetryStats(); got.Devices != 0 {
		t.Fatalf("invalid sample queued: %+v", got)
	}
}

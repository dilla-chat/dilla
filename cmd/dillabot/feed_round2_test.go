//go:build linux

package main

// Bot v2 task 14, fix round 2: a bounded op 24 hold (N-1), per-installation pass backoffs and the gateway restart
// backoff (N-3), and a live event inside a snapshot applied after it (N-4).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/trust"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
)

// TestOneUsersFailingDeviceListDoesNotStallTheFeed (N-1, as Bot v2 Task 16 R2-1 changed it): user A's op 24 fails on
// every attempt. A's event is acknowledged in order at once and retried out of band; user B's revocation behind it is
// applied and the op 64 behind that acknowledged without waiting on A's retries. A is retried on the policy, stays held
// (their commands are refused) once the attempts run out, and is released by a later successful ingest. A failure
// because the helper closed also asks for a pass, which binds a fresh helper.
func TestOneUsersFailingDeviceListDoesNotStallTheFeed(t *testing.T) {
	defer func(p trust.RetryPolicy) { feedTrustRetry = p }(feedTrustRetry)
	feedTrustRetry = trust.RetryPolicy{First: 10 * time.Millisecond, Max: time.Second, Attempts: 7} // 6 waits: 10+20+40+80+160+320 ms
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	community, installation, ada, bea := feedID(1), feedID(9), feedID(4), feedID(5)
	revoking := []byte{0xbe, 0xa0}
	var adaCalls atomic.Int64
	var adaRecovers, beaApplied atomic.Bool
	var passes atomic.Int64
	h := newInstallationFeed(installation, community, feedID(8), quietLogger(), func() { passes.Add(1) })
	h.setDeps(feedDeps{ingest: func(_ context.Context, u id.ID, push []byte) (native.DeviceListIngest, error) {
		switch {
		case u == ada && !adaRecovers.Load():
			adaCalls.Add(1)
			return native.DeviceListIngest{}, fmt.Errorf("ingest: %w", helper.ErrClosed)
		case u == bea && string(push) == string(revoking):
			beaApplied.Store(true)
		}
		return native.DeviceListIngest{Version: 2}, nil
	}})
	var d *fakeDillad
	d = newFakeDillad(t, func(ctx context.Context, ws *websocket.Conn, _ uint64, _ bool) {
		_ = d.sendRaw(ctx, ws, 2, botEventRaw(t, gateway.BotEventDeviceList, []any{ada, []byte{0xad}}))
		_ = d.sendRaw(ctx, ws, 3, botEventRaw(t, gateway.BotEventDeviceList, []any{bea, revoking}))
		_ = d.send(ctx, ws, gatewayclient.OpBotCommand, 4, []any{feedID(0x50), community, feedID(0x51), feedID(0x52), []byte{0x85}})
		<-ctx.Done()
	})
	r := &botGatewayRuntime{feeds: map[id.ID]*installationFeed{installation: h}}
	started := time.Now()
	runFeed(ctx, botGatewayClient(t, d), community, h)
	waitFor(t, "B's revocation and the command acknowledged", 5*time.Second, func() bool { return d.ackedThrough() >= 4 })
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("the feed moved past A's failing list after %v, want at once", took)
	}
	waitFor(t, "A's retries run out", 5*time.Second, func() bool { return adaCalls.Load() == int64(feedTrustRetry.Attempts) })
	time.Sleep(50 * time.Millisecond)
	if got := adaCalls.Load(); got != int64(feedTrustRetry.Attempts) {
		t.Fatalf("A's list was attempted %d times, want %d", got, feedTrustRetry.Attempts)
	}
	if !beaApplied.Load() {
		t.Fatal("B's revocation behind A's failing list was not applied")
	}
	if !r.Held(installation, ada) || r.Held(installation, bea) {
		t.Fatalf("held: A %v, B %v; want A held, B not", r.Held(installation, ada), r.Held(installation, bea))
	}
	if passes.Load() == 0 {
		t.Fatal("a closed helper asked for no pass")
	}
	// A's next ingest succeeds: A is released.
	adaRecovers.Store(true)
	if err := h.DeviceList(ctx, ada, []byte{0xad, 2}, false); err != nil || r.Held(installation, ada) {
		t.Fatalf("A after a successful ingest: err %v, held %v", err, r.Held(installation, ada))
	}
}

// TestALiveEventInsideASnapshotIsAppliedAfterIt (N-4): a live op 65 (n > 0) seen while a snapshot streams is applied
// after the snapshot's swap, not before it, where the swap would undo it.
func TestALiveEventInsideASnapshotIsAppliedAfterIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	community, live, body := feedID(1), feedID(2), feedID(3)
	h := &recordingFeed{}
	var d *fakeDillad
	d = newFakeDillad(t, func(ctx context.Context, ws *websocket.Conn, _ uint64, _ bool) {
		_ = d.sendRaw(ctx, ws, 0, botEventRaw(t, gateway.BotEventSnapshotBegin, []any{community}))
		_ = d.sendRaw(ctx, ws, 2, botEventRaw(t, gateway.BotEventBotChannel, []any{community, live, uint64(1)}))
		_ = d.sendRaw(ctx, ws, 0, botEventRaw(t, gateway.BotEventBotChannel, []any{community, body, uint64(1)}))
		_ = d.sendRaw(ctx, ws, 3, botEventRaw(t, gateway.BotEventSnapshotEnd, []any{community}))
		<-ctx.Done()
	})
	runFeed(ctx, botGatewayClient(t, d), community, h)
	waitFor(t, "the snapshot and the live event acknowledged", 5*time.Second, func() bool { return d.ackedThrough() >= 3 })
	if got, want := h.appliedKeys(), []string{body.String(), live.String()}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want the snapshot body then the live event (%v)", got, want)
	}
}

// TestAFailingInstallationBacksOffOnItsOwn (N-3): installation A fails on every pass, while member activity keeps
// asking for passes of installation B, which succeed. A is retried alone (never a full pass) on its own backoff,
// doubling from 1 s; B's successes do not reset it.
func TestAFailingInstallationBacksOffOnItsOwn(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	a, b := feedID(0xa), feedID(0xb)
	passes := make(chan struct{}, 1)
	var mu sync.Mutex
	asked := map[id.ID]bool{}
	type run struct {
		at      time.Duration
		targets map[id.ID]bool
	}
	var runs []run
	start := clk.Now()
	loop := botLoop{clock: clk, log: quietLogger(), passes: passes, reload: make(chan os.Signal), wake: make(chan struct{}),
		take: func() (map[id.ID]bool, bool) {
			mu.Lock()
			defer mu.Unlock()
			out := asked
			asked = map[id.ID]bool{}
			return out, false
		},
		poll: func(context.Context) {}, maintain: func(context.Context) error { return nil }, due: time.Hour,
		pass: func(_ context.Context, targets map[id.ID]bool) error {
			mu.Lock()
			runs = append(runs, run{clk.Now().Sub(start), targets})
			mu.Unlock()
			if targets == nil || targets[a] {
				return &passFailed{installations: map[id.ID]bool{a: true}, err: errors.New("1 of 2 eligible installations reconciled")}
			}
			return nil
		}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = loop.run(ctx) }()
	waitFor(t, "the start pass", 5*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(runs) == 1 })
	for range 40 { // 40 s of fake time, B asked every second
		mu.Lock()
		asked[b] = true
		mu.Unlock()
		select {
		case passes <- struct{}{}:
		default:
		}
		time.Sleep(2 * time.Millisecond)
		clk.Advance(time.Second)
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	var retriesOfA []time.Duration
	for _, r := range runs[1:] {
		if r.targets == nil {
			t.Fatalf("a full pass ran at %v after the start; only A's retries and B's asks should", r.at)
		}
		if r.targets[a] {
			retriesOfA = append(retriesOfA, r.at)
		}
	}
	t.Logf("%d passes; A retried at %v", len(runs), retriesOfA)
	// Doubling from 1 s (1, 2, 4, 8, 16 s), shifted by the 2 s pass spacing: at most 6 in 40 s. Reset by B's
	// successes, it would be one every 1 to 2 s.
	if len(retriesOfA) == 0 || len(retriesOfA) > 6 {
		t.Fatalf("A was retried %d times in 40 s (at %v), want 1..6 on its own doubling backoff", len(retriesOfA), retriesOfA)
	}
	for i := 2; i < len(retriesOfA); i++ {
		if gap, prev := retriesOfA[i]-retriesOfA[i-1], retriesOfA[i-1]-retriesOfA[i-2]; gap < prev {
			t.Fatalf("A's retries at %v: a gap shrank, so its backoff was reset", retriesOfA)
		}
	}
}

// TestAGatewayEndingBeforeReadyIsRestartedOnABackoff (N-3): the bare-identify loop of I-4 (a gateway that ends with
// an error again and again) asks for a pass at once the first time, then after 2, 4, 8 s and so on; its ready resets it.
func TestAGatewayEndingBeforeReadyIsRestartedOnABackoff(t *testing.T) {
	var delays []time.Duration
	defer func(f func(time.Duration, func())) { gatewayAfter = f }(gatewayAfter)
	gatewayAfter = func(d time.Duration, f func()) { delays = append(delays, d); f() }
	r := newBotGatewayRuntime(t.Context(), nil, true, quietLogger())
	installation := feedID(9)
	asked := func() bool {
		targets, _ := r.takePassTargets()
		return targets[installation]
	}
	for range 4 {
		r.requestPassAfterGatewayEnd(installation)
		if !asked() {
			t.Fatal("a gateway end asked for no pass")
		}
	}
	if want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}; !slices.Equal(delays, want) {
		t.Fatalf("restart delays %v, want at once and then %v", delays, want)
	}
	r.gatewayReady(installation)
	delays = nil
	r.requestPassAfterGatewayEnd(installation)
	if !asked() || len(delays) != 0 {
		t.Fatalf("after ready the next end waited %v, want a pass at once", delays)
	}
}

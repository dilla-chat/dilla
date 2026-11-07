//go:build linux

package main

// Bot v2 Task 16: membership trust behind the feed. Failing device lists are retried out of band, per user (R2-1); a
// closed helper is replaced and ingest works again without a Bot restart (R2-3); a transient helper status keeps the
// user held until the push applies (R2-4, see also internal/bot/trust).

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/trust"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
)

// trustCoreFunc is a TrustCore from an op 24 function, with no MLS leaves.
type trustCoreFunc func(context.Context, id.ID, []byte) (native.DeviceListIngest, error)

func (f trustCoreFunc) IngestDeviceList(ctx context.Context, u id.ID, push []byte) (native.DeviceListIngest, error) {
	return f(ctx, u, push)
}
func (trustCoreFunc) PendingLeaves(context.Context) ([]native.GroupPendingLeaf, error) {
	return nil, nil
}
func (trustCoreFunc) RevalidateGroup(context.Context, id.ID, uint64, uint64) (uint32, error) {
	return 0, nil
}

// TestFiveFailingUsersDoNotDelayASixthsRevocation (R2-1): five users' lists fail at once (a snapshot-sized burst), each
// on every attempt. The sixth user's revocation behind them, and the command behind that, are applied and acknowledged
// within one retry step (trust.DefaultRetry.First), not after N users' retries; the five stay held.
func TestFiveFailingUsersDoNotDelayASixthsRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	community, installation, sixth := feedID(1), feedID(9), feedID(0x60)
	failing := []id.ID{feedID(0x50), feedID(0x51), feedID(0x52), feedID(0x53), feedID(0x54)}
	revoking := []byte{0x60, 0x0f}
	var applied atomic.Bool
	h := newInstallationFeed(installation, community, feedID(8), quietLogger(), func() {})
	defer h.trust.Close()
	h.setDeps(feedDeps{core: trustCoreFunc(func(_ context.Context, u id.ID, push []byte) (native.DeviceListIngest, error) {
		if u == sixth {
			applied.Store(string(push) == string(revoking))
			return native.DeviceListIngest{Changed: true, Version: 2}, nil
		}
		return native.DeviceListIngest{}, &helper.StatusError{Status: 9} // internal: transient
	})})
	var d *fakeDillad
	d = newFakeDillad(t, func(ctx context.Context, ws *websocket.Conn, _ uint64, _ bool) {
		for i, u := range failing {
			_ = d.sendRaw(ctx, ws, uint64(2+i), botEventRaw(t, gateway.BotEventDeviceList, []any{u, []byte{byte(i), 1}}))
		}
		_ = d.sendRaw(ctx, ws, 7, botEventRaw(t, gateway.BotEventDeviceList, []any{sixth, revoking}))
		_ = d.send(ctx, ws, gatewayclient.OpBotCommand, 8, []any{feedID(0x70), community, feedID(0x71), feedID(0x72), []byte{0x85}})
		<-ctx.Done()
	})
	started := time.Now()
	runFeed(ctx, botGatewayClient(t, d), community, h)
	waitFor(t, "the sixth user's revocation and the command acknowledged", 5*time.Second, func() bool { return d.ackedThrough() >= 8 })
	if took := time.Since(started); took >= trust.DefaultRetry.First {
		t.Fatalf("the sixth user's revocation was acknowledged after %v, want within one retry step (%v)", took, trust.DefaultRetry.First)
	}
	if !applied.Load() || h.Held(sixth) {
		t.Fatalf("sixth: applied %v held %v; want applied and not held", applied.Load(), h.Held(sixth))
	}
	for _, u := range failing {
		if !h.Held(u) {
			t.Fatalf("failing user %s is not held", u)
		}
	}
}

// TestAfterErrClosedIngestWorksAgainWithoutRestart (R2-3): the bound helper closed, so a user's list fails with
// helper.ErrClosed: the user is held and a pass is asked for. The pass binds a fresh helper (retainedNativeClient
// retires the closed one), and binding it retries the held user at once: the list applies and the user is released,
// with no Bot restart and no wait for the retry backoff.
func TestAfterErrClosedIngestWorksAgainWithoutRestart(t *testing.T) {
	community, installation, ada := feedID(1), feedID(9), feedID(4)
	var passes atomic.Int64
	h := newInstallationFeed(installation, community, feedID(8), quietLogger(), func() { passes.Add(1) })
	defer h.trust.Close()
	closed := trustCoreFunc(func(context.Context, id.ID, []byte) (native.DeviceListIngest, error) {
		return native.DeviceListIngest{}, fmt.Errorf("call: %w", helper.ErrClosed)
	})
	h.setDeps(feedDeps{core: closed})
	if err := h.DeviceList(t.Context(), ada, []byte{0xad}, false); err != nil {
		t.Fatalf("DeviceList returned %v; a failure is retried, never returned to the feed", err)
	}
	if !h.Held(ada) || passes.Load() == 0 {
		t.Fatalf("held %v, passes %d; want held and a pass asked for", h.Held(ada), passes.Load())
	}
	var fresh atomic.Int64
	h.setDeps(feedDeps{core: trustCoreFunc(func(context.Context, id.ID, []byte) (native.DeviceListIngest, error) {
		fresh.Add(1)
		return native.DeviceListIngest{Changed: true, Version: 2}, nil
	})})
	waitFor(t, "the held user released by the fresh helper", 500*time.Millisecond, func() bool { return !h.Held(ada) })
	if fresh.Load() != 1 {
		t.Fatalf("the fresh helper ingested %d times, want 1", fresh.Load())
	}
}

// closedRetained is a retained helper whose process went away.
type closedRetained struct{ musicRuntimeNative }

func (closedRetained) Usable() bool { return false }

type fakeNativeRegistry struct {
	mu       sync.Mutex
	native   musicRuntimeNative
	disabled []id.ID
}

func (r *fakeNativeRegistry) Native(id.ID, id.ID) musicRuntimeNative { return r.native }
func (r *fakeNativeRegistry) Disable(installation id.ID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disabled = append(r.disabled, installation)
	r.native = nil
	return nil
}

// TestAClosedRetainedHelperIsReplacedByThePass (R2-3): the pass does not reuse a retained helper that closed; it retires
// it, so the pass starts a fresh one.
func TestAClosedRetainedHelperIsReplacedByThePass(t *testing.T) {
	installation := feedID(9)
	reg := &fakeNativeRegistry{native: closedRetained{}}
	if c, ok := retainedNativeClient(reg, installation, feedID(1)); ok || c != nil {
		t.Fatalf("a closed retained helper was reused: %v %v", c, ok)
	}
	if len(reg.disabled) != 1 || reg.disabled[0] != installation {
		t.Fatalf("disabled %v, want the installation retired once", reg.disabled)
	}
	if _, ok := retainedNativeClient(&fakeNativeRegistry{}, installation, feedID(1)); ok {
		t.Fatal("no retained helper reported one")
	}
}

// sweepCore counts pending-leaf reads and blocks each until released.
type sweepCore struct {
	trustCoreFunc
	reads   atomic.Int64
	release chan struct{}
}

func (c *sweepCore) PendingLeaves(context.Context) ([]native.GroupPendingLeaf, error) {
	c.reads.Add(1)
	<-c.release
	return nil, nil
}

// TestASnapshotSweepsLeavesWithoutDelayingReadiness: an applied snapshot runs one trust sweep (leaves left unjudged by
// a restart), off the feed's worker: SnapshotDone returns while the sweep is still reading.
func TestASnapshotSweepsLeavesWithoutDelayingReadiness(t *testing.T) {
	core := &sweepCore{trustCoreFunc: func(context.Context, id.ID, []byte) (native.DeviceListIngest, error) {
		return native.DeviceListIngest{}, nil
	}, release: make(chan struct{})}
	h := newInstallationFeed(feedID(9), feedID(1), feedID(8), quietLogger(), func() {})
	defer h.trust.Close()
	h.setDeps(feedDeps{core: core})
	done := make(chan struct{})
	go func() {
		h.SnapshotBegin(t.Context())
		h.SnapshotDone(t.Context())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SnapshotDone waited on the sweep")
	}
	waitFor(t, "the sweep's read", time.Second, func() bool { return core.reads.Load() == 1 })
	close(core.release)
}

// TestADroppedFeedStopsItsTrustRetries (fix round 1, M-3): an installation that is removed or disabled drops its feed,
// and the feed's pending trust retries stop with it; the failing user stays held.
func TestADroppedFeedStopsItsTrustRetries(t *testing.T) {
	defer func(p trust.RetryPolicy) { feedTrustRetry = p }(feedTrustRetry)
	feedTrustRetry = trust.RetryPolicy{First: 20 * time.Millisecond, Max: 20 * time.Millisecond, Attempts: 1000}
	installation, ada := feedID(9), feedID(4)
	var calls atomic.Int64
	h := newInstallationFeed(installation, feedID(1), feedID(8), quietLogger(), func() {})
	h.setDeps(feedDeps{core: trustCoreFunc(func(context.Context, id.ID, []byte) (native.DeviceListIngest, error) {
		calls.Add(1)
		return native.DeviceListIngest{}, &helper.StatusError{Status: 6}
	})})
	_ = h.DeviceList(t.Context(), ada, []byte{0xad}, false)
	waitFor(t, "a retry", time.Second, func() bool { return calls.Load() >= 2 })
	r := &botGatewayRuntime{feeds: map[id.ID]*installationFeed{installation: h}}
	r.RetainOnly(nil)
	time.Sleep(50 * time.Millisecond)
	settledAt := calls.Load()
	time.Sleep(150 * time.Millisecond)
	if got := calls.Load(); got != settledAt {
		t.Fatalf("a dropped feed's retries kept running: %d more op 24 calls", got-settledAt)
	}
	if !h.Held(ada) {
		t.Fatal("the failing user was released when the feed was dropped")
	}
}

// TestLeftGroupsReportedByMembershipCostNoTrustWork (fix round 1, M-5): the groups the membership reconciler reports as
// left are skipped by the feed's trust; the Bot channels still need re-adding are kept for the admin page.
func TestLeftGroupsReportedByMembershipCostNoTrustWork(t *testing.T) {
	installation, left, kept, channel := feedID(9), feedID(0x41), feedID(0x42), feedID(0x43)
	h := newInstallationFeed(installation, feedID(1), feedID(8), quietLogger(), func() {})
	defer h.trust.Close()
	r := &botGatewayRuntime{feeds: map[id.ID]*installationFeed{installation: h}}
	r.BotChannels(installation).Membership(map[id.ID]bool{left: true}, map[id.ID]bool{channel: true})
	if h.inTrustGroup(left) || !h.inTrustGroup(kept) {
		t.Fatalf("in trust: left %v kept %v; want the left group skipped", h.inTrustGroup(left), h.inTrustGroup(kept))
	}
	if !h.NeedsReAdd()[channel] {
		t.Fatal("the channel needing re-add was not kept")
	}
}

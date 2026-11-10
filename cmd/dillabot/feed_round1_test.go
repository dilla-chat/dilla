//go:build linux

package main

// Bot v2 task 14, fix round 1: the snapshot replaces state (I-1), a transient op 24 failure is held (I-2), passes are
// targeted and spaced (I-3), readiness from the loop's start (I-5), the highest channel-group epoch (M-4), and the
// idle loop with the real maintenance (M-7).

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/bot/trust"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
)

func noTargets() (map[id.ID]bool, bool) { return nil, false }

// TestSnapshotReplacesTheFeedState (I-1): what a snapshot does not name is gone after it (a role deleted, a channel
// unmarked, a member who left, a call group closed while the Bot was away), and a snapshot cut off before
// snapshot_end changes nothing.
func TestSnapshotReplacesTheFeedState(t *testing.T) {
	ctx := t.Context()
	community, role, channel, ada, bea, group := feedID(1), feedID(2), feedID(3), feedID(4), feedID(5), feedID(6)
	f := newInstallationFeed(feedID(9), community, feedID(8), quietLogger(), nil)
	f.SnapshotBegin(ctx)
	f.Role(ctx, RoleEvent{Community: community, Role: role, Allow: 1, FromSnapshot: true})
	f.BotChannel(ctx, community, channel, true, true)
	f.Member(ctx, MemberEvent{Community: community, User: ada, Joined: true, Snapshot: true})
	f.Member(ctx, MemberEvent{Community: community, User: bea, Joined: true, Snapshot: true})
	f.ChannelGroup(ctx, ChannelGroupEvent{Community: community, Channel: channel, Group: group, Epoch: 4, Snapshot: true})
	f.Community(ctx, community, ada, true)
	f.SnapshotDone(ctx)
	first := f.snapshotOfState()
	if len(first.roles) != 1 || !first.botChannels[channel] || len(first.members) != 2 || len(first.groups) != 1 || first.owner != ada {
		t.Fatalf("after the first snapshot: %+v", first)
	}
	// A cut-off snapshot: begun, partly streamed, never done. Nothing changes.
	f.SnapshotBegin(ctx)
	f.Member(ctx, MemberEvent{Community: community, User: ada, Joined: true, Snapshot: true})
	if got := f.snapshotOfState(); len(got.roles) != 1 || len(got.members) != 2 || !got.botChannels[channel] {
		t.Fatalf("a cut-off snapshot changed the state: %+v", got)
	}
	// The next snapshot names only Ada: the role, the Bot channel, Bea and the group are gone.
	f.SnapshotBegin(ctx)
	f.Member(ctx, MemberEvent{Community: community, User: ada, Joined: true, Snapshot: true})
	f.Community(ctx, community, bea, true)
	f.SnapshotDone(ctx)
	got := f.snapshotOfState()
	if len(got.roles) != 0 || len(got.botChannels) != 0 || len(got.groups) != 0 || len(got.members) != 1 || !got.members[ada] || got.owner != bea {
		t.Fatalf("a snapshot did not replace the state: %+v", got)
	}
}

// TestChannelGroupKeepsTheHighestEpoch (M-4): an older epoch, queued before a newer one was applied, never goes back.
func TestChannelGroupKeepsTheHighestEpoch(t *testing.T) {
	ctx := t.Context()
	community, channel, group := feedID(1), feedID(2), feedID(3)
	f := newInstallationFeed(feedID(9), community, feedID(8), quietLogger(), nil)
	f.ChannelGroup(ctx, ChannelGroupEvent{Community: community, Channel: channel, Group: group, Epoch: 7})
	f.ChannelGroup(ctx, ChannelGroupEvent{Community: community, Channel: channel, Group: group, Epoch: 5})
	if got := f.snapshotOfState().groups[group].Epoch; got != 7 {
		t.Fatalf("epoch %d after an older event, want 7", got)
	}
}

// TestStateEventsAskForNoPass (I-3): role, channel_group and community only update state; membership asks for a pass.
func TestStateEventsAskForNoPass(t *testing.T) {
	ctx := t.Context()
	community := feedID(1)
	var asks atomic.Int64
	f := newInstallationFeed(feedID(9), community, feedID(8), quietLogger(), func() { asks.Add(1) })
	f.Role(ctx, RoleEvent{Community: community, Role: feedID(2), Allow: 1})
	f.ChannelGroup(ctx, ChannelGroupEvent{Community: community, Channel: feedID(3), Group: feedID(4), Epoch: 1})
	f.Community(ctx, community, feedID(5), false)
	if asks.Load() != 0 {
		t.Fatalf("state-only events asked for %d passes", asks.Load())
	}
	f.Member(ctx, MemberEvent{Community: community, User: feedID(6), Joined: true})
	if asks.Load() != 1 {
		t.Fatalf("a join asked for %d passes, want 1", asks.Load())
	}
}

// TestARevocationSurvivesATransientHelperError (I-2, as Bot v2 Task 16 R2-1 changed it): op 24 fails transiently (not
// a StatusError) on a pushed list that revokes a device. The user is held (their commands fail closed) at once; the
// event and the frames behind it are acknowledged in order without waiting (the retry is out of band), and the push is
// applied on the retry, which releases the user.
func TestARevocationSurvivesATransientHelperError(t *testing.T) {
	defer func(p trust.RetryPolicy) { feedTrustRetry = p }(feedTrustRetry)
	feedTrustRetry = trust.RetryPolicy{First: 50 * time.Millisecond, Max: time.Second, Attempts: 8}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	community, user := feedID(1), feedID(4)
	revoking := []byte{0xaa, 0xbb}
	var calls atomic.Int64
	var applied atomic.Bool
	release := make(chan struct{})
	h := newInstallationFeed(feedID(9), community, feedID(8), quietLogger(), nil)
	h.setDeps(feedDeps{ingest: func(_ context.Context, u id.ID, push []byte) (native.DeviceListIngest, error) {
		if calls.Add(1) == 1 {
			return native.DeviceListIngest{}, errors.New("helper: connection reset") // transient
		}
		<-release
		if u == user && string(push) == string(revoking) {
			applied.Store(true)
		}
		return native.DeviceListIngest{Changed: true, Version: 3}, nil
	}})
	var d *fakeDillad
	d = newFakeDillad(t, func(ctx context.Context, ws *websocket.Conn, _ uint64, _ bool) {
		_ = d.sendRaw(ctx, ws, 2, botEventRaw(t, gateway.BotEventDeviceList, []any{user, revoking}))
		_ = d.sendRaw(ctx, ws, 3, botEventRaw(t, gateway.BotEventBotChannel, []any{community, feedID(5), uint64(1)}))
		<-ctx.Done()
	})
	runFeed(ctx, botGatewayClient(t, d), community, h)
	waitFor(t, "the failed attempt and the retry", 5*time.Second, func() bool { return calls.Load() >= 2 })
	if !h.Held(user) {
		t.Fatal("the user is not held while their device list waits to be applied")
	}
	waitFor(t, "the device list and the frame behind it acknowledged in order", 5*time.Second, func() bool { return d.ackedThrough() >= 3 })
	if applied.Load() || !h.Held(user) {
		t.Fatalf("applied %v, held %v before the retry finished; want not applied and held", applied.Load(), h.Held(user))
	}
	close(release)
	waitFor(t, "the revocation applied by the retry", 5*time.Second, func() bool { return applied.Load() && !h.Held(user) })
}

// TestAMemberBurstCostsAtMostOnePassPerTwoSeconds (I-3): 50 member events over 10 s of fake clock cost at most one
// pass per passMinSpacing, and each pass runs only the installation the events are about.
func TestAMemberBurstCostsAtMostOnePassPerTwoSeconds(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := newBotGatewayRuntime(ctx, nil, true, quietLogger())
	installation, community := feedID(9), feedID(1)
	f := newInstallationFeed(installation, community, feedID(8), quietLogger(), func() { r.requestPass(installation) })
	var mu sync.Mutex
	var passes []map[id.ID]bool
	loop := botLoop{clock: clk, log: quietLogger(), passes: r.Pass(), take: r.takePassTargets, reload: make(chan os.Signal),
		wake: make(chan struct{}), poll: func(context.Context) {}, maintain: func(context.Context) error { return nil }, due: time.Hour,
		pass: func(_ context.Context, targets map[id.ID]bool) error {
			mu.Lock()
			passes = append(passes, targets)
			mu.Unlock()
			return nil
		}}
	go func() { _ = loop.run(ctx) }()
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(passes) }
	waitFor(t, "the start pass", 5*time.Second, func() bool { return count() == 1 })
	for i := range 50 {
		u := feedID(byte(20 + i))
		f.Member(ctx, MemberEvent{Community: community, User: u, Joined: true})
		time.Sleep(time.Millisecond)
		clk.Advance(200 * time.Millisecond)
	}
	clk.Advance(passMinSpacing)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	// The start pass at t = 0, then at most one per 2 s over the 10 s burst plus the trailing one.
	if n := len(passes) - 1; n < 1 || n > 6 {
		t.Fatalf("%d passes for 50 member events over 10 s, want 1..6 (one per 2 s)", n)
	}
	for _, targets := range passes[1:] {
		if len(targets) != 1 || !targets[installation] {
			t.Fatalf("a member event's pass ran %v, want only installation %s", targets, installation)
		}
	}
}

// TestBotLoopRunsAPassOnlyOnAnEvent: a feed change, a SIGHUP and a failed pass's retry each run one pass, spaced
// passMinSpacing apart; a success arms no retry; an incomplete installation is retried on its own.
func TestBotLoopRunsAPassOnlyOnAnEvent(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	passes := make(chan struct{}, 1)
	reload := make(chan os.Signal, 1)
	installation := feedID(9)
	var runs atomic.Int64
	var fail, incomplete atomic.Bool
	var last atomic.Value
	loop := botLoop{clock: clk, log: quietLogger(), passes: passes, reload: reload, wake: make(chan struct{}),
		take: func() (map[id.ID]bool, bool) { return map[id.ID]bool{installation: true}, false },
		poll: func(context.Context) {}, maintain: func(context.Context) error { return nil }, due: time.Hour,
		pass: func(_ context.Context, targets map[id.ID]bool) error {
			runs.Add(1)
			last.Store(targets)
			switch {
			case fail.Load():
				return errors.New("transient")
			case incomplete.Load():
				return &passIncomplete{after: map[id.ID]time.Duration{installation: 0}}
			}
			return nil
		}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = loop.run(ctx) }()
	step := func(what string, want int64, advance time.Duration) {
		t.Helper()
		time.Sleep(20 * time.Millisecond)
		clk.Advance(advance)
		waitFor(t, what, 5*time.Second, func() bool { return runs.Load() == want })
	}
	waitFor(t, "the start pass", 5*time.Second, func() bool { return runs.Load() == 1 })
	passes <- struct{}{}
	step("a spaced pass on a feed change", 2, passMinSpacing)
	if targets, _ := last.Load().(map[id.ID]bool); len(targets) != 1 || !targets[installation] {
		t.Fatalf("the feed change's pass ran %v, want only its installation", targets)
	}
	reload <- syscall.SIGHUP
	step("a pass on SIGHUP", 3, passMinSpacing)
	if targets, _ := last.Load().(map[id.ID]bool); targets != nil {
		t.Fatalf("SIGHUP's pass ran %v, want every installation", targets)
	}
	fail.Store(true)
	passes <- struct{}{}
	step("the failing pass", 4, passMinSpacing)
	fail.Store(false)
	step("the retry", 5, passMinSpacing)
	incomplete.Store(true)
	passes <- struct{}{}
	step("an incomplete pass", 6, passMinSpacing)
	incomplete.Store(false)
	step("the incomplete installation's retry", 7, passMinSpacing)
	clk.Advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	if got := runs.Load(); got != 7 {
		t.Fatalf("%d passes, want no retry after a success", got)
	}
}

// TestBotIsReadyForCommandsWithin5sFromLoopStart (I-5): from the start of the Bot's main loop, through its first
// pass (which starts the gateway and binds the feed handler), the snapshot of 200 members' device lists (600 devices,
// three per member) through op 24 with 2 ms per call, to the first op 64 behind it acknowledged: within 5 s. Task 15's
// sealed intake does not exist yet, so the furthest real point is the op 64's acknowledgement, which the ordered acks
// hold behind the applied snapshot.
func TestBotIsReadyForCommandsWithin5sFromLoopStart(t *testing.T) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	community, installation := feedID(1), feedID(9)
	const members = 200
	push := make([]byte, 3*400)
	var d *fakeDillad
	d = newFakeDillad(t, func(ctx context.Context, ws *websocket.Conn, _ uint64, _ bool) {
		send := func(n uint64, kind uint64, body any) bool {
			return d.sendRaw(ctx, ws, n, botEventRaw(t, kind, body)) == nil
		}
		_ = send(0, gateway.BotEventSnapshotBegin, []any{community})
		for i := range members {
			u := feedID(byte(i))
			u[1] = byte(i >> 8)
			_ = send(0, gateway.BotEventDeviceList, []any{u, push})
			_ = send(0, gateway.BotEventMember, []any{community, u, uint64(1), []id.ID{}})
		}
		_ = send(2, gateway.BotEventSnapshotEnd, []any{community})
		_ = d.send(ctx, ws, gatewayclient.OpBotCommand, 3, []any{feedID(0x50), community, feedID(0x51), feedID(0x52), []byte{0x85}})
		<-ctx.Done()
	})
	api, err := client.New(client.Config{BaseURL: supervisedOrigin, InstanceID: d.instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	launch, err := gatewayclient.NewLaunchToken(append([]byte(nil), d.launch...))
	if err != nil {
		t.Fatal(err)
	}
	r := newBotGatewayRuntime(ctx, api, true, quietLogger())
	r.httpClient, r.launch = &http.Client{Transport: unixTransport(d.sock)}, launch
	defer r.Close()
	var ingested atomic.Int64
	ingest := func(context.Context, id.ID, []byte) (native.DeviceListIngest, error) {
		time.Sleep(2 * time.Millisecond)
		ingested.Add(1)
		return native.DeviceListIngest{Changed: true, Version: 1}, nil
	}
	loop := botLoop{clock: clock.System(), log: quietLogger(), passes: r.Pass(), take: r.takePassTargets, reload: make(chan os.Signal),
		wake: r.Wake(), poll: func(context.Context) {}, maintain: func(context.Context) error { return nil }, due: time.Hour,
		pass: func(_ context.Context, _ map[id.ID]bool) error {
			if err := r.Ensure(installation, community, d.dev, d.user, "session-bearer"); err != nil {
				return err
			}
			r.feedFor(installation, community, feedID(8), feedDeps{ingest: ingest})
			return nil
		}}
	go func() { _ = loop.run(ctx) }()
	waitFor(t, "the command acknowledged", 10*time.Second, func() bool { return d.ackedThrough() >= 3 })
	if took := time.Since(started); took > readyBudget {
		t.Fatalf("the first command was accepted %v after the loop started, want within %v", took, readyBudget)
	}
	if ingested.Load() != members {
		t.Fatalf("op 24 ran %d times, want %d", ingested.Load(), members)
	}
	e := r.entry(installation)
	if e == nil || e.feed.readyWithin(ctx, time.Millisecond) != nil {
		t.Fatal("the feed is not ready after its snapshot was applied")
	}
}

// TestBotMakesNoPeriodicRequestsWithTheRealMaintenance (M-7): the due tick runs the real maintenance (scheduled
// announcements, reminders and poll deadlines over a real helper with an empty state) for 10 minutes of fake clock;
// dillad sees no request.
func TestBotMakesNoPeriodicRequestsWithTheRealMaintenance(t *testing.T) {
	binary := os.Getenv("DILLA_BOT_HELPER_BIN")
	if binary == "" {
		for _, candidate := range []string{"../../target/release/dilla-bot-core", "../../target/debug/dilla-bot-core"} {
			if _, err := os.Stat(candidate); err == nil {
				binary = candidate
				break
			}
		}
	}
	if binary == "" {
		t.Skip("dilla-bot-core is not built: cargo build -p dilla-bot-core, or set DILLA_BOT_HELPER_BIN")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	instance, community, installation := id.New(), id.New(), id.New()
	api, err := client.New(client.Config{BaseURL: srv.URL, InstanceID: instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	session, err := api.Session("session-bearer")
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	key[0] = 7
	n, err := native.Start(t.Context(), helper.Options{Executable: binary, StateDir: filepath.Join(t.TempDir(), "helper")},
		native.Binding{Instance: instance, Community: community, Installation: installation}, key)
	if err != nil {
		t.Fatalf("start the helper: %v", err)
	}
	defer func() { _ = n.Close() }()
	scheduler, err := service.NewAnnouncementScheduler(n, session, api)
	if err != nil {
		t.Fatal(err)
	}
	utilities, err := newUtilityRuntime(n, session, api, community, installation, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var maintained atomic.Int64
	clk := clock.NewFake(time.Unix(1_800_000_000, 0))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	loop := botLoop{clock: clk, log: log, passes: make(chan struct{}), take: noTargets, reload: make(chan os.Signal),
		wake: make(chan struct{}), poll: func(context.Context) {}, due: dueInterval,
		pass: func(context.Context, map[id.ID]bool) error { return nil },
		maintain: func(ctx context.Context) error {
			defer maintained.Add(1)
			return runBackgroundMaintenance(client.WithBackground(ctx), scheduler.RunDue, utilities.maintain)
		}}
	go func() { _ = loop.run(ctx) }()
	// Advance until fifty real maintenance passes ran (at least 500 idle seconds at dueInterval). The clock keeps moving
	// while a pass runs the real helper, so a loaded runner that coalesces a tick still reaches fifty instead of
	// running out of advances.
	var advanced time.Duration
	for deadline := time.Now().Add(30 * time.Second); maintained.Load() < 50; {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the due ticks: %d maintenance passes in %v of idle time", maintained.Load(), advanced)
		}
		clk.Advance(time.Second)
		advanced += time.Second
		time.Sleep(200 * time.Microsecond)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("%d requests reached dillad in %v of idle real maintenance, want 0", got, advanced)
	}
}

//go:build linux

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

type presenceTestStore struct{}

func (presenceTestStore) GroupsForDevice(context.Context, id.ID) ([]id.ID, error) { return nil, nil }
func (presenceTestStore) GetCursor(context.Context, id.ID, id.ID) (store.CursorRow, error) {
	return store.CursorRow{}, nil
}
func (presenceTestStore) CountKeyPackages(context.Context, id.ID, int64) (int64, error) {
	return 0, nil
}

type presenceTestAuth struct{ session auth.Session }

func (a presenceTestAuth) Resolve(context.Context, string) (auth.Session, error) {
	return a.session, nil
}
func (presenceTestAuth) RecordCapabilities(context.Context, []byte, uint64, uint64, uint64) error {
	return nil
}
func TestBotGatewayRuntimePublishesPresenceAndStopsOnRemoval(t *testing.T) {
	instance, device, user, installation := id.New(), id.New(), id.New(), id.New()
	gw := gateway.New(gateway.Options{InstanceID: instance, Generation: 1, WireVersions: []uint64{3}, E2EEVersions: []uint64{1}, MediaVersions: []uint64{1}, Store: presenceTestStore{}, Auth: presenceTestAuth{auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled, TokenHash: make([]byte, 32)}}, TrustedOrigins: []string{"*"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	server := httptest.NewServer(gw.Handler())
	defer server.Close()
	defer gw.Shutdown(context.Background())
	api, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newBotGatewayRuntime(t.Context(), api, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer runtime.Close()
	if err := runtime.Ensure(installation, device, user, "session"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.Wake():
	case <-time.After(3 * time.Second):
		t.Fatal("gateway did not wake durable polling on Ready")
	}
	deadline := time.Now().Add(2 * time.Second)
	for gw.PresenceStatus(user, 0) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if gw.PresenceStatus(user, 0) == 0 {
		t.Fatal("bot remained offline")
	}
	runtime.RetainOnly(nil)
	deadline = time.Now().Add(2 * time.Second)
	for gw.PresenceStatus(user, 0) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if gw.PresenceStatus(user, 0) != 0 {
		t.Fatal("removed installation retained gateway presence")
	}
}

type lateNotificationStore struct {
	presenceTestStore
	joined atomic.Bool
	group  id.ID
}

func (s *lateNotificationStore) GroupsForDevice(context.Context, id.ID) ([]id.ID, error) {
	if s.joined.Load() {
		return []id.ID{s.group}, nil
	}
	return nil, nil
}

func (s *lateNotificationStore) GroupResource(context.Context, id.ID) (gateway.GroupResource, error) {
	return gateway.GroupResource{Kind: 3}, nil
}
func (s *lateNotificationStore) ChannelCommunity(context.Context, id.ID) (*id.ID, error) {
	return nil, nil
}
func (s *lateNotificationStore) Enabled(context.Context, id.ID) (bool, error) {
	return true, nil
}

type privateNotificationSource struct{ group id.ID }

func (s privateNotificationSource) Groups(context.Context) ([]native.GroupInfo, error) {
	return nil, nil
}
func (s privateNotificationSource) InteractionGroups(context.Context) ([]native.InteractionGroupDescriptor, error) {
	return []native.InteractionGroupDescriptor{{Group: s.group, State: 2}}, nil
}

func TestPrivateGroupJoinedAfterReadyWakesCommandIntake(t *testing.T) {
	instance, device, user, installation, group := id.New(), id.New(), id.New(), id.New(), id.New()
	store := &lateNotificationStore{group: group}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(gateway.Options{InstanceID: instance, Generation: 1, WireVersions: []uint64{3}, E2EEVersions: []uint64{1}, MediaVersions: []uint64{1}, Store: store, ScopeResolver: store, CommunityCompatibility: store, Auth: presenceTestAuth{auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled, TokenHash: make([]byte, 32)}}, TrustedOrigins: []string{"*"}, Log: log})
	server := httptest.NewServer(gw.Handler())
	defer server.Close()
	defer gw.Shutdown(context.Background())
	api, err := client.New(client.Config{BaseURL: server.URL, InstanceID: instance, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	runtime := newBotGatewayRuntime(t.Context(), api, true, log)
	defer runtime.Close()
	polled := make(chan id.ID, 1)
	runtime.SetInteractionPoller(installation, func(_ context.Context, group id.ID) error {
		polled <- group
		return nil
	})
	runtime.StartCommandWorkers()
	if err := runtime.Ensure(installation, device, user, "session"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.Wake():
	case <-time.After(3 * time.Second):
		t.Fatal("gateway did not become ready")
	}
	store.joined.Store(true)
	gw.SetGroupMembers(group, []id.ID{device})
	if err := runtime.SubscribeNativeGroups(t.Context(), installation, privateNotificationSource{group}); err != nil {
		t.Fatal(err)
	}
	// Bot uploads are already durably processed locally. Echoing its own reply
	// must not trigger another complete source sweep (and another reply wake).
	ownPayload, err := gateway.MessageCTPayload(1, 1, device, []byte("own reply"), make([]byte, 32), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ownDeadline := time.NewTimer(200 * time.Millisecond)
	defer ownDeadline.Stop()
	ownTicker := time.NewTicker(10 * time.Millisecond)
	defer ownTicker.Stop()
ownEcho:
	for {
		gw.DeliverGroup(group, gateway.Frame{Op: gateway.OpMessageCT, GroupID: &group, Payload: ownPayload, Replay: true})
		select {
		case <-runtime.Wake():
			t.Fatal("own ciphertext echo woke command intake")
		case <-polled:
			t.Fatal("own ciphertext echo woke interaction intake")
		case <-ownDeadline.C:
			break ownEcho
		case <-ownTicker.C:
		}
	}
	payload, err := gateway.MessageCTPayload(1, 1, id.New(), []byte("ciphertext"), make([]byte, 32), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Sending Subscribe completes before the server necessarily consumes it.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		gw.DeliverGroup(group, gateway.Frame{Op: gateway.OpMessageCT, GroupID: &group, Payload: payload, Replay: true})
		select {
		case polledGroup := <-polled:
			if polledGroup != group {
				t.Fatalf("polled group %s, want private group %s", polledGroup, group)
			}
			// A participant's message reaches its installation's command worker.
			select {
			case <-runtime.Wake():
				t.Fatal("an interaction-group message also took the throttled general wake")
			default:
			}
			return
		case <-runtime.Wake():
			t.Fatal("an interaction-group message took the throttled general wake")
		case <-ticker.C:
		case <-timeout.C:
			t.Fatal("new private group did not wake command intake")
		}
	}
}

// Repeated notifications coalesce while every group keeps its first place in line.
func TestInteractionSignalsCoalescePerGroup(t *testing.T) {
	runtime := newBotGatewayRuntime(t.Context(), nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	installation, a, b := id.New(), id.New(), id.New()
	runtime.SetInteractionPoller(installation, func(context.Context, id.ID) error { return nil })
	for range 10 {
		runtime.notifyInteraction(installation, a)
	}
	runtime.notifyInteraction(installation, b)
	pool := newCommandBatchTestPool(t)
	now := time.Now()
	for _, group := range []id.ID{a, b} {
		jobs, _, _ := runtime.commandBatch(pool, now)
		if len(jobs) != 1 || jobs[0].work.group != group {
			t.Fatalf("jobs=%v, want group %s once in notification order", jobs, group)
		}
		releaseCommandBatch(pool, jobs)
		now = now.Add(interactionSpacing)
	}
	if jobs, _, pending := runtime.commandBatch(pool, now); len(jobs) != 0 || pending {
		t.Fatalf("drained notifications left jobs=%v pending=%v", jobs, pending)
	}
}

func newCommandBatchTestPool(t *testing.T) *commandWorkers {
	t.Helper()
	return &commandWorkers{ctx: t.Context(), active: make(map[id.ID]context.CancelFunc), background: make(map[id.ID]bool), retry: make(map[interactionWork]time.Time)}
}

func releaseCommandBatch(pool *commandWorkers, jobs []commandJob) {
	for _, job := range jobs {
		job.cancel()
		delete(pool.active, job.work.installation)
		delete(pool.background, job.work.installation)
	}
}

// RetainOnly forgets removed installations' pending groups and budgets. Drop keeps
// the budget so a manager restart cannot reset a flood's allowance.
func TestRetainOnlyForgetsRemovedInstallationsInteractionState(t *testing.T) {
	runtime := newBotGatewayRuntime(t.Context(), nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	kept, removed := id.New(), id.New()
	t0 := time.Unix(1_800_000_000, 0)
	for _, installation := range []id.ID{kept, removed} {
		runtime.SetInteractionPoller(installation, func(context.Context, id.ID) error { return nil })
		runtime.notifyInteraction(installation, id.New())
	}
	pool := newCommandBatchTestPool(t)
	jobs, _, _ := runtime.commandBatch(pool, t0) // creates both budgets
	releaseCommandBatch(pool, jobs)
	runtime.notifyInteraction(removed, id.New())
	runtime.notifyInteraction(kept, id.New())
	state := func(installation id.ID) (budget, pending bool) {
		runtime.pendingMu.Lock()
		defer runtime.pendingMu.Unlock()
		return runtime.budgets[installation] != nil, len(runtime.pending[installation]) > 0
	}
	runtime.RetainOnly([]id.ID{kept})
	if budget, pending := state(removed); budget || pending {
		t.Fatalf("removed installation kept budget=%v pending=%v", budget, pending)
	}
	if budget, pending := state(kept); !budget || !pending {
		t.Fatalf("retained installation lost budget=%v pending=%v", budget, pending)
	}
	runtime.Drop(kept)
	if budget, _ := state(kept); !budget {
		t.Fatal("Drop (restart) reset the installation's budget")
	}
}

func TestInteractionRateLimitRequeuesForNextTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newBotGatewayRuntime(t.Context(), nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer runtime.Close()
		installation, group := id.New(), id.New()
		var calls atomic.Int32
		retry := uint64(1000)
		runtime.SetInteractionPoller(installation, func(context.Context, id.ID) error {
			if calls.Add(1) == 1 {
				return &client.APIError{Status: 429, Code: "E_RATE_LIMITED", Outcome: client.OutcomeDefinitive, RetryAfterMS: &retry}
			}
			return nil
		})
		runtime.StartCommandWorkers()
		runtime.notifyInteraction(installation, group)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("initial calls=%d", calls.Load())
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("retried before Retry-After")
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("deferred command not resumed after Retry-After")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatal("delivered command retried again")
		}
	})
}

func TestInteractionRateLimitStopsBatchAndDefersFromCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newBotGatewayRuntime(t.Context(), nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer runtime.Close()
		installation := id.New()
		retry := uint64(1000)
		var calls atomic.Int32
		polled := make(chan time.Time, 4)
		runtime.SetInteractionPoller(installation, func(context.Context, id.ID) error {
			polled <- time.Now()
			if calls.Add(1) == 1 {
				time.Sleep(30 * time.Second)
				return &client.APIError{Status: 429, Code: "E_RATE_LIMITED", Outcome: client.OutcomeDefinitive, RetryAfterMS: &retry}
			}
			return nil
		})
		for range 3 {
			runtime.notifyInteraction(installation, id.New())
		}
		started := time.Now()
		runtime.StartCommandWorkers()
		synctest.Wait()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("rate limit did not stop the installation: %d calls", calls.Load())
		}
		time.Sleep(time.Second - time.Millisecond)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("Retry-After was measured from job start instead of completion")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		for range 2 {
			time.Sleep(250 * time.Millisecond)
			synctest.Wait()
		}
		if calls.Load() != 4 {
			t.Fatalf("deferred groups did not resume after completion: %d calls", calls.Load())
		}
		<-polled // the original failed attempt
		if resumed := (<-polled).Sub(started); resumed != 31*time.Second {
			t.Fatalf("deferred work resumed at %s, want 31s", resumed)
		}
	})
}

func TestInteractionRetryAfterDeferralCappedAtThirtySeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newBotGatewayRuntime(t.Context(), nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer runtime.Close()
		installation, group := id.New(), id.New()
		retry := ^uint64(0)
		var calls atomic.Int32
		runtime.SetInteractionPoller(installation, func(context.Context, id.ID) error {
			if calls.Add(1) == 1 {
				return &client.APIError{Status: 429, Code: "E_RATE_LIMITED", Outcome: client.OutcomeDefinitive, RetryAfterMS: &retry}
			}
			return nil
		})
		runtime.StartCommandWorkers()
		runtime.notifyInteraction(installation, group)
		synctest.Wait()
		time.Sleep(30*time.Second - time.Millisecond)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("retried before capped delay: %d calls", calls.Load())
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("did not retry after capped delay: %d calls", calls.Load())
		}
	})
}

func TestInteractionGroupBudgetExhaustionDoesNotSpendAnotherGroupsAllowance(t *testing.T) {
	runtime := newBotGatewayRuntime(t.Context(), nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	installation, flooded, quiet := id.New(), id.New(), id.New()
	runtime.SetInteractionPoller(installation, func(context.Context, id.ID) error { return nil })
	now := time.Now()
	runtime.budgetLocked(installation, flooded).spend(now, interactionBurst)
	runtime.notifyInteraction(installation, flooded)
	runtime.notifyInteraction(installation, quiet)
	pool := newCommandBatchTestPool(t)
	jobs, _, _ := runtime.commandBatch(pool, now)
	if len(jobs) != 1 || jobs[0].work.group != quiet {
		t.Fatalf("jobs=%v, want only quiet group", jobs)
	}
	releaseCommandBatch(pool, jobs)
	jobs, delay, pending := runtime.commandBatch(pool, now)
	if len(jobs) != 0 || !pending || delay != interactionRefill {
		t.Fatalf("flood refill: jobs=%v pending=%v delay=%s", jobs, pending, delay)
	}
	jobs, _, _ = runtime.commandBatch(pool, now.Add(interactionRefill))
	defer releaseCommandBatch(pool, jobs)
	if len(jobs) != 1 || jobs[0].work.group != flooded {
		t.Fatalf("refilled jobs=%v, want flooded group", jobs)
	}
}

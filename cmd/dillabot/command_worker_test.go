//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

// blockedSweep starts real HTTP background work with a ten-second response.
// Cleanup cancels it early, after the command has proved it did not wait for it.
func blockedSweep(t *testing.T, gateway *botGatewayRuntime) <-chan struct{} {
	t.Helper()
	entered, done := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-time.After(10 * time.Second):
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { cancel(); server.Close(); <-done })
	installation := id.New()
	gateway.SetTextPoller(installation, func(pollCtx context.Context) error {
		req, err := http.NewRequestWithContext(pollCtx, http.MethodGet, server.URL, nil)
		if err != nil {
			return err
		}
		resp, err := server.Client().Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	})
	go func() { defer close(done); gateway.PollText(ctx) }()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("background sweep never entered HTTP")
	}
	return done
}

func commandRuntime(t *testing.T) *botGatewayRuntime {
	t.Helper()
	r := newBotGatewayRuntime(t.Context(), nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.StartCommandWorkers()
	t.Cleanup(r.Close)
	return r
}

func TestCommandAnsweredWhileBackgroundSweepRuns(t *testing.T) {
	r := commandRuntime(t)
	sweepDone := blockedSweep(t, r)
	installation, group := id.New(), id.New()
	answered := make(chan time.Time, 1)
	replyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil || string(body) != "command-response" {
			t.Error("reply was not delivered intact")
		}
		w.WriteHeader(http.StatusNoContent)
		answered <- time.Now()
	}))
	defer replyServer.Close()
	event := native.GroupEvent{DeliveryID: 1, Group: group}
	handler := newInteractionEventHandler(
		interactionGroupInspectorFunc(func(context.Context, id.ID) (native.GroupInfo, error) {
			return native.GroupInfo{ID: group, Kind: 3}, nil
		}),
		interactionDispatcherFunc(func(context.Context, native.GroupEvent) (string, error) { return "command-response", nil }),
		interactionResponseSenderFunc(func(ctx context.Context, _ native.GroupEvent, response string) (bool, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, replyServer.URL, strings.NewReader(response))
			if err != nil {
				return false, err
			}
			resp, err := replyServer.Client().Do(req)
			if err != nil {
				return false, err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				return false, errors.New("reply refused")
			}
			return true, nil
		}))
	events := newInteractionEventRuntime(interactionPollerFunc(func(ctx context.Context, h service.GroupEventHandler) (service.GroupEventPollResult, error) {
		err := h.HandleGroupEvent(ctx, event)
		return service.GroupEventPollResult{Handled: 1}, err
	}), handler)
	var steps []string
	poll := newInteractionIntakePoller(namedGroupSync{steps: &steps}, readinessTrust{steps: &steps}, nil, events, noUnrefreshed(t))
	completed := make(chan error, 1)
	r.SetInteractionPoller(installation, func(ctx context.Context, group id.ID) error {
		if client.RequestPriority(ctx) < 1 {
			t.Error("command lost interactive priority")
		}
		err := poll(ctx, group)
		completed <- err
		return err
	})
	started := time.Now()
	r.notifyInteraction(installation, group)
	select {
	case when := <-answered:
		latency := when.Sub(started)
		t.Logf("command reply latency during blocked HTTP sweep: %v", latency)
		if latency >= time.Second {
			t.Fatalf("command reply latency %v >=1s", latency)
		}
	case <-time.After(time.Second):
		t.Fatal("command waited for the blocked background sweep")
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reply did not complete")
	}

	select {
	case <-sweepDone:
		t.Fatal("background sweep finished before the command reply")
	default:
	}
}

func TestCommandWorkerPerInstallationOrderAndGlobalCap(t *testing.T) {
	r := commandRuntime(t)
	const count = 6
	entered := make(chan int, count)
	release := make(chan struct{})
	defer close(release)
	var active, maximum atomic.Int32
	for i := range count {
		installation, group := id.New(), id.New()
		r.SetInteractionPoller(installation, func(ctx context.Context, _ id.ID) error {
			current := active.Add(1)
			defer active.Add(-1)
			for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
			}
			entered <- i
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		})
		r.notifyInteraction(installation, group)
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("independent installations did not run concurrently")
		}
	}
	select {
	case <-entered:
		t.Fatal("global command concurrency exceeded four")
	case <-time.After(30 * time.Millisecond):
	}
	if maximum.Load() != 4 {
		t.Fatalf("maximum concurrency=%d want4", maximum.Load())
	}
}

func TestCommandWorkerSameGroupStaysOrdered(t *testing.T) {
	r := commandRuntime(t)
	installation, group := id.New(), id.New()
	entered := make(chan int, 2)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var calls atomic.Int32
	r.SetInteractionPoller(installation, func(ctx context.Context, _ id.ID) error {
		n := int(calls.Add(1))
		entered <- n
		if n == 1 {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil
	})
	r.notifyInteraction(installation, group)
	select {
	case n := <-entered:
		if n != 1 {
			t.Fatal(n)
		}
	case <-time.After(time.Second):
		t.Fatal("first command did not enter")
	}
	r.notifyInteraction(installation, group)
	select {
	case <-entered:
		t.Fatal("same-group commands overlapped")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)

	select {
	case n := <-entered:
		if n != 2 {
			t.Fatal(n)
		}
	case <-time.After(time.Second):
		t.Fatal("pending same-group command lost")
	}
}

func TestCommandWorkerConcurrentBackgroundStressAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	r := newBotGatewayRuntime(ctx, nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.StartCommandWorkers()
	var answered atomic.Int32
	ids := []id.ID{id.New(), id.New(), id.New(), id.New()}
	var producers sync.WaitGroup
	for _, installation := range ids {
		r.SetInteractionPoller(installation, func(ctx context.Context, _ id.ID) error { answered.Add(1); return ctx.Err() })
		group := id.New()
		producers.Go(func() {
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					r.notifyInteraction(installation, group)
					r.SetTextPoller(installation, func(context.Context) error { return nil })
					r.PollText(ctx)
				}
			}
		})
	}
	time.Sleep(2 * time.Second)
	cancel()
	producers.Wait()
	done := make(chan struct{})
	go func() { r.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("command workers leaked after shutdown")
	}
	if answered.Load() == 0 {
		t.Fatal("no commands executed during stress")
	}
}

func TestCommandWorkerShutdownDoesNotLeakGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	for range 8 {
		ctx, cancel := context.WithCancel(t.Context())
		r := newBotGatewayRuntime(ctx, nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
		r.StartCommandWorkers()
		cancel()
		r.Close()
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before+1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Fatalf("goroutines after shutdown=%d before=%d", after, before)
	}
}

func TestMaintenanceProgressWhileCommandWorkerBlocked(t *testing.T) {
	r := commandRuntime(t)
	installation, group := id.New(), id.New()
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	r.SetInteractionPoller(installation, func(context.Context, id.ID) error { close(entered); <-release; return nil })
	r.notifyInteraction(installation, group)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("command did not enter")
	}
	var due atomic.Int32
	r.SetMaintenance(installation, backgroundMaintenance{run: func(ctx context.Context) error {
		return runBackgroundMaintenance(ctx, func(context.Context) error { due.Add(1); return nil })
	}})
	done := make(chan error, 1)
	go func() { done <- r.maintain(t.Context(), map[id.ID]bool{}) }()
	select {
	case err := <-done:
		if err != nil || due.Load() != 1 {
			t.Fatalf("RunDue=%d err=%v", due.Load(), err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance waited for command worker")
	}
}

type boundedCommandSync struct {
	calls atomic.Int32
	more  bool
}

func (s *boundedCommandSync) ReconcileInteractionGroupOnce(context.Context, *client.Session, id.ID) (service.MembershipResult, error) {
	return service.MembershipResult{More: s.calls.Add(1) == 1 && s.more}, nil
}
func TestCommandWorkerRequeuesBoundedSourceAndEventCatchup(t *testing.T) {
	for _, sourceMore := range []bool{true, false} {
		t.Run(map[bool]string{true: "source", false: "events"}[sourceMore], func(t *testing.T) {
			r := commandRuntime(t)
			installation, group := id.New(), id.New()
			if !sourceMore {
				group = id.ID{}
			}
			source := &boundedCommandSync{more: sourceMore}
			var drains atomic.Int32
			done := make(chan struct{}, 2)
			var steps []string
			poll := newInteractionIntakePoller(source, readinessTrust{steps: &steps}, nil, commandDrainFunc(func(context.Context) (service.GroupEventPollResult, error) {
				n := drains.Add(1)
				done <- struct{}{}
				return service.GroupEventPollResult{More: !sourceMore && n == 1}, nil
			}), noUnrefreshed(t))
			r.SetInteractionPoller(installation, poll)
			r.notifyInteraction(installation, group)
			for range 2 {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("bounded command continuation was lost")
				}
			}
		})
	}
}

func TestCommandWorkerRemovalPrunesRetryAndPendingWork(t *testing.T) {
	r := commandRuntime(t)
	installation, group := id.New(), id.New()
	r.SetInteractionPoller(installation, func(context.Context, id.ID) error { return errCommandWorkRemaining })
	r.notifyInteraction(installation, group)
	deadline := time.Now().Add(time.Second)
	for {
		r.pendingMu.Lock()
		_, retried := r.commands.retry[interactionWork{installation, group}]
		r.pendingMu.Unlock()
		if retried {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command never reached retry state")
		}
		time.Sleep(time.Millisecond)
	}
	r.RetainOnly(nil)
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	if len(r.commands.retry) != 0 || len(r.pending) != 0 || len(r.pendingOrder) != 0 {
		t.Fatalf("removed installation retained retries=%d pending=%d queued=%d", len(r.commands.retry), len(r.pending), len(r.pendingOrder))
	}
}

func TestGatewayCommandHintPrecedesPeriodicCatchup(t *testing.T) {
	r := commandRuntime(t)
	installation, first, background, command := id.New(), id.New(), id.New(), id.New()
	entered := make(chan id.ID, 4)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	r.SetInteractionPoller(installation, func(ctx context.Context, group id.ID) error {
		entered <- group
		if group == first {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil
	})
	r.notifyInteraction(installation, first)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first turn never entered")
	}
	r.notifyBackgroundInteraction(installation, background)
	r.notifyBackgroundInteraction(installation, command)
	r.notifyInteraction(installation, command) // promote this group's existing catch-up hint
	close(release)
	select {
	case group := <-entered:
		if group != command {
			t.Fatalf("periodic catch-up ran before command: %s", group)
		}
	case <-time.After(time.Second):
		t.Fatal("command never entered")
	}
}

type priorityCheckingIntake struct {
	t    *testing.T
	want int
}

func (p priorityCheckingIntake) check(ctx context.Context) {
	p.t.Helper()
	if got := client.RequestPriority(ctx); got != p.want {
		p.t.Errorf("intake priority = %d, want %d", got, p.want)
	}
}
func (p priorityCheckingIntake) ReconcileInteractionGroupOnce(ctx context.Context, _ *client.Session, _ id.ID) (service.MembershipResult, error) {
	p.check(ctx)
	return service.MembershipResult{}, nil
}
func (p priorityCheckingIntake) RefreshPendingLeaves(ctx context.Context, _ *client.Session) (service.TrustRefresh, error) {
	p.check(ctx)
	return service.TrustRefresh{}, nil
}
func (p priorityCheckingIntake) DrainHolding(ctx context.Context, _ map[id.ID]bool) (service.GroupEventPollResult, error) {
	p.check(ctx)
	return service.GroupEventPollResult{}, nil
}

func TestCommandWorkerPreservesCommandAndCatchupPriority(t *testing.T) {
	r := commandRuntime(t)
	for _, background := range []bool{false, true} {
		installation, group := id.New(), id.New()
		want := 1
		if background {
			want = 0
		}
		probe := priorityCheckingIntake{t: t, want: want}
		poll := newInteractionIntakePoller(probe, probe, nil, probe, noUnrefreshed(t))
		done := make(chan error, 1)
		r.SetInteractionPoller(installation, func(ctx context.Context, group id.ID) error {
			err := poll(ctx, group)
			done <- err
			return err
		})
		if background {
			r.notifyBackgroundInteraction(installation, group)
		} else {
			r.notifyInteraction(installation, group)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("intake never ran")
		}
	}
}

func TestGatewayCommandCooperativelyStopsPeriodicCatchup(t *testing.T) {
	r := commandRuntime(t)
	installation, catchup, command := id.New(), id.New(), id.New()
	blocked, answered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.SetInteractionPoller(installation, func(ctx context.Context, group id.ID) error {
		if group == catchup {
			once.Do(func() { close(blocked) })
			for {
				if err := client.CooperativeCheckpoint(ctx); err != nil {
					return err
				}
				time.Sleep(time.Millisecond)
			}
		}
		close(answered)
		return nil
	})
	r.notifyBackgroundInteraction(installation, catchup)
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("periodic catch-up never started")
	}
	r.notifyInteraction(installation, command)
	select {
	case <-answered:
	case <-time.After(time.Second):
		t.Fatal("command waited behind periodic catch-up")
	}
}

func TestCommandWorkerShutdownBoundsUnresponsiveTransportGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		r := newBotGatewayRuntime(ctx, nil, true, nil)
		r.StartCommandWorkers()
		installation, group := id.New(), id.New()
		entered, closed := make(chan struct{}), make(chan struct{})
		r.SetInteractionPoller(installation, func(ctx context.Context, _ id.ID) error {
			close(entered)
			<-ctx.Done() // models a transport which never returns on its own
			return ctx.Err()
		})
		r.notifyInteraction(installation, group)
		<-entered
		cancel()
		go func() { r.Close(); close(closed) }()
		synctest.Wait()
		time.Sleep(commandRetirementGrace - time.Millisecond)
		select {
		case <-closed:
			t.Fatal("transport cancelled before grace elapsed")
		default:
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		select {
		case <-closed:
		default:
			t.Fatal("unresponsive transport outlived retirement grace")
		}
	})
}

func TestRetiredCommandDoesNotRequeueIntoReplacement(t *testing.T) {
	r := newBotGatewayRuntime(t.Context(), nil, true, nil)
	pool := newCommandBatchTestPool(t)
	installation, group := id.New(), id.New()
	r.SetInteractionPoller(installation, func(context.Context, id.ID) error { return nil })
	r.notifyInteraction(installation, group)
	jobs, _, _ := r.commandBatch(pool, time.Now())
	if len(jobs) != 1 {
		t.Fatalf("jobs=%d", len(jobs))
	}
	job := jobs[0]
	job.turn.retire()
	job.cancel()
	r.SetInteractionPoller(installation, func(context.Context, id.ID) error { return nil })
	r.finishCommandJob(pool, job, errors.New("old installation failed"), time.Now())
	if len(r.pendingOrder) != 0 {
		t.Fatal("retired command queued against replacement")
	}
}

func TestCommandWorkerSerializesDifferentGroupsWithinInstallation(t *testing.T) {
	r := commandRuntime(t)
	installation, first, second := id.New(), id.New(), id.New()
	entered := make(chan id.ID, 2)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	r.SetInteractionPoller(installation, func(_ context.Context, group id.ID) error {
		entered <- group
		if group == first {
			<-release
		}
		return nil
	})
	r.notifyInteraction(installation, first)
	select {
	case group := <-entered:
		if group != first {
			t.Fatal(group)
		}
	case <-time.After(time.Second):
		t.Fatal("first group did not start")
	}
	r.notifyInteraction(installation, second)
	select {
	case <-entered:
		t.Fatal("shared installation cursor used concurrently")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case group := <-entered:
		if group != second {
			t.Fatal(group)
		}
	case <-time.After(time.Second):
		t.Fatal("second group did not start")
	}
}

func TestBackgroundCatchupDeduplicatesPendingAndRunningGroups(t *testing.T) {
	r := newBotGatewayRuntime(t.Context(), nil, true, nil)
	pool := newCommandBatchTestPool(t)
	r.commands = pool
	installation, first, second := id.New(), id.New(), id.New()
	r.SetInteractionPoller(installation, func(context.Context, id.ID) error { return nil })
	for range 50 {
		r.notifyBackgroundInteraction(installation, first)
	}
	if len(r.pendingOrder) != 1 {
		t.Fatalf("pending catchups=%d, want1", len(r.pendingOrder))
	}
	now := time.Now()
	jobs, _, _ := r.commandBatch(pool, now)
	if len(jobs) != 1 {
		t.Fatalf("jobs=%d", len(jobs))
	}
	for range 50 {
		r.notifyBackgroundInteraction(installation, first)
		r.notifyBackgroundInteraction(installation, second)
	}
	if len(r.pendingOrder) != 1 || r.pendingOrder[0].group != second {
		t.Fatalf("queued catchups=%v, want only second group", r.pendingOrder)
	}
	jobs[0].cancel()
	r.finishCommandJob(pool, jobs[0], nil, now)
	jobs, _, _ = r.commandBatch(pool, now.Add(interactionSpacing))
	if len(jobs) != 1 || jobs[0].work.group != second {
		t.Fatalf("next jobs=%v", jobs)
	}
	jobs[0].cancel()
	r.finishCommandJob(pool, jobs[0], nil, now.Add(interactionSpacing))
}

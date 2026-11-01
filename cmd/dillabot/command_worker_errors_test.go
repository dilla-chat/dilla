//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

type commandWorkerLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *commandWorkerLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *commandWorkerLogBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	data := append([]byte(nil), b.buf.Bytes()...)
	b.mu.Unlock()
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("invalid structured command log: %v", err)
		}
		records = append(records, record)
	}
	return records
}

func TestCommandWorkerFailureRetriesBackOffToThirtySeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newBotGatewayRuntime(t.Context(), nil, false, nil)
		defer r.Close()
		installation, group := id.New(), id.New()
		var calls atomic.Int32
		r.SetInteractionPoller(installation, func(context.Context, id.ID) error {
			calls.Add(1)
			return errors.New("source temporarily unavailable")
		})
		r.StartCommandWorkers()
		r.notifyInteraction(installation, group)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("initial attempts=%d, want 1", calls.Load())
		}
		for i, delay := range []time.Duration{
			500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second,
			8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second,
		} {
			before := int32(i + 1)
			time.Sleep(delay - time.Nanosecond)
			synctest.Wait()
			if got := calls.Load(); got != before {
				t.Fatalf("retry %d ran before %s: attempts=%d, want %d", i+1, delay, got, before)
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			if got := calls.Load(); got != before+1 {
				t.Fatalf("retry %d did not run after %s: attempts=%d, want %d", i+1, delay, got, before+1)
			}
		}
	})
}

func TestCommandWorkerSuppressesRepeatedWarningsWithoutDroppingRetries(t *testing.T) {
	var logs commandWorkerLogBuffer
	r := newBotGatewayRuntime(t.Context(), nil, false, slog.New(slog.NewJSONHandler(&logs, nil)))
	installation, group := id.New(), id.New()
	r.SetInteractionPoller(installation, func(context.Context, id.ID) error { return nil })
	pool := newCommandBatchTestPool(t)
	job := commandJob{work: interactionWork{installation, group}, version: 1}
	first, distinct := errors.New("event fetch failed"), errors.New("event acknowledgement failed")
	start := time.Now()
	for _, step := range []struct {
		at    time.Duration
		err   error
		delay time.Duration
	}{
		{0, first, 500 * time.Millisecond},
		{250 * time.Millisecond, first, time.Second},
		{250 * time.Millisecond, distinct, 2 * time.Second},
		{500 * time.Millisecond, first, 4 * time.Second},
	} {
		now := start.Add(step.at)
		r.finishCommandJob(pool, job, step.err, now)
		if got := pool.retry[job.work].Sub(now); got != step.delay {
			t.Fatalf("warning suppression changed retry delay: got %s, want %s", got, step.delay)
		}
		if !r.pending[installation][group] {
			t.Fatal("warning suppression dropped pending command work")
		}
	}
	records := logs.records(t)
	if len(records) != 3 {
		t.Fatalf("warnings=%d, want the first failure, distinct failure, and later first failure", len(records))
	}
	for i, message := range []string{first.Error(), distinct.Error(), first.Error()} {
		if record := records[i]; record["level"] != "WARN" || record["error"] != message || record["installation_id"] != installation.String() || record["group_id"] != group.String() {
			t.Fatalf("warning %d lost failure identity or scope: %v", i, record)
		}
	}
}

func TestCommandWorkerRateLimitLogsInfoAndRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs commandWorkerLogBuffer
		r := newBotGatewayRuntime(t.Context(), nil, false, slog.New(slog.NewJSONHandler(&logs, nil)))
		defer r.Close()
		installation, group := id.New(), id.New()
		var calls atomic.Int32
		retry := uint64(750)
		r.SetInteractionPoller(installation, func(context.Context, id.ID) error {
			if calls.Add(1) == 1 {
				return &client.APIError{Status: 429, Code: "E_RATE_LIMITED", Outcome: client.OutcomeDefinitive, RetryAfterMS: &retry}
			}
			return nil
		})
		r.StartCommandWorkers()
		r.notifyInteraction(installation, group)
		synctest.Wait()
		records := logs.records(t)
		if len(records) != 1 || records[0]["level"] != "INFO" || records[0]["installation_id"] != installation.String() || records[0]["group_id"] != group.String() || records[0]["retry_after"] != float64(750*time.Millisecond) {
			t.Fatalf("rate refusal log=%v, want one scoped Info with Retry-After", records)
		}
		time.Sleep(750 * time.Millisecond)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("rate-refused command attempts=%d, want successful retry", calls.Load())
		}
		if got := len(logs.records(t)); got != 1 {
			t.Fatalf("successful retry added %d unexpected logs", got-1)
		}
	})
}

func TestCommandWorkerNativeLeaseFailureWaitsForPollerRepublish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newMusicQueueRegistry()
		defer func() { _ = registry.Close() }()
		installation, community, group := id.New(), id.New(), id.New()
		n := &runtimeStateNative{binding: native.Binding{Instance: id.New(), Community: community, Installation: installation}, rows: make(map[string]native.StateValue)}
		var dispatched atomic.Int32
		retained := retainedInteractionPoller(registry, installation, community, n, func(context.Context, id.ID) error {
			dispatched.Add(1)
			return nil
		})
		err := retained(t.Context(), group)
		var leaseError *nativeLeaseUnavailableError
		if !errors.As(err, &leaseError) || !errors.Is(err, errNativeLeaseUnavailable) || errors.Is(err, context.Canceled) {
			t.Fatalf("unavailable lease error=%v, want typed generation failure", err)
		}
		var logs commandWorkerLogBuffer
		r := newBotGatewayRuntime(t.Context(), nil, false, slog.New(slog.NewJSONHandler(&logs, nil)))
		defer r.Close()
		var attempts atomic.Int32
		poll := func(ctx context.Context, group id.ID) error {
			attempts.Add(1)
			return retained(ctx, group)
		}
		r.SetInteractionPoller(installation, poll)
		r.StartCommandWorkers()
		r.notifyInteraction(installation, group)
		synctest.Wait()
		for range 3 {
			time.Sleep(30 * time.Second)
			r.notifyInteraction(installation, group)
			synctest.Wait()
		}
		if attempts.Load() != 1 || dispatched.Load() != 0 {
			t.Fatalf("unavailable generation retried: attempts=%d dispatched=%d", attempts.Load(), dispatched.Load())
		}
		if records := logs.records(t); len(records) != 1 || records[0]["level"] != "WARN" {
			t.Fatalf("lease failure logs=%v, want one warning while suspended", records)
		}
		if err := registry.RetainInteractions(installation, community, n); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if attempts.Load() != 1 {
			t.Fatal("restoring the helper resumed a callback before poller publication")
		}
		r.SetInteractionPoller(installation, poll)
		synctest.Wait()
		if attempts.Load() != 2 || dispatched.Load() != 1 {
			t.Fatalf("republished generation did not resume pending work: attempts=%d dispatched=%d", attempts.Load(), dispatched.Load())
		}
	})
}

func TestCommandWorkerRecoversPanicLogsStackAndContinues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs commandWorkerLogBuffer
		r := newBotGatewayRuntime(t.Context(), nil, false, slog.New(slog.NewJSONHandler(&logs, nil)))
		defer r.Close()
		installation, group, nextGroup := id.New(), id.New(), id.New()
		var calls atomic.Int32
		answered := make(chan id.ID, 2)
		r.SetInteractionPoller(installation, func(_ context.Context, group id.ID) error {
			if calls.Add(1) == 1 {
				panic("decoder invariant")
			}
			answered <- group
			return nil
		})
		r.StartCommandWorkers()
		r.notifyInteraction(installation, group)
		synctest.Wait()
		records := logs.records(t)
		if len(records) != 1 || records[0]["level"] != "WARN" || records[0]["installation_id"] != installation.String() || records[0]["group_id"] != group.String() || !strings.Contains(records[0]["error"].(string), "decoder invariant") {
			t.Fatalf("recovered panic log=%v", records)
		}
		stack, _ := records[0]["stack"].(string)
		if !strings.Contains(stack, "runCommandJob") || !strings.Contains(stack, "TestCommandWorkerRecoversPanicLogsStackAndContinues") {
			t.Fatalf("panic log omitted its handler stack: %q", stack)
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if calls.Load() != 2 || <-answered != group {
			t.Fatal("worker did not retry the command after recovering the panic")
		}
		r.notifyInteraction(installation, nextGroup)
		time.Sleep(250 * time.Millisecond)
		synctest.Wait()
		if calls.Load() != 3 || <-answered != nextGroup {
			t.Fatal("worker did not continue with the next group")
		}
	})
}

func TestCommandWorkerLogsRecoveredPanicDuringShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs commandWorkerLogBuffer
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r := newBotGatewayRuntime(ctx, nil, false, slog.New(slog.NewJSONHandler(&logs, nil)))
		installation, group := id.New(), id.New()
		entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
		r.SetInteractionPoller(installation, func(context.Context, id.ID) error {
			close(entered)
			<-release
			panic("shutdown handler invariant")
		})
		r.StartCommandWorkers()
		r.notifyInteraction(installation, group)
		<-entered
		cancel()
		go func() { r.Close(); close(closed) }()
		synctest.Wait()
		close(release)
		<-closed
		records := logs.records(t)
		if len(records) != 1 || records[0]["level"] != "WARN" || records[0]["installation_id"] != installation.String() || records[0]["group_id"] != group.String() {
			t.Fatalf("shutdown panic logs=%v, want one scoped warning", records)
		}
		stack, _ := records[0]["stack"].(string)
		if !strings.Contains(stack, "TestCommandWorkerLogsRecoveredPanicDuringShutdown") {
			t.Fatalf("shutdown panic stack=%q", stack)
		}
	})
}

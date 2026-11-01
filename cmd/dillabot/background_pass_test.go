//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

func backgroundAPI(t *testing.T, active ...id.ID) *client.Client {
	t.Helper()
	instance := id.New()
	document, err := cborx.Marshal([]any{[]any{uint64(3)}, []any{uint64(1)}, []any{uint64(1)}, instance, uint64(1), "probe", uint64(0), []any{uint64(0)}, uint64(1), id.New(), make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]any, 0, len(active))
	for _, installation := range active {
		rows = append(rows, []any{installation, id.New(), uint64(3), uint64(1), id.New(), id.New(), uint64(1), uint64(1), uint64(0), uint64(0), nil, uint64(100), uint64(5)})
	}
	inventory, err := cborx.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := cborx.Marshal([]any{"E_RATE_LIMITED", "try later", nil})
	if err != nil {
		t.Fatal(err)
	}
	api, err := client.New(client.Config{BaseURL: "https://example.invalid", InstanceID: instance,
		RateLimits: client.RateLimits{ReadPerSecond: 1e6, ReadBurst: 1000000},
		HTTPClient: &http.Client{Transport: spotifyCatalogRoundTripper(func(r *http.Request) (*http.Response, error) {
			body, status := document, http.StatusOK
			if r.URL.Path == "/v1/bot-runtime/installations" {
				body = inventory
			} else if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/status") {
				// Defer dependency rebuilding after retained maintenance has run.
				body, status = deferred, http.StatusTooManyRequests
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/cbor"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
		})}})
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func backgroundProbe(t *testing.T) func(context.Context) error {
	t.Helper()
	api := backgroundAPI(t)
	return func(ctx context.Context) error { _, err := api.Discover(ctx); return err }
}

func TestMaintenanceRunsWithCommandsEveryFiveSeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		installation := id.New()
		var announcements, utilities []time.Time
		// These are the same two tasks bound to scheduler.RunDue and utilities.maintain
		// in reconcile. Real admission must keep running while a worker handles a command.
		probe := backgroundProbe(t)
		task := func(times *[]time.Time) func(context.Context) error {
			return func(ctx context.Context) error {
				if err := probe(ctx); err != nil {
					return err
				}
				*times = append(*times, time.Now())
				return nil
			}
		}
		api := backgroundAPI(t, installation)
		if _, err := api.Discover(t.Context()); err != nil {
			t.Fatal(err)
		}
		runtime, err := api.Runtime("runtime-credential")
		if err != nil {
			t.Fatal(err)
		}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		r := newBotGatewayRuntime(t.Context(), api, false, logger)
		registry := newMusicQueueRegistry()
		defer func() { _ = registry.Close() }()
		cfg := config{StateDir: t.TempDir()}
		statuses := newRuntimeStatusTracker("test")
		r.maintenance = map[id.ID]backgroundMaintenance{installation: {run: func(ctx context.Context) error {
			return runBackgroundMaintenance(ctx, task(&announcements), task(&utilities))
		}}}
		commandStarted, releaseCommand := make(chan struct{}), make(chan struct{})
		r.SetInteractionPoller(installation, func(context.Context, id.ID) error {
			select {
			case <-commandStarted:
			default:
				close(commandStarted)
			}
			<-releaseCommand
			return nil
		})
		r.StartCommandWorkers()
		defer r.Close()
		defer close(releaseCommand)
		r.notifyInteraction(installation, id.New())
		<-commandStarted
		for elapsed := time.Duration(0); elapsed <= 125*time.Second; elapsed += 5 * time.Second {
			// New hints stay pending while the command worker is occupied.
			r.notifyInteraction(installation, id.New())
			if err := reconcile(t.Context(), cfg, api, runtime, nil, registry, newAutoModerationEventState(), nil, r, statuses, logger); err != nil {
				t.Fatalf("reconciliation stopped during a command: %v", err)
			}
			time.Sleep(5 * time.Second)
		}
		for _, times := range [][]time.Time{announcements, utilities} {
			if len(times) != 26 {
				t.Fatalf("maintenance did not run each pass: %d runs", len(times))
			}
			last := now
			for _, at := range times {
				if at.Sub(last) > 60*time.Second {
					t.Fatalf("maintenance gap %s", at.Sub(last))
				}
				last = at
			}
		}
	})
}

func TestRemovedInstallationDoesNotRunMaintenance(t *testing.T) {
	api := backgroundAPI(t)
	if _, err := api.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	runtime, err := api.Runtime("runtime-credential")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := newBotGatewayRuntime(t.Context(), api, false, logger)
	installation := id.New()
	r.SetInteractionPoller(installation, func(context.Context, id.ID) error { return nil })
	r.notifyInteraction(installation, id.New())
	runs := 0
	task := func(ctx context.Context) error {
		if _, err := api.Discover(ctx); err != nil {
			return err
		}
		runs++
		return nil
	}
	r.maintenance = map[id.ID]backgroundMaintenance{installation: {run: func(ctx context.Context) error { return runBackgroundMaintenance(ctx, task, task) }}}
	registry := newMusicQueueRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	err = reconcile(t.Context(), config{StateDir: t.TempDir()}, api, runtime, nil, registry, newAutoModerationEventState(), nil, r, newRuntimeStatusTracker("test"), logger)
	if runs != 0 {
		t.Fatalf("removed installation ran RunDue/maintain %d times", runs)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := r.maintenance[installation]; exists {
		t.Fatal("removed installation retained maintenance")
	}
}

func TestStaleMaintenanceDoesNotSkipRecoveredRuntime(t *testing.T) {
	r := newBotGatewayRuntime(t.Context(), nil, false, nil)
	installation := id.New()
	probe := backgroundProbe(t)
	live := false
	runs := 0
	task := func(ctx context.Context) error {
		if err := probe(ctx); err != nil {
			return err
		}
		runs++
		return nil
	}
	r.maintenance = map[id.ID]backgroundMaintenance{installation: {current: func() bool { return live }, run: func(ctx context.Context) error { return runBackgroundMaintenance(ctx, task, task) }}}
	done := make(map[id.ID]bool)
	if err := r.maintainOne(t.Context(), installation, done); err != nil {
		t.Fatal(err)
	}
	live = true
	r.maintenance[installation] = backgroundMaintenance{run: func(ctx context.Context) error { return runBackgroundMaintenance(ctx, task, task) }}
	if err := r.maintainOne(t.Context(), installation, done); err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Fatalf("recovered helper skipped RunDue/maintain: %d runs", runs)
	}
}

func TestMaintenanceErrorsCarryInstallationID(t *testing.T) {
	r := newBotGatewayRuntime(t.Context(), nil, false, nil)
	installation := id.New()
	failure := errors.New("announcement publication failed")
	r.maintenance = map[id.ID]backgroundMaintenance{installation: {run: func(context.Context) error { return failure }}}
	err := r.maintain(t.Context(), make(map[id.ID]bool))
	var scoped *installationMaintenanceError
	if !errors.As(err, &scoped) || scoped.installation != installation || !errors.Is(err, failure) || !strings.Contains(err.Error(), installation.String()) {
		t.Fatalf("maintenance error lost installation or cause: %v", err)
	}
}

func TestMaintenanceRunsOncePerPassAfterTaskFailure(t *testing.T) {
	r := newBotGatewayRuntime(t.Context(), nil, false, nil)
	installation := id.New()
	runs := 0
	failure := errors.New("announcement publication failed")
	r.SetMaintenance(installation, backgroundMaintenance{run: func(context.Context) error {
		runs++
		return failure
	}})
	for range 2 {
		done := make(map[id.ID]bool)
		if err := r.maintain(t.Context(), done); !errors.Is(err, failure) {
			t.Fatalf("maintenance error=%v", err)
		}
		// Rebinding during this pass must not run a retained task twice.
		if err := r.maintainOne(t.Context(), installation, done); err != nil {
			t.Fatal(err)
		}
	}
	if runs != 2 {
		t.Fatalf("maintenance ran %d times in two passes, want 2", runs)
	}
}

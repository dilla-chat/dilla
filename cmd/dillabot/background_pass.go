//go:build linux

package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/id"
)

func reportBackgroundPass(logger *slog.Logger, err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("bot identity reconciliation degraded", "error", err.Error())
	} else if err == nil {
		logger.Info("bot identity reconciliation healthy")
	}
}

// Maintenance runs independently of command workers. Tasks are retained without
// their request contexts and run once per reconciliation pass.
func runBackgroundMaintenance(ctx context.Context, tasks ...func(context.Context) error) error {
	var result error
	for _, task := range tasks {
		if task != nil {
			result = errors.Join(result, task(ctx))
		}
	}
	return result
}

type backgroundMaintenance struct {
	current func() bool
	run     func(context.Context) error
}

// installationMaintenanceError keeps the installation visible in both logs
// and error trees while preserving the original retry/refusal classification.
type installationMaintenanceError struct {
	installation id.ID
	cause        error
}

func (e *installationMaintenanceError) Error() string {
	return "bot maintenance failed for installation " + e.installation.String() + ": " + e.cause.Error()
}
func (e *installationMaintenanceError) Unwrap() error { return e.cause }

func (r *botGatewayRuntime) maintainOne(ctx context.Context, installation id.ID, done map[id.ID]bool) error {
	r.mu.Lock()
	task, exists := r.maintenance[installation]
	r.mu.Unlock()
	if done[installation] || !exists {
		return nil
	}
	if task.current != nil && !task.current() {
		r.SetMaintenance(installation, backgroundMaintenance{})
		return nil
	}
	done[installation] = true
	if err := runBackgroundMaintenance(ctx, task.run); err != nil {
		if errors.Is(err, helper.ErrClosed) {
			// The native helper is gone (it exited, or was stopped after a call whose outcome is unknown). Bot v2 has
			// no reconcile interval, so without this nothing would ever start another: v0.1.0-bot2 on an upgraded
			// instance logged "helper client is closed" every 10 s for good. The pass replaces the unusable client
			// (retainedNativeClient) with a new helper.
			r.requestPass(installation)
		}
		return &installationMaintenanceError{installation: installation, cause: err}
	}
	return nil
}

func (r *botGatewayRuntime) maintain(ctx context.Context, done map[id.ID]bool) error {
	var result error
	r.mu.Lock()
	installations := make([]id.ID, 0, len(r.maintenance))
	for installation := range r.maintenance {
		installations = append(installations, installation)
	}
	r.mu.Unlock()
	for _, installation := range installations {
		result = errors.Join(result, r.maintainOne(ctx, installation, done))
	}
	return result
}

// SetMaintenance publishes immutable task closures without retaining a request context.
func (r *botGatewayRuntime) SetMaintenance(installation id.ID, task backgroundMaintenance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if task.run == nil {
		delete(r.maintenance, installation)
		return
	}
	if r.maintenance == nil {
		r.maintenance = make(map[id.ID]backgroundMaintenance)
	}
	r.maintenance[installation] = task
}

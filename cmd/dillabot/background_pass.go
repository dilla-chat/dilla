//go:build linux

package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

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
	// usable reports whether the task's helper can still serve calls (native.Client.Usable); nil: always.
	usable func() bool
	// client is the helper the task runs on (a comparable pointer, *native.Client in production): the due tick logs a
	// helper gone once per client, and the respawn backoff ends only once one client stayed usable for helperServedFor.
	client any
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

// helperGone reports whether a maintenance run found its helper unable to serve: the call met a closed helper, or one
// whose outcome is unknown (the client stops the helper after such a call), or the client was invalidated.
func helperGone(task backgroundMaintenance, err error) bool {
	if errors.Is(err, helper.ErrClosed) || errors.Is(err, helper.ErrOutcomeUnknown) {
		return true
	}
	return task.usable != nil && !task.usable()
}

// maintenanceCaller is who runs an installation's maintenance; it decides what a helper that is gone means.
type maintenanceCaller int

const (
	// maintenanceInPass is the run inside the installation's own pass, on the helper the pass just bound: a helper gone
	// there fails the installation's pass, which the loop retries on its own backoff (instFail).
	maintenanceInPass maintenanceCaller = iota
	// maintenancePassStart is the run at the start of every pass (reconcile), before the pass replaces unusable
	// helpers: a helper gone there asks for nothing and is not an error of the pass, and the installation's own run
	// later in the pass, on its replacement, is not skipped (fix round, I-1).
	maintenancePassStart
	// maintenanceDue is the due-work tick: a helper gone there asks for a pass of its installation, which replaces it,
	// on the installation's respawn backoff (requestPassAfterHelperGone).
	maintenanceDue
)

func (r *botGatewayRuntime) maintainOne(ctx context.Context, installation id.ID, done map[id.ID]bool) error {
	return r.maintainAs(ctx, installation, done, maintenanceInPass)
}

func (r *botGatewayRuntime) maintainAs(ctx context.Context, installation id.ID, done map[id.ID]bool, caller maintenanceCaller) error {
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
	// v0.1.0-bot2.2 (F8): a helper already known to be unusable is not called; before, every due tick ran the
	// maintenance against the closed client.
	var err error
	if task.usable == nil || task.usable() {
		err = runBackgroundMaintenance(ctx, task.run)
		if ctx.Err() != nil {
			// A cancelled run says nothing about the helper.
			if err != nil {
				return &installationMaintenanceError{installation: installation, cause: err}
			}
			return nil
		}
	}
	if !helperGone(task, err) {
		// The helper served this run: one that keeps serving for helperServedFor ends the installation's respawn backoff.
		r.helperServed(installation, task.client)
		if err != nil {
			return &installationMaintenanceError{installation: installation, cause: err}
		}
		return nil
	}
	r.helperGoneSeen(installation, task.client, err)
	switch caller {
	case maintenancePassStart:
		// The pass replaces the helper (retainedNativeClient) when it covers the installation, and the replacement's own
		// run later in the pass must not be skipped. A helper found gone here is not a failure of the pass as a whole:
		// before, it made the pass "failed as a whole" and queued another pass from inside the replacing one.
		delete(done, installation)
		return nil
	case maintenanceDue:
		// The native helper is gone (it exited, or was stopped after a call whose outcome is unknown). Bot v2 has no
		// reconcile interval, so without this nothing would ever start another: v0.1.0-bot2 on an upgraded instance
		// logged "helper client is closed" every 10 s for good. The pass replaces the unusable client with a new helper.
		r.requestPassAfterHelperGone(installation)
		// v0.1.0-bot2.2 (F8): only a call that failed is an error of the tick. A helper found unusable without a call
		// is the "bot native helper gone" line, once per client, not an invented "helper client is closed" every 10 s.
		if err == nil {
			return nil
		}
		return &installationMaintenanceError{installation: installation, cause: err}
	case maintenanceInPass:
	}
	if err == nil {
		err = helper.ErrClosed
	}
	return &installationMaintenanceError{installation: installation, cause: err}
}

// maintain runs every installation's maintenance at the start of a pass (maintenancePassStart).
func (r *botGatewayRuntime) maintain(ctx context.Context, done map[id.ID]bool) error {
	return r.maintainEach(ctx, done, maintenancePassStart)
}

// maintainDue runs every installation's maintenance on the due-work tick (maintenanceDue).
func (r *botGatewayRuntime) maintainDue(ctx context.Context) error {
	return r.maintainEach(ctx, map[id.ID]bool{}, maintenanceDue)
}

func (r *botGatewayRuntime) maintainEach(ctx context.Context, done map[id.ID]bool, caller maintenanceCaller) error {
	var result error
	r.mu.Lock()
	installations := make([]id.ID, 0, len(r.maintenance))
	for installation := range r.maintenance {
		installations = append(installations, installation)
	}
	r.mu.Unlock()
	for _, installation := range installations {
		result = errors.Join(result, r.maintainAs(ctx, installation, done, caller))
	}
	return result
}

// helperRespawnBase and helperRespawnCap bound the backoff of replacing an installation's helper that the due tick
// found gone (fix round, I-1): a helper that dies on its first maintenance call every time is replaced at once the
// first time, then after 10 s, 20 s, 40 s and so on up to 5 min, not on every pass.
const (
	helperRespawnBase = dueInterval
	helperRespawnCap  = 5 * time.Minute
)

// helperRespawnAfter schedules f after d; a variable so tests run it on a fake clock.
var helperRespawnAfter = func(d time.Duration, f func()) { time.AfterFunc(d, f) }

// helperRespawn is one installation's respawn backoff (guarded by botGatewayRuntime.mu).
type helperRespawn struct {
	delay   time.Duration // the wait before the next ask
	waiting bool          // an ask is scheduled; the ticks until then ask for nothing more
}

// requestPassAfterHelperGone asks for a pass of installation, whose helper the due tick found gone: at once the first
// time, then after a backoff that doubles from helperRespawnBase to helperRespawnCap until a replacement helper serves
// a maintenance run (helperServed). It follows requestPassAfterGatewayEnd.
func (r *botGatewayRuntime) requestPassAfterHelperGone(installation id.ID) {
	r.mu.Lock()
	if r.helperRespawns == nil {
		r.helperRespawns = map[id.ID]*helperRespawn{}
	}
	state := r.helperRespawns[installation]
	if state == nil {
		r.helperRespawns[installation] = &helperRespawn{delay: helperRespawnBase}
		r.mu.Unlock()
		r.askRespawn(installation, 0)
		return
	}
	if state.waiting {
		r.mu.Unlock()
		return
	}
	delay := state.delay
	state.delay = min(delay*2, helperRespawnCap)
	state.waiting = true
	r.mu.Unlock()
	helperRespawnAfter(delay, func() {
		r.mu.Lock()
		state.waiting = false
		current := r.helperRespawns[installation] == state
		r.mu.Unlock()
		if current {
			r.askRespawn(installation, delay)
		}
	})
}

// askRespawn asks for the pass that replaces installation's gone helper, and logs the ask (after: the backoff waited).
func (r *botGatewayRuntime) askRespawn(installation id.ID, after time.Duration) {
	r.logger().Info("bot native helper respawn requested", "installation_id", installation.String(), "after", after)
	r.requestPass(installation)
}

// helperServedFor is how long one helper client must stay usable before its installation's respawn backoff ends. CT 144
// (bot2.1) showed why one served tick is not enough: a helper that dies on the same work after every respawn served a
// tick each time, which reset the backoff, and was replaced every 10-30 s.
const helperServedFor = 60 * time.Second

// helperServing is since when an installation's helper client has served its maintenance (guarded by mu).
type helperServing struct {
	client any
	since  time.Time
}

// helperServed records that installation's helper client served a maintenance run; once the same client has served
// for helperServedFor, the installation's respawn backoff ends.
func (r *botGatewayRuntime) helperServed(installation id.ID, client any) {
	now := r.clockNow()
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.helperGoneLogged, installation)
	if r.helperServing == nil {
		r.helperServing = map[id.ID]helperServing{}
	}
	serving, ok := r.helperServing[installation]
	if !ok || serving.client != client {
		r.helperServing[installation] = helperServing{client: client, since: now}
		return
	}
	if now.Sub(serving.since) >= helperServedFor {
		delete(r.helperRespawns, installation)
	}
}

// helperGoneSeen notes that installation's helper client can no longer serve: its serving time ends, and the first time
// for this client it logs "bot native helper gone" (with the class of the call that failed, when one did).
func (r *botGatewayRuntime) helperGoneSeen(installation id.ID, client any, err error) {
	r.mu.Lock()
	delete(r.helperServing, installation)
	logged, seen := r.helperGoneLogged[installation]
	first := !seen || logged != client
	if first {
		if r.helperGoneLogged == nil {
			r.helperGoneLogged = map[id.ID]any{}
		}
		r.helperGoneLogged[installation] = client
	}
	r.mu.Unlock()
	if !first {
		return
	}
	attrs := []any{"installation_id", installation.String()}
	if err != nil {
		attrs = append(attrs, "error_class", helper.Class(err))
	}
	r.logger().Warn("bot native helper gone", attrs...)
}

func (r *botGatewayRuntime) clockNow() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

// logger is the runtime's logger; a runtime built without one logs nothing.
func (r *botGatewayRuntime) logger() *slog.Logger {
	if r.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.log
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

//go:build linux

package main

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// The Bot's main loop (Bot v2 task 14). There is no reconcile interval and no polling: a reconciliation pass runs once
// at start, for every installation, and then only when something says the Bot's state changed:
//
//   - passes: the gateway runtime asks (requestPass) for one installation, after a membership or device-list change on
//     its feed (op 65), a Bot-channel mark, a fresh identify (ready), an op 54 community_changed, an op 39 welcome, a
//     lost gateway connection or a failed command-intake turn. Role, channel_group and community events only update
//     the feed handler's state and ask for nothing (fix round 1, I-3);
//   - reload: SIGHUP, which dillad's supervisor sends when an installation changed (configure, disable, remove, a
//     restart request, the music defaults; Supervisor.Nudge): a pass for every installation;
//   - retry: an installation a pass failed is run again on its own, on its own backoff (passRetryBase doubling to
//     passRetryCap), which only a pass of that installation resets (fix round 2, N-3); a pass that failed as a whole
//     (the installation list, the maintenance) runs every installation again on the full-pass backoff, which only a
//     successful full pass resets. An installation a pass left incomplete is run again on its own: a joining one on its
//     own doubling capped at passJoiningCap, and a registration that waits on a device's KeyPackages when that device's
//     registration backoff (30 s up to 1 h) is due (fix round 1, M-3). Once a pass leaves everything ready, nothing is
//     armed. A gateway that ends with an error is restarted at once, then on a backoff until it reaches ready again
//     (requestPassAfterGatewayEnd).
//
// Passes are spaced (fix round 1, I-3): one starts at least passMinSpacing after the previous one started; the asks in
// between are coalesced into it, so a burst of member events costs at most one pass per passMinSpacing.
//
// Two more timers remain, both justified in the task report: the text-poll throttle (gatewayWakeThrottle), armed only
// by a gateway hint and spacing the text-channel intake it starts; and the due-work tick (dueInterval), which runs only
// the installations' local maintenance (scheduled announcements, due reminders, poll deadlines). Those read the
// helper's state and send a request only when something is due; with nothing scheduled the tick sends no request.
type botLoop struct {
	clock clock.Clock
	log   *slog.Logger
	// pass runs one reconciliation pass over targets, or over every installation when targets is nil.
	pass func(ctx context.Context, targets map[id.ID]bool) error
	// passes signals that take has asks; take returns the installations asked for, and whether all were.
	passes   <-chan struct{}
	take     func() (targets map[id.ID]bool, all bool)
	reload   <-chan os.Signal
	wake     <-chan struct{}
	poll     func(context.Context)
	maintain func(context.Context) error
	due      time.Duration
}

// gatewayWakeThrottle spaces text-source refreshes that a general gateway hint (a welcome, an epoch change, a channel
// message) starts.
const gatewayWakeThrottle = 5 * time.Second

// dueInterval is how often scheduled local work (announcements, reminders, poll deadlines) is checked.
const dueInterval = 10 * time.Second

// passMinSpacing is the least time between the starts of two passes.
const passMinSpacing = 2 * time.Second

// A failed pass is retried after passRetryBase, doubling to passRetryCap. An installation left joining is retried on
// the same doubling, capped at passJoiningCap.
const (
	passRetryBase  = time.Second
	passRetryCap   = 5 * time.Minute
	passJoiningCap = 30 * time.Second
)

// passIncomplete is a pass that succeeded but left installations to run again: each with the delay it waits (0: the
// loop's joining backoff).
type passIncomplete struct {
	after map[id.ID]time.Duration
}

func (e *passIncomplete) Error() string { return "bot installation or interaction group still joining" }

// errPassIncomplete matches every *passIncomplete.
var errPassIncomplete = errors.New("bot installation or interaction group still joining")

func (e *passIncomplete) Is(target error) bool { return target == errPassIncomplete }

// passFailed is a pass whose failures are each an installation's (fix round 2, N-3): only those are retried, each on its
// own backoff, and the rest of the pass counts as done. incomplete, when set, is what the pass left joining.
type passFailed struct {
	installations map[id.ID]bool
	incomplete    *passIncomplete
	err           error
}

func (e *passFailed) Error() string { return e.err.Error() }
func (e *passFailed) Unwrap() error { return e.err }

func (l botLoop) run(ctx context.Context) error {
	var (
		pendingAll     = true // the start pass
		pendingTargets = map[id.ID]bool{}
		lastStart      time.Time
		started        bool
		spaced         clock.Timer
		failRetry      clock.Timer
		// failBackoff is the backoff of a full pass that failed as a whole (the installation list, the maintenance):
		// only a successful full pass resets it.
		failBackoff = passRetryBase
		// instFail and instJoin are each installation's failure and joining backoffs (fix round 2, N-3): only a pass of
		// that installation resets them.
		instFail = map[id.ID]time.Duration{}
		instJoin = map[id.ID]time.Duration{}
		// retries are the failed and incomplete installations and when each is due.
		retries    = map[id.ID]time.Time{}
		retryTimer clock.Timer
	)
	stop := func(t *clock.Timer) {
		if *t != nil {
			(*t).Stop()
			*t = nil
		}
	}
	defer stop(&spaced)
	defer stop(&failRetry)
	defer stop(&retryTimer)
	armRetries := func() {
		stop(&retryTimer)
		var next time.Time
		for _, at := range retries {
			if next.IsZero() || at.Before(next) {
				next = at
			}
		}
		if !next.IsZero() {
			retryTimer = l.clock.NewTimer(max(next.Sub(l.clock.Now()), time.Millisecond))
		}
	}
	runPass := func() {
		targets := maps.Clone(pendingTargets)
		if pendingAll {
			targets = nil
		}
		pendingAll, pendingTargets = false, map[id.ID]bool{}
		lastStart, started = l.clock.Now(), true
		// This pass covers their retries; it re-arms one for an installation it leaves incomplete.
		if targets == nil {
			clear(retries)
		}
		for inst := range targets {
			delete(retries, inst)
		}
		armRetries()
		err := l.pass(ctx, targets)
		covered := func(inst id.ID) bool { return targets == nil || targets[inst] }
		var failed *passFailed
		var incomplete *passIncomplete
		if errors.As(err, &failed) {
			incomplete = failed.incomplete
		} else {
			errors.As(err, &incomplete)
		}
		if failed == nil && incomplete == nil && err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			// The pass failed as a whole: every installation again, on the full-pass backoff.
			reportBackgroundPass(l.log, err)
			stop(&failRetry)
			failRetry = l.clock.NewTimer(failBackoff)
			failBackoff = min(failBackoff*2, passRetryCap)
			return
		}
		if targets == nil && ctx.Err() == nil {
			failBackoff = passRetryBase
		}
		now := l.clock.Now()
		// Every installation the pass covered and did not leave failed or joining is done: its backoffs start over.
		for _, backoffs := range []map[id.ID]time.Duration{instFail, instJoin} {
			for inst := range backoffs {
				if covered(inst) && (failed == nil || !failed.installations[inst]) && (incomplete == nil || !hasKey(incomplete.after, inst)) {
					delete(backoffs, inst)
				}
			}
		}
		if failed != nil {
			reportBackgroundPass(l.log, failed.err)
			for inst := range failed.installations {
				b := max(instFail[inst], passRetryBase)
				retries[inst] = now.Add(b)
				instFail[inst] = min(b*2, passRetryCap)
			}
		}
		if incomplete != nil {
			for inst, after := range incomplete.after {
				if failed != nil && failed.installations[inst] {
					continue
				}
				if after <= 0 {
					b := max(instJoin[inst], passRetryBase)
					after = b
					instJoin[inst] = min(b*2, passJoiningCap)
				}
				retries[inst] = now.Add(after)
			}
			l.log.Info("bot installation or interaction group still joining; reconciling it again", "installations", len(incomplete.after))
		}
		if failed == nil && incomplete == nil {
			reportBackgroundPass(l.log, err)
		}
		armRetries()
	}
	// schedule runs the pending pass now, or arms the spacing timer for it.
	schedule := func() {
		if (!pendingAll && len(pendingTargets) == 0) || spaced != nil {
			return
		}
		if wait := passMinSpacing - l.clock.Since(lastStart); started && wait > 0 {
			spaced = l.clock.NewTimer(wait)
			return
		}
		runPass()
	}
	schedule()
	due := l.clock.NewTimer(l.due)
	defer due.Stop()
	var general clock.Timer
	lastPoll := l.clock.Now()
	for {
		var spacedC, failC, retryC, generalC <-chan time.Time
		if spaced != nil {
			spacedC = spaced.C()
		}
		if failRetry != nil {
			failC = failRetry.C()
		}
		if retryTimer != nil {
			retryC = retryTimer.C()
		}
		if general != nil {
			generalC = general.C()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-l.passes:
			targets, all := l.take()
			pendingAll = pendingAll || all
			for inst := range targets {
				pendingTargets[inst] = true
			}
			schedule()
		case <-l.reload:
			l.log.Info("bot installations changed; reconciling")
			pendingAll = true
			schedule()
		case <-failC:
			failRetry = nil
			pendingAll = true
			schedule()
		case <-retryC:
			retryTimer = nil
			now := l.clock.Now()
			for inst, at := range retries {
				if !now.Before(at) {
					pendingTargets[inst] = true
					delete(retries, inst)
				}
			}
			armRetries()
			schedule()
		case <-spacedC:
			spaced = nil
			if pendingAll || len(pendingTargets) > 0 {
				runPass()
			}
		case <-l.wake:
			if general == nil {
				general = l.clock.NewTimer(max(gatewayWakeThrottle-l.clock.Since(lastPoll), time.Millisecond))
			}
		case <-generalC:
			general = nil
			l.poll(ctx)
			lastPoll = l.clock.Now()
		case <-due.C():
			if err := l.maintain(ctx); err != nil && ctx.Err() == nil {
				l.log.Warn("bot scheduled work deferred", "error", err.Error())
			}
			due.Reset(l.due)
		}
	}
}

func hasKey[K comparable, V any](m map[K]V, k K) bool {
	_, ok := m[k]
	return ok
}

// gatewayEndBase and gatewayEndCap bound the backoff of restarting an installation's gateway that ended with an error
// before reaching ready again (fix round 2, N-3): a refused bare identify no longer re-runs at the pass spacing.
const (
	gatewayEndBase = passMinSpacing
	gatewayEndCap  = 5 * time.Minute
)

// gatewayAfter schedules f after d; a variable so tests run it on a fake clock.
var gatewayAfter = func(d time.Duration, f func()) { time.AfterFunc(d, f) }

// requestPassAfterGatewayEnd asks for a pass of installation after its gateway ended with an error: at once the first
// time, then after a backoff that doubles from gatewayEndBase to gatewayEndCap until the gateway reaches ready.
func (r *botGatewayRuntime) requestPassAfterGatewayEnd(installation id.ID) {
	r.mu.Lock()
	if r.gatewayEnds == nil {
		r.gatewayEnds = map[id.ID]time.Duration{}
	}
	delay, again := r.gatewayEnds[installation]
	if !again {
		r.gatewayEnds[installation] = gatewayEndBase
	} else {
		r.gatewayEnds[installation] = min(delay*2, gatewayEndCap)
	}
	r.mu.Unlock()
	if !again {
		r.requestPass(installation)
		return
	}
	gatewayAfter(delay, func() { r.requestPass(installation) })
}

// gatewayReady ends installation's gateway restart backoff: its gateway reached ready.
func (r *botGatewayRuntime) gatewayReady(installation id.ID) {
	r.mu.Lock()
	delete(r.gatewayEnds, installation)
	r.mu.Unlock()
}

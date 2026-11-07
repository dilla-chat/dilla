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
//   - retry: a pass that failed is run again for every installation after a backoff (passRetryBase doubling to
//     passRetryCap). An installation a pass left incomplete is run again on its own: a joining one on the same doubling
//     capped at passJoiningCap, and a registration that waits on a device's KeyPackages when that device's registration
//     backoff (30 s up to 1 h) is due (fix round 1, M-3). Once a pass leaves everything ready, nothing is armed.
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

func (l botLoop) run(ctx context.Context) error {
	var (
		pendingAll     = true // the start pass
		pendingTargets = map[id.ID]bool{}
		lastStart      time.Time
		started        bool
		spaced         clock.Timer
		failRetry      clock.Timer
		failBackoff    = passRetryBase
		joinBackoff    = passRetryBase
		// retries are the incomplete installations and when each is due.
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
		var incomplete *passIncomplete
		switch {
		case errors.As(err, &incomplete):
			now := l.clock.Now()
			for inst, after := range incomplete.after {
				if after <= 0 {
					after = joinBackoff
				}
				retries[inst] = now.Add(after)
			}
			l.log.Info("bot installation or interaction group still joining; reconciling it again", "installations", len(incomplete.after), "joining_backoff", joinBackoff)
			joinBackoff = min(joinBackoff*2, passJoiningCap)
			failBackoff = passRetryBase
			armRetries()
		case err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled):
			reportBackgroundPass(l.log, err)
			failBackoff, joinBackoff = passRetryBase, passRetryBase
		default:
			reportBackgroundPass(l.log, err)
			stop(&failRetry)
			failRetry = l.clock.NewTimer(failBackoff)
			failBackoff = min(failBackoff*2, passRetryCap)
		}
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

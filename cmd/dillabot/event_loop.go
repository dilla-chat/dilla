//go:build linux

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
)

// The Bot's main loop (Bot v2 task 14). There is no reconcile interval and no polling: a reconciliation pass runs once
// at start and then only when something says the Bot's state changed:
//
//   - passes: the gateway runtime asks (requestPass) after a feed change (op 65), a fresh identify (ready), an op 54
//     community_changed, or a lost gateway connection;
//   - reload: SIGHUP, which dillad's supervisor sends when an installation changed (configure, disable, remove, a
//     restart request, the music defaults; Supervisor.Nudge), since a new installation has no connection to hear on;
//   - retry: a pass that failed, or left an installation joining, is run again after a backoff (passRetryBase doubling),
//     and never again once one succeeded with every installation ready.
//
// Two timers remain, both justified in the task report: the text-poll throttle (gatewayWakeThrottle), armed only by a
// gateway hint and spacing the text-channel intake it starts; and the due-work tick (dueInterval), which runs only the
// installations' local maintenance (scheduled announcements, due reminders, poll deadlines). Those read the helper's
// state and send a request only when something is due; with nothing scheduled the tick sends no request.
type botLoop struct {
	clock    clock.Clock
	log      *slog.Logger
	pass     func(context.Context) error
	passes   <-chan struct{}
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

// A failed pass is retried after passRetryBase, doubling to passRetryCap. A pass that left an installation joining or
// an interaction-group registration in flight (errPassIncomplete: nothing the Bot hears reports either as done) runs
// again on the same doubling; once a pass leaves everything ready, no retry is armed.
const (
	passRetryBase = time.Second
	passRetryCap  = 5 * time.Minute
)

// errPassIncomplete is a pass that succeeded but left an installation or an interaction group joining.
var errPassIncomplete = errors.New("bot installation or interaction group still joining")

func (l botLoop) run(ctx context.Context) error {
	var retry clock.Timer
	backoff := passRetryBase
	stopRetry := func() {
		if retry != nil {
			retry.Stop()
			retry = nil
		}
	}
	runPass := func() {
		stopRetry()
		err := l.pass(ctx)
		if errors.Is(err, errPassIncomplete) {
			// Not a failure: an installation is joining or a registration is in flight. Run again on the backoff.
			l.log.Info("bot installation or interaction group still joining; reconciling again", "after", backoff)
			retry = l.clock.NewTimer(backoff)
			backoff = min(backoff*2, passRetryCap)
			return
		}
		reportBackgroundPass(l.log, err)
		if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
			backoff = passRetryBase
			return
		}
		retry = l.clock.NewTimer(backoff)
		backoff = min(backoff*2, passRetryCap)
	}
	runPass()
	due := l.clock.NewTimer(l.due)
	defer due.Stop()
	var general clock.Timer
	lastPoll := l.clock.Now()
	for {
		var retryC, generalC <-chan time.Time
		if retry != nil {
			retryC = retry.C()
		}
		if general != nil {
			generalC = general.C()
		}
		select {
		case <-ctx.Done():
			stopRetry()
			return nil
		case <-l.passes:
			runPass()
		case <-l.reload:
			l.log.Info("bot installations changed; reconciling")
			runPass()
		case <-retryC:
			retry = nil
			runPass()
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

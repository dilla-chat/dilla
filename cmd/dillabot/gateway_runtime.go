//go:build linux

package main

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

type botGatewayEntry struct {
	mu           sync.Mutex
	token        string
	device, user id.ID
	cancel       context.CancelFunc
	done         chan struct{}
	client       *gatewayclient.Client
	// feed applies the connection's op 65 events (Bot v2 task 14).
	feed *feedConsumer
	// private is the installation's current private interaction groups, and known every group it was last told of,
	// active or not (guarded by mu). source is the helper they were read from; unknown collects the groups a hint named
	// that are in neither, for the next refresh (Bot v2 task 14: no periodic pass refreshes them).
	private, known, unknown map[id.ID]bool
	source                  notificationGroupSource
	refreshing              bool
}

func (e *botGatewayEntry) setPrivate(groups map[id.ID]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.private = groups
}

// setGroups records what a SubscribeNativeGroups read: the private groups, every group seen, and their source.
func (e *botGatewayEntry) setGroups(private, known map[id.ID]bool, source notificationGroupSource) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.private, e.known, e.source = private, known, source
}

// noteUnknown records a hinted group the Bot knows nothing of, and reports whether a refresh should start (none is
// running and the helper is known).
func (e *botGatewayEntry) noteUnknown(group id.ID) (notificationGroupSource, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.private[group] || e.known[group] || e.source == nil {
		return nil, false
	}
	if e.unknown == nil {
		e.unknown = map[id.ID]bool{}
	}
	e.unknown[group] = true
	if e.refreshing {
		return nil, false
	}
	e.refreshing = true
	return e.source, true
}

// finishRefresh ends a refresh: it returns the hinted groups that are now private, marks the rest known (a group that
// is not private now is not refreshed for again until the next pass re-reads the groups), and reports whether more
// unknown groups arrived meanwhile.
func (e *botGatewayEntry) finishRefresh(checked map[id.ID]bool) (private []id.ID, again bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for g := range checked {
		if e.private[g] {
			private = append(private, g)
		} else {
			if e.known == nil {
				e.known = map[id.ID]bool{}
			}
			e.known[g] = true
		}
		delete(e.unknown, g)
	}
	// More hints arrived meanwhile: the refresh goes on (refreshing stays set), so no second one starts.
	e.refreshing = len(e.unknown) > 0
	return private, e.refreshing
}

func (e *botGatewayEntry) takeUnknown() map[id.ID]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[id.ID]bool, len(e.unknown))
	for g := range e.unknown {
		out[g] = true
	}
	return out
}

// discoverGroup handles a message hint for a group the installation knows nothing of: typically a private interaction
// group registered after the last read of the helper's groups, whose participant's command would otherwise wait for
// the next pass. It re-reads the groups once (one refresh at a time; hints meanwhile are collected) and starts the
// interaction intake of every hinted group that is now private.
func (r *botGatewayRuntime) discoverGroup(ctx context.Context, installation id.ID, e *botGatewayEntry, group id.ID) {
	source, start := e.noteUnknown(group)
	if !start {
		return
	}
	go func() {
		for {
			checked := e.takeUnknown()
			refreshCtx, cancel := context.WithTimeout(client.WithBackground(ctx), 30*time.Second)
			err := r.SubscribeNativeGroups(refreshCtx, installation, source)
			cancel()
			if err != nil && ctx.Err() == nil {
				r.log.Warn("bot gateway native group discovery unavailable", "installation_id", installation.String())
			}
			private, again := e.finishRefresh(checked)
			for _, g := range private {
				r.notifyInteraction(installation, g)
			}
			if !again {
				return
			}
			if ctx.Err() != nil {
				e.finishRefresh(e.takeUnknown())
				return
			}
		}
	}()
}

func (e *botGatewayEntry) isPrivate(group *id.ID) bool {
	if group == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.private[*group]
}

type interactionWork struct{ installation, group id.ID }

type botGatewayRuntime struct {
	mu             sync.Mutex      // entry, text-poller, maintenance and schedule snapshots only
	commandsClosed bool            // guarded by pendingMu
	commands       *commandWorkers // guarded by pendingMu
	now            func() time.Time
	ctx            context.Context
	api            *client.Client
	loopback       bool
	// httpClient and launch are the supervised Bot's: its gateway connections dial dillad's Bot socket, and the first
	// identify carries the launch token. Both nil for a Bot on the network.
	httpClient *http.Client
	launch     *gatewayclient.LaunchToken
	log        *slog.Logger
	entries    map[id.ID]*botGatewayEntry
	wake       chan struct{}
	// pass asks the main loop for a reconciliation pass (Bot v2 task 14: the Bot polls nothing; a pass follows a feed
	// change, a fresh identify, an op 54, a lost connection or SIGHUP). Coalesced: one pending ask is enough.
	pass chan struct{}
	// passTargets are the installations asked for since the loop last took them (guarded by mu).
	passTargets map[id.ID]bool
	// gatewayEnds is each installation's backoff for restarting a gateway that ended with an error, reset by its ready
	// (guarded by mu; fix round 2, N-3).
	gatewayEnds map[id.ID]time.Duration
	// feeds is each installation's feed handler, which outlives a pass and a gateway connection (guarded by mu).
	feeds map[id.ID]*installationFeed
	// interaction carries a participant's message in a private interaction group: a command
	// waiting for an answer. It wakes the dedicated command workers directly.
	interaction chan struct{}
	pollers     map[id.ID]func(context.Context) error
	maintenance map[id.ID]backgroundMaintenance
	startedAt   uint64

	// pending holds the interaction groups messaged since the last fast poll, per installation
	// (guarded by pendingMu: the per-installation gateway goroutines write it).
	pendingMu           sync.Mutex
	pending             map[id.ID]map[id.ID]bool
	deferred            map[id.ID]time.Time
	idlePolls           map[id.ID]*service.InteractionPollSchedule
	budgets             map[id.ID]map[id.ID]*interactionBudget // per group, outliving reconcile passes
	backgroundPending   map[interactionWork]bool               // pendingMu, periodic catch-up behind real hints
	pendingOrder        []interactionWork                      // FIFO: a noisy group rejoins behind existing work
	lastInteractionPoll map[id.ID]time.Time                    // per installation, guarded by pendingMu
	interactionPollers  map[id.ID]func(context.Context, id.ID) error
	pollerVersions      map[id.ID]uint64
	// sealed is each installation's sealed command intake (Bot v2 Task 15, command_worker.go).
	sealed sealedRegistry
	// webhooks is each installation's incoming-webhook poster (Bot v2 Task 17, webhook_post.go).
	webhooks webhookRegistry
}

func newBotGatewayRuntime(ctx context.Context, api *client.Client, loopback bool, log *slog.Logger) *botGatewayRuntime {
	return &botGatewayRuntime{now: time.Now, startedAt: unixSecondsOrZero(time.Now()) * 1000, ctx: client.WithBackground(ctx), api: api, loopback: loopback, log: log, entries: map[id.ID]*botGatewayEntry{}, wake: make(chan struct{}, 1), pass: make(chan struct{}, 1), feeds: map[id.ID]*installationFeed{}, interaction: make(chan struct{}, 1), pollers: map[id.ID]func(context.Context) error{}, lastInteractionPoll: map[id.ID]time.Time{}}
}

// InteractionPollSchedule survives the short-lived reconciler built each tick.
func (r *botGatewayRuntime) InteractionPollSchedule(installation id.ID) *service.InteractionPollSchedule {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.idlePolls == nil {
		r.idlePolls = map[id.ID]*service.InteractionPollSchedule{}
	}
	if r.idlePolls[installation] == nil {
		r.idlePolls[installation] = &service.InteractionPollSchedule{}
	}
	return r.idlePolls[installation]
}

func (r *botGatewayRuntime) Wake() <-chan struct{} { return r.wake }

// Pass is the main loop's channel of pass requests (requestPass).
func (r *botGatewayRuntime) Pass() <-chan struct{} { return r.pass }

// requestPass asks for one reconciliation pass of installation (fix round 1, I-3: never of every installation for one
// installation's event); asks while one is pending coalesce.
func (r *botGatewayRuntime) requestPass(installation id.ID) {
	r.mu.Lock()
	if r.passTargets == nil {
		r.passTargets = map[id.ID]bool{}
	}
	r.passTargets[installation] = true
	r.mu.Unlock()
	select {
	case r.pass <- struct{}{}:
	default:
	}
}

// takePassTargets returns and clears the installations asked for.
func (r *botGatewayRuntime) takePassTargets() (map[id.ID]bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	targets := r.passTargets
	r.passTargets = nil
	return targets, false
}

// feedHandler is the installation's feed handler, or nil before a pass built one.
func (r *botGatewayRuntime) feedHandler(installation id.ID) *installationFeed {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.feeds[installation]
}

// feedFor returns the installation's feed handler, creating it on first use: it outlives passes and connections, so
// what it learned from the feed (members, roles, Bot channels) survives a reconnect.
func (r *botGatewayRuntime) feedFor(installation, community, botUser id.ID, deps feedDeps) *installationFeed {
	r.mu.Lock()
	f := r.feeds[installation]
	if f == nil || f.community != community || f.botUser != botUser {
		f = newInstallationFeed(installation, community, botUser, r.log.With("installation_id", installation.String()), func() { r.requestPass(installation) })
		r.feeds[installation] = f
	}
	f.setDeps(deps)
	e := r.entries[installation]
	r.mu.Unlock()
	if e != nil && e.feed != nil {
		e.feed.setFeed(r.sealed.feed(installation, community, f))
	}
	return f
}
func (r *botGatewayRuntime) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
func (r *botGatewayRuntime) notifyInteraction(installation, group id.ID) {
	r.pendingMu.Lock()
	r.queueInteractionLocked(installation, group)
	var stop context.CancelFunc
	if r.commands != nil && r.commands.background[installation] {
		stop = r.commands.active[installation]
	}
	r.pendingMu.Unlock()
	// Stop at the next safe boundary; never abort an active helper/HTTP call.
	if stop != nil {
		stop()
	}
	r.signalCommands()
}
func (r *botGatewayRuntime) queueInteractionLocked(installation, group id.ID) {
	work := interactionWork{installation, group}
	if r.pending[installation][group] && !r.backgroundPending[work] {
		return
	}
	if r.pending == nil {
		r.pending = map[id.ID]map[id.ID]bool{}
	}
	if r.pending[installation] == nil {
		r.pending[installation] = map[id.ID]bool{}
	}
	if r.backgroundPending[work] {
		delete(r.backgroundPending, work)
		remaining := r.pendingOrder[:0]
		for _, queued := range r.pendingOrder {
			if queued != work {
				remaining = append(remaining, queued)
			}
		}
		r.pendingOrder = remaining
	}
	r.pending[installation][group] = true
	// Keep FIFO within foreground work, ahead of periodic catch-up hints.
	at := len(r.pendingOrder)
	for i, queued := range r.pendingOrder {
		if r.backgroundPending[queued] {
			at = i
			break
		}
	}
	r.pendingOrder = append(r.pendingOrder, work)
	copy(r.pendingOrder[at+1:], r.pendingOrder[at:len(r.pendingOrder)-1])
	r.pendingOrder[at] = work
}
func (r *botGatewayRuntime) notifyBackgroundInteraction(installation, group id.ID) {
	r.pendingMu.Lock()
	r.queueBackgroundInteractionLocked(installation, group)
	r.pendingMu.Unlock()
	r.signalCommands()
}
func (r *botGatewayRuntime) queueBackgroundInteractionLocked(installation, group id.ID) {
	if r.commands != nil {
		if turn := r.commands.activeJobs[installation]; turn != nil && turn.work.group == group {
			return
		}
	}
	if r.pending[installation][group] {
		return
	}
	if r.pending == nil {
		r.pending = map[id.ID]map[id.ID]bool{}
	}
	if r.pending[installation] == nil {
		r.pending[installation] = map[id.ID]bool{}
	}
	if r.backgroundPending == nil {
		r.backgroundPending = make(map[interactionWork]bool)
	}
	work := interactionWork{installation, group}
	r.backgroundPending[work] = true
	r.pending[installation][group] = true
	r.pendingOrder = append(r.pendingOrder, work)
}

// SetInteractionPoller registers the installation's fast path for interaction-group messages;
// nil retires it when the installation is removed or restarted.
func (r *botGatewayRuntime) SetInteractionPoller(installation id.ID, poll func(context.Context, id.ID) error) {
	r.pendingMu.Lock()
	if r.interactionPollers == nil {
		r.interactionPollers = map[id.ID]func(context.Context, id.ID) error{}
	}
	if poll == nil {
		delete(r.interactionPollers, installation)
	} else {
		r.interactionPollers[installation] = poll
	}
	if r.pollerVersions == nil {
		r.pollerVersions = make(map[id.ID]uint64)
	}
	r.pollerVersions[installation]++
	var retiring *commandTurn
	if poll == nil && r.commands != nil {
		retiring = r.commands.activeJobs[installation]
	}
	r.pendingMu.Unlock()
	// Publishing a new callback never interrupts or restarts an active command.
	if retiring != nil {
		retiring.retire()
	}
	r.signalCommands()
}

func (r *botGatewayRuntime) InteractionPoller(installation id.ID) func(context.Context, id.ID) error {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	return r.interactionPollers[installation]
}

func (r *botGatewayRuntime) signalCommands() {
	select {
	case r.interaction <- struct{}{}:
	default:
	}
}

func (r *botGatewayRuntime) budgetLocked(installation, group id.ID) *interactionBudget {
	if r.budgets == nil {
		r.budgets = map[id.ID]map[id.ID]*interactionBudget{}
	}
	if r.budgets[installation] == nil {
		r.budgets[installation] = map[id.ID]*interactionBudget{}
	}
	b := r.budgets[installation][group]
	if b == nil {
		b = &interactionBudget{}
		r.budgets[installation][group] = b
	}
	return b
}

func (r *botGatewayRuntime) interactionDelayLocked(installation, group id.ID, now time.Time) time.Duration {
	return max(r.budgetLocked(installation, group).delay(now), r.deferred[installation].Sub(now), interactionSpacing-now.Sub(r.lastInteractionPoll[installation]))
}

func (r *botGatewayRuntime) Ensure(installation, community, device, user id.ID, token string) error {
	if e := r.entry(installation); e != nil {
		select {
		case <-e.done:
			r.removeEntry(installation, e)
		default:
			if e.device == device && e.user == user {
				e.mu.Lock()
				e.token = token
				e.mu.Unlock()
				return nil
			}
			e.cancel()
			<-e.done
			r.removeEntry(installation, e)
			r.SetTextPoller(installation, nil)
			r.SetInteractionPoller(installation, nil)
			r.SetMaintenance(installation, backgroundMaintenance{})
		}
	}
	log := r.log.With("installation_id", installation.String())
	gateway, err := gatewayclient.New(gatewayclient.Config{Client: r.api, AllowLoopbackHTTP: r.loopback, ExpectedDeviceID: device, ExpectedUserID: user, Logger: log,
		HTTPClient: r.httpClient, Launch: r.launch,
		// A cold-start snapshot streams one frame per member's device list, then the members and roles, faster than
		// any handler runs; the feed buffers them, and the client's queue must hold the burst (Bot v2 task 14).
		QueueSize: botGatewayQueue})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.ctx)
	acks := &orderedAcks{log: log}
	feed := newFeedConsumer(community, acks, log)
	if h := r.feedHandler(installation); h != nil {
		feed.setFeed(r.sealed.feed(installation, community, h))
	}
	e := &botGatewayEntry{token: token, device: device, user: user, cancel: cancel, done: make(chan struct{}), client: gateway, feed: feed}
	r.mu.Lock()
	r.entries[installation] = e
	r.mu.Unlock()
	// Bot v2: the device's session sends its socket routes as request frames on this gateway.
	r.api.UseTransport(device, gateway)
	go feed.run(ctx)
	// Readiness (fix round 1, I-5): from the connection's start to its first applied snapshot, within the spec's 5 s.
	go func() {
		started := time.Now()
		if err := feed.readyWithin(ctx, readyBudget); err == nil {
			log.Info("bot feed ready for commands", "after", time.Since(started))
		} else if ctx.Err() == nil {
			log.Warn("bot feed not ready within the budget", "budget", readyBudget)
		}
	}()
	go func() {
		defer close(e.done)
		defer r.api.ReleaseTransport(device, gateway)
		finished := make(chan error, 1)
		go func() {
			finished <- gateway.Run(ctx, func(context.Context) (string, error) { e.mu.Lock(); defer e.mu.Unlock(); return e.token, nil })
		}()
		consumeDeliveries(ctx, gateway.Events(), acks, feed, log, func(delivery gatewayclient.Delivery) {
			switch delivery.Frame.Op {
			case gatewayclient.OpReady:
				r.gatewayReady(installation)
				if ready := delivery.Frame.Ready; ready != nil && len(ready.Groups) > 0 {
					groups := make([]id.ID, 0, len(ready.Groups))
					for _, g := range ready.Groups {
						groups = append(groups, g.GroupID)
					}
					if err := gateway.Subscribe(ctx, groups); err != nil && ctx.Err() == nil {
						log.Warn("bot gateway group notifications unavailable")
					}
				}
				r.notify()
				// A fresh identify (a new or restarted socket session): what changed while it was away is read again.
				r.requestPass(installation)
			case gatewayclient.OpMessageCT:
				// Outgoing ciphertext is already processed by the native sender.
				// Its gateway echo must not start another full intake sweep.
				if message := delivery.Frame.MessageCT; message == nil || message.UploaderDevice != device {
					// A participant's message in a private interaction group is a command
					// waiting for an answer; it skips the general throttle.
					if e.isPrivate(delivery.Frame.GroupID) {
						r.notifyInteraction(installation, *delivery.Frame.GroupID)
					} else {
						r.notify()
						// A group the Bot was never told of may be an interaction group registered since the last read.
						if g := delivery.Frame.GroupID; g != nil {
							r.discoverGroup(ctx, installation, e, *g)
						}
					}
				}
			case gatewayclient.OpBotCommand:
				// A member's sealed command (Task 15): queued for the installation's sealed intake, acknowledged here.
				r.relayCommand(installation, delivery.Frame.BotPayload, feed.barrier())
			case gatewayclient.OpBotReply:
				// The answer to the Bot's own health probe (Task 17 fix round 2), through dillad's reply route.
				r.probeReply(installation, delivery.Frame.BotReply)
			case gatewayclient.OpBotWebhook:
				// An incoming webhook POST (Task 17): posted into its Bot channel, then acknowledged with op 67 on the
				// socket it came on (fix round 2, M4: none if that socket was resumed or replaced meanwhile), off this loop.
				r.deliverWebhook(ctx, installation, delivery.WebhookAck, delivery.Frame.BotPayload)
			case gatewayclient.OpCommunityChanged:
				// The community's channels, settings or the Bot's own installation changed (op 54): read them again.
				musicSignals.communityChanged(installation)
				r.notify()
				r.requestPass(installation)
			case gatewayclient.OpWelcome:
				// A Welcome adds the Bot to a group: the pass joins it (and may then mark the installation ready).
				r.notify()
				r.requestPass(installation)
			case gatewayclient.OpMessagePlain, gatewayclient.OpHandshake, gatewayclient.OpEpochChanged, gatewayclient.OpVoiceState:
				if op := delivery.Frame.Op; (op == gatewayclient.OpHandshake || op == gatewayclient.OpEpochChanged) && delivery.Frame.GroupID != nil {
					// A playing room in this call group applies the commit and re-keys now (Bot v2 Task 19 fix round 1, C1).
					musicSignals.callGroupChanged(installation, *delivery.Frame.GroupID)
				}
				r.notify()
			}
		})
		err := <-finished
		onItsOwn := ctx.Err() == nil // the connection ended itself, not through Ensure, RetainOnly or Drop
		// The connection is over, so its feed worker is too (Bot v2 Task 16 fix round 4, N-7). Left running, it would
		// keep applying this stream's device lists (the rest of a snapshot, applyHeld's retries) beside the next
		// connection's snapshot: two Pushed callers for one user on one installationFeed.
		cancel()
		if err != nil && onItsOwn {
			log.Warn("bot gateway requires session reconciliation")
			r.notify()
			// A gateway that ends before ready again (a refused identify: the bare-identify loop of fix round 1, I-4) is
			// restarted on a growing backoff, not at the pass spacing (fix round 2, N-3).
			r.requestPassAfterGatewayEnd(installation)
		}
	}()
	return nil
}

// botGatewayQueue is the gateway client's delivery queue for a Bot connection: the client's largest, so a cold-start
// snapshot (n = 0 frames, one per member's device list, then members and roles) never overflows it.
const botGatewayQueue = 4096

// consumeDeliveries is one Bot connection's delivery loop (Bot v2 task 14). Every ringed delivery is acknowledged in
// order (orderedAcks): an op 65 bot_event once the feed applied it, anything else once hint ran. Op 64 bot_command is
// acknowledged as received (sealed intake is Task 15's) and op 66 bot_webhook, never ringed, goes to hint like the
// others: its poster (Task 17) posts it and answers with op 67 only once the message is accepted. It returns when
// events closes.
func consumeDeliveries(ctx context.Context, events <-chan gatewayclient.Delivery, acks *orderedAcks, feed *feedConsumer, log *slog.Logger, hint func(gatewayclient.Delivery)) {
	for delivery := range events {
		if delivery.Frame.Op == gatewayclient.OpReady {
			// A new stream: its n restart at 1, and what the old one left unapplied comes again in its snapshot.
			acks.reset()
			feed.resetStream()
		}
		slot := acks.track(delivery)
		switch delivery.Frame.Op {
		case gatewayclient.OpBotEvent:
			feed.deliver(delivery, slot)
			continue
		default:
			// Op 66 bot_webhook is never ringed (n = 0); its poster acknowledges it with op 67 once the post is made.
			hint(delivery)
		}
		acks.done(slot)
	}
}
func (r *botGatewayRuntime) entry(installation id.ID) *botGatewayEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[installation]
}
func (r *botGatewayRuntime) removeEntry(installation id.ID, expected *botGatewayEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries[installation] == expected {
		delete(r.entries, installation)
	}
}
func (r *botGatewayRuntime) RetainOnly(active []id.ID) {
	keep := map[id.ID]bool{}
	for _, v := range active {
		keep[v] = true
	}
	r.mu.Lock()
	var removed []*botGatewayEntry
	for installation, e := range r.entries {
		if !keep[installation] {
			removed = append(removed, e)
			delete(r.entries, installation)
		}
	}
	for installation := range r.pollers {
		if !keep[installation] {
			delete(r.pollers, installation)
		}
	}
	for installation := range r.maintenance {
		if !keep[installation] {
			delete(r.maintenance, installation)
		}
	}
	for installation, f := range r.feeds {
		if !keep[installation] {
			f.close() // its trust retries stop (Bot v2 Task 16, fix round 1 M-3)
			delete(r.feeds, installation)
		}
	}
	r.sealed.retain(keep)
	r.webhooks.retain(keep)
	for installation := range r.idlePolls {
		if !keep[installation] {
			delete(r.idlePolls, installation)
		}
	}
	r.mu.Unlock()
	for _, e := range removed {
		e.cancel()
		<-e.done
	}
	r.pendingMu.Lock()
	var retiring []*commandTurn
	for installation := range r.interactionPollers {
		if !keep[installation] {
			delete(r.interactionPollers, installation)
			delete(r.pollerVersions, installation)
			if r.commands != nil {
				if turn := r.commands.activeJobs[installation]; turn != nil {
					retiring = append(retiring, turn)
				}
			}
		}
	}
	for work := range r.backgroundPending {
		if !keep[work.installation] {
			delete(r.backgroundPending, work)
		}
	}
	if r.commands != nil {
		for work := range r.commands.retry {
			if !keep[work.installation] {
				delete(r.commands.retry, work)
			}
		}
		for work := range r.commands.failures {
			if !keep[work.installation] {
				delete(r.commands.failures, work)
			}
		}
		for work := range r.commands.blockedLease {
			if !keep[work.installation] {
				delete(r.commands.blockedLease, work)
			}
		}
	}
	for installation := range r.pending {
		if !keep[installation] {
			delete(r.pending, installation)
		}
	}
	for installation := range r.budgets {
		if !keep[installation] {
			delete(r.budgets, installation)
			delete(r.deferred, installation)
		}
	}
	for installation := range r.lastInteractionPoll {
		if !keep[installation] {
			delete(r.lastInteractionPoll, installation)
		}
	}
	remaining := r.pendingOrder[:0]
	for _, w := range r.pendingOrder {
		if keep[w.installation] {
			remaining = append(remaining, w)
		}
	}
	r.pendingOrder = remaining
	r.pendingMu.Unlock()
	for _, turn := range retiring {
		turn.retire()
	}
	r.signalCommands()
}
func (r *botGatewayRuntime) Close() { r.stopCommandWorkers(); r.RetainOnly(nil) }

// Drop stops intake and waits for its borrowed helper to be returned before a
// manager can reopen the same state directory.
func (r *botGatewayRuntime) Drop(installation id.ID) {
	r.SetInteractionPoller(installation, nil)
	r.pendingMu.Lock()
	var running *commandTurn
	if r.commands != nil {
		running = r.commands.activeJobs[installation]
	}
	r.pendingMu.Unlock()
	if running != nil {
		running.retire()
		<-running.done
	}
	r.SetTextPoller(installation, nil)
	r.SetMaintenance(installation, backgroundMaintenance{})
	if e := r.entry(installation); e != nil {
		r.removeEntry(installation, e)
		e.cancel()
		<-e.done
	}
}

func (r *botGatewayRuntime) SubscribeGroups(ctx context.Context, installation id.ID, groups []id.ID) {
	if e := r.entry(installation); e != nil && len(groups) > 0 {
		// The initial Ready may precede MLS membership reconciliation. Subscribe the
		// current native groups too, so newly joined text channels wake the poller.
		_ = e.client.Subscribe(ctx, groups)
	}
}

type notificationGroupSource interface {
	Groups(context.Context) ([]native.GroupInfo, error)
	InteractionGroups(context.Context) ([]native.InteractionGroupDescriptor, error)
}

func (r *botGatewayRuntime) SubscribeNativeGroups(ctx context.Context, installation id.ID, source notificationGroupSource) error {
	groups, err := source.Groups(ctx)
	if err != nil {
		return err
	}
	private, err := source.InteractionGroups(ctx)
	if err != nil {
		return err
	}
	ids := make([]id.ID, 0, len(groups)+len(private))
	known := make(map[id.ID]bool, len(groups)+len(private))
	for _, group := range groups {
		known[group.ID] = true
		if group.State == 2 && group.Kind == 0 {
			ids = append(ids, group.ID)
		}
	}
	// Generic Groups deliberately excludes kind 3; discover private interaction
	// groups through the native API that authenticates their installation binding.
	active := make(map[id.ID]bool, len(private))
	for _, group := range private {
		known[group.Group] = true
		if group.State == 2 {
			ids = append(ids, group.Group)
			active[group.Group] = true
		}
	}
	if e := r.entry(installation); e != nil {
		// A private group not active yet is not known: a later hint for it re-reads (discoverGroup).
		for g := range known {
			if !active[g] && !slices.ContainsFunc(groups, func(x native.GroupInfo) bool { return x.ID == g }) {
				delete(known, g)
			}
		}
		e.setGroups(active, known, source)
	}
	r.SubscribeGroups(ctx, installation, ids)
	return nil
}

func (r *botGatewayRuntime) SetTextPoller(installation id.ID, poll func(context.Context) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if poll == nil {
		delete(r.pollers, installation)
	} else {
		r.pollers[installation] = poll
	}
}
func (r *botGatewayRuntime) TextPoller(installation id.ID) func(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pollers[installation]
}
func (r *botGatewayRuntime) PollText(ctx context.Context) {
	ctx = client.WithBackground(ctx)
	r.mu.Lock()
	pollers := make(map[id.ID]func(context.Context) error, len(r.pollers))
	for installation, poll := range r.pollers {
		pollers[installation] = poll
	}
	r.mu.Unlock()
	for installation, poll := range pollers {
		if err := poll(ctx); err != nil && ctx.Err() == nil {
			if refusal := client.RateLimitError(err); refusal != nil {
				logBotRateLimit(r.log, installation, refusal)
			} else {
				r.log.Warn("bot text command intake deferred", "installation_id", installation.String())
			}
		}
	}
}

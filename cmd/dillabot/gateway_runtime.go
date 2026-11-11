//go:build linux

package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/native"
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
}

type botGatewayRuntime struct {
	mu       sync.Mutex // entry, text-poller and maintenance snapshots only
	now      func() time.Time
	ctx      context.Context
	api      *client.Client
	loopback bool
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
	// helperRespawns is each installation's backoff for replacing a helper the due tick found gone, reset when a
	// replacement serves a maintenance run (guarded by mu; fix round, I-1).
	helperRespawns map[id.ID]*helperRespawn
	// starts runs the native helper starts off the loop (fix round, I-2; helper_start.go).
	starts helperStarts
	// feeds is each installation's feed handler, which outlives a pass and a gateway connection (guarded by mu).
	feeds       map[id.ID]*installationFeed
	pollers     map[id.ID]func(context.Context) error
	maintenance map[id.ID]backgroundMaintenance
	startedAt   uint64

	// sealed is each installation's sealed command intake (Bot v2 Task 15, command_worker.go).
	sealed sealedRegistry
	// webhooks is each installation's incoming-webhook poster (Bot v2 Task 17, webhook_post.go).
	webhooks webhookRegistry
}

func newBotGatewayRuntime(ctx context.Context, api *client.Client, loopback bool, log *slog.Logger) *botGatewayRuntime {
	r := &botGatewayRuntime{now: time.Now, startedAt: unixSecondsOrZero(time.Now()) * 1000, ctx: client.WithBackground(ctx), api: api, loopback: loopback, log: log, entries: map[id.ID]*botGatewayEntry{}, wake: make(chan struct{}, 1), pass: make(chan struct{}, 1), feeds: map[id.ID]*installationFeed{}, pollers: map[id.ID]func(context.Context) error{}}
	r.starts.ctx, r.starts.finished = r.ctx, r.requestPass
	return r
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
				// A (re)started session may have missed an op 54: the music path reads its installation row and policy
				// again (Bot v2 Task 19 fix round 2, N4).
				musicSignals.communityChanged(installation)
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
					r.notify()
				}
			case gatewayclient.OpBotCommand:
				// A member's sealed command (Task 15): queued for the installation's sealed intake, acknowledged here.
				r.relayCommandFor(installation, community, delivery.Frame.BotPayload, feed.barrier())
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
				musicSignals.communityChanged(installation)
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
		// Its waiting commands go to the installation's next connection, after that one's snapshot (Task 21b fix
		// round 2): a session reset must not answer them offline.
		feed.end(func() *feedConsumer {
			if next := r.entry(installation); next != nil && next != e {
				return next.feed
			}
			return nil
		})
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
	for installation := range r.helperRespawns {
		if !keep[installation] {
			delete(r.helperRespawns, installation)
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
	r.mu.Unlock()
	r.starts.retain(keep)
	for _, e := range removed {
		e.cancel()
		<-e.done
	}
}

// Close stops every installation's intake, cancels the helper starts in flight and waits for them to end.
func (r *botGatewayRuntime) Close() {
	r.RetainOnly(nil)
	r.starts.close()
}

// Drop stops intake and waits for its borrowed helper to be returned before a
// manager can reopen the same state directory.
func (r *botGatewayRuntime) Drop(installation id.ID) {
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
}

// SubscribeNativeGroups subscribes the installation's gateway to its active text groups (the Bot channels' groups).
func (r *botGatewayRuntime) SubscribeNativeGroups(ctx context.Context, installation id.ID, source notificationGroupSource) error {
	groups, err := source.Groups(ctx)
	if err != nil {
		return err
	}
	ids := make([]id.ID, 0, len(groups))
	for _, group := range groups {
		if group.State == 2 && group.Kind == 0 {
			ids = append(ids, group.ID)
		}
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
				r.log.Warn("bot text command intake deferred", "installation_id", installation.String(), "error_class", helper.Class(err))
			}
		}
	}
}

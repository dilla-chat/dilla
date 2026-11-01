//go:build linux

package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/gatewayclient"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

type botGatewayEntry struct {
	mu           sync.Mutex
	token        string
	device, user id.ID
	cancel       context.CancelFunc
	done         chan struct{}
	client       *gatewayclient.Client
	// private is the installation's current private interaction groups (guarded by mu).
	private map[id.ID]bool
}

func (e *botGatewayEntry) setPrivate(groups map[id.ID]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.private = groups
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
	log            *slog.Logger
	entries        map[id.ID]*botGatewayEntry
	wake           chan struct{}
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
}

func newBotGatewayRuntime(ctx context.Context, api *client.Client, loopback bool, log *slog.Logger) *botGatewayRuntime {
	return &botGatewayRuntime{now: time.Now, startedAt: unixSecondsOrZero(time.Now()) * 1000, ctx: client.WithBackground(ctx), api: api, loopback: loopback, log: log, entries: map[id.ID]*botGatewayEntry{}, wake: make(chan struct{}, 1), interaction: make(chan struct{}, 1), pollers: map[id.ID]func(context.Context) error{}, lastInteractionPoll: map[id.ID]time.Time{}}
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

func (r *botGatewayRuntime) Ensure(installation, device, user id.ID, token string) error {
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
	gateway, err := gatewayclient.New(gatewayclient.Config{Client: r.api, AllowLoopbackHTTP: r.loopback, ExpectedDeviceID: device, ExpectedUserID: user, Logger: r.log.With("installation_id", installation.String())})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.ctx)
	e := &botGatewayEntry{token: token, device: device, user: user, cancel: cancel, done: make(chan struct{}), client: gateway}
	r.mu.Lock()
	r.entries[installation] = e
	r.mu.Unlock()
	go func() {
		defer close(e.done)
		finished := make(chan error, 1)
		go func() {
			finished <- gateway.Run(ctx, func(context.Context) (string, error) { e.mu.Lock(); defer e.mu.Unlock(); return e.token, nil })
		}()
		for delivery := range gateway.Events() {
			switch delivery.Frame.Op {
			case gatewayclient.OpReady:
				if ready := delivery.Frame.Ready; ready != nil && len(ready.Groups) > 0 {
					groups := make([]id.ID, 0, len(ready.Groups))
					for _, g := range ready.Groups {
						groups = append(groups, g.GroupID)
					}
					if err := gateway.Subscribe(ctx, groups); err != nil && ctx.Err() == nil {
						r.log.Warn("bot gateway group notifications unavailable", "installation_id", installation.String())
					}
				}
				r.notify()
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
					}
				}
			case gatewayclient.OpMessagePlain, gatewayclient.OpWelcome, gatewayclient.OpHandshake, gatewayclient.OpEpochChanged, gatewayclient.OpCommunityChanged, gatewayclient.OpVoiceState:
				r.notify()
			}
			// Gateway deliveries are wake-up hints only. MLS source cursors and bot
			// event acknowledgements advance independently after durable HTTP intake.
			if err := acknowledgeGatewayHint(delivery); err != nil && !errors.Is(err, gatewayclient.ErrStaleDelivery) && ctx.Err() == nil {
				r.log.Warn("bot gateway notification acknowledgement deferred", "installation_id", installation.String())
			}
		}
		if err := <-finished; err != nil && ctx.Err() == nil {
			r.log.Warn("bot gateway requires session reconciliation", "installation_id", installation.String())
			r.notify()
		}
	}()
	return nil
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
	for _, group := range groups {
		if group.State == 2 && group.Kind == 0 {
			ids = append(ids, group.ID)
		}
	}
	// Generic Groups deliberately excludes kind 3; discover private interaction
	// groups through the native API that authenticates their installation binding.
	active := make(map[id.ID]bool, len(private))
	for _, group := range private {
		if group.State == 2 {
			ids = append(ids, group.Group)
			active[group.Group] = true
		}
	}
	if e := r.entry(installation); e != nil {
		e.setPrivate(active)
	}
	r.SubscribeGroups(ctx, installation, ids)
	return nil
}

func acknowledgeGatewayHint(delivery gatewayclient.Delivery) error {
	if delivery.Frame.N == 0 {
		return nil
	}
	return delivery.Ack()
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

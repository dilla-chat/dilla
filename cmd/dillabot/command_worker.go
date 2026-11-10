//go:build linux

package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/bot/catalog"
	"github.com/dilla-chat/dilla/internal/bot/health"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/intake"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
)

// Each installation has one sealed intake that outlives passes and gateway connections:
//
//   - its Authz is fed by the installation's feed (sealedFeed tees member, role, community and snapshot events
//     to it), so a command is authorised from pushed state only;
//   - op 64 is decoded on the delivery loop (relayCommand) and queued; sealedWorkers goroutines run
//     intake.Handle, so one slow command ("Searching…") never holds the others, and nothing waits on a clock;
//   - a pass hands it the current helper, session, capabilities and handlers (SetSealedIntake); the intake's
//     answer cache (5 min, for a Retry) and the Authz survive the swap;
//   - its catalog.Publisher publishes the catalog and command key on start, on a capability change, every hour
//     (the helper rotates the key; a request is sent only when the bytes changed) and when a command named a key
//     the helper no longer holds.
//
// A command costs one helper op 22, one op 23 per answer, and one POST /v1/bot-runtime/replies per answer: no
// device-list read, no member read (TestCommandCostIsFlatAt200Members600Devices).

const (
	// sealedWorkers is each installation's concurrent commands; the helper serialises its own ops.
	sealedWorkers = 4
	// sealedQueue bounds the commands waiting for a worker. A command over it is dropped (logged): the client
	// times out after 5 s and its Retry is relayed again.
	sealedQueue = 256
)

// sealedHelper is the installation's native helper: ops 21, 22 and 23.
type sealedHelper interface {
	intake.Helper
	catalog.KeySource
}

// sealedSession is the installation's Bot session: replies, the catalog and the command key.
type sealedSession interface {
	intake.ReplySender
	catalog.Session
}

// sealedIntakeConfig is what a pass hands an installation's sealed intake.
type sealedIntakeConfig struct {
	installation, community, botUser id.ID
	capabilities                     uint64
	helper                           sealedHelper
	session                          sealedSession
	// handlers is the pass's feature handler map (the same one the interaction dispatcher serves).
	handlers map[protocol.Operation]service.InteractionHandlerSpec
	// reminders, when set, is served from the feed: membership from Authz, no capability read per create
	// (the catalog only offers reminder operations while the capability is on).
	reminders *service.ReminderInteractions
	// polls are the poll handlers, served in the sealed path with the feed authority and a read cache (Task 21b fix
	// round 1).
	polls *service.PollInteractions
	held  intake.HeldChecker
	log   *slog.Logger
	// musicStartP95 reads the installation's music start p95 for the health report (Bot v2 Task 17); nil is zero.
	musicStartP95 func() time.Duration
	// musicStarts reads the installation's last music starts for the Bot page's status read (Task 22); nil is none.
	musicStarts func() []music.StartTiming
	// healthDir is where the health monitor leaves its last report for `dillabot doctor` (health_state.go); empty
	// writes none.
	healthDir string
}

// sealedEntry is one installation's sealed intake. It is the intake's helper, dispatcher and reply sender and the
// publisher's key source and session, each forwarding to the pass's current dependencies.
type sealedEntry struct {
	installation, community id.ID
	authz                   *intake.Authz
	queue                   chan intake.RelayedCommand

	mu         sync.Mutex
	cfg        sealedIntakeConfig
	dispatcher *service.SealedDispatcher
	operations map[uint64]catalog.Rule
	in         *intake.Intake
	publisher  *catalog.Publisher
	cancel     context.CancelFunc
	started    bool

	// Bot v2 Task 17 (health_probe.go): the Bot's probes of its own command path, the commands not yet answered, and
	// the health monitor that reports both.
	probes  probeTable
	load    commandLoad
	monitor *health.Monitor
	clk     clock.Clock // the probe's clock; nil is the system's
}

func (e *sealedEntry) current() (sealedIntakeConfig, *service.SealedDispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg, e.dispatcher
}

var errSealedNotConfigured = errors.New("bot sealed intake: no pass has configured the installation")

func (e *sealedEntry) OpenCommand(ctx context.Context, sealed []byte, user, device id.ID, now time.Time) (helper.OpenedCommand, error) {
	cfg, _ := e.current()
	if cfg.helper == nil {
		return helper.OpenedCommand{}, errSealedNotConfigured
	}
	return cfg.helper.OpenCommand(ctx, sealed, user, device, now)
}

func (e *sealedEntry) SealReply(ctx context.Context, key [32]byte, request id.ID, seq uint32, body []byte) ([]byte, error) {
	cfg, _ := e.current()
	if cfg.helper == nil {
		return nil, errSealedNotConfigured
	}
	return cfg.helper.SealReply(ctx, key, request, seq, body)
}

func (e *sealedEntry) CommandKey(ctx context.Context, community id.ID, now time.Time) ([]byte, error) {
	cfg, _ := e.current()
	if cfg.helper == nil {
		return nil, errSealedNotConfigured
	}
	return cfg.helper.CommandKey(ctx, community, now)
}

func (e *sealedEntry) Reply(ctx context.Context, device, request id.ID, seq uint32, sealed []byte, final bool) error {
	cfg, _ := e.current()
	if cfg.session == nil {
		return errSealedNotConfigured
	}
	return cfg.session.Reply(ctx, device, request, seq, sealed, final)
}

func (e *sealedEntry) PublishCatalog(ctx context.Context, community id.ID, commands []byte) (uint64, error) {
	cfg, _ := e.current()
	if cfg.session == nil {
		return 0, errSealedNotConfigured
	}
	return cfg.session.PublishCatalog(ctx, community, commands)
}

func (e *sealedEntry) PublishCommandKey(ctx context.Context, community id.ID, publication []byte) error {
	cfg, _ := e.current()
	if cfg.session == nil {
		return errSealedNotConfigured
	}
	return cfg.session.PublishCommandKey(ctx, community, publication)
}

func (e *sealedEntry) Dispatch(ctx context.Context, req protocol.SealedRequest, reply func(intake.Answer)) {
	_, d := e.current()
	d.Dispatch(ctx, req, reply)
}

func (e *sealedEntry) Held(user id.ID) bool {
	cfg, _ := e.current()
	return cfg.held != nil && cfg.held.Held(user)
}

func (e *sealedEntry) features() []catalog.Feature {
	cfg, _ := e.current()
	return catalog.FeaturesOf(cfg.capabilities)
}

func (e *sealedEntry) operation(op uint64) (intake.OperationRule, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rule, ok := e.operations[op]
	return intake.OperationRule{Permission: rule.Permission, Read: rule.Read, ChannelScoped: rule.ChannelScoped, ChannelArgs: rule.ChannelArgs}, ok
}

// sealedRegistry is the runtime's sealed intakes by installation. Its zero value is ready.
type sealedRegistry struct {
	mu      sync.Mutex
	entries map[id.ID]*sealedEntry
}

// entry returns the installation's entry, creating it (and its Authz) on first use; a community change replaces it.
func (s *sealedRegistry) entry(installation, community id.ID) *sealedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[id.ID]*sealedEntry{}
	}
	e := s.entries[installation]
	if e == nil || e.community != community {
		if e != nil {
			e.stop()
		}
		e = &sealedEntry{installation: installation, community: community, authz: intake.NewAuthz(community), queue: make(chan intake.RelayedCommand, sealedQueue)}
		s.entries[installation] = e
	}
	return e
}

func (s *sealedRegistry) lookup(installation id.ID) *sealedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[installation]
}

// retain stops and forgets every installation not in keep.
func (s *sealedRegistry) retain(keep map[id.ID]bool) {
	s.mu.Lock()
	var stopped []*sealedEntry
	for installation, e := range s.entries {
		if !keep[installation] {
			stopped = append(stopped, e)
			delete(s.entries, installation)
		}
	}
	s.mu.Unlock()
	for _, e := range stopped {
		e.stop()
	}
}

func (e *sealedEntry) stop() {
	e.mu.Lock()
	cancel := e.cancel
	e.cancel = nil
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// feed tees the installation's feed handler to its Authz: what it returns is what the feed consumer applies.
func (s *sealedRegistry) feed(installation, community id.ID, inner *installationFeed) Feed {
	if inner == nil {
		return nil
	}
	return sealedFeed{Feed: inner, authz: s.entry(installation, community).authz}
}

// sealedFeed applies each event to the feed handler and to the intake's Authz, in feed order.
type sealedFeed struct {
	Feed
	authz *intake.Authz
}

func (f sealedFeed) SnapshotBegin(ctx context.Context) {
	f.authz.SnapshotBegin()
	f.Feed.SnapshotBegin(ctx)
}

func (f sealedFeed) Member(ctx context.Context, ev MemberEvent) {
	f.authz.Member(ev.User, ev.Joined, ev.Roles, ev.Snapshot)
	f.Feed.Member(ctx, ev)
}

func (f sealedFeed) Role(ctx context.Context, ev RoleEvent) {
	f.authz.Role(ev.Role, ev.Allow, ev.Deny, ev.Position, ev.Deleted, ev.FromSnapshot)
	f.Feed.Role(ctx, ev)
}

func (f sealedFeed) Community(ctx context.Context, community, owner id.ID, snapshot bool) {
	f.authz.Owner(owner, snapshot)
	f.Feed.Community(ctx, community, owner, snapshot)
}

// ChannelOverwrites and MemberTimeout are kinds 9 and 10 (feed_authz.go): only the Authz keeps them.
func (f sealedFeed) ChannelOverwrites(_ context.Context, _, channel id.ID, overwrites []intake.Overwrite, deleted, snapshot bool) {
	f.authz.ChannelOverwrites(channel, overwrites, deleted, snapshot)
}

func (f sealedFeed) MemberTimeout(_ context.Context, _, user id.ID, until uint64, snapshot bool) {
	f.authz.MemberTimeout(user, until, snapshot)
}

func (f sealedFeed) SnapshotDone(ctx context.Context) {
	f.authz.SnapshotDone()
	f.Feed.SnapshotDone(ctx)
}

// sealedReminderAuthority answers the reminder handlers' membership question from the feed, and their source-channel
// question through the pass's own authority (Task 21b fix round 1, review I1): a reminder's source may be any text
// channel, which the feed names only when it is a Bot or voice channel. With no such authority every source is refused.
type sealedReminderAuthority struct {
	authz  *intake.Authz
	source reminderSourceChecker
}

type reminderSourceChecker interface {
	ReminderSource(context.Context, id.ID, id.ID) (bool, error)
}

func (a sealedReminderAuthority) ReminderMember(_ context.Context, user id.ID) (bool, error) {
	return a.authz.IsMember(user), nil
}

func (a sealedReminderAuthority) ReminderSource(ctx context.Context, actor, channel id.ID) (bool, error) {
	if a.source == nil {
		return false, nil
	}
	return a.source.ReminderSource(ctx, actor, channel)
}

// sealedReminders is the pass's reminder handlers as the sealed path serves them.
func sealedReminders(rem service.ReminderInteractions, authz *intake.Authz) service.ReminderInteractions {
	source, _ := rem.Authority.(reminderSourceChecker)
	rem.Authority = sealedReminderAuthority{authz: authz, source: source}
	rem.CreateEnabled = nil
	return rem
}

// sealedHandlers is the pass's handler map with the reminders served from the feed.
func sealedHandlers(cfg sealedIntakeConfig, authz *intake.Authz) map[protocol.Operation]service.InteractionHandlerSpec {
	out := make(map[protocol.Operation]service.InteractionHandlerSpec, len(cfg.handlers)+5)
	for op, h := range cfg.handlers {
		out[op] = h
	}
	if cfg.reminders != nil {
		rem := sealedReminders(*cfg.reminders, authz)
		for op, h := range rem.Handlers() {
			out[op] = h
		}
	}
	// Task 21b fix round 1: polls answer from the feed too (no dillad read per member), and many members' reads of one
	// poll within 100 ms share one computation of the poll and its voters. Poll creation still checks the capability.
	if cfg.polls != nil {
		p := *cfg.polls
		p.Authority = service.FeedPollAuthority{Feed: authz}
		p.Reads = service.NewPollReadCache()
		for op, h := range p.Handlers() {
			out[op] = h
		}
	}
	return out
}

// SetSealedIntake configures the installation's sealed intake from a pass and starts it on first use: its
// workers and its catalog publisher. A capability change republishes the catalog at once.
func (r *botGatewayRuntime) SetSealedIntake(cfg sealedIntakeConfig) {
	if cfg.installation.IsZero() || cfg.community.IsZero() || cfg.helper == nil || cfg.session == nil {
		return
	}
	if cfg.log == nil {
		cfg.log = r.log.With("installation_id", cfg.installation.String())
	}
	e := r.sealed.entry(cfg.installation, cfg.community)
	features := catalog.FeaturesOf(cfg.capabilities)
	e.mu.Lock()
	changed := e.started && e.cfg.capabilities != cfg.capabilities
	// A new Bot session (fix round 1 M-2): dillad may hold an older publication, or none after a restore, so the key
	// and the catalog are sent again whatever the publisher believes it sent.
	renewed := e.started && e.cfg.session != cfg.session
	e.cfg = cfg
	e.dispatcher = &service.SealedDispatcher{Community: cfg.community, Handlers: sealedHandlers(cfg, e.authz), Feed: e.authz,
		Status: r.botStatusSource(cfg.installation, cfg.musicStarts)}
	e.operations = catalog.Operations(features)
	start := !e.started
	if start {
		e.started = true
		e.in = intake.New(e, e.authz, e, e, clock.System())
		e.in.SetHeld(e)
		// At most one parked start-up command per worker (final review I2).
		e.in.SetParkCap(sealedWorkers)
		e.in.SetOperations(e.operation)
		e.in.SetLogger(cfg.log)
		e.publisher = catalog.NewPublisher(e, e, clock.System(), e.features)
		e.publisher.SetLogger(cfg.log)
		// A command sealed to a key the helper does not hold: send the current key and catalog again even when their
		// bytes are unchanged, so the client's refetch finds them (fix round 1 M-2), at most once a minute (fix round
		// 2, N-3).
		e.in.SetKeyStale(e.publisher.Stale)
		// Fail closed until the installation's feed exists (Held is false for an installation with no feed) and has
		// applied a snapshot (the Authz's own readiness): 503 E_BOT_OFFLINE, retryable.
		installation := cfg.installation
		e.in.SetReady(func() bool { return r.feedHandler(installation) != nil })
		ctx, cancel := context.WithCancel(r.ctx)
		e.cancel = cancel
		for range sealedWorkers {
			go e.work(ctx)
		}
		go e.publisher.Run(ctx, cfg.community)
		e.startHealth(ctx, func() time.Duration {
			current, _ := e.current()
			if current.musicStartP95 == nil {
				return 0
			}
			return current.musicStartP95()
		}, clock.System())
	}
	in, publisher := e.in, e.publisher
	e.mu.Unlock()
	in.SetBotUser(cfg.botUser)
	if renewed {
		publisher.Forget()
	}
	if changed || renewed {
		publisher.Changed()
	}
}

func (e *sealedEntry) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-e.queue:
			e.mu.Lock()
			in := e.in
			e.mu.Unlock()
			in.Handle(ctx, cmd)
			// Fix round 2, M3: counted until its handling ends, so a command stuck in a worker (a hanging helper call,
			// a blocked handler) shows as waiting even while other workers are free.
			e.load.answered(cmd.RequestID)
		}
	}
}

// decodeRelayedCommand reads op 64, [request_id, community_id, user_id, device_id, sealed] (gateway.BotCommandPayload).
func decodeRelayedCommand(payload []cbor.RawMessage) (intake.RelayedCommand, error) {
	var cmd intake.RelayedCommand
	if len(payload) != 5 {
		return cmd, errFeedShape
	}
	for i, dst := range []*id.ID{&cmd.RequestID, &cmd.CommunityID, &cmd.UserID, &cmd.DeviceID} {
		v, err := feedField[id.ID](payload[i])
		if err != nil || v.IsZero() {
			return intake.RelayedCommand{}, errFeedShape
		}
		*dst = v
	}
	sealed, err := feedField[[]byte](payload[4])
	if err != nil || len(sealed) == 0 {
		return intake.RelayedCommand{}, errFeedShape
	}
	cmd.Sealed = sealed
	return cmd, nil
}

// relayCommand queues one op 64 for the installation's sealed intake. It never blocks the delivery loop.
func (r *botGatewayRuntime) relayCommand(installation id.ID, payload []cbor.RawMessage, ready func(context.Context) bool) {
	r.relayCommandFor(installation, id.ID{}, payload, ready)
}

// relayCommandFor is relayCommand on the connection of the installation's community: a command that arrives before
// the installation's first pass configured its sealed intake (a restarted Bot gets the commands dillad carried across
// its reset right after its snapshot, Task 21b fix round 2) waits in the intake's queue, whose workers start with the
// pass, instead of being dropped.
func (r *botGatewayRuntime) relayCommandFor(installation, community id.ID, payload []cbor.RawMessage, ready func(context.Context) bool) {
	cmd, err := decodeRelayedCommand(payload)
	// The feed barrier (fix round 1 I-3): the command is opened and authorised only after the events dillad sent
	// before it are applied.
	cmd.Ready = ready
	e := r.sealed.lookup(installation)
	if e == nil && err == nil && !community.IsZero() && cmd.CommunityID == community {
		e = r.sealed.entry(installation, community)
	}
	switch {
	case err != nil:
		r.log.Warn("bot command relay malformed", "installation_id", installation.String())
	case e == nil || cmd.CommunityID != e.community:
		r.log.Warn("bot command relay for no sealed intake", "installation_id", installation.String())
	default:
		e.load.enqueue(cmd.RequestID, time.Now())
		select {
		case e.queue <- cmd:
		default:
			e.load.answered(cmd.RequestID)
			r.log.Warn("bot command intake queue full; command dropped", "installation_id", installation.String())
		}
	}
}

// installationHeld is the runtime's held-user query for one installation (Task 14's botGatewayRuntime.Held).
type installationHeld struct {
	r            *botGatewayRuntime
	installation id.ID
}

func (h installationHeld) Held(user id.ID) bool { return h.r.Held(h.installation, user) }

// newSealedIntakeConfig is a pass's sealed intake configuration: the pass's feature handlers with the utilities'
// polls and reminders (as the interaction dispatcher serves them, without its capability discovery).
func newSealedIntakeConfig(r *botGatewayRuntime, installation, community, botUser id.ID, capabilities uint64, h sealedHelper, session sealedSession,
	handlers map[protocol.Operation]service.InteractionHandlerSpec, utilities *utilityRuntime) sealedIntakeConfig {
	cfg := sealedIntakeConfig{installation: installation, community: community, botUser: botUser, capabilities: capabilities,
		helper: h, session: session, handlers: assembleUtilityInteractionHandlers(handlers, utilities),
		held: installationHeld{r, installation}}
	if utilities != nil {
		reminders := utilities.reminders
		cfg.reminders = &reminders
		polls := utilities.polls
		cfg.polls = &polls
	}
	return cfg
}

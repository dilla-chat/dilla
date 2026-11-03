//go:build linux

package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/moderation"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/id"
)

type groupEventHighWaterSource interface {
	Binding() native.Binding
	ReadGroupEvents(context.Context, uint64, uint64) (native.GroupEventPage, error)
}

type autoModerationEventStateEntry struct {
	source                groupEventHighWaterSource
	community             id.ID
	revision              uint64
	replayThrough         uint64
	replayReceivedThrough uint64
	runtime               *service.AutoModerationRuntime
}

// autoModerationEventState coordinates each installation's durable evaluator,
// pending actions and activation barrier. Native state restores them before
// classifying historical deliveries; legacy installs seed a barrier once.
type autoModerationEventState struct {
	lifecycle installationLifecycle
	epoch     uint64 // removal fence, protected by mu
	mu        sync.Mutex
	entries   map[id.ID]autoModerationEventStateEntry
	now       func() time.Time
}

func newAutoModerationEventState() *autoModerationEventState {
	return &autoModerationEventState{entries: make(map[id.ID]autoModerationEventStateEntry), now: time.Now}
}

func logOnlyModerationConfig() moderation.Config {
	return moderation.Config{Actions: map[moderation.Rule]moderation.Action{
		moderation.RuleRate: moderation.ActionLog, moderation.RuleDuplicate: moderation.ActionLog, moderation.RuleMentions: moderation.ActionLog,
	}}
}

func configureAutoModeration(ctx context.Context, state *autoModerationEventState, n *native.Client, session *client.Session,
	installation, community id.ID, logger *slog.Logger) (*service.AutoModerationRuntime, uint64, uint64, *service.ModerationCaseSink, error) {
	if n == nil || session == nil {
		return nil, 0, 0, nil, errors.New("missing automatic moderation client or session")
	}
	cfg, revision, err := moderation.NewStore(n).Load(ctx)
	if err != nil {
		if logger != nil {
			logger.Warn("bot automatic moderation settings unavailable", "installation_id", installation.String(), "error", err.Error())
		}
		return nil, 0, 0, nil, err
	}
	authority, sink, err := prepareAutoModerationDependencies(n, session, community, cfg)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	runtime, replayThrough, replayReceivedThrough, err := state.Configure(ctx, n, installation, community, revision, cfg, authority, sink)
	return runtime, replayThrough, replayReceivedThrough, sink, err
}

func prepareAutoModerationDependencies(n *native.Client, session *client.Session, community id.ID, cfg moderation.Config) (service.ModerationActorAuthority, *service.ModerationCaseSink, error) {
	var authority service.ModerationActorAuthority
	if cfg.Enabled {
		resolved, err := service.NewCommunityActorAuthority(session, community, nil)
		if err != nil {
			return nil, nil, err
		}
		authority = resolved
	}
	// Disabled/log-only policies must still reconcile receipts of older pending
	// decisions. The runtime alone decides whether a request is receipt-only;
	// constructing this adapter does not authorize a new enforcement effect.
	backend, err := service.NewModerationRemovalBackend(n, session)
	if err != nil {
		return nil, nil, err
	}
	sink, err := service.NewModerationCaseSink(n, backend, nil)
	return authority, sink, err
}

func setModerationActionsLogOnly(cfg *moderation.Config) {
	if cfg == nil {
		return
	}
	for rule, action := range cfg.Actions {
		if action == moderation.ActionRemove || action == moderation.ActionWarn || action == moderation.ActionTimeout {
			cfg.Actions[rule] = moderation.ActionLog
		}
	}
}

func (s *autoModerationEventState) Configure(ctx context.Context, source groupEventHighWaterSource, installation, community id.ID, revision uint64,
	cfg moderation.Config, authority service.ModerationActorAuthority, sink service.ModerationActionSink) (*service.AutoModerationRuntime, uint64, uint64, error) {
	if s == nil || ctx == nil || source == nil || installation.IsZero() || community.IsZero() {
		return nil, 0, 0, errors.New("missing automatic moderation event state dependency")
	}
	binding := source.Binding()
	if binding.Installation != installation || binding.Community != community || binding.Installation.IsZero() || binding.Community.IsZero() {
		return nil, 0, 0, errors.New("automatic moderation event source binding mismatch")
	}
	unlock, available := s.lifecycle.tryLock(installation)
	if !available {
		// A command is publishing settings. Keep the active policy for this
		// background pass without waiting on its helper or persistence request.
		s.mu.Lock()
		entry, ok := s.entries[installation]
		s.mu.Unlock()
		if ok && entry.community == community && entry.runtime != nil {
			return entry.runtime, entry.replayThrough, entry.replayReceivedThrough, nil
		}
		return nil, 0, 0, errors.New("automatic moderation runtime activation in progress")
	}
	defer unlock()
	s.mu.Lock()
	epoch := s.epoch
	entry, exists := s.entries[installation]
	s.mu.Unlock()
	if exists && entry.community == community && entry.revision > revision {
		// A stale reconciliation must not roll back an acknowledged settings PUT.
		return entry.runtime, entry.replayThrough, entry.replayReceivedThrough, nil
	}
	// A replacement helper opens the same durable store through a new client.
	// Reattach its checkpoints rather than retaining the retired client's handles.
	if exists && entry.source == source && entry.community == community && entry.revision == revision && entry.runtime.ConfigurationReady() {
		entry.runtime.UpdateActionDependencies(authority, sink)
		s.mu.Lock()
		if s.epoch == epoch {
			entry.source = source
			s.entries[installation] = entry
		}
		s.mu.Unlock()
		return entry.runtime, entry.replayThrough, entry.replayReceivedThrough, nil
	}
	// Capture before sampling the journal. Source-sync pages may arrive later,
	// so their authoritative server receipt times are the second replay fence.
	replayReceivedThrough := uint64(s.now().UnixMilli())
	page, err := source.ReadGroupEvents(ctx, 0, 1)
	if err != nil {
		return nil, 0, 0, err
	}
	if page.Next > page.HighWater || page.HighWater > math.MaxInt64 {
		return nil, 0, 0, errors.New("invalid automatic moderation event high-water mark")
	}
	runtime := service.NewAutoModerationRuntime(cfg, authority, sink, s.now)
	_, err = runtime.ActivateConfig(ctx, service.AutoModerationActivation{Config: cfg, Revision: revision, ReplayThrough: page.HighWater, ReplayReceivedThrough: replayReceivedThrough, Authority: authority, Sink: sink}, func() error { return nil })
	if err != nil {
		return nil, 0, 0, err
	}
	if durable, ok := source.(service.AutoModerationStateClient); ok {
		if err := runtime.AttachDurableState(ctx, durable); err != nil {
			return nil, 0, 0, err
		}
	}
	replayThrough, replayReceivedThrough := runtime.ReplayBoundary()
	entry = autoModerationEventStateEntry{source: source, community: community, revision: revision, replayThrough: replayThrough,
		replayReceivedThrough: replayReceivedThrough, runtime: runtime}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.epoch != epoch {
		return nil, 0, 0, errors.New("automatic moderation runtime retired during activation")
	}
	s.entries[installation] = entry
	return runtime, entry.replayThrough, entry.replayReceivedThrough, nil
}

func (s *autoModerationEventState) RetainOnly(installations []id.ID) {
	if s == nil {
		return
	}
	active := make(map[id.ID]struct{}, len(installations))
	for _, installation := range installations {
		active[installation] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch++ // also fences an initial Configure that has not published an entry
	for installation := range s.entries {
		if _, ok := active[installation]; !ok {
			delete(s.entries, installation)
		}
	}
}

func (s *autoModerationEventState) Disable(installation id.ID) {
	if s == nil || installation.IsZero() {
		return
	}
	s.mu.Lock()
	s.epoch++
	delete(s.entries, installation)
	s.mu.Unlock()
}

// Configuration and event adapters share the installation's active runtime.
type autoModerationDependencies func(moderation.Config) (service.ModerationActorAuthority, service.ModerationActionSink, error)

type autoModerationConfigRuntime struct {
	state                   *autoModerationEventState
	source                  groupEventHighWaterSource
	installation, community id.ID
	prepare                 autoModerationDependencies
}

func (s *autoModerationEventState) ConfigRuntime(source groupEventHighWaterSource, installation, community id.ID, prepare autoModerationDependencies) service.AutoModerationConfigRuntime {
	return autoModerationConfigRuntime{state: s, source: source, installation: installation, community: community, prepare: prepare}
}

func (a autoModerationConfigRuntime) PersistConfig(ctx context.Context, cfg moderation.Config, expectedRevision uint64, persist func() error) error {
	if a.state == nil || a.source == nil || a.prepare == nil || ctx == nil || persist == nil || expectedRevision >= math.MaxInt64 {
		return errors.New("missing automatic moderation activation dependency")
	}
	binding := a.source.Binding()
	if binding.Installation != a.installation || binding.Community != a.community {
		return errors.New("automatic moderation activation binding mismatch")
	}
	unlock := a.state.lifecycle.lock(a.installation)
	defer unlock()
	a.state.mu.Lock()
	entry, ok := a.state.entries[a.installation]
	a.state.mu.Unlock()
	if !ok || entry.community != a.community || entry.runtime == nil || entry.source != a.source {
		return errors.New("automatic moderation runtime unavailable")
	}
	if entry.revision > expectedRevision {
		// The durable transaction validates the exact request receipt; do not reset
		// observations or slide the activation boundary on a successful retry.
		return persist()
	}
	// Prepare every fallible dependency before committing the settings CAS.
	authority, sink, err := a.prepare(cfg)
	if err != nil {
		return err
	}
	if cfg.Enabled && (authority == nil || sink == nil) {
		return errors.New("missing automatic moderation action dependency")
	}
	boundary := uint64(a.state.now().UnixMilli())
	page, err := a.source.ReadGroupEvents(ctx, 0, 1)
	if err != nil {
		return err
	}
	if page.Next > page.HighWater || page.HighWater > math.MaxInt64 {
		return errors.New("invalid automatic moderation activation high-water mark")
	}
	activation := service.AutoModerationActivation{Config: cfg, Revision: expectedRevision + 1, ReplayThrough: page.HighWater, ReplayReceivedThrough: boundary, Authority: authority, Sink: sink}
	// Only the installation lifecycle fence spans persistence. Registry reads
	// and retirement remain available while the runtime configuration CAS runs.
	boundary, err = entry.runtime.ActivateConfig(ctx, activation, persist)
	if err != nil {
		return err
	}
	entry.revision, entry.replayThrough, entry.replayReceivedThrough = activation.Revision, activation.ReplayThrough, boundary
	a.state.mu.Lock()
	defer a.state.mu.Unlock()
	current, exists := a.state.entries[a.installation]
	if !exists || current.runtime != entry.runtime || current.source != a.source {
		return errors.New("automatic moderation runtime retired during activation")
	}
	a.state.entries[a.installation] = entry
	return nil
}

type autoModerationEventHandler struct {
	state                   *autoModerationEventState
	installation, community id.ID
}

func (s *autoModerationEventState) EventHandler(installation, community id.ID) automaticModerationEventHandler {
	return autoModerationEventHandler{s, installation, community}
}
func (h autoModerationEventHandler) Handle(ctx context.Context, event native.GroupEvent, replay bool, mentions []string) (moderation.Decision, error) {
	h.state.mu.Lock()
	entry, ok := h.state.entries[h.installation]
	h.state.mu.Unlock()
	if !ok || entry.community != h.community || entry.runtime == nil {
		return moderation.Decision{Ignored: true}, errors.New("automatic moderation runtime unavailable")
	}
	// Do not hold the registry mutex during Handle: interaction persistence takes
	// the registry mutex before the runtime fence, and runtime callbacks may block.
	return entry.runtime.Handle(ctx, event, replay, mentions)
}

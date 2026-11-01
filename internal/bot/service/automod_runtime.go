package service

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/native"
)

// ModerationAction is the bounded information handed to an action or audit
// sink. The decrypted message body is never retained here.
type ModerationAction struct {
	Community, Group, Channel, Actor, Message string
	SourceRevision                            uint64
	Decision                                  moderation.Decision
	Excerpt                                   string
	ReceivedAt                                time.Time
	runtimeGenerated                          bool
	receiptOnly                               bool
	removalProof                              []byte
}

// ModerationActionSink is the application boundary for logging, warnings, or
// future enforcement primitives. The runtime itself does not remove members
// or apply timeouts.
type ModerationActionSink interface {
	ApplyModerationAction(context.Context, ModerationAction) error
}

// ModerationActorAuthority resolves privileged roles from trusted community
// membership state, never from the message event itself.
type ModerationActorAuthority interface {
	ModerationActorRoles(context.Context, native.GroupEvent) (owner, admin bool, err error)
}

// AutoModerationRuntime evaluates authenticated, decrypted native group
// events. Callers must pass events obtained from native.ReadGroupEvents.
type AutoModerationRuntime struct {
	handleMu              sync.Mutex
	mu                    sync.RWMutex
	configMu              sync.RWMutex
	engine                *moderation.Engine
	authority             ModerationActorAuthority
	sink                  ModerationActionSink
	now                   func() time.Time
	enabled               bool
	revision              uint64
	revisionKnown         bool
	replayThrough         uint64
	replayReceivedThrough uint64
	state                 *autoModerationStore
	pending               map[string]pendingModeration
	stateDirty            bool
	activationFailed      bool
}

// AutoModerationStageError exposes only a stable processing stage in its
// message while retaining a cause for errors.Is/errors.As callers.
type AutoModerationStageError struct {
	Stage     string
	Code      string
	Status    int
	Deferrals uint64
	cause     error
}

func (e AutoModerationStageError) Error() string {
	return "automatic moderation " + e.Stage + " failed"
}

func (e AutoModerationStageError) Unwrap() error { return e.cause }

func NewAutoModerationRuntime(cfg moderation.Config, authority ModerationActorAuthority, sink ModerationActionSink, now func() time.Time) *AutoModerationRuntime {
	if now == nil {
		now = time.Now
	}
	return &AutoModerationRuntime{engine: moderation.New(cfg), authority: authority, sink: sink, now: now, enabled: cfg.Enabled, pending: make(map[string]pendingModeration)}
}

// UpdateActionDependencies refreshes authenticated service handles while
// preserving the process-local rolling counters for the same configuration.
func (r *AutoModerationRuntime) UpdateActionDependencies(authority ModerationActorAuthority, sink ModerationActionSink) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.authority = authority
	r.sink = sink
	r.mu.Unlock()
}

// AutoModerationActivation couples a policy revision with its ready action handles
// and the conservative delivery/receipt boundary captured when it is activated.
type AutoModerationActivation struct {
	Config                               moderation.Config
	Revision                             uint64
	ReplayThrough, ReplayReceivedThrough uint64
	Authority                            ModerationActorAuthority
	Sink                                 ModerationActionSink
}

func (r *AutoModerationRuntime) PersistConfig(ctx context.Context, cfg moderation.Config, expectedRevision uint64, persist func() error) error {
	_, err := r.persistConfig(ctx, AutoModerationActivation{Config: cfg, Revision: expectedRevision + 1}, persist, false)
	return err
}

// ActivateConfig publishes dependencies, policy and replay boundaries under the
// same fence. An exact persisted retry checks its receipt without replacing state.
func (r *AutoModerationRuntime) ActivateConfig(ctx context.Context, activation AutoModerationActivation, persist func() error) (uint64, error) {
	if activation.Config.Enabled && (activation.Authority == nil || activation.Sink == nil) {
		return 0, errors.New("missing automatic moderation activation dependency")
	}
	return r.persistConfig(ctx, activation, persist, true)
}

func (r *AutoModerationRuntime) persistConfig(ctx context.Context, activation AutoModerationActivation, persist func() error, dependencies bool) (uint64, error) {
	if r == nil || ctx == nil || persist == nil {
		return 0, errors.New("missing automatic moderation config update dependency")
	}
	r.configMu.Lock()
	defer r.configMu.Unlock()
	if r.engine == nil {
		return 0, errors.New("missing automatic moderation runtime")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := persist(); err != nil {
		return 0, err
	}
	if r.revisionKnown && activation.Revision <= r.revision {
		return r.replayReceivedThrough, nil
	}
	engine := moderation.New(activation.Config)
	if dependencies {
		// Include receipts received while the successful CAS was in flight. This
		// cannot fail after commit and does not perform another source read.
		activation.ReplayReceivedThrough = max(activation.ReplayReceivedThrough, uint64(r.now().UnixMilli()))
		if r.state != nil {
			meta := r.state.meta
			meta.Policy = activation.Revision
			meta.ReplayThrough = activation.ReplayThrough
			meta.ReplayReceivedThrough = activation.ReplayReceivedThrough
			meta.Frontier = 0
			if err := r.state.saveEvaluation(ctx, engine, meta, nil); err != nil {
				r.activationFailed = true
				return 0, AutoModerationStageError{Stage: "activation_checkpoint", cause: err}
			}
			r.state.shards = make(map[byte][]byte)
		}
		r.mu.Lock()
		r.authority, r.sink = activation.Authority, activation.Sink
		r.mu.Unlock()
		r.replayThrough, r.replayReceivedThrough = activation.ReplayThrough, activation.ReplayReceivedThrough
	}
	r.activationFailed = false
	r.engine = engine
	r.enabled = activation.Config.Enabled
	r.revision, r.revisionKnown = activation.Revision, true
	return r.replayReceivedThrough, nil
}

// Handle evaluates one decrypted message. replay must be derived from the
// durable event cursor/recovery path, never from message contents.
func (r *AutoModerationRuntime) Handle(ctx context.Context, event native.GroupEvent, replay bool, mentionIDs []string) (moderation.Decision, error) {
	if r == nil {
		return moderation.Decision{Action: moderation.ActionLog, Ignored: true}, errors.New("missing moderation runtime")
	}
	if event.DeliveryID == 0 || event.Community.IsZero() || event.Group.IsZero() || event.Channel.IsZero() || event.Message.IsZero() ||
		event.SourceSequence == 0 || event.SenderUser.IsZero() || event.SenderDevice.IsZero() ||
		event.SenderKind > 1 || event.SenderTier > 1 || event.EnvelopeType > 1 || event.Body == nil || event.ReceivedAt == 0 || event.ReceivedAt > math.MaxInt64 {
		return moderation.Decision{Action: moderation.ActionLog, Ignored: true}, nil
	}
	r.handleMu.Lock()
	defer r.handleMu.Unlock()
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	if r.activationFailed {
		return moderation.Decision{}, moderationDeferral("activation_checkpoint", ErrModerationCheckpoint)
	}
	if r.stateDirty {
		if err := r.restoreDurable(ctx); err != nil {
			return moderation.Decision{}, moderationDeferral("checkpoint_restore", err)
		}
	}
	// These boundaries are read under the policy fence too: a handler that took
	// a registry snapshot before a save must still use the acknowledged policy.
	if event.DeliveryID <= r.replayThrough || event.ReceivedAt <= math.MaxInt64/1000 &&
		event.ReceivedAt*1000 <= r.replayReceivedThrough {
		replay = true
	}
	engine, enabled := r.engine, r.enabled
	if engine == nil {
		return moderation.Decision{Action: moderation.ActionLog, Ignored: true}, errors.New("missing moderation runtime")
	}
	r.mu.RLock()
	authority, sink := r.authority, r.sink
	r.mu.RUnlock()
	key := pendingKey(ModerationAction{Group: event.Group.String(), SourceRevision: event.SourceSequence})
	if pending, ok := r.pending[key]; ok {
		if pending.Message != event.Message.String() || pending.Community != event.Community.String() || pending.Actor != event.SenderUser.String() || pending.Channel != event.Channel.String() {
			return moderation.Decision{}, ErrModerationCheckpoint
		}
		action := pending.action()
		action.receiptOnly = pending.Policy != r.revision || !enabled || r.now().Sub(action.ReceivedAt) > moderation.DuplicateWindow
		if !action.receiptOnly {
			if authority == nil {
				return action.Decision, AutoModerationStageError{Stage: "actor_authorization"}
			}
			o, a, err := authority.ModerationActorRoles(ctx, event)
			if err != nil {
				return action.Decision, AutoModerationStageError{Stage: "actor_authorization"}
			}
			action.receiptOnly = o || a
		}
		return r.finishPending(ctx, key, action, sink)
	}
	if r.state != nil && event.DeliveryID <= r.state.meta.Frontier {
		return moderation.Decision{DuplicateDelivery: true, Action: moderation.ActionLog}, nil
	}
	if len(r.pending) >= MaxModerationPending {
		return moderation.Decision{}, moderationDeferral("pending_capacity", ErrModerationPendingCapacity)
	}
	var owner, admin bool
	if enabled {
		if authority == nil {
			return moderation.Decision{Action: moderation.ActionLog, Ignored: true}, AutoModerationStageError{Stage: "actor_authorization"}
		}
		if sink == nil {
			return moderation.Decision{Action: moderation.ActionLog, Ignored: true}, AutoModerationStageError{Stage: "case_sink"}
		}
		var err error
		owner, admin, err = authority.ModerationActorRoles(ctx, event)
		if err != nil {
			return moderation.Decision{Action: moderation.ActionLog, Ignored: true}, AutoModerationStageError{Stage: "actor_authorization"}
		}
	}
	// Native group events carry Unix seconds, while this runtime uses Go time
	// values and the replay fence is stored in milliseconds.
	receivedAt := time.Unix(int64(event.ReceivedAt), 0)
	now := r.now()
	input := moderation.Input{
		CommunityID: event.Community.String(), SourceID: event.Message.String(), Revision: event.SourceSequence,
		UserID: event.SenderUser.String(), Bot: event.SenderKind == 1, Owner: owner, Admin: admin, Edit: event.EnvelopeType == 1, ReceivedAt: receivedAt,
		Body: *event.Body, MentionIDs: append([]string(nil), mentionIDs...), Replay: replay,
	}
	evaluationTime := now
	// The evaluator deliberately rejects stale live messages. Historical replay
	// still rebuilds its rolling state, while Input.Replay suppresses punishment.
	if replay && (receivedAt.After(now) || now.Sub(receivedAt) > moderation.DuplicateWindow) {
		evaluationTime = receivedAt
	}
	decision := engine.Evaluate(input, evaluationTime)
	needsAction := !replay && !decision.Ignored && !decision.DuplicateDelivery && len(decision.Rules) > 0 && sink != nil
	action := ModerationAction{Community: event.Community.String(), Channel: event.Channel.String(),
		Group: event.Group.String(), Actor: event.SenderUser.String(), Message: event.Message.String(), SourceRevision: event.SourceSequence, Decision: decision,
		Excerpt: moderation.Excerpt(*event.Body), ReceivedAt: receivedAt, runtimeGenerated: true}
	if needsAction {
		if preparer, ok := sink.(interface {
			PrepareModerationAction(context.Context, ModerationAction) (ModerationAction, error)
		}); ok {
			prepared, err := preparer.PrepareModerationAction(ctx, action)
			if err != nil {
				if r.state != nil {
					r.stateDirty = true
					if restoreErr := r.restoreDurable(ctx); restoreErr != nil {
						return decision, moderationDeferral("checkpoint_restore", restoreErr)
					}
				}
				return decision, moderationDeferral("action_preparation", err)
			}
			action = prepared
		}
	}
	pending := pendingFromAction(action, r.revision)
	if r.state != nil {
		meta := r.state.meta
		meta.Policy = r.revision
		meta.ReplayThrough = r.replayThrough
		meta.ReplayReceivedThrough = r.replayReceivedThrough
		meta.Frontier = event.DeliveryID
		var toSave *pendingModeration
		if needsAction {
			toSave = &pending
		}
		if err := r.state.saveEvaluation(ctx, engine, meta, toSave); err != nil {
			// Restore the last durable evaluator before retrying this event. No
			// external action has run and the journal has not been acknowledged.
			r.stateDirty = true
			if restoreErr := r.restoreDurable(ctx); restoreErr != nil {
				return decision, moderationDeferral("checkpoint_restore", restoreErr)
			}
			return decision, moderationDeferral("checkpoint_persistence", err)
		}
	}
	if !needsAction {
		return decision, nil
	}
	r.pending[key] = pending
	return r.finishPending(ctx, key, action, sink)
}

func (r *AutoModerationRuntime) finishPending(ctx context.Context, key string, action ModerationAction, sink ModerationActionSink) (moderation.Decision, error) {
	decision := action.Decision
	if sink == nil {
		return decision, AutoModerationStageError{Stage: "case_sink"}
	}
	if action.receiptOnly {
		if _, ok := sink.(*ModerationCaseSink); !ok {
			return r.completeRefused(ctx, key, action, sink)
		}
	}
	if err := sink.ApplyModerationAction(ctx, action); err != nil {
		if errors.Is(err, ErrModerationActionRefused) {
			// A definitive authorization refusal proves no action was accepted.
			// Consume the event as log-only so this shared journal cursor can move;
			// unknown and transient errors remain retryable below.
			return r.completeRefused(ctx, key, action, sink)
		}
		var stageErr AutoModerationStageError
		if errors.As(err, &stageErr) {
			return decision, stageErr
		}
		return decision, AutoModerationStageError{Stage: "case_sink"}
	}
	if r.state != nil {
		if err := r.state.complete(ctx, key); err != nil {
			r.stateDirty = true
			return decision, AutoModerationStageError{Stage: "pending_completion", cause: err}
		}
	}
	delete(r.pending, key)
	return decision, nil
}

func (r *AutoModerationRuntime) completeRefused(ctx context.Context, key string, action ModerationAction, sink ModerationActionSink) (moderation.Decision, error) {
	action.Decision.EnforcementRefused = true
	action.Decision.Action = moderation.ActionLog
	action.Decision.Timeout = 0
	action.receiptOnly = false
	// The log case is durable before releasing the pending slot and journal.
	if sink != nil {
		if err := sink.ApplyModerationAction(ctx, action); err != nil {
			return action.Decision, AutoModerationStageError{Stage: "refusal_persistence", cause: err}
		}
	}
	if r.state != nil {
		if err := r.state.complete(ctx, key); err != nil {
			r.stateDirty = true
			return action.Decision, AutoModerationStageError{Stage: "pending_completion", cause: err}
		}
	}
	delete(r.pending, key)
	return action.Decision, nil
}

var moderationCheckpointDeferrals atomic.Uint64
var moderationCapacityDeferrals atomic.Uint64

func moderationDeferral(stage string, cause error) AutoModerationStageError {
	counter := &moderationCheckpointDeferrals
	if stage == "pending_capacity" {
		counter = &moderationCapacityDeferrals
	}
	return AutoModerationStageError{Stage: stage, Deferrals: counter.Add(1), cause: cause}
}

func (r *AutoModerationRuntime) ConfigurationReady() bool {
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	return !r.activationFailed
}

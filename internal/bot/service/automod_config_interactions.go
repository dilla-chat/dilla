package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	autoModerationManageMessagesBit  = uint64(1 << 2)
	autoModerationModerateMembersBit = uint64(1 << 24)
	autoModerationConfigVersion      = uint64(1)
	autoModerationConfigMaxBytes     = 2400
)

type autoModerationConfigAuthority interface {
	BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error)
}

type autoModerationConfigStore interface {
	Binding() nativeBinding
	Load(context.Context) (moderation.Config, uint64, error)
	SaveWithID(context.Context, id.ID, moderation.Config, uint64) error
}

// AutoModerationGrantRedeemer consumes a one-use human step-up grant whose
// server-side tuple is bound to this authenticated interaction and request.
type AutoModerationGrantRedeemer interface {
	RedeemAutoModerationGrant(context.Context, string, AutoModerationGrantBinding) (bool, error)
}

type AutoModerationConfigRuntime interface {
	PersistConfig(context.Context, moderation.Config, uint64, func() error) error
}

func autoModerationConfigRuntimeUnavailable(runtime AutoModerationConfigRuntime) bool {
	if runtime == nil {
		return true
	}
	// The production runtime is a pointer receiver. A nil *AutoModerationRuntime
	// stored in this interface is non-nil, and calling PersistConfig on it would
	// consume a grant before failing. Treat that typed nil as unavailable too.
	concrete, ok := runtime.(*AutoModerationRuntime)
	return ok && concrete == nil
}

// AutoModerationGrantBinding contains only the tuple and config digest needed
// for server-side grant redemption. It deliberately excludes plaintext config.
type AutoModerationGrantBinding struct {
	RequestID, CommunityID, ActorUser, ActorDevice, Group, EventChannel id.ID
	Operation                                                           protocol.Operation
	ExpectedRevision, RequestExpires                                    uint64
	ConfigHash                                                          [32]byte
}

// nativeBinding is aliased locally to keep the handler's persistence contract
// limited to the binding value it needs for scope validation.
type nativeBinding = native.Binding

// AutoModerationConfigInteractions exposes encrypted settings to current
// community managers. Its actor is supplied by InteractionDispatcher from the
// authenticated native event; arguments never name an actor.
type AutoModerationConfigInteractions struct {
	Authority     autoModerationConfigAuthority
	Store         autoModerationConfigStore
	GrantRedeemer AutoModerationGrantRedeemer
	Runtime       AutoModerationConfigRuntime
	CommunityID   id.ID
}

func (h AutoModerationConfigInteractions) Handlers() map[protocol.Operation]InteractionHandlerSpec {
	return map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpAutoModerationGet: {RecomputeOnReplay: true, Handle: h.get},
		protocol.OpAutoModerationPut: {RevisionInHandler: true, Handle: h.put},
	}
}

func (h AutoModerationConfigInteractions) authorize(ctx context.Context, actor InteractionActor, community id.ID) (client.ActorAuthority, bool, error) {
	if h.Authority == nil || h.Store == nil || h.CommunityID.IsZero() || actor.User.IsZero() {
		return client.ActorAuthority{}, false, errors.New("missing automatic moderation config dependency")
	}
	if actor.Kind != 0 {
		return client.ActorAuthority{}, false, nil
	}
	if community != h.CommunityID {
		return client.ActorAuthority{}, false, nil
	}
	binding := h.Store.Binding()
	if binding.Community != h.CommunityID || binding.Instance.IsZero() || binding.Installation.IsZero() {
		return client.ActorAuthority{}, false, errors.New("automatic moderation config binding mismatch")
	}
	resolved, present, err := h.Authority.BotActorAuthority(ctx, h.CommunityID, actor.User)
	if err != nil || !present {
		return resolved, false, err
	}
	manager := resolved.Owner || resolved.Administrator || resolved.EffectiveBits&manageCommunityBit != 0
	return resolved, manager, nil
}

//nolint:nilerr // Invalid client input intentionally maps to an unavailable protocol result.
func (h AutoModerationConfigInteractions) get(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	if len(request.Args) != 0 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	_, allowed, err := h.authorize(ctx, actor, request.CommunityID)
	if err != nil || !allowed {
		if err != nil {
			return InteractionResult{}, err
		}
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	cfg, revision, err := h.Store.Load(ctx)
	if err != nil || revision > math.MaxInt64 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	data, err := encodeAutoModerationConfig(cfg)
	if err != nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	return InteractionResult{Revision: &revision, Data: data}, nil
}

//nolint:nilerr // Invalid client input intentionally maps to an unavailable protocol result.
func (h AutoModerationConfigInteractions) put(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	if len(request.Args) != 2 || request.Revision == nil || request.Expires == nil || *request.Revision >= math.MaxInt64 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	resolved, allowed, err := h.authorize(ctx, actor, request.CommunityID)
	if err != nil || !allowed {
		if err != nil {
			return InteractionResult{}, err
		}
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	grant, grantOK := request.Args[0].(string)
	raw, ok := request.Args[1].([]byte)
	if !grantOK || grant == "" || !ok || len(raw) > autoModerationConfigMaxBytes {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	cfg, err := decodeAutoModerationConfig(raw)
	if err != nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if !configActionsAuthorized(resolved, cfg) {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	// A successful PUT must be fenced against the live evaluator. Persisting
	// without Runtime could report success while moderation continues using an
	// older policy, so treat runtime startup failure as unavailable.
	if autoModerationConfigRuntimeUnavailable(h.Runtime) {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	if h.GrantRedeemer == nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	binding := AutoModerationGrantBinding{RequestID: request.RequestID, CommunityID: request.CommunityID,
		ActorUser: actor.User, ActorDevice: actor.Device, Group: actor.Group, EventChannel: actor.Channel,
		Operation: request.Operation, ExpectedRevision: *request.Revision, RequestExpires: *request.Expires,
		ConfigHash: sha256.Sum256(raw)}
	grantAccepted, err := h.GrantRedeemer.RedeemAutoModerationGrant(ctx, grant, binding)
	if err != nil {
		return InteractionResult{}, err
	}
	if !grantAccepted {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	persist := func() error { return h.Store.SaveWithID(ctx, request.RequestID, cfg, *request.Revision) }
	err = h.Runtime.PersistConfig(ctx, cfg, *request.Revision, persist)
	if err != nil {
		if errors.Is(err, moderation.ErrConfigConflict) {
			_, latest, loadErr := h.Store.Load(ctx)
			if loadErr == nil && latest <= math.MaxInt64 && latest != *request.Revision {
				return InteractionResult{Status: interactionStatusConflict, Revision: &latest}, nil
			}
			return InteractionResult{Status: interactionStatusUnavailable}, nil
		}
		return InteractionResult{}, err
	}
	revision := *request.Revision + 1
	empty, err := cborx.Marshal([]any{})
	if err != nil {
		return InteractionResult{}, err
	}
	return InteractionResult{Revision: &revision, Data: empty}, nil
}

func configActionsAuthorized(authority client.ActorAuthority, cfg moderation.Config) bool {
	for _, action := range cfg.Actions {
		switch action {
		case moderation.ActionRemove:
			if !authority.Owner && !authority.Administrator && authority.EffectiveBits&autoModerationManageMessagesBit == 0 {
				return false
			}
		case moderation.ActionWarn, moderation.ActionTimeout:
			if !authority.Owner && !authority.Administrator && authority.EffectiveBits&autoModerationModerateMembersBit == 0 {
				return false
			}
		}
	}
	return true
}

func encodeAutoModerationConfig(cfg moderation.Config) ([]byte, error) {
	if !validAutoModerationConfig(cfg) {
		return nil, moderation.ErrConfigState
	}
	if cfg.Timeout < 0 {
		return nil, moderation.ErrConfigState
	}
	users := make([]any, len(cfg.ExemptUsers))
	for i, user := range cfg.ExemptUsers {
		users[i] = user
	}
	var enabled uint64
	if cfg.Enabled {
		enabled = 1
	}
	body, err := cborx.Marshal([]any{autoModerationConfigVersion, enabled,
		string(cfg.Actions[moderation.RuleRate]), string(cfg.Actions[moderation.RuleDuplicate]), string(cfg.Actions[moderation.RuleMentions]),
		uint64(cfg.Timeout / time.Millisecond), users})
	if err != nil || len(body) > autoModerationConfigMaxBytes {
		return nil, moderation.ErrConfigState
	}
	return body, nil
}

func decodeAutoModerationConfig(raw []byte) (moderation.Config, error) {
	var fields []any
	if cborx.ExpectMajor(raw, cborx.MajorArray) != nil || cborx.Unmarshal(raw, &fields) != nil || len(fields) != 7 || fields[0] != autoModerationConfigVersion {
		return moderation.Config{}, moderation.ErrConfigState
	}
	enabled, ok := fields[1].(uint64)
	if !ok || enabled > 1 {
		return moderation.Config{}, moderation.ErrConfigState
	}
	actions := make(map[moderation.Rule]moderation.Action, 3)
	for i, rule := range []moderation.Rule{moderation.RuleRate, moderation.RuleDuplicate, moderation.RuleMentions} {
		action, ok := fields[2+i].(string)
		if !ok {
			return moderation.Config{}, moderation.ErrConfigState
		}
		actions[rule] = moderation.Action(action)
	}
	timeoutMS, ok := fields[5].(uint64)
	if !ok || timeoutMS == 0 || timeoutMS > uint64(moderation.MaxTimeout/time.Millisecond) {
		return moderation.Config{}, moderation.ErrConfigState
	}
	values, ok := fields[6].([]any)
	if !ok || len(values) > moderation.MaxExemptUsers {
		return moderation.Config{}, moderation.ErrConfigState
	}
	users := make([]string, 0, len(values))
	for _, value := range values {
		user, ok := value.(string)
		if !ok {
			return moderation.Config{}, moderation.ErrConfigState
		}
		users = append(users, user)
	}
	cfg := moderation.Config{Enabled: enabled == 1, Actions: actions, Timeout: time.Duration(timeoutMS) * time.Millisecond, ExemptUsers: users}
	if !validAutoModerationConfig(cfg) {
		return moderation.Config{}, moderation.ErrConfigState
	}
	canonical, err := encodeAutoModerationConfig(cfg)
	if err != nil || string(canonical) != string(raw) {
		return moderation.Config{}, moderation.ErrConfigState
	}
	return cfg, nil
}

func validAutoModerationConfig(cfg moderation.Config) bool {
	if cfg.Timeout <= 0 || cfg.Timeout > moderation.MaxTimeout || cfg.Timeout%time.Millisecond != 0 || len(cfg.Actions) != 3 || len(cfg.ExemptUsers) > moderation.MaxExemptUsers {
		return false
	}
	for _, rule := range []moderation.Rule{moderation.RuleRate, moderation.RuleDuplicate, moderation.RuleMentions} {
		action, exists := cfg.Actions[rule]
		if !exists || (action != moderation.ActionLog && action != moderation.ActionRemove && action != moderation.ActionWarn && action != moderation.ActionTimeout) {
			return false
		}
	}
	seen := make(map[string]struct{}, len(cfg.ExemptUsers))
	for _, user := range cfg.ExemptUsers {
		if user == "" || len(user) > moderation.MaxExemptUserIDBytes {
			return false
		}
		if _, ok := seen[user]; ok {
			return false
		}
		seen[user] = struct{}{}
	}
	return true
}

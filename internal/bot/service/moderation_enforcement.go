package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/moderation"
	"github.com/jonasthim/dilla/internal/bot/moderation/operationid"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/id"
)

const botModerationAcceptanceWindow = 25 * time.Second

var (
	ErrModerationActionUnsupported = errors.New("moderation action is unsupported by this enforcement adapter")
	ErrModerationActionSource      = errors.New("moderation action lacks an authoritative bound source")
	// ErrModerationActionRefused is a definitive server-side refusal (for
	// example a revoked capability). The source event may be consumed safely.
	ErrModerationActionRefused = errors.New("moderation action definitively refused")
)

type moderationRemovalSigner interface {
	Binding() native.Binding
	SignRemoval(context.Context, id.ID, id.ID, uint64, uint64, uint64) ([]byte, error)
}

type moderationRemovalSession interface {
	AcceptModerationRemoval(context.Context, id.ID, []byte) (client.ModerationRemovalReceipt, error)
}

type moderationActionSession interface {
	AcceptBotModerationAction(context.Context, id.ID, id.ID, id.ID, uint64, string, []string, uint64, uint64) (client.ModerationActionReceipt, error)
}

type moderationReceiptSession interface {
	LookupModerationRemoval(context.Context, id.ID, []byte) (client.ModerationRemovalReceipt, error)
	LookupBotModerationAction(context.Context, id.ID, id.ID, id.ID, uint64, string, []string, uint64, uint64) (client.ModerationActionReceipt, error)
}

// ModerationRemovalBackend sends signed removals and installation-bound warn
// or timeout decisions. The server rechecks the active installation capability,
// current moderation permission and hierarchy for every accepted action.
type ModerationRemovalBackend struct {
	native  moderationRemovalSigner
	session interface {
		moderationRemovalSession
		moderationActionSession
	}
	binding native.Binding
}

func NewModerationRemovalBackend(n *native.Client, session *client.Session) (*ModerationRemovalBackend, error) {
	if n == nil || session == nil {
		return nil, ErrModerationEnforcementUnavailable
	}
	return newModerationRemovalBackend(n, session)
}

func newModerationRemovalBackend(n moderationRemovalSigner, session interface {
	moderationRemovalSession
	moderationActionSession
}) (*ModerationRemovalBackend, error) {
	if n == nil || session == nil {
		return nil, ErrModerationEnforcementUnavailable
	}
	binding := n.Binding()
	if binding.Instance.IsZero() || binding.Community.IsZero() || binding.Installation.IsZero() {
		return nil, ErrModerationCaseBinding
	}
	return &ModerationRemovalBackend{native: n, session: session, binding: binding}, nil
}

func (b *ModerationRemovalBackend) EnforceModerationAction(ctx context.Context, action ModerationAction) error {
	_, err := b.AcceptModerationAction(ctx, action)
	return err
}

func (b *ModerationRemovalBackend) PrepareModerationAction(ctx context.Context, action ModerationAction) (ModerationAction, error) {
	if action.Decision.Action != moderation.ActionRemove {
		return action, nil
	}
	community, ce := id.Parse(action.Community)
	group, ge := id.Parse(action.Group)
	message, me := id.Parse(action.Message)
	received := action.ReceivedAt.Unix()
	if b == nil || ctx == nil || !action.runtimeGenerated || ce != nil || ge != nil || me != nil || community != b.binding.Community || group.IsZero() || message.IsZero() || action.SourceRevision == 0 || action.SourceRevision > math.MaxInt64 || received <= 0 || received > math.MaxInt64-30 {
		return action, ErrModerationActionSource
	}
	if len(action.removalProof) == 0 {
		proof, err := b.native.SignRemoval(ctx, moderationRemovalOperationID(group, message, action.SourceRevision), group, action.SourceRevision, uint64(received)+30, uint64(received))
		if err != nil {
			return action, err
		}
		if len(proof) == 0 || len(proof) > 1024 {
			return action, ErrModerationActionSource
		}
		action.removalProof = append([]byte(nil), proof...)
	}
	return action, nil
}

// AcceptModerationAction returns a typed, source-bound record only after the
// server reports the immutable accepted action ID and community revision.
func (b *ModerationRemovalBackend) AcceptModerationAction(ctx context.Context, action ModerationAction) (AcceptedModerationAction, error) {
	if b == nil || ctx == nil {
		return AcceptedModerationAction{}, ErrModerationActionSource
	}
	if action.Decision.Action != moderation.ActionRemove && action.Decision.Action != moderation.ActionWarn && action.Decision.Action != moderation.ActionTimeout {
		return AcceptedModerationAction{}, ErrModerationActionUnsupported
	}
	if !action.runtimeGenerated || len(action.Decision.Rules) == 0 || (action.Decision.Action != moderation.ActionTimeout && action.Decision.Timeout != 0) ||
		(action.Decision.Action == moderation.ActionTimeout && (action.Decision.Timeout <= 0 || action.Decision.Timeout > moderation.MaxTimeout)) {
		return AcceptedModerationAction{}, ErrModerationActionSource
	}
	receivedAt := action.ReceivedAt.Unix()
	community, communityErr := id.Parse(action.Community)
	group, groupErr := id.Parse(action.Group)
	channel, channelErr := id.Parse(action.Channel)
	actor, actorErr := id.Parse(action.Actor)
	message, messageErr := id.Parse(action.Message)
	if communityErr != nil || groupErr != nil || channelErr != nil || actorErr != nil || messageErr != nil ||
		community.IsZero() || group.IsZero() || channel.IsZero() || actor.IsZero() || message.IsZero() || community != b.binding.Community ||
		action.SourceRevision == 0 || action.SourceRevision > math.MaxInt64 || action.ReceivedAt.IsZero() ||
		receivedAt <= 0 || receivedAt > math.MaxInt64-30 {
		return AcceptedModerationAction{}, ErrModerationActionSource
	}
	if err := ctx.Err(); err != nil {
		return AcceptedModerationAction{}, err
	}
	var request id.ID
	var accepted client.ModerationActionReceipt
	if action.Decision.Action == moderation.ActionRemove {
		request = moderationRemovalOperationID(group, message, action.SourceRevision)
		now := uint64(receivedAt)
		expires := now + 30
		signed := action.removalProof
		var err error
		if len(signed) == 0 {
			// Direct adapter callers retain their existing API; durable runtime
			// calls always carry the proof prepared before checkpoint commit.
			signed, err = b.native.SignRemoval(ctx, request, group, action.SourceRevision, expires, now)
			if err != nil {
				return AcceptedModerationAction{}, err
			}
		}
		var receipt client.ModerationRemovalReceipt
		if action.receiptOnly {
			lookup, ok := b.session.(moderationReceiptSession)
			if !ok {
				return AcceptedModerationAction{}, ErrModerationActionRefused
			}
			receipt, err = lookup.LookupModerationRemoval(ctx, group, signed)
		} else {
			receipt, err = b.session.AcceptModerationRemoval(ctx, group, signed)
		}
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Outcome == client.OutcomeDefinitive && apiErr.Code == "E_REMOVAL_PROOF_EXPIRED" {
			return AcceptedModerationAction{}, ErrModerationActionRefused
		}

		if err != nil {
			if action.receiptOnly && errors.As(err, &apiErr) && apiErr.Outcome == client.OutcomeDefinitive && (apiErr.Code == "E_NOT_FOUND" || apiErr.Code == "E_FORBIDDEN") {
				return AcceptedModerationAction{}, ErrModerationActionRefused
			}
			if errors.As(err, &apiErr) && apiErr.Outcome == client.OutcomeDefinitive && apiErr.Code == "E_FORBIDDEN" {
				return AcceptedModerationAction{}, ErrModerationActionRefused
			}
			return AcceptedModerationAction{}, err
		}
		if receipt.ActionID.IsZero() || receipt.Revision == 0 || receipt.Revision > math.MaxInt64 {
			return AcceptedModerationAction{}, client.ErrProtocol
		}
		return AcceptedModerationAction{actionID: receipt.ActionID, serverRevision: receipt.Revision, community: community,
			group: group, targetSequence: action.SourceRevision, operationID: request, action: moderation.ActionRemove}, nil
	}
	request = operationid.Automatic(group, action.SourceRevision)
	if request.IsZero() {
		return AcceptedModerationAction{}, ErrModerationActionSource
	}
	rules := make([]string, len(action.Decision.Rules))
	for i, rule := range action.Decision.Rules {
		switch rule {
		case moderation.RuleRate:
			rules[i] = "message_rate"
		case moderation.RuleDuplicate:
			rules[i] = "duplicate_body"
		case moderation.RuleMentions:
			rules[i] = "mention_count"
		default:
			return AcceptedModerationAction{}, ErrModerationActionSource
		}
	}
	for i := 1; i < len(rules); i++ {
		for j := i; j > 0 && rules[j] < rules[j-1]; j-- {
			rules[j], rules[j-1] = rules[j-1], rules[j]
		}
	}
	now := time.Now().Unix()
	windowSeconds := int64(botModerationAcceptanceWindow / time.Second)
	if now <= 0 || now > math.MaxInt64-windowSeconds {
		return AcceptedModerationAction{}, ErrModerationActionSource
	}
	var timeoutMillis uint64
	if action.Decision.Action == moderation.ActionTimeout {
		timeout := action.Decision.Timeout / time.Millisecond
		if timeout < 0 {
			return AcceptedModerationAction{}, ErrModerationActionSource
		}
		timeoutMillis = uint64(timeout)
	}
	var err error
	if action.receiptOnly {
		lookup, ok := b.session.(moderationReceiptSession)
		if !ok {
			return AcceptedModerationAction{}, ErrModerationActionRefused
		}
		accepted, err = lookup.LookupBotModerationAction(ctx, b.binding.Installation, request, group, action.SourceRevision, string(action.Decision.Action), rules, timeoutMillis, uint64(now+windowSeconds))
	} else {
		accepted, err = b.session.AcceptBotModerationAction(ctx, b.binding.Installation, request, group, action.SourceRevision, string(action.Decision.Action), rules, timeoutMillis, uint64(now+windowSeconds))
	}
	if err != nil {
		var apiErr *client.APIError
		if action.receiptOnly && errors.As(err, &apiErr) && apiErr.Outcome == client.OutcomeDefinitive && (apiErr.Code == "E_NOT_FOUND" || apiErr.Code == "E_FORBIDDEN") {
			return AcceptedModerationAction{}, ErrModerationActionRefused
		}
		if errors.As(err, &apiErr) && apiErr.Outcome == client.OutcomeDefinitive && apiErr.Code == "E_FORBIDDEN" {
			return AcceptedModerationAction{}, ErrModerationActionRefused
		}
		return AcceptedModerationAction{}, err
	}
	if accepted.ActionID.IsZero() || accepted.Revision == 0 || accepted.Revision > math.MaxInt64 {
		return AcceptedModerationAction{}, client.ErrProtocol
	}
	return AcceptedModerationAction{actionID: accepted.ActionID, serverRevision: accepted.Revision, community: community,
		group: group, targetSequence: action.SourceRevision, operationID: request, action: action.Decision.Action}, nil

}

func moderationRemovalOperationID(group, message id.ID, revision uint64) id.ID {
	var encoded [8]byte
	for i := range encoded {
		encoded[7-i] = byte((revision >> (8 * i)) & 0xff)
	}
	sum := sha256.Sum256(append(append(append([]byte("dilla:automod:remove:v1\x00"), group[:]...), message[:]...), encoded[:]...))
	var operation id.ID
	copy(operation[:], sum[:len(operation)])
	if operation.IsZero() {
		operation[15] = 1
	}
	return operation
}

var _ ModerationEnforcementBackend = (*ModerationRemovalBackend)(nil)

package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/protocol"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
)

const (
	rolePanelManageRolesBit = uint64(1 << 15)
	rolePanelMaxOptions     = 100
	rolePanelMaxMessage     = 3500
)

type rolePanelAuthority interface {
	BotActorAuthority(context.Context, id.ID, id.ID) (client.ActorAuthority, bool, error)
}

type rolePanelDestination interface {
	ResolveTextGroup(context.Context, id.ID, id.ID) (id.ID, error)
}

type rolePanelSender interface {
	Send(context.Context, *client.Session, SendInput) (protocol.SendReceipt, error)
}

type rolePanelDiscovery interface {
	DiscoverObserved(context.Context) (client.Discovery, client.ResponseObservation, error)
}

type RolePanelInteractions struct {
	Authority   rolePanelAuthority
	Destination rolePanelDestination
	Sender      rolePanelSender
	Discovery   rolePanelDiscovery
	Session     *client.Session
	CommunityID id.ID
	Now         func() time.Time
}

type rolePanelOption struct {
	Emoji, Name string
	Role        id.ID
}

// Handlers sends role panels only in response to a current, authenticated human
// interaction. The public message goes through the installed Bot's MLS group
// and durable send receipt; the human registers that exact sequence separately.
func (h RolePanelInteractions) Handlers() map[protocol.Operation]InteractionHandlerSpec {
	return map[protocol.Operation]InteractionHandlerSpec{
		protocol.OpRolePanelPublish: {RevisionInHandler: true, Handle: h.publish},
	}
}

//nolint:nilerr // Invalid client input intentionally maps to an unavailable protocol result.
func (h RolePanelInteractions) publish(ctx context.Context, actor InteractionActor, request protocol.Request) (InteractionResult, error) {
	if h.Authority == nil || h.Destination == nil || h.Sender == nil || h.Discovery == nil || h.Session == nil || h.CommunityID.IsZero() || actor.User.IsZero() || actor.Kind != 0 ||
		request.Operation != protocol.OpRolePanelPublish || request.CommunityID != h.CommunityID || request.RequestID.IsZero() || request.Revision == nil || request.Expires == nil || len(request.Args) != 2 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	channel, ok := request.Args[0].(id.ID)
	encoded, payloadOK := request.Args[1].([]byte)
	if !ok || channel.IsZero() || !payloadOK {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	options, err := decodeRolePanelOptions(encoded)
	if err != nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	authority, present, err := h.Authority.BotActorAuthority(ctx, h.CommunityID, actor.User)
	if err != nil {
		return InteractionResult{}, err
	}
	if !present || actor.Kind != 0 || (!authority.Owner && !authority.Administrator && authority.EffectiveBits&rolePanelManageRolesBit == 0) {
		return InteractionResult{Status: interactionStatusForbidden}, nil
	}
	group, err := h.Destination.ResolveTextGroup(ctx, h.CommunityID, channel)
	if err != nil || group.IsZero() {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	discovery, _, err := h.Discovery.DiscoverObserved(ctx)
	if err != nil {
		return InteractionResult{}, err
	}
	if discovery.Generation == 0 || discovery.InstanceID.IsZero() {
		return InteractionResult{}, client.ErrProtocol
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	created := now().UTC().Unix()
	if created < 0 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	message, err := rolePanelMessage(options)
	if err != nil {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	body, err := cborx.Marshal([]any{uint64(0), nil, message, []any{}, nil})
	if err != nil {
		return InteractionResult{}, err
	}
	attempt := rolePanelAttemptID(request.RequestID)
	receipt, err := h.Sender.Send(ctx, h.Session, SendInput{OperationID: request.RequestID, AttemptID: attempt, GroupID: group,
		RequestBody: body, OriginalNow: uint64(created), ExpectedGeneration: discovery.Generation})
	if err != nil {
		return InteractionResult{}, err
	}
	if receipt.OperationID != request.RequestID || receipt.AttemptID != attempt || receipt.CommunityID != h.CommunityID || receipt.GroupID != group || receipt.ExpectedGeneration != discovery.Generation {
		return InteractionResult{}, client.ErrProtocol
	}
	if receipt.Outcome != protocol.ReceiptAccepted || receipt.SendResponse == nil || receipt.SendResponse.Sequence == 0 {
		return InteractionResult{Status: interactionStatusUnavailable}, nil
	}
	data, err := cborx.Marshal([]any{channel, receipt.SendResponse.Sequence})
	if err != nil {
		return InteractionResult{}, err
	}
	return InteractionResult{Data: data}, nil
}

func decodeRolePanelOptions(raw []byte) ([]rolePanelOption, error) {
	if len(raw) == 0 || len(raw) > 2200 {
		return nil, errors.New("invalid role panel options")
	}
	var rows []any
	if cborx.Unmarshal(raw, &rows) != nil || len(rows) == 0 || len(rows) > rolePanelMaxOptions {
		return nil, errors.New("invalid role panel options")
	}
	out := make([]rolePanelOption, len(rows))
	emojis := map[string]bool{}
	roles := map[id.ID]bool{}
	for i, value := range rows {
		row, ok := value.([]any)
		if !ok || len(row) != 3 {
			return nil, errors.New("invalid role panel option")
		}
		emoji, emojiOK := row[0].(string)
		roleBytes, roleOK := row[1].([]byte)
		name, nameOK := row[2].(string)
		if !emojiOK || !roleOK || len(roleBytes) != 16 || !nameOK {
			return nil, errors.New("invalid role panel option")
		}
		var role id.ID
		copy(role[:], roleBytes)
		if role.IsZero() || !utf8.ValidString(emoji) || len([]byte(emoji)) == 0 || len([]byte(emoji)) > 32 || strings.ContainsAny(emoji, "\x00\r\n\x7f") || emojis[emoji] || roles[role] ||
			!utf8.ValidString(name) || len([]byte(name)) == 0 || len([]byte(name)) > 100 || strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n\x00\x7f") {
			return nil, errors.New("invalid role panel option")
		}
		for _, r := range emoji {
			if r < 32 {
				return nil, errors.New("invalid role panel option")
			}
		}
		for _, r := range name {
			if r < 32 {
				return nil, errors.New("invalid role panel option")
			}
		}
		emojis[emoji], roles[role] = true, true
		out[i] = rolePanelOption{Emoji: emoji, Role: role, Name: name}
	}
	return out, nil
}

func rolePanelMessage(options []rolePanelOption) (string, error) {
	var builder strings.Builder
	builder.WriteString("React with the matching emoji to choose a role:\n\n")
	for _, option := range options {
		builder.WriteString(option.Emoji)
		builder.WriteByte(' ')
		builder.WriteString(option.Name)
		builder.WriteByte('\n')
		if builder.Len() > rolePanelMaxMessage {
			return "", errors.New("role panel message is too long")
		}
	}
	// Security review (task 8b): option names never carry the music player marker.
	return NeutralizeBotMarkers(builder.String()), nil
}

func rolePanelAttemptID(operation id.ID) id.ID {
	digest := sha256.Sum256(append([]byte("dilla role panel attempt v1\x00"), operation[:]...))
	var attempt id.ID
	copy(attempt[:], digest[:len(attempt)])
	return attempt
}

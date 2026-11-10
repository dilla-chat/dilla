package api

import (
	"context"
	"errors"
	"fmt"

	botaccess "github.com/dilla-chat/dilla/internal/bot/access"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// StructureChannels is the real ds.Channels: invariant 1's channel-mode source
// and the registration ACL, backed by the channels, communities and members
// tables. It replaces Plan 1's ds.PermissiveChannels{}, which reported "no
// channel row" for every target and admitted every registration so that part
// 1b could ship before the channels table existed (Plan 1 NV-B5 and follow-up
// card 14). It lives in internal/api, not internal/ds, because internal/ds must
// not learn the channel vocabulary it deliberately keeps behind the seam.
type StructureChannels struct{ Repo store.Repository }

var _ ds.Channels = StructureChannels{}

// Group kinds, protocol/01 "Group kinds". The two channel kinds are exported
// because a binding names them (ExpectedBinding).
const (
	GroupText        uint8 = 0
	GroupCall        uint8 = 1
	groupText              = GroupText
	groupCall              = GroupCall
	groupPairing     uint8 = 2
	groupInteraction uint8 = 3
)

// Channel returns the channel's visibility and mode. A target that is not a
// channel — a DM pairing group, an interaction group — is ds.ErrNoChannel, NOT
// an error: that is the contract Plan 1's checkChannelMode reads, and returning
// store.ErrNotFound here would 500 every such registration.
func (c StructureChannels) Channel(ctx context.Context, targetID id.ID) (uint8, uint8, error) {
	row, err := c.Repo.GetChannel(ctx, targetID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, 0, ds.ErrNoChannel
	}
	if err != nil {
		return 0, 0, err
	}
	return row.Visibility, row.Mode, nil
}

// MayRegister is the registration ACL (Plan 1 follow-up card 14): before it, any
// enrolled device could register a group for any target. The rules, by group
// kind (protocol/01 § dilla_binding):
//
//   - text and call: when the target is a live channel, the binding's
//     community_id must be the channel's, the channel's kind must carry that
//     group kind (a text group on a text channel, a call group on a voice
//     channel; a DM carries both), and the user must hold, in that channel and
//     through the permission resolver, what ResolverACL requires of a joiner:
//     PermViewChannel for a text group, PermViewChannel|PermConnect for a call
//     group. A text or call group whose binding names a community but whose
//     target is not a live channel of it is refused: target_id is the channel id
//     for both kinds (protocol/01 dilla_binding; R9 keeps a call's id in a
//     companion column), and ResolverACL refuses the same group to a joiner.
//   - text and call with no community: a DM or group DM (task 6). The target
//     must be a live DM channel (ds.ErrBindingTarget otherwise, a community
//     channel included) and the user one of its participants, its
//     channel_members (ds.ErrNotEligible otherwise).
//   - pairing: not a channel group, gated by the session scope rules of auth;
//     a managed Bot never binds one.
//   - interaction (kind 3): retired in Bot v2 (Task 23); refused for everyone
//     as ds.ErrBindingTarget.
//
// A refusal wraps ds.ErrNotEligible (403 E_FORBIDDEN) or ds.ErrBindingTarget (400
// E_BINDING_INVALID); any other error is the store's.
func (c StructureChannels) MayRegister(ctx context.Context, userID id.ID, b ds.Binding) error {
	switch b.Kind {
	case groupPairing:
		bot, err := botaccess.IsBotUser(ctx, c.Repo, userID)
		if err != nil {
			return err
		}
		if bot {
			return fmt.Errorf("%w: managed bots cannot bind pairing groups", ds.ErrNotEligible)
		}
		return nil
	case groupInteraction:
		return fmt.Errorf("%w: the interaction group kind is retired", ds.ErrBindingTarget)
	case groupText, groupCall:
	default:
		return fmt.Errorf("%w: unknown group kind %d", ds.ErrBindingTarget, b.Kind)
	}

	row, err := c.Repo.GetChannel(ctx, b.TargetID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return mayRegisterWithoutChannel(b)
	case err != nil:
		return err
	}
	if b.CommunityID == nil {
		bot, err := botaccess.IsBotUser(ctx, c.Repo, userID)
		if err != nil {
			return err
		}
		if bot {
			return fmt.Errorf("%w: managed bots cannot bind DMs", ds.ErrNotEligible)
		}
	} else {
		binding, bot, err := botaccess.ForUserCommunity(ctx, c.Repo, userID, *b.CommunityID)
		if err != nil {
			if errors.Is(err, botaccess.ErrIneligible) {
				return fmt.Errorf("%w: bot is not eligible for this community", ds.ErrNotEligible)
			}
			return err
		}
		if bot && (binding.State != store.BotInstallationJoining && binding.State != store.BotInstallationReady || binding.CommunityID != *b.CommunityID) {
			return fmt.Errorf("%w: bot is not eligible for this community", ds.ErrNotEligible)
		}
	}

	switch {
	case row.CommunityID == nil && b.CommunityID != nil:
		return fmt.Errorf("%w: the target is a DM, which has no community", ds.ErrBindingTarget)
	case row.CommunityID != nil && b.CommunityID == nil:
		return fmt.Errorf("%w: a group with no community must name a DM", ds.ErrBindingTarget)
	case row.CommunityID != nil && *b.CommunityID != *row.CommunityID:
		return fmt.Errorf("%w: the target channel belongs to another community", ds.ErrBindingTarget)
	}
	// A DM or group DM carries both a text and a call group; a community
	// channel carries the one its kind names. The schema ties a channel with no
	// community to kinds 3 and 4.
	switch {
	case b.Kind == groupText && row.Kind != ChannelText && row.CommunityID != nil:
		return fmt.Errorf("%w: a text group is bound to a text channel", ds.ErrBindingTarget)
	case b.Kind == groupCall && row.Kind != ChannelVoice && row.CommunityID != nil:
		return fmt.Errorf("%w: a call group is bound to a voice channel", ds.ErrBindingTarget)
	}
	// The resolver answers a DM with its participants (channel_members) and a
	// community channel with roles and overwrites.
	bits, err := NewResolver(c.Repo).Resolve(ctx, userID, row)
	if err != nil {
		return err
	}
	if !bits.Has(groupBits(b.Kind)) {
		return fmt.Errorf("%w: not permitted in this channel", ds.ErrNotEligible)
	}
	return nil
}

var _ ds.GroupRecreation = StructureChannels{}

// MayRecreate is invariant 11's "re-created by the channel owner's device": while a
// restore's heal is pending, the channel's replacement text or call group may be
// registered only by the community's owner. A DM has no owner, and MayRegister has
// already required a participant, so any participant may. The delivery service
// asks only after MayRegister admitted the same user and binding.
func (c StructureChannels) MayRecreate(ctx context.Context, userID id.ID, b ds.Binding) error {
	if b.CommunityID == nil {
		return nil
	}
	com, err := c.Repo.GetCommunity(ctx, *b.CommunityID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: the community is gone", ds.ErrNotEligible)
	}
	if err != nil {
		return err
	}
	if com.Owner != userID {
		return fmt.Errorf("%w: only the community owner re-creates a channel's group", ds.ErrNotEligible)
	}
	return nil
}

// groupBits is what a channel group of kind needs, the same set ResolverACL
// requires of a joiner.
func groupBits(kind uint8) Bits {
	if kind == groupCall {
		return callGroupBits
	}
	return textGroupBits
}

// mayRegisterWithoutChannel is MayRegister for a text or call group whose target
// has no live channel row: with a community it names no channel of it, and
// without one it names no DM (a DM's id is always a channel's, P2-D31).
func mayRegisterWithoutChannel(b ds.Binding) error {
	if b.CommunityID == nil {
		return fmt.Errorf("%w: a group with no community must name a live DM", ds.ErrBindingTarget)
	}
	return fmt.Errorf("%w: a community channel group must name a live channel of that community", ds.ErrBindingTarget)
}

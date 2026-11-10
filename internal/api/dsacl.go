package api

import (
	"context"
	"errors"
	"slices"

	botaccess "github.com/dilla-chat/dilla/internal/bot/access"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// ResolverACL is the real ds.ACL: invariant 4's "a credential whose user is
// eligible under the channel's ACL", answered by the permission resolver over
// roles and channel overwrites. It replaces Plan 1's ds.DenyUnlessMember (NV-B6)
// as the eligibility check for an Add, for a join by external commit (resync)
// and for the GroupInfo and tree a joiner reads. Like StructureChannels it lives
// in internal/api, because internal/ds must not learn the permission vocabulary
// it keeps behind the seam.
//
// The rules, by the group's kind (protocol/01 "Group kinds"):
//
//   - text, in a community: the group's target is a live channel of the group's
//     community and the user holds PermViewChannel in it, overwrites applied.
//   - call, in a community: the same with PermViewChannel|PermConnect. The
//     target is the voice channel (protocol/01 dilla_binding; R9 keeps the call
//     id in a companion column, never in target_id), so a call group whose
//     channel is gone is refused exactly as a text group is. There is no
//     community-wide fallback: it would drop the channel's overwrites and open
//     a deleted private voice channel's group to every member.
//   - text or call with no community, bound to a live DM or group DM: the DM's
//     participants, task 6's channel_members, which the resolver answers with
//     the DM bits (Resolver.resolveDM).
//   - everything else — a pairing group, an interaction group, a DM-shaped
//     group whose target is no channel — keeps Plan 1's rule: eligible only
//     where the user is already in the group.
//
// An error means "cannot answer", which every caller in internal/ds treats as a
// refusal.
type ResolverACL struct{ Repo store.Repository }

var _ ds.ACL = ResolverACL{}

// The bits each channel group needs.
const (
	textGroupBits = PermViewChannel
	callGroupBits = PermViewChannel | PermConnect
)

func (a ResolverACL) Eligible(ctx context.Context, groupID, userID id.ID) (bool, error) {
	g, err := a.Repo.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	isBot := false
	if g.CommunityID == nil {
		bot, err := botaccess.IsBotUser(ctx, a.Repo, userID)
		if err != nil || bot {
			return false, err
		}
	} else {
		binding, bot, err := botaccess.ForUserCommunity(ctx, a.Repo, userID, *g.CommunityID)
		isBot = bot
		if err != nil {
			if errors.Is(err, botaccess.ErrIneligible) {
				return false, nil
			}
			return false, err
		}
		if bot && (binding.State != store.BotInstallationJoining && binding.State != store.BotInstallationReady ||
			binding.CommunityID != *g.CommunityID || g.Kind != groupText && g.Kind != groupCall) {
			return false, nil
		}
	}
	if g.Kind != groupText && g.Kind != groupCall {
		return ds.DenyUnlessMember{Store: a.Repo}.Eligible(ctx, groupID, userID)
	}
	ch, err := a.Repo.GetChannel(ctx, g.TargetID)
	switch {
	case errors.Is(err, store.ErrNotFound) && g.CommunityID == nil:
		// A DM-shaped group whose target is no channel: StructureChannels no
		// longer registers one, but a group registered before task 6 may still
		// name such a target, and it keeps Plan 1's rule.
		return ds.DenyUnlessMember{Store: a.Repo}.Eligible(ctx, groupID, userID)
	case errors.Is(err, store.ErrNotFound):
		return false, nil // a community channel group whose channel is gone
	case err != nil:
		return false, err
	}
	switch {
	case g.CommunityID == nil && ch.CommunityID != nil,
		g.CommunityID != nil && (ch.CommunityID == nil || *ch.CommunityID != *g.CommunityID):
		return false, nil
	}
	// A DM's participants hold its bits and nobody else does (Resolver.resolveDM),
	// so a participant's devices can be added before any of them holds a leaf,
	// and a removed participant's cannot.
	//
	// A text group whose channel no longer carries one (a PATCH to readable, or
	// to a visibility that forces it) admits nobody: the channel's content is
	// server-readable now, and the group is being closed (fix wave I3).
	if g.Kind == groupText && !TextGroupAllowed(ch) {
		return false, nil
	}
	// The community's Bot is a member of its Bot channels' text groups only (Bot v2 Task 22; Task 23, R1 b): an Add of
	// the Bot into another text group is refused at commit time (add_acl), its outstanding Add is voided, a queued join
	// is dropped at the drain, and the sweeper proposes a leaf it holds there out. Call groups are not narrowed.
	if isBot && g.Kind == groupText {
		marked, err := a.Repo.ListBotChannels(ctx, *g.CommunityID)
		if err != nil {
			return false, err
		}
		if !slices.ContainsFunc(marked, func(row store.BotChannelRow) bool { return row.ChannelID == ch.ID }) {
			return false, nil
		}
	}
	bits, err := NewResolver(a.Repo).Resolve(ctx, userID, ch)
	if err != nil {
		return false, err
	}
	return bits.Has(groupBits(g.Kind)), nil
}

package api

import (
	"context"
	"errors"
	"slices"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// eligibleUsersPage is how many community members one page of EligibleUsers reads.
const eligibleUsersPage = 500

// EligibleUsers returns the community members whose resolved permissions in ch
// carry PermViewChannel. It is the definition of "may be a leaf of this
// channel's text group", and it is what channel_members materialises so the
// delivery service never has to resolve permissions itself.
//
// A DM or group DM has no community: its participants ARE its channel_members,
// so they are returned as they stand.
//
// The community, its roles and the channel's overwrites are read once; per
// member it reads only the member's role grants, so the cost is one query per
// member and never a full snapshot per member.
func EligibleUsers(ctx context.Context, repo store.Repository, ch store.ChannelRow) ([]id.ID, error) {
	if ch.CommunityID == nil {
		return repo.ListChannelMembers(ctx, ch.ID)
	}
	overwrites, err := repo.ListOverwrites(ctx, ch.ID)
	if err != nil {
		return nil, err
	}
	com, err := repo.GetCommunity(ctx, *ch.CommunityID)
	if err != nil {
		return nil, err
	}
	roles, err := repo.ListRoles(ctx, *ch.CommunityID)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(roles, byPositionThenID)
	// R35: every member is eligible for the built-in AFK channel; the pointer is read once.
	afk, err := repo.GetCommunityAFK(ctx, *ch.CommunityID)
	if err != nil {
		return nil, err
	}
	isAFK := afk.ChannelID != nil && *afk.ChannelID == ch.ID
	// @everyone is the role at position 0, identified here exactly as
	// LoadSnapshot identifies it; Snapshot.Resolve keys on the id, never on the
	// position (task 3, step 3).
	var everyone id.ID
	if len(roles) > 0 && roles[0].Position == 0 {
		everyone = roles[0].ID
	}

	var out []id.ID
	var after id.ID
	for {
		page, err := repo.ListMembersOfCommunity(ctx, *ch.CommunityID, after, eligibleUsersPage)
		if err != nil {
			return nil, err
		}
		for _, m := range page {
			held, err := repo.ListMemberRoles(ctx, *ch.CommunityID, m.UserID)
			if err != nil {
				return nil, err
			}
			snap := Snapshot{Owner: com.Owner, Everyone: everyone, Roles: roles, Overwrite: overwrites,
				Held: make(map[id.ID]bool, len(held)), AFK: isAFK}
			for _, r := range held {
				snap.Held[r] = true
			}
			if snap.ResolveChannel(m.UserID).Has(PermViewChannel) {
				out = append(out, m.UserID)
			}
		}
		if len(page) < eligibleUsersPage {
			return out, nil
		}
		after = page[len(page)-1].UserID
	}
}

// MaterialiseChannelMembers rewrites channel_members for ch from EligibleUsers,
// then hands the difference to the delivery service through SyncGroupMembers,
// which batches the Adds (at most 256 per commit, ds.ProposeAddBatch) and issues
// the Removes. The two halves are deliberately separate transactions: the row
// write must not be held open while the delivery service issues its proposals,
// and SyncGroupMembers MUST NOT run inside a Tx at all (RemoveUserFromCommunityGroups
// gives the single-writer reason).
//
// A category holds no members and is left alone. The rows are written even when
// dsvc is nil; the error is then errNoDS, for a caller to log.
func MaterialiseChannelMembers(ctx context.Context, repo store.Repository, dsvc DS, ch store.ChannelRow, now int64) error {
	if ch.Kind == ChannelCategory {
		return nil
	}
	want, err := EligibleUsers(ctx, repo, ch)
	if err != nil {
		return err
	}
	have, err := repo.ListChannelMembers(ctx, ch.ID)
	if err != nil {
		return err
	}
	wanted := make(map[id.ID]bool, len(want))
	for _, u := range want {
		wanted[u] = true
	}
	had := make(map[id.ID]bool, len(have))
	for _, u := range have {
		had[u] = true
	}
	if err := repo.Tx(ctx, func(tx store.Repository) error {
		for _, u := range want {
			if !had[u] {
				if err := tx.PutChannelMember(ctx, ch.ID, u, now); err != nil {
					return err
				}
			}
		}
		for _, u := range have {
			if !wanted[u] {
				if err := tx.DeleteChannelMember(ctx, ch.ID, u); err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return SyncGroupMembers(ctx, repo, dsvc, ch, now)
}

// SyncRegisteredGroup is what a freshly registered group bound to a channel
// needs: protocol/01 § Joining's "creating a private channel … is done by the DS
// issuing Add proposals in batches … until all eligible devices are members".
// The creator's device registered the group holding the only leaf; every other
// eligible device is proposed here, 256 per commit.
//
// A group bound to no live channel (a pairing or an interaction group) is left
// alone. The composition root calls it after POST /v1/groups succeeds, outside
// any transaction.
func SyncRegisteredGroup(ctx context.Context, repo store.Repository, dsvc DS, groupID id.ID, now int64) error {
	g, err := repo.GetGroup(ctx, groupID)
	if err != nil {
		return err
	}
	if g.Kind != GroupText && g.Kind != GroupCall {
		return nil
	}
	ch, err := repo.GetChannel(ctx, g.TargetID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return MaterialiseChannelMembers(ctx, repo, dsvc, ch, now)
}

// syncChannelEligibility brings one user's channel_members rows across every
// channel of the community in line with the resolver's verdict, after a change
// that can move only that user's eligibility (a role granted or revoked). For
// each channel whose verdict for the user CHANGED it re-derives the channel with
// MaterialiseChannelMembers, which also proposes the Adds or Removes; a channel
// whose verdict stands is not touched. EligibleUsers is O(members) and is called
// once per affected channel, which is why this walks channels rather than
// re-deriving the whole community.
//
// It MUST NOT run inside a Tx. Every channel is attempted; the failures are
// returned together.
func syncChannelEligibility(ctx context.Context, repo store.Repository, dsvc DS, communityID, userID id.ID, now int64) error {
	channels, err := repo.ListChannels(ctx, communityID)
	if err != nil {
		return err
	}
	res := NewResolver(repo)
	var errs []error
	for _, ch := range channels {
		if ch.Kind == ChannelCategory {
			continue
		}
		bits, err := res.Resolve(ctx, userID, ch)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		members, err := repo.ListChannelMembers(ctx, ch.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if bits.Has(PermViewChannel) == slices.Contains(members, userID) {
			// The view verdict stands. A voice channel's call group also needs
			// connect, which channel_members does not record: a member who kept view
			// but lost connect still loses their call leaves (fix wave I4).
			if CallGroupAllowed(ch) && bits.Has(PermViewChannel) && !bits.Has(callGroupBits) {
				if err := SyncGroupMembers(ctx, repo, dsvc, ch, now); err != nil {
					errs = append(errs, err)
				}
			}
			continue
		}
		if err := MaterialiseChannelMembers(ctx, repo, dsvc, ch, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SyncBotChannelEligibility applies the ordinary per-member channel audience
// reconciliation after a bot's enrollment transaction commits.
func SyncBotChannelEligibility(ctx context.Context, repo store.Repository, dsvc DS, communityID, userID id.ID, now int64) error {
	return syncChannelEligibility(ctx, repo, dsvc, communityID, userID, now)
}

// SyncEnrolledBotChannels retries channel reconciliation after the enrolled
// bot device publishes KeyPackages. It only touches communities whose current
// installation still binds that exact user and device.
func SyncEnrolledBotChannels(ctx context.Context, repo store.Repository, dsvc DS, userID, deviceID id.ID, now int64) error {
	communities, err := repo.ListCommunitiesForUser(ctx, userID)
	if err != nil {
		return err
	}
	var errs []error
	for _, community := range communities {
		installation, err := repo.GetActiveBotInstallationByCommunity(ctx, community.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if installation.RemovedAt != nil || installation.UserID == nil || *installation.UserID != userID ||
			installation.DeviceID == nil || *installation.DeviceID != deviceID ||
			(installation.State != store.BotInstallationJoining && installation.State != store.BotInstallationReady) {
			continue
		}
		if err := syncChannelEligibility(ctx, repo, dsvc, community.ID, userID, now); err != nil {
			errs = append(errs, err)
		}
		// Enrollment may already have materialised channel_members before this
		// device had a KeyPackage. In that case the view verdict did not change,
		// so syncChannelEligibility alone would skip the group. Re-run each live
		// channel's group sync now that this device can be proposed.
		channels, err := repo.ListChannels(ctx, community.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, channel := range channels {
			if channel.Kind == ChannelCategory || channel.DeletedAt != nil {
				continue
			}
			if err := SyncGroupMembers(ctx, repo, dsvc, channel, now); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// materialiseJoiner adds a user who has just joined a community to
// channel_members of every channel of it that carries no MLS group — a
// server-readable text channel — and that the resolver lets them view, the
// verdict EligibleUsers gives. For such a channel channel_members IS the live
// audience of message.plain (store.ListReadableAudience), and nothing else
// re-derives it until a role, overwrite or visibility change does.
//
// A channel that can carry a text or call group is left alone: its
// channel_members drives SyncGroupMembers' proposals, and the joiner enters its
// group by external commit (protocol/01), so writing the row without the Adds
// would only make syncChannelEligibility skip the channel on a later grant.
// Batch-Adding on join is task 7's separate follow-up.
//
// PutChannelMember is idempotent, so a repeat is harmless. It MUST NOT run
// inside a Tx.
func materialiseJoiner(ctx context.Context, repo store.Repository, communityID, userID id.ID, now int64) error {
	channels, err := repo.ListChannels(ctx, communityID)
	if err != nil {
		return err
	}
	res := NewResolver(repo)
	var add []id.ID
	var errs []error
	for _, ch := range channels {
		if ch.Kind == ChannelCategory || ch.DeletedAt != nil || TextGroupAllowed(ch) || CallGroupAllowed(ch) {
			continue
		}
		bits, err := res.Resolve(ctx, userID, ch)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if bits.Has(PermViewChannel) {
			add = append(add, ch.ID)
		}
	}
	if len(add) > 0 {
		if err := repo.Tx(ctx, func(tx store.Repository) error {
			for _, ch := range add {
				if err := tx.PutChannelMember(ctx, ch, userID, now); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// materialiseChannel re-derives one channel by id after a change that can move
// anyone's eligibility in it (an overwrite, a visibility change). It MUST NOT
// run inside a Tx.
func materialiseChannel(ctx context.Context, repo store.Repository, dsvc DS, channelID id.ID, now int64) error {
	ch, err := repo.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}
	return MaterialiseChannelMembers(ctx, repo, dsvc, ch, now)
}

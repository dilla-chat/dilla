package ds

import (
	"context"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// reconcilePage is how many open groups one sweep tick reconciles. The sweeper ticks every minute
// and the reconcile asks the ACL once per distinct user of each group, so it walks the open groups
// a page per tick, resuming where the last tick stopped, rather than the whole instance every
// minute.
const reconcilePage = 64

// reconcileLeaves is the backstop for a membership change whose Removes were never issued (fix
// wave C3). A kick, a ban or a leave commits first and proposes its Removes afterwards; if that
// second half dies — a crash, a fault in the middle of the loop — nothing else re-drives it: the
// ACL gates only Adds and joins, commit and upload check only leaf currency, and the inactivity
// sweep skips active devices. So each tick takes the next page of open text and call groups and,
// in each, proposes an instance Remove for every live leaf whose user the ACL no longer admits and
// that no outstanding INSTANCE Remove already targets. A member's own Remove of the leaf does not
// count: a member proposal never stands in for an instance proposal.
//
// A group the ACL admits nobody to is left alone: it is the leftover of a deleted channel whose
// Close was lost, and emptying it leaf by leaf would only queue Removes nobody could commit.
// An epoch-unknown group is skipped too: it heals first.
//
// THE BARRED DEVICES (R-2 of the DS re-review). In every open text and call group (DMs included), a
// live leaf whose DEVICE is quarantined or revoked is owed an instance Remove whatever the ACL says
// of its user, and this pass is the durable source of it. Pairing and interaction groups carry no
// external sender (protocol/01 § External senders), so their members reject an instance Remove and
// the guest refuses to issue one: they are not asked (M-1 of the integration re-review), and their
// own members remove a revoked device (01, member Removes: a device revocation). A fork quorum
// fires once and proposes once; nothing proposes a revoked device's removal at all (revocation closes its sockets and cuts it
// from calls, and the user-level ACL still admits its user); and a heal can drop a restored
// Remove whose re-issue failed, or orphan it in a crash before the carry. One query per group
// (ListBarredMembers), and the Remove is bound to the device and deduped like every other.
func (d *DS) reconcileLeaves(ctx context.Context) (int, error) {
	d.reconcileMu.Lock()
	after := d.reconcileAfter
	d.reconcileMu.Unlock()

	groups, err := d.opts.Store.ListOpenGroups(ctx, after, reconcilePage)
	if err != nil {
		return 0, err
	}
	next := id.ID{} // a short page wraps the walk back to the start
	if len(groups) == reconcilePage {
		next = groups[len(groups)-1].GroupID
	}
	proposed := 0
	for _, g := range groups {
		if g.EpochUnknown {
			continue
		}
		unlock := d.lock(g.GroupID)
		n, err := d.reconcileGroupLocked(ctx, g.GroupID, isChannelGroupKind(g.Kind))
		unlock()
		proposed += n
		if err != nil {
			if ctx.Err() != nil {
				return proposed, ctx.Err()
			}
			d.log().Error("reconciling a group's leaves with its ACL failed",
				"group", g.GroupID.String()[:8], "err", err)
		}
	}
	d.reconcileMu.Lock()
	d.reconcileAfter = next
	d.reconcileMu.Unlock()
	return proposed, nil
}

// leafHoldersEligible is the ACL's verdict for every user holding a live leaf in members: one
// question per group from a BatchACL, one per distinct user otherwise.
func (d *DS) leafHoldersEligible(ctx context.Context, groupID id.ID, members []store.MemberRow) (map[id.ID]bool, error) {
	var users []id.ID
	seen := map[id.ID]bool{}
	for _, m := range members {
		if m.RemovedEpoch == nil && !seen[m.UserID] {
			seen[m.UserID] = true
			users = append(users, m.UserID)
		}
	}
	return d.eligibleUsers(ctx, groupID, users)
}

// reconcileGroupLocked is reconcileLeaves for one group, with its lock held. With acl set (a text or
// call group) it first voids the outstanding Adds no commit could carry (VoidIneligibleAdds), so
// the Removes it issues land in a group that can commit them, proposes a Remove for every leaf
// whose user the ACL no longer admits, and one for every leaf whose device is quarantined or
// revoked. Without acl (a pairing or interaction group, which carries no external sender) it
// proposes nothing. It elects one committer for the whole batch.
func (d *DS) reconcileGroupLocked(ctx context.Context, groupID id.ID, acl bool) (int, error) {
	if !acl {
		return 0, nil
	}
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return 0, err
	}
	if row.ClosedAt != nil || row.EpochUnknown {
		return 0, nil
	}
	targets, err := d.opts.Store.ListBarredMembers(ctx, groupID)
	if err != nil {
		return 0, err
	}
	owed, err := d.aclRemovesOwed(ctx, groupID)
	if err != nil {
		return 0, err // cannot answer: remove nobody
	}
	targets = append(targets, owed...)
	if len(targets) == 0 {
		return 0, nil
	}
	outstanding, err := d.opts.Store.ListProposals(ctx, groupID, row.Epoch, false)
	if err != nil {
		return 0, err
	}
	// A leaf with a non-void INSTANCE Remove outstanding is skipped: a second would leave the first
	// unreferenced. A member's own self-Remove is not counted. It is not mandatory for a commit,
	// freezes nothing and elects nobody, so a member who lost access and keeps one posted would
	// otherwise never be removed; the instance's Remove is issued on top of it, OpenMLS keeps the
	// later of two Removes of one leaf, and the commit applies the instance's.
	pendingRemove := map[uint32]bool{}
	for _, p := range outstanding {
		if p.TargetLeaf != nil && isInstanceRemoveOf(p, *p.TargetLeaf) {
			pendingRemove[*p.TargetLeaf] = true
		}
	}
	release := d.suppressElections(groupID)
	proposed := 0
	for _, m := range targets {
		if pendingRemove[m.LeafIndex] {
			continue
		}
		// Bound to the device the read named; the read was under this lock, so it still holds the
		// leaf, and the binding is what lets a voided Remove of it be re-armed (DS-1).
		if err := d.proposeRemoveOfLocked(ctx, groupID, m.LeafIndex, &m.DeviceID, id.New()); err != nil {
			if ctx.Err() != nil {
				release()
				return proposed, ctx.Err()
			}
			d.log().Warn("a reconcile Remove was refused", "group", groupID.String()[:8],
				"leaf", m.LeafIndex, "err", err)
			continue
		}
		pendingRemove[m.LeafIndex] = true // a leaf both barred and ineligible is proposed once
		d.log().Info("reconcile: proposed a Remove for a leaf whose device is barred or whose user lost access",
			"group", groupID.String()[:8], "leaf", m.LeafIndex)
		proposed++
	}
	release()
	if proposed > 0 {
		if err := d.RequestCommit(ctx, groupID); err != nil {
			d.log().Error("electing a committer for reconcile Removes failed",
				"group", groupID.String()[:8], "err", err)
		}
	}
	return proposed, nil
}

// aclRemovesOwed is the leaves of a text or call group whose user the ACL no longer admits, after
// voiding the outstanding Adds no commit could carry. A group the ACL admits nobody to owes none:
// it is the leftover of a deleted channel whose Close was lost.
func (d *DS) aclRemovesOwed(ctx context.Context, groupID id.ID) ([]store.MemberRow, error) {
	if _, err := d.voidIneligibleAddsLocked(ctx, groupID); err != nil {
		return nil, err
	}
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return nil, err
	}
	eligible, err := d.leafHoldersEligible(ctx, groupID, members)
	if err != nil {
		return nil, err
	}
	anyEligible := false
	for _, ok := range eligible {
		anyEligible = anyEligible || ok
	}
	if !anyEligible {
		return nil, nil
	}
	var owed []store.MemberRow
	for _, m := range members {
		if m.RemovedEpoch == nil && !eligible[m.UserID] {
			owed = append(owed, m)
		}
	}
	return owed, nil
}

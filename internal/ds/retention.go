package ds

import (
	"context"
	"errors"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// SweepReport is what one sweeper run did. Every field is a count, never an error budget: a sweep
// that cannot prune reports zero and logs, it does not fail a request path.
type SweepReport struct {
	ProposalsVoided  int
	JoinsDrained     int // Adds the sweeper proposed from stalled join storms (pending_joins)
	LeavesReconciled int // Removes for leaves whose user the ACL no longer admits (reconcileLeaves)
	InactiveRemoved  int
	HandshakesPruned int64
	MessagesPruned   int64
	WelcomesPruned   int64
}

// Sweep voids expired proposals and applies both halves of retention.
//
// Delivery retention is how long the instance keeps an object so a device that was away can still
// fetch it: handshakes 30 days, Welcomes 30 days, application ciphertext until every eligible
// cursor has passed it or 30 days, whichever comes first.
//
// Archival retention is the community's own policy for ciphertext (default indefinite), recorded
// per message in `expires`, where NULL means retained. It is compared against NOW, never against
// the delivery floor, and it never deletes a row that set no expiry: the two halves are
// independent triggers and either one alone deletes. Nothing in Plan 1 writes `expires` — `Upload`
// writes NULL — so this half fires only once Plan 2's community policy fills the column. An
// archival deletion never raises the group's E_PRUNED high-water (Plan 2 task 8's retention
// ruling): `expires` need not be monotone in seq, and the mark must never stand above a message
// that survives.
//
// The sweep PAGES: `sweepPage` (task 21) is how many groups one query reads, and the loop runs
// until a short page. A fixed batch from the zero id would mean only the first N groups are ever
// swept — on an instance with more groups than the batch, the tail's ciphertext never prunes, and
// nothing else does that work.
//
// The loop ends with `d.closeUnhealedGroups(ctx)`, invariant 11's terminal path: a group nobody
// healed inside the 24-hour window is closed and re-created by the channel owner's device. Task 26
// recorded the call as owed to task 27 (`internal/ds/heal.go`), which now owns the method; it runs
// AFTER both retention passes so a group closed on this tick has already had its ciphertext swept
// by the same run.
func (d *DS) Sweep(ctx context.Context) (SweepReport, error) {
	var report SweepReport
	// Whatever this tick did, or failed to do, the outstanding-proposals gauge ends it equal to SQL:
	// that corrects the paths that end a proposal without decrementing (proposal_gauge.go).
	defer func() {
		if err := d.seedProposalGauge(ctx); err != nil {
			d.log().Error("re-seeding the outstanding-proposals gauge failed", "err", err)
		}
	}()

	voided, err := d.sweepProposals(ctx)
	if err != nil {
		return report, err
	}
	// A closed group's proposals are past every walk above, which reads open groups only.
	closedVoided, err := d.voidClosedGroupProposals(ctx)
	if err != nil {
		d.log().Error("voiding closed-group proposals failed", "err", err)
	}
	report.ProposalsVoided = voided + closedVoided

	// After the void, which frees the room a voided Add held: a join storm no commit re-drives —
	// its Adds voided by invariant 6's TTL, or its tail carried across a restart — takes its next
	// slice here.
	drained, err := d.drainStalledJoins(ctx)
	if err != nil {
		return report, err
	}
	report.JoinsDrained = drained

	// A membership change whose Removes were never issued is caught here, a page of groups a
	// tick (fix wave C3).
	reconciled, err := d.reconcileLeaves(ctx)
	if err != nil {
		return report, err
	}
	report.LeavesReconciled = reconciled

	removed, err := d.removeInactive(ctx)
	if err != nil {
		return report, err
	}
	report.InactiveRemoved = removed

	now := d.opts.Clock.Now()
	handshakeFloor := now.Add(-d.opts.Policy.HandshakeRetention).Unix()
	messageFloor := now.Add(-d.opts.Policy.MessageRetention).Unix()

	n, err := d.opts.Store.PruneHandshakes(ctx, handshakeFloor)
	if err != nil {
		return report, err
	}
	report.HandshakesPruned = n

	// A Welcome carries its own expiry, so the cutoff here is `now` rather than a retention
	// window: `PruneWelcomes` deletes the rows whose `expires` has passed.
	n, err = d.opts.Store.PruneWelcomes(ctx, now.Unix())
	if err != nil {
		return report, err
	}
	report.WelcomesPruned = n

	// activeSince is the eligibility horizon: a device that is revoked, disabled or unseen for
	// 90 days does not hold the floor. MinCursor applies all three, which is why it takes the
	// timestamp rather than the DS filtering rows afterwards.
	activeSince := now.Add(-d.opts.Policy.InactivityRemove).Unix()
	// The walk is over EVERY group, not only the open ones (`ListGroupsForRetention`, not
	// `ListOpenGroups`). Invariant 10 caps application ciphertext at thirty days, and a group
	// invariant 11 closed is still ciphertext on the disk: skipping closed groups would mean
	// neither half of retention ever touches their blobs again, and nothing else reclaims them,
	// so the invariant would invert into "kept forever" exactly when a group stops being useful.
	after := id.ID{}
	for {
		groups, err := d.opts.Store.ListGroupsForRetention(ctx, after, sweepPage)
		if err != nil {
			return report, err
		}
		if len(groups) == 0 {
			break
		}
		for _, g := range groups {
			floor, err := d.opts.Store.MinCursor(ctx, g.GroupID, activeSince)
			if err != nil {
				return report, err
			}
			pruned, err := d.opts.Store.PruneAppMessages(ctx, g.GroupID, floor, messageFloor, now.Unix())
			if err != nil {
				return report, err
			}
			report.MessagesPruned += pruned
			// PruneAppMessages raises the group's PrunedBelow to the highest seq it deleted, in
			// the same transaction, so the catch-up's E_PRUNED is exact; nothing is recorded here.
			after = g.GroupID
		}
		if len(groups) < sweepPage {
			break
		}
	}
	if err := d.closeUnhealedGroups(ctx); err != nil {
		return report, err
	}
	return report, nil
}

// removeInactive is protocol/01 § Cadence's inactivity rule: "a device that has not connected for
// 90 days is removed from every group by a DS Remove proposal" (founder decision 2026-09-23; the
// spec's chaos list says 30 and is wrong, deviation D13). A device connects when the gateway sends
// it `ready`, which records `devices.last_seen`; a device holding a live connection has connected,
// whatever that column says. It rejoins by external commit.
//
// One Remove per leaf: a leaf an outstanding non-void instance Remove already targets is left
// alone, so a sweep every minute does not stack a proposal per tick on a device that stays away.
// A member whose device row the instance does not hold is skipped rather than guessed at.
func (d *DS) removeInactive(ctx context.Context) (int, error) {
	horizon := d.opts.Clock.Now().Add(-d.opts.Policy.InactivityRemove).Unix()
	lastSeen := map[id.ID]int64{}
	removed := 0
	after := id.ID{}
	for {
		groups, err := d.opts.Store.ListOpenGroups(ctx, after, sweepPage)
		if err != nil {
			return removed, err
		}
		for _, g := range groups {
			after = g.GroupID
			members, err := d.opts.Store.ListMembers(ctx, g.GroupID)
			if err != nil {
				return removed, err
			}
			pending, err := d.opts.Store.ListProposals(ctx, g.GroupID, g.Epoch, false)
			if err != nil {
				return removed, err
			}
			targeted := map[uint32]bool{}
			for _, p := range pending {
				if p.Origin == 0 && p.Kind == uint8(mlswasi.ProposalRemove) && p.TargetLeaf != nil {
					targeted[*p.TargetLeaf] = true
				}
			}
			for _, m := range members {
				if m.RemovedEpoch != nil || targeted[m.LeafIndex] {
					continue
				}
				seen, ok := lastSeen[m.DeviceID]
				if !ok {
					device, err := d.opts.Store.GetDevice(ctx, m.DeviceID)
					if errors.Is(err, store.ErrNotFound) {
						continue
					}
					if err != nil {
						return removed, err
					}
					seen = device.LastSeen
					lastSeen[m.DeviceID] = seen
				}
				if seen >= horizon {
					continue
				}
				if d.opts.Gateway != nil && d.opts.Gateway.Online(m.DeviceID) {
					continue
				}
				// The member set was read outside the group lock, so the Remove names the device
				// it read and is refused when another device holds the leaf by the time the lock is
				// held (DS-2 of the server-half review): one commit can remove this device and add a
				// newcomer at its index. A device that left its leaf is nothing to do.
				err := d.ProposeRemoveOf(ctx, g.GroupID, m.LeafIndex, m.DeviceID, id.New())
				if errors.Is(err, ErrRemoveTargetGone) {
					continue
				}
				if err != nil {
					// One group's refusal must not stop the sweep of the others.
					d.log().Warn("an inactivity Remove was not proposed",
						"group", g.GroupID.String()[:8], "leaf", m.LeafIndex, "err", err)
					continue
				}
				removed++
			}
		}
		if len(groups) < sweepPage {
			return removed, nil
		}
	}
}

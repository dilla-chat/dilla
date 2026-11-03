package ds

import (
	"context"
	"errors"
	"slices"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// BatchPlan is the split of a set of devices into commit-sized batches.
type BatchPlan struct{ Batches [][]id.ID }

// PlanBatches splits devices into batches of at most limit, preserving order and
// dropping duplicates. With limit = MaxAddsPerCommit a batch is one commit's worth
// of Adds, and therefore at most one Welcome payload row: protocol/02's commit path
// writes exactly one mls_welcome_payloads row per commit (interfaces.md §6.2
// step 7), so the batch size is what bounds the Welcome fan-out. ProposeAddBatch
// queues the plan's devices (pending_joins drains oldest first, ties broken by
// device id), and the drain fills each commit's room from the queue, so a device
// dropped as no longer eligible leaves its seat to the next one waiting.
func PlanBatches(devices []id.ID, limit int) BatchPlan {
	if limit < 1 {
		limit = 1
	}
	seen := make(map[id.ID]bool, len(devices))
	uniq := make([]id.ID, 0, len(devices))
	for _, d := range devices {
		if seen[d] {
			continue
		}
		seen[d] = true
		uniq = append(uniq, d)
	}
	var plan BatchPlan
	for chunk := range slices.Chunk(uniq, limit) {
		plan.Batches = append(plan.Batches, chunk)
	}
	return plan
}

// stillEligible reports whether deviceID may still be added to groupID. It is
// re-read when a batch is drained, because a role revocation or a device
// revocation landing during a thousand-device join must not be undone by a batch
// that was planned before it.
//
// It is the single-device form of eligibleNow, which the drain uses with one
// snapshot of the group for a whole slice.
func (d *DS) stillEligible(ctx context.Context, groupID, deviceID id.ID, now int64) (bool, error) {
	snap, err := d.groupSnapshot(ctx, groupID)
	if err != nil {
		return false, err
	}
	return d.eligibleNow(ctx, snap, groupID, deviceID, now)
}

// groupSnapshot is what eligibleNow reads about the group once per slice rather than once per
// device: its live leaves and the devices an outstanding instance Add already targets. Reading
// ListMembers per device would make a 1,000-device drain O(n²) in rows read.
type groupSnapshot struct {
	live    map[id.ID]bool
	pending map[id.ID]bool
	// adds is the number of outstanding, non-void instance Adds at the group's epoch: what is left
	// of MaxAddsPerCommit is the room the next slice may take.
	adds int
}

func (d *DS) groupSnapshot(ctx context.Context, groupID id.ID) (groupSnapshot, error) {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return groupSnapshot{}, errNotFound("group")
	}
	if err != nil {
		return groupSnapshot{}, err
	}
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return groupSnapshot{}, err
	}
	snap := groupSnapshot{live: make(map[id.ID]bool, len(members)), pending: map[id.ID]bool{}}
	for _, m := range members {
		if m.RemovedEpoch == nil {
			snap.live[m.DeviceID] = true
		}
	}
	// The outstanding count is read at the GROUP'S CURRENT EPOCH, not at literal epoch 0: past
	// epoch 0 a `ListProposals(…, 0, …)` sees nothing, the room is always the full 256, and
	// successive batches push straight past MaxAddsPerCommit — which is the one thing the batching
	// exists to prevent.
	outstanding, err := d.opts.Store.ListProposals(ctx, groupID, row.Epoch, false)
	if err != nil {
		return groupSnapshot{}, err
	}
	for _, p := range outstanding {
		if p.Origin != 0 || p.VoidAt != nil || p.Kind != uint8(mlswasi.ProposalAdd) {
			continue
		}
		snap.adds++
		if p.TargetDevice != nil {
			snap.pending[*p.TargetDevice] = true
		}
	}
	return snap, nil
}

// eligibleNow is the eligibility rule itself. Five conditions, all of them already stored:
//
//   - the device row exists and is neither revoked nor quarantined;
//   - it holds at least one unconsumed, unexpired ordinary KeyPackage (the rule SyncGroupMembers
//     applies before it asks for the Add: an Add the directory cannot serve wastes the round);
//   - it is not already a live leaf of the group, and no outstanding instance Add targets it —
//     a second Add would spend a second KeyPackage and, committed, give one device two leaves;
//   - its user is eligible under the channel ACL. Invariant 4 refuses a commit whose Add names an
//     ineligible user, and a committer cannot omit an instance proposal, so ONE such Add in a
//     slice would stall every Add beside it. This is the check that turns a role revoked during a
//     join storm into a device dropped from the rest of it.
func (d *DS) eligibleNow(ctx context.Context, snap groupSnapshot, groupID, deviceID id.ID, now int64) (bool, error) {
	if snap.live[deviceID] || snap.pending[deviceID] {
		return false, nil
	}
	dev, err := d.opts.Store.GetDevice(ctx, deviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if dev.RevokedAt != nil || dev.QuarantinedAt != nil {
		return false, nil
	}
	n, err := d.opts.Store.CountKeyPackages(ctx, deviceID, now)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	return d.opts.ACL.Eligible(ctx, groupID, dev.UserID)
}

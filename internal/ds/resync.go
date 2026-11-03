package ds

import (
	"context"
	"errors"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// ResyncRequest is POST /v1/groups/{id}/resync: [external_commit(bstr), group_info(bstr)].
type ResyncRequest struct {
	ExternalCommit []byte
	GroupInfo      []byte
}

// Resync is an own-leaf external commit: how a device that has fallen out of the epoch returns.
//
// R25 exempts it from invariant 5's freeze, with two guards. First, it is refused when the
// resyncing device is the target of an outstanding non-void instance Remove — otherwise a device
// the instance is evicting could re-add itself forever. Second, the instance re-issues its
// outstanding proposals for the new epoch immediately afterwards, so the freeze that was skipped
// is not lost.
//
// Resync takes the group lock ONCE, for the whole read-guard-commit sequence, which is why it
// calls `commitLocked` rather than `commit`.
//
// Reading the epoch with `d.epochOf` outside the lock and only then calling `d.commit` — which
// takes the lock itself and compares `c.Epoch != row.Epoch` — would let a commit landing between
// the two reads turn a valid resync into a spurious E_COMMIT_CONFLICT, and the device that is
// already out of the epoch is precisely the one that cannot recover from it. `guardResyncTarget`
// would read the group row unlocked too. Every read that feeds commit's epoch comparison happens
// under the same lock acquisition.
func (d *DS) Resync(ctx context.Context, s Session, groupID id.ID, r ResyncRequest) (CommitResult, error) {
	defer d.flushEvictions(ctx, groupID) // after the unlock below: defers run last-in, first-out
	unlock := d.lock(groupID)
	defer unlock()
	return d.resyncLocked(ctx, s, groupID, r)
}

func (d *DS) resyncLocked(ctx context.Context, s Session, groupID id.ID, r ResyncRequest) (CommitResult, error) {
	// A quarantined device neither resyncs nor rejoins (invariant 9; deviation B36's ruling).
	if err := d.refuseQuarantined(ctx, s.DeviceID); err != nil {
		return CommitResult{}, err
	}
	epoch, err := d.epochOf(ctx, groupID)
	if err != nil {
		return CommitResult{}, err
	}
	// A device that holds no leaf has nothing to resync: its external commit is a JOIN
	// (protocol/01 § Joining), which is gated by the channel ACL and is NOT exempt from
	// invariant 5's freeze — R25's exemption exists for "the device that has fallen out of the
	// epoch", and a joiner was never in it. Deviation B36.
	_, leafErr := d.leafOf(ctx, groupID, s.DeviceID)
	var noLeaf *Error
	if leafErr != nil && !errors.As(leafErr, &noLeaf) {
		return CommitResult{}, leafErr // the store failed; that is not an answer about the leaf
	}
	joining := leafErr != nil
	if joining {
		ok, err := d.opts.ACL.Eligible(ctx, groupID, s.UserID)
		if err != nil {
			return CommitResult{}, err
		}
		if !ok {
			// E_NOT_FOUND, as every read answers a device the ACL does not admit: a 403 would
			// say the group exists.
			return CommitResult{}, errNotFound("group")
		}
	}
	if err := d.guardResyncTarget(ctx, s, groupID, epoch); err != nil {
		return CommitResult{}, err
	}
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return CommitResult{}, err
	}
	if err := d.requireGroupCapability(ctx, s, row); err != nil {
		return CommitResult{}, err
	}
	out, err := d.commitLocked(ctx, s, groupID, CommitRequest{
		Epoch:     epoch,
		Commit:    r.ExternalCommit,
		GroupInfo: r.GroupInfo,
	}, commitOptions{
		external:      true,
		skipFreeze:    !joining,
		joining:       joining,
		handshakeKind: handshakeExternalCommit,
	})
	if err != nil {
		return CommitResult{}, err
	}
	if joining {
		// A join the freeze admitted is invariant 5's nobody-online exception, whose re-issue
		// commit step (9) has already run (`reissueOmitted`). R25's belt-and-braces re-issue
		// below is the resync's own.
		return out, nil //nolint:nilerr // leafErr only records that the device holds no leaf, which is what makes this a join; the store failure case returned above
	}
	// R25's tail: whatever was outstanding is re-issued for the epoch the resync created, and a
	// committer is elected for it.
	//
	// `commitLocked` step (9) already ran `reissueOmitted` for the same epoch — every external
	// commit does — and each of ITS re-issues elects a committer of its own, so on the ordinary
	// path this call finds nothing and does nothing. It is still made, because `reissueOmitted` is
	// keyed on what the commit REFERENCED while this is keyed on what is still outstanding, and
	// because step (9) only LOGS its error: a proposal that survived both is one a freeze would
	// hold on to, and R25's promise is that a resync leaves the freeze intact.
	if err := d.reissueAll(ctx, groupID, out.Epoch); err != nil {
		return CommitResult{}, err
	}
	return out, nil
}

// guardResyncTarget refuses a resync — and a heal, which runs it at the restored epoch — by the
// target of an outstanding non-void instance Remove: by the device the Remove recorded, or by the
// leaf the device holds in mls_members. The refusal is 409 E_REMOVE_PENDING, "you are being removed"
// (errRemoveTarget): its own code, so a call client that rejoins while its leave's Remove stands
// waits for exactly this refusal and fails at once on any other 403 (the task 10 fix round).
func (d *DS) guardResyncTarget(ctx context.Context, s Session, groupID id.ID, epoch uint64) error {
	rows, err := d.opts.Store.ListProposals(ctx, groupID, epoch, false)
	if err != nil {
		return err
	}
	// The resyncing device may have no current leaf at all — that is the ordinary case, and why
	// it is resyncing — so a leaf lookup that fails is not an error here, only a clause that does
	// not apply.
	leaf, leafErr := d.leafOf(ctx, groupID, s.DeviceID)
	for _, p := range rows {
		if p.Origin != 0 || p.VoidAt != nil || p.Kind != uint8(mlswasi.ProposalRemove) {
			continue
		}
		if p.TargetDevice != nil && *p.TargetDevice == s.DeviceID {
			return errRemoveTarget()
		}
		if leafErr == nil && p.TargetLeaf != nil && *p.TargetLeaf == leaf {
			return errRemoveTarget()
		}
	}
	return nil
}

// checkExternalCommitScope is the third clause of invariant 4 for external commits: an external
// commit may Remove only the joiner's own previous leaf.
func (d *DS) checkExternalCommitScope(ctx context.Context, groupID id.ID, s Session, applied []mlswasi.AppliedProposal) error {
	for _, a := range applied {
		if a.Kind != mlswasi.ProposalRemove || a.TargetLeaf == nil {
			continue
		}
		device, err := d.deviceOfLeaf(ctx, groupID, *a.TargetLeaf)
		if err != nil {
			return err
		}
		if device != s.DeviceID {
			return errCommitInvalid("external_commit_remove_scope",
				"an external commit may only Remove the joining device's own previous leaf")
		}
	}
	return nil
}

// checkExternalCommitReplacesOwnLeaf is one device, one leaf (finding G1 of the second hardening
// review): an external commit by a device that already holds a leaf must remove every leaf that
// device holds, or the device ends with two leaves and the new one is a second identity beside the
// old. The device's leaves are the mls_members rows written from the pre-commit tree's own
// credentials, never anything the request names; the joiner is the session's device, which
// checkExternalJoiner then requires the new leaf to name. OpenMLS lets an external commit remove one
// leaf, so a device holding two can never pass. The core's public group applies the same rule in
// Process (`leaf_credentials_unchanged`), so this is the delivery service's own statement of it,
// under the joiner's rule name.
func (d *DS) checkExternalCommitReplacesOwnLeaf(ctx context.Context, groupID id.ID, s Session, applied []mlswasi.AppliedProposal) error {
	removed := make(map[uint32]struct{}, len(applied))
	for _, a := range applied {
		if a.Kind == mlswasi.ProposalRemove && a.TargetLeaf != nil {
			removed[*a.TargetLeaf] = struct{}{}
		}
	}
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.RemovedEpoch != nil || m.DeviceID != s.DeviceID {
			continue
		}
		if _, ok := removed[m.LeafIndex]; !ok {
			return errCommitInvalid("external_joiner",
				"an external commit must remove the leaf its device already holds (one device, one leaf)")
		}
	}
	return nil
}

// reissueAll re-issues every non-void instance proposal still outstanding at the epoch the resync
// replaced, for the new one, and elects a committer for what it re-issued.
//
// It is the belt-and-braces half of R25's tail. `commitLocked` step (9) already ran
// `reissueOmitted` over the same rows, and every re-issue there goes through
// `storeInstanceProposal`, which ends in `RequestCommit` — so on the ordinary path this loop finds
// nothing, and its only remaining job is to catch the case where step (9) logged an error instead
// of finishing.
//
// THE ELECTION IS CONDITIONAL ON THE LOOP, and that is not a micro-optimisation. `RequestCommit`
// -> `beginRound` ADVANCES the round and picks the next untried candidate, so calling it for work
// nobody re-issued fires one extra round that skips past the lowest-index online device invariant
// 7 names, and invalidates the round the candidate step (9) just elected was told to ack.
func (d *DS) reissueAll(ctx context.Context, groupID id.ID, epoch uint64) error {
	if epoch == 0 {
		return nil
	}
	rows, err := d.opts.Store.ListProposals(ctx, groupID, epoch-1, false)
	if err != nil {
		return err
	}
	reissued := false
	for _, r := range rows {
		if r.Origin != 0 || r.VoidAt != nil {
			continue
		}
		if err := d.reissue(ctx, groupID, r); err != nil {
			return err
		}
		reissued = true
	}
	if !reissued {
		return nil
	}
	return d.RequestCommit(ctx, groupID)
}

// epochOf is the group's epoch as SQL holds it — R12's record. It is a hard error rather than a
// zero default: a zero epoch fed to commit's `c.Epoch != row.Epoch` comparison would answer
// E_COMMIT_CONFLICT for a group that does not exist, which is a refusal the client cannot act on.
func (d *DS) epochOf(ctx context.Context, groupID id.ID) (uint64, error) {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, errNotFound("group")
	}
	if err != nil {
		return 0, err
	}
	return row.Epoch, nil
}

package ds

import (
	"context"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

// freezeState reports whether the group is frozen and, if so, the refs a committer must reference.
//
// Invariant 5 as amended (R10): the freeze holds while a non-void instance proposal is outstanding
// AND some member device is online. When every instance proposal has gone void, the freeze lifts
// even if no member device is online — otherwise a group whose members are all away is frozen
// forever, and invariant 11's close-and-recreate becomes the only exit.
//
// The refs are returned whether or not the freeze holds: the caller that accepts an external
// commit because nobody is online still has to re-issue everything the commit omitted, and that is
// the same list.
func (d *DS) freezeState(ctx context.Context, groupID id.ID, epoch uint64) (bool, [][]byte, error) {
	refs, err := d.outstandingRefs(ctx, groupID, epoch)
	if err != nil {
		return false, nil, err
	}
	if len(refs) == 0 {
		return false, nil, nil
	}
	// A nil gateway is "nobody is online": the composition root always supplies one, and a DS
	// built without it (the seam tests) must not report a freeze it has no way to lift.
	if d.opts.Gateway == nil {
		return false, refs, nil
	}
	if len(d.opts.Gateway.OnlineIn(groupID)) > 0 {
		return true, refs, nil
	}
	return false, refs, nil
}

// Frozen is the freeze predicate the API and the tests read, at the group's own epoch.
func (d *DS) Frozen(ctx context.Context, groupID id.ID) (bool, error) {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return false, err
	}
	frozen, _, err := d.freezeState(ctx, groupID, row.Epoch)
	return frozen, err
}

// outstandingRefs is the freeze predicate WITHOUT the online clause: the refs of every non-void
// INSTANCE proposal at this epoch.
//
// The online clause belongs to the external-commit path alone. protocol/02 invariant 5 reads
// "application messages get 425 commit_required, and external commits get 425 commit_required too
// — unless no member device is online, in which case the EXTERNAL COMMIT is accepted". A device
// holding an HTTP session but no gateway connection is not online under R10, so sharing
// freezeState with the message path would let it upload ciphertext straight through a live freeze.
//
// It is a different answer from `refsOf`, which the two conflict bodies use: that one lists every
// non-void proposal whatever its origin, because a committer has to reference member proposals
// too. This one is the freeze's own input and counts only origin 0.
func (d *DS) outstandingRefs(ctx context.Context, groupID id.ID, epoch uint64) ([][]byte, error) {
	rows, err := d.opts.Store.ListProposals(ctx, groupID, epoch, false)
	if err != nil {
		return nil, err
	}
	refs := make([][]byte, 0, len(rows))
	for _, r := range rows {
		if r.Origin != 0 || r.VoidAt != nil {
			continue
		}
		refs = append(refs, r.Ref)
	}
	return refs, nil
}

// requireNoFreeze is the guard on the message path: a message during a freeze is
// E_COMMIT_REQUIRED carrying the refs, and the client commits them (or waits for
// mls.commit_needed) and resends. It does NOT consult the online predicate — see outstandingRefs.
func (d *DS) requireNoFreeze(ctx context.Context, groupID id.ID, epoch uint64) error {
	refs, err := d.outstandingRefs(ctx, groupID, epoch)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return errCommitRequired(refs, uint64(d.opts.Policy.CommitDeadline.Milliseconds())) //nolint:gosec // G115: a non-negative duration in milliseconds
	}
	return nil
}

// reissueOmitted re-issues every non-void instance proposal the commit did not reference, for the
// new epoch, and keeps the freeze. Only an EXTERNAL commit can omit one: clause 1 exempts it on
// invariant 5's nobody-online path and on R25's resync (`clause1Exempt`), and commit step (9) calls
// this for external commits alone. A member commit is never exempt; the one instance proposal it
// may leave unreferenced is a Remove it satisfied by applying another Remove of the same leaf
// (invariant 4, rule (3a) of the task-9 review), and that row is deleted in the commit's own
// transaction, so it is never read here and is owed nothing.
//
// It runs with the group lock held but OUTSIDE withGroup: everything it reaches calls withGroup
// again to queue the re-signed proposal in the guest, and the handle lock withGroup takes is a
// plain sync.Mutex. Calling it from inside the commit's own withGroup closure would deadlock that
// group's request goroutine for the life of the process. The property holds because `reissue`
// calls only the lock-free proposal bodies and `RequestCommit` takes `elections.mu`, the store and
// the gateway — never `d.lock(groupID)`; TestACommitThatReissuesAnOmittedRemoveDoesNotDeadlock
// (resync_test.go) is its regression guard.
func (d *DS) reissueOmitted(ctx context.Context, groupID id.ID, oldEpoch uint64, applied []mlswasi.AppliedProposal) error {
	rows, err := d.opts.Store.ListProposals(ctx, groupID, oldEpoch, false)
	if err != nil {
		return err
	}
	referenced := map[string]struct{}{}
	for _, a := range applied {
		referenced[string(a.ProposalRef)] = struct{}{}
	}
	for _, r := range rows {
		if r.Origin != 0 || r.VoidAt != nil {
			continue
		}
		if _, ok := referenced[string(r.Ref)]; ok {
			continue
		}
		if err := d.reissue(ctx, groupID, r); err != nil {
			return err
		}
	}
	return nil
}

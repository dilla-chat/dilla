package ds

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// sweepPage is how many groups one page of a sweep reads. It is a page size, not a bound on the
// work: every sweeper in this plan loops until a short page.
const sweepPage = 512

// ProposeAdd issues an external Add. Invariant 6's "before proposing" gate runs first: the
// KeyPackage must validate, its lifetime must not have expired, it must advertise 0xF001 and it
// must not already be consumed — the expiry and the consumption are `TakeKeyPackage`'s, against
// the delivery service's own clock, and the rest is `validate_key_package`'s inside the guest.
func (d *DS) ProposeAdd(ctx context.Context, groupID, deviceID, actionID id.ID) error {
	unlock := d.lock(groupID)
	defer unlock()
	return d.proposeAddLocked(ctx, groupID, deviceID, actionID)
}

func (d *DS) proposeAddLocked(ctx context.Context, groupID, deviceID, actionID id.ID) error {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("group")
	}
	if err != nil {
		return err
	}
	// Invariant 6's "validate before proposing", for the target itself. A device that is already
	// a current leaf is refused, and a device whose Add is already outstanding at this epoch is
	// not proposed twice: two Adds for one signature key make every commit that carries them
	// invalid (OpenMLS refuses the commit with DuplicateSignatureKey, so the group freezes) and
	// would spend a second KeyPackage. The check runs under the group lock, so two proposers
	// racing for one device (a channel-membership sync and an admit, say) cannot both pass it;
	// the second is a no-op, because the action it asks for is already in flight.
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.RemovedEpoch == nil && m.DeviceID == deviceID {
			refusal := errInvalid("the device is already a member of the group")
			refusal.cause = ErrAlreadyMember
			return refusal
		}
	}
	outstanding, err := d.opts.Store.ListProposals(ctx, groupID, row.Epoch, false)
	if err != nil {
		return err
	}
	for _, p := range outstanding {
		if p.Origin == 0 && p.VoidAt == nil && p.Kind == uint8(mlswasi.ProposalAdd) &&
			p.TargetDevice != nil && *p.TargetDevice == deviceID {
			return nil
		}
	}

	inst, err := d.opts.Wasm.Acquire(ctx)
	if err != nil {
		return err
	}
	defer inst.Release()

	// Invariant 4's device-list clause, checked BEFORE a KeyPackage is spent (invariant 6's
	// "before proposing"). A device that enrolled and published its KeyPackages before its user's
	// new signed list names it (protocol/03 § Pairing, steps 2 and 5) would otherwise be proposed:
	// every member commit that includes the Add is refused by checkAddedMember, every commit that
	// omits it is refused by clause 1, and the group is frozen until the Add voids, having also
	// spent the KeyPackage the pairing needed.
	if err := d.checkListedDevice(ctx, inst, deviceID); err != nil {
		return err
	}
	kp, err := d.takeBoundKeyPackage(ctx, inst, deviceID)
	if err != nil {
		return err
	}
	blob, err := inst.ExternalProposeAdd(ctx, groupID[:], row.Epoch, kp.Blob,
		d.opts.Keys.ExternalSenderPriv[:])
	if err != nil {
		return err
	}
	return d.storeInstanceProposal(ctx, groupID, row, blob, store.ProposalRow{
		Kind:         uint8(mlswasi.ProposalAdd),
		TargetDevice: &deviceID,
		KeyPackage:   kp.Blob,
		Origin:       0,
		ActionID:     actionID,
	})
}

// takeBoundKeyPackage takes the device's next directory KeyPackage that is bound to the device —
// its credential names the device and the device's user, and its leaf is keyed by the device's
// registered key (leafKeyIsRegistered, the binding PublishKeyPackages applies on upload). A package
// that is not — one stored before that binding existed — would become an Add no commit can carry
// (checkAddedMember refuses it, and clause 1 refuses every commit that leaves it out), so it is
// deleted from the directory before it is spent and the device's next package is tried. Each pass
// takes a row out of the directory (consumed or deleted), so the loop ends; an empty directory is
// the same "no usable KeyPackage" refusal as ever. A package that no longer validates at all is
// refused as before.
func (d *DS) takeBoundKeyPackage(ctx context.Context, inst *mlswasi.Instance, deviceID id.ID) (store.KeyPackageRow, error) {
	device, err := d.opts.Store.GetDevice(ctx, deviceID)
	if err != nil {
		return store.KeyPackageRow{}, err
	}
	for {
		kp, err := d.opts.Store.TakeKeyPackage(ctx, deviceID, d.now())
		if errors.Is(err, store.ErrNotFound) {
			return store.KeyPackageRow{}, errInvalid("no usable KeyPackage for that device")
		}
		if err != nil {
			return store.KeyPackageRow{}, err
		}
		info, err := inst.ValidateKeyPackage(ctx, kp.Blob)
		if err != nil {
			return store.KeyPackageRow{}, errInvalid("the stored KeyPackage no longer validates: " + err.Error())
		}
		if bytes.Equal(info.DeviceID, deviceID[:]) && bytes.Equal(info.UserID, device.UserID[:]) &&
			leafKeyIsRegistered(device, info.SignatureKey) {
			// A package stored before the kind binding existed (Task S1) is dropped like an
			// unbound one: checkAddedMember would refuse the Add that carried it.
			kindErr := checkCredentialKind(ctx, d.opts.Store, device.UserID, info.Credential)
			if kindErr == nil {
				return kp, nil
			}
			if !errors.Is(kindErr, errCredentialKind) {
				return store.KeyPackageRow{}, kindErr
			}
		}
		d.log().Warn("dropping a directory KeyPackage not bound to its device's registered key or account kind",
			"device", deviceID.String()[:8], "last_resort", kp.LastResort == 1)
		if err := d.opts.Store.DeleteKeyPackage(ctx, deviceID, kp.KPRef); err != nil {
			return store.KeyPackageRow{}, err
		}
	}
}

// checkListedDevice answers errInvalid unless the device exists, is live, and its DSK is in the
// newest signed device list of its user, verified in v (the guest the caller holds). It is the
// same test checkAddedMember applies to the commit that will carry the Add, so an Add the delivery
// service proposes is one a member can commit. A list the verifier cannot reach at all
// (ErrDeviceListUnavailable) is returned as is: that is "cannot answer", not "not listed".
func (d *DS) checkListedDevice(ctx context.Context, v DeviceListVerifier, deviceID id.ID) error {
	dev, err := d.opts.Store.GetDevice(ctx, deviceID)
	if errors.Is(err, store.ErrNotFound) {
		return errInvalid("the device is unknown to this instance")
	}
	if err != nil {
		return err
	}
	if dev.RevokedAt != nil || dev.QuarantinedAt != nil {
		return errInvalid("the device is revoked or quarantined")
	}
	entries, err := d.opts.DeviceLists.Entries(ctx, v, dev.UserID)
	if err != nil {
		if errors.Is(err, ErrDeviceListUnavailable) || ctx.Err() != nil {
			return err
		}
		return errInvalid("no verifiable signed device list for the device's user: " + err.Error())
	}
	if auth.Listed(entries, dev.ID, dev.DSKPub) {
		return nil
	}
	return errInvalid("the device's DSK is not in its user's newest signed device list")
}

// ProposeRemove issues an external Remove. A target leaf that is already gone is dropped rather
// than proposed (invariant 6's second half), and so is one the instance is already removing: a
// second instance Remove of one leaf would leave the first unreferenced (OpenMLS keeps the later of
// two Removes of one leaf), and clause 1 would refuse every commit until its TTL. A member's own
// Remove of the leaf does not count: the instance's is issued on top of it.
//
// Like ProposeAdd it is a thin locking wrapper over a lock-free body. d.lock hands out a plain
// per-group sync.Mutex, which is not reentrant, and both re-issue entry points already hold it:
// `ReissueFor` takes it at the top, and `reissueOmitted` runs inside the commit path, which has
// held it since its first statement. A ProposeRemove that re-locked would deadlock the group's
// request goroutine forever — on exactly invariant 5's nobody-online path that reissueOmitted
// exists to serve.
func (d *DS) ProposeRemove(ctx context.Context, groupID id.ID, leaf uint32, actionID id.ID) error {
	unlock := d.lock(groupID)
	defer unlock()
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("group")
	}
	if err != nil {
		return err
	}
	standing, err := d.instanceRemoveOutstanding(ctx, groupID, row.Epoch, leaf)
	if err != nil || standing {
		return err
	}
	return d.proposeRemoveLocked(ctx, groupID, leaf, actionID)
}

// ProposeRemoveOf is ProposeRemove for a caller that read the leaf outside the group lock and knows
// which device it meant (m1 of the task-9 review): the Remove is issued only while deviceID still
// holds leaf, and is otherwise refused exactly as a Remove of a leaf that is gone. MLS reuses a blank
// leaf, so a leaf index read before the lock can name another device by the time the lock is held —
// one commit can remove the device and add another at its index — and a Remove by index alone would
// then remove the newcomer.
func (d *DS) ProposeRemoveOf(ctx context.Context, groupID id.ID, leaf uint32, deviceID, actionID id.ID) error {
	unlock := d.lock(groupID)
	defer unlock()
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("group")
	}
	if err != nil {
		return err
	}
	standing, err := d.instanceRemoveOutstanding(ctx, groupID, row.Epoch, leaf)
	if err != nil || standing {
		return err
	}
	return d.proposeRemoveOfLocked(ctx, groupID, leaf, &deviceID, actionID)
}

// ProposeRemoveOfMember is ProposeRemoveOf bound to one MEMBERSHIP rather than one device: the
// Remove is issued only while deviceID holds leaf with the added epoch the caller read
// (mls_members.added_epoch), and is otherwise refused with ErrRemoveTargetGone. A device that left
// and came back at the same leaf index between the caller's read and the group lock — a call
// leaver rejoining by external commit, which MLS puts in the leftmost blank leaf — holds a new
// membership, and a Remove meant for the old one must not end it.
//
// The rule for added_epoch (replaceMembersTx): a leaf keeps the epoch its device took it at across
// every commit while that device holds it; a leaf another device takes, and the leaf an external
// commit puts its joiner on (an own-leaf resync or an external join), start at that commit's epoch
// — at the device's old index or another one alike. So a Remove bound to the membership before a
// resync is always refused, whatever the blank-leaf layout (R-3 of the DS re-review).
func (d *DS) ProposeRemoveOfMember(ctx context.Context, groupID id.ID, leaf uint32, deviceID id.ID, addedEpoch uint64, actionID id.ID) error {
	unlock := d.lock(groupID)
	defer unlock()
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("group")
	}
	if err != nil {
		return err
	}
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return err
	}
	same := false
	for _, m := range members {
		if m.LeafIndex == leaf && m.RemovedEpoch == nil {
			same = m.DeviceID == deviceID && m.AddedEpoch == addedEpoch
		}
	}
	if !same {
		gone := errInvalid("the Remove target no longer holds that membership; the action is dropped")
		gone.cause = ErrRemoveTargetGone
		return gone
	}
	standing, err := d.instanceRemoveOutstanding(ctx, groupID, row.Epoch, leaf)
	if err != nil || standing {
		return err
	}
	return d.proposeRemoveOfLocked(ctx, groupID, leaf, &deviceID, actionID)
}

func (d *DS) proposeRemoveLocked(ctx context.Context, groupID id.ID, leaf uint32, actionID id.ID) error {
	return d.proposeRemoveOfLocked(ctx, groupID, leaf, nil, actionID)
}

// ErrRemoveTargetGone is the cause of the refusal of a Remove whose leaf no longer holds the device
// it was meant for — no device at all, or another one (errors.Is matches it through the *Error).
// ProposeRemoveDevice and the inactivity sweep read it as "nothing to remove"; a caller of
// ProposeRemoveOf outside the package can read it the same way.
var ErrRemoveTargetGone = errors.New("ds: the Remove target is no longer at its leaf")

// proposeRemoveOfLocked proposes removing leaf with the group lock held. With expect set, the device
// at leaf must be *expect or the Remove is refused (ErrRemoveTargetGone), never aimed at whoever
// holds the leaf instead.
func (d *DS) proposeRemoveOfLocked(ctx context.Context, groupID id.ID, leaf uint32, expect *id.ID, actionID id.ID) error {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("group")
	}
	if err != nil {
		return err
	}
	// The device holding the leaf NOW, under the group lock, is the one this Remove is for. It is
	// recorded on the row: MLS reuses a blank leaf, so a later re-issue by leaf alone could remove
	// whoever joined at the index after this device left (removeStillTargets).
	device, present, err := d.deviceAtLeaf(ctx, groupID, leaf)
	if err != nil {
		return err
	}
	if !present || (expect != nil && device != *expect) {
		gone := errInvalid("the Remove target is no longer a member; the action is dropped")
		gone.cause = ErrRemoveTargetGone
		return gone
	}
	inst, err := d.opts.Wasm.Acquire(ctx)
	if err != nil {
		return err
	}
	defer inst.Release()

	blob, err := inst.ExternalProposeRemove(ctx, groupID[:], row.Epoch, leaf,
		d.opts.Keys.ExternalSenderPriv[:])
	if err != nil {
		return err
	}
	return d.storeInstanceProposal(ctx, groupID, row, blob, store.ProposalRow{
		Kind:         uint8(mlswasi.ProposalRemove),
		TargetLeaf:   &leaf,
		TargetDevice: &device,
		Origin:       0,
		ActionID:     actionID,
	})
}

// sameTarget reports whether two proposal rows name the same target leaf and device.
func sameTarget(a, b store.ProposalRow) bool {
	eqLeaf := (a.TargetLeaf == nil) == (b.TargetLeaf == nil) && (a.TargetLeaf == nil || *a.TargetLeaf == *b.TargetLeaf)
	eqDevice := (a.TargetDevice == nil) == (b.TargetDevice == nil) && (a.TargetDevice == nil || *a.TargetDevice == *b.TargetDevice)
	return eqLeaf && eqDevice
}

// deviceAtLeaf answers which device holds leaf in the group's current member set.
func (d *DS) deviceAtLeaf(ctx context.Context, groupID id.ID, leaf uint32) (id.ID, bool, error) {
	m, present, err := d.memberAtLeaf(ctx, groupID, leaf)
	return m.DeviceID, present, err
}

// memberAtLeaf answers the membership that holds leaf in the group's current member set.
func (d *DS) memberAtLeaf(ctx context.Context, groupID id.ID, leaf uint32) (store.MemberRow, bool, error) {
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return store.MemberRow{}, false, err
	}
	for _, m := range members {
		if m.LeafIndex == leaf && m.RemovedEpoch == nil {
			return m, true, nil
		}
	}
	return store.MemberRow{}, false, nil
}

// removeStillTargets reports whether an instance Remove issued as old still names the membership
// that holds its leaf now. A Remove that recorded its device is re-issued only onto that device's
// membership the Remove was issued for: one it took at or before the Remove's epoch (added_epoch,
// as ProposeRemoveOfMember binds). The same device back at the same leaf by an own-leaf resync or an
// external join holds a new membership from that commit's epoch, later than any Remove issued
// before it, and a voided Remove is never re-aimed at it (the f10app review's re-drive card). One
// that recorded no device (written before devices were recorded) is re-issued only within the epoch
// it was issued in, where no commit can have moved anybody onto the leaf.
func (d *DS) removeStillTargets(ctx context.Context, groupID id.ID, old store.ProposalRow) (bool, error) {
	member, present, err := d.memberAtLeaf(ctx, groupID, *old.TargetLeaf)
	if err != nil || !present {
		return false, err
	}
	if old.TargetDevice != nil {
		return member.DeviceID == *old.TargetDevice && member.AddedEpoch <= old.Epoch, nil
	}
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return false, err
	}
	return old.Epoch == row.Epoch, nil
}

// ProposeRemoveDevice proposes removing deviceID's leaf (DEV-45). The leaf is resolved under the
// group lock — a leaf read outside it can name another device once its index is reused — and the
// action is dropped, answering nil, when the device holds no leaf or when a non-void INSTANCE
// Remove of that leaf already stands: OpenMLS keeps only the later of two Removes of one leaf, so a
// second instance Remove would leave the first unreferenced and invariant 4's clause 1 would refuse
// every commit until its TTL. A member's own Remove of the leaf never stands in for the instance's:
// it is not mandatory for a commit, freezes nothing and elects nobody, so the instance issues its
// Remove on top of it. Whichever of the two the commit applies removes the device, and clause 1
// counts the instance's as satisfied by an applied Remove of its leaf.
//
// The Remove is built for deviceID only: proposeRemoveOfLocked checks it still holds the leaf.
func (d *DS) ProposeRemoveDevice(ctx context.Context, groupID, deviceID, actionID id.ID) error {
	unlock := d.lock(groupID)
	defer unlock()
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("group")
	}
	if err != nil {
		return err
	}
	leaf, err := d.leafOf(ctx, groupID, deviceID)
	if err != nil {
		var dsErr *Error
		if errors.As(err, &dsErr) && dsErr.Code == errLeafNotCurrent().Code {
			return nil // no leaf: the device is not in the group, and there is nothing to remove
		}
		return err
	}
	standing, err := d.instanceRemoveOutstanding(ctx, groupID, row.Epoch, leaf)
	if err != nil || standing {
		return err
	}
	if err := d.proposeRemoveOfLocked(ctx, groupID, leaf, &deviceID, actionID); err != nil {
		if errors.Is(err, ErrRemoveTargetGone) {
			return nil // the device left the leaf: there is nothing to remove
		}
		return err
	}
	return nil
}

// instanceRemoveOutstanding reports whether a non-void INSTANCE Remove of leaf stands at epoch. A
// member's own Remove is deliberately not counted: a member proposal never cancels, blocks or stands
// in for an instance proposal.
func (d *DS) instanceRemoveOutstanding(ctx context.Context, groupID id.ID, epoch uint64, leaf uint32) (bool, error) {
	rows, err := d.opts.Store.ListProposals(ctx, groupID, epoch, false)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if isInstanceRemoveOf(r, leaf) {
			return true, nil
		}
	}
	return false, nil
}

// isInstanceRemoveOf is a non-void instance Remove of leaf.
func isInstanceRemoveOf(r store.ProposalRow, leaf uint32) bool {
	return r.Origin == 0 && r.VoidAt == nil && mlswasi.ProposalKind(r.Kind) == mlswasi.ProposalRemove &&
		r.TargetLeaf != nil && *r.TargetLeaf == leaf
}

// redriveCallRemovesLocked re-issues, keeping its action_id, every voided instance Remove at
// fromEpoch whose device still holds its leaf and which no non-void INSTANCE Remove at curEpoch
// already covers (F9, DEV-50). In a call group a void lifts the freeze but leaves the member
// decrypting; only a commit removes it, so the instance asks again. A member's own Remove of the
// leaf does not cover it: a member proposal never stands in for an instance proposal. The device
// check is reissue's (removeStillTargets): MLS reuses blank leaves, so a Remove re-issued by leaf
// alone after its device left could remove whoever joined at the index since. The sweep calls it
// with fromEpoch == curEpoch; the commit path calls it with the epoch a commit left behind, whose
// voided rows no sweep reads again. The group lock is held.
func (d *DS) redriveCallRemovesLocked(ctx context.Context, groupID id.ID, fromEpoch, curEpoch uint64) error {
	from, err := d.opts.Store.ListProposals(ctx, groupID, fromEpoch, true)
	if err != nil {
		return err
	}
	cur := from
	if curEpoch != fromEpoch {
		if cur, err = d.opts.Store.ListProposals(ctx, groupID, curEpoch, false); err != nil {
			return err
		}
	}
	live := map[uint32]bool{}
	for _, r := range cur {
		if r.TargetLeaf != nil && isInstanceRemoveOf(r, *r.TargetLeaf) {
			live[*r.TargetLeaf] = true
		}
	}
	for _, r := range from {
		if r.VoidAt == nil || r.Origin != 0 || mlswasi.ProposalKind(r.Kind) != mlswasi.ProposalRemove ||
			r.TargetLeaf == nil || live[*r.TargetLeaf] {
			continue
		}
		// reissue drops (deletes) a Remove whose device no longer holds the leaf, and re-issues the
		// rest onto the device they were issued for.
		if err := d.reissue(ctx, groupID, r); err != nil {
			return err
		}
		live[*r.TargetLeaf] = true
	}
	return nil
}

// ProposeAddBatch adds many devices to one group, at most MaxAddsPerCommit (256) Adds per commit,
// until every device still eligible is in: protocol/01 § Joining's "the creator's device commits
// at most 256 Adds per commit, each producing one Welcome, until all eligible devices are
// members".
//
// The batch is QUEUED WHOLE, durably, and then drained: the first slice is proposed now, into
// whatever room the group's current epoch has left, and each later slice by the commit that
// applies the one before it (commit step 9) or, when no commit comes, by the sweeper. It cannot
// loop "propose 256, request a commit, propose 256" in one call: the instance does not commit, a
// member does, asynchronously, and a second slice proposed before the first is committed would sit
// at the same epoch and push one commit past the cap the batching exists to keep.
//
// Plan 2 task 7's brief rewrote this method as that loop; it is this form instead because of the
// sentence above. What the brief wanted from the loop is kept: PlanBatches splits and de-duplicates
// the batch, and stillEligible's rule (eligibleNow) is re-read for every device when its slice is
// drained, so a device revoked, or a user whose role was revoked, while the storm runs is dropped
// from the rest of it rather than added by a batch planned before the change.
//
// The queue is `pending_joins` (Plan 1 follow-up card 8, deviation B22, ruling 43): a restart
// between two slices loses nothing.
func (d *DS) ProposeAddBatch(ctx context.Context, groupID id.ID, devices []id.ID) error {
	// The per-group lock is taken once for the whole call, exactly as every other write path on
	// *DS takes it: the room a slice may take is read from the group's outstanding Adds, and two
	// drains interleaving on one group would both see the same room.
	unlock := d.lock(groupID)
	defer unlock()

	if _, err := d.opts.Store.GetGroup(ctx, groupID); errors.Is(err, store.ErrNotFound) {
		return errNotFound("group")
	} else if err != nil {
		return err
	}
	plan := PlanBatches(devices, d.opts.Policy.MaxAddsPerCommit)
	if err := d.opts.Store.QueuePendingJoins(ctx, groupID, slices.Concat(plan.Batches...), d.now()); err != nil {
		return err
	}
	_, err := d.drainPendingJoins(ctx, groupID)
	return err
}

// drainPendingJoins proposes the next slice of the group's join storm: as many queued devices as
// the group's epoch has room for, each re-checked by eligibleNow when it is taken. It runs with the
// group lock already held — by ProposeAddBatch, by the commit path after a merge (withGroup has
// returned by then), and by the sweeper — so it calls only the lock-free proposal bodies.
//
// A device that is no longer eligible, or whose Add the guest or the directory refuses, is DROPPED
// from the queue and logged, never re-queued: it would only be refused again, and a device that
// becomes eligible later is queued again by the membership change that made it so
// (api.SyncGroupMembers). A device the check cannot ANSWER for — a store or ACL fault, or the
// context ending — stays queued, with every device of the slice after it, and the drain stops: a
// transient fault must not silently shorten a storm.
//
// The queue is READ, not taken: a device's row is deleted only once the device is resolved —
// after its Add is stored, or once it has been judged ineligible or refused. So a process that
// dies in the middle of a slice (a restart, an OOM kill) leaves every device it had not resolved
// in pending_joins for the sweeper, where a take-then-propose would have lost them; no error path
// runs on a crash. Dying between the Add's store and the row's delete is harmless too: the
// restarted drain finds the outstanding Add (eligibleNow's pending check) and drops the row
// without spending a second KeyPackage.
//
// It elects once for the whole slice, exactly as the batch it drains would: the commit that
// applied this epoch's Adds is followed by ONE mls.commit_needed for the next slice, never by one
// per Add.
func (d *DS) drainPendingJoins(ctx context.Context, groupID id.ID) (int, error) {
	queued, err := d.opts.Store.CountPendingJoins(ctx, groupID)
	if err != nil || queued == 0 {
		return 0, err
	}
	release := d.suppressElections(groupID)
	issued, err := d.drainSlice(ctx, groupID)
	release()
	if issued > 0 {
		// Logged, not returned, for storeInstanceProposal's own reason: every proposal of the
		// slice is already durable and fanned out, and a failed election is re-run by the
		// watchdog's next tick.
		if rerr := d.RequestCommit(ctx, groupID); rerr != nil {
			d.log().Error("electing a committer for a slice of a join storm failed",
				"group", groupID.String()[:8], "err", rerr)
		}
	}
	return issued, err
}

func (d *DS) drainSlice(ctx context.Context, groupID id.ID) (int, error) {
	now := d.now()
	issued := 0
	for {
		snap, err := d.groupSnapshot(ctx, groupID)
		if err != nil {
			return issued, err
		}
		room := d.opts.Policy.MaxAddsPerCommit - snap.adds
		if room <= 0 {
			return issued, nil
		}
		queued, err := d.opts.Store.ListPendingJoins(ctx, groupID, int32(room)) //nolint:gosec // G115: room is at most MaxAddsPerCommit
		if err != nil || len(queued) == 0 {
			return issued, err
		}
		for _, device := range queued {
			ok, err := d.eligibleNow(ctx, snap, groupID, device, now)
			if err == nil && ctx.Err() != nil {
				err = ctx.Err()
			}
			if err != nil {
				// Unanswered: this device and the rest of the slice are still queued.
				return issued, err
			}
			if !ok {
				d.log().Info("queued add dropped: the device is no longer eligible",
					"group", groupID.String()[:8], "device", device.String()[:8])
			} else if err := d.proposeAddLocked(ctx, groupID, device, id.New()); err != nil {
				if ctx.Err() != nil {
					return issued, ctx.Err()
				}
				if errors.Is(err, ErrDeviceListUnavailable) {
					// Unanswered, not refused: the device stays queued.
					return issued, err
				}
				// One unusable KeyPackage must not sink the slice: the device is skipped and the
				// storm continues.
				d.log().Warn("queued add skipped", "group", groupID.String()[:8],
					"device", device.String()[:8], "err", err)
			} else {
				snap.pending[device] = true
				issued++
			}
			// Resolved one way or the other: only now does the device leave the queue.
			if err := d.opts.Store.DeletePendingJoins(ctx, groupID, []id.ID{device}); err != nil {
				return issued, err
			}
		}
		// Every device dropped or skipped left its room free; the loop reads the next ones into
		// it, so a slice is filled while the queue holds eligible devices.
	}
}

// drainStalledJoins is the sweeper's half of the drain: every group holding a queue gets one
// drain, under its lock. It is what re-drives a storm no commit re-drives — one whose outstanding
// Adds were voided (invariant 6's TTL), and one in flight across a restart. A group whose epoch has
// no room issues nothing.
func (d *DS) drainStalledJoins(ctx context.Context) (int, error) {
	drained := 0
	after := id.ID{}
	for {
		groups, err := d.opts.Store.ListPendingJoinGroups(ctx, after, sweepPage)
		if err != nil {
			return drained, err
		}
		for _, groupID := range groups {
			after = groupID
			unlock := d.lock(groupID)
			n, err := d.drainPendingJoins(ctx, groupID)
			unlock()
			drained += n
			if err != nil {
				d.log().Error("draining a stalled join storm failed", "group", groupID.String()[:8], "err", err)
			}
		}
		if len(groups) < sweepPage {
			return drained, nil
		}
	}
}

// storeInstanceProposal appends the handshake, stores the pending row and fans the proposal out.
//
// The put into the guest's queue necessarily precedes the transaction: the SQL row is keyed on the
// ref `ProposalPut` hands back. So, exactly as `queueMemberProposal` does for a member's proposal,
// every failure after the put takes the proposal back OUT of the queue. A proposal left behind is
// one the cached PublicGroup holds and SQL does not — R12's "SQL is the record", inverted — and it
// is not merely held in memory: the next SUCCESSFUL proposal's `persistState` runs inside its own
// transaction and writes the divergence into the durable state blob, where it survives a restart
// and is cleared only by the next merge.
//
// THE SAME PROPOSAL AGAIN (DS-1 of the server-half review). The external sender signs
// deterministically and the frame carries no nonce, so a proposal re-signed in the epoch it was
// voided in — the reconcile, the inactivity sweep, a repeated kick, a call re-drive — is
// byte-identical to the voided one, and `ProposalPut` hands back the voided row's ref. That row is
// RE-ARMED through `ReissueProposal` (delete then insert, in this transaction): live again, issued
// now, a fresh TTL, its original action_id. A plain INSERT would hit the primary key. And a failure
// on the way must not take the ref out of the guest's queue, which held it before this call:
// unqueueProposal refuses a ref SQL still holds.
func (d *DS) storeInstanceProposal(ctx context.Context, groupID id.ID, row store.GroupRow, blob []byte, p store.ProposalRow) error {
	var seq uint64
	err := d.withGroup(ctx, groupID, func(g *mlswasi.PublicGroup) error {
		ref, err := g.ProposalPut(ctx, 0, blob)
		if err != nil {
			return err
		}
		accepted := false
		defer func() {
			if !accepted {
				d.unqueueProposal(ctx, g, groupID, ref)
			}
		}()
		existing, err := d.proposalRowAt(ctx, groupID, row.Epoch, ref)
		if err != nil {
			return err
		}
		// Only the voided instance proposal itself is re-armed: same kind, same target. A live row
		// is never touched here (every issuer dedupes against live rows first), and a row for
		// another target with this ref cannot be the same action; both fall through to an INSERT,
		// which refuses them, and the guest keeps what it held.
		rearm := existing != nil && existing.VoidAt != nil && existing.Origin == 0 &&
			existing.Kind == p.Kind && sameTarget(*existing, p)
		if err := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
			if err := tx.LockGroup(ctx, groupID); err != nil {
				return err
			}
			lockedGroup, err := tx.GetGroup(ctx, groupID)
			if err != nil {
				return err
			}
			if lockedGroup.ClosedAt != nil || lockedGroup.EpochUnknown || lockedGroup.Epoch != row.Epoch {
				return errCommitInvalid("epoch", "the group changed while this proposal was being prepared")
			}
			seq, err = nextSeq(ctx, tx, groupID)
			if err != nil {
				return err
			}
			if err := tx.AppendHandshake(ctx, store.HandshakeRow{
				GroupID: groupID, Seq: seq, Epoch: row.Epoch, Kind: handshakeProposal,
				SenderLeaf: nil, SenderDevice: nil, Blob: blob, Created: d.now(),
			}); err != nil {
				return err
			}
			p.GroupID = groupID
			p.Ref = ref
			p.Epoch = row.Epoch
			p.IssuedAt = d.now()
			p.TTL = uint64(d.proposalTTL(row.Kind).Seconds())
			// A re-issue replaces the row it supersedes in the same transaction, keeping
			// action_id; the same proposal again re-arms its own row (DS-1, above); anything else
			// is a fresh row.
			supersedes := d.takeSupersede(groupID)
			switch {
			case supersedes != nil && rearm && !bytes.Equal(supersedes, ref):
				// A re-issue onto an epoch that already holds this very proposal under a row of its
				// own: that row goes, and the re-issue carries the superseded action.
				if err := tx.DeleteProposals(ctx, groupID, [][]byte{ref}); err != nil {
					return err
				}
				if err := tx.ReissueProposal(ctx, supersedes, p); err != nil {
					return err
				}
			case supersedes != nil:
				if err := tx.ReissueProposal(ctx, supersedes, p); err != nil {
					return err
				}
			case rearm:
				if err := tx.ReissueProposal(ctx, ref, p); err != nil {
					return err
				}
			default:
				if err := tx.PutProposal(ctx, p); err != nil {
					return err
				}
			}
			return persistState(ctx, tx, groupID, g, row.GroupInfoBlob)
		}); err != nil {
			return err
		}
		accepted = true
		return nil
	})
	if d.takeStaleAfterFailedMerge(groupID) {
		// Outside withGroup, so the handle lock is free: the guest could not be brought back in
		// line with SQL (unqueueProposal), and the next request re-imports the committed blob.
		if evErr := d.states.evict(ctx, groupID); evErr != nil {
			d.log().Error("evicting a group whose proposal queue outran its transaction failed",
				"group", groupID, "err", evErr)
		}
	}
	if err != nil {
		return err
	}
	if p.Kind == uint8(mlswasi.ProposalRemove) && p.TargetLeaf != nil {
		d.log().InfoContext(ctx, "proposed a Remove", "group_id", groupID.String(), "device_id", p.TargetDevice, "leaf", *p.TargetLeaf)
	}
	if d.opts.Gateway != nil {
		payload, perr := gateway.HandshakePayload(seq, row.Epoch, handshakeProposal, nil, blob)
		if perr == nil {
			d.opts.Gateway.DeliverGroup(groupID, gateway.Frame{
				Op: gateway.OpMLSHandshake, GroupID: &groupID, Payload: payload, Replay: true,
			})
		}
	}
	// plan-1a task 7 declares `DSProposals *prometheus.GaugeVec` for
	// `dilla_ds_proposals_outstanding{kind}`. It is a GAUGE: every path that voids or deletes a live
	// instance proposal decrements it, and Sweep re-seeds it from SQL (proposal_gauge.go).
	d.gaugeInc(p.Kind)
	// A fresh instance proposal is what invariant 7's election exists to get committed, so the
	// election is held here, the moment the proposal is durable — UNLESS a batch window is open.
	// This function is the sink of every issuing path, single and batched alike (ProposeAdd,
	// ProposeRemove, reissue, and each device of a join storm's slice in drainPendingJoins), and
	// invariant 7 elects ONE committer per round: a batch therefore suppresses the per-proposal
	// election and makes one call of its own when the whole batch is durable.
	//
	// The error is LOGGED, not returned: the proposal is already written, fanned out and counted,
	// and answering the caller an error now would say the proposal failed when it did not. A
	// failed election is not lost either — the watchdog re-elects on its own tick, and so does the
	// next proposal.
	if row.Kind == groupKindCall {
		d.markCallWork(groupID) // the call sweeper voids and re-drives it (sweepCallProposals)
	}
	if !d.electionsSuppressed(groupID) {
		if err := d.RequestCommit(ctx, groupID); err != nil {
			d.log().Error("electing a committer for a fresh instance proposal failed",
				"group", groupID.String()[:8], "err", err)
		}
	}
	return nil
}

// Void marks one proposal void. A commit MAY omit a void proposal.
func (d *DS) Void(ctx context.Context, groupID id.ID, ref []byte) error {
	// Read before the void: only a row that was a live instance proposal was counted.
	kind, counted := d.liveInstanceKind(ctx, groupID, ref)
	if err := d.opts.Store.VoidProposal(ctx, groupID, ref, d.now()); err != nil {
		return err
	}
	if counted {
		d.gaugeDec(kind)
	}
	return nil
}

// VoidIneligibleAdds voids every outstanding instance Add at the group's current epoch whose
// device is gone, revoked or quarantined, or whose user the channel ACL no longer admits.
//
// Such an Add is a deadlock, not a delay: clause 1 of invariant 4 refuses every member commit that
// leaves it out, and checkAddedMember refuses every member commit that includes it (the add_acl
// clause), so no commit can land and the group answers 425 until the Add's TTL voids it (24 h for
// a text group), longer when a re-issue restarts the clock. A kick, a ban, a leave, a role revoked
// mid-storm or a group-DM participant removed while their Add is outstanding all reach it. A void
// proposal MAY be omitted, so voiding it is what lets the next member commit through.
//
// The api layer calls it for every group a user is being removed from; commitLocked also runs it
// before invariant 4's clauses, so a change the api layer did not see is caught at the next commit.
func (d *DS) VoidIneligibleAdds(ctx context.Context, groupID id.ID) error {
	unlock := d.lock(groupID)
	defer unlock()
	_, err := d.voidIneligibleAddsLocked(ctx, groupID)
	return err
}

// voidIneligibleAddsLocked is VoidIneligibleAdds with the group lock already held. It answers how
// many Adds it voided. An eligibility question it cannot answer stops it with the error: an Add is
// voided only on a definite "no".
func (d *DS) voidIneligibleAddsLocked(ctx context.Context, groupID id.ID) (int, error) {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, errNotFound("group")
	}
	if err != nil {
		return 0, err
	}
	rows, err := d.opts.Store.ListProposals(ctx, groupID, row.Epoch, false)
	if err != nil {
		return 0, err
	}
	voided := 0
	for _, r := range rows {
		if r.Origin != 0 || r.VoidAt != nil || mlswasi.ProposalKind(r.Kind) != mlswasi.ProposalAdd || r.TargetDevice == nil {
			continue
		}
		ok, err := d.addStillEligible(ctx, groupID, *r.TargetDevice)
		if err != nil {
			return voided, err
		}
		if ok {
			continue
		}
		if err := d.opts.Store.VoidProposal(ctx, groupID, r.Ref, d.now()); err != nil {
			return voided, err
		}
		d.gaugeDec(r.Kind) // origin 0 and non-void: the loop's guard above
		d.log().Info("an outstanding Add was voided: its device or user is no longer eligible",
			"group", groupID.String()[:8], "device", r.TargetDevice.String()[:8])
		voided++
	}
	return voided, nil
}

// addStillEligible is the part of invariant 4's Add clause that can change after the Add was
// proposed: the device must still be live, still named in its user's newest signed device list,
// and its user still admitted by the channel ACL.
func (d *DS) addStillEligible(ctx context.Context, groupID, deviceID id.ID) (bool, error) {
	dev, err := d.opts.Store.GetDevice(ctx, deviceID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if dev.RevokedAt != nil || dev.QuarantinedAt != nil {
		return false, nil
	}
	// The device-list clause: the same test checkAddedMember applies to the commit that would
	// carry the Add (checkListedDevice acquires its own guest, and no caller holds one here). A
	// list that no longer names the device, or that does not verify, is a definite "no"; a list
	// the verifier cannot reach at all is "cannot answer" and stops the pass.
	if err := d.checkListedDevice(ctx, nil, deviceID); err != nil {
		var dsErr *Error
		if errors.As(err, &dsErr) && dsErr.Code == errInvalid("").Code {
			return false, nil
		}
		return false, err
	}
	return d.opts.ACL.Eligible(ctx, groupID, dev.UserID)
}

// liveInstanceKind reads one row's kind when it is a live instance proposal (origin 0, non-void),
// the only rows the gauge counts; Void reads it before voiding, so a second void of one row, or a
// void of a member proposal, never decrements.
//
// It reads the group's current epoch, the only epoch whose proposals can be committed. Old
// open-group rows are excluded from the seed too; closed-group voids count their own rows.
func (d *DS) liveInstanceKind(ctx context.Context, groupID id.ID, ref []byte) (uint8, bool) {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return 0, false
	}
	rows, err := d.opts.Store.ListProposals(ctx, groupID, row.Epoch, false)
	if err != nil {
		return 0, false
	}
	for _, r := range rows {
		if bytes.Equal(r.Ref, ref) {
			return r.Kind, r.Origin == 0
		}
	}
	return 0, false
}

// ReissueFor retries a logical action with a fresh KeyPackage, keeping action_id. It is the retry
// half of invariant 6: "The underlying action is retried with a fresh KeyPackage, or dropped if
// the target leaf is already gone."
func (d *DS) ReissueFor(ctx context.Context, groupID, actionID id.ID) error {
	unlock := d.lock(groupID)
	defer unlock()

	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("group")
	}
	if err != nil {
		return err
	}
	rows, err := d.opts.Store.ListProposals(ctx, groupID, row.Epoch, true)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.ActionID == actionID {
			return d.reissue(ctx, groupID, r)
		}
	}
	return errNotFound("action")
}

// reissue re-signs one proposal for the group's current epoch, keeping its action_id and dropping
// it when the action no longer applies.
//
// Two rules hold here and are load-bearing:
//
//  1. Every branch calls the LOCK-FREE form. Both callers already hold the group lock.
//  2. The superseded row is RETIRED in the same breath as the new one is written. Leaving it
//     behind at the old epoch, non-void, makes `checkAppliedProposals`' clause 1 demand a ref no
//     commit can reference — the group freezes permanently. `interfaces.md` §4.1 declares
//     `ReissueProposal(ctx, oldRef []byte, p ProposalRow) error // keeps action_id` for exactly
//     this, so the new proposal is written through it: the old row is replaced atomically and
//     `action_id` is carried.
//
// `reissueVia` is the seam: it marks the next `storeInstanceProposal` as a replacement, and that
// function calls `tx.ReissueProposal(ctx, supersedes, p)` instead of `tx.PutProposal(ctx, p)`.
func (d *DS) reissue(ctx context.Context, groupID id.ID, old store.ProposalRow) error {
	switch mlswasi.ProposalKind(old.Kind) {
	case mlswasi.ProposalAdd:
		if old.TargetDevice == nil {
			return nil
		}
		// An Add whose device or user is no longer eligible is dropped, never re-proposed: a
		// fresh copy would be exactly the Add no commit can satisfy (VoidIneligibleAdds), with a
		// fresh TTL.
		ok, err := d.addStillEligible(ctx, groupID, *old.TargetDevice)
		if err != nil {
			return err
		}
		if !ok {
			return d.opts.Store.DeleteProposals(ctx, groupID, [][]byte{old.Ref})
		}
		return d.reissueVia(groupID, old.Ref, func() error {
			return d.proposeAddLocked(ctx, groupID, *old.TargetDevice, old.ActionID)
		})
	case mlswasi.ProposalRemove:
		if old.TargetLeaf == nil {
			return nil
		}
		still, err := d.removeStillTargets(ctx, groupID, old)
		if err != nil {
			return err
		}
		if !still {
			// The device it was for no longer holds the leaf: the action is satisfied, not retried,
			// and never re-aimed at whoever holds the index now.
			return d.opts.Store.DeleteProposals(ctx, groupID, [][]byte{old.Ref})
		}
		return d.reissueVia(groupID, old.Ref, func() error {
			return d.proposeRemoveOfLocked(ctx, groupID, *old.TargetLeaf, old.TargetDevice, old.ActionID)
		})
	default:
		return nil
	}
}

// reissueVia marks the next storeInstanceProposal FOR THIS GROUP as a replacement of oldRef, so it
// writes through ReissueProposal rather than PutProposal. The window runs under that group's own
// lock, and the marker is keyed by group, so a re-issue in another group cannot pick it up.
func (d *DS) reissueVia(groupID id.ID, oldRef []byte, fn func() error) error {
	d.supersedeMu.Lock()
	if d.supersede == nil {
		d.supersede = map[id.ID][]byte{}
	}
	d.supersede[groupID] = oldRef
	d.supersedeMu.Unlock()
	defer func() {
		d.supersedeMu.Lock()
		delete(d.supersede, groupID)
		d.supersedeMu.Unlock()
	}()
	return fn()
}

// takeSupersede reads and clears the group's marker, so a re-issue that fans out into more than
// one storeInstanceProposal supersedes exactly the first.
func (d *DS) takeSupersede(groupID id.ID) []byte {
	d.supersedeMu.Lock()
	defer d.supersedeMu.Unlock()
	ref, ok := d.supersede[groupID]
	if !ok {
		return nil
	}
	delete(d.supersede, groupID)
	return ref
}

func proposalLabel(kind uint8) string {
	switch mlswasi.ProposalKind(kind) {
	case mlswasi.ProposalAdd:
		return "add"
	case mlswasi.ProposalRemove:
		return "remove"
	case mlswasi.ProposalUpdate:
		return "update"
	default:
		return "other"
	}
}

// sweepProposals voids every proposal past its TTL and reports how many it voided; in a call group
// it then re-drives the voided Removes whose device still holds the leaf (F9). It runs on the
// one-minute Sweep, and sweepCallProposals runs the call-group half every
// Policy.ProposalSweepInterval. The TTL is evaluated here and nowhere else: every reader consults
// the stored VoidAt.
//
// It PAGES to completion rather than reading one fixed batch from the zero id. `ListOpenGroups`
// takes an `after` cursor; a constant `id.ID{}` start with a fixed limit means only the first N
// groups are ever swept, so on an instance with more groups than the batch the tail's proposals
// never void — and nothing else voids them.
func (d *DS) sweepProposals(ctx context.Context) (int, error) {
	voided := 0
	after := id.ID{}
	for {
		groups, err := d.opts.Store.ListOpenGroups(ctx, after, sweepPage)
		if err != nil {
			return voided, err
		}
		if len(groups) == 0 {
			return voided, nil
		}
		for _, g := range groups {
			after = g.GroupID
			if g.Kind == groupKindCall {
				n, more, err := d.sweepCallGroup(ctx, g.GroupID)
				voided += n
				if err != nil {
					return voided, err
				}
				if more {
					d.markCallWork(g.GroupID) // a restart's call sweeper may not have found it yet
				}
				continue
			}
			n, err := d.voidExpired(ctx, g.GroupID, g.Epoch)
			voided += n
			if err != nil {
				return voided, err
			}
		}
		if len(groups) < sweepPage {
			return voided, nil
		}
	}
}

// callSweepBudget bounds how many call groups one call-sweeper tick visits. The groups past it are
// visited on the next tick, round robin (nextCallWork).
const callSweepBudget = 256

// sweepCallProposals is sweepProposals over the call groups with instance work (DEV-49). Its cost
// per tick is the call groups that have instance proposals outstanding, at most callSweepBudget,
// not the instance's open groups: those are walked once, on the first tick after a start, to find
// the work a previous process left behind. Every instance proposal issued into a call group marks
// its group (storeInstanceProposal), and a group leaves the set once it has none left.
//
// A group whose sweep fails is logged and kept in the set, and the tick goes on to the rest of its
// slice: nextCallWork has already moved the cursor past the whole slice, so stopping at the failure
// would leave every group behind it for a full rotation (m4 of the task-9 review).
func (d *DS) sweepCallProposals(ctx context.Context) (int, error) {
	if err := d.seedCallWork(ctx); err != nil {
		return 0, err
	}
	voided := 0
	for _, groupID := range d.nextCallWork(callSweepBudget) {
		n, more, err := d.sweepCallGroup(ctx, groupID)
		voided += n
		if err != nil {
			d.log().Error("sweeping a call group's proposals failed; the next tick retries it",
				"group", groupID.String()[:8], "err", err)
			continue
		}
		if !more {
			d.unmarkCallWork(groupID)
		}
	}
	return voided, nil
}

// sweepCallGroup voids one call group's expired proposals and re-drives its voided Removes, under the
// group lock — the lock order of every other writer: the group lock, then the guest's handle lock
// inside storeInstanceProposal. The one-minute Sweep and the call sweeper can meet here, and the
// re-drive must read the voids it re-drives. The epoch is read under the lock: a commit that landed
// meanwhile has already re-driven what it left behind (commitLocked step 9), and re-driving an older
// epoch's voids again would stack a second Remove of one leaf at the new epoch. more reports whether
// the group still has a non-void instance proposal, so the call sweeper keeps visiting it.
func (d *DS) sweepCallGroup(ctx context.Context, groupID id.ID) (voided int, more bool, err error) {
	unlock := d.lock(groupID)
	defer unlock()
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, true, err
	}
	if row.ClosedAt != nil || row.Kind != groupKindCall {
		return 0, false, nil
	}
	voided, err = d.voidExpired(ctx, groupID, row.Epoch)
	if err != nil {
		return voided, true, err
	}
	if rerr := d.redriveCallRemovesLocked(ctx, groupID, row.Epoch, row.Epoch); rerr != nil {
		d.log().Error("re-driving a voided call Remove failed; the next sweep retries it",
			"group", groupID.String()[:8], "err", rerr)
		return voided, true, nil
	}
	rows, err := d.opts.Store.ListProposals(ctx, groupID, row.Epoch, false)
	if err != nil {
		return voided, true, err
	}
	for _, r := range rows {
		if r.Origin == 0 && r.VoidAt == nil {
			return voided, true, nil
		}
	}
	return voided, false, nil
}

// markCallWork puts a call group in the call sweeper's set.
func (d *DS) markCallWork(groupID id.ID) {
	d.callWorkMu.Lock()
	defer d.callWorkMu.Unlock()
	if d.callWork == nil {
		d.callWork = map[id.ID]struct{}{}
	}
	d.callWork[groupID] = struct{}{}
}

func (d *DS) unmarkCallWork(groupID id.ID) {
	d.callWorkMu.Lock()
	defer d.callWorkMu.Unlock()
	delete(d.callWork, groupID)
}

// seedCallWork walks the open groups once per process and marks every open call group; the first
// tick then visits each and keeps those with instance work.
func (d *DS) seedCallWork(ctx context.Context) error {
	d.callWorkMu.Lock()
	seeded := d.callWorkSeeded
	d.callWorkMu.Unlock()
	if seeded {
		return nil
	}
	after := id.ID{}
	for {
		groups, err := d.opts.Store.ListOpenGroups(ctx, after, sweepPage)
		if err != nil {
			return err
		}
		for _, g := range groups {
			after = g.GroupID
			if g.Kind == groupKindCall {
				d.markCallWork(g.GroupID)
			}
		}
		if len(groups) < sweepPage {
			break
		}
	}
	d.callWorkMu.Lock()
	d.callWorkSeeded = true
	d.callWorkMu.Unlock()
	return nil
}

// nextCallWork is up to limit groups of the set in id order, starting after the last group the
// previous tick reached, so a set larger than limit is visited in turn.
func (d *DS) nextCallWork(limit int) []id.ID {
	d.callWorkMu.Lock()
	defer d.callWorkMu.Unlock()
	all := make([]id.ID, 0, len(d.callWork))
	for g := range d.callWork {
		all = append(all, g)
	}
	slices.SortFunc(all, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	if len(all) <= limit {
		return all
	}
	start, found := slices.BinarySearchFunc(all, d.callWorkAfter, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	if found {
		start++
	}
	out := append(slices.Clone(all[start:]), all[:start]...)[:limit]
	d.callWorkAfter = out[len(out)-1]
	return out
}

// voidExpired voids the group's proposals at epoch whose TTL has passed.
func (d *DS) voidExpired(ctx context.Context, groupID id.ID, epoch uint64) (int, error) {
	now := d.now()
	rows, err := d.opts.Store.ListProposals(ctx, groupID, epoch, false)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if r.VoidAt != nil || now < r.IssuedAt+int64(r.TTL) { //nolint:gosec // G115: a proposal TTL in seconds, at most a few days
			continue
		}
		if err := d.opts.Store.VoidProposal(ctx, groupID, r.Ref, now); err != nil {
			return n, err
		}
		if r.Origin == 0 { // the gauge counts instance proposals only
			d.gaugeDec(r.Kind)
		}
		n++
	}
	return n, nil
}

// voidClosedGroupProposals voids every live proposal, of any epoch, of a closed group. No commit
// lands in a closed group and none is reopened, so such a proposal is owed to nobody — but the TTL
// sweeps (sweepProposals, sweepCallGroup) read open groups only, so without this pass it stays
// non-void forever and keeps dilla_ds_proposals raised. The common case is a call group: the last
// device's leave issues the instance Remove of its leaf, and the call's end closes the group in
// the same second. A void, not a delete, as everywhere else the TTL ends a proposal. Each void
// leaves the result set, so the loop re-reads from the start until a short page; the first sweep
// after an upgrade voids the backlog the same way.
func (d *DS) voidClosedGroupProposals(ctx context.Context) (int, error) {
	n := 0
	for {
		rows, err := d.opts.Store.ListClosedGroupLiveProposals(ctx, sweepPage)
		if err != nil {
			return n, err
		}
		for _, r := range rows {
			if err := d.opts.Store.VoidProposal(ctx, r.GroupID, r.Ref, d.now()); err != nil {
				return n, err
			}
			if r.Origin == 0 { // the gauge counts instance proposals only
				d.gaugeDec(r.Kind)
			}
			n++
		}
		if len(rows) < sweepPage {
			return n, nil
		}
	}
}

package ds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/bot/access"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// Handshake kinds, as protocol/02's "Handshake records" fixes them.
const (
	handshakeProposal       uint8 = 0
	handshakeCommit         uint8 = 1
	handshakeExternalCommit uint8 = 2
)

// WelcomeFor is one addressed Welcome a commit carries: the joining device and the blob only that
// device can open.
type WelcomeFor struct {
	DeviceID id.ID
	Blob     []byte
}

// CommitRequest is POST /v1/groups/{id}/commit. RatchetTree exists on the shape only because
// POST /heal shares it (interfaces §5.1 row 5); on this endpoint it must be empty, and the
// refusal is invariant 2's.
type CommitRequest struct {
	Epoch       uint64
	Commit      []byte
	GroupInfo   []byte
	Welcomes    []WelcomeFor
	RatchetTree []byte
}

type CommitResult struct {
	Seq   uint64
	Epoch uint64
}

// Commit is invariants 3 and 4, in the order the invariants are written. Each numbered step is a
// named test in commit_test.go; the order IS the invariant, and reordering it changes what the
// delivery service accepts.
func (d *DS) Commit(ctx context.Context, s Session, groupID id.ID, c CommitRequest) (CommitResult, error) {
	return d.commit(ctx, s, groupID, c, commitOptions{})
}

// commitOptions are the knobs the later external-commit paths turn. Task 20 sets none of them:
// `Commit` is the member path, and every field below is false or zero for it. They are declared
// here, with the one caller they have, because the branches they guard are written into `commit`
// now and the tasks that own those branches — 21 (the freeze), 24 (Welcomes) and 25 (resync and
// external join) — turn them on without reshaping the function.
type commitOptions struct {
	external         bool  // an external commit: resync and join
	joining          bool  // an external commit by a device that holds no leaf: a join (B36)
	skipFreeze       bool  // R25: resync is exempt, under its own two guards
	allowRatchetTree bool  // invariant 11's heal endpoint, the one route that DOES take a tree
	handshakeKind    uint8 // 0 means "derive it from `external`"
}

// CheckCommit is Commit's first two steps — the enrolled scope, a current leaf in the group, and
// invariant 3's epoch — answered from the path, the session and the epoch alone, with the same
// refusals Commit gives. POST /commit runs it BEFORE it reads a body the Welcome headroom lets grow
// to 16 MiB (deviation B37), so a stranger or a committer a round behind is refused for the price
// of a header. Commit runs both steps again under the group lock; this is an early answer, not
// the check.
func (d *DS) CheckCommit(ctx context.Context, s Session, groupID id.ID, epoch uint64) error {
	_, err := d.commitPreflight(ctx, s, groupID, epoch, false)
	return err
}

// commitPreflight is steps (1) and (2) of the commit path.
func (d *DS) commitPreflight(ctx context.Context, s Session, groupID id.ID, epoch uint64, external bool) (store.GroupRow, error) {
	// (1) scope and membership.
	// Named, not the literal 0: a future reordering of auth.Scope's constants would silently
	// change what this line means without touching it.
	if s.Scope != auth.ScopeEnrolled {
		return store.GroupRow{}, errForbidden("a commit needs an enrolled session")
	}
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return store.GroupRow{}, errNotFound("group")
	}
	if err != nil {
		return store.GroupRow{}, err
	}
	if !external {
		if _, err := d.leafOf(ctx, groupID, s.DeviceID); err != nil {
			return store.GroupRow{}, err
		}
		// A stale-epoch refusal carries the winning commit and outstanding proposal refs. Those
		// recovery bytes are part of the scoped group capability, so authorize the negotiated wire
		// version before reading or returning any conflict metadata.
		if err := d.requireGroupCapability(ctx, s, row); err != nil {
			return store.GroupRow{}, err
		}
	}

	// (2) one commit per epoch (invariant 3). protocol/02 defines the conflict as "a later one
	// for the SAME epoch", so a client AHEAD of the instance — after a restore, say — is a
	// structural error, not a conflict: winningCommit has nothing to return for a future epoch,
	// and a conflict body with a null winner is one the client cannot act on.
	if epoch > row.Epoch {
		return store.GroupRow{}, errCommitInvalid("epoch_ahead",
			fmt.Sprintf("the commit is for epoch %d; this instance is at %d", epoch, row.Epoch))
	}
	if epoch != row.Epoch {
		outstanding, err := d.refsOf(ctx, groupID, row.Epoch)
		if err != nil {
			return store.GroupRow{}, err
		}
		winner, err := d.winningCommit(ctx, groupID, epoch)
		if err != nil {
			return store.GroupRow{}, err
		}
		return store.GroupRow{}, errCommitConflict(winner, outstanding)
	}
	return row, nil
}

func (d *DS) commit(ctx context.Context, s Session, groupID id.ID, c CommitRequest, o commitOptions) (CommitResult, error) {
	defer d.flushEvictions(ctx, groupID) // after the unlock below: defers run last-in, first-out
	unlock := d.lock(groupID)
	defer unlock()
	return d.commitLocked(ctx, s, groupID, c, o)
}

// commitLocked is the commit path with the group lock ALREADY HELD.
//
// The split exists for `Resync` (R25), which has to read the group's epoch and run its own guard
// against the same epoch the commit will compare `c.Epoch` with. `d.lock` is a plain sync.Mutex
// and is not reentrant, so a resync that read the epoch through `commit` would either take the
// lock twice — deadlocking that group's request goroutine for the life of the process — or read
// the epoch outside it, where a commit landing in between turns a valid resync into a spurious
// E_COMMIT_CONFLICT. The device that is already out of the epoch is exactly the one that cannot
// recover from that.
func (d *DS) commitLocked(ctx context.Context, s Session, groupID id.ID, c CommitRequest, o commitOptions) (CommitResult, error) {
	// (1) scope and membership, (2) one commit per epoch: commitPreflight.
	row, err := d.commitPreflight(ctx, s, groupID, c.Epoch, o.external)
	if err != nil {
		return CommitResult{}, err
	}

	// (1a) an outstanding instance Add whose device or user has become ineligible is voided before
	// clause 1 is applied, so a commit that leaves it out can land (VoidIneligibleAdds says why no
	// commit could otherwise). A question the ACL cannot answer voids nothing and is logged: the
	// clauses below then decide exactly as they would have.
	if _, verr := d.voidIneligibleAddsLocked(ctx, groupID); verr != nil {
		d.log().Warn("checking the outstanding Adds' eligibility before a commit failed",
			"group", groupID.String()[:8], "err", verr)
	}

	// (2a) invariant 2: the delivery service serves the ratchet tree from its own PublicGroup, so
	// a committer never uploads one. Accepting-and-ignoring the field would leave the one route a
	// client could smuggle a tree through unguarded and untested.
	if !o.allowRatchetTree && len(c.RatchetTree) != 0 {
		return CommitResult{}, errInvalid(
			"ratchet_tree must be null on POST /commit: the delivery service serves the tree from its own PublicGroup")
	}

	// (3) the freeze (invariant 5) belongs HERE, between the epoch check and the parse: an
	// external commit is refused while a non-void proposal is outstanding and some member device
	// is online, and a resync skips it under R25's two guards. `resyncLocked`, which also carries an
	// external join, is the path that sets `o.external`.
	//
	// A MEMBER commit is never refused here: it is the thing that lifts the freeze.
	if o.external && !o.skipFreeze {
		frozen, refs, ferr := d.freezeState(ctx, groupID, row.Epoch)
		if ferr != nil {
			return CommitResult{}, ferr
		}
		if frozen {
			return CommitResult{}, errCommitRequired(refs,
				uint64(d.opts.Policy.CommitDeadline.Milliseconds())) //nolint:gosec // G115: a non-negative duration in milliseconds
		}
	}

	var result CommitResult
	// The applied list and the epoch the commit came FROM are carried out of the closure: the
	// re-issue of whatever an external commit omitted (invariant 5's nobody-online exception) and
	// the next slice of a join storm both call back into the proposal path, which takes withGroup
	// again — and withGroup's handle lock is a plain sync.Mutex. Running either inside the closure
	// would deadlock this group's request goroutine for the life of the process.
	var applied []mlswasi.AppliedProposal
	oldEpoch := row.Epoch
	err = d.withGroup(ctx, groupID, func(g *mlswasi.PublicGroup) error {
		// (4) structural validation.
		processed, err := g.Process(ctx, c.Commit)
		if err != nil {
			// On the external path the core's one dilla-level commit rule that answers E_CREDENTIAL
			// is the joiner's: a resync keeps its device's credential, removes the device's old leaf,
			// and a device holding two leaves cannot resync (`leaf_credentials_unchanged`'s
			// NewMemberCommit arm; an external commit carries no Update). It is the external joiner's
			// clause, so it answers under that rule's name, as checkExternalJoiner and
			// checkExternalCommitReplacesOwnLeaf do.
			var abiErr *mlswasi.ABIError
			if o.external && errors.As(err, &abiErr) && abiErr.Code == "E_CREDENTIAL" {
				return errCommitInvalid("external_joiner", err.Error())
			}
			return errCommitInvalid("structural", err.Error())
		}
		// Every refusal from HERE on — the structural checks below included — must release the
		// staged commit: public_group_process inserts one for every StagedCommitMessage it
		// accepts, public_group_merge is the only other consumer, and merging is exactly what a
		// refusal must not do. Registering this after the structural checks would leak one handle
		// per refused external commit, which any enrolled device drives in a loop until the guest
		// runs out of linear memory.
		merged := false
		defer func() {
			if !merged && processed.Staged != nil {
				if derr := g.Discard(ctx, *processed.Staged); derr != nil {
					d.log().Warn("discarding a refused staged commit failed",
						"group", groupID, "err", derr)
				}
			}
		}()

		// A commit is KindCommit on BOTH paths. KindExternalJoin (2) is the external-join
		// PROPOSAL arm — exports.rs returns `(2, None, None, Some(proposal_ref))` for it, with no
		// staged commit at all — while a genuine external commit is a StagedCommitMessage and so
		// arrives as KindCommit (1). What tells the two apart is the SENDER, not the kind:
		// `Sender::NewMemberCommit` maps to no sender leaf (interfaces.md:774), and that is the
		// sole marker the R25 scope guard is keyed on.
		if processed.Kind != mlswasi.KindCommit {
			return errCommitInvalid("structural", "the uploaded message is not a commit")
		}
		if processed.Staged == nil {
			return errCommitInvalid("structural", "the commit did not stage")
		}
		if o.external {
			if processed.SenderLeaf != nil {
				return errCommitInvalid("structural", "an external commit must not name a leaf")
			}
		} else if processed.SenderLeaf == nil {
			return errCommitInvalid("structural", "a member commit must name its leaf")
		} else if leaf, lerr := d.leafOf(ctx, groupID, s.DeviceID); lerr != nil || leaf != *processed.SenderLeaf {
			// The committer is the leaf the PublicGroup authenticated, and the session uploading
			// it must be that leaf's device, as Proposal requires of a member proposal: the
			// clauses below scope a Remove to its proposer's user, the log records the uploader
			// as the sender, and neither may be another member relaying someone else's commit.
			return errForbidden("a member commit must be signed by the uploading device's own leaf")
		}

		// (5) invariant 4's clauses over the applied list. satisfied is every outstanding instance
		// Remove the commit did not reference but removes the device of all the same (clause 1).
		satisfied, err := d.checkAppliedProposals(ctx, g, groupID, row, s, processed, o)
		if err != nil {
			return err
		}

		// (5a) every addressed Welcome names a device THIS commit adds.
		if err := checkAddressedWelcomes(processed.Applied, c.Welcomes); err != nil {
			return err
		}

		// (6) the GroupInfo: epoch n+1, signed by the committer.
		//
		// The signer is the committer's own leaf, and leaf 0 is not a safe default for a commit
		// that names none: on the external path it would check the joiner's GroupInfo against the
		// CREATOR's signature key, which is a different question from the one invariant 4 asks.
		//
		// On the external path the joiner occupies no leaf of the current tree, so there is no
		// tree key to check against: ABI v3 reports the leaf the joiner lands on (`NewLeaf`) and
		// checks the GroupInfo under the key the staged commit's UpdatePath brings for it
		// (deviation B33, closing B30(a)). Both happen BEFORE the merge, which is irreversible. A
		// commit that reports no new leaf still fails closed rather than borrowing leaf 0's key.
		var check mlswasi.GroupInfoCheck
		if o.external {
			if processed.NewLeaf == nil {
				return errCommitInvalid("group_info_signature",
					"the joiner's leaf is unknown, so the GroupInfo's signer cannot be checked")
			}
			check, err = g.ValidateStagedGroupInfo(ctx, *processed.Staged, c.GroupInfo)
		} else {
			if processed.SenderLeaf == nil {
				return errCommitInvalid("group_info_signature",
					"the committer's leaf is unknown, so the GroupInfo's signer cannot be checked")
			}
			check, err = g.ValidateGroupInfo(ctx, c.GroupInfo, *processed.SenderLeaf)
		}
		if err != nil {
			return errCommitInvalid("group_info", err.Error())
		}
		if check.Epoch != row.Epoch+1 {
			return errCommitInvalid("group_info_epoch",
				fmt.Sprintf("GroupInfo is at epoch %d, want %d", check.Epoch, row.Epoch+1))
		}
		if !check.SignatureOK {
			return errCommitInvalid("group_info_signature",
				"the GroupInfo is not signed by the committer's leaf")
		}

		// (7) one transaction: merge, persist, append, replace members, drop proposals.
		//
		// Merge is irreversible: it consumes the staged handle and advances the guest's in-memory
		// group. If any later statement fails, SQL rolls back to epoch n while the cached handle
		// sits at n+1, and R12's "SQL is the record" is violated for the rest of the process's
		// life — every later commit for that group then validates against the wrong epoch. So a
		// failure after Merge EVICTS the group from the state cache, and the next request
		// re-imports the committed blob.
		var members memberView
		var handshakeKind uint8
		// The welcoming epoch's tree, carried out of the transaction to the fan-out below.
		var welcomeTree, welcomeTreeHash []byte
		mergeRan := false
		txErr := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
			if err := tx.LockGroup(ctx, groupID); err != nil {
				return err
			}
			lockedGroup, err := tx.GetGroup(ctx, groupID)
			if err != nil {
				return err
			}
			if lockedGroup.ClosedAt != nil || lockedGroup.Epoch != row.Epoch {
				return errCommitInvalid("epoch", "the group changed while this commit was being prepared")
			}
			epoch, err := g.Merge(ctx, *processed.Staged)
			if err != nil {
				return err
			}
			mergeRan = true
			merged = true
			seq, err := nextSeq(ctx, tx, groupID)
			if err != nil {
				return err
			}
			kind := o.handshakeKind
			if kind == 0 {
				kind = handshakeCommit
				if o.external {
					kind = handshakeExternalCommit
				}
			}
			handshakeKind = kind
			senderDevice := s.DeviceID
			if err := tx.AppendHandshake(ctx, store.HandshakeRow{
				GroupID:      groupID,
				Seq:          seq,
				Epoch:        epoch,
				Kind:         kind,
				SenderLeaf:   processed.SenderLeaf,
				SenderDevice: &senderDevice,
				Blob:         c.Commit,
				Created:      d.now(),
			}); err != nil {
				return err
			}
			if err := persistState(ctx, tx, groupID, g, c.GroupInfo); err != nil {
				return err
			}
			state, err := g.State(ctx)
			if err != nil {
				return err
			}
			// (6, continued) the GroupInfo describes the group this commit merged to: its group id
			// and its tree hash, as heal checks them. The epoch and the committer's signature above
			// do not tie it to the tree - a committer can sign the GroupInfo of another commit of the
			// same epoch, or of another group - and the stored GroupInfo is what every device that
			// resyncs or joins builds its external commit from until the next commit. The tree hash
			// is the merged group's own, so the check runs here, after the merge; a refusal rolls the
			// transaction back and the stale handle is evicted below, as for any failure after it.
			if !bytes.Equal(check.GroupID, groupID[:]) {
				return errCommitInvalid("group_info", "the GroupInfo names another group")
			}
			if !bytes.Equal(check.TreeHash, state.TreeHash) {
				return errCommitInvalid("group_info",
					"the GroupInfo's tree_hash is not the tree this commit merges to")
			}
			// The joiner's own leaf is checked against the session that uploaded it, on the
			// merged state because that is the first place the leaf exists. A refusal here rolls
			// the transaction back and the stale handle is evicted below, as for any failure
			// after the merge.
			if o.external {
				if err := d.checkExternalJoiner(ctx, g, groupID, s, state, processed.NewLeaf, o.joining); err != nil {
					return err
				}
			}
			// An external commit's joiner starts a new membership (added_epoch = this epoch) even
			// when it lands at the index it held before an own-leaf resync (R-3 of the DS re-review).
			var joined []uint32
			if o.external && processed.NewLeaf != nil {
				joined = append(joined, *processed.NewLeaf)
			}
			members, err = d.replaceMembersTx(ctx, tx, groupID, row.Kind, state, joined...)
			if err != nil {
				return err
			}
			// The applied refs, and every instance Remove a member's Remove of the same leaf
			// satisfied (clause 1): that row is done with exactly as if it had been referenced, and
			// left non-void at the old epoch it would be read by a later re-issue (reissueAll,
			// redriveCallRemovesLocked) as work still owed.
			refs := make([][]byte, 0, len(processed.Applied)+len(satisfied))
			for _, a := range processed.Applied {
				refs = append(refs, a.ProposalRef)
			}
			refs = append(refs, satisfied...)
			if err := tx.DeleteProposals(ctx, groupID, refs); err != nil {
				return err
			}
			// The addressed Welcomes are stored HERE, in this same transaction: a Welcome row
			// that outlives a rolled-back commit addresses an epoch that never happened. `g` has
			// already merged, so the epoch tree it writes beside them is the welcoming epoch's —
			// and it hands that tree back for the fan-out, which needs the same two values for
			// every joiner and must not read one copy of them back per joiner.
			welcomeTree, welcomeTreeHash, err = d.storeWelcomesTx(ctx, tx, groupID, epoch, seq, g, c.Welcomes)
			if err != nil {
				return err
			}
			result = CommitResult{Seq: seq, Epoch: epoch}
			return nil
		})
		if txErr != nil {
			if mergeRan {
				// withGroup holds the handle's own lock, so evict cannot be called from here —
				// it would wait on that same lock. The eviction is recorded and performed by the
				// caller, below, once withGroup has returned.
				d.markStaleAfterFailedMerge(groupID)
			}
			return txErr
		}
		// The devices this commit took out of a call group leave the call's room once the group lock
		// is released (commit's deferred flushEvictions; G29). Only here, after the transaction has
		// committed: TestARefusedCommitEvictsNobody fails a transaction after its last statement.
		d.queueEviction(groupID, members.removed)

		// (8) fan out. Everything reachable from here runs with the group lock ALREADY HELD by
		// commit, so it must be lock-free: the re-issue below and drainPendingJoins call only the
		// …Locked forms of the proposal API.
		applied = processed.Applied
		if d.opts.Gateway != nil {
			d.opts.Gateway.SetGroupMembers(groupID, members.devices)
			d.opts.Gateway.SetGroupLeaves(groupID, members.leaves)
		}
		d.fanOutCommit(ctx, groupID, result, processed, c, handshakeKind, welcomeTree, welcomeTreeHash)
		return nil
	})
	if d.takeStaleAfterFailedMerge(groupID) {
		// Outside withGroup, so the handle lock is free.
		if evErr := d.states.evict(ctx, groupID); evErr != nil {
			d.log().Error("evicting a group whose merge outran its transaction failed",
				"group", groupID, "err", evErr)
		}
	}
	if err != nil {
		if d.opts.Metrics != nil {
			d.opts.Metrics.DSCommits.WithLabelValues("refused").Inc()
		}
		return CommitResult{}, err
	}
	d.accepted.Add(1)
	if d.opts.Metrics != nil {
		// plan-1a task 7 declares `DSCommits *prometheus.CounterVec` for
		// `dilla_ds_commits_total{result}`. There is no `CommitsTotal`.
		d.opts.Metrics.DSCommits.WithLabelValues("accepted").Inc()
	}

	// (8b) the round is WON. Invariant 7's watchdog charges a lost round to the candidate of an
	// overdue election that was acknowledged, and three of those remove the device from the group
	// by an instance Remove — so the election must learn that this committer did what it was asked
	// to do. Nothing else tells it: `clearElection`'s other call sites are all inside
	// RequestCommit, and `e.acked` is cleared only by the next round.
	//
	// The refs this commit applied are deleted in the transaction above, so the fresh election
	// step (9) and the next proposal hold is over the NEW epoch's outstanding work, from round one
	// of a fresh rotation. RunWatchdogOnce carries the same rule for the window in which its tick
	// beats this line: an election whose epoch the group has already left is won, never lost.
	d.clearElection(groupID)

	// (9) invariant 5's tail, with the group lock still held and withGroup returned. Only an
	// EXTERNAL commit can omit an outstanding instance proposal — invariant 5's nobody-online
	// exception or R25's resync (clause1Exempt); a member commit never can, and an instance Remove
	// it satisfied by another Remove of the leaf was deleted with it and is owed nothing. So the
	// re-issue runs for external commits: every proposal the commit did not reference is re-signed
	// for the new epoch, keeping its action_id, and the freeze stays.
	if o.external {
		if rerr := d.reissueOmitted(ctx, groupID, oldEpoch, applied); rerr != nil {
			d.log().Error("re-issuing the proposals an external commit omitted failed",
				"group", groupID, "err", rerr)
		}
	}
	// And the next slice of a join storm, once this commit has landed. Without it a 1,000-device
	// batch stalls after its first 256. It runs after EVERY accepted commit, not only one that
	// applied Adds: a commit of Removes, or one after the storm's own Adds were voided, frees room
	// too, and an empty queue costs one read. The committer's request context is not the drain's —
	// the commit is durable, and a client that hangs up now must not cut the next slice short.
	if _, derr := d.drainPendingJoins(context.WithoutCancel(ctx), groupID); derr != nil {
		d.log().Error("proposing the next slice of a join storm failed; the sweeper retries it",
			"group", groupID, "err", derr)
	}
	// F9, a call group's epoch boundary: a voided instance Remove this commit left out — voided by its
	// TTL — whose device still holds the same leaf is re-driven at the new epoch. The sweep re-drives
	// within an epoch and never reads the old one.
	if row.Kind == groupKindCall {
		if rerr := d.redriveCallRemovesLocked(context.WithoutCancel(ctx), groupID, oldEpoch, result.Epoch); rerr != nil {
			d.log().Error("re-driving the call Removes a commit left out failed",
				"group", groupID, "err", rerr)
		}
	}
	return result, nil
}

// markStaleAfterFailedMerge and takeStaleAfterFailedMerge carry one bit from inside withGroup's
// handle lock to outside it, so the eviction happens where it cannot deadlock.
func (d *DS) markStaleAfterFailedMerge(groupID id.ID) {
	d.staleMu.Lock()
	if d.stale == nil {
		d.stale = map[id.ID]struct{}{}
	}
	d.stale[groupID] = struct{}{}
	d.staleMu.Unlock()
}

func (d *DS) takeStaleAfterFailedMerge(groupID id.ID) bool {
	d.staleMu.Lock()
	defer d.staleMu.Unlock()
	_, ok := d.stale[groupID]
	delete(d.stale, groupID)
	return ok
}

// checkAppliedProposals is invariant 4's clauses over the applied list. The clause numbers are
// protocol/02's own. It answers the refs of the outstanding instance Removes the commit satisfied
// without referencing them (clause 1), which the commit's transaction deletes with the applied ones.
func (d *DS) checkAppliedProposals(ctx context.Context, g DeviceListVerifier, groupID id.ID, row store.GroupRow, s Session, p mlswasi.Processed, o commitOptions) ([][]byte, error) {
	outstanding, err := d.opts.Store.ListProposals(ctx, groupID, row.Epoch, false)
	if err != nil {
		return nil, err
	}
	applied := map[string]struct{}{}
	removed := map[uint32]struct{}{}
	for _, a := range p.Applied {
		applied[string(a.ProposalRef)] = struct{}{}
		if a.Kind == mlswasi.ProposalRemove && a.TargetLeaf != nil {
			removed[*a.TargetLeaf] = struct{}{}
		}
	}
	var satisfied [][]byte
	// Clause 1: every outstanding non-void DS proposal is referenced — or, for a DS Remove, the
	// commit applies another Remove of its leaf (I1 of the task-9 review).
	//
	// THE REMOVE CASE. OpenMLS commits only the later of two Removes of one leaf in the committer's
	// queue order, so a committer that holds the DS's Remove of leaf L ahead of a member's own Remove
	// of L commits the member's. Refusing that commit would refuse every such committer until the
	// TTL — 24 h in a text group — and charge each a lost round, until three of them removed the
	// committer itself; the member being removed sets the trap just by posting its own Remove before
	// the kick lands, which it may. Within one epoch L holds exactly the device the DS's Remove
	// recorded, so any Remove of L removes that device: the outcome is the one the DS's Remove has.
	// The member's Remove has already passed clause 3 (below) against the proposer the PublicGroup
	// authenticated, or the whole commit is refused. A member proposal still never stands in for
	// the DS's without being committed: this counts only a Remove THIS commit applies.
	//
	// The recorded device must still be the one at L at this epoch; a row recording another device
	// (or none) is not satisfied this way, only by its own ref.
	//
	// THE EXEMPTION (deviation B21, ruling 42). An external commit cannot reference the instance's
	// outstanding proposals — it is built by a device that is not in the epoch — and there are
	// exactly two places protocol/02 accepts one anyway:
	//
	//  1. Invariant 5's nobody-online case. The freeze has LIFTED because no member device is
	//     online, the external commit is accepted, and the proposals it omitted are re-issued for
	//     the new epoch by `reissueOmitted` at step (9). Without this exemption step (9) is
	//     unreachable and `reissueOmitted` can never fire.
	//  2. R25's resync, which is exempt from the freeze under its own two guards and re-issues
	//     the outstanding proposals for the new epoch immediately afterwards (`reissueAll`).
	//
	// The freeze's own answer is what decides case 1 — never an unconditional external carve-out,
	// which would let an outsider commit straight through a live freeze — and `o.skipFreeze`,
	// which only `Resync` sets, is what decides case 2.
	exemptFromClause1, err := d.clause1Exempt(ctx, groupID, row.Epoch, o)
	if err != nil {
		return nil, err
	}
	for _, pending := range outstanding {
		if pending.Origin != 0 || pending.VoidAt != nil {
			continue
		}
		if exemptFromClause1 {
			continue
		}
		if _, ok := applied[string(pending.Ref)]; ok {
			continue
		}
		ok, err := d.removeSatisfiedBy(ctx, groupID, pending, removed)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errCommitInvalid("outstanding_proposals",
				fmt.Sprintf("the commit does not reference the outstanding proposal %x", pending.Ref))
		}
		satisfied = append(satisfied, pending.Ref)
	}
	for _, a := range p.Applied {
		switch a.Kind {
		case mlswasi.ProposalUpdate:
			// Clause 2: no Update from the committer. A committer who wants to rotate its own
			// leaf uses the commit's UpdatePath, which is not a proposal.
			if a.SenderLeaf != nil && p.SenderLeaf != nil && *a.SenderLeaf == *p.SenderLeaf {
				return nil, errCommitInvalid("committer_update", "the commit carries the committer's own Update")
			}
		case mlswasi.ProposalRemove:
			// Clause 3: every member-originated Remove targets its proposer's own user — the
			// committer's for a Remove the commit carries by value, the proposing member's for a
			// member Remove proposal the commit references. The second is how a member leaves
			// (protocol/01 "Leaving", DEV-47): a device cannot commit its own removal, so it
			// proposes it and another member commits it. Proposal already refused a member proposal
			// that removes anybody else.
			//
			// The proposer is the sender the PublicGroup authenticated for that proposal — OpenMLS
			// attributes a by-value proposal to the committer's own leaf and a referenced one to
			// the leaf that signed it — never a value the uploading client supplied.
			if a.SenderLeaf == nil || a.TargetLeaf == nil {
				continue // an instance Remove, which invariant 6 governs instead
			}
			target, err := d.userOfLeaf(ctx, groupID, *a.TargetLeaf)
			if err != nil {
				return nil, err
			}
			owner, err := d.userOfLeaf(ctx, groupID, *a.SenderLeaf)
			if err != nil {
				return nil, err
			}
			if target != owner {
				return nil, errCommitInvalid("member_remove_scope",
					"a member-originated Remove may only target its proposer's own user")
			}
		case mlswasi.ProposalAdd:
			// Clause 4: the added KeyPackage validates, its user is eligible and its DSK is in
			// the newest device list.
			if err := d.checkAddedMember(ctx, g, groupID, a); err != nil {
				return nil, err
			}
		}
	}
	if o.external {
		// R25: an external commit may remove nobody but the joiner's own prior leaf.
		if err := d.checkExternalCommitScope(ctx, groupID, s, p.Applied); err != nil {
			return nil, err
		}
		// …and it must remove that leaf, if the device holds one (one device, one leaf).
		return satisfied, d.checkExternalCommitReplacesOwnLeaf(ctx, groupID, s, p.Applied)
	}
	return satisfied, nil
}

// removeSatisfiedBy reports whether the outstanding instance proposal pending is a Remove whose leaf
// the commit removes anyway (removed holds the applied Removes' target leaves) while that leaf still
// holds the device pending recorded. Clause 1's Remove case; the comment there says why.
func (d *DS) removeSatisfiedBy(ctx context.Context, groupID id.ID, pending store.ProposalRow, removed map[uint32]struct{}) (bool, error) {
	if mlswasi.ProposalKind(pending.Kind) != mlswasi.ProposalRemove || pending.TargetLeaf == nil || pending.TargetDevice == nil {
		return false, nil
	}
	if _, ok := removed[*pending.TargetLeaf]; !ok {
		return false, nil
	}
	device, present, err := d.deviceAtLeaf(ctx, groupID, *pending.TargetLeaf)
	if err != nil {
		return false, err
	}
	return present && device == *pending.TargetDevice, nil
}

// clause1Exempt answers whether this commit may omit the instance's outstanding proposals. The
// two cases are written out at clause 1 itself; a member commit is never exempt, and it is the
// thing that lifts the freeze.
func (d *DS) clause1Exempt(ctx context.Context, groupID id.ID, epoch uint64, o commitOptions) (bool, error) {
	if !o.external {
		return false, nil
	}
	if o.skipFreeze {
		// R25's resync. `reissueAll` re-issues whatever it omitted, for the new epoch.
		return true, nil
	}
	frozen, _, err := d.freezeState(ctx, groupID, epoch)
	if err != nil {
		return false, err
	}
	// Invariant 5's nobody-online exception: exempt exactly when the freeze is NOT holding.
	return !frozen, nil
}

// checkAddressedWelcomes is the clause that keeps `CommitRequest.Welcomes` inside the commit that
// carries it: every addressed device must be one this commit's applied Add proposals name.
//
// Without it `storeWelcomesTx` writes whatever the committer addressed, so any enrolled member can
// queue an opaque blob to an arbitrary device id at that group's epoch. The recipient cannot open
// it — the blob is no Welcome for its key material — but it learns the group_id and the epoch of a
// group it was never added to, and its Welcome queue holds that row for the 30-day retention;
// `mls_welcomes` is unique only on (device_id, blob_sha256), so varying the blob defeats the
// dedupe and the queue grows one row per request.
//
// protocol/02 does not state the clause and task 24's brief did not ask for it; it is added here
// because the material is already in hand (invariant 4's Add clause decodes the same credentials)
// and because an unvalidated addressed Welcome left unstated in the code is the worst of the
// options. Recorded as deviation B26 in the plan.
func checkAddressedWelcomes(applied []mlswasi.AppliedProposal, welcomes []WelcomeFor) error {
	if len(welcomes) == 0 {
		return nil
	}
	added := make(map[id.ID]struct{}, len(applied))
	for _, a := range applied {
		if a.Kind != mlswasi.ProposalAdd {
			continue
		}
		deviceID, _, err := decodeCredentialIdentity(a.CredentialIdentity)
		if err != nil {
			// The same refusal invariant 4's Add clause gives an undecodable credential; reaching
			// this line means that clause has already passed it, so it is belt and braces.
			return errCommitInvalid("add_key_package", "undecodable credential identity")
		}
		added[deviceID] = struct{}{}
	}
	for _, wf := range welcomes {
		if _, ok := added[wf.DeviceID]; !ok {
			return errCommitInvalid("welcome_addressee",
				fmt.Sprintf("the commit addresses a Welcome to device %s, which it does not add", wf.DeviceID))
		}
	}
	return nil
}

// leafKeyIsRegistered is the one comparison that binds a NEW leaf to the device its credential
// names (hardening C): the leaf's signature key must be the device's registered key,
// `devices.dsk_pub`, which the device proved possession of when its session was established
// (auth.Sessions.issue) or registered together with its first session (POST /v1/accounts,
// POST /v1/devices). No flow changes a device's dsk_pub, so the value read at the check is the one
// the device has always had. It is applied at the three points a new leaf reaches the delivery
// service: a published KeyPackage, an Add a commit applies, and the leaf an external commit
// creates. An empty or absent leaf key never matches.
func leafKeyIsRegistered(device store.DeviceRow, leafKey []byte) bool {
	return len(leafKey) != 0 && bytes.Equal(leafKey, device.DSKPub)
}

// errCredentialKind is checkCredentialKind's refusal: the credential's `kind` is not the kind of
// the account it names, or cannot be read, or names no account this instance holds.
var errCredentialKind = errors.New("the credential's kind is not its account's kind")

// checkCredentialKind binds a leaf credential's self-signed `kind` to the account it names (Task
// S1). A credential is signed by its own user's SSK, so without this a member could present a
// Bot-kind leaf and every client that reads a sender's kind from its credential would render that
// member as a Bot. It is applied wherever a new leaf reaches the delivery service — a published
// KeyPackage, an Add a commit applies, the leaf an external commit creates, the one leaf a group is
// registered with, a reseeded tree, a replayed resync, and a directory KeyPackage before it is
// spent. A human account (users.kind 0) must present a human-kind credential. A Bot account
// (users.kind 1) is bound more tightly by checkManagedBotCredential: its leaf must be the exact
// credential its installation verified, which enrolment verified as Bot-kind
// (internal/bot/identity), so that clause, unchanged, governs it here. Any other users.kind
// refuses. It fails closed: an unreadable kind or an unknown account is errCredentialKind; any
// other store failure is returned as is (the caller answers it as an error, never as a pass).
func checkCredentialKind(ctx context.Context, repo store.Repository, userID id.ID, identity []byte) error {
	kind, err := decodeCredentialKind(identity)
	if err != nil {
		return fmt.Errorf("%w: %w", errCredentialKind, err)
	}
	user, err := repo.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: the account is unknown to this instance", errCredentialKind)
	}
	if err != nil {
		return err
	}
	return accountKindVerdict(user.Kind, kind)
}

// accountKindVerdict is checkCredentialKind's comparison: a human account presents only a
// human-kind credential; a Bot account is left to checkManagedBotCredential; no other account kind
// presents any credential.
func accountKindVerdict(accountKind, credentialKind uint8) error {
	switch accountKind {
	case 0:
		if credentialKind != 0 {
			return fmt.Errorf("%w: a human account presented a Bot-kind credential", errCredentialKind)
		}
		return nil
	case 1:
		return nil
	default:
		return fmt.Errorf("%w: account kind %d", errCredentialKind, accountKind)
	}
}

// credentialKindVerdict maps checkCredentialKind's answer to the caller's refusal: a kind refusal
// becomes refuse(detail), a store error is passed through.
func credentialKindVerdict(err error, refuse func(string) error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errCredentialKind) {
		return refuse(err.Error())
	}
	return err
}

// checkManagedBotCredential binds a managed account's MLS identity to the final credential the
// installation verified. A human account's credential is bound by checkCredentialKind alone.
// requireCurrent is true at admission points; member-cache rewrites use false so disabling a Bot
// does not block unrelated commits that preserve its already admitted leaf.
func checkManagedBotCredential(ctx context.Context, repo store.Repository, userID id.ID, device store.DeviceRow, identity []byte, requireCurrent bool) (bool, error) {
	user, err := repo.GetUser(ctx, userID)
	if err != nil {
		return false, err
	}
	if user.Kind != 1 {
		return false, nil
	}
	if !bytes.Equal(identity, device.CredentialBlob) {
		return true, errors.New("the Bot leaf credential differs from the verified device credential")
	}
	if requireCurrent {
		binding, bot, err := access.ForUserDevice(ctx, repo, userID, device.ID)
		if err != nil || !bot || binding.State < store.BotInstallationJoining || binding.State > store.BotInstallationReady {
			if err != nil {
				return true, err
			}
			return true, errors.New("the Bot installation is not currently authorized to admit a leaf")
		}
	} else {
		verified, err := hasVerifiedBotCredential(ctx, repo, userID, device.ID)
		if err != nil {
			return true, err
		}
		if !verified {
			return true, errors.New("the Bot has no verified installation credential")
		}
	}
	return true, nil
}

func hasVerifiedBotCredential(ctx context.Context, repo store.Repository, userID, deviceID id.ID) (bool, error) {
	after := id.ID{}
	for {
		rows, err := repo.ListBotInstallationsByDevice(ctx, deviceID, after, 100)
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			if row.UserID != nil && *row.UserID == userID && row.DeviceID != nil && *row.DeviceID == deviceID && row.CredentialVerifiedAt != nil {
				return true, nil
			}
		}
		if len(rows) < 100 {
			return false, nil
		}
		after = rows[len(rows)-1].ID
	}
}

// checkAddedMember validates one Add's credential against the device list the delivery service
// holds and the channel ACL. v is the guest the caller already holds, which the device list is
// verified in (DeviceLists.Entries says why a second instance must not be acquired here).
func (d *DS) checkAddedMember(ctx context.Context, v DeviceListVerifier, groupID id.ID, a mlswasi.AppliedProposal) error {
	deviceID, userID, err := decodeCredentialIdentity(a.CredentialIdentity)
	if err != nil {
		return errCommitInvalid("add_key_package", "undecodable credential identity")
	}
	device, err := d.opts.Store.GetDevice(ctx, deviceID)
	if errors.Is(err, store.ErrNotFound) {
		return errCommitInvalid("add_key_package", "the added device is unknown to this instance")
	}
	if err != nil {
		return err
	}
	if device.UserID != userID {
		return errCommitInvalid("add_key_package", "the credential's user does not own the device")
	}
	if device.RevokedAt != nil {
		return errCommitInvalid("add_key_package", "the added device is revoked")
	}
	// The added leaf is keyed by the device's registered key. A KeyPackage is self-signed under its
	// own leaf key, so without this a committer could mint one that names another user's device
	// under a key of its own, and every clause above and below would pass on the device's records.
	if !leafKeyIsRegistered(device, a.SignatureKey) {
		return errCommitInvalid("add_key_package",
			"the added leaf's signature key is not the device's registered key")
	}
	if err := credentialKindVerdict(checkCredentialKind(ctx, d.opts.Store, userID, a.CredentialIdentity),
		func(detail string) error { return errCommitInvalid("add_key_package", detail) }); err != nil {
		return err
	}
	if _, err := checkManagedBotCredential(ctx, d.opts.Store, userID, device, a.CredentialIdentity, true); err != nil {
		return errCommitInvalid("add_key_package", "the Bot credential is not the current verified installation credential")
	}

	// The device-list clause. A substring search over the serialized blob is NOT membership: a
	// 32-byte window can fall inside another entry's signature or inside any attacker-influenced
	// field, and a blob whose ssk_signature does not verify would be accepted outright. This is
	// the clause that stops a compromised instance or a stale list from admitting a device the
	// user never authorised, so the list is decoded, its signature verified, and the comparison
	// made against a decoded entry.
	entries, err := d.opts.DeviceLists.Entries(ctx, v, userID)
	if err != nil {
		// A missing or unverifiable list is a refusal, never a pass: an Add of an unlisted device
		// must not be accepted because the user happens to have no device_lists row.
		return errCommitInvalid("add_key_package",
			"no verifiable signed device list for the added user: "+err.Error())
	}
	if !auth.Listed(entries, device.ID, device.DSKPub) {
		return errCommitInvalid("add_key_package",
			"the added device's DSK is not in the newest signed device list")
	}

	// The ACL clause (protocol/02 invariant 4, spec line 458).
	ok, err := d.opts.ACL.Eligible(ctx, groupID, userID)
	if err != nil {
		return errCommitInvalid("add_acl", "the channel ACL cannot be resolved: "+err.Error())
	}
	if !ok {
		return errCommitInvalid("add_acl", "the added user is not eligible under the channel's ACL")
	}
	return nil
}

// checkExternalJoiner is invariant 4's Add clause applied to the leaf an external commit creates:
// an external commit adds its committer, so the leaf must be the uploading session's own device and
// user — a joiner may not land a leaf in another device's name — keyed by that device's registered
// key on the join AND the resync path (hardening C: a resync's new leaf is a new leaf like any
// other), and, for a device joining rather than resyncing, its device must not be revoked, and its
// DSK must be in the newest signed device list, exactly as for an Add by proposal
// (`checkAddedMember`). Deviation B36.
func (d *DS) checkExternalJoiner(ctx context.Context, v DeviceListVerifier, groupID id.ID, s Session, state mlswasi.GroupState, newLeaf *uint32, joining bool) error {
	if newLeaf == nil {
		return errCommitInvalid("external_joiner", "the joiner's leaf is unknown")
	}
	var leaf *mlswasi.Member
	for i := range state.Members {
		if state.Members[i].LeafIndex == *newLeaf {
			leaf = &state.Members[i]
			break
		}
	}
	if leaf == nil {
		return errCommitInvalid("external_joiner", "the merged tree holds no leaf where the joiner landed")
	}
	deviceID, userID, err := decodeCredentialIdentity(leaf.CredentialIdentity)
	if err != nil {
		return errCommitInvalid("external_joiner", "undecodable credential identity")
	}
	if deviceID != s.DeviceID || userID != s.UserID {
		return errCommitInvalid("external_joiner",
			"an external commit's leaf must be the uploading device's own")
	}
	device, err := d.opts.Store.GetDevice(ctx, deviceID)
	if err != nil {
		return err
	}
	if joining && device.RevokedAt != nil {
		return errCommitInvalid("external_joiner", "the joining device is revoked")
	}
	if !leafKeyIsRegistered(device, leaf.SignatureKey) {
		return errCommitInvalid("external_joiner", "the joiner's leaf key is not its device key")
	}
	if err := credentialKindVerdict(checkCredentialKind(ctx, d.opts.Store, userID, leaf.CredentialIdentity),
		func(detail string) error { return errCommitInvalid("external_joiner", detail) }); err != nil {
		return err
	}
	if _, err := checkManagedBotCredential(ctx, d.opts.Store, userID, device, leaf.CredentialIdentity, true); err != nil {
		return errCommitInvalid("external_joiner", "the Bot credential is not the current verified installation credential")
	}
	if !joining {
		return nil
	}
	entries, err := d.opts.DeviceLists.Entries(ctx, v, userID)
	if err != nil {
		return errCommitInvalid("external_joiner",
			"no verifiable signed device list for the joining user: "+err.Error())
	}
	if auth.Listed(entries, device.ID, device.DSKPub) {
		return nil
	}
	return errCommitInvalid("external_joiner",
		"the joining device's DSK is not in the newest signed device list")
}

// fanOutCommit sends mls.handshake and mls.epoch_changed to the group. Every payload is encoded
// once (facts-gateway-design §3.3).
//
// kind is the value the transaction actually stored, not a constant: a resync and an external join
// store kind 2 (external_commit), and hardcoding 1 here would make the same handshake kind 1 over
// the gateway and kind 2 in GET /v1/groups/{id}/handshakes — a contradiction for any client that
// reconciles the two streams, and a violation of protocol/02's "Handshake records".
//
// The addressed mls.welcome fan-out comes AFTER the two frames below; protocol/02 fixes that
// order, so a Welcome never precedes the epoch change that produced it.
func (d *DS) fanOutCommit(ctx context.Context, groupID id.ID, r CommitResult, p mlswasi.Processed, c CommitRequest, kind uint8, welcomeTree, welcomeTreeHash []byte) {
	if d.opts.Gateway == nil {
		return
	}
	hs, err := gateway.HandshakePayload(r.Seq, r.Epoch, kind, p.SenderLeaf, c.Commit)
	if err != nil {
		return
	}
	d.opts.Gateway.DeliverGroup(groupID, gateway.Frame{
		Op: gateway.OpMLSHandshake, GroupID: &groupID, Payload: hs, Replay: true,
	})
	ec, err := gateway.EpochChangedPayload(r.Epoch, r.Seq)
	if err != nil {
		return
	}
	d.opts.Gateway.DeliverGroup(groupID, gateway.Frame{
		Op: gateway.OpMLSEpochChanged, GroupID: &groupID, Payload: ec, Replay: true,
	})
	d.fanOutWelcomes(ctx, groupID, r.Epoch, welcomeTree, welcomeTreeHash, c.Welcomes)
}

// Proposal accepts a member's own proposal. Only two shapes are allowed: an Update, and a Remove
// of one of the sender's own devices. ProposalInspect is what tells them apart — for a member
// proposal the delivery service knows neither kind nor target by construction.
func (d *DS) Proposal(ctx context.Context, s Session, groupID id.ID, epoch uint64, blob []byte) (uint64, error) {
	unlock := d.lock(groupID)
	defer unlock()

	if s.Scope != auth.ScopeEnrolled {
		return 0, errForbidden("a proposal needs an enrolled session")
	}
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, errNotFound("group")
	}
	if err != nil {
		return 0, err
	}
	if epoch != row.Epoch {
		return 0, errCommitInvalid("epoch",
			fmt.Sprintf("proposal is for epoch %d, group is at %d", epoch, row.Epoch))
	}
	leaf, err := d.leafOf(ctx, groupID, s.DeviceID)
	if err != nil {
		return 0, err
	}

	var seq uint64
	again := false
	err = d.withGroup(ctx, groupID, func(g *mlswasi.PublicGroup) error {
		processed, err := g.Process(ctx, blob)
		if err != nil {
			return errCommitInvalid("structural", err.Error())
		}
		if processed.Kind != mlswasi.KindProposal {
			return errCommitInvalid("structural", "the uploaded message is not a proposal")
		}
		if processed.SenderLeaf == nil || *processed.SenderLeaf != leaf {
			return errForbidden("a member proposal must be signed by the sending device's own leaf")
		}
		// The same proposal again — a client retrying an upload whose answer it lost. Process wrote
		// nothing, so it is decided here, before the guest's queue is touched at all.
		if len(processed.ProposalRef) > 0 {
			existing, err := d.proposalRowAt(ctx, groupID, row.Epoch, processed.ProposalRef)
			if err != nil {
				return err
			}
			if existing != nil {
				again = true
				seq, err = d.memberProposalAgain(ctx, groupID, row.Epoch, leaf, s.DeviceID, blob, *existing)
				return err
			}
		}
		if err := d.requireGroupCapability(ctx, s, row); err != nil {
			return err
		}
		ref, detail, err := d.queueMemberProposal(ctx, g, s, groupID, row.Epoch, blob)
		if err != nil {
			return err
		}

		// senderDevice is taken once, outside the closure: `s` is the session parameter and must
		// not be shadowed by the seq variable below.
		senderDevice := s.DeviceID
		txErr := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
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
			allocated, err := nextSeq(ctx, tx, groupID)
			if err != nil {
				return err
			}
			seq = allocated
			if err := tx.AppendHandshake(ctx, store.HandshakeRow{
				GroupID: groupID, Seq: seq, Epoch: row.Epoch, Kind: handshakeProposal,
				SenderLeaf: &leaf, SenderDevice: &senderDevice,
				Blob: blob, Created: d.now(),
			}); err != nil {
				return err
			}
			if err := tx.PutProposal(ctx, store.ProposalRow{
				GroupID:    groupID,
				Ref:        ref,
				Epoch:      row.Epoch,
				Kind:       uint8(detail.Kind),
				TargetLeaf: detail.TargetLeaf,
				Origin:     1, // member
				ActionID:   id.New(),
				IssuedAt:   d.now(),
				TTL:        uint64(d.proposalTTL(row.Kind).Seconds()),
			}); err != nil {
				return err
			}
			return persistState(ctx, tx, groupID, g, row.GroupInfoBlob)
		})
		if txErr != nil {
			// The put is already in the guest's queue and the row it was going to get has rolled
			// back. Take it out again, for the same reason the shape refusals do.
			d.unqueueProposal(ctx, g, groupID, ref)
			return txErr
		}
		return nil
	})
	if d.takeStaleAfterFailedMerge(groupID) {
		// Outside withGroup, so the handle lock is free. Reached only when the guest refused to
		// give a refused proposal back, which leaves the cached group ahead of SQL.
		if evErr := d.states.evict(ctx, groupID); evErr != nil {
			d.log().Error("evicting a group whose proposal queue outran its transaction failed",
				"group", groupID, "err", evErr)
		}
	}
	if err != nil {
		return 0, err
	}
	if again {
		return seq, nil // answered as the first upload was; it was fanned out then
	}
	if d.opts.Gateway != nil {
		p, err := gateway.HandshakePayload(seq, row.Epoch, handshakeProposal, &leaf, blob)
		if err == nil {
			d.opts.Gateway.DeliverGroup(groupID, gateway.Frame{
				Op: gateway.OpMLSHandshake, GroupID: &groupID, Payload: p, Replay: true,
			})
		}
	}
	return seq, nil
}

// memberProposalAgain answers a member proposal whose ref SQL already holds at this epoch. A ref is
// a hash over the whole authenticated content — sender, epoch, proposal and signature — and member
// proposals carry no nonce, so this is a client re-sending what it sent before, typically a retry
// of an upload whose answer it lost.
//
// When the row is that member's own proposal — a member row logged with this very blob from this
// leaf and device — the upload is answered as the first one was, with its seq, and nothing is
// queued or fanned out again (the guest and every holder already have it). A live row is left as
// it is. A VOID row is re-armed, exactly as an instance proposal re-signed in its epoch is (R-1 of
// the DS re-review): live again, issued now, a fresh TTL, its action_id kept. The member is asking
// for the same thing again, and answering it anything else would leave it believing a leave or an
// Update stands, or — under E_REMOVE_PENDING — that it is being removed, while nothing removes it.
//
// Only what no conforming client can produce is refused: a ref that names an instance proposal, or
// whose logged sender is another leaf or device (the ref covers the sender, so both need a forged
// table or log). They get 409 E_REMOVE_PENDING, and the row the instance holds is left as it is.
func (d *DS) memberProposalAgain(ctx context.Context, groupID id.ID, epoch uint64, leaf uint32, device id.ID, blob []byte, existing store.ProposalRow) (uint64, error) {
	conflict := errRemovePending()
	conflict.Detail = "this proposal reference names a proposal the uploader did not send"
	if existing.Origin != 1 {
		return 0, conflict
	}
	logged, err := d.loggedProposal(ctx, groupID, epoch, blob)
	if err != nil {
		return 0, err
	}
	if logged == nil || logged.SenderLeaf == nil || *logged.SenderLeaf != leaf ||
		logged.SenderDevice == nil || *logged.SenderDevice != device {
		return 0, conflict
	}
	if existing.VoidAt != nil {
		row, err := d.opts.Store.GetGroup(ctx, groupID)
		if err != nil {
			return 0, err
		}
		rearmed := existing
		rearmed.VoidAt = nil
		rearmed.IssuedAt = d.now()
		rearmed.TTL = uint64(d.proposalTTL(row.Kind).Seconds())
		if err := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
			return tx.ReissueProposal(ctx, existing.Ref, rearmed)
		}); err != nil {
			return 0, err
		}
	}
	return logged.Seq, nil
}

// loggedProposal finds the proposal handshake carrying blob at epoch: it is after the commit that
// opened the epoch (or anywhere in the log for the epoch a group was registered at), and a live
// member proposal is younger than its TTL, so well inside handshake retention.
func (d *DS) loggedProposal(ctx context.Context, groupID id.ID, epoch uint64, blob []byte) (*store.HandshakeRow, error) {
	from := uint64(0)
	opened, err := d.opts.Store.GetCommitAtEpoch(ctx, groupID, epoch)
	switch {
	case err == nil:
		from = opened.Seq
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	for {
		rows, err := d.opts.Store.ListHandshakes(ctx, groupID, from, sweepPage)
		if err != nil {
			return nil, err
		}
		for i := range rows {
			r := rows[i]
			if r.Kind == handshakeProposal && r.Epoch == epoch && bytes.Equal(r.Blob, blob) {
				return &r, nil
			}
		}
		if len(rows) < sweepPage {
			return nil, nil
		}
		from = rows[len(rows)-1].Seq + 1
	}
}

// queueMemberProposal puts a member's proposal into the guest's queue and decides whether its
// shape is one a member may send. `ProposalInspect` is keyed on a ref the queue hands out, so the
// put necessarily precedes the decision.
//
// Every refusal after the put must take the proposal back OUT of the queue. A refused proposal
// left behind is one the cached PublicGroup holds and SQL does not — R12's "SQL is the record",
// inverted — and it stays there until the group is evicted, so an enrolled member grows the queue
// by one entry per refused request. The commit path guards its own equivalent with Discard; this
// is that guard.
func (d *DS) queueMemberProposal(ctx context.Context, g *mlswasi.PublicGroup, s Session, groupID id.ID, epoch uint64, blob []byte) ([]byte, mlswasi.ProposalDetail, error) {
	ref, err := g.ProposalPut(ctx, 0, blob)
	if err != nil {
		return nil, mlswasi.ProposalDetail{}, err
	}
	accepted := false
	defer func() {
		if !accepted {
			d.unqueueProposal(ctx, g, groupID, ref)
		}
	}()
	detail, err := g.ProposalInspect(ctx, ref)
	if err != nil {
		return nil, mlswasi.ProposalDetail{}, err
	}
	switch detail.Kind {
	case mlswasi.ProposalUpdate:
	case mlswasi.ProposalRemove:
		if detail.TargetLeaf == nil {
			return nil, mlswasi.ProposalDetail{}, errForbidden("a member Remove must name its target")
		}
		target, err := d.userOfLeaf(ctx, groupID, *detail.TargetLeaf)
		if err != nil {
			return nil, mlswasi.ProposalDetail{}, err
		}
		if target != s.UserID {
			return nil, mlswasi.ProposalDetail{}, errForbidden("a member may only Remove its own user's devices")
		}
		// A member proposal never cancels, voids, blocks or stands in for an instance proposal.
		// A member Remove is not mandatory for a commit (invariant 4 clause 1 covers instance
		// proposals), freezes nothing and elects nobody, so a member whose leaf the instance is
		// already removing — a kick, a ban, an eviction — who could post its own Remove of that
		// leaf would make OpenMLS keep the later of the two and keep its leaf by never having it
		// committed. It is refused instead, with 409 E_REMOVE_PENDING, which the client reads as
		// "I am being removed".
		pending, err := d.instanceRemoveOutstanding(ctx, groupID, epoch, *detail.TargetLeaf)
		if err != nil {
			return nil, mlswasi.ProposalDetail{}, err
		}
		if pending {
			return nil, mlswasi.ProposalDetail{}, errRemovePending()
		}
	default:
		return nil, mlswasi.ProposalDetail{}, errForbidden(
			"a member may only propose an Update or a Remove of its own devices")
	}
	accepted = true
	return ref, detail, nil
}

// unqueueProposal removes one proposal from the guest's queue by ref (public_group_proposal_put
// op 1). If the removal itself fails the cached PublicGroup is left holding a proposal SQL does
// not have, so the group is marked for eviction and the next request re-imports the committed
// state blob — the same escape hatch a merge that outran its transaction takes.
//
// A ref SQL holds a row for is NOT removed (DS-1 of the server-half review). Proposals are
// deterministic, so the put this call undoes may have put a proposal the guest already held — a
// voided instance proposal re-signed in its epoch, a member's proposal sent twice — and the guest's
// queue keeps one entry per ref: removing it would take the ORIGINAL out, leaving SQL with a row
// the guest no longer has. A store that cannot answer leaves the guest as it is and marks the group
// for eviction, so the next request re-imports the committed state blob, which is right either way.
func (d *DS) unqueueProposal(ctx context.Context, g *mlswasi.PublicGroup, groupID id.ID, ref []byte) {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	held := false
	if err == nil {
		held, err = d.proposalRowExists(ctx, groupID, row.Epoch, ref)
	}
	if err != nil {
		d.log().Warn("checking a refused proposal against SQL failed; the group is re-imported",
			"group", groupID, "err", err)
		d.markStaleAfterFailedMerge(groupID)
		return
	}
	if held {
		return
	}
	if _, err := g.ProposalPut(ctx, 1, ref); err != nil {
		d.log().Warn("removing a refused proposal from the guest's queue failed",
			"group", groupID, "err", err)
		d.markStaleAfterFailedMerge(groupID)
	}
}

// proposalRowExists reports whether SQL holds a proposal row with ref at epoch, void or not. A
// proposal's ref covers its epoch, so the epoch the group is at is the one place to look.
func (d *DS) proposalRowExists(ctx context.Context, groupID id.ID, epoch uint64, ref []byte) (bool, error) {
	row, err := d.proposalRowAt(ctx, groupID, epoch, ref)
	return row != nil, err
}

// proposalRowAt is proposalRowExists' row, or nil.
func (d *DS) proposalRowAt(ctx context.Context, groupID id.ID, epoch uint64, ref []byte) (*store.ProposalRow, error) {
	rows, err := d.opts.Store.ListProposals(ctx, groupID, epoch, true)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if bytes.Equal(rows[i].Ref, ref) {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// leafOf is invariant 8's "current leaf" check, shared by Commit, Proposal and Upload.
func (d *DS) leafOf(ctx context.Context, groupID, deviceID id.ID) (uint32, error) {
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return 0, err
	}
	for _, m := range members {
		if m.DeviceID == deviceID && m.RemovedEpoch == nil {
			return m.LeafIndex, nil
		}
	}
	return 0, errLeafNotCurrent()
}

func (d *DS) userOfLeaf(ctx context.Context, groupID id.ID, leaf uint32) (id.ID, error) {
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return id.ID{}, err
	}
	for _, m := range members {
		if m.LeafIndex == leaf && m.RemovedEpoch == nil {
			return m.UserID, nil
		}
	}
	return id.ID{}, errCommitInvalid("member_remove_scope", "the Remove targets a leaf that is not a member")
}

// deviceOfLeaf is `userOfLeaf`'s sibling: the same `ListMembers` scan, answering with the leaf's
// DEVICE. R25's external-commit scope clause is about the device — "the joining device's own
// previous leaf" — and a user with two devices in one group would pass a user-level check while
// removing the other device's leaf.
func (d *DS) deviceOfLeaf(ctx context.Context, groupID id.ID, leaf uint32) (id.ID, error) {
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return id.ID{}, err
	}
	for _, m := range members {
		if m.LeafIndex == leaf && m.RemovedEpoch == nil {
			return m.DeviceID, nil
		}
	}
	return id.ID{}, errCommitInvalid("external_commit_remove_scope",
		"the Remove targets a leaf that is not a member")
}

func (d *DS) refsOf(ctx context.Context, groupID id.ID, epoch uint64) ([][]byte, error) {
	rows, err := d.opts.Store.ListProposals(ctx, groupID, epoch, false)
	if err != nil {
		return nil, err
	}
	refs := make([][]byte, 0, len(rows))
	for _, r := range rows {
		refs = append(refs, r.Ref)
	}
	return refs, nil
}

// winningCommit is the commit that already took the epoch the loser tried to take.
//
// It is a single indexed lookup, not a scan: paging `ListHandshakes(ctx, groupID, 0, 512)` from
// seq 0 and scanning backwards would, on any group with more than 512 handshakes above the
// retention floor — the 1,500-leaf fixture and every long-lived channel — leave the winner
// outside the page and send E_COMMIT_CONFLICT with a nil `winning_commit`. protocol/02 declares
// that element a `bstr` and the losing client processes it to catch up, so a null there breaks the
// conflict-recovery loop the whole invariant exists for (deviation B13).
func (d *DS) winningCommit(ctx context.Context, groupID id.ID, epoch uint64) ([]byte, error) {
	row, err := d.opts.Store.GetCommitAtEpoch(ctx, groupID, epoch+1)
	if errors.Is(err, store.ErrNotFound) {
		// The epoch bump is recorded but its handshake has been pruned. An empty bstr is honest;
		// a null is not, and the client's decoder expects a bstr.
		d.log().Warn("no handshake for the winning epoch; the log has been pruned past it",
			"group", groupID, "epoch", epoch+1)
		return []byte{}, nil
	}
	if err != nil {
		return nil, err
	}
	return row.Blob, nil
}

func (d *DS) proposalTTL(groupKind uint8) time.Duration {
	if groupKind == groupKindCall {
		return d.opts.Policy.ProposalTTLCall
	}
	return d.opts.Policy.ProposalTTLText
}

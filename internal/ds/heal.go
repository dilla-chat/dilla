package ds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// maxHealTail is how many handshakes a healing member may upload. Clients keep the last 64 per
// group (protocol/02, "Retention"), so 64 is both the client's buffer and the server's bound.
const maxHealTail = 64

type HealStatus struct {
	Epoch       uint64
	NextSeq     uint64
	Generation  uint64
	NeedFromSeq uint64
	NeedsHeal   bool
}

type HealTailItem struct {
	Seq    uint64
	Epoch  uint64
	Kind   uint8
	Sender *uint32
	Blob   []byte
}

type HealRequest struct {
	GroupInfo   []byte
	Tail        []HealTailItem
	RatchetTree []byte
}

// OnRestore is invariant 11's first half. It runs once, after `dillad restore` has replaced the
// database, and it is deliberately loud: everything it does is visible to every client on its next
// request, through the generation.
//
// The `generation` parameter is the generation the caller wants the instance to be at — `dillad
// restore` passes the one it read out of the backup's manifest. It is honoured, not ignored: a
// blind `BumpGeneration` would silently disregard a caller that named a specific value. Passing 0
// means "just bump". `SetGeneration` is monotone in SQL, so a manifest older than the instance's
// own generation cannot walk the number backwards and revive the resume tokens an earlier restore
// already killed.
func (d *DS) OnRestore(ctx context.Context, generation uint64) error {
	deadline := d.opts.Clock.Now().Add(d.opts.Policy.HealWindow).Unix()
	var restored store.InstanceRow
	// Generation fencing, group invalidation, voice shutdown and package purge share one database
	// transaction with managed publication. A publication can therefore land wholly before restore
	// (and be purged) or wholly after it (and see the new generation), never between those steps.
	if err := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockInstanceGeneration(ctx); err != nil {
			return err
		}
		if generation != 0 {
			if err := tx.SetGeneration(ctx, generation); err != nil {
				return err
			}
		} else if _, err := tx.BumpGeneration(ctx); err != nil {
			return err
		}
		if err := tx.MarkAllGroupsEpochUnknown(ctx, deadline); err != nil {
			return err
		}
		if err := tx.EndAllVoiceSessions(ctx, d.now()); err != nil {
			return err
		}
		if _, err := tx.PurgeKeyPackages(ctx, true); err != nil {
			return err
		}
		var err error
		restored, err = tx.GetInstance(ctx)
		return err
	}); err != nil {
		return err
	}

	// Every cached handle from before the restore describes a group state the database no longer
	// holds. Dropping the whole cache is one call and cannot miss a group.
	if err := d.states.closeAll(ctx); err != nil {
		return err
	}
	// "every response carries the new generation" reaches the WEBSOCKET only through this call.
	// `gateway.Options.Generation` is read once at construction, so without it a restored instance
	// would keep greeting clients with the old number and — worse — keep ACCEPTING resume tokens
	// minted before the restore, replaying a ring of frames about epochs it no longer holds to a
	// client that never learned the database changed underneath it. The SQL read below is what
	// makes the published number the one that actually landed, monotone clamp included.
	if d.opts.Gateway != nil {
		d.opts.Gateway.SetGeneration(restored.Generation)
	}
	return nil
}

// RestorePendingKey is the instance setting `dillad restore` leaves behind: the decimal
// generation it set. `dillad restore` runs with no delivery service, so it performs OnRestore's
// SQL itself and the next start finishes the restore through FinishRestore. An empty value is
// "nothing pending".
const RestorePendingKey = "restore_pending_generation"

// FinishRestore completes a restore `dillad restore` left pending, and reports whether one was.
// It runs OnRestore with the recorded generation, which SetGeneration's monotone clamp makes a
// no-op on the number itself, so the CLI path and the in-process path end in the same state;
// what it adds is the heal window measured from this start rather than from the restore, since a
// window that ran while the instance was down gave no member a chance to heal. The mark is
// cleared last, so a start that fails half-way through finishes the restore on the next one.
// The composition root calls it before Start, so the sweeper never closes a group on the stale
// deadline first.
func (d *DS) FinishRestore(ctx context.Context) (bool, error) {
	mark, err := d.opts.Store.GetSetting(ctx, RestorePendingKey)
	if errors.Is(err, store.ErrNotFound) || (err == nil && len(mark) == 0) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	generation, err := strconv.ParseUint(string(mark), 10, 64)
	if err != nil || generation == 0 {
		return false, fmt.Errorf("ds: %s holds %q, not a generation", RestorePendingKey, mark)
	}
	if err := d.OnRestore(ctx, generation); err != nil {
		return false, err
	}
	if err := d.opts.Store.PutSetting(ctx, RestorePendingKey, []byte{}, d.now()); err != nil {
		return false, err
	}
	return true, nil
}

// HealStatus tells a member what the instance still needs.
//
// It is member-only, like every other group-scoped read: a caller whose device is not a member is
// answered E_NOT_FOUND, so neither the group's existence nor its epoch, high-water and generation
// can be probed by any enrolled device on the instance (`requireMember`'s own comment).
func (d *DS) HealStatus(ctx context.Context, s Session, groupID id.ID) (HealStatus, error) {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return HealStatus{}, errNotFound("group")
	}
	if err != nil {
		return HealStatus{}, err
	}
	if err := d.requireMember(ctx, groupID, s); err != nil {
		return HealStatus{}, err
	}
	instance, err := d.opts.Store.GetInstance(ctx)
	if err != nil {
		return HealStatus{}, err
	}
	return HealStatus{
		Epoch:       row.Epoch,
		NextSeq:     row.Seq + 1,
		Generation:  instance.Generation,
		NeedFromSeq: row.Seq + 1,
		NeedsHeal:   row.EpochUnknown,
	}, nil
}

// Heal is invariant 11's second half. The instance rebuilds its PublicGroup from its OWN restored
// state blob and replays the member's tail through it; it adopts the result only when its stored
// epoch is <= the uploaded GroupInfo's epoch, the tail replays cleanly, and the GroupInfo's
// tree_hash equals the rebuilt tree's.
//
// The reseed path is the one exception: when the instance holds no usable blob, the healing member
// uploads the ratchet tree in the same request and the instance reseeds from it. That is the one
// upload where the tree is allowed, because the instance has none, and RFC 9420 §12.4.3.3's signed
// tree_hash is what makes that source safe.
//
// Heal is NOT a second commit path. It is refused unless a restore has marked the group
// epoch-unknown and the heal window is still open, and every commit the tail replays passes
// invariant 4's clauses (`checkHealedCommit`) before it is merged. Without both, any enrolled
// member could merge an Add the channel ACL refuses by uploading it as a one-item "tail" together
// with a GroupInfo it signs itself for the result — every adoption check below only proves that
// the member built the state it claims.
func (d *DS) Heal(ctx context.Context, s Session, groupID id.ID, h HealRequest) (CommitResult, error) {
	return d.HealWithGuard(ctx, s, groupID, h, nil)
}

// HealWithGuard applies an optional acceptance-time authorization check inside the same
// transaction that adopts the healed state. The guard runs after the restored group is fenced and
// before any persisted heal state changes.
func (d *DS) HealWithGuard(ctx context.Context, s Session, groupID id.ID, h HealRequest,
	guard func(context.Context, store.Repository, store.GroupRow) error) (CommitResult, error) {
	if len(h.Tail) > maxHealTail {
		return CommitResult{}, errInvalid("the handshake tail carries more than 64 items")
	}
	if s.Scope != auth.ScopeEnrolled {
		return CommitResult{}, errForbidden("a heal needs an enrolled session")
	}
	defer d.flushEvictions(ctx, groupID) // after the unlock below: defers run last-in, first-out
	unlock := d.lock(groupID)
	defer unlock()

	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return CommitResult{}, errNotFound("group")
	}
	if err != nil {
		return CommitResult{}, err
	}
	if err := d.checkHealState(row); err != nil {
		return CommitResult{}, err
	}
	// The two guards Resync has, for the same reason (DS-3 of the server-half review). The replay
	// below runs no clause 1, so a device the instance is removing — a kick, a ban, an inactivity
	// or watchdog removal, a fork quarantine — could otherwise heal first with a commit that omits
	// its own Remove, and every honest member's heal would be refused after it. A quarantined device
	// neither reads, resyncs nor heals (invariant 9).
	if err := d.refuseQuarantined(ctx, s.DeviceID); err != nil {
		return CommitResult{}, err
	}
	if err := d.guardResyncTarget(ctx, s, groupID, row.Epoch); err != nil {
		return CommitResult{}, err
	}

	inst, err := d.opts.Wasm.Acquire(ctx)
	if err != nil {
		return CommitResult{}, err
	}
	release := true
	defer func() {
		if release {
			inst.Release()
		}
	}()

	var group *mlswasi.PublicGroup
	reseeded := false
	switch {
	case len(row.PublicGroupState) > 0:
		group, err = inst.PublicGroupImport(ctx, row.PublicGroupState, groupID[:])
		if err != nil {
			return CommitResult{}, errCommitInvalid("state",
				"the restored state blob does not import: "+err.Error())
		}
	case len(h.RatchetTree) > 0:
		// The tree is the healer's upload, so the healer must be somebody the instance itself knew
		// as a member: a device of the group as the restore left it (hardening G2b). A device that
		// only its own tree names gets the refusal every heal by a non-member gets.
		if err := d.requireRestoredMember(ctx, groupID, s.DeviceID); err != nil {
			return CommitResult{}, err
		}
		group, err = inst.PublicGroupFromExternal(ctx, h.RatchetTree, h.GroupInfo)
		if err != nil {
			return CommitResult{}, errCommitInvalid("reseed", err.Error())
		}
		reseeded = true
	default:
		return CommitResult{}, errCommitInvalid("reseed",
			"the instance holds no state for this group and the request carries no ratchet tree")
	}
	// The handle belongs to this call until it is either installed in the cache or dropped. A
	// refusal below that returned without closing it would leak one PublicGroup inside the guest
	// for every rejected heal, and the instance it lives in goes back to the pool holding it.
	closeGroup := true
	defer func() {
		if closeGroup {
			_ = group.Close(ctx)
		}
	}()

	base, err := group.State(ctx)
	if err != nil {
		return CommitResult{}, err
	}
	// A reseeded tree already contains all of its prior commits, so replay cannot recover the
	// handshakes between the restored epoch and an ahead snapshot. The uploaded tail does not prove
	// completeness: replay deliberately skips earlier-epoch items because they are already in the
	// tree. Refuse every ahead reseed, even with a nonempty tail, so adoption cannot leave the
	// instance's catch-up log behind. A same-epoch reseed remains safe and may have an empty tail.
	if reseeded && base.Epoch > row.Epoch {
		return CommitResult{}, errCommitInvalid("reseed",
			"the uploaded tree is ahead of the restored epoch and its missing handshake history cannot be verified")
	}
	// Replay the tail. Every item the rebuilt group does not already hold is processed, checked
	// and merged (or queued) in order; a gap or a rejection is a refusal, not a partial adoption.
	replayed, err := d.replayHealTail(ctx, groupID, group, healBase{
		seq: row.Seq, epoch: base.Epoch, reseeded: reseeded,
	}, h.Tail)
	if err != nil {
		return CommitResult{}, err
	}
	state, err := group.State(ctx)
	if err != nil {
		return CommitResult{}, err
	}
	// The uploaded GroupInfo is the member's claim; its signature, epoch and tree hash are what
	// make the claim checkable. The signer is the leaf the GroupInfo names, which the DS now has
	// in its own rebuilt tree.
	signer, err := d.signerLeafOf(s.DeviceID, state)
	if err != nil {
		return CommitResult{}, err
	}
	check, err := group.ValidateGroupInfo(ctx, h.GroupInfo, signer)
	if err != nil {
		return CommitResult{}, errCommitInvalid("group_info", err.Error())
	}
	if !check.SignatureOK {
		return CommitResult{}, errCommitInvalid("group_info_signature",
			"the healing GroupInfo is not member-signed")
	}
	if !bytes.Equal(check.GroupID, groupID[:]) {
		return CommitResult{}, errCommitInvalid("group_info",
			"the healing GroupInfo names another group")
	}
	if check.Epoch < row.Epoch {
		return CommitResult{}, errCommitInvalid("heal_epoch",
			"the uploaded GroupInfo is older than the instance's own stored epoch")
	}
	if check.Epoch != state.Epoch {
		return CommitResult{}, errCommitInvalid("heal_epoch",
			"the uploaded GroupInfo epoch does not match the replayed group state")
	}
	if !bytes.Equal(check.TreeHash, state.TreeHash) {
		return CommitResult{}, errCommitInvalid("tree_hash",
			"the rebuilt tree does not match the GroupInfo's tree_hash")
	}
	// The authenticated GroupInfo signer and group/tree binding above establish authority before
	// the compatibility refusal. The gate still runs before any heal state is persisted.
	if err := d.requireGroupCapability(ctx, s, row); err != nil {
		return CommitResult{}, err
	}
	// A reseeded tree is the member's upload, not the instance's own state: every leaf in it must
	// be one an Add could have put there (finding G2b). The GroupInfo checks above only prove that
	// the healer built the tree it claims.
	if reseeded {
		if err := checkReseededBinding(row.Binding, state.Binding); err != nil {
			return CommitResult{}, err
		}
		// Invariant 1's external-sender rule (DS-MEMBERSHIP-01), on the tree as the replay leaves
		// it: a reseeded group must still be one the instance can propose into.
		if problem := d.externalSendersProblem(row.Kind, state.ExternalSenders); problem != "" {
			return CommitResult{}, errCommitInvalid("reseed", problem)
		}
		if err := d.checkReseededLeaves(ctx, group, groupID, state); err != nil {
			return CommitResult{}, err
		}
	}

	// The replayed tail is APPENDED to mls_handshakes and the group's high-water advances with it.
	// Without this the group is healed for the one device that uploaded the tail and permanently
	// behind for everyone else: every other member catches up with
	// `GET /handshakes?from=<its cursor>` and would receive nothing for the epochs the instance
	// just adopted, while the response's `next_seq` still reported the pre-heal high-water.
	//
	// Every column is what the REPLAY derived, never what the member claimed: `GetCommitAtEpoch`
	// selects on kind and epoch to name a winning commit, the catch-up serves both to every other
	// member, and invariant 9 quarantines the SenderDevice of a reported commit. An item the
	// restored log already holds (its seq is at or below the restored high-water) is not written a
	// second time.
	var high uint64
	var members memberView
	err = d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		if row.CommunityID != nil {
			if err := tx.LockCommunity(ctx, *row.CommunityID); err != nil {
				return err
			}
		}
		if err := tx.LockGroup(ctx, groupID); err != nil {
			return err
		}
		lockedRow, err := tx.GetGroup(ctx, groupID)
		if err != nil {
			return err
		}
		if lockedRow.ClosedAt != nil || !lockedRow.EpochUnknown || lockedRow.Epoch != row.Epoch ||
			(lockedRow.CommunityID == nil) != (row.CommunityID == nil) ||
			(lockedRow.CommunityID != nil && *lockedRow.CommunityID != *row.CommunityID) {
			return errCommitInvalid("heal_state", "the group changed while this heal was being prepared")
		}
		if guard != nil {
			if err := withBorrowedDeviceListVerifier(ctx, d.opts.Wasm, inst, func(scoped context.Context) error { return guard(scoped, tx, lockedRow) }); err != nil {
				return err
			}
		}
		// Sequence-only writers may have committed while the guest replay was being
		// prepared in another process. Continue from the fenced database high-water.
		high = lockedRow.Seq
		for _, r := range replayed {
			if r.item.Seq <= row.Seq {
				continue // already in the restored log
			}
			seq, err := nextSeq(ctx, tx, groupID)
			if err != nil {
				return err
			}
			if err := tx.AppendHandshake(ctx, store.HandshakeRow{
				GroupID:      groupID,
				Seq:          seq,
				Epoch:        r.epoch,
				Kind:         r.kind,
				SenderLeaf:   r.senderLeaf,
				SenderDevice: r.senderDevice,
				Blob:         r.item.Blob,
				Created:      d.now(),
			}); err != nil {
				return err
			}
			high = seq
		}
		if err := persistState(ctx, tx, groupID, group, h.GroupInfo); err != nil {
			return err
		}
		var mErr error
		if members, mErr = d.replaceMembersTx(ctx, tx, groupID, row.Kind, state); mErr != nil {
			return mErr
		}
		return tx.ClearEpochUnknown(ctx, groupID)
	})
	if err != nil {
		return CommitResult{}, err
	}
	// The devices the heal's adopted tree no longer holds leave the call's room once the group lock
	// is released (the deferred flushEvictions above; G29).
	d.queueEviction(groupID, members.removed)

	// The member set the heal adopted is the one invariant 7 elects a committer over and the one
	// every fan-out addresses. Register republishes both after it writes `mls_members`, and a heal
	// that did not would leave the gateway electing devices the rebuilt tree no longer holds.
	if d.opts.Gateway != nil {
		d.opts.Gateway.SetGroupMembers(groupID, members.devices)
		d.opts.Gateway.SetGroupLeaves(groupID, members.leaves)
	}

	if err := d.states.put(ctx, groupID, inst, group); err != nil {
		return CommitResult{}, err
	}
	release = false
	closeGroup = false

	// The restored epoch's instance work, carried across a heal that moved the epoch (DS-3). The
	// replay runs no clause 1 (replayHealCommit says why), so this is what keeps a Remove owed at the
	// backup's epoch owed at the healed one. The adopted state is installed, so the re-issues sign
	// for the healed epoch against the healed tree. A failure is logged, not answered: the heal is
	// durable.
	//
	// It is not inside the heal's transaction: each re-issue is a guest put plus a transaction of its
	// own against the INSTALLED healed group (storeInstanceProposal), which exists only once the heal
	// has committed. A crash between the two, or a failed re-issue, drops the restored epoch's
	// proposals; the backstop is each proposal's own source (R-2 of the DS re-review): the reconcile
	// re-proposes the Removes of quarantined and revoked devices and of users the ACL no longer
	// admits, and the inactivity sweep its own (protocol/02 invariant 11).
	if state.Epoch > row.Epoch {
		if err := d.carryHealedProposals(context.WithoutCancel(ctx), groupID, row.Epoch); err != nil {
			d.log().Error("carrying the restored epoch's instance proposals across a heal failed",
				"group", groupID.String()[:8], "err", err)
		}
	}

	// §2.5's POST /heal response is [epoch, next_seq], so the result carries the NEW high-water.
	return CommitResult{Seq: high, Epoch: state.Epoch}, nil
}

// carryHealedProposals is the restored epoch's instance work after a heal that moved the group from
// fromEpoch to a later one (DS-3 of the server-half review). Every reader of the proposal table
// looks at the current epoch, or at the one before it (reissueAll), so rows left at fromEpoch would
// never be read again: a Remove owed there would be silently dropped while its device keeps its leaf.
//
// Each non-void instance proposal at fromEpoch goes through reissue, which already decides across
// epochs: a Remove whose recorded device no longer holds its leaf in the adopted tree is deleted
// (satisfied), one whose device is still there is re-issued for the healed epoch onto that device
// with the same action_id, and an Add whose device is no longer eligible is deleted while an
// eligible one is re-proposed with a fresh KeyPackage. An Add whose device the adopted tree already
// holds is done and is not re-proposed. Whatever is left at fromEpoch afterwards — void rows,
// members' own proposals, and a re-issue that failed — is deleted. One committer is elected for the
// lot. The group lock is held.
func (d *DS) carryHealedProposals(ctx context.Context, groupID id.ID, fromEpoch uint64) error {
	rows, err := d.opts.Store.ListProposals(ctx, groupID, fromEpoch, true)
	if err != nil || len(rows) == 0 {
		return err
	}
	release := d.suppressElections(groupID)
	carried := false
	for _, r := range rows {
		if r.Origin != 0 || r.VoidAt != nil {
			continue
		}
		if mlswasi.ProposalKind(r.Kind) == mlswasi.ProposalAdd && r.TargetDevice != nil {
			if _, lerr := d.leafOf(ctx, groupID, *r.TargetDevice); lerr == nil {
				continue // already a member of the adopted tree: the Add is done
			}
		}
		if err := d.reissue(ctx, groupID, r); err != nil {
			if ctx.Err() != nil {
				release()
				return ctx.Err()
			}
			d.log().Error("an instance proposal of the restored epoch was not carried across the heal",
				"group", groupID.String()[:8], "action", r.ActionID.String()[:8], "err", err)
			continue
		}
		carried = true
	}
	release()

	left, err := d.opts.Store.ListProposals(ctx, groupID, fromEpoch, true)
	if err != nil {
		return err
	}
	refs := make([][]byte, 0, len(left))
	for _, r := range left {
		refs = append(refs, r.Ref)
		if r.VoidAt == nil && r.Origin == 0 { // the gauge counts live instance proposals only
			d.gaugeDec(r.Kind)
		}
	}
	if len(refs) > 0 {
		if err := d.opts.Store.DeleteProposals(ctx, groupID, refs); err != nil {
			return err
		}
	}
	if carried {
		return d.RequestCommit(ctx, groupID)
	}
	return nil
}

// checkHealState is the gate that keeps Heal the post-restore path it is written for. protocol/02
// invariant 11 defines the heal over a group a restore made epoch-unknown, inside the 24-hour
// window; outside that state the one way to move a group's epoch is POST /commit, with invariants
// 3, 4 and 5 in front of it.
func (d *DS) checkHealState(row store.GroupRow) error {
	if row.ClosedAt != nil {
		return errCommitInvalid("heal_state", "the group is closed")
	}
	if !row.EpochUnknown {
		return errCommitInvalid("heal_state",
			"the group is not awaiting a heal: no restore has marked it epoch-unknown")
	}
	if row.HealDeadline != nil && d.now() >= *row.HealDeadline {
		return errCommitInvalid("heal_state", "the heal window has closed")
	}
	return nil
}

// healBase is what the rebuilt group already holds before the tail is replayed.
type healBase struct {
	seq      uint64 // the restored log's high-water
	epoch    uint64 // the rebuilt group's epoch
	reseeded bool   // built from the member's tree rather than the instance's own blob
}

// healTailAlreadyApplied reports whether the rebuilt group already holds the item, so replaying
// it would be a wrong-epoch (or duplicate) refusal of a legitimate heal.
//
// On the blob path the restored blob covers exactly the log up to its high-water, so the answer is
// the item's seq. On the RESEED path the group was built from the member's tree at the uploaded
// GroupInfo's epoch — the member's tip — so every item for an EARLIER epoch is already baked into
// it, whatever its seq; an item at the tip epoch (a proposal the tip has not committed yet) is not.
// A member that misstates an item's epoch or seq gains nothing: an item it hides is simply not
// replayed or appended, and one it misplaces fails to process.
func healTailAlreadyApplied(item HealTailItem, b healBase) bool {
	if b.reseeded {
		return item.Epoch < b.epoch
	}
	return item.Seq <= b.seq
}

// healReplayed is one tail item as the replay saw it: every field but the item itself is derived
// by the guest or from the tree, never taken from the member's upload.
type healReplayed struct {
	item         HealTailItem
	epoch        uint64
	kind         uint8
	senderLeaf   *uint32
	senderDevice *id.ID
}

// leafIdentity is the device and user behind one leaf, read from the leaf's own credential.
type leafIdentity struct {
	device id.ID
	user   id.ID
}

func leafIdentities(state mlswasi.GroupState) map[uint32]leafIdentity {
	out := make(map[uint32]leafIdentity, len(state.Members))
	for _, m := range state.Members {
		device, user, err := decodeCredentialIdentity(m.CredentialIdentity)
		if err != nil {
			continue
		}
		out[m.LeafIndex] = leafIdentity{device: device, user: user}
	}
	return out
}

func deviceAtLeaf(leaves map[uint32]leafIdentity, leaf *uint32) *id.ID {
	if leaf == nil {
		return nil
	}
	who, ok := leaves[*leaf]
	if !ok {
		return nil
	}
	device := who.device
	return &device
}

// replayHealTail processes every tail item the rebuilt group does not already hold, in order.
//
// A proposal is queued in the guest, not merely processed: `Process` writes nothing, and a later
// commit in the same tail that references the proposal by ref would otherwise fail to process —
// and the healed state would be missing a proposal the members hold. A commit goes through
// `replayHealCommit`, which checks it before it merges it.
func (d *DS) replayHealTail(ctx context.Context, groupID id.ID, g *mlswasi.PublicGroup, b healBase, tail []HealTailItem) ([]healReplayed, error) {
	out := make([]healReplayed, 0, len(tail))
	for _, item := range tail {
		if healTailAlreadyApplied(item, b) {
			continue
		}
		before, err := g.State(ctx)
		if err != nil {
			return nil, err
		}
		leaves := leafIdentities(before)
		processed, err := g.Process(ctx, item.Blob)
		if err != nil {
			return nil, errCommitInvalid("tail", err.Error())
		}
		switch processed.Kind {
		case mlswasi.KindProposal, mlswasi.KindExternalJoin:
			if _, err := g.ProposalPut(ctx, 0, item.Blob); err != nil {
				return nil, errCommitInvalid("tail", err.Error())
			}
			out = append(out, healReplayed{
				item:         item,
				epoch:        processed.Epoch,
				kind:         handshakeProposal,
				senderLeaf:   processed.SenderLeaf,
				senderDevice: deviceAtLeaf(leaves, processed.SenderLeaf),
			})
		case mlswasi.KindCommit:
			r, err := d.replayHealCommit(ctx, groupID, g, leaves, processed)
			if err != nil {
				return nil, err
			}
			r.item = item
			out = append(out, r)
		default:
			return nil, errCommitInvalid("tail", "the tail carries a message the group rejects")
		}
	}
	return out, nil
}

// replayHealCommit is invariant 4 over one replayed commit, then the merge.
//
// The clauses that are about the commit's CONTENT run here exactly as the commit path runs them:
// no Update from the committer, a member-originated Remove only of its proposer's own user, every
// Add's device known, unrevoked, in its user's newest signed device list and eligible under the
// channel ACL, and an external commit's Remove only of the joiner's own previous leaf. The
// identities come from the rebuilt tree as it stood before the commit — the committer is whoever
// signed it, not the device uploading the heal, and `mls_members` describes the restored epoch,
// not the one the replay has reached.
//
// Clause 1 (every outstanding instance proposal is referenced) and invariant 5's freeze are NOT
// re-run: both are judged against the instance's live proposal queue at the moment of the original
// commit, and the restored database's queue describes the backup's epoch, not the ones the tail
// crosses. A replayed commit that legitimately omitted a proposal voided after the backup would be
// refused by them.
func (d *DS) replayHealCommit(ctx context.Context, groupID id.ID, g *mlswasi.PublicGroup, leaves map[uint32]leafIdentity, p mlswasi.Processed) (healReplayed, error) {
	if p.Staged == nil {
		return healReplayed{}, errCommitInvalid("tail", "a commit in the tail did not stage")
	}
	merged := false
	defer func() {
		if !merged {
			if derr := g.Discard(ctx, *p.Staged); derr != nil {
				d.log().Warn("discarding a refused heal commit failed", "group", groupID, "err", derr)
			}
		}
	}()
	if err := d.checkHealedCommit(ctx, g, groupID, leaves, p); err != nil {
		return healReplayed{}, err
	}
	epoch, err := g.Merge(ctx, *p.Staged)
	if err != nil {
		return healReplayed{}, errCommitInvalid("tail", err.Error())
	}
	merged = true

	r := healReplayed{epoch: epoch, senderLeaf: p.SenderLeaf}
	if p.SenderLeaf != nil {
		r.kind = handshakeCommit
		r.senderDevice = deviceAtLeaf(leaves, p.SenderLeaf)
		return r, nil
	}
	r.kind = handshakeExternalCommit
	after, err := g.State(ctx)
	if err != nil {
		return healReplayed{}, err
	}
	joiner, err := d.checkHealedExternalCommit(ctx, g, groupID, leaves, after, p.Applied)
	if err != nil {
		return healReplayed{}, err
	}
	r.senderDevice = &joiner
	return r, nil
}

// checkHealedCommit is invariant 4's content clauses over one replayed commit, before its merge.
// The clause numbers are protocol/02's, as in `checkAppliedProposals`.
func (d *DS) checkHealedCommit(ctx context.Context, g DeviceListVerifier, groupID id.ID, leaves map[uint32]leafIdentity, p mlswasi.Processed) error {
	var committer *leafIdentity
	if p.SenderLeaf != nil {
		who, ok := leaves[*p.SenderLeaf]
		if !ok {
			return errCommitInvalid("structural", "a commit in the tail names a leaf the group does not hold")
		}
		committer = &who
	}
	for _, a := range p.Applied {
		switch a.Kind {
		case mlswasi.ProposalUpdate:
			// Clause 2: no Update from the committer.
			if a.SenderLeaf != nil && p.SenderLeaf != nil && *a.SenderLeaf == *p.SenderLeaf {
				return errCommitInvalid("committer_update", "the commit carries the committer's own Update")
			}
		case mlswasi.ProposalRemove:
			// Clause 3: every member-originated Remove targets its proposer's own user. An
			// instance Remove (no sender leaf) is invariant 6's, and an external commit's own
			// Remove is checked after the merge, where the joiner can be seen.
			if a.SenderLeaf == nil || a.TargetLeaf == nil || committer == nil {
				continue
			}
			// The proposer's own user, as commitLocked's clause 3: a member's referenced Remove of
			// its own leaf (DEV-47) is scoped to that member, not to the committer.
			owner := committer.user
			if *a.SenderLeaf != *p.SenderLeaf {
				proposer, ok := leaves[*a.SenderLeaf]
				if !ok {
					return errCommitInvalid("member_remove_scope", "a Remove in the tail names a proposer the group does not hold")
				}
				owner = proposer.user
			}
			target, ok := leaves[*a.TargetLeaf]
			if !ok || target.user != owner {
				return errCommitInvalid("member_remove_scope",
					"a member-originated Remove may only target its proposer's own user")
			}
		case mlswasi.ProposalAdd:
			// Clause 4: the added device is known, unrevoked, listed and eligible.
			if err := d.checkAddedMember(ctx, g, groupID, a); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkHealedExternalCommit is R25's scope clause over a replayed external commit, and returns the
// joining device. An external commit adds exactly one leaf, the joiner's: on a resync it also
// Removes that same device's previous leaf, so a removed device must be back in the tree after the
// merge; on a fresh join nothing is removed and the joiner is the one device the merge added,
// which must pass the same eligibility clause an Add does.
func (d *DS) checkHealedExternalCommit(ctx context.Context, g DeviceListVerifier, groupID id.ID, before map[uint32]leafIdentity, after mlswasi.GroupState, applied []mlswasi.AppliedProposal) (id.ID, error) {
	present := make(map[id.ID]struct{}, len(before))
	for _, who := range before {
		present[who.device] = struct{}{}
	}
	now := make(map[id.ID][]byte, len(after.Members))
	keys := make(map[id.ID][]byte, len(after.Members))
	for _, m := range after.Members {
		device, _, err := decodeCredentialIdentity(m.CredentialIdentity)
		if err != nil {
			continue
		}
		now[device] = m.CredentialIdentity
		keys[device] = m.SignatureKey
	}

	var joiner id.ID
	found := false
	for _, a := range applied {
		if a.Kind != mlswasi.ProposalRemove || a.TargetLeaf == nil {
			continue
		}
		target, ok := before[*a.TargetLeaf]
		if _, back := now[target.device]; !ok || !back || (found && target.device != joiner) {
			return id.ID{}, errCommitInvalid("external_commit_remove_scope",
				"an external commit may only Remove the joining device's own previous leaf")
		}
		joiner, found = target.device, true
	}
	if found {
		// A replayed resync creates a new leaf like any other: it is keyed by the device's
		// registered key, as checkExternalJoiner requires of a live one.
		device, err := d.opts.Store.GetDevice(ctx, joiner)
		if err != nil {
			return id.ID{}, err
		}
		if !leafKeyIsRegistered(device, keys[joiner]) {
			return id.ID{}, errCommitInvalid("external_joiner", "the joiner's leaf key is not its device key")
		}
		if err := credentialKindVerdict(checkCredentialKind(ctx, d.opts.Store, device.UserID, now[joiner]),
			func(detail string) error { return errCommitInvalid("external_joiner", detail) }); err != nil {
			return id.ID{}, err
		}
		return joiner, nil
	}

	var added []id.ID
	for device := range now {
		if _, was := present[device]; !was {
			added = append(added, device)
		}
	}
	if len(added) != 1 {
		return id.ID{}, errCommitInvalid("structural", "an external commit must add exactly one device")
	}
	// The joiner's leaf key travels with its credential: checkAddedMember binds it to the device's
	// registered key exactly as it binds an Add's.
	if err := d.checkAddedMember(ctx, g, groupID, mlswasi.AppliedProposal{
		Kind: mlswasi.ProposalAdd, CredentialIdentity: now[added[0]], SignatureKey: keys[added[0]],
	}); err != nil {
		return id.ID{}, err
	}
	return added[0], nil
}

// checkReseededLeaves is what a reseed may adopt (finding G2b of the second hardening review). The
// reseed builds the group from the healing member's uploaded ratchet tree, because the instance
// lost its own state blob; nothing in that tree passed invariant 4 on its way in, so every leaf
// passes here the clause an Add passes (`checkAddedMember`): the device the credential names is
// known to this instance and owned by the credential's user, is not revoked, carries its
// registered key, is in its user's newest signed device list, and the user is eligible under the
// ACL. The verdicts are the Add clause's own rules (add_key_package, add_acl). The tree must also
// hold no leaf the member list cannot name (LeafCount, ABI v5): such a leaf passes no check and
// would still receive the group's secrets.
//
// It is batched over the tree - one device list per user, one ACL question per group where the ACL
// answers in batches - because a reseeded channel group holds as many leaves as the channel has
// devices. What it does NOT bound is the set of devices: a tree may hold a device the restored
// member rows do not, because the members' commits after the backup are already inside the tree
// and the reseed path has no state to replay them from (see the hardening-G report).
func (d *DS) checkReseededLeaves(ctx context.Context, v DeviceListVerifier, groupID id.ID, state mlswasi.GroupState) error {
	if state.LeafCount != uint64(len(state.Members)) {
		return errCommitInvalid("reseed", fmt.Sprintf(
			"the reseeded tree holds %d leaves and names %d: a leaf's credential is no dilla identity",
			state.LeafCount, len(state.Members)))
	}
	type leafDevice struct {
		device store.DeviceRow
		user   id.ID
	}
	byUser := map[id.ID][]leafDevice{}
	var users []id.ID
	for _, m := range state.Members {
		deviceID, userID, err := decodeCredentialIdentity(m.CredentialIdentity)
		if err != nil {
			return errCommitInvalid("add_key_package", "undecodable credential identity")
		}
		device, err := d.opts.Store.GetDevice(ctx, deviceID)
		if errors.Is(err, store.ErrNotFound) {
			return errCommitInvalid("add_key_package", "a reseeded leaf's device is unknown to this instance")
		}
		if err != nil {
			return err
		}
		if device.UserID != userID {
			return errCommitInvalid("add_key_package", "a reseeded leaf's user does not own the device")
		}
		if device.RevokedAt != nil {
			return errCommitInvalid("add_key_package", "a reseeded leaf's device is revoked")
		}
		if !leafKeyIsRegistered(device, m.SignatureKey) {
			return errCommitInvalid("add_key_package",
				"a reseeded leaf's signature key is not the device's registered key")
		}
		if err := credentialKindVerdict(checkCredentialKind(ctx, d.opts.Store, userID, m.CredentialIdentity),
			func(detail string) error { return errCommitInvalid("add_key_package", "a reseeded leaf: "+detail) }); err != nil {
			return err
		}
		if _, seen := byUser[userID]; !seen {
			users = append(users, userID)
		}
		byUser[userID] = append(byUser[userID], leafDevice{device: device, user: userID})
	}
	for _, userID := range users {
		entries, err := d.opts.DeviceLists.Entries(ctx, v, userID)
		if err != nil {
			return errCommitInvalid("add_key_package",
				"no verifiable signed device list for a reseeded leaf's user: "+err.Error())
		}
		for _, leaf := range byUser[userID] {
			if !auth.Listed(entries, leaf.device.ID, leaf.device.DSKPub) {
				return errCommitInvalid("add_key_package",
					"a reseeded leaf's device is not in its user's newest signed device list")
			}
		}
	}
	eligible, err := d.eligibleUsers(ctx, groupID, users)
	if err != nil {
		return errCommitInvalid("add_acl", "the channel ACL cannot be resolved: "+err.Error())
	}
	for _, userID := range users {
		if !eligible[userID] {
			return errCommitInvalid("add_acl", "a reseeded leaf's user is not eligible under the channel's ACL")
		}
	}
	return nil
}

// requireRestoredMember is the reseed's gate on its healer: the uploading device holds a live leaf
// in mls_members as the restore left them, the instance's own record of the group. Otherwise
// 403 E_FORBIDDEN, the answer signerLeafOf gives a heal by a device that is not a member.
func (d *DS) requireRestoredMember(ctx context.Context, groupID, deviceID id.ID) error {
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.RemovedEpoch == nil && m.DeviceID == deviceID {
			return nil
		}
	}
	return errForbidden("a reseeding heal needs a healer the restored group holds as a member")
}

// checkReseededBinding is the reseed's binding check: the uploaded tree's dilla_binding names the
// group the instance holds - the same kind, community and target as the binding stored with the
// group. The GroupInfo checks only tie the tree to the group id; without this a reseed could move
// a group to another target or kind. Rule "reseed".
func checkReseededBinding(stored, uploaded []byte) error {
	wantV2, wantV2Err := decodeReseededInteractionBinding(stored)
	gotV2, gotV2Err := decodeReseededInteractionBinding(uploaded)
	if wantV2Err == nil || gotV2Err == nil {
		if wantV2Err != nil || gotV2Err != nil {
			return errCommitInvalid("reseed", "the stored and reseeded dilla_binding versions do not match")
		}
		if wantV2.InstanceID != gotV2.InstanceID || wantV2.BotUserID != gotV2.BotUserID ||
			wantV2.Kind != gotV2.Kind || wantV2.E2EEVersion != gotV2.E2EEVersion || wantV2.MediaVersion != gotV2.MediaVersion ||
			wantV2.InstallationID != gotV2.InstallationID || wantV2.CommunityID != gotV2.CommunityID ||
			wantV2.ParticipantUserID != gotV2.ParticipantUserID || wantV2.ParticipantDeviceID != gotV2.ParticipantDeviceID {
			return errCommitInvalid("reseed", "the reseeded v2 dilla_binding identity or protocol versions are not the group's")
		}
		return nil
	}
	want, err := decodeBinding(stored)
	if err != nil {
		return errCommitInvalid("reseed", "the instance's stored binding does not decode: "+err.Error())
	}
	got, err := decodeBinding(uploaded)
	if err != nil {
		return errCommitInvalid("reseed", "the reseeded tree's binding does not decode: "+err.Error())
	}
	if want.V != 1 || got.V != 1 {
		return errCommitInvalid("reseed", "the stored and reseeded legacy dilla_binding versions must both be 1")
	}
	sameCommunity := (want.CommunityID == nil) == (got.CommunityID == nil) &&
		(want.CommunityID == nil || *want.CommunityID == *got.CommunityID)
	if got.Kind != want.Kind || got.TargetID != want.TargetID || !sameCommunity {
		return errCommitInvalid("reseed",
			"the reseeded tree's dilla_binding (kind, community, target) is not the group's")
	}
	return nil
}

// decodeReseededInteractionBinding is the strict v2 shape used for Bot interaction groups. The
// policy version intentionally is not compared: like v1, it records the policy snapshot at group
// creation and is not part of the group's immutable identity.
func decodeReseededInteractionBinding(raw []byte) (interactionBindingV2, error) {
	var b interactionBindingV2
	if err := cborx.Unmarshal(raw, &b); err != nil {
		return interactionBindingV2{}, err
	}
	if b.Version != 2 || b.LegacyCommunityID != nil || b.Kind != 3 || b.PolicyVersion == 0 || b.E2EEVersion != 1 || b.MediaVersion != 0 ||
		b.InstanceID.IsZero() || b.BotUserID.IsZero() || b.InstallationID.IsZero() || b.CommunityID.IsZero() ||
		b.ParticipantUserID.IsZero() || b.ParticipantDeviceID.IsZero() {
		return interactionBindingV2{}, errors.New("invalid v2 interaction binding")
	}
	return b, nil
}

// eligibleUsers is the ACL's verdict for many users of one group: one question when the ACL
// answers in batches (BatchACL), one per user otherwise.
func (d *DS) eligibleUsers(ctx context.Context, groupID id.ID, users []id.ID) (map[id.ID]bool, error) {
	if b, ok := d.opts.ACL.(BatchACL); ok {
		return b.EligibleUsers(ctx, groupID, users)
	}
	out := make(map[id.ID]bool, len(users))
	for _, u := range users {
		ok, err := d.opts.ACL.Eligible(ctx, groupID, u)
		if err != nil {
			return nil, err
		}
		out[u] = ok
	}
	return out, nil
}

// signerLeafOf is the leaf the healing device occupies in the rebuilt tree.
func (d *DS) signerLeafOf(deviceID id.ID, state mlswasi.GroupState) (uint32, error) {
	for _, m := range state.Members {
		device, _, err := decodeCredentialIdentity(m.CredentialIdentity)
		if err != nil {
			continue
		}
		if device == deviceID {
			return m.LeafIndex, nil
		}
	}
	return 0, errForbidden("the healing device is not a member of the rebuilt tree")
}

// closeUnhealedGroups runs on the sweeper: a group that has not been healed within the heal window
// is closed, and the channel owner's device re-creates it.
//
// It PAGES to completion, like every other sweeper in this plan: a fixed batch from the zero id
// would leave a group beyond the batch un-closed forever, still serving state the restored
// database no longer matches.
//
// The cursor advances on every row, closed or not: `ListOpenGroups` filters on `closed_at IS NULL`,
// so a page whose groups this pass closes would otherwise be a page the NEXT query no longer
// returns while `after` still pointed before it — and the loop would re-read the same window
// forever on an instance with more unhealed groups than one page.
func (d *DS) closeUnhealedGroups(ctx context.Context) error {
	now := d.now()
	after := id.ID{}
	for {
		groups, err := d.opts.Store.ListOpenGroups(ctx, after, sweepPage)
		if err != nil {
			return err
		}
		if len(groups) == 0 {
			return nil
		}
		for _, g := range groups {
			after = g.GroupID
			if !g.EpochUnknown || g.HealDeadline == nil || now < *g.HealDeadline {
				continue
			}
			if err := d.Close(ctx, g.GroupID); err != nil {
				return err
			}
			d.log().Warn("group closed: no heal within the window", "group", g.GroupID.String()[:8])
		}
		if len(groups) < sweepPage {
			return nil
		}
	}
}

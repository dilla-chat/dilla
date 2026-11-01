package ds

import (
	"context"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// export_test.go exposes three unexported decoders and one unexported rule to the external test
// package. The two decoders are pinned against committed vectors and the rule against a channel
// shape no committed fixture can supply, which is the whole point of testing them here.

// DecodeCredentialIdentityForTest is decodeCredentialIdentity.
var DecodeCredentialIdentityForTest = decodeCredentialIdentity

// DecodeBindingForTest is decodeBinding.
func DecodeBindingForTest(b []byte) (Binding, error) { return decodeBinding(b) }

// CheckChannelModeForTest is invariant 1's rule on its own. The committed fixture is a TEXT group
// and there is no call-group fixture, so the clause that lets a call group onto a readable or
// discoverable channel is exercised here rather than through a Register no fixture can drive.
func CheckChannelModeForTest(d *DS, ctx context.Context, b Binding) error {
	return d.checkChannelMode(ctx, b)
}

// ExternalSendersProblemForTest is invariant 1's external-sender rule on its own
// (DS-MEMBERSHIP-01). The fixtures supply the wrong-key and the pairing-with-a-sender shapes end to
// end; the wrong credential, a second entry and a missing one are shapes only a patched client
// builds, so the rule's every branch is pinned here.
func ExternalSendersProblemForTest(d *DS, kind uint8, senders []mlswasi.ExternalSender) string {
	return d.externalSendersProblem(kind, senders)
}

// ReconcileLeavesForTest is the sweeper's leaf reconcile on its own, without the rest of Sweep
// (whose inactivity pass would also propose Removes over the fixture's devices).
func ReconcileLeavesForTest(d *DS, ctx context.Context) (int, error) { return d.reconcileLeaves(ctx) }

// MemberProposalAgainForTest is Proposal's answer to a member proposal whose ref SQL already holds.
func MemberProposalAgainForTest(d *DS, ctx context.Context, groupID id.ID, epoch uint64, leaf uint32, device id.ID, blob []byte, existing store.ProposalRow) (uint64, error) {
	return d.memberProposalAgain(ctx, groupID, epoch, leaf, device, blob, existing)
}

// RemoveInactiveForTest is the sweeper's inactivity pass on its own, so the leaf-reuse hook of the
// store (reuseLeafAfterNextRead) fires on that pass's member read and not on the reconcile's.
func RemoveInactiveForTest(d *DS, ctx context.Context) (int, error) { return d.removeInactive(ctx) }

// ACLForTest and DeviceListsForTest are the seams New defaulted, which is the only way to see
// that a DS built without them is built with the conservative Plan-1 stubs rather than with
// nothing at all.
func ACLForTest(d *DS) ACL { return d.opts.ACL }

func DeviceListsForTest(d *DS) DeviceLists { return d.opts.DeviceLists }

// CheckAddedMemberForTest is invariant 4's Add clause on its own, verified in the group's own
// instance exactly as the commit path does. No committed commit adds a device this instance can
// be made to know AND that passes the ACL, so the clause's accepting branch is reached here.
func CheckAddedMemberForTest(d *DS, ctx context.Context, groupID id.ID, a mlswasi.AppliedProposal) error {
	return d.withGroup(ctx, groupID, func(g *mlswasi.PublicGroup) error {
		return d.checkAddedMember(ctx, g, groupID, a)
	})
}

// DecodeCredentialKindForTest is decodeCredentialKind (Task S1).
var DecodeCredentialKindForTest = decodeCredentialKind

// CheckCredentialKindForTest is checkCredentialKind against the delivery service's own store.
func CheckCredentialKindForTest(d *DS, ctx context.Context, userID id.ID, identity []byte) error {
	return checkCredentialKind(ctx, d.opts.Store, userID, identity)
}

// ErrCredentialKindForTest is checkCredentialKind's refusal.
var ErrCredentialKindForTest = errCredentialKind

// CheckExternalJoinerForTest is checkExternalJoiner verified in the group's own instance, over a
// merged state the test supplies (no committed fixture holds an external commit).
func CheckExternalJoinerForTest(d *DS, ctx context.Context, s Session, groupID id.ID, state mlswasi.GroupState, newLeaf uint32, joining bool) error {
	return d.withGroup(ctx, groupID, func(g *mlswasi.PublicGroup) error {
		return d.checkExternalJoiner(ctx, g, groupID, s, state, &newLeaf, joining)
	})
}

// WithGroupForTest is withGroup WITHOUT the per-group lock every public read path takes first.
// Tree and Register serialise one group's work through d.lock, so the state cache's own
// concurrency — the reservation that keeps two first touches of one group from importing it
// twice, and from holding two instances out of a pool that hands back neither — can only be
// driven by calling the cache directly.
func WithGroupForTest(d *DS, ctx context.Context, groupID id.ID, fn func(*mlswasi.PublicGroup) error) error {
	return d.withGroup(ctx, groupID, fn)
}

// EvictStateForTest drops one group's cached handle. Task 27's reseed test rewrites
// `public_group_state` behind the delivery service's back, which is exactly what a restore from a
// backup that predates the group does to SQL — and the cached handle, which R12 says SQL is the
// record for, has to go with it.
func EvictStateForTest(d *DS, ctx context.Context, groupID id.ID) error {
	return d.states.evict(ctx, groupID)
}

// CommitExternalForTest is `commit` on the EXTERNAL path — the one tasks 24 and 25 turn on. No
// caller sets commitOptions.external in task 20, so the two branches that separate a member commit
// from an external one (the structural sender check, and the staged-handle release that has to
// cover it) have no entry point from the public surface. They are reachable code in the shipped
// binary the moment task 24 lands, and a handle leak on that path is driven by any enrolled
// device, so they are tested here rather than left for the task that turns the flag on.
func CommitExternalForTest(d *DS, ctx context.Context, s Session, groupID id.ID, c CommitRequest) (CommitResult, error) {
	return d.commit(ctx, s, groupID, c, commitOptions{external: true})
}

// QueueMemberProposalForTest is queueMemberProposal: the guest-side half of `Proposal`, from the
// put into the PublicGroup's queue to the shape decision. It is exported because the leaf check
// above it refuses every proposal this repository holds — the fixture's one proposal has an
// external sender — so the refusals that happen AFTER the queue has been written cannot be reached
// through `Proposal` with committed material.
func QueueMemberProposalForTest(d *DS, ctx context.Context, g *mlswasi.PublicGroup, s Session, groupID id.ID, blob []byte) ([]byte, mlswasi.ProposalDetail, error) {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return nil, mlswasi.ProposalDetail{}, err
	}
	return d.queueMemberProposal(ctx, g, s, groupID, row.Epoch, blob)
}

// ------------------------------------------------------------------- task 21

// FreezeStateForTest is freezeState: invariant 5's predicate WITH the online clause. It is
// exported because the two callers that consult it — the external-commit path (tasks 24 and 25)
// and the resync exemption — do not exist yet, and R10's "the freeze lifts when nobody is online"
// is the one clause a later task could quietly drop.
func FreezeStateForTest(d *DS, ctx context.Context, groupID id.ID, epoch uint64) (bool, [][]byte, error) {
	return d.freezeState(ctx, groupID, epoch)
}

// RequireNoFreezeForTest is requireNoFreeze: the guard the MESSAGE path takes, which deliberately
// does NOT consult the online predicate. `DS.Upload` is task 23's, so this is the only way to
// assert that difference before the endpoint exists — and getting it wrong lets ciphertext
// through a live freeze.
func RequireNoFreezeForTest(d *DS, ctx context.Context, groupID id.ID, epoch uint64) error {
	return d.requireNoFreeze(ctx, groupID, epoch)
}

// SweepProposalsForTest is sweepProposals. The retention sweeper that owns its tick is task 26's;
// invariant 6's TTL is task 21's, and it is evaluated lazily at a decision point rather than by an
// armed timer, so the tests drive the sweep directly against clock.Fake.
func SweepProposalsForTest(d *DS, ctx context.Context) (int, error) {
	return d.sweepProposals(ctx)
}

// ProposalTTLForTest is proposalTTL: the map from a group's kind to invariant 6's TTL. It is
// exported because no committed fixture registers a CALL group — `Register` takes the kind from
// the binding the fixture's group context carries, and that fixture is text — so the 30 s arm is
// unreachable through any public entry point in this package, and a test that writes the TTL into
// the proposal row itself asserts only the sweep's arithmetic against a number it supplied.
func ProposalTTLForTest(d *DS, groupKind uint8) time.Duration { return d.proposalTTL(groupKind) }

// ------------------------------------------------------------------- task 22

// ArmedElectionsForTest is how many groups hold a live committer election. Invariant 7's "a group
// with nobody online arms no timer" is a statement about the delivery service's own memory, and
// nothing on the wire shows it: the frame that is not sent leaves no trace.
func ArmedElectionsForTest(d *DS) int { return d.armedElections() }

// SuppressElectionsForTest opens a batch's election window and returns its release. It is
// exported because the property it carries — MANY instance proposals in one operation hold ONE
// election, not one each — cannot be driven through `ProposeAddBatch` with committed material:
// `testkit/fixtures/ds-1500` ships a single KeyPackage, so every Add built from it is byte for
// byte the same proposal with the same ref, and the second row of a two-device batch is refused by
// `mls_pending_proposals`' primary key. Two Removes of two leaves are two real proposals, and the
// window is what makes them one round.
func SuppressElectionsForTest(d *DS, groupID id.ID) func() { return d.suppressElections(groupID) }

// ElectionSuppressedForTest reports whether a batch window is open for this group. A test that
// observes it from inside `PutProposal` sees whether the loop that wrote the row was running in
// one.
func ElectionSuppressedForTest(d *DS, groupID id.ID) bool { return d.electionsSuppressed(groupID) }

// LostRoundsForTest is how many acknowledged-and-lost rounds a device has been charged. Three of
// them remove it from the group, and the charge itself is in memory: nothing on the wire or in SQL
// shows a device one round away from an instance Remove.
func LostRoundsForTest(d *DS, groupID, deviceID id.ID) int { return d.lostRounds(groupID, deviceID) }

// PolicyForTest is the Policy the delivery service actually runs with, after New filled in
// whatever the caller left unset. A zero interval here is a panic in Start's goroutine.
func PolicyForTest(d *DS) Policy { return d.opts.Policy }

// CheckKeyPackageLifetimeForTest is the KeyPackage lifetime bound on its own: the guest accepts any
// `not_after` in the future, so the hostile values (u64::MAX, 2^63) are reachable only here, since
// no generator in this repository can mint a package that carries them.
func CheckKeyPackageLifetimeForTest(now int64, notAfter uint64, maxLifetime time.Duration) error {
	return checkKeyPackageLifetime(now, notAfter, maxLifetime)
}

// StillEligibleForTest is stillEligible, the one-device form of the drain's eligibility rule.
func StillEligibleForTest(d *DS, ctx context.Context, groupID, deviceID id.ID, now int64) (bool, error) {
	return d.stillEligible(ctx, groupID, deviceID, now)
}

// PendingJoinsForTest is how many devices of a join storm are waiting for the next commit.
func PendingJoinsForTest(d *DS, groupID id.ID) int {
	n, err := d.opts.Store.CountPendingJoins(context.Background(), groupID)
	if err != nil {
		panic(err)
	}
	return int(n)
}

// ------------------------------------------------------------------- task 24

// StoreWelcomesForTest is storeWelcomesTx, run in a transaction of its own over the same group
// handle the commit's transaction holds.
//
// It is exported because NO commit can be ACCEPTED against the committed fixture: the fixture
// holds one GroupInfo, at epoch 6, and invariant 4's sixth clause wants epoch n+1
// (commit_test.go's header; task 20's report names the fixture work that closes it). `Commit`
// therefore refuses before it ever reaches the Welcome writer, and every rule the writer carries —
// one payload row per distinct blob, one welcome row per addressed device, the ratchet tree of the
// welcoming epoch — would be untested until that fixture work lands.
func StoreWelcomesForTest(d *DS, ctx context.Context, groupID id.ID, epoch, commitSeq uint64, welcomes []WelcomeFor) error {
	return d.withGroup(ctx, groupID, func(g *mlswasi.PublicGroup) error {
		return d.opts.Store.Tx(ctx, func(tx store.Repository) error {
			_, _, err := d.storeWelcomesTx(ctx, tx, groupID, epoch, commitSeq, g, welcomes)
			return err
		})
	})
}

// CheckAddressedWelcomesForTest is checkAddressedWelcomes: the clause that keeps a commit's
// addressed Welcomes inside the set of devices that commit ADDS. It is exported for the reason
// StoreWelcomesForTest is — no commit can be accepted against the committed fixture, so the clause
// has no reachable path through `Commit` — and because the Adds it is defined over cannot be minted
// for devices a test invents.
func CheckAddressedWelcomesForTest(applied []mlswasi.AppliedProposal, welcomes []WelcomeFor) error {
	return checkAddressedWelcomes(applied, welcomes)
}

// FanOutWelcomesForTest is fanOutWelcomes, the addressed half of the commit's fan-out. Same
// reason: `fanOutCommit` runs only after a commit the fixture cannot make succeed, and a joiner
// that is online when the commit lands must not have to poll row 15 to discover its Welcome.
// `tree` and `treeHash` are the welcoming epoch's, which the commit's own transaction hands the
// fan-out; a test supplies the pair `storeWelcomesTx` or `putWelcomeRow` stored for that epoch.
func FanOutWelcomesForTest(d *DS, ctx context.Context, groupID id.ID, epoch uint64, tree, treeHash []byte, welcomes []WelcomeFor) {
	d.fanOutWelcomes(ctx, groupID, epoch, tree, treeHash, welcomes)
}

// CheckExternalCommitScopeForTest is checkExternalCommitScope, R25's guard 2: an external commit
// may Remove nobody but the joining device's own previous leaf. It is exported for the same reason
// CheckAddressedWelcomesForTest is — no commit in this repository can be ACCEPTED, and the fixture
// holds no external commit at all — so the clause is asserted over the applied list it is defined
// on rather than over a commit that cannot exist.
func CheckExternalCommitScopeForTest(d *DS, ctx context.Context, groupID id.ID, s Session, applied []mlswasi.AppliedProposal) error {
	return d.checkExternalCommitScope(ctx, groupID, s, applied)
}

// CheckHealedExternalCommitForTest is checkHealedExternalCommit, heal's clause over a replayed
// external commit, in the group's own instance. before maps a leaf of the pre-commit tree to its
// [device, user]. The fixture holds no external commit, so the clause is asserted over the merged
// state it is defined on (T2 of the second hardening review pins its key check).
func CheckHealedExternalCommitForTest(d *DS, ctx context.Context, groupID id.ID, before map[uint32][2]id.ID, after mlswasi.GroupState, applied []mlswasi.AppliedProposal) (id.ID, error) {
	leaves := make(map[uint32]leafIdentity, len(before))
	for leaf, who := range before {
		leaves[leaf] = leafIdentity{device: who[0], user: who[1]}
	}
	var joiner id.ID
	err := d.withGroup(ctx, groupID, func(g *mlswasi.PublicGroup) error {
		var err error
		joiner, err = d.checkHealedExternalCommit(ctx, g, groupID, leaves, after, applied)
		return err
	})
	return joiner, err
}

// CheckExternalCommitReplacesOwnLeafForTest is checkExternalCommitReplacesOwnLeaf (one device, one
// leaf), exported for the reason CheckExternalCommitScopeForTest is: the core's public group
// refuses the same shape inside Process, so no commit reaches the Go clause, and it is asserted
// over the applied list it is defined on.
func CheckExternalCommitReplacesOwnLeafForTest(d *DS, ctx context.Context, groupID id.ID, s Session, applied []mlswasi.AppliedProposal) error {
	return d.checkExternalCommitReplacesOwnLeaf(ctx, groupID, s, applied)
}

// ReissueOmittedUnderLockForTest runs `reissueOmitted` exactly as commit step (9) runs it: with the
// group lock HELD and outside `withGroup`. It is deviation B24 / ruling 45's regression guard —
// everything reachable from `reissue` must be lock-free, and nothing FAILS today if a later change
// makes one of those paths take `d.lock(groupID)` again.
//
// It is a seam rather than an accepted commit because no commit in this repository can be
// accepted, which is the same reason task 22 had to carry the test here in the first place.
func ReissueOmittedUnderLockForTest(d *DS, ctx context.Context, groupID id.ID, oldEpoch uint64, applied []mlswasi.AppliedProposal) error {
	unlock := d.lock(groupID)
	defer unlock()
	return d.reissueOmitted(ctx, groupID, oldEpoch, applied)
}

// ReissueAllUnderLockForTest runs `reissueAll` exactly as `resyncLocked` runs it, with the group
// lock HELD. It is a seam for the same reason ReissueOmittedUnderLockForTest is: `Resync` cannot
// reach its own tail, because no external commit in this repository can be accepted (task 25's
// report §C1), so the one live question about `reissueAll` — whether it fires an election round
// for work it did not re-issue — has no other entry point.
func ReissueAllUnderLockForTest(d *DS, ctx context.Context, groupID id.ID, epoch uint64) error {
	unlock := d.lock(groupID)
	defer unlock()
	return d.reissueAll(ctx, groupID, epoch)
}

// ------------------------------------------------------------------- dilla-media task 9

// ReplaceMembersForTest runs replaceMembersTx for a group of the given kind in one transaction and
// queues its removed devices, exactly as every member-set writer does after its transaction.
func ReplaceMembersForTest(d *DS, ctx context.Context, groupID id.ID, kind uint8, state mlswasi.GroupState, joined ...uint32) error {
	var view memberView
	if err := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		var err error
		view, err = d.replaceMembersTx(ctx, tx, groupID, kind, state, joined...)
		return err
	}); err != nil {
		return err
	}
	d.queueEviction(groupID, view.removed)
	return nil
}

// FlushEvictionsForTest is the flush every member-set entry point defers past its unlock.
func FlushEvictionsForTest(d *DS, ctx context.Context, groupID id.ID) { d.flushEvictions(ctx, groupID) }

// GroupLockFreeForTest reports whether nobody holds groupID's lock right now.
func GroupLockFreeForTest(d *DS, groupID id.ID) bool {
	v, _ := d.groupLocks.LoadOrStore(groupID, &sync.Mutex{})
	mu, _ := v.(*sync.Mutex)
	if mu.TryLock() {
		mu.Unlock()
		return true
	}
	return false
}

// SweepCallProposalsForTest is one pass of the call sweeper.
func SweepCallProposalsForTest(d *DS, ctx context.Context) (int, error) {
	return d.sweepCallProposals(ctx)
}

// MarkCallWorkForTest puts a group in the call sweeper's set, as storeInstanceProposal does.
func MarkCallWorkForTest(d *DS, groupID id.ID) { d.markCallWork(groupID) }

// CallWorkForTest is the call sweeper's set, in no particular order.
func CallWorkForTest(d *DS) []id.ID {
	d.callWorkMu.Lock()
	defer d.callWorkMu.Unlock()
	out := make([]id.ID, 0, len(d.callWork))
	for g := range d.callWork {
		out = append(out, g)
	}
	return out
}

// RedriveCallRemovesForTest is the call re-drive from fromEpoch's voided instance Removes into
// curEpoch, under the group lock as both callers run it. The fixture never advances an epoch, so a
// cross-epoch re-drive — the one commitLocked step (9) runs — is reached through this seam with
// rows a test wrote at an earlier epoch.
func RedriveCallRemovesForTest(d *DS, ctx context.Context, groupID id.ID, fromEpoch, curEpoch uint64) error {
	unlock := d.lock(groupID)
	defer unlock()
	return d.redriveCallRemovesLocked(ctx, groupID, fromEpoch, curEpoch)
}

// GroupLockFree reports whether nobody holds groupID's lock right now.
func (d *DS) GroupLockFree(groupID id.ID) bool {
	v, _ := d.groupLocks.LoadOrStore(groupID, &sync.Mutex{})
	mu, _ := v.(*sync.Mutex)
	if !mu.TryLock() {
		return false
	}
	mu.Unlock()
	return true
}

// StoreInstanceProposalForTest reaches the durable proposal sink with incomplete metadata.
func StoreInstanceProposalForTest(d *DS, ctx context.Context, groupID id.ID, row store.GroupRow, blob []byte, proposal store.ProposalRow) error {
	return d.storeInstanceProposal(ctx, groupID, row, blob, proposal)
}

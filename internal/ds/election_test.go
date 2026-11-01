package ds_test

import (
	"context"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// Every test in this file issues an instance proposal FIRST. RequestCommit returns immediately on
// a group with no outstanding proposals —
//
//	refs, _ := d.refsOf(...); if len(refs) == 0 { d.clearElection(groupID); return nil }
//
// — so calling it on a proposal-free group sends no mls.commit_needed at all, and
// h.expectDeviceFrames would time out on every one of them. h.proposeRemoveOf issues an instance
// Remove, whose storeInstanceProposal already ends in RequestCommit, so the frame under test is
// the one that call produced. `h.groupWithMembers` issues one seed Remove of its own, of a leaf no
// test names, so that TestTheWatchdogNudgesTheNextCandidate — which calls RequestCommit without
// proposing anything — has something to elect for too; see the helper.
//
// TWO TESTS OF THE BRIEF WERE CARRIED. Both were ASSIGNED, in the plan's part-1b deviation table
// and in the SDD workspace rulings:
//
//   - TestTheBackoffWindowIsAdvertisedInHello -> TASK 27a (deviation B23, ruling 44). It is at the
//     end of this file, verbatim from the plan's task 22 step 1, and landed with the `hello`
//     7 -> 9 amendment and the `Backoff`/`BackoffJitter` fields on `gateway.Options`.
//   - TestACommitThatReissuesAnOmittedRemoveDoesNotDeadlock -> TASK 25 (deviation B24, ruling 45),
//     in the same commit that exempts `checkAppliedProposals`' clause 1 for
//     `o.external && !frozen` (B21) and beside the merged GroupInfo the fixture owes. The comment
//     on `reissueOmitted` (internal/ds/freeze.go) states the property it guards.
//
// Why neither could be written here:
//
//   - TestTheBackoffWindowIsAdvertisedInHello asserts backoff_ms and backoff_jitter_ms as two new
//     elements of the `hello` payload. `hello` is a SEVEN-element frame in protocol/02 §2.3 (line
//     133), in gateway.HelloPayload, in opSpecs[OpHello], in packages/protocol-vectors/src/
//     frames.ts and in the committed protocol/vectors/frames.json the Rust guest's vectors_check
//     reads — adding two elements is a wire amendment across three languages and a wasm rebuild,
//     with no seam for Policy.Backoff to reach the gateway (gateway.Options has no such field).
//     Task 22's Files are three new files in internal/ds; the amendment is not made here.
//   - TestACommitThatReissuesAnOmittedRemoveDoesNotDeadlock needs an EXTERNAL commit the delivery
//     service accepts. commit.go step (6) refuses every commit whose processed.SenderLeaf is nil
//     ("the committer's leaf is unknown, so the GroupInfo's signer cannot be checked") and
//     freeze.go says of reissueOmitted, in terms: "IT CANNOT FIRE UNTIL TASK 25 (deviation B21,
//     ruling 42)". There is also no external-commit blob in testkit/fixtures/ds-1500 and no MLS
//     client in Go to make one. The deadlock it guards is nonetheless real, and the lock-free
//     `proposeRemoveLocked` it depends on is already in place and commented for it.
//
// Both are reported to the reviewer as carried work, with the exact shape each needs.

// Invariant 7: mls.commit_needed goes to the lowest-index online device, bot devices first.
func TestCommitNeededGoesToTheLowestIndexOnlineDeviceBotsFirst(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 3)
	h.online(g.members[0], g.members[1], g.members[2])
	h.setLeaves(g, map[int]uint32{0: 5, 1: 2, 2: 9})
	h.proposeRemoveOf(t, g, 9) // an outstanding instance proposal, and the election it triggers

	h.expectDeviceFrames(t, g.members[1], "mls.commit_needed") // leaf 2 is the lowest
	h.expectNoFrames(t, g.members[0], g.members[2])

	h.markBot(g.members[2])
	if err := h.ds.RequestCommit(context.Background(), g.id); err != nil {
		t.Fatalf("RequestCommit: %v", err)
	}
	h.expectDeviceFrames(t, g.members[2], "mls.commit_needed") // bots first, whatever the leaf
}

// DS-5 of the server-half review: the device an outstanding instance Remove targets can never
// commit it (OpenMLS refuses a commit that removes the committer), so it is never elected. The
// lowest-index online device being removed, the round goes to the next one; when it is the only
// device online, nobody is elected and nothing is armed.
func TestTheElectionSkipsTheDeviceTheInstanceIsRemoving(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	h.online(g.members[0], g.members[1])
	h.setLeaves(g, map[int]uint32{0: g.leafOf(0), 1: g.leafOf(1)})
	h.proposeRemoveOf(t, g, g.leafOf(0)) // members[0] is the lowest leaf and the one being removed

	h.expectDeviceFrames(t, g.members[1], "mls.commit_needed")
	h.expectNoFrames(t, g.members[0])

	h.offline(g.members[1])
	if err := h.ds.RequestCommit(context.Background(), g.id); err != nil {
		t.Fatalf("RequestCommit: %v", err)
	}
	h.expectNoFrames(t, g.members[0])
	if n := h.armedElections(); n != 0 {
		t.Fatalf("%d elections armed with only the removed device online, want 0", n)
	}
}

// deadline_ms is re-based at the writer, not at election time: a frame that waited 400 ms in the
// queue arrives with 400 ms less on its clock.
// The re-base happens when the frame is DEQUEUED, before sink.write, so an idle writer dequeues
// in the same instant the frame is enqueued and `waited` is 0. To make the frame actually wait in
// the queue, a first frame must already be blocking the writer INSIDE sink.write: only then does
// the commit_needed frame sit in the queue while the clock advances. This is the one place D11's
// queue-wait semantics are exercised, so the ordering is spelled out rather than implied.
func TestTheDeadlineIsRebasedAtTheWriter(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 1)
	h.online(g.members[0])

	// (1) a frame that blocks the writer inside write.
	h.stallWriter(g.members[0])
	h.sendFiller(t, g.members[0])
	h.waitWriterBlocked(t, g.members[0])

	// (2) the commit_needed frame, which now queues behind it.
	h.proposeRemoveOf(t, g, 1)

	// (3) the clock advances while it waits, and only then is the writer released.
	h.clk.Advance(400 * time.Millisecond)
	h.releaseWriter(g.members[0])

	h.waitDeviceFrame(t, g.members[0], "filler")
	frame := h.waitDeviceFrame(t, g.members[0], "mls.commit_needed")
	deadline := h.deadlineMSOf(t, frame)
	full := uint64(h.policy().CommitDeadline / time.Millisecond)
	if deadline >= full {
		t.Fatalf("deadline_ms = %d, want less than %d — it is re-based when the frame reaches the writer",
			deadline, full)
	}
}

// An UNacknowledged round does not count against a device; three acknowledged-and-lost rounds do.
// The rounds are read back from the delivery service, never counted by the test: every
// RequestCommit — including the ones RunWatchdogOnce triggers — increments the round, so by the
// second loop the live round is well past 3 and acking 1, 2, 3 acks nothing at all.
func TestOnlyAcknowledgedRoundsCountAgainstADevice(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 1)
	h.online(g.members[0])
	h.proposeRemoveOf(t, g, 1) // something to commit, and the first round

	for range 5 {
		h.clk.Advance(h.policy().WatchdogInterval + time.Second)
		h.ds.RunWatchdogOnce(context.Background())
	}
	if h.removedLeaves(t, g) != 0 {
		t.Fatal("unacknowledged rounds must only advance the election, never remove a device")
	}

	for range 3 {
		round := h.ds.CurrentRound(g.id)
		if round == 0 {
			t.Fatal("no election is armed; RequestCommit found no outstanding proposal")
		}
		if err := h.ds.AckCommitNeeded(context.Background(), g.id, g.members[0], round); err != nil {
			t.Fatalf("AckCommitNeeded: %v", err)
		}
		h.clk.Advance(h.policy().WatchdogInterval + time.Second)
		h.ds.RunWatchdogOnce(context.Background())
	}
	if h.removedLeaves(t, g) != 1 {
		t.Fatal("three acknowledged-and-lost rounds must remove the device by an instance Remove")
	}
}

// failRounds acknowledges the live round as device and lets it lose, n times.
func (h *dsHarness) failRounds(t *testing.T, g *dsGroup, device id.ID, n int) {
	t.Helper()
	for range n {
		round := h.ds.CurrentRound(g.id)
		if round == 0 {
			t.Fatal("no election is armed; the group has nobody left to elect")
		}
		if err := h.ds.AckCommitNeeded(context.Background(), g.id, device, round); err != nil {
			t.Fatalf("AckCommitNeeded: %v", err)
		}
		h.clk.Advance(h.policy().WatchdogInterval + time.Second)
		h.ds.RunWatchdogOnce(context.Background())
	}
}

// keepOnlyMembers rewrites mls_members to the exposed members named, which is the member set the
// delivery service reads: a channel group its creator registered alone, before anybody else's Add
// was committed.
func (h *dsHarness) keepOnlyMembers(t *testing.T, g *dsGroup, keep ...int) {
	t.Helper()
	ctx := context.Background()
	row, err := h.repo.GetGroup(ctx, g.id)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	members, err := h.repo.ListMembers(ctx, g.id)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	var out []store.MemberRow
	for _, m := range members {
		for _, i := range keep {
			if m.DeviceID == g.members[i] {
				out = append(out, m)
			}
		}
	}
	if len(out) != len(keep) {
		t.Fatalf("kept %d members, want %d", len(out), len(keep))
	}
	if err := h.repo.ReplaceMembers(ctx, g.id, row.Epoch, out); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
}

// The watchdog's Remove exists so that ANOTHER member can commit what the failing device did not.
// When the failing device is the group's only member — a channel its creator just registered, whose
// Adds (the community Bot's, say) are the work outstanding — no other member exists: the Remove
// could only be committed by the device it removes, which OpenMLS refuses and the election never
// asks for, so it froze the group for good and every later joiner got 425 E_COMMIT_REQUIRED. The
// sole member is kept and asked again.
func TestTheWatchdogNeverRemovesTheGroupsLastMember(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 1)
	h.keepOnlyMembers(t, g, 0)
	h.online(g.members[0])
	if err := h.ds.RequestCommit(context.Background(), g.id); err != nil {
		t.Fatalf("RequestCommit: %v", err)
	}

	h.failRounds(t, g, g.members[0], h.policy().MaxLostRounds)

	if n := h.removedLeaves(t, g); n != 0 {
		t.Fatalf("the watchdog proposed removing the group's only member (%d Removes)", n)
	}
	if h.ds.CurrentRound(g.id) == 0 {
		t.Fatal("the only member must stay elected while work is outstanding")
	}
}

// The same holds when the other members are all leaving: a device an outstanding instance Remove
// is taking out can commit nothing (it is never elected), so removing the last member that is NOT
// leaving would again leave nobody who can commit either Remove.
func TestTheWatchdogNeverRemovesTheLastMemberThatCouldCommit(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	h.keepOnlyMembers(t, g, 0, 1)
	h.online(g.members[0], g.members[1])
	h.proposeRemoveOf(t, g, g.leafOf(1)) // members[1] is leaving; members[0] is elected

	h.failRounds(t, g, g.members[0], h.policy().MaxLostRounds)

	if n := h.removedLeaves(t, g); n != 1 {
		t.Fatalf("%d Removes of the group's members, want 1 (members[1]'s own)", n)
	}
}

func TestTheWatchdogRemovesAFailingDeviceWithAnOfflineMember(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	h.keepOnlyMembers(t, g, 0, 1)
	h.online(g.members[0])
	if err := h.ds.RequestCommit(context.Background(), g.id); err != nil {
		t.Fatal(err)
	}
	h.failRounds(t, g, g.members[0], h.policy().MaxLostRounds)
	if n := h.removedLeaves(t, g); n != 1 {
		t.Fatalf("%d Removes, want the failing device removed while the other member is offline", n)
	}
}

func TestTheWatchdogKeepsTheLastUnbarredMember(t *testing.T) {
	for _, reason := range []string{"quarantined", "revoked"} {
		t.Run(reason, func(t *testing.T) {
			h := newDSHarness(t)
			g := h.groupWithMembers(t, 2)
			h.keepOnlyMembers(t, g, 0, 1)
			ctx := context.Background()
			h.account(t, h.sessions[g.members[1]].UserID, g.members[1])
			var err error
			if reason == "quarantined" {
				err = h.repo.QuarantineDevice(ctx, g.members[1], h.clk.Now().Unix(), "fork quorum")
			} else {
				err = h.repo.RevokeDevice(ctx, g.members[1], h.clk.Now().Unix())
			}
			if err != nil {
				t.Fatal(err)
			}
			h.online(g.members[0])
			if err := h.ds.RequestCommit(ctx, g.id); err != nil {
				t.Fatal(err)
			}
			h.failRounds(t, g, g.members[0], h.policy().MaxLostRounds)
			if n := h.removedLeaves(t, g); n != 0 {
				t.Fatalf("the watchdog removed the last unbarred member (%d Removes)", n)
			}
		})
	}
}

func TestKeptCommitterBackoffDoublesAndCaps(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 1)
	h.keepOnlyMembers(t, g, 0)
	h.online(g.members[0])
	ctx := context.Background()
	if err := h.ds.RequestCommit(ctx, g.id); err != nil {
		t.Fatal(err)
	}
	for i, delay := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 60 * time.Second, 60 * time.Second} {
		h.offline(g.members[0])
		h.online(g.members[0])
		h.failRounds(t, g, g.members[0], h.policy().MaxLostRounds-1)
		round := h.ds.CurrentRound(g.id)
		if err := h.ds.AckCommitNeeded(ctx, g.id, g.members[0], round); err != nil {
			t.Fatal(err)
		}
		h.clk.Advance(h.policy().WatchdogInterval)
		h.ds.RunWatchdogOnce(ctx)
		if got := h.ds.CurrentRound(g.id); got != round {
			t.Fatalf("kept cycle %d re-elected immediately: round %d, want %d", i, got, round)
		}
		var metric dto.Metric
		if err := h.metrics.ElectionRounds.WithLabelValues("kept").Write(&metric); err != nil {
			t.Fatal(err)
		}
		if got := metric.GetCounter().GetValue(); got != float64(i+1) {
			t.Fatalf("kept rounds = %v, want %d", got, i+1)
		}
		h.clk.Advance(delay - time.Nanosecond)
		h.ds.RunWatchdogOnce(ctx)
		// A new proposal or ready must not bypass the backoff either.
		if err := h.ds.RequestCommit(ctx, g.id); err != nil {
			t.Fatal(err)
		}
		if got := h.ds.CurrentRound(g.id); got != round {
			t.Fatalf("cycle %d re-elected before %v: round %d, want %d", i, delay, got, round)
		}
		h.clk.Advance(time.Nanosecond)
		h.ds.RunWatchdogOnce(ctx)
		if got := h.ds.CurrentRound(g.id); got != round+1 {
			t.Fatalf("cycle %d did not re-elect after %v: round %d, want %d", i, delay, got, round+1)
		}
	}
}

func TestKeptCommitterBackoffResetsOnEpochAdvance(t *testing.T) {
	for _, trigger := range []string{"watchdog", "proposal"} {
		t.Run(trigger, func(t *testing.T) {
			h := newDSHarness(t)
			g := h.groupWithMembers(t, 1)
			h.keepOnlyMembers(t, g, 0)
			h.online(g.members[0])
			ctx := context.Background()
			if err := h.ds.RequestCommit(ctx, g.id); err != nil {
				t.Fatal(err)
			}
			h.failRounds(t, g, g.members[0], h.policy().MaxLostRounds)
			h.clk.Advance(2 * time.Second)
			h.ds.RunWatchdogOnce(ctx)
			h.failRounds(t, g, g.members[0], h.policy().MaxLostRounds)
			epoch := h.advanceGroupEpoch(t, g.id)
			h.putDSProposal(t, g.id, epoch, false)
			if trigger == "watchdog" {
				h.ds.RunWatchdogOnce(ctx)
			} else if err := h.ds.RequestCommit(ctx, g.id); err != nil {
				t.Fatal(err)
			}
			round := h.ds.CurrentRound(g.id)
			h.clk.Advance(h.policy().WatchdogInterval)
			h.ds.RunWatchdogOnce(ctx)
			if got := h.ds.CurrentRound(g.id); got != round+1 {
				t.Fatalf("epoch advance kept the old backoff: round %d, want %d", got, round+1)
			}
			h.failRounds(t, g, g.members[0], h.policy().MaxLostRounds)
			round = h.ds.CurrentRound(g.id)
			h.clk.Advance(2 * time.Second)
			h.ds.RunWatchdogOnce(ctx)
			if got := h.ds.CurrentRound(g.id); got != round+1 {
				t.Fatalf("first kept delay after epoch advance did not reset to 2s: round %d, want %d", got, round+1)
			}
		})
	}
}

// The watchdog nudges the next candidate after 2 s; the others back off 300 ms + rand(0..300 ms).
func TestTheWatchdogNudgesTheNextCandidate(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	h.online(g.members[0], g.members[1])
	h.setLeaves(g, map[int]uint32{0: 1, 1: 2})

	if err := h.ds.RequestCommit(context.Background(), g.id); err != nil {
		t.Fatalf("RequestCommit: %v", err)
	}
	h.expectDeviceFrames(t, g.members[0], "mls.commit_needed")
	h.clk.Advance(h.policy().WatchdogInterval + time.Second)
	h.ds.RunWatchdogOnce(context.Background())
	h.expectDeviceFrames(t, g.members[1], "mls.commit_needed")
}

// A group with nobody online arms no timer and re-elects on the next ready.
func TestAGroupWithNobodyOnlineArmsNoTimer(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 1)
	h.offline(g.members[0])
	h.proposeRemoveOf(t, g, 1) // an outstanding proposal, so the election has something to elect for

	if h.armedElections() != 0 {
		t.Fatal("an election with no candidate must arm no timer")
	}
	h.online(g.members[0])
	if err := h.ds.RequestCommit(context.Background(), g.id); err != nil {
		t.Fatalf("RequestCommit: %v", err)
	}
	h.expectDeviceFrames(t, g.members[0], "mls.commit_needed")
}

// ------------------------------------------------- one election per BATCH, not per proposal

// Invariant 7 elects ONE device and has the others back off. An operation that issues many
// instance proposals therefore holds ONE election, when they are all durable — not one per
// proposal, which walks beginRound's rotation once per device and tells several of them,
// concurrently, that each is the committer.
//
// The window is driven directly here because the batch that motivates it — ProposeAddBatch's 256
// Adds — cannot issue two real Adds from committed material: testkit/fixtures/ds-1500 ships ONE
// KeyPackage, every Add built from it is byte for byte the same external proposal with the same
// ref, and the second row of a two-device batch is refused by mls_pending_proposals' primary key
// (verified: "UNIQUE constraint failed: mls_pending_proposals.group_id, …ref"). Two Removes of two
// leaves are two real proposals. TestProposeAddBatchElectsOnceForTheWholeBatch below pins that
// ProposeAddBatch is a caller that opens the window.
func TestManyInstanceProposalsInOneWindowElectOnce(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 3)
	h.online(g.members[0], g.members[1], g.members[2])
	h.setLeaves(g, map[int]uint32{0: 1, 1: 2, 2: 3})

	release := ds.SuppressElectionsForTest(h.ds, g.id)
	h.proposeRemoveOf(t, g, g.leaves[1])
	h.proposeRemoveOf(t, g, g.leaves[2])
	release()
	if err := h.ds.RequestCommit(context.Background(), g.id); err != nil {
		t.Fatalf("RequestCommit: %v", err)
	}

	if got := h.ds.CurrentRound(g.id); got != 1 {
		t.Fatalf("two proposals in one window held %d rounds, want exactly 1", got)
	}
	// And the one frame goes to the lowest-index online device, which a per-proposal election
	// would have rotated past.
	h.expectExactlyOneCommitNeeded(t, g.members[0], g.members[1], g.members[2])
}

// ProposeAddBatch is a batch: every proposal of it is written with the election window open, and
// the batch elects once afterwards.
func TestProposeAddBatchElectsOnceForTheWholeBatch(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.groupWithMembers(t, 2)
	h.online(g.members[0], g.members[1])
	h.setLeaves(g, map[int]uint32{0: 1, 1: 2})

	// The channel ACL admits the joiner: a batch proposes no device its commit would be refused for.
	joiner := h.eligibleDeviceWithKeyPackage(t)

	rows, inWindow := 0, 0
	h.repo.observeProposals(func(store.ProposalRow) {
		rows++
		if ds.ElectionSuppressedForTest(h.ds, g.id) {
			inWindow++
		}
	})
	// The second device has no KeyPackage, so the batch skips it and keeps going: what is under
	// test is the loop, not the arithmetic.
	if err := h.ds.ProposeAddBatch(ctx, g.id, []id.ID{joiner, id.New()}); err != nil {
		t.Fatalf("ProposeAddBatch: %v", err)
	}
	h.repo.observeProposals(nil)

	if rows == 0 {
		t.Fatal("the batch issued no proposal at all; the assertions below would be vacuous")
	}
	if inWindow != rows {
		t.Fatalf("%d of the batch's %d proposals were written inside the election window, want all "+
			"of them: a batch that elects per proposal nominates a different device each time",
			inWindow, rows)
	}
	if got := h.ds.CurrentRound(g.id); got != 1 {
		t.Fatalf("the batch held %d rounds, want exactly 1", got)
	}
	h.expectExactlyOneCommitNeeded(t, g.members[0], g.members[1])
}

// ------------------------------------------------- a round that was WON is not a round lost

// A device whose commit was ACCEPTED is never charged a lost round. Three charged rounds remove a
// device from the group by an instance Remove, so a stale charge against the one device that did
// exactly what it was told removes the group's best committer.
//
// The accepted commit is staged as the epoch move it leaves behind: no commit in this package can
// be accepted (the fixture ships one GroupInfo, at epoch 6, and invariant 4 wants epoch n+1 — the
// blocker that moved TestAnAcceptedCommitFansOutHandshakeEpochChangedAndWelcomes to
// internal/testkit/accepted_test.go, where real clients drive the instance), and the epoch
// is what the watchdog reads. The commit path's own half of the rule is the clearElection at
// commit step (8b); this is the half that covers the window in which the watchdog's tick beats it.
func TestADeviceWhoseCommitLandedIsNeverChargedALostRound(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.groupWithMembers(t, 1)
	h.online(g.members[0])

	if err := h.ds.RequestCommit(ctx, g.id); err != nil {
		t.Fatalf("RequestCommit: %v", err)
	}
	round := h.ds.CurrentRound(g.id)
	if round == 0 {
		t.Fatal("no election is armed; RequestCommit found no outstanding proposal")
	}
	if err := h.ds.AckCommitNeeded(ctx, g.id, g.members[0], round); err != nil {
		t.Fatalf("AckCommitNeeded: %v", err)
	}

	// The commit lands. The next proposal is already in SQL at the NEW epoch when the tick
	// arrives — the window between its row and its own RequestCommit, which spans DeliverGroup's
	// fan-out to every member of the group — so the election is not cleared by an empty ref set.
	epoch := h.advanceGroupEpoch(t, g.id)
	h.putDSProposal(t, g.id, epoch, false)

	h.clk.Advance(h.policy().WatchdogInterval + time.Second)
	h.ds.RunWatchdogOnce(ctx)

	if got := ds.LostRoundsForTest(h.ds, g.id, g.members[0]); got != 0 {
		t.Fatalf("the committer was charged %d lost rounds; its commit was ACCEPTED", got)
	}
	if h.removedLeaves(t, g) != 0 {
		t.Fatal("a device whose commit landed was proposed for removal")
	}
}

// ------------------------------------------------- the background loops actually run

// Start runs the watchdog and the sweeper, and Shutdown drains both. Nothing in the module calls
// DS.Start before the composition root (task 27a), so without this test runWatchdog, runSweeper,
// the wg.Add(2) and Shutdown's wait are entirely unexercised — and the ticker they arm panics, in
// a goroutine, on any non-positive interval.
func TestStartRunsTheWatchdogAndSweeperAndShutdownEndsThem(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()

	if err := h.ds.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stop, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := h.ds.Shutdown(stop); err != nil {
		t.Fatalf("Shutdown: %v — Start's two goroutines did not drain", err)
	}

	// A config-derived Policy is filled field by field, and a partially filled one is exactly what
	// task 27a can hand New. A zero WatchdogInterval must never reach time.NewTicker: the panic it
	// raises is inside Start's goroutine, unrecoverable, and takes the process down at startup.
	partial, err := ds.New(ds.Options{
		Store: h.repo, Wasm: h.wasm, Gateway: h.gw, Clock: h.clk,
		Keys: testInstanceKeys(t), Channels: h.channels,
		Policy: ds.Policy{MaxCiphertextBytes: 131072},
	})
	if err != nil {
		t.Fatalf("ds.New: %v", err)
	}
	p := ds.PolicyForTest(partial)
	if p.WatchdogInterval <= 0 || p.MaxLostRounds <= 0 || p.CommitDeadline <= 0 {
		t.Fatalf("New kept the zeros of a partially filled Policy: watchdog %v, lost rounds %d, "+
			"deadline %v", p.WatchdogInterval, p.MaxLostRounds, p.CommitDeadline)
	}
	if err := partial.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	drain, cancelDrain := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDrain()
	if err := partial.Shutdown(drain); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// The back-off the other candidates observe is advertised once, in hello, and is not a second
// frame: protocol/02 invariant 7 says "other devices back off 300 ms + random(0..300 ms)", and the
// instance only ever addresses the chosen device. Policy.Backoff and Policy.BackoffJitter are
// therefore read HERE, into the gateway's hello payload, and nowhere else; a client that never
// receives mls.commit_needed waits that long before volunteering.
func TestTheBackoffWindowIsAdvertisedInHello(t *testing.T) {
	h := newDSHarness(t)
	backoff, jitter := h.helloBackoff(t)
	if backoff != uint64(h.policy().Backoff/time.Millisecond) {
		t.Errorf("hello advertises backoff_ms = %d, want %d", backoff, h.policy().Backoff/time.Millisecond)
	}
	if jitter != uint64(h.policy().BackoffJitter/time.Millisecond) {
		t.Errorf("hello advertises backoff_jitter_ms = %d, want %d",
			jitter, h.policy().BackoffJitter/time.Millisecond)
	}
}

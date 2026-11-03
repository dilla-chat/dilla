package ds_test

// resync_test.go is R25: the own-leaf external commit that brings a device which has fallen out of
// the epoch back, exempt from invariant 5's freeze under two guards.
//
// DEVIATION FROM THE BRIEF'S STEP 1 (recorded in the task-25 report). The brief's first and third
// tests drive an ACCEPTED external commit through `h.resyncOf` / `h.resyncRemovingLeaf`. Neither
// fixture nor ABI can produce one:
//
//   - `testkit/fixtures/ds-1500` ships exactly one GroupInfo, at epoch 6, and invariant 4's step
//     (6) wants epoch n+1 — the blocker `commit_test.go` records in two skips and the plan in
//     deviations B21 and B24.
//   - the fixture holds no external commit at all. (Task 27a closed the ABI half, deviation
//     B33: `mlswasi.Processed.NewLeaf` names the joiner's leaf and `ValidateStagedGroupInfo`
//     checks its GroupInfo under the key its commit brings, so commit step (6) is satisfiable
//     on the external path; `core/dilla-core-wasi`'s
//     `an_external_commit_reports_the_leaf_the_joiner_lands_on` drives a real external commit
//     through both. What is still missing is external-commit material a Go test can hold,
//     which Ruling C assigns to task 29's harness.)
//
// So everything R25 states that IS reachable from here is asserted for real against the real store
// and the real guest: guard 1 in full, the freeze exemption in full, and guard 2 over the applied
// list its own clause is defined on. The happy path — an accepted resync during a freeze, and the
// re-issue after it — shipped here as a skip and is now task 29's harness-driven
// TestAResyncSucceedsDuringAFreezeAndReissuesTheProposals in internal/testkit/accepted_test.go,
// where real dilla-core clients produce the external commit (Ruling C(5)).

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

// The freeze exemption itself, without an acceptable commit: a resync during a live freeze is NOT
// refused with E_COMMIT_REQUIRED. It is refused later, by the parse, and that is the whole point —
// `commitOptions.skipFreeze` carried R25 past invariant 5's clause. `Frozen` is asserted true
// first, so a green run cannot be a group that was never frozen.
func TestAResyncIsExemptFromTheFreeze(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	h.online(g.members[0], g.members[1])
	if frozen, err := h.ds.Frozen(context.Background(), g.id); err != nil || !frozen {
		t.Fatalf("Frozen = %v, %v; the group must be frozen for this test to mean anything", frozen, err)
	}

	_, err := h.ds.Resync(context.Background(), g.sessionOf(0), g.id, ds.ResyncRequest{
		ExternalCommit: []byte{0x00, 0x01, 0x02},
		GroupInfo:      []byte{0x03},
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) {
		t.Fatalf("got %v, want a *ds.Error", err)
	}
	if dsErr.Code == "E_COMMIT_REQUIRED" {
		t.Fatal("a resync must be exempt from the freeze, not refused by it")
	}
	if dsErr.Code != "E_COMMIT_INVALID" {
		t.Fatalf("got %s, want E_COMMIT_INVALID from the parse", dsErr.Code)
	}
}

// Guard 1: a resync by the target of an outstanding non-void Remove is 409 E_REMOVE_PENDING ("you are being
// removed"; its own code since the task 10 fix round, so a call client waits on exactly this refusal).
func TestAResyncByTheTargetOfAnOutstandingRemoveIsForbidden(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	if err := h.ds.ProposeRemove(context.Background(), g.id, g.leafOf(1), id.New()); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	_, err := h.ds.Resync(context.Background(), g.sessionOf(1), g.id, ds.ResyncRequest{
		ExternalCommit: []byte{0x00},
		GroupInfo:      []byte{0x01},
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_REMOVE_PENDING" || dsErr.Status != http.StatusConflict {
		t.Fatalf("got %v, want 409 E_REMOVE_PENDING", err)
	}
}

// Guard 1 holds in a call group too, also when the device's own leave preceded the instance's
// Remove and its user is eligible (web-3 task 10, concern 2; the security review of e619526 and
// dd88a47): a member Remove of the leaf proves nothing about why the instance removes it — any
// device of the user may post one at any time of the epoch, before an eviction — so no exception
// is made, and the rejoin is the client's to wait for (calls/join.ts).
func TestACallGroupRejoinWhileItsOwnLeaveIsOutstandingIsForbidden(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	h.repo.markCall(g.id)
	h.putMemberRemove(t, g.id, g.leafOf(1), 30)
	if err := h.ds.ProposeRemove(context.Background(), g.id, g.leafOf(1), id.New()); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	_, err := h.ds.Resync(context.Background(), g.sessionOf(1), g.id, ds.ResyncRequest{
		ExternalCommit: []byte{0x00},
		GroupInfo:      []byte{0x01},
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_REMOVE_PENDING" || dsErr.Status != http.StatusConflict {
		t.Fatalf("got %v, want 409 E_REMOVE_PENDING", err)
	}
}

// …and never for an eviction either: an instance Remove the device did not ask for (the cut of a
// device whose media was not dilla's, the room sweep's absent device, the fork watchdog's
// committer) keeps the device out until a member commits it, while its user is eligible (the
// security review of e619526; re-added by the task 10 fix round, f10app m5).
func TestACallGroupRejoinByTheTargetOfAnEvictionIsForbidden(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	h.repo.markCall(g.id)
	if err := h.ds.ProposeRemove(context.Background(), g.id, g.leafOf(1), id.New()); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	_, err := h.ds.Resync(context.Background(), g.sessionOf(1), g.id, ds.ResyncRequest{
		ExternalCommit: []byte{0x00},
		GroupInfo:      []byte{0x01},
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_REMOVE_PENDING" || dsErr.Status != http.StatusConflict {
		t.Fatalf("got %v, want 409 E_REMOVE_PENDING: an eviction the device did not ask for is never superseded", err)
	}
}

// …and a device that is NOT the target is not refused by guard 1: the same group, the same
// outstanding Remove, a different session.
func TestAResyncByADeviceThatIsNotTheRemoveTargetPassesTheGuard(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	if err := h.ds.ProposeRemove(context.Background(), g.id, g.leafOf(1), id.New()); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	_, err := h.ds.Resync(context.Background(), g.sessionOf(0), g.id, ds.ResyncRequest{
		ExternalCommit: []byte{0x00},
		GroupInfo:      []byte{0x01},
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" {
		t.Fatalf("got %v, want the parse's E_COMMIT_INVALID, not guard 1's refusal", err)
	}
}

// Guard 2: an external commit whose inner Remove targets a leaf that is not the joiner's own
// device is refused.
//
// The clause is asserted over the applied list it is defined on rather than over an accepted
// external commit, for the reason this file's header gives.
func TestAnExternalCommitRemovingSomebodyElseIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	other := g.leafOf(1)
	err := ds.CheckExternalCommitScopeForTest(h.ds, context.Background(), g.id, g.sessionOf(0),
		[]mlswasi.AppliedProposal{{Kind: mlswasi.ProposalRemove, TargetLeaf: &other}})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" {
		t.Fatalf("got %v, want E_COMMIT_INVALID", err)
	}
	if dsErr.Rule != "external_commit_remove_scope" {
		t.Fatalf("rule = %q, want external_commit_remove_scope", dsErr.Rule)
	}
}

// …and the joiner's OWN previous leaf is exactly what R25 allows it to remove.
func TestAnExternalCommitRemovingTheJoinersOwnLeafIsAccepted(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	own := g.leafOf(0)
	if err := ds.CheckExternalCommitScopeForTest(h.ds, context.Background(), g.id, g.sessionOf(0),
		[]mlswasi.AppliedProposal{{Kind: mlswasi.ProposalRemove, TargetLeaf: &own}}); err != nil {
		t.Fatalf("an external commit may remove the joiner's own previous leaf: %v", err)
	}
}

// One device, one leaf (finding G1 of the second hardening review): an external commit by a device
// that already holds a leaf must remove that leaf. A commit that removes nothing would leave the
// device with two leaves, the new one under whatever credential it carried. The device's leaves are
// read from mls_members, the tree's own credentials, never from the request. Asserted over the
// applied list, like guard 2 above; the core's public group refuses the same shape first (the
// commit's Process), and that refusal is mapped to the same rule.
func TestAnExternalCommitThatLeavesTheJoinersOwnLeafInPlaceIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	other := g.leafOf(1)
	for name, applied := range map[string][]mlswasi.AppliedProposal{
		"no Remove at all":            nil,
		"a Remove of another leaf":    {{Kind: mlswasi.ProposalRemove, TargetLeaf: &other}},
		"only the ExternalInit kinds": {{Kind: mlswasi.ProposalExternalInit}},
	} {
		err := ds.CheckExternalCommitReplacesOwnLeafForTest(h.ds, context.Background(), g.id, g.sessionOf(0), applied)
		var dsErr *ds.Error
		if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" || dsErr.Rule != "external_joiner" {
			t.Fatalf("%s: got %v, want E_COMMIT_INVALID/external_joiner", name, err)
		}
	}
	// The honest resync removes the device's own leaf, and a device that holds no leaf (a first
	// join) has nothing to remove.
	own := g.leafOf(0)
	if err := ds.CheckExternalCommitReplacesOwnLeafForTest(h.ds, context.Background(), g.id, g.sessionOf(0),
		[]mlswasi.AppliedProposal{{Kind: mlswasi.ProposalRemove, TargetLeaf: &own}}); err != nil {
		t.Fatalf("a resync that removes the device's own leaf: %v", err)
	}
	stranger := ds.Session{UserID: id.New(), DeviceID: id.New()}
	if err := ds.CheckExternalCommitReplacesOwnLeafForTest(h.ds, context.Background(), g.id, stranger, nil); err != nil {
		t.Fatalf("a first join removes nothing: %v", err)
	}
}

// `reissueAll` elects a committer only when it actually re-issued something.
//
// On the resync path `commitLocked` step (9) has already run `reissueOmitted`, which replaces the
// outstanding rows at the NEW epoch and whose every re-issue goes through `storeInstanceProposal`
// -> `RequestCommit`. `reissueAll` then lists at `epoch-1`, finds nothing — and an unconditional
// `RequestCommit` at that point is not a no-op: `beginRound` ADVANCES the round and picks the next
// untried candidate, so the resync fires one extra election round that skips past the lowest-index
// online device invariant 7 names and invalidates the round the just-elected candidate was told to
// ack.
func TestAResyncsReissueDoesNotFireAnElectionRoundForWorkItDidNotReissue(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	h.online(g.members[0], g.members[1])
	if err := h.ds.ProposeRemove(context.Background(), g.id, g.leafOf(1), id.New()); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	round := h.ds.CurrentRound(g.id)
	if round == 0 {
		t.Fatal("the instance Remove must have armed an election for this test to mean anything")
	}

	// The epoch BELOW the group's own holds no outstanding proposal, which is the shape step (9)
	// leaves behind on every ordinary resync.
	if err := ds.ReissueAllUnderLockForTest(h.ds, context.Background(), g.id, g.epoch(t)); err != nil {
		t.Fatalf("reissueAll: %v", err)
	}
	if got := h.ds.CurrentRound(g.id); got != round {
		t.Fatalf("round = %d after re-issuing nothing, want %d: the resync fired a spurious election round", got, round)
	}
}

// …and when it DOES re-issue, the new epoch's work gets a committer: the belt-and-braces half of
// the loop is still wired to an election.
func TestAResyncsReissueElectsACommitterForWhatItReissued(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	h.online(g.members[0], g.members[1])
	if err := h.ds.ProposeRemove(context.Background(), g.id, g.leafOf(1), id.New()); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	round := h.ds.CurrentRound(g.id)

	// One epoch ABOVE the group's own, so the list at `epoch-1` is the outstanding work itself.
	if err := ds.ReissueAllUnderLockForTest(h.ds, context.Background(), g.id, g.epoch(t)+1); err != nil {
		t.Fatalf("reissueAll: %v", err)
	}
	if got := h.ds.CurrentRound(g.id); got <= round {
		t.Fatalf("round = %d after re-issuing, want above %d", got, round)
	}
}

// Deviation B24 / ruling 45, carried here from task 22: `reissueOmitted` runs with the group lock
// HELD and outside `withGroup`, and everything it reaches must therefore be lock-free. The timeout
// is the assertion — if any path from `reissue` through `storeInstanceProposal` to
// `RequestCommit` ever takes `d.lock(groupID)` again, this hangs.
//
// It is driven through the same lock-held entry the commit path uses rather than through an
// accepted commit, for the reason this file's header gives: no commit in this repository can be
// accepted, which is exactly why task 22 could not write it.
func TestACommitThatReissuesAnOmittedRemoveDoesNotDeadlock(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 2)
	if err := h.ds.ProposeRemove(context.Background(), g.id, g.leafOf(1), id.New()); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		// Nothing applied: every outstanding instance proposal was omitted, which is the shape
		// invariant 5's nobody-online exception leaves behind.
		done <- ds.ReissueOmittedUnderLockForTest(h.ds, context.Background(), g.id, g.epoch(t), nil)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reissueOmitted under the group lock: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("re-issuing an omitted Remove under the group lock deadlocked")
	}
}

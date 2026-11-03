package ds_test

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

// C3 (fix wave): a kick, ban or leave issues its Removes after the change commits, and a Remove
// that never got issued (a crash, a fault in the middle of the loop) has nothing else to re-drive
// it: the ACL only gates Adds and joins, and the inactivity sweep skips active devices. So the
// sweeper reconciles: every live leaf of an open text or call group whose user the ACL no longer
// admits gets an instance Remove, once, and an eligible user's leaves are left alone.
// The reconcile asks about every leaf holder of every open channel group each sweep. Answered one
// user at a time, an ACL that reads the group's members per question (ds.DenyUnlessMember, which
// api.ResolverACL falls back to for groups it has no rule for) costs O(leaves²) store reads a
// group: on the 1,500-leaf fixture that was ~7 s of every -race sweep, most of internal/ds's run
// time. An ACL that implements ds.BatchACL is asked once per group instead.
func TestTheReconcileAsksABatchACLOncePerGroup(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	h.mustRegister(t)
	singles, batches := h.acl.questions()
	if _, err := ds.ReconcileLeavesForTest(h.ds, ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	s, b := h.acl.questions()
	if s-singles != 0 || b-batches != 1 {
		t.Fatalf("the reconcile asked %d single questions and %d batches, want 0 and 1", s-singles, b-batches)
	}
}

// DenyUnlessMember's batch answer is its single answer for each user: a live leaf holder is
// eligible, anyone else is not.
func TestDenyUnlessMemberAnswersABatchLikeItsSingles(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	member := h.memberSession(t, reg.GroupID, 5).UserID
	stranger := id.New()
	acl := ds.DenyUnlessMember{Store: h.repo}
	got, err := acl.EligibleUsers(ctx, reg.GroupID, []id.ID{member, stranger})
	if err != nil {
		t.Fatalf("EligibleUsers: %v", err)
	}
	for _, u := range []id.ID{member, stranger} {
		single, err := acl.Eligible(ctx, reg.GroupID, u)
		if err != nil {
			t.Fatal(err)
		}
		if got[u] != single {
			t.Errorf("user %s: batch %t, single %t", u, got[u], single)
		}
	}
	if !got[member] || got[stranger] {
		t.Fatalf("batch = %v, want the member eligible and the stranger not", got)
	}
}

func TestTheSweeperRemovesTheLeavesOfAUserWhoLostAccess(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	banned := h.memberSession(t, reg.GroupID, 5).UserID
	h.acl.forbid(banned)

	members, err := h.repo.ListMembers(ctx, reg.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	want := map[uint32]bool{}
	for _, m := range members {
		if m.UserID == banned && m.RemovedEpoch == nil {
			want[m.LeafIndex] = true
		}
	}

	n, err := ds.ReconcileLeavesForTest(h.ds, ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != len(want) {
		t.Fatalf("reconcile proposed %d Removes, want %d (one per live leaf of the banned user)", n, len(want))
	}
	rows, err := h.repo.ListProposals(ctx, reg.GroupID, 6, false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	got := map[uint32]bool{}
	for _, r := range rows {
		if mlswasi.ProposalKind(r.Kind) != mlswasi.ProposalRemove || r.TargetLeaf == nil {
			t.Fatalf("reconcile proposed something other than a Remove: %+v", r)
		}
		if !want[*r.TargetLeaf] {
			t.Fatalf("reconcile removed leaf %d, which is not the banned user's", *r.TargetLeaf)
		}
		got[*r.TargetLeaf] = true
	}
	if len(got) != len(want) {
		t.Fatalf("Removes for leaves %v, want %v", got, want)
	}

	// A second pass finds the Removes outstanding and issues nothing more.
	if n, err := ds.ReconcileLeavesForTest(h.ds, ctx); err != nil || n != 0 {
		t.Fatalf("a second reconcile proposed %d (%v), want 0", n, err)
	}
}

// A group in which the ACL admits nobody at all (the leftover of a deleted channel whose Close was
// lost) is not emptied leaf by leaf: nobody could commit those Removes.
func TestTheSweeperLeavesAGroupWithNoEligibleMemberAlone(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	members, err := h.repo.ListMembers(ctx, reg.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	seen := map[id.ID]bool{}
	for _, m := range members {
		if !seen[m.UserID] {
			seen[m.UserID] = true
			h.acl.forbid(m.UserID)
		}
	}
	if n, err := ds.ReconcileLeavesForTest(h.ds, ctx); err != nil || n != 0 {
		t.Fatalf("reconcile over a group nobody may be in proposed %d (%v), want 0", n, err)
	}
}

// D(iii): a member the ACL no longer admits is removed by the instance even while its own
// self-Remove is outstanding. A member Remove is not mandatory for a commit, freezes nothing and
// elects nobody, so letting it stand in for the instance's would let a banned member keep it posted
// and stay in the group. The instance's Remove is issued on top of it: OpenMLS keeps the later of
// two Removes of one leaf, so the commit applies the instance's and leaves the member's unreferenced.
func TestTheSweeperRemovesAnIneligibleMemberEvenWhileItsOwnRemoveStands(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	banned := h.memberSession(t, reg.GroupID, 5).UserID
	h.acl.forbid(banned)

	members, err := h.repo.ListMembers(ctx, reg.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	want := map[uint32]bool{}
	for _, m := range members {
		if m.UserID == banned && m.RemovedEpoch == nil {
			want[m.LeafIndex] = true
		}
	}
	h.putMemberRemove(t, reg.GroupID, 5, 86400) // leaf 5 keeps its own Remove posted

	n, err := ds.ReconcileLeavesForTest(h.ds, ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != len(want) {
		t.Fatalf("reconcile proposed %d Removes, want %d (one for every leaf of the banned user)", n, len(want))
	}
	if got := h.instanceRemovesOf(t, reg.GroupID, 5); len(got) != 1 {
		t.Fatalf("%d instance Removes of leaf 5, want 1: its own Remove stood in for the instance's", len(got))
	}
}

// R-2 of the DS re-review: a quarantined or revoked device that still holds a leaf is owed an
// instance Remove, and the reconcile is its durable source. A fork quorum fires once, and nothing
// else proposes a revoked device's removal, so a Remove that was lost — a heal's carry that failed,
// a crash between the heal and the carry, a row deleted by hand — is proposed again by the next
// pass, bound to the device, whatever the ACL says of its user.
func TestTheReconcileReProposesTheRemoveOfAQuarantinedOrRevokedLeaf(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	quarantined, revoked := h.memberSession(t, reg.GroupID, 1), h.memberSession(t, reg.GroupID, 2)
	h.account(t, quarantined.UserID, quarantined.DeviceID)
	h.account(t, revoked.UserID, revoked.DeviceID)
	if err := h.repo.QuarantineDevice(ctx, quarantined.DeviceID, h.clk.Now().Unix(), "fork quorum"); err != nil {
		t.Fatalf("QuarantineDevice: %v", err)
	}
	if err := h.repo.RevokeDevice(ctx, revoked.DeviceID, h.clk.Now().Unix()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if _, err := ds.ReconcileLeavesForTest(h.ds, ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for leaf, dev := range map[uint32]id.ID{1: quarantined.DeviceID, 2: revoked.DeviceID} {
		got := h.instanceRemovesOf(t, reg.GroupID, leaf)
		if len(got) != 1 || got[0].TargetDevice == nil || *got[0].TargetDevice != dev {
			t.Fatalf("instance Removes of leaf %d: %+v, want one bound to its barred device", leaf, got)
		}
	}
	// A second pass stacks nothing on the outstanding Removes.
	if n, err := ds.ReconcileLeavesForTest(h.ds, ctx); err != nil || n != 0 {
		t.Fatalf("the second pass proposed %d (err %v), want 0", n, err)
	}
}

// A device that is quarantined but no longer holds a leaf is owed nothing.
func TestTheReconcileProposesNothingForAQuarantinedDeviceWithNoLeaf(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	gone := h.memberSession(t, reg.GroupID, 1)
	h.account(t, gone.UserID, gone.DeviceID)
	if err := h.repo.QuarantineDevice(ctx, gone.DeviceID, h.clk.Now().Unix(), "fork quorum"); err != nil {
		t.Fatalf("QuarantineDevice: %v", err)
	}
	h.dropMember(t, reg.GroupID, 1)
	if n, err := ds.ReconcileLeavesForTest(h.ds, ctx); err != nil || n != 0 {
		t.Fatalf("the reconcile proposed %d (err %v), want 0", n, err)
	}
	if rows := h.proposals(t, reg.GroupID, true); len(rows) != 0 {
		t.Fatalf("proposals %+v, want none", rows)
	}
}

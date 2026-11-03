package ds_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// The security fix to dilla-media task 9 (task-9-security-fix-report.md): a member-originated
// proposal never cancels, voids, blocks or stands in for an instance proposal. A member being
// kicked, banned or evicted who keeps a self-Remove outstanding must not keep the instance's Remove
// from being issued, from being mandatory, or from freezing the group — a member Remove is not
// mandatory for a commit (invariant 4 clause 1 covers instance proposals only), freezes nothing and
// triggers no election, so standing in for the instance's is staying in the group.

// instanceRemovesOf is every non-void instance Remove of leaf at the fixture epoch.
func (h *dsHarness) instanceRemovesOf(t *testing.T, groupID id.ID, leaf uint32) []store.ProposalRow {
	t.Helper()
	var out []store.ProposalRow
	for _, r := range h.proposals(t, groupID, false) {
		if r.Origin == 0 && r.VoidAt == nil && mlswasi.ProposalKind(r.Kind) == mlswasi.ProposalRemove &&
			r.TargetLeaf != nil && *r.TargetLeaf == leaf {
			out = append(out, r)
		}
	}
	return out
}

// D(i): with an instance Remove of leaf L outstanding, the member at L posting its own Remove of L
// is refused — 409 E_REMOVE_PENDING, which the client branches on — and taken back out of
// the guest's queue; the instance's Remove is still non-void and the group still frozen. Proposal's
// leaf check refuses every committed proposal (the fixture's one is the instance's own Remove of
// leaf 0), so the shape decision is driven through the seam with leaf 0's own session: to the
// decision it is leaf 0's Remove of leaf 0. The real member-signed refusal runs end to end in
// call_remove_refused_leave.scn.
func TestAMembersOwnRemoveOfALeafTheInstanceIsRemovingIsRefused(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)
	// The instance's Remove of leaf 0, in SQL only: the guest's queue must stay free for the
	// fixture's bytes, which are that same Remove's.
	ref, leaf, device := id.New(), uint32(0), session.DeviceID
	instance := ref[:]
	if err := h.repo.PutProposal(ctx, store.ProposalRow{
		GroupID: reg.GroupID, Ref: instance, Epoch: 6, Kind: uint8(mlswasi.ProposalRemove),
		TargetLeaf: &leaf, TargetDevice: &device, Origin: 0, ActionID: id.New(),
		IssuedAt: h.clk.Now().Unix(), TTL: 30,
	}); err != nil {
		t.Fatalf("PutProposal: %v", err)
	}

	if err := ds.WithGroupForTest(h.ds, ctx, reg.GroupID, func(g *mlswasi.PublicGroup) error {
		_, _, err := ds.QueueMemberProposalForTest(h.ds, ctx, g, session, reg.GroupID, fixtureFile(t, "remove_leaf0.mls"))
		var dsErr *ds.Error
		if !errors.As(err, &dsErr) || dsErr.Code != "E_REMOVE_PENDING" || dsErr.Status != 409 {
			t.Errorf("leaf 0's own Remove while the instance removes leaf 0: got %v, want 409 E_REMOVE_PENDING", err)
		}
		queued, err := g.ProposalList(ctx)
		if err != nil {
			return err
		}
		if len(queued) != 0 {
			t.Errorf("the guest holds %d queued proposals after the refusal, want 0", len(queued))
		}
		return nil
	}); err != nil {
		t.Fatalf("withGroup: %v", err)
	}
	if got := h.instanceRemovesOf(t, reg.GroupID, 0); len(got) != 1 || !bytes.Equal(got[0].Ref, instance) {
		t.Fatalf("instance Removes of leaf 0 after the refusal: %+v, want the one still non-void", got)
	}
	// Still frozen: an application message is 425 E_COMMIT_REQUIRED.
	err := ds.RequireNoFreezeForTest(h.ds, ctx, reg.GroupID, 6)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_REQUIRED" || dsErr.Status != 425 {
		t.Fatalf("a message after the refusal: got %v, want 425 E_COMMIT_REQUIRED", err)
	}
}

// D(ii): a member with a standing self-Remove is kicked. The instance's Remove IS issued on top of
// it — OpenMLS keeps the later of two Removes of one leaf, so the commit that applies the instance's
// leaves the member's unreferenced, which clause 1 allows — it records the device it was issued
// for, and a commit that omits it is refused.
func TestAKickIsIssuedOnTopOfTheMembersOwnRemoveAndIsMandatory(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)
	h.repo.markCall(reg.GroupID)
	target := h.memberSession(t, reg.GroupID, 2).DeviceID
	h.putMemberRemove(t, reg.GroupID, 2, 3600)

	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, target, id.New()); err != nil {
		t.Fatalf("ProposeRemoveDevice: %v", err)
	}
	got := h.instanceRemovesOf(t, reg.GroupID, 2)
	if len(got) != 1 {
		t.Fatalf("%d instance Removes of leaf 2, want 1: the member's own Remove stood in for the kick", len(got))
	}
	if got[0].TargetDevice == nil || *got[0].TargetDevice != target {
		t.Errorf("the instance Remove records device %v, want %s", got[0].TargetDevice, target)
	}

	// Mandatory: a member commit that does not reference it is refused by clause 1.
	_, err := h.ds.Commit(ctx, session, reg.GroupID, ds.CommitRequest{
		Epoch: 6, Commit: fixtureFile(t, "commits/09.mls"), GroupInfo: dsFixture(t).groupInfo,
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Rule != "outstanding_proposals" {
		t.Fatalf("a commit omitting the kick: got %v, want E_COMMIT_INVALID outstanding_proposals", err)
	}

	// The leaf-addressed path is the same: a member's own Remove does not count, the instance's does.
	h.putMemberRemove(t, reg.GroupID, 3, 3600)
	for range 2 {
		if err := h.ds.ProposeRemove(ctx, reg.GroupID, 3, id.New()); err != nil {
			t.Fatalf("ProposeRemove(3): %v", err)
		}
	}
	if n := len(h.instanceRemovesOf(t, reg.GroupID, 3)); n != 1 {
		t.Fatalf("%d instance Removes of leaf 3, want exactly 1 (issued over the member's, deduped against its own)", n)
	}
}

// D(iv): a voided instance Remove is re-driven across an epoch only when the device now at its
// leaf is the device it was issued for. MLS reuses blank leaves, so after the original device left
// and another joined at the same index, a re-drive by leaf would remove a different member. A
// voided row with no recorded device is never re-driven across epochs.
func TestAReDriveNeverRemovesADifferentDeviceAtAReusedLeaf(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.repo.markCall(reg.GroupID)

	voidedAt5 := func(leaf uint32, device *id.ID) id.ID {
		t.Helper()
		ref, action := id.New(), id.New()
		if err := h.repo.PutProposal(ctx, store.ProposalRow{
			GroupID: reg.GroupID, Ref: ref[:], Epoch: 5, Kind: uint8(mlswasi.ProposalRemove),
			TargetLeaf: &leaf, TargetDevice: device, Origin: 0, ActionID: action,
			IssuedAt: h.clk.Now().Add(-time.Minute).Unix(), TTL: 30,
		}); err != nil {
			t.Fatalf("PutProposal: %v", err)
		}
		if err := h.repo.VoidProposal(ctx, reg.GroupID, ref[:], h.clk.Now().Unix()); err != nil {
			t.Fatalf("VoidProposal: %v", err)
		}
		return action
	}
	left := id.New() // held leaf 1 at epoch 5; leaf 1 now holds another device
	voidedAt5(1, &left)
	voidedAt5(2, nil) // no recorded device
	stillThere := h.memberSession(t, reg.GroupID, 3).DeviceID
	action := voidedAt5(3, &stillThere)
	// The device the Remove was for has held leaf 3 since before it was issued (the fixture's rows
	// start at its registration epoch, 6; the re-drive binds to the membership by added_epoch).
	members, err := h.repo.ListMembers(ctx, reg.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	for i := range members {
		if members[i].LeafIndex == 3 {
			members[i].AddedEpoch = 5
		}
	}
	if err := h.repo.Tx(ctx, func(tx store.Repository) error {
		return tx.ReplaceMembers(ctx, reg.GroupID, 6, members)
	}); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}

	if err := ds.RedriveCallRemovesForTest(h.ds, ctx, reg.GroupID, 5, 6); err != nil {
		t.Fatalf("re-drive: %v", err)
	}
	for _, leaf := range []uint32{1, 2} {
		if n := len(h.instanceRemovesOf(t, reg.GroupID, leaf)); n != 0 {
			t.Errorf("leaf %d: %d Removes re-driven onto the device now holding it, want 0", leaf, n)
		}
	}
	got := h.instanceRemovesOf(t, reg.GroupID, 3)
	if len(got) != 1 || got[0].ActionID != action || got[0].TargetDevice == nil || *got[0].TargetDevice != stillThere {
		t.Fatalf("leaf 3: %+v, want one re-driven Remove with its action id and device", got)
	}
}

// The F9 re-drive is bound to the membership, as ProposeRemoveOfMember is (f10app review, the
// re-drive card): a voided instance Remove issued at epoch 5 for a device that has since rejoined
// the call group at the SAME leaf — an own-leaf resync, which OpenMLS puts in the leftmost blank
// leaf, usually the old index — is not re-aimed at the new membership. The new membership started
// at the resync's epoch (added_epoch 6, replaceMembersTx), after the Remove was issued; the one the
// Remove was for held the leaf from epoch 5 or earlier. The fixture cannot commit, so the resync's
// result is written as the store holds it after one.
func TestAReDriveNeverRemovesTheNewMembershipOfADeviceThatRejoinedAtItsLeaf(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.repo.markCall(reg.GroupID)
	setAdded := func(leaf uint32, added uint64) id.ID {
		t.Helper()
		members, err := h.repo.ListMembers(ctx, reg.GroupID)
		if err != nil {
			t.Fatalf("ListMembers: %v", err)
		}
		var dev id.ID
		for i := range members {
			if members[i].LeafIndex == leaf {
				members[i].AddedEpoch = added
				dev = members[i].DeviceID
			}
		}
		if err := h.repo.Tx(ctx, func(tx store.Repository) error {
			return tx.ReplaceMembers(ctx, reg.GroupID, 6, members)
		}); err != nil {
			t.Fatalf("ReplaceMembers: %v", err)
		}
		return dev
	}
	voidedAt5 := func(leaf uint32, device id.ID) id.ID {
		t.Helper()
		ref, action := id.New(), id.New()
		if err := h.repo.PutProposal(ctx, store.ProposalRow{
			GroupID: reg.GroupID, Ref: ref[:], Epoch: 5, Kind: uint8(mlswasi.ProposalRemove),
			TargetLeaf: &leaf, TargetDevice: &device, Origin: 0, ActionID: action,
			IssuedAt: h.clk.Now().Add(-time.Minute).Unix(), TTL: 30,
		}); err != nil {
			t.Fatalf("PutProposal: %v", err)
		}
		if err := h.repo.VoidProposal(ctx, reg.GroupID, ref[:], h.clk.Now().Unix()); err != nil {
			t.Fatalf("VoidProposal: %v", err)
		}
		return action
	}
	rejoined := setAdded(1, 6) // left, and came back at leaf 1 by the resync that made epoch 6
	stayed := setAdded(3, 4)   // has held leaf 3 since epoch 4: the membership the Remove was for
	voidedAt5(1, rejoined)
	action := voidedAt5(3, stayed)

	if err := ds.RedriveCallRemovesForTest(h.ds, ctx, reg.GroupID, 5, 6); err != nil {
		t.Fatalf("re-drive: %v", err)
	}
	if got := h.instanceRemovesOf(t, reg.GroupID, 1); len(got) != 0 {
		t.Fatalf("leaf 1: %d Removes re-driven onto the device's new membership, want 0: %+v", len(got), got)
	}
	if rows, _ := h.repo.ListProposals(ctx, reg.GroupID, 5, true); slices.ContainsFunc(rows, func(r store.ProposalRow) bool {
		return r.TargetLeaf != nil && *r.TargetLeaf == 1
	}) {
		t.Fatal("leaf 1's voided Remove is still stored: the action is satisfied and must be dropped")
	}
	got := h.instanceRemovesOf(t, reg.GroupID, 3)
	if len(got) != 1 || got[0].ActionID != action || got[0].TargetDevice == nil || *got[0].TargetDevice != stayed {
		t.Fatalf("leaf 3: %+v, want one re-driven Remove with its action id and device", got)
	}
}

// F: a member commit is signed by the uploading device's own leaf. Invariant 4's clause 3 measures
// a by-value Remove against the committer, and the committer is the leaf the PublicGroup
// authenticated, never the session that happened to upload the bytes.
func TestAMemberCommitMustBeSignedByTheUploadingDevicesOwnLeaf(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	// commits/08 is leaf 0's commit removing leaf 1 by value; leaf 1's own device uploads it.
	other := h.memberSession(t, reg.GroupID, 1)
	_, err := h.ds.Commit(ctx, other, reg.GroupID, ds.CommitRequest{
		Epoch: 6, Commit: fixtureFile(t, "commits/08.mls"), GroupInfo: dsFixture(t).groupInfo,
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_FORBIDDEN" {
		t.Fatalf("another leaf's commit uploaded by leaf 1's device: got %v, want E_FORBIDDEN", err)
	}
}

// I1, (3a) of the task-9 review: an outstanding instance Remove of leaf L is satisfied by a commit
// that applies ANY Remove of L — the instance's own, or a member's that passes clause 3 against its
// authenticated proposer. Within one epoch L holds exactly the device the instance Remove recorded,
// so either Remove removes that device. Without it a committer whose queue holds the instance's
// Remove ahead of the member's own (OpenMLS commits the later of two Removes of one leaf) is refused
// until the TTL and charged a lost round per refusal.
//
// commits/08 is leaf 0's commit removing leaf 1 by value (leaves 0 and 1 are one user, so clause 3
// passes); it carries no instance proposal. No GroupInfo at epoch 7 exists for it, so a commit that
// clears clause 1 is refused by the GroupInfo clause instead: that rule is what "clause 1 passed"
// looks like here. The accepted path runs end to end in call_remove_member_remove_committed.scn.
func TestAnyAppliedRemoveOfTheLeafSatisfiesTheInstanceRemove(t *testing.T) {
	for _, c := range []struct {
		name   string
		leaf   uint32
		device func(t *testing.T, h *dsHarness, g id.ID) id.ID
		rule   string
	}{
		{
			name: "the member's Remove of the instance Remove's own leaf",
			leaf: 1,
			device: func(t *testing.T, h *dsHarness, g id.ID) id.ID {
				return h.memberSession(t, g, 1).DeviceID
			},
			rule: "group_info_epoch",
		},
		{
			name: "a Remove of a different leaf",
			leaf: 2,
			device: func(t *testing.T, h *dsHarness, g id.ID) id.ID {
				return h.memberSession(t, g, 2).DeviceID
			},
			rule: "outstanding_proposals",
		},
		{
			// Defence in depth: a row whose recorded device is not the one at its leaf at this
			// epoch is never treated as satisfied by a Remove of that leaf.
			name:   "an instance Remove recording a device the leaf does not hold",
			leaf:   1,
			device: func(*testing.T, *dsHarness, id.ID) id.ID { return id.New() },
			rule:   "outstanding_proposals",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			ctx := context.Background()
			reg, session := h.mustRegister(t)
			ref, leaf, device := id.New(), c.leaf, c.device(t, h, reg.GroupID)
			if err := h.repo.PutProposal(ctx, store.ProposalRow{
				GroupID: reg.GroupID, Ref: ref[:], Epoch: 6, Kind: uint8(mlswasi.ProposalRemove),
				TargetLeaf: &leaf, TargetDevice: &device, Origin: 0, ActionID: id.New(),
				IssuedAt: h.clk.Now().Unix(), TTL: 30,
			}); err != nil {
				t.Fatalf("PutProposal: %v", err)
			}
			_, err := h.ds.Commit(ctx, session, reg.GroupID, ds.CommitRequest{
				Epoch: 6, Commit: fixtureFile(t, "commits/08.mls"), GroupInfo: dsFixture(t).groupInfo,
			})
			var dsErr *ds.Error
			if !errors.As(err, &dsErr) || dsErr.Rule != c.rule {
				t.Fatalf("commits/08 with an instance Remove of leaf %d outstanding: got %v, want rule %q", c.leaf, err, c.rule)
			}
		})
	}
}

// F: the call evictor hears only about devices a committed epoch removed — a refused commit queues
// nothing, neither one refused before the merge nor one whose transaction fails after it. The second
// half pins WHERE the queueing is: after the transaction has committed. A queueEviction moved inside
// the transaction would hear about the phantom device below and fail the "after the merge" cases.
//
// commits/09 is the creator's self-update and commits/09.group_info.mls the GroupInfo it merges to,
// so it is accepted. It removes nobody from the tree, so a device that is not in it is written into
// the call group's member record beforehand: the commit's member rewrite finds that device gone.
func TestARefusedCommitEvictsNobody(t *testing.T) {
	type evictions struct {
		mu  sync.Mutex
		got [][]id.ID
	}
	setup := func(t *testing.T) (*dsHarness, ds.RegisterResult, auth.Session, *evictions, id.ID) {
		t.Helper()
		h := newDSHarness(t)
		reg, session := h.mustRegister(t)
		h.repo.markCall(reg.GroupID)
		ev := &evictions{}
		h.restartWithEvictor(func(_ context.Context, _ id.ID, removed []id.ID) {
			ev.mu.Lock()
			defer ev.mu.Unlock()
			ev.got = append(ev.got, removed)
		})
		phantom := id.New()
		members, err := h.repo.ListMembers(context.Background(), reg.GroupID)
		if err != nil {
			t.Fatalf("ListMembers: %v", err)
		}
		members = append(members, store.MemberRow{
			GroupID: reg.GroupID, LeafIndex: 99_999, UserID: id.New(), DeviceID: phantom,
			SignatureKey: make([]byte, 32), AddedEpoch: 6,
		})
		if err := h.repo.ReplaceMembers(context.Background(), reg.GroupID, 6, members); err != nil {
			t.Fatalf("ReplaceMembers: %v", err)
		}
		return h, reg, session, ev, phantom
	}
	selfUpdate := func(t *testing.T) ds.CommitRequest {
		return ds.CommitRequest{
			Epoch: 6, Commit: fixtureFile(t, "commits/09.mls"),
			GroupInfo: fixtureFile(t, "commits/09.group_info.mls"),
		}
	}
	evicted := func(h *dsHarness, reg ds.RegisterResult, ev *evictions) [][]id.ID {
		ds.FlushEvictionsForTest(h.ds, context.Background(), reg.GroupID)
		ev.mu.Lock()
		defer ev.mu.Unlock()
		return slices.Clone(ev.got)
	}

	t.Run("refused before the merge", func(t *testing.T) {
		h, reg, session, ev, _ := setup(t)
		// commits/08 removes leaf 1; its GroupInfo is epoch 6's, so it is refused after staging.
		if _, err := h.ds.Commit(context.Background(), session, reg.GroupID, ds.CommitRequest{
			Epoch: 6, Commit: fixtureFile(t, "commits/08.mls"), GroupInfo: dsFixture(t).groupInfo,
		}); err == nil {
			t.Fatal("the fixture commit was accepted")
		}
		if got := evicted(h, reg, ev); len(got) != 0 {
			t.Fatalf("a refused commit evicted %v", got)
		}
	})
	t.Run("accepted: the control", func(t *testing.T) {
		h, reg, session, ev, phantom := setup(t)
		if _, err := h.ds.Commit(context.Background(), session, reg.GroupID, selfUpdate(t)); err != nil {
			t.Fatalf("the self-update was refused: %v", err)
		}
		if got := evicted(h, reg, ev); len(got) != 1 || !slices.Equal(got[0], []id.ID{phantom}) {
			t.Fatalf("an accepted commit evicted %v, want [[%s]]", got, phantom)
		}
	})
	for _, fault := range []string{"DeleteProposals", "TxCommit"} {
		t.Run("the transaction fails after the merge at "+fault, func(t *testing.T) {
			h, reg, session, ev, _ := setup(t)
			h.failNextTx(fault)
			if _, err := h.ds.Commit(context.Background(), session, reg.GroupID, selfUpdate(t)); !errors.Is(err, errInjected) {
				t.Fatalf("the self-update with %s failing: got %v, want the injected failure", fault, err)
			}
			if got := evicted(h, reg, ev); len(got) != 0 {
				t.Fatalf("a commit whose transaction rolled back evicted %v", got)
			}
		})
	}
}

// m1 of the task-9 review: the watchdog removes its failing candidate BY DEVICE, resolving the leaf
// under the group lock and checking the device still holds it when the Remove is built. Before, it
// read the leaf outside the lock and proposed a Remove of whatever device held that index by then,
// so a leaf reused in between (a Remove and an Add in one commit) redirected the Remove onto the new
// occupant. The store hook rewrites the leaf's occupant right after the first member read the
// removal makes, which is that window.
func TestTheWatchdogNeverRedirectsItsRemoveOntoAReusedLeaf(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.groupWithMembers(t, 1)
	h.online(g.members[0])
	h.proposeRemoveOf(t, g, 1) // something to commit, and the first round
	candidate, leaf := g.members[0], g.leaves[0]
	newcomer := id.New()

	for i := range 3 {
		round := h.ds.CurrentRound(g.id)
		if round == 0 {
			t.Fatal("no election is armed")
		}
		if err := h.ds.AckCommitNeeded(ctx, g.id, candidate, round); err != nil {
			t.Fatalf("AckCommitNeeded: %v", err)
		}
		if i == 2 {
			h.repo.reuseLeafAfterNextRead(t, g.id, leaf, newcomer)
		}
		h.clk.Advance(h.policy().WatchdogInterval + time.Second)
		h.ds.RunWatchdogOnce(ctx)
	}
	for _, r := range h.instanceRemovesOf(t, g.id, leaf) {
		if r.TargetDevice != nil && *r.TargetDevice == newcomer {
			t.Fatalf("the watchdog's Remove of its candidate %s landed on %s, which took leaf %d meanwhile", candidate, newcomer, leaf)
		}
	}
}

// m1, the leaf-addressed path with the device it expects (the text-group removal in
// internal/api): the Remove is issued only while that device holds the leaf, and is refused with
// the "no longer a member" refusal both when the leaf is gone and when another device holds it.
func TestALeafRemoveNamingItsDeviceIsNeverRedirected(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	at1 := h.memberSession(t, reg.GroupID, 1).DeviceID

	err := h.ds.ProposeRemoveOf(ctx, reg.GroupID, 1, id.New(), id.New())
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_INVALID_REQUEST" {
		t.Fatalf("a Remove of leaf 1 naming another device: got %v, want E_INVALID_REQUEST", err)
	}
	if got := h.instanceRemovesOf(t, reg.GroupID, 1); len(got) != 0 {
		t.Fatalf("a Remove naming the wrong device was issued: %+v", got)
	}
	if err := h.ds.ProposeRemoveOf(ctx, reg.GroupID, 99_999, at1, id.New()); !errors.As(err, &dsErr) || dsErr.Code != "E_INVALID_REQUEST" {
		t.Fatalf("a Remove of a leaf that is gone: got %v, want E_INVALID_REQUEST", err)
	}
	if err := h.ds.ProposeRemoveOf(ctx, reg.GroupID, 1, at1, id.New()); err != nil {
		t.Fatalf("a Remove of leaf 1 naming its device: %v", err)
	}
	got := h.instanceRemovesOf(t, reg.GroupID, 1)
	if len(got) != 1 || got[0].TargetDevice == nil || *got[0].TargetDevice != at1 {
		t.Fatalf("instance Removes of leaf 1: %+v, want one recording %s", got, at1)
	}
}

// m4 of the task-9 review: one call group whose sweep fails does not stop the tick. The cursor has
// already moved past the whole slice, so a sweep that returned on the first failure left every group
// behind it for a full rotation.
func TestTheCallSweepContinuesPastAFailingGroup(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.repo.markCall(reg.GroupID)
	if _, err := ds.SweepCallProposalsForTest(h.ds, ctx); err != nil { // the first tick seeds
		t.Fatalf("sweep: %v", err)
	}
	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, h.memberSession(t, reg.GroupID, 1).DeviceID, id.New()); err != nil {
		t.Fatal(err)
	}
	// Three groups in id order: a healthy one before, the broken one, and the real group after it.
	before, broken := id.ID{}, id.ID{}
	broken[len(broken)-1] = 1
	if bytes.Compare(broken[:], reg.GroupID[:]) >= 0 {
		t.Fatalf("the fixture's group id %s sorts before the broken group's", reg.GroupID)
	}
	ds.MarkCallWorkForTest(h.ds, before)
	ds.MarkCallWorkForTest(h.ds, broken)
	h.repo.breakGroup(broken)

	h.clk.Advance(31 * time.Second)
	if _, err := ds.SweepCallProposalsForTest(h.ds, ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rows := h.proposals(t, reg.GroupID, true)
	if len(rows) != 1 || rows[0].VoidAt != nil || rows[0].IssuedAt != h.clk.Now().Unix() {
		t.Fatalf("rows %+v, want the Remove behind the broken group voided and re-driven in the same tick", rows)
	}
	if work := ds.CallWorkForTest(h.ds); !slices.Contains(work, broken) || slices.Contains(work, before) {
		t.Fatalf("call work %v, want the broken group kept for the next tick and the gone one dropped", work)
	}
}

// F: the call sweeper's work per tick does not grow with the instance. It visits the call groups
// that have instance work outstanding, not every open group, and still voids and re-drives within
// its interval; after a restart its first tick finds the call groups again.
func TestTheCallSweeperVisitsOnlyCallGroupsWithInstanceWork(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.repo.markCall(reg.GroupID)
	if _, err := ds.SweepCallProposalsForTest(h.ds, ctx); err != nil { // the first tick seeds
		t.Fatalf("sweep: %v", err)
	}
	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, h.memberSession(t, reg.GroupID, 1).DeviceID, id.New()); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(31 * time.Second)
	before := h.repo.openGroupPages.Load()
	if _, err := ds.SweepCallProposalsForTest(h.ds, ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if walked := h.repo.openGroupPages.Load() - before; walked != 0 {
		t.Errorf("the call sweep listed open groups %d times, want 0 after its first tick", walked)
	}
	rows := h.proposals(t, reg.GroupID, true)
	if len(rows) != 1 || rows[0].VoidAt != nil || rows[0].IssuedAt != h.clk.Now().Unix() {
		t.Fatalf("rows %+v, want the Remove voided and re-driven by the call sweep", rows)
	}

	// A restart forgets which groups had work; the new sweeper's first tick finds them again.
	h.restartDS()
	h.clk.Advance(31 * time.Second)
	if _, err := ds.SweepCallProposalsForTest(h.ds, ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rows = h.proposals(t, reg.GroupID, true)
	if len(rows) != 1 || rows[0].VoidAt != nil || rows[0].IssuedAt != h.clk.Now().Unix() {
		t.Fatalf("rows %+v after a restart, want the Remove re-driven by the first call sweep", rows)
	}
}

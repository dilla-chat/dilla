package ds_test

// heal_reseed_test.go is finding G2b of the second hardening review: heal's reseed adopts an
// uploaded ratchet tree (the instance holds no state blob after the restore), and before this it
// checked nothing about the tree's leaves - only that the GroupInfo is signed by the leaf naming the
// healer's device, names this group, is not older than the stored epoch, and carries the tree's
// hash. Every leaf of a reseeded tree now passes the clause an Add passes (invariant 4's Add
// clause: the device is known, owned by the credential's user, not revoked, keyed by its registered
// key, listed in its user's newest signed device list, and the user is eligible under the ACL), and
// the tree holds no leaf the delivery service cannot name.
//
// And T2 of the same review: the key check on a REPLAYED resync (heal.go's
// checkHealedExternalCommit) is pinned by its own test here.

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

// A reseed from an ahead snapshot cannot prove that a nonempty tail contains the missing commits:
// reseed replay skips items from epochs already baked into the uploaded tree.
func TestAReseedAheadOfRestoredEpochIsRefusedEvenWithAnIncompleteTail(t *testing.T) {
	h := newDSHarness(t)
	g := h.enrolledGroup(t)
	ctx := context.Background()
	inst, err := h.wasm.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := inst.PublicGroupImport(ctx, h.groupRow(t, g.id).PublicGroupState, g.id[:])
	if err != nil {
		inst.Release()
		t.Fatal(err)
	}
	processed, err := advanced.Process(ctx, fixtureFile(t, "commits/09.mls"))
	if err != nil || processed.Staged == nil {
		t.Fatalf("process advancing commit: staged=%v err=%v", processed.Staged, err)
	}
	if _, err := advanced.Merge(ctx, *processed.Staged); err != nil {
		t.Fatalf("merge advancing commit: %v", err)
	}
	tree, _, epoch, err := advanced.Tree(ctx)
	if err != nil {
		t.Fatalf("export advanced tree: %v", err)
	}
	if epoch != g.Epoch()+1 {
		t.Fatalf("advanced tree epoch = %d, want %d", epoch, g.Epoch()+1)
	}
	if err := advanced.Close(ctx); err != nil {
		t.Fatal(err)
	}
	inst.Release()

	if err := h.ds.OnRestore(ctx, h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	h.destroyStateBlob(t, g)
	_, err = h.ds.Heal(ctx, g.session, g.id, ds.HealRequest{
		GroupInfo: h.groupInfoAt(t, g, epoch), RatchetTree: tree,
		Tail: h.proposalTail(t, g, 0),
	})
	if !hasRule(err, "reseed") {
		t.Fatalf("ahead reseed with incomplete tail: got %v, want E_COMMIT_INVALID/reseed", err)
	}
	if !h.epochUnknown(t, g.id) {
		t.Fatal("refused ahead reseed cleared epoch_unknown")
	}
	if got := h.handshakeCount(t, g.id); got != 0 {
		t.Fatalf("refused ahead reseed wrote %d handshakes, want 0", got)
	}
}

// restoreWithoutBlob is the reseed's starting point for an enrolled group: the tree and the
// GroupInfo its healer (leaf 0, the GroupInfo's signer) uploads, after a restore that lost the
// group's state blob.
func (h *dsHarness) restoreWithoutBlob(t *testing.T, g *dsMessageGroup) (tree, groupInfo []byte) {
	t.Helper()
	tree = h.currentTree(t, g)
	groupInfo = h.groupInfoAt(t, g, g.Epoch())
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	h.destroyStateBlob(t, g)
	return tree, groupInfo
}

// refusedReseed asserts that the reseed left the group as the restore left it: still
// epoch-unknown, no handshake row, the restored member rows unchanged.
func (h *dsHarness) refusedReseed(t *testing.T, g *dsMessageGroup, members int) {
	t.Helper()
	if !h.epochUnknown(t, g.id) {
		t.Error("a refused reseed must leave the group epoch-unknown")
	}
	if got := h.handshakeCount(t, g.id); got != 0 {
		t.Errorf("a refused reseed wrote %d handshake rows", got)
	}
	after, err := h.repo.ListMembers(context.Background(), g.id)
	if err != nil || len(after) != members {
		t.Errorf("the member rows changed: %d -> %d (%v)", members, len(after), err)
	}
}

// The reseed's healer must have been a member of the group as the instance restored it. A device
// that names itself in the uploaded tree, but that the restored member rows do not hold (here:
// leaf 0's row is missing from the restore, every leaf's device is enrolled and its user is
// eligible), gets the refusal every heal by a non-member gets: 403 E_FORBIDDEN.
func TestAReseedByADeviceTheRestoreDoesNotHoldIsForbidden(t *testing.T) {
	h := newDSHarness(t)
	g := h.enrolledGroup(t)
	ctx := context.Background()
	members, err := h.repo.ListMembers(ctx, g.id)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	kept := make([]store.MemberRow, 0, len(members))
	for _, m := range members {
		if m.DeviceID == g.device {
			h.acl.allow(m.UserID) // its user stays eligible: the refusal must be about the device
			continue
		}
		kept = append(kept, m)
	}
	if len(kept) != len(members)-1 {
		t.Fatalf("the healer holds %d rows, want 1", len(members)-len(kept))
	}
	if err := h.repo.ReplaceMembers(ctx, g.id, g.Epoch(), kept); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
	tree, groupInfo := h.restoreWithoutBlob(t, g)

	_, err = h.ds.Heal(ctx, g.session, g.id, ds.HealRequest{GroupInfo: groupInfo, RatchetTree: tree})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_FORBIDDEN" {
		t.Fatalf("got %v, want E_FORBIDDEN", err)
	}
	h.refusedReseed(t, g, len(kept))
}

// The reseeded tree's dilla_binding must be the one the instance holds for the group: its kind,
// its community and its target. Here the instance's stored binding is changed in each of the
// three (the uploaded tree is the honest one, so the two disagree in exactly that field), and the
// reseed is refused with rule "reseed".
func TestAReseedWhoseBindingIsNotTheGroupsIsRefused(t *testing.T) {
	community := id.New()
	for _, c := range []struct {
		name   string
		change func(b *ds.Binding)
	}{
		{"kind", func(b *ds.Binding) { b.Kind = 1 }},
		{"community", func(b *ds.Binding) { b.CommunityID = &community }},
		{"target", func(b *ds.Binding) { b.TargetID = id.New() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			g := h.enrolledGroup(t)
			ctx := context.Background()
			members, err := h.repo.ListMembers(ctx, g.id)
			if err != nil {
				t.Fatalf("ListMembers: %v", err)
			}
			tree, groupInfo := h.restoreWithoutBlob(t, g)
			stored, err := ds.DecodeBindingForTest(h.groupRow(t, g.id).Binding)
			if err != nil {
				t.Fatalf("decode the stored binding: %v", err)
			}
			c.change(&stored)
			other, err := cborx.Marshal(stored)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			db, err := sqlite.OpenWrite(h.path)
			if err != nil {
				t.Fatalf("sqlite.OpenWrite: %v", err)
			}
			if _, err := db.ExecContext(ctx, "UPDATE mls_groups SET binding = ? WHERE group_id = ?", other, g.id[:]); err != nil {
				t.Fatalf("set the stored binding: %v", err)
			}
			_ = db.Close()

			_, err = h.ds.Heal(ctx, g.session, g.id, ds.HealRequest{GroupInfo: groupInfo, RatchetTree: tree})
			if !hasRule(err, "reseed") {
				t.Fatalf("got %v, want E_COMMIT_INVALID/reseed", err)
			}
			h.refusedReseed(t, g, len(members))
		})
	}
}

// leafEnrolment is what enrolLeaves makes of one leaf's device.
type leafEnrolment int

const (
	// enrolHonest: the device is registered under the key its leaf carries and listed.
	enrolHonest leafEnrolment = iota
	// enrolNone: the instance holds no row for the device.
	enrolNone
	// enrolOtherKey: the device is registered (and listed) under a key its leaf does not carry.
	enrolOtherKey
	// enrolUnlisted: the device is registered under its leaf key, but its user's list omits it.
	enrolUnlisted
)

// enrolLeaves makes the devices of a seeded group's leaves devices of this instance, as signup and
// pairing leave them: one user per distinct user id with an SSK, one device row per leaf, one
// signed device list per user. how decides each leaf's enrolment; nil is enrolHonest for all.
func (h *dsHarness) enrolLeaves(t *testing.T, groupID id.ID, how func(leaf uint32) leafEnrolment) {
	t.Helper()
	members, err := h.repo.ListMembers(context.Background(), groupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	ssk := testSSK(0x75)
	lists := map[id.ID][]listEntry{}
	var order []id.ID
	for _, m := range members {
		e := enrolHonest
		if how != nil {
			e = how(m.LeafIndex)
		}
		if _, seen := lists[m.UserID]; !seen {
			order = append(order, m.UserID)
			lists[m.UserID] = nil
			if _, err := h.repo.GetUser(context.Background(), m.UserID); err != nil {
				h.userWithSSK(t, m.UserID, ssk)
			}
		}
		switch e {
		case enrolNone:
			continue
		case enrolOtherKey:
			other := bytes.Repeat([]byte{0x5f}, 32)
			h.accountWithKey(t, m.UserID, m.DeviceID, other)
			lists[m.UserID] = append(lists[m.UserID], listEntry{DeviceID: m.DeviceID[:], DSKPub: other, AddedAt: 1})
		case enrolUnlisted:
			h.accountWithKey(t, m.UserID, m.DeviceID, m.SignatureKey)
		default:
			h.accountWithKey(t, m.UserID, m.DeviceID, m.SignatureKey)
			lists[m.UserID] = append(lists[m.UserID], listEntry{DeviceID: m.DeviceID[:], DSKPub: m.SignatureKey, AddedAt: 1})
		}
	}
	for _, user := range order {
		h.publishDeviceList(t, user, signedDeviceList(t, ssk, user, lists[user]))
	}
}

// enrolledGroup is h.group with every leaf of the fixture a real device of this instance: a reseed
// adopts the uploaded tree only when every leaf passes the Add clause (G2b), so a test of an
// accepted reseed needs the fixture's 1,500 devices to exist as signup left them.
func (h *dsHarness) enrolledGroup(t *testing.T) *dsMessageGroup {
	t.Helper()
	got, session := h.mustRegister(t)
	h.enrolLeaves(t, got.GroupID, nil)
	row, err := h.repo.GetGroup(context.Background(), got.GroupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	g := &dsMessageGroup{id: got.GroupID, device: session.DeviceID, session: session, epoch: row.Epoch}
	if h.sessions == nil {
		h.sessions = map[id.ID]auth.Session{}
	}
	h.sessions[g.device] = session
	return g
}

// G2b: a reseed whose tree holds a leaf that would not pass the Add clause is refused, and the group
// is left exactly as the restore left it (epoch-unknown, the restored member rows, no handshake
// row). Each case spoils one leaf - leaf 7, not the healer's - of an otherwise honest group; the
// honest reseed of the same group is accepted (TestAHealWithNoBlobReseedsFromTheSuppliedTree).
func TestAReseedRefusesATreeWithALeafThatWouldNotPassTheAddClause(t *testing.T) {
	const spoiled = 7
	for _, c := range []struct {
		name string
		how  leafEnrolment
		acl  bool // the leaf's user is forbidden by the ACL instead
		rule string
	}{
		{"a leaf's device is unknown to the instance", enrolNone, false, "add_key_package"},
		{"a leaf's key is not its device's registered key", enrolOtherKey, false, "add_key_package"},
		{"a leaf's device is not in its user's signed list", enrolUnlisted, false, "add_key_package"},
		{"a leaf's user is not eligible under the ACL", enrolHonest, true, "add_acl"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			got, session := h.mustRegister(t)
			h.enrolLeaves(t, got.GroupID, func(leaf uint32) leafEnrolment {
				if leaf == spoiled {
					return c.how
				}
				return enrolHonest
			})
			members, err := h.repo.ListMembers(context.Background(), got.GroupID)
			if err != nil {
				t.Fatalf("ListMembers: %v", err)
			}
			if c.acl {
				for _, m := range members {
					if m.LeafIndex == spoiled {
						h.acl.forbid(m.UserID)
					}
				}
			}
			g := &dsMessageGroup{id: got.GroupID, device: session.DeviceID, session: session, epoch: 6}
			tree := h.currentTree(t, g)
			groupInfo := h.groupInfoAt(t, g, g.Epoch())
			if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
				t.Fatalf("OnRestore: %v", err)
			}
			h.destroyStateBlob(t, g)

			_, err = h.ds.Heal(context.Background(), session, g.id, ds.HealRequest{
				GroupInfo: groupInfo, RatchetTree: tree,
			})
			if !hasRule(err, c.rule) {
				t.Fatalf("got %v, want E_COMMIT_INVALID/%s", err, c.rule)
			}
			if !h.epochUnknown(t, g.id) {
				t.Error("a refused reseed must leave the group epoch-unknown")
			}
			if got := h.handshakeCount(t, g.id); got != 0 {
				t.Errorf("a refused reseed wrote %d handshake rows", got)
			}
			after, err := h.repo.ListMembers(context.Background(), g.id)
			if err != nil || len(after) != len(members) {
				t.Errorf("the member rows changed: %d -> %d (%v)", len(members), len(after), err)
			}
		})
	}
}

// T2: the key check on a replayed resync (checkHealedExternalCommit) is pinned. A replayed external
// commit that removes the joiner's previous leaf and lands the device back with a key that is not
// its registered key is refused with the joiner's rule; the same check passes when the key is the
// registered one. The clause is asserted over the merged state it is defined on - the fixture holds
// no external commit, and the honest replayed resync is driven end to end by internal/testkit's
// heal scenarios.
func TestAReplayedResyncMustLandOnTheDevicesRegisteredKey(t *testing.T) {
	h := newDSHarness(t)
	got, _ := h.mustRegister(t)
	members, err := h.repo.ListMembers(context.Background(), got.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	var joiner store.MemberRow
	for _, m := range members {
		if m.LeafIndex == 3 {
			joiner = m
		}
	}
	h.enrolLeaves(t, got.GroupID, nil)
	identity, _, _ := h.memberIdentity(t, got.GroupID, 3)
	before := map[uint32][2]id.ID{}
	for _, m := range members {
		before[m.LeafIndex] = [2]id.ID{m.DeviceID, m.UserID}
	}
	removed := joiner.LeafIndex
	applied := []mlswasi.AppliedProposal{{Kind: mlswasi.ProposalRemove, TargetLeaf: &removed}}
	after := func(key []byte) mlswasi.GroupState {
		return mlswasi.GroupState{Members: []mlswasi.Member{{
			LeafIndex: 1_500, SignatureKey: key, CredentialIdentity: identity,
		}}}
	}

	if _, err := ds.CheckHealedExternalCommitForTest(h.ds, context.Background(), got.GroupID, before,
		after(bytes.Repeat([]byte{0x5f}, 32)), applied); !hasRule(err, "external_joiner") {
		t.Fatalf("a replayed resync on a key the device never registered: got %v, want E_COMMIT_INVALID/external_joiner", err)
	}
	device, err := ds.CheckHealedExternalCommitForTest(h.ds, context.Background(), got.GroupID, before,
		after(joiner.SignatureKey), applied)
	if err != nil || device != joiner.DeviceID {
		t.Fatalf("the replayed resync on the registered key: %v, %v; want %s", device, err, joiner.DeviceID)
	}
}

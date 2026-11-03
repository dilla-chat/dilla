package ds_test

// rearm_test.go is DS-1 of the server-half branch review: an instance proposal re-issued in the
// epoch it was voided in. The external sender signs deterministically and the frame carries no
// nonce, so the re-signed proposal is byte-identical to the voided one and its ref is the voided
// row's ref. It must re-arm that row — live again, issued now, a fresh TTL, the original action —
// and a failure on the way must never take the proposal out of the guest's queue, which held it
// before the call.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// guestHolds reports whether the group's PublicGroup queues ref. With reimport set, the cached
// handle is evicted first, so the answer is the durable state blob's.
func (h *dsHarness) guestHolds(t *testing.T, groupID id.ID, ref []byte, reimport bool) bool {
	t.Helper()
	ctx := context.Background()
	if reimport {
		if err := ds.EvictStateForTest(h.ds, ctx, groupID); err != nil {
			t.Fatalf("evict: %v", err)
		}
	}
	held := false
	if err := ds.WithGroupForTest(h.ds, ctx, groupID, func(g *mlswasi.PublicGroup) error {
		list, err := g.ProposalList(ctx)
		if err != nil {
			return err
		}
		for _, p := range list {
			held = held || bytes.Equal(p[0], ref)
		}
		return nil
	}); err != nil {
		t.Fatalf("withGroup: %v", err)
	}
	return held
}

// proposalsAt is every proposal row of the group at epoch.
func (h *dsHarness) proposalsAt(t *testing.T, groupID id.ID, epoch uint64, includeVoid bool) []store.ProposalRow {
	t.Helper()
	rows, err := h.repo.ListProposals(context.Background(), groupID, epoch, includeVoid)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	return rows
}

// voidedRemoveOfLeafOne issues an instance Remove of leaf 1 in the fixture's text group and lets
// its 24-hour TTL void it, and returns the voided row.
func (h *dsHarness) voidedRemoveOfLeafOne(t *testing.T, g *dsMessageGroup, action id.ID) store.ProposalRow {
	t.Helper()
	ctx := context.Background()
	if err := h.ds.ProposeRemove(ctx, g.id, 1, action); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	h.clk.Advance(24*time.Hour + time.Second)
	if _, err := ds.SweepProposalsForTest(h.ds, ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rows := h.proposalsAt(t, g.id, g.Epoch(), true)
	if len(rows) != 1 || rows[0].VoidAt == nil {
		t.Fatalf("rows %+v, want the one Remove voided by its TTL", rows)
	}
	return rows[0]
}

// (a) A text group's voided Remove re-proposed in its epoch — by reconcile, the inactivity sweep or
// a repeated kick, all of which dedupe only against non-void rows — re-arms the one row: live, issued
// now, the text TTL, the first action's id, the same ref. The group is frozen again, and the guest
// holds the proposal, in memory and in the durable state blob.
func TestAVoidedTextRemoveIsReArmedInItsEpoch(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.group(t)
	action := id.New()
	voided := h.voidedRemoveOfLeafOne(t, g, action)

	if err := h.ds.ProposeRemove(ctx, g.id, 1, id.New()); err != nil {
		t.Fatalf("re-proposing the voided Remove: %v", err)
	}
	rows := h.proposalsAt(t, g.id, g.Epoch(), true)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want the one Remove re-armed rather than duplicated", len(rows))
	}
	r := rows[0]
	if r.VoidAt != nil || !bytes.Equal(r.Ref, voided.Ref) || r.ActionID != action ||
		r.IssuedAt != h.clk.Now().Unix() || r.TTL != uint64((24*time.Hour).Seconds()) {
		t.Fatalf("re-armed row %+v: want live, ref %x, action %s, issued now with the 24 h TTL", r, voided.Ref, action)
	}
	_, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_REQUIRED" {
		t.Fatalf("a send after the re-arm: got %v, want 425 E_COMMIT_REQUIRED", err)
	}
	if !h.guestHolds(t, g.id, voided.Ref, false) {
		t.Fatal("the guest's queue lost the re-armed proposal")
	}
	if !h.guestHolds(t, g.id, voided.Ref, true) {
		t.Fatal("the durable state blob lost the re-armed proposal")
	}
}

// (b) The re-arm fails inside its transaction. The guest held the proposal before the call, so the
// failure leaves it there (and in the state blob), and the row stays as it was: void. The next
// attempt re-arms it.
func TestAFailedReArmKeepsTheProposalInTheGuest(t *testing.T) {
	for _, fault := range []string{"AppendHandshake", "ReissueProposal"} {
		t.Run(fault, func(t *testing.T) {
			h := newDSHarness(t)
			ctx := context.Background()
			g := h.group(t)
			action := id.New()
			voided := h.voidedRemoveOfLeafOne(t, g, action)

			h.failNextTx(fault)
			if err := h.ds.ProposeRemove(ctx, g.id, 1, id.New()); !errors.Is(err, errInjected) {
				t.Fatalf("the re-arm with %s failing: got %v, want the injected failure", fault, err)
			}
			h.failNextTx("")
			if rows := h.proposalsAt(t, g.id, g.Epoch(), true); len(rows) != 1 || rows[0].VoidAt == nil {
				t.Fatalf("rows %+v after the failed re-arm, want the one Remove still void", rows)
			}
			if !h.guestHolds(t, g.id, voided.Ref, false) {
				t.Fatal("the failed re-arm took the proposal out of the guest's queue, which held it before the call")
			}
			if !h.guestHolds(t, g.id, voided.Ref, true) {
				t.Fatal("the state blob no longer holds the proposal")
			}

			if err := h.ds.ProposeRemove(ctx, g.id, 1, id.New()); err != nil {
				t.Fatalf("the next re-arm: %v", err)
			}
			rows := h.proposalsAt(t, g.id, g.Epoch(), true)
			if len(rows) != 1 || rows[0].VoidAt != nil || rows[0].ActionID != action {
				t.Fatalf("rows %+v, want the Remove re-armed with its first action", rows)
			}
		})
	}
}

// syncBuffer is a log sink a test can read while the delivery service writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// restartWithLog rebuilds the delivery service over the same store with its log written to w.
func (h *dsHarness) restartWithLog(w io.Writer) {
	h.t.Helper()
	if err := h.ds.Shutdown(context.Background()); err != nil {
		h.t.Fatalf("Shutdown: %v", err)
	}
	d, err := ds.New(ds.Options{
		Store: h.repo, Wasm: h.wasm, Gateway: h.gw, Clock: h.clk,
		Keys: testInstanceKeys(h.t), Policy: ds.DefaultPolicy(), Channels: h.channels,
		ACL: h.acl, Log: slog.New(slog.NewTextHandler(w, nil)),
	})
	if err != nil {
		h.t.Fatalf("ds.New: %v", err)
	}
	h.t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	h.ds = d
}

// (c) The inactivity sweep across a void: the Remove of a device that stays away is voided after
// 24 h with nobody committing it, and the same sweep re-proposes it. It is re-armed — live, issued
// now, the first sweep's action — and nothing is logged as refused.
func TestTheInactivitySweepReArmsItsVoidedRemove(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	var logs syncBuffer
	h.restartWithLog(&logs)
	g := h.groupWithMembers(t, 2)
	for i := range 2 {
		h.account(t, g.sessionOf(i).UserID, g.members[i])
	}
	h.clk.Advance(91 * 24 * time.Hour)
	if err := h.repo.TouchDevice(ctx, g.members[0], h.clk.Now().Add(-24*time.Hour).Unix()); err != nil {
		t.Fatalf("TouchDevice: %v", err)
	}
	if _, err := h.ds.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	first := h.instanceRemovesOf(t, g.id, g.leafOf(1))
	if len(first) != 1 {
		t.Fatalf("instance Removes of the unseen member's leaf: %+v, want one", first)
	}

	h.clk.Advance(24*time.Hour + time.Second)
	if _, err := h.ds.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if strings.Contains(logs.String(), "was not proposed") {
		t.Fatalf("the sweep logged a refused inactivity Remove:\n%s", logs.String())
	}
	rows := h.instanceRemovesOf(t, g.id, g.leafOf(1))
	if len(rows) != 1 || !bytes.Equal(rows[0].Ref, first[0].Ref) || rows[0].ActionID != first[0].ActionID ||
		rows[0].IssuedAt != h.clk.Now().Unix() {
		t.Fatalf("instance Removes of the leaf after the void: %+v, want the first one re-armed now", rows)
	}
	if !h.guestHolds(t, g.id, first[0].Ref, true) {
		t.Fatal("the state blob lost the re-armed proposal")
	}
}

// (d) A call group's same-epoch re-drive whose transaction fails leaves the voided Remove in the
// guest's queue (the guest held it before the re-drive), and the next sweep re-drives it.
func TestAFailedCallReDriveKeepsTheProposalInTheGuest(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.repo.markCall(reg.GroupID)
	action := id.New()
	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, h.memberSession(t, reg.GroupID, 1).DeviceID, action); err != nil {
		t.Fatal(err)
	}
	ref := h.proposals(t, reg.GroupID, false)[0].Ref
	h.clk.Advance(31 * time.Second)
	h.failNextTx("ReissueProposal")
	if _, err := ds.SweepProposalsForTest(h.ds, ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	h.failNextTx("")
	if rows := h.proposals(t, reg.GroupID, true); len(rows) != 1 || rows[0].VoidAt == nil {
		t.Fatalf("rows %+v after the failed re-drive, want the Remove void", rows)
	}
	if !h.guestHolds(t, reg.GroupID, ref, false) || !h.guestHolds(t, reg.GroupID, ref, true) {
		t.Fatal("the failed re-drive took the voided Remove out of the guest, which held it before")
	}
	if _, err := ds.SweepProposalsForTest(h.ds, ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rows := h.proposals(t, reg.GroupID, true)
	if len(rows) != 1 || rows[0].VoidAt != nil || rows[0].ActionID != action {
		t.Fatalf("rows %+v, want the Remove re-driven with its action", rows)
	}
}

// DS-7's residual window, closed under the group lock: a Remove bound to one membership — device AND
// the epoch it took the leaf — is refused with ErrRemoveTargetGone, proposing nothing, when the same
// device holds the same leaf from another epoch (it left and came back at that index), and is
// proposed when the membership is the one the caller read.
func TestARemoveBoundToAMembershipIsRefusedForALaterOneAtTheSameLeaf(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	var at1 store.MemberRow
	members, err := h.repo.ListMembers(ctx, reg.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	for _, m := range members {
		if m.LeafIndex == 1 {
			at1 = m
		}
	}
	err = h.ds.ProposeRemoveOfMember(ctx, reg.GroupID, 1, at1.DeviceID, at1.AddedEpoch+1, id.New())
	if !errors.Is(err, ds.ErrRemoveTargetGone) {
		t.Fatalf("a Remove of another membership of the same device and leaf: got %v, want ErrRemoveTargetGone", err)
	}
	if got := h.instanceRemovesOf(t, reg.GroupID, 1); len(got) != 0 {
		t.Fatalf("instance Removes of leaf 1 after the refusal: %+v, want none", got)
	}
	action := id.New()
	if err := h.ds.ProposeRemoveOfMember(ctx, reg.GroupID, 1, at1.DeviceID, at1.AddedEpoch, action); err != nil {
		t.Fatalf("the Remove of the membership read: %v", err)
	}
	got := h.instanceRemovesOf(t, reg.GroupID, 1)
	if len(got) != 1 || got[0].ActionID != action || got[0].TargetDevice == nil || *got[0].TargetDevice != at1.DeviceID {
		t.Fatalf("instance Removes of leaf 1: %+v, want one for %s", got, at1.DeviceID)
	}
}

// mls_members.added_epoch is the epoch the device took its leaf, kept across member-set rewrites
// while the same device holds the same leaf, and restarted when another device (or the same device,
// back after leaving) holds it — which is what ProposeRemoveOfMember binds to.
func TestAddedEpochIsKeptWhileTheSameDeviceHoldsTheLeaf(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	var state mlswasi.GroupState
	if err := ds.WithGroupForTest(h.ds, ctx, reg.GroupID, func(g *mlswasi.PublicGroup) error {
		var err error
		state, err = g.State(ctx)
		return err
	}); err != nil {
		t.Fatalf("State: %v", err)
	}
	base := state.Epoch
	state.Epoch = base + 1
	members := slices.Clone(state.Members)
	var i1, i2 int
	for i, m := range members {
		switch m.LeafIndex {
		case 1:
			i1 = i
		case 2:
			i2 = i
		}
	}
	// Leaves 1 and 2 trade devices; every other leaf keeps its own.
	members[i1].CredentialIdentity, members[i2].CredentialIdentity = members[i2].CredentialIdentity, members[i1].CredentialIdentity
	state.Members = members
	if err := ds.ReplaceMembersForTest(h.ds, ctx, reg.GroupID, 0, state); err != nil {
		t.Fatalf("replace: %v", err)
	}
	rows, err := h.repo.ListMembers(ctx, reg.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	for _, m := range rows {
		want := base
		if m.LeafIndex == 1 || m.LeafIndex == 2 {
			want = base + 1
		}
		if m.AddedEpoch != want {
			t.Fatalf("leaf %d added_epoch %d, want %d", m.LeafIndex, m.AddedEpoch, want)
		}
	}
}

// R-3 of the DS re-review: a device that rejoins by external commit (an own-leaf resync) starts a
// new membership, stamped with the new epoch, whether it lands at its old index (the leftmost blank)
// or at a lower blank one. A membership-bound Remove meant for the session before the resync is then
// refused in both positions instead of depending on where blank leaves happen to lie.
func TestAnExternalJoinersLeafIsANewMembershipAtAnyIndex(t *testing.T) {
	for _, c := range []struct {
		name string
		to   uint32 // the leaf the resynced device lands on; it held leaf 3 before
	}{{"the same index", 3}, {"a lower blank index", 2}} {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			ctx := context.Background()
			reg, _ := h.mustRegister(t)
			var state mlswasi.GroupState
			if err := ds.WithGroupForTest(h.ds, ctx, reg.GroupID, func(g *mlswasi.PublicGroup) error {
				var err error
				state, err = g.State(ctx)
				return err
			}); err != nil {
				t.Fatalf("State: %v", err)
			}
			base := state.Epoch
			before, err := h.repo.ListMembers(ctx, reg.GroupID)
			if err != nil {
				t.Fatalf("ListMembers: %v", err)
			}
			var at3 store.MemberRow
			for _, m := range before {
				if m.LeafIndex == 3 {
					at3 = m
				}
			}
			var cred3 []byte
			for _, m := range state.Members {
				if m.LeafIndex == 3 {
					cred3 = m.CredentialIdentity
				}
			}
			// The resync: leaf 3's device lands on c.to; leaves 2 and 3 are otherwise blank.
			members := slices.DeleteFunc(slices.Clone(state.Members), func(m mlswasi.Member) bool {
				return m.LeafIndex == 3 || m.LeafIndex == 2
			})
			members = append(members, mlswasi.Member{LeafIndex: c.to, SignatureKey: make([]byte, 32), CredentialIdentity: cred3})
			state.Members, state.Epoch = members, base+1
			if err := ds.ReplaceMembersForTest(h.ds, ctx, reg.GroupID, 1, state, c.to); err != nil {
				t.Fatalf("replace: %v", err)
			}
			err = h.ds.ProposeRemoveOfMember(ctx, reg.GroupID, c.to, at3.DeviceID, at3.AddedEpoch, id.New())
			if !errors.Is(err, ds.ErrRemoveTargetGone) {
				t.Fatalf("a Remove bound to the membership before the resync: got %v, want ErrRemoveTargetGone", err)
			}
			if err := h.ds.ProposeRemoveOfMember(ctx, reg.GroupID, c.to, at3.DeviceID, base+1, id.New()); err != nil {
				t.Fatalf("a Remove bound to the resynced membership: %v", err)
			}
		})
	}
}

// POST /proposal sent twice, including the branches a real client cannot reach (a ref is a hash over
// the sender and the signature, so only a forged table or log gets there). The member's own live
// row logged from this leaf and device is answered with its seq and left alone; the member's own
// VOID row is re-armed (live, issued now, the 24 h TTL, the same action) and answered with its seq
// (R-1 of the DS re-review); an instance row and another sender are 409 E_REMOVE_PENDING.
// TestAMemberProposalUploadedTwiceIsAnsweredAsTheFirst and
// TestAMemberReUploadingItsVoidProposalIsReArmedAndRemoved (internal/testkit) drive the identical
// and the void duplicate through POST /proposal with a real client.
func TestAMemberProposalAgainIsIdempotentOnlyForTheSameLiveProposal(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.group(t)
	blob := []byte("a member proposal")
	leaf := uint32(0)
	seq, err := h.repo.NextSeq(ctx, g.id)
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	if err := h.repo.AppendHandshake(ctx, store.HandshakeRow{
		GroupID: g.id, Seq: seq, Epoch: g.Epoch(), Kind: 0, SenderLeaf: &leaf, SenderDevice: &g.device,
		Blob: blob, Created: h.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("AppendHandshake: %v", err)
	}
	action := id.New()
	live := store.ProposalRow{GroupID: g.id, Ref: []byte("ref"), Epoch: g.Epoch(), Kind: 3, TargetLeaf: &leaf,
		Origin: 1, ActionID: action, IssuedAt: h.clk.Now().Unix(), TTL: 86400}
	if err := h.repo.PutProposal(ctx, live); err != nil {
		t.Fatalf("PutProposal: %v", err)
	}

	got, err := ds.MemberProposalAgainForTest(h.ds, ctx, g.id, g.Epoch(), leaf, g.device, blob, live)
	if err != nil || got != seq {
		t.Fatalf("the same live proposal again: seq %d, err %v; want seq %d", got, err, seq)
	}
	if rows := h.proposalsAt(t, g.id, g.Epoch(), true); len(rows) != 1 || rows[0].IssuedAt != live.IssuedAt {
		t.Fatalf("rows %+v after the live duplicate, want the one row untouched", rows)
	}

	h.clk.Advance(25 * time.Hour)
	if err := h.repo.VoidProposal(ctx, g.id, live.Ref, h.clk.Now().Unix()); err != nil {
		t.Fatalf("VoidProposal: %v", err)
	}
	void := h.proposalsAt(t, g.id, g.Epoch(), true)[0]
	got, err = ds.MemberProposalAgainForTest(h.ds, ctx, g.id, g.Epoch(), leaf, g.device, blob, void)
	if err != nil || got != seq {
		t.Fatalf("the member's own void proposal again: seq %d, err %v; want seq %d", got, err, seq)
	}
	rows := h.proposalsAt(t, g.id, g.Epoch(), true)
	if len(rows) != 1 || rows[0].VoidAt != nil || rows[0].IssuedAt != h.clk.Now().Unix() || rows[0].TTL != 86400 ||
		rows[0].ActionID != action || rows[0].Origin != 1 || !bytes.Equal(rows[0].Ref, live.Ref) {
		t.Fatalf("rows %+v, want the member's row re-armed: live, issued now, the same action", rows)
	}

	instance := live
	instance.Origin = 0
	for _, c := range []struct {
		name     string
		existing store.ProposalRow
		device   id.ID
	}{
		{"an instance proposal", instance, g.device},
		{"another sender", live, id.New()},
	} {
		_, err := ds.MemberProposalAgainForTest(h.ds, ctx, g.id, g.Epoch(), leaf, c.device, blob, c.existing)
		var dsErr *ds.Error
		if !errors.As(err, &dsErr) || dsErr.Code != "E_REMOVE_PENDING" || dsErr.Status != 409 {
			t.Errorf("%s: got %v, want 409 E_REMOVE_PENDING", c.name, err)
		}
	}
}

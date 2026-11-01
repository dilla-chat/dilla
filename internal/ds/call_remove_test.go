package ds_test

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// putMemberRemove writes an outstanding member Remove of leaf (origin 1) straight into SQL: the
// dedupe reads SQL, and no committed fixture holds a member-signed Remove.
func (h *dsHarness) putMemberRemove(t *testing.T, groupID id.ID, leaf uint32, ttl uint64) []byte {
	t.Helper()
	ref := id.New()
	if err := h.repo.PutProposal(context.Background(), store.ProposalRow{
		GroupID: groupID, Ref: ref[:], Epoch: 6, Kind: uint8(mlswasi.ProposalRemove), TargetLeaf: &leaf,
		Origin: 1, ActionID: id.New(), IssuedAt: h.clk.Now().Unix(), TTL: ttl,
	}); err != nil {
		t.Fatalf("PutProposal: %v", err)
	}
	return ref[:]
}

func (h *dsHarness) proposals(t *testing.T, groupID id.ID, includeVoid bool) []store.ProposalRow {
	t.Helper()
	rows, err := h.repo.ListProposals(context.Background(), groupID, 6, includeVoid)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	return rows
}

// DEV-45, as corrected by the security fix: a device-addressed Remove resolves the leaf under the
// group lock and is dropped when the device holds no leaf or when a non-void INSTANCE Remove of that
// leaf already stands. A member's own Remove of the leaf does not stand in for it.
func TestProposeRemoveDeviceDropsDuplicatesAndGoneDevices(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.repo.markCall(reg.GroupID)
	dev := func(leaf uint32) id.ID { return h.memberSession(t, reg.GroupID, leaf).DeviceID }

	if err := h.ds.ProposeRemove(ctx, reg.GroupID, 1, id.New()); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, dev(1), id.New()); err != nil {
		t.Fatalf("ProposeRemoveDevice of a leaf the instance already proposes: %v", err)
	}
	h.putMemberRemove(t, reg.GroupID, 2, 30)
	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, dev(2), id.New()); err != nil {
		t.Fatalf("ProposeRemoveDevice of a leaf whose member proposed its own Remove: %v", err)
	}
	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, id.New(), id.New()); err != nil {
		t.Fatalf("ProposeRemoveDevice of a device with no leaf: %v", err)
	}
	// The instance's Remove of leaf 1, leaf 2's own Remove and the instance's Remove issued over it.
	if n := len(h.proposals(t, reg.GroupID, false)); n != 3 {
		t.Fatalf("%d live proposals, want 3: a duplicate or the gone device was proposed, or leaf 2's own Remove stood in for the instance's", n)
	}
	action := id.New()
	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, dev(3), action); err != nil {
		t.Fatalf("ProposeRemoveDevice: %v", err)
	}
	var fresh *store.ProposalRow
	for _, r := range h.proposals(t, reg.GroupID, false) {
		if r.ActionID == action {
			fresh = &r
		}
	}
	if fresh == nil || fresh.Origin != 0 || fresh.TargetLeaf == nil || *fresh.TargetLeaf != 3 {
		t.Fatalf("the fresh Remove = %+v, want an instance Remove of leaf 3 with the action id", fresh)
	}
}

// F9 / DEV-50: a call-group Remove the TTL voided while its leaf is still present is re-driven by
// the sweep, keeping its action_id. A void lifts the freeze; only a commit removes the member.
func TestAVoidedCallRemoveIsReDrivenWithItsActionID(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.repo.markCall(reg.GroupID)
	action := id.New()
	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, h.memberSession(t, reg.GroupID, 1).DeviceID, action); err != nil {
		t.Fatal(err)
	}
	first := h.proposals(t, reg.GroupID, false)
	if len(first) != 1 || first[0].TTL != 30 {
		t.Fatalf("proposals %+v, want one Remove with the 30 s call TTL", first)
	}
	h.clk.Advance(31 * time.Second)
	if _, err := ds.SweepProposalsForTest(h.ds, ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	rows := h.proposals(t, reg.GroupID, true)
	if len(rows) != 1 {
		t.Fatalf("%d rows after the re-drive, want 1 (the voided row is replaced)", len(rows))
	}
	// Within one epoch the re-signed Remove is byte-identical to the voided one (the same external
	// sender, epoch and leaf, and Ed25519 signs deterministically), so its ref is the old ref: the
	// re-drive shows as the row live again with a fresh IssuedAt (measured on the fixture, plan
	// review 2026-10-03).
	r := rows[0]
	if r.VoidAt != nil || r.ActionID != action || r.IssuedAt != h.clk.Now().Unix() || r.IssuedAt == first[0].IssuedAt {
		t.Fatalf("re-driven row %+v: want live, the same action id, issued now", r)
	}
}

// No re-drive when the leaf is gone or the group is a text group; a member's own Remove of the leaf
// does not stop one, because a member proposal never stands in for an instance proposal.
func TestNoReDriveWhenTheLeafIsGoneOrTheGroupIsTextButOverAMemberRemove(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.repo.markCall(reg.GroupID)
	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, h.memberSession(t, reg.GroupID, 1).DeviceID, id.New()); err != nil {
		t.Fatal(err)
	}
	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, h.memberSession(t, reg.GroupID, 2).DeviceID, id.New()); err != nil {
		t.Fatal(err)
	}
	h.putMemberRemove(t, reg.GroupID, 2, 3600) // leaf 2's own Remove outlives the instance's
	h.clk.Advance(31 * time.Second)
	h.dropMember(t, reg.GroupID, 1) // leaf 1 is gone before the sweep
	if _, err := ds.SweepProposalsForTest(h.ds, ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := len(h.instanceRemovesOf(t, reg.GroupID, 1)); n != 0 {
		t.Errorf("leaf 1 is gone, yet %d instance Removes of it were re-driven", n)
	}
	if n := len(h.instanceRemovesOf(t, reg.GroupID, 2)); n != 1 {
		t.Errorf("%d instance Removes of leaf 2 after the sweep, want 1: its own Remove stopped the re-drive", n)
	}

	text := newDSHarness(t)
	treg, _ := text.mustRegister(t)
	if err := text.ds.ProposeRemove(ctx, treg.GroupID, 1, id.New()); err != nil {
		t.Fatal(err)
	}
	text.clk.Advance(25 * time.Hour)
	if _, err := ds.SweepProposalsForTest(text.ds, ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rows := text.proposals(t, treg.GroupID, true); len(rows) != 1 || rows[0].VoidAt == nil {
		t.Fatalf("text group rows %+v, want the one Remove voided and not re-driven", rows)
	}
}

// DEV-49: a separate sweep at Policy.ProposalSweepInterval (5 s) runs for call groups only, so a call
// Remove is void — and, its leaf still present, re-driven — within 35 s of issue, not 30-90 s.
func TestTheCallSweeperActsWithinThirtyFiveSecondsOfIssue(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.repo.markCall(reg.GroupID)
	action := id.New()
	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, h.memberSession(t, reg.GroupID, 1).DeviceID, action); err != nil {
		t.Fatal(err)
	}
	first := h.proposals(t, reg.GroupID, false)[0]
	issued := h.clk.Now()
	if err := h.ds.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for {
		rows := h.proposals(t, reg.GroupID, true)
		// Re-driven: live again, the same action, issued after the first (the ref stays the same within
		// an epoch; see TestAVoidedCallRemoveIsReDrivenWithItsActionID).
		if len(rows) == 1 && rows[0].VoidAt == nil && rows[0].ActionID == action && rows[0].IssuedAt > first.IssuedAt {
			if late := h.clk.Since(issued); late > 36*time.Second {
				t.Fatalf("re-driven %s after issue, want at most 35 s (+1 s of Unix-second rounding)", late)
			}
			return
		}
		if h.clk.Since(issued) > 40*time.Second {
			t.Fatalf("no re-drive 40 s after issue: %+v", rows)
		}
		h.clk.Advance(time.Second)
		time.Sleep(100 * time.Millisecond) // the sweeper runs on its own goroutine
	}
}

// A call group is closed when its call ends, and the last device's leave has by then asked for the
// Remove of its leaf (CT 144: 58 closed call groups, each with an instance Remove issued the second
// it closed). Every proposal sweep walked only open groups, so that Remove stayed non-void forever
// and kept dilla_ds_proposals raised. The sweep voids the live proposals of a closed group: nobody
// can commit them, and a closed group is never reopened.
func TestTheSweepVoidsTheLiveProposalsOfAClosedGroup(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.repo.markCall(reg.GroupID)
	if err := h.ds.ProposeRemoveDevice(ctx, reg.GroupID, h.memberSession(t, reg.GroupID, 1).DeviceID, id.New()); err != nil {
		t.Fatal(err)
	}
	h.putMemberRemove(t, reg.GroupID, 2, 3600) // a member's own Remove, not yet expired
	if n := len(h.proposals(t, reg.GroupID, false)); n != 2 {
		t.Fatalf("%d live proposals before the close, want 2", n)
	}
	for _, origin := range []uint8{0, 1} {
		h.putProposalRow(t, reg.GroupID, 5, origin, h.clk.Now().Unix(), 3600)
	}
	if err := h.ds.Close(ctx, reg.GroupID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h.clk.Advance(31 * time.Second)
	if _, err := h.ds.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := ds.SweepCallProposalsForTest(h.ds, ctx); err != nil {
		t.Fatalf("call sweep: %v", err)
	}
	rows := h.proposals(t, reg.GroupID, true)
	old, err := h.repo.ListProposals(ctx, reg.GroupID, 5, true)
	if err != nil || len(old) != 2 {
		t.Fatalf("old-epoch proposals = %v, %v, want 2", old, err)
	}
	rows = append(rows, old...)
	if len(rows) != 4 {
		t.Fatalf("%d rows after the sweep, want the 4 kept (voided, not re-driven or deleted)", len(rows))
	}
	for _, r := range rows {
		if r.VoidAt == nil {
			t.Errorf("proposal %x (kind %d, origin %d, issued %d, ttl %d) is still live in a closed group",
				r.Ref[:4], r.Kind, r.Origin, r.IssuedAt, r.TTL)
		}
	}
}

// G29: every member-set writer hands the call evictor exactly the devices it removed — computed
// before mls_members is rewritten, because a removed device has no row afterwards — and the evictor
// runs after the group lock is released.
func TestTheCallEvictorGetsExactlyTheRemovedDevicesOutsideTheLock(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	var mu sync.Mutex
	var got [][]id.ID
	var lockFree []bool
	h.restartWithEvictor(func(_ context.Context, g id.ID, removed []id.ID) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, slices.Clone(removed))
		lockFree = append(lockFree, ds.GroupLockFreeForTest(h.ds, g))
	})
	var state mlswasi.GroupState
	if err := ds.WithGroupForTest(h.ds, ctx, reg.GroupID, func(g *mlswasi.PublicGroup) error {
		var err error
		state, err = g.State(ctx)
		return err
	}); err != nil {
		t.Fatalf("State: %v", err)
	}
	want := []id.ID{h.memberSession(t, reg.GroupID, 1).DeviceID, h.memberSession(t, reg.GroupID, 2).DeviceID}
	state.Members = slices.DeleteFunc(slices.Clone(state.Members), func(m mlswasi.Member) bool {
		return m.LeafIndex == 1 || m.LeafIndex == 2
	})
	if err := ds.ReplaceMembersForTest(h.ds, ctx, reg.GroupID, 1, state); err != nil {
		t.Fatalf("replace (call): %v", err)
	}
	ds.FlushEvictionsForTest(h.ds, ctx, reg.GroupID)
	mu.Lock()
	if len(got) != 1 || len(got[0]) != 2 || !lockFree[0] {
		t.Fatalf("evictions %v (lock free %v), want one call with two devices outside the lock", got, lockFree)
	}
	slices.SortFunc(got[0], func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	slices.SortFunc(want, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	if !slices.Equal(got[0], want) {
		t.Fatalf("evicted %v, want %v", got[0], want)
	}
	mu.Unlock()

	// A text group's replacement queues nothing, and a flush with nothing queued calls nobody.
	state.Members = slices.DeleteFunc(slices.Clone(state.Members), func(m mlswasi.Member) bool { return m.LeafIndex == 3 })
	if err := ds.ReplaceMembersForTest(h.ds, ctx, reg.GroupID, 0, state); err != nil {
		t.Fatalf("replace (text): %v", err)
	}
	ds.FlushEvictionsForTest(h.ds, ctx, reg.GroupID)
	ds.FlushEvictionsForTest(h.ds, ctx, reg.GroupID)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("evictions %v, want still one", got)
	}
}

// The four entry points that rewrite a group's members — Commit (through commit), Resync, Heal and
// Register — defer the flush before they take the lock, so it runs after the unlock; and every call
// of replaceMembersTx passes the group's kind. Order cannot be observed through the fixture (no
// accepted commit exists), so it is asserted in the source, as TestPrometheusInitPrecedesInitializeServer does.
func TestEveryMemberSetWriterFlushesEvictionsAfterUnlocking(t *testing.T) {
	for _, c := range []struct{ file, fn string }{
		{"commit.go", "func (d *DS) commit("},
		{"resync.go", "func (d *DS) Resync("},
		{"heal.go", "func (d *DS) HealWithGuard("},
	} {
		src, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		start := strings.Index(text, c.fn)
		if start < 0 {
			t.Fatalf("%s: %s not found", c.file, c.fn)
		}
		body := text[start:]
		if end := strings.Index(body[1:], "\nfunc "); end > 0 {
			body = body[:end+1]
		}
		flush, lock := strings.Index(body, "defer d.flushEvictions("), strings.Index(body, "d.lock(")
		if flush < 0 || lock < 0 || flush > lock {
			t.Errorf("%s %s: the flush must be deferred before the lock is taken (flush at %d, lock at %d)", c.file, c.fn, flush, lock)
		}
	}
	// Register delegates both human and managed-bot registration to one transaction helper. Keep
	// the post-unlock eviction guarantee on that shared path, and ensure the public human entry
	// point still reaches it.
	registry, err := os.ReadFile("registry.go")
	if err != nil {
		t.Fatal(err)
	}
	registryText := string(registry)
	registerStart := strings.Index(registryText, "func (d *DS) Register(")
	helperStart := strings.Index(registryText, "func (d *DS) register(")
	if registerStart < 0 || helperStart < 0 {
		t.Fatal("registry.go: public Register and shared register helper must both exist")
	}
	registerBody := registryText[registerStart:helperStart]
	if !strings.Contains(registerBody, "d.register(ctx, r, nil)") {
		t.Error("registry.go Register: human registration no longer reaches the shared acceptance path")
	}
	helperBody := registryText[helperStart:]
	if end := strings.Index(helperBody[1:], "\nfunc "); end > 0 {
		helperBody = helperBody[:end+1]
	}
	flush, lock := strings.Index(helperBody, "defer d.flushEvictions("), strings.Index(helperBody, "d.lock(")
	if flush < 0 || lock < 0 || flush > lock {
		t.Errorf("registry.go register helper: the flush must be deferred before the lock is taken (flush at %d, lock at %d)", flush, lock)
	}
	for _, file := range []string{"commit.go", "heal.go", "registry.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		for _, line := range strings.Split(string(src), "\n") {
			i := strings.Index(line, "d.replaceMembersTx(")
			if i < 0 {
				continue
			}
			calls++
			args, _, _ := strings.Cut(line[i+len("d.replaceMembersTx("):], ")")
			if strings.Count(args, ",") < 4 { // ctx, tx, groupID, kind, state[, joined...]
				t.Errorf("%s: %q does not pass the group kind", file, strings.TrimSpace(line))
			}
		}
		if file == "registry.go" {
			calls += strings.Count(registryText, "d.replaceMembersTx(")
		}
		if calls == 0 {
			t.Errorf("%s: no replaceMembersTx call found; the check passed vacuously", file)
		}
	}
}

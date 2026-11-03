package ds_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

func TestPlanBatchesNeverExceedsTheCommitCap(t *testing.T) {
	for _, n := range []int{0, 1, 255, 256, 257, 512, 1000} {
		devices := make([]id.ID, n)
		for i := range devices {
			devices[i] = id.New()
		}
		plan := ds.PlanBatches(devices, 256)
		var total int
		for i, b := range plan.Batches {
			if len(b) == 0 {
				t.Fatalf("n=%d: batch %d is empty", n, i)
			}
			if len(b) > 256 {
				t.Fatalf("n=%d: batch %d holds %d devices", n, i, len(b))
			}
			total += len(b)
		}
		if total != n {
			t.Fatalf("n=%d: batches hold %d devices", n, total)
		}
		want := (n + 255) / 256
		if len(plan.Batches) != want {
			t.Fatalf("n=%d: %d batches, want %d", n, len(plan.Batches), want)
		}
	}
}

func TestPlanBatchesIsOrderPreservingAndDeduplicated(t *testing.T) {
	a, b := id.New(), id.New()
	plan := ds.PlanBatches([]id.ID{a, b, a, b, a}, 2)
	if len(plan.Batches) != 1 || len(plan.Batches[0]) != 2 {
		t.Fatalf("plan = %+v, want one batch of two", plan)
	}
	if plan.Batches[0][0] != a || plan.Batches[0][1] != b {
		t.Fatal("PlanBatches reordered the devices")
	}
}

// eligibleDeviceWithKeyPackage is deviceWithKeyPackage for a user the channel ACL admits. A batch
// only proposes a device whose user the ACL admits, because the commit carrying the Add would
// otherwise be refused by invariant 4's own ACL clause — and a committer cannot leave an instance
// proposal out, so one ineligible Add would stall every Add beside it.
func (h *dsHarness) eligibleDeviceWithKeyPackage(t *testing.T) id.ID {
	t.Helper()
	device := h.deviceWithKeyPackage(t)
	row, err := h.repo.GetDevice(context.Background(), device)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	h.acl.allow(row.UserID)
	return device
}

// outstandingAdds is the instance Adds outstanding at the fixture group's epoch (6).
func (h *dsHarness) outstandingAdds(t *testing.T, groupID id.ID) []store.ProposalRow {
	t.Helper()
	rows, err := h.repo.ListProposals(context.Background(), groupID, 6, false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	var out []store.ProposalRow
	for _, r := range rows {
		if r.Origin == 0 && r.Kind == 1 && r.TargetDevice != nil {
			out = append(out, r)
		}
	}
	return out
}

// fillTheCommit puts MaxAddsPerCommit placeholder Adds outstanding, so a batch has no room and
// queues everything; it returns their refs, for the test to clear when it wants the room back.
func (h *dsHarness) fillTheCommit(t *testing.T, groupID id.ID) [][]byte {
	t.Helper()
	refs := make([][]byte, 0, ds.DefaultPolicy().MaxAddsPerCommit)
	for range ds.DefaultPolicy().MaxAddsPerCommit {
		refs = append(refs, h.putDSAddProposal(t, groupID, 6))
	}
	return refs
}

// Plan 1 follow-up card 8: the tail of a join storm is a row, not an entry in a map, so a restart
// between two slices of a 1,000-device storm loses nothing.
func TestAJoinStormTailSurvivesARestart(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.fillTheCommit(t, reg.GroupID)

	devices := make([]id.ID, 8)
	for i := range devices {
		devices[i] = h.eligibleDeviceWithKeyPackage(t)
	}
	if err := h.ds.ProposeAddBatch(ctx, reg.GroupID, devices); err != nil {
		t.Fatalf("ProposeAddBatch: %v", err)
	}
	if got := h.countRows(t, "pending_joins"); got != int64(len(devices)) {
		t.Fatalf("pending_joins holds %d rows, want the %d devices that did not fit", got, len(devices))
	}

	h.restartDS()
	if got := ds.PendingJoinsForTest(h.ds, reg.GroupID); got != len(devices) {
		t.Fatalf("after a restart %d devices are waiting, want %d: the tail of the storm was lost",
			got, len(devices))
	}
}

// queuedDevices is the group's pending_joins rows, in the order a drain reads them, straight from
// the database file: what a restarted instance would find.
func (h *dsHarness) queuedDevices(t *testing.T, groupID id.ID) []id.ID {
	t.Helper()
	db, err := sqlite.OpenRead(h.path)
	if err != nil {
		t.Fatalf("sqlite.OpenRead: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(t.Context(),
		"SELECT device_id FROM pending_joins WHERE group_id = ? ORDER BY queued, device_id", groupID)
	if err != nil {
		t.Fatalf("reading pending_joins: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []id.ID
	for rows.Next() {
		var d id.ID
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scanning pending_joins: %v", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading pending_joins: %v", err)
	}
	return out
}

// A drain that dies in the middle of a slice — a restart, an OOM kill — after it read the slice
// and before it proposed every device in it loses none of the devices it had not yet resolved:
// a device leaves pending_joins only once its Add is stored or it has been judged ineligible, so
// the rows the dead drain never reached are still there for the sweeper after the restart.
// Requeue-on-error covers only a fault the code sees; a crash runs no error path.
func TestADrainThatDiesMidSliceLeavesTheUnresolvedDevicesQueued(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	placeholders := h.fillTheCommit(t, reg.GroupID)

	devices := make([]id.ID, 5)
	for i := range devices {
		devices[i] = h.eligibleDeviceWithKeyPackage(t)
	}
	if err := h.ds.ProposeAddBatch(ctx, reg.GroupID, devices); err != nil {
		t.Fatalf("ProposeAddBatch: %v", err)
	}
	// Queued in one second, so the drain reads them in device-id order.
	order := slices.Clone(devices)
	slices.SortFunc(order, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	if got := h.queuedDevices(t, reg.GroupID); !slices.Equal(got, order) {
		t.Fatalf("queued %v, want all five in device-id order %v", got, order)
	}

	// The process dies while the drain asks about the third device of the slice: the first two
	// are resolved by then (the first proposed, the second resolved one way or the other), the
	// last three are not.
	third, err := h.repo.GetDevice(ctx, order[2])
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	h.acl.mu.Lock()
	h.acl.crashOn = &third.UserID
	h.acl.mu.Unlock()
	if err := h.repo.DeleteProposals(ctx, reg.GroupID, placeholders); err != nil {
		t.Fatalf("DeleteProposals: %v", err)
	}
	func() {
		defer func() {
			r := recover()
			if err, ok := r.(error); !ok || !errors.Is(err, errSimulatedCrash) {
				t.Fatalf("the drain ended with %v, want the simulated crash", r)
			}
		}()
		_ = h.ds.ProposeAddBatch(ctx, reg.GroupID, nil)
	}()
	h.acl.mu.Lock()
	h.acl.crashOn = nil
	h.acl.mu.Unlock()

	if got := h.queuedDevices(t, reg.GroupID); !slices.Equal(got, order[2:]) {
		t.Fatalf("after the crash pending_joins holds %v, want the three devices the drain never "+
			"resolved %v: a device left the queue before its Add was stored", got, order[2:])
	}
	// The first device's Add is among them. Not necessarily first in the list: outstanding
	// proposals issued in one second are listed by ref, and with each device's own KeyPackage the
	// refs of the two Adds are unrelated to the devices' order.
	adds := h.outstandingAdds(t, reg.GroupID)
	firstProposed := false
	for _, a := range adds {
		if a.TargetDevice != nil && *a.TargetDevice == order[0] {
			firstProposed = true
		}
	}
	if !firstProposed {
		t.Fatalf("%d outstanding Adds, none for the first device: the drain did run before it died", len(adds))
	}

	// And the restarted instance still has them to drain.
	h.restartDS()
	if got := ds.PendingJoinsForTest(h.ds, reg.GroupID); got != 3 {
		t.Fatalf("after the restart %d devices are waiting, want 3", got)
	}
}

// stillEligible is re-read when a slice is drained, not when the batch was planned: a device
// revoked, a user the channel no longer admits, a device already in the group and one with no
// KeyPackage left are all dropped from the queue — dropped, not kept for a later slice — and the
// one device still eligible is proposed.
func TestADrainDropsWhatStoppedBeingEligibleSinceTheBatchWasPlanned(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	placeholders := h.fillTheCommit(t, reg.GroupID)

	eligible := h.eligibleDeviceWithKeyPackage(t)
	revoked := h.eligibleDeviceWithKeyPackage(t)
	unadmitted := h.deviceWithKeyPackage(t) // the ACL never admits its user
	noKeyPackage := h.eligibleDeviceWithKeyPackage(t)

	// A device that is already a live leaf of the group, with a KeyPackage it could spend.
	members, err := h.repo.ListMembers(ctx, reg.GroupID)
	if err != nil || len(members) == 0 {
		t.Fatalf("ListMembers: %v", err)
	}
	leaf := members[0]
	h.account(t, leaf.UserID, leaf.DeviceID)
	h.seedKeyPackages(t, leaf.DeviceID, 1, 24*time.Hour)

	if err := h.ds.ProposeAddBatch(ctx, reg.GroupID,
		[]id.ID{revoked, unadmitted, leaf.DeviceID, noKeyPackage, eligible}); err != nil {
		t.Fatalf("ProposeAddBatch: %v", err)
	}
	if got := ds.PendingJoinsForTest(h.ds, reg.GroupID); got != 5 {
		t.Fatalf("%d devices queued, want all 5: the commit had no room", got)
	}

	// Between the batch and the drain: one device is revoked, one spends its only KeyPackage.
	if err := h.repo.RevokeDevice(ctx, revoked, h.clk.Now().Unix()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if _, err := h.repo.TakeKeyPackage(ctx, noKeyPackage, h.clk.Now().Unix()); err != nil {
		t.Fatalf("TakeKeyPackage: %v", err)
	}
	// The commit that applied the placeholders lands, and the next slice is drained.
	if err := h.repo.DeleteProposals(ctx, reg.GroupID, placeholders); err != nil {
		t.Fatalf("DeleteProposals: %v", err)
	}
	if err := h.ds.ProposeAddBatch(ctx, reg.GroupID, nil); err != nil {
		t.Fatalf("ProposeAddBatch (drain): %v", err)
	}

	adds := h.outstandingAdds(t, reg.GroupID)
	if len(adds) != 1 || *adds[0].TargetDevice != eligible {
		targets := make([]id.ID, 0, len(adds))
		for _, a := range adds {
			targets = append(targets, *a.TargetDevice)
		}
		t.Fatalf("the drain proposed %v, want exactly the one eligible device %s", targets, eligible)
	}
	if got := ds.PendingJoinsForTest(h.ds, reg.GroupID); got != 0 {
		t.Fatalf("%d devices still queued, want 0: an ineligible device is dropped, not re-queued", got)
	}
	if n := h.ordinaryKeyPackagesLeft(t, leaf.DeviceID); n != 1 {
		t.Fatalf("the live leaf's device has %d KeyPackages left, want 1: an Add was built for a "+
			"device already in the group", n)
	}
}

// A device whose Add is already outstanding is not proposed a second time: the second proposal
// would spend a second KeyPackage and, committed, put two leaves of one device in the group.
func TestABatchDoesNotReproposeADeviceWhoseAddIsOutstanding(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	device := h.eligibleDeviceWithKeyPackage(t)
	h.seedKeyPackages(t, device, 1, 24*time.Hour) // a second package it could spend

	if err := h.ds.ProposeAddBatch(ctx, reg.GroupID, []id.ID{device}); err != nil {
		t.Fatalf("ProposeAddBatch: %v", err)
	}
	if got := len(h.outstandingAdds(t, reg.GroupID)); got != 1 {
		t.Fatalf("%d Adds outstanding after the first batch, want 1", got)
	}
	left := h.ordinaryKeyPackagesLeft(t, device)

	if err := h.ds.ProposeAddBatch(ctx, reg.GroupID, []id.ID{device}); err != nil {
		t.Fatalf("ProposeAddBatch (again): %v", err)
	}
	if got := len(h.outstandingAdds(t, reg.GroupID)); got != 1 {
		t.Fatalf("%d Adds outstanding after the second batch, want still 1", got)
	}
	if n := h.ordinaryKeyPackagesLeft(t, device); n != left {
		t.Fatalf("the second batch spent a KeyPackage (%d left, want %d)", n, left)
	}
	if got := ds.PendingJoinsForTest(h.ds, reg.GroupID); got != 0 {
		t.Fatalf("%d devices queued, want 0", got)
	}
}

// stillEligible is the one-device form of the rule the drain applies per slice.
func TestStillEligibleAnswersForOneDevice(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	now := h.clk.Now().Unix()

	device := h.eligibleDeviceWithKeyPackage(t)
	if ok, err := ds.StillEligibleForTest(h.ds, ctx, reg.GroupID, device, now); err != nil || !ok {
		t.Fatalf("stillEligible(eligible) = %v, %v; want true", ok, err)
	}
	if err := h.repo.RevokeDevice(ctx, device, now); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if ok, err := ds.StillEligibleForTest(h.ds, ctx, reg.GroupID, device, now); err != nil || ok {
		t.Fatalf("stillEligible(revoked) = %v, %v; want false", ok, err)
	}
	if ok, err := ds.StillEligibleForTest(h.ds, ctx, reg.GroupID, id.New(), now); err != nil || ok {
		t.Fatalf("stillEligible(unknown device) = %v, %v; want false", ok, err)
	}
}

// The sweeper re-drives a storm no commit re-drives: here the storm's own Adds were voided by
// invariant 6's TTL, which frees the room, and the next tick proposes the waiting device.
func TestTheSweeperDrainsAStormWhoseAddsWereVoided(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.fillTheCommit(t, reg.GroupID)
	waiting := h.eligibleDeviceWithKeyPackage(t)
	if err := h.ds.ProposeAddBatch(ctx, reg.GroupID, []id.ID{waiting}); err != nil {
		t.Fatalf("ProposeAddBatch: %v", err)
	}
	if got := ds.PendingJoinsForTest(h.ds, reg.GroupID); got != 1 {
		t.Fatalf("%d devices queued, want 1", got)
	}

	h.clk.Advance(25 * time.Hour) // past the text group's 24-hour proposal TTL
	report, err := h.ds.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.JoinsDrained != 1 {
		t.Fatalf("the sweep drained %d devices, want 1", report.JoinsDrained)
	}
	adds := h.outstandingAdds(t, reg.GroupID)
	var found bool
	for _, a := range adds {
		if *a.TargetDevice == waiting && a.VoidAt == nil {
			found = true
		}
	}
	if !found {
		t.Fatal("the waiting device was not proposed after its storm's Adds were voided")
	}
}

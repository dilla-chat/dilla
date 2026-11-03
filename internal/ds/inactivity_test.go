package ds_test

// protocol/01 § Cadence: "a device that has not connected for 90 days is removed from every group
// by a DS Remove proposal". The connection is the gateway's `ready` (gateway_touch_test.go in
// internal/gateway records it as the device's last_seen); the removal is the sweep's.

import (
	"context"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

// A member device unseen for more than 90 days gets one instance Remove of its leaf; a member seen
// inside the window keeps its leaf, and a second sweep does not propose the same removal twice.
func TestADeviceUnseenForNinetyDaysIsRemovedByTheInstance(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.groupWithMembers(t, 2)
	for i := range 2 {
		h.account(t, g.sessionOf(i).UserID, g.members[i])
	}

	h.clk.Advance(91 * 24 * time.Hour)
	// members[0] connected yesterday; members[1] has not connected since the account was made.
	if err := h.repo.TouchDevice(ctx, g.members[0], h.clk.Now().Add(-24*time.Hour).Unix()); err != nil {
		t.Fatalf("TouchDevice: %v", err)
	}

	report, err := h.ds.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.InactiveRemoved < 1 {
		t.Fatalf("the sweep proposed %d inactivity Removes, want at least the unseen member's",
			report.InactiveRemoved)
	}
	targets := removeTargets(t, h, g)
	if !targets[g.leafOf(1)] {
		t.Fatalf("no instance Remove targets the unseen member's leaf %d: %v", g.leafOf(1), targets)
	}
	if targets[g.leafOf(0)] {
		t.Fatal("a member that connected yesterday must keep its leaf")
	}

	before := len(targets)
	if _, err := h.ds.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if after := len(removeTargets(t, h, g)); after != before {
		t.Fatalf("a second sweep changed the outstanding Removes from %d to %d; one is enough",
			before, after)
	}
}

// A device that connects is seen: the gateway's `ready` records last_seen, so a device that comes
// back after 91 days of silence is not removed by the sweep that follows its connection.
func TestADeviceThatConnectsIsNotRemovedForInactivity(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.groupWithMembers(t, 1)
	h.account(t, g.sessionOf(0).UserID, g.members[0])
	h.clk.Advance(91 * 24 * time.Hour)
	h.online(g.members[0])

	if _, err := h.ds.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removeTargets(t, h, g)[g.leafOf(0)] {
		t.Fatal("a device that has just connected was proposed for removal for inactivity")
	}
}

// DS-2 of the server-half review: the sweep reads its members outside the group lock, so the leaf
// it read can hold another device by the time the Remove is built (a commit that removed the
// unseen device and added a newcomer at its index). The Remove names the device the sweep read and
// is refused when the leaf holds another one: the newcomer keeps its leaf.
func TestAnInactivityRemoveOfALeafReusedSinceTheReadIsRefused(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.groupWithMembers(t, 2)
	for i := range 2 {
		h.account(t, g.sessionOf(i).UserID, g.members[i])
	}
	h.clk.Advance(91 * 24 * time.Hour)
	if err := h.repo.TouchDevice(ctx, g.members[0], h.clk.Now().Add(-24*time.Hour).Unix()); err != nil {
		t.Fatalf("TouchDevice: %v", err)
	}
	newcomer := id.New()
	h.repo.reuseLeafAfterNextRead(t, g.id, g.leafOf(1), newcomer)
	if _, err := ds.RemoveInactiveForTest(h.ds, ctx); err != nil {
		t.Fatalf("removeInactive: %v", err)
	}
	for _, r := range h.instanceRemovesOf(t, g.id, g.leafOf(1)) {
		if r.TargetDevice != nil && *r.TargetDevice == newcomer {
			t.Fatalf("the inactivity Remove of %s landed on %s, which took leaf %d after the sweep read it",
				g.members[1].String()[:8], newcomer.String()[:8], g.leafOf(1))
		}
	}
	if n := len(h.instanceRemovesOf(t, g.id, g.leafOf(1))); n != 0 {
		t.Fatalf("%d instance Removes of the reused leaf, want none", n)
	}
}

// removeTargets is every leaf an outstanding non-void instance Remove names at the group's epoch.
func removeTargets(t *testing.T, h *dsHarness, g *dsGroup) map[uint32]bool {
	t.Helper()
	rows, err := h.repo.ListProposals(context.Background(), g.id, g.epoch(t), false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	out := map[uint32]bool{}
	for _, r := range rows {
		if r.Origin == 0 && r.Kind == uint8(mlswasi.ProposalRemove) && r.TargetLeaf != nil {
			out[*r.TargetLeaf] = true
		}
	}
	return out
}

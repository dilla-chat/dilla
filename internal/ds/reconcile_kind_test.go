package ds_test

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

// M-1 of the integration re-review: a quarantined or revoked device's leaf is owed an instance Remove
// only in the group kinds that carry the instance as an external sender — text (0) and call (1),
// protocol/01 § External senders. A pairing (2) or interaction (3) group carries none, so its members
// would reject such a Remove and the guest refuses it: the reconcile proposes nothing there (and so
// logs no refused Remove), and the group's own members remove a revoked device (01, member
// Removes: a device revocation).
func TestTheReconcileProposesNoBarredRemoveInAGroupWithoutAnExternalSender(t *testing.T) {
	for _, kind := range []int{2, 3} {
		h := newDSHarness(t)
		ctx := context.Background()
		reg, _ := h.mustRegister(t)
		barred := h.memberSession(t, reg.GroupID, 1)
		h.account(t, barred.UserID, barred.DeviceID)
		if err := h.repo.RevokeDevice(ctx, barred.DeviceID, h.clk.Now().Unix()); err != nil {
			t.Fatalf("RevokeDevice: %v", err)
		}
		// The fixture group's binding is a text group's; the reconcile reads the kind the store
		// records, which a pairing or interaction registration writes.
		db, err := sqlite.OpenWrite(h.path)
		if err != nil {
			t.Fatalf("sqlite.OpenWrite: %v", err)
		}
		if _, err := db.ExecContext(ctx, "UPDATE mls_groups SET kind = ? WHERE group_id = ?", kind, reg.GroupID[:]); err != nil {
			t.Fatalf("set the group's kind: %v", err)
		}
		_ = db.Close()
		n, err := ds.ReconcileLeavesForTest(h.ds, ctx)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if n != 0 || len(h.instanceRemovesOf(t, reg.GroupID, 1)) != 0 {
			t.Fatalf("kind %d: the reconcile proposed %d Removes (%d of the revoked leaf), want none", kind, n,
				len(h.instanceRemovesOf(t, reg.GroupID, 1)))
		}
	}
}

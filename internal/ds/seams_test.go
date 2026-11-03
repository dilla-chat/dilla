package ds_test

import (
	"context"
	"testing"

	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
)

// Options carries the three injected seams of interfaces.md §6.2 — Channels, ACL and DeviceLists
// — and New defaults each of them to its Plan-1 stub. The defaults are what makes a DS built by a
// composition root that knows nothing about Plan 2 refuse rather than pass: an absent ACL would
// be a nil interface at invariant 4's first call site, and a nil check there is one someone has
// to remember to write.
func TestNewDefaultsTheInjectedSeamsToTheirPlan1Stubs(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegister(t)
	ctx := context.Background()

	// NV-B6: the Plan-1 ACL admits a user the instance can already see in the group, and nobody
	// else. The member here is read back out of mls_members, so the identity is the one the
	// guest's own credential carried.
	acl := ds.ACLForTest(h.ds)
	if acl == nil {
		t.Fatal("New left Options.ACL nil; invariant 4's eligibility clause would be a nil call")
	}
	ok, err := acl.Eligible(ctx, reg.GroupID, session.UserID)
	if err != nil {
		t.Fatalf("Eligible for a member: %v", err)
	}
	if !ok {
		t.Error("a user already in the group was refused; DenyUnlessMember admits exactly those")
	}
	ok, err = acl.Eligible(ctx, reg.GroupID, id.New())
	if err != nil {
		t.Fatalf("Eligible for a stranger: %v", err)
	}
	if ok {
		t.Error("a user the instance cannot see in the group was declared eligible")
	}

	// NV-B8: the device-list verifier fails closed for a user who has published no list.
	// Returning entries — or no error — would let an Add past invariant 4's DSK clause on a list
	// nobody signed. devicelist_test.go covers the lists that do verify.
	lists := ds.DeviceListsForTest(h.ds)
	if lists == nil {
		t.Fatal("New left Options.DeviceLists nil; invariant 4's DSK clause would be a nil call")
	}
	entries, err := lists.Entries(ctx, nil, session.UserID)
	if err == nil {
		t.Errorf("a user with no device list answered %d entries; it must fail closed", len(entries))
	}
	if entries != nil {
		t.Errorf("a user with no device list returned %d entries beside its error", len(entries))
	}
}

// And a DS built with nothing but a store gets the same defaults: the stubs are New's, not the
// harness's.
func TestADeliveryServiceBuiltWithoutSeamsStillHasThem(t *testing.T) {
	h := newDSHarness(t)
	d, err := ds.New(ds.Options{Store: h.repo, Wasm: h.wasm, Clock: h.clk})
	if err != nil {
		t.Fatalf("ds.New: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	if ds.ACLForTest(d) == nil || ds.DeviceListsForTest(d) == nil {
		t.Fatal("New left an injected seam nil")
	}
}

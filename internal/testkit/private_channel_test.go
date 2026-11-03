package testkit_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/testkit"
)

// Registering a channel's text group through the WIRED instance populates it (Plan 2 task 7, fix
// round 1): the composition root sets api.Groups.AfterRegister to api.SyncRegisteredGroup, which
// runs after the 201 and proposes every eligible device that is not already a leaf. Real clients
// register a real group; the channel row and its members come from the scenario's
// `channel … members=alice,bob`. What is asserted is the instance's own store: one outstanding
// instance Add, for bob's device, and none for alice, whose device is the group's only leaf.
func TestRegisteringAChannelGroupThroughTheInstanceProposesItsMembers(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
	t.Cleanup(h.Stop)
	result := h.Run(t, filepath.Join("..", "..", "testkit", "scenarios",
		"private_channel_populated_on_registration.scn"))
	if result.Err != nil {
		t.Fatalf("scenario: %v\nstdout:\n%s\nstderr:\n%s", result.Err, result.Stdout, result.Stderr)
	}

	ctx := context.Background()
	repo := h.Repo()
	group, err := id.Parse("f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7")
	if err != nil {
		t.Fatalf("id.Parse: %v", err)
	}
	bob, err := repo.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatalf("GetUserByUsername(bob): %v", err)
	}
	bobDevices, err := repo.ListDevicesByUser(ctx, bob.ID)
	if err != nil || len(bobDevices) != 1 {
		t.Fatalf("bob's devices = %v, %v; want one", bobDevices, err)
	}

	// The hook runs after the answer, so the Add may land a moment after the scenario ended.
	deadline := time.Now().Add(15 * time.Second)
	var adds []store.ProposalRow
	for {
		row, err := repo.GetGroup(ctx, group)
		if err != nil {
			t.Fatalf("GetGroup: %v", err)
		}
		proposals, err := repo.ListProposals(ctx, group, row.Epoch, false)
		if err != nil {
			t.Fatalf("ListProposals: %v", err)
		}
		adds = adds[:0]
		for _, p := range proposals {
			if p.Origin == 0 && p.Kind == 1 && p.VoidAt == nil && p.TargetDevice != nil {
				adds = append(adds, p)
			}
		}
		if len(adds) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(adds) != 1 || *adds[0].TargetDevice != bobDevices[0].ID {
		t.Fatalf("outstanding instance Adds = %+v, want exactly one, for bob's device %s: the "+
			"registered channel group was not populated", adds, bobDevices[0].ID)
	}
	if n, err := repo.CountPendingJoins(ctx, group); err != nil || n != 0 {
		t.Fatalf("pending_joins = %d, %v; want the one device proposed and nothing left queued", n, err)
	}
}

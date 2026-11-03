package testkit_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/testkit"
)

// Fix wave I6: the kick and the storm scenarios the plan's tasks 4 and 7 own, run against an
// instance wired as production wires it (api.ResolverACL and api.StructureChannels, a real
// community with a real channel) rather than the harness's admit-everyone seams, so a regression
// in RemoveUserFromCommunityGroups, the resolver, SyncGroupMembers or the fix-wave C2/C3 voids
// fails a scenario instead of passing every one. The scenario runs green, and the instance's own
// store is then read for what the clients cannot see.

// productionScenario runs one scenario against a production-ACL harness and fails the test on
// any scenario failure.
func productionScenario(t *testing.T, name string) *testkit.Harness {
	t.Helper()
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir(), ProductionACL: true})
	t.Cleanup(h.Stop)
	result := h.Run(t, filepath.Join("..", "..", "testkit", "scenarios", name))
	if result.Err != nil {
		t.Fatalf("%s failed: %v\nstdout:\n%s\nstderr:\n%s", name, result.Err, result.Stdout, result.Stderr)
	}
	if result.Steps == 0 {
		t.Fatalf("%s ran no steps", name)
	}
	t.Logf("%s", result.Stdout)
	return h
}

// textGroupOf is the one open text group bound to the scenario's channel.
func textGroupOf(t *testing.T, repo store.Repository, channel string) store.GroupRow {
	t.Helper()
	target, err := id.Parse(channel)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := repo.GroupsForTarget(context.Background(), target, api.GroupText)
	if err != nil || len(groups) != 1 {
		t.Fatalf("open text groups of %s = %v, %v; want one", channel, groups, err)
	}
	return groups[0]
}

// deviceOf is the one device of a scenario client, by its username.
func deviceOf(t *testing.T, repo store.Repository, username string) store.DeviceRow {
	t.Helper()
	ctx := context.Background()
	u, err := repo.GetUserByUsername(ctx, username)
	if err != nil {
		t.Fatalf("user %s: %v", username, err)
	}
	devices, err := repo.ListDevicesByUser(ctx, u.ID)
	if err != nil || len(devices) != 1 {
		t.Fatalf("devices of %s = %v, %v; want one", username, devices, err)
	}
	return devices[0]
}

// addsFor is every instance Add ever proposed in g for device, void or not, across every epoch.
func addsFor(t *testing.T, repo store.Repository, g store.GroupRow, device id.ID) []store.ProposalRow {
	t.Helper()
	var out []store.ProposalRow
	for epoch := uint64(0); epoch <= g.Epoch; epoch++ {
		rows, err := repo.ListProposals(context.Background(), g.GroupID, epoch, true)
		if err != nil {
			t.Fatalf("ListProposals(%d): %v", epoch, err)
		}
		for _, p := range rows {
			if p.Origin == 0 && p.Kind == 1 && p.TargetDevice != nil && *p.TargetDevice == device {
				out = append(out, p)
			}
		}
	}
	return out
}

// currentLeaf reports whether device holds a leaf of g that has not been removed.
func currentLeaf(t *testing.T, repo store.Repository, g store.GroupRow, device id.ID) bool {
	t.Helper()
	members, err := repo.ListMembers(context.Background(), g.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	for _, m := range members {
		if m.DeviceID == device && m.RemovedEpoch == nil {
			return true
		}
	}
	return false
}

func TestAKickWithAnOutstandingAddRunsThroughTheProductionACL(t *testing.T) {
	h := productionScenario(t, "kick_with_outstanding_add_production_acl.scn")
	repo := h.Repo()
	g := textGroupOf(t, repo, "c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4c4")

	carol := deviceOf(t, repo, "carol")
	adds := addsFor(t, repo, g, carol.ID)
	if len(adds) == 0 {
		t.Fatal("carol never had an instance Add: the scenario did not reach the C2 case")
	}
	for _, p := range adds {
		if p.VoidAt == nil {
			t.Errorf("carol's Add at epoch %d is not void after her kick", p.Epoch)
		}
	}
	if currentLeaf(t, repo, g, carol.ID) {
		t.Error("carol, kicked with her Add outstanding, holds a leaf")
	}
	if currentLeaf(t, repo, g, deviceOf(t, repo, "bob").ID) {
		t.Error("bob, kicked while offline, is still a current leaf")
	}
	for _, name := range []string{"alice", "dave"} {
		if !currentLeaf(t, repo, g, deviceOf(t, repo, name).ID) {
			t.Errorf("%s lost their leaf", name)
		}
	}
}

// Fix wave I19: a DM participant who publishes their first KeyPackage after the DM's group exists
// is added by the instance, through the composition root's AfterKeyPackages hook.
func TestAKeyPackagePublishAddsTheDeviceToItsDMs(t *testing.T) {
	h := productionScenario(t, "dm_member_added_on_key_package_publish.scn")
	repo := h.Repo()
	ctx := context.Background()
	if err := h.Server().DrainHooks(ctx); err != nil {
		t.Fatalf("DrainHooks: %v", err)
	}
	g := textGroupOf(t, repo, "d7d7d7d7d7d7d7d7d7d7d7d7d7d7d7d7")
	bob := deviceOf(t, repo, "bob")
	live := 0
	for _, p := range addsFor(t, repo, g, bob.ID) {
		if p.VoidAt == nil && p.Epoch == g.Epoch {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("outstanding instance Adds for bob's device = %d, want 1: publishing his first "+
			"KeyPackage did not add him to the DM", live)
	}
}

func TestAStormWithViewRevokedMidwayRunsThroughTheProductionACL(t *testing.T) {
	h := productionScenario(t, "join_storm_revoked_mid_storm_production_acl.scn")
	repo := h.Repo()
	g := textGroupOf(t, repo, "c6c6c6c6c6c6c6c6c6c6c6c6c6c6c6c6")

	// 298 joiners at most 256 an epoch: two commits, both accepted.
	if n := h.CommitCount(); n != 2 {
		t.Errorf("the instance accepted %d commits, want 2 (298 joiners, 256 a commit)", n)
	}
	if n, err := repo.CountPendingJoins(context.Background(), g.GroupID); err != nil || n != 0 {
		t.Errorf("pending_joins = %d, %v; want the queue drained", n, err)
	}
	for _, name := range []string{"chat-3", "chat-300"} {
		dev := deviceOf(t, repo, name)
		if currentLeaf(t, repo, g, dev.ID) {
			t.Errorf("%s, whose view was revoked mid-storm, holds a leaf", name)
		}
		for _, p := range addsFor(t, repo, g, dev.ID) {
			if p.VoidAt == nil {
				t.Errorf("%s has a live Add at epoch %d", name, p.Epoch)
			}
		}
	}
	members, err := repo.ListMembers(context.Background(), g.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, m := range members {
		if m.RemovedEpoch == nil {
			live++
		}
	}
	if live != 299 {
		t.Errorf("%d current leaves, want 299 (the owner and 298 joiners)", live)
	}
}

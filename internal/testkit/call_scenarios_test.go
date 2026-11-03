package testkit_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/testkit"
)

// dilla-media task 9 and its security fix: DEV-45's two Removes of one call leaf in both orders
// (SP-22 b) — a member's own Remove never cancels, blocks or stands in for the instance's — and F9's
// re-drive of a voided call Remove, against a real instance with real clients: the commit-path
// halves no DS unit test can reach with the committed fixture.
func TestTheCallRemoveScenariosRunGreen(t *testing.T) {
	for _, name := range []string{
		"call_remove_dedupe.scn", "call_remove_redrive.scn", "call_remove_refused_leave.scn",
		"call_remove_member_remove_committed.scn",
	} {
		t.Run(name, func(t *testing.T) {
			h, result := runCallScenario(t, name)
			if result.Steps == 0 {
				t.Fatal("the scenario ran no steps")
			}
			switch name {
			case "call_remove_refused_leave.scn":
				assertTheKickStillStands(t, h)
			case "call_remove_member_remove_committed.scn":
				assertTheMemberRemoveSatisfiedTheKick(t, h)
			}
		})
	}
}

// theCallGroup is the one open call group a call scenario leaves on the instance.
func theCallGroup(t *testing.T, h *testkit.Harness) store.GroupRow {
	t.Helper()
	groups, err := h.Repo().ListOpenGroups(context.Background(), id.ID{}, 64)
	if err != nil {
		t.Fatalf("ListOpenGroups: %v", err)
	}
	var call *store.GroupRow
	for i := range groups {
		if groups[i].Kind == 1 {
			call = &groups[i]
		}
	}
	if call == nil {
		t.Fatal("no call group on the instance")
	}
	return *call
}

// assertTheMemberRemoveSatisfiedTheKick is (3a) of the task-9 review, read from the instance after
// call_remove_member_remove_committed.scn: alice's commit applied carol's own Remove, not the
// instance's, and was accepted. The instance's Remove was deleted with the commit — no row is left
// at the epoch the commit left behind, void or not, for a re-issue to read — nothing was issued at
// the new epoch, carol holds no leaf, and the election the kick started is over: the committer won
// its round and was charged none.
func assertTheMemberRemoveSatisfiedTheKick(t *testing.T, h *testkit.Harness) {
	t.Helper()
	ctx := context.Background()
	repo := h.Repo()
	call := theCallGroup(t, h)
	for _, epoch := range []uint64{call.Epoch - 1, call.Epoch} {
		rows, err := repo.ListProposals(ctx, call.GroupID, epoch, true)
		if err != nil {
			t.Fatalf("ListProposals(%d): %v", epoch, err)
		}
		for _, r := range rows {
			if r.Origin == 0 {
				t.Fatalf("an instance proposal is left at epoch %d (the call is at %d): %+v", epoch, call.Epoch, r)
			}
		}
	}
	members, err := repo.ListMembers(ctx, call.GroupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	current := 0
	for _, m := range members {
		if m.RemovedEpoch == nil {
			current++
		}
	}
	if current != 2 {
		t.Fatalf("%d current members of the call, want alice and bob: %+v", current, members)
	}
	if n := h.DS().OpenElections(); n != 0 {
		t.Fatalf("%d open elections, want none: the commit won the kick's round", n)
	}
}

func runCallScenario(t *testing.T, name string) (*testkit.Harness, testkit.ScenarioResult) {
	t.Helper()
	path := filepath.Join("..", "..", "testkit", "scenarios", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("scenario %s is missing: %v", name, err)
	}
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
	t.Cleanup(h.Stop)
	result := h.Run(t, path)
	if result.Err != nil {
		t.Fatalf("%s failed: %v\nstdout:\n%s\nstderr:\n%s", result.Name, result.Err, result.Stdout, result.Stderr)
	}
	t.Logf("%s", result.Stdout)
	return h, result
}

// assertTheKickStillStands is D(i) of the security fix, read from the instance after
// call_remove_refused_leave.scn: carol's own Remove was refused — no member proposal reached the
// handshake log or the proposal table — the instance's Remove of her leaf is still the one
// non-void proposal, and the election it started still stands.
func assertTheKickStillStands(t *testing.T, h *testkit.Harness) {
	t.Helper()
	ctx := context.Background()
	repo := h.Repo()
	call := theCallGroup(t, h)
	rows, err := repo.ListProposals(ctx, call.GroupID, call.Epoch, true)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d proposals at the call's epoch, want only the instance's Remove: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Origin != 0 || mlswasi.ProposalKind(r.Kind) != mlswasi.ProposalRemove || r.VoidAt != nil || r.TargetDevice == nil {
		t.Fatalf("the proposal %+v, want the instance's non-void Remove of carol's device", r)
	}
	handshakes, err := repo.ListHandshakes(ctx, call.GroupID, 0, 512)
	if err != nil {
		t.Fatalf("ListHandshakes: %v", err)
	}
	for _, hs := range handshakes {
		if hs.Epoch == call.Epoch && hs.Kind == 0 && hs.SenderLeaf != nil {
			t.Fatalf("a member proposal reached the log at seq %d: carol's own Remove was accepted", hs.Seq)
		}
	}
	if n := h.DS().OpenElections(); n != 1 {
		t.Fatalf("%d open elections, want the one the kick started", n)
	}
}

package testkit_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/testkit"
)

// runDSScenario runs one scenario of testkit/scenarios on an instance of its own and fails the test
// on a failed or empty run. The harness is returned for assertions on the instance afterwards.
func runDSScenario(t *testing.T, name string) *testkit.Harness {
	t.Helper()
	path := filepath.Join("..", "..", "testkit", "scenarios", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("scenario missing: %v", err)
	}
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
	t.Cleanup(h.Stop)
	result := h.Run(t, path)
	if result.Err != nil {
		t.Fatalf("%s failed: %v\nstdout:\n%s\nstderr:\n%s", result.Name, result.Err, result.Stdout, result.Stderr)
	}
	if result.Steps == 0 {
		t.Fatal("the scenario ran no steps")
	}
	return h
}

// theTextGroup is the one open text group a scenario leaves on the instance.
func theTextGroup(t *testing.T, h *testkit.Harness) store.GroupRow {
	t.Helper()
	groups, err := h.Repo().ListOpenGroups(context.Background(), id.ID{}, 64)
	if err != nil {
		t.Fatalf("ListOpenGroups: %v", err)
	}
	var text *store.GroupRow
	for i := range groups {
		if groups[i].Kind == 0 {
			if text != nil {
				t.Fatal("more than one open text group on the instance")
			}
			text = &groups[i]
		}
	}
	if text == nil {
		t.Fatal("no text group on the instance")
	}
	return *text
}

// memberProposalHandshakes counts the group's logged member proposals (kind 0 with a sender leaf):
// one per upload the instance accepted and fanned out.
func memberProposalHandshakes(t *testing.T, h *testkit.Harness, groupID id.ID) int {
	t.Helper()
	rows, err := h.Repo().ListHandshakes(context.Background(), groupID, 0, 512)
	if err != nil {
		t.Fatalf("ListHandshakes: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Kind == 0 && r.SenderLeaf != nil {
			n++
		}
	}
	return n
}

// DS-1 of the server-half review, end to end: an instance Remove the 24 h TTL voided and the
// inactivity sweep re-proposed in the same epoch is re-armed, freezes the group again, and a real
// client's commit referencing it is accepted — the commit half no DS unit test can reach with the
// committed fixture.
func TestAVoidedRemoveReArmedInItsEpochIsCommitted(t *testing.T) {
	runDSScenario(t, "inactivity_remove_rearmed_after_void.scn")
}

// liveMembers is how many leaves the group holds now.
func liveMembers(t *testing.T, h *testkit.Harness, groupID id.ID) int {
	t.Helper()
	ms, err := h.Repo().ListMembers(context.Background(), groupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	return len(ms)
}

// POST /proposal, the identical duplicate: carol's second upload of her own Remove in the same epoch
// is answered as the first, so carol keeps it queued (a 409 would make her withdraw it, and her
// `sync` of the commit that applies it would then fail on the missing proposal); the log holds the
// proposal once — it was written and fanned out once — and alice's commit removes carol.
func TestAMemberProposalUploadedTwiceIsAnsweredAsTheFirst(t *testing.T) {
	h := runDSScenario(t, "member_proposal_uploaded_twice.scn")
	g := theTextGroup(t, h)
	if n := memberProposalHandshakes(t, h, g.GroupID); n != 1 {
		t.Fatalf("%d member proposals logged, want carol's one: the second upload was written again", n)
	}
	if n := liveMembers(t, h, g.GroupID); n != 2 {
		t.Fatalf("%d members after alice's commit, want 2: carol was not removed", n)
	}

	// From the leaver's side: the retry's answer leaves carol's leave queued, so her own client
	// refuses her next application message (OpenMLS sends nothing while the device's own proposal
	// is pending) and nothing from her reaches the instance. A refusal of the retry would have
	// made her withdraw the leave and send as a member.
	path := filepath.Join("..", "..", "testkit", "scenarios", "member_proposal_retry_keeps_the_leave.scn")
	k := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
	t.Cleanup(k.Stop)
	result := k.Run(t, path)
	if result.Err == nil || strings.Count(result.Stdout, `: Leave { client: "carol"`) != 2 ||
		strings.Count(result.Stdout, "FAIL line") != 1 ||
		!strings.Contains(result.Stdout, `FAIL line 18: Send { client: "carol"`) {
		t.Fatalf("carol's send after her retried leave: err %v, want the scenario to stop at that send\nstdout:\n%s",
			result.Err, result.Stdout)
	}
	kg := theTextGroup(t, k)
	msgs, err := k.Repo().ListAppMessages(context.Background(), kg.GroupID, 0, 64)
	if err != nil {
		t.Fatalf("ListAppMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("%d application messages reached the instance, want none from carol", len(msgs))
	}
}

// POST /proposal, the member's own void proposal again (R-1 of the DS re-review): carol's Remove went
// void, and the same proposal uploaded again is re-armed and answered as the first; carol keeps it,
// alice's commit applies it, and carol is removed. No row is left, and the proposal is logged once.
func TestAMemberReUploadingItsVoidProposalIsReArmedAndRemoved(t *testing.T) {
	h := runDSScenario(t, "member_proposal_again_after_void.scn")
	g := theTextGroup(t, h)
	if n := liveMembers(t, h, g.GroupID); n != 2 {
		t.Fatalf("%d members after alice's commit, want 2: carol's leave was abandoned", n)
	}
	rows, err := h.Repo().ListProposals(context.Background(), g.GroupID, g.Epoch, true)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("proposals %+v at the new epoch, want none", rows)
	}
	if n := memberProposalHandshakes(t, h, g.GroupID); n != 1 {
		t.Fatalf("%d member proposals logged, want 1", n)
	}
}

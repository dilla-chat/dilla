package testkit_test

// The accepted-commit tests internal/ds could not write (Ruling C(5)). Their package drives the
// delivery service with the committed 1,500-leaf fixture, which holds exactly one GroupInfo and no
// external commit, so no commit in it can be accepted; commit_test.go, proposal_test.go and
// resync_test.go recorded each as a skip naming that blocker. They are replaced here by
// equivalents under the same names, driven by real dilla-core clients through the in-process
// instance: a short scenario produces the accepted commits, and the test then reads what the
// instance stored.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/testkit"
)

// runScript runs a scenario written here rather than committed under testkit/scenarios: a test
// that reads the instance's state at a chosen point needs the scenario to end at that point.
func runScript(t *testing.T, h *testkit.Harness, script string) testkit.ScenarioResult {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accepted.scn")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write the scenario: %v", err)
	}
	result := h.Run(t, path)
	if result.Err != nil {
		t.Fatalf("scenario: %v\nstdout:\n%s\nstderr:\n%s", result.Err, result.Stdout, result.Stderr)
	}
	return result
}

func groupOf(t *testing.T, hex string) id.ID {
	t.Helper()
	g, err := id.Parse(hex)
	if err != nil {
		t.Fatalf("group id: %v", err)
	}
	return g
}

func handshakes(t *testing.T, h *testkit.Harness, g id.ID) []store.HandshakeRow {
	t.Helper()
	rows, err := h.Repo().ListHandshakes(context.Background(), g, 0, 512)
	if err != nil {
		t.Fatalf("ListHandshakes: %v", err)
	}
	return rows
}

func outstanding(t *testing.T, h *testkit.Harness, g id.ID) (store.GroupRow, []store.ProposalRow) {
	t.Helper()
	row, err := h.Repo().GetGroup(context.Background(), g)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	rows, err := h.Repo().ListProposals(context.Background(), g, row.Epoch, false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	return row, rows
}

// An accepted commit is appended to the handshake log at the epoch it creates, moves the group's
// epoch, stores the Welcome it addresses, and fans out mls.handshake and mls.epoch_changed to the
// members and mls.welcome to the joiner (commit_test.go's skipped test of this name).
func TestAnAcceptedCommitFansOutHandshakeEpochChangedAndWelcomes(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
	t.Cleanup(h.Stop)
	const gid = "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1"
	runScript(t, h, `instance dilla
client alice
client bob
group chat kind=text target=`+gid+` community=none creator=alice
join bob chat via=welcome
sync alice
expect_frame mls.handshake kind=1 epoch=1
expect_frame mls.epoch_changed epoch=1
sync bob
expect_frame mls.welcome
`)
	g := groupOf(t, gid)
	row, err := h.Repo().GetGroup(context.Background(), g)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if row.Epoch != 1 {
		t.Fatalf("epoch = %d, want 1: the Add commit was accepted", row.Epoch)
	}
	log := handshakes(t, h, g)
	if len(log) != 2 || log[0].Kind != 0 || log[1].Kind != 1 || log[1].Epoch != 1 {
		t.Fatalf("handshake log %v, want the instance's Add proposal then the commit at epoch 1",
			kindsAndEpochs(log))
	}
	if log[1].SenderDevice == nil {
		t.Fatal("the accepted commit records no committing device")
	}
	members, err := h.Repo().ListMembers(context.Background(), g)
	if err != nil || len(members) != 2 {
		t.Fatalf("members: %d, %v; want alice and bob", len(members), err)
	}
	if h.CommitCount() != 1 {
		t.Fatalf("the instance counted %d accepted commits, want 1", h.CommitCount())
	}
}

// Handshakes and application messages share ONE seq space per group: every seq is used once, by
// exactly one of the two streams, with no gap (commit_test.go's skipped test of this name).
func TestOneSeqSpaceNumbersHandshakesAndMessages(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
	t.Cleanup(h.Stop)
	const gid = "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2"
	runScript(t, h, `instance dilla
client alice
client bob
group chat kind=text target=`+gid+` community=none creator=alice
join bob chat via=welcome
send alice chat one
commit alice
send alice chat two
sync bob
expect_decrypts bob chat two
`)
	g := groupOf(t, gid)
	var seqs []uint64
	for _, r := range handshakes(t, h, g) {
		seqs = append(seqs, r.Seq)
	}
	messages, err := h.Repo().ListAppMessages(context.Background(), g, 0, 512)
	if err != nil {
		t.Fatalf("ListAppMessages: %v", err)
	}
	for _, m := range messages {
		seqs = append(seqs, m.Seq)
	}
	slices.Sort(seqs)
	// proposal 1, commit 2, message 3, commit 4, message 5.
	if want := []uint64{1, 2, 3, 4, 5}; !slices.Equal(seqs, want) {
		t.Fatalf("seqs across both streams = %v, want %v", seqs, want)
	}
	if len(messages) != 2 || messages[0].Seq != 3 || messages[1].Seq != 5 {
		t.Fatalf("the two messages took seqs %v, want 3 and 5", messageSeqs(messages))
	}
}

// Invariant 5's exception: with nobody online, an external commit is accepted although an instance
// proposal is outstanding, and the proposal it omitted is re-issued FOR THE NEW EPOCH — a fresh
// proposal, not the old one carried over (proposal_test.go's skipped test of this name).
func TestWithNobodyOnlineAnExternalCommitIsAcceptedAndTheProposalsAreReissued(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
	t.Cleanup(h.Stop)
	const gid = "e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3"
	runScript(t, h, `instance dilla
client alice
client bob
client carol
group chat kind=text target=`+gid+` community=none creator=alice
join bob chat via=welcome
sync bob
kick alice bob
go_offline alice
go_offline bob
external_join carol chat
expect_frame mls.commit_needed
`)
	g := groupOf(t, gid)
	row, pending := outstanding(t, h, g)
	log := handshakes(t, h, g)
	last := log[len(log)-1]
	joined := log[len(log)-2]
	if joined.Kind != 2 || joined.Epoch != row.Epoch {
		t.Fatalf("the external commit is not the handshake that carried the group to epoch %d: %v",
			row.Epoch, kindsAndEpochs(log))
	}
	if len(pending) != 1 || pending[0].Kind != uint8(mlswasi.ProposalRemove) || pending[0].Epoch != row.Epoch {
		t.Fatalf("outstanding at epoch %d: %d proposals, want the one Remove re-issued for it",
			row.Epoch, len(pending))
	}
	if last.Kind != 0 || last.Epoch != row.Epoch {
		t.Fatalf("the re-issued Remove is not the log's last handshake: %v", kindsAndEpochs(log))
	}
	var original *store.HandshakeRow
	for i := range log {
		if log[i].Kind == 0 && log[i].Epoch == row.Epoch-1 {
			original = &log[i]
		}
	}
	if original == nil {
		t.Fatalf("no Remove proposal at epoch %d: %v", row.Epoch-1, kindsAndEpochs(log))
	}
	if bytes.Equal(original.Blob, last.Blob) {
		t.Fatal("the re-issued proposal is the old one: a proposal is framed in its epoch")
	}
}

// R25: a resync is accepted DURING a freeze — it is how the device that has fallen out of the
// epoch returns — and the outstanding proposals are re-issued for the epoch it creates
// (resync_test.go's skipped test of this name).
func TestAResyncSucceedsDuringAFreezeAndReissuesTheProposals(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
	t.Cleanup(h.Stop)
	const gid = "e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4"
	runScript(t, h, `instance dilla
client alice
client bob
client carol
group chat kind=text target=`+gid+` community=none creator=alice
join bob chat via=welcome
join carol chat via=welcome
sync bob
go_offline bob
commit alice
kick alice carol
expect_425 send alice chat the group is frozen
go_online bob
resync bob chat
expect_425 send bob chat the re-issued Remove still holds the group
`)
	g := groupOf(t, gid)
	row, pending := outstanding(t, h, g)
	log := handshakes(t, h, g)
	resync := log[len(log)-2]
	if resync.Kind != 2 || resync.Epoch != row.Epoch || resync.SenderDevice == nil {
		t.Fatalf("the resync is not the handshake that carried the group to epoch %d: %v",
			row.Epoch, kindsAndEpochs(log))
	}
	if len(pending) != 1 || pending[0].Kind != uint8(mlswasi.ProposalRemove) {
		t.Fatalf("outstanding at epoch %d: %d proposals, want carol's Remove re-issued for it",
			row.Epoch, len(pending))
	}
	if pending[0].Epoch != row.Epoch {
		t.Fatalf("the Remove is outstanding at epoch %d, want the resync's %d",
			pending[0].Epoch, row.Epoch)
	}
}

// An eviction is never superseded (the security review of e619526; re-added by the task 10 fix
// round, f10app m5): the instance removes bob without his asking (a cut, the room sweep, the fork
// watchdog), and his resync is refused while that Remove stands.
func TestACallGroupEvictionIsNotSupersededByARejoin(t *testing.T) {
	h := testkit.Start(t, testkit.Options{DataDir: t.TempDir()})
	t.Cleanup(h.Stop)
	const gid = "e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9"
	runScript(t, h, `instance dilla
client alice
client bob
group call kind=call target=`+gid+` community=none creator=alice
join bob call via=external
sync alice
kick alice bob
expect_reject E_REMOVE_PENDING resync bob call
expect_425 send alice call the eviction still holds the call
`)
}

func kindsAndEpochs(log []store.HandshakeRow) [][3]uint64 {
	out := make([][3]uint64, 0, len(log))
	for _, r := range log {
		out = append(out, [3]uint64{r.Seq, uint64(r.Kind), r.Epoch})
	}
	return out
}

func messageSeqs(rows []store.AppMessageRow) []uint64 {
	out := make([]uint64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Seq)
	}
	return out
}

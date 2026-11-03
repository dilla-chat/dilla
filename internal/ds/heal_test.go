package ds_test

// heal_test.go is invariant 11: `dillad restore` bumps the generation and marks every group
// epoch-unknown, a member heals a group by uploading its member-signed GroupInfo and its handshake
// tail, and a group nobody heals inside the window is closed.
//
// WHAT THE COMMITTED FIXTURE GIVES, AND WHAT IT WITHHOLDS, shapes several of these tests, exactly
// as it shapes commit_test.go, proposal_test.go and directory_harness_test.go before them:
//
//   - `testkit/fixtures/ds-1500` ships the base GroupInfo (epoch 6, signed by leaf 0) and, since
//     task 27's fix round 1, `commits/09.group_info.mls`: the GroupInfo of the epoch the
//     creator's self-update `commits/09.mls` merges to. That pair is what lets a heal's tail
//     MERGE and move the instance forward, which is invariant 11's central promise.
//   - The fixture's one proposal, `remove_leaf0.mls`, is the instance's external Remove of leaf 0
//     at the base epoch. It is what a tail that only queues looks like.
//   - Commits 00-07 each Add 256 devices this instance has never enrolled. They are what a tail
//     that tries to carry an Add past invariant 4 looks like.
//   - The one KeyPackage the fixture ships carries no `last_resort` extension and cannot be
//     minted a second time (directory_harness_test.go's header), so the directory a restore
//     purges is seeded through the store, which is where the purge rule lives anyway.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

func TestOnRestoreBumpsTheGenerationMarksEveryGroupUnknownAndPurgesKeyPackages(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	device := g.device
	// DEVIATION from the brief, which publishes four packages and a last-resort one through
	// `PublishKeyPackages`. The fixture ships ONE KeyPackage and it is not a last-resort one, and
	// nothing in this repository can mint another (directory_harness_test.go's header), so the
	// directory is seeded through the store — which is where the purge rule lives.
	h.seedKeyPackages(t, device, 4, 30*24*time.Hour)
	h.seedLastResort(t, device, 90*24*time.Hour)
	call := h.liveCallGroup(t)
	before := h.generation(t)

	if err := h.ds.OnRestore(context.Background(), before+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	if got := h.generation(t); got != before+1 {
		t.Fatalf("generation = %d, want %d", got, before+1)
	}
	if !h.epochUnknown(t, g.id) {
		t.Error("every group becomes epoch-unknown on restore")
	}
	if got := h.countKeyPackages(t, device); got != 1 {
		t.Errorf("%d KeyPackages survive, want the 1 last-resort", got)
	}
	if h.liveCalls(t) != 0 {
		t.Error("live calls end on restore")
	}
	if !h.isClosed(t, call) {
		t.Error("the call group is still open after a restore")
	}
	status, err := h.ds.HealStatus(context.Background(), g.session, g.id)
	if err != nil {
		t.Fatalf("HealStatus: %v", err)
	}
	if status.Generation != before+1 {
		t.Errorf("HealStatus generation = %d, want %d", status.Generation, before+1)
	}
	if status.Epoch != g.Epoch() || status.NeedFromSeq != status.NextSeq {
		t.Errorf("HealStatus = %+v, want epoch %d and need_from_seq == next_seq",
			status, g.Epoch())
	}
}

// The instance adopts a heal only when its own stored epoch is <= the uploaded GroupInfo's.
//
// DEVIATION from the brief, which advances the epoch with `h.ds.Commit`. No commit in this
// repository can be accepted, so the epoch moves through `advanceGroupEpoch` — the same UPDATE
// `persistState` makes, which is what leaves the column `Heal` reads.
func TestAHealBelowTheStoredEpochIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ahead := h.advanceGroupEpoch(t, g.id)
	if ahead <= g.Epoch() {
		t.Fatalf("the stored epoch is %d, want it ahead of the fixture's %d", ahead, g.Epoch())
	}
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	_, err := h.ds.Heal(context.Background(), g.session, g.id, h.healAtEpoch(t, g, g.Epoch()))
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" {
		t.Fatalf("got %v, want E_COMMIT_INVALID", err)
	}
	if dsErr.Rule != "heal_epoch" {
		t.Errorf("rule = %q, want heal_epoch", dsErr.Rule)
	}
	if !h.epochUnknown(t, g.id) {
		t.Error("a refused heal must leave the group epoch-unknown")
	}
}

// The happy path: the tail replays cleanly through the restored blob, MERGES, and the rebuilt
// tree's hash equals the GroupInfo's, so the instance moves forward to the members' epoch.
//
// The restored blob is the fixture's base epoch and the members are one commit ahead of it: the
// tail is the creator's self-update `commits/09.mls` and the GroupInfo is the one it merges to.
func TestAHealWhoseTailReplaysCleanlyIsAdopted(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	tail := h.commitTail(t, g, 0)
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}

	out, err := h.ds.Heal(context.Background(), g.session, g.id, ds.HealRequest{
		GroupInfo: h.groupInfoAt(t, g, g.Epoch()+1),
		Tail:      tail,
	})
	if err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if out.Epoch != g.Epoch()+1 {
		t.Fatalf("healed to epoch %d, want %d: the tail's commit must move the instance forward",
			out.Epoch, g.Epoch()+1)
	}
	row := h.groupRow(t, g.id)
	if row.Epoch != g.Epoch()+1 {
		t.Errorf("the stored epoch is %d, want %d", row.Epoch, g.Epoch()+1)
	}
	if row.EpochUnknown {
		t.Error("an adopted heal clears epoch_unknown")
	}
	// The replayed tail is APPENDED and the high-water advances with it: without that, every other
	// member's `GET /handshakes?from=<its cursor>` would receive nothing for what the instance
	// just adopted.
	if out.Seq != uint64(len(tail)) {
		t.Errorf("next high-water = %d, want %d", out.Seq, len(tail))
	}
	if got := h.handshakeCount(t, g.id); got != int64(len(tail)) {
		t.Errorf("%d handshake rows, want %d", got, len(tail))
	}
	if row.Seq != out.Seq {
		t.Errorf("the group's seq column is %d, want %d", row.Seq, out.Seq)
	}
	// The adopted state is the one every later request is served from.
	if err := ds.WithGroupForTest(h.ds, context.Background(), g.id, func(pg *mlswasi.PublicGroup) error {
		state, err := pg.State(context.Background())
		if err != nil {
			return err
		}
		if state.Epoch != g.Epoch()+1 {
			t.Errorf("the cached PublicGroup is at epoch %d, want %d", state.Epoch, g.Epoch()+1)
		}
		return nil
	}); err != nil {
		t.Fatalf("withGroup: %v", err)
	}
}

// The member's signed GroupInfo must name the exact epoch reached by replay. Tree-hash equality
// alone cannot stand in for this: an empty MLS commit can advance epoch without changing the tree.
func TestAHealWhoseGroupInfoEpochDoesNotMatchReplayedStateIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	_, err := h.ds.Heal(context.Background(), g.session, g.id, ds.HealRequest{
		GroupInfo: h.groupInfoAt(t, g, g.Epoch()),
		Tail:      h.commitTail(t, g, 0),
	})
	if !hasRule(err, "heal_epoch") {
		t.Fatalf("mismatched GroupInfo epoch: got %v, want E_COMMIT_INVALID/heal_epoch", err)
	}
	if !h.epochUnknown(t, g.id) {
		t.Fatal("a mismatched GroupInfo epoch cleared epoch_unknown")
	}
}

func TestHealAcceptanceGuardRefusalLeavesTheRestoredGroupUnknown(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	guardCalled := false
	_, err := h.ds.HealWithGuard(context.Background(), g.session, g.id, h.healAtEpoch(t, g, g.Epoch()),
		func(context.Context, store.Repository, store.GroupRow) error {
			guardCalled = true
			return errors.New("Bot installation was disabled before heal acceptance")
		})
	if err == nil || err.Error() != "Bot installation was disabled before heal acceptance" {
		t.Fatalf("HealWithGuard error = %v, want acceptance-time authorization refusal", err)
	}
	if !guardCalled {
		t.Fatal("acceptance-time guard was not called")
	}
	if !h.epochUnknown(t, g.id) {
		t.Fatal("refused heal cleared EpochUnknown")
	}
}

// The rows a heal appends are what every other member's catch-up is served and what
// E_COMMIT_CONFLICT names as a winning commit, so they carry what the replay DERIVED, never what
// the uploading member claimed: the epoch the group actually reached, the kind the guest reported,
// the leaf that signed it and the device behind that leaf.
func TestAHealStoresTheReplayedMetadataNotTheClaimedOne(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	tail := h.commitTail(t, g, 0)
	tail[0].Epoch = 99 // a lie
	tail[0].Kind = 0   // "a proposal"
	tail[0].Sender = nil
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	if _, err := h.ds.Heal(context.Background(), g.session, g.id, ds.HealRequest{
		GroupInfo: h.groupInfoAt(t, g, g.Epoch()+1),
		Tail:      tail,
	}); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	rows := h.handshakes(t, g.id)
	if len(rows) != 1 {
		t.Fatalf("%d handshake rows, want 1", len(rows))
	}
	got := rows[0]
	if got.Epoch != g.Epoch()+1 {
		t.Errorf("stored epoch = %d, want %d (the epoch the merge reached)", got.Epoch, g.Epoch()+1)
	}
	if got.Kind != 1 {
		t.Errorf("stored kind = %d, want 1 (a member commit)", got.Kind)
	}
	if got.SenderLeaf == nil || *got.SenderLeaf != 0 {
		t.Errorf("stored sender leaf = %v, want 0 (the committer)", got.SenderLeaf)
	}
	if got.SenderDevice == nil || *got.SenderDevice != g.device {
		t.Errorf("stored sender device = %v, want the committer's device %s",
			got.SenderDevice, g.device.String()[:8])
	}
}

// A tail that only queues: the fixture's instance Remove of leaf 0. It is replayed INTO the
// guest's queue, because a later commit in the same tail references it by ref and the healed
// state must carry it, and it is stored as the proposal it is, at the epoch it was issued for,
// with no sender leaf or device, because the instance's external sender has neither.
func TestAHealReplaysAProposalIntoTheGuestsQueue(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	tail := h.proposalTail(t, g, 0)
	tail[0].Kind = 1 // a lie: "a commit"
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	out, err := h.ds.Heal(context.Background(), g.session, g.id, ds.HealRequest{
		GroupInfo: h.groupInfoAt(t, g, g.Epoch()),
		Tail:      tail,
	})
	if err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if out.Epoch != g.Epoch() {
		t.Fatalf("healed to epoch %d, want %d: a proposal does not move the epoch", out.Epoch, g.Epoch())
	}
	rows := h.handshakes(t, g.id)
	if len(rows) != 1 {
		t.Fatalf("%d handshake rows, want 1", len(rows))
	}
	if r := rows[0]; r.Kind != 0 || r.Epoch != g.Epoch() || r.SenderLeaf != nil || r.SenderDevice != nil {
		t.Errorf("stored row = kind %d epoch %d leaf %v device %v, want the instance's proposal at epoch %d",
			r.Kind, r.Epoch, r.SenderLeaf, r.SenderDevice, g.Epoch())
	}
	if err := ds.WithGroupForTest(h.ds, context.Background(), g.id, func(pg *mlswasi.PublicGroup) error {
		queued, err := pg.ProposalList(context.Background())
		if err != nil {
			return err
		}
		if len(queued) != 1 {
			t.Errorf("the healed guest queues %d proposals, want the 1 the tail replayed", len(queued))
		}
		return nil
	}); err != nil {
		t.Fatalf("withGroup: %v", err)
	}
}

// Invariant 11 is the post-restore path and nothing else. A group that was never restored is not
// epoch-unknown, and a heal there would be a second, unguarded commit path: the tail's commits
// would be merged outside invariant 3's one-commit-per-epoch rule and invariant 5's freeze.
func TestAHealOfAGroupThatWasNeverRestoredIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	_, err := h.ds.Heal(context.Background(), g.session, g.id, h.healAtEpoch(t, g, g.Epoch()))
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" || dsErr.Rule != "heal_state" {
		t.Fatalf("got %v, want E_COMMIT_INVALID/heal_state", err)
	}
}

// The heal window closes at the deadline, even before the sweeper has closed the group.
func TestAHealAfterTheWindowIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	h.clk.Advance(25 * time.Hour)
	_, err := h.ds.Heal(context.Background(), g.session, g.id, h.healAtEpoch(t, g, g.Epoch()))
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" || dsErr.Rule != "heal_state" {
		t.Fatalf("got %v, want E_COMMIT_INVALID/heal_state", err)
	}
}

// Invariant 4's Add clause runs over every commit a tail replays. `commits/00.mls` adds 256
// devices this instance has never enrolled. Replayed without the clause it is merged into the
// instance's tree, and with a GroupInfo the healing member signs for the result it would be
// adopted, written to mls_members and republished to the gateway.
func TestAHealWhoseTailAddsAnIneligibleDeviceIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	leaf := uint32(0)
	tail := []ds.HealTailItem{{
		Seq: 1, Epoch: g.Epoch(), Kind: 1, Sender: &leaf, Blob: fixtureFile(t, "commits/00.mls"),
	}}
	_, err := h.ds.Heal(context.Background(), g.session, g.id, ds.HealRequest{
		GroupInfo: h.groupInfoAt(t, g, g.Epoch()),
		Tail:      tail,
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" || dsErr.Rule != "add_key_package" {
		t.Fatalf("got %v, want E_COMMIT_INVALID/add_key_package", err)
	}
	if !h.epochUnknown(t, g.id) {
		t.Error("a refused heal must leave the group epoch-unknown")
	}
	if got := h.handshakeCount(t, g.id); got != 0 {
		t.Errorf("a refused heal wrote %d handshake rows", got)
	}
}

// A reseed builds the group at the uploaded GroupInfo's epoch, the member's tip, so every tail
// item for an EARLIER epoch is already in the tree it was built from. Replaying one would be a
// wrong-epoch refusal of a legitimate heal, and tree plus tail is protocol/02's own shape. Only
// what the tree does not already hold is replayed and appended.
//
// The group's devices are enrolled honestly (enrolledGroup): a reseed adopts the uploaded tree only
// when every leaf passes the Add clause (hardening G2b).
func TestAReseedingHealSkipsTheTailTheTreeAlreadyHolds(t *testing.T) {
	h := newDSHarness(t)
	g := h.enrolledGroup(t)
	tree := h.currentTree(t, g)
	groupInfo := h.groupInfoAt(t, g, g.Epoch())
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	h.destroyStateBlob(t, g)

	leaf := uint32(0)
	tail := append([]ds.HealTailItem{{
		// An earlier epoch's commit: the reseeded tree already includes it.
		Seq: 1, Epoch: g.Epoch() - 1, Kind: 1, Sender: &leaf, Blob: fixtureFile(t, "commits/09.mls"),
	}}, h.proposalTail(t, g, 1)...)
	out, err := h.ds.Heal(context.Background(), g.session, g.id, ds.HealRequest{
		GroupInfo: groupInfo, Tail: tail, RatchetTree: tree,
	})
	if err != nil {
		t.Fatalf("reseeding heal with a tail: %v", err)
	}
	if out.Epoch != g.Epoch() {
		t.Errorf("healed to epoch %d, want %d", out.Epoch, g.Epoch())
	}
	rows := h.handshakes(t, g.id)
	if len(rows) != 1 || rows[0].Kind != 0 {
		t.Fatalf("%d handshake rows appended, want only the 1 proposal the tree did not hold", len(rows))
	}
}

// The restored log already holds seq 1 and only the state blob is lost. A reseeding heal whose
// tail carries seq 1 again replays it into the reseeded guest, but does not write it a second
// time under a fresh seq. The devices are enrolled honestly, as above.
func TestAReseedingHealDoesNotDuplicateTheRestoredLog(t *testing.T) {
	h := newDSHarness(t)
	g := h.enrolledGroup(t)
	tree := h.currentTree(t, g)
	groupInfo := h.groupInfoAt(t, g, g.Epoch())
	tail := h.proposalTail(t, g, 0)
	h.appendLoggedHandshake(t, g.id, tail[0])
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	h.destroyStateBlob(t, g)

	out, err := h.ds.Heal(context.Background(), g.session, g.id, ds.HealRequest{
		GroupInfo: groupInfo, Tail: tail, RatchetTree: tree,
	})
	if err != nil {
		t.Fatalf("reseeding heal: %v", err)
	}
	if got := h.handshakeCount(t, g.id); got != 1 {
		t.Errorf("%d handshake rows after the heal, want the 1 the restored log already held", got)
	}
	if out.Seq != 1 {
		t.Errorf("high-water = %d, want 1", out.Seq)
	}
}

// A heal with no usable blob AND a supplied ratchet tree reseeds through from_external — the one
// upload where the tree is allowed, because the instance has none, and RFC 9420 §12.4.3.3's
// signed tree_hash is what makes that source safe. Since hardening G2b every leaf of the tree also
// passes the Add clause, so the group's devices are enrolled honestly.
func TestAHealWithNoBlobReseedsFromTheSuppliedTree(t *testing.T) {
	h := newDSHarness(t)
	g := h.enrolledGroup(t)
	tree := h.currentTree(t, g)
	groupInfo := h.groupInfoAt(t, g, g.Epoch())
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	h.destroyStateBlob(t, g)

	if _, err := h.ds.Heal(context.Background(), g.session, g.id, ds.HealRequest{
		GroupInfo:   groupInfo,
		RatchetTree: tree,
	}); err != nil {
		t.Fatalf("reseeding heal: %v", err)
	}
	if h.epochUnknown(t, g.id) {
		t.Error("a reseeding heal clears epoch_unknown")
	}

	// Without the tree there is nothing to reseed from, and the heal is refused rather than
	// accepted on the member's word.
	h.destroyStateBlob(t, g)
	_, err := h.ds.Heal(context.Background(), g.session, g.id, ds.HealRequest{GroupInfo: groupInfo})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" {
		t.Fatalf("got %v, want E_COMMIT_INVALID", err)
	}
}

func TestATailLongerThanSixtyFourItemsIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	req := ds.HealRequest{GroupInfo: h.groupInfoAt(t, g, g.Epoch()), Tail: h.syntheticTail(65)}
	_, err := h.ds.Heal(context.Background(), g.session, g.id, req)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_INVALID_REQUEST" {
		t.Fatalf("got %v, want E_INVALID_REQUEST", err)
	}
}

func TestAGroupThatIsNotHealedWithinTheWindowIsClosed(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	// Inside the window nothing happens: the sweep that ran before the deadline must not close a
	// group a member is still on its way to healing.
	if _, err := h.ds.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep inside the window: %v", err)
	}
	if h.isClosed(t, g.id) {
		t.Fatal("the group was closed before the heal window elapsed")
	}

	h.clk.Advance(25 * time.Hour)
	if _, err := h.ds.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if !h.isClosed(t, g.id) {
		t.Fatal("after the 24-hour heal window with no successful heal the group is closed")
	}
}

// A group that WAS healed is not closed when the deadline passes: the adoption cleared both the
// flag and the deadline, which is the pair closeUnhealedGroups reads.
func TestAHealedGroupSurvivesTheWindow(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	if _, err := h.ds.Heal(context.Background(), g.session, g.id,
		h.healAtEpoch(t, g, g.Epoch())); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	h.clk.Advance(25 * time.Hour)
	if _, err := h.ds.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if h.isClosed(t, g.id) {
		t.Fatal("a healed group was closed by the heal-window sweep anyway")
	}
}

// Every response and every gateway hello carries the new generation, which invalidates
// outstanding resume tokens.
func TestTheNewGenerationInvalidatesResumeTokens(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	conn := h.connectDevice(t, g.device)
	token := conn.ResumeToken(t)

	// The same token IS accepted while the generation stands, so what the restore changes is the
	// generation and not merely the socket.
	if !h.resumeAccepted(t, conn, token) {
		t.Fatal("a resume under the current generation must be accepted")
	}

	conn = h.connectDevice(t, g.device)
	token = conn.ResumeToken(t)
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	if h.resumeAccepted(t, conn, token) {
		t.Fatal("a resume under the old generation must be refused after a restore")
	}
}

// DS-3 of the server-half review: the device an outstanding instance Remove targets, and a
// quarantined device, cannot heal — the same two refusals Resync makes. Otherwise a device being
// removed builds a commit at the restored epoch that omits its Remove, heals first, and every honest
// member's heal is refused after it.
func TestTheTargetOfAnOutstandingRemoveAndAQuarantinedDeviceCannotHeal(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, h *dsHarness, g *dsMessageGroup)
		code  string
		want  string
	}{
		// Guard 1's refusal is 409 E_REMOVE_PENDING, for a heal as for a resync (task 10 fix round).
		{"the target of an outstanding Remove", func(t *testing.T, h *dsHarness, g *dsMessageGroup) {
			if err := h.ds.ProposeRemove(context.Background(), g.id, 0, id.New()); err != nil {
				t.Fatalf("ProposeRemove: %v", err)
			}
		}, "E_REMOVE_PENDING", "outstanding Remove"},
		{"a quarantined device", func(t *testing.T, h *dsHarness, g *dsMessageGroup) {
			if err := h.repo.QuarantineDevice(context.Background(), g.device, h.clk.Now().Unix(), "fork quorum"); err != nil {
				t.Fatalf("QuarantineDevice: %v", err)
			}
		}, "E_FORBIDDEN", "quarantined"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			g := h.group(t)
			c.setup(t, h, g)
			if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
				t.Fatalf("OnRestore: %v", err)
			}
			_, err := h.ds.Heal(context.Background(), g.session, g.id, ds.HealRequest{
				GroupInfo: h.groupInfoAt(t, g, g.Epoch()+1),
				Tail:      h.commitTail(t, g, h.groupRow(t, g.id).Seq),
			})
			var dsErr *ds.Error
			if !errors.As(err, &dsErr) || dsErr.Code != c.code || !strings.Contains(dsErr.Detail, c.want) {
				t.Fatalf("the heal: got %v, want %s naming %q", err, c.code, c.want)
			}
			if !h.epochUnknown(t, g.id) || h.groupRow(t, g.id).Epoch != g.Epoch() {
				t.Fatal("a refused heal must leave the group epoch-unknown at its restored epoch")
			}
		})
	}
}

// healAcrossOneEpoch restores and heals the fixture group one epoch forward (commits/09.mls).
func (h *dsHarness) healAcrossOneEpoch(t *testing.T, g *dsMessageGroup) {
	t.Helper()
	if err := h.ds.OnRestore(context.Background(), h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	out, err := h.ds.Heal(context.Background(), g.session, g.id, ds.HealRequest{
		GroupInfo: h.groupInfoAt(t, g, g.Epoch()+1),
		Tail:      h.commitTail(t, g, h.groupRow(t, g.id).Seq),
	})
	if err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if out.Epoch != g.Epoch()+1 {
		t.Fatalf("healed to epoch %d, want %d", out.Epoch, g.Epoch()+1)
	}
}

// DS-3: a heal that moves the epoch carries the restored epoch's outstanding instance Remove across:
// its device still holds the leaf in the adopted tree, so the Remove is re-issued for the healed
// epoch onto that device with the same action_id, and the group is frozen on it. Nothing is left at
// the restored epoch, where no reader would ever look again.
func TestAHealThatMovesTheEpochReissuesAnOutstandingRemove(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.group(t)
	target := h.memberSession(t, g.id, 1).DeviceID
	action := id.New()
	if err := h.ds.ProposeRemove(ctx, g.id, 1, action); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	h.healAcrossOneEpoch(t, g)

	if rows := h.proposalsAt(t, g.id, g.Epoch(), true); len(rows) != 0 {
		t.Fatalf("%d rows left at the restored epoch, want 0: %+v", len(rows), rows)
	}
	rows := h.proposalsAt(t, g.id, g.Epoch()+1, true)
	if len(rows) != 1 {
		t.Fatalf("rows at the healed epoch: %+v, want the one Remove re-issued", rows)
	}
	r := rows[0]
	if r.Origin != 0 || r.VoidAt != nil || r.TargetLeaf == nil || *r.TargetLeaf != 1 ||
		r.TargetDevice == nil || *r.TargetDevice != target || r.ActionID != action {
		t.Fatalf("re-issued row %+v: want a live instance Remove of leaf 1 recording %s with action %s",
			r, target.String()[:8], action)
	}
	_, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch()+1, h.message(t, g, g.Epoch()+1), 0)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_REQUIRED" {
		t.Fatalf("a send after the heal: got %v, want 425 E_COMMIT_REQUIRED", err)
	}
	if !h.guestHolds(t, g.id, r.Ref, true) {
		t.Fatal("the healed state blob does not hold the re-issued Remove")
	}
}

// DS-3: what no longer applies after an epoch-moving heal is dropped, at every epoch: a Remove whose
// recorded device no longer holds its leaf in the adopted tree, an Add whose device is already a
// member, an Add whose device the instance does not know, and the restored epoch's void and member
// rows.
func TestAHealThatMovesTheEpochDropsWhatNoLongerApplies(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.group(t)
	epoch := g.Epoch()
	put := func(r store.ProposalRow) {
		t.Helper()
		ref := id.New()
		r.GroupID, r.Ref, r.Epoch, r.ActionID = g.id, ref[:], epoch, id.New()
		r.IssuedAt, r.TTL = h.clk.Now().Unix(), uint64((24 * time.Hour).Seconds())
		if err := h.repo.PutProposal(ctx, r); err != nil {
			t.Fatalf("PutProposal: %v", err)
		}
	}
	remove, add := uint8(mlswasi.ProposalRemove), uint8(mlswasi.ProposalAdd)
	leaf2, leaf4 := uint32(2), uint32(4)
	gone, stranger := id.New(), id.New()
	member := h.memberSession(t, g.id, 3).DeviceID
	put(store.ProposalRow{Kind: remove, TargetLeaf: &leaf2, TargetDevice: &gone})      // its device left leaf 2
	put(store.ProposalRow{Kind: add, TargetDevice: &member, KeyPackage: []byte{0x01}}) // already a member
	put(store.ProposalRow{Kind: add, TargetDevice: &stranger, KeyPackage: []byte{0x01}})
	put(store.ProposalRow{Kind: remove, TargetLeaf: &leaf4, Origin: 1}) // a member's own Remove
	voidAt := h.clk.Now().Unix()
	put(store.ProposalRow{Kind: remove, TargetLeaf: &leaf4, TargetDevice: &gone, VoidAt: &voidAt})

	h.healAcrossOneEpoch(t, g)
	if rows := h.proposalsAt(t, g.id, epoch, true); len(rows) != 0 {
		t.Fatalf("%d rows left at the restored epoch, want 0: %+v", len(rows), rows)
	}
	if rows := h.proposalsAt(t, g.id, epoch+1, true); len(rows) != 0 {
		t.Fatalf("rows at the healed epoch: %+v, want none: nothing of the restored epoch still applies", rows)
	}
}

// R-2 of the DS re-review: a fork-quarantine Remove the heal's carry fails to re-issue is dropped
// with the restored epoch's rows — and the next reconcile pass proposes it again at the healed
// epoch, bound to the quarantined device, so the device is still removed.
func TestAQuarantineRemoveTheHealFailsToCarryIsProposedAgain(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.group(t)
	target := h.memberSession(t, g.id, 1)
	h.account(t, target.UserID, target.DeviceID)
	if err := h.repo.QuarantineDevice(ctx, target.DeviceID, h.clk.Now().Unix(), "fork quorum"); err != nil {
		t.Fatalf("QuarantineDevice: %v", err)
	}
	if err := h.ds.ProposeRemoveDevice(ctx, g.id, target.DeviceID, id.New()); err != nil {
		t.Fatalf("ProposeRemoveDevice: %v", err)
	}
	h.failNextTx("ReissueProposal") // the carry's re-issue, which the heal's own transaction never calls
	h.healAcrossOneEpoch(t, g)
	h.failNextTx("")
	if rows := h.proposalsAt(t, g.id, g.Epoch()+1, true); len(rows) != 0 {
		t.Fatalf("rows at the healed epoch %+v, want none: the carry was meant to fail", rows)
	}

	if _, err := ds.ReconcileLeavesForTest(h.ds, ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rows := h.proposalsAt(t, g.id, g.Epoch()+1, false)
	if len(rows) != 1 || rows[0].Origin != 0 || rows[0].TargetLeaf == nil || *rows[0].TargetLeaf != 1 ||
		rows[0].TargetDevice == nil || *rows[0].TargetDevice != target.DeviceID {
		t.Fatalf("rows at the healed epoch after the reconcile: %+v, want the quarantined device's Remove", rows)
	}
}

// DS-3: a heal that leaves the epoch where it was (the members never moved past the backup) leaves
// the restored epoch's proposals exactly as they are: they are still this epoch's work.
func TestAHealAtTheRestoredEpochLeavesItsProposalsAlone(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.group(t)
	if err := h.ds.ProposeRemove(ctx, g.id, 1, id.New()); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	before := h.proposalsAt(t, g.id, g.Epoch(), true)
	if err := h.ds.OnRestore(ctx, h.generation(t)+1); err != nil {
		t.Fatalf("OnRestore: %v", err)
	}
	h.clk.Advance(time.Minute)
	if _, err := h.ds.Heal(ctx, g.session, g.id, h.healAtEpoch(t, g, g.Epoch())); err != nil {
		t.Fatalf("Heal: %v", err)
	}
	after := h.proposalsAt(t, g.id, g.Epoch(), true)
	if len(after) != 1 || len(before) != 1 || !bytes.Equal(after[0].Ref, before[0].Ref) ||
		after[0].IssuedAt != before[0].IssuedAt || after[0].ActionID != before[0].ActionID || after[0].VoidAt != nil {
		t.Fatalf("rows after a same-epoch heal %+v, want them untouched: %+v", after, before)
	}
}

// ---------------------------------------------------------------- the helpers

// generation is the instance generation SQL holds.
func (h *dsHarness) generation(t *testing.T) uint64 {
	t.Helper()
	row, err := h.repo.GetInstance(context.Background())
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	return row.Generation
}

func (h *dsHarness) groupRow(t *testing.T, groupID id.ID) store.GroupRow {
	t.Helper()
	row, err := h.repo.GetGroup(context.Background(), groupID)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	return row
}

func (h *dsHarness) epochUnknown(t *testing.T, groupID id.ID) bool {
	t.Helper()
	return h.groupRow(t, groupID).EpochUnknown
}

func (h *dsHarness) isClosed(t *testing.T, groupID id.ID) bool {
	t.Helper()
	return h.groupRow(t, groupID).ClosedAt != nil
}

// countKeyPackages is EVERY row the device still holds, the last-resort one included —
// `CountKeyPackages` deliberately excludes it, and the purge rule is precisely about which of the
// two kinds survives.
func (h *dsHarness) countKeyPackages(t *testing.T, device id.ID) int64 {
	t.Helper()
	db, err := sqlite.OpenRead(h.path)
	if err != nil {
		t.Fatalf("sqlite.OpenRead: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int64
	if err := db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM key_packages WHERE device_id = ?", device[:]).Scan(&n); err != nil {
		t.Fatalf("count key_packages: %v", err)
	}
	return n
}

func (h *dsHarness) handshakeCount(t *testing.T, groupID id.ID) int64 {
	t.Helper()
	db, err := sqlite.OpenRead(h.path)
	if err != nil {
		t.Fatalf("sqlite.OpenRead: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int64
	if err := db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM mls_handshakes WHERE group_id = ?", groupID[:]).Scan(&n); err != nil {
		t.Fatalf("count mls_handshakes: %v", err)
	}
	return n
}

// liveCalls counts the OPEN call groups. A live call is its call group: R9 puts the call id in a
// companion column and `voice_sessions` is Plan 2's table, so on a Plan-1 database this is the
// whole of "live calls".
func (h *dsHarness) liveCalls(t *testing.T) int64 {
	t.Helper()
	db, err := sqlite.OpenRead(h.path)
	if err != nil {
		t.Fatalf("sqlite.OpenRead: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int64
	if err := db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM mls_groups WHERE call_id IS NOT NULL AND closed_at IS NULL",
	).Scan(&n); err != nil {
		t.Fatalf("count call groups: %v", err)
	}
	return n
}

// liveCallGroup writes one open call group. It goes through the store rather than through
// `Register`, because the committed fixture's binding is a TEXT group's (kind 0, fixtures.rs:106)
// and nothing in this repository can mint a call group's GroupInfo — while what is under test is
// what a restore does to a row that has a call id.
func (h *dsHarness) liveCallGroup(t *testing.T) id.ID {
	t.Helper()
	groupID := id.New()
	callID := id.New()
	if err := h.repo.CreateGroup(context.Background(), store.GroupRow{
		GroupID:             groupID,
		Binding:             []byte{0x80},
		Kind:                1, // call
		TargetID:            callID,
		CallID:              &callID,
		Ciphersuite:         1,
		ExternalSenderKeyID: testInstanceKeys(t).ExternalSenderKeyID,
		E2EEVersion:         1,
		MediaVersion:        1,
		PolicyVersion:       1,
		Created:             h.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("CreateGroup(call): %v", err)
	}
	return groupID
}

// groupInfoAt is the member-signed GroupInfo for that epoch. The fixture ships two, both signed
// by leaf 0 — the leaf `h.group` hands the test its session for: the base epoch's, and the one
// `commits/09.mls` merges to. Any other epoch is a test asking for material that does not exist.
func (h *dsHarness) groupInfoAt(t *testing.T, g *dsMessageGroup, epoch uint64) []byte {
	t.Helper()
	switch epoch {
	case g.Epoch():
		return dsFixture(t).groupInfo
	case g.Epoch() + 1:
		return fixtureFile(t, "commits/09.group_info.mls")
	}
	t.Fatalf("the fixture ships GroupInfos for epochs %d and %d; epoch %d is not available",
		g.Epoch(), g.Epoch()+1, epoch)
	return nil
}

// currentTree is the ratchet tree of the group's current epoch, as a healing member would upload
// it when the instance holds no blob of its own.
func (h *dsHarness) currentTree(t *testing.T, _ *dsMessageGroup) []byte {
	t.Helper()
	return dsFixture(t).ratchetTree
}

// healAtEpoch is a bare heal: the GroupInfo alone, no tail and no tree.
func (h *dsHarness) healAtEpoch(t *testing.T, g *dsMessageGroup, epoch uint64) ds.HealRequest {
	t.Helper()
	return ds.HealRequest{GroupInfo: h.groupInfoAt(t, g, epoch)}
}

// commitTail is a tail that MERGES: the creator's self-update, whose resulting epoch
// `commits/09.group_info.mls` describes. It is numbered from the seq the caller is behind at.
func (h *dsHarness) commitTail(t *testing.T, g *dsMessageGroup, from uint64) []ds.HealTailItem {
	t.Helper()
	leaf := uint32(0)
	return []ds.HealTailItem{{
		Seq:    from + 1,
		Epoch:  g.Epoch(),
		Kind:   1, // commit
		Sender: &leaf,
		Blob:   fixtureFile(t, "commits/09.mls"),
	}}
}

// proposalTail is a tail that only queues: the fixture's one proposal, the instance's external
// Remove of leaf 0, numbered from the seq the caller is behind at.
func (h *dsHarness) proposalTail(t *testing.T, g *dsMessageGroup, from uint64) []ds.HealTailItem {
	t.Helper()
	return []ds.HealTailItem{{
		Seq:   from + 1,
		Epoch: g.Epoch(),
		Kind:  0, // proposal
		Blob:  fixtureFile(t, "remove_leaf0.mls"),
	}}
}

// appendLoggedHandshake writes one tail item into the log the way the live path would have before
// the backup was taken: under the next seq, which advances the group's high-water with it.
func (h *dsHarness) appendLoggedHandshake(t *testing.T, groupID id.ID, item ds.HealTailItem) {
	t.Helper()
	ctx := context.Background()
	var allocated uint64
	if err := h.repo.Tx(ctx, func(tx store.Repository) error {
		seq, err := tx.NextSeq(ctx, groupID)
		if err != nil {
			return err
		}
		allocated = seq
		return tx.AppendHandshake(ctx, store.HandshakeRow{
			GroupID: groupID, Seq: seq, Epoch: item.Epoch, Kind: item.Kind,
			SenderLeaf: item.Sender, Blob: item.Blob, Created: h.clk.Now().Unix(),
		})
	}); err != nil {
		t.Fatalf("append the logged handshake: %v", err)
	}
	if allocated != item.Seq {
		t.Fatalf("the log allocated seq %d, the tail item claims %d", allocated, item.Seq)
	}
}

// handshakes is the group's whole handshake log, in seq order.
func (h *dsHarness) handshakes(t *testing.T, groupID id.ID) []store.HandshakeRow {
	t.Helper()
	rows, err := h.repo.ListHandshakes(context.Background(), groupID, 0, 512)
	if err != nil {
		t.Fatalf("ListHandshakes: %v", err)
	}
	return rows
}

// syntheticTail is n items of nothing in particular: the length cap is checked before a single
// blob is parsed, which is the point of checking it first.
func (h *dsHarness) syntheticTail(n int) []ds.HealTailItem {
	out := make([]ds.HealTailItem, 0, n)
	for i := range n {
		out = append(out, ds.HealTailItem{Seq: uint64(i) + 1, Kind: 0, Blob: []byte{0x01}})
	}
	return out
}

// destroyStateBlob empties `public_group_state`, which is what a restore from a backup that
// predates the group leaves behind: the row exists and the blob does not.
func (h *dsHarness) destroyStateBlob(t *testing.T, g *dsMessageGroup) {
	t.Helper()
	row := h.groupRow(t, g.id)
	if err := h.repo.PutGroupState(context.Background(), g.id, row.Epoch,
		nil, row.GroupInfoBlob, row.TreeHash); err != nil {
		t.Fatalf("PutGroupState: %v", err)
	}
	// PutGroupState clears epoch_unknown, which the restore had set; put it back so the test is
	// still healing a group the restore marked.
	if err := h.repo.MarkAllGroupsEpochUnknown(context.Background(),
		h.clk.Now().Add(h.policy().HealWindow).Unix()); err != nil {
		t.Fatalf("MarkAllGroupsEpochUnknown: %v", err)
	}
	// The cached handle still describes the blob that is now gone.
	if err := ds.EvictStateForTest(h.ds, context.Background(), g.id); err != nil {
		t.Fatalf("evict: %v", err)
	}
}

// ---------------------------------------------------------------- resume over the wire

// connectDevice puts the device on a live gateway connection and hands back the connection the
// resume test dials again.
func (h *dsHarness) connectDevice(t *testing.T, device id.ID) *deviceConn {
	t.Helper()
	return h.connect(device)
}

// ResumeToken is element 3 of the `ready` frame this connection was greeted with.
func (c *deviceConn) ResumeToken(t *testing.T) []byte {
	t.Helper()
	if len(c.ready.payload) < 4 {
		t.Fatalf("ready carries %d elements, want at least 4", len(c.ready.payload))
	}
	var token []byte
	if err := cborx.Unmarshal(c.ready.payload[3], &token); err != nil {
		t.Fatalf("decode the resume token: %v", err)
	}
	return token
}

// readyGeneration is element 2 of `ready`: the generation the client learned, and the one its
// `resume` frame must carry back.
func (c *deviceConn) readyGeneration(t *testing.T) uint64 {
	t.Helper()
	var generation uint64
	if err := cborx.Unmarshal(c.ready.payload[2], &generation); err != nil {
		t.Fatalf("decode the generation: %v", err)
	}
	return generation
}

// resumeAccepted drops the socket, waits for the gateway to park the connection in its resume
// window, and dials again with the token. It answers what the instance answered: `resumed`, or
// `invalid_session`.
//
// It is driven over a real WebSocket rather than against the gateway's internals because
// internal/ds is an external test package — and because the refusal invariant 11 promises is one
// a client observes on the wire, not one a field holds.
func (h *dsHarness) resumeAccepted(t *testing.T, c *deviceConn, token []byte) bool {
	t.Helper()
	generation := c.readyGeneration(t)
	_ = c.ws.CloseNow()
	delete(h.conns, c.deviceID)
	deadline := time.Now().Add(15 * time.Second)
	for h.gw.Online(c.deviceID) {
		if time.Now().After(deadline) {
			t.Fatalf("device %s is still online after its socket closed", c.deviceID.String()[:8])
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(c.url, "http"), //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		&websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + c.bearer}},
			Subprotocols: []string{"dilla.v1"},
		})
	if err != nil {
		t.Fatalf("re-dial the gateway: %v", err)
	}
	defer func() { _ = ws.CloseNow() }()
	if hello := readRecordedFrame(t, ctx, ws); hello.op != gateway.OpHello {
		t.Fatalf("the first frame is %s, want hello", hello.name)
	}
	payload, err := cborx.Marshal([]any{"", token, generation, uint64(0)})
	if err != nil {
		t.Fatalf("encode resume: %v", err)
	}
	frame, err := gateway.Encode(
		gateway.Frame{Op: gateway.OpResume, Payload: cborx.Raw(payload)}, 2)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("write resume: %v", err)
	}
	switch got := readRecordedFrame(t, ctx, ws); got.op {
	case gateway.OpResumed:
		return true
	case gateway.OpInvalidSession:
		return false
	default:
		t.Fatalf("the answer to resume is op %d, want resumed or invalid_session", got.op)
		return false
	}
}

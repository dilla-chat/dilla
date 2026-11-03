package ds_test

// fork_test.go is invariant 9: a member that cannot process an accepted commit reports it, and
// three DISTINCT reporter devices against one commit quarantine its committer.
//
// DEVIATION FROM THE BRIEF'S STEP 1 (recorded in the task-25 report). The brief's test obtains the
// commit it reports against with
//
//	commit, err := h.ds.Commit(ctx, g.sessionOf(0), g.id, h.commitFor(t, g, g.Epoch()))
//
// and no commit in this repository can be ACCEPTED: `testkit/fixtures/ds-1500` ships exactly one
// GroupInfo, at epoch 6, and invariant 4's step (6) wants epoch n+1 — the blocker
// `commit_test.go` records in two skips, `proposal_test.go` in a third and the plan in deviations
// B21/B24. Writing the test the brief's way would have produced a fourth skip and left invariant 9
// — which is entirely about the fork_reports rows and the quarantine, not about how the commit got
// into the log — completely unexercised.
//
// So the accepted commit is SEEDED into the handshake log, exactly as `appendHandshake` already
// seeds the winner `E_COMMIT_CONFLICT` names, with the one thing invariant 9 reads that
// `appendHandshake` does not write: the committer's device and leaf. Everything else — the store,
// the quorum, the distinctness, the quarantine flag and the instance Remove — is the real thing.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

// Invariant 9: three DISTINCT reporter devices against one commit quarantine the committer.
func TestThreeDistinctReportersQuarantineTheCommitterAndOneDeviceDoesNot(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 4)
	commit := h.acceptedCommitBy(t, g, 0)

	// Three reports from ONE device must not quarantine anybody.
	for range 3 {
		if err := h.ds.ForkReport(context.Background(), g.sessionOf(1), g.id, commit.Epoch, commit.Seq, "cannot process"); err != nil {
			t.Fatalf("ForkReport: %v", err)
		}
	}
	if h.isQuarantined(t, g.members[0]) {
		t.Fatal("three reports from one device must not quarantine the committer")
	}

	for _, reporter := range []int{1, 2, 3} {
		if err := h.ds.ForkReport(context.Background(), g.sessionOf(reporter), g.id, commit.Epoch, commit.Seq, "cannot process"); err != nil {
			t.Fatalf("ForkReport from %d: %v", reporter, err)
		}
	}
	if !h.isQuarantined(t, g.members[0]) {
		t.Fatal("three distinct reporters must quarantine the committer")
	}
	if !h.hasOutstandingRemoveOfLeaf(t, g, g.leafOf(0)) {
		t.Fatal("quarantine must also Remove the committer's leaf by an instance proposal")
	}
}

// The quarantine is told to the composition root at once (OnQuarantine), with the committer, so the
// device is cut from every live call without waiting for the members' Removes or the room sweep;
// reports short of the quorum tell nobody. The hook runs with the group's lock free, so whatever it
// does cannot freeze the group (the composition root's hook only queues the cut).
func TestTheQuarantineTellsOnQuarantineTheCommitter(t *testing.T) {
	h := newDSHarness(t)
	var mu sync.Mutex
	var told []id.ID
	var lockHeld bool
	var gid id.ID
	if err := h.ds.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	d, err := ds.New(ds.Options{
		Store: h.repo, Wasm: h.wasm, Gateway: h.gw, Clock: h.clk,
		Keys: testInstanceKeys(t), Policy: ds.DefaultPolicy(), Channels: h.channels, ACL: h.acl,
		OnQuarantine: func(_ context.Context, device id.ID) {
			mu.Lock()
			told = append(told, device)
			lockHeld = lockHeld || !h.ds.GroupLockFree(gid)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("ds.New: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })
	h.ds = d
	g := h.groupWithMembers(t, 4)
	mu.Lock()
	gid = g.id
	mu.Unlock()
	commit := h.acceptedCommitBy(t, g, 0)
	for i, reporter := range []int{1, 2, 3} {
		if err := h.ds.ForkReport(context.Background(), g.sessionOf(reporter), g.id, commit.Epoch, commit.Seq, "cannot process"); err != nil {
			t.Fatalf("ForkReport from %d: %v", reporter, err)
		}
		mu.Lock()
		n := len(told)
		mu.Unlock()
		if i < 2 && n != 0 {
			t.Fatalf("OnQuarantine was told before the quorum (%d reports)", i+1)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(told) != 1 || told[0] != g.members[0] {
		t.Fatalf("OnQuarantine was told %v, want the committer %s once", told, g.members[0])
	}
	if lockHeld {
		t.Fatal("OnQuarantine ran under the group's lock")
	}
}

// The quarantine flag is READ: a device three reporters quarantined cannot read the GroupInfo and
// tree a rejoin needs, nor resync itself back into the epoch (deviation B36's ruling). Before the
// final review nothing read `devices.quarantined_at`, so the committer answered /info as a member
// and could rejoin by external commit the moment its leaf was removed.
func TestAQuarantinedDeviceCannotReadOrRejoinTheGroup(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 4)
	commit := h.acceptedCommitBy(t, g, 0)
	ctx := context.Background()
	committer := g.sessionOf(0)
	if _, err := h.ds.Info(ctx, g.id, committer); err != nil {
		t.Fatalf("before the quorum the committer reads the group: %v", err)
	}
	for _, reporter := range []int{1, 2, 3} {
		if err := h.ds.ForkReport(ctx, g.sessionOf(reporter), g.id, commit.Epoch, commit.Seq, "cannot process"); err != nil {
			t.Fatalf("ForkReport from %d: %v", reporter, err)
		}
	}
	if !h.isQuarantined(t, g.members[0]) {
		t.Fatal("the quorum did not quarantine the committer")
	}

	var dsErr *ds.Error
	if _, err := h.ds.Info(ctx, g.id, committer); !errors.As(err, &dsErr) || dsErr.Code != "E_FORBIDDEN" {
		t.Errorf("/info from the quarantined device: %v, want E_FORBIDDEN", err)
	}
	if _, err := h.ds.Tree(ctx, g.id, committer); !errors.As(err, &dsErr) || dsErr.Code != "E_FORBIDDEN" {
		t.Errorf("/tree from the quarantined device: %v, want E_FORBIDDEN", err)
	}
	_, err := h.ds.Resync(ctx, committer, g.id, ds.ResyncRequest{ExternalCommit: []byte{0x01}, GroupInfo: []byte{0x01}})
	if !errors.As(err, &dsErr) || dsErr.Code != "E_FORBIDDEN" || !strings.Contains(dsErr.Detail, "quarantined") {
		t.Errorf("/resync from the quarantined device: %v, want E_FORBIDDEN for the quarantine", err)
	}
	// The reporters are unaffected.
	if _, err := h.ds.Info(ctx, g.id, g.sessionOf(1)); err != nil {
		t.Errorf("a reporter's /info: %v", err)
	}
}

// A quorum against the instance's OWN handshake — one with no sender device, which is what an
// external-sender proposal leaves in the log — quarantines nobody. Without the guard the
// committer lookup dereferences a nil device id.
func TestAForkQuorumAgainstAnInstanceHandshakeQuarantinesNobody(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 4)
	seq := h.seedHandshake(t, g, nil, nil)

	for _, reporter := range []int{1, 2, 3} {
		if err := h.ds.ForkReport(context.Background(), g.sessionOf(reporter), g.id, g.epoch(t), seq, "cannot process"); err != nil {
			t.Fatalf("ForkReport from %d: %v", reporter, err)
		}
	}
	if h.hasOutstandingRemoveOfLeaf(t, g, g.leafOf(0)) {
		t.Fatal("a quorum against an instance handshake must remove nobody")
	}
}

// A report against a seq the log no longer holds — a real GAP, which is what a pruned handshake
// leaves behind — is recorded and quarantines nobody. The client's view of the log is not the
// instance's, and a fork report is a bug report, not an accusation the instance must be able to
// resolve.
//
// This is the test for `quarantineCommitterOf`'s identity clause (`rows[0].Seq != seq`).
// `ListHandshakes`' `fromSeq` is INCLUSIVE, so a page from a seq the log has lost starts at the
// next SURVIVING handshake; without the clause its committer — a device nobody reported — is
// quarantined and Removed.
func TestAForkQuorumAgainstAPrunedSeqQuarantinesNobody(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 4)

	// `skipped` is reserved off the group's own sequence and never appended; the NEXT seq carries
	// member 0's commit, so the log really does have a hole with a known committer behind it.
	skipped, err := h.repo.NextSeq(context.Background(), g.id)
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	survivor := h.acceptedCommitBy(t, g, 0)
	if survivor.Seq <= skipped {
		t.Fatalf("the surviving handshake is at seq %d, want above the gap at %d", survivor.Seq, skipped)
	}

	for _, reporter := range []int{1, 2, 3} {
		if err := h.ds.ForkReport(context.Background(), g.sessionOf(reporter), g.id, g.epoch(t), skipped, "cannot process"); err != nil {
			t.Fatalf("ForkReport from %d: %v", reporter, err)
		}
	}
	if n := h.countForkReporters(t, g, skipped); n != 3 {
		t.Fatalf("fork reporters = %d, want 3", n)
	}
	if h.isQuarantined(t, g.members[0]) {
		t.Fatal("a quorum against a seq the log no longer holds must quarantine nobody")
	}
	if h.hasOutstandingRemoveOfLeaf(t, g, g.leafOf(0)) {
		t.Fatal("a quorum against a seq the log no longer holds must Remove nobody")
	}
}

// A report for a seq the instance NEVER appended is refused, and no row is written.
//
// Without the bound any member may spend one `fork_reports` row per arbitrary uint64 — the primary
// key (group_id, seq, reporter_device) makes every distinct seq a new row, each carrying up to 256
// bytes of reason — and nothing in the tree prunes that table, so the endpoint is unbounded
// member-driven storage growth. A client cannot have observed a handshake the instance never
// appended, and `protocol/02-delivery-service.md:81` names E_NOT_FOUND as this endpoint's one
// refusal.
func TestAForkReportAgainstASeqTheInstanceNeverAppendedIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 4)

	err := h.ds.ForkReport(context.Background(), g.sessionOf(1), g.id, g.epoch(t), 9999, "cannot process")
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
		t.Fatalf("got %v, want E_NOT_FOUND", err)
	}
	if n := h.countForkReporters(t, g, 9999); n != 0 {
		t.Fatalf("fork reporters = %d, want 0: the row must be refused BEFORE it is written", n)
	}
}

// …and seq 0 the same way, on the FIRST report rather than after two rows have already been
// accepted: the first handshake a group ever appends is 1.
func TestAForkReportOfSeqZeroIsRefusedOnTheFirstReport(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 4)

	err := h.ds.ForkReport(context.Background(), g.sessionOf(1), g.id, g.epoch(t), 0, "cannot process")
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
		t.Fatalf("got %v, want E_NOT_FOUND", err)
	}
	if n := h.countForkReporters(t, g, 0); n != 0 {
		t.Fatalf("fork reporters = %d, want 0", n)
	}
}

func TestForkReportPreservesSequenceRefusalsBeforeCapabilityGate(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 4)
	seq := h.seedHandshake(t, g, &g.members[0], &g.leaves[0])
	h.repo.markInteraction(g.id)
	ctx := context.Background()

	for _, wireVersion := range []uint64{0, 2} {
		t.Run(fmt.Sprintf("wire_%d", wireVersion), func(t *testing.T) {
			session := g.sessionOf(1)
			session.WireVersion = wireVersion
			for _, invalidSeq := range []uint64{0, seq + 1} {
				err := h.ds.ForkReport(ctx, session, g.id, g.epoch(t), invalidSeq, "cannot process")
				var dsErr *ds.Error
				if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
					t.Fatalf("invalid seq %d report = %v, want E_NOT_FOUND", invalidSeq, err)
				}
				if n := h.countForkReporters(t, g, invalidSeq); n != 0 {
					t.Fatalf("invalid seq %d wrote %d fork reports", invalidSeq, n)
				}
			}
			err := h.ds.ForkReport(ctx, session, g.id, g.epoch(t), seq, "cannot process")
			var upgrade *capability.UpgradeRequired
			if !errors.As(err, &upgrade) {
				t.Fatalf("valid target report = %v, want typed upgrade refusal", err)
			}
			if n := h.countForkReporters(t, g, seq); n != 0 {
				t.Fatalf("wire %d refusal wrote %d fork reports", wireVersion, n)
			}
		})
	}

	wireThree := g.sessionOf(1)
	wireThree.WireVersion = 3
	if err := h.ds.ForkReport(ctx, wireThree, g.id, g.epoch(t), seq, "cannot process"); err != nil {
		t.Fatalf("wire 3 report: %v", err)
	}
	if n := h.countForkReporters(t, g, seq); n != 1 {
		t.Fatalf("wire 3 reports = %d, want 1", n)
	}
}

// A reason longer than the bound is truncated on a RUNE boundary, not on a byte boundary.
//
// `internal/cborx/decode.go:149` guarantees the incoming text is valid UTF-8, and slicing it at
// byte 256 destroys that guarantee: Postgres' `reason TEXT NOT NULL`
// (internal/store/postgres/migrations/00002_mls.sql:136) refuses invalid UTF-8 outright with
// `invalid byte sequence for encoding "UTF8"`, while SQLite's STRICT TEXT stores the broken bytes
// without a word — so the two engines diverge and only Postgres answers 500, on a path no local
// test run reaches. The repository's own convention for bounding free text is
// `internal/auth/handle.go:42,88`, which counts runes.
func TestALongMultiByteForkReasonIsStoredAsValidUTF8(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 4)
	commit := h.acceptedCommitBy(t, g, 0)
	// '€' is three bytes, so byte 256 of this string falls INSIDE a rune: 85 runes are 255 bytes
	// and the 86th spans bytes 256, 257 and 258.
	reason := strings.Repeat("€", 200)

	session := g.sessionOf(1)
	if err := h.ds.ForkReport(context.Background(), session, g.id, commit.Epoch, commit.Seq, reason); err != nil {
		t.Fatalf("ForkReport: %v", err)
	}

	got := h.storedForkReason(t, g, commit.Seq, session.DeviceID)
	if !utf8.ValidString(got) {
		t.Fatalf("the stored reason is not valid UTF-8 (%d bytes): Postgres refuses the INSERT", len(got))
	}
	if len(got) > 256 {
		t.Fatalf("the stored reason is %d bytes, want at most 256", len(got))
	}
	if !strings.HasPrefix(reason, got) {
		t.Fatal("the stored reason must be a prefix of the reported one")
	}
	// …and the bound stays tight: the truncation gives up at most one rune, not the whole tail.
	if len(got) < 256-utf8.UTFMax {
		t.Fatalf("the stored reason is %d bytes, want within %d of the 256-byte bound", len(got), utf8.UTFMax)
	}
}

// A reason that already fits is stored exactly as it was reported, multi-byte runes and all.
func TestAForkReasonThatFitsIsStoredUnchanged(t *testing.T) {
	h := newDSHarness(t)
	g := h.groupWithMembers(t, 4)
	commit := h.acceptedCommitBy(t, g, 0)
	reason := "cannot process — épée ✓"

	session := g.sessionOf(1)
	if err := h.ds.ForkReport(context.Background(), session, g.id, commit.Epoch, commit.Seq, reason); err != nil {
		t.Fatalf("ForkReport: %v", err)
	}
	if got := h.storedForkReason(t, g, commit.Seq, session.DeviceID); got != reason {
		t.Fatalf("stored reason = %q, want %q", got, reason)
	}
}

// DS-2 of the server-half review: the reported commit's sender leaf is where the committer sat THEN.
// By the time the quorum lands it may have left (a resync, a leave) and a newcomer may sit at that
// index, since MLS fills the leftmost blank leaf. The quarantine removes the committer's device where
// it is now — or proposes nothing when it holds no leaf — and never the newcomer.
func TestAForkQuorumRemovesTheCommittersCurrentLeafNotTheReportedOne(t *testing.T) {
	for _, c := range []struct {
		name  string
		moved bool // the committer holds another leaf now; otherwise none
	}{{"the committer moved to another leaf", true}, {"the committer holds no leaf", false}} {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			ctx := context.Background()
			g := h.groupWithMembers(t, 4)
			commit := h.acceptedCommitBy(t, g, 0)
			committer, reported := g.members[0], g.leafOf(0)

			members, err := h.repo.ListMembers(ctx, g.id)
			if err != nil {
				t.Fatalf("ListMembers: %v", err)
			}
			newcomer, elsewhere := id.New(), uint32(10)
			for i := range members {
				switch {
				case members[i].LeafIndex == reported:
					members[i].DeviceID = newcomer
				case c.moved && members[i].LeafIndex == elsewhere:
					members[i].DeviceID = committer
				}
			}
			if err := h.repo.Tx(ctx, func(tx store.Repository) error {
				return tx.ReplaceMembers(ctx, g.id, g.epoch(t), members)
			}); err != nil {
				t.Fatalf("ReplaceMembers: %v", err)
			}

			for _, reporter := range []int{1, 2, 3} {
				if err := h.ds.ForkReport(ctx, g.sessionOf(reporter), g.id, commit.Epoch, commit.Seq, "cannot process"); err != nil {
					t.Fatalf("ForkReport from %d: %v", reporter, err)
				}
			}
			if !h.isQuarantined(t, committer) {
				t.Fatal("the quorum did not quarantine the committer")
			}
			if h.hasOutstandingRemoveOfLeaf(t, g, reported) {
				t.Fatalf("the quarantine proposed removing leaf %d, which the newcomer holds now", reported)
			}
			got := h.instanceRemovesOf(t, g.id, elsewhere)
			if c.moved && (len(got) != 1 || got[0].TargetDevice == nil || *got[0].TargetDevice != committer) {
				t.Fatalf("instance Removes of leaf %d: %+v, want one of the committer, which holds it now", elsewhere, got)
			}
			if !c.moved && len(got) != 0 {
				t.Fatalf("instance Removes of leaf %d: %+v, want none", elsewhere, got)
			}
		})
	}
}

// ---------------------------------------------------------------- the helpers

// acceptedCommitBy seeds the handshake log with the commit member `i` committed, and returns the
// same [seq, epoch] pair an accepted `DS.Commit` would have. It also writes the committer's
// `users` and `devices` rows: `QuarantineDevice` is an UPDATE on `devices`, and the fixture's
// members exist only as MLS leaves replayed into `mls_members` by Register, so without them the
// quarantine would be a silent no-op.
func (h *dsHarness) acceptedCommitBy(t *testing.T, g *dsGroup, i int) commitLogEntry {
	t.Helper()
	session := g.sessionOf(i)
	h.account(t, session.UserID, session.DeviceID)
	device := g.members[i]
	leaf := g.leaves[i]
	return commitLogEntry{Seq: h.seedHandshake(t, g, &device, &leaf), Epoch: g.epoch(t)}
}

type commitLogEntry struct {
	Seq   uint64
	Epoch uint64
}

// seedHandshake appends one commit handshake at the group's own next seq. A nil device is the
// instance's own external sender, which names no leaf either.
func (h *dsHarness) seedHandshake(t *testing.T, g *dsGroup, device *id.ID, leaf *uint32) uint64 {
	t.Helper()
	ctx := context.Background()
	seq, err := h.repo.NextSeq(ctx, g.id)
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	if err := h.repo.AppendHandshake(ctx, store.HandshakeRow{
		GroupID:      g.id,
		Seq:          seq,
		Epoch:        g.epoch(t),
		Kind:         1, // commit
		SenderLeaf:   leaf,
		SenderDevice: device,
		Blob:         []byte{0x01},
		Created:      h.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("AppendHandshake: %v", err)
	}
	return seq
}

// isQuarantined reads the device's own row: invariant 9's "a flag on the device".
func (h *dsHarness) isQuarantined(t *testing.T, device id.ID) bool {
	t.Helper()
	row, err := h.repo.GetDevice(context.Background(), device)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	return row.QuarantinedAt != nil
}

// hasOutstandingRemoveOfLeaf is invariant 9's other half: the instance Remove of the quarantined
// committer's leaf.
func (h *dsHarness) hasOutstandingRemoveOfLeaf(t *testing.T, g *dsGroup, leaf uint32) bool {
	t.Helper()
	rows, err := h.ds.Outstanding(context.Background(), g.id)
	if err != nil {
		t.Fatalf("Outstanding: %v", err)
	}
	for _, r := range rows {
		if r.Origin != 0 || r.VoidAt != nil || r.Kind != uint8(mlswasi.ProposalRemove) {
			continue
		}
		if r.TargetLeaf != nil && *r.TargetLeaf == leaf {
			return true
		}
	}
	return false
}

// storedForkReason reads the `reason` column straight off the harness's database file, the way
// `countRows` reads a table's size. `store.MLS`'s whole fork_reports surface is `PutForkReport`
// and `CountForkReporters` — there is no reader — and what the row actually HOLDS is the claim the
// two truncation tests make.
func (h *dsHarness) storedForkReason(t *testing.T, g *dsGroup, seq uint64, reporter id.ID) string {
	t.Helper()
	db, err := sqlite.OpenRead(h.path)
	if err != nil {
		t.Fatalf("sqlite.OpenRead: %v", err)
	}
	defer func() { _ = db.Close() }()
	var reason string
	row := db.QueryRowContext(t.Context(),
		"SELECT reason FROM fork_reports WHERE group_id = ? AND seq = ? AND reporter_device = ?",
		g.id, int64(seq), reporter)
	if err := row.Scan(&reason); err != nil {
		t.Fatalf("read the fork report's reason: %v", err)
	}
	return reason
}

func (h *dsHarness) countForkReporters(t *testing.T, g *dsGroup, seq uint64) int64 {
	t.Helper()
	n, err := h.repo.CountForkReporters(context.Background(), g.id, seq)
	if err != nil {
		t.Fatalf("CountForkReporters: %v", err)
	}
	return n
}

// sessionOf is the enrolled session of the group's i-th exposed member device, as
// `groupWithMembers` recorded it.
func (g *dsGroup) sessionOf(i int) auth.Session { return g.h.sessions[g.members[i]] }

// leafOf is the MLS leaf the i-th exposed member sits at.
func (g *dsGroup) leafOf(i int) uint32 { return g.leaves[i] }

// epoch is the group's current epoch, read back from SQL — R12's record.
func (g *dsGroup) epoch(t *testing.T) uint64 {
	t.Helper()
	row, err := g.h.repo.GetGroup(context.Background(), g.id)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	return row.Epoch
}

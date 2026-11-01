package ds_test

// retention_test.go is task 26's half of the delivery service's tests: invariant 10's two
// independent deletion triggers, the eligibility of the cursors that hold the delivery floor, and
// the sweeper that applies both on one tick.
//
// The harness helpers these tests need live here rather than in harness_test.go for the reason
// message_harness_test.go and election_harness_test.go give: `dsHarness` is one type and
// harness_test.go is already the longest file in the package.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
	"github.com/jonasthim/dilla/internal/store/sqlite"
)

// Delivery retention: handshakes are kept 30 days, and a ?from= below the floor is E_PRUNED,
// which tells the client to resync rather than to retry.
//
// DEVIATION from the brief's literal test, which drives two `h.ds.Commit` calls through
// `h.commitFor`. No commit in this package can be ACCEPTED — the committed fixture ships one
// GroupInfo at epoch 6 and invariant 4 wants epoch n+1, the blocker that moved
// `TestAnAcceptedCommitFansOutHandshakeEpochChangedAndWelcomes` to internal/testkit — and there is no
// `commitFor` helper. The handshake log is therefore seeded exactly as every other test in this
// package seeds it (`appendHandshake`, commit_test.go), which is what
// `TestACatchUpBelowTheRetentionFloorIsPruned` already does; the assertion — the SWEEP deletes the
// row older than the window, and the catch-up below the resulting floor is E_PRUNED/410 — is the
// brief's own.
func TestAHandshakeOlderThanThirtyDaysIsPrunedAndFromBelowTheFloorIsEPruned(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)

	// Seq 1 is written now and seq 20 thirty-one days later, so exactly one of the two is older
	// than HandshakeRetention when the sweep runs.
	h.appendHandshake(t, reg.GroupID, 1, 6, 1, []byte("swept"))
	h.clk.Advance(31 * 24 * time.Hour)
	h.appendHandshake(t, reg.GroupID, 20, 7, 1, []byte("commit"))

	report, err := h.ds.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.HandshakesPruned != 1 {
		t.Fatalf("the sweep deleted %d handshakes, want the one at seq 1: "+
			"the hole below the floor has to be real", report.HandshakesPruned)
	}

	_, err = h.ds.Handshakes(ctx, reg.GroupID, session, 0, 100)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_PRUNED" {
		t.Fatalf("from=0 after pruning: got %v, want E_PRUNED", err)
	}
	if dsErr.Status != 410 {
		t.Errorf("status = %d, want 410", dsErr.Status)
	}
	rows, err := h.ds.Handshakes(ctx, reg.GroupID, session, 19, 100)
	if err != nil {
		t.Fatalf("from the floor: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d handshakes, want the 1 that survived", len(rows))
	}
}

// Ciphertext is kept until every ELIGIBLE cursor has passed it. A revoked, disabled or
// 90-day-inactive device does not hold the floor — otherwise one abandoned phone pins a
// community's storage forever.
func TestAnIneligibleDeviceDoesNotHoldThePruneFloor(t *testing.T) {
	for _, c := range []struct {
		name string
		make func(h *dsHarness, t *testing.T, g *dsMessageGroup, laggard id.ID)
	}{
		{"a revoked device", func(h *dsHarness, t *testing.T, _ *dsMessageGroup, laggard id.ID) {
			h.revokeDevice(t, laggard)
		}},
		{"a disabled user's device", func(h *dsHarness, t *testing.T, _ *dsMessageGroup, laggard id.ID) {
			h.disableUserOf(t, laggard)
		}},
		{"a device unseen for 90 days", func(h *dsHarness, t *testing.T, g *dsMessageGroup, laggard id.ID) {
			h.ageCursor(t, g, laggard, 91*24*time.Hour)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			h := newDSHarness(t)
			g := h.group(t)
			laggard := h.laggardMember(t, g) // its cursor stays at 0
			c.make(h, t, g, laggard)

			out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
			if err != nil {
				t.Fatalf("Upload: %v", err)
			}
			if err := h.ds.AdvanceCursor(ctx, g.session, g.id, out.Seq, out.Epoch); err != nil {
				t.Fatalf("AdvanceCursor: %v", err)
			}
			if _, err := h.ds.Sweep(ctx); err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			// The sweep emptied the stream, so a catch-up from before the message is told it is
			// gone (E_PRUNED, emptied_log_test.go) rather than served an empty page; either way
			// the message did not survive, which is what the ineligible device must not prevent.
			rows, err := h.ds.Messages(ctx, g.id, g.session, 0, 10)
			var dsErr *ds.Error
			if errors.As(err, &dsErr) && dsErr.Code == "E_PRUNED" {
				return
			}
			if err != nil {
				t.Fatalf("Messages: %v", err)
			}
			if len(rows) != 0 {
				t.Fatal("every eligible cursor has passed the message; " +
					"an ineligible device must not hold it")
			}
		})
	}
}

// A live laggard DOES hold the floor.
func TestALiveDeviceBehindTheStreamHoldsTheFloor(t *testing.T) {
	ctx := context.Background()
	h := newDSHarness(t)
	g := h.group(t)
	laggard := h.laggardMember(t, g)
	h.online(laggard)

	out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := h.ds.AdvanceCursor(ctx, g.session, g.id, out.Seq, out.Epoch); err != nil {
		t.Fatalf("AdvanceCursor: %v", err)
	}
	if _, err := h.ds.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	rows, err := h.ds.Messages(ctx, g.id, g.session, 0, 10)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(rows) != 1 {
		t.Fatal("a live member that has not acknowledged the message must hold it")
	}
}

// Archival retention (R28/D14) is a separate half: expires NULL means retained indefinitely, and
// a passed expires deletes even though no cursor has moved and the delivery window has not closed.
//
// DEVIATION from the brief's literal test, which also calls `AdvanceCursor` to the expiring
// message's seq. The brief's own prose for this test says it "advances **no** cursor and stays
// inside the 30-day window", and the two are not the same test: with the acknowledgement in place
// the delivery half deletes BOTH rows (the cursor floor is the higher seq, and `seq <= floor`
// covers the lower one), which is rule 1 firing, not rule 2 — and the brief's assertion that the
// `expires IS NULL` row survives is then unsatisfiable for any definition of the two rules. The
// prose is the normative half and is what this test does.
func TestArchivalRetentionIsIndependentOfDeliveryRetention(t *testing.T) {
	ctx := context.Background()
	h := newDSHarness(t)
	g := h.group(t)
	kept, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	expiring, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	h.setExpires(t, g, expiring.Seq, h.clk.Now().Add(time.Hour))

	h.clk.Advance(2 * time.Hour)
	if _, err := h.ds.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	rows, err := h.repo.ListAppMessages(ctx, g.id, 0, 10)
	if err != nil {
		t.Fatalf("ListAppMessages: %v", err)
	}
	if len(rows) != 1 || rows[0].Seq != kept.Seq {
		t.Fatalf("got %v, want only the message with expires = NULL (retained)", rows)
	}
	// And the catch-up: the expired message is NEWER than the retained one, so a floor raised to
	// its seq would stand above a message that still exists. Plan 2 task 8's retention ruling
	// exempts archival deletions from the floor, so the catch-up serves the survivor instead of
	// answering E_PRUNED for a range it can serve.
	got, err := h.ds.Messages(ctx, g.id, g.session, kept.Seq, 10)
	if err != nil {
		t.Fatalf("a catch-up from the retained message = %v; an archival expiry above it must not prune it", err)
	}
	if len(got) != 1 || got[0].Seq != kept.Seq {
		t.Fatalf("catch-up = %v, want the retained message alone", got)
	}
}

// The sweeper is idempotent and bounded: a second run changes nothing, and one run never scans
// the whole table.
func TestTheSweeperIsIdempotentAndBounded(t *testing.T) {
	ctx := context.Background()
	h := newDSHarness(t)
	g := h.group(t)
	for range 5 {
		if _, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0); err != nil {
			t.Fatalf("Upload: %v", err)
		}
	}
	h.clk.Advance(31 * 24 * time.Hour)

	first, err := h.ds.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	second, err := h.ds.Sweep(ctx)
	if err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if second.MessagesPruned != 0 || second.HandshakesPruned != 0 {
		t.Fatalf("a second sweep pruned %+v, want nothing", second)
	}
	if first.MessagesPruned == 0 {
		t.Fatal("the first sweep pruned nothing")
	}
}

// The delivery-cursor half of the sweep deletes whenever every eligible cursor has passed, in a
// group of any age, so the catch-up has to see it. What it must NOT do is read "some
// cursor has moved" as "a message is gone": `from` is a cursor in the ONE seq space handshakes and
// application messages share, so the gap between a member's cursor and the oldest surviving
// message is routinely handshakes. A group whose early seqs are handshakes has lost nothing, and
// protocol/02 makes E_PRUNED mean "resync by external commit" — a full rejoin for a healthy
// member.
func TestAnAdvancedCursorOverHandshakeSeqsDoesNotForceAResync(t *testing.T) {
	ctx := context.Background()
	h := newDSHarness(t)
	g := h.group(t)

	// Seqs 1-3 are handshakes: the seq space is the group's, not the table's, so each one is
	// burned on `mls_groups.seq` and recorded in the handshake log, exactly as Commit does it.
	for seq := uint64(1); seq <= 3; seq++ {
		if err := h.repo.Tx(ctx, func(tx store.Repository) error {
			_, err := tx.NextSeq(ctx, g.id)
			return err
		}); err != nil {
			t.Fatalf("NextSeq: %v", err)
		}
		h.appendHandshake(t, g.id, seq, g.Epoch(), 1, []byte("commit"))
	}
	out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if out.Seq != 4 {
		t.Fatalf("the message took seq %d, want 4", out.Seq)
	}
	// The member has acknowledged the three handshakes and nothing else. Its cursor is above zero
	// and below the message floor — the shape every healthy catch-up has.
	if err := h.ds.AdvanceCursor(ctx, g.session, g.id, 3, g.Epoch()); err != nil {
		t.Fatalf("AdvanceCursor: %v", err)
	}

	rows, err := h.ds.Messages(ctx, g.id, g.session, 0, 10)
	if err != nil {
		t.Fatalf("nothing has been deleted in this group; the catch-up must be served: %v", err)
	}
	if len(rows) != 1 || rows[0].Seq != out.Seq {
		t.Fatalf("got %d rows, want the one message at seq %d", len(rows), out.Seq)
	}
}

// The hole the clause above must still catch: a cursor-floor prune is bounded by no age at all, so
// a group younger than MessageRetention CAN have lost a message. Below that floor the answer must
// be E_PRUNED, not a silently short list.
func TestACatchUpBelowACursorFloorPruneIsEPrunedInAYoungGroup(t *testing.T) {
	ctx := context.Background()
	h := newDSHarness(t)
	g := h.group(t)
	first, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	second, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("second Upload: %v", err)
	}
	// Every eligible cursor has passed the first message and only the first: delivery retention is
	// satisfied for it while the group is minutes old.
	if err := h.ds.AdvanceCursor(ctx, g.session, g.id, first.Seq, first.Epoch); err != nil {
		t.Fatalf("AdvanceCursor: %v", err)
	}
	report, err := h.ds.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.MessagesPruned != 1 {
		t.Fatalf("the sweep deleted %d messages, want the one at seq %d", report.MessagesPruned, first.Seq)
	}

	_, err = h.ds.Messages(ctx, g.id, g.session, 0, 10)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_PRUNED" {
		t.Fatalf("a catch-up below the cursor-floor prune: got %v, want E_PRUNED", err)
	}
	if dsErr.Status != 410 {
		t.Errorf("status = %d, want 410", dsErr.Status)
	}
	rows, err := h.ds.Messages(ctx, g.id, g.session, first.Seq+1, 10)
	if err != nil {
		t.Fatalf("a catch-up from one past the deleted seq must be served: %v", err)
	}
	if len(rows) != 1 || rows[0].Seq != second.Seq {
		t.Fatalf("got %d rows, want the survivor at seq %d", len(rows), second.Seq)
	}
}

// `MinCursor` returning 0 means NO ELIGIBLE DEVICE EXISTS, which must delete nothing rather than
// everything. The three ineligibility rules are what make that reachable while cursors are on
// file: here both cursors have passed the message and both are outside the 90-day horizon, so the
// floor is 0 and the young ciphertext stays.
func TestAGroupWithOnlyIneligibleCursorsKeepsItsCiphertext(t *testing.T) {
	ctx := context.Background()
	h := newDSHarness(t)
	g := h.group(t)
	laggard := h.laggardMember(t, g)

	out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// Both cursors sit ON the message, so an eligibility rule that did not fire would put the
	// floor at out.Seq and delete it. Both were last touched 91 days ago, so none is eligible.
	stale := h.clk.Now().Add(-91 * 24 * time.Hour).Unix()
	for _, device := range []id.ID{g.device, laggard} {
		if err := h.repo.PutCursor(ctx, device, g.id, out.Seq, out.Epoch, stale); err != nil {
			t.Fatalf("PutCursor: %v", err)
		}
	}

	report, err := h.ds.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.MessagesPruned != 0 {
		t.Fatalf("the sweep deleted %d messages; no ELIGIBLE cursor has passed anything, "+
			"so delivery retention deletes nothing", report.MessagesPruned)
	}
	rows, err := h.ds.Messages(ctx, g.id, g.session, 0, 10)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(rows) != 1 || rows[0].Seq != out.Seq {
		t.Fatalf("got %d rows, want the message the sweep must not have touched", len(rows))
	}
}

// Invariant 10 caps ciphertext at thirty days, and a group that invariant 11 closed is still
// ciphertext on the disk. `ListOpenGroups` is the wrong walk for retention: a closed group would
// never be swept again by either half, so its blobs would outlive the promise forever and no other
// path would ever reclaim them.
func TestAClosedGroupStillHasItsCiphertextPruned(t *testing.T) {
	ctx := context.Background()
	h := newDSHarness(t)
	g := h.group(t)
	out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := h.ds.AdvanceCursor(ctx, g.session, g.id, out.Seq, out.Epoch); err != nil {
		t.Fatalf("AdvanceCursor: %v", err)
	}
	if err := h.repo.CloseGroup(ctx, g.id, h.clk.Now().Unix()); err != nil {
		t.Fatalf("CloseGroup: %v", err)
	}

	if _, err := h.ds.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n := h.countRows(t, "mls_app_messages"); n != 0 {
		t.Fatalf("a closed group kept %d ciphertext rows; retention does not stop at close", n)
	}
}

// A device has no `device_cursors` row until its FIRST `AdvanceCursor` call, so a member that has
// been quiet since the group's genesis is ABSENT from `MinCursor` rather than sitting in it at
// zero: it does not hold the prune floor, and a cursor-floor sweep deletes ciphertext it never
// received. When it finally speaks — an ordinary presence `AdvanceCursor(0)`, which the endpoint
// keeps idempotent for retried and reordered posts — the floor recomputed at that moment is a
// different number from the floor the sweep actually deleted at, and it is lower. Its catch-up
// must still answer E_PRUNED: a silently short list, with no error and no resync, is the one thing
// this predicate exists to prevent.
func TestAQuietMemberIsToldItsCiphertextIsGoneRatherThanServedAShortList(t *testing.T) {
	ctx := context.Background()
	h := newDSHarness(t)
	g := h.group(t)
	quiet := h.silentMember(t, g) // a genesis member with NO device_cursors row at all

	var last ds.UploadResult
	for range 5 {
		out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
		if err != nil {
			t.Fatalf("Upload: %v", err)
		}
		last = out
	}
	// Only the uploader acknowledges, and it is the only cursor ON FILE: the floor the sweep
	// deletes at is its own, and the quiet member is not in the aggregate at all.
	if err := h.ds.AdvanceCursor(ctx, g.session, g.id, last.Seq, last.Epoch); err != nil {
		t.Fatalf("AdvanceCursor: %v", err)
	}
	report, err := h.ds.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.MessagesPruned != 5 {
		t.Fatalf("the sweep deleted %d messages, want the 5 uploaded so far: "+
			"the loss the quiet member has to be told about must be real", report.MessagesPruned)
	}
	survivor, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload after the sweep: %v", err)
	}

	// The quiet member's FIRST cursor write, at zero. It acknowledges nothing; it only makes the
	// row exist, which is enough to drag a freshly recomputed MinCursor down to 0.
	quietSession := h.sessions[quiet]
	if err := h.ds.AdvanceCursor(ctx, quietSession, g.id, 0, g.Epoch()); err != nil {
		t.Fatalf("the quiet member's first AdvanceCursor: %v", err)
	}
	_, err = h.ds.Messages(ctx, g.id, quietSession, 0, 10)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_PRUNED" {
		t.Fatalf("the quiet member's catch-up from 0: got %v, want E_PRUNED — "+
			"5 application messages it was entitled to were deleted", err)
	}
	if dsErr.Status != 410 {
		t.Errorf("status = %d, want 410", dsErr.Status)
	}
	// A cursor contiguous with the surviving floor is still served, so the refusal above is the
	// hole talking and not a group-wide refusal.
	rows, err := h.ds.Messages(ctx, g.id, quietSession, survivor.Seq, 10)
	if err != nil {
		t.Fatalf("a catch-up from the first surviving seq must be served: %v", err)
	}
	if len(rows) != 1 || rows[0].Seq != survivor.Seq {
		t.Fatalf("got %d rows, want the survivor at seq %d", len(rows), survivor.Seq)
	}
}

// The same divergence through the other gap, and the reason the answer is a recorded high-water
// rather than a member-shaped `MinCursor`: a device unseen for 90 days is INELIGIBLE, so it does
// not hold the floor and the sweep deletes past it — and then it comes back. Its first
// acknowledgement moves `updated` inside the horizon again, so a floor recomputed at that moment
// has it in the aggregate at the low seq it left off at. What the sweep deleted does not un-happen,
// and the returning device must be told so.
func TestAReturningIdleDeviceIsToldItsCiphertextIsGone(t *testing.T) {
	ctx := context.Background()
	h := newDSHarness(t)
	g := h.group(t)
	idle := h.laggardMember(t, g)
	h.ageCursor(t, g, idle, 91*24*time.Hour) // on file, but last touched outside the horizon

	var last ds.UploadResult
	for range 3 {
		out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
		if err != nil {
			t.Fatalf("Upload: %v", err)
		}
		last = out
	}
	if err := h.ds.AdvanceCursor(ctx, g.session, g.id, last.Seq, last.Epoch); err != nil {
		t.Fatalf("AdvanceCursor: %v", err)
	}
	report, err := h.ds.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.MessagesPruned != 3 {
		t.Fatalf("the sweep deleted %d messages, want the 3 the idle device did not hold",
			report.MessagesPruned)
	}
	survivor, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload after the sweep: %v", err)
	}

	idleSession := h.sessions[idle]
	if err := h.ds.AdvanceCursor(ctx, idleSession, g.id, 0, g.Epoch()); err != nil {
		t.Fatalf("the returning device's AdvanceCursor: %v", err)
	}
	_, err = h.ds.Messages(ctx, g.id, idleSession, 0, 10)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_PRUNED" {
		t.Fatalf("the returning device's catch-up from 0: got %v, want E_PRUNED — "+
			"3 application messages were deleted while it was away", err)
	}
	rows, err := h.ds.Messages(ctx, g.id, idleSession, survivor.Seq, 10)
	if err != nil {
		t.Fatalf("a catch-up from the first surviving seq must be served: %v", err)
	}
	if len(rows) != 1 || rows[0].Seq != survivor.Seq {
		t.Fatalf("got %d rows, want the survivor at seq %d", len(rows), survivor.Seq)
	}
}

// The high-water only ever rises. The sweep's floor is a MINIMUM over eligible cursors, so it
// falls whenever a device behind the stream becomes eligible — and a later, lower floor must not
// un-say what an earlier, higher one deleted. Seqs 1-2 here are handshakes and 3-5 messages, so a
// high-water walked back to 2 would find the window fully covered by the handshake log and serve a
// silently short list.
func TestTheRetentionHighWaterNeverWalksBack(t *testing.T) {
	ctx := context.Background()
	h := newDSHarness(t)
	g := h.group(t)
	behind := h.laggardMember(t, g)

	// Seqs 1-2 are handshakes, burned on the group's counter exactly as Commit burns them.
	for seq := uint64(1); seq <= 2; seq++ {
		if err := h.repo.Tx(ctx, func(tx store.Repository) error {
			_, err := tx.NextSeq(ctx, g.id)
			return err
		}); err != nil {
			t.Fatalf("NextSeq: %v", err)
		}
		h.appendHandshake(t, g.id, seq, g.Epoch(), 1, []byte("commit"))
	}
	var last ds.UploadResult
	for range 3 {
		out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
		if err != nil {
			t.Fatalf("Upload: %v", err)
		}
		last = out
	}
	if last.Seq != 5 {
		t.Fatalf("the last message took seq %d, want 5", last.Seq)
	}
	// The device behind the stream is not eligible yet, so the first sweep deletes at seq 5.
	if err := h.repo.PutCursor(ctx, behind, g.id, 0, g.Epoch(),
		h.clk.Now().Add(-91*24*time.Hour).Unix()); err != nil {
		t.Fatalf("PutCursor: %v", err)
	}
	if err := h.ds.AdvanceCursor(ctx, g.session, g.id, last.Seq, last.Epoch); err != nil {
		t.Fatalf("AdvanceCursor: %v", err)
	}
	report, err := h.ds.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.MessagesPruned != 3 {
		t.Fatalf("the first sweep deleted %d messages, want 3", report.MessagesPruned)
	}
	survivor, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload after the sweep: %v", err)
	}

	// It comes back and acknowledges the two handshakes, which puts the NEXT sweep's floor at 2 —
	// below the 5 the first one deleted at.
	if err := h.ds.AdvanceCursor(ctx, h.sessions[behind], g.id, 2, g.Epoch()); err != nil {
		t.Fatalf("the returning device's AdvanceCursor: %v", err)
	}
	if _, err := h.ds.Sweep(ctx); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}

	_, err = h.ds.Messages(ctx, g.id, h.sessions[behind], 0, 10)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_PRUNED" {
		t.Fatalf("a catch-up from 0 after the lower second floor: got %v, want E_PRUNED — "+
			"the three messages the first sweep deleted are still gone", err)
	}
	rows, err := h.ds.Messages(ctx, g.id, h.sessions[behind], survivor.Seq, 10)
	if err != nil {
		t.Fatalf("a catch-up from the first surviving seq must be served: %v", err)
	}
	if len(rows) != 1 || rows[0].Seq != survivor.Seq {
		t.Fatalf("got %d rows, want the survivor at seq %d", len(rows), survivor.Seq)
	}
}

// ---------------------------------------------------------------- retention harness

// silentMember is a SECOND current member device of the fixture group, of a different user from
// the uploader, with the `users` and `devices` rows the eligibility joins read and — the point —
// NO `device_cursors` row at all. That is what every member looks like before its first
// POST /cursor: absent from the `MinCursor` aggregate rather than sitting in it at zero.
//
// The user must differ from the uploader's: the disabled-user case disables the whole account, and
// a shared user would take the uploader's own cursor out of the floor with it.
func (h *dsHarness) silentMember(t *testing.T, g *dsMessageGroup) id.ID {
	t.Helper()
	ctx := context.Background()
	members, err := h.repo.ListMembers(ctx, g.id)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	for _, m := range members {
		if m.DeviceID == g.device || m.UserID == g.session.UserID || m.RemovedEpoch != nil {
			continue
		}
		h.account(t, m.UserID, m.DeviceID)
		if h.sessions == nil {
			h.sessions = map[id.ID]auth.Session{}
		}
		h.sessions[m.DeviceID] = auth.Session{
			UserID: m.UserID, DeviceID: m.DeviceID, Scope: auth.ScopeEnrolled,
		}
		return m.DeviceID
	}
	t.Fatal("the fixture has no second member of another user")
	return id.ID{}
}

// laggardMember is `silentMember` that HAS spoken: the same second member with a live cursor that
// has acknowledged nothing. It is what "one device is behind the stream" looks like in SQL, and
// the difference from silentMember — a row at zero versus no row — is exactly what decides whether
// the device holds the prune floor.
func (h *dsHarness) laggardMember(t *testing.T, g *dsMessageGroup) id.ID {
	t.Helper()
	device := h.silentMember(t, g)
	if err := h.repo.PutCursor(context.Background(), device, g.id, 0, g.Epoch(),
		h.clk.Now().Unix()); err != nil {
		t.Fatalf("PutCursor: %v", err)
	}
	return device
}

// revokeDevice is the first ineligibility: the device's credential is gone, so nothing will ever
// fetch on its behalf again.
func (h *dsHarness) revokeDevice(t *testing.T, device id.ID) {
	t.Helper()
	if err := h.repo.RevokeDevice(context.Background(), device, h.clk.Now().Unix()); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
}

// disableUserOf is the second: the whole account is disabled, so none of its devices may fetch.
func (h *dsHarness) disableUserOf(t *testing.T, device id.ID) {
	t.Helper()
	ctx := context.Background()
	row, err := h.repo.GetDevice(ctx, device)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	at := h.clk.Now().Unix()
	if err := h.repo.SetUserDisabled(ctx, row.UserID, &at); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
}

// ageCursor is the third: the cursor was last touched longer ago than InactivityRemove, which is
// the horizon MinCursor takes as its `activeSince`.
func (h *dsHarness) ageCursor(t *testing.T, g *dsMessageGroup, device id.ID, age time.Duration) {
	t.Helper()
	if err := h.repo.PutCursor(context.Background(), device, g.id, 0, g.Epoch(),
		h.clk.Now().Add(-age).Unix()); err != nil {
		t.Fatalf("PutCursor: %v", err)
	}
}

// setExpires writes one message's ARCHIVAL deadline. `expires` is a column no Plan-1 path fills —
// `Upload` writes NULL and the community policy that sets it is Plan 2's — and `store.Messages`
// has no method for it (ID1 fixes that method set), so the row is updated straight on the
// harness's database file, exactly as `countRows` reads straight off it.
func (h *dsHarness) setExpires(t *testing.T, g *dsMessageGroup, seq uint64, at time.Time) {
	t.Helper()
	db, err := sqlite.OpenWrite(h.path)
	if err != nil {
		t.Fatalf("sqlite.OpenWrite: %v", err)
	}
	defer func() { _ = db.Close() }()
	res, err := db.ExecContext(t.Context(),
		`UPDATE mls_app_messages SET expires = ? WHERE group_id = ? AND seq = ?`,
		at.Unix(), g.id, int64(seq))
	if err != nil {
		t.Fatalf("UPDATE expires: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	if n != 1 {
		t.Fatalf("UPDATE expires touched %d rows, want 1", n)
	}
}

func TestASweepStillPrunesHandshakesWhenTheClosedGroupVoidFails(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.appendHandshake(t, reg.GroupID, 1, 6, 1, []byte("expired"))
	h.clk.Advance(31 * 24 * time.Hour)
	h.repo.failNext("ListClosedGroupLiveProposals")
	report, err := h.ds.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.HandshakesPruned != 1 {
		t.Fatalf("HandshakesPruned = %d, want 1", report.HandshakesPruned)
	}
	rows, err := h.repo.ListHandshakes(ctx, reg.GroupID, 0, 100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("handshakes after Sweep = %v, %v, want none", rows, err)
	}
}

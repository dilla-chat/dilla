package ds_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// The commit path is exercised against the committed 1,500-leaf fixture, which is the only real
// MLS material this repository holds. What it gives and what it withholds shapes every test here:
//
//   - commits/00..07 are 256-Add batches, commits/08 a Remove of leaf 1, commits/09 a self-update
//     with no proposals at all. All ten are alternatives at the fixture's base epoch 6.
//   - group_info.mls is the GroupInfo of epoch 6, signed by leaf 0.
//
// There is NO GroupInfo at epoch 7, so no commit in this repository can be accepted: invariant 4's
// sixth clause wants epoch n+1 and the fixture stops at n. Every clause that fires BEFORE the
// merge is therefore assertable here and the accepted path is not; task 20's report records the
// gap and names the fixture work that closes it. The two clauses with no committed material at all
// — the committer's own Update, and a GroupInfo signed by another leaf — are skipped by name
// rather than dropped, so the hole is visible in the test output instead of only in prose.

// ---------------------------------------------------------------- invariant 3

// Invariant 3: one commit per epoch. The commit that took epoch n+1 wins, and a later commit for
// epoch n is refused with the winner and the outstanding proposals, so the loser can process the
// winner and re-commit rather than guess.
func TestACommitForAnAlreadyDecidedEpochIsAConflictCarryingTheWinner(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)

	// The group sits at the fixture's epoch 6; the handshake that carried it there is the winner.
	winner := fixtureFile(t, "commits/09.mls")
	h.appendHandshake(t, reg.GroupID, 1, 6, 1, winner)
	ref := h.putDSProposal(t, reg.GroupID, 6, false)

	_, err := h.ds.Commit(ctx, session, reg.GroupID, ds.CommitRequest{
		Epoch:     5,
		Commit:    fixtureFile(t, "commits/09.mls"),
		GroupInfo: dsFixture(t).groupInfo,
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_CONFLICT" {
		t.Fatalf("got %v, want E_COMMIT_CONFLICT", err)
	}
	if dsErr.Status != 409 {
		t.Errorf("status = %d, want 409", dsErr.Status)
	}
	if !bytes.Equal(dsErr.WinningCommit, winner) {
		t.Error("the conflict must carry the winning commit so the loser can process it")
	}
	if len(dsErr.Proposals) != 1 || !bytes.Equal(dsErr.Proposals[0], ref) {
		t.Errorf("proposals = %x, want the one outstanding ref %x", dsErr.Proposals, ref)
	}
}

func TestWireTwoCannotReceiveStaleEpochWinningCommitForInteractionGroup(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)
	h.repo.markInteraction(reg.GroupID)

	// The group is at epoch 6; retain the winning commit that a stale epoch 5 request
	// would otherwise receive in its conflict response.
	winner := fixtureFile(t, "commits/09.mls")
	h.appendHandshake(t, reg.GroupID, 1, 6, 1, winner)

	wireTwo := session
	wireTwo.WireVersion = 2
	err := h.ds.CheckCommit(ctx, wireTwo, reg.GroupID, 5)
	var upgrade *capability.UpgradeRequired
	if !errors.As(err, &upgrade) {
		t.Fatalf("wire 2 CheckCommit error = %v, want typed upgrade refusal", err)
	}
	_, err = h.ds.Commit(ctx, wireTwo, reg.GroupID, ds.CommitRequest{
		Epoch:     5,
		Commit:    fixtureFile(t, "commits/09.mls"),
		GroupInfo: dsFixture(t).groupInfo,
	})
	if !errors.As(err, &upgrade) {
		t.Fatalf("wire 2 Commit error = %v, want typed upgrade refusal", err)
	}
	var wireTwoConflict *ds.Error
	if errors.As(err, &wireTwoConflict) && (wireTwoConflict.Code == "E_COMMIT_CONFLICT" || len(wireTwoConflict.WinningCommit) != 0) {
		t.Fatalf("wire 2 received stale-epoch recovery content: %+v", wireTwoConflict)
	}

	wireThree := session
	wireThree.WireVersion = 3
	_, err = h.ds.Commit(ctx, wireThree, reg.GroupID, ds.CommitRequest{
		Epoch:     5,
		Commit:    fixtureFile(t, "commits/09.mls"),
		GroupInfo: dsFixture(t).groupInfo,
	})
	var conflict *ds.Error
	if !errors.As(err, &conflict) || conflict.Code != "E_COMMIT_CONFLICT" {
		t.Fatalf("wire 3 Commit error = %v, want E_COMMIT_CONFLICT", err)
	}
	if !bytes.Equal(conflict.WinningCommit, winner) {
		t.Fatal("wire 3 conflict must retain the winning commit")
	}
}

// A client AHEAD of the instance is a structural error, not a conflict: there is no winner to hand
// back for an epoch that has not happened, and a conflict body with a null winner is one the
// client cannot act on.
func TestACommitForAnEpochTheInstanceHasNotReachedIsNotAConflict(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegister(t)

	_, err := h.ds.Commit(context.Background(), session, reg.GroupID, ds.CommitRequest{
		Epoch:     7,
		Commit:    fixtureFile(t, "commits/09.mls"),
		GroupInfo: dsFixture(t).groupInfo,
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" {
		t.Fatalf("got %v, want E_COMMIT_INVALID", err)
	}
	if dsErr.Rule != "epoch_ahead" {
		t.Fatalf("rule = %q, want %q", dsErr.Rule, "epoch_ahead")
	}
}

// Deviation B13's reason, as an assertion: the winner is found by epoch over
// `mls_handshakes_by_epoch`, not by paging the log from seq 0. A group with more than one page of
// live handshakes above the retention floor — the ordinary case for any long-lived channel —
// would otherwise answer E_COMMIT_CONFLICT with a null `winning_commit`, and protocol/02 declares
// that element a bstr the losing client processes.
func TestTheConflictWinnerIsFoundByEpochNotByPagingTheLog(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)

	// 600 proposal handshakes at the previous epoch, then the winning commit last. 600 is past
	// the 512-row page `Handshakes` serves, so a paging implementation reads none of it.
	for seq := uint64(1); seq <= 600; seq++ {
		h.appendHandshake(t, reg.GroupID, seq, 5, 0, []byte{byte(seq), byte(seq >> 8)})
	}
	winner := fixtureFile(t, "commits/08.mls")
	h.appendHandshake(t, reg.GroupID, 601, 6, 1, winner)

	_, err := h.ds.Commit(ctx, session, reg.GroupID, ds.CommitRequest{
		Epoch:     5,
		Commit:    fixtureFile(t, "commits/09.mls"),
		GroupInfo: dsFixture(t).groupInfo,
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_CONFLICT" {
		t.Fatalf("got %v, want E_COMMIT_CONFLICT", err)
	}
	if !bytes.Equal(dsErr.WinningCommit, winner) {
		t.Fatalf("winning_commit is %d bytes, want the %d-byte commit at seq 601",
			len(dsErr.WinningCommit), len(winner))
	}
}

// ---------------------------------------------------------------- invariant 4

// Invariant 4, clause by clause. Each case names the `rule` the refusal must carry, so a future
// change that collapses two clauses into one generic message fails here.
func TestEachCommitValidityClauseHasItsOwnRule(t *testing.T) {
	for _, c := range []struct {
		name string
		rule string
		skip string
		mut  func(t *testing.T, h *dsHarness, g id.ID, s *auth.Session, req *ds.CommitRequest)
	}{
		{
			name: "a message that is not a commit at all",
			rule: "structural",
			mut: func(_ *testing.T, _ *dsHarness, _ id.ID, _ *auth.Session, req *ds.CommitRequest) {
				req.Commit = []byte{0x01, 0x02, 0x03}
			},
		},
		{
			name: "a commit that omits an outstanding DS proposal",
			rule: "outstanding_proposals",
			mut: func(t *testing.T, h *dsHarness, g id.ID, _ *auth.Session, _ *ds.CommitRequest) {
				h.putDSProposal(t, g, 6, false)
			},
		},
		{
			name: "an Add whose device this instance has never seen",
			rule: "add_key_package",
			mut: func(_ *testing.T, _ *dsHarness, _ id.ID, _ *auth.Session, req *ds.CommitRequest) {
				req.Commit = fixtureFile(nil, "commits/00.mls")
			},
		},
		{
			name: "a member Remove targeting another user",
			rule: "member_remove_scope",
			mut: func(t *testing.T, h *dsHarness, g id.ID, s *auth.Session, req *ds.CommitRequest) {
				// commits/08 is leaf 0's commit removing leaf 1 by value, uploaded by leaf 0's own
				// device. A by-value Remove is the committer's, and the committer is the leaf the
				// PublicGroup authenticated. The fixture gives leaves 0 and 1 one user (a device
				// revocation, which is allowed), so the instance's record is told leaf 1 is another
				// user's: the clause reads the owners from mls_members.
				req.Commit = fixtureFile(t, "commits/08.mls")
				members, err := h.repo.ListMembers(context.Background(), g)
				if err != nil {
					t.Fatalf("ListMembers: %v", err)
				}
				for i := range members {
					if members[i].LeafIndex == 1 {
						members[i].UserID = id.New()
					}
				}
				if err := h.repo.ReplaceMembers(context.Background(), g, 6, members); err != nil {
					t.Fatalf("ReplaceMembers: %v", err)
				}
			},
		},
		{
			name: "a GroupInfo at epoch n rather than n+1",
			rule: "group_info_epoch",
			mut:  func(*testing.T, *dsHarness, id.ID, *auth.Session, *ds.CommitRequest) {},
		},
		{
			name: "a commit carrying the committer's own Update",
			rule: "committer_update",
			skip: "no committed fixture carries an Update proposal from the committer's own leaf: " +
				"commits/09.mls is a self-update, whose applied list is empty by construction",
			mut: func(*testing.T, *dsHarness, id.ID, *auth.Session, *ds.CommitRequest) {},
		},
		{
			name: "a GroupInfo signed by a leaf other than the committer",
			rule: "group_info_signature",
			skip: "the fixture holds one GroupInfo, signed by leaf 0 at epoch 6; a second signer " +
				"needs a generator change",
			mut: func(*testing.T, *dsHarness, id.ID, *auth.Session, *ds.CommitRequest) {},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.skip != "" {
				t.Skip(c.skip)
			}
			h := newDSHarness(t)
			reg, session := h.mustRegister(t)
			// The base request is a well-formed self-update at the group's own epoch with the
			// fixture's GroupInfo, so each case's mutation is the only thing that can refuse it.
			req := ds.CommitRequest{
				Epoch:     6,
				Commit:    fixtureFile(t, "commits/09.mls"),
				GroupInfo: dsFixture(t).groupInfo,
			}
			c.mut(t, h, reg.GroupID, &session, &req)

			_, err := h.ds.Commit(context.Background(), session, reg.GroupID, req)
			var dsErr *ds.Error
			if !errors.As(err, &dsErr) {
				t.Fatalf("got %v, want a *ds.Error", err)
			}
			if dsErr.Code != "E_COMMIT_INVALID" {
				t.Fatalf("code = %s, want E_COMMIT_INVALID (%v)", dsErr.Code, err)
			}
			if dsErr.Rule != c.rule {
				t.Fatalf("rule = %q, want %q (%v)", dsErr.Rule, c.rule, err)
			}
			if dsErr.Status != 422 {
				t.Errorf("status = %d, want 422", dsErr.Status)
			}
		})
	}
}

// Every refusal after public_group_process must release the staged commit. Merge is the only other
// consumer of a staged handle, so without the discard a client retrying a malformed commit in a
// loop grows the guest's handle table until the module runs out of linear memory.
func TestARefusedCommitReleasesItsStagedHandle(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegister(t)
	before := h.wasmCalls("public_group_staged_discard")

	for i := 0; i < 3; i++ {
		_, err := h.ds.Commit(context.Background(), session, reg.GroupID, ds.CommitRequest{
			Epoch:     6,
			Commit:    fixtureFile(t, "commits/09.mls"),
			GroupInfo: dsFixture(t).groupInfo,
		})
		if err == nil {
			t.Fatal("a GroupInfo at epoch n must not be accepted")
		}
	}
	if got := h.wasmCalls("public_group_staged_discard") - before; got != 3 {
		t.Fatalf("public_group_staged_discard called %d times for 3 refused commits, want 3", got)
	}
}

// Invariant 2: the delivery service serves the ratchet tree from its own PublicGroup, so a
// committer never uploads one. Accepting and ignoring the field would leave the one route a client
// could smuggle a tree through unguarded and untested.
func TestACommitMayNotUploadARatchetTree(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegister(t)

	_, err := h.ds.Commit(context.Background(), session, reg.GroupID, ds.CommitRequest{
		Epoch:       6,
		Commit:      fixtureFile(t, "commits/09.mls"),
		GroupInfo:   dsFixture(t).groupInfo,
		RatchetTree: dsFixture(t).ratchetTree,
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_INVALID_REQUEST" {
		t.Fatalf("got %v, want E_INVALID_REQUEST", err)
	}
}

// A commit needs an enrolled session whose device is a CURRENT leaf of the group (invariant 8's
// "current leaf" check). A provisional session and a stranger's device are refused before the
// guest is asked to parse anything.
func TestOnlyAnEnrolledMemberDeviceMayCommit(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)
	req := ds.CommitRequest{
		Epoch:     6,
		Commit:    fixtureFile(t, "commits/09.mls"),
		GroupInfo: dsFixture(t).groupInfo,
	}

	provisional := session
	provisional.Scope = auth.ScopeProvisional
	_, err := h.ds.Commit(ctx, provisional, reg.GroupID, req)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_FORBIDDEN" {
		t.Fatalf("provisional session: got %v, want E_FORBIDDEN", err)
	}

	stranger := auth.Session{UserID: id.New(), DeviceID: id.New(), Scope: auth.ScopeEnrolled}
	_, err = h.ds.Commit(ctx, stranger, reg.GroupID, req)
	if !errors.As(err, &dsErr) || dsErr.Code != "E_LEAF_NOT_CURRENT" {
		t.Fatalf("a device that is not a leaf: got %v, want E_LEAF_NOT_CURRENT", err)
	}

	_, err = h.ds.Commit(ctx, session, id.New(), req)
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
		t.Fatalf("an unknown group: got %v, want E_NOT_FOUND", err)
	}
}

// ------------------------------------------------------------ member proposals

// A member proposal is only an Update or an own-device Remove, and it must be signed by the
// sending device's own leaf. The fixture's one proposal is the instance's external Remove of leaf
// 0, which is exactly what this endpoint must NOT take: an external sender has no leaf, so the
// leaf check refuses it before ProposalInspect is reached.
func TestAMemberProposalMustBeSignedByTheSendingDevicesOwnLeaf(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)

	_, err := h.ds.Proposal(ctx, session, reg.GroupID, 6, fixtureFile(t, "remove_leaf0.mls"))
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_FORBIDDEN" {
		t.Fatalf("an external-sender proposal on the member endpoint: got %v, want E_FORBIDDEN", err)
	}

	// A commit is not a proposal, and the endpoint says so structurally rather than queueing it.
	_, err = h.ds.Proposal(ctx, session, reg.GroupID, 6, fixtureFile(t, "commits/09.mls"))
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" || dsErr.Rule != "structural" {
		t.Fatalf("a commit on the proposal endpoint: got %v, want E_COMMIT_INVALID/structural", err)
	}

	// A proposal for another epoch is refused before the guest is asked to parse it.
	_, err = h.ds.Proposal(ctx, session, reg.GroupID, 5, fixtureFile(t, "remove_leaf0.mls"))
	if !errors.As(err, &dsErr) || dsErr.Rule != "epoch" {
		t.Fatalf("a proposal for another epoch: got %v, want rule \"epoch\"", err)
	}
}

// The two accepted member shapes — an Update, and a Remove of one of the sender's own devices —
// need a member-authored proposal, which no committed fixture holds.
func TestAMemberProposalIsOnlyAnUpdateOrAnOwnDeviceRemove(t *testing.T) {
	t.Skip("no committed fixture holds a member-authored Update or Remove: testkit/fixtures/ds-1500 " +
		"ships one external-sender Remove and ten commits, and generating a member proposal needs " +
		"the creator's signing key, which the fixture deliberately does not publish")
}

// ------------------------------------------------------------- the seq space

// The handshake log names every leaf that ever committed and every epoch transition of the group,
// so it is member-only and a non-member is E_NOT_FOUND, never E_FORBIDDEN: a 403 would tell any
// authenticated device on the instance which group ids are live.
func TestTheHandshakeStreamIsMemberOnlyAndSparseOverTheOneSeqSpace(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)

	// Two handshakes at seq 1 and seq 6: the space between them belongs to application messages,
	// which is what makes one cursor enough for both streams.
	h.appendHandshake(t, reg.GroupID, 1, 6, 1, []byte("commit"))
	h.appendHandshake(t, reg.GroupID, 6, 7, 0, []byte("proposal"))

	rows, err := h.ds.Handshakes(ctx, reg.GroupID, session, 0, 100)
	if err != nil {
		t.Fatalf("Handshakes: %v", err)
	}
	if len(rows) != 2 || rows[0].Seq != 1 || rows[1].Seq != 6 {
		t.Fatalf("handshakes = %v, want the two rows at seq 1 and 6", rows)
	}

	stranger := auth.Session{UserID: id.New(), DeviceID: id.New(), Scope: auth.ScopeEnrolled}
	_, err = h.ds.Handshakes(ctx, reg.GroupID, stranger, 0, 100)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
		t.Fatalf("a non-member: got %v, want E_NOT_FOUND", err)
	}
}

// A `from` below the retention floor is E_PRUNED, which tells the client to resync rather than to
// retry. This is the TRUE POSITIVE, and it is built by really deleting a handshake: the sweep's one
// deletion rule (`DELETE FROM mls_handshakes WHERE created < now - HandshakeRetention`, task 26's
// `PruneHandshakes`) runs here over a log that straddles the window, so the hole below the floor is
// a hole the instance genuinely cannot fill. The two tests below pin the other halves of the rule.
func TestACatchUpBelowTheRetentionFloorIsPruned(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)

	// Seq 1 is written now and seq 20 thirty-one days later, so exactly one of the two is older
	// than HandshakeRetention when the sweep runs.
	h.appendHandshake(t, reg.GroupID, 1, 6, 1, []byte("swept"))
	h.clk.Advance(31 * 24 * time.Hour)
	h.appendHandshake(t, reg.GroupID, 20, 7, 1, []byte("commit"))

	cutoff := h.clk.Now().Add(-ds.DefaultPolicy().HandshakeRetention).Unix()
	gone, err := h.repo.PruneHandshakes(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneHandshakes: %v", err)
	}
	if gone != 1 {
		t.Fatalf("the sweep deleted %d rows, want the one at seq 1: the hole below the floor has to be real", gone)
	}

	// `from` is the first seq wanted. Seqs 2-19 were never handshakes the sweep took, so every
	// catch-up from 2 up is whole; seq 1 is the one the sweep deleted, so a catch-up that still
	// wants it — from 1, or from 0 — has a hole and is refused.
	for _, from := range []uint64{19, 5, 2} {
		if _, err := h.ds.Handshakes(ctx, reg.GroupID, session, from, 100); err != nil {
			t.Fatalf("from %d lost nothing to the sweep: %v", from, err)
		}
	}
	for _, from := range []uint64{1, 0} {
		_, err = h.ds.Handshakes(ctx, reg.GroupID, session, from, 100)
		var dsErr *ds.Error
		if !errors.As(err, &dsErr) || dsErr.Code != "E_PRUNED" {
			t.Fatalf("from %d: got %v, want E_PRUNED", from, err)
		}
		if dsErr.Status != 410 {
			t.Errorf("status = %d, want 410", dsErr.Status)
		}
	}
}

// An old group with nothing swept is SERVED. Until the final review the predicate compared the
// group's age with the retention window and refused this catch-up although nothing had been
// deleted (the over-refusal ruling 41 / deviation B20 accepted for the wave). The store now records
// the highest handshake seq retention deleted, in the transaction that deletes, so the refusal is
// exact: a group older than the window whose early seqs are messages has lost nothing, and a
// member catching up from 0 is served rather than sent through a full rejoin.
func TestAnOldGroupWithNothingSweptIsServed(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)

	// Older than the retention window, and its first handshake is recent: seqs 1-19 are the
	// message stream's, so the log has no hole at all.
	h.clk.Advance(31 * 24 * time.Hour)
	h.appendHandshake(t, reg.GroupID, 20, 6, 1, []byte("commit"))

	// Nothing has ever been deleted from this group: a sweep run right now keeps every row.
	cutoff := h.clk.Now().Add(-ds.DefaultPolicy().HandshakeRetention).Unix()
	gone, err := h.repo.PruneHandshakes(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneHandshakes: %v", err)
	}
	if gone != 0 {
		t.Fatalf("the sweep deleted %d rows: this fixture must have lost nothing", gone)
	}

	rows, err := h.ds.Handshakes(ctx, reg.GroupID, session, 0, 100)
	if err != nil {
		t.Fatalf("an old group that lost nothing must be served: %v", err)
	}
	if len(rows) != 1 || rows[0].Seq != 20 {
		t.Fatalf("handshakes = %v, want the one row at seq 20", rows)
	}
}

// The floor is about PRUNING, not about the shape of the seq space. Handshakes and application
// messages share ONE seq space, so a group whose early seqs carry messages has its first handshake
// well above 1 with nothing ever deleted — and a healthy member catching up from 0 must then be
// SERVED, not sent through the full external-commit rejoin protocol/02's error table makes
// E_PRUNED mean. Nothing can have been pruned from a group younger than HandshakeRetention: the
// sweep deletes handshakes by `created`, and every row of a group is younger than the group.
func TestAFirstHandshakeAboveSeqOneIsNotAPrunedLog(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)

	// Seqs 1-19 belong to the message stream (task 23 uploads them); seq 20 is this group's FIRST
	// handshake, and no sweep has ever run.
	h.appendHandshake(t, reg.GroupID, 20, 6, 1, []byte("commit"))

	rows, err := h.ds.Handshakes(ctx, reg.GroupID, session, 0, 100)
	if err != nil {
		t.Fatalf("catching up from 0 on a group that has pruned nothing: %v", err)
	}
	if len(rows) != 1 || rows[0].Seq != 20 {
		t.Fatalf("handshakes = %v, want the one row at seq 20", rows)
	}
}

// Outstanding is every NON-VOID proposal of the group's CURRENT epoch: it is what the two conflict
// bodies carry, and a void or stale ref in there would send a client to commit something the
// instance has already withdrawn.
func TestOutstandingIsEveryNonVoidProposalOfTheCurrentEpoch(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)

	live := h.putDSProposal(t, reg.GroupID, 6, false)
	h.putDSProposal(t, reg.GroupID, 6, true) // voided
	h.putDSProposal(t, reg.GroupID, 5, false)

	rows, err := h.ds.Outstanding(ctx, reg.GroupID)
	if err != nil {
		t.Fatalf("Outstanding: %v", err)
	}
	if len(rows) != 1 || !bytes.Equal(rows[0].Ref, live) {
		t.Fatalf("outstanding = %d rows, want the one live proposal of epoch 6", len(rows))
	}
}

// ------------------------------------------------- the external path's refusals

// An external commit is recognised by its SENDER, not by its kind. `public_group_process` answers
// KindCommit (1) with a staged handle for every StagedCommitMessage, and an external commit is
// exactly that — `Sender::NewMemberCommit` maps to no sender leaf. KindExternalJoin (2) is the
// external-join PROPOSAL arm, which carries no staged commit at all, so expecting it on the
// external path would refuse every real external commit the moment task 24 or 25 turns the flag
// on, and would accept a proposal in its place.
func TestTheExternalPathTellsAnExternalCommitApartByItsSenderNotItsKind(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegister(t)

	// A MEMBER commit on the external path: it is KindCommit, it stages, and it names a leaf —
	// which is the one thing an external commit cannot do.
	_, err := ds.CommitExternalForTest(h.ds, context.Background(), session, reg.GroupID, ds.CommitRequest{
		Epoch:     6,
		Commit:    fixtureFile(t, "commits/09.mls"),
		GroupInfo: dsFixture(t).groupInfo,
	})
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" || dsErr.Rule != "structural" {
		t.Fatalf("got %v, want E_COMMIT_INVALID/structural", err)
	}
	if !strings.Contains(dsErr.Detail, "leaf") {
		t.Fatalf("detail = %q, want the refusal to be about the SENDER; a kind check here would "+
			"refuse every genuine external commit, which arrives as KindCommit with no leaf",
			dsErr.Detail)
	}
}

// Every refusal after `public_group_process` must release the staged commit, on BOTH paths. The
// external path is where it bites: a staged handle is inserted for every StagedCommitMessage, and
// an enrolled device can post external commits in a loop until the guest's handle table exhausts
// the module's linear memory.
func TestAnExternalCommitRefusedForItsShapeReleasesItsStagedHandle(t *testing.T) {
	h := newDSHarness(t)
	reg, session := h.mustRegister(t)
	before := h.wasmCalls("public_group_staged_discard")

	for i := 0; i < 3; i++ {
		_, err := ds.CommitExternalForTest(h.ds, context.Background(), session, reg.GroupID, ds.CommitRequest{
			Epoch:     6,
			Commit:    fixtureFile(t, "commits/09.mls"),
			GroupInfo: dsFixture(t).groupInfo,
		})
		if err == nil {
			t.Fatal("a member commit must not be accepted on the external path")
		}
	}
	if got := h.wasmCalls("public_group_staged_discard") - before; got != 3 {
		t.Fatalf("public_group_staged_discard called %d times for 3 refused external commits, "+
			"want 3: the release must cover every return after Process, not only the ones after "+
			"the structural checks", got)
	}
}

// ------------------------------------------------------ member proposals, again

// `ProposalInspect` needs the proposal to BE in the guest's queue, so the put necessarily happens
// before the shape is judged. Every refusal after it must therefore take the proposal back out:
// otherwise the cached PublicGroup carries a proposal with no SQL row — R12's "SQL is the record"
// divergence — and a member grows the queue by one entry per refused request for as long as the
// group stays cached.
func TestARefusedMemberProposalIsTakenBackOutOfTheGuestsQueue(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	// The fixture's one proposal removes leaf 0. The sender here belongs to another user, so the
	// shape check refuses it — after the put.
	session := h.memberSessionOfAnotherUser(t, reg.GroupID, 0)
	blob := fixtureFile(t, "remove_leaf0.mls")

	if err := ds.WithGroupForTest(h.ds, ctx, reg.GroupID, func(g *mlswasi.PublicGroup) error {
		for i := 0; i < 3; i++ {
			_, _, err := ds.QueueMemberProposalForTest(h.ds, ctx, g, session, reg.GroupID, blob)
			var dsErr *ds.Error
			if !errors.As(err, &dsErr) || dsErr.Code != "E_FORBIDDEN" {
				t.Fatalf("got %v, want E_FORBIDDEN for a Remove of another user's leaf", err)
			}
		}
		queued, err := g.ProposalList(ctx)
		if err != nil {
			t.Fatalf("ProposalList: %v", err)
		}
		if len(queued) != 0 {
			t.Fatalf("the guest holds %d queued proposals after 3 refusals, want 0", len(queued))
		}
		return nil
	}); err != nil {
		t.Fatalf("withGroup: %v", err)
	}
}

// ---------------------------------------------------------------- endpoint 19

// Endpoint 19's row is `[ref, kind, target_leaf|null, blob, void]`, and `mls_pending_proposals`
// has no blob column: the bytes are the ones the guest queued, so the answer is a join of the SQL
// row onto `PublicGroup::queued_proposals`. A row with no queued blob is still a row — the
// committer needs the ref to know what the instance is waiting for.
func TestProposalsJoinsTheSqlRowToTheGuestsQueuedBlob(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)
	blob := fixtureFile(t, "remove_leaf0.mls")

	// Queue the fixture's proposal in the guest and put its ref in SQL, which is what the DS
	// proposal path does inside one transaction.
	var ref []byte
	if err := ds.WithGroupForTest(h.ds, ctx, reg.GroupID, func(g *mlswasi.PublicGroup) error {
		var err error
		ref, err = g.ProposalPut(ctx, 0, blob)
		return err
	}); err != nil {
		t.Fatalf("queue the fixture proposal: %v", err)
	}
	target := uint32(0)
	if err := h.repo.PutProposal(ctx, store.ProposalRow{
		GroupID: reg.GroupID, Ref: ref, Epoch: 6, Kind: 3, TargetLeaf: &target,
		Origin: 0, ActionID: id.New(), IssuedAt: h.clk.Now().Unix(), TTL: 86400,
	}); err != nil {
		t.Fatalf("PutProposal: %v", err)
	}
	voided := h.putDSProposal(t, reg.GroupID, 6, true)
	h.putDSProposal(t, reg.GroupID, 5, false) // another epoch: not this answer's business

	rows, err := h.ds.Proposals(ctx, reg.GroupID, session)
	if err != nil {
		t.Fatalf("Proposals: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("Proposals returned %d rows, want the two of epoch 6 (the void one included)", len(rows))
	}
	byRef := map[string]ds.Proposal{}
	for _, p := range rows {
		byRef[string(p.Row.Ref)] = p
	}
	live, ok := byRef[string(ref)]
	if !ok {
		t.Fatal("the queued proposal is missing from the answer")
	}
	if !bytes.Equal(live.Blob, blob) {
		t.Errorf("blob is %d bytes, want the %d the guest queued", len(live.Blob), len(blob))
	}
	if live.Row.TargetLeaf == nil || *live.Row.TargetLeaf != 0 {
		t.Errorf("target_leaf = %v, want 0", live.Row.TargetLeaf)
	}
	if live.Row.VoidAt != nil {
		t.Error("the live proposal is flagged void")
	}
	dead, ok := byRef[string(voided)]
	if !ok {
		t.Fatal("the void proposal is missing: endpoint 19 lists void rows and flags them, so a " +
			"client can tell a withdrawn proposal from one it has not seen")
	}
	if dead.Row.VoidAt == nil {
		t.Error("the void proposal is not flagged void")
	}
	if len(dead.Blob) != 0 {
		t.Errorf("a row the guest never queued carried %d bytes of blob", len(dead.Blob))
	}

	// Member-only, and a non-member is E_NOT_FOUND rather than E_FORBIDDEN, for the same reason
	// Info, Tree and Handshakes are: the list names leaves and the devices behind them.
	stranger := auth.Session{UserID: id.New(), DeviceID: id.New(), Scope: auth.ScopeEnrolled}
	_, err = h.ds.Proposals(ctx, reg.GroupID, stranger)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
		t.Fatalf("a non-member: got %v, want E_NOT_FOUND", err)
	}
}

// ------------------------------------------------------ the gaps, named in code

// The brief's step 1 named two more tests here, TestOneSeqSpaceNumbersHandshakesAndMessages and
// TestAnAcceptedCommitFansOutHandshakeEpochChangedAndWelcomes. Both need an ACCEPTED commit, and
// the committed fixture cannot produce one (one GroupInfo, at epoch 6; invariant 4 wants n+1), so
// they shipped here as skips. Task 29 (Ruling C(5)) replaced them, under the same names, with
// harness-driven tests in internal/testkit/accepted_test.go: real dilla-core clients commit
// through the in-process instance and the tests read back what it stored.

// ---------------------------------------------------------------- the helpers

// fixtureFile reads one file of the committed 1,500-leaf fixture. tb may be nil, which the table
// above uses from a closure that has no *testing.T of its own.
func fixtureFile(tb testing.TB, rel string) []byte {
	if tb != nil {
		tb.Helper()
	}
	raw, err := os.ReadFile(filepath.Join(dsFixtureDir, rel))
	if err != nil {
		if tb == nil {
			panic(err)
		}
		tb.Fatalf("read %s: %v", rel, err)
	}
	return raw
}

// appendHandshake writes one row of the group's handshake log directly. Registration produces no
// handshake — the group is created from an uploaded tree, not from a commit — and no committed
// fixture can carry a commit all the way to a merge, so the log the sequencer serves and the
// winner the conflict names are seeded here.
func (h *dsHarness) appendHandshake(t *testing.T, groupID id.ID, seq, epoch uint64, kind uint8, blob []byte) {
	t.Helper()
	if err := h.repo.AppendHandshake(context.Background(), store.HandshakeRow{
		GroupID: groupID, Seq: seq, Epoch: epoch, Kind: kind, Blob: blob, Created: h.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("AppendHandshake: %v", err)
	}
}

// putDSProposal queues one instance-originated (origin 0) Remove proposal and returns its ref.
// Task 21 owns the endpoint that issues these; invariant 4's first clause and the two conflict
// bodies are defined over the rows, so the rows are what these tests need.
func (h *dsHarness) putDSProposal(t *testing.T, groupID id.ID, epoch uint64, void bool) []byte {
	t.Helper()
	ref := id.New()
	row := store.ProposalRow{
		GroupID:  groupID,
		Ref:      ref[:],
		Epoch:    epoch,
		Kind:     3, // Remove
		Origin:   0, // the instance
		ActionID: id.New(),
		IssuedAt: h.clk.Now().Unix(),
		TTL:      86400,
	}
	if void {
		at := h.clk.Now().Unix()
		row.VoidAt = &at
	}
	if err := h.repo.PutProposal(context.Background(), row); err != nil {
		t.Fatalf("PutProposal: %v", err)
	}
	return row.Ref
}

// memberSessionOfAnotherUser is a session for some current member whose user is NOT the user at
// `leaf`. The fixture gives 1,500 leaves across a handful of synthetic users, so the search is
// over real credentials rather than over invented identities.
func (h *dsHarness) memberSessionOfAnotherUser(t *testing.T, groupID id.ID, leaf uint32) auth.Session {
	t.Helper()
	members, err := h.repo.ListMembers(context.Background(), groupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	var target id.ID
	found := false
	for _, m := range members {
		if m.LeafIndex == leaf {
			target, found = m.UserID, true
			break
		}
	}
	if !found {
		t.Fatalf("no member at leaf %d", leaf)
	}
	for _, m := range members {
		if m.UserID != target && m.RemovedEpoch == nil {
			return auth.Session{UserID: m.UserID, DeviceID: m.DeviceID, Scope: auth.ScopeEnrolled}
		}
	}
	t.Fatalf("every member of %s belongs to the user at leaf %d", groupID, leaf)
	return auth.Session{}
}

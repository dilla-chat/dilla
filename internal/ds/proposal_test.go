package ds_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"
	"golang.org/x/crypto/chacha20"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/ds"
	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// Task 21 is invariants 5 and 6 of protocol/02: the freeze an outstanding instance proposal puts
// on a group, and the TTL that voids one.
//
// What the committed fixture can and cannot drive, so the holes are visible in the test output
// rather than only in prose:
//
//   - An instance proposal CAN be issued end to end. The fixture's `external_senders` extension
//     carries the testkit's deterministic instance key (testkit/src/fixtures.rs:106-126), so
//     `fixtureExternalSenderKey` below re-derives it and `ProposeAdd`/`ProposeRemove` produce
//     proposals the guest's `queue_proposal` actually accepts — signature, sender resolution and
//     all.
//   - No commit can be ACCEPTED (commit_test.go's two named skips: the fixture ships one
//     GroupInfo, at epoch 6, and invariant 4 wants epoch n+1). The group therefore never advances
//     an epoch in this package, so every assertion that needs a NEW epoch — a re-issue whose ref
//     differs, and invariant 5's nobody-online external commit that re-issues what it omitted —
//     is skipped by name below rather than faked.

// ------------------------------------------------------------------ step 1a

// Every Policy field has a default. A zero time.Duration read as a deadline is
// "already expired", and a zero int read as a cap is "nothing is allowed", so a
// field DefaultPolicy() forgets does not fail loudly — it fails as a freeze that
// never warns, or a group that closes the instant it freezes. reflect walks the
// struct so that adding a field without a default breaks this test rather than
// production.
func TestDefaultPolicyLeavesNoFieldUnset(t *testing.T) {
	p := ds.DefaultPolicy()
	v := reflect.ValueOf(p)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Errorf("Policy.%s has no default", v.Type().Field(i).Name)
		}
	}
}

// The two freeze backstops, by value, against gap-21-ds.md §5.3 and §11. They are
// asserted rather than merely defaulted because both numbers are load-bearing
// citations: freeze_warn equals the text proposal TTL and freeze_max equals
// handshake retention, and a later edit that drifts either silently decouples the
// warning from the TTL or the close from retention.
func TestTheFreezeBackstopsAreTwentyFourHoursAndThirtyDays(t *testing.T) {
	p := ds.DefaultPolicy()
	if p.FreezeWarn != 24*time.Hour {
		t.Errorf("FreezeWarn = %v, want 24h (gap-21-ds §5.3)", p.FreezeWarn)
	}
	if p.FreezeMax != 30*24*time.Hour {
		t.Errorf("FreezeMax = %v, want 30d (gap-21-ds §5.3, R26)", p.FreezeMax)
	}
	if p.FreezeWarn != p.ProposalTTLText {
		t.Errorf("FreezeWarn %v != ProposalTTLText %v: gap-21-ds §11 defines the warning AS one text TTL",
			p.FreezeWarn, p.ProposalTTLText)
	}
	if p.FreezeMax != p.HandshakeRetention {
		t.Errorf("FreezeMax %v != HandshakeRetention %v: gap-21-ds §5.3 defines the close AS the retention horizon",
			p.FreezeMax, p.HandshakeRetention)
	}
	// Invariant 6's two TTLs, by value. `TestDefaultPolicyLeavesNoFieldUnset` only asks that they
	// are non-zero, and no test in the tree pins the call group's 30 s: the sweep tests write a
	// TTL into the proposal row themselves, so a drift of ProposalTTLCall to any non-zero value
	// would pass everything else.
	if p.ProposalTTLText != 24*time.Hour {
		t.Errorf("ProposalTTLText = %v, want 24h (protocol/02 invariant 6)", p.ProposalTTLText)
	}
	if p.ProposalTTLCall != 30*time.Second {
		t.Errorf("ProposalTTLCall = %v, want 30s (protocol/02 invariant 6)", p.ProposalTTLCall)
	}
}

// …and the map from a group's kind to its TTL, which is the half the sweep tests cannot reach: no
// committed fixture registers a CALL group, so `d.proposalTTL(row.Kind)` is only ever called with
// kind 0 through a public entry point. A swapped pair — 30 s on text, 24 h on calls — would void
// every text proposal half a minute after it was issued, and nothing else here would notice.
func TestTheProposalTTLIsTwentyFourHoursForTextAndThirtySecondsForCalls(t *testing.T) {
	h := newDSHarness(t)
	if got := ds.ProposalTTLForTest(h.ds, 0); got != 24*time.Hour {
		t.Errorf("proposalTTL(0 text) = %v, want 24h", got)
	}
	if got := ds.ProposalTTLForTest(h.ds, 1); got != 30*time.Second {
		t.Errorf("proposalTTL(1 call) = %v, want 30s", got)
	}
}

// ------------------------------------------------------------- invariant 5

// Invariant 5 as R10 amends it: the freeze holds while a non-void instance proposal is outstanding
// AND some member device is online. With nobody online the freeze lifts, so an external commit is
// accepted — otherwise a group whose members are all away stays frozen until invariant 11's
// close-and-recreate, which is the only other exit.
func TestTheFreezeHoldsWhileSomebodyIsOnlineAndLiftsWhenNobodyIs(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)
	ref := h.putDSProposal(t, reg.GroupID, 6, false)

	// Nobody has a gateway connection yet.
	frozen, refs, err := ds.FreezeStateForTest(h.ds, ctx, reg.GroupID, 6)
	if err != nil {
		t.Fatalf("freezeState: %v", err)
	}
	if frozen {
		t.Error("with nobody online the freeze must lift: R10's amendment to invariant 5")
	}
	if len(refs) != 1 || !bytes.Equal(refs[0], ref) {
		t.Errorf("refs = %x, want the one outstanding ref %x even while the freeze is lifted", refs, ref)
	}

	// The member at leaf 0 opens a real gateway connection and reaches ready.
	h.onlineSession(t, session)
	frozen, refs, err = ds.FreezeStateForTest(h.ds, ctx, reg.GroupID, 6)
	if err != nil {
		t.Fatalf("freezeState: %v", err)
	}
	if !frozen {
		t.Fatal("a non-void instance proposal with a member online freezes the group")
	}
	if len(refs) != 1 {
		t.Errorf("the freeze must name the refs a committer has to reference, got %d", len(refs))
	}

	// Frozen is the predicate the API reads, and it takes the group's own epoch.
	got, err := h.ds.Frozen(ctx, reg.GroupID)
	if err != nil {
		t.Fatalf("Frozen: %v", err)
	}
	if !got {
		t.Error("Frozen disagrees with freezeState at the group's current epoch")
	}
}

// The message path does NOT consult the online predicate. protocol/02 invariant 5 exempts the
// EXTERNAL COMMIT when nobody is online, not the application message: a device holding an HTTP
// session but no gateway connection would otherwise upload ciphertext straight through a live
// freeze.
func TestAMessageDuringAFreezeIsCommitRequiredWhoeverIsOnline(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	ref := h.putDSProposal(t, reg.GroupID, 6, false)

	err := ds.RequireNoFreezeForTest(h.ds, ctx, reg.GroupID, 6)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_REQUIRED" {
		t.Fatalf("a message during a freeze: got %v, want E_COMMIT_REQUIRED", err)
	}
	if dsErr.Status != 425 {
		t.Errorf("status = %d, want 425", dsErr.Status)
	}
	if len(dsErr.Proposals) != 1 || !bytes.Equal(dsErr.Proposals[0], ref) {
		t.Errorf("the refusal must carry the outstanding proposals, got %x", dsErr.Proposals)
	}
	if dsErr.RetryAfterMS == nil || *dsErr.RetryAfterMS != uint64(ds.DefaultPolicy().CommitDeadline.Milliseconds()) {
		t.Errorf("retry_after_ms = %v, want the commit deadline in milliseconds", dsErr.RetryAfterMS)
	}

	// A void proposal holds nothing back.
	if err := h.ds.Void(ctx, reg.GroupID, ref); err != nil {
		t.Fatalf("Void: %v", err)
	}
	if err := ds.RequireNoFreezeForTest(h.ds, ctx, reg.GroupID, 6); err != nil {
		t.Fatalf("a void proposal must not refuse a message: %v", err)
	}
}

// ------------------------------------------------------------- invariant 6

// Invariant 6: a proposal past its TTL is void, and the freeze it held lifts — with nobody online
// too (R10). The TTLs are 24 h in text groups and 30 s in call groups, driven by clock.Fake and
// never by a real timer: the sweep evaluates them lazily, at the decision point.
func TestAProposalPastItsTTLIsVoidAndTheFreezeLifts(t *testing.T) {
	for _, c := range []struct {
		name string
		ttl  time.Duration
	}{
		{"text group", 24 * time.Hour},
		{"call group", 30 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newDSHarness(t)
			ctx := context.Background()
			reg, session := h.mustRegister(t)
			h.onlineSession(t, session)
			ref := h.putDSProposalWithTTL(t, reg.GroupID, 6, uint64(c.ttl.Seconds()))

			if frozen, _ := h.ds.Frozen(ctx, reg.GroupID); !frozen {
				t.Fatal("a fresh proposal freezes the group")
			}

			// One second short of the TTL nothing is void, and the message path still refuses.
			// `Frozen` is not the assertion here: advancing the clock past session_idle_close
			// (90 s) takes the connection offline, and R10's freeze is lifted by that on purpose.
			// The message path never consults the online predicate, so it is the one that still
			// answers for a proposal inside its TTL.
			h.clk.Advance(c.ttl - time.Second)
			if n, err := ds.SweepProposalsForTest(h.ds, ctx); err != nil || n != 0 {
				t.Fatalf("sweep inside the TTL voided %d proposals (err %v), want 0", n, err)
			}
			if err := ds.RequireNoFreezeForTest(h.ds, ctx, reg.GroupID, 6); err == nil {
				t.Fatal("a proposal inside its TTL still holds the message path")
			}

			h.clk.Advance(2 * time.Second)
			n, err := ds.SweepProposalsForTest(h.ds, ctx)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if n != 1 {
				t.Fatalf("the sweep voided %d proposals, want 1", n)
			}
			if frozen, _ := h.ds.Frozen(ctx, reg.GroupID); frozen {
				t.Fatal("a proposal past its TTL is void and must not hold the freeze")
			}
			rows, err := h.repo.ListProposals(ctx, reg.GroupID, 6, true)
			if err != nil {
				t.Fatalf("ListProposals: %v", err)
			}
			if len(rows) != 1 || rows[0].VoidAt == nil || !bytes.Equal(rows[0].Ref, ref) {
				t.Fatalf("the swept row is not flagged void: %+v", rows)
			}
			// And a commit may now omit it: invariant 4's clause 1 skips void rows.
			if err := ds.RequireNoFreezeForTest(h.ds, ctx, reg.GroupID, 6); err != nil {
				t.Errorf("a void proposal still refuses the message path: %v", err)
			}
		})
	}
}

// R10 / invariant 6: when every proposal has voided the freeze lifts even with nobody online, so
// the group is not frozen forever because its members happen to be away.
func TestTheFreezeLiftsWhenEveryProposalHasVoidedWithNobodyOnline(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.putDSProposalWithTTL(t, reg.GroupID, 6, uint64((24 * time.Hour).Seconds()))

	h.clk.Advance(25 * time.Hour)
	if _, err := ds.SweepProposalsForTest(h.ds, ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if frozen, _ := h.ds.Frozen(ctx, reg.GroupID); frozen {
		t.Fatal("with every proposal void the freeze is lifted, online or not")
	}
	if err := ds.RequireNoFreezeForTest(h.ds, ctx, reg.GroupID, 6); err != nil {
		t.Errorf("the message path still refuses after every proposal voided: %v", err)
	}
}

// The sweep PAGES to completion. A fixed first page from the zero id would leave every group past
// the page unswept, and nothing else voids a proposal: the group's freeze would then be permanent.
func TestTheSweepPagesPastItsFirstPageOfGroups(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	h.putDSProposalWithTTL(t, reg.GroupID, 6, 30)

	// 600 more open groups, past the 512-group page, each holding one expired proposal.
	for i := 0; i < 600; i++ {
		gid := h.bareGroup(t)
		h.putDSProposalWithTTL(t, gid, 0, 30)
	}
	h.clk.Advance(time.Minute)
	n, err := ds.SweepProposalsForTest(h.ds, ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 601 {
		t.Fatalf("the sweep voided %d proposals, want 601: it stopped at its first page", n)
	}
	_ = reg
}

// ------------------------------------------------- issuing an instance proposal

// ProposeRemove signs an external Remove as the instance, queues it in the guest, writes the SQL
// row, appends the handshake and fans the proposal out — and the group is frozen from that moment.
func TestProposeRemoveIssuesAnExternalRemoveThatFreezesTheGroup(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)
	h.onlineSession(t, session)

	action := id.New()
	if err := h.ds.ProposeRemove(ctx, reg.GroupID, 1, action); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}

	rows, err := h.repo.ListProposals(ctx, reg.GroupID, 6, false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d proposal rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Origin != 0 {
		t.Errorf("origin = %d, want 0 (the instance)", row.Origin)
	}
	if row.Kind != 3 {
		t.Errorf("kind = %d, want 3 (Remove)", row.Kind)
	}
	if row.TargetLeaf == nil || *row.TargetLeaf != 1 {
		t.Errorf("target_leaf = %v, want 1", row.TargetLeaf)
	}
	if row.ActionID != action {
		t.Errorf("action_id = %s, want %s", row.ActionID, action)
	}
	if row.TTL != uint64((24 * time.Hour).Seconds()) {
		t.Errorf("ttl = %d, want the text group's 24 h", row.TTL)
	}

	// The blob is in the guest's queue, so `Proposals` can hand it back to a committer.
	list, err := h.ds.Proposals(ctx, reg.GroupID, session)
	if err != nil {
		t.Fatalf("Proposals: %v", err)
	}
	if len(list) != 1 || len(list[0].Blob) == 0 {
		t.Fatalf("the guest queued no blob for the issued proposal: %+v", list)
	}

	// The handshake log carries it, with no sender leaf: the instance has none.
	hs, err := h.repo.ListHandshakes(ctx, reg.GroupID, 0, 16)
	if err != nil {
		t.Fatalf("ListHandshakes: %v", err)
	}
	if len(hs) != 1 || hs[0].Kind != 0 || hs[0].SenderLeaf != nil || hs[0].SenderDevice != nil {
		t.Fatalf("the handshake log does not carry the instance proposal: %+v", hs)
	}

	if frozen, _ := h.ds.Frozen(ctx, reg.GroupID); !frozen {
		t.Error("issuing an instance proposal freezes the group while a member is online")
	}
}

// R12's "SQL is the record", on the instance path. `ProposalPut` necessarily runs BEFORE the
// transaction — the SQL row is keyed on the ref the guest's queue hands back — so every failure
// after the put has to take the proposal out of the queue again. Without that guard a refused
// ProposeRemove leaves the cached PublicGroup one proposal ahead of SQL; worse, the NEXT
// successful proposal writes that divergence into the durable state blob through `persistState`
// inside its own transaction, so it survives a restart and is cleared only by the next merge.
//
// Task 20's `queueMemberProposal` guards its own put exactly this way
// (TestARefusedMemberProposalIsTakenBackOutOfTheGuestsQueue); this is the same guard on the
// instance path, and the same fault injector proves it.
func TestARefusedInstanceProposalIsTakenBackOutOfTheGuestsQueue(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)

	queued := func(t *testing.T) int {
		t.Helper()
		n := 0
		if err := ds.WithGroupForTest(h.ds, ctx, reg.GroupID, func(g *mlswasi.PublicGroup) error {
			list, err := g.ProposalList(ctx)
			if err != nil {
				return err
			}
			n = len(list)
			return nil
		}); err != nil {
			t.Fatalf("withGroup: %v", err)
		}
		return n
	}

	h.failNextTx("AppendHandshake")
	if err := h.ds.ProposeRemove(ctx, reg.GroupID, 1, id.New()); err == nil {
		t.Fatal("ProposeRemove must fail when the handshake append inside its transaction does")
	}
	if rows, _ := h.repo.ListProposals(ctx, reg.GroupID, 6, true); len(rows) != 0 {
		t.Fatalf("%d proposal rows after a failed transaction, want 0", len(rows))
	}
	if got := queued(t); got != 0 {
		t.Fatalf("the guest holds %d queued proposals SQL does not have, want 0", got)
	}

	// And the successful one after it leaves exactly one proposal on BOTH sides — the assertion
	// that catches the divergence being written into the state blob rather than merely held.
	if err := h.ds.ProposeRemove(ctx, reg.GroupID, 1, id.New()); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	rows, err := h.repo.ListProposals(ctx, reg.GroupID, 6, true)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d proposal rows, want 1", len(rows))
	}
	if got := queued(t); got != 1 {
		t.Fatalf("the guest holds %d queued proposals for 1 SQL row", got)
	}
}

// Invariant 6's second half: a Remove of a leaf that is already gone is DROPPED, not proposed.
func TestProposeRemoveOfALeafThatIsAlreadyGoneIsDropped(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)

	if err := h.ds.ProposeRemove(ctx, reg.GroupID, 99_999, id.New()); err == nil {
		t.Error("a Remove of a leaf that is not a member must be dropped, not proposed")
	}
	if rows, _ := h.repo.ListProposals(ctx, reg.GroupID, 6, true); len(rows) != 0 {
		t.Errorf("%d proposals were stored for a target that is gone, want 0", len(rows))
	}
}

// Invariant 6's "before proposing" gate: an expired KeyPackage never becomes a proposal.
func TestAnExpiredKeyPackageIsRefusedBeforeProposing(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	joiner := h.deviceWithKeyPackage(t)

	h.clk.Advance(100 * 24 * time.Hour) // past the fixture KeyPackage's lifetime
	if err := h.ds.ProposeAdd(ctx, reg.GroupID, joiner, id.New()); err == nil {
		t.Fatal("ProposeAdd accepted an expired KeyPackage")
	}
	if rows, _ := h.repo.ListProposals(ctx, reg.GroupID, 6, true); len(rows) != 0 {
		t.Fatalf("%d proposals were stored for an expired KeyPackage, want 0", len(rows))
	}
}

// A device with no KeyPackage at all is refused the same way, and nothing is written.
func TestProposeAddWithoutAKeyPackageIsRefused(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)

	err := h.ds.ProposeAdd(ctx, reg.GroupID, id.New(), id.New())
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_INVALID_REQUEST" {
		t.Fatalf("got %v, want E_INVALID_REQUEST", err)
	}
	if rows, _ := h.repo.ListProposals(ctx, reg.GroupID, 6, true); len(rows) != 0 {
		t.Fatalf("%d proposals were stored, want 0", len(rows))
	}
}

// ProposeAdd issues a real external Add and consumes the device's KeyPackage, so the same
// KeyPackage cannot be proposed twice.
func TestProposeAddIssuesAnExternalAddAndConsumesTheKeyPackage(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	joiner := h.deviceWithKeyPackage(t)

	action := id.New()
	if err := h.ds.ProposeAdd(ctx, reg.GroupID, joiner, action); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	rows, err := h.repo.ListProposals(ctx, reg.GroupID, 6, false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d proposal rows, want 1", len(rows))
	}
	if rows[0].Kind != 1 {
		t.Errorf("kind = %d, want 1 (Add)", rows[0].Kind)
	}
	if rows[0].TargetDevice == nil || *rows[0].TargetDevice != joiner {
		t.Errorf("target_device = %v, want %s", rows[0].TargetDevice, joiner)
	}
	if rows[0].ActionID != action {
		t.Errorf("action_id = %s, want %s", rows[0].ActionID, action)
	}
	if len(rows[0].KeyPackage) == 0 {
		t.Error("the row must keep the KeyPackage the proposal was built from, for the re-issue")
	}

	// A second Add for the same device while the first is outstanding is a no-op: the action is
	// already in flight, so nothing is proposed and no second KeyPackage is spent.
	if err := h.ds.ProposeAdd(ctx, reg.GroupID, joiner, id.New()); err != nil {
		t.Errorf("a repeated ProposeAdd for an outstanding Add must be a no-op, got %v", err)
	}
	if rows, _ := h.repo.ListProposals(ctx, reg.GroupID, 6, true); len(rows) != 1 {
		t.Errorf("%d proposal rows after the repeat, want 1: the device was proposed twice", len(rows))
	}
}

// Two Adds for one signature key make every commit that carries them invalid (OpenMLS:
// DuplicateSignatureKey), so the delivery service never issues the second: a device whose Add is
// outstanding is not proposed again, and a device that is already a current leaf is refused. The
// kick_with_outstanding_add_production_acl scenario found the race between a channel-membership
// sync and an admit on CI.
func TestProposeAddNeverIssuesTwoAddsForOneDeviceAndRefusesACurrentMember(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, creator := h.mustRegister(t)
	joiner := h.deviceWithKeyPackage(t)

	for i := range 3 {
		if err := h.ds.ProposeAdd(ctx, reg.GroupID, joiner, id.New()); err != nil {
			t.Fatalf("ProposeAdd #%d: %v", i+1, err)
		}
	}
	rows, err := h.repo.ListProposals(ctx, reg.GroupID, 6, true)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d Add proposals for one device, want exactly 1", len(rows))
	}

	err = h.ds.ProposeAdd(ctx, reg.GroupID, creator.DeviceID, id.New())
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_INVALID_REQUEST" {
		t.Fatalf("an Add for a current member: got %v, want E_INVALID_REQUEST", err)
	}
	if !errors.Is(err, ds.ErrAlreadyMember) {
		t.Fatalf("an Add for a current member: %v does not match ds.ErrAlreadyMember", err)
	}
	if rows, _ := h.repo.ListProposals(ctx, reg.GroupID, 6, true); len(rows) != 1 {
		t.Fatalf("%d proposal rows after refusing a current member, want 1", len(rows))
	}
}

// C4 (fix wave): a device its user's newest signed list does not name is never proposed, and its
// KeyPackage is not spent. A member commit could satisfy neither checkAddedMember (which refuses
// the Add) nor clause 1 (which refuses leaving it out), so proposing it froze the group until the
// Add voided, and took the KeyPackage the device's own pairing needed. Once the user publishes a
// list that names the device, it is proposed.
func TestProposeAddRefusesADeviceAbsentFromItsUsersSignedDeviceList(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	laptop := h.deviceWithKeyPackageListed(t, false)
	before, err := h.repo.CountKeyPackages(ctx, laptop, h.clk.Now().Unix())
	if err != nil || before != 1 {
		t.Fatalf("CountKeyPackages before = %d, %v; want the one fixture package", before, err)
	}

	err = h.ds.ProposeAdd(ctx, reg.GroupID, laptop, id.New())
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_INVALID_REQUEST" {
		t.Fatalf("ProposeAdd of an unlisted device = %v, want E_INVALID_REQUEST", err)
	}
	if rows, _ := h.repo.ListProposals(ctx, reg.GroupID, 6, true); len(rows) != 0 {
		t.Fatalf("%d proposals stored for an unlisted device, want 0", len(rows))
	}
	if n, _ := h.repo.CountKeyPackages(ctx, laptop, h.clk.Now().Unix()); n != before {
		t.Fatalf("the unlisted device's KeyPackages went from %d to %d: the refusal spent one", before, n)
	}

	// Pairing step 5: the user publishes the list that names the laptop.
	dev, err := h.repo.GetDevice(ctx, laptop)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	blob := signedDeviceList(t, testSSK(0x6b), dev.UserID, []listEntry{
		{DeviceID: laptop[:], DSKPub: dev.DSKPub, AddedAt: 2},
	})
	if err := h.repo.PutDeviceList(ctx, store.DeviceListRow{
		UserID: dev.UserID, Version: 2, Blob: blob, SSKSignature: blob[len(blob)-64:],
		PrevHash: make([]byte, 32), Created: h.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutDeviceList v2: %v", err)
	}
	if err := h.ds.ProposeAdd(ctx, reg.GroupID, laptop, id.New()); err != nil {
		t.Fatalf("ProposeAdd once listed: %v", err)
	}
	if rows, _ := h.repo.ListProposals(ctx, reg.GroupID, 6, false); len(rows) != 1 {
		t.Fatalf("%d proposals once listed, want 1", len(rows))
	}
}

// registeredDeviceFor registers the device of a committed-set KeyPackage under the given user and
// key, listed in that user's signed device list. With kp.user and kp.dsk it is the honest device;
// with anything else it is a device whose directory holds a package it could not publish today —
// one uploaded before the delivery service bound a package to its device's registered key.
func (h *dsHarness) registeredDeviceFor(t *testing.T, kp keyPackageSetEntry, user id.ID, dsk []byte) {
	t.Helper()
	now := h.clk.Now().Unix()
	ssk := testSSK(0x6b)
	h.userWithSSK(t, user, ssk)
	if err := h.repo.CreateDevice(context.Background(), store.DeviceRow{
		ID: kp.device, UserID: user, DSKPub: dsk,
		Tier: 0, SignerTier: 0, CredentialBlob: []byte{0xf6}, LastSeen: now, Created: now,
	}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	h.publishDeviceList(t, user, signedDeviceList(t, ssk, user, []listEntry{
		{DeviceID: kp.device[:], DSKPub: dsk, AddedAt: 1},
	}))
}

// putDirectoryPackage stores one package in a device's directory straight through the store, as
// an upload accepted before the key binding left it there.
func (h *dsHarness) putDirectoryPackage(t *testing.T, device id.ID, blob []byte, lastResort uint8, expiresIn time.Duration) []byte {
	t.Helper()
	ref := id.New()
	if err := h.repo.PutKeyPackages(context.Background(), device, []store.KeyPackageRow{{
		DeviceID: device, KPRef: ref[:], Blob: blob, LastResort: lastResort,
		Expires: h.clk.Now().Add(expiresIn).Unix(), Created: h.clk.Now().Unix(),
	}}); err != nil {
		t.Fatalf("PutKeyPackages: %v", err)
	}
	return ref[:]
}

// The follow-up to hardening C: the instance's own Add takes a package from the directory, and a
// package that is not bound to its device — one stored before the binding existed — is skipped
// before it is spent and deleted, and the device's next package is proposed. The device here is
// honest; the package served first (it expires first) was built by another device of another user
// under another key.
func TestProposeAddSkipsAndDeletesADirectoryPackageNotBoundToItsDevice(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	honest := h.nextKeyPackageDevice(t)
	stranger := h.nextKeyPackageDevice(t)
	h.registeredDeviceFor(t, honest, honest.user, honest.dsk)
	h.putDirectoryPackage(t, honest.device, stranger.blob, 0, 10*24*time.Hour)
	h.putDirectoryPackage(t, honest.device, honest.blob, 0, 80*24*time.Hour)

	if err := h.ds.ProposeAdd(ctx, reg.GroupID, honest.device, id.New()); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	rows, err := h.repo.ListProposals(ctx, reg.GroupID, 6, true)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(rows) != 1 || !bytes.Equal(rows[0].KeyPackage, honest.blob) {
		t.Fatalf("got %d proposals, the first built from the honest package: %v; want exactly one",
			len(rows), len(rows) == 1 && bytes.Equal(rows[0].KeyPackage, honest.blob))
	}
	// The stranger's package is gone from the directory; the honest one is the consumed row left.
	if got := h.countRows(t, "key_packages"); got != 1 {
		t.Fatalf("key_packages holds %d rows, want 1: the unbound package must be deleted", got)
	}
}

// With only unbound packages in its directory the device has none to propose: the outcome is the
// existing "no usable KeyPackage" refusal, nothing is proposed, and every unbound package —
// the last-resort one included, which is otherwise never consumed — is deleted.
func TestProposeAddWithOnlyUnboundPackagesIsTheNoPackageRefusal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lastResort uint8
		mismatch   string
	}{
		{"a package under another key", 0, "key"},
		{"a last-resort package under another key", 1, "key"},
		{"a package naming another user", 0, "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDSHarness(t)
			ctx := context.Background()
			reg, _ := h.mustRegister(t)
			kp := h.nextKeyPackageDevice(t)
			user, dsk := kp.user, kp.dsk
			if tc.mismatch == "key" {
				dsk = bytes.Repeat([]byte{4}, 32)
			} else {
				user = id.New()
			}
			h.registeredDeviceFor(t, kp, user, dsk)
			h.putDirectoryPackage(t, kp.device, kp.blob, tc.lastResort, 80*24*time.Hour)

			err := h.ds.ProposeAdd(ctx, reg.GroupID, kp.device, id.New())
			var dsErr *ds.Error
			if !errors.As(err, &dsErr) || dsErr.Code != "E_INVALID_REQUEST" ||
				!strings.Contains(dsErr.Detail, "no usable KeyPackage") {
				t.Fatalf("got %v, want E_INVALID_REQUEST: no usable KeyPackage", err)
			}
			if rows, _ := h.repo.ListProposals(ctx, reg.GroupID, 6, true); len(rows) != 0 {
				t.Fatalf("%d proposals stored, want 0", len(rows))
			}
			if got := h.countRows(t, "key_packages"); got != 0 {
				t.Fatalf("key_packages holds %d rows, want 0: the unbound package must be deleted", got)
			}
		})
	}
}

// The join-storm drain applies the same clause: an unlisted device is dropped from the queue
// without spending its KeyPackage, and the listed device beside it is proposed.
func TestProposeAddBatchDropsAnUnlistedDeviceWithoutSpendingItsKeyPackage(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	unlisted := h.deviceWithKeyPackageListed(t, false)
	listed := h.eligibleDeviceWithKeyPackage(t)
	if row, err := h.repo.GetDevice(ctx, unlisted); err == nil {
		h.acl.allow(row.UserID)
	}
	if err := h.ds.ProposeAddBatch(ctx, reg.GroupID, []id.ID{unlisted, listed}); err != nil {
		t.Fatalf("ProposeAddBatch: %v", err)
	}
	rows, err := h.repo.ListProposals(ctx, reg.GroupID, 6, false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(rows) != 1 || rows[0].TargetDevice == nil || *rows[0].TargetDevice != listed {
		t.Fatalf("outstanding Adds = %+v, want exactly the listed device's", rows)
	}
	if n, _ := h.repo.CountKeyPackages(ctx, unlisted, h.clk.Now().Unix()); n != 1 {
		t.Fatalf("the unlisted device has %d KeyPackages left, want its 1", n)
	}
	if got := ds.PendingJoinsForTest(h.ds, reg.GroupID); got != 0 {
		t.Fatalf("%d devices still queued, want 0: the unlisted one is dropped, not retried", got)
	}
}

// ProposeAddBatch caps one commit's Adds at MaxAddsPerCommit, and it reads the outstanding count
// at the GROUP'S CURRENT EPOCH. Reading it at literal epoch 0 would see nothing past epoch 0, so
// `room` would always be the full 256 and successive batches would push straight past the cap —
// the one thing the batching exists to prevent.
func TestProposeAddBatchNeverExceedsTwoHundredAndFiftySixAddsPerCommit(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)

	// 256 Adds are already outstanding at the group's epoch.
	for i := 0; i < ds.DefaultPolicy().MaxAddsPerCommit; i++ {
		h.putDSAddProposal(t, reg.GroupID, 6)
	}
	devices := make([]id.ID, 8)
	for i := range devices {
		devices[i] = h.deviceWithKeyPackage(t)
	}
	if err := h.ds.ProposeAddBatch(ctx, reg.GroupID, devices); err != nil {
		t.Fatalf("ProposeAddBatch: %v", err)
	}
	rows, err := h.repo.ListProposals(ctx, reg.GroupID, 6, false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(rows) > ds.DefaultPolicy().MaxAddsPerCommit {
		t.Fatalf("%d Adds outstanding for one commit, want at most %d",
			len(rows), ds.DefaultPolicy().MaxAddsPerCommit)
	}
	// The remainder is not dropped: it waits for the next epoch.
	if got := ds.PendingJoinsForTest(h.ds, reg.GroupID); got != len(devices) {
		t.Errorf("%d devices queued for the next commit, want %d — a dropped tail stalls a join storm",
			got, len(devices))
	}
}

// A batch that fits issues real Adds, and one unusable KeyPackage does not sink the rest.
func TestProposeAddBatchSkipsAnUnusableKeyPackageAndKeepsGoing(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)

	good := h.eligibleDeviceWithKeyPackage(t)
	// bad is eligible in every respect the drain checks — a live device of an admitted user with
	// an available KeyPackage — but its KeyPackage does not validate, so the guest refuses the Add.
	bad := h.eligibleDeviceWithKeyPackage(t)
	if _, err := h.repo.TakeKeyPackage(ctx, bad, h.clk.Now().Unix()); err != nil {
		t.Fatalf("TakeKeyPackage: %v", err)
	}
	junk := id.New()
	if err := h.repo.PutKeyPackages(ctx, bad, []store.KeyPackageRow{{
		DeviceID: bad, KPRef: junk[:], Blob: []byte{0x00, 0x01}, Expires: h.clk.Now().Unix() + 86_400,
		Created: h.clk.Now().Unix(),
	}}); err != nil {
		t.Fatalf("PutKeyPackages: %v", err)
	}
	if err := h.ds.ProposeAddBatch(ctx, reg.GroupID, []id.ID{bad, good}); err != nil {
		t.Fatalf("ProposeAddBatch: %v", err)
	}
	rows, err := h.repo.ListProposals(ctx, reg.GroupID, 6, false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(rows) != 1 || rows[0].TargetDevice == nil || *rows[0].TargetDevice != good {
		t.Fatalf("the batch did not continue past the unusable device: %+v", rows)
	}
}

// ------------------------------------------------------------- the re-issue

// The logical action survives, and it is DROPPED when the target leaf is already gone: invariant
// 6's "retried with a fresh KeyPackage, or dropped if the target leaf is already gone".
func TestAReissuedActionKeepsItsActionIdAndIsDroppedWhenTheTargetIsGone(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)

	action := id.New()
	if err := h.ds.ProposeRemove(ctx, reg.GroupID, 1, action); err != nil {
		t.Fatalf("ProposeRemove: %v", err)
	}
	before, err := h.repo.ListProposals(ctx, reg.GroupID, 6, false)
	if err != nil || len(before) != 1 {
		t.Fatalf("ListProposals: %+v %v", before, err)
	}

	// A re-issue at the same epoch supersedes the row rather than adding a second one, and the
	// logical action is carried across.
	if err := h.ds.ReissueFor(ctx, reg.GroupID, action); err != nil {
		t.Fatalf("ReissueFor: %v", err)
	}
	after, err := h.repo.ListProposals(ctx, reg.GroupID, 6, false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("%d proposals after the re-issue, want 1: the superseded row must be retired, "+
			"or invariant 4's clause 1 demands a ref no commit can reference", len(after))
	}
	if after[0].ActionID != action {
		t.Errorf("action_id = %s after the re-issue, want %s — the logical action survives",
			after[0].ActionID, action)
	}

	// The target leaves the group. The action is now satisfied, not retried.
	h.dropMember(t, reg.GroupID, 1)
	if err := h.ds.ReissueFor(ctx, reg.GroupID, action); err != nil {
		t.Fatalf("ReissueFor after the target left: %v", err)
	}
	gone, err := h.repo.ListProposals(ctx, reg.GroupID, 6, true)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("%d proposals survive a target that is already gone, want 0", len(gone))
	}
}

// ReissueFor names an action nobody issued.
func TestReissueForAnUnknownActionIsNotFound(t *testing.T) {
	h := newDSHarness(t)
	reg, _ := h.mustRegister(t)
	err := h.ds.ReissueFor(context.Background(), reg.GroupID, id.New())
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
		t.Fatalf("got %v, want E_NOT_FOUND", err)
	}
}

// ------------------------------------------ C2: an Add whose user became ineligible

// putAddFor stores one outstanding instance Add row at the fixture epoch targeting device. The
// eligibility sweep reads SQL rows only, and the committed fixture's one KeyPackage can back just
// one real Add per group (a second would carry the same proposal ref).
func (h *dsHarness) putAddFor(t *testing.T, groupID, device id.ID) []byte {
	t.Helper()
	ref := id.New()
	if err := h.repo.PutProposal(context.Background(), store.ProposalRow{
		GroupID: groupID, Ref: ref[:], Epoch: 6, Kind: 1, Origin: 0, TargetDevice: &device,
		ActionID: id.New(), IssuedAt: h.clk.Now().Unix(), TTL: 86400,
	}); err != nil {
		t.Fatalf("PutProposal: %v", err)
	}
	return ref[:]
}

// voidAtOf reads one proposal's void_at at the fixture epoch; found is false when the row is gone.
func (h *dsHarness) voidAtOf(t *testing.T, groupID id.ID, ref []byte) (voidAt *int64, found bool) {
	t.Helper()
	rows, err := h.repo.ListProposals(context.Background(), groupID, 6, true)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	for _, r := range rows {
		if bytes.Equal(r.Ref, ref) {
			return r.VoidAt, true
		}
	}
	return nil, false
}

// A kick while Bob's Add is outstanding: clause 1 refuses every member commit that leaves the Add
// out and checkAddedMember refuses every commit that includes it, so without a void the group is
// frozen until the 24-hour TTL. VoidIneligibleAdds voids exactly the Adds whose user the ACL no
// longer admits, or whose device is gone, and leaves an eligible user's Add outstanding.
func TestVoidIneligibleAddsVoidsOnlyTheAddsNoCommitCouldCarry(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	bob := h.eligibleDeviceWithKeyPackage(t)
	carol := h.eligibleDeviceWithKeyPackage(t)
	bobAdd := h.putAddFor(t, reg.GroupID, bob)
	carolAdd := h.putAddFor(t, reg.GroupID, carol)
	goneAdd := h.putAddFor(t, reg.GroupID, id.New()) // a device the instance no longer knows

	bobRow, err := h.repo.GetDevice(ctx, bob)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	h.acl.revoke(bobRow.UserID) // the kick
	if err := h.ds.VoidIneligibleAdds(ctx, reg.GroupID); err != nil {
		t.Fatalf("VoidIneligibleAdds: %v", err)
	}
	if v, ok := h.voidAtOf(t, reg.GroupID, bobAdd); !ok || v == nil {
		t.Fatalf("the kicked user's Add: void_at %v (found %v), want it voided", v, ok)
	}
	if v, ok := h.voidAtOf(t, reg.GroupID, goneAdd); !ok || v == nil {
		t.Fatalf("the unknown device's Add: void_at %v (found %v), want it voided", v, ok)
	}
	if v, ok := h.voidAtOf(t, reg.GroupID, carolAdd); !ok || v != nil {
		t.Fatalf("the eligible user's Add: void_at %v (found %v), want it outstanding", v, ok)
	}
}

// The commit path runs the same void before invariant 4's clauses, so a change the api layer never
// reported (a role revoked for a user in the storm's in-flight slice) is caught by the next commit
// attempt. The commit here is refused structurally; the Add is void all the same.
func TestACommitAttemptVoidsAnOutstandingAddWhoseUserBecameIneligible(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, session := h.mustRegister(t)
	bob := h.eligibleDeviceWithKeyPackage(t)
	bobAdd := h.putAddFor(t, reg.GroupID, bob)
	bobRow, err := h.repo.GetDevice(ctx, bob)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	h.acl.revoke(bobRow.UserID)

	if _, err := h.ds.Commit(ctx, session, reg.GroupID, ds.CommitRequest{
		Epoch: 6, Commit: []byte{0x00, 0x01}, GroupInfo: dsFixture(t).groupInfo,
	}); err == nil {
		t.Fatal("a two-byte commit was accepted")
	}
	if v, ok := h.voidAtOf(t, reg.GroupID, bobAdd); !ok || v == nil {
		t.Fatalf("after a commit attempt the ineligible Add has void_at %v (found %v), want it voided", v, ok)
	}
}

// The re-issue drops an Add whose user is no longer eligible instead of re-proposing it with a
// fresh TTL, which would restart the freeze on a proposal no commit can satisfy.
func TestAReissuedAddIsDroppedWhenItsUserIsNoLongerEligible(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	bob := h.eligibleDeviceWithKeyPackage(t)
	action := id.New()
	if err := h.ds.ProposeAdd(ctx, reg.GroupID, bob, action); err != nil {
		t.Fatalf("ProposeAdd: %v", err)
	}
	// A second KeyPackage, so a re-issue would have one to spend.
	h.seedKeyPackages(t, bob, 1, 80*24*time.Hour)
	bobRow, err := h.repo.GetDevice(ctx, bob)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	h.acl.revoke(bobRow.UserID)

	if err := h.ds.ReissueFor(ctx, reg.GroupID, action); err != nil {
		t.Fatalf("ReissueFor: %v", err)
	}
	if rows, _ := h.repo.ListProposals(ctx, reg.GroupID, 6, true); len(rows) != 0 {
		t.Fatalf("%d proposal rows after re-issuing an ineligible Add, want 0: %+v", len(rows), rows)
	}
}

// An outstanding Add for a device its user's newest signed list no longer names (not revoked) is
// an Add no commit can carry: checkAddedMember refuses the commit that includes it and clause 1
// refuses the one that omits it. The void pass catches it the same way it catches an ACL change.
func TestAnOutstandingAddForADeviceDroppedFromItsSignedListIsVoided(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	reg, _ := h.mustRegister(t)
	bob := h.eligibleDeviceWithKeyPackage(t)
	carol := h.eligibleDeviceWithKeyPackage(t)
	bobAdd := h.putAddFor(t, reg.GroupID, bob)
	carolAdd := h.putAddFor(t, reg.GroupID, carol)

	// Both are listed: nothing is voided.
	if err := h.ds.VoidIneligibleAdds(ctx, reg.GroupID); err != nil {
		t.Fatalf("VoidIneligibleAdds: %v", err)
	}
	if v, ok := h.voidAtOf(t, reg.GroupID, bobAdd); !ok || v != nil {
		t.Fatalf("a listed device's Add: void_at %v (found %v), want it outstanding", v, ok)
	}

	// Bob's user publishes a newer list that names another device only.
	bobRow, err := h.repo.GetDevice(ctx, bob)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	other := id.New()
	blob := signedDeviceList(t, testSSK(0x6b), bobRow.UserID, []listEntry{
		{DeviceID: other[:], DSKPub: bytes.Repeat([]byte{9}, 32), AddedAt: 2},
	})
	if err := h.repo.PutDeviceList(ctx, store.DeviceListRow{
		UserID: bobRow.UserID, Version: 2, Blob: blob,
		SSKSignature: blob[len(blob)-64:], PrevHash: make([]byte, 32), Created: h.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutDeviceList: %v", err)
	}

	if err := h.ds.VoidIneligibleAdds(ctx, reg.GroupID); err != nil {
		t.Fatalf("VoidIneligibleAdds: %v", err)
	}
	if v, ok := h.voidAtOf(t, reg.GroupID, bobAdd); !ok || v == nil {
		t.Fatalf("the delisted device's Add: void_at %v (found %v), want it voided", v, ok)
	}
	if v, ok := h.voidAtOf(t, reg.GroupID, carolAdd); !ok || v != nil {
		t.Fatalf("the still-listed device's Add: void_at %v (found %v), want it outstanding", v, ok)
	}
	rows, err := h.repo.ListProposals(ctx, reg.GroupID, 6, false)
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	for _, r := range rows {
		if bytes.Equal(r.Ref, bobAdd) {
			t.Fatalf("the voided Add is still among the proposals a commit must carry")
		}
	}
}

// ------------------------------------------------------ the gaps, named in code

// Invariant 5's nobody-online exception ends in a re-issue FOR THE NEW EPOCH, which needs an
// accepted external commit and so a second epoch; this package can reach only the fixture's one.
// TestWithNobodyOnlineAnExternalCommitIsAcceptedAndTheProposalsAreReissued, which shipped here as
// a skip, is task 29's harness-driven test of the same name in internal/testkit/accepted_test.go
// (Ruling C(5)); join_test.go asserts the freeze's half that needs no accepted commit.

// ---------------------------------------------------------------- the helpers

// fixtureExternalSenderKey re-derives the instance signing key the committed fixture put in the
// group's `external_senders` extension. Without it no proposal this package builds would be
// ACCEPTED by the guest: `queue_proposal` resolves an external proposal's sender through that
// extension and verifies the signature (core/dilla-core/src/public_group/state.rs:260-285), so a
// key of the test's own invention would refuse every ProposeAdd and ProposeRemove and leave the
// whole issuing path untested.
//
// The derivation is testkit/src/client.rs:57-80 in Go: the seed's big-endian bytes and
// seed*0x9e3779b9's fill the first sixteen of a 32-byte ChaCha20 seed, the rest is zero, and the
// first 32 bytes of that stream are the Ed25519 secret. testkit/src/fixtures.rs:116 makes the
// instance's seed `spec.seed ^ 0x0d15_0d15`, and the fixture was generated with the CLI's default
// seed 0x5eed (testkit/src/bin/dilla-testkit.rs:33).
//
// It is not a hardcoded constant on purpose: a regenerated fixture with a different seed must
// change ONE number here, and the tests that use the key fail loudly rather than silently
// skipping the accept path.
func fixtureExternalSenderKey() [32]byte {
	const seed = uint64(0x5eed) ^ 0x0d15_0d15
	var material [32]byte
	binary.BigEndian.PutUint64(material[0:8], seed)
	binary.BigEndian.PutUint64(material[8:16], seed*0x9e37_79b9)
	var nonce [12]byte
	c, err := chacha20.NewUnauthenticatedCipher(material[:], nonce[:])
	if err != nil {
		panic(err)
	}
	var out [32]byte
	c.XORKeyStream(out[:], out[:])
	return out
}

// putDSProposalWithTTL is putDSProposal with the TTL the caller's group kind implies, which is
// what the sweep reads.
func (h *dsHarness) putDSProposalWithTTL(t *testing.T, groupID id.ID, epoch, ttl uint64) []byte {
	t.Helper()
	ref := id.New()
	row := store.ProposalRow{
		GroupID: groupID, Ref: ref[:], Epoch: epoch, Kind: 3, Origin: 0,
		ActionID: id.New(), IssuedAt: h.clk.Now().Unix(), TTL: ttl,
	}
	if err := h.repo.PutProposal(context.Background(), row); err != nil {
		t.Fatalf("PutProposal: %v", err)
	}
	return row.Ref
}

// putDSAddProposal is one instance-originated Add row, which is what ProposeAddBatch counts.
func (h *dsHarness) putDSAddProposal(t *testing.T, groupID id.ID, epoch uint64) []byte {
	t.Helper()
	ref := id.New()
	row := store.ProposalRow{
		GroupID: groupID, Ref: ref[:], Epoch: epoch, Kind: 1, Origin: 0,
		ActionID: id.New(), IssuedAt: h.clk.Now().Unix(), TTL: 86400,
	}
	if err := h.repo.PutProposal(context.Background(), row); err != nil {
		t.Fatalf("PutProposal: %v", err)
	}
	return row.Ref
}

// bareGroup is one more open group row, with no MLS state: the sweep reads `mls_pending_proposals`
// and never touches the guest, so a row is all a paging test needs.
func (h *dsHarness) bareGroup(t *testing.T) id.ID {
	t.Helper()
	gid := id.New()
	if err := h.repo.CreateGroup(context.Background(), store.GroupRow{
		GroupID: gid, Binding: []byte{0xf6}, Kind: 0, TargetID: id.New(),
		Ciphersuite: 1, Epoch: 0, Created: h.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return gid
}

// deviceWithKeyPackage registers one user and one device with the instance and stores that
// device's own KeyPackage for it. `key_packages.device_id` references `devices(id)`, so the two
// rows are not optional. The device is the next one of the committed KeyPackage set
// (keypackage_set_test.go): its id, its user and its registered key are the ones its package's
// credential and leaf carry, as for every honest device — the delivery service proposes no
// package its device did not build under its registered key.
//
// The user's signed device list names the device, as it does for every device a real user has
// finished pairing: the delivery service proposes an Add only for a listed device (invariant 4's
// device-list clause, checked before the KeyPackage is spent).
func (h *dsHarness) deviceWithKeyPackage(t *testing.T) id.ID {
	t.Helper()
	return h.deviceWithKeyPackageListed(t, true)
}

// deviceWithKeyPackageListed is deviceWithKeyPackage with the device named in its user's signed
// device list or not: listed == false is a device that enrolled and published its KeyPackages
// before its user published the list that names it (protocol/03 § Pairing, steps 2 and 5). The
// user's list then names only another device.
func (h *dsHarness) deviceWithKeyPackageListed(t *testing.T, listed bool) id.ID {
	t.Helper()
	ctx := context.Background()
	now := h.clk.Now().Unix()
	kp := h.nextKeyPackageDevice(t)
	user := kp.user
	ssk := testSSK(0x6b)
	h.userWithSSK(t, user, ssk)
	device := kp.device
	dsk := kp.dsk
	if err := h.repo.CreateDevice(ctx, store.DeviceRow{
		ID: device, UserID: user, DSKPub: dsk,
		Tier: 0, SignerTier: 0, CredentialBlob: []byte{0xf6}, LastSeen: now, Created: now,
	}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	entry := listEntry{DeviceID: device[:], DSKPub: dsk, AddedAt: 1}
	if !listed {
		other := id.New()
		entry = listEntry{DeviceID: other[:], DSKPub: bytes.Repeat([]byte{5}, 32), AddedAt: 1}
	}
	h.publishDeviceList(t, user, signedDeviceList(t, ssk, user, []listEntry{entry}))
	ref := id.New()
	if err := h.repo.PutKeyPackages(ctx, device, []store.KeyPackageRow{{
		DeviceID: device, KPRef: ref[:], Blob: kp.blob, LastResort: 0,
		Expires: h.clk.Now().Add(80 * 24 * time.Hour).Unix(), Created: now,
	}}); err != nil {
		t.Fatalf("PutKeyPackages: %v", err)
	}
	return device
}

// dropMember rewrites the group's leaf table without the given leaf, which is what a commit that
// removed it would leave behind.
func (h *dsHarness) dropMember(t *testing.T, groupID id.ID, leaf uint32) {
	t.Helper()
	ctx := context.Background()
	members, err := h.repo.ListMembers(ctx, groupID)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	kept := make([]store.MemberRow, 0, len(members))
	for _, m := range members {
		if m.LeafIndex != leaf {
			kept = append(kept, m)
		}
	}
	if err := h.repo.Tx(ctx, func(tx store.Repository) error {
		return tx.ReplaceMembers(ctx, groupID, 6, kept)
	}); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
}

// onlineSession puts the session's device on a REAL gateway connection: hello, identify, ready,
// over a real WebSocket against the real handler. `Gateway.Online` is the single source invariant
// 5 reads and it is defined over a connection in state ready, so a test that faked it would be
// asserting the fake.
//
// It was `online` until task 22, which needs that name for the variadic device-id form its own
// tests are written against (`h.online(g.members[0], g.members[1])`). Go has no overloading, so
// the session form is spelled out here; the two differ in what they take, not in what they do —
// task 22's form records the frames the connection receives, this one does not.
func (h *dsHarness) onlineSession(t *testing.T, session auth.Session) {
	t.Helper()
	h.auth.add(session)

	srv := httptest.NewServer(h.gw.Handler())
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{ //nolint:bodyclose // websocket.Dial documents that the handshake response body never needs closing
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + h.auth.token(session)}},
		Subprotocols: []string{"dilla.v1"},
	})
	if err != nil {
		t.Fatalf("dial the gateway: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })

	if op := readGatewayOp(t, ctx, c); op != gateway.OpHello {
		t.Fatalf("first frame op %d, want hello", op)
	}
	payload, err := cborx.Marshal([]any{"", uint64(1), uint64(1), uint64(1), uint64(0)})
	if err != nil {
		t.Fatalf("encode identify: %v", err)
	}
	frame, err := gateway.Encode(gateway.Frame{Op: gateway.OpIdentify, Payload: cborx.Raw(payload)}, 1)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("write identify: %v", err)
	}
	if op := readGatewayOp(t, ctx, c); op != gateway.OpReady {
		t.Fatalf("op %d, want ready", op)
	}
	if !h.gw.Online(session.DeviceID) {
		t.Fatal("a device that reached ready is not online")
	}
}

// readGatewayOp reads one server frame and returns its opcode. The frame is the four-element
// array [op, n, group_id, payload]; only the opcode matters here.
func readGatewayOp(t *testing.T, ctx context.Context, c *websocket.Conn) gateway.Op {
	t.Helper()
	typ, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("message type %v, want binary: dilla's wire is CBOR, never text", typ)
	}
	var frame []cbor.RawMessage
	if err := cborx.Unmarshal(b, &frame); err != nil {
		t.Fatalf("decode the frame: %v", err)
	}
	if len(frame) != 4 {
		t.Fatalf("the frame has %d elements, want 4", len(frame))
	}
	var op uint64
	if err := cborx.Unmarshal(frame[0], &op); err != nil {
		t.Fatalf("decode the opcode: %v", err)
	}
	return gateway.Op(op)
}

func TestAnInstanceRemoveWithoutTargetLeafDoesNotPanicAfterPersisting(t *testing.T) {
	h := newDSHarness(t)
	ctx := t.Context()
	reg, _ := h.mustRegister(t)
	row, err := h.repo.GetGroup(ctx, reg.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := h.wasm.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keys := testInstanceKeys(t)
	blob, err := inst.ExternalProposeRemove(ctx, reg.GroupID[:], row.Epoch, 1, keys.ExternalSenderPriv[:])
	inst.Release()
	if err != nil {
		t.Fatal(err)
	}
	if err := ds.StoreInstanceProposalForTest(h.ds, ctx, reg.GroupID, row, blob, store.ProposalRow{Kind: uint8(mlswasi.ProposalRemove), Origin: 0, ActionID: id.New()}); err != nil {
		t.Fatal(err)
	}
	proposals, err := h.repo.ListProposals(ctx, reg.GroupID, row.Epoch, false)
	if err != nil || len(proposals) != 1 {
		t.Fatalf("durable proposals = %+v, %v; want one", proposals, err)
	}
}

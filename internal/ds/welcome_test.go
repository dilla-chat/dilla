package ds_test

// Store-and-forward Welcomes: protocol/02's endpoints 15 (`GET /v1/welcomes`) and 16
// (`DELETE /v1/welcomes/{welcome_id}`), and the writer the commit's transaction calls.
//
// Every test here drives `storeWelcomesTx` through `StoreWelcomesForTest` rather than through
// `Commit`, because no commit can be ACCEPTED against the committed fixture: there is no GroupInfo
// at epoch 7 (commit_test.go's header, task 20's report). The writer is the delivery service's
// own, called where the commit's transaction calls it.

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// gap-13: a Welcome is NOT consumed by the fetch. `StagedWelcome::new_from_welcome` consumes the
// key material inside the client even when the client then fails, so only the explicit DELETE
// marks it delivered.
func TestAWelcomeIsNotConsumedByTheFetchAndOnlyTheDeleteMarksItDelivered(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.group(t)
	joiner := h.device(t)
	h.storeWelcomes(t, g.id, g.epoch, 1, welcomeBlob(0x41, 96), joiner)
	session := h.sessionOf(t, joiner)

	first, err := h.ds.Welcomes(ctx, session, 0, 16)
	if err != nil {
		t.Fatalf("Welcomes: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("got %d welcomes, want 1", len(first))
	}
	second, err := h.ds.Welcomes(ctx, session, 0, 16)
	if err != nil {
		t.Fatalf("Welcomes again: %v", err)
	}
	if len(second) != 1 {
		t.Fatal("a fetch must not consume the Welcome")
	}
	if err := h.ds.AckWelcome(ctx, session, first[0].WelcomeID); err != nil {
		t.Fatalf("AckWelcome: %v", err)
	}
	third, err := h.ds.Welcomes(ctx, session, 0, 16)
	if err != nil {
		t.Fatalf("Welcomes after ack: %v", err)
	}
	if len(third) != 0 {
		t.Fatal("the DELETE marks it delivered")
	}
	// The row survives the acknowledgement: it is marked, not removed, so the retention sweep
	// rather than the client decides when the payload goes.
	if n := h.countRows(t, "mls_welcomes"); n != 1 {
		t.Fatalf("%d welcome rows after the ack, want the marked one", n)
	}
}

// Acknowledging a Welcome that is not this device's, or is already delivered, is E_NOT_FOUND —
// the code protocol/02 fixes for row 16.
func TestAcknowledgingAnotherDevicesWelcomeIsNotFound(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.group(t)
	joiner := h.device(t)
	h.storeWelcomes(t, g.id, g.epoch, 1, welcomeBlob(0x42, 96), joiner)
	welcomeID := h.welcomeIDOf(t, joiner, g.id)

	stranger := h.sessionOf(t, h.device(t))
	err := h.ds.AckWelcome(ctx, stranger, welcomeID)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
		t.Fatalf("got %v, want E_NOT_FOUND", err)
	}
	// And the Welcome is still queued for the device it belongs to.
	rows, err := h.ds.Welcomes(ctx, h.sessionOf(t, joiner), 0, 16)
	if err != nil || len(rows) != 1 {
		t.Fatalf("the owner's Welcome was consumed by a stranger's ack: %d rows, %v", len(rows), err)
	}
}

// gap-13: dilla Welcomes carry no tree, and the live tree has moved on, so the response carries
// the tree of the WELCOMING epoch.
func TestTheWelcomeResponseCarriesTheTreeOfTheWelcomingEpoch(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.group(t)
	joiner := h.device(t)
	h.storeWelcomes(t, g.id, g.epoch, 1, welcomeBlob(0x43, 96), joiner)

	live, err := h.ds.Tree(ctx, g.id, g.session)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}

	// The group moves on before the joiner collects: a later epoch, with a tree of its own.
	later := welcomeBlob(0x5A, 128)
	if err := h.repo.PutEpochTree(ctx, store.EpochTreeRow{
		GroupID: g.id, Epoch: g.epoch + 1, RatchetTree: later,
		TreeHash: bytes.Repeat([]byte{0x5B}, 32), Created: h.clk.Now().Unix(),
	}); err != nil {
		t.Fatalf("PutEpochTree: %v", err)
	}

	got, err := h.ds.Welcomes(ctx, h.sessionOf(t, joiner), 0, 16)
	if err != nil {
		t.Fatalf("Welcomes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d welcomes, want 1", len(got))
	}
	if got[0].Epoch != g.epoch {
		t.Fatalf("welcome epoch = %d, want %d", got[0].Epoch, g.epoch)
	}
	if bytes.Equal(got[0].RatchetTree, later) {
		t.Fatal("the Welcome must carry the tree of its own epoch, not the live one")
	}
	if !bytes.Equal(got[0].RatchetTree, live.RatchetTree) {
		t.Fatal("the Welcome's tree is not the one the welcoming epoch's PublicGroup exported")
	}
	if len(got[0].TreeHash) != 32 {
		t.Fatalf("tree_hash is %d bytes, want 32", len(got[0].TreeHash))
	}
	if !bytes.Equal(got[0].TreeHash, live.TreeHash) {
		t.Fatal("the Welcome's tree_hash is not the welcoming epoch's")
	}
}

// One 256-device commit stores ONE payload row and 256 welcome rows: the Welcome blob is
// identical for every joiner of one commit, and storing it 256 times is how a join storm fills a
// disk.
func TestA256DeviceCommitStoresOnePayloadAnd256WelcomeRows(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	joiners := make([]id.ID, 256)
	for i := range joiners {
		joiners[i] = h.device(t)
	}
	h.storeWelcomes(t, g.id, g.epoch, 1, welcomeBlob(0x44, 1024), joiners...)

	if got := h.countRows(t, "mls_welcome_payloads"); got != 1 {
		t.Fatalf("%d payload rows, want 1", got)
	}
	if got := h.countRows(t, "mls_welcomes"); got != 256 {
		t.Fatalf("%d welcome rows, want 256", got)
	}
	// And one epoch tree, not one per joiner.
	if got := h.countRows(t, "mls_epoch_trees"); got != 1 {
		t.Fatalf("%d epoch trees, want 1", got)
	}
	// Every joiner gets the whole body back, from the one row.
	rows, err := h.ds.Welcomes(context.Background(), h.sessionOf(t, joiners[255]), 0, 16)
	if err != nil {
		t.Fatalf("Welcomes: %v", err)
	}
	if len(rows) != 1 || len(rows[0].Blob) != 1024 {
		t.Fatalf("the last joiner got %d rows; blob is %d bytes, want 1024", len(rows), len(rows[0].Blob))
	}
}

// A Welcome past its 30-day delivery retention is gone from the fetch, whether or not the
// retention sweep has run yet: `expires` is stored on the row and the read honours it.
func TestAWelcomeIsGoneAfterItsExpiry(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	joiner := h.device(t)
	h.storeWelcomes(t, g.id, g.epoch, 1, welcomeBlob(0x45, 96), joiner)

	h.clk.Advance(31 * 24 * time.Hour)

	got, err := h.ds.Welcomes(context.Background(), h.sessionOf(t, joiner), 0, 16)
	if err != nil {
		t.Fatalf("Welcomes: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("a Welcome past its 30-day delivery retention is gone")
	}
}

// Row 15 is a CURSOR api: a client advances `after` by the last welcome_id it was handed and stops
// on an empty page. So a filter applied AFTER the SQL LIMIT strands it: if the first `limit`
// undelivered rows are all expired, the call answers an empty page while live rows sit behind
// them, and the client can never advance past them — the ids it would need for `after` are exactly
// the ones that were dropped. The read therefore keeps paging until it has `limit` live rows or
// the queue is exhausted.
func TestWelcomesPagesPastExpiredRowsInsteadOfStrandingTheCursor(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	joiner := h.device(t)
	groupID := id.New()

	// A full page of Welcomes the joiner never acknowledged, which then pass their 30-day
	// retention, and three live ones queued behind them.
	for i := range 64 {
		h.putWelcomeRow(t, joiner, groupID, uint64(i+1), welcomeBlob(byte(i+1), 96))
	}
	h.clk.Advance(31 * 24 * time.Hour)
	for i := range 3 {
		h.putWelcomeRow(t, joiner, groupID, uint64(200+i), welcomeBlob(byte(200+i), 96))
	}

	got, err := h.ds.Welcomes(ctx, h.sessionOf(t, joiner), 0, 16)
	if err != nil {
		t.Fatalf("Welcomes: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d welcomes, want the 3 live ones behind a page of expired rows", len(got))
	}
	for _, row := range got {
		if row.Expires <= h.clk.Now().Unix() {
			t.Fatal("an expired Welcome was served")
		}
	}
}

func TestWireTwoFiltersInteractionWelcomesAndRefusesTheirAcknowledgement(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	joiner := h.device(t)
	blocked, allowed := id.New(), id.New()
	h.putWelcomeRowKind(t, joiner, blocked, 1, welcomeBlob(0xA1, 96), 3) // interaction groups always need wire 3
	h.putWelcomeRowKind(t, joiner, allowed, 1, welcomeBlob(0xA2, 96), 2) // pairing remains wire-2 capable
	session := h.sessionOf(t, joiner)
	session.WireVersion = 2

	rows, err := h.ds.Welcomes(ctx, session, 0, 16)
	if err != nil {
		t.Fatalf("Welcomes: %v", err)
	}
	if len(rows) != 1 || rows[0].GroupID != allowed {
		t.Fatalf("wire 2 received welcome groups %v, want only pairing group %s", welcomeGroups(rows), allowed)
	}

	blockedID := h.welcomeIDOf(t, joiner, blocked)
	var upgrade *capability.UpgradeRequired
	if err := h.ds.AckWelcome(ctx, session, blockedID); !errors.As(err, &upgrade) {
		t.Fatalf("AckWelcome for interaction group = %v, want E_VERSION", err)
	}
	stored, err := h.repo.ListWelcomes(ctx, joiner, 0, 16)
	if err != nil {
		t.Fatalf("ListWelcomes after refused ack: %v", err)
	}
	for _, row := range stored {
		if row.GroupID == blocked && row.DeliveredAt != nil {
			t.Fatal("refused acknowledgement marked the filtered Welcome delivered")
		}
	}
}

func welcomeGroups(rows []store.WelcomeFull) []id.ID {
	groups := make([]id.ID, 0, len(rows))
	for _, row := range rows {
		groups = append(groups, row.GroupID)
	}
	return groups
}

// The same stranding, on the other filter: a provisional session sees only its pairing group's
// Welcome, and that row can sit behind a whole page of Welcomes for other groups.
func TestWelcomesPagesPastOtherGroupsForAProvisionalSession(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	joiner := h.device(t)
	other := id.New()
	pairing := id.New()

	for i := range 64 {
		h.putWelcomeRow(t, joiner, other, uint64(i+1), welcomeBlob(byte(i+1), 96))
	}
	h.putWelcomeRow(t, joiner, pairing, 1, welcomeBlob(0xF1, 96))

	rows, err := h.ds.Welcomes(ctx, h.provisionalSessionOf(t, joiner, pairing), 0, 16)
	if err != nil {
		t.Fatalf("Welcomes: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("a provisional session saw %d Welcomes, want its pairing group's one", len(rows))
	}
	if rows[0].GroupID != pairing {
		t.Fatalf("the served Welcome is group %s, want the pairing group %s", rows[0].GroupID, pairing)
	}
}

// Rows 15 and 16 accept a provisional session, and the restriction that makes that safe is
// enforced, not assumed: a provisional session is bound to ONE pairing group and collects that
// group's Welcome, nothing else. Without the check a session issued before enrolment can harvest
// the Welcomes of every group its device was ever added to — the escalation interfaces §2.2
// point 4 forbids.
func TestAProvisionalSessionCannotReachAnotherGroupsWelcome(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	pairing := h.group(t)
	joiner := h.device(t)

	// The device is added to BOTH groups, so a Welcome exists for each. The second group is
	// written directly: `Register` is bound to the committed fixture's one group id, so a harness
	// cannot register a second group at all.
	h.storeWelcomes(t, pairing.id, pairing.epoch, 1, welcomeBlob(0x46, 96), joiner)
	other := id.New()
	h.putWelcomeRow(t, joiner, other, 3, welcomeBlob(0x47, 96))

	// An enrolled session sees both.
	if rows, err := h.ds.Welcomes(ctx, h.sessionOf(t, joiner), 0, 64); err != nil || len(rows) != 2 {
		t.Fatalf("an enrolled session saw %d Welcomes (%v), want both", len(rows), err)
	}

	provisional := h.provisionalSessionOf(t, joiner, pairing.id)
	rows, err := h.ds.Welcomes(ctx, provisional, 0, 64)
	if err != nil {
		t.Fatalf("Welcomes: %v", err)
	}
	for _, row := range rows {
		if row.GroupID != pairing.id {
			t.Fatalf("a provisional session was served the Welcome of group %s, outside its pairing group %s",
				row.GroupID, pairing.id)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("a provisional session saw %d Welcomes, want exactly its pairing group's", len(rows))
	}

	// And acknowledging another group's Welcome is refused by code, not merely absent.
	err = h.ds.AckWelcome(ctx, provisional, h.welcomeIDOf(t, joiner, other))
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_PROVISIONAL_OUTSIDE_PAIRING" {
		t.Fatalf("got %v, want E_PROVISIONAL_OUTSIDE_PAIRING", err)
	}
	// Its own pairing group's Welcome it may acknowledge.
	if err := h.ds.AckWelcome(ctx, provisional, h.welcomeIDOf(t, joiner, pairing.id)); err != nil {
		t.Fatalf("a provisional session must be able to acknowledge its pairing Welcome: %v", err)
	}

	// A provisional session bound to no pairing group at all reaches nothing.
	unbound := provisional
	unbound.PairingGroup = nil
	if _, err := h.ds.Welcomes(ctx, unbound, 0, 64); !errors.As(err, &dsErr) ||
		dsErr.Code != "E_PROVISIONAL_OUTSIDE_PAIRING" {
		t.Fatalf("got %v, want E_PROVISIONAL_OUTSIDE_PAIRING for a session bound to no group", err)
	}
}

// The addressed fan-out: every joiner of a commit is sent its own `mls.welcome`, carrying the
// welcome_id the DELETE names. A client that is online when the commit lands must not have to poll
// row 15 to discover it.
func TestEveryJoinerIsSentItsOwnWelcomeFrame(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	joiners := []id.ID{h.device(t), h.device(t)}
	if h.sessions == nil {
		h.sessions = map[id.ID]auth.Session{}
	}
	for _, j := range joiners {
		h.sessions[j] = h.sessionOf(t, j)
	}
	h.online(joiners...)

	// A group whose epoch tree FITS the gateway's frame budget. The committed fixture's is 620 KiB
	// against a budget of 131 584 — the case the next test covers; this one is the group a handful
	// of devices actually make.
	groupID := id.New()
	blob := welcomeBlob(0x48, 96)
	welcomes := make([]ds.WelcomeFor, 0, len(joiners))
	for _, j := range joiners {
		h.putWelcomeRow(t, j, groupID, 3, blob)
		welcomes = append(welcomes, ds.WelcomeFor{DeviceID: j, Blob: blob})
	}
	// The tree and its hash are the welcoming epoch's, handed to the fan-out by the commit that
	// wrote them rather than read back once per joiner; `putWelcomeRow` stored these two.
	ds.FanOutWelcomesForTest(h.ds, ctx, groupID, 3,
		welcomeBlob(0xEE, 64), bytes.Repeat([]byte{0xAB}, 32), welcomes)

	for _, j := range joiners {
		f := h.waitDeviceFrame(t, j, "mls.welcome")
		if len(f.payload) != 6 {
			t.Fatalf("the welcome payload has %d elements, want 6: "+
				"[welcome_id, epoch, commit_seq, blob, ratchet_tree, tree_hash]", len(f.payload))
		}
		var welcomeID uint64
		if err := cborx.Unmarshal(f.payload[0], &welcomeID); err != nil {
			t.Fatalf("decode welcome_id: %v", err)
		}
		if int64(welcomeID) != h.welcomeIDOf(t, j, groupID) {
			t.Fatalf("the frame names welcome_id %d, not the row the DELETE addresses", welcomeID)
		}
		var treeHash []byte
		if err := cborx.Unmarshal(f.payload[5], &treeHash); err != nil {
			t.Fatalf("decode tree_hash: %v", err)
		}
		if len(treeHash) != 32 {
			t.Fatalf("tree_hash is %d bytes, want 32", len(treeHash))
		}
	}
}

// A Welcome whose ratchet tree is larger than the gateway's own `max_frame_bytes` is NOT sent as a
// frame, and the row stays queued for `GET /v1/welcomes`.
//
// This is not a nicety. A client sizes its websocket read limit from the `max_frame_bytes` the
// same gateway advertised in `hello`, so an oversize frame closes the joiner's connection rather
// than reaching it — and the joiner reconnects to be closed by it again. The committed 1,500-leaf
// fixture's tree is 620 KiB against a budget of 131 584, so this is the ordinary case for a group
// of real size, not a corner. See the CONCERN in fanOutWelcomes.
func TestAWelcomeTooLargeForTheFrameBudgetIsLeftToTheQueue(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.group(t)
	joiner := h.device(t)
	if h.sessions == nil {
		h.sessions = map[id.ID]auth.Session{}
	}
	h.sessions[joiner] = h.sessionOf(t, joiner)
	h.online(joiner)

	blob := welcomeBlob(0x49, 96)
	h.storeWelcomes(t, g.id, g.epoch, 9, blob, joiner)
	live, err := h.ds.Tree(ctx, g.id, g.session)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	ds.FanOutWelcomesForTest(h.ds, ctx, g.id, g.epoch, live.RatchetTree, live.TreeHash,
		[]ds.WelcomeFor{{DeviceID: joiner, Blob: blob}})

	// No frame, and the connection is still up: the oversize frame was never enqueued.
	h.expectNoWelcomeFrame(t, joiner)

	// And the Welcome is still there to be collected over HTTP, tree and all.
	rows, err := h.ds.Welcomes(ctx, h.sessionOf(t, joiner), 0, 16)
	if err != nil {
		t.Fatalf("Welcomes: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d welcomes, want the one the frame could not carry", len(rows))
	}
	if len(rows[0].RatchetTree) <= 131584 {
		t.Fatalf("the fixture's tree is %d bytes; this test needs one over the frame budget",
			len(rows[0].RatchetTree))
	}
}

// And it is skipped BEFORE it is paid for. The epoch tree is the same for every joiner of one
// commit, so the size is knowable once; the fan-out used to read the Welcome queue (up to 16 pages
// of 64 rows, every row carrying that 620 KiB tree through the join) and encode the whole payload
// for EACH joiner before comparing the result with the frame budget and dropping it. At
// Policy.MaxAddsPerCommit = 256 that is hundreds of megabytes read and allocated per commit and
// then thrown away — all of it inside the group lock `commit` still holds, with every other commit
// on that group waiting behind it.
func TestAnOversizeEpochTreeSkipsTheFanOutWithoutReadingTheQueue(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	g := h.group(t)
	if h.sessions == nil {
		h.sessions = map[id.ID]auth.Session{}
	}
	joiners := make([]id.ID, 4)
	for i := range joiners {
		joiners[i] = h.device(t)
		h.sessions[joiners[i]] = h.sessionOf(t, joiners[i])
	}
	h.online(joiners...)

	blob := welcomeBlob(0x4A, 96)
	h.storeWelcomes(t, g.id, g.epoch, 11, blob, joiners...)
	live, err := h.ds.Tree(ctx, g.id, g.session)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(live.RatchetTree) <= 131584 {
		t.Fatalf("the fixture's tree is %d bytes; this test needs one over the frame budget",
			len(live.RatchetTree))
	}
	welcomes := make([]ds.WelcomeFor, 0, len(joiners))
	for _, j := range joiners {
		welcomes = append(welcomes, ds.WelcomeFor{DeviceID: j, Blob: blob})
	}

	before := h.welcomeQueueReads()
	ds.FanOutWelcomesForTest(h.ds, ctx, g.id, g.epoch, live.RatchetTree, live.TreeHash, welcomes)
	if reads := h.welcomeQueueReads() - before; reads != 0 {
		t.Fatalf("the fan-out made %d Welcome-queue reads for a tree no frame can carry, want 0", reads)
	}
	for _, j := range joiners {
		h.expectNoWelcomeFrame(t, j)
	}
}

// A commit may address a Welcome ONLY to a device it ADDS.
//
// `storeWelcomesTx` writes whatever `CommitRequest.Welcomes` names, so without this clause any
// enrolled committer can queue an opaque blob to an arbitrary device id at that group's epoch: the
// recipient cannot open it, but it learns the group_id and epoch of a group it was never added to
// and its Welcome queue is filled for the 30-day retention — and `mls_welcomes` being unique only
// on (device_id, blob_sha256) means varying the blob defeats the dedupe. The material for the
// check is already in hand: invariant 4's Add clause decodes exactly these credentials.
func TestACommitMayOnlyAddressAWelcomeToADeviceItAdds(t *testing.T) {
	joiner := id.New()
	stranger := id.New()
	applied := []mlswasi.AppliedProposal{
		{Kind: mlswasi.ProposalAdd, CredentialIdentity: credentialIdentity(t, id.New(), joiner)},
		{Kind: mlswasi.ProposalRemove},
	}

	if err := ds.CheckAddressedWelcomesForTest(applied,
		[]ds.WelcomeFor{{DeviceID: joiner, Blob: welcomeBlob(0x4B, 96)}}); err != nil {
		t.Fatalf("the Welcome of a device this commit adds must be accepted: %v", err)
	}

	var dsErr *ds.Error
	err := ds.CheckAddressedWelcomesForTest(applied, []ds.WelcomeFor{
		{DeviceID: joiner, Blob: welcomeBlob(0x4B, 96)},
		{DeviceID: stranger, Blob: welcomeBlob(0x4C, 96)},
	})
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" {
		t.Fatalf("got %v, want E_COMMIT_INVALID for a Welcome addressed outside the commit's Adds", err)
	}

	// A commit that adds nobody carries no Welcome at all.
	err = ds.CheckAddressedWelcomesForTest(nil, []ds.WelcomeFor{{DeviceID: joiner}})
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" {
		t.Fatalf("got %v, want E_COMMIT_INVALID for a Welcome on a commit with no Adds", err)
	}

	// And the ordinary commit — no Adds, no Welcomes — is untouched.
	if err := ds.CheckAddressedWelcomesForTest(nil, nil); err != nil {
		t.Fatalf("a commit with no Welcomes must be accepted: %v", err)
	}
}

// credentialIdentity is the ten-element `dilla_identity` array with the user at element 2 and the
// device at element 3, which is the layout `protocol/vectors/identity.json` pins and
// `decodeCredentialIdentity` reads (state_test.go's TestCredentialIdentityDecodesTheCommittedVector
// holds the decoder to that vector). It is built here because no fixture can mint an Add for a
// device this test invents.
func credentialIdentity(t *testing.T, userID, deviceID id.ID) []byte {
	t.Helper()
	b, err := cborx.Marshal([]any{
		uint64(1), uint64(0), userID, deviceID,
		uint64(0), uint64(0), uint64(0), uint64(0), uint64(0), uint64(0),
	})
	if err != nil {
		t.Fatalf("encode a credential identity: %v", err)
	}
	return b
}

// Web-1 card 15 (Q25): the frame budget is judged on the ENCODED mls.welcome frame — the
// four-element [op, n, group_id, payload] the gateway writes — not on the bare payload. A payload
// 10 bytes under the budget still makes a frame over it (the frame adds up to 28 bytes: the array
// header, the op, n at its widest and the 16-byte group id), so that joiner is left to the queue;
// a payload 200 bytes under is framed. Attacker statement: only the Welcome's own size triggers
// the skip, it spares the joiner a frame its read limit would close the connection on, and the
// Welcome stays queued for GET /v1/welcomes either way.
func TestTheWelcomeFrameBudgetIsJudgedOnTheEncodedFrame(t *testing.T) {
	const budget = 4096
	h := newDSHarnessWithFrameBudget(t, budget)
	ctx := context.Background()
	if got := h.gw.MaxFrameBytes(); got != budget {
		t.Fatalf("the harness gateway's budget is %d, want %d", got, budget)
	}
	over, under := h.device(t), h.device(t)
	if h.sessions == nil {
		h.sessions = map[id.ID]auth.Session{}
	}
	for _, j := range []id.ID{over, under} {
		h.sessions[j] = h.sessionOf(t, j)
	}
	h.online(over, under)

	groupID := id.New()
	const epoch = 3
	tree := welcomeBlob(0xEE, 64)
	treeHash := bytes.Repeat([]byte{0xAB}, 32)
	frameOf := func(payload []byte) []byte {
		t.Helper()
		f, err := gateway.Encode(gateway.Frame{Op: gateway.OpMLSWelcome, GroupID: &groupID, Payload: payload, Replay: true}, math.MaxUint64)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		return f
	}
	// sized stores joiner's Welcome row with a blob whose payload — as the fan-out builds it from
	// the row: [welcome_id, epoch, commit_seq 1, blob, tree, tree_hash] — is exactly target bytes,
	// and proves the size against the row's real welcome_id once the row exists.
	sized := func(joiner id.ID, tag byte, target int) []byte {
		t.Helper()
		var blob []byte
		for n := 256; n < target && blob == nil; n++ {
			p, err := gateway.WelcomePayload(1, epoch, 1, welcomeBlob(tag, n), tree, treeHash)
			if err != nil {
				t.Fatalf("WelcomePayload: %v", err)
			}
			if len(p) == target {
				blob = welcomeBlob(tag, n)
			}
		}
		if blob == nil {
			t.Fatalf("no blob length gives a %d-byte payload", target)
		}
		h.putWelcomeRow(t, joiner, groupID, epoch, blob)
		p, err := gateway.WelcomePayload(uint64(h.welcomeIDOf(t, joiner, groupID)), epoch, 1, blob, tree, treeHash)
		if err != nil || len(p) != target {
			t.Fatalf("the stored row's payload is %d bytes (err %v), want %d", len(p), err, target)
		}
		return blob
	}
	overBlob := sized(over, 0x4C, budget-10)
	underBlob := sized(under, 0x4D, budget-200)

	// The premise, so the test cannot pass by accident: the first payload fits and its frame does
	// not; the second frame fits.
	overPayload, err := gateway.WelcomePayload(uint64(h.welcomeIDOf(t, over, groupID)), epoch, 1, overBlob, tree, treeHash)
	if err != nil {
		t.Fatalf("WelcomePayload: %v", err)
	}
	if len(overPayload) > budget || len(frameOf(overPayload)) <= budget {
		t.Fatalf("payload %d bytes, frame %d bytes: the test needs a payload within %d whose frame is over it",
			len(overPayload), len(frameOf(overPayload)), budget)
	}
	underPayload, err := gateway.WelcomePayload(uint64(h.welcomeIDOf(t, under, groupID)), epoch, 1, underBlob, tree, treeHash)
	if err != nil {
		t.Fatalf("WelcomePayload: %v", err)
	}
	if len(frameOf(underPayload)) > budget {
		t.Fatalf("the framed case's frame is %d bytes, over %d", len(frameOf(underPayload)), budget)
	}

	ds.FanOutWelcomesForTest(h.ds, ctx, groupID, epoch, tree, treeHash,
		[]ds.WelcomeFor{{DeviceID: over, Blob: overBlob}, {DeviceID: under, Blob: underBlob}})

	f := h.waitDeviceFrame(t, under, "mls.welcome")
	var got []byte
	if len(f.payload) != 6 {
		t.Fatalf("the framed welcome has %d elements, want 6", len(f.payload))
	}
	if err := cborx.Unmarshal(f.payload[3], &got); err != nil || !bytes.Equal(got, underBlob) {
		t.Fatalf("the framed welcome carries a %d-byte blob (err %v), want the 200-under joiner's", len(got), err)
	}
	// The mutation "compare len(payload)" sends this one: its payload is within the budget.
	h.expectNoWelcomeFrame(t, over)
	// Still queued for GET /v1/welcomes.
	if h.welcomeIDOf(t, over, groupID) == 0 {
		t.Fatal("the unframed Welcome left the queue")
	}
}

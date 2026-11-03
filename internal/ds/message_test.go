package ds_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

// Invariant 8: the DS reads C from private_message.authenticated_data and refuses an upload whose
// authenticated_data is not exactly 32 bytes.
func TestAnUploadWithTheWrongCommitmentLengthIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	for _, n := range []int{0, 31, 33} {
		_, err := h.ds.Upload(context.Background(), g.session, g.id, g.Epoch(), h.messageWithAAD(t, g, n), 0)
		var dsErr *ds.Error
		if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMITMENT_INVALID" {
			t.Fatalf("%d-byte authenticated_data: got %v, want E_COMMITMENT_INVALID", n, err)
		}
		if dsErr.Status != 422 {
			t.Errorf("status = %d, want 422", dsErr.Status)
		}
	}
	if _, err := h.ds.Upload(context.Background(), g.session, g.id, g.Epoch(), h.messageWithAAD(t, g, 32), 0); err != nil {
		t.Fatalf("32 bytes must be accepted: %v", err)
	}
}

// facts-ds-contract.md §1.2 fixes endpoint 7's parse: "strip u16 version + u16 wire format
// (must be 2), then read opaque<V> group_id, uint64 epoch, u8 content_type (MUST BE 1), then
// opaque<V> authenticated_data". The guest reports the byte; only the delivery service can refuse
// on it.
//
// It is not a formality. The instance is the SOLE SEQUENCER for handshakes (invariants 1-5): a
// Commit or a Proposal accepted here would reach every member as `message.ct`, carrying a seq out
// of the group's one shared seq space with no `mls_handshakes` row behind it — precisely the fork
// invariant 9 exists to detect.
func TestOnlyAnApplicationMessageMayBeUploaded(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	// 2 is proposal, 3 is commit. Both are well formed in every other respect: the group's own
	// epoch, a 32-byte commitment, an ordinary ciphertext.
	for _, ct := range []byte{2, 3} {
		_, err := h.ds.Upload(context.Background(), g.session, g.id, g.Epoch(), h.messageOfContentType(t, g, ct), 0)
		var dsErr *ds.Error
		if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" {
			t.Fatalf("content_type %d: got %v, want E_COMMIT_INVALID", ct, err)
		}
		if dsErr.Rule != "content_type" {
			t.Errorf("content_type %d: rule = %q, want content_type", ct, dsErr.Rule)
		}
	}
	// Nothing was sequenced, stored or franked by either refusal.
	rows, err := h.ds.Messages(context.Background(), g.id, g.session, 0, 10)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a refused handshake left %d rows in mls_app_messages", len(rows))
	}
	if _, err := h.ds.Upload(context.Background(), g.session, g.id, g.Epoch(), h.messageOfContentType(t, g, 1), 0); err != nil {
		t.Fatalf("content_type 1 must be accepted: %v", err)
	}
}

func TestAnUploadFromADeviceWhoseLeafIsGoneIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	h.removeLeafOfDevice(t, g, g.device)
	_, err := h.ds.Upload(context.Background(), g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_LEAF_NOT_CURRENT" {
		t.Fatalf("got %v, want E_LEAF_NOT_CURRENT", err)
	}
}

// R6/R32/D7: the cap is 131072 bytes of MLS ciphertext, exactly.
func TestTheCiphertextCapIsExactlyOneHundredAndThirtyOneThousandAndSeventyTwoBytes(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	if _, err := h.ds.Upload(context.Background(), g.session, g.id, g.Epoch(), h.messageOfSize(t, g, 131072), 0); err != nil {
		t.Fatalf("131072 bytes must be accepted: %v", err)
	}
	_, err := h.ds.Upload(context.Background(), g.session, g.id, g.Epoch(), h.messageOfSize(t, g, 131073), 0)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_TOO_LARGE" {
		t.Fatalf("131073 bytes: got %v, want E_TOO_LARGE", err)
	}
	if dsErr.Status != 413 {
		t.Errorf("status = %d, want 413", dsErr.Status)
	}
}

// R30: message.ct reaches the uploader too, so the per-group seq stream is dense on every device,
// and the response carries seq.
func TestMessageCTReachesTheUploaderAndTheResponseCarriesSeq(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	h.online(g.device)
	out, err := h.ds.Upload(context.Background(), g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if out.Seq == 0 {
		t.Fatal("the upload response must carry seq")
	}
	if len(out.FrankingTag) != 32 {
		t.Fatalf("franking tag is %d bytes, want 32", len(out.FrankingTag))
	}
	h.expectDeviceFrames(t, g.device, "message.ct")
}

// Plan 2 task 17 (P2-D21): the stored row names the franking key its tag was made under, and the
// stored tuple recomputes to the stored tag under that key, which is exactly what a report checks.
func TestAnUploadRecordsTheFrankingKeyItWasTaggedUnder(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	out, err := h.ds.Upload(context.Background(), g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	row, err := h.repo.GetAppMessage(context.Background(), g.id, out.Seq)
	if err != nil {
		t.Fatalf("GetAppMessage: %v", err)
	}
	keys := testInstanceKeys(t)
	if row.FrankingKeyID != keys.FrankingKeyID {
		t.Fatalf("franking_key_id = %v, want the instance's current key %v", row.FrankingKeyID, keys.FrankingKeyID)
	}
	want := ds.FrankingTag(keys.FrankingKey, g.id, row.Epoch, row.Seq, row.UploaderDevice, row.CommitmentC, uint64(row.Created))
	if !bytes.Equal(row.FrankingTag, want) || !bytes.Equal(out.FrankingTag, want) {
		t.Fatal("the stored tuple does not recompute to the stored tag under the recorded key")
	}
}

// addMemberLeaf gives userID a new device with a current leaf in g, as a merged Add leaves it in
// SQL (the leaf index after the highest one), writes the device's account rows, and returns the
// device's session.
func (h *dsHarness) addMemberLeaf(t *testing.T, g *dsMessageGroup, userID id.ID) auth.Session {
	t.Helper()
	ctx := context.Background()
	device := id.New()
	h.account(t, userID, device)
	members, err := h.repo.ListMembers(ctx, g.id)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	var next uint32
	for _, m := range members {
		if m.LeafIndex >= next {
			next = m.LeafIndex + 1
		}
	}
	members = append(members, store.MemberRow{
		GroupID: g.id, LeafIndex: next, UserID: userID, DeviceID: device,
		SignatureKey: bytes.Repeat([]byte{0x5c}, 32), AddedEpoch: g.epoch,
	})
	if err := h.repo.Tx(ctx, func(tx store.Repository) error {
		return tx.ReplaceMembers(ctx, g.id, g.epoch, members)
	}); err != nil {
		t.Fatalf("ReplaceMembers: %v", err)
	}
	return auth.Session{UserID: userID, DeviceID: device, Scope: auth.ScopeEnrolled}
}

// R29 and L-HTTP-80: only a current member may delete — anyone else is E_NOT_FOUND, the answer of
// every member-only group route — then only the uploading user, from any of their member devices;
// a tombstone keeps everything but the ciphertext.
func TestDeleteIsMemberOnlyThenUploaderOnlyAndKeepsTheTombstoneFields(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ctx := context.Background()
	out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	var dsErr *ds.Error

	stranger := h.sessionOfAnotherUser(t, g)
	err = h.ds.DeleteMessage(ctx, stranger, g.id, out.Seq)
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" || dsErr.Status != 404 {
		t.Fatalf("delete by a device outside the group: got %v, want 404 E_NOT_FOUND", err)
	}
	outsideSibling := h.otherDeviceOfSameUser(t, g)
	err = h.ds.DeleteMessage(ctx, outsideSibling, g.id, out.Seq)
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
		t.Fatalf("delete by the uploader's device outside the group: got %v, want E_NOT_FOUND", err)
	}
	if err := h.ds.DeleteMessage(ctx, g.session, id.New(), out.Seq); !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
		t.Fatalf("delete in an unknown group: got %v, want E_NOT_FOUND", err)
	}

	otherMember := h.addMemberLeaf(t, g, id.New())
	err = h.ds.DeleteMessage(ctx, otherMember, g.id, out.Seq)
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_UPLOADER" || dsErr.Status != 403 {
		t.Fatalf("delete by a member who is another user: got %v, want 403 E_NOT_UPLOADER", err)
	}

	sibling := h.addMemberLeaf(t, g, g.session.UserID)
	if err := h.ds.DeleteMessage(ctx, sibling, g.id, out.Seq); err != nil {
		t.Fatalf("any member device of the uploading user may delete: %v", err)
	}

	rows, err := h.ds.Messages(ctx, g.id, g.session, 0, 10)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("catch-up returned %d rows, want 1 tombstone", len(rows))
	}
	row := rows[0]
	if row.Blob != nil {
		t.Error("a tombstone keeps no ciphertext")
	}
	if row.DeletedAt == nil {
		t.Error("a tombstone must carry deleted_at")
	}
	if row.Seq != out.Seq || row.Epoch == 0 || len(row.FrankingTag) != 32 || len(row.CommitmentC) != 32 {
		t.Error("a tombstone keeps seq, epoch, uploader_device, commitment_c, franking_tag and recv_ts")
	}
}

func TestDeleteMessagePreservesTargetRefusalsBeforeCapabilityGate(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ctx := context.Background()
	out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	h.repo.markInteraction(g.id)
	otherMember := h.addMemberLeaf(t, g, id.New())

	for _, wireVersion := range []uint64{0, 2} {
		t.Run(fmt.Sprintf("wire_%d", wireVersion), func(t *testing.T) {
			session := g.session
			session.WireVersion = wireVersion
			var dsErr *ds.Error
			err := h.ds.DeleteMessage(ctx, session, g.id, out.Seq+1)
			if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
				t.Fatalf("delete nonexistent message = %v, want E_NOT_FOUND", err)
			}
			unowned := otherMember
			unowned.WireVersion = wireVersion
			err = h.ds.DeleteMessage(ctx, unowned, g.id, out.Seq)
			if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_UPLOADER" {
				t.Fatalf("delete another user's message = %v, want E_NOT_UPLOADER", err)
			}
			err = h.ds.DeleteMessage(ctx, session, g.id, out.Seq)
			var upgrade *capability.UpgradeRequired
			if !errors.As(err, &upgrade) {
				t.Fatalf("delete authorized target = %v, want typed upgrade refusal", err)
			}
			row, getErr := h.repo.GetAppMessage(ctx, g.id, out.Seq)
			if getErr != nil || row.DeletedAt != nil || row.Blob == nil {
				t.Fatalf("wire %d refusal mutated the target message: %+v, %v", wireVersion, row, getErr)
			}
		})
	}

	wireThree := g.session
	wireThree.WireVersion = 3
	if err := h.ds.DeleteMessage(ctx, wireThree, g.id, out.Seq); err != nil {
		t.Fatalf("wire 3 delete: %v", err)
	}
	row, err := h.repo.GetAppMessage(ctx, g.id, out.Seq)
	if err != nil || row.DeletedAt == nil || row.Blob != nil {
		t.Fatalf("wire 3 did not tombstone the target message: %+v, %v", row, err)
	}
}

// L-HTTP-80's attacker statement from the honest side: a device removed from the group cannot
// delete even its own message, and the message stays as it was.
func TestARemovedDeviceCannotDeleteItsOwnMessage(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ctx := context.Background()
	out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	h.removeLeafOfDevice(t, g, g.device)
	err = h.ds.DeleteMessage(ctx, g.session, g.id, out.Seq)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" || dsErr.Status != 404 {
		t.Fatalf("a removed device's delete: got %v, want 404 E_NOT_FOUND", err)
	}
	row, err := h.repo.GetAppMessage(ctx, g.id, out.Seq)
	if err != nil {
		t.Fatalf("GetAppMessage: %v", err)
	}
	if row.Blob == nil || row.DeletedAt != nil {
		t.Fatal("a refused delete tombstoned the message")
	}
}

// interfaces.md §5.1 row 12 lists E_PRUNED as GET /v1/groups/{id}/messages' error, and the sibling
// handshake path already enforces it. Without it a client whose cursor is below the floor gets a
// silently short list starting at the floor and cannot tell "these are gone, resync" from
// "nothing new" — the precise failure the 410 exists to prevent.
func TestACatchUpBelowTheMessageRetentionFloorIsPruned(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ctx := context.Background()
	first, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	// Thirty-one days pass between the two uploads, so exactly one of them is older than
	// MessageRetention when the sweep runs.
	h.clk.Advance(31 * 24 * time.Hour)
	second, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	rows, err := h.ds.Messages(ctx, g.id, g.session, 0, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("before the sweep: %d rows, %v — want both messages", len(rows), err)
	}

	// The sweep task 26 owns, with its three retention parameters (D14): no eligible cursor has
	// acknowledged anything, so only the DELIVERY floor — now - MessageRetention — deletes, and
	// it takes exactly the message below it.
	n, err := h.repo.PruneAppMessages(ctx, g.id, 0,
		h.clk.Now().Add(-ds.DefaultPolicy().MessageRetention).Unix(), h.clk.Now().Unix())
	if err != nil {
		t.Fatalf("PruneAppMessages: %v", err)
	}
	if n != 1 {
		t.Fatalf("the sweep deleted %d rows, want the one at seq %d", n, first.Seq)
	}

	_, err = h.ds.Messages(ctx, g.id, g.session, 0, 10)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_PRUNED" {
		t.Fatalf("a cursor below the floor: got %v, want E_PRUNED", err)
	}
	if dsErr.Status != 410 {
		t.Errorf("status = %d, want 410", dsErr.Status)
	}

	// `from` is the first seq wanted, so from = the deleted seq itself still wants it: a hole.
	// Until the final review this was served — the one-seq hole of the old `from+1 < floor` guard.
	if _, err := h.ds.Messages(ctx, g.id, g.session, first.Seq, 10); !errors.As(err, &dsErr) ||
		dsErr.Code != "E_PRUNED" {
		t.Fatalf("from the deleted seq %d: got %v, want E_PRUNED", first.Seq, err)
	}
	// One past what was deleted has no hole and is served.
	rows, err = h.ds.Messages(ctx, g.id, g.session, first.Seq+1, 10)
	if err != nil {
		t.Fatalf("a catch-up past the deleted seq must be served: %v", err)
	}
	if len(rows) != 1 || rows[0].Seq != second.Seq {
		t.Fatalf("got %d rows, want the surviving message at seq %d", len(rows), second.Seq)
	}
}

// The other half of the same predicate, and the reason it is not the floor alone: `from` is a
// cursor in the ONE seq space handshakes and application messages share, so a group whose early
// seqs carry handshakes is below its message floor with nothing ever deleted. protocol/02's error
// table makes E_PRUNED mean "resync by external commit", so refusing that would send a healthy
// member through a full rejoin.
func TestAGroupWhoseEarlySeqsAreNotMessagesIsServed(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ctx := context.Background()
	// Burn seq 1 the way a handshake does: the seq space is the group's, not the table's.
	if err := h.repo.Tx(ctx, func(tx store.Repository) error {
		_, err := tx.NextSeq(ctx, g.id)
		return err
	}); err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if out.Seq != 2 {
		t.Fatalf("the message took seq %d, want 2", out.Seq)
	}
	rows, err := h.ds.Messages(ctx, g.id, g.session, 0, 10)
	if err != nil {
		t.Fatalf("a group younger than MessageRetention has lost nothing: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want the one message", len(rows))
	}
}

// A cursor is a group-scoped write, so it carries the same authorisation as every group-scoped
// read: a device with no leaf in the group is E_NOT_FOUND, never E_FORBIDDEN and never 204.
//
// Two things ride on it. `device_cursors` feeds `store.Cursors.MinCursor`, which task 26 makes the
// retention floor, so a stranger's row would let any enrolled device that knows a group id pin
// that group's messages at floor 0 forever; and answering 204 where a member gets 204 but an
// unknown group gets 404 is the group-existence oracle requireMember's own comment refuses to
// give.
func TestACursorFromANonMemberIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	stranger := h.sessionOfAnotherUser(t, g)

	err := h.ds.AdvanceCursor(context.Background(), stranger, g.id, 0, g.Epoch())
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
		t.Fatalf("a non-member's cursor: got %v, want E_NOT_FOUND", err)
	}
	if dsErr.Status != 404 {
		t.Errorf("status = %d, want 404", dsErr.Status)
	}
	if got := h.cursorOf(t, stranger.DeviceID, g.id); got != 0 {
		t.Fatalf("a non-member wrote a cursor: last_seq = %d", got)
	}
}

// The floor task 26 builds on MinCursor must only ever move forward, and an acknowledgement can
// only name something the instance actually sequenced.
func TestACursorNeverPassesTheHighWaterAndNeverRewinds(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ctx := context.Background()
	out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	// Above the group's high-water there is nothing to acknowledge.
	err = h.ds.AdvanceCursor(ctx, g.session, g.id, out.Seq+1, out.Epoch)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_INVALID_REQUEST" {
		t.Fatalf("a cursor above the high-water: got %v, want E_INVALID_REQUEST", err)
	}
	if got := h.cursorOf(t, g.device, g.id); got != 0 {
		t.Fatalf("the refused cursor was written anyway: last_seq = %d", got)
	}

	if err := h.ds.AdvanceCursor(ctx, g.session, g.id, out.Seq, out.Epoch); err != nil {
		t.Fatalf("AdvanceCursor: %v", err)
	}
	// A rewind is idempotent, not an error — a retried or reordered acknowledgement is ordinary —
	// but it must not move the retention floor backwards.
	if err := h.ds.AdvanceCursor(ctx, g.session, g.id, 0, out.Epoch); err != nil {
		t.Fatalf("a rewind must be accepted and ignored, not refused: %v", err)
	}
	if got := h.cursorOf(t, g.device, g.id); got != out.Seq {
		t.Fatalf("cursor = %d after a rewind, want %d", got, out.Seq)
	}
}

// A cursor advances only on the explicit POST /cursor, never on fan-out.
func TestACursorAdvancesOnlyOnAnExplicitAcknowledgement(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	h.online(g.device)
	out, err := h.ds.Upload(context.Background(), g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := h.cursorOf(t, g.device, g.id); got != 0 {
		t.Fatalf("cursor = %d after fan-out, want 0 — a frame on a writer queue is not a delivery", got)
	}
	// Epoch is a field on UploadResult, not a method.
	if err := h.ds.AdvanceCursor(context.Background(), g.session, g.id, out.Seq, out.Epoch); err != nil {
		t.Fatalf("AdvanceCursor: %v", err)
	}
	if got := h.cursorOf(t, g.device, g.id); got != out.Seq {
		t.Fatalf("cursor = %d, want %d", got, out.Seq)
	}
}

// recordingPostPolicy is ds.PostPolicy as the delivery service sees it: every question it is asked,
// and one answer for all of them (nil until a test sets err).
type recordingPostPolicy struct {
	mu    sync.Mutex
	calls []postPolicyCall
	err   error
}

type postPolicyCall struct {
	group store.GroupRow
	user  id.ID
	class uint8
	now   int64
}

func (p *recordingPostPolicy) Check(_ context.Context, g store.GroupRow, userID id.ID, class uint8, now int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, postPolicyCall{group: g, user: userID, class: class, now: now})
	return p.err
}

func (p *recordingPostPolicy) take() []postPolicyCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.calls
	p.calls = nil
	return out
}

// web3.7 SB1 (R8 amended): Upload asks the post policy about every upload, class 0 and class 1,
// with the group row, the session's user and the class, and a refusal is the answer unchanged:
// nothing is stored, nothing is delivered. (web3.8 S1: every upload names its class.)
func TestUploadAsksThePostPolicyAndItsRefusalIsTheAnswer(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ctx := context.Background()

	out, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload(class 0): %v", err)
	}
	calls := h.postPolicy.take()
	if len(calls) != 1 || calls[0].group.GroupID != g.id || calls[0].user != g.session.UserID ||
		calls[0].class != 0 || calls[0].now != h.clk.Now().Unix() {
		t.Fatalf("a class-0 upload asked %+v, want one call with the group, the user, class 0 and now", calls)
	}
	row, err := h.repo.GetAppMessage(ctx, g.id, out.Seq)
	if err != nil || row.Class == nil || *row.Class != 0 {
		t.Fatalf("the stored row's class = %v, %v; want 0", row.Class, err)
	}

	fold, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 1)
	if err != nil {
		t.Fatalf("Upload(class 1): %v", err)
	}
	if calls := h.postPolicy.take(); len(calls) != 1 || calls[0].class != 1 || calls[0].user != g.session.UserID {
		t.Fatalf("a class-1 upload asked %+v, want one call with class 1", calls)
	}
	if row, err := h.repo.GetAppMessage(ctx, g.id, fold.Seq); err != nil || row.Class == nil || *row.Class != 1 {
		t.Fatalf("the class-1 row's class = %v, %v; want 1", row.Class, err)
	}

	before, err := h.repo.GetGroup(ctx, g.id)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	h.online(g.device)
	refusal := server.Errorf(server.CodeForbidden, "missing permission")
	h.postPolicy.err = refusal
	if _, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0); err != refusal { //nolint:errorlint // identity, not errors.Is: the policy's error comes back unchanged, not wrapped
		t.Fatalf("Upload under a refusing policy = %v, want the policy's error unchanged", err)
	}
	if calls := h.postPolicy.take(); len(calls) != 1 {
		t.Fatalf("the refused upload asked %d times, want 1", len(calls))
	}
	after, err := h.repo.GetGroup(ctx, g.id)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if after.Seq != before.Seq {
		t.Fatalf("a refused upload moved the group's seq from %d to %d", before.Seq, after.Seq)
	}
	if _, err := h.repo.GetAppMessage(ctx, g.id, before.Seq+1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused upload stored a row: %v", err)
	}
	// Nothing was delivered: the next message.ct the device sees is the one accepted after the
	// policy relents, at the next seq.
	h.postPolicy.err = nil
	next, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0)
	if err != nil {
		t.Fatalf("Upload after the refusal: %v", err)
	}
	if next.Seq != before.Seq+1 {
		t.Fatalf("the next upload's seq = %d, want %d", next.Seq, before.Seq+1)
	}
	f := h.waitDeviceFrame(t, g.device, "message.ct")
	var seq uint64
	if len(f.payload) != 7 || cborx.Unmarshal(f.payload[0], &seq) != nil || seq != next.Seq ||
		!bytes.Equal(f.payload[6], []byte{0x00}) {
		t.Fatalf("the first message.ct after the refusal = %x, want seq %d with class 0 as its 7th element", f.payload, next.Seq)
	}
}

// S1 fix round 1 (m4): Upload range-checks the class itself, not only the route: 2 and 255 are
// 400 E_INVALID_REQUEST before the policy is asked or anything is stored. The control is class 1
// into the same group, which is accepted.
func TestUploadRefusesAClassOutsideZeroAndOne(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ctx := context.Background()
	before, err := h.repo.GetGroup(ctx, g.id)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	for _, class := range []uint8{2, 255} {
		_, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), class)
		var dsErr *ds.Error
		if !errors.As(err, &dsErr) || dsErr.Code != "E_INVALID_REQUEST" || dsErr.Status != 400 {
			t.Fatalf("an upload of class %d = %v, want 400 E_INVALID_REQUEST", class, err)
		}
	}
	if calls := h.postPolicy.take(); len(calls) != 0 {
		t.Fatalf("an out-of-range class reached the post policy %d times", len(calls))
	}
	if after, err := h.repo.GetGroup(ctx, g.id); err != nil || after.Seq != before.Seq {
		t.Fatalf("a refused class moved the group's seq from %d to %d (%v)", before.Seq, after.Seq, err)
	}
	// Control: class 1.
	if _, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 1); err != nil {
		t.Fatalf("control: class 1: %v", err)
	}
}

// SB1 fix round 1 (review): a closed group takes no upload, whatever its class. ds.Close closes a
// deleted channel's group and one a restore replaced; before the fix an upload into it was still
// sequenced and fanned out. 404 E_NOT_FOUND, as for a group that does not exist, and the policy is
// never asked. Control: the same upload before the close is accepted.
func TestAnUploadIntoAClosedGroupIsRefused(t *testing.T) {
	h := newDSHarness(t)
	g := h.group(t)
	ctx := context.Background()
	if _, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), 0); err != nil {
		t.Fatalf("control: an upload into the open group: %v", err)
	}
	if err := h.ds.Close(ctx, g.id); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h.postPolicy.take()
	before, err := h.repo.GetGroup(ctx, g.id)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	for _, class := range []uint8{0, 1} {
		_, err := h.ds.Upload(ctx, g.session, g.id, g.Epoch(), h.message(t, g, g.Epoch()), class)
		var dsErr *ds.Error
		if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" || dsErr.Status != 404 {
			t.Fatalf("an upload (class %v) into a closed group = %v, want 404 E_NOT_FOUND", class, err)
		}
	}
	after, err := h.repo.GetGroup(ctx, g.id)
	if err != nil || after.Seq != before.Seq {
		t.Fatalf("a refused upload into a closed group moved its seq from %d to %d (%v)", before.Seq, after.Seq, err)
	}
	if calls := h.postPolicy.take(); len(calls) != 0 {
		t.Fatalf("the post policy was asked %d times about a closed group", len(calls))
	}
}

package ds

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// contentTypeApplication is RFC 9420's ContentType `application`, the only one endpoint 7
// accepts. `proposal` (2) and `commit` (3) are handshakes and reach the instance through
// endpoints 5 and 6, which sequence them into `mls_handshakes`.
const contentTypeApplication = 1

// UploadResult carries Epoch as well as the endpoint's three response elements. The endpoint's
// body is [seq, franking_tag, recv_ts] (§5.1 row 7) and does NOT carry the epoch; the Go value
// does, because every in-process caller — AdvanceCursor, the retention tests, the testkit host —
// needs the epoch the message was accepted at. Recorded as deviation B14 against §6.2.
type UploadResult struct {
	Seq         uint64
	Epoch       uint64
	FrankingTag []byte
	RecvTS      uint64
}

// Upload is invariant 8. The order is fixed: size, then a closed group, then the freeze, then the
// current-leaf check, then the post policy, then the commitment, then the tag.
//
// class is the upload class (web3.7 SB1, protocol/02 § Upload class): 0 a post, 1 anything else.
// Every upload names one (web3.8 S1: the two-element legacy body is refused at the route). It is
// stored on the row and relayed as message.ct's 7th element; the post policy, when one is set,
// decides every upload by it.
func (d *DS) Upload(ctx context.Context, s Session, groupID id.ID, epoch uint64, pm []byte, class uint8) (UploadResult, error) {
	// The class set is 0 and 1 (protocol/02 § Upload class). The route refuses anything else at
	// decode; this keeps the service from relying on it (S1 fix round 1, m4), with the route's text.
	if class > 1 {
		return UploadResult{}, errInvalid("class must be 0 or 1")
	}
	if len(pm) > d.opts.Policy.MaxCiphertextBytes {
		return UploadResult{}, errTooLarge(len(pm), d.opts.Policy.MaxCiphertextBytes)
	}
	unlock := d.lock(groupID)
	defer unlock()
	initialGroup, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return UploadResult{}, errNotFound("group")
	}
	if err != nil {
		return UploadResult{}, err
	}
	inst, err := d.opts.Wasm.Acquire(ctx)
	if err != nil {
		return UploadResult{}, err
	}
	defer inst.Release()

	var row store.GroupRow
	var out UploadResult
	err = d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		// The community row serializes permission and managed-installation changes;
		// the MLS row then serializes epoch, membership, freeze and sequence state.
		if initialGroup.CommunityID != nil {
			if err := tx.LockCommunity(ctx, *initialGroup.CommunityID); err != nil {
				return err
			}
		}
		if err := tx.LockGroup(ctx, groupID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errNotFound("group")
			}
			return err
		}
		var err error
		row, err = tx.GetGroup(ctx, groupID)
		if errors.Is(err, store.ErrNotFound) {
			return errNotFound("group")
		}
		if err != nil {
			return err
		}
		if !equalNullableID(initialGroup.CommunityID, row.CommunityID) {
			return botSendConflict("the group's community scope changed during upload")
		}
		if row.ClosedAt != nil {
			return errNotFound("group")
		}
		refs, err := tx.ListProposals(ctx, groupID, row.Epoch, false)
		if err != nil {
			return err
		}
		freezeRefs := make([][]byte, 0, len(refs))
		for _, proposal := range refs {
			if proposal.Origin == 0 && proposal.VoidAt == nil {
				freezeRefs = append(freezeRefs, proposal.Ref)
			}
		}
		if len(freezeRefs) > 0 {
			return errCommitRequired(freezeRefs, uint64(d.opts.Policy.CommitDeadline.Milliseconds())) //nolint:gosec // G115: a positive configured duration
		}
		members, err := tx.ListMembers(ctx, groupID)
		if err != nil {
			return err
		}
		currentLeaf := false
		for _, member := range members {
			if member.DeviceID == s.DeviceID && member.RemovedEpoch == nil {
				currentLeaf = true
				break
			}
		}
		if !currentLeaf {
			return errLeafNotCurrent()
		}
		if err := d.requireGroupCapabilityTx(ctx, tx, s, row); err != nil {
			return err
		}
		recvAt := d.now()
		if policy, ok := d.opts.PostPolicy.(TransactionalPostPolicy); ok {
			if err := policy.CheckTx(ctx, tx, row, s.UserID, class, recvAt); err != nil {
				return err
			}
		} else if d.opts.PostPolicy != nil {
			// Compatibility for test and embedding policies predating CheckTx. The
			// production api.PostPolicy implements the transaction-scoped interface.
			if err := d.opts.PostPolicy.Check(ctx, row, s.UserID, class, recvAt); err != nil {
				return err
			}
		}
		// Keep the established policy-before-ciphertext refusal order while the
		// group row and permission state remain fenced.
		meta, err := inst.PrivateMessageAAD(ctx, pm)
		if err != nil {
			return errCommitmentInvalid(0)
		}
		if len(meta.AuthenticatedData) != 32 {
			return errCommitmentInvalid(len(meta.AuthenticatedData))
		}
		if meta.ContentType != contentTypeApplication {
			return errCommitInvalid("content_type", "only application messages may be uploaded here")
		}
		if meta.Epoch != row.Epoch || meta.Epoch != epoch {
			return errCommitInvalid("epoch", "the message's epoch is not the group's")
		}
		recvTS := uint64(recvAt) //nolint:gosec // G115: a server unix timestamp
		seq, err := nextSeq(ctx, tx, groupID)
		if err != nil {
			return err
		}
		tag := FrankingTag(d.opts.Keys.FrankingKey, groupID, row.Epoch, seq, s.DeviceID, meta.AuthenticatedData, recvTS)
		if err := tx.PutAppMessage(ctx, store.AppMessageRow{
			GroupID:        groupID,
			Seq:            seq,
			Epoch:          row.Epoch,
			UploaderDevice: s.DeviceID,
			Blob:           pm,
			CommitmentC:    meta.AuthenticatedData,
			FrankingTag:    tag,
			Size:           uint64(len(pm)),
			Created:        int64(recvTS), //nolint:gosec // G115: a unix second, far below 2^63
			// Archival retention: NULL means retained. The community policy fills it in Plan 2;
			// delivery retention is the sweeper's business, not this column's.
			Expires: nil,
			// The key the tag was made under (Plan 2 task 17, P2-D21): a report selects it by
			// this id, so a message stays verifiable after the franking key rotates.
			FrankingKeyID: d.opts.Keys.FrankingKeyID,
			Class:         &class,
		}); err != nil {
			return err
		}
		// Slow mode's durable mark (SB1 fix round 1): an accepted post into a community text group
		// raises the user's mark in the channel, in this transaction. Retention prunes the row
		// above; it never touches the mark, so a pruned post still holds the wait.
		if class == 0 && row.Kind == groupKindText && row.CommunityID != nil {
			if err := tx.PutSlowmodeMark(ctx, row.TargetID, s.UserID, int64(recvTS)); err != nil { //nolint:gosec // G115: a unix second, far below 2^63
				return err
			}
		}
		out = UploadResult{Seq: seq, Epoch: row.Epoch, FrankingTag: tag, RecvTS: recvTS}
		return nil
	})
	if err != nil {
		return UploadResult{}, err
	}

	// R30: the uploader's own devices receive it too, so every device sees a dense seq stream.
	payload, err := gateway.MessageCTPayload(out.Seq, out.Epoch, s.DeviceID, pm, out.FrankingTag, out.RecvTS, &class)
	if err == nil {
		d.opts.Gateway.DeliverGroup(groupID, gateway.Frame{
			Op: gateway.OpMessageCT, GroupID: &groupID, Payload: payload, Replay: true,
		})
	}
	return out, nil
}

// Messages serves the catch-up stream. A tombstoned row comes back with no blob and deleted = 1.
func (d *DS) Messages(ctx context.Context, groupID id.ID, session Session, from uint64, limit int32) ([]store.AppMessageRow, error) {
	// Member-only: the ciphertext, the commitments and the franking tags of a group are enough to
	// correlate membership and message timing across communities.
	if err := d.requireMember(ctx, groupID, session); err != nil {
		return nil, err
	}
	from = clampCursor(from)
	if limit <= 0 || limit > 256 {
		limit = 256
	}
	// interfaces.md §5.1 row 12 gives this endpoint E_PRUNED, for the same reason the handshake
	// catch-up has it: past a deletion the answer would be a silently short list, and the client
	// cannot tell "these are gone, resync" from "nothing new". The group's PrunedBelow is the
	// highest seq DELIVERY retention has deleted from this stream (the cursor floor or the 30-day
	// delivery window), raised by PruneAppMessages in the transaction that deletes: a catch-up
	// from `from` has lost something exactly when `from` is at or below it. Read back, never
	// recomputed — MinCursor moves, and the oldest surviving message says nothing about the seqs
	// below it, which are as often handshakes. An archival expiry deletes without raising it
	// (Plan 2 task 8's retention ruling): `expires` is not monotone in seq, and a mark above a
	// surviving message would refuse a range this endpoint can serve. An expired message is simply
	// absent from the list, for every device alike; no resync could bring it back.
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if through := row.PrunedBelow; through > 0 && from <= through {
		return nil, errPruned(from, through+1)
	}
	rows, err := d.opts.Store.ListAppMessages(ctx, groupID, from, limit)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// DeleteMessage tombstones one message. At v1 only the uploading user may delete, from any of
// their devices; moderator deletion of E2EE messages needs a signed moderation event and is a
// later card (R29). A caller whose device is not a current member of the group is E_NOT_FOUND
// before anything else is read (L-HTTP-80, requireMember's reasoning), so the route says nothing
// about a group to a device outside it.
func (d *DS) DeleteMessage(ctx context.Context, s Session, groupID id.ID, seq uint64) error {
	if err := d.requireMemberOnly(ctx, groupID, s); err != nil {
		return err
	}
	// A seq no group can have reached is no message. Left alone it wraps negative in the store
	// adapters, which is a lookup that happens to miss today and a wrong row the day it does not.
	if seq > math.MaxInt64 {
		return errNotFound("message")
	}
	row, err := d.opts.Store.GetAppMessage(ctx, groupID, seq)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("message")
	}
	if err != nil {
		return err
	}
	uploader, err := d.opts.Store.GetDevice(ctx, row.UploaderDevice)
	if err != nil {
		return err
	}
	if uploader.UserID != s.UserID {
		return errNotUploader()
	}
	group, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("group")
	}
	if err != nil {
		return err
	}
	if err := d.requireGroupCapability(ctx, s, group); err != nil {
		return err
	}
	at := d.now()
	if err := d.opts.Store.TombstoneAppMessage(ctx, groupID, seq, at); err != nil {
		return err
	}
	d.NotifyMessageDeleted(groupID, seq, at)
	return nil
}

// NotifyMessageDeleted emits the existing replayable wakeup after a caller has
// committed a durable tombstone. It does not mutate storage or authorize a deletion.
func (d *DS) NotifyMessageDeleted(groupID id.ID, seq uint64, at int64) {
	if d.opts.Gateway == nil || at < 0 {
		return
	}
	payload, err := gateway.MessageDeletedPayload(seq, uint64(at))
	if err != nil {
		return
	}
	d.opts.Gateway.DeliverGroup(groupID, gateway.Frame{
		Op: gateway.OpMessageDeleted, GroupID: &groupID, Payload: payload, Replay: true,
	})
}

// AdvanceCursor is the only thing that moves a device's cursor. A frame put on a writer queue is
// not a delivery (protocol/02's retention wording, as amended).
//
// It is a group-scoped write and carries the same three guards: the caller must be a current
// member, the seq must be one the instance actually allocated, and the cursor only ever moves
// forward. All three exist because `device_cursors` is not a private scratch pad — it feeds
// `store.Cursors.MinCursor`, which task 26 makes the retention floor of the whole group.
func (d *DS) AdvanceCursor(ctx context.Context, s Session, groupID id.ID, seq, epoch uint64) error {
	// The read-modify-write below is only monotone if one goroutine at a time runs it; the
	// per-group lock is the same one Upload allocates seqs under.
	unlock := d.lock(groupID)
	defer unlock()

	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("group")
	} else if err != nil {
		return err
	}
	// Member-only, and E_NOT_FOUND rather than E_FORBIDDEN for the reason requireMember states: a
	// 403 here would tell any authenticated device on the instance which group ids are live. A
	// stranger's row would also sit in MinCursor forever, pinning the group's messages at floor 0.
	if err := d.requireMember(ctx, groupID, s); err != nil {
		return err
	}
	// `row.Seq` is the group's high-water in the one seq space; there is nothing above it to
	// acknowledge, and a cursor there would hold the floor above every message the group has.
	if seq > row.Seq {
		return errInvalid(fmt.Sprintf(
			"last_seq %d is above the group's high-water %d", seq, row.Seq))
	}
	cur, err := d.opts.Store.GetCursor(ctx, s.DeviceID, groupID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	// Monotone. Re-reading is done with `?from=`, not by rewinding the acknowledgement, so a lower
	// seq is a no-op rather than a refusal: a retried or reordered POST /cursor is ordinary and
	// the endpoint stays idempotent. What it must never do is move the retention floor backwards.
	if seq < cur.LastSeq {
		return nil
	}
	return d.opts.Store.PutCursor(ctx, s.DeviceID, groupID, seq, epoch, d.now())
}

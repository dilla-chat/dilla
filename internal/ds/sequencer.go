package ds

import (
	"context"
	"math"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// nextSeq allocates the next number in the group's ONE seq space. Handshakes and application
// messages share it, so a device's cursor is a single number and the stream is dense.
//
// The allocation runs inside the caller's transaction: `UPDATE mls_groups SET seq = seq + 1 …
// RETURNING seq` is atomic on both engines, and the per-group lock above it makes the
// read-modify cycle observable only to one goroutine at a time.
func nextSeq(ctx context.Context, tx store.Repository, groupID id.ID) (uint64, error) {
	return tx.NextSeq(ctx, groupID)
}

// clampCursor bounds a client-supplied seq cursor to what the stores can hold. Seqs are uint64 on
// the wire and int64 in SQL, so an unbounded cursor wraps negative in the adapters (2^63 became a
// cursor BEFORE the first row and served the whole log) and overflows `from+1` in the floor checks
// (2^64-1 became 0, below every floor, and answered E_PRUNED for a log with no hole). A cursor past
// MaxInt64 names no row, so it saturates: the page is empty, as it should be.
func clampCursor(from uint64) uint64 { return min(from, math.MaxInt64) }

// Handshakes serves the catch-up stream. A `from` at or below a handshake retention has deleted
// is E_PRUNED, which tells the client to resync rather than to retry.
func (d *DS) Handshakes(ctx context.Context, groupID id.ID, session Session, from uint64, limit int32) ([]store.HandshakeRow, error) {
	// Member-only: the handshake log names every leaf that ever committed and every epoch
	// transition of the group. A non-member is E_NOT_FOUND, never E_FORBIDDEN.
	if err := d.requireMember(ctx, groupID, session); err != nil {
		return nil, err
	}
	from = clampCursor(from)
	if limit <= 0 || limit > 512 {
		limit = 512
	}
	// `from` is the first seq the caller wants. The log has a hole for it exactly when retention
	// has deleted a handshake at or above it, which is what the group's HandshakesPruned records:
	// PruneHandshakes raises it to the highest seq it deletes, in the same transaction. Exact in
	// both directions (deviation B20, closed by the final review): a young group or an old one
	// with nothing swept is served; a cursor one below the lowest surviving handshake is refused
	// only if that seq was a handshake the sweep took, never because it happens to be a message.
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if through := row.HandshakesPruned; through > 0 && from <= through {
		return nil, errPruned(from, through+1)
	}
	return d.opts.Store.ListHandshakes(ctx, groupID, from, limit)
}

// Outstanding is every non-void proposal of the group's current epoch. It is the list both
// E_COMMIT_CONFLICT and E_COMMIT_REQUIRED carry, so a void or stale ref in here would send a
// client to commit something the instance has already withdrawn.
func (d *DS) Outstanding(ctx context.Context, groupID id.ID) ([]store.ProposalRow, error) {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	return d.opts.Store.ListProposals(ctx, groupID, row.Epoch, false)
}

// Proposal is one row of endpoint 19's answer: the SQL row plus the queued proposal blob the
// guest holds. protocol/02 fixes the shape as `[ref, kind, target_leaf|null, blob, void]`, and
// `mls_pending_proposals` has no blob column — the bytes live in the guest's proposal queue,
// which is also where `PublicGroup::queued_proposals` reads them for the two conflict bodies.
type Proposal struct {
	Row  store.ProposalRow
	Blob []byte
}

// Proposals is endpoint 19: the outstanding proposals of the group's current epoch, void ones
// included and flagged, for a member of the group only. A non-member is E_NOT_FOUND for the same
// reason Info, Tree and Handshakes are: the list names leaves and the devices they belong to.
func (d *DS) Proposals(ctx context.Context, groupID id.ID, session Session) ([]Proposal, error) {
	unlock := d.lock(groupID)
	defer unlock()
	if err := d.requireMember(ctx, groupID, session); err != nil {
		return nil, err
	}
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	rows, err := d.opts.Store.ListProposals(ctx, groupID, row.Epoch, true)
	if err != nil {
		return nil, err
	}
	blobs, err := d.queuedProposals(ctx, groupID)
	if err != nil {
		return nil, err
	}
	out := make([]Proposal, 0, len(rows))
	for _, r := range rows {
		out = append(out, Proposal{Row: r, Blob: blobs[string(r.Ref)]})
	}
	return out, nil
}

// queuedProposals is the guest's own proposal queue, keyed by ref. The blob a client needs is the
// one the guest stored, not a re-serialised Proposal: protocol/02's `blob` is the framed message
// the committer includes by reference.
func (d *DS) queuedProposals(ctx context.Context, groupID id.ID) (map[string][]byte, error) {
	out := map[string][]byte{}
	err := d.withGroup(ctx, groupID, func(g *mlswasi.PublicGroup) error {
		pairs, err := g.ProposalList(ctx)
		if err != nil {
			return err
		}
		for _, p := range pairs {
			out[string(p[0])] = p[1]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

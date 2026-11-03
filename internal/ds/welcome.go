package ds

// welcome.go is the delivery service's store-and-forward of Welcomes: protocol/02's role 4,
// endpoints 15 (`GET /v1/welcomes`) and 16 (`DELETE /v1/welcomes/{welcome_id}`), and the writer
// the commit's own transaction calls.
//
// Three facts shape the whole file (gap-13).
//
//  1. A Welcome blob is IDENTICAL for every joiner of one commit, so 256 joiners share ONE payload
//     row keyed by its SHA-256 and get 256 cheap pointers at it. Storing the body 256 times is how
//     a join storm fills a disk.
//  2. A dilla Welcome carries NO ratchet tree, and the live tree moves on with every commit, so
//     the tree of the welcoming epoch is stored beside the Welcome and served with it.
//  3. A fetch does NOT consume. `StagedWelcome::new_from_welcome` consumes the key material inside
//     the client even when the client then fails, so only the explicit DELETE marks one delivered.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/gateway"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
)

// welcomeRetentionDays is the delivery retention of a queued Welcome: 30 days, as protocol/02's
// Retention section fixes it. Past it the row is gone and the joiner must be re-added.
const welcomeRetentionDays = 30

// maxWelcomesPerPage is the cap `Welcomes` clamps a caller's `limit` to. It is also the page size
// the fan-out walks with.
const maxWelcomesPerPage = 64

// storeWelcomesTx writes one payload row per distinct blob, one welcome row per addressed device,
// and the epoch tree the joiners need — ALL INSIDE THE COMMIT'S OWN TRANSACTION. A Welcome row
// that outlived a rolled-back commit would address an epoch that never happened.
//
// `g` is the PublicGroup AFTER the merge, so `g.Tree` is the tree of the epoch this commit created
// — the welcoming epoch, which is the one fact 2 above requires.
//
// It RETURNS that tree and its hash: the fan-out needs both, they are the same two values for
// every joiner of this commit, and exporting the tree a second time — or reading one copy of it
// back per joiner through the Welcome queue's join — is the expensive way to learn what this
// function already holds.
func (d *DS) storeWelcomesTx(ctx context.Context, tx store.Repository, groupID id.ID, epoch, commitSeq uint64, g *mlswasi.PublicGroup, welcomes []WelcomeFor) (tree, treeHash []byte, err error) {
	if len(welcomes) == 0 {
		return nil, nil, nil
	}
	tree, treeHash, _, err = g.Tree(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.PutEpochTree(ctx, store.EpochTreeRow{
		GroupID:     groupID,
		Epoch:       epoch,
		RatchetTree: tree,
		TreeHash:    treeHash,
		Created:     d.now(),
	}); err != nil {
		return nil, nil, err
	}

	expires := d.now() + welcomeRetentionDays*24*60*60
	seen := map[[32]byte]struct{}{}
	rows := make([]store.WelcomeRow, 0, len(welcomes))
	for _, wf := range welcomes {
		sum := sha256.Sum256(wf.Blob)
		if _, ok := seen[sum]; !ok {
			seen[sum] = struct{}{}
			if err := tx.PutWelcomePayload(ctx, store.WelcomePayloadRow{
				BlobSHA256: sum[:],
				GroupID:    groupID,
				Epoch:      epoch,
				Blob:       wf.Blob,
				Created:    d.now(),
			}); err != nil {
				return nil, nil, err
			}
		}
		rows = append(rows, store.WelcomeRow{
			DeviceID:   wf.DeviceID,
			GroupID:    groupID,
			Epoch:      epoch,
			CommitSeq:  commitSeq,
			BlobSHA256: sum[:],
			Created:    d.now(),
			Expires:    expires,
		})
	}
	if err := tx.PutWelcomes(ctx, rows); err != nil {
		return nil, nil, err
	}
	return tree, treeHash, nil
}

// fanOutWelcomes sends one `mls.welcome` per addressed device, AFTER the commit's transaction and
// after the group's own two frames — protocol/02 fixes that order, so a Welcome must not precede
// the epoch change that produced it.
//
// It addresses the rows THIS commit created, found by the payload hash the commit itself computed.
// Re-deriving the row with "the device's oldest undelivered Welcome" would be wrong in the common
// case rather than the rare one: fact 3 above means a device joining a second channel, or one that
// has not acknowledged an earlier Welcome, still has that older row at the head of its queue — and
// would be re-sent it with the wrong welcome_id, group_id, epoch and blob, and never told about
// the new one.
// `tree` and `treeHash` are the welcoming epoch's, as `storeWelcomesTx` wrote them: one pair for
// the whole commit. They are ARGUMENTS rather than something each iteration reads back, because
// the frame budget can then be judged before anything is paid for — see the budget comment below.
func (d *DS) fanOutWelcomes(ctx context.Context, groupID id.ID, epoch uint64, tree, treeHash []byte, welcomes []WelcomeFor) {
	if d.opts.Gateway == nil || len(welcomes) == 0 {
		return
	}
	// The ratchet tree is the one payload in the protocol with no bound of its own — a 1,500-leaf
	// tree is 620 KiB against a frame budget of 131 584 — and a client sizes its websocket read
	// limit from the `max_frame_bytes` this same gateway advertised in `hello`. Sending an oversize
	// frame therefore does not merely fail to arrive: it closes the joiner's connection, and the
	// joiner comes back to be closed by it again.
	//
	// The Welcome is durably queued either way, so the frame is a NOTIFICATION and the queue is the
	// delivery: a joiner that is not told collects the same row from `GET /v1/welcomes`, whose body
	// cap is the delivery service's 2 MiB, not the gateway's.
	//
	// The tree is the same for every joiner of this commit, so the verdict is reached ONCE, here,
	// before a single Welcome row is read or a single payload encoded. It used to be reached per
	// joiner, after both: at Policy.MaxAddsPerCommit = 256 that is ~256 reads of a 620 KiB tree
	// through the queue's join and ~256 encodings of the same bytes, every one of them discarded
	// one line later — and all of it inside the group lock `commit` still holds.
	//
	// DEVIATION B25 (plan §C), carried from this task's report as C1: protocol/02's frame 20 fixes
	// the six elements with `ratchet_tree(bstr)` and names no size rule, so for any group whose
	// tree is over the budget — which is most of them — the addressed fan-out does not happen and
	// the joiner MUST poll row 15. Closing that needs an amendment (a tree-less notification, or a
	// chunked tree) and a controller ruling, not a number.
	budget := d.opts.Gateway.MaxFrameBytes()
	if budget > 0 && uint64(len(tree)) >= budget {
		d.log().Warn("the welcoming epoch's ratchet tree is larger than the gateway's frame budget; "+
			"every joiner of this commit must collect its Welcome from GET /v1/welcomes",
			"group", groupID, "epoch", epoch, "joiners", len(welcomes),
			"tree_bytes", len(tree), "budget", budget)
		return
	}
	for _, wf := range welcomes {
		// The blob is per joiner in shape even though one commit's Welcome is one blob, so its
		// share of the budget is judged before the row is read, for the same reason the tree's is.
		if budget > 0 && uint64(len(tree)+len(wf.Blob)) >= budget {
			d.log().Warn("an mls.welcome is larger than the gateway's frame budget; "+
				"the joiner must collect it from GET /v1/welcomes",
				"group", groupID, "device", wf.DeviceID,
				"bytes", len(tree)+len(wf.Blob), "budget", budget)
			continue
		}
		sum := sha256.Sum256(wf.Blob)
		row, ok := d.findWelcome(ctx, wf.DeviceID, groupID, epoch, sum[:])
		if !ok {
			continue
		}
		payload, err := gateway.WelcomePayload(uint64(row.WelcomeID), row.Epoch, row.CommitSeq, //nolint:gosec // G115: a unix second or row id this server wrote, never negative
			row.Blob, tree, treeHash)
		if err != nil {
			d.log().Error("encoding an mls.welcome failed", "group", groupID, "err", err)
			continue
		}
		// The frame the gateway writes, not the payload: the four-element frame adds the op, n,
		// the group id and the array header (at most 28 bytes with n at its widest), and a
		// client's read limit is the advertised max_frame_bytes.
		encoded, err := gateway.Encode(gateway.Frame{
			Op: gateway.OpMLSWelcome, GroupID: &groupID, Payload: payload, Replay: true,
		}, math.MaxUint64)
		if err != nil {
			d.log().Error("encoding an mls.welcome frame failed", "group", groupID, "device", wf.DeviceID, "err", err)
			continue
		}
		if budget > 0 && uint64(len(encoded)) > budget {
			d.log().Warn("an mls.welcome is larger than the gateway's frame budget; "+
				"the joiner must collect it from GET /v1/welcomes",
				"group", groupID, "device", wf.DeviceID, "bytes", len(encoded), "budget", budget)
			continue
		}
		d.opts.Gateway.DeliverDevice(wf.DeviceID, gateway.Frame{
			Op: gateway.OpMLSWelcome, GroupID: &groupID, Payload: payload, Replay: true,
		})
	}
}

// findWelcome is the row `PutWelcomes` just inserted for one device, named by the only key the
// caller holds: the payload hash. `welcome_id` is the table's AUTOINCREMENT surrogate, so the
// writer never learns it, and `mls_welcomes` is unique on (device_id, blob_sha256) — the pair is
// therefore exact, not a heuristic.
//
// It pages rather than taking one shot at the first 64 rows: a device that has let Welcomes pile
// up unacknowledged has them AHEAD of this one in welcome_id order, and dropping its `mls.welcome`
// because of that is precisely the failure the paging avoids. The walk is bounded — a device with
// more queued Welcomes than this has a problem no frame will fix.
func (d *DS) findWelcome(ctx context.Context, deviceID, groupID id.ID, epoch uint64, sum []byte) (store.WelcomeFull, bool) {
	const maxPages = 16
	var after int64
	for range maxPages {
		rows, err := d.opts.Store.ListWelcomes(ctx, deviceID, after, maxWelcomesPerPage)
		if err != nil {
			d.log().Error("reading back a stored Welcome failed", "device", deviceID, "err", err)
			return store.WelcomeFull{}, false
		}
		if len(rows) == 0 {
			return store.WelcomeFull{}, false
		}
		for _, row := range rows {
			if row.GroupID == groupID && row.Epoch == epoch && bytes.Equal(row.BlobSHA256, sum) {
				return row, true
			}
		}
		after = rows[len(rows)-1].WelcomeID
	}
	return store.WelcomeFull{}, false
}

// Welcomes lists a device's undelivered Welcomes with the ratchet tree of each one's own epoch.
// The fetch does NOT consume (fact 3): only AckWelcome marks one delivered.
func (d *DS) Welcomes(ctx context.Context, s Session, after int64, limit int32) ([]store.WelcomeFull, error) {
	// A provisional session sees ONLY its pairing group's Welcome. The route accepts a provisional
	// session (interfaces §5.1 row 15) so a device can complete pairing before it is enrolled;
	// returning every undelivered Welcome for the device would let a session issued before
	// enrolment harvest the Welcomes of every group that device was ever added to, which is
	// exactly the escalation §2.2 point 4 forbids. The check runs BEFORE the read, so an unbound
	// provisional session never reaches SQL at all.
	provisional := s.Scope == auth.ScopeProvisional
	if provisional && s.PairingGroup == nil {
		return nil, errProvisionalOutsidePairing("this provisional session is bound to no pairing group")
	}
	if limit <= 0 || limit > maxWelcomesPerPage {
		limit = maxWelcomesPerPage
	}
	if after < 0 {
		after = 0
	}
	now := d.now()
	out := make([]store.WelcomeFull, 0, limit)
	// Both filters below are applied in Go, AFTER the SQL LIMIT, so the read must keep paging: row
	// 15 is a cursor API — the client advances `after` by the last welcome_id it was handed and
	// stops on an empty page — and a page whose rows are all dropped would answer "nothing left"
	// while live rows sit behind it, with the ids the client needs for `after` among the dropped
	// ones. So the queue is walked until `limit` rows are collected or it is exhausted.
	//
	// The first read is the caller's own page size, which is what an unfiltered queue costs today;
	// once rows are filtered, the walk takes the maximum stride. It must continue until it has
	// collected the requested number or reached the end: the response has no separate scan cursor,
	// so stopping at a bounded scan page could strand allowed rows behind filtered Welcomes.
	cursor := after
	pageSize := limit
	for {
		rows, err := d.opts.Store.ListWelcomes(ctx, s.DeviceID, cursor, pageSize)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		cursor = rows[len(rows)-1].WelcomeID
		for _, row := range rows {
			// Past its 30-day delivery retention a Welcome is gone, whether or not the retention
			// sweep has reached it yet: `expires` is on the row, and serving an expired Welcome
			// would hand a joiner key material the group has long since rotated past.
			if row.Expires <= now {
				continue
			}
			if provisional && row.GroupID != *s.PairingGroup {
				continue
			}
			group, groupErr := d.opts.Store.GetGroup(ctx, row.GroupID)
			if errors.Is(groupErr, store.ErrNotFound) {
				continue
			}
			if groupErr != nil {
				return nil, groupErr
			}
			if capabilityErr := d.requireGroupCapability(ctx, s, group); capabilityErr != nil {
				var upgrade *capability.UpgradeRequired
				if errors.As(capabilityErr, &upgrade) {
					// Filter only this row. The cursor still advances across every inspected row, so
					// unaffected DMs and communities behind it remain discoverable.
					continue
				}
				return nil, capabilityErr
			}
			out = append(out, row)
			if int32(len(out)) == limit { //nolint:gosec // G115: bounded by the int32 page size the slice was requested with
				return out, nil
			}
		}
		if int32(len(rows)) < pageSize { //nolint:gosec // G115: bounded by the int32 page size the slice was requested with
			break // a short page is the end of the queue
		}
		pageSize = maxWelcomesPerPage
	}
	return out, nil
}

// AckWelcome marks one Welcome delivered. A device may only acknowledge its own — an unknown or
// already-delivered welcome_id is E_NOT_FOUND, the code protocol/02 fixes for row 16 — and a
// provisional session only its pairing group's.
func (d *DS) AckWelcome(ctx context.Context, s Session, welcomeID int64) error {
	if s.Scope == auth.ScopeProvisional && s.PairingGroup == nil {
		return errProvisionalOutsidePairing("this provisional session is bound to no pairing group")
	}
	row, err := d.welcomeByID(ctx, s.DeviceID, welcomeID)
	if err != nil {
		return err
	}
	if s.Scope == auth.ScopeProvisional && row.GroupID != *s.PairingGroup {
		return errProvisionalOutsidePairing(
			"a provisional session may only acknowledge its pairing group's Welcome")
	}
	group, err := d.opts.Store.GetGroup(ctx, row.GroupID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("welcome")
	}
	if err != nil {
		return err
	}
	if err := d.requireGroupCapability(ctx, s, group); err != nil {
		return err
	}
	return d.opts.Store.DeleteWelcome(ctx, s.DeviceID, welcomeID, d.now())
}

// welcomeByID is one undelivered Welcome of one device, by its surrogate key.
//
// It reads through `ListWelcomes(after = welcomeID-1, limit = 1)` rather than through a getter of
// its own: the query is `welcome_id > after AND delivered_at IS NULL` ordered by welcome_id, so
// the single row it can return is exactly this one — and `store.MLS`'s method set is fixed by
// deviation ID1, which says no later task invents a name on it.
func (d *DS) welcomeByID(ctx context.Context, deviceID id.ID, welcomeID int64) (store.WelcomeFull, error) {
	if welcomeID <= 0 {
		return store.WelcomeFull{}, errNotFound("welcome")
	}
	rows, err := d.opts.Store.ListWelcomes(ctx, deviceID, welcomeID-1, 1)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.WelcomeFull{}, errNotFound("welcome")
		}
		return store.WelcomeFull{}, err
	}
	if len(rows) == 0 || rows[0].WelcomeID != welcomeID {
		return store.WelcomeFull{}, errNotFound("welcome")
	}
	return rows[0], nil
}

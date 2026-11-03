package blob

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// MaxPurgeReasonBytes bounds the reason a purge records in the audit log.
const MaxPurgeReasonBytes = 1024

// ValidPurgeReason is the rule both entry points apply before calling Purge: 1
// to MaxPurgeReasonBytes bytes, no NUL. A NUL is legal in a Go string and in a
// SQLite TEXT column but refused by Postgres, so it is refused on both engines
// alike.
func ValidPurgeReason(reason string) bool {
	return reason != "" && len(reason) <= MaxPurgeReasonBytes && !strings.ContainsRune(reason, 0)
}

// PurgeRequest is one administrator's removal of one blob's bytes. By is the
// instance admin who asked; the zero id means the operator at a shell
// (`dillad admin blob purge`), who is not a users row: the audit row then names
// no actor, and the tombstone carries the all-zero id, the same "not a real
// user" value P2-D32 uses for "not device scoped".
type PurgeRequest struct {
	BlobID []byte
	Reason string
	By     id.ID
	At     int64
}

// PurgeResult reports what a purge did beyond removing the references.
type PurgeResult struct {
	// UnlinkErr is the error from unlinking the file. The row and every
	// reference are already gone and the tombstone refuses the bytes on every
	// route, so a file left behind is unreachable; `dillad doctor` reports it
	// as an orphan. It is nil when the file was removed or was never there.
	UnlinkErr error
	// BackupKept reports that a backups row names the bytes (security review F10, branch review
	// BACKUPS-RECOVERY-05). A backup object is a user's sealed key material and the root object is
	// written once, so deleting its bytes would end the account's recovery for good. The purge then
	// still deleted every channel reference and wrote its tombstone and audit row, but kept the
	// blobs row and did not unlink the file; a state object's replacement unlinks them later.
	BackupKept bool
}

// Purge is the one implementation behind DELETE /v1/admin/blobs/{blob_id} and
// `dillad admin blob purge`: in one transaction it writes the tombstone,
// removes every reference and the blobs row, and writes the audit row; after the
// commit it unlinks the file.
//
// The tombstone is not optional: without it, anyone still holding the
// ciphertext re-PUTs it and content addressing hands back the same name. A purge
// of bytes the instance does not hold still writes the tombstone, which makes
// the table the operator's hash blocklist (gap-47 §7.3). A second purge of the
// same bytes is harmless: the first tombstone stands and the audit log records
// both acts.
//
// Every attachment is encrypted under a random per-attachment key, so the same
// file sent by someone else has a different blob_id. A purge removes BYTES, not
// CONTENT.
func Purge(ctx context.Context, repo store.Repository, bs *Store, req PurgeRequest) (PurgeResult, error) {
	var actor *id.ID
	if !req.By.IsZero() {
		by := req.By
		actor = &by
	}
	target := hex.EncodeToString(req.BlobID)
	var backup bool
	if err := repo.Tx(ctx, func(tx store.Repository) error {
		// The per-blob lock the two recording transactions take before their cross-table checks
		// (fix-wave review NEW-4): on Postgres a backup PUT of the same bytes otherwise commits its
		// backups row between this answer and the unlink below. A purge of bytes with no committed
		// blobs row locks nothing (the lock is the row); a backup PUT whose row is still uncommitted
		// then rests on its own in-transaction tombstone check.
		if err := tx.LockBlob(ctx, req.BlobID); err != nil {
			return err
		}
		// Inside the transaction that removes the references, so the answer is the one the
		// commit acts on.
		var err error
		backup, err = tx.BackupRefersToBlob(ctx, req.BlobID)
		if err != nil {
			return err
		}
		// Every reference goes whatever else names the bytes ("Purge removes every reference to the
		// blob in every channel"); before BACKUPS-RECOVERY-05 bytes a backup named refused the
		// whole purge, so an attachment shaped as a backup object stayed downloadable.
		// DeleteAllBlobRefs returns (int64, error) - P2-D18 - so the count is discarded explicitly.
		if _, err := tx.DeleteAllBlobRefs(ctx, req.BlobID); err != nil {
			return err
		}
		// The tombstone always: it refuses every new attachment or backup of these bytes, so the
		// takedown still holds once a backup that names them is replaced (the replacement then
		// unlinks them and the tombstone stays). A backup object's own reads do not consult it.
		if err := tx.PutBlobTombstone(ctx, req.BlobID, req.Reason, req.By, req.At); err != nil {
			return err
		}
		if !backup {
			if err := tx.DeleteBlob(ctx, req.BlobID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		return tx.Audit(ctx, store.AuditRow{
			Actor: actor, Action: "blob.purge", Target: target, Detail: req.Reason, At: req.At,
		})
	}); err != nil {
		return PurgeResult{}, err
	}
	if backup {
		return PurgeResult{BackupKept: true}, nil
	}
	return PurgeResult{UnlinkErr: bs.Delete(req.BlobID)}, nil
}

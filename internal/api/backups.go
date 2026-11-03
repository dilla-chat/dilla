// Package api serves the two header objects of protocol/06 ("Header") at /v1/backups (protocol/09 § Backups).
// The root object is written once (F3), the state object is replaced; both are stored
// content-addressed in the blob store and read back by the user's own enrolled and pending sessions.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"

	"github.com/fxamacker/cbor/v2"

	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/server"
	"github.com/dilla-chat/dilla/internal/store"
)

const (
	rootObjectLen       = 103
	maxRootBody         = 256
	maxStateBody        = 1<<20 + 64
	minBackupCiphertext = 16
)

var errRootStored = errors.New("api: a different root object is stored")

// errAttachmentBytes and errBackupBytes keep backup objects and attachments disjoint (branch review
// BACKUPS-RECOVERY-05): each refuses, with 409, bytes the other kind already names.
var (
	errAttachmentBytes = errors.New("api: an attachment references these bytes")
	errBackupBytes     = errors.New("api: a backup object names these bytes")
)

func backupPath(r *http.Request) (kind int32, limit int64, err error) {
	if r.PathValue("chunk_seq") != "0" {
		return 0, 0, server.Errorf(server.CodeNotFound, "no such backup")
	}
	switch r.PathValue("kind") {
	case "0":
		return 0, maxRootBody, nil
	case "1":
		return 1, maxStateBody, nil
	default:
		return 0, 0, server.Errorf(server.CodeNotFound, "no such backup")
	}
}

func backupObject(raw cbor.RawMessage, kind int32) ([]byte, error) {
	items, err := decodeArray(raw)
	if err != nil || len(items) != 1 {
		return nil, server.Errorf(server.CodeInvalidRequest, "body is [object]")
	}
	object, err := decodeBytes(items[0])
	if err != nil {
		return nil, server.Errorf(server.CodeInvalidRequest, "body is [object]")
	}
	parts, err := decodeArray(object)
	if err != nil || len(parts) != 3 {
		return nil, server.Errorf(server.CodeInvalidRequest, "object is not a stored header object")
	}
	version, versionErr := decodeUint(parts[0])
	nonce, nonceErr := decodeBytes(parts[1])
	ciphertext, ciphertextErr := decodeBytes(parts[2])
	if versionErr != nil || version != 1 || nonceErr != nil || len(nonce) != 12 || ciphertextErr != nil || len(ciphertext) < minBackupCiphertext {
		return nil, server.Errorf(server.CodeInvalidRequest, "object is not a stored header object")
	}
	if kind == 0 && len(object) != rootObjectLen {
		return nil, server.Errorf(server.CodeInvalidRequest, "a root object is 103 bytes")
	}
	return object, nil
}

// PutBackup accepts one sealed object and stores its content address before recording its row.
func (d Deps) PutBackup(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	kind, limit, err := backupPath(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if d.Blobs == nil || d.Config == nil {
		d.logf(r, "api: backup store not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the backup store is not wired"))
		return
	}
	// The root (kind 0) spends the blob upload meter before a byte of the body is read, exactly as
	// the attachment PUT does (security review F3); the reservation is the body cap or the announced
	// length, and every byte read spends the day's budget whatever happens to the object. The state
	// object (kind 1) does not: it spends only the device session's write bucket the route mounts
	// (branch review BACKUPS-RECOVERY-01). On the user's shared meter a stolen session of the same
	// user kept the bucket empty at its own device rate, and the owner's state PUT, which revoking
	// another device starts with, answered 429 for as long as the thief kept at it. Instead it
	// spends the device's own daily byte budget (StateMeter, keyed by the session's device), which
	// with the immediate deletion of the replaced object below bounds what one device can make the
	// instance read and write through the route.
	meter, meterKey := d.UploadMeter, sess.UserID
	if kind == 1 {
		meter, meterKey = d.StateMeter, sess.DeviceID
	}
	reserve := limit
	if r.ContentLength >= 0 && r.ContentLength < reserve {
		reserve = r.ContentLength
	}
	held, err := meter.begin(meterKey, reserve)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	counted := &countingReader{r: r.Body}
	r.Body = struct {
		io.Reader
		io.Closer
	}{counted, r.Body}
	var raw cbor.RawMessage
	err = server.DecodeBody(w, r, limit, &raw)
	meter.settle(meterKey, held, counted.n)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	object, err := backupObject(raw, kind)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	ctx := r.Context()
	sum := sha256.Sum256(object)
	blobID := sum[:]
	// The blob routes' tombstone gate: content addressing would otherwise let anyone holding the
	// ciphertext of an administrator-purged blob store it again as a backup object.
	if err := refuseTombstoned(ctx, d.Repo, blobID); err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if maxBytes := d.Config.Blobs.StoreMaxBytes; maxBytes > 0 {
		_, err := d.Repo.GetBlob(ctx, blobID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			used, err := d.Repo.InstanceBlobBytes(ctx)
			if err != nil {
				server.WriteError(w, d.storeError(r, err))
				return
			}
			if used > maxBytes-int64(len(object)) {
				server.WriteError(w, server.Errorf(server.CodeStorageFull, "the instance's storage is full"))
				return
			}
		case err != nil:
			server.WriteError(w, d.storeError(r, err))
			return
		}
	}
	n, created, err := d.Blobs.Put(ctx, blobID, bytes.NewReader(object), int64(len(object)))
	if err != nil {
		d.logf(r, "api: store backup bytes", "err", err)
		server.WriteError(w, server.Errorf(server.CodeInternal, ""))
		return
	}
	now := d.Clock.Now().Unix()
	status := http.StatusCreated
	var replaced store.BlobRow // the replaced state object's row, deleted in the transaction, to unlink
	err = d.Repo.Tx(ctx, func(tx store.Repository) error {
		replaced = store.BlobRow{}
		if kind == 1 {
			// One state replacement of a user at a time: the users row FOR UPDATE on Postgres, the
			// write transaction on SQLite (the lock device registration takes). Without it two
			// devices' concurrent PUTs both read the same predecessor under READ COMMITTED, and the
			// object the first one stored was replaced by the second and never deleted.
			if err := tx.LockUserForDeviceRegistration(ctx, sess.UserID); err != nil {
				return err
			}
		}
		if err := tx.PutBlob(ctx, store.BlobRow{
			BlobID: blobID, Size: uint64(n), //nolint:gosec // G115: n is a byte count returned by blob.Store.Put, never negative
			StorageRef: blob.StorageRef(d.Config.Blobs.Backend, blobID), Created: now,
		}); err != nil {
			return err
		}
		// The per-blob lock before the cross-table checks below, the one the attachment route takes
		// too, so the two routes cannot both find the other table empty for the same bytes.
		if err := tx.LockBlob(ctx, blobID); err != nil {
			return err
		}
		// The file is checked after the row is claimed: a replaced object's delete claims the same
		// row before it unlinks, so either it saw this row and kept the file, or the file is gone now.
		if err := bytesPresent(d.Blobs, blobID); err != nil {
			return err
		}
		// The tombstone again, inside the transaction that records the object: a purge that
		// committed after the first check must not be undone by this upload (blobs.go, fix wave I9).
		tomb, err := tx.GetBlobTombstone(ctx, blobID)
		if err != nil {
			return err
		}
		if tomb {
			return errTombstoned
		}
		// Backup objects and attachments never share bytes (branch review BACKUPS-RECOVERY-05): a
		// sender chooses every byte of an attachment's ciphertext, so one shaped as a state object and
		// also stored as a backup made the admin purge refuse it and its channel references survive.
		refs, err := tx.CountBlobRefs(ctx, blobID)
		if err != nil {
			return err
		}
		if refs > 0 {
			return errAttachmentBytes
		}
		if err := tx.ClearBlobUnreferenced(ctx, blobID); err != nil {
			return err
		}
		row := store.BackupRow{UserID: sess.UserID, Kind: uint64(kind), //nolint:gosec // G115: backupPath returns only 0 or 1
			DeviceID: id.ID{}, ChunkSeq: 0, BlobID: blobID, Created: now}
		if kind == 0 {
			if err := tx.InsertBackup(ctx, row); errors.Is(err, store.ErrConflict) {
				return errRootStored
			} else if err != nil {
				return err
			}
			return d.withinQuota(ctx, tx, sess.UserID)
		}
		prev, err := tx.GetBackup(ctx, sess.UserID, 1, id.ID{}, 0)
		// noLarger: the replacement is no larger than the state object it replaces.
		noLarger := false
		var prevBlob store.BlobRow
		if errors.Is(err, store.ErrNotFound) {
			prev = store.BackupRow{}
		} else if err != nil {
			return err
		} else {
			status = http.StatusOK
			prevBlob, err = tx.GetBlob(ctx, prev.BlobID)
			if err != nil {
				return err
			}
			noLarger = uint64(n) <= prevBlob.Size //nolint:gosec // G115: n is a nonnegative byte count
		}
		if err := tx.PutBackup(ctx, row); err != nil {
			return err
		}
		// The quota check for a replacement is the delta over the object it replaces (branch review
		// BACKUPS-RECOVERY-01): one no larger never fails it, even for a user already at or past the
		// quota, so re-sealing the state object can never be refused for storage. A larger one is
		// checked after the upsert, so the replaced object has left the count.
		if !noLarger {
			if err := d.withinQuota(ctx, tx, sess.UserID); err != nil {
				return err
			}
		}
		if prev.BlobID != nil && !bytes.Equal(prev.BlobID, blobID) {
			refers, err := tx.BackupRefersToBlob(ctx, prev.BlobID)
			if err != nil || refers {
				return err
			}
			refs, err := tx.CountBlobRefs(ctx, prev.BlobID)
			if err != nil {
				return err
			}
			if refs > 0 {
				return tx.MarkBlobUnreferenced(ctx, prev.BlobID, now)
			}
			// Nothing else names the replaced bytes: their row goes in this transaction and their
			// file right after the commit, never through blobs.gc_grace (branch review
			// BACKUPS-RECOVERY-01 as amended). Kept for the grace, every distinct replacement at
			// the device's write rate stayed on disk a day, uncounted by any quota.
			if err := tx.DeleteBlob(ctx, prev.BlobID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			replaced = prevBlob
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errTombstoned) {
			// The purge's own unlink may have run before this upload wrote the file; purged bytes
			// stay removed, so they go now — unless a backups row still names them: a purge of
			// root bytes keeps the file while the write-once root is served, and a racing
			// identical re-PUT must not unlink a root that GET still serves.
			named, nerr := d.Repo.BackupRefersToBlob(r.Context(), blobID)
			if nerr != nil {
				d.logf(r, "api: check purged bytes against backups", "err", nerr)
			} else if !named {
				if derr := d.Blobs.Delete(blobID); derr != nil {
					d.logf(r, "api: remove purged bytes a backup rewrote", "err", derr)
				}
			}
			server.WriteError(w, errPruned())
			return
		}
		if errors.Is(err, errAttachmentBytes) {
			// The bytes are an attachment's, referenced and so kept; nothing to orphan.
			server.WriteError(w, server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest,
				"these bytes are an attachment; a backup object cannot share them")))
			return
		}
		if errors.Is(err, errBytesGone) {
			// A replaced object's delete unlinked these bytes under this upload; no row may name a
			// missing file, so nothing is recorded and the client sends them again.
			server.WriteError(w, errBytesGoneRetry())
			return
		}
		if created {
			d.orphanBackup(ctx, blobID, n, now)
		}
		if errors.Is(err, errQuota) {
			server.WriteError(w, errStorageFull())
			return
		}
		if errors.Is(err, errRootStored) {
			existing, readErr := d.Repo.GetBackup(ctx, sess.UserID, 0, id.ID{}, 0)
			if readErr != nil {
				server.WriteError(w, d.storeError(r, readErr))
				return
			}
			if bytes.Equal(existing.BlobID, blobID) {
				d.write(w, r, http.StatusOK, []any{blobID, uint64(n)}) //nolint:gosec // G115: n is a nonnegative byte count
				return
			}
			server.WriteError(w, server.WithStatus(http.StatusConflict, server.Errorf(server.CodeInvalidRequest, "root object already stored")))
			return
		}
		server.WriteError(w, d.storeError(r, err))
		return
	}
	if replaced.BlobID != nil {
		d.unlinkReplaced(ctx, replaced, now)
	}
	d.write(w, r, status, []any{blobID, uint64(n)}) //nolint:gosec // G115: n is a nonnegative byte count
}

// errBytesGone is a recording transaction finding the file it is about to name gone: a replaced
// state object's delete unlinked the same bytes after this upload found them on disk.
var errBytesGone = errors.New("api: the uploaded bytes were unlinked under the upload")

// errBytesGoneRetry is errBytesGone's answer: 503 with a short retry, after which the upload writes
// the bytes again.
func errBytesGoneRetry() error {
	return server.Unavailable(100, "the stored bytes were removed during the upload; send them again")
}

// bytesPresent is the recording transactions' check that the file the row names exists. It runs
// after the transaction claimed the blobs row (PutBlob), which is what orders it against
// unlinkReplaced: that claims the same row before it unlinks.
func bytesPresent(bs *blob.Store, blobID []byte) error {
	if _, err := bs.Stat(blobID); errors.Is(err, blob.ErrNotFound) {
		return errBytesGone
	} else if err != nil {
		return err
	}
	return nil
}

// unlinkReplaced unlinks the file of a replaced state object whose row the committed transaction
// deleted, safely against an upload of the same bytes (same content address) racing it. In its own
// transaction it claims the row again (PutBlob, an insert that is a no-op when the row exists: on
// Postgres it waits on an uncommitted insert of the same id, on SQLite the write transaction
// serialises it with every other recording transaction) and re-checks under that claim that no
// backups row and no blob_refs row names the bytes. Only then does it delete the row and unlink the
// file, before its commit. An upload that recorded first keeps its row and file; one that records
// after finds the file gone in its own transaction (bytesPresent) and records nothing.
func (d Deps) unlinkReplaced(ctx context.Context, row store.BlobRow, now int64) {
	ctx = context.WithoutCancel(ctx)
	err := d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.PutBlob(ctx, store.BlobRow{BlobID: row.BlobID, Size: row.Size, StorageRef: row.StorageRef, Created: now}); err != nil {
			return err
		}
		if err := tx.LockBlob(ctx, row.BlobID); err != nil {
			return err
		}
		backup, err := tx.BackupRefersToBlob(ctx, row.BlobID)
		if err != nil || backup {
			return err
		}
		refs, err := tx.CountBlobRefs(ctx, row.BlobID)
		if err != nil || refs > 0 {
			return err
		}
		if err := tx.DeleteBlob(ctx, row.BlobID); err != nil {
			return err
		}
		if err := d.Blobs.Delete(row.BlobID); err != nil && !errors.Is(err, blob.ErrNotFound) {
			return err
		}
		return nil
	})
	if err != nil {
		// The replacing transaction already deleted the row; when this one fails (the unlink
		// included) the file stays without a row, a stray file `dillad doctor` reports.
		d.Log.ErrorContext(ctx, "unlink a replaced state object", "err", err)
	}
}

// withinQuota is the blob route's exact quota check, run inside the transaction that records the
// object: backup objects count toward blobs.quota_bytes_per_user with the user's attachments, each
// distinct blob once (store.UserBlobBytes).
func (d Deps) withinQuota(ctx context.Context, tx store.Repository, userID id.ID) error {
	quota := d.Config.Blobs.QuotaBytesPerUser
	if quota <= 0 {
		return nil
	}
	used, err := tx.UserBlobBytes(ctx, userID)
	if err != nil {
		return err
	}
	if used > quota {
		return errQuota
	}
	return nil
}

func (d Deps) orphanBackup(ctx context.Context, blobID []byte, n, now int64) {
	ctx = context.WithoutCancel(ctx)
	err := d.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.PutBlob(ctx, store.BlobRow{
			BlobID: blobID, Size: uint64(n), //nolint:gosec // G115: n is a nonnegative byte count
			StorageRef: blob.StorageRef(d.Config.Blobs.Backend, blobID), Created: now,
		}); err != nil {
			return err
		}
		return tx.MarkBlobUnreferenced(ctx, blobID, now)
	})
	if err != nil {
		d.Log.ErrorContext(ctx, "record an unreferenced backup", "err", err)
	}
}

// ListBackups returns the two served kinds in root-then-state order.
func (d Deps) ListBackups(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	out := make([]any, 0)
	for _, kind := range []int32{0, 1} {
		rows, err := d.Repo.ListBackups(r.Context(), sess.UserID, kind)
		if err != nil {
			server.WriteError(w, d.storeError(r, err))
			return
		}
		for _, row := range rows {
			blobRow, err := d.Repo.GetBlob(r.Context(), row.BlobID)
			if err != nil {
				d.logf(r, "api: backup blob row missing", "err", err)
				server.WriteError(w, server.Errorf(server.CodeInternal, ""))
				return
			}
			out = append(out, []any{row.Kind, row.ChunkSeq, blobRow.Size, uint64(row.Created)}) //nolint:gosec // G115: a unix second this server wrote, never negative
		}
	}
	d.write(w, r, http.StatusOK, out)
}

// GetBackup returns stored bytes; an unreadable referenced object is a server fault.
func (d Deps) GetBackup(w http.ResponseWriter, r *http.Request) {
	sess, ok := session(r)
	if !ok {
		server.WriteError(w, server.Errorf(server.CodeUnauthenticated, ""))
		return
	}
	kind, _, err := backupPath(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	if d.Blobs == nil {
		d.logf(r, "api: backup store not wired")
		server.WriteError(w, server.Errorf(server.CodeInternal, "the backup store is not wired"))
		return
	}
	row, err := d.Repo.GetBackup(r.Context(), sess.UserID, kind, id.ID{}, 0)
	if errors.Is(err, store.ErrNotFound) {
		server.WriteError(w, server.Errorf(server.CodeNotFound, "no backup of this kind"))
		return
	}
	if err != nil {
		server.WriteError(w, d.storeError(r, err))
		return
	}
	f, size, err := d.Blobs.Get(row.BlobID)
	if err != nil {
		d.logf(r, "api: read backup bytes", "err", err)
		server.WriteError(w, server.Errorf(server.CodeInternal, ""))
		return
	}
	defer func() { _ = f.Close() }()
	if size < 0 || size > maxStateBody {
		d.logf(r, "api: backup size outside bound", "size", size)
		server.WriteError(w, server.Errorf(server.CodeInternal, ""))
		return
	}
	object, err := io.ReadAll(io.LimitReader(f, size))
	if err != nil || int64(len(object)) != size {
		d.logf(r, "api: short backup read", "err", err)
		server.WriteError(w, server.Errorf(server.CodeInternal, ""))
		return
	}
	d.write(w, r, http.StatusOK, []any{object, uint64(row.Created)}) //nolint:gosec // G115: a unix second this server wrote, never negative
}

// DeleteBackup is reserved at wire 1.
func (d Deps) DeleteBackup(w http.ResponseWriter, _ *http.Request) {
	server.WriteError(w, notImplemented("DELETE /v1/backups is not served at wire 1"))
}

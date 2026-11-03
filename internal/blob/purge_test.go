package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// Purge is the one implementation behind DELETE /v1/admin/blobs/{blob_id} and
// `dillad admin blob purge`. The CLI acts as the operator, who is not a users
// row, so its audit row names no actor and its tombstone carries the all-zero id.
func TestPurgeWithNoActorRemovesTheBlobAndAuditsAnOperatorAct(t *testing.T) {
	h := newGCHarness(t)
	ctx := t.Context()
	body := []byte("ciphertext of an attachment")
	sum := sha256.Sum256(body)
	blobID := sum[:]
	if _, _, err := h.Store.Put(ctx, blobID, bytes.NewReader(body), 1<<20); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := h.Repo.PutBlob(ctx, store.BlobRow{BlobID: blobID, Size: uint64(len(body)),
		StorageRef: "fs:x", Created: h.Clock.Now().Unix()}); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	res, err := blob.Purge(ctx, h.Repo, h.Store, blob.PurgeRequest{
		BlobID: blobID, Reason: "court order 42", At: h.Clock.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.UnlinkErr != nil {
		t.Fatalf("the file was not unlinked: %v", res.UnlinkErr)
	}
	if _, err := h.Store.Stat(blobID); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("the file survived the purge: %v", err)
	}
	if _, err := h.Repo.GetBlob(ctx, blobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the blobs row survived: %v", err)
	}
	if ok, err := h.Repo.GetBlobTombstone(ctx, blobID); err != nil || !ok {
		t.Fatalf("no tombstone: ok=%v err=%v", ok, err)
	}
	rows, err := h.Repo.ListAudit(ctx, 0, 10)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(rows) != 1 || rows[0].Action != "blob.purge" || rows[0].Detail != "court order 42" || rows[0].Actor != nil {
		t.Fatalf("audit = %+v, want one blob.purge row with no actor", rows)
	}

	// A second purge of the same bytes is harmless and audits again.
	if _, err := blob.Purge(ctx, h.Repo, h.Store, blob.PurgeRequest{
		BlobID: blobID, Reason: "again", By: id.New(), At: h.Clock.Now().Unix(),
	}); err != nil {
		t.Fatalf("a second Purge: %v", err)
	}
}

// lockRecorder records the blob-lock and backup reads a purge's transaction makes, in order.
type lockRecorder struct {
	store.Repository
	calls *[]string
}

func (l lockRecorder) Tx(ctx context.Context, fn func(store.Repository) error) error {
	return l.Repository.Tx(ctx, func(tx store.Repository) error {
		return fn(lockRecorder{Repository: tx, calls: l.calls})
	})
}

func (l lockRecorder) LockBlob(ctx context.Context, blobID []byte) error {
	*l.calls = append(*l.calls, "LockBlob")
	return l.Repository.LockBlob(ctx, blobID)
}

func (l lockRecorder) BackupRefersToBlob(ctx context.Context, blobID []byte) (bool, error) {
	*l.calls = append(*l.calls, "BackupRefersToBlob")
	return l.Repository.BackupRefersToBlob(ctx, blobID)
}

// Fix-wave review NEW-4: the purge takes the per-blob lock the two recording transactions take
// (store.LockBlob) before it asks whether a backup names the bytes, so on Postgres a backup PUT of
// the same bytes cannot commit a backups row between that answer and the purge's unlink.
func TestPurgeTakesTheBlobLockBeforeItReadsTheBackups(t *testing.T) {
	h := newGCHarness(t)
	ctx := t.Context()
	body := []byte("unreferenced bytes a backup PUT may be storing")
	sum := sha256.Sum256(body)
	if err := h.Repo.PutBlob(ctx, store.BlobRow{BlobID: sum[:], Size: uint64(len(body)),
		StorageRef: "fs:x", Created: h.Clock.Now().Unix()}); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	var calls []string
	if _, err := blob.Purge(ctx, lockRecorder{Repository: h.Repo, calls: &calls}, h.Store, blob.PurgeRequest{
		BlobID: sum[:], Reason: "abuse", At: h.Clock.Now().Unix(),
	}); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if !slices.Equal(calls, []string{"LockBlob", "BackupRefersToBlob"}) {
		t.Fatalf("the purge's transaction made %v, want [LockBlob BackupRefersToBlob]", calls)
	}
}

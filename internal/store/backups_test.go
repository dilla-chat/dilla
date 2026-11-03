package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// web-2a L-SQL-21, F3 and Q27: the root object (kind 0) is written once. InsertBackup has no
// ON CONFLICT, so a second root for the same user is ErrConflict and the first row stands.
func TestTheRootBackupIsInsertOnly(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user := seedUser(ctx, t, repo).ID
			var none id.ID

			root := store.BackupRow{UserID: user, Kind: 0, DeviceID: none, ChunkSeq: 0, BlobID: digest(0x31), Created: 10}
			if err := repo.InsertBackup(ctx, root); err != nil {
				t.Fatalf("InsertBackup root: %v", err)
			}
			second := root
			second.BlobID, second.Created = digest(0x32), 11
			if err := repo.InsertBackup(ctx, second); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("a second InsertBackup of the root = %v, want ErrConflict", err)
			}
			got, err := repo.GetBackup(ctx, user, 0, none, 0)
			if err != nil {
				t.Fatalf("GetBackup root: %v", err)
			}
			if got.UserID != user || got.Kind != 0 || got.DeviceID != none || got.ChunkSeq != 0 ||
				!bytes.Equal(got.BlobID, digest(0x31)) || got.ManifestSig != nil || got.Created != 10 {
				t.Fatalf("GetBackup root = %+v, want the first row unchanged", got)
			}
			if _, err := repo.GetBackup(ctx, user, 1, none, 0); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetBackup of an absent state object = %v, want ErrNotFound", err)
			}
			if _, err := repo.GetBackup(ctx, seedUser(ctx, t, repo).ID, 0, none, 0); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetBackup of another user's root = %v, want ErrNotFound", err)
			}

			// The conflict aborts the transaction it happens in as a whole (task 6 relies on this:
			// on Postgres a unique violation aborts the transaction, so the existing row is read
			// after the rollback).
			err = repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.PutBlob(ctx, store.BlobRow{BlobID: digest(0x33), Size: 103, StorageRef: "fs:x", Created: 12}); err != nil {
					return err
				}
				again := root
				again.BlobID = digest(0x33)
				return tx.InsertBackup(ctx, again)
			})
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("Tx with a conflicting InsertBackup = %v, want ErrConflict", err)
			}
			if _, err := repo.GetBlob(ctx, digest(0x33)); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("the rolled-back transaction left its blobs row: %v", err)
			}

			// PutBackup (the upsert) still replaces the state object.
			state := store.BackupRow{UserID: user, Kind: 1, DeviceID: none, BlobID: digest(0x34), Created: 13}
			if err := repo.PutBackup(ctx, state); err != nil {
				t.Fatalf("PutBackup state: %v", err)
			}
			state.BlobID, state.Created = digest(0x35), 14
			if err := repo.PutBackup(ctx, state); err != nil {
				t.Fatalf("PutBackup state again: %v", err)
			}
			got, err = repo.GetBackup(ctx, user, 1, none, 0)
			if err != nil || !bytes.Equal(got.BlobID, digest(0x35)) || got.Created != 14 {
				t.Fatalf("GetBackup state = %+v, %v; want the replaced row", got, err)
			}
		})
	}
}

// BackupRefersToBlob looks at every user's rows of every kind.
func TestBackupRefersToBlobSeesEveryKindAndUser(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user, other := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			dev := seedDevice(ctx, t, repo, user)
			var none id.ID

			if err := repo.PutBackup(ctx, store.BackupRow{UserID: user, Kind: 1, DeviceID: none, BlobID: digest(0x41), Created: 1}); err != nil {
				t.Fatalf("PutBackup state: %v", err)
			}
			if err := repo.PutBackup(ctx, store.BackupRow{UserID: user, Kind: 2, DeviceID: dev, ChunkSeq: 3, BlobID: digest(0x42), Created: 1}); err != nil {
				t.Fatalf("PutBackup chunk: %v", err)
			}
			for _, b := range [][]byte{digest(0x41), digest(0x42)} {
				if ok, err := repo.BackupRefersToBlob(ctx, b); err != nil || !ok {
					t.Fatalf("BackupRefersToBlob(%x) = %v, %v; want true", b[:2], ok, err)
				}
			}
			if ok, err := repo.BackupRefersToBlob(ctx, digest(0x43)); err != nil || ok {
				t.Fatalf("BackupRefersToBlob of an unknown blob = %v, %v; want false", ok, err)
			}
			if err := repo.PutBackup(ctx, store.BackupRow{UserID: user, Kind: 1, DeviceID: none, BlobID: digest(0x44), Created: 2}); err != nil {
				t.Fatalf("PutBackup state replaced: %v", err)
			}
			if ok, err := repo.BackupRefersToBlob(ctx, digest(0x41)); err != nil || ok {
				t.Fatalf("BackupRefersToBlob of the replaced state = %v, %v; want false", ok, err)
			}
			if err := repo.PutBackup(ctx, store.BackupRow{UserID: other, Kind: 1, DeviceID: none, BlobID: digest(0x41), Created: 3}); err != nil {
				t.Fatalf("PutBackup other's state: %v", err)
			}
			if ok, err := repo.BackupRefersToBlob(ctx, digest(0x41)); err != nil || !ok {
				t.Fatalf("BackupRefersToBlob across users = %v, %v; want true", ok, err)
			}
		})
	}
}

// LockBlob (BACKUPS-RECOVERY-05) is refused outside Tx on both engines, and inside Tx locks the
// blobs row whether it exists or not (an absent row locks nothing and is not an error).
func TestLockBlobRequiresTx(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			if err := repo.LockBlob(ctx, digest(0x51)); err == nil {
				t.Fatal("LockBlob outside Tx succeeded")
			}
			if err := repo.PutBlob(ctx, store.BlobRow{BlobID: digest(0x51), Size: 1, StorageRef: "fs:x", Created: 1}); err != nil {
				t.Fatalf("PutBlob: %v", err)
			}
			err := repo.Tx(ctx, func(tx store.Repository) error {
				if err := tx.LockBlob(ctx, digest(0x51)); err != nil {
					return err
				}
				return tx.LockBlob(ctx, digest(0x52))
			})
			if err != nil {
				t.Fatalf("LockBlob in Tx: %v", err)
			}
		})
	}
}

// Boundary ruling 2 (task 6 scan): blobs.quota_bytes_per_user counts the user's backup objects
// with their attachments, each distinct blob once; a replaced state object drops out of the count
// with its row, and another user's objects never count.
func TestUserBlobBytesCountsTheUsersBackupObjects(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			ch := seedChannel(ctx, t, repo)
			user, other := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			dev := seedDevice(ctx, t, repo, user)
			var none id.ID
			for b, size := range map[byte]uint64{0x71: 103, 0x72: 137, 0x73: 200, 0x74: 1000} {
				if err := repo.PutBlob(ctx, store.BlobRow{BlobID: digest(b), Size: size, StorageRef: "fs:x", Created: 1}); err != nil {
					t.Fatalf("PutBlob %x: %v", b, err)
				}
			}
			want := func(what string, u id.ID, n int64) {
				t.Helper()
				if used, err := repo.UserBlobBytes(ctx, u); err != nil || used != n {
					t.Fatalf("%s: UserBlobBytes = %d, %v; want %d", what, used, err, n)
				}
			}
			if err := repo.InsertBackup(ctx, store.BackupRow{UserID: user, Kind: 0, DeviceID: none, BlobID: digest(0x71), Created: 2}); err != nil {
				t.Fatalf("InsertBackup: %v", err)
			}
			if err := repo.PutBackup(ctx, store.BackupRow{UserID: user, Kind: 1, DeviceID: none, BlobID: digest(0x72), Created: 2}); err != nil {
				t.Fatalf("PutBackup: %v", err)
			}
			want("root and state", user, 240)
			if err := repo.PutBlobRef(ctx, digest(0x74), ch, dev, "", 3); err != nil {
				t.Fatalf("PutBlobRef: %v", err)
			}
			want("root, state and an attachment", user, 1240)
			if err := repo.PutBackup(ctx, store.BackupRow{UserID: user, Kind: 1, DeviceID: none, BlobID: digest(0x73), Created: 4}); err != nil {
				t.Fatalf("PutBackup replace: %v", err)
			}
			want("the replaced state drops out", user, 1303)
			// The same bytes as an attachment and as the state object count once.
			if err := repo.PutBackup(ctx, store.BackupRow{UserID: user, Kind: 1, DeviceID: none, BlobID: digest(0x74), Created: 5}); err != nil {
				t.Fatalf("PutBackup the attachment's bytes: %v", err)
			}
			want("a blob named twice counts once", user, 1103)
			want("another user before any object", other, 0)
			if err := repo.PutBackup(ctx, store.BackupRow{UserID: other, Kind: 1, DeviceID: none, BlobID: digest(0x71), Created: 6}); err != nil {
				t.Fatalf("PutBackup other: %v", err)
			}
			want("another user's state", other, 103)
			want("the first user, unchanged by another user's state", user, 1103)
		})
	}
}

// L-SQL-21's guard: ListCollectableBlobs never lists a blob a backups row names, whatever its
// unref_since. The control blob, marked the same way and named by no backup, is listed.
func TestABackupBlobIsNeverCollectable(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user := seedUser(ctx, t, repo).ID
			var none id.ID
			for _, b := range [][]byte{digest(0x51), digest(0x52)} {
				if err := repo.PutBlob(ctx, store.BlobRow{BlobID: b, Size: 103, StorageRef: "fs:x", Created: 1}); err != nil {
					t.Fatalf("PutBlob: %v", err)
				}
				if err := repo.MarkBlobUnreferenced(ctx, b, 5); err != nil {
					t.Fatalf("MarkBlobUnreferenced: %v", err)
				}
			}
			if err := repo.InsertBackup(ctx, store.BackupRow{UserID: user, Kind: 0, DeviceID: none, BlobID: digest(0x51), Created: 6}); err != nil {
				t.Fatalf("InsertBackup: %v", err)
			}
			rows, err := repo.ListCollectableBlobs(ctx, 100, 10)
			if err != nil {
				t.Fatalf("ListCollectableBlobs: %v", err)
			}
			if len(rows) != 1 || !bytes.Equal(rows[0].BlobID, digest(0x52)) {
				firsts := make([]byte, 0, len(rows))
				for _, r := range rows {
					firsts = append(firsts, r.BlobID[0])
				}
				t.Fatalf("ListCollectableBlobs listed %d rows (first bytes %x); want only the control blob 52: the root object's blob must never be listed", len(rows), firsts)
			}
		})
	}
}

// The same guard seen through the real sweeper: a root object whose blob is marked unreferenced
// survives two grace windows with its file and its row; the control blob is collected.
func TestTheSweeperNeverCollectsABackupObject(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			bs, err := blob.Open(t.TempDir(), "fs")
			if err != nil {
				t.Fatalf("blob.Open: %v", err)
			}
			t.Cleanup(func() { _ = bs.Close() })
			clk := clock.NewFake(time.Unix(1_790_000_000, 0).UTC())
			sweeper := blob.NewSweeper(repo, bs, clk, 24*time.Hour, 24*time.Hour, time.Hour, slog.New(slog.DiscardHandler))
			user := seedUser(ctx, t, repo).ID
			var none id.ID

			keep := func(payload []byte) []byte {
				t.Helper()
				sum := sha256.Sum256(payload)
				if _, _, err := bs.Put(ctx, sum[:], bytes.NewReader(payload), int64(len(payload))); err != nil {
					t.Fatalf("Put: %v", err)
				}
				if err := repo.PutBlob(ctx, store.BlobRow{
					BlobID: sum[:], Size: uint64(len(payload)), StorageRef: blob.StorageRef("fs", sum[:]), Created: clk.Now().Unix(),
				}); err != nil {
					t.Fatalf("PutBlob: %v", err)
				}
				if err := repo.MarkBlobUnreferenced(ctx, sum[:], clk.Now().Unix()); err != nil {
					t.Fatalf("MarkBlobUnreferenced: %v", err)
				}
				return sum[:]
			}
			rootID := keep(bytes.Repeat([]byte{0x61}, 103))
			staleID := keep(bytes.Repeat([]byte{0x62}, 140))
			if err := repo.InsertBackup(ctx, store.BackupRow{UserID: user, Kind: 0, DeviceID: none, BlobID: rootID, Created: clk.Now().Unix()}); err != nil {
				t.Fatalf("InsertBackup: %v", err)
			}

			clk.Advance(48 * time.Hour)
			if n, err := sweeper.SweepOnce(ctx); err != nil || n != 1 {
				t.Fatalf("SweepOnce = (%d, %v), want (1, nil): only the blob no backup names goes", n, err)
			}
			if _, err := bs.Stat(rootID); err != nil {
				t.Fatalf("the root object's file was unlinked: %v", err)
			}
			if _, err := repo.GetBlob(ctx, rootID); err != nil {
				t.Fatalf("the root object's blobs row was deleted: %v", err)
			}
			if _, err := bs.Stat(staleID); !errors.Is(err, blob.ErrNotFound) {
				t.Fatalf("the unreferenced control blob survived the sweep: %v", err)
			}
		})
	}
}

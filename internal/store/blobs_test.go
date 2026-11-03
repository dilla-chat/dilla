package store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// seedChannel creates one live text channel in a fresh community.
func seedChannel(ctx context.Context, t *testing.T, repo store.Repository) id.ID {
	t.Helper()
	ch := channelIn(seedCommunity(ctx, t, repo), 0, 0, 0, "files", 0)
	if err := repo.CreateChannel(ctx, ch); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	return ch.ID
}

// seedDevice creates one device for user.
func seedDevice(ctx context.Context, t *testing.T, repo store.Repository, user id.ID) id.ID {
	t.Helper()
	did := id.New()
	if err := repo.CreateDevice(ctx, store.DeviceRow{
		ID: did, UserID: user, DSKPub: make([]byte, 32), CredentialBlob: []byte{1},
		LastSeen: 1, Created: 1,
	}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return did
}

func digest(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// C7 (fix wave): blobs.store_max_bytes bounds what the instance holds on disk, so InstanceBlobBytes
// counts every blob row, referenced or not (an orphan and a blob whose last reference was deleted
// still occupy their bytes until the sweeper collects them).
func TestInstanceBlobBytesCountsEveryRow(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			if n, err := repo.InstanceBlobBytes(ctx); err != nil || n != 0 {
				t.Fatalf("an empty instance = %d, %v; want 0", n, err)
			}
			for i, size := range []uint64{10, 20, 4000} {
				if err := repo.PutBlob(ctx, store.BlobRow{BlobID: digest(byte(0x70 + i)), Size: size, StorageRef: "fs:x", Created: 1}); err != nil {
					t.Fatalf("PutBlob: %v", err)
				}
			}
			if err := repo.MarkBlobUnreferenced(ctx, digest(0x72), 5); err != nil {
				t.Fatalf("MarkBlobUnreferenced: %v", err)
			}
			if n, err := repo.InstanceBlobBytes(ctx); err != nil || n != 4030 {
				t.Fatalf("InstanceBlobBytes = %d, %v; want 4030 (unreferenced rows included)", n, err)
			}
		})
	}
}

// Plan 2 task 10: 00010_blobs.sql's blobs, blob_refs and blob_tombstones, with
// P2-D16's ClearBlobUnreferenced and P2-D17's GetBlobRef, on both engines.
func TestBlobReferencesAndCollection(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			chA, chB := seedChannel(ctx, t, repo), seedChannel(ctx, t, repo)
			alice := seedUser(ctx, t, repo).ID
			laptop, phone := seedDevice(ctx, t, repo, alice), seedDevice(ctx, t, repo, alice)
			x, y := digest(0xa1), digest(0xb2)

			if _, err := repo.GetBlob(ctx, x); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetBlob before any upload = %v, want ErrNotFound", err)
			}
			// A reference needs its blob: blob_refs.blob_id is a foreign key.
			if err := repo.PutBlobRef(ctx, x, chA, laptop, "", 10); err == nil {
				t.Fatal("PutBlobRef without a blobs row succeeded")
			}
			row := store.BlobRow{BlobID: x, Size: 1000, StorageRef: "fs:att/a1/a1/x", Created: 10}
			if err := repo.PutBlob(ctx, row); err != nil {
				t.Fatalf("PutBlob: %v", err)
			}
			// Content addressing: a second PutBlob of the same id is a no-op, not
			// a conflict, and does not overwrite the first row.
			if err := repo.PutBlob(ctx, store.BlobRow{BlobID: x, Size: 1, StorageRef: "other", Created: 99}); err != nil {
				t.Fatalf("PutBlob again: %v", err)
			}
			got, err := repo.GetBlob(ctx, x)
			if err != nil {
				t.Fatalf("GetBlob: %v", err)
			}
			if !bytes.Equal(got.BlobID, x) || got.Size != 1000 || got.StorageRef != "fs:att/a1/a1/x" ||
				got.Created != 10 || got.UnrefSince != nil {
				t.Fatalf("GetBlob = %+v", got)
			}

			if err := repo.PutBlobRef(ctx, x, chA, laptop, "image/png", 11); err != nil {
				t.Fatalf("PutBlobRef A: %v", err)
			}
			// The same (blob, channel) again is idempotent and keeps the first uploader.
			if err := repo.PutBlobRef(ctx, x, chA, phone, "", 12); err != nil {
				t.Fatalf("PutBlobRef A again: %v", err)
			}
			ref, err := repo.GetBlobRef(ctx, x, chA)
			if err != nil {
				t.Fatalf("GetBlobRef: %v", err)
			}
			if !bytes.Equal(ref.BlobID, x) || ref.ChannelID != chA || ref.UploaderDevice != laptop ||
				ref.Mime != "image/png" || ref.Created != 11 {
				t.Fatalf("GetBlobRef = %+v", ref)
			}
			if _, err := repo.GetBlobRef(ctx, x, chB); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetBlobRef in a channel with no reference = %v, want ErrNotFound", err)
			}
			if err := repo.PutBlobRef(ctx, x, chB, phone, "", 13); err != nil {
				t.Fatalf("PutBlobRef B: %v", err)
			}
			if n, err := repo.CountBlobRefs(ctx, x); err != nil || n != 2 {
				t.Fatalf("CountBlobRefs = %d, %v; want 2", n, err)
			}

			// Quota: each distinct blob a user uploaded counts once, from any of
			// their devices and however many channels it is in.
			if err := repo.PutBlob(ctx, store.BlobRow{BlobID: y, Size: 24, StorageRef: "fs:y", Created: 14}); err != nil {
				t.Fatalf("PutBlob y: %v", err)
			}
			if err := repo.PutBlobRef(ctx, y, chA, phone, "", 14); err != nil {
				t.Fatalf("PutBlobRef y: %v", err)
			}
			if used, err := repo.UserBlobBytes(ctx, alice); err != nil || used != 1024 {
				t.Fatalf("UserBlobBytes = %d, %v; want 1024", used, err)
			}
			if used, err := repo.UserBlobBytes(ctx, id.New()); err != nil || used != 0 {
				t.Fatalf("UserBlobBytes of a user with no upload = %d, %v; want 0", used, err)
			}

			// UserReferencesBlob: true for the uploader of a reference, from any device, false for
			// another user and for a blob nobody references.
			if ok, err := repo.UserReferencesBlob(ctx, alice, y); err != nil || !ok {
				t.Fatalf("UserReferencesBlob(alice, y) = %v, %v; want true", ok, err)
			}
			if ok, err := repo.UserReferencesBlob(ctx, id.New(), y); err != nil || ok {
				t.Fatalf("UserReferencesBlob(stranger, y) = %v, %v; want false", ok, err)
			}
			if ok, err := repo.UserReferencesBlob(ctx, alice, []byte("no such blob")); err != nil || ok {
				t.Fatalf("UserReferencesBlob(alice, unknown) = %v, %v; want false", ok, err)
			}

			// Marking is refused while any reference stands.
			if err := repo.DeleteBlobRef(ctx, x, chA); err != nil {
				t.Fatalf("DeleteBlobRef A: %v", err)
			}
			if err := repo.MarkBlobUnreferenced(ctx, x, 20); err != nil {
				t.Fatalf("MarkBlobUnreferenced: %v", err)
			}
			if got, _ := repo.GetBlob(ctx, x); got.UnrefSince != nil {
				t.Fatalf("unref_since = %d while channel B still references the blob", *got.UnrefSince)
			}
			// Deleting an absent reference is not an error.
			if err := repo.DeleteBlobRef(ctx, x, chA); err != nil {
				t.Fatalf("DeleteBlobRef of a gone reference = %v", err)
			}
			if err := repo.DeleteBlobRef(ctx, x, chB); err != nil {
				t.Fatalf("DeleteBlobRef B: %v", err)
			}
			if err := repo.MarkBlobUnreferenced(ctx, x, 30); err != nil {
				t.Fatalf("MarkBlobUnreferenced: %v", err)
			}
			// A second mark keeps the first time, so a repeated delete cannot
			// push collection back.
			if err := repo.MarkBlobUnreferenced(ctx, x, 40); err != nil {
				t.Fatalf("MarkBlobUnreferenced again: %v", err)
			}
			if got, _ := repo.GetBlob(ctx, x); got.UnrefSince == nil || *got.UnrefSince != 30 {
				t.Fatalf("unref_since = %v, want 30", got.UnrefSince)
			}

			// Collectable: unreferenced and marked strictly before the cutoff.
			if list, err := repo.ListCollectableBlobs(ctx, 30, 10); err != nil || len(list) != 0 {
				t.Fatalf("ListCollectableBlobs(30) = %v, %v; want none", list, err)
			}
			list, err := repo.ListCollectableBlobs(ctx, 31, 10)
			if err != nil || len(list) != 1 || !bytes.Equal(list[0].BlobID, x) {
				t.Fatalf("ListCollectableBlobs(31) = %+v, %v; want x", list, err)
			}

			// A new reference clears the mark (gap-47 §8.3, P2-D16).
			if err := repo.PutBlobRef(ctx, x, chB, laptop, "", 50); err != nil {
				t.Fatalf("PutBlobRef B again: %v", err)
			}
			if err := repo.ClearBlobUnreferenced(ctx, x); err != nil {
				t.Fatalf("ClearBlobUnreferenced: %v", err)
			}
			if got, _ := repo.GetBlob(ctx, x); got.UnrefSince != nil {
				t.Fatalf("unref_since = %d after a new reference", *got.UnrefSince)
			}
			if list, _ := repo.ListCollectableBlobs(ctx, 1<<40, 10); len(list) != 0 {
				t.Fatalf("a referenced blob is collectable: %+v", list)
			}

			// ON DELETE RESTRICT: the row cannot go while a reference stands.
			if err := repo.DeleteBlob(ctx, x); err == nil {
				t.Fatal("DeleteBlob succeeded while a reference stood")
			}
			if err := repo.DeleteBlobRef(ctx, x, chB); err != nil {
				t.Fatalf("DeleteBlobRef: %v", err)
			}
			if err := repo.DeleteBlob(ctx, x); err != nil {
				t.Fatalf("DeleteBlob: %v", err)
			}
			if err := repo.DeleteBlob(ctx, x); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("DeleteBlob of a gone row = %v, want ErrNotFound", err)
			}

			// Tombstones.
			if tomb, err := repo.GetBlobTombstone(ctx, x); err != nil || tomb {
				t.Fatalf("GetBlobTombstone before a purge = %v, %v", tomb, err)
			}
			if err := repo.PutBlobTombstone(ctx, x, "takedown", alice, 60); err != nil {
				t.Fatalf("PutBlobTombstone: %v", err)
			}
			if err := repo.PutBlobTombstone(ctx, x, "again", alice, 61); err != nil {
				t.Fatalf("PutBlobTombstone again: %v", err)
			}
			if tomb, err := repo.GetBlobTombstone(ctx, x); err != nil || !tomb {
				t.Fatalf("GetBlobTombstone after a purge = %v, %v", tomb, err)
			}
		})
	}
}

// Plan 2 task 10 lands the backups table and OpsBackups (P2-D5) with the
// all-zero device sentinel of P2-D32.
func TestBackupsRoundTrip(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user := seedUser(ctx, t, repo).ID
			dev := seedDevice(ctx, t, repo, user)
			var none id.ID

			root := store.BackupRow{UserID: user, Kind: 0, DeviceID: none, ChunkSeq: 0, BlobID: digest(1), ManifestSig: bytes.Repeat([]byte{9}, 64), Created: 1}
			if err := repo.PutBackup(ctx, root); err != nil {
				t.Fatalf("PutBackup root: %v", err)
			}
			// One root per user: a second PutBackup of the same key replaces it,
			// where a nullable device_id in the key would have let SQLite keep both.
			root2 := root
			root2.BlobID, root2.Created = digest(2), 2
			if err := repo.PutBackup(ctx, root2); err != nil {
				t.Fatalf("PutBackup root again: %v", err)
			}
			// A root or state backup is not device scoped.
			if err := repo.PutBackup(ctx, store.BackupRow{UserID: user, Kind: 1, DeviceID: dev, BlobID: digest(3), Created: 3}); err == nil {
				t.Fatal("a kind 1 backup with a device id was accepted")
			}
			for seq := range uint64(2) {
				if err := repo.PutBackup(ctx, store.BackupRow{UserID: user, Kind: 2, DeviceID: dev, ChunkSeq: seq, BlobID: digest(byte(10 + seq)), Created: 4}); err != nil {
					t.Fatalf("PutBackup chunk %d: %v", seq, err)
				}
			}

			roots, err := repo.ListBackups(ctx, user, 0)
			if err != nil {
				t.Fatalf("ListBackups root: %v", err)
			}
			if len(roots) != 1 || !bytes.Equal(roots[0].BlobID, digest(2)) || roots[0].DeviceID != none ||
				roots[0].Created != 2 || len(roots[0].ManifestSig) != 64 {
				t.Fatalf("ListBackups root = %+v", roots)
			}
			chunks, err := repo.ListBackups(ctx, user, 2)
			if err != nil {
				t.Fatalf("ListBackups chunks: %v", err)
			}
			if len(chunks) != 2 || chunks[0].ChunkSeq != 0 || chunks[1].ChunkSeq != 1 ||
				chunks[0].DeviceID != dev || chunks[0].Kind != 2 || chunks[0].ManifestSig != nil {
				t.Fatalf("ListBackups chunks = %+v", chunks)
			}
		})
	}
}

// Plan 2 task 11: P2-D18's DeleteAllBlobRefs, and the reads behind the
// sweeper's reference-expiry phase — the retention policy of every live
// community that holds a reference (R28), a community's references older than a
// cutoff, and the references left in deleted channels.
func TestBlobReferenceExpiryAndPurge(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			alice := seedUser(ctx, t, repo).ID
			dev := seedDevice(ctx, t, repo, alice)
			cidA, cidB, cidEmpty := seedCommunity(ctx, t, repo), seedCommunity(ctx, t, repo), seedCommunity(ctx, t, repo)
			if err := repo.UpdateCommunityPolicy(ctx, cidA, []byte(`{"retention_days":3}`), 2); err != nil {
				t.Fatalf("UpdateCommunityPolicy: %v", err)
			}
			mk := func(cid id.ID) id.ID {
				ch := channelIn(cid, 0, 0, 0, "files", 0)
				if err := repo.CreateChannel(ctx, ch); err != nil {
					t.Fatalf("CreateChannel: %v", err)
				}
				return ch.ID
			}
			chA1, chA2, chB := mk(cidA), mk(cidA), mk(cidB)
			mk(cidEmpty)
			x, y, z := digest(0x11), digest(0x22), digest(0x33)
			for _, b := range [][]byte{x, y, z} {
				if err := repo.PutBlob(ctx, store.BlobRow{BlobID: b, Size: 10, StorageRef: "fs:b", Created: 1}); err != nil {
					t.Fatalf("PutBlob: %v", err)
				}
			}
			for _, r := range []struct {
				blob    []byte
				ch      id.ID
				created int64
			}{{x, chA1, 100}, {y, chA1, 200}, {x, chA2, 150}, {x, chB, 100}, {z, chB, 300}} {
				if err := repo.PutBlobRef(ctx, r.blob, r.ch, dev, "", r.created); err != nil {
					t.Fatalf("PutBlobRef: %v", err)
				}
			}

			// Only live communities that hold a reference are listed, each once,
			// with the policy exactly as stored.
			pols, err := repo.ListBlobRetentionPolicies(ctx)
			if err != nil {
				t.Fatalf("ListBlobRetentionPolicies: %v", err)
			}
			got := map[id.ID]string{}
			for _, p := range pols {
				got[p.CommunityID] = string(p.PolicyJSON)
			}
			if len(pols) != 2 || got[cidA] != `{"retention_days":3}` || got[cidB] != `{}` {
				t.Fatalf("ListBlobRetentionPolicies = %+v", pols)
			}

			// A community's references created strictly before the cutoff, oldest
			// first, bounded by limit; another community's never.
			refs, err := repo.ListExpiredBlobRefs(ctx, cidA, 200, 10)
			if err != nil {
				t.Fatalf("ListExpiredBlobRefs: %v", err)
			}
			if len(refs) != 2 || refs[0].Created != 100 || refs[0].ChannelID != chA1 || !bytes.Equal(refs[0].BlobID, x) ||
				refs[1].Created != 150 || refs[1].ChannelID != chA2 || refs[1].UploaderDevice != dev {
				t.Fatalf("ListExpiredBlobRefs(A, 200) = %+v", refs)
			}
			if refs, _ := repo.ListExpiredBlobRefs(ctx, cidA, 1<<40, 1); len(refs) != 1 || refs[0].Created != 100 {
				t.Fatalf("ListExpiredBlobRefs limit 1 = %+v", refs)
			}

			// No channel is deleted yet.
			if refs, err := repo.ListBlobRefsOfDeletedChannels(ctx, 10); err != nil || len(refs) != 0 {
				t.Fatalf("ListBlobRefsOfDeletedChannels before a delete = %+v, %v", refs, err)
			}
			if _, err := repo.DeleteChannelsOfCommunity(ctx, cidB, 500); err != nil {
				t.Fatalf("DeleteChannelsOfCommunity: %v", err)
			}
			refs, err = repo.ListBlobRefsOfDeletedChannels(ctx, 10)
			if err != nil || len(refs) != 2 || refs[0].ChannelID != chB || refs[0].Created != 100 || refs[1].Created != 300 {
				t.Fatalf("ListBlobRefsOfDeletedChannels = %+v, %v", refs, err)
			}
			if refs, _ := repo.ListBlobRefsOfDeletedChannels(ctx, 1); len(refs) != 1 {
				t.Fatalf("ListBlobRefsOfDeletedChannels limit 1 = %+v", refs)
			}

			// The purge's one statement: every reference to x, everywhere.
			n, err := repo.DeleteAllBlobRefs(ctx, x)
			if err != nil || n != 3 {
				t.Fatalf("DeleteAllBlobRefs = %d, %v; want 3", n, err)
			}
			if c, _ := repo.CountBlobRefs(ctx, x); c != 0 {
				t.Fatalf("CountBlobRefs after DeleteAllBlobRefs = %d", c)
			}
			if c, _ := repo.CountBlobRefs(ctx, y); c != 1 {
				t.Fatalf("DeleteAllBlobRefs touched another blob: %d left", c)
			}
			if n, err := repo.DeleteAllBlobRefs(ctx, x); err != nil || n != 0 {
				t.Fatalf("DeleteAllBlobRefs again = %d, %v; want 0", n, err)
			}
		})
	}
}

// Plan 2 task 12: ListBlobs is the backup's walk over the blob table, in
// blob_id byte order, resumed after the last id of the previous page.
func TestListBlobsWalksInBlobIDOrder(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			// Random ids, so the Postgres leg's shared database may hold other
			// rows: the assertions are about the relative order of these three.
			ids := make([][]byte, 3)
			for i := range ids {
				ids[i] = make([]byte, 32)
				if _, err := rand.Read(ids[i]); err != nil {
					t.Fatalf("rand: %v", err)
				}
				if err := repo.PutBlob(ctx, store.BlobRow{
					BlobID: ids[i], Size: uint64(i + 1), StorageRef: "fs:x", Created: int64(10 + i),
				}); err != nil {
					t.Fatalf("PutBlob: %v", err)
				}
			}
			slices.SortFunc(ids, bytes.Compare)
			var seen [][]byte
			var after []byte
			for {
				page, err := repo.ListBlobs(ctx, after, 2)
				if err != nil {
					t.Fatalf("ListBlobs: %v", err)
				}
				if len(page) > 2 {
					t.Fatalf("ListBlobs returned %d rows, limit 2", len(page))
				}
				if len(page) == 0 {
					break
				}
				for _, row := range page {
					if after != nil && bytes.Compare(row.BlobID, after) <= 0 {
						t.Fatalf("ListBlobs after %x returned %x", after, row.BlobID)
					}
					for _, want := range ids {
						if bytes.Equal(row.BlobID, want) {
							seen = append(seen, row.BlobID)
						}
					}
				}
				after = page[len(page)-1].BlobID
			}
			if len(seen) != 3 {
				t.Fatalf("the walk saw %d of the three blobs", len(seen))
			}
			for i := range ids {
				if !bytes.Equal(seen[i], ids[i]) {
					t.Fatalf("walk order %x, want %x", seen, ids)
				}
			}
			// An empty `after` starts at the beginning.
			first, err := repo.ListBlobs(ctx, nil, 1)
			if err != nil || len(first) != 1 {
				t.Fatalf("ListBlobs(nil, 1) = %v, %v", first, err)
			}
		})
	}
}

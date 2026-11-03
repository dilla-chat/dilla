package store_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// dilla-web-2b task 4 (L-SQL-31): a PUT's reference is pending until its uploader confirms it, a
// repeat keeps the first row, ListPendingBlobRefs lists only unconfirmed references created strictly
// before the cutoff, oldest first, and ConfirmBlobRef is idempotent and ErrNotFound for no row.
func TestPendingBlobReferences(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			ch := seedChannel(ctx, t, repo)
			alice := seedUser(ctx, t, repo).ID
			laptop := seedDevice(ctx, t, repo, alice)
			x, y, z := digest(0xc1), digest(0xc2), digest(0xc3)
			for _, b := range [][]byte{x, y, z} {
				if err := repo.PutBlob(ctx, store.BlobRow{BlobID: b, Size: 10, StorageRef: "fs:x", Created: 1}); err != nil {
					t.Fatalf("PutBlob: %v", err)
				}
			}
			if err := repo.PutPendingBlobRef(ctx, x, ch, laptop, "", 100); err != nil {
				t.Fatalf("PutPendingBlobRef x: %v", err)
			}
			if err := repo.PutPendingBlobRef(ctx, y, ch, laptop, "", 200); err != nil {
				t.Fatalf("PutPendingBlobRef y: %v", err)
			}
			if err := repo.PutBlobRef(ctx, z, ch, laptop, "", 50); err != nil {
				t.Fatalf("PutBlobRef z: %v", err)
			}

			ref, err := repo.GetBlobRef(ctx, x, ch)
			if err != nil || ref.Confirmed || ref.Created != 100 || ref.UploaderDevice != laptop {
				t.Fatalf("GetBlobRef x = %+v, %v; want a pending reference created at 100 by the laptop", ref, err)
			}
			if ref, err := repo.GetBlobRef(ctx, z, ch); err != nil || !ref.Confirmed {
				t.Fatalf("GetBlobRef z = %+v, %v; PutBlobRef writes a confirmed reference", ref, err)
			}
			// A pending repeat over a confirmed reference keeps the first row.
			if err := repo.PutPendingBlobRef(ctx, z, ch, laptop, "", 300); err != nil {
				t.Fatalf("PutPendingBlobRef z again: %v", err)
			}
			if ref, err := repo.GetBlobRef(ctx, z, ch); err != nil || !ref.Confirmed || ref.Created != 50 {
				t.Fatalf("GetBlobRef z after a repeat = %+v, %v; want the first, confirmed row", ref, err)
			}

			ids := func(rows []store.BlobRefRow) [][]byte {
				out := make([][]byte, 0, len(rows))
				for _, r := range rows {
					if r.Confirmed {
						t.Fatalf("ListPendingBlobRefs returned a confirmed row %x", r.BlobID)
					}
					out = append(out, r.BlobID)
				}
				return out
			}
			list := func(before int64, limit int32) [][]byte {
				rows, err := repo.ListPendingBlobRefs(ctx, before, limit)
				if err != nil {
					t.Fatalf("ListPendingBlobRefs(%d, %d): %v", before, limit, err)
				}
				return ids(rows)
			}
			eq := func(got, want [][]byte) bool {
				if len(got) != len(want) {
					return false
				}
				for i := range got {
					if !bytes.Equal(got[i], want[i]) {
						return false
					}
				}
				return true
			}
			if got := list(201, 10); !eq(got, [][]byte{x, y}) {
				t.Fatalf("pending before 201 = %x, want x then y", got)
			}
			if got := list(200, 10); !eq(got, [][]byte{x}) {
				t.Fatalf("pending before 200 = %x, want x only (strictly before)", got)
			}
			if got := list(1000, 1); !eq(got, [][]byte{x}) {
				t.Fatalf("pending with limit 1 = %x, want the oldest", got)
			}

			for range 2 {
				if err := repo.ConfirmBlobRef(ctx, x, ch); err != nil {
					t.Fatalf("ConfirmBlobRef x: %v", err)
				}
			}
			if ref, err := repo.GetBlobRef(ctx, x, ch); err != nil || !ref.Confirmed {
				t.Fatalf("GetBlobRef x after confirm = %+v, %v", ref, err)
			}
			if got := list(1000, 10); !eq(got, [][]byte{y}) {
				t.Fatalf("pending after confirming x = %x, want y only", got)
			}
			if err := repo.ConfirmBlobRef(ctx, digest(0xc9), ch); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("ConfirmBlobRef of no reference = %v, want ErrNotFound", err)
			}
		})
	}
}

// L-SQL-31: every reference stored before 00014 reads confirmed, so nothing uploaded before this
// plan is ever swept as pending.
func TestAReferenceStoredBeforeTheMigrationIsConfirmed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "before-00014.db")
	write, err := sqlite.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if _, err := p.UpTo(ctx, 13); err != nil {
		t.Fatalf("up to 13: %v", err)
	}
	read, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { repo.Close() })
	// users has no factor_owed column before 00017, so the repository's CreateUser cannot write
	// the owner and the uploader at schema 13: both rows are written by hand.
	cid := id.New()
	if err := repo.CreateCommunity(ctx, store.CommunityRow{
		ID: cid, Owner: userBefore17(ctx, t, write, "owner", 0), Name: "c", PolicyJSON: []byte(`{}`),
		PolicyVersion: 1, Created: 1,
	}); err != nil {
		t.Fatalf("CreateCommunity: %v", err)
	}
	channel := channelIn(cid, 0, 0, 0, "files", 0)
	if err := repo.CreateChannel(ctx, channel); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	ch := channel.ID
	user := userBefore17(ctx, t, write, "uploader", 0)
	dev := id.New()
	if _, err := write.ExecContext(ctx, `INSERT INTO devices
		(id, user_id, dsk_pub, tier, signer_tier, credential_blob, verified_at, revoked_at,
		 quarantined_at, quarantine_reason, last_seen, created)
		VALUES (?, ?, ?, 0, 0, ?, NULL, NULL, NULL, '', 1, 1)`,
		dev, user, make([]byte, 32), []byte{1}); err != nil {
		t.Fatalf("insert device at schema 13: %v", err)
	}
	x := digest(0xd1)
	if err := repo.PutBlob(ctx, store.BlobRow{BlobID: x, Size: 10, StorageRef: "fs:x", Created: 1}); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if err := repo.PutBlobRef(ctx, x, ch, dev, "", 10); err != nil {
		t.Fatalf("PutBlobRef at schema 13: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	ref, err := repo.GetBlobRef(ctx, x, ch)
	if err != nil || !ref.Confirmed {
		t.Fatalf("a reference stored before 00014 = %+v, %v; want confirmed", ref, err)
	}
	if rows, err := repo.ListPendingBlobRefs(ctx, 1<<40, 10); err != nil || len(rows) != 0 {
		t.Fatalf("ListPendingBlobRefs after the migration = %d rows, %v; want none", len(rows), err)
	}
}

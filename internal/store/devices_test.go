package store_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// seedDeviceAt creates one device of user with the given creation time and revocation.
func seedDeviceAt(ctx context.Context, t *testing.T, repo store.Repository, user id.ID, created int64, revokedAt *int64) id.ID {
	t.Helper()
	did := id.New()
	if err := repo.CreateDevice(ctx, store.DeviceRow{
		ID: did, UserID: user, DSKPub: make([]byte, 32), CredentialBlob: []byte{1},
		RevokedAt: revokedAt, LastSeen: created, Created: created,
	}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return did
}

// A listed device remains listed; an unlisted device stops consuming a slot after the cutoff.
// Revoked rows consume neither slot, including the one-hour creation allowance.
func TestLiveUnlistedDeviceCountsExcludeRevokedAndExpired(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user, other := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			listed := seedDeviceAt(ctx, t, repo, user, 9900, nil)
			seedDeviceAt(ctx, t, repo, user, 6400, nil) // live but expired at cutoff 6500
			seedDeviceAt(ctx, t, repo, user, 6500, nil) // boundary: live
			seedDeviceAt(ctx, t, repo, user, 9800, nil)
			revoked := int64(9999)
			seedDeviceAt(ctx, t, repo, user, 9700, &revoked)
			seedDeviceAt(ctx, t, repo, other, 9800, nil)
			if n, err := repo.CountLiveUnlistedDevicesByUser(ctx, user, []id.ID{listed}, 6500); err != nil || n != 2 {
				t.Fatalf("live unlisted count = %d, %v; want 2", n, err)
			}
			got, err := repo.ListLiveDeviceCreationsSince(ctx, user, []id.ID{listed}, 9000, 6500)
			if err != nil || !slices.Equal(got, []int64{9800, 9900}) {
				t.Fatalf("live creations = %v, %v; want [9800 9900]", got, err)
			}
		})
	}
}

// The lock must span count and insert. With one free slot, at most one registration commits.
func TestConcurrentRegistrationsCannotBothPassAtCapMinusOne(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user := seedUser(ctx, t, repo).ID
			seedDeviceAt(ctx, t, repo, user, 100, nil)
			start := make(chan struct{})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					results <- repo.Tx(ctx, func(tx store.Repository) error {
						if err := tx.LockUserForDeviceRegistration(ctx, user); err != nil {
							return err
						}
						n, err := tx.CountLiveDevicesByUser(ctx, user)
						if err != nil {
							return err
						}
						if n >= 2 {
							return store.ErrConflict
						}
						return tx.CreateDevice(ctx, store.DeviceRow{ID: id.New(), UserID: user, DSKPub: make([]byte, 32), CredentialBlob: []byte{1}, Created: 200})
					})
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			committed, refused := 0, 0
			for err := range results {
				switch {
				case err == nil:
					committed++
				case errors.Is(err, store.ErrConflict):
					refused++
				default:
					t.Fatalf("registration transaction: %v", err)
				}
			}
			if committed != 1 || refused != 1 {
				t.Fatalf("committed=%d refused=%d; want 1 and 1", committed, refused)
			}
			if n, err := repo.CountLiveDevicesByUser(ctx, user); err != nil || n != 2 {
				t.Fatalf("final live count=%d, %v; want 2", n, err)
			}
		})
	}
}

// L-SQL-21 for the per-user device cap (Q04): only unrevoked devices of the user count.
func TestCountLiveDevicesByUserSkipsRevokedAndOtherUsers(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user, other := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			if n, err := repo.CountLiveDevicesByUser(ctx, user); err != nil || n != 0 {
				t.Fatalf("a user with no device = %d, %v; want 0", n, err)
			}
			for range 3 {
				seedDeviceAt(ctx, t, repo, user, 100, nil)
			}
			revoked := int64(150)
			seedDeviceAt(ctx, t, repo, user, 120, &revoked)
			later := seedDeviceAt(ctx, t, repo, user, 130, nil)
			if err := repo.RevokeDevice(ctx, later, 160); err != nil {
				t.Fatalf("RevokeDevice: %v", err)
			}
			seedDeviceAt(ctx, t, repo, other, 100, nil)
			if n, err := repo.CountLiveDevicesByUser(ctx, user); err != nil || n != 3 {
				t.Fatalf("CountLiveDevicesByUser = %d, %v; want 3 (two revoked devices and another user's device excluded)", n, err)
			}
			if n, err := repo.CountLiveDevicesByUser(ctx, other); err != nil || n != 1 {
				t.Fatalf("CountLiveDevicesByUser(other) = %d, %v; want 1", n, err)
			}
		})
	}
}

// L-SQL-21 for the enrolment rate (Q04): every enrolment since the cutoff, inclusive, ascending,
// revoked devices included.
func TestListDeviceCreationsSinceIsAscendingAndKeepsRevokedDevices(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user, other := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			revoked := int64(6000)
			seedDeviceAt(ctx, t, repo, user, 7200, nil)
			seedDeviceAt(ctx, t, repo, user, 1000, nil)
			seedDeviceAt(ctx, t, repo, user, 5000, &revoked)
			seedDeviceAt(ctx, t, repo, user, 3600, nil)
			seedDeviceAt(ctx, t, repo, user, 3599, nil)
			seedDeviceAt(ctx, t, repo, other, 4000, nil)

			for _, c := range []struct {
				since int64
				want  []int64
			}{
				{3600, []int64{3600, 5000, 7200}},
				{0, []int64{1000, 3599, 3600, 5000, 7200}},
				{7201, []int64{}},
			} {
				got, err := repo.ListDeviceCreationsSince(ctx, user, c.since)
				if err != nil {
					t.Fatalf("ListDeviceCreationsSince(%d): %v", c.since, err)
				}
				if !slices.Equal(got, c.want) {
					t.Fatalf("ListDeviceCreationsSince(%d) = %v, want %v", c.since, got, c.want)
				}
			}
			if got, err := repo.ListDeviceCreationsSince(ctx, other, 0); err != nil || !slices.Equal(got, []int64{4000}) {
				t.Fatalf("ListDeviceCreationsSince(other) = %v, %v; want [4000]", got, err)
			}
		})
	}
}

// L-SQL-21 for the history route (L-HTTP-56): versions strictly after `after`, ascending,
// at most limit; out-of-range bounds answer nothing rather than everything.
func TestListDeviceListsAfterPagesTheHistoryInOrder(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			user, other := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			put := func(u id.ID, v uint64) {
				t.Helper()
				if err := repo.PutDeviceList(ctx, store.DeviceListRow{
					UserID: u, Version: v, Blob: []byte{byte(v)}, SSKSignature: bytes.Repeat([]byte{9}, 64),
					PrevHash: bytes.Repeat([]byte{byte(v - 1)}, 32), Created: int64(100 + v),
				}); err != nil {
					t.Fatalf("PutDeviceList v%d: %v", v, err)
				}
			}
			// Inserted out of order, so the ORDER BY is what sorts them.
			for _, v := range []uint64{1, 2, 3, 5, 4} {
				put(user, v)
			}
			put(other, 1)
			put(other, 2)
			versions := func(rows []store.DeviceListRow) []uint64 {
				out := make([]uint64, 0, len(rows))
				for _, r := range rows {
					out = append(out, r.Version)
				}
				return out
			}

			got, err := repo.ListDeviceListsAfter(ctx, user, 2, 64)
			if err != nil || !slices.Equal(versions(got), []uint64{3, 4, 5}) {
				t.Fatalf("after 2 = %v, %v; want [3 4 5]", versions(got), err)
			}
			first := got[0]
			if first.UserID != user || !bytes.Equal(first.Blob, []byte{3}) || len(first.SSKSignature) != 64 ||
				!bytes.Equal(first.PrevHash, bytes.Repeat([]byte{2}, 32)) || first.Created != 103 {
				t.Fatalf("row v3 = %+v, want every column as stored", first)
			}
			if got, err := repo.ListDeviceListsAfter(ctx, user, 0, 2); err != nil || !slices.Equal(versions(got), []uint64{1, 2}) {
				t.Fatalf("after 0 limit 2 = %v, %v; want [1 2]", versions(got), err)
			}
			for _, c := range []struct {
				name  string
				after uint64
				limit int32
			}{
				{"after the newest", 5, 64},
				{"after the largest uint64", math.MaxUint64, 64},
				{"after above MaxInt64", math.MaxInt64 + 1, 64},
				{"limit 0", 0, 0},
				{"a negative limit", 0, -1},
			} {
				got, err := repo.ListDeviceListsAfter(ctx, user, c.after, c.limit)
				if err != nil || got == nil || len(got) != 0 {
					t.Fatalf("%s = %v (nil: %t), %v; want a non-nil empty slice", c.name, versions(got), got == nil, err)
				}
			}
			// The guarded bounds answer without querying. The harness has no query counter, so a
			// cancelled context stands in for one: database/sql checks the context before it sends a
			// statement, so a call that queried would answer context.Canceled instead of [], nil.
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			for _, c := range []struct {
				name  string
				after uint64
				limit int32
			}{
				{"after the largest uint64", math.MaxUint64, 64},
				{"limit 0", 0, 0},
				{"a negative limit", 0, -1},
			} {
				got, err := repo.ListDeviceListsAfter(cancelled, user, c.after, c.limit)
				if err != nil || got == nil || len(got) != 0 {
					t.Fatalf("%s with a cancelled context = %v (nil: %t), %v; want a non-nil empty slice and no query", c.name, versions(got), got == nil, err)
				}
			}
			// Control: the same cancelled context on an in-range call does query, and fails.
			if _, err := repo.ListDeviceListsAfter(cancelled, user, 0, 64); err == nil {
				t.Fatalf("an in-range call with a cancelled context = nil error; the cancelled-context probe proves nothing")
			}
			if got, err := repo.ListDeviceListsAfter(ctx, other, 0, 64); err != nil || !slices.Equal(versions(got), []uint64{1, 2}) {
				t.Fatalf("other after 0 = %v, %v; want [1 2]", versions(got), err)
			}
		})
	}
}

// A device label is the owner's own word for a device. Only the owner's row changes: another
// user's id answers ErrNotFound and leaves the label alone; nil clears it; the engine keeps it
// to 64 characters.
func TestSetDeviceLabelIsOwnerOnlyAndRoundTrips(t *testing.T) {
	for engine, repo := range engines(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			owner, other := seedUser(ctx, t, repo).ID, seedUser(ctx, t, repo).ID
			did := seedDeviceAt(ctx, t, repo, owner, 100, nil)
			if got, err := repo.GetDevice(ctx, did); err != nil || got.Label != nil {
				t.Fatalf("a new device's label = %v, %v; want nil", got.Label, err)
			}
			label := "work laptop"
			if err := repo.SetDeviceLabel(ctx, owner, did, &label); err != nil {
				t.Fatalf("SetDeviceLabel: %v", err)
			}
			got, err := repo.GetDevice(ctx, did)
			if err != nil || got.Label == nil || *got.Label != label {
				t.Fatalf("GetDevice label = %v, %v; want %q", got.Label, err, label)
			}
			rows, err := repo.ListDevicesByUser(ctx, owner)
			if err != nil || len(rows) != 1 || rows[0].Label == nil || *rows[0].Label != label {
				t.Fatalf("ListDevicesByUser = %+v, %v; want the label", rows, err)
			}
			stolen := "mine now"
			if err := repo.SetDeviceLabel(ctx, other, did, &stolen); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("another user's SetDeviceLabel = %v, want ErrNotFound", err)
			}
			if got, _ := repo.GetDevice(ctx, did); got.Label == nil || *got.Label != label {
				t.Fatalf("label after a foreign write = %v, want %q", got.Label, label)
			}
			if err := repo.SetDeviceLabel(ctx, owner, id.New(), &label); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("an unknown device = %v, want ErrNotFound", err)
			}
			long := strings.Repeat("x", 65)
			if err := repo.SetDeviceLabel(ctx, owner, did, &long); err == nil {
				t.Fatal("a 65-character label was stored; the column allows 64")
			}
			if err := repo.SetDeviceLabel(ctx, owner, did, nil); err != nil {
				t.Fatalf("clearing: %v", err)
			}
			if got, _ := repo.GetDevice(ctx, did); got.Label != nil {
				t.Fatalf("label after clearing = %v, want nil", got.Label)
			}
			// A removed device keeps the name it had and takes no new one.
			if err := repo.SetDeviceLabel(ctx, owner, did, &label); err != nil {
				t.Fatalf("naming again: %v", err)
			}
			if err := repo.RevokeDevice(ctx, did, 200); err != nil {
				t.Fatalf("RevokeDevice: %v", err)
			}
			renamed := "renamed after removal"
			if err := repo.SetDeviceLabel(ctx, owner, did, &renamed); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("SetDeviceLabel on a revoked device = %v, want ErrNotFound", err)
			}
			if err := repo.SetDeviceLabel(ctx, owner, did, nil); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("clearing a revoked device = %v, want ErrNotFound", err)
			}
			if got, _ := repo.GetDevice(ctx, did); got.Label == nil || *got.Label != label {
				t.Fatalf("label of a revoked device = %v, want %q kept", got.Label, label)
			}
		})
	}
}

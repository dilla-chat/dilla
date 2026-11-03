package store_test

// The KeyPackage directory's one concurrency invariant: N callers taking from a directory of N
// packages get N DISTINCT packages, never one twice.
//
// It lives here, beside `engines`, rather than in internal/ds, because the rule is the QUERY's and
// the two engines encode it differently. On SQLite writes serialise through the single write
// connection, so the sub-select cannot hand one row to two callers. On Postgres they do not: two
// concurrent takes read the same snapshot, and only `FOR UPDATE SKIP LOCKED` makes the second
// caller skip the row the first is consuming. `ProposeAddBatch` makes concurrent takes for one
// device routine, and a KeyPackage served twice is two joiners holding the same init key.
//
// The Postgres leg is skipped locally and runs in CI's service container.

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

func TestConcurrentTakesNeverServeOneKeyPackageTwice(t *testing.T) {
	ctx := context.Background()
	for name, repo := range engines(t) {
		t.Run(name, func(t *testing.T) {
			const n = 16
			user := seedUser(ctx, t, repo)
			device := store.DeviceRow{
				ID: id.New(), UserID: user.ID, DSKPub: make([]byte, 32),
				CredentialBlob: []byte{1}, LastSeen: 1_700_000_000, Created: 1_700_000_000,
			}
			if err := repo.CreateDevice(ctx, device); err != nil {
				t.Fatalf("CreateDevice: %v", err)
			}
			rows := make([]store.KeyPackageRow, 0, n)
			for i := range n {
				ref := make([]byte, 32)
				ref[0], ref[1] = byte(i), byte(i>>8)
				rows = append(rows, store.KeyPackageRow{
					DeviceID: device.ID, KPRef: ref, Blob: []byte{byte(i)}, LastResort: 0,
					Expires: 2_000_000_000, Created: 1_700_000_000,
				})
			}
			if err := repo.PutKeyPackages(ctx, device.ID, rows); err != nil {
				t.Fatalf("PutKeyPackages: %v", err)
			}

			refs := make(chan string, n)
			var wg sync.WaitGroup
			for range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					kp, err := repo.TakeKeyPackage(ctx, device.ID, 1_700_000_001)
					if err != nil {
						return // a take that found nothing is not a double-serve
					}
					refs <- string(kp.KPRef)
				}()
			}
			wg.Wait()
			close(refs)

			seen := map[string]struct{}{}
			for ref := range refs {
				if _, dup := seen[ref]; dup {
					t.Fatal("one KeyPackage was served to two callers")
				}
				seen[ref] = struct{}{}
			}
			if len(seen) != n {
				t.Fatalf("%d distinct packages served, want %d", len(seen), n)
			}
		})
	}
}

// DeleteKeyPackage takes exactly the named package out of the named device's directory — an
// unconsumed ordinary one, the last-resort one alike — and touches no other row. The delivery
// service uses it for a package it will never propose (one not bound to its device), which for a
// last-resort package is the only way out of the directory: a take never consumes it.
func TestDeleteKeyPackageRemovesExactlyTheNamedPackage(t *testing.T) {
	ctx := context.Background()
	for name, repo := range engines(t) {
		t.Run(name, func(t *testing.T) {
			user := seedUser(ctx, t, repo)
			devices := make([]id.ID, 2)
			for i := range devices {
				devices[i] = id.New()
				if err := repo.CreateDevice(ctx, store.DeviceRow{
					ID: devices[i], UserID: user.ID, DSKPub: make([]byte, 32),
					CredentialBlob: []byte{1}, LastSeen: 1_700_000_000, Created: 1_700_000_000,
				}); err != nil {
					t.Fatalf("CreateDevice: %v", err)
				}
			}
			ref := func(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }
			for _, d := range devices {
				if err := repo.PutKeyPackages(ctx, d, []store.KeyPackageRow{
					{DeviceID: d, KPRef: ref(1), Blob: []byte{1}, Expires: 2_000_000_000, Created: 1_700_000_000},
					{DeviceID: d, KPRef: ref(9), Blob: []byte{9}, LastResort: 1, Expires: 2_000_000_000, Created: 1_700_000_000},
				}); err != nil {
					t.Fatalf("PutKeyPackages: %v", err)
				}
			}
			const now = 1_700_000_001
			if err := repo.DeleteKeyPackage(ctx, devices[0], ref(1)); err != nil {
				t.Fatalf("DeleteKeyPackage (ordinary): %v", err)
			}
			if n, _ := repo.CountKeyPackages(ctx, devices[0], now); n != 0 {
				t.Fatalf("device 0 has %d ordinary packages after the delete, want 0", n)
			}
			if n, _ := repo.CountKeyPackages(ctx, devices[1], now); n != 1 {
				t.Fatalf("device 1 has %d ordinary packages, want its own 1 untouched", n)
			}
			// Device 0's take now falls back to its last-resort package; deleting that leaves nothing.
			if kp, err := repo.TakeKeyPackage(ctx, devices[0], now); err != nil || kp.LastResort != 1 {
				t.Fatalf("take after deleting the ordinary package = %+v, %v; want the last-resort one", kp, err)
			}
			if err := repo.DeleteKeyPackage(ctx, devices[0], ref(9)); err != nil {
				t.Fatalf("DeleteKeyPackage (last resort): %v", err)
			}
			if _, err := repo.TakeKeyPackage(ctx, devices[0], now); err == nil {
				t.Fatal("device 0's directory must be empty after both deletes")
			}
			// Deleting what is not there is not an error.
			if err := repo.DeleteKeyPackage(ctx, devices[0], ref(9)); err != nil {
				t.Fatalf("a second delete of the same package: %v", err)
			}
			if kp, err := repo.TakeKeyPackage(ctx, devices[1], now); err != nil || !bytes.Equal(kp.KPRef, ref(1)) {
				t.Fatalf("device 1's take = %+v, %v; want its own ordinary package", kp, err)
			}
		})
	}
}

// A device holds AT MOST ONE last-resort KeyPackage, and publishing a new one REPLACES the old.
//
// The rule lives here, in the adapters, because it is the only bound there is on the last-resort
// half of the directory: `CountKeyPackages` filters `last_resort = 0` by contract — the number it
// reports is what a client refills against, and the fallback package is not one a client should
// refill on — so the delivery service's directory cap cannot see a last-resort row at all. Without
// this rule `key_packages` is unique only on `(device_id, kp_ref)`, and an enrolled device that
// mints a fresh, valid last-resort package and publishes it in a loop adds one more unbounded row
// per call, each holding a full KeyPackage blob for its 90-day lifetime.
//
// Both engines are driven because both encode it: the replacement is a second statement in the
// same transaction as the insert, and the two adapters' write paths differ (SQLite's single
// writer, Postgres' MVCC). The Postgres leg is skipped locally and runs in CI's service container.
func TestANewLastResortKeyPackageReplacesTheOldOne(t *testing.T) {
	ctx := context.Background()
	for name, repo := range engines(t) {
		t.Run(name, func(t *testing.T) {
			const publishes = 8
			user := seedUser(ctx, t, repo)
			device := store.DeviceRow{
				ID: id.New(), UserID: user.ID, DSKPub: make([]byte, 32),
				CredentialBlob: []byte{1}, LastSeen: 1_700_000_000, Created: 1_700_000_000,
			}
			if err := repo.CreateDevice(ctx, device); err != nil {
				t.Fatalf("CreateDevice: %v", err)
			}

			// Two ORDINARY packages, which the rule must leave alone.
			ordinary := make([]store.KeyPackageRow, 0, 2)
			for i := range 2 {
				ref := make([]byte, 32)
				ref[0] = byte(i)
				ordinary = append(ordinary, store.KeyPackageRow{
					DeviceID: device.ID, KPRef: ref, Blob: []byte{byte(i)}, LastResort: 0,
					Expires: 2_000_000_000, Created: 1_700_000_000,
				})
			}
			if err := repo.PutKeyPackages(ctx, device.ID, ordinary); err != nil {
				t.Fatalf("PutKeyPackages (ordinary): %v", err)
			}

			// Eight DISTINCT last-resort packages, one per call — the shape a device that
			// republishes its last-resort package over and over produces.
			newest := make([]byte, 32)
			for i := range publishes {
				ref := make([]byte, 32)
				ref[0], ref[1] = 0xff, byte(i)
				if err := repo.PutKeyPackages(ctx, device.ID, []store.KeyPackageRow{{
					DeviceID: device.ID, KPRef: ref, Blob: []byte{0xff, byte(i)}, LastResort: 1,
					Expires: 2_000_000_000, Created: 1_700_000_000,
				}}); err != nil {
					t.Fatalf("PutKeyPackages (last resort %d): %v", i, err)
				}
				if got := countKeyPackageRows(ctx, t, name, repo, device.ID, 1); got != 1 {
					t.Fatalf("after %d last-resort publishes the device holds %d last-resort rows, want 1",
						i+1, got)
				}
				newest = ref
			}

			// The ordinary half is untouched: two rows, both still takeable.
			if got := countKeyPackageRows(ctx, t, name, repo, device.ID, 0); got != 2 {
				t.Fatalf("%d ordinary rows, want the 2 that were published", got)
			}
			for i := range 2 {
				kp, err := repo.TakeKeyPackage(ctx, device.ID, 1_700_000_001)
				if err != nil {
					t.Fatalf("take %d: %v", i, err)
				}
				if kp.LastResort != 0 {
					t.Fatalf("take %d served the last-resort package while ordinary ones remained", i)
				}
			}

			// And the last-resort row that survived is the NEWEST one, not the first: a device
			// that rotates its fallback package must be addressable through the new one.
			kp, err := repo.TakeKeyPackage(ctx, device.ID, 1_700_000_001)
			if err != nil {
				t.Fatalf("take after exhaustion: %v", err)
			}
			if kp.LastResort != 1 {
				t.Fatal("with the ordinary packages consumed the last-resort one must be served")
			}
			if !bytes.Equal(kp.KPRef, newest) {
				t.Fatalf("the surviving last-resort package is %x, want the newest %x", kp.KPRef, newest)
			}
		})
	}
}

// countKeyPackageRows reads a count the Repository interface does not expose: one device's rows on
// one side of the `last_resort` split. `CountKeyPackages` answers the ordinary side only, and the
// side it excludes is exactly what the test above is about.
func countKeyPackageRows(ctx context.Context, t *testing.T, engine string, repo store.Repository,
	device id.ID, lastResort int,
) int64 {
	t.Helper()
	q := `SELECT count(*) FROM key_packages WHERE device_id = ? AND last_resort = ?`
	if engine == "postgres" {
		q = `SELECT count(*) FROM key_packages WHERE device_id = $1 AND last_resort = $2`
	}
	var n int64
	if err := rawDB(t, repo).QueryRowContext(ctx, q, device, lastResort).Scan(&n); err != nil {
		t.Fatalf("count key_packages: %v", err)
	}
	return n
}

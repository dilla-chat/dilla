package ds_test

// directory_harness_test.go is task 24's half of the delivery service's harness: the KeyPackage
// directory of one device, the Welcome rows a commit leaves behind, and the two raw counts the
// three-table Welcome split (D4) can only be read with.
//
// It is a separate file for the same reason election_harness_test.go and message_harness_test.go
// are: `dsHarness` is one type and harness_test.go is already the longest file in the package.
//
// WHAT THE COMMITTED FIXTURE GIVES, AND WHAT IT WITHHOLDS, shapes every helper here.
//
//   - `testkit/fixtures/ds-1500` ships exactly ONE KeyPackage (`key_package.mls`), built for one
//     device with `last_resort = false` and the 90-day `KEY_PACKAGE_LIFETIME_DAYS` lifetime. There
//     is no generator in this repository that can mint a second one: `build_key_package` is Rust,
//     the wasi ABI exports no builder (interfaces §3.2 lists the 17 names), and Go MLS code is
//     forbidden by the Global Constraints. So `PublishKeyPackages` — which validates every blob
//     inside the guest and keys the row by the guest's own `kp_ref` — is driven with that one
//     package, and the TAKE side, whose rule lives in SQL, is driven over a directory seeded
//     through the store. Seeding is not a double: `store.Repository` is the real SQLite one, and
//     the rule under test is which row the query picks, not who wrote it.
//   - There is NO GroupInfo at epoch 7, so no commit in this repository can be ACCEPTED
//     (commit_test.go's header records this; task 20's report names the fixture work that closes
//     it). Every Welcome test therefore drives `storeWelcomesTx` through `StoreWelcomesForTest`,
//     which is the delivery service's own writer called where the commit's transaction calls it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

// ------------------------------------------------------------------ devices and sessions

// device registers one user and one device with the instance and returns the device. The rows are
// not decoration: `key_packages.device_id` references `devices(id)`, so a KeyPackage for a device
// with no account is not a state the database can hold.
func (h *dsHarness) device(t *testing.T) id.ID {
	t.Helper()
	device := id.New()
	h.account(t, id.New(), device)
	return device
}

// sessionOf is an ENROLLED session for the device. `users.id` is read back from the row the
// account helper wrote, so the session's user is the one SQL holds rather than one the test
// invented.
func (h *dsHarness) sessionOf(t *testing.T, device id.ID) auth.Session {
	t.Helper()
	row, err := h.repo.GetDevice(context.Background(), device)
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	return auth.Session{UserID: row.UserID, DeviceID: device, Scope: auth.ScopeEnrolled}
}

// provisionalSessionOf is the session a device holds BEFORE it is enrolled: scope `provisional`,
// bound to the one pairing group it exists to join (protocol/02 § Device sessions item 4).
func (h *dsHarness) provisionalSessionOf(t *testing.T, device, pairingGroup id.ID) auth.Session {
	t.Helper()
	s := h.sessionOf(t, device)
	s.Scope = auth.ScopeProvisional
	s.PairingGroup = &pairingGroup
	return s
}

// ------------------------------------------------------------------ the one committed KeyPackage

// keyPackageFixture is `key_package.mls` together with what the GUEST says about it. The identity
// and the kp_ref are read out of `validate_key_package` rather than out of the manifest, because
// they are what `PublishKeyPackages` compares against and a test that supplied them itself would
// assert only its own arithmetic.
func (h *dsHarness) keyPackageFixture(t *testing.T) ([]byte, mlswasi.KeyPackageInfo) {
	t.Helper()
	ctx := context.Background()
	blob := fixtureFile(t, "key_package.mls")
	inst, err := h.wasm.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer inst.Release()
	info, err := inst.ValidateKeyPackage(ctx, blob)
	if err != nil {
		t.Fatalf("ValidateKeyPackage on the committed fixture: %v", err)
	}
	return blob, info
}

// keyPackageOwner is the account and device the committed KeyPackage was built for, written into
// `users` and `devices` so the publish can store a row for it. The device is registered under the
// package's own leaf key, as the honest device that built it registered it: the publish binds
// every package to the device's registered key (hardening C).
func (h *dsHarness) keyPackageOwner(t *testing.T) (id.ID, auth.Session, []byte, mlswasi.KeyPackageInfo) {
	t.Helper()
	blob, info := h.keyPackageFixture(t)
	if len(info.DeviceID) != id.Size || len(info.UserID) != id.Size {
		t.Fatalf("the fixture KeyPackage names a %d-byte device and a %d-byte user, want 16 each",
			len(info.DeviceID), len(info.UserID))
	}
	var device, user id.ID
	copy(device[:], info.DeviceID)
	copy(user[:], info.UserID)
	if len(info.SignatureKey) != 32 {
		t.Fatalf("the fixture KeyPackage's leaf key is %d bytes, want 32", len(info.SignatureKey))
	}
	h.accountWithKey(t, user, device, info.SignatureKey)
	return device, auth.Session{UserID: user, DeviceID: device, Scope: auth.ScopeEnrolled}, blob, info
}

// ------------------------------------------------------------------ the KeyPackage directory

// seedKeyPackages writes n ORDINARY packages for the device, each with its own 32-byte kp_ref and
// the given lifetime. The blob is the committed fixture's real material; only the refs are the
// test's, because the fixture holds one package and the primary key is (device_id, kp_ref).
func (h *dsHarness) seedKeyPackages(t *testing.T, device id.ID, n int, lifetime time.Duration) {
	t.Helper()
	h.seedKeyPackageRows(t, device, n, lifetime, 0, 0)
}

// seedLastResort writes the device's ONE last-resort package. It is the package the directory
// serves for ever once the ordinary ones are gone, and `TakeKeyPackage` never consumes it.
func (h *dsHarness) seedLastResort(t *testing.T, device id.ID, lifetime time.Duration) {
	t.Helper()
	h.seedKeyPackageRows(t, device, 1, lifetime, 1, 0)
}

// republishLastResort writes n DISTINCT last-resort packages, each in its own store call: the
// shape a device that mints a fresh fallback package and publishes it over and over produces.
//
// It goes through the store rather than through `PublishKeyPackages` for the reason this file's
// header gives: the committed fixture ships ONE KeyPackage and it carries no `last_resort`
// extension, so the publish path — which validates every blob inside the guest — refuses it as the
// contradiction it is (`TestPublishRefusesAPackageWhoseLastResortExtensionDisagrees`), and no
// generator in this repository can mint a second package. What is under test is what the
// DIRECTORY ends up holding, and that rule is the store's wherever the row came from.
func (h *dsHarness) republishLastResort(t *testing.T, device id.ID, n int, lifetime time.Duration) {
	t.Helper()
	for i := range n {
		h.seedKeyPackageRows(t, device, 1, lifetime, 1, byte(i+1))
	}
}

func (h *dsHarness) seedKeyPackageRows(t *testing.T, device id.ID, n int, lifetime time.Duration,
	lastResort, generation uint8,
) {
	t.Helper()
	blob := fixtureFile(t, "key_package.mls")
	now := h.clk.Now().Unix()
	rows := make([]store.KeyPackageRow, 0, n)
	for i := range n {
		// A 32-byte ref, as RFC 9420's KeyPackageRef is, derived from the device, the index and the
		// generation so that two seeded packages never collide on the primary key — and so that a
		// package seeded by a later call is a DIFFERENT package, not the same row rewritten.
		ref := sha256.Sum256(append(append([]byte{lastResort, generation, byte(i), byte(i >> 8)},
			device[:]...), blob...))
		rows = append(rows, store.KeyPackageRow{
			DeviceID:   device,
			KPRef:      ref[:],
			Blob:       blob,
			LastResort: lastResort,
			Expires:    h.clk.Now().Add(lifetime).Unix(),
			Created:    now,
		})
	}
	if err := h.repo.PutKeyPackages(context.Background(), device, rows); err != nil {
		t.Fatalf("PutKeyPackages: %v", err)
	}
}

// ordinaryKeyPackagesLeft is what `CountKeyPackages` reports: the UNCONSUMED, UNEXPIRED ORDINARY
// packages. The last-resort one is deliberately outside the count — it is the fallback, not a
// package a client should stop refilling because of.
func (h *dsHarness) ordinaryKeyPackagesLeft(t *testing.T, device id.ID) int64 {
	t.Helper()
	n, err := h.repo.CountKeyPackages(context.Background(), device, h.clk.Now().Unix())
	if err != nil {
		t.Fatalf("CountKeyPackages: %v", err)
	}
	return n
}

// readyKeypackagesRemaining is the number the gateway's `ready` frame carries, read through the
// delivery service's own accessor — the one the composition root hands the gateway.
func (h *dsHarness) readyKeypackagesRemaining(t *testing.T, device id.ID) uint64 {
	t.Helper()
	n, err := h.ds.KeyPackagesRemaining(context.Background(), device)
	if err != nil {
		t.Fatalf("KeyPackagesRemaining: %v", err)
	}
	return n
}

// ------------------------------------------------------------------ Welcomes

// welcomeBlob is one opaque Welcome body. The delivery service never parses it — store and
// forward, addressed by device — so a distinct byte string per test is the whole requirement.
func welcomeBlob(tag byte, n int) []byte { return bytes.Repeat([]byte{tag}, n) }

// storeWelcomes runs the delivery service's own Welcome writer where the commit's transaction
// calls it: one payload row per distinct blob, one welcome row per addressed device, and the
// ratchet tree of the welcoming epoch.
//
// It goes through `StoreWelcomesForTest` rather than through `Commit` because NO commit can be
// accepted against the committed fixture — there is no GroupInfo at epoch 7 (commit_test.go's
// header) — so `Commit` refuses before it ever reaches the writer.
func (h *dsHarness) storeWelcomes(t *testing.T, groupID id.ID, epoch, commitSeq uint64, blob []byte, joiners ...id.ID) {
	t.Helper()
	welcomes := make([]ds.WelcomeFor, 0, len(joiners))
	for _, j := range joiners {
		welcomes = append(welcomes, ds.WelcomeFor{DeviceID: j, Blob: blob})
	}
	if err := ds.StoreWelcomesForTest(h.ds, context.Background(), groupID, epoch, commitSeq, welcomes); err != nil {
		t.Fatalf("storeWelcomesTx: %v", err)
	}
}

// putWelcomeRow writes one Welcome for a group this harness never registered. It is how a second
// group's queued Welcome exists at all: `Register` is bound to the committed fixture's own group
// id, so a test that needs TWO groups cannot get the second one from the fixture.
func (h *dsHarness) putWelcomeRow(t *testing.T, device, groupID id.ID, epoch uint64, blob []byte) {
	h.putWelcomeRowKind(t, device, groupID, epoch, blob, 0)
}

func (h *dsHarness) putWelcomeRowKind(t *testing.T, device, groupID id.ID, epoch uint64, blob []byte, kind uint8) {
	t.Helper()
	ctx := context.Background()
	sum := sha256.Sum256(blob)
	now := h.clk.Now().Unix()
	if _, err := h.repo.GetGroup(ctx, groupID); errors.Is(err, store.ErrNotFound) {
		if err := h.repo.CreateGroup(ctx, store.GroupRow{
			GroupID: groupID, Binding: []byte{0x80}, Kind: kind, TargetID: groupID,
			ExternalSenderKeyID: testInstanceKeys(t).ExternalSenderKeyID,
			E2EEVersion:         1, MediaVersion: 1, PolicyVersion: 1, Created: now,
		}); err != nil {
			t.Fatalf("CreateGroup for Welcome fixture: %v", err)
		}
	} else if err != nil {
		t.Fatalf("GetGroup for Welcome fixture: %v", err)
	}
	if err := h.repo.PutWelcomePayload(ctx, store.WelcomePayloadRow{
		BlobSHA256: sum[:], GroupID: groupID, Epoch: epoch, Blob: blob, Created: now,
	}); err != nil {
		t.Fatalf("PutWelcomePayload: %v", err)
	}
	if err := h.repo.PutEpochTree(ctx, store.EpochTreeRow{
		GroupID: groupID, Epoch: epoch, RatchetTree: welcomeBlob(0xEE, 64),
		TreeHash: bytes.Repeat([]byte{0xAB}, 32), Created: now,
	}); err != nil {
		t.Fatalf("PutEpochTree: %v", err)
	}
	if err := h.repo.PutWelcomes(ctx, []store.WelcomeRow{{
		DeviceID: device, GroupID: groupID, Epoch: epoch, CommitSeq: 1,
		BlobSHA256: sum[:], Created: now, Expires: now + 30*24*60*60,
	}}); err != nil {
		t.Fatalf("PutWelcomes: %v", err)
	}
}

// welcomeIDOf is the surrogate key of the device's queued Welcome for one group.
func (h *dsHarness) welcomeIDOf(t *testing.T, device, groupID id.ID) int64 {
	t.Helper()
	rows, err := h.repo.ListWelcomes(context.Background(), device, 0, 64)
	if err != nil {
		t.Fatalf("ListWelcomes: %v", err)
	}
	for _, row := range rows {
		if row.GroupID == groupID {
			return row.WelcomeID
		}
	}
	t.Fatalf("no queued Welcome for device %s in group %s", device.String()[:8], groupID.String()[:8])
	return 0
}

// ------------------------------------------------------------------ raw counts

// countRows reads one COUNT(*) straight off the harness's database file. The three-table Welcome
// split (D4) has no store method that counts payloads, and "one payload for 256 joiners" is
// precisely a claim about the row counts of two tables — a claim `ListWelcomes`, which joins them
// back together, cannot make.
func (h *dsHarness) countRows(t *testing.T, table string) int64 {
	t.Helper()
	db, err := sqlite.OpenRead(h.path)
	if err != nil {
		t.Fatalf("sqlite.OpenRead: %v", err)
	}
	defer func() { _ = db.Close() }()
	return countIn(t, db, table)
}

// keyPackageExpiry is the `expires` of the one row in key_packages.
func (h *dsHarness) keyPackageExpiry(t *testing.T) int64 {
	t.Helper()
	db, err := sqlite.OpenRead(h.path)
	if err != nil {
		t.Fatalf("sqlite.OpenRead: %v", err)
	}
	defer func() { _ = db.Close() }()
	var expires int64
	if err := db.QueryRowContext(t.Context(), "SELECT expires FROM key_packages").Scan(&expires); err != nil {
		t.Fatalf("reading key_packages.expires: %v", err)
	}
	return expires
}

func countIn(t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	var n int64
	// `table` is a literal at every call site in this package; no value from a test's data
	// reaches it.
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// expectNoWelcomeFrame asserts that no `mls.welcome` reached the device AND that its connection is
// still open. Both halves matter: the failure this guards against is a frame the client's own read
// limit refuses, which shows up as a closed socket rather than as a missing frame.
func (h *dsHarness) expectNoWelcomeFrame(t *testing.T, device id.ID) {
	t.Helper()
	c := h.connOf(t, device)
	// One settle: the fan-out writes from the same call the test just returned from, so a frame on
	// its way is already on the wire.
	time.Sleep(200 * time.Millisecond)
	select {
	case <-c.closed:
		t.Fatalf("the connection of device %s was closed by the fan-out", device.String()[:8])
	default:
	}
	for {
		select {
		case f := <-c.frames:
			if f.name == "mls.welcome" {
				t.Fatalf("device %s was sent an mls.welcome the gateway cannot carry",
					device.String()[:8])
			}
		default:
			return
		}
	}
}

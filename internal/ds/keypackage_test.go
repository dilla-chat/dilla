package ds_test

// The KeyPackage directory: protocol/02's endpoints 10 (`POST /v1/keypackages`) and 11
// (`GET /v1/devices/{device_id}/keypackage`), seen from the delivery service.
//
// Two halves, driven differently on purpose (directory_harness_test.go's header has the reasons):
// the PUBLISH side runs against the one committed KeyPackage, because validation happens inside
// the guest and only real material reaches it; the TAKE side runs against a directory seeded
// through the real store, because the rule it encodes — ordinary first, the last-resort package
// never consumed, an expired row never served — lives in SQL.

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

const (
	// The lifetime `build_key_package` gives every dilla KeyPackage:
	// `dilla_core::mls::KEY_PACKAGE_LIFETIME_DAYS`.
	keyPackageLifetime = 90 * 24 * time.Hour
	// A SHORTER lifetime for the ordinary packages of the expiry test, so the clock can cross
	// theirs while staying well inside the last-resort package's.
	shortKeyPackageLifetime = 7 * 24 * time.Hour
)

// Role 1: ordinary packages are consumed on use; the last-resort package never is.
func TestOrdinaryKeyPackagesAreConsumedAndTheLastResortIsNot(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	device := h.device(t)
	session := h.sessionOf(t, device)
	h.seedKeyPackages(t, device, 32, keyPackageLifetime)
	h.seedLastResort(t, device, keyPackageLifetime)

	seen := map[string]struct{}{}
	for i := range 32 {
		got, err := h.ds.TakeKeyPackage(ctx, session, device)
		if err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
		if got.LastResort {
			t.Fatalf("take %d returned the last-resort package while ordinary ones remained", i)
		}
		if len(got.KPRef) != 32 {
			t.Fatalf("kp_ref is %d bytes, want 32", len(got.KPRef))
		}
		if _, dup := seen[string(got.KPRef)]; dup {
			t.Fatalf("take %d served a package that was already handed out", i)
		}
		seen[string(got.KPRef)] = struct{}{}
	}
	for i := range 3 {
		got, err := h.ds.TakeKeyPackage(ctx, session, device)
		if err != nil {
			t.Fatalf("take %d after exhaustion: %v", i, err)
		}
		if !got.LastResort {
			t.Fatal("with the ordinary packages consumed the last-resort one must be served")
		}
	}
	// The count the refill threshold is read against is the ORDINARY one: every ordinary package
	// is gone, and the last-resort row is still there to be served for ever.
	if left := h.ordinaryKeyPackagesLeft(t, device); left != 0 {
		t.Fatalf("%d ordinary packages remain, want 0", left)
	}
	if total := h.countRows(t, "key_packages"); total != 33 {
		t.Fatalf("%d rows in key_packages, want the 32 consumed ordinary ones and the last-resort", total)
	}
}

// The clock crosses the ORDINARY packages' expiry and stops short of the last-resort one's.
//
// `TakeKeyPackage`'s query filters `expires > ?` for every row, and a fixture whose ordinary and
// last-resort packages shared one 90-day lifetime would have no row left at all — the take would
// answer E_NOT_FOUND and the test would die on its own Fatalf rather than on the rule. So the
// ordinary packages are seeded with a SHORTER lifetime and the clock crosses that one, which is
// the expiry actually under test.
func TestAnExpiredKeyPackageIsSkipped(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	device := h.device(t)
	session := h.sessionOf(t, device)
	h.seedKeyPackages(t, device, 2, shortKeyPackageLifetime)
	h.seedLastResort(t, device, keyPackageLifetime)

	// Past the ordinary packages' 7-day expiry, well inside the last-resort package's 90 days.
	h.clk.Advance(shortKeyPackageLifetime + time.Hour)

	got, err := h.ds.TakeKeyPackage(ctx, session, device)
	if err != nil {
		t.Fatalf("TakeKeyPackage: %v", err)
	}
	if !got.LastResort {
		t.Fatal("an expired ordinary package must be skipped")
	}
	if left := h.ordinaryKeyPackagesLeft(t, device); left != 0 {
		t.Fatalf("%d ordinary packages count as available, want 0 — both have expired", left)
	}
}

// A device with nothing in its directory at all is E_NOT_FOUND, not a nil package.
func TestTakingFromAnEmptyDirectoryIsNotFound(t *testing.T) {
	h := newDSHarness(t)
	device := h.device(t)
	_, err := h.ds.TakeKeyPackage(context.Background(), h.sessionOf(t, device), device)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_NOT_FOUND" {
		t.Fatalf("got %v, want E_NOT_FOUND", err)
	}
}

// keypackages_remaining in `ready` drops below the refill threshold and is reported. The
// threshold is config's own default, not a literal: one number governs the knob an operator
// writes and the count a client refills against.
func TestKeypackagesRemainingIsReportedAgainstTheRefillThreshold(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	device := h.device(t)
	session := h.sessionOf(t, device)
	h.seedKeyPackages(t, device, 10, keyPackageLifetime)

	for i := range 5 {
		if _, err := h.ds.TakeKeyPackage(ctx, session, device); err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
	}
	remaining := h.readyKeypackagesRemaining(t, device)
	if remaining != 5 {
		t.Fatalf("ready reports %d remaining, want 5", remaining)
	}
	threshold := config.Default().Limits.KeypackageRefillThreshold
	if threshold != 8 {
		t.Fatalf("the refill threshold is %d, want config's 8", threshold)
	}
	if remaining >= uint64(threshold) {
		t.Fatalf("the test must drive the count below the threshold of %d", threshold)
	}
}

// ---------------------------------------------------------------- the publish side

// The one committed KeyPackage goes in, and the row is keyed by the ref the GUEST computed — not
// by anything the client sent. A client that publishes the same package twice writes one row.
func TestPublishStoresTheGuestsKeyPackageRefAndIsIdempotent(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	device, session, blob, info := h.keyPackageOwner(t)

	n, err := h.ds.PublishKeyPackages(ctx, session, [][]byte{blob}, nil)
	if err != nil {
		t.Fatalf("PublishKeyPackages: %v", err)
	}
	if n != 1 {
		t.Fatalf("stored %d packages, want 1", n)
	}
	if len(info.KPRef) != 32 {
		t.Fatalf("the guest's kp_ref is %d bytes, want 32", len(info.KPRef))
	}
	got, err := h.ds.TakeKeyPackage(ctx, session, device)
	if err != nil {
		t.Fatalf("TakeKeyPackage: %v", err)
	}
	if !bytes.Equal(got.KPRef, info.KPRef) {
		t.Fatalf("kp_ref = %x, want the guest's %x", got.KPRef, info.KPRef)
	}
	if !bytes.Equal(got.Blob, blob) {
		t.Fatal("the stored blob is not the published one")
	}
	if got.LastResort {
		t.Fatal("the fixture package carries no last_resort extension")
	}

	// Publishing it again writes no second row: the primary key is (device_id, kp_ref).
	if _, err := h.ds.PublishKeyPackages(ctx, session, [][]byte{blob}, nil); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if total := h.countRows(t, "key_packages"); total != 1 {
		t.Fatalf("%d rows after republishing the same package, want 1", total)
	}
}

// A device may only publish KeyPackages for itself. Without the check, any enrolled session could
// fill another device's directory with packages only it holds the private keys for, and every Add
// of that device would then hand the joiner a KeyPackage it cannot open.
func TestPublishRefusesAKeyPackageForAnotherDevice(t *testing.T) {
	h := newDSHarness(t)
	_, _, blob, _ := h.keyPackageOwner(t)
	other := h.device(t)

	_, err := h.ds.PublishKeyPackages(context.Background(), h.sessionOf(t, other), [][]byte{blob}, nil)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_FORBIDDEN" {
		t.Fatalf("got %v, want E_FORBIDDEN", err)
	}
	if total := h.countRows(t, "key_packages"); total != 0 {
		t.Fatalf("%d rows were written for a refused publish, want 0", total)
	}
}

// The ruling of hardening C, point (a): a published KeyPackage's leaf is bound to the device's
// REGISTERED key, the `devices.dsk_pub` the session was established under. Here the device the
// committed package names is registered with another key (the harness's 0x04…), as a device that
// minted the package under a key it never registered would be: the publish is refused as an
// invalid KeyPackage and nothing is stored.
func TestPublishRefusesAKeyPackageWhoseLeafKeyIsNotTheDevicesRegisteredKey(t *testing.T) {
	h := newDSHarness(t)
	blob, info := h.keyPackageFixture(t)
	var device, user id.ID
	copy(device[:], info.DeviceID)
	copy(user[:], info.UserID)
	h.account(t, user, device) // devices.dsk_pub = 0x04…, not the package's leaf key
	session := h.sessionOf(t, device)

	_, err := h.ds.PublishKeyPackages(context.Background(), session, [][]byte{blob}, nil)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" || dsErr.Rule != "key_package" {
		t.Fatalf("got %v, want E_COMMIT_INVALID/key_package", err)
	}
	if total := h.countRows(t, "key_packages"); total != 0 {
		t.Fatalf("%d rows were written for a refused publish, want 0", total)
	}
}

// Point (a) again: the package's credential must name the uploading session's USER as well as its
// device. The session here holds the package's device id under another user — the shape a client
// produces when it mints its credential for an account it is not — and the publish is refused.
func TestPublishRefusesAKeyPackageWhoseCredentialNamesAnotherUser(t *testing.T) {
	h := newDSHarness(t)
	blob, info := h.keyPackageFixture(t)
	var device id.ID
	copy(device[:], info.DeviceID)
	other := id.New()
	h.account(t, other, device)
	session := h.sessionOf(t, device)
	if session.UserID != other {
		t.Fatal("the session must be the other user's")
	}

	_, err := h.ds.PublishKeyPackages(context.Background(), session, [][]byte{blob}, nil)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" || dsErr.Rule != "key_package" ||
		!strings.Contains(dsErr.Detail, "user") {
		t.Fatalf("got %v, want E_COMMIT_INVALID/key_package naming the user", err)
	}
	if total := h.countRows(t, "key_packages"); total != 0 {
		t.Fatalf("%d rows were written for a refused publish, want 0", total)
	}
}

// The committed package carries no last_resort extension, so publishing it AS the last-resort one
// is a contradiction the directory must not store: the row would be served for ever, and
// `TakeKeyPackage` would hand the same package to every joiner.
func TestPublishRefusesAPackageWhoseLastResortExtensionDisagrees(t *testing.T) {
	h := newDSHarness(t)
	_, session, blob, _ := h.keyPackageOwner(t)

	_, err := h.ds.PublishKeyPackages(context.Background(), session, nil, blob)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" {
		t.Fatalf("got %v, want E_COMMIT_INVALID", err)
	}
	if total := h.countRows(t, "key_packages"); total != 0 {
		t.Fatalf("%d rows were written for a refused publish, want 0", total)
	}
}

func TestPublishRefusesAKeyPackageThatDoesNotValidate(t *testing.T) {
	h := newDSHarness(t)
	device := h.device(t)
	_, err := h.ds.PublishKeyPackages(context.Background(), h.sessionOf(t, device),
		[][]byte{{0x00, 0x01}}, nil)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" {
		t.Fatalf("got %v, want E_COMMIT_INVALID", err)
	}
}

// A KeyPackage's lifetime is CLIENT-controlled, and OpenMLS 0.9.0 only checks
// `not_before <= now < not_after`: a package built with `not_after = u64::MAX` is valid to the
// guest. Stored as `int64(u64::MAX)` that is -1, and `CountKeyPackages` filters `expires > now`, so
// such a row would never count against `max_keypackages_per_device` and a device could publish
// batches of it for ever. The delivery service therefore bounds the lifetime itself (RFC 9420
// ValSem #32: "applications MUST define a maximum total lifetime").
func TestPublishRefusesAKeyPackageWhoseLifetimeExceedsThePolicyMaximum(t *testing.T) {
	h := newDSHarness(t)
	_, session, blob, info := h.keyPackageOwner(t)

	// The fixture's own lifetime is 90 days; a policy that allows 30 must refuse it. This is the
	// route end to end: the guest accepts the package, the delivery service does not.
	tight := ds.DefaultPolicy()
	tight.MaxKeyPackageLifetime = 30 * 24 * time.Hour
	strict, err := ds.New(ds.Options{
		Store: h.repo, Wasm: h.wasm, Gateway: h.gw, Clock: h.clk,
		Keys: testInstanceKeys(t), Policy: tight, Channels: h.channels, ACL: h.acl,
	})
	if err != nil {
		t.Fatalf("ds.New: %v", err)
	}
	t.Cleanup(func() { _ = strict.Shutdown(context.Background()) })

	_, err = strict.PublishKeyPackages(context.Background(), session, [][]byte{blob}, nil)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID" {
		t.Fatalf("got %v, want E_COMMIT_INVALID for a lifetime over the policy maximum", err)
	}
	if total := h.countRows(t, "key_packages"); total != 0 {
		t.Fatalf("%d rows were written for a refused publish, want 0", total)
	}

	// The default policy accepts the same package, and the row carries the guest's not_after.
	if _, err := h.ds.PublishKeyPackages(context.Background(), session, [][]byte{blob}, nil); err != nil {
		t.Fatalf("the default policy must accept a client-default 90-day package: %v", err)
	}
	if got := h.keyPackageExpiry(t); got != int64(info.NotAfter) {
		t.Fatalf("stored expires = %d, want the package's not_after %d", got, info.NotAfter)
	}
}

// The rule on its own, at the values a hostile client picks. `not_after` is a uint64 on the wire,
// so the ones that matter are u64::MAX, 2^63 (the first value that wraps negative as an int64),
// int64's own maximum, and the boundary itself.
func TestKeyPackageLifetimeBound(t *testing.T) {
	const day = 24 * 60 * 60
	now := time.Unix(1_800_000_000, 0)
	maxLifetime := 91 * 24 * time.Hour
	limit := uint64(now.Unix()) + uint64(maxLifetime/time.Second)

	for _, tc := range []struct {
		name     string
		notAfter uint64
		ok       bool
	}{
		{"u64 max", math.MaxUint64, false},
		{"first value that wraps negative as int64", 1 << 63, false},
		{"int64 max", math.MaxInt64, false},
		{"one second over the maximum", limit + 1, false},
		{"exactly the maximum", limit, true},
		{"a client-default 90 days", uint64(now.Unix()) + 90*day, true},
		{"one hour", uint64(now.Unix()) + 3600, true},
	} {
		err := ds.CheckKeyPackageLifetimeForTest(now.Unix(), tc.notAfter, maxLifetime)
		var dsErr *ds.Error
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case !tc.ok && (!errors.As(err, &dsErr) || dsErr.Code != "E_COMMIT_INVALID"):
			t.Errorf("%s: got %v, want E_COMMIT_INVALID", tc.name, err)
		}
	}
}

// One device's directory is bounded by config's `limits.max_keypackages_per_device`, and the cap
// is checked BEFORE the guest is asked to validate anything: a caller that could not exceed the
// cap can still make the instance run 10 000 validations by trying.
func TestPublishRefusesMoreThanThePolicyCap(t *testing.T) {
	h := newDSHarness(t)
	device := h.device(t)
	limit := ds.PolicyForTest(h.ds).MaxKeyPackagesPerDevice
	if limit != config.Default().Limits.MaxKeypackagesPerDevice {
		t.Fatalf("the policy cap is %d, want config's %d",
			limit, config.Default().Limits.MaxKeypackagesPerDevice)
	}
	before := h.wasmCalls("validate_key_package")

	packages := make([][]byte, limit+1)
	for i := range packages {
		packages[i] = []byte{0x00, byte(i)}
	}
	_, err := h.ds.PublishKeyPackages(context.Background(), h.sessionOf(t, device), packages, nil)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_TOO_LARGE" {
		t.Fatalf("got %v, want E_TOO_LARGE", err)
	}
	if got := h.wasmCalls("validate_key_package") - before; got != 0 {
		t.Fatalf("the guest was asked to validate %d packages over the cap, want 0", got)
	}
}

// Rows 10, 15 and 16 accept a provisional session, and the restriction that makes that safe is
// enforced, not assumed: a provisional session is bound to ONE pairing group and may publish the
// one KeyPackage for it, nothing else. Without the check a session issued before enrolment can
// stock a whole directory — and, on the Welcome side, harvest every group its device was ever
// added to, which is the escalation interfaces §2.2 point 4 forbids.
func TestAProvisionalSessionMayPublishOnlyItsOnePairingKeyPackage(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	device, _, blob, _ := h.keyPackageOwner(t)
	provisional := h.provisionalSessionOf(t, device, id.New())

	// One package is the pairing package, and it is accepted.
	if _, err := h.ds.PublishKeyPackages(ctx, provisional, [][]byte{blob}, nil); err != nil {
		t.Fatalf("a provisional session must be able to publish its one pairing KeyPackage: %v", err)
	}

	var dsErr *ds.Error
	_, err := h.ds.PublishKeyPackages(ctx, provisional, [][]byte{blob, blob, blob, blob}, nil)
	if !errors.As(err, &dsErr) || dsErr.Code != "E_PROVISIONAL_OUTSIDE_PAIRING" {
		t.Fatalf("got %v, want E_PROVISIONAL_OUTSIDE_PAIRING for a bulk publish", err)
	}
	_, err = h.ds.PublishKeyPackages(ctx, provisional, nil, blob)
	if !errors.As(err, &dsErr) || dsErr.Code != "E_PROVISIONAL_OUTSIDE_PAIRING" {
		t.Fatalf("got %v, want E_PROVISIONAL_OUTSIDE_PAIRING for a last-resort publish", err)
	}

	// A provisional session bound to NO pairing group reaches nothing at all.
	unbound := h.provisionalSessionOf(t, device, id.New())
	unbound.PairingGroup = nil
	_, err = h.ds.PublishKeyPackages(ctx, unbound, [][]byte{blob}, nil)
	if !errors.As(err, &dsErr) || dsErr.Code != "E_PROVISIONAL_OUTSIDE_PAIRING" {
		t.Fatalf("got %v, want E_PROVISIONAL_OUTSIDE_PAIRING for a session bound to no group", err)
	}
}

// The cap bounds what the DIRECTORY HOLDS, not what one request carries. A per-request bound is no
// bound at all: an enrolled device publishes `limit` valid packages per call in a loop and
// `key_packages` grows without end — every row a full KeyPackage blob, unconsumed for its 90-day
// lifetime — and the same framing lets a provisional session, which may publish one package per
// call, stock a whole directory one call at a time.
//
// The refusal is still counted in PACKAGES and still costs the guest nothing: the directory is
// counted with one SQL statement BEFORE any blob reaches the guest.
func TestThePolicyCapBoundsTheDirectoryNotOneRequest(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	device, session, blob, _ := h.keyPackageOwner(t)
	limit := ds.PolicyForTest(h.ds).MaxKeyPackagesPerDevice

	// One package short of the cap, and the publish that fills it exactly is accepted.
	h.seedKeyPackages(t, device, limit-1, keyPackageLifetime)
	if _, err := h.ds.PublishKeyPackages(ctx, session, [][]byte{blob}, nil); err != nil {
		t.Fatalf("the publish that fills the directory exactly must be accepted: %v", err)
	}
	if left := h.ordinaryKeyPackagesLeft(t, device); left != int64(limit) {
		t.Fatalf("the directory holds %d packages, want the cap's %d", left, limit)
	}

	// The next one is refused although it is a single package in a single request: the directory
	// is full.
	before := h.wasmCalls("validate_key_package")
	_, err := h.ds.PublishKeyPackages(ctx, session, [][]byte{blob}, nil)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_TOO_LARGE" {
		t.Fatalf("got %v, want E_TOO_LARGE once the directory is at the cap", err)
	}
	if got := h.wasmCalls("validate_key_package") - before; got != 0 {
		t.Fatalf("the guest validated %d packages for a publish over the cap, want 0", got)
	}
	if total := h.countRows(t, "key_packages"); total != int64(limit) {
		t.Fatalf("%d rows after the refused publish, want the cap's %d", total, limit)
	}

	// And the bound really is on what the directory HOLDS: one take consumes a row, and the same
	// publish then fits.
	if _, err := h.ds.TakeKeyPackage(ctx, session, device); err != nil {
		t.Fatalf("TakeKeyPackage: %v", err)
	}
	if _, err := h.ds.PublishKeyPackages(ctx, session, [][]byte{blob}, nil); err != nil {
		t.Fatalf("a consumed package must make room for a new one: %v", err)
	}
}

// The LAST-RESORT half of the directory is bounded too — at one row per device.
//
// The cap above cannot see it: `CountKeyPackages` filters `last_resort = 0` by contract, because
// the number it reports is what a client refills against and the fallback package is not one a
// client refills. So without a rule of its own the last-resort column is the hole in the bound: a
// device mints a fresh, valid last-resort package — distinct `kp_ref`, so distinct row under
// `key_packages`' only unique key — and publishes it with no ordinary packages beside it, over and
// over, growing the table by one full KeyPackage blob a call for each blob's 90-day lifetime.
//
// The rule that closes it is the store's ("a new last-resort package REPLACES the old", pinned per
// engine by internal/store's TestANewLastResortKeyPackageReplacesTheOldOne), and this is the
// delivery service seeing its effect: the directory of a device that republished sixteen times
// holds one fallback row, and the device is still addressable through it.
func TestRepublishingTheLastResortPackageDoesNotGrowTheDirectory(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	device := h.device(t)
	session := h.sessionOf(t, device)
	const ordinary = 4

	h.seedKeyPackages(t, device, ordinary, keyPackageLifetime)
	h.republishLastResort(t, device, 16, keyPackageLifetime)

	if total := h.countRows(t, "key_packages"); total != ordinary+1 {
		t.Fatalf("%d rows in key_packages after 16 last-resort publishes, want the %d ordinary ones "+
			"and ONE fallback", total, ordinary)
	}
	if left := h.ordinaryKeyPackagesLeft(t, device); left != ordinary {
		t.Fatalf("%d ordinary packages remain, want the %d seeded — the rule must not touch them",
			left, ordinary)
	}
	// The one row that survived is a working fallback: with the ordinary packages consumed the
	// directory still answers, which is the whole point of keeping one rather than none.
	for i := range ordinary {
		if _, err := h.ds.TakeKeyPackage(ctx, session, device); err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
	}
	got, err := h.ds.TakeKeyPackage(ctx, session, device)
	if err != nil {
		t.Fatalf("take after exhaustion: %v", err)
	}
	if !got.LastResort {
		t.Fatal("the surviving row must be the last-resort package")
	}
}

// setUserColumn writes one users column straight on the harness's database file: the store has no
// method that sets role flags, disable or tombstone on an existing account, and these tests need
// exactly that state.
func (h *dsHarness) setUserColumn(t *testing.T, user id.ID, column string, value any) {
	t.Helper()
	db, err := sqlite.OpenWrite(h.path)
	if err != nil {
		t.Fatalf("sqlite.OpenWrite: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), "UPDATE users SET "+column+" = ? WHERE id = ?", value, user[:]); err != nil {
		t.Fatalf("UPDATE users SET %s: %v", column, err)
	}
}

// users.flags are role bits (bit 0 InstanceAdmin, bit 1 BotOperator), not a disabled marker, so an
// account that carries any of them still publishes for its own devices.
func TestPublishAcceptsAnInstanceAdmin(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	device, session, blob, info := h.keyPackageOwner(t)
	h.setUserColumn(t, session.UserID, "flags", 1)

	if _, err := h.ds.PublishKeyPackages(ctx, session, [][]byte{blob}, nil); err != nil {
		t.Fatalf("PublishKeyPackages: %v", err)
	}
	got, err := h.ds.TakeKeyPackage(ctx, session, device)
	if err != nil {
		t.Fatalf("TakeKeyPackage: %v", err)
	}
	if !bytes.Equal(got.KPRef, info.KPRef) {
		t.Fatalf("kp_ref = %x, want the published %x", got.KPRef, info.KPRef)
	}
}

func TestPublishAcceptsABotOperatorFlaggedHuman(t *testing.T) {
	h := newDSHarness(t)
	ctx := context.Background()
	device, session, blob, info := h.keyPackageOwner(t)
	h.setUserColumn(t, session.UserID, "flags", 2)

	if _, err := h.ds.PublishKeyPackages(ctx, session, [][]byte{blob}, nil); err != nil {
		t.Fatalf("PublishKeyPackages: %v", err)
	}
	got, err := h.ds.TakeKeyPackage(ctx, session, device)
	if err != nil {
		t.Fatalf("TakeKeyPackage: %v", err)
	}
	if !bytes.Equal(got.KPRef, info.KPRef) {
		t.Fatalf("kp_ref = %x, want the published %x", got.KPRef, info.KPRef)
	}
}

func TestPublishStillRefusesADisabledAccount(t *testing.T) {
	h := newDSHarness(t)
	_, session, blob, _ := h.keyPackageOwner(t)
	h.setUserColumn(t, session.UserID, "disabled_at", h.clk.Now().Unix())
	h.requirePublishForbidden(t, session, blob)
}

func TestPublishStillRefusesADeletedAccount(t *testing.T) {
	h := newDSHarness(t)
	_, session, blob, _ := h.keyPackageOwner(t)
	h.setUserColumn(t, session.UserID, "deleted_at", h.clk.Now().Unix())
	h.requirePublishForbidden(t, session, blob)
}

// A managed Bot's session is refused on this generic route, whatever its account flags: Bots publish
// through their installation-scoped route. (Bot sessions are minted only for Bot-kind accounts.)
func TestPublishStillRefusesABotKindAccount(t *testing.T) {
	h := newDSHarness(t)
	_, session, blob, _ := h.keyPackageOwner(t)
	h.setUserColumn(t, session.UserID, "kind", 1)
	h.setUserColumn(t, session.UserID, "flags", 1)
	session.Bot = &auth.BotSessionBinding{}

	h.requirePublishForbidden(t, session, blob)
}

func (h *dsHarness) requirePublishForbidden(t *testing.T, session auth.Session, blob []byte) {
	t.Helper()
	_, err := h.ds.PublishKeyPackages(context.Background(), session, [][]byte{blob}, nil)
	var dsErr *ds.Error
	if !errors.As(err, &dsErr) || dsErr.Code != "E_FORBIDDEN" {
		t.Fatalf("got %v, want E_FORBIDDEN", err)
	}
	if total := h.countRows(t, "key_packages"); total != 0 {
		t.Fatalf("%d rows were written for a refused publish, want 0", total)
	}
}

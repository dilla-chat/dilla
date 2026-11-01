package ds

// keypackage.go is the delivery service's KeyPackage directory: protocol/02's role 4, endpoints 10
// (`POST /v1/keypackages`) and 11 (`GET /v1/devices/{device_id}/keypackage`).
//
// Two rules make the directory what it is. Every published package is validated INSIDE THE GUEST —
// the leaf must advertise 0xF001, the credential identity must decode and name the publishing
// device and user, the leaf's signature key must be the device's registered key, and the lifetime
// must not have passed — so a package the delivery service would later
// hand a joiner is never one it has not checked. And the LAST-RESORT package is never consumed:
// once the ordinary ones are gone it is served for ever, which is what keeps a device that has run
// out of packages addable rather than unreachable.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// KeyPackage is one served package. KPRef is the RFC 9420 KeyPackageRef the GUEST computed, which
// is also the row's primary key: a client that publishes the same package twice writes one row.
type KeyPackage struct {
	Blob       []byte
	LastResort bool
	KPRef      []byte
}

// maxKeyPackagesPerDevice bounds one device's directory. It is not a literal here: the value is
// Policy's, which the composition root fills from `config.Default().Limits
// .MaxKeypackagesPerDevice`, so one number governs this refusal and the
// `limits.max_keypackages_per_device` element of `GET /v1/instance/limits`.
func (d *DS) maxKeyPackagesPerDevice() int { return d.opts.Policy.MaxKeyPackagesPerDevice }

// errTooManyKeyPackages is E_TOO_LARGE counted in PACKAGES. `errTooLarge` counts bytes of
// ciphertext and says so in its detail, which would be a lie on this route.
func errTooManyKeyPackages(n, limit int) *Error {
	return &Error{
		Code:   "E_TOO_LARGE",
		Detail: fmt.Sprintf("%d key packages in one publish, limit %d", n, limit),
		Status: http.StatusRequestEntityTooLarge,
	}
}

// errKeyPackageDirectoryFull is the same E_TOO_LARGE for the bound that actually matters: what the
// DIRECTORY holds. The detail names both numbers, because a client that gets "limit 32" back for a
// one-package publish has no way to tell otherwise that the request was fine and the shelf is full.
func errKeyPackageDirectoryFull(held int64, adding, limit int) *Error {
	return &Error{
		Code: "E_TOO_LARGE",
		Detail: fmt.Sprintf("the directory holds %d key packages and this publish adds %d, limit %d",
			held, adding, limit),
		Status: http.StatusRequestEntityTooLarge,
	}
}

// checkKeyPackageLifetime refuses a `not_after` (unix seconds, the client's) later than now plus
// the policy's maximum lifetime. The comparison is in uint64 so no value a client can send
// overflows or wraps it; `now` is a clock reading and never negative in practice, and a negative
// one is treated as zero rather than as a huge unsigned number.
func checkKeyPackageLifetime(now int64, notAfter uint64, maxLifetime time.Duration) error {
	if notAfter > math.MaxInt64 {
		return errCommitInvalid("key_package", "the package expiry cannot be stored")
	}
	base := uint64(0)
	if now > 0 {
		base = uint64(now)
	}
	if maxLifetime < 0 {
		maxLifetime = 0
	}
	limit := base + uint64(maxLifetime/time.Second)
	if notAfter > limit {
		return errCommitInvalid("key_package", fmt.Sprintf(
			"the package's lifetime ends beyond the instance's maximum of %s from now",
			maxLifetime.Round(time.Second)))
	}
	return nil
}

// PublishKeyPackages validates every package inside the guest and stores it with the guest's own
// kp_ref. It returns how many rows the publish covers; republishing a package the directory
// already holds is not an error and writes no second row.
func (d *DS) PublishKeyPackages(ctx context.Context, s Session, packages [][]byte, lastResort []byte) (int, error) {
	if s.Bot != nil {
		return 0, errForbidden("managed Bots must use their installation-scoped KeyPackage publication route")
	}
	// A provisional session exists to bring ONE device into ONE pairing group, so it may publish
	// the single KeyPackage that group's Welcome will consume and nothing more. The route accepts
	// a provisional session (interfaces §5.1 row 10); this is what makes that safe. It is checked
	// FIRST, before the cap and before the guest, because it is the cheapest refusal and the one
	// an unenrolled caller is most likely to reach for.
	if s.Scope == auth.ScopeProvisional {
		if s.PairingGroup == nil {
			return 0, errProvisionalOutsidePairing("this provisional session is bound to no pairing group")
		}
		if len(packages) > 1 || lastResort != nil {
			return 0, errProvisionalOutsidePairing(
				"a provisional session may publish exactly one pairing KeyPackage and no last-resort package")
		}
	}
	// The per-request cap is checked BEFORE the guest is asked for anything. Directory capacity is
	// enforced atomically after validation under the device publication lock, where refs can be
	// deduplicated accurately.
	if len(packages) > d.maxKeyPackagesPerDevice() {
		return 0, errTooManyKeyPackages(len(packages), d.maxKeyPackagesPerDevice())
	}
	if len(packages) == 0 && lastResort == nil {
		return 0, nil
	}
	// The device's registered key: the dsk_pub the session was established under (or registered
	// with, for the session POST /v1/accounts mints). Read once, before the guest is asked for
	// anything; every package below is bound to it.
	device, err := d.opts.Store.GetDevice(ctx, s.DeviceID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, errForbidden("the session's device is unknown to this instance")
	}
	if err != nil {
		return 0, err
	}
	publicationInstance, err := d.opts.Store.GetInstance(ctx)
	if err != nil {
		return 0, err
	}
	// Preserve the human publication route's cheap full-directory refusal before WASI work. This
	// preflight follows the same generation → user → device lock order as final admission; capacity
	// is checked again in the write transaction because concurrent publishers may race after it.
	// accountKind is users.kind, read in the preflight below; every package's credential must
	// carry it (checkCredentialKind's rule, Task S1).
	var accountKind uint8
	if err := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockInstanceGeneration(ctx); err != nil {
			return err
		}
		current, err := tx.GetInstance(ctx)
		if err != nil {
			return err
		}
		if current.Generation != publicationInstance.Generation {
			return &Error{Code: "E_CONFLICT", Detail: "the instance generation changed during publication", Status: http.StatusConflict}
		}
		if err := tx.LockUserForDeviceRegistration(ctx, s.UserID); err != nil {
			return err
		}
		if err := tx.LockKeyPackageDevice(ctx, s.DeviceID); err != nil {
			return err
		}
		user, err := tx.GetUser(ctx, s.UserID)
		if err != nil {
			return err
		}
		if user.DisabledAt != nil || user.DeletedAt != nil || user.Flags != 0 {
			return errForbidden("the publishing account is disabled or deleted")
		}
		accountKind = user.Kind
		currentDevice, err := tx.GetDevice(ctx, s.DeviceID)
		if err != nil || currentDevice.UserID != s.UserID || currentDevice.RevokedAt != nil || currentDevice.QuarantinedAt != nil || string(currentDevice.DSKPub) != string(device.DSKPub) {
			return errForbidden("the publishing device authority changed")
		}
		held, err := tx.CountKeyPackages(ctx, s.DeviceID, d.now())
		if err != nil {
			return err
		}
		if held+int64(len(packages)) > int64(d.maxKeyPackagesPerDevice()) {
			return errKeyPackageDirectoryFull(held, len(packages), d.maxKeyPackagesPerDevice())
		}
		return nil
	}); err != nil {
		return 0, err
	}

	inst, err := d.opts.Wasm.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer inst.Release()

	rows := make([]store.KeyPackageRow, 0, len(packages)+1)
	add := func(blob []byte, last bool) error {
		info, err := inst.ValidateKeyPackage(ctx, blob)
		if err != nil {
			return errCommitInvalid("key_package", err.Error())
		}
		if string(info.DeviceID) != string(s.DeviceID[:]) {
			return errForbidden("a device may only publish KeyPackages for itself")
		}
		// A new leaf is bound to the authenticated device (protocol/02 invariant 4): the
		// credential names the session's user as well as its device, and the leaf is keyed by the
		// device's registered key. A package that fails either is one no Add may carry, and
		// storing it would only hand a committer a KeyPackage invariant 4 then refuses.
		if string(info.UserID) != string(s.UserID[:]) {
			return errCommitInvalid("key_package",
				"the package's credential names another user than the publishing session's")
		}
		if !leafKeyIsRegistered(device, info.SignatureKey) {
			return errCommitInvalid("key_package",
				"the package's leaf signature key is not the device's registered key")
		}
		// The credential's self-signed kind must fit the publishing account (Task S1,
		// checkCredentialKind's rule): a human account may not publish a Bot-kind leaf. An
		// unreadable kind is a refusal.
		kind, err := decodeCredentialKind(info.Credential)
		if err == nil {
			err = accountKindVerdict(accountKind, kind)
		}
		if err != nil {
			return errCommitInvalid("key_package",
				"the package's credential kind is not the publishing account's kind")
		}
		if info.LastResort != last {
			return errCommitInvalid("key_package",
				"the package's last_resort extension disagrees with where it was published")
		}
		// not_after is the client's. The guest only checks it is in the future, so a lifetime is
		// bounded HERE, and only then converted: an unbounded uint64 wraps negative as an int64.
		if err := checkKeyPackageLifetime(d.now(), info.NotAfter, d.opts.Policy.MaxKeyPackageLifetime); err != nil {
			return err
		}
		var lr uint8
		if last {
			lr = 1
		}
		rows = append(rows, store.KeyPackageRow{
			DeviceID:   s.DeviceID,
			KPRef:      info.KPRef,
			Blob:       blob,
			LastResort: lr,
			// G115: checkKeyPackageLifetime above refused every not_after past now plus
			// MaxKeyPackageLifetime, which is far below 2^63.
			Expires: int64(info.NotAfter), //nolint:gosec // bounded by checkKeyPackageLifetime
			Created: d.now(),
		})
		return nil
	}
	// Nothing is written until every package has passed: a publish is all or nothing, so a client
	// that sent one bad blob in a batch of thirty-two is not left guessing which of them landed.
	for _, blob := range packages {
		if err := add(blob, false); err != nil {
			return 0, err
		}
	}
	if lastResort != nil {
		if err := add(lastResort, true); err != nil {
			return 0, err
		}
	}
	if err := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockInstanceGeneration(ctx); err != nil {
			return err
		}
		currentInstance, err := tx.GetInstance(ctx)
		if err != nil {
			return err
		}
		if currentInstance.Generation != publicationInstance.Generation {
			return &Error{Code: "E_CONFLICT", Detail: "the instance generation changed during publication", Status: http.StatusConflict}
		}
		if err := tx.LockUserForDeviceRegistration(ctx, s.UserID); err != nil {
			return err
		}
		if err := tx.LockKeyPackageDevice(ctx, s.DeviceID); err != nil {
			return err
		}
		currentUser, err := tx.GetUser(ctx, s.UserID)
		if err != nil {
			return err
		}
		if currentUser.DisabledAt != nil || currentUser.DeletedAt != nil || currentUser.Flags != 0 {
			return errForbidden("the publishing account is disabled or deleted")
		}
		currentDevice, err := tx.GetDevice(ctx, s.DeviceID)
		if err != nil {
			return errForbidden("the publishing device is no longer registered")
		}
		if currentDevice.UserID != s.UserID || currentDevice.RevokedAt != nil || currentDevice.QuarantinedAt != nil || string(currentDevice.DSKPub) != string(device.DSKPub) {
			return errForbidden("the publishing device authority changed")
		}
		held, err := tx.CountKeyPackages(ctx, s.DeviceID, d.now())
		if err != nil {
			return err
		}
		adding := 0
		seen := map[string]struct{}{}
		for _, row := range rows {
			if row.LastResort != 0 {
				continue
			}
			if _, ok := seen[string(row.KPRef)]; ok {
				continue
			}
			seen[string(row.KPRef)] = struct{}{}
			if _, err := tx.GetKeyPackage(ctx, s.DeviceID, row.KPRef); errors.Is(err, store.ErrNotFound) {
				adding++
			} else if err != nil {
				return err
			}
		}
		if held+int64(adding) > int64(d.maxKeyPackagesPerDevice()) {
			return errKeyPackageDirectoryFull(held, adding, d.maxKeyPackagesPerDevice())
		}
		for _, row := range rows {
			if row.Expires <= d.now() {
				return errCommitInvalid("key_package", "the package expired before publication was admitted")
			}
		}
		return tx.PutKeyPackages(ctx, s.DeviceID, rows)
	}); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// TakeKeyPackage serves one package for a target device: an unconsumed, unexpired ORDINARY one if
// there is any, otherwise the last-resort package, which is served repeatedly and never consumed.
//
// The choice is the store's, not this function's: the SQL orders by `last_resort ASC, expires` and
// marks `consumed_at` only on an ordinary row, and a Go-side "take one, and if it is the
// last-resort put it back" would race every concurrent joiner.
func (d *DS) TakeKeyPackage(ctx context.Context, s Session, target id.ID) (KeyPackage, error) {
	// Row 11 is enrolled-only. A provisional session is a device that is not yet a member of
	// anything; handing it the directory of an arbitrary device would let it learn which devices
	// exist and burn their packages.
	if s.Scope != auth.ScopeEnrolled {
		return KeyPackage{}, errForbidden("taking a KeyPackage needs an enrolled session")
	}
	row, err := d.opts.Store.TakeKeyPackage(ctx, target, d.now())
	if errors.Is(err, store.ErrNotFound) {
		return KeyPackage{}, errNotFound("key package")
	}
	if err != nil {
		return KeyPackage{}, err
	}
	return KeyPackage{Blob: row.Blob, LastResort: row.LastResort == 1, KPRef: row.KPRef}, nil
}

// KeyPackagesRemaining is what the gateway's `ready` frame reports so a client knows to refill. It
// counts the ORDINARY packages only: the last-resort one is the fallback, not a package a client
// should stop refilling because of.
func (d *DS) KeyPackagesRemaining(ctx context.Context, deviceID id.ID) (uint64, error) {
	n, err := d.opts.Store.CountKeyPackages(ctx, deviceID, d.now())
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, nil
	}
	return uint64(n), nil
}

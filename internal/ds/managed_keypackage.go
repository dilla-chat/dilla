package ds

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/dilla-chat/dilla/internal/auth"
	"github.com/dilla-chat/dilla/internal/bot/access"
	"github.com/dilla-chat/dilla/internal/bot/capability"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// BotKeyPackageReceipt is the historical result returned by the installation receipt endpoint.
type BotKeyPackageReceipt struct {
	Generation uint64
	UploadHash [32]byte
	Count      uint64
}

// BotKeyPackageInventory is a read-only, generation-fenced snapshot of a managed Bot's current
// KeyPackage directory. HasCurrentFallback is encoded as 0 or 1 by the HTTP layer.
type BotKeyPackageInventory struct {
	Generation          uint64
	ServerTime          uint64
	OrdinaryActiveCount uint64
	HasCurrentFallback  uint64
}

// PublishBotKeyPackages accepts one canonical human-directory upload under a managed operation
// ID. Current installation authority is rechecked under the installation and device locks before
// even looking up a historical receipt.
func (d *DS) PublishBotKeyPackages(ctx context.Context, s Session, installation, operation id.ID, expectedGeneration uint64, upload []byte) (uint64, bool, error) {
	if s.Bot == nil || s.Bot.InstallationID != installation || s.Scope != auth.ScopeEnrolled {
		return 0, false, errForbidden("managed KeyPackage publication requires its current installation session")
	}
	packages, fallback, err := decodeKeyPackageUpload(upload)
	if err != nil {
		return 0, false, errInvalid("upload_body is not a canonical KeyPackage upload")
	}
	hash := sha256.Sum256(upload)

	// First serialize with restore and lifecycle changes, and establish current authority before
	// looking at a receipt. Exact retries return here without revalidating historical package data.
	var device store.DeviceRow
	var count uint64
	receiptFound := false
	if err := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		_, currentDevice, err := d.lockManagedKeyPackageAuthority(ctx, tx, s, installation)
		if err != nil {
			return err
		}
		receipt, err := tx.GetBotKeyPackagePublicationReceipt(ctx, installation, operation)
		if err == nil {
			if receipt.UserID != s.UserID || receipt.DeviceID != s.DeviceID || receipt.Generation != expectedGeneration || !equalHash(receipt.UploadSHA256, hash[:]) {
				return &Error{Code: "E_CONFLICT", Detail: "operation ID was already accepted with different publication data", Status: http.StatusConflict}
			}
			count, receiptFound = receipt.Count, true
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		instance, err := tx.GetInstance(ctx)
		if err != nil {
			return err
		}
		if instance.Generation != expectedGeneration {
			return &Error{Code: "E_CONFLICT", Detail: fmt.Sprintf("expected generation %d, current generation %d", expectedGeneration, instance.Generation), Status: http.StatusConflict}
		}
		device = currentDevice
		return nil
	}); err != nil {
		return 0, false, err
	}
	if receiptFound {
		return count, false, nil
	}

	// WASI work happens outside the database transaction. The final transaction below repeats
	// every authority and generation check, and checks expiry again after reacquiring the shared
	// locks so slow crypto or a queue behind another writer cannot admit an expired batch.
	wasm, err := d.opts.Wasm.Acquire(ctx)
	if err != nil {
		return 0, false, err
	}
	defer wasm.Release()
	rows := make([]store.KeyPackageRow, 0, len(packages)+1)
	seen := make(map[string]store.KeyPackageRow, len(packages)+1)
	validate := func(blob []byte, last bool) error {
		info, err := wasm.ValidateKeyPackage(ctx, blob)
		if err != nil {
			return errCommitInvalid("key_package", err.Error())
		}
		if string(info.DeviceID) != string(s.DeviceID[:]) || string(info.UserID) != string(s.UserID[:]) {
			return errForbidden("the package credential does not match the authenticated account and device")
		}
		if !bytes.Equal(info.Credential, device.CredentialBlob) {
			return errForbidden("the package does not carry the installation's verified final Bot credential")
		}
		if !leafKeyIsRegistered(device, info.SignatureKey) {
			return errCommitInvalid("key_package", "the package's leaf signature key is not the device's registered key")
		}
		if info.LastResort != last {
			return errCommitInvalid("key_package", "the package's last_resort extension disagrees with its upload position")
		}
		if err := checkKeyPackageLifetime(d.now(), info.NotAfter, d.opts.Policy.MaxKeyPackageLifetime); err != nil {
			return err
		}
		if info.NotAfter > math.MaxInt64 {
			return errCommitInvalid("key_package", "the package expiry cannot be stored")
		}
		row := store.KeyPackageRow{DeviceID: s.DeviceID, KPRef: info.KPRef, Blob: blob, Expires: int64(info.NotAfter), Created: d.now()}
		if last {
			row.LastResort = 1
		}
		key := string(row.KPRef)
		if previous, ok := seen[key]; ok {
			if previous.LastResort != row.LastResort {
				return errInvalid("the upload repeats one KeyPackage reference in different positions")
			}
			return nil
		}
		seen[key] = row
		rows = append(rows, row)
		return nil
	}
	for _, blob := range packages {
		if err := validate(blob, false); err != nil {
			return 0, false, err
		}
	}
	if fallback != nil {
		if err := validate(fallback, true); err != nil {
			return 0, false, err
		}
	}
	if len(rows) == 0 {
		return 0, false, errInvalid("the upload contains no distinct KeyPackages")
	}

	accepted := false
	err = d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		_, currentDevice, err := d.lockManagedKeyPackageAuthority(ctx, tx, s, installation)
		if err != nil {
			return err
		}
		receipt, err := tx.GetBotKeyPackagePublicationReceipt(ctx, installation, operation)
		if err == nil {
			if receipt.UserID != s.UserID || receipt.DeviceID != s.DeviceID || receipt.Generation != expectedGeneration || !equalHash(receipt.UploadSHA256, hash[:]) {
				return &Error{Code: "E_CONFLICT", Detail: "operation ID was already accepted with different publication data", Status: http.StatusConflict}
			}
			count = receipt.Count
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		instance, err := tx.GetInstance(ctx)
		if err != nil {
			return err
		}
		if instance.Generation != expectedGeneration {
			return &Error{Code: "E_CONFLICT", Detail: fmt.Sprintf("expected generation %d, current generation %d", expectedGeneration, instance.Generation), Status: http.StatusConflict}
		}
		if currentDevice.UserID != device.UserID || currentDevice.UserID != s.UserID || !bytes.Equal(currentDevice.DSKPub, device.DSKPub) || !bytes.Equal(currentDevice.CredentialBlob, device.CredentialBlob) {
			return errForbidden("the registered device credential changed during publication")
		}
		now := d.now()
		ordinaryNew := 0
		for _, row := range rows {
			if row.Expires <= now {
				return errCommitInvalid("key_package", "the package expired while publication was waiting for its device lock")
			}
			if row.LastResort != 0 {
				continue
			}
			if _, err := tx.GetKeyPackage(ctx, s.DeviceID, row.KPRef); errors.Is(err, store.ErrNotFound) {
				ordinaryNew++
			} else if err != nil {
				return err
			}
		}
		held, err := tx.CountKeyPackages(ctx, s.DeviceID, now)
		if err != nil {
			return err
		}
		if held+int64(ordinaryNew) > int64(d.maxKeyPackagesPerDevice()) {
			return errKeyPackageDirectoryFull(held, ordinaryNew, d.maxKeyPackagesPerDevice())
		}
		if err := tx.PutKeyPackages(ctx, s.DeviceID, rows); err != nil {
			return err
		}
		count = uint64(len(rows))
		if err := tx.CreateBotKeyPackagePublicationReceipt(ctx, store.KeyPackagePublicationReceipt{
			InstallationID: installation, UserID: s.UserID, DeviceID: s.DeviceID, OperationID: operation,
			Generation: expectedGeneration, UploadSHA256: hash[:], Count: count, AcceptedAt: now,
		}); err != nil {
			return err
		}
		accepted = true
		return nil
	})
	return count, accepted, err
}

// lockManagedKeyPackageAuthority takes every shared fence in the project-wide order and resolves
// the current Bot binding while those locks remain held. The returned device row is the credential
// snapshot WASI validates outside the transaction.
func (d *DS) lockManagedKeyPackageAuthority(ctx context.Context, tx store.Repository, s Session, installation id.ID) (store.BotInstallationRow, store.DeviceRow, error) {
	initial, err := tx.GetBotInstallation(ctx, installation)
	if err != nil || initial.RuntimeID == nil || initial.RemovedAt != nil {
		return store.BotInstallationRow{}, store.DeviceRow{}, errForbidden("the managed installation has no active runtime")
	}
	if err := tx.LockInstanceGeneration(ctx); err != nil {
		return store.BotInstallationRow{}, store.DeviceRow{}, err
	}
	if err := tx.LockBotRuntime(ctx, *initial.RuntimeID); err != nil {
		return store.BotInstallationRow{}, store.DeviceRow{}, err
	}
	if err := tx.LockBotCommunity(ctx, initial.CommunityID); err != nil {
		return store.BotInstallationRow{}, store.DeviceRow{}, err
	}
	if err := tx.LockBotInstallation(ctx, installation); err != nil {
		return store.BotInstallationRow{}, store.DeviceRow{}, err
	}
	row, err := tx.GetBotInstallation(ctx, installation)
	if err != nil {
		return store.BotInstallationRow{}, store.DeviceRow{}, err
	}
	if row.RuntimeID == nil || *row.RuntimeID != *initial.RuntimeID || row.CommunityID != initial.CommunityID || row.RemovedAt != nil || row.CredentialVerifiedAt == nil {
		return store.BotInstallationRow{}, store.DeviceRow{}, errForbidden("the managed installation changed during publication")
	}
	if err := tx.LockUserForDeviceRegistration(ctx, s.UserID); err != nil {
		return store.BotInstallationRow{}, store.DeviceRow{}, err
	}
	if err := tx.LockKeyPackageDevice(ctx, s.DeviceID); err != nil {
		return store.BotInstallationRow{}, store.DeviceRow{}, err
	}
	binding, bot, err := access.ForUserDevice(ctx, tx, s.UserID, s.DeviceID)
	if err != nil || !bot || binding.InstallationID != installation || binding.CommunityID != s.Bot.CommunityID || binding.State < store.BotInstallationJoining || binding.State > store.BotInstallationReady {
		if err != nil && !errors.Is(err, access.ErrIneligible) && !errors.Is(err, store.ErrNotFound) {
			return store.BotInstallationRow{}, store.DeviceRow{}, err
		}
		return store.BotInstallationRow{}, store.DeviceRow{}, errForbidden("the managed installation no longer controls this account and device")
	}
	if binding.State != s.Bot.State {
		return store.BotInstallationRow{}, store.DeviceRow{}, errForbidden("the managed installation changed state during publication")
	}
	if s.WireVersion != auth.WireVersion {
		return store.BotInstallationRow{}, store.DeviceRow{}, &capability.UpgradeRequired{}
	}
	device, err := tx.GetDevice(ctx, s.DeviceID)
	if errors.Is(err, store.ErrNotFound) {
		return store.BotInstallationRow{}, store.DeviceRow{}, errForbidden("the session device is no longer registered")
	}
	if err != nil {
		return store.BotInstallationRow{}, store.DeviceRow{}, err
	}
	return row, device, nil
}

func (d *DS) BotKeyPackagePublicationReceipt(ctx context.Context, s Session, installation, operation id.ID) (BotKeyPackageReceipt, error) {
	if s.Bot == nil || s.Bot.InstallationID != installation || s.Scope != auth.ScopeEnrolled {
		return BotKeyPackageReceipt{}, errForbidden("managed KeyPackage receipts require the current installation session")
	}
	var out BotKeyPackageReceipt
	initial, err := d.opts.Store.GetBotInstallation(ctx, installation)
	if err != nil || initial.RuntimeID == nil || initial.RemovedAt != nil {
		return BotKeyPackageReceipt{}, errNotFound("key package publication receipt")
	}
	err = d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockInstanceGeneration(ctx); err != nil {
			return err
		}
		if err := tx.LockBotRuntime(ctx, *initial.RuntimeID); err != nil {
			return err
		}
		if err := tx.LockBotCommunity(ctx, initial.CommunityID); err != nil {
			return err
		}
		if err := tx.LockBotInstallation(ctx, installation); err != nil {
			return err
		}
		installationRow, err := tx.GetBotInstallation(ctx, installation)
		if err != nil {
			return err
		}
		if installationRow.RuntimeID == nil || *installationRow.RuntimeID != *initial.RuntimeID || installationRow.CommunityID != initial.CommunityID || installationRow.RemovedAt != nil {
			return errForbidden("the managed installation changed")
		}
		if err := tx.LockUserForDeviceRegistration(ctx, s.UserID); err != nil {
			return err
		}
		if err := tx.LockKeyPackageDevice(ctx, s.DeviceID); err != nil {
			return err
		}
		binding, bot, err := access.ForUserDevice(ctx, tx, s.UserID, s.DeviceID)
		if err != nil || !bot || binding.InstallationID != installation || binding.CommunityID != s.Bot.CommunityID || binding.State < store.BotInstallationJoining || binding.State > store.BotInstallationReady {
			if err != nil && !errors.Is(err, access.ErrIneligible) && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			return errForbidden("the managed installation no longer controls this account and device")
		}
		if binding.State != s.Bot.State {
			return errForbidden("the managed installation changed state")
		}
		if s.WireVersion != auth.WireVersion {
			return &capability.UpgradeRequired{}
		}
		r, err := tx.GetBotKeyPackagePublicationReceipt(ctx, installation, operation)
		if err != nil {
			return err
		}
		if r.UserID != s.UserID || r.DeviceID != s.DeviceID {
			return store.ErrNotFound
		}
		out.Generation, out.Count = r.Generation, r.Count
		copy(out.UploadHash[:], r.UploadSHA256)
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return BotKeyPackageReceipt{}, errNotFound("key package publication receipt")
	}
	return out, err
}

// ManagedBotKeyPackageInventory returns the current directory state under the same generation,
// runtime, community, installation, user, and device fences as publication. All reads and the
// injected clock sample occur while those locks remain held, so the tuple describes one snapshot.
func (d *DS) ManagedBotKeyPackageInventory(ctx context.Context, s Session, installation id.ID) (BotKeyPackageInventory, error) {
	if s.Bot == nil || s.Bot.InstallationID != installation || s.Scope != auth.ScopeEnrolled {
		return BotKeyPackageInventory{}, errForbidden("managed KeyPackage inventory requires the current installation session")
	}
	var out BotKeyPackageInventory
	err := d.opts.Store.Tx(ctx, func(tx store.Repository) error {
		_, _, err := d.lockManagedKeyPackageAuthority(ctx, tx, s, installation)
		if err != nil {
			return err
		}
		instance, err := tx.GetInstance(ctx)
		if err != nil {
			return err
		}
		now := d.now()
		if now < 0 || instance.Generation > math.MaxInt64 {
			return errForbidden("the directory snapshot cannot be represented by the wire format")
		}
		count, err := tx.CountKeyPackages(ctx, s.DeviceID, now)
		if err != nil {
			return err
		}
		if count < 0 {
			return errForbidden("the directory count cannot be represented by the wire format")
		}
		fallback, err := tx.HasCurrentLastResortKeyPackage(ctx, s.DeviceID, now)
		if err != nil {
			return err
		}
		out = BotKeyPackageInventory{
			Generation:          instance.Generation,
			ServerTime:          uint64(now),
			OrdinaryActiveCount: uint64(count),
		}
		if fallback {
			out.HasCurrentFallback = 1
		}
		return nil
	})
	return out, err
}

func decodeKeyPackageUpload(raw []byte) ([][]byte, []byte, error) {
	var value []any
	if len(raw) == 0 || len(raw) > 1_047_296 || cborx.Unmarshal(raw, &value) != nil || len(value) != 2 {
		return nil, nil, errors.New("invalid upload")
	}
	canonical, err := cborx.Marshal(value)
	if err != nil || string(canonical) != string(raw) {
		return nil, nil, errors.New("noncanonical upload")
	}
	values, ok := value[0].([]any)
	if !ok || len(values) < 1 || len(values) > 32 {
		return nil, nil, errors.New("invalid package list")
	}
	packages := make([][]byte, len(values))
	for i, value := range values {
		b, ok := value.([]byte)
		if !ok || len(b) == 0 {
			return nil, nil, errors.New("invalid package")
		}
		packages[i] = append([]byte(nil), b...)
	}
	var fallback []byte
	if value[1] != nil {
		b, ok := value[1].([]byte)
		if !ok || len(b) == 0 {
			return nil, nil, errors.New("invalid fallback")
		}
		fallback = append([]byte(nil), b...)
	}
	return packages, fallback, nil
}

func equalHash(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var different byte
	for i := range a {
		different |= a[i] ^ b[i]
	}
	return different == 0
}

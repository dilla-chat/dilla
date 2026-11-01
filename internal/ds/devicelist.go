package ds

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonasthim/dilla/internal/auth"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// DeviceLists decodes and verifies a user's signed device list, which is invariant 4's DSK clause:
// "its DSK is in the newest signed device list". The decoder and the ssk_signature check are the
// core's (`core/dilla-core/src/identity/device_list.rs`), reached through the guest's
// `device_list_entries` export (ABI v3), so the Go side never re-implements the format and cannot
// disagree with the client that produced it.
// Entries reads and verifies the stored newest list; Verify checks a candidate before
// PUT /v1/users/{id}/device-list stores it.
//
// It is declared here, with its Plan-1 implementation, because ds.Options names the seam (the same
// reason as ACL and Channels).
type DeviceLists interface {
	// WithVerification reserves a guest before a transaction and lends it to
	// Verify for the callback lifetime. Wrappers must forward this method too.
	WithVerification(context.Context, func(context.Context) error) error

	// Entries verifies the user's newest stored list against the user's ssk_pub and returns the
	// (device_id, dsk_pub) pair of every entry that is not revoked. A device is listed only when
	// both its id and its key match one entry (auth.Listed; security review F2). A missing list, or
	// one that does not verify, is an error, and an error is a refusal.
	//
	// v is the guest to verify in. Every caller on the commit and heal paths already holds a wasm
	// instance — the group's own, inside withGroup — and passes it (a *mlswasi.PublicGroup
	// satisfies DeviceListVerifier): acquiring a second instance there would wait on a pool the
	// state cache may have filled, which is the deadlock state.go's accounting exists to rule out.
	// nil means "acquire one", for a caller that holds none.
	Entries(ctx context.Context, v DeviceListVerifier, userID id.ID) ([]auth.ListedDevice, error)
	// Verify decodes a candidate list in the guest and verifies its ssk_signature against
	// sskPub and that it names userID; it reads nothing from the store and returns every
	// entry, revoked ones flagged.
	Verify(ctx context.Context, blob, sskPub []byte, userID id.ID) ([]mlswasi.DeviceListEntry, error)
}

// DeviceListVerifier is the guest a device list is decoded and verified in: *mlswasi.Instance and
// *mlswasi.PublicGroup both satisfy it.
type DeviceListVerifier interface {
	DeviceListEntries(ctx context.Context, list, sskPub, userID []byte) ([]mlswasi.DeviceListEntry, error)
}

// ErrDeviceListUnavailable is what DeviceLists answers when it has no guest to verify in: no
// verifier was passed and the delivery service was built without a wasm runtime.
var ErrDeviceListUnavailable = errors.New("ds: the device-list verifier is not available")

// ErrNoDeviceList is a user who has published no signed device list. Invariant 4 refuses an Add of
// any of that user's devices: a device that appears in no list the user signed is not one the user
// authorised, however the instance came to know it.
var ErrNoDeviceList = errors.New("ds: the user has published no signed device list")

// NewDeviceLists builds the DeviceLists invariant 4's Add clause uses. It reads the newest list
// through repo (`device_lists`, written verbatim by PUT /v1/users/{id}/device-list) and the user's
// ssk_pub (`users.ssk_pub`), and verifies the list in the guest.
//
// NV-B8, resolved by task 27a (Ruling C, deviation B32): until then Entries failed closed for every
// user, so every Add refused with rule = "add_key_package". The substring search it replaced was
// never equivalent — a 32-byte window can fall inside another entry's signature or any
// attacker-influenced field — and the guest's decoder is what makes the comparison a membership
// test over decoded entries.
func NewDeviceLists(repo store.Repository, wasm *mlswasi.Runtime) DeviceLists {
	return &coreDeviceLists{repo: repo, wasm: wasm}
}

type coreDeviceLists struct {
	repo store.Repository
	wasm *mlswasi.Runtime
}

func (c *coreDeviceLists) Verify(ctx context.Context, blob, sskPub []byte, userID id.ID) ([]mlswasi.DeviceListEntry, error) {
	if scope, ok := ctx.Value(deviceListScopeKey{}).(*deviceListScope); ok {
		return scope.verify(ctx, c.wasm, blob, sskPub, userID)
	}
	if c.wasm == nil {
		return nil, ErrDeviceListUnavailable
	}
	inst, err := c.wasm.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer inst.Release()
	return inst.DeviceListEntries(ctx, blob, sskPub, userID[:])
}

func (c *coreDeviceLists) Entries(ctx context.Context, v DeviceListVerifier, userID id.ID) ([]auth.ListedDevice, error) {
	row, err := c.repo.GetDeviceList(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNoDeviceList
	}
	if err != nil {
		return nil, err
	}
	user, err := c.repo.GetUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("ds: the device list's user: %w", err)
	}
	if v == nil {
		if c.wasm == nil {
			return nil, ErrDeviceListUnavailable
		}
		inst, err := c.wasm.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer inst.Release()
		v = inst
	}
	entries, err := v.DeviceListEntries(ctx, row.Blob, user.SSKPub, userID[:])
	if err != nil {
		return nil, err
	}
	listed := make([]auth.ListedDevice, 0, len(entries))
	for _, e := range entries {
		// An entry whose device_id is not 16 bytes names no device row and can match none.
		if e.Revoked || len(e.DeviceID) != len(id.ID{}) {
			continue
		}
		var dev id.ID
		copy(dev[:], e.DeviceID)
		listed = append(listed, auth.ListedDevice{DeviceID: dev, DSKPub: e.DSKPub})
	}
	return listed, nil
}

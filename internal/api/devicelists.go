package api

// devicelists.go is the device-list publication's server half: the verifier seam, the
// outer-element read, and the prompt Removes of a revoked device's leaves.

import (
	"context"
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"

	"github.com/jonasthim/dilla/internal/cborx"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/mlswasi"
	"github.com/jonasthim/dilla/internal/store"
)

// DeviceListVerifier verifies a candidate list in the guest before it is stored.
type DeviceListVerifier interface {
	// WithVerification reserves the native verifier before a transaction.
	// A wrapper must forward this alongside Verify to preserve resource order.
	WithVerification(context.Context, func(context.Context) error) error

	Verify(ctx context.Context, blob, sskPub []byte, userID id.ID) ([]mlswasi.DeviceListEntry, error)
}

// L-HTTP-56 and head ruling 19 cap one history page at 64 lists.
const deviceListHistoryPage = 64

func listOuter(blob []byte) (version uint64, prevHash, sig []byte, ok bool) {
	var outer []cbor.RawMessage
	if err := cborx.Unmarshal(blob, &outer); err != nil || len(outer) != 6 {
		return 0, nil, nil, false
	}
	if err := cborx.Unmarshal(outer[2], &version); err != nil {
		return 0, nil, nil, false
	}
	if err := cborx.Unmarshal(outer[3], &prevHash); err != nil {
		return 0, nil, nil, false
	}
	if err := cborx.Unmarshal(outer[5], &sig); err != nil {
		return 0, nil, nil, false
	}
	return version, prevHash, sig, true
}

// RevokedRemoveError is one refused prompt Remove; the composition root logs each with its group
// and device.
type RevokedRemoveError struct {
	Group, Device id.ID
	Err           error
}

func (e *RevokedRemoveError) Error() string {
	return fmt.Sprintf("revoked device %s in group %s: %v", e.Device, e.Group, e.Err)
}

func (e *RevokedRemoveError) Unwrap() error { return e.Err }

// RemoveRevokedDevices promptly proposes Removes for a revoked device's live leaves. The
// reconcile sweep is the durable backstop; a pairing or interaction group may refuse an
// instance Remove, which the caller only logs.
func RemoveRevokedDevices(ctx context.Context, repo store.Repository, dsvc DS, devices []id.ID) error {
	if dsvc == nil {
		return errNoDS
	}
	var errs []error
	for _, device := range devices {
		groups, err := repo.GroupsForDevice(ctx, device)
		if err != nil {
			errs = append(errs, fmt.Errorf("groups of revoked device %s: %w", device, err))
			continue
		}
		for _, group := range groups {
			if err := dsvc.ProposeRemoveDevice(ctx, group, device, id.New()); err != nil {
				errs = append(errs, &RevokedRemoveError{Group: group, Device: device, Err: err})
			}
		}
	}
	return errors.Join(errs...)
}

package protocol

import "github.com/dilla-chat/dilla/internal/cborx"

// Device-list push bounds, as the core's validate_history_from.
const (
	MaxDeviceListPushRows  = 1024
	MaxDeviceListPushBytes = 4 << 20
)

// DeviceListRow is one device-list version as the device-list PUT body carries it.
type DeviceListRow struct {
	Version      uint64
	Blob         []byte // the signed 6-element list
	SSKSignature [64]byte
	PrevHash     [32]byte
}

// DeviceListPush is what dillad pushes to the Bot when a user's device list changes, and the
// body of helper op 24 (Bot v2 task 8). Deterministic CBOR
// `[umk_pub(32), ssk_pub(32), sig_umk_ssk(64), [[version, blob, ssk_signature(64), prev_hash(32)], ...]]`.
//
// History holds 1..=1024 rows in version order. It must start at version 1 for a user the Bot
// has not pinned, or for a UMK other than the pinned one (the helper verifies the whole chain
// before it pins); once the Bot holds a pinned list, it may start anywhere at or below the
// Bot's cached version + 1. A head pushed alone that skips versions leaves the user "behind"
// (commands refused) until a history reaching it arrives.
type DeviceListPush struct {
	UMK       [32]byte
	SSK       [32]byte
	SigUMKSSK [64]byte
	History   []DeviceListRow
}

func EncodeDeviceListPush(p DeviceListPush) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	rows := make([]any, len(p.History))
	for i, r := range p.History {
		rows[i] = []any{r.Version, r.Blob, r.SSKSignature[:], r.PrevHash[:]}
	}
	b, err := cborx.Marshal([]any{p.UMK[:], p.SSK[:], p.SigUMKSSK[:], rows})
	if err != nil || len(b) > MaxDeviceListPushBytes {
		return nil, invalid()
	}
	return b, nil
}

func DecodeDeviceListPush(b []byte) (DeviceListPush, error) {
	var p DeviceListPush
	if len(b) > MaxDeviceListPushBytes {
		return p, ErrLimit
	}
	n, err := parse(b)
	if err != nil || n.major != 4 || len(n.a) != 4 {
		return p, invalid()
	}
	a := n.a
	sized := func(x node, size int) bool { return x.major == 2 && len(x.b) == size }
	if !sized(a[0], 32) || !sized(a[1], 32) || !sized(a[2], 64) || a[3].major != 4 {
		return p, invalid()
	}
	copy(p.UMK[:], a[0].b)
	copy(p.SSK[:], a[1].b)
	copy(p.SigUMKSSK[:], a[2].b)
	for _, row := range a[3].a {
		if row.major != 4 || len(row.a) != 4 || row.a[0].major != 0 || row.a[1].major != 2 || !sized(row.a[2], 64) || !sized(row.a[3], 32) {
			return DeviceListPush{}, invalid()
		}
		r := DeviceListRow{Version: row.a[0].u, Blob: append([]byte(nil), row.a[1].b...)}
		copy(r.SSKSignature[:], row.a[2].b)
		copy(r.PrevHash[:], row.a[3].b)
		p.History = append(p.History, r)
	}
	if err := p.validate(); err != nil {
		return DeviceListPush{}, err
	}
	return p, nil
}

func (p DeviceListPush) validate() error {
	if allZero(p.UMK[:]) || allZero(p.SSK[:]) || len(p.History) == 0 || len(p.History) > MaxDeviceListPushRows {
		return invalid()
	}
	for _, r := range p.History {
		if r.Version == 0 || len(r.Blob) == 0 {
			return invalid()
		}
	}
	return nil
}

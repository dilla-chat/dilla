package client

import (
	"bytes"
	"context"
	"sort"

	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// MaxDeviceCandidates bounds the number of entries accepted from one device list.
const MaxDeviceCandidates = 256

// DeviceCandidates returns unrevoked device IDs from the user's current signed-list envelope.
// The returned IDs are untrusted candidate hints: callers must authorize every device against
// the admission/native signed snapshot. This Go helper does not verify the device-list signature.
func (s *Session) DeviceCandidates(ctx context.Context, user id.ID) ([]id.ID, error) {
	raw, err := s.OwnDeviceList(ctx, user)
	if err != nil {
		return nil, err
	}
	return parseDeviceCandidates(raw, user)
}

func parseDeviceCandidates(raw []byte, user id.ID) ([]id.ID, error) {
	if user.IsZero() || len(raw) == 0 || len(raw) > MaxSmallBody || cborx.CheckDeterministic(raw) != nil {
		return nil, ErrProtocol
	}
	outer, err := decode(raw, 4)
	if err != nil {
		return nil, ErrProtocol
	}
	outerVersion, ok := asUint(outer[0])
	if !ok || outerVersion == 0 {
		return nil, ErrProtocol
	}
	blob, ok := asBytes(outer[1], -1)
	if !ok || len(blob) == 0 || len(blob) > MaxSmallBody || cborx.CheckDeterministic(blob) != nil {
		return nil, ErrProtocol
	}
	outerSig, ok := asBytes(outer[2], 64)
	if !ok {
		return nil, ErrProtocol
	}
	outerPrev, ok := asBytes(outer[3], 32)
	if !ok {
		return nil, ErrProtocol
	}
	var list []any
	if cborx.Unmarshal(blob, &list) != nil || len(list) != 6 {
		return nil, ErrProtocol
	}
	v, ok := asUint(list[0])
	if !ok || v != 1 {
		return nil, ErrProtocol
	}
	listUser, ok := asID(list[1])
	if !ok || !equalID(listUser, user) {
		return nil, ErrProtocol
	}
	version, ok := asUint(list[2])
	if !ok || version == 0 || version != outerVersion {
		return nil, ErrProtocol
	}
	prev, ok := asBytes(list[3], 32)
	if !ok || !bytes.Equal(prev, outerPrev) {
		return nil, ErrProtocol
	}
	entries, ok := list[4].([]any)
	if !ok || len(entries) > MaxDeviceCandidates {
		return nil, ErrProtocol
	}
	sig, ok := asBytes(list[5], 64)
	if !ok || !bytes.Equal(sig, outerSig) {
		return nil, ErrProtocol
	}

	seen := make(map[id.ID]struct{}, len(entries))
	candidates := make([]id.ID, 0, len(entries))
	for _, rawEntry := range entries {
		entry, ok := rawEntry.([]any)
		if !ok || len(entry) != 5 {
			return nil, ErrProtocol
		}
		device, ok := asID(entry[0])
		if !ok {
			return nil, ErrProtocol
		}
		if _, ok := asBytes(entry[1], 32); !ok {
			return nil, ErrProtocol
		}
		tier, ok := asUint(entry[2])
		if !ok || tier > 1 {
			return nil, ErrProtocol
		}
		if _, ok := asUint(entry[3]); !ok {
			return nil, ErrProtocol
		}
		revoked := entry[4] != nil
		if revoked {
			if _, ok := asUint(entry[4]); !ok {
				return nil, ErrProtocol
			}
		}
		if revoked {
			continue
		}
		if _, exists := seen[device]; exists {
			continue
		}
		seen[device] = struct{}{}
		candidates = append(candidates, device)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return bytes.Compare(candidates[i][:], candidates[j][:]) < 0
	})
	return candidates, nil
}

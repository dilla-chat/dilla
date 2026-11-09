package service

import (
	"context"
	"errors"
	"math"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/cborx"
	"github.com/dilla-chat/dilla/internal/id"
)

// Bounded signed device-list history reads for a removal proof's actor (RemovalSync). The native core verifies every
// row; this side only bounds and orders what it fetches.
const (
	maxSenderHistoryVersions = 1024
	maxSenderHistoryBytes    = 4 << 20
	maxSenderOuterRowBytes   = 1 << 20
	senderHistoryPageSize    = 64
)

var ErrSenderHistoryStale = errors.New("sender device-list history is stale")

type senderHistorySession interface {
	OwnDeviceList(context.Context, id.ID) ([]byte, error)
	DeviceListHistory(context.Context, id.ID, uint64) ([][]any, error)
}

func readSenderHistory(ctx context.Context, session senderHistorySession, user id.ID, cachedVersion uint64) ([]byte, []byte, uint64, error) {
	if session == nil || user.IsZero() || cachedVersion > math.MaxInt64 {
		return nil, nil, 0, ErrSenderHistoryStale
	}
	var rowsUsed, bytesUsed uint64
	for attempt := 0; attempt < 2; attempt++ {
		if err := client.CooperativeCheckpoint(ctx); err != nil {
			return nil, nil, 0, err
		}
		candidateRaw, err := session.OwnDeviceList(ctx, user)
		if err != nil {
			return nil, nil, 0, err
		}
		candidate, version, err := decodeSenderOuter(candidateRaw)
		if err != nil || version < cachedVersion || version-cachedVersion > maxSenderHistoryVersions {
			return nil, nil, 0, ErrSenderHistoryStale
		}
		if uint64(len(candidateRaw)) > maxSenderOuterRowBytes {
			return nil, nil, 0, ErrSenderHistoryStale
		}
		candidateBytes := uint64(len(candidateRaw))
		if bytesUsed+candidateBytes > maxSenderHistoryBytes {
			return nil, nil, 0, ErrSenderHistoryStale
		}
		bytesUsed += candidateBytes
		collected := make([]any, 0, version-cachedVersion+1)
		after := uint64(0)
		if cachedVersion > 0 {
			after = cachedVersion - 1
		}
		overranCandidate := false
		if version == cachedVersion {
			collected = append(collected, candidate)
			rowsUsed++
		} else {
			for after < version {
				if err = client.CooperativeCheckpoint(ctx); err != nil {
					return nil, nil, 0, err
				}
				page, pageErr := session.DeviceListHistory(ctx, user, after)
				if pageErr != nil {
					return nil, nil, 0, pageErr
				}
				if len(page) == 0 || len(page) > senderHistoryPageSize {
					return nil, nil, 0, ErrSenderHistoryStale
				}
				for _, row := range page {
					rowVersion, rowRaw, rowErr := encodeSenderOuter(row)
					if rowErr != nil || rowVersion != after+1 {
						return nil, nil, 0, ErrSenderHistoryStale
					}
					if rowVersion > version {
						overranCandidate = true
						break
					}
					rowsUsed++
					bytesUsed += uint64(len(rowRaw))
					if rowsUsed > maxSenderHistoryVersions || bytesUsed > maxSenderHistoryBytes {
						return nil, nil, 0, ErrSenderHistoryStale
					}
					collected = append(collected, row)
					after = rowVersion
				}
				if overranCandidate {
					break
				}
			}
		}
		if rowsUsed > maxSenderHistoryVersions || bytesUsed > maxSenderHistoryBytes {
			return nil, nil, 0, ErrSenderHistoryStale
		}
		if !overranCandidate {
			if len(collected) == 0 {
				return nil, nil, 0, ErrSenderHistoryStale
			}
			lastRow, ok := collected[len(collected)-1].([]any)
			if !ok {
				return nil, nil, 0, ErrSenderHistoryStale
			}
			lastVersion, lastRaw, lastErr := encodeSenderOuter(lastRow)
			if lastErr != nil || lastVersion != version || !bytesEqual(lastRaw, candidateRaw) {
				return nil, nil, 0, ErrSenderHistoryStale
			}
		}
		currentRaw, readErr := session.OwnDeviceList(ctx, user)
		if readErr != nil {
			return nil, nil, 0, readErr
		}
		current, currentVersion, decodeErr := decodeSenderOuter(currentRaw)
		if decodeErr == nil && currentVersion == version && bytesEqual(currentRaw, candidateRaw) && !overranCandidate {
			innerValue, ok := candidate[1].([]byte)
			if !ok {
				return nil, nil, 0, ErrSenderHistoryStale
			}
			inner := append([]byte(nil), innerValue...)
			body, marshalErr := cborx.Marshal(collected)
			if marshalErr != nil || len(body) > maxSenderHistoryBytes {
				return nil, nil, 0, ErrSenderHistoryStale
			}
			return body, inner, version, nil
		}
		// Only a moving forward head justifies the one bounded retry. A rollback, malformed head,
		// or changed bytes at the same version is stale and must never be trusted.
		if decodeErr != nil || currentVersion <= version {
			return nil, nil, 0, ErrSenderHistoryStale
		}
		_ = current
	}
	return nil, nil, 0, ErrSenderHistoryStale
}

func decodeSenderOuter(raw []byte) ([]any, uint64, error) {
	if len(raw) == 0 || len(raw) > maxSenderOuterRowBytes {
		return nil, 0, ErrSenderHistoryStale
	}
	var row []any
	if err := cborx.Unmarshal(raw, &row); err != nil {
		return nil, 0, ErrSenderHistoryStale
	}
	version, _, err := encodeSenderOuter(row)
	if err != nil {
		return nil, 0, err
	}
	return row, version, nil
}

func encodeSenderOuter(row []any) (uint64, []byte, error) {
	if len(row) != 4 {
		return 0, nil, ErrSenderHistoryStale
	}
	version, ok := row[0].(uint64)
	if !ok || version == 0 || version > math.MaxInt64 {
		return 0, nil, ErrSenderHistoryStale
	}
	if _, ok = row[1].([]byte); !ok {
		return 0, nil, ErrSenderHistoryStale
	}
	signature, ok := row[2].([]byte)
	if !ok || len(signature) != 64 {
		return 0, nil, ErrSenderHistoryStale
	}
	previous, ok := row[3].([]byte)
	if !ok || len(previous) != 32 {
		return 0, nil, ErrSenderHistoryStale
	}
	raw, err := cborx.Marshal(row)
	if err != nil || len(raw) > maxSenderOuterRowBytes {
		return 0, nil, ErrSenderHistoryStale
	}
	return version, raw, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

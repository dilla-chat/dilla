package native

import (
	"context"
	"errors"
	"strings"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

const maxCallEpochRoster = 4096

// CallMediaEpoch fetches the active call's current media epoch from the native helper.
func (c *Client) CallMediaEpoch(ctx context.Context, call id.ID) (CallMediaEpoch, error) {
	if call.IsZero() {
		return CallMediaEpoch{}, errors.New("invalid native call ID")
	}
	v, err := c.call(ctx, protocol.HelperCallEpoch, []any{call}, false, wrappedBody(decodeCallMediaEpoch))
	if err != nil {
		return CallMediaEpoch{}, err
	}
	return resultAs[CallMediaEpoch](v)
}

func decodeCallMediaEpoch(body []byte) (any, error) {
	a, err := decodeArrayBytes(body, 5)
	if err != nil {
		return nil, errors.New("call epoch shape")
	}
	epoch, ok := nativeUint(a[0])
	if !ok {
		return nil, errors.New("call epoch number")
	}
	key, ok := a[1].([]byte)
	if !ok || len(key) != 16 || allZeroCallKey(key) {
		return nil, errors.New("call epoch key shape")
	}
	ownLeaf, ok := nativeUint(a[2])
	if !ok || ownLeaf > uint64(^uint16(0)) {
		return nil, errors.New("call epoch leaf")
	}
	rows, ok := a[3].([]any)
	if !ok || len(rows) == 0 || len(rows) > maxCallEpochRoster {
		return nil, errors.New("call epoch roster shape")
	}
	code, ok := a[4].(string)
	if !ok || !validCallSAS(code) {
		return nil, errors.New("call epoch code shape")
	}
	result := CallMediaEpoch{Epoch: epoch, OwnLeaf: ownLeaf, Roster: make([]CallMediaMember, 0, len(rows)), Code: code}
	copy(result.BaseKey[:], key)
	seenOwn := false
	for _, raw := range rows {
		row, ok := raw.([]any)
		if !ok || len(row) != 3 {
			return nil, errors.New("call epoch roster row")
		}
		leaf, ok := nativeUint(row[0])
		device, deviceOK := callEpochID(row[1])
		user, userOK := callEpochID(row[2])
		if !ok || leaf > uint64(^uint32(0)) || !deviceOK || !userOK {
			return nil, errors.New("call epoch roster value")
		}
		if leaf == ownLeaf {
			seenOwn = true
			if device.IsZero() || user.IsZero() {
				return nil, errors.New("call epoch own roster value")
			}
		}
		result.Roster = append(result.Roster, CallMediaMember{Leaf: leaf, DeviceID: device, UserID: user})
	}
	if !seenOwn {
		return nil, errors.New("call epoch own leaf missing")
	}
	return result, nil
}

func callEpochID(v any) (id.ID, bool) {
	b, ok := v.([]byte)
	if !ok || len(b) != 16 {
		return id.Zero, false
	}
	var out id.ID
	copy(out[:], b)
	return out, !out.IsZero()
}

func allZeroCallKey(b []byte) bool {
	var aggregate byte
	for _, v := range b {
		aggregate |= v
	}
	return aggregate == 0
}

func validCallSAS(s string) bool {
	parts := strings.Split(s, " ")
	if len(parts) != 6 {
		return false
	}
	for _, part := range parts {
		if len(part) != 5 {
			return false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
}

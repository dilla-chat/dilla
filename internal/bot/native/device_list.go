package native

import (
	"context"
	"errors"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
	"github.com/dilla-chat/dilla/internal/id"
)

// DeviceListState is helper op 24's verdict on a user's cached device list after an ingest.
type DeviceListState uint8

const (
	// DeviceListCurrent: the cache holds the pushed head; the user's commands are judged against it.
	DeviceListCurrent DeviceListState = 0
	// DeviceListHeld: the push was recorded, but a key change is pending (a UMK other than the pinned one); the helper
	// refuses the user's commands until the change is verified.
	DeviceListHeld DeviceListState = 1
	// DeviceListBehind: the pushed head does not chain from the cache (a gap or a broken link); the cache is unchanged
	// and the helper refuses the user's commands until a history reaching that head is ingested.
	DeviceListBehind DeviceListState = 2
)

// DeviceListIngest is op 24's answer: whether the cache changed, its version afterwards (0 for none) and the state.
type DeviceListIngest struct {
	Changed bool
	Version uint64
	State   DeviceListState
}

// IngestDeviceList hands the helper a device-list push for user (op 24), the body of an op 65 device_list event
// exactly as dillad sent it (protocol.DeviceListPush: the whole signed history from version 1). It is the only way the
// helper's device-list cache changes; a user the helper has not seen is pinned (trust on first use). A push the helper
// refuses outright is a *helper.StatusError and changes nothing; held and behind are answers, not errors.
func (c *Client) IngestDeviceList(ctx context.Context, user id.ID, push []byte) (DeviceListIngest, error) {
	now := time.Now().Unix()
	if user.IsZero() || len(push) == 0 || len(push) > protocol.MaxDeviceListPushBytes || now <= 0 {
		return DeviceListIngest{}, errors.New("invalid device list ingest request")
	}
	v, err := c.call(ctx, protocol.HelperDeviceListIngest, []any{user, push, uint64(now)}, true, func(result []any) (any, error) {
		if len(result) != 3 {
			return nil, errors.New("device list ingest result shape")
		}
		changed, okChanged := result[0].(uint64)
		version, okVersion := result[1].(uint64)
		state, okState := result[2].(uint64)
		if !okChanged || !okVersion || !okState || changed > 1 || state > 2 {
			return nil, errors.New("device list ingest result")
		}
		return DeviceListIngest{Changed: changed == 1, Version: version, State: DeviceListState(state)}, nil
	})
	if err != nil {
		return DeviceListIngest{}, err
	}
	return resultAs[DeviceListIngest](v)
}

//go:build linux

package main

import (
	"errors"

	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/id"
)

func configuredWelcomeChannel(rows []client.Channel, community, channel id.ID) (client.Channel, error) {
	for _, row := range rows {
		if row.ID != channel {
			continue
		}
		if row.CommunityID != community || row.Kind != 0 || row.TextGroupID == nil || row.TextGroupID.IsZero() {
			return client.Channel{}, errors.New("configured welcome destination is not an accessible text channel")
		}
		return row, nil
	}
	return client.Channel{}, errors.New("configured welcome destination is unavailable")
}

// botRolesCapability is the installation's roles capability bit (role panels). Bot v2 Task 12 took the
// welcome handler, a structural-event consumer, out with the poller; Task 14 brings it back on op 65.
const botRolesCapability uint64 = 1 << 2

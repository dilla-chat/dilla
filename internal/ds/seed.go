// Package ds rebuilds the gateway's in-memory group registry from SQL at start, so a cold
// instance can deliver to quiet groups and elect their committers (C12, gap G5).
package ds

import (
	"context"
	"fmt"

	"github.com/dilla-chat/dilla/internal/id"
)

const seedPage int32 = 256

// SeedGateway rebuilds the gateway's fan-out list and leaf map for every open group from
// mls_members, so a restarted instance delivers group frames and elects committers before the
// group's next commit. It returns how many groups it seeded. A nil gateway seeds nothing.
func (d *DS) SeedGateway(ctx context.Context) (int, error) {
	if d.opts.Gateway == nil {
		return 0, nil
	}
	after := id.ID{}
	n := 0
	for {
		page, err := d.opts.Store.ListOpenGroups(ctx, after, seedPage)
		if err != nil {
			return n, fmt.Errorf("ds: seed the gateway: list open groups after %s: %w", after, err)
		}
		for _, row := range page {
			after = row.GroupID
			if err := d.seedGroup(ctx, row.GroupID); err != nil {
				return n, err
			}
			n++
		}
		if len(page) < int(seedPage) {
			break
		}
	}
	d.log().Info("seeded the gateway's group registry", "groups", n)
	return n, nil
}

func (d *DS) seedGroup(ctx context.Context, groupID id.ID) error {
	unlock := d.lock(groupID)
	defer unlock()
	rows, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return fmt.Errorf("ds: seed the gateway: members of %s: %w", groupID, err)
	}
	devices := make([]id.ID, 0, len(rows))
	leaves := make(map[id.ID]uint32, len(rows))
	for _, row := range rows {
		if row.RemovedEpoch != nil {
			continue
		}
		devices = append(devices, row.DeviceID)
		leaves[row.DeviceID] = row.LeafIndex
	}
	d.opts.Gateway.SetGroupMembers(groupID, devices)
	d.opts.Gateway.SetGroupLeaves(groupID, leaves)
	return nil
}

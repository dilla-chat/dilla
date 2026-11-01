package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// DS is the delivery-service surface the api package calls. It is a subset of
// *ds.DS (interfaces.md §6.2) so that the api tests can record the calls a
// membership change makes without standing in for the delivery service itself.
type DS interface {
	ProposeAdd(ctx context.Context, groupID, deviceID, actionID id.ID) error
	ProposeRemove(ctx context.Context, groupID id.ID, leaf uint32, actionID id.ID) error
	// ProposeRemoveOf is ProposeRemove for a leaf read outside the delivery service's group lock:
	// the Remove is issued only while deviceID still holds leaf, and is refused like a Remove of a
	// leaf that is gone otherwise, so a leaf reused in between never redirects it.
	ProposeRemoveOf(ctx context.Context, groupID id.ID, leaf uint32, deviceID, actionID id.ID) error
	// ProposeRemoveOfMember is ProposeRemoveOf bound to one membership: the Remove is issued only
	// while deviceID holds leaf from addedEpoch (the epoch it took the leaf in), and is refused with
	// ds.ErrRemoveTargetGone once the device left the leaf or holds it again from a later epoch. The
	// call paths use it for a departed device (DS-7, ruling (a) of the calls re-review).
	ProposeRemoveOfMember(ctx context.Context, groupID id.ID, leaf uint32, deviceID id.ID, addedEpoch uint64, actionID id.ID) error
	// ProposeRemoveDevice proposes removing deviceID's leaf, resolving it under the group lock, and
	// drops the action when the device holds no leaf or an instance Remove of that leaf already
	// stands (DEV-45). The call paths use it: a call leaf is short-lived and races the device's own
	// Remove.
	ProposeRemoveDevice(ctx context.Context, groupID, deviceID, actionID id.ID) error
	ProposeAddBatch(ctx context.Context, groupID id.ID, devices []id.ID) error
	Close(ctx context.Context, groupID id.ID) error
	// CloseIfAlone is Close while deviceID holds the group's only current non-bot leaf, decided under the
	// group lock; ds.ErrNotAlone otherwise (the conditional end of a call, protocol/09 § Voice).
	CloseIfAlone(ctx context.Context, groupID, deviceID id.ID) error
	// VoidIneligibleAdds voids the group's outstanding instance Adds whose device or user is no
	// longer eligible. Every removal path calls it for each group it visits: an Add for a user
	// who has just lost access is one no member commit can satisfy (invariant 4's clause 1 and
	// its ACL clause pull in opposite directions), so without the void the group stays frozen
	// until the Add's TTL.
	VoidIneligibleAdds(ctx context.Context, groupID id.ID) error
}

// afterCommit is the context every delivery-service step that follows a
// committed membership or structure change runs on: the request's values
// without its cancellation. The change has already landed, so a client that
// disconnects must not abort the Removes, Adds and Closes that make the groups
// match it; a cancelled context would fail every repository read and proposal
// still to come, leaving a removed user with a live leaf (fix wave C3).
func afterCommit(r *http.Request) context.Context {
	return context.WithoutCancel(r.Context())
}

// errNoDS is what a membership change answers when the handler was built
// without a delivery service: a mis-wired composition root must not silently
// leave a removed user in the channel's groups.
var errNoDS = errors.New("api: no delivery service to issue the membership change to")

// RemoveUserFromChannelGroups issues one delivery-service Remove per live leaf
// of userID in the open text and call groups bound to channelID. The Remove
// freezes its group until a member commits it (protocol/02 invariant 5).
//
// Each Remove carries its own action_id. The delivery service's re-issue path
// (ds.ReissueFor, invariant 6) retries "the" proposal of one action in a group,
// so two leaves of one user sharing an action_id would leave the second leaf's
// Remove un-retried after an epoch change.
//
// Every group is attempted: one refused Remove (a leaf a concurrent commit
// already removed, which invariant 6 drops) must not keep the user in the
// channel's other groups. The refusals are returned together.
func RemoveUserFromChannelGroups(ctx context.Context, repo store.Repository, dsvc DS, channelID, userID id.ID) error {
	if dsvc == nil {
		return errNoDS
	}
	var errs []error
	for _, kind := range []uint8{groupText, groupCall} {
		groups, err := repo.GroupsForTarget(ctx, channelID, kind)
		if err != nil {
			return err
		}
		for _, g := range groups {
			// The user may hold an outstanding Add here rather than (or as well as) a leaf.
			if err := dsvc.VoidIneligibleAdds(ctx, g.GroupID); err != nil {
				errs = append(errs, err)
			}
			members, err := repo.ListMembers(ctx, g.GroupID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, m := range members {
				if m.UserID != userID || m.RemovedEpoch != nil {
					continue
				}
				var err error
				if kind == groupCall {
					// DEV-45: by device, under the group lock, never stacked on a standing instance Remove.
					err = dsvc.ProposeRemoveDevice(ctx, g.GroupID, m.DeviceID, id.New())
				} else {
					// The leaf was read above, outside the group lock: the Remove names the device
					// it is for, so a leaf reused since then is refused rather than redirected.
					err = dsvc.ProposeRemoveOf(ctx, g.GroupID, m.LeafIndex, m.DeviceID, id.New())
				}
				if err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// RemoveUserFromCommunityGroups does the same across every live channel of a
// community. It is the kick, ban and leave path.
//
// It MUST NOT be called from inside a store.Repository.Tx. internal/ds writes
// through its own store.Repository, which is the same *sql.DB write pool, and
// the SQLite write pool is SetMaxOpenConns(1): the enclosing transaction holds
// the single write connection, so the delivery service's first write blocks
// forever. busy_timeout cannot break it, because the contention is in Go's
// sql.DB pool and not in SQLite's locking. Every caller commits its rows first
// and calls this afterwards.
//
// channel_members is not touched here: for a community channel it is derived
// state, and task 7's eligibility materialiser owns it.
func RemoveUserFromCommunityGroups(ctx context.Context, repo store.Repository, dsvc DS, communityID, userID id.ID) error {
	if dsvc == nil {
		return errNoDS
	}
	channels, err := repo.ListChannels(ctx, communityID)
	if err != nil {
		return err
	}
	var errs []error
	for _, ch := range channels {
		if err := RemoveUserFromChannelGroups(ctx, repo, dsvc, ch.ID, userID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// openChannelGroups collects the open text and call groups bound to each of
// channels, for a caller that is about to tombstone those channels. It reads
// through repo, which may be a transaction; the delivery-service Close calls
// happen after that transaction commits (closeGroups), for
// RemoveUserFromCommunityGroups' reason.
func openChannelGroups(ctx context.Context, repo store.Repository, channels []id.ID) ([]id.ID, error) {
	var out []id.ID
	for _, ch := range channels {
		for _, kind := range []uint8{groupText, groupCall} {
			groups, err := repo.GroupsForTarget(ctx, ch, kind)
			if err != nil {
				return nil, err
			}
			for _, g := range groups {
				out = append(out, g.GroupID)
			}
		}
	}
	return out, nil
}

// closeGroups asks the delivery service to close each group, and returns every
// refusal together. It MUST NOT run inside a store.Repository.Tx.
func closeGroups(ctx context.Context, dsvc DS, groups []id.ID) error {
	if len(groups) == 0 {
		return nil
	}
	if dsvc == nil {
		return errNoDS
	}
	var errs []error
	for _, g := range groups {
		if err := dsvc.Close(ctx, g); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// BanStands reports whether a ban row still gates: a row with no expiry always
// does, a row with an expiry only until that second.
func BanStands(b store.BanRow, now int64) bool {
	return b.Expires == nil || *b.Expires > now
}

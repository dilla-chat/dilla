package ds

import (
	"context"
	"time"

	"github.com/jonasthim/dilla/internal/id"
)

// watchdogInterval is the tick, and never a non-positive duration: time.NewTicker panics on one,
// inside a goroutine Start cannot recover from. New already fills every unset Policy field from
// DefaultPolicy (normalisePolicy), so this is the second belt rather than the first.
func (d *DS) watchdogInterval() time.Duration {
	if d.opts.Policy.WatchdogInterval <= 0 {
		return DefaultPolicy().WatchdogInterval
	}
	return d.opts.Policy.WatchdogInterval
}

// runWatchdog ticks every WatchdogInterval (2 s) and advances every overdue election.
func (d *DS) runWatchdog(ctx context.Context) {
	ticker := time.NewTicker(d.watchdogInterval())
	defer ticker.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.RunWatchdogOnce(ctx)
		}
	}
}

// RunWatchdogOnce advances every election whose deadline has passed. It is exported so a test
// driving a clock.Fake can run exactly one tick rather than waiting on wall time.
func (d *DS) RunWatchdogOnce(ctx context.Context) {
	now := d.opts.Clock.Now()
	type overdue struct {
		groupID   id.ID
		candidate id.ID
		acked     bool
		round     uint64
		epoch     uint64
		retryAt   time.Time
	}
	var due []overdue

	d.elections.mu.Lock()
	for groupID, e := range d.elections.m {
		if e.candidate == (id.ID{}) || (e.retryAt.IsZero() && now.Sub(e.sentAt) < d.watchdogInterval()) {
			continue
		}
		due = append(due, overdue{
			groupID: groupID, candidate: e.candidate, acked: e.acked,
			round: e.round, epoch: e.epoch, retryAt: e.retryAt,
		})
	}
	d.elections.mu.Unlock()

	for _, o := range due {
		// A ROUND THAT WAS WON is not a round that was lost. The election is charged nowhere else,
		// so this is the one place that distinction is made: an accepted Commit clears the group's
		// election (commit step (8b)), and where the watchdog's tick beats that clear — the window
		// between the next proposal's row and its own RequestCommit, which spans DeliverGroup's
		// fan-out to every member — the group's epoch has already moved past the one the round was
		// elected for. Either way the candidate committed exactly what it was told to commit, and
		// three such charges would remove it from the group by an instance Remove.
		row, err := d.opts.Store.GetGroup(ctx, o.groupID)
		if err != nil {
			d.log().Error("reading the group of an overdue election failed",
				"group", o.groupID.String()[:8], "err", err)
			continue
		}
		if row.Epoch != o.epoch {
			d.clearElection(o.groupID)
			if d.opts.Metrics != nil {
				d.opts.Metrics.ElectionRounds.WithLabelValues("won").Inc()
			}
			// Whatever is outstanding at the NEW epoch gets a fresh election, from round one of a
			// fresh rotation.
			if err := d.RequestCommit(ctx, o.groupID); err != nil {
				d.log().Error("re-electing after a won round failed",
					"group", o.groupID.String()[:8], "err", err)
			}
			continue
		}
		if !o.retryAt.IsZero() {
			if !now.Before(o.retryAt) {
				if err := d.RequestCommit(ctx, o.groupID); err != nil {
					d.log().Error("watchdog re-election failed",
						"group", o.groupID.String()[:8], "err", err)
				}
			}
			continue
		}
		lost := 0
		if o.acked {
			lost = d.chargeLostRound(o.groupID, o.candidate, o.round)
		}
		if o.acked && lost >= d.opts.Policy.MaxLostRounds {
			// The Remove exists so that ANOTHER member commits what this device did not. When no
			// other member could — the device is the group's last unbarred member that no instance
			// Remove is already taking out, like the creator of a channel group whose first Adds are
			// the work outstanding — nobody could ever commit the Remove (a device never commits its own
			// removal, and the election never asks it to), and the group would stay frozen with
			// every joiner refused 425 E_COMMIT_REQUIRED. The device is kept and asked again.
			last, err := d.isLastCommitter(ctx, o.groupID, o.candidate, row.Epoch)
			if err != nil {
				d.log().Error("reading the members of an overdue election failed",
					"group", o.groupID.String()[:8], "err", err)
				continue
			}
			if last {
				if d.keepCommitter(o.groupID, o.candidate, o.round, now) && d.opts.Metrics != nil {
					d.opts.Metrics.ElectionRounds.WithLabelValues("kept").Inc()
				}
				continue
			}
			// Three acknowledged-and-lost rounds: the device is removed by an instance Remove.
			// By DEVICE, through the locking form (the watchdog runs on its own goroutine and holds
			// no group lock): the leaf is resolved under the lock and the Remove is built only for
			// the candidate, so a leaf reused since the election cannot redirect it onto another
			// device. A candidate that holds no leaf any more is dropped.
			if err := d.ProposeRemoveDevice(ctx, o.groupID, o.candidate, id.New()); err != nil {
				d.log().Error("watchdog remove failed",
					"group", o.groupID.String()[:8], "err", err)
			}
			d.resetLost(o.groupID, o.candidate)
			if d.opts.Metrics != nil {
				d.opts.Metrics.ElectionRounds.WithLabelValues("removed").Inc()
			}
			continue
		}
		if d.opts.Metrics != nil {
			label := "unacked"
			if o.acked {
				label = "lost"
			}
			d.opts.Metrics.ElectionRounds.WithLabelValues(label).Inc()
		}
		if err := d.RequestCommit(ctx, o.groupID); err != nil {
			d.log().Error("watchdog re-election failed",
				"group", o.groupID.String()[:8], "err", err)
		}
	}
}

// chargeLostRound charges one ACKNOWLEDGED-and-lost round and returns the running count. The
// round is named, so a round that has already been superseded between the two passes of
// RunWatchdogOnce — by a proposal's own RequestCommit, say — is not charged twice and is not
// charged to a candidate that is no longer the one that acknowledged.
func (d *DS) chargeLostRound(groupID, deviceID id.ID, round uint64) int {
	d.elections.mu.Lock()
	defer d.elections.mu.Unlock()
	e, ok := d.elections.m[groupID]
	if !ok || e.round != round || e.candidate != deviceID || !e.acked {
		return 0
	}
	e.lost[deviceID]++
	return e.lost[deviceID]
}

// isLastCommitter reports whether deviceID is the only current member that could commit:
// every other member is barred or targeted by an outstanding non-void instance Remove.
// Online state is irrelevant: an offline member can commit when it comes back.
func (d *DS) isLastCommitter(ctx context.Context, groupID, deviceID id.ID, epoch uint64) (bool, error) {
	members, err := d.opts.Store.ListMembers(ctx, groupID)
	if err != nil {
		return false, err
	}
	rows, err := d.opts.Store.ListProposals(ctx, groupID, epoch, false)
	if err != nil {
		return false, err
	}
	barred, err := d.opts.Store.ListBarredMembers(ctx, groupID)
	if err != nil {
		return false, err
	}
	leavingDevice, leavingLeaf := leaving(rows)
	for _, m := range barred {
		leavingDevice[m.DeviceID] = true
	}
	for _, m := range members {
		if m.RemovedEpoch != nil || m.DeviceID == deviceID {
			continue
		}
		if !leavingDevice[m.DeviceID] && !leavingLeaf[m.LeafIndex] {
			return false, nil
		}
	}
	return true, nil
}

// keepCommitter delays the next election instead of asking the last committer immediately again.
func (d *DS) keepCommitter(groupID, deviceID id.ID, round uint64, now time.Time) bool {
	d.elections.mu.Lock()
	defer d.elections.mu.Unlock()
	e, ok := d.elections.m[groupID]
	if !ok || e.round != round || e.candidate != deviceID || !e.retryAt.IsZero() {
		return false
	}
	delay := e.keptBackoff[deviceID]
	if delay == 0 {
		delay = d.watchdogInterval()
	} else {
		delay *= 2
	}
	delay = min(delay, time.Minute)
	e.keptBackoff[deviceID] = delay
	e.retryAt = now.Add(delay)
	e.acked = false
	delete(e.lost, deviceID)
	return true
}

func (d *DS) lostRounds(groupID, deviceID id.ID) int {
	d.elections.mu.Lock()
	defer d.elections.mu.Unlock()
	if e, ok := d.elections.m[groupID]; ok {
		return e.lost[deviceID]
	}
	return 0
}

func (d *DS) resetLost(groupID, deviceID id.ID) {
	d.elections.mu.Lock()
	defer d.elections.mu.Unlock()
	if e, ok := d.elections.m[groupID]; ok {
		delete(e.lost, deviceID)
	}
}

// runSweeper voids expired proposals and prunes retention on one ticker: both are lazy evaluations
// at decision points, so one minute of granularity is enough and a test can call the sweep
// directly.
//
// Task 26 replaced the bare `sweepProposals` call with `Sweep`, which runs invariant 6's TTL sweep
// and then both halves of invariant 10's retention on the same tick. A test drives it by calling
// `Sweep` against a clock.Fake rather than by waiting on this wall-clock ticker.
func (d *DS) runSweeper(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := d.Sweep(ctx); err != nil {
				d.log().Error("sweep failed", "err", err)
			}
		}
	}
}

// runCallSweeper runs sweepCallProposals every Policy.ProposalSweepInterval. It reads the DS's
// clock, which is the wall clock in production, so a test drives it with clock.Fake.Advance.
func (d *DS) runCallSweeper(ctx context.Context) {
	for {
		t := d.opts.Clock.NewTimer(d.opts.Policy.ProposalSweepInterval)
		select {
		case <-d.stop:
			t.Stop()
			return
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
			if _, err := d.sweepCallProposals(ctx); err != nil {
				d.log().Error("call proposal sweep failed", "err", err)
			}
		}
	}
}

package ds

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/jonasthim/dilla/internal/gateway"
	"github.com/jonasthim/dilla/internal/id"
	"github.com/jonasthim/dilla/internal/store"
)

// election is one group's in-flight committer round. It lives in memory: an election that was
// decided while everybody was away is stale by definition, and the instance re-elects.
type election struct {
	round     uint64
	candidate id.ID
	sentAt    time.Time
	acked     bool
	// epoch is the group epoch the round was elected FOR: the epoch whose refs the
	// mls.commit_needed frame named. It is what tells a round that was WON from one that was
	// lost. A commit that lands moves the group to epoch+1 and deletes the refs it applied, so a
	// round whose epoch the group has already left asked for work that is done, and charging its
	// candidate a lost round would punish the device that did exactly what it was told
	// (RunWatchdogOnce).
	epoch uint64
	// lost counts ACKNOWLEDGED rounds this device failed to commit. An unacknowledged round only
	// advances the election — the frame reaching the connection's writer is not an
	// acknowledgement (protocol/02 invariant 7 as amended).
	lost  map[id.ID]int
	tried map[id.ID]struct{}
	// keptBackoff survives lost-count resets until the epoch advances or a commit clears the election.
	keptBackoff map[id.ID]time.Duration
	retryAt     time.Time
}

type elections struct {
	mu sync.Mutex
	m  map[id.ID]*election
	// suppressed counts the open election windows of a group. While one is open,
	// storeInstanceProposal holds NO election: an operation that issues MANY instance proposals
	// — ProposeAddBatch's 256 Adds, drainPendingJoins' next slice of a join storm — elects ONCE,
	// at the end, for the whole batch. It is a counter rather than a flag so that a window nested
	// inside another (a batch whose body ever grows a nested issuing path) closes in the right
	// order.
	suppressed map[id.ID]int
}

// RequestCommit elects a committer and sends mls.commit_needed. It is called whenever an instance
// proposal is issued and whenever the watchdog advances a round.
func (d *DS) RequestCommit(ctx context.Context, groupID id.ID) error {
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if err != nil {
		return err
	}
	rows, err := d.opts.Store.ListProposals(ctx, groupID, row.Epoch, false)
	if err != nil {
		return err
	}
	refs := make([][]byte, 0, len(rows))
	for _, r := range rows {
		refs = append(refs, r.Ref)
	}
	leavingDevice, leavingLeaf := leaving(rows)
	if len(refs) == 0 {
		d.clearElection(groupID)
		return nil
	}

	// A DS built without a gateway (the seam tests) can elect nobody: there is no candidate list
	// and no way to address the frame.
	if d.opts.Gateway == nil {
		d.clearElection(groupID)
		return nil
	}
	// OnlineIn is already bots-first, then by leaf index, and a fresh slice of the caller's own.
	candidates := slices.DeleteFunc(d.opts.Gateway.OnlineIn(groupID), func(c gateway.OnlineDevice) bool {
		return leavingDevice[c.DeviceID] || leavingLeaf[c.LeafIndex]
	})
	if len(candidates) == 0 {
		// No candidate: arm nothing. The next ready re-elects, which is what the online predicate
		// is for. The same when the only devices online are the ones being removed.
		d.clearElection(groupID)
		return nil
	}

	// The whole mutate-and-choose block runs under d.elections.mu, inside beginRound. Writing
	// e.tried, e.candidate, e.acked and e.sentAt out here with no lock held would race
	// RunWatchdogOnce, which reads exactly those fields under the mutex from its own goroutine —
	// a map write against a map read, which `go test -race ./internal/ds/` reports.
	chosen, round := d.beginRound(groupID, candidates, d.opts.Clock.Now(), row.Epoch)
	if chosen == (id.ID{}) {
		return nil
	}

	payload, err := gateway.CommitNeededPayload(
		row.Epoch, refs, uint64(d.opts.Policy.CommitDeadline/time.Millisecond), round) //nolint:gosec // G115: a non-negative duration in milliseconds
	if err != nil {
		return err
	}
	// n = 0: the frame is never replayed. deadline_ms is re-based by the writer (R31, D11).
	d.opts.Gateway.DeliverDevice(chosen, gateway.Frame{
		Op: gateway.OpMLSCommitNeeded, GroupID: &groupID, Payload: payload,
	})
	if d.opts.Metrics != nil {
		d.opts.Metrics.ElectionRounds.WithLabelValues("sent").Inc()
	}
	return nil
}

// leaving is the devices (and, for a Remove that recorded none, the leaves) an outstanding
// instance Remove among rows is taking out. Such a device can never commit its own removal —
// OpenMLS refuses a commit that removes the committer — so electing it only wastes a round, and
// charges it a lost one if it acks (DS-5 of the server-half review); and it is no member the
// watchdog can hand the group to (isLastCommitter).
func leaving(rows []store.ProposalRow) (devices map[id.ID]bool, leaves map[uint32]bool) {
	devices, leaves = map[id.ID]bool{}, map[uint32]bool{}
	for _, r := range rows {
		if r.TargetLeaf == nil || !isInstanceRemoveOf(r, *r.TargetLeaf) {
			continue
		}
		if r.TargetDevice != nil {
			devices[*r.TargetDevice] = true
		} else {
			leaves[*r.TargetLeaf] = true
		}
	}
	return devices, leaves
}

// CurrentRound reports the live round of a group's election, or 0 when none is armed. The
// acknowledgement carries a round, and a caller that acks a number it guessed acks nothing:
// AckCommitNeeded returns early on a round mismatch, so `e.acked` would never be set and the
// watchdog would never count a lost round.
func (d *DS) CurrentRound(groupID id.ID) uint64 {
	d.elections.mu.Lock()
	defer d.elections.mu.Unlock()
	if e, ok := d.elections.m[groupID]; ok {
		return e.round
	}
	return 0
}

// AckCommitNeeded records that a device accepted the task. Only an acknowledged round can be lost.
// It is what `gateway.Options.CommitAck` is wired to in the composition root: opcode 12 carries
// [round] and the connection's own group and device.
func (d *DS) AckCommitNeeded(_ context.Context, groupID, deviceID id.ID, round uint64) error {
	d.elections.mu.Lock()
	defer d.elections.mu.Unlock()
	e, ok := d.elections.m[groupID]
	if !ok || e.round != round || e.candidate != deviceID || !e.retryAt.IsZero() {
		return nil // a stale ack is ignored, never an error
	}
	e.acked = true
	return nil
}

// beginRound advances the round and picks the next untried candidate, entirely under the mutex,
// and returns copies. Nothing outside this function touches an *election's fields.
func (d *DS) beginRound(groupID id.ID, candidates []gateway.OnlineDevice, now time.Time, epoch uint64) (chosen id.ID, round uint64) {
	d.elections.mu.Lock()
	defer d.elections.mu.Unlock()
	e, ok := d.elections.m[groupID]
	if !ok || e.epoch != epoch {
		e = &election{lost: map[id.ID]int{}, tried: map[id.ID]struct{}{}, keptBackoff: map[id.ID]time.Duration{}}
		d.elections.m[groupID] = e
	}
	if now.Before(e.retryAt) {
		return id.ID{}, e.round
	}
	e.retryAt = time.Time{}
	e.round++
	for _, c := range candidates {
		if _, tried := e.tried[c.DeviceID]; tried {
			continue
		}
		chosen = c.DeviceID
		break
	}
	if chosen == (id.ID{}) {
		// Every candidate has had this round; start the rotation again.
		e.tried = map[id.ID]struct{}{}
		chosen = candidates[0].DeviceID
	}
	e.tried[chosen] = struct{}{}
	e.candidate = chosen
	e.acked = false
	e.sentAt = now
	e.epoch = epoch
	return chosen, e.round
}

// suppressElections opens a window in which storeInstanceProposal holds no election, and returns
// the function that closes it. Invariant 7 elects ONE device per round; a caller that issues many
// proposals in one operation therefore elects once, when they are all durable, and not once per
// proposal.
//
// Without the window, ProposeAddBatch's 256 Adds held 256 elections: beginRound marks each chosen
// candidate `tried` and restarts the rotation when everybody has been tried, so a batch told
// several devices at once that they were the committer — the conflict storm invariant 7 exists to
// prevent — and left the last frame wherever the rotation happened to land rather than at the
// lowest-index online device.
//
// RequestCommit itself is NOT suppressed: it is the explicit call, and the batch makes it once
// after its loop.
func (d *DS) suppressElections(groupID id.ID) (release func()) {
	d.elections.mu.Lock()
	if d.elections.suppressed == nil {
		d.elections.suppressed = map[id.ID]int{}
	}
	d.elections.suppressed[groupID]++
	d.elections.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			d.elections.mu.Lock()
			if n := d.elections.suppressed[groupID] - 1; n > 0 {
				d.elections.suppressed[groupID] = n
			} else {
				delete(d.elections.suppressed, groupID)
			}
			d.elections.mu.Unlock()
		})
	}
}

// electionsSuppressed reports whether a batch window is open for this group.
func (d *DS) electionsSuppressed(groupID id.ID) bool {
	d.elections.mu.Lock()
	defer d.elections.mu.Unlock()
	return d.elections.suppressed[groupID] > 0
}

func (d *DS) clearElection(groupID id.ID) {
	d.elections.mu.Lock()
	delete(d.elections.m, groupID)
	d.elections.mu.Unlock()
}

// armedElections is how many groups hold a live election. A group with no candidate arms nothing,
// which is the one thing "the election is in memory" has to be observable for.
func (d *DS) armedElections() int {
	d.elections.mu.Lock()
	defer d.elections.mu.Unlock()
	return len(d.elections.m)
}

package ds

import (
	"context"

	"github.com/dilla-chat/dilla/internal/id"
)

// DebugState is the counter bundle the test control listener reports (task 29's GET
// /debug/state): what a scenario asserts on without reaching into the database.
//
// It is declared HERE rather than in internal/dillad/dilladtest, which is where plan task 29 puts
// it (deviation B35): dilladtest imports internal/dillad, so a dillad accessor returning a
// dilladtest type is an import cycle, and it would also link the test-only package into the
// release binary, which TestTheReleaseBinaryDoesNotLinkTheTestHelpers forbids. dilladtest aliases
// this type instead. The JSON names are the ones task 29 writes; NowUnix is the field task 29's own
// clock test reads and its struct omits.
type DebugState struct {
	Generation    uint64 `json:"generation"`
	Groups        int    `json:"groups"`
	Commits       int    `json:"commits"`
	OpenElections int    `json:"open_elections"`
	FrozenGroups  int    `json:"frozen_groups"`
	NowUnix       int64  `json:"now_unix"`
}

// AcceptedCommits is how many commits this delivery service has accepted since it started: the
// same event `dilla_ds_commits_total{result="accepted"}` counts, kept on the DS itself so it can
// be read without a metrics registry. A scenario asserting "at most four commits for 1,000
// devices" needs the instance's count, not the client's.
func (d *DS) AcceptedCommits() int { return int(d.accepted.Load()) }

// OpenElections is the number of groups with an in-flight committer round.
func (d *DS) OpenElections() int {
	d.elections.mu.Lock()
	defer d.elections.mu.Unlock()
	return len(d.elections.m)
}

// DebugState reads the counters. Groups and FrozenGroups walk every open group, which is fine for
// a test instance and is why this is a debug read and not a metric.
func (d *DS) DebugState(ctx context.Context) (DebugState, error) {
	instance, err := d.opts.Store.GetInstance(ctx)
	if err != nil {
		return DebugState{}, err
	}
	out := DebugState{
		Generation:    instance.Generation,
		Commits:       d.AcceptedCommits(),
		OpenElections: d.OpenElections(),
		NowUnix:       d.now(),
	}
	const page = 256
	var after id.ID
	for {
		rows, err := d.opts.Store.ListOpenGroups(ctx, after, page)
		if err != nil {
			return DebugState{}, err
		}
		for _, row := range rows {
			out.Groups++
			frozen, err := d.Frozen(ctx, row.GroupID)
			if err != nil {
				return DebugState{}, err
			}
			if frozen {
				out.FrozenGroups++
			}
		}
		if len(rows) < page {
			return out, nil
		}
		after = rows[len(rows)-1].GroupID
	}
}

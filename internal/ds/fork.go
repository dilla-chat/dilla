package ds

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
)

// forkQuorum is how many DISTINCT reporter devices quarantine a committer. The distinctness is the
// whole point: one device reporting three times is one opinion, and the primary key
// (group_id, seq, reporter_device) is what makes a repeat report idempotent rather than a vote.
const forkQuorum = 3

// maxForkReason bounds the free-text reason a client may attach, IN BYTES. It is truncated rather
// than refused: a fork report is a bug report, and losing the tail of a stack trace is better than
// losing the report.
const maxForkReason = 256

// boundReason truncates a reason to maxForkReason bytes ON A RUNE BOUNDARY.
//
// The boundary is the whole point. `internal/cborx/decode.go:149` guarantees the incoming text is
// valid UTF-8, and a plain `reason[:maxForkReason]` throws that guarantee away whenever byte 256
// falls inside a multi-byte rune: Postgres' `reason TEXT NOT NULL` then refuses the INSERT with
// `invalid byte sequence for encoding "UTF8"` while SQLite's STRICT TEXT stores the broken bytes
// without a word, so the two engines disagree and only Postgres answers 500 — on a path no local
// run reaches, because the Postgres tests skip without a server. `internal/auth/handle.go` bounds
// its free text by runes for the same reason.
func boundReason(reason string) string {
	if len(reason) <= maxForkReason {
		return reason
	}
	cut := maxForkReason
	// At most utf8.UTFMax-1 steps: a rune's continuation bytes are never rune starts.
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}

// ForkReport records that a member could not process an accepted commit. Three distinct reports
// against one commit quarantine its committer: an instance Remove of its device's current leaf and
// a flag on the device (invariant 9).
func (d *DS) ForkReport(ctx context.Context, s Session, groupID id.ID, epoch, seq uint64, reason string) error {
	// Member-only. protocol/02's row for `POST /v1/groups/{id}/fork-report` names E_NOT_FOUND as
	// its one refusal, and `requireMember` is what answers it: a device that is not in the group
	// learns nothing about it, and cannot spend the instance's fork_reports rows on a group it
	// was never in.
	if err := d.requireMemberOnly(ctx, groupID, s); err != nil {
		return err
	}
	// `seq` is bounded by the group's own log BEFORE anything is written. A client cannot have
	// observed a handshake this instance never appended, and without the bound any member may
	// spend one `fork_reports` row per arbitrary uint64 — the primary key
	// (group_id, seq, reporter_device) makes every distinct seq a new row, each carrying up to
	// maxForkReason bytes of reason, and nothing in the tree prunes that table. That is unbounded
	// member-driven storage growth on a write endpoint.
	//
	// `row.Seq` is the high-water mark, so `seq <= row.Seq` is "the log reached here": a seq that
	// is inside the log but no longer IN it (pruned) still lands, and `quarantineCommitterOf`
	// answers it by quarantining nobody. Seq 0 is not a handshake either — the first one a group
	// ever appends is 1 — and both are E_NOT_FOUND, the one refusal
	// protocol/02-delivery-service.md:81 names for this endpoint.
	row, err := d.opts.Store.GetGroup(ctx, groupID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound("group")
	}
	if err != nil {
		return err
	}
	if seq == 0 || seq > row.Seq {
		return errNotFound("handshake")
	}
	if err := d.requireGroupCapability(ctx, s, row); err != nil {
		return err
	}
	if err := d.opts.Store.PutForkReport(ctx, store.ForkReportRow{
		GroupID:        groupID,
		Seq:            seq,
		ReporterDevice: s.DeviceID,
		Epoch:          epoch,
		Reason:         boundReason(reason),
		Created:        d.now(),
	}); err != nil {
		return err
	}
	n, err := d.opts.Store.CountForkReporters(ctx, groupID, seq)
	if err != nil {
		return err
	}
	if n < forkQuorum {
		return nil
	}
	return d.quarantineCommitterOf(ctx, groupID, seq)
}

// quarantineCommitterOf is invariant 9's second half: the device that committed handshake `seq` is
// flagged and its current leaf is Removed by an instance proposal.
//
// `seq` was bounded by the group's high-water mark in `ForkReport` before the first row was
// written, so it is 1 or above and no higher than the log ever reached; everything below is about
// what the log still HOLDS, which is a different question.
func (d *DS) quarantineCommitterOf(ctx context.Context, groupID id.ID, seq uint64) error {
	// `ListHandshakes`' `fromSeq` is INCLUSIVE — `seq >= ?` in
	// internal/store/sqlite/queries/mls.sql:34, and `Handshakes` documents `from` as "the first
	// seq the caller still wants" — so the row for `seq` is the first of a one-row page from
	// `seq`, not from `seq-1`. The identity is checked rather than assumed: a pruned `seq` makes
	// the page start at the next surviving handshake, and quarantining whoever committed THAT
	// would punish a device nobody reported.
	rows, err := d.opts.Store.ListHandshakes(ctx, groupID, seq, 1)
	if err != nil {
		return err
	}
	if len(rows) == 0 || rows[0].Seq != seq {
		// The reported handshake is not in the log — pruned, or a seq this instance never
		// appended. The reports stand; there is nobody to quarantine.
		return nil
	}
	committer := rows[0].SenderDevice
	if committer == nil {
		// The instance's own external sender cannot be quarantined; a quorum against an instance
		// handshake is a bug report, not a moderation event.
		d.log().Error("fork quorum against an instance handshake",
			"group", groupID.String()[:8], "seq", seq)
		return nil
	}
	if err := d.opts.Store.QuarantineDevice(ctx, *committer, d.now(), "fork quorum"); err != nil {
		return err
	}
	// The quarantined device leaves every live call now: its call-group leaves stay until members
	// commit their Removes, and the call routes refuse it from here on.
	if d.opts.OnQuarantine != nil {
		d.opts.OnQuarantine(context.WithoutCancel(ctx), *committer)
	}
	// The Remove is of the committer's device where it is NOW, resolved under the group lock
	// (DS-2 of the server-half review). The handshake's sender leaf is where it sat when the
	// reported commit was appended, which can be any seq still in the 30-day log: it may have
	// resynced or left since, and MLS fills the leftmost blank leaf, so by now that index can hold
	// a newcomer, whom a Remove by index would freeze out. A committer that holds no leaf any more
	// is flagged and has nothing to remove.
	return d.ProposeRemoveDevice(ctx, groupID, *committer, id.New())
}

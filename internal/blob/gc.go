package blob

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/obs"
	"github.com/dilla-chat/dilla/internal/store"
)

// sweepBatch bounds one pass so the sweeper never holds the single SQLite write
// connection for an unbounded delete. It bounds each phase of a pass: at most
// sweepBatch references of deleted channels, sweepBatch references past a
// retention policy, and sweepBatch collected blobs.
const sweepBatch = 256

// LastBackupStartedKey is the instance_settings key `dillad backup` writes
// (decimal unix seconds) before it takes its snapshot and empties when it is
// done. While it is set, the sweeper collects nothing unreferenced after it.
const LastBackupStartedKey = "last_backup_started"

// maxRetentionDays mirrors the policy validator's bound (api.ParseCommunityPolicy):
// a century, so days * 86400 stays far inside an int64 even for a stored
// document written before the bound existed.
const maxRetentionDays = 36500

// Sweeper is the blob garbage collector of gap-47 §8. A channel never deletes a
// blob, only its reference; the sweeper unlinks a file only when no reference
// remains anywhere and unref_since is older than the grace window. The window is
// what makes the check-then-unlink race harmless: a forward that creates a
// reference between the listing and the unlink would otherwise lose its bytes,
// and with a 24 h window that race needs a 24 h stall inside one pass.
//
// Each pass first drops the references that have expired — those of a deleted
// channel, those their uploader never confirmed within blobs.pending_ttl, and those older than
// their community's archival retention (R28, the policy's retention_days; absent or 0 keeps them
// indefinitely) — which
// marks their blobs unreferenced, and then collects the blobs whose grace
// window has passed. A reference therefore expires into the same grace window a
// deletion does.
type Sweeper struct {
	repo       store.Repository
	store      *Store
	clk        clock.Clock
	grace      time.Duration
	pendingTTL time.Duration
	interval   time.Duration
	log        *slog.Logger
	metrics    *obs.Metrics
}

// NewSweeper builds a sweeper over one repository and blob store. A negative
// grace is treated as zero, and an interval that is not positive as one hour,
// so a misconfigured duration can neither collect a blob before it is marked
// nor spin the loop. pendingTTL is blobs.pending_ttl: an upload whose reference its uploader
// never confirmed is dropped once it is older; 0 or less sweeps none.
func NewSweeper(repo store.Repository, s *Store, clk clock.Clock, grace, pendingTTL, interval time.Duration, log *slog.Logger) *Sweeper {
	if grace < 0 {
		grace = 0
	}
	if interval <= 0 {
		interval = time.Hour
	}
	if pendingTTL < 0 {
		pendingTTL = 0
	}
	return &Sweeper{repo: repo, store: s, clk: clk, grace: grace, pendingTTL: pendingTTL, interval: interval, log: log}
}

// WithMetrics records every pass in m (dilla_blob_gc_*,
// dilla_blob_refs_expired_total) and returns the sweeper.
func (s *Sweeper) WithMetrics(m *obs.Metrics) *Sweeper {
	s.metrics = m
	return s
}

// Run sweeps every interval until ctx is done. dillad serve starts it once.
func (s *Sweeper) Run(ctx context.Context) {
	t := s.clk.NewTimer(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			if n, err := s.SweepOnce(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				s.log.Error("blob sweep", "err", err)
			} else if n > 0 {
				s.log.Info("blob sweep", "deleted", n)
			}
			t.Reset(s.interval)
		}
	}
}

// SweepOnce runs one pass and returns how many blobs it unlinked. Blobs are
// collected at most sweepBatch at a time, only when they have no reference and
// unref_since is older than the grace window. The row is deleted first, inside a
// transaction that re-checks for references; the file is unlinked after the
// commit, so a crash between the two leaves an orphan file (harmless, and
// dillad doctor reports it) rather than a row pointing at nothing.
func (s *Sweeper) SweepOnce(ctx context.Context) (int, error) {
	n, err := s.sweep(ctx)
	s.metrics.BlobSweep(err)
	return n, err
}

func (s *Sweeper) sweep(ctx context.Context) (int, error) {
	now := s.clk.Now()
	if err := s.expireReferences(ctx, now.Unix()); err != nil {
		return 0, err
	}
	cutoff := now.Add(-s.grace).Unix()
	// A backup in flight pins the cutoff to its start (gap-47 §8.4; Plan 2 task
	// 12 writes the setting before its snapshot and clears it when it is done):
	// a blob that lost its last reference after the snapshot was taken is still
	// referenced BY the snapshot, and collecting it would put a gap in an
	// archive that claims to be complete. An empty value is "no backup running".
	if raw, err := s.repo.GetSetting(ctx, LastBackupStartedKey); err == nil {
		if started, perr := strconv.ParseInt(string(raw), 10, 64); perr == nil && started < cutoff {
			cutoff = started
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	rows, err := s.repo.ListCollectableBlobs(ctx, cutoff, sweepBatch)
	if err != nil {
		return 0, err
	}
	var deleted int
	for _, row := range rows {
		var collect bool
		if err := s.repo.Tx(ctx, func(tx store.Repository) error {
			n, err := tx.CountBlobRefs(ctx, row.BlobID)
			if err != nil {
				return err
			}
			if n > 0 {
				// A reference appeared between the listing and now: clear the
				// mark and leave the bytes alone. This is the forward case.
				return tx.ClearBlobUnreferenced(ctx, row.BlobID)
			}
			collect = true
			return tx.DeleteBlob(ctx, row.BlobID)
		}); err != nil {
			return deleted, err
		}
		if !collect {
			continue
		}
		if err := s.store.Delete(row.BlobID); err != nil {
			return deleted, err
		}
		s.metrics.BlobCollected(row.Size)
		deleted++
	}
	return deleted, nil
}

// expireReferences drops the references of deleted channels and the references
// past their community's retention policy, each in its own transaction that
// also marks the blob unreferenced when it was the last reference.
func (s *Sweeper) expireReferences(ctx context.Context, now int64) error {
	gone, err := s.repo.ListBlobRefsOfDeletedChannels(ctx, sweepBatch)
	if err != nil {
		return err
	}
	for _, ref := range gone {
		if err := s.dropReference(ctx, ref, now, "channel_deleted"); err != nil {
			return err
		}
	}

	policies, err := s.repo.ListBlobRetentionPolicies(ctx)
	if err != nil {
		return err
	}
	budget := sweepBatch
	var expired int
	for _, p := range policies {
		days := retentionDays(p.PolicyJSON)
		if days == 0 {
			continue
		}
		// days is at most maxRetentionDays, so the product stays far inside an int64.
		before := now - int64(days)*24*60*60 //nolint:gosec // G115: bounded by maxRetentionDays
		refs, err := s.repo.ListExpiredBlobRefs(ctx, p.CommunityID, before, int32(budget))
		if err != nil {
			return err
		}
		for _, ref := range refs {
			if err := s.dropReference(ctx, ref, now, "retention"); err != nil {
				return err
			}
		}
		expired += len(refs)
		if budget -= len(refs); budget <= 0 {
			break
		}
	}
	var pending int
	if s.pendingTTL > 0 {
		refs, err := s.repo.ListPendingBlobRefs(ctx, now-int64(s.pendingTTL/time.Second), sweepBatch)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			dropped, err := s.dropPendingReference(ctx, ref, now)
			if err != nil {
				return err
			}
			if dropped {
				pending++
			}
		}
	}
	if n := len(gone) + expired + pending; n > 0 {
		s.log.Info("blob references expired", "channel_deleted", len(gone), "retention", expired, "pending", pending)
	}
	return nil
}

// dropPendingReference re-reads under the blob lock to keep a confirm that landed after listing:
// the confirm route takes the same lock (L-HTTP-82).
func (s *Sweeper) dropPendingReference(ctx context.Context, ref store.BlobRefRow, now int64) (bool, error) {
	var dropped bool
	err := s.repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.LockBlob(ctx, ref.BlobID); err != nil {
			return err
		}
		current, err := tx.GetBlobRef(ctx, ref.BlobID, ref.ChannelID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Confirmed {
			return nil
		}
		if err := tx.DeleteBlobRef(ctx, ref.BlobID, ref.ChannelID); err != nil {
			return err
		}
		if err := tx.MarkBlobUnreferenced(ctx, ref.BlobID, now); err != nil {
			return err
		}
		dropped = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if dropped {
		s.metrics.BlobRefExpired("pending")
	}
	return dropped, nil
}

func (s *Sweeper) dropReference(ctx context.Context, ref store.BlobRefRow, now int64, reason string) error {
	if err := s.repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.DeleteBlobRef(ctx, ref.BlobID, ref.ChannelID); err != nil {
			return err
		}
		// A no-op while another channel still references the blob.
		return tx.MarkBlobUnreferenced(ctx, ref.BlobID, now)
	}); err != nil {
		return err
	}
	s.metrics.BlobRefExpired(reason)
	return nil
}

// retentionDays reads retention_days from a stored community policy document
// (protocol/09 § Communities, api.CommunityPolicy). The document was validated
// when it was written; a stored document this cannot read keeps its
// attachments, because failing towards deletion would lose data an owner never
// asked to delete. 0 is "indefinitely".
func retentionDays(policy []byte) uint64 {
	var p struct {
		RetentionDays uint64 `json:"retention_days"`
	}
	if err := json.Unmarshal(policy, &p); err != nil {
		return 0
	}
	return min(p.RetentionDays, maxRetentionDays)
}

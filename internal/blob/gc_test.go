package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/dilla-chat/dilla/internal/api"
	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/obs"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// gcHarness is a real migrated SQLite repository, a real blob directory, a fake
// clock and a Sweeper over them with a 24 h grace window, which is
// blobs.gc_grace's default.
type gcHarness struct {
	t       *testing.T
	Repo    store.Repository
	Store   *blob.Store
	Clock   *clock.Fake
	Sweeper *blob.Sweeper
	Metrics *obs.Metrics
	reg     *prometheus.Registry
	owner   id.ID
	device  id.ID
}

const (
	gcGrace    = 24 * time.Hour
	gcPending  = 24 * time.Hour
	gcInterval = time.Hour
)

func newGCHarness(t *testing.T) *gcHarness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dilla.db")
	write, err := sqlite.OpenWrite(path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, write, sqlitemigrations.FS)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	read, err := sqlite.OpenRead(path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { _ = repo.Close() })
	bs, err := blob.Open(t.TempDir(), "fs")
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	clk := clock.NewFake(time.Unix(1_790_000_000, 0).UTC())
	reg := prometheus.NewRegistry()
	m := obs.NewMetrics(reg, reg)
	h := &gcHarness{
		t: t, Repo: repo, Store: bs, Clock: clk, Metrics: m, reg: reg,
		Sweeper: blob.NewSweeper(repo, bs, clk, gcGrace, gcPending, gcInterval, slog.New(slog.DiscardHandler)).WithMetrics(m),
		owner:   id.New(), device: id.New(),
	}
	now := clk.Now().Unix()
	if err := repo.CreateUser(t.Context(), store.UserRow{
		ID: h.owner, Username: "owner-" + h.owner.String()[:8], Display: "owner",
		UMKPub: make([]byte, 32), SSKPub: make([]byte, 32), SigUMKSSK: make([]byte, 64), Created: now,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := repo.CreateDevice(t.Context(), store.DeviceRow{
		ID: h.device, UserID: h.owner, DSKPub: make([]byte, 32), CredentialBlob: []byte{1},
		LastSeen: now, Created: now,
	}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return h
}

// metric is the value of the one sample of the named metric whose labels
// include every pair in labels; 0 when there is none yet.
func (h *gcHarness) metric(name string, labels ...string) float64 {
	h.t.Helper()
	families, err := h.reg.Gather()
	if err != nil {
		h.t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	next:
		for _, m := range f.GetMetric() {
			for i := 0; i+1 < len(labels); i += 2 {
				var ok bool
				for _, l := range m.GetLabel() {
					if l.GetName() == labels[i] && l.GetValue() == labels[i+1] {
						ok = true
					}
				}
				if !ok {
					continue next
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

// NewChannel creates a text channel in a fresh community whose policy keeps
// history indefinitely (the default).
func (h *gcHarness) NewChannel() id.ID {
	h.t.Helper()
	return h.NewChannelWithPolicy(api.CommunityPolicy{})
}

// NewChannelWithPolicy creates a text channel in a fresh community with the
// given policy document, spelled as the api package spells it.
func (h *gcHarness) NewChannelWithPolicy(p api.CommunityPolicy) id.ID {
	h.t.Helper()
	doc, err := json.Marshal(p)
	if err != nil {
		h.t.Fatalf("marshal policy: %v", err)
	}
	cid := id.New()
	if err := h.Repo.CreateCommunity(h.t.Context(), store.CommunityRow{
		ID: cid, Owner: h.owner, Name: "c", PolicyJSON: doc, PolicyVersion: 1,
		Created: h.Clock.Now().Unix(),
	}); err != nil {
		h.t.Fatalf("CreateCommunity: %v", err)
	}
	c := cid
	ch := store.ChannelRow{
		ID: id.New(), CommunityID: &c, Name: "files", SettingsJSON: []byte(`{}`),
		HostPolicyVersion: 1, Created: h.Clock.Now().Unix(),
	}
	if err := h.Repo.CreateChannel(h.t.Context(), ch); err != nil {
		h.t.Fatalf("CreateChannel: %v", err)
	}
	return ch.ID
}

// Upload does what PUT /v1/channels/{id}/blobs/{blob_id} does: store the bytes,
// then, in one transaction, write the blobs row, clear any unreferenced mark
// and add the channel's reference.
func (h *gcHarness) Upload(sum, payload []byte, ch id.ID) {
	h.t.Helper()
	ctx := h.t.Context()
	n, _, err := h.Store.Put(ctx, sum, bytes.NewReader(payload), 1<<20)
	if err != nil {
		h.t.Fatalf("Put: %v", err)
	}
	now := h.Clock.Now().Unix()
	if err := h.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.PutBlob(ctx, store.BlobRow{
			BlobID: sum, Size: uint64(n), StorageRef: blob.StorageRef("fs", sum), Created: now,
		}); err != nil {
			return err
		}
		if err := tx.ClearBlobUnreferenced(ctx, sum); err != nil {
			return err
		}
		return tx.PutBlobRef(ctx, sum, ch, h.device, "", now)
	}); err != nil {
		h.t.Fatalf("record upload: %v", err)
	}
}

// UploadPending does what PUT /v1/channels/{id}/blobs/{blob_id} does since L-HTTP-81: Upload with
// the reference written pending.
func (h *gcHarness) UploadPending(sum, payload []byte, ch id.ID) {
	h.t.Helper()
	ctx := h.t.Context()
	n, _, err := h.Store.Put(ctx, sum, bytes.NewReader(payload), 1<<20)
	if err != nil {
		h.t.Fatalf("Put: %v", err)
	}
	now := h.Clock.Now().Unix()
	if err := h.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.PutBlob(ctx, store.BlobRow{
			BlobID: sum, Size: uint64(n), StorageRef: blob.StorageRef("fs", sum), Created: now,
		}); err != nil {
			return err
		}
		if err := tx.ClearBlobUnreferenced(ctx, sum); err != nil {
			return err
		}
		return tx.PutPendingBlobRef(ctx, sum, ch, h.device, "", now)
	}); err != nil {
		h.t.Fatalf("record pending upload: %v", err)
	}
}

// confirmAfterListing confirms every pending reference it lists before handing the list back: a
// confirm that lands between the sweep's listing and its drop.
type confirmAfterListing struct{ store.Repository }

func (r confirmAfterListing) ListPendingBlobRefs(ctx context.Context, before int64, limit int32) ([]store.BlobRefRow, error) {
	rows, err := r.Repository.ListPendingBlobRefs(ctx, before, limit)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := r.ConfirmBlobRef(ctx, row.BlobID, row.ChannelID); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// DeleteRef does what the uploader's DELETE does: drop this channel's
// reference and mark the blob unreferenced when it was the last one.
func (h *gcHarness) DeleteRef(sum []byte, ch id.ID) {
	h.t.Helper()
	ctx := h.t.Context()
	if err := h.Repo.Tx(ctx, func(tx store.Repository) error {
		if err := tx.DeleteBlobRef(ctx, sum, ch); err != nil {
			return err
		}
		return tx.MarkBlobUnreferenced(ctx, sum, h.Clock.Now().Unix())
	}); err != nil {
		h.t.Fatalf("delete reference: %v", err)
	}
}

func TestTheFileSurvivesWhileAnyReferenceStands(t *testing.T) {
	h := newGCHarness(t) // repo + blob.Store + clock.Fake + Sweeper with a 24h grace
	payload := bytes.Repeat([]byte("a"), 100)
	sum := sha256.Sum256(payload)
	chA, chB := h.NewChannel(), h.NewChannel()
	h.Upload(sum[:], payload, chA)
	h.Upload(sum[:], payload, chB)

	h.DeleteRef(sum[:], chA)
	h.Clock.Advance(48 * time.Hour)
	if n, err := h.Sweeper.SweepOnce(t.Context()); err != nil || n != 0 {
		t.Fatalf("SweepOnce deleted %d blobs while a reference stood (%v)", n, err)
	}
	if _, err := h.Store.Stat(sum[:]); err != nil {
		t.Fatalf("the file was unlinked while referenced: %v", err)
	}

	h.DeleteRef(sum[:], chB)
	// Still inside the grace window.
	if n, _ := h.Sweeper.SweepOnce(t.Context()); n != 0 {
		t.Fatalf("SweepOnce deleted %d blobs inside the grace window", n)
	}
	h.Clock.Advance(25 * time.Hour)
	if n, err := h.Sweeper.SweepOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("SweepOnce = (%d, %v), want (1, nil)", n, err)
	}
	if _, err := h.Store.Stat(sum[:]); err == nil {
		t.Fatal("the unreferenced file survived the sweep")
	}
	if _, err := h.Repo.GetBlob(t.Context(), sum[:]); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the blobs row survived: %v", err)
	}
}

func TestAReferenceCreatedDuringTheGraceWindowSavesTheBlob(t *testing.T) {
	h := newGCHarness(t)
	payload := []byte("forwarded")
	sum := sha256.Sum256(payload)
	chA, chB := h.NewChannel(), h.NewChannel()
	h.Upload(sum[:], payload, chA)
	h.DeleteRef(sum[:], chA)
	h.Clock.Advance(25 * time.Hour)
	// A forward lands after the grace window has passed but before the sweep.
	h.Upload(sum[:], payload, chB)
	if n, err := h.Sweeper.SweepOnce(t.Context()); err != nil || n != 0 {
		t.Fatalf("SweepOnce = (%d, %v); a re-referenced blob must not be collected", n, err)
	}
	if _, err := h.Store.Stat(sum[:]); err != nil {
		t.Fatalf("the forwarded blob was unlinked: %v", err)
	}
}

func TestTheSweeperIsIdempotentAndBounded(t *testing.T) {
	h := newGCHarness(t)
	for i := range 300 {
		payload := []byte{byte(i), byte(i >> 8)}
		sum := sha256.Sum256(payload)
		ch := h.NewChannel()
		h.Upload(sum[:], payload, ch)
		h.DeleteRef(sum[:], ch)
	}
	h.Clock.Advance(25 * time.Hour)
	first, err := h.Sweeper.SweepOnce(t.Context())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if first > 256 {
		t.Fatalf("SweepOnce collected %d blobs in one pass; the batch is capped at 256", first)
	}
	total := first
	for range 4 {
		n, err := h.Sweeper.SweepOnce(t.Context())
		if err != nil {
			t.Fatalf("SweepOnce: %v", err)
		}
		total += n
	}
	if total != 300 {
		t.Fatalf("collected %d of 300", total)
	}
	if n, _ := h.Sweeper.SweepOnce(t.Context()); n != 0 {
		t.Fatalf("a sweep with nothing to do collected %d", n)
	}
	if got := h.metric("dilla_blob_gc_deleted_total"); got != 300 {
		t.Fatalf("dilla_blob_gc_deleted_total = %v, want 300", got)
	}
	if got := h.metric("dilla_blob_gc_bytes_total"); got != 600 {
		t.Fatalf("dilla_blob_gc_bytes_total = %v, want 600", got)
	}
	if got := h.metric("dilla_blob_gc_runs_total", "result", "ok"); got != 6 {
		t.Fatalf("dilla_blob_gc_runs_total{result=ok} = %v, want 6", got)
	}
}

// R28 (the controller's ruling for this task): a community's archival
// retention policy is also its attachments' retention. A reference older than
// retention_days is dropped, which makes the blob unreferenced, and the
// ordinary grace window then collects it. A community with no retention keeps
// its attachments indefinitely.
func TestACommunityRetentionPolicyExpiresOldReferences(t *testing.T) {
	h := newGCHarness(t)
	short := h.NewChannelWithPolicy(api.CommunityPolicy{RetentionDays: 1})
	forever := h.NewChannel()
	old := []byte("old enough to expire")
	oldSum := sha256.Sum256(old)
	kept := []byte("kept by a community with no retention")
	keptSum := sha256.Sum256(kept)
	h.Upload(oldSum[:], old, short)
	h.Upload(keptSum[:], kept, forever)

	// Twelve hours in: inside the one-day policy, nothing moves.
	h.Clock.Advance(12 * time.Hour)
	young := []byte("uploaded later")
	youngSum := sha256.Sum256(young)
	h.Upload(youngSum[:], young, short)
	if n, err := h.Sweeper.SweepOnce(t.Context()); err != nil || n != 0 {
		t.Fatalf("SweepOnce inside the policy = (%d, %v)", n, err)
	}
	if n, _ := h.Repo.CountBlobRefs(t.Context(), oldSum[:]); n != 1 {
		t.Fatalf("a reference inside the retention window was dropped (%d left)", n)
	}

	// Past the policy for the first upload only.
	h.Clock.Advance(13 * time.Hour)
	if n, err := h.Sweeper.SweepOnce(t.Context()); err != nil || n != 0 {
		t.Fatalf("SweepOnce past the policy = (%d, %v); the expired blob still owes its grace window", n, err)
	}
	if n, _ := h.Repo.CountBlobRefs(t.Context(), oldSum[:]); n != 0 {
		t.Fatalf("the expired reference survived (%d left)", n)
	}
	if n, _ := h.Repo.CountBlobRefs(t.Context(), youngSum[:]); n != 1 {
		t.Fatalf("a reference younger than the policy was dropped (%d left)", n)
	}
	row, err := h.Repo.GetBlob(t.Context(), oldSum[:])
	if err != nil || row.UnrefSince == nil || *row.UnrefSince != h.Clock.Now().Unix() {
		t.Fatalf("the expired blob was not marked unreferenced now: %+v, %v", row, err)
	}
	if got := h.metric("dilla_blob_refs_expired_total", "reason", "retention"); got != 1 {
		t.Fatalf("dilla_blob_refs_expired_total = %v, want 1", got)
	}

	// And the grace window then collects it, while the indefinite community's
	// attachment survives a year.
	h.Clock.Advance(25 * time.Hour)
	if n, err := h.Sweeper.SweepOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("SweepOnce after the grace window = (%d, %v), want (1, nil)", n, err)
	}
	if _, err := h.Store.Stat(oldSum[:]); err == nil {
		t.Fatal("the expired file survived")
	}
	h.Clock.Advance(365 * 24 * time.Hour)
	if _, err := h.Sweeper.SweepOnce(t.Context()); err != nil {
		t.Fatalf("SweepOnce a year on: %v", err)
	}
	if n, _ := h.Repo.CountBlobRefs(t.Context(), keptSum[:]); n != 1 {
		t.Fatal("a community with no retention policy lost an attachment")
	}
	if _, err := h.Store.Stat(keptSum[:]); err != nil {
		t.Fatalf("the indefinitely retained file was unlinked: %v", err)
	}
}

// A deleted channel's references go with it (gap-47 §10: "delete a channel,
// reclaim its attachments"): channels are tombstoned, never removed, so the
// ON DELETE CASCADE on blob_refs never fires and the sweeper drops them. A blob
// the channel shared with a live one survives.
func TestADeletedChannelsReferencesAreDropped(t *testing.T) {
	h := newGCHarness(t)
	gone, live := h.NewChannel(), h.NewChannel()
	only := []byte("only in the deleted channel")
	onlySum := sha256.Sum256(only)
	shared := []byte("in both")
	sharedSum := sha256.Sum256(shared)
	h.Upload(onlySum[:], only, gone)
	h.Upload(sharedSum[:], shared, gone)
	h.Upload(sharedSum[:], shared, live)
	row, err := h.Repo.GetChannel(t.Context(), gone)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if _, err := h.Repo.DeleteChannelsOfCommunity(t.Context(), *row.CommunityID, h.Clock.Now().Unix()); err != nil {
		t.Fatalf("DeleteChannelsOfCommunity: %v", err)
	}
	if _, err := h.Sweeper.SweepOnce(t.Context()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n, _ := h.Repo.CountBlobRefs(t.Context(), onlySum[:]); n != 0 {
		t.Fatalf("the deleted channel's reference survived (%d)", n)
	}
	if n, _ := h.Repo.CountBlobRefs(t.Context(), sharedSum[:]); n != 1 {
		t.Fatalf("the shared blob has %d references, want the live channel's 1", n)
	}
	h.Clock.Advance(25 * time.Hour)
	if n, err := h.Sweeper.SweepOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("SweepOnce = (%d, %v), want (1, nil)", n, err)
	}
	if _, err := h.Store.Stat(sharedSum[:]); err != nil {
		t.Fatalf("the blob a live channel still references was unlinked: %v", err)
	}
}

// Run sweeps once per interval on the instance clock and returns when its
// context ends.
func TestRunSweepsEveryIntervalUntilCancelled(t *testing.T) {
	h := newGCHarness(t)
	payload := []byte("collected by the loop")
	sum := sha256.Sum256(payload)
	ch := h.NewChannel()
	h.Upload(sum[:], payload, ch)
	h.DeleteRef(sum[:], ch)
	h.Clock.Advance(25 * time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Sweeper.Run(ctx)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		// Advancing fires the timer Run is waiting on; until Run has created it,
		// an advance fires nothing, so keep nudging.
		h.Clock.Advance(gcInterval)
		if _, err := h.Store.Stat(sum[:]); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never swept")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

// gap-47 §8.4: a backup in flight pins the sweeper's cutoff to its start. The
// snapshot still names a blob whose last reference went after the backup
// started, and collecting it would put a false gap in the archive.
func TestASweepDuringABackupCollectsNothing(t *testing.T) {
	h := newGCHarness(t)
	payload := []byte("snapshotted")
	sum := sha256.Sum256(payload)
	ch := h.NewChannel()
	h.Upload(sum[:], payload, ch)
	started := h.Clock.Now().Unix()
	if err := h.Repo.PutSetting(t.Context(), "last_backup_started", []byte(strconv.FormatInt(started, 10)), started); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	h.DeleteRef(sum[:], ch)
	h.Clock.Advance(gcGrace + time.Hour)
	if n, err := h.Sweeper.SweepOnce(t.Context()); err != nil || n != 0 {
		t.Fatalf("SweepOnce during a backup = (%d, %v), want (0, nil)", n, err)
	}
	if _, err := h.Store.Stat(sum[:]); err != nil {
		t.Fatalf("the file the snapshot names was unlinked: %v", err)
	}

	// The backup finishes and clears the mark: the next pass collects it.
	if err := h.Repo.PutSetting(t.Context(), "last_backup_started", []byte{}, h.Clock.Now().Unix()); err != nil {
		t.Fatalf("PutSetting: %v", err)
	}
	if n, err := h.Sweeper.SweepOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("SweepOnce after the backup = (%d, %v), want (1, nil)", n, err)
	}
}

// L-HTTP-83: an upload its uploader never confirmed is dropped after pending_ttl like an expired
// reference: the blob is marked unreferenced now and the grace window then collects it.
func TestAnUploadNeverConfirmedIsSweptAfterPendingTTL(t *testing.T) {
	h := newGCHarness(t)
	payload := []byte("attached, never sent")
	sum := sha256.Sum256(payload)
	ch := h.NewChannel()
	h.UploadPending(sum[:], payload, ch)

	h.Clock.Advance(23 * time.Hour)
	if n, err := h.Sweeper.SweepOnce(t.Context()); err != nil || n != 0 {
		t.Fatalf("SweepOnce inside pending_ttl = (%d, %v)", n, err)
	}
	if n, _ := h.Repo.CountBlobRefs(t.Context(), sum[:]); n != 1 {
		t.Fatalf("a pending reference younger than pending_ttl was dropped (%d left)", n)
	}

	h.Clock.Advance(2 * time.Hour)
	if n, err := h.Sweeper.SweepOnce(t.Context()); err != nil || n != 0 {
		t.Fatalf("SweepOnce past pending_ttl = (%d, %v); the blob still owes its grace window", n, err)
	}
	if n, _ := h.Repo.CountBlobRefs(t.Context(), sum[:]); n != 0 {
		t.Fatalf("the unconfirmed reference survived pending_ttl (%d left)", n)
	}
	row, err := h.Repo.GetBlob(t.Context(), sum[:])
	if err != nil || row.UnrefSince == nil || *row.UnrefSince != h.Clock.Now().Unix() {
		t.Fatalf("the swept blob was not marked unreferenced now: %+v, %v", row, err)
	}
	if got := h.metric("dilla_blob_refs_expired_total", "reason", "pending"); got != 1 {
		t.Fatalf("dilla_blob_refs_expired_total{reason=pending} = %v, want 1", got)
	}

	h.Clock.Advance(25 * time.Hour)
	if n, err := h.Sweeper.SweepOnce(t.Context()); err != nil || n != 1 {
		t.Fatalf("SweepOnce after the grace window = (%d, %v), want (1, nil)", n, err)
	}
	if _, err := h.Store.Stat(sum[:]); err == nil {
		t.Fatal("the never-confirmed file survived")
	}
}

// L-HTTP-83's companion: a confirmed reference, and one PutBlobRef wrote, outlive pending_ttl; an
// unconfirmed one younger than it is kept until it is older.
func TestAConfirmedReferenceOutlivesPendingTTL(t *testing.T) {
	h := newGCHarness(t)
	ch := h.NewChannel()
	confirmed := []byte("confirmed before sending")
	confirmedSum := sha256.Sum256(confirmed)
	plain := []byte("stored before pending existed")
	plainSum := sha256.Sum256(plain)
	h.UploadPending(confirmedSum[:], confirmed, ch)
	if err := h.Repo.ConfirmBlobRef(t.Context(), confirmedSum[:], ch); err != nil {
		t.Fatalf("ConfirmBlobRef: %v", err)
	}
	h.Upload(plainSum[:], plain, ch)

	h.Clock.Advance(12 * time.Hour)
	young := []byte("attached later")
	youngSum := sha256.Sum256(young)
	h.UploadPending(youngSum[:], young, ch)

	h.Clock.Advance(13 * time.Hour)
	if _, err := h.Sweeper.SweepOnce(t.Context()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	for name, sum := range map[string][]byte{"confirmed": confirmedSum[:], "plain": plainSum[:], "young": youngSum[:]} {
		if n, _ := h.Repo.CountBlobRefs(t.Context(), sum); n != 1 {
			t.Fatalf("the %s reference was dropped at 25 h (%d left)", name, n)
		}
	}
	if got := h.metric("dilla_blob_refs_expired_total", "reason", "pending"); got != 0 {
		t.Fatalf("dilla_blob_refs_expired_total{reason=pending} = %v, want 0", got)
	}

	h.Clock.Advance(12 * time.Hour)
	if _, err := h.Sweeper.SweepOnce(t.Context()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n, _ := h.Repo.CountBlobRefs(t.Context(), youngSum[:]); n != 0 {
		t.Fatalf("the young unconfirmed reference survived its own pending_ttl (%d left)", n)
	}
	for name, sum := range map[string][]byte{"confirmed": confirmedSum[:], "plain": plainSum[:]} {
		if n, _ := h.Repo.CountBlobRefs(t.Context(), sum); n != 1 {
			t.Fatalf("the %s reference was dropped at 37 h (%d left)", name, n)
		}
	}
}

// A pending reference dropped while another channel references the same bytes leaves the blob
// referenced (MarkBlobUnreferenced's NOT EXISTS).
func TestAPendingReferenceThatSharesItsBytesLeavesTheBlobReferenced(t *testing.T) {
	h := newGCHarness(t)
	payload := []byte("forwarded, then attached again")
	sum := sha256.Sum256(payload)
	chA, chB := h.NewChannel(), h.NewChannel()
	h.Upload(sum[:], payload, chA)
	h.UploadPending(sum[:], payload, chB)
	h.Clock.Advance(25 * time.Hour)
	if _, err := h.Sweeper.SweepOnce(t.Context()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n, _ := h.Repo.CountBlobRefs(t.Context(), sum[:]); n != 1 {
		t.Fatalf("references = %d, want chA's", n)
	}
	if row, err := h.Repo.GetBlob(t.Context(), sum[:]); err != nil || row.UnrefSince != nil {
		t.Fatalf("a still-referenced blob was marked unreferenced: %+v, %v", row, err)
	}
}

// pending_ttl = 0 turns the sweep off; a negative value is treated as 0.
func TestAPendingTTLOfZeroSweepsNothing(t *testing.T) {
	h := newGCHarness(t)
	payload := []byte("kept while the sweep is off")
	sum := sha256.Sum256(payload)
	h.UploadPending(sum[:], payload, h.NewChannel())
	h.Clock.Advance(100 * 24 * time.Hour)
	for _, ttl := range []time.Duration{0, -time.Hour} {
		off := blob.NewSweeper(h.Repo, h.Store, h.Clock, gcGrace, ttl, gcInterval, slog.New(slog.DiscardHandler))
		if _, err := off.SweepOnce(t.Context()); err != nil {
			t.Fatalf("SweepOnce with pending_ttl %v: %v", ttl, err)
		}
		if n, _ := h.Repo.CountBlobRefs(t.Context(), sum[:]); n != 1 {
			t.Fatalf("pending_ttl %v dropped a reference (%d left)", ttl, n)
		}
	}
}

// L-HTTP-83's race: a confirm that lands between the listing and the drop keeps the reference.
func TestAReferenceConfirmedAfterTheListingIsKept(t *testing.T) {
	h := newGCHarness(t)
	payload := []byte("confirmed while the sweep ran")
	sum := sha256.Sum256(payload)
	ch := h.NewChannel()
	h.UploadPending(sum[:], payload, ch)
	h.Clock.Advance(25 * time.Hour)
	racing := blob.NewSweeper(confirmAfterListing{h.Repo}, h.Store, h.Clock, gcGrace, gcPending, gcInterval,
		slog.New(slog.DiscardHandler)).WithMetrics(h.Metrics)
	if _, err := racing.SweepOnce(t.Context()); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n, _ := h.Repo.CountBlobRefs(t.Context(), sum[:]); n != 1 {
		t.Fatalf("a reference confirmed after the listing was dropped (%d left)", n)
	}
	if got := h.metric("dilla_blob_refs_expired_total", "reason", "pending"); got != 0 {
		t.Fatalf("dilla_blob_refs_expired_total{reason=pending} = %v, want 0", got)
	}
}

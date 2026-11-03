package ops_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/ops"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

func (h *opsHarness) backup(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &buf, IncludeBlobs: true, Clock: h.Clock}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return buf.Bytes()
}

func (h *opsHarness) restore(t *testing.T, archive []byte, o ops.RestoreOptions) (ops.RestorePlan, error) {
	t.Helper()
	o.From = bytes.NewReader(archive)
	if o.Clock == nil {
		o.Clock = h.Clock
	}
	return ops.Restore(t.Context(), h.Cfg, o)
}

// restored opens the database at the configured path as it stands now; the
// harness's own pools still point at the directory a restore moved aside.
func (h *opsHarness) restored(t *testing.T) store.Repository {
	t.Helper()
	write, err := sqlite.OpenWrite(h.Cfg.DB.Path)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	read, err := sqlite.OpenRead(h.Cfg.DB.Path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	repo := sqlite.New(write, read)
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func countMessages(t *testing.T, repo store.Repository, group id.ID) int {
	t.Helper()
	rows, err := repo.ListAppMessages(t.Context(), group, 0, 1000)
	if err != nil {
		t.Fatalf("ListAppMessages: %v", err)
	}
	return len(rows)
}

// The lock Restore took is on <old>/dilla.lock once the directories are
// swapped; the restored directory must be locked before anything else can see
// it, or a second dillad could start against it mid-restore.
func TestASecondDilladStillCannotStartAfterARestore(t *testing.T) {
	h := newOpsHarness(t)
	archive := h.backup(t)
	var during error
	ops.SetAfterSwapHook(t, func(dataDir string) {
		if dataDir != h.Cfg.Instance.DataDir {
			t.Errorf("hook dataDir = %q, want %q", dataDir, h.Cfg.Instance.DataDir)
		}
		_, during = ops.AcquireServeLock(dataDir)
	})
	if _, err := h.restore(t, archive, ops.RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !errors.Is(during, ops.ErrLocked) {
		t.Fatalf("a serve lock taken right after the swap = %v, want ErrLocked", during)
	}
	// And once Restore has returned, the directory is free again.
	l, err := ops.AcquireServeLock(h.Cfg.Instance.DataDir)
	if err != nil {
		t.Fatalf("AcquireServeLock after the restore: %v", err)
	}
	_ = l.Release()
}

// The archive's database, blobs and ACME key replace the live ones; files the
// archive does not carry (an issued certificate, the operator's dilla.toml)
// are kept; the live directory is left beside the restored one.
func TestRestoreReplacesTheDatabaseAndBlobsAndKeepsTheRest(t *testing.T) {
	h := newOpsHarness(t)
	h.SeedRows(5)
	archive := h.backup(t)

	// After the backup: more messages, a lost blob, a changed ACME key, and a
	// certificate the archive never carries.
	h.SeedRows(7)
	h.LoseOneBlob()
	keyPath := filepath.Join(h.Cfg.TLS.StorageDir, "acme", "acme-v02.api.letsencrypt.org-directory", "users", "ops@dilla.test", "ops.key")
	if err := os.WriteFile(keyPath, []byte("changed"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cert := filepath.Join(h.Cfg.TLS.StorageDir, "certificates", "dilla.test.crt")
	if err := os.MkdirAll(filepath.Dir(cert), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(cert, []byte("issued"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	before, err := os.Stat(h.Cfg.Instance.DataDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	plan, err := h.restore(t, archive, ops.RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// The data directory itself is never renamed: it may be a mount point.
	after, err := os.Stat(h.Cfg.Instance.DataDir)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("the data directory was replaced rather than its entries (%v)", err)
	}
	repo := h.restored(t)
	if n := countMessages(t, repo, h.group); n != 5 {
		t.Fatalf("%d messages after the restore, want the archive's 5", n)
	}
	bs, err := blob.Open(h.Cfg.Blobs.Dir, h.Cfg.Blobs.Backend)
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	defer func() { _ = bs.Close() }()
	for _, b := range h.blobIDs {
		if _, err := bs.Stat(b); err != nil {
			t.Fatalf("blob %x after the restore: %v", b, err)
		}
	}
	if got, err := os.ReadFile(keyPath); err != nil || string(got) != h.ACMEKey {
		t.Fatalf("ACME key after the restore = %q, %v; want the archived key", got, err)
	}
	if got, err := os.ReadFile(cert); err != nil || string(got) != "issued" {
		t.Fatalf("the certificate the archive does not carry = %q, %v; want it kept", got, err)
	}
	if plan.OldDir == "" || !strings.HasPrefix(plan.OldDir, filepath.Join(h.Cfg.Instance.DataDir, ".old-")) {
		t.Fatalf("plan.OldDir = %q", plan.OldDir)
	}
	// Nothing is created beside the data directory: in the packaged deployments
	// it is a mount point, or the only writable path under /var/lib.
	assertNoLeftovers(t, h.Cfg.Instance.DataDir, ".old-")
	// The directory moved aside is the live one as it was: its database still
	// holds all twelve messages.
	oldDB, err := sqlite.OpenRead(filepath.Join(plan.OldDir, "dilla.db"))
	if err != nil {
		t.Fatalf("open the old database: %v", err)
	}
	old := sqlite.New(nil, oldDB)
	defer func() { _ = old.Close() }()
	if n := countMessages(t, old, h.group); n != 12 {
		t.Fatalf("%d messages in the directory moved aside, want 12", n)
	}
	if plan.BlobsToWrite != len(h.blobIDs) {
		t.Fatalf("BlobsToWrite = %d, want %d", plan.BlobsToWrite, len(h.blobIDs))
	}
	if plan.RowCounts["mls_app_messages"] != 5 {
		t.Fatalf("RowCounts[mls_app_messages] = %d, want 5", plan.RowCounts["mls_app_messages"])
	}
}

func TestRestoreRemoveOldDeletesTheDirectoryMovedAside(t *testing.T) {
	h := newOpsHarness(t)
	archive := h.backup(t)
	plan, err := h.restore(t, archive, ops.RestoreOptions{RemoveOld: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if plan.OldDir != "" {
		t.Fatalf("plan.OldDir = %q with RemoveOld", plan.OldDir)
	}
	assertNoLeftovers(t, h.Cfg.Instance.DataDir)
}

// assertNoLeftovers fails when anything a restore stages or moves aside is left
// beside the data directory, or inside it apart from the prefixes allowed.
func assertNoLeftovers(t *testing.T, dataDir string, allowed ...string) {
	t.Helper()
	siblings, err := filepath.Glob(dataDir + ".*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(siblings) != 0 {
		t.Fatalf("left beside the data directory: %v", siblings)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == ops.RestoreMarkerFile {
			t.Fatalf("the swap marker %s was left behind", name)
		}
		if !strings.HasPrefix(name, ".restore-") && !strings.HasPrefix(name, ".old-") {
			continue
		}
		ok := false
		for _, a := range allowed {
			ok = ok || strings.HasPrefix(name, a)
		}
		if !ok {
			t.Fatalf("left inside the data directory: %s", name)
		}
	}
}

// A swap that stopped half-way leaves the marker naming both halves; neither a
// restore nor a serve (cmd/dillad's test) runs over it.
func TestAnInterruptedSwapIsRefused(t *testing.T) {
	h := newOpsHarness(t)
	archive := h.backup(t)
	marker := filepath.Join(h.Cfg.Instance.DataDir, ops.RestoreMarkerFile)
	if err := os.WriteFile(marker, []byte("the live entries are moving to X\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := h.restore(t, archive, ops.RestoreOptions{})
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Data || !strings.Contains(err.Error(), "interrupted") ||
		!strings.Contains(err.Error(), "moving to X") {
		t.Fatalf("restore over an interrupted swap = %v, want exit.Data naming both halves", err)
	}
}

// The generation never walks backwards: restoring a backup taken at
// generation 1 onto an instance already at 5 lands on 6, so no resume token
// minted at 2..5 is valid against the restored state.
func TestRestoreNeverWalksTheGenerationBackwards(t *testing.T) {
	h := newOpsHarness(t)
	archive := h.backup(t)
	for range 4 {
		if _, err := h.Repo.BumpGeneration(t.Context()); err != nil {
			t.Fatalf("BumpGeneration: %v", err)
		}
	}
	plan, err := h.restore(t, archive, ops.RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if plan.GenerationOld != 5 || plan.GenerationNew != 6 {
		t.Fatalf("plan generations = %d -> %d, want 5 -> 6", plan.GenerationOld, plan.GenerationNew)
	}
	in, err := h.restored(t).GetInstance(t.Context())
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if in.Generation != 6 {
		t.Fatalf("generation after the restore = %d, want 6", in.Generation)
	}
}

// Every SQLite snapshot carries last_backup_started, written before the
// snapshot; left there it would pin the restored instance's blob sweeper at
// the backup's start for good (task 12's concern).
func TestRestoreClearsTheBackupPinAndMarksTheRestorePending(t *testing.T) {
	h := newOpsHarness(t)
	archive := h.backup(t)
	if v := h.ArchivedSetting(t, archive, blob.LastBackupStartedKey); v == "" {
		t.Fatal("precondition: the snapshot carries no backup pin")
	}
	plan, err := h.restore(t, archive, ops.RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	repo := h.restored(t)
	if v, err := repo.GetSetting(t.Context(), blob.LastBackupStartedKey); err != nil || len(v) != 0 {
		t.Fatalf("%s after the restore = %q, %v; want empty", blob.LastBackupStartedKey, v, err)
	}
	v, err := repo.GetSetting(t.Context(), ds.RestorePendingKey)
	if err != nil || string(v) != strconv.FormatUint(plan.GenerationNew, 10) {
		t.Fatalf("%s = %q, %v; want %d", ds.RestorePendingKey, v, err, plan.GenerationNew)
	}
}

func TestRestoreRefusesAnotherInstancesArchiveUnlessForced(t *testing.T) {
	other := newOpsHarness(t)
	archive := other.backup(t)
	h := newOpsHarness(t)
	before := h.Generation(t)
	_, err := h.restore(t, archive, ops.RestoreOptions{})
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Config || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("restoring another instance's archive = %v, want exit.Config naming --force", err)
	}
	if h.Generation(t) != before {
		t.Fatal("a refused restore changed the instance")
	}
	if _, err := h.restore(t, archive, ops.RestoreOptions{Force: true}); err != nil {
		t.Fatalf("Restore --force: %v", err)
	}
	in, err := h.restored(t).GetInstance(t.Context())
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if in.InstanceID != other.Instance(t).InstanceID {
		t.Fatal("--force did not restore the other instance's identity")
	}
}

func TestRestoreRefusesAnArchiveWithGapsUnlessForced(t *testing.T) {
	h := newOpsHarness(t)
	h.LoseOneBlob()
	var buf bytes.Buffer
	if _, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{
		Out: &buf, IncludeBlobs: true, AllowGaps: true, Clock: h.Clock,
	}); err != nil {
		t.Fatalf("Backup --allow-gaps: %v", err)
	}
	_, err := h.restore(t, buf.Bytes(), ops.RestoreOptions{})
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Data || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("restoring an archive with a gap = %v, want exit.Data naming the gap", err)
	}
	if _, err := h.restore(t, buf.Bytes(), ops.RestoreOptions{Force: true}); err != nil {
		t.Fatalf("Restore --force: %v", err)
	}
}

// Plan 1 edited 00002_mls.sql in place (handshakes_pruned_through), so a
// database an earlier build of the branch created claims schema 2 or later
// without the column. Restoring one would serve every group from a table the
// queries do not match; it is refused with the remedy.
func TestRestoreRefusesADatabaseFromAnEarlierBranchBuild(t *testing.T) {
	h := newOpsHarness(t)
	if _, err := h.write.ExecContext(t.Context(), `ALTER TABLE mls_groups DROP COLUMN handshakes_pruned_through`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	archive := h.backup(t)
	_, err := h.restore(t, archive, ops.RestoreOptions{})
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Data || !strings.Contains(err.Error(), "handshakes_pruned_through") {
		t.Fatalf("restoring a pre-edit database = %v, want exit.Data naming the column", err)
	}
}

func TestRestoreRefusesAnEngineMismatch(t *testing.T) {
	h := newOpsHarness(t)
	archive := h.backup(t)
	h.Cfg.DB.Driver = "postgres"
	_, err := h.restore(t, archive, ops.RestoreOptions{DryRun: true})
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Config {
		t.Fatalf("a sqlite archive onto a postgres config = %v, want exit.Config", err)
	}
}

// The restore also runs on a fresh host: no data directory at all.
func TestRestoreOntoAnEmptyDataDirectory(t *testing.T) {
	h := newOpsHarness(t)
	h.SeedRows(3)
	archive := h.backup(t)
	fresh := *h.Cfg
	dir := filepath.Join(t.TempDir(), "fresh")
	fresh.Instance.DataDir = dir
	fresh.DB.Path = filepath.Join(dir, "dilla.db")
	fresh.Blobs.Dir = filepath.Join(dir, "blobs")
	fresh.TLS.StorageDir = filepath.Join(dir, "certmagic")
	plan, err := ops.Restore(t.Context(), &fresh, ops.RestoreOptions{From: bytes.NewReader(archive), Clock: h.Clock})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if plan.GenerationOld != 1 || plan.GenerationNew != 2 {
		t.Fatalf("plan generations = %d -> %d, want 1 -> 2", plan.GenerationOld, plan.GenerationNew)
	}
	read, err := sqlite.OpenRead(fresh.DB.Path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	repo := sqlite.New(nil, read)
	defer func() { _ = repo.Close() }()
	if n := countMessages(t, repo, h.group); n != 3 {
		t.Fatalf("%d messages on the fresh host, want 3", n)
	}
}

// Nothing is written until the whole archive has verified: a damaged member
// anywhere leaves the live directory exactly as it was, with no sibling.
func TestADamagedArchiveLeavesTheInstanceUntouched(t *testing.T) {
	h := newOpsHarness(t)
	archive := truncateLastMember(t)(h.backup(t))
	before := h.Generation(t)
	_, err := h.restore(t, archive, ops.RestoreOptions{})
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Data {
		t.Fatalf("Restore of a truncated archive = %v, want exit.Data", err)
	}
	if h.Generation(t) != before {
		t.Fatal("the live database changed")
	}
	assertNoLeftovers(t, h.Cfg.Instance.DataDir)
}

// The heal deadline is the heal window from the restore's clock, and every
// open group is epoch-unknown.
func TestRestoreArmsTheHealOnEveryOpenGroup(t *testing.T) {
	h := newOpsHarness(t)
	archive := h.backup(t)
	h.Clock.Advance(time.Hour)
	if _, err := h.restore(t, archive, ops.RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	g, err := h.restored(t).GetGroup(context.Background(), h.group)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	want := h.Clock.Now().Add(ds.DefaultPolicy().HealWindow).Unix()
	if !g.EpochUnknown || g.HealDeadline == nil || *g.HealDeadline != want {
		t.Fatalf("group after restore: epoch_unknown %v deadline %v, want true and %d", g.EpochUnknown, g.HealDeadline, want)
	}
}

// On SQLite the heal runs on the staged copy before the swap: a heal that fails
// leaves the live instance exactly as it was, with nothing left behind.
func TestAFailedHealLeavesTheInstanceUntouched(t *testing.T) {
	h := newOpsHarness(t)
	h.SeedRows(3)
	archive := h.backup(t)
	h.SeedRows(2)
	before := h.Generation(t)
	ops.SetBeforeHealCommit(t, func() error { return errors.New("injected: the heal transaction fails") })
	_, err := h.restore(t, archive, ops.RestoreOptions{})
	var code exit.Code
	if !errors.As(err, &code) || code != exit.IOErr || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("restore with a failing heal = %v, want exit.IOErr", err)
	}
	if h.Generation(t) != before {
		t.Fatal("a failed heal changed the live generation")
	}
	if n := countMessages(t, h.restored(t), h.group); n != 5 {
		t.Fatalf("%d messages after a failed heal, want the live 5", n)
	}
	assertNoLeftovers(t, h.Cfg.Instance.DataDir)
}

// failRestoredDBMove makes the swap's last move, the restored database into
// place, fail; every other rename runs, unless also refuses it.
func failRestoredDBMove(t *testing.T, h *opsHarness, also func(from, to string) error) {
	t.Helper()
	sep := string(filepath.Separator)
	ops.SetSwapRename(t, func(from, to string) error {
		if to == h.Cfg.DB.Path && strings.Contains(from, sep+".restore-") {
			return errors.New("injected: the restored database cannot move into place")
		}
		if also != nil {
			if err := also(from, to); err != nil {
				return err
			}
		}
		return os.Rename(from, to)
	})
}

// A move that fails half-way through the swap is undone: every live entry is
// back where it was, the marker is gone, and nothing a restore made is left.
func TestAFailedSwapIsUndoneCompletely(t *testing.T) {
	h := newOpsHarness(t)
	h.SeedRows(3)
	archive := h.backup(t)
	h.SeedRows(2)
	dataDir := h.Cfg.Instance.DataDir
	failRestoredDBMove(t, h, nil)
	_, err := h.restore(t, archive, ops.RestoreOptions{})
	var code exit.Code
	if !errors.As(err, &code) || code != exit.IOErr {
		t.Fatalf("restore with a failing swap = %v, want exit.IOErr", err)
	}
	if err := ops.InterruptedRestore(dataDir); err != nil {
		t.Fatalf("a fully undone swap still reads as interrupted: %v", err)
	}
	assertNoLeftovers(t, dataDir)
	if n := countMessages(t, h.restored(t), h.group); n != 5 {
		t.Fatalf("%d messages after the undone swap, want the live 5", n)
	}
}

// An undo that cannot move an entry back leaves the data directory split
// between itself, .old-<hex> and .restore-<hex>. The marker must survive, name
// what is out of place, and keep serve (InterruptedRestore) and a second
// restore from running over the half-swapped directory; the staging directory
// is kept, since it holds half of what the operator must put back.
func TestAFailedUndoKeepsTheMarkerAndRefusesServe(t *testing.T) {
	h := newOpsHarness(t)
	archive := h.backup(t)
	dataDir := h.Cfg.Instance.DataDir
	blobs := filepath.Join(dataDir, "blobs")
	failRestoredDBMove(t, h, func(from, to string) error {
		if to == blobs && strings.Contains(from, string(filepath.Separator)+".old-") {
			return errors.New("injected: the live blobs cannot move back")
		}
		return nil
	})
	_, err := h.restore(t, archive, ops.RestoreOptions{})
	var code exit.Code
	if !errors.As(err, &code) || code != exit.IOErr || !strings.Contains(err.Error(), "manual repair") {
		t.Fatalf("restore with a failing undo = %v, want exit.IOErr asking for manual repair", err)
	}
	body, rerr := os.ReadFile(filepath.Join(dataDir, ops.RestoreMarkerFile))
	if rerr != nil {
		t.Fatalf("the marker is gone after an undo that failed: %v", rerr)
	}
	if !strings.Contains(string(body), blobs) || !strings.Contains(string(body), "cannot move back") {
		t.Fatalf("the marker does not name the entry left out of place: %q", body)
	}
	// serve's check, and a second restore, both refuse.
	if ierr := ops.InterruptedRestore(dataDir); !errors.As(ierr, &code) || code != exit.Data {
		t.Fatalf("InterruptedRestore after a failed undo = %v, want exit.Data", ierr)
	}
	if _, err := h.restore(t, archive, ops.RestoreOptions{}); !errors.As(err, &code) || code != exit.Data ||
		!strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("a restore over the half-swapped directory = %v, want the interrupted refusal", err)
	}
	// Both halves are still on disk for the operator to put back.
	if olds, _ := filepath.Glob(filepath.Join(dataDir, ".old-*", "blobs")); len(olds) != 1 {
		t.Fatalf("the live blobs are not in the .old- directory: %v", olds)
	}
	if stages, _ := filepath.Glob(filepath.Join(dataDir, ".restore-*")); len(stages) != 1 {
		t.Fatalf("the staging directory was removed after a failed undo: %v", stages)
	}
}

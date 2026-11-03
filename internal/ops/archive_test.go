package ops_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/ops"
)

func TestSQLiteBackupIsConsistentWithAReaderOpen(t *testing.T) {
	h := newOpsHarness(t) // a migrated SQLite instance with two blobs and a reader pool open
	h.SeedRows(100)
	var buf bytes.Buffer
	man, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &buf, IncludeBlobs: true, Clock: h.Clock})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if man.Engine != "sqlite" {
		t.Fatalf("engine = %q", man.Engine)
	}
	if man.SchemaVersion != h.SchemaVersion(t) {
		t.Fatalf("manifest schema_version = %d, want %d", man.SchemaVersion, h.SchemaVersion(t))
	}
	names := tarNames(t, buf.Bytes())
	want := []string{"dilla-backup/MANIFEST.json", "dilla-backup/db/dilla.sqlite",
		"dilla-backup/config/dilla.toml", "dilla-backup/keys/instance.json"}
	for i, w := range want {
		if names[i] != w {
			t.Fatalf("entry %d = %q, want %q", i, names[i], w)
		}
	}
	// The restored database opens and holds the rows.
	if n := h.RowsInArchivedDB(t, buf.Bytes()); n != 100 {
		t.Fatalf("archived database holds %d rows, want 100", n)
	}
}

// Renamed from TestTheArchiveIsByteIdenticalForAnUnchangedInstance, which
// promised more than it checked: MANIFEST.json carries created_at, which this
// test advances by an hour, so the streams cannot be byte-identical. What the
// format actually guarantees is that no HOST state reaches the archive, and that
// is what is asserted here. TestTwoBackupsWithTheClockHeldAreIdentical below
// carries the byte-for-byte half.
func TestTheArchiveCarriesNoHostState(t *testing.T) {
	h := newOpsHarness(t)
	h.SeedRows(10)
	var a, b bytes.Buffer
	if _, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &a, IncludeBlobs: true, Clock: h.Clock}); err != nil {
		t.Fatalf("first Backup: %v", err)
	}
	h.Clock.Advance(time.Hour) // only the manifest's created_at moves
	if _, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &b, IncludeBlobs: true, Clock: h.Clock}); err != nil {
		t.Fatalf("second Backup: %v", err)
	}
	na, nb := tarNames(t, a.Bytes()), tarNames(t, b.Bytes())
	if !slices.Equal(na, nb) {
		t.Fatalf("entry order moved:\n%v\n%v", na, nb)
	}
	ha, hb := tarHeaders(t, a.Bytes()), tarHeaders(t, b.Bytes())
	for i := range ha {
		if ha[i].ModTime != hb[i].ModTime || ha[i].Uid != hb[i].Uid || ha[i].Gid != hb[i].Gid ||
			ha[i].Uname != hb[i].Uname || ha[i].Gname != hb[i].Gname || ha[i].Format != tar.FormatPAX {
			t.Fatalf("entry %q carries host state: %+v", ha[i].Name, ha[i])
		}
	}
}

func TestTwoBackupsWithTheClockHeldAreIdentical(t *testing.T) {
	h := newOpsHarness(t)
	h.SeedRows(10)
	var a, b bytes.Buffer
	// The clock does NOT move, so created_at is the same and the gzip streams must
	// match byte for byte. This is the property the fixed entry order, the zeroed
	// tar headers and the zeroed gzip header exist for.
	if _, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &a, IncludeBlobs: true, Clock: h.Clock}); err != nil {
		t.Fatalf("first Backup: %v", err)
	}
	if _, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &b, IncludeBlobs: true, Clock: h.Clock}); err != nil {
		t.Fatalf("second Backup: %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("two backups of an unchanged instance differ (%d vs %d bytes)", a.Len(), b.Len())
	}
}

func TestVerifyDetectsATruncatedMemberAWrongDigestAndASchemaMismatch(t *testing.T) {
	h := newOpsHarness(t)
	h.SeedRows(5)
	var good bytes.Buffer
	if _, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &good, IncludeBlobs: true, Clock: h.Clock}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := ops.Verify(t.Context(), bytes.NewReader(good.Bytes())); err != nil {
		t.Fatalf("Verify of a good archive: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func([]byte) []byte
		want string
	}{
		{"truncated member", truncateLastMember(t), "short"},
		{"wrong digest", flipOneByteOfTheDatabase(t), "sha256"},
		{"schema newer than the binary", bumpManifestSchema(t), "schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ops.Verify(t.Context(), bytes.NewReader(tc.mut(slices.Clone(good.Bytes()))))
			if err == nil {
				t.Fatal("Verify accepted a damaged archive")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestTheArchiveHoldsNoEndToEndPlaintext(t *testing.T) {
	h := newOpsHarness(t)
	// A marker that would only be present if an envelope were stored decrypted.
	const marker = "PLAINTEXT-CANARY-8f31"
	h.SeedE2EEMessageWithPlaintext(marker) // stores the marker as MLS ciphertext only
	var buf bytes.Buffer
	if _, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &buf, IncludeBlobs: true, Clock: h.Clock}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	all, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if bytes.Contains(all, []byte(marker)) {
		t.Fatal("the archive contains end-to-end-encrypted plaintext")
	}
}

// The manifest names every member after itself, in tar order, and Verify
// returns the same manifest Backup did.
func TestTheManifestListsEveryMemberInOrder(t *testing.T) {
	h := newOpsHarness(t)
	h.SeedRows(3)
	var buf bytes.Buffer
	man, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &buf, IncludeBlobs: true, Clock: h.Clock, Label: "nightly"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	names := tarNames(t, buf.Bytes())
	if len(man.Entries) != len(names)-1 {
		t.Fatalf("manifest lists %d entries for %d members after itself", len(man.Entries), len(names)-1)
	}
	for i, e := range man.Entries {
		if e.Path != names[i+1] {
			t.Fatalf("entry %d names %q, member is %q", i, e.Path, names[i+1])
		}
	}
	if man.BlobCount != 2 {
		t.Fatalf("blob_count = %d, want the harness's 2", man.BlobCount)
	}
	blobs := names[len(names)-2:]
	if !slices.IsSorted(blobs) || !strings.HasPrefix(blobs[0], "dilla-backup/blobs/") {
		t.Fatalf("the blob members are not last and sorted: %v", blobs)
	}
	if man.Generation != h.Generation(t) || man.FormatVersion != ops.FormatVersion || man.Label != "nightly" {
		t.Fatalf("manifest = %+v", man)
	}
	if man.CreatedAt != h.Clock.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("created_at = %q, want the instance clock's %q", man.CreatedAt, h.Clock.Now().UTC().Format(time.RFC3339))
	}
	got, err := ops.Verify(t.Context(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !slices.Equal(got.Entries, man.Entries) || got.Generation != man.Generation {
		t.Fatalf("Verify returned %+v, Backup %+v", got, man)
	}
}

// R38: the archive's own README states what a backup holds, verbatim.
func TestTheArchiveREADMEStatesWhatABackupHolds(t *testing.T) {
	h := newOpsHarness(t)
	var buf bytes.Buffer
	if _, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &buf, Clock: h.Clock}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	readme := tarMember(t, buf.Bytes(), "dilla-backup/README.txt")
	const r38 = "backups hold no end-to-end-encrypted plaintext: they contain ciphertext, " +
		"server-readable channel content, revealed report envelopes, TLS material and the instance keys"
	if ops.ContentNotice != r38 {
		t.Fatalf("ContentNotice = %q, want R38's wording", ops.ContentNotice)
	}
	if !strings.Contains(string(readme), r38) {
		t.Fatalf("README does not carry R38's wording:\n%s", readme)
	}
}

// keys/instance.json carries the instance keys and the ACME account key.
func TestTheKeysMemberCarriesTheInstanceKeysAndTheACMEAccount(t *testing.T) {
	h := newOpsHarness(t)
	var buf bytes.Buffer
	if _, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &buf, Clock: h.Clock}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	var doc struct {
		Generation      uint64 `json:"generation"`
		FrankingKeyID   string `json:"franking_key_id"`
		KeyHistory      []byte `json:"key_history"`
		ACMEAccountKeys []struct {
			Path string `json:"path"`
			PEM  string `json:"pem"`
		} `json:"acme_account_keys"`
	}
	if err := json.Unmarshal(tarMember(t, buf.Bytes(), "dilla-backup/keys/instance.json"), &doc); err != nil {
		t.Fatalf("keys/instance.json: %v", err)
	}
	row := h.Instance(t)
	if doc.Generation != row.Generation || doc.FrankingKeyID != row.FrankingKeyID.String() ||
		!bytes.Equal(doc.KeyHistory, row.KeyHistory) {
		t.Fatalf("keys/instance.json = %+v, instance row = %+v", doc, row)
	}
	if len(doc.ACMEAccountKeys) != 1 || doc.ACMEAccountKeys[0].PEM != h.ACMEKey {
		t.Fatalf("acme_account_keys = %+v, want the harness's one key", doc.ACMEAccountKeys)
	}
}

// A blob the snapshot names but the blob directory lost is a gap: the backup
// refuses, unless --allow-gaps, and then the manifest names it.
func TestAMissingBlobIsAGapUnlessAllowed(t *testing.T) {
	h := newOpsHarness(t)
	lost := h.LoseOneBlob()
	var buf bytes.Buffer
	_, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &buf, IncludeBlobs: true, Clock: h.Clock})
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Data || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("Backup with a lost blob = %v, want an exit.Data gap refusal", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a refused backup wrote %d bytes", buf.Len())
	}
	man, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &buf, IncludeBlobs: true, AllowGaps: true, Clock: h.Clock})
	if err != nil {
		t.Fatalf("Backup --allow-gaps: %v", err)
	}
	if len(man.Gaps) != 1 || man.Gaps[0] != lost || man.BlobCount != 1 {
		t.Fatalf("gaps = %v, blob_count = %d; want [%s] and 1", man.Gaps, man.BlobCount, lost)
	}
	if _, err := ops.Verify(t.Context(), bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Verify of an archive with a named gap: %v", err)
	}
}

// Backup writes last_backup_started before the snapshot, so the snapshot
// holds it, and clears it when it is done, so the sweeper is pinned only while
// a backup is in flight.
func TestBackupPinsTheSweeperOnlyWhileInFlight(t *testing.T) {
	h := newOpsHarness(t)
	var buf bytes.Buffer
	if _, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &buf, Clock: h.Clock}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got := h.ArchivedSetting(t, buf.Bytes(), "last_backup_started"); got != "1790000000" {
		t.Fatalf("the snapshot's last_backup_started = %q, want the backup's start", got)
	}
	if v, err := h.Repo.GetSetting(t.Context(), "last_backup_started"); err != nil || len(v) != 0 {
		t.Fatalf("last_backup_started after the backup = %q, %v; want it cleared", v, err)
	}
}

func TestBackupRefusesASchemaNewerThanTheBinary(t *testing.T) {
	h := newOpsHarness(t)
	h.ForgeSchemaVersion(t, 1_000_000)
	var buf bytes.Buffer
	_, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &buf, Clock: h.Clock})
	var code exit.Code
	if !errors.As(err, &code) || code != exit.Config {
		t.Fatalf("Backup of a newer schema = %v, want exit.Config", err)
	}
	if !strings.Contains(err.Error(), "1000000") {
		t.Fatalf("error %q does not name the database's version", err)
	}
}

// A backup takes the SHARED data-directory lock: it runs beside another
// backup, and it refuses while restore (or anything else) holds it exclusively.
func TestBackupRefusesWhileTheDirectoryIsHeldExclusively(t *testing.T) {
	h := newOpsHarness(t)
	shared, err := ops.AcquireSharedLock(h.Cfg.Instance.DataDir)
	if err != nil {
		t.Fatalf("AcquireSharedLock: %v", err)
	}
	var buf bytes.Buffer
	if _, err := ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: &buf, Clock: h.Clock}); err != nil {
		t.Fatalf("Backup beside another shared holder: %v", err)
	}
	if err := shared.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l, err := ops.AcquireLock(h.Cfg.Instance.DataDir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer func() { _ = l.Release() }()
	_, err = ops.Backup(t.Context(), h.Cfg, h.Repo, ops.BackupOptions{Out: io.Discard, Clock: h.Clock})
	var code exit.Code
	if !errors.Is(err, ops.ErrLocked) || !errors.As(err, &code) || code != exit.TempFail {
		t.Fatalf("Backup under an exclusive lock = %v, want ErrLocked with exit.TempFail", err)
	}
}

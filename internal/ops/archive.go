// Package ops holds dillad's operator-facing archive and diagnostics. The
// archive format is fixed: entry order is part of it, because that is what makes
// restore streamable and two backups of an unchanged instance comparable.
//
//	dilla-backup/
//	  MANIFEST.json            first member, always; every other member's size and sha256
//	  db/dilla.sqlite          VACUUM INTO output (SQLite), or
//	  db/dilla.dump            store.DumpPostgres's DILLADMP container (Postgres)
//	  config/dilla.toml
//	  keys/instance.json       the instance keys and the ACME account key(s)
//	  README.txt               what the archive is and what it holds (R38)
//	  blobs/<aa>/<bb>/<hex>    byte order of the full path, which is blob_id order
package ops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/store"
	postgresmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
	sqlitemigrations "github.com/dilla-chat/dilla/internal/store/sqlite/migrations"
)

// FormatVersion is the archive layout's version, recorded in the manifest and
// in every member's PAX header.
const FormatVersion = 1

// ContentNotice is R38's wording (D15), verbatim: the archive's README and
// `dillad backup`'s output both carry it.
const ContentNotice = "backups hold no end-to-end-encrypted plaintext: they contain ciphertext, " +
	"server-readable channel content, revealed report envelopes, TLS material and the instance keys"

// The member names. root prefixes every one of them.
const (
	root           = "dilla-backup/"
	manifestMember = root + "MANIFEST.json"
	sqliteMember   = root + "db/dilla.sqlite"
	postgresMember = root + "db/dilla.dump"
	configMember   = root + "config/dilla.toml"
	keysMember     = root + "keys/instance.json"
	readmeMember   = root + "README.txt"
	blobsPrefix    = root + "blobs/"
)

// paxFormatKey is a vendor PAX record every header carries. Without one,
// archive/tar writes a FormatPAX header that needs no records as a plain USTAR
// block, and a reader reports it as USTAR: the record is what makes every
// member PAX on disk, not only in the writer's intent.
const paxFormatKey = "DILLA.format_version"

// maxManifestBytes bounds the manifest Verify reads into memory. A manifest
// entry is about 200 bytes, so this is several hundred thousand blobs.
const maxManifestBytes = 256 << 20

// blobPage is how many blob rows one ListBlobs call returns.
const blobPage = 256

// Manifest is MANIFEST.json. Entries covers every member after the manifest,
// in tar order. Label and Gaps are omitted when empty.
type Manifest struct {
	FormatVersion int     `json:"format_version"`
	DillaVersion  string  `json:"dilla_version"`
	SchemaVersion int64   `json:"schema_version"`
	Engine        string  `json:"engine"`
	Generation    uint64  `json:"generation"`
	CreatedAt     string  `json:"created_at"`
	BlobCount     int     `json:"blob_count"`
	TotalBytes    int64   `json:"total_bytes"`
	Entries       []Entry `json:"entries"`
	// Label is the operator's --label.
	Label string `json:"label,omitempty"`
	// Gaps names, by the member path it would have had, every blob the
	// snapshot references whose file was missing (only with --allow-gaps).
	Gaps []string `json:"gaps,omitempty"`
}

// Entry is one member's path, size and SHA-256 (lower-case hex).
type Entry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// BackupOptions is `dillad backup`'s flags plus what the caller supplies.
type BackupOptions struct {
	// Out receives the gzip stream. Nothing is written to it before every
	// check has passed, so a refused backup leaves it empty.
	Out          io.Writer
	IncludeBlobs bool
	Label        string
	AllowGaps    bool
	// Clock stamps created_at and last_backup_started; nil is the system clock.
	Clock clock.Clock
	// ConfigPath, when set, is archived as config/dilla.toml byte for byte, so
	// the operator's comments survive; otherwise cfg is re-encoded.
	ConfigPath string
	// DillaVersion is recorded as the manifest's dilla_version.
	DillaVersion string
}

// epoch is the ModTime every header carries. FileInfoHeader would copy the
// host's mtime, uid and gid into the archive; none of that is instance state.
var epoch = time.Unix(0, 0).UTC()

// header is the one fully deterministic tar header. Format is set explicitly to
// PAX: left unspecified, archive/tar picks the first of USTAR, PAX, GNU that
// can encode the header, so the format changes silently the first time a path
// exceeds 100 bytes.
func header(name string, mode, size int64) *tar.Header {
	return &tar.Header{
		Typeflag:   tar.TypeReg,
		Name:       name,
		Size:       size,
		Mode:       mode,
		ModTime:    epoch,
		Uid:        0,
		Gid:        0,
		Uname:      "",
		Gname:      "",
		Format:     tar.FormatPAX,
		PAXRecords: map[string]string{paxFormatKey: strconv.Itoa(FormatVersion)},
	}
}

// writeEntry appends one member whose bytes the process already holds.
func writeEntry(tw *tar.Writer, name string, mode int64, body []byte) (Entry, error) {
	if err := tw.WriteHeader(header(name, mode, int64(len(body)))); err != nil {
		return Entry{}, err
	}
	if _, err := tw.Write(body); err != nil {
		return Entry{}, err
	}
	sum := sha256.Sum256(body)
	return Entry{Path: name, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}, nil
}

// writeEntryFrom writes one member by copying size bytes from r, tee-ing them
// through SHA-256 as they go. The digest it returns is of the bytes actually
// written, so a file that changed between the two passes is caught rather than
// silently archived under a stale digest.
func writeEntryFrom(tw *tar.Writer, name string, mode, size int64, r io.Reader) (Entry, error) {
	if err := tw.WriteHeader(header(name, mode, size)); err != nil {
		return Entry{}, err
	}
	h := sha256.New()
	n, err := io.Copy(tw, io.TeeReader(io.LimitReader(r, size), h))
	if err != nil {
		return Entry{}, err
	}
	if n != size {
		return Entry{}, fmt.Errorf("ops: %s: wrote %d bytes, header says %d", name, n, size)
	}
	return Entry{Path: name, Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// member is one planned member: pass one fills in its size and digest, pass
// two writes it. body holds a small member's bytes; a large one is opened.
type member struct {
	Entry
	mode int64
	body []byte
	open func() (io.ReadCloser, error)
}

// HighestMigration is the highest goose version embedded in this binary for
// engine ("sqlite" or "postgres"), read from the migration file names.
func HighestMigration(engine string) (int64, error) {
	var fsys fs.ReadDirFS
	switch engine {
	case "sqlite":
		fsys = sqlitemigrations.FS
	case "postgres":
		fsys = postgresmigrations.FS
	default:
		return 0, fmt.Errorf("ops: unknown engine %q", engine)
	}
	entries, err := fsys.ReadDir(".")
	if err != nil {
		return 0, err
	}
	var highest int64
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			continue
		}
		highest = max(highest, v)
	}
	if highest == 0 {
		return 0, fmt.Errorf("ops: no embedded %s migrations", engine)
	}
	return highest, nil
}

// Backup writes the archive of the instance cfg and repo describe to o.Out and
// returns its manifest. The order is load-bearing:
//
//  1. take the SHARED data-directory lock: a second backup may run beside this
//     one, a restore may not;
//  2. refuse a database whose schema is newer than this binary (exit.Config);
//  3. write last_backup_started, which pins the blob sweeper, and clear it on
//     the way out;
//  4. take the database snapshot and read generation and the blob list from
//     it (SQLite reads them from the VACUUM INTO copy itself; Postgres reads the
//     generation before and after the dump and refuses if it moved, and skips a
//     blob created after the dump began);
//  5. pass one: size and digest every member, and count gaps (exit.Data unless
//     AllowGaps) — nothing has been written to o.Out yet;
//  6. pass two: write MANIFEST.json first and then every member, through gzip.
func Backup(ctx context.Context, cfg *config.Config, repo store.Repository, o BackupOptions) (Manifest, error) {
	if o.Out == nil {
		return Manifest{}, fmt.Errorf("ops: backup: no output writer: %w", exit.Software)
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.System()
	}
	lock, err := AcquireSharedLock(cfg.Instance.DataDir)
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return Manifest{}, fmt.Errorf("ops: backup: %w: %w", err, exit.TempFail)
		}
		return Manifest{}, fmt.Errorf("ops: backup: %w: %w", err, exit.CantCreate)
	}
	defer func() { _ = lock.Release() }()

	engine := cfg.DB.Driver
	highest, err := HighestMigration(engine)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", err, exit.Config)
	}
	live, err := repo.SchemaVersion(ctx)
	if err != nil {
		return Manifest{}, fmt.Errorf("ops: backup: read schema version: %w: %w", err, exit.Unavailable)
	}
	if live > highest {
		return Manifest{}, fmt.Errorf("ops: backup: database schema %d is newer than this binary's highest migration %d: refusing to back it up: %w",
			live, highest, exit.Config)
	}

	started := clk.Now()
	mark := []byte(strconv.FormatInt(started.Unix(), 10))
	if err := repo.PutSetting(ctx, blob.LastBackupStartedKey, mark, started.Unix()); err != nil {
		return Manifest{}, fmt.Errorf("ops: backup: write %s: %w: %w", blob.LastBackupStartedKey, err, exit.Unavailable)
	}
	defer clearMark(ctx, repo, mark, clk)

	work, err := os.MkdirTemp(cfg.Instance.DataDir, ".backup-")
	if err != nil {
		return Manifest{}, fmt.Errorf("ops: backup: %w: %w", err, exit.CantCreate)
	}
	defer func() { _ = os.RemoveAll(work) }()

	snap, err := snapshot(ctx, cfg, repo, work, started)
	if err != nil {
		return Manifest{}, err
	}
	defer snap.close()
	if snap.schema > highest {
		return Manifest{}, fmt.Errorf("ops: backup: database schema %d is newer than this binary's highest migration %d: refusing to back it up: %w",
			snap.schema, highest, exit.Config)
	}

	members, err := plan(ctx, cfg, o, snap)
	if err != nil {
		return Manifest{}, err
	}
	var gaps []string
	var blobMembers []member
	if o.IncludeBlobs {
		// Open for the whole of both passes: pass two reads every file again.
		bs, err := blob.Open(cfg.Blobs.Dir, cfg.Blobs.Backend)
		if err != nil {
			return Manifest{}, fmt.Errorf("ops: backup: open the blob store %s: %w: %w", cfg.Blobs.Dir, err, exit.IOErr)
		}
		defer func() { _ = bs.Close() }()
		blobMembers, gaps, err = planBlobs(ctx, bs, snap)
		if err != nil {
			return Manifest{}, err
		}
		if len(gaps) > 0 && !o.AllowGaps {
			return Manifest{}, fmt.Errorf("ops: backup: %d referenced blob(s) are missing from %s, a gap in the archive (first: %s); "+
				"run again with --allow-gaps to archive without them: %w", len(gaps), cfg.Blobs.Dir, gaps[0], exit.Data)
		}
		members = append(members, blobMembers...)
	}

	man := Manifest{
		FormatVersion: FormatVersion,
		DillaVersion:  o.DillaVersion,
		SchemaVersion: snap.schema,
		Engine:        engine,
		Generation:    snap.instance.Generation,
		CreatedAt:     started.UTC().Format(time.RFC3339),
		BlobCount:     len(blobMembers),
		Entries:       make([]Entry, 0, len(members)),
		Label:         o.Label,
		Gaps:          gaps,
	}
	for _, m := range members {
		man.Entries = append(man.Entries, m.Entry)
		man.TotalBytes += m.Size
	}
	body, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return Manifest{}, fmt.Errorf("ops: backup: manifest: %w: %w", err, exit.Software)
	}
	body = append(body, '\n')

	if err := write(ctx, o.Out, body, members); err != nil {
		return Manifest{}, err
	}
	return man, nil
}

// clearMark empties last_backup_started when it still holds this backup's
// start, so the sweeper is pinned only while a backup is in flight. It runs on
// every return path, cancelled context included; a failure leaves the pin in
// place, which delays collection and loses nothing.
func clearMark(ctx context.Context, repo store.Repository, mark []byte, clk clock.Clock) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if cur, err := repo.GetSetting(ctx, blob.LastBackupStartedKey); err != nil || !bytes.Equal(cur, mark) {
		return
	}
	_ = repo.PutSetting(ctx, blob.LastBackupStartedKey, []byte{}, clk.Now().Unix())
}

// snap is one consistent database snapshot on disk plus what was read from it.
type snap struct {
	member   string // db/dilla.sqlite or db/dilla.dump
	path     string
	schema   int64
	instance store.InstanceRow
	// blobs lists the snapshot's blob rows in blob_id order; after is the
	// resume key.
	blobs func(ctx context.Context, after []byte) ([]store.BlobRow, error)
	// createdBefore, when non-zero, skips a blob row created after the dump
	// began (Postgres only: its blob list is read live, not from the dump).
	createdBefore int64
	close         func()
}

func snapshot(ctx context.Context, cfg *config.Config, repo store.Repository, work string, started time.Time) (*snap, error) {
	switch cfg.DB.Driver {
	case "sqlite":
		return snapshotSQLite(ctx, cfg, work)
	case "postgres":
		return snapshotPostgres(ctx, cfg, repo, work, started)
	default:
		return nil, fmt.Errorf("ops: backup: db.driver %q is neither sqlite nor postgres: %w", cfg.DB.Driver, exit.Config)
	}
}

// snapshotSQLite runs VACUUM INTO on a read-only connection of its own (it
// takes a read lock, not the writer's, and is consistent with WAL on;
// facts-storage §4.1), then opens the copy and reads the generation, the schema
// version and the blob list from it, so all of them are the snapshot's.
func snapshotSQLite(ctx context.Context, cfg *config.Config, work string) (*snap, error) {
	out := filepath.Join(work, "dilla.sqlite")
	src, err := sqlite.OpenRead(cfg.DB.Path)
	if err != nil {
		return nil, fmt.Errorf("ops: backup: %w: %w", err, exit.Unavailable)
	}
	err = store.VacuumInto(ctx, src, out)
	_ = src.Close()
	if err != nil {
		return nil, fmt.Errorf("ops: backup: snapshot: %w: %w", err, exit.IOErr)
	}
	read, err := sqlite.OpenRead(out)
	if err != nil {
		return nil, fmt.Errorf("ops: backup: open the snapshot: %w: %w", err, exit.IOErr)
	}
	copyRepo := sqlite.New(nil, read)
	s := &snap{member: sqliteMember, path: out, close: func() { _ = copyRepo.Close() }}
	if s.schema, err = copyRepo.SchemaVersion(ctx); err != nil {
		s.close()
		return nil, fmt.Errorf("ops: backup: the snapshot's schema version: %w: %w", err, exit.Data)
	}
	if s.instance, err = copyRepo.GetInstance(ctx); err != nil {
		s.close()
		return nil, fmt.Errorf("ops: backup: the snapshot's instance row: %w: %w", err, exit.Data)
	}
	s.blobs = func(ctx context.Context, after []byte) ([]store.BlobRow, error) {
		return copyRepo.ListBlobs(ctx, after, blobPage)
	}
	return s, nil
}

// snapshotPostgres runs store.DumpPostgres as shipped: it opens its own
// connection from the DSN and runs every COPY TO inside one repeatable-read
// read-only transaction. The generation cannot be read inside that
// transaction, so it is read before and after, and a dump it moved across is
// refused; only restore moves it, and restore takes the exclusive lock this
// backup's shared one excludes.
func snapshotPostgres(ctx context.Context, cfg *config.Config, repo store.Repository, work string, started time.Time) (*snap, error) {
	before, err := repo.GetInstance(ctx)
	if err != nil {
		return nil, fmt.Errorf("ops: backup: read the instance row: %w: %w", err, exit.Unavailable)
	}
	schema, err := repo.SchemaVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("ops: backup: read schema version: %w: %w", err, exit.Unavailable)
	}
	out := filepath.Join(work, "dilla.dump")
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a file in this backup's own temp directory
	if err != nil {
		return nil, fmt.Errorf("ops: backup: %w: %w", err, exit.CantCreate)
	}
	err = store.DumpPostgres(ctx, cfg.DB.DSN, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("ops: backup: snapshot: %w: %w", err, exit.Unavailable)
	}
	after, err := repo.GetInstance(ctx)
	if err != nil {
		return nil, fmt.Errorf("ops: backup: read the instance row: %w: %w", err, exit.Unavailable)
	}
	if after.Generation != before.Generation {
		return nil, fmt.Errorf("ops: backup: the instance generation moved from %d to %d during the dump: %w",
			before.Generation, after.Generation, exit.TempFail)
	}
	return &snap{
		member: postgresMember, path: out, schema: schema, instance: before,
		blobs: func(ctx context.Context, after []byte) ([]store.BlobRow, error) {
			return repo.ListBlobs(ctx, after, blobPage)
		},
		createdBefore: started.Unix() + 1,
		close:         func() {},
	}, nil
}

// plan is pass one over the fixed, non-blob members.
func plan(ctx context.Context, cfg *config.Config, o BackupOptions, s *snap) ([]member, error) {
	dbSize, dbSum, err := hashFile(ctx, s.path)
	if err != nil {
		return nil, fmt.Errorf("ops: backup: read the snapshot: %w: %w", err, exit.IOErr)
	}
	dbPath := s.path
	members := []member{{
		Entry: Entry{Path: s.member, Size: dbSize, SHA256: dbSum}, mode: 0o600,
		open: func() (io.ReadCloser, error) { return os.Open(dbPath) }, //nolint:gosec // G304: this backup's own snapshot file
	}}

	var conf []byte
	if o.ConfigPath != "" {
		if conf, err = os.ReadFile(o.ConfigPath); err != nil {
			return nil, fmt.Errorf("ops: backup: read %s: %w: %w", o.ConfigPath, err, exit.Config)
		}
	} else {
		var buf bytes.Buffer
		if err := cfg.WriteConfig(&buf); err != nil {
			return nil, fmt.Errorf("ops: backup: %w: %w", err, exit.Software)
		}
		conf = buf.Bytes()
	}
	keys, err := instanceKeys(cfg, s.instance)
	if err != nil {
		return nil, err
	}
	for _, m := range []struct {
		name string
		body []byte
	}{{configMember, conf}, {keysMember, keys}, {readmeMember, []byte(readme)}} {
		sum := sha256.Sum256(m.body)
		members = append(members, member{
			Entry: Entry{Path: m.name, Size: int64(len(m.body)), SHA256: hex.EncodeToString(sum[:])},
			mode:  0o600, body: m.body,
		})
	}
	return members, nil
}

// planBlobs walks the snapshot's blob rows and stats each file. A blob's
// SHA-256 is its id, so pass one needs no read of the bytes; pass two hashes
// what it writes and refuses a file that does not match. A missing file is a
// gap when the snapshot holds a reference to it; an unreferenced blob (its
// unref_since set) is garbage awaiting the sweeper, and its absence is not a
// gap.
func planBlobs(ctx context.Context, bs *blob.Store, s *snap) ([]member, []string, error) {
	var members []member
	var gaps []string
	var after []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		rows, err := s.blobs(ctx, after)
		if err != nil {
			return nil, nil, fmt.Errorf("ops: backup: list blobs: %w: %w", err, exit.Unavailable)
		}
		if len(rows) == 0 {
			break
		}
		after = rows[len(rows)-1].BlobID
		for _, row := range rows {
			if s.createdBefore != 0 && row.Created >= s.createdBefore {
				continue // created after the dump began: the dump does not name it
			}
			name := blobMember(row.BlobID)
			size, err := bs.Stat(row.BlobID)
			if errors.Is(err, blob.ErrNotFound) {
				if row.UnrefSince == nil {
					gaps = append(gaps, name)
				}
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("ops: backup: stat %s: %w: %w", name, err, exit.IOErr)
			}
			id := row.BlobID
			members = append(members, member{
				Entry: Entry{Path: name, Size: size, SHA256: hex.EncodeToString(id)}, mode: 0o644,
				open: func() (io.ReadCloser, error) {
					f, _, err := bs.Get(id)
					if err != nil {
						return nil, err
					}
					return f, nil
				},
			})
		}
	}
	slices.SortFunc(members, func(a, b member) int { return strings.Compare(a.Path, b.Path) })
	return members, gaps, nil
}

func blobMember(blobID []byte) string {
	h := hex.EncodeToString(blobID)
	return blobsPrefix + path.Join(h[0:2], h[2:4], h)
}

// write is pass two: gzip, MANIFEST.json first, then every member in plan
// order, each checked against the digest pass one recorded.
func write(ctx context.Context, out io.Writer, manifest []byte, members []member) error {
	gz, err := gzip.NewWriterLevel(out, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("ops: backup: %w: %w", err, exit.Software)
	}
	// All four non-OS header fields are written into the stream; zeroed, two
	// archives of the same instance are byte-identical.
	gz.Name, gz.ModTime, gz.Comment, gz.Extra = "", time.Time{}, "", nil
	tw := tar.NewWriter(gz)
	fail := func(e error) error { return fmt.Errorf("ops: backup: write the archive: %w: %w", e, exit.IOErr) }

	if _, err := writeEntry(tw, manifestMember, 0o600, manifest); err != nil {
		return fail(err)
	}
	for _, m := range members {
		if err := ctx.Err(); err != nil {
			return err
		}
		var got Entry
		if m.body != nil {
			got, err = writeEntry(tw, m.Path, m.mode, m.body)
		} else {
			got, err = copyMember(tw, m)
		}
		if err != nil {
			return fail(err)
		}
		if got != m.Entry {
			return fmt.Errorf("ops: backup: %s changed between the two passes (sha256 %s, planned %s): %w",
				m.Path, got.SHA256, m.SHA256, exit.TempFail)
		}
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	return nil
}

func copyMember(tw *tar.Writer, m member) (Entry, error) {
	rc, err := m.open()
	if err != nil {
		return Entry{}, err
	}
	got, err := writeEntryFrom(tw, m.Path, m.mode, m.Size, rc)
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	return got, err
}

func hashFile(ctx context.Context, p string) (int64, string, error) {
	f, err := os.Open(p) //nolint:gosec // G304: this backup's own snapshot file
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, readerCtx{ctx, f})
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// countReader counts what is read through it, for the "short member" message,
// and keeps the first read error, so a sink that fails because the archive was
// short is reported as the damaged archive it is (exit.Data), not as the
// sink's own failure.
type countReader struct {
	r   io.Reader
	n   int64
	err error
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && !errors.Is(err, io.EOF) && c.err == nil {
		c.err = err
	}
	return n, err
}

// readerCtx stops a long copy when ctx is done.
type readerCtx struct {
	ctx context.Context
	r   io.Reader
}

func (r readerCtx) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// keysDoc is keys/instance.json: the instance row's key material, and every
// ACME account key certmagic stored under tls.storage_dir.
type keysDoc struct {
	InstanceID          string    `json:"instance_id"`
	ExternalSenderKeyID string    `json:"external_sender_key_id"`
	FrankingKeyID       string    `json:"franking_key_id"`
	Generation          uint64    `json:"generation"`
	PolicyVersion       uint64    `json:"policy_version"`
	Created             int64     `json:"created"`
	KeyHistory          []byte    `json:"key_history"` // protocol/03 § Instance keys, CBOR, base64 here
	ACMEAccountKeys     []acmeKey `json:"acme_account_keys"`
}

type acmeKey struct {
	Path string `json:"path"` // relative to tls.storage_dir
	PEM  string `json:"pem"`
}

// instanceKeys renders keys/instance.json. certmagic v0.25.4 stores an ACME
// account's private key at acme/<issuer>/users/<email>/<username>.key under its
// storage root (account.go, storageKeyUserPrivateKey).
func instanceKeys(cfg *config.Config, in store.InstanceRow) ([]byte, error) {
	doc := keysDoc{
		InstanceID:          in.InstanceID.String(),
		ExternalSenderKeyID: in.ExternalSenderKeyID.String(),
		FrankingKeyID:       in.FrankingKeyID.String(),
		Generation:          in.Generation,
		PolicyVersion:       in.PolicyVersion,
		Created:             in.Created,
		KeyHistory:          in.KeyHistory,
		ACMEAccountKeys:     []acmeKey{},
	}
	if cfg.TLS.StorageDir != "" {
		paths, err := filepath.Glob(filepath.Join(cfg.TLS.StorageDir, "acme", "*", "users", "*", "*.key"))
		if err != nil {
			return nil, fmt.Errorf("ops: backup: %w: %w", err, exit.Config)
		}
		slices.Sort(paths)
		for _, p := range paths {
			pem, err := os.ReadFile(p) //nolint:gosec // G304: a file under the configured tls.storage_dir
			if err != nil {
				return nil, fmt.Errorf("ops: backup: read %s: %w: %w", p, err, exit.IOErr)
			}
			rel, err := filepath.Rel(cfg.TLS.StorageDir, p)
			if err != nil {
				return nil, fmt.Errorf("ops: backup: %w: %w", err, exit.Software)
			}
			doc.ACMEAccountKeys = append(doc.ACMEAccountKeys, acmeKey{Path: filepath.ToSlash(rel), PEM: string(pem)})
		}
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("ops: backup: %w: %w", err, exit.Software)
	}
	return append(body, '\n'), nil
}

// readme is README.txt. R38's sentence is on one line of its own, so a search
// for it finds it.
const readme = `dilla instance backup archive (format 1)

Written by ` + "`dillad backup`" + `. MANIFEST.json, the first member, lists every
other member in order with its size and SHA-256. Check an archive with
` + "`dillad backup verify --from=PATH`" + ` and restore it with
` + "`dillad restore --from=PATH`" + `.

Note: ` + ContentNotice + `.

Keep it as you keep the instance itself: keys/instance.json holds the
instance's private keys, and db/ holds every account and server-readable
channel.
`

// Verify re-reads an archive end to end: MANIFEST.json must be member 0, its
// format and schema no newer than this binary, and every other member present
// in the manifest's order with the manifest's size and SHA-256, and nothing
// after the last. Every refusal carries exit.Data.
func Verify(ctx context.Context, r io.Reader) (Manifest, error) {
	return readArchive(ctx, r, nil, nil)
}

// readArchive is Verify's one reader, which restore shares. check, when set,
// sees the manifest before any member and before Verify's own format and schema
// checks, so restore refuses a newer schema with its own exit code. sink, when
// set, is handed every member after the manifest as it streams past; the bytes
// it reads are the bytes hashed, and whatever it leaves unread is drained and
// hashed too, so a sink never weakens the check. A sink that writes files must
// treat them as provisional until readArchive returns nil: the digest of a
// member is compared only once the member has been read to its end.
func readArchive(ctx context.Context, r io.Reader, check func(Manifest) error, sink func(Entry, io.Reader) error) (Manifest, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("ops: verify: %s: %w", fmt.Sprintf(format, args...), exit.Data)
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Manifest{}, bad("not a gzip stream: %v", err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		return Manifest{}, bad("no MANIFEST.json: the archive is empty or short: %v", err)
	}
	if hdr.Name != manifestMember {
		return Manifest{}, bad("member 0 is %q, want %q", hdr.Name, manifestMember)
	}
	if hdr.Size > maxManifestBytes {
		return Manifest{}, bad("MANIFEST.json is %d bytes, over the %d-byte bound", hdr.Size, maxManifestBytes)
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		return Manifest{}, bad("MANIFEST.json is short: %v", err)
	}
	var man Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		return Manifest{}, bad("MANIFEST.json: %v", err)
	}
	if man.FormatVersion != FormatVersion {
		return Manifest{}, bad("format_version %d, this binary reads %d", man.FormatVersion, FormatVersion)
	}
	if check != nil {
		if err := check(man); err != nil {
			return Manifest{}, err
		}
	}
	highest, err := HighestMigration(man.Engine)
	if err != nil {
		return Manifest{}, bad("engine %q: %v", man.Engine, err)
	}
	if man.SchemaVersion > highest {
		return Manifest{}, bad("schema_version %d is newer than this binary's highest migration %d", man.SchemaVersion, highest)
	}
	for i, e := range man.Entries {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		hdr, err := tr.Next()
		if err != nil {
			return Manifest{}, bad("the archive is short: it ends before member %d, %s: %v", i+1, e.Path, err)
		}
		if hdr.Name != e.Path {
			return Manifest{}, bad("member %d is %q, the manifest says %q", i+1, hdr.Name, e.Path)
		}
		if hdr.Size != e.Size {
			return Manifest{}, bad("member %s is %d bytes, the manifest says %d", e.Path, hdr.Size, e.Size)
		}
		h := sha256.New()
		counted := &countReader{r: io.TeeReader(tr, h)}
		if sink != nil {
			if err := sink(e, counted); err != nil {
				if counted.err != nil {
					return Manifest{}, bad("member %s is short: read %d of %d bytes: %v", e.Path, counted.n, e.Size, counted.err)
				}
				return Manifest{}, err
			}
		}
		// Streamed into a hash, never buffered: the member is bounded by its
		// header's size, which must already equal the manifest's.
		if _, err := io.Copy(io.Discard, counted); err != nil {
			return Manifest{}, bad("member %s is short: read %d of %d bytes: %v", e.Path, counted.n, e.Size, err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
			return Manifest{}, bad("member %s: sha256 mismatch: the archive holds %s, the manifest says %s", e.Path, got, e.SHA256)
		}
	}
	if hdr, err := tr.Next(); err == nil {
		return Manifest{}, bad("member %q is not in the manifest", hdr.Name)
	} else if !errors.Is(err, io.EOF) {
		return Manifest{}, bad("after the last member: %v", err)
	}
	// Drain the gzip stream so its trailer (CRC-32 and length) is checked.
	if _, err := io.Copy(io.Discard, gz); err != nil { //nolint:gosec // G110: discarded, never buffered; only the tar end padding remains
		return Manifest{}, bad("gzip trailer: %v", err)
	}
	return man, nil
}

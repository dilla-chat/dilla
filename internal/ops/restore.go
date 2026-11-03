package ops

import (
	"context"
	"crypto/rand"
	"database/sql"
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

	"github.com/pressly/goose/v3"

	"github.com/dilla-chat/dilla/internal/blob"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/config"
	"github.com/dilla-chat/dilla/internal/ds"
	"github.com/dilla-chat/dilla/internal/exit"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/store"
	"github.com/dilla-chat/dilla/internal/store/postgres"
	postgresmigrations "github.com/dilla-chat/dilla/internal/store/postgres/migrations"
	"github.com/dilla-chat/dilla/internal/store/sqlite"
)

// RestoreOptions is `dillad restore`'s flags plus what the caller supplies.
type RestoreOptions struct {
	// From is the archive, read once, front to back.
	From io.Reader
	// DryRun verifies the whole archive and returns the plan without writing
	// anything to the instance.
	DryRun bool
	// Force restores an archive the refusal rules would otherwise stop: one
	// written by a different instance (its instance id differs from the live
	// one's), or one whose manifest names gaps.
	Force bool
	// RemoveOld deletes the live entries once the restored ones are in place;
	// by default they are kept in <data_dir>/.old-<hex>.
	RemoveOld bool
	// Clock stamps the heal deadline and the end of every live call; nil is
	// the system clock.
	Clock clock.Clock
}

// RestorePlan is what a restore did, or on a dry run what it would do.
type RestorePlan struct {
	Manifest Manifest
	// RowCounts is the archived database's rows per table, by DumpTables name.
	RowCounts map[string]int64
	// BlobsToWrite is the number of blob members the archive carries.
	BlobsToWrite int
	// GenerationOld is the live instance's generation (the archive's when there
	// is no readable live instance); GenerationNew is the one the restored
	// instance runs at, above both the live and the archived one.
	GenerationOld uint64
	GenerationNew uint64
	// BinarySchema is this binary's highest migration: the restored database is
	// at Manifest.SchemaVersion until `serve` migrates it.
	BinarySchema int64
	// HealDeadline is when a group nobody healed is closed (unix seconds).
	HealDeadline int64
	// KeyPackagesPurged counts the non-last-resort KeyPackages removed.
	KeyPackagesPurged int64
	// OldDir is where the live entries were moved, <data_dir>/.old-<hex>;
	// empty on a dry run and with RemoveOld.
	OldDir string
	// ArchivedConfig is where the archive's dilla.toml was written, for the
	// operator to compare with the one in use; restore never replaces that.
	ArchivedConfig string
	// Warnings are things the operator must know that did not stop the restore.
	Warnings []string
}

// afterSwap is a test hook: it runs right after the restored entries are in
// place, while Restore still holds the lock.
var afterSwap func(dataDir string)

// swapRename is every rename the swap and its undo make; a test replaces it to
// make one fail.
var swapRename = os.Rename

// beforeHealCommit is a test hook: it runs as the heal transaction's last
// statement, and an error it returns rolls the transaction back.
var beforeHealCommit func() error

// maxKeysBytes bounds keys/instance.json, which Restore reads into memory.
const maxKeysBytes = 16 << 20

// restoredConfigName is the archived dilla.toml's name inside the restored
// data directory.
const restoredConfigName = "dilla.toml.restored"

// Restore replaces the instance cfg describes with the archive in o.From. The
// order is the contract:
//
//  1. take the EXCLUSIVE data-directory lock (exit.TempFail while a serve or a
//     backup holds it), and refuse while an earlier restore's interrupted swap
//     is unrepaired (exit.Data);
//  2. read the whole archive through Verify's reader before anything is
//     written to the instance: the manifest first (a newer schema or another
//     engine is exit.Config, a gap without Force is exit.Data), then every
//     member's size and SHA-256, staging each into <data_dir>/.restore-<hex>
//     as it streams past;
//  3. refuse an archive from another instance without Force (exit.Config), and
//     a SQLite database an earlier build of the branch created (exit.Data);
//  4. with DryRun, stop here: the staging directory is removed and the plan
//     returned;
//  5. in ONE transaction on the restored database: set the generation above
//     both the live and the archived one, mark every open group epoch-unknown
//     with the heal deadline, end every live call, purge the non-last-resort
//     KeyPackages, clear the backup pin and record the restore as pending for
//     the next start (ds.RestorePendingKey) — `dillad restore` has no delivery
//     service, so `serve` finishes it through ds.FinishRestore;
//  6. swap the data directory's entries (swap): every live one into
//     <data_dir>/.old-<hex>, every staged one into place. The lock file never
//     moves, so no second dillad can start against the directory at any point.
//
// The staged directory starts as hard links of every live file the archive does
// not own (issued certificates, the operator's dilla.toml, caches), so a restore
// loses nothing it did not carry. The restored SQLite database stays at the
// archive's schema: `serve` migrates it at start, after its pre-migration
// backup, which is the one migration path (R35).
func Restore(ctx context.Context, cfg *config.Config, o RestoreOptions) (RestorePlan, error) {
	if o.From == nil {
		return RestorePlan{}, fmt.Errorf("ops: restore: no archive reader: %w", exit.Software)
	}
	clk := o.Clock
	if clk == nil {
		clk = clock.System()
	}
	lay, err := newLayout(cfg)
	if err != nil {
		return RestorePlan{}, err
	}
	lock, err := AcquireLock(lay.dataDir)
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return RestorePlan{}, fmt.Errorf("ops: restore: the data directory %s is in use; stop dillad first (%w): %w",
				lay.dataDir, err, exit.TempFail)
		}
		return RestorePlan{}, fmt.Errorf("ops: restore: %w: %w", err, exit.CantCreate)
	}
	defer func() { _ = lock.Release() }()
	if err := InterruptedRestore(lay.dataDir); err != nil {
		return RestorePlan{}, err
	}

	highest, err := HighestMigration(cfg.DB.Driver)
	if err != nil {
		return RestorePlan{}, fmt.Errorf("ops: restore: %w: %w", err, exit.Config)
	}
	r := &restorer{cfg: cfg, o: o, lay: lay, now: clk.Now(), plan: RestorePlan{
		RowCounts: map[string]int64{}, BinarySchema: highest,
	}}
	r.stage = filepath.Join(lay.dataDir, stagePrefix+hex8())
	if err := os.Mkdir(r.stage, 0o700); err != nil {
		return RestorePlan{}, fmt.Errorf("ops: restore: %w: %w", err, exit.CantCreate)
	}
	defer func() {
		if !r.keepStage {
			_ = os.RemoveAll(r.stage)
		}
	}()
	if !o.DryRun {
		if err := linkTree(lay.dataDir, r.stage, lay.owned); err != nil {
			return RestorePlan{}, fmt.Errorf("ops: restore: carry the live files over: %w: %w", err, exit.IOErr)
		}
	}

	if err := r.extract(ctx, highest); err != nil {
		return RestorePlan{}, err
	}
	if err := r.inspect(ctx); err != nil {
		return RestorePlan{}, err
	}
	if o.DryRun {
		return r.plan, nil
	}
	if err := r.apply(ctx); err != nil {
		return RestorePlan{}, err
	}
	old, err := r.swap()
	if err != nil {
		if r.cfg.DB.Driver == "postgres" {
			err = fmt.Errorf("%w (the Postgres database already holds the archive; rerun the restore)", err)
		}
		return RestorePlan{}, err
	}
	if afterSwap != nil {
		afterSwap(lay.dataDir)
	}
	if err := r.installExternal(ctx); err != nil {
		return RestorePlan{}, err
	}
	r.plan.OldDir = old
	if o.RemoveOld {
		if err := os.RemoveAll(old); err != nil {
			r.plan.Warnings = append(r.plan.Warnings, fmt.Sprintf("could not remove %s: %v", old, err))
		} else {
			r.plan.OldDir = ""
		}
	}
	return r.plan, nil
}

// The names a restore uses inside the data directory. The staging directory and
// the directory the live files move to are both INSIDE it, never beside it: the
// packaged deployments make the data directory a mount point (the container's
// VOLUME, a Compose bind mount), which cannot be renamed, or the only writable
// path under a read-only /var/lib (systemd's StateDirectory= with
// ProtectSystem=strict), where a sibling cannot be created.
const (
	stagePrefix = ".restore-"
	oldPrefix   = ".old-"
	// RestoreMarkerFile exists only while a restore is swapping the data
	// directory's entries. Found at any other time, the swap was interrupted:
	// serve and restore both refuse to run until the operator has put the
	// directory back together, and the file says where each half is.
	RestoreMarkerFile = "RESTORE-IN-PROGRESS"
)

// InterruptedRestore reports, as an exit.Data error, a restore that stopped in
// the middle of its swap.
func InterruptedRestore(dataDir string) error {
	body, err := os.ReadFile(filepath.Join(dataDir, RestoreMarkerFile)) //nolint:gosec // G304: a fixed name in the operator's data directory
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ops: read %s: %w: %w", RestoreMarkerFile, err, exit.IOErr)
	}
	return fmt.Errorf("ops: a restore of %s was interrupted while it swapped the directory's entries (%s): "+
		"move every entry of the old directory back, remove the restore's staging directory and %s, and run the restore again: %w",
		dataDir, strings.TrimSpace(string(body)), RestoreMarkerFile, exit.Data)
}

// swap is step 6: every live entry moves into <data_dir>/.old-<hex>, then every
// staged entry moves into the data directory, the database's last. Each move is
// one rename on one file system; a move that fails undoes every earlier one. The
// lock file never moves, so the directory stays locked throughout, and the
// marker file names both halves while the swap runs.
func (r *restorer) swap() (string, error) {
	dataDir := r.lay.dataDir
	old := filepath.Join(dataDir, oldPrefix+hex8())
	fail := func(what string, err error) (string, error) {
		return "", fmt.Errorf("ops: restore: %s: %w: %w", what, err, exit.IOErr)
	}
	if err := os.Mkdir(old, 0o700); err != nil {
		return fail("create "+old, err)
	}
	marker := filepath.Join(dataDir, RestoreMarkerFile)
	note := fmt.Sprintf("the live entries are moving to %s and the restored ones from %s", old, r.stage)
	if err := writeNew(marker, strings.NewReader(note+"\n")); err != nil {
		_ = os.Remove(old)
		return fail("write "+marker, err)
	}
	type move struct{ from, to string }
	var moved []move
	// undo moves every entry back, the latest first. Only a complete undo
	// removes the marker: an entry that cannot move back leaves the directory
	// split between itself, <old> and the staging directory, so the marker
	// stays, gains a line per entry out of place, and keeps serve and restore
	// from running (InterruptedRestore) until the operator has repaired it.
	undo := func(what string, cause error) (string, error) {
		var stuck []string
		occupied := map[string]bool{} // data-directory paths a restored entry still holds
		for i := len(moved) - 1; i >= 0; i-- {
			m := moved[i]
			if occupied[m.from] {
				stuck = append(stuck, fmt.Sprintf("%s is still at %s: the restored entry could not leave %s", m.from, m.to, m.from))
				continue
			}
			if err := swapRename(m.to, m.from); err != nil {
				stuck = append(stuck, fmt.Sprintf("%s is still at %s: %v", m.from, m.to, err))
				if filepath.Dir(m.from) == r.stage {
					occupied[m.to] = true
				}
			}
		}
		if len(stuck) == 0 {
			_ = os.Remove(old)
			_ = os.Remove(marker)
			return fail(what, cause)
		}
		// The staging directory now holds half of what the operator puts back.
		r.keepStage = true
		repair := fmt.Sprintf("the swap failed (%s: %v) and its undo left these entries out of place:\n%s\n",
			what, cause, strings.Join(stuck, "\n"))
		recorded := ""
		if err := appendFile(marker, repair); err != nil {
			recorded = fmt.Sprintf(" (%s could not record them: %v)", marker, err)
		}
		return "", fmt.Errorf("ops: restore: %s: %w; undoing the swap left %d entries out of place, so %s needs manual repair "+
			"before dillad can start; %s names both halves%s: %s: %w",
			what, cause, len(stuck), dataDir, marker, recorded, strings.Join(stuck, "; "), exit.IOErr)
	}
	live, err := os.ReadDir(dataDir)
	if err != nil {
		return undo("list "+dataDir, err)
	}
	for _, e := range live {
		if swapExempt(e.Name()) || filepath.Join(dataDir, e.Name()) == old {
			continue
		}
		m := move{filepath.Join(dataDir, e.Name()), filepath.Join(old, e.Name())}
		if err := swapRename(m.from, m.to); err != nil {
			return undo("move "+m.from+" aside", err)
		}
		moved = append(moved, m)
	}
	staged, err := os.ReadDir(r.stage)
	if err != nil {
		return undo("list "+r.stage, err)
	}
	// The database's top-level entry goes last, so an interruption never leaves
	// a restored database beside the live directory's other files.
	dbTop := ""
	if rel, ok := r.lay.inside(r.lay.db); ok {
		dbTop = strings.SplitN(rel, string(filepath.Separator), 2)[0]
	}
	slices.SortStableFunc(staged, func(a, b fs.DirEntry) int {
		return cmpBool(a.Name() == dbTop, b.Name() == dbTop)
	})
	for _, e := range staged {
		if swapExempt(e.Name()) || e.Name() == ".external" {
			continue
		}
		m := move{filepath.Join(r.stage, e.Name()), filepath.Join(dataDir, e.Name())}
		if err := swapRename(m.from, m.to); err != nil {
			return undo("move "+m.from+" into place", err)
		}
		moved = append(moved, m)
	}
	if err := os.Remove(marker); err != nil {
		return fail("remove "+marker, err)
	}
	if d, err := os.Open(dataDir); err == nil { //nolint:gosec // G304: the operator's data directory, opened to sync it
		_ = d.Sync()
		_ = d.Close()
	}
	return old, nil
}

// swapExempt names the entries a swap never moves: the lock files (so the lock
// is held throughout), the marker, and the directories restores keep inside the
// data directory.
func swapExempt(name string) bool {
	return name == LockFile || name == ServeLockFile || name == RestoreMarkerFile ||
		strings.HasPrefix(name, stagePrefix) || strings.HasPrefix(name, oldPrefix)
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}

// layout maps the configured paths onto the data directory. A path inside it
// is restored through the directory swap; one outside it (a database or blob
// directory configured elsewhere) is staged under .external and installed
// after the swap.
type layout struct {
	dataDir string
	db      string // SQLite database file; "" for Postgres
	blobs   string
	tls     string
}

func newLayout(cfg *config.Config) (layout, error) {
	var l layout
	var err error
	abs := func(p string) string {
		if err != nil || p == "" {
			return ""
		}
		var a string
		a, err = filepath.Abs(p)
		return a
	}
	l.dataDir = abs(cfg.Instance.DataDir)
	if cfg.DB.Driver == "sqlite" {
		l.db = abs(cfg.DB.Path)
	}
	l.blobs = abs(cfg.Blobs.Dir)
	l.tls = abs(cfg.TLS.StorageDir)
	if err != nil {
		return layout{}, fmt.Errorf("ops: restore: %w: %w", err, exit.Config)
	}
	if l.dataDir == "" || l.dataDir == string(filepath.Separator) {
		return layout{}, fmt.Errorf("ops: restore: instance.data_dir %q cannot be swapped: %w", cfg.Instance.DataDir, exit.Config)
	}
	return l, nil
}

// inside is p's path relative to the data directory, when p is under it.
func (l layout) inside(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	rel, err := filepath.Rel(l.dataDir, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// staged is where p lives in the staging directory: under the same relative
// path when it is inside the data directory, under .external/<name> otherwise.
func (l layout) staged(stage, p, name string) string {
	if rel, ok := l.inside(p); ok {
		return filepath.Join(stage, rel)
	}
	return filepath.Join(stage, ".external", name)
}

// owned reports the live paths (relative to the data directory) the carry-over
// leaves out: the lock files, a backup's scratch directories, and the SQLite
// database with its WAL, shared-memory and journal files, which the archive
// replaces.
func (l layout) owned(rel string) bool {
	if rel == restoredConfigName || swapExempt(rel) {
		return true
	}
	if !strings.Contains(rel, string(filepath.Separator)) && strings.HasPrefix(rel, ".backup-") {
		return true
	}
	if db, ok := l.inside(l.db); ok {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if rel == db+suffix {
				return true
			}
		}
	}
	return false
}

type restorer struct {
	cfg   *config.Config
	o     RestoreOptions
	lay   layout
	stage string
	now   time.Time
	plan  RestorePlan

	// keepStage keeps the staging directory when Restore returns: a swap whose
	// undo failed left restored entries in it that the operator must see.
	keepStage bool

	dbFile string // the staged database member (SQLite file or DILLADMP stream)
	keys   keysDoc
	hasKey bool
}

func (r *restorer) dbMember() string {
	if r.cfg.DB.Driver == "postgres" {
		return postgresMember
	}
	return sqliteMember
}

// extract is step 2: the whole archive through readArchive, staging as it goes.
func (r *restorer) extract(ctx context.Context, highest int64) error {
	check := func(m Manifest) error {
		if m.Engine != r.cfg.DB.Driver {
			return fmt.Errorf("ops: restore: the archive is a %s instance's and this config's db.driver is %s: %w",
				m.Engine, r.cfg.DB.Driver, exit.Config)
		}
		if m.SchemaVersion > highest {
			return fmt.Errorf("ops: restore: the archive's schema %d is newer than this binary's highest migration %d; "+
				"restore it with the dillad that wrote it, or a newer one: %w", m.SchemaVersion, highest, exit.Config)
		}
		if len(m.Gaps) > 0 && !r.o.Force {
			return fmt.Errorf("ops: restore: the archive names %d gap(s), referenced blobs it does not hold (first: %s); "+
				"rerun with --force to restore without them: %w", len(m.Gaps), m.Gaps[0], exit.Data)
		}
		return nil
	}

	r.dbFile = filepath.Join(r.stage, ".restore-database")
	var bs *blob.Store
	if !r.o.DryRun {
		var err error
		if bs, err = blob.Open(r.lay.staged(r.stage, r.lay.blobs, "blobs"), r.cfg.Blobs.Backend); err != nil {
			return fmt.Errorf("ops: restore: open the staged blob store: %w: %w", err, exit.CantCreate)
		}
		defer func() { _ = bs.Close() }()
	}
	seenDB := false
	sink := func(e Entry, body io.Reader) error {
		damaged := func(format string, args ...any) error {
			return fmt.Errorf("ops: restore: %s: %w", fmt.Sprintf(format, args...), exit.Data)
		}
		switch {
		case e.Path == sqliteMember || e.Path == postgresMember:
			if e.Path != r.dbMember() {
				return damaged("the archive's database member is %s, and its manifest says %s", e.Path, r.cfg.DB.Driver)
			}
			seenDB = true
			return writeNew(r.dbFile, body)
		case e.Path == configMember:
			if r.o.DryRun {
				return nil
			}
			r.plan.ArchivedConfig = filepath.Join(r.lay.dataDir, restoredConfigName)
			return writeNew(filepath.Join(r.stage, restoredConfigName), body)
		case e.Path == keysMember:
			if e.Size > maxKeysBytes {
				return damaged("keys/instance.json is %d bytes, over the %d-byte bound", e.Size, maxKeysBytes)
			}
			raw, err := io.ReadAll(body)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &r.keys); err != nil {
				return damaged("keys/instance.json: %v", err)
			}
			r.hasKey = true
			return nil
		case e.Path == readmeMember:
			return nil
		case strings.HasPrefix(e.Path, blobsPrefix):
			blobID, err := blobIDOf(e.Path)
			if err != nil || hex.EncodeToString(blobID) != e.SHA256 {
				return damaged("%s is not a blob member whose name is its SHA-256", e.Path)
			}
			r.plan.BlobsToWrite++
			if r.o.DryRun {
				return nil
			}
			if _, _, err := bs.Put(ctx, blobID, body, e.Size); err != nil {
				if errors.Is(err, blob.ErrHashMismatch) || errors.Is(err, blob.ErrTooLarge) {
					return damaged("%s: %v", e.Path, err)
				}
				return fmt.Errorf("ops: restore: write %s: %w: %w", e.Path, err, exit.IOErr)
			}
			return nil
		default:
			return damaged("member %s is not part of archive format %d", e.Path, FormatVersion)
		}
	}
	man, err := readArchive(ctx, r.o.From, check, sink)
	if err != nil {
		var code exit.Code
		if !errors.As(err, &code) {
			err = fmt.Errorf("%w: %w", err, exit.IOErr)
		}
		return err
	}
	if !seenDB {
		return fmt.Errorf("ops: restore: the archive holds no database: %w", exit.Data)
	}
	r.plan.Manifest = man
	return nil
}

// blobIDOf is the id a blobs/<aa>/<bb>/<hex> member names, or an error when the
// path is anything else.
func blobIDOf(member string) ([]byte, error) {
	rest := strings.TrimPrefix(member, blobsPrefix)
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || len(parts[2]) != 64 || parts[0] != parts[2][0:2] || parts[1] != parts[2][2:4] {
		return nil, fmt.Errorf("not a blob path: %s", member)
	}
	if strings.ToLower(parts[2]) != parts[2] {
		return nil, fmt.Errorf("not a lower-case blob path: %s", member)
	}
	return hex.DecodeString(parts[2])
}

// liveInstance reads the instance row of the database the restore replaces.
// Its absence — a fresh host, or a database too damaged to read, which is a
// reason to restore — is not an error.
func (r *restorer) liveInstance(ctx context.Context) (store.InstanceRow, bool, error) {
	switch r.cfg.DB.Driver {
	case "sqlite":
		if _, err := os.Stat(r.lay.db); errors.Is(err, fs.ErrNotExist) {
			return store.InstanceRow{}, false, nil
		}
		read, err := sqlite.OpenRead(r.lay.db)
		if err != nil {
			// An unreadable live database is what a restore replaces.
			return store.InstanceRow{}, false, nil
		}
		repo := sqlite.New(nil, read)
		defer func() { _ = repo.Close() }()
		in, err := repo.GetInstance(ctx)
		if err != nil {
			return store.InstanceRow{}, false, nil
		}
		return in, true, nil
	case "postgres":
		db, err := postgres.Open(r.cfg.DB.DSN, r.cfg.DB.MaxOpenConns, r.cfg.DB.ConnMaxLifetime.Value())
		if err != nil {
			return store.InstanceRow{}, false, fmt.Errorf("ops: restore: open the database: %w: %w", err, exit.Unavailable)
		}
		repo := postgres.New(db)
		defer func() { _ = repo.Close() }()
		if err := db.PingContext(ctx); err != nil {
			return store.InstanceRow{}, false, fmt.Errorf("ops: restore: reach the database: %w: %w", err, exit.Unavailable)
		}
		in, err := repo.GetInstance(ctx)
		if err != nil {
			// An empty database has no instance row, and no table for one.
			return store.InstanceRow{}, false, nil
		}
		return in, true, nil
	default:
		return store.InstanceRow{}, false, fmt.Errorf("ops: restore: db.driver %q is neither sqlite nor postgres: %w", r.cfg.DB.Driver, exit.Config)
	}
}

// inspect is step 3: what the archive holds, and whether it may replace the
// live instance.
func (r *restorer) inspect(ctx context.Context) error {
	var archived id.ID
	switch r.cfg.DB.Driver {
	case "sqlite":
		read, err := sqlite.OpenRead(r.dbFile)
		if err != nil {
			return fmt.Errorf("ops: restore: open the archived database: %w: %w", err, exit.Data)
		}
		defer func() { _ = read.Close() }()
		repo := sqlite.New(nil, read)
		in, err := repo.GetInstance(ctx)
		if err != nil {
			return fmt.Errorf("ops: restore: the archived database has no instance row: %w: %w", err, exit.Data)
		}
		archived = in.InstanceID
		if err := checkEarlierBranchBuild(ctx, read); err != nil {
			return err
		}
		if r.plan.RowCounts, err = sqliteRowCounts(ctx, read); err != nil {
			return fmt.Errorf("ops: restore: count the archived rows: %w: %w", err, exit.Data)
		}
	case "postgres":
		if !r.hasKey {
			return fmt.Errorf("ops: restore: the archive has no keys/instance.json: %w", exit.Data)
		}
		var err error
		if archived, err = id.Parse(r.keys.InstanceID); err != nil {
			return fmt.Errorf("ops: restore: keys/instance.json instance_id: %w: %w", err, exit.Data)
		}
		if r.plan.RowCounts, err = dumpRowCounts(r.dbFile); err != nil {
			return fmt.Errorf("ops: restore: count the archived rows: %w: %w", err, exit.Data)
		}
	}

	live, ok, err := r.liveInstance(ctx)
	if err != nil {
		return err
	}
	archivedGen := r.plan.Manifest.Generation
	r.plan.GenerationOld = archivedGen
	if ok {
		r.plan.GenerationOld = live.Generation
		if live.InstanceID != archived && !r.o.Force {
			return fmt.Errorf("ops: restore: the archive is instance %s's and this is instance %s; "+
				"rerun with --force to replace this instance's identity with the archive's: %w",
				archived, live.InstanceID, exit.Config)
		}
		if live.InstanceID != archived {
			r.plan.Warnings = append(r.plan.Warnings, fmt.Sprintf("--force: instance %s replaces instance %s", archived, live.InstanceID))
		}
	} else {
		r.plan.Warnings = append(r.plan.Warnings, "no readable live instance: the generation is set from the archive's alone")
	}
	r.plan.GenerationNew = max(r.plan.GenerationOld, archivedGen) + 1
	r.plan.HealDeadline = r.now.Add(ds.DefaultPolicy().HealWindow).Unix()
	return nil
}

// checkEarlierBranchBuild refuses a database created before Plan 1 edited
// 00002_mls.sql in place: goose recorded version 2 for it, so no migration will
// ever add the column the queries read.
func checkEarlierBranchBuild(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'mls_groups'`).Scan(&n); err != nil {
		return fmt.Errorf("ops: restore: read the archived schema: %w: %w", err, exit.Data)
	}
	if n == 0 {
		return nil
	}
	// Preparing the statement is enough: SQLite resolves every column at prepare time.
	stmt, err := db.PrepareContext(ctx, `SELECT handshakes_pruned_through FROM mls_groups`)
	if err != nil {
		return fmt.Errorf("ops: restore: the archived database has no mls_groups.handshakes_pruned_through column (%v): "+
			"it was created by an earlier build of this branch, before 00002_mls.sql was edited in place, and no migration adds it; "+
			"such a database cannot be restored and the instance must be recreated with `dillad init`: %w", err, exit.Data)
	}
	defer func() { _ = stmt.Close() }()
	return nil
}

// sqliteRowCounts counts every DumpTables table the archived database holds.
func sqliteRowCounts(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	counts := map[string]int64{}
	for _, table := range store.DumpTables {
		var present int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&present); err != nil {
			return nil, err
		}
		if present == 0 {
			continue
		}
		var n int64
		// table is a name from the fixed DumpTables list, never input.
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "`+table+`"`).Scan(&n); err != nil {
			return nil, err
		}
		counts[table] = n
	}
	return counts, nil
}

// dumpRowCounts counts the tuples of every table in a DILLADMP stream.
func dumpRowCounts(p string) (map[string]int64, error) {
	f, err := os.Open(p) //nolint:gosec // G304: this restore's own staged member
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	counts := map[string]int64{}
	err = store.ReadDump(f, func(table string, payload io.Reader) error {
		n, err := store.CountCopyRows(payload)
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		counts[table] = n
		return nil
	})
	return counts, err
}

// apply is step 5, on the restored database, before the swap: the SQLite file
// in the staging directory, or the Postgres database loaded in place.
func (r *restorer) apply(ctx context.Context) error {
	if r.hasKey && r.tlsInside() {
		if err := writeACMEKeys(r.lay.staged(r.stage, r.lay.tls, "tls"), r.keys.ACMEAccountKeys); err != nil {
			return err
		}
	}
	switch r.cfg.DB.Driver {
	case "sqlite":
		final := r.lay.staged(r.stage, r.lay.db, "dilla.db")
		if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
			return fmt.Errorf("ops: restore: %w: %w", err, exit.CantCreate)
		}
		if err := os.Rename(r.dbFile, final); err != nil {
			return fmt.Errorf("ops: restore: %w: %w", err, exit.IOErr)
		}
		r.dbFile = final
		write, err := sqlite.OpenWrite(final)
		if err != nil {
			return fmt.Errorf("ops: restore: open the restored database: %w: %w", err, exit.IOErr)
		}
		read, err := sqlite.OpenRead(final)
		if err != nil {
			_ = write.Close()
			return fmt.Errorf("ops: restore: open the restored database: %w: %w", err, exit.IOErr)
		}
		repo := sqlite.New(write, read)
		err = r.armHeal(ctx, repo)
		if cerr := repo.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("ops: restore: close the restored database: %w: %w", cerr, exit.IOErr)
		}
		return err
	case "postgres":
		return r.applyPostgres(ctx)
	default:
		return fmt.Errorf("ops: restore: db.driver %q is neither sqlite nor postgres: %w", r.cfg.DB.Driver, exit.Config)
	}
}

// applyPostgres brings the live database to the archive's schema (an empty
// database is migrated up to it; a newer one is refused, since a binary COPY
// cannot land in columns it does not name), then loads the dump and arms the
// heal in ONE transaction.
func (r *restorer) applyPostgres(ctx context.Context) error {
	db, err := postgres.Open(r.cfg.DB.DSN, r.cfg.DB.MaxOpenConns, r.cfg.DB.ConnMaxLifetime.Value())
	if err != nil {
		return fmt.Errorf("ops: restore: open the database: %w: %w", err, exit.Unavailable)
	}
	defer func() { _ = db.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, postgresmigrations.FS)
	if err != nil {
		return fmt.Errorf("ops: restore: migrations: %w: %w", err, exit.Software)
	}
	current, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("ops: restore: read the database's schema version: %w: %w", err, exit.Unavailable)
	}
	want := r.plan.Manifest.SchemaVersion
	if current > want {
		return fmt.Errorf("ops: restore: the database is at schema %d and the archive at %d; a Postgres archive loads only into a "+
			"database at its own schema: point db.dsn at an empty database and rerun, and restore migrates it to %d first: %w",
			current, want, want, exit.Config)
	}
	if current < want {
		if _, err := provider.UpTo(ctx, want); err != nil {
			return fmt.Errorf("ops: restore: migrate the database to the archive's schema %d: %w: %w", want, err, exit.Software)
		}
	}
	f, err := os.Open(r.dbFile)
	if err != nil {
		return fmt.Errorf("ops: restore: %w: %w", err, exit.IOErr)
	}
	defer func() { _ = f.Close() }()
	// The load and the heal are ONE transaction: a heal that fails rolls the
	// load back with it, so the database never holds the archive at its old
	// generation with no group epoch-unknown (invariant 11).
	var healErr error
	err = postgres.LoadDump(ctx, db, f, func(tx store.Repository) error {
		healErr = r.heal(ctx, tx)
		return healErr
	})
	switch {
	case err == nil:
		return nil
	case healErr != nil:
		return fmt.Errorf("ops: restore: %w (the load rolled back with it: the database is unchanged): %w", healErr, exit.IOErr)
	default:
		return fmt.Errorf("ops: restore: load the dump: %w: %w", err, exit.Data)
	}
}

// armHeal runs heal as ONE transaction on repo.
func (r *restorer) armHeal(ctx context.Context, repo store.Repository) error {
	if err := repo.Tx(ctx, func(tx store.Repository) error { return r.heal(ctx, tx) }); err != nil {
		return fmt.Errorf("ops: restore: %w: %w", err, exit.IOErr)
	}
	return nil
}

// heal is invariant 11's restore half, the same statements ds.OnRestore runs
// plus the two settings only a restore owns. tx must be a transaction: the
// statements move together or not at all.
func (r *restorer) heal(ctx context.Context, tx store.Repository) error {
	// Share the same instance-generation row fence as publishers and the in-process OnRestore path.
	// This is the first row lock in this transaction; the remaining restore writes all happen while
	// it is held, so accepted uploads are either purged by this restore or observe its generation.
	if err := tx.LockInstanceGeneration(ctx); err != nil {
		return fmt.Errorf("lock the instance generation: %w", err)
	}
	gen := r.plan.GenerationNew
	if err := tx.SetGeneration(ctx, gen); err != nil {
		return fmt.Errorf("set the generation: %w", err)
	}
	if err := tx.MarkAllGroupsEpochUnknown(ctx, r.plan.HealDeadline); err != nil {
		return fmt.Errorf("mark every group epoch-unknown: %w", err)
	}
	if err := tx.EndAllVoiceSessions(ctx, r.now.Unix()); err != nil {
		return fmt.Errorf("end the live calls: %w", err)
	}
	n, err := tx.PurgeKeyPackages(ctx, true)
	if err != nil {
		return fmt.Errorf("purge the KeyPackages: %w", err)
	}
	r.plan.KeyPackagesPurged = n
	// Every SQLite snapshot carries the backup's own pin: left, it would hold
	// the restored sweeper at the backup's start for good.
	if err := tx.PutSetting(ctx, blob.LastBackupStartedKey, []byte{}, r.now.Unix()); err != nil {
		return fmt.Errorf("clear %s: %w", blob.LastBackupStartedKey, err)
	}
	if err := tx.PutSetting(ctx, ds.RestorePendingKey, []byte(strconv.FormatUint(gen, 10)), r.now.Unix()); err != nil {
		return fmt.Errorf("record the pending restore: %w", err)
	}
	in, err := tx.GetInstance(ctx)
	if err != nil {
		return err
	}
	if in.Generation != gen {
		return fmt.Errorf("the generation is %d after setting it to %d", in.Generation, gen)
	}
	if beforeHealCommit != nil {
		return beforeHealCommit()
	}
	return nil
}

func (r *restorer) tlsInside() bool {
	_, ok := r.lay.inside(r.lay.tls)
	return ok
}

// installExternal installs what the swap could not carry: a database, blob
// directory or TLS storage configured outside the data directory.
func (r *restorer) installExternal(ctx context.Context) error {
	// .external is exempt from the swap, so it is still in the staging
	// directory, which Restore removes once this returns.
	external := filepath.Join(r.stage, ".external")
	if r.hasKey && !r.tlsInside() && r.lay.tls != "" {
		if err := writeACMEKeys(r.lay.tls, r.keys.ACMEAccountKeys); err != nil {
			return err
		}
	}
	if _, ok := r.lay.inside(r.lay.blobs); !ok {
		if err := mergeBlobs(ctx, filepath.Join(external, "blobs"), r.lay.blobs, r.cfg.Blobs.Backend); err != nil {
			return err
		}
	}
	if _, ok := r.lay.inside(r.lay.db); !ok && r.lay.db != "" {
		suffix := ".old-" + hex8()
		for _, s := range []string{"", "-wal", "-shm", "-journal"} {
			if err := os.Rename(r.lay.db+s, r.lay.db+suffix+s); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("ops: restore: move %s aside: %w: %w", r.lay.db+s, err, exit.IOErr)
			}
		}
		if err := moveFile(filepath.Join(external, "dilla.db"), r.lay.db); err != nil {
			return fmt.Errorf("ops: restore: install the database at %s (the live one is at %s): %w: %w",
				r.lay.db, r.lay.db+suffix, err, exit.IOErr)
		}
		r.plan.Warnings = append(r.plan.Warnings, "the database outside the data directory was moved to "+r.lay.db+suffix)
	}
	return nil
}

// writeACMEKeys writes each archived ACME account key under dir. A key's path
// must be certmagic's acme/<issuer>/users/<email>/<name>.key, relative and
// clean; anything else is a damaged archive.
func writeACMEKeys(dir string, keys []acmeKey) error {
	for _, k := range keys {
		p := path.Clean(k.Path)
		parts := strings.Split(p, "/")
		if p != k.Path || path.IsAbs(p) || len(parts) != 5 || parts[0] != "acme" || parts[2] != "users" ||
			!strings.HasSuffix(p, ".key") || slicesContainsDotDot(parts) {
			return fmt.Errorf("ops: restore: keys/instance.json names %q, not an ACME account key path: %w", k.Path, exit.Data)
		}
		if err := writeNew(filepath.Join(dir, filepath.FromSlash(p)), strings.NewReader(k.PEM)); err != nil {
			return fmt.Errorf("ops: restore: write %s: %w: %w", p, err, exit.IOErr)
		}
	}
	return nil
}

func slicesContainsDotDot(parts []string) bool {
	for _, p := range parts {
		if p == ".." || p == "." || p == "" {
			return true
		}
	}
	return false
}

// mergeBlobs puts every blob of the staged store at from into the store at to.
// Put hashes each one again, and keeps an object the destination already has.
func mergeBlobs(ctx context.Context, from, to, backend string) error {
	if _, err := os.Stat(from); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	dst, err := blob.Open(to, backend)
	if err != nil {
		return fmt.Errorf("ops: restore: open the blob store %s: %w: %w", to, err, exit.CantCreate)
	}
	defer func() { _ = dst.Close() }()
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() || len(d.Name()) != 64 {
			return nil
		}
		blobID, err := hex.DecodeString(d.Name())
		if err != nil {
			return nil // not a blob object: a staging leftover
		}
		f, err := os.Open(p) //nolint:gosec // G304: this restore's own staged blob
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if _, _, err := dst.Put(ctx, blobID, f, info.Size()); err != nil {
			return fmt.Errorf("ops: restore: install blob %s in %s: %w: %w", d.Name(), to, err, exit.IOErr)
		}
		return nil
	})
}

// linkTree recreates src under dst: directories with their permissions, each
// regular file as a hard link (a copy when the link fails), each symlink as the
// same link. Paths skip reports (relative to src) are left out, with
// everything under them. A hard link is safe because nothing in the staged
// tree is ever written in place: every file a restore writes is removed and
// created anew (writeNew), and the blob store renames over its objects.
func linkTree(src, dst string, skip func(rel string) bool) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			// Recreated, never followed: WalkDir does not descend a symlink, and the
			// tree is the operator's own data directory, held under the exclusive lock.
			return os.Symlink(link, target) //nolint:gosec // G122: see above
		case d.Type().IsRegular():
			if err := os.Link(p, target); err == nil { //nolint:gosec // G122: the locked data directory into this restore's own staging directory
				return nil
			}
			return copyFile(p, target)
		default:
			return nil // a socket or a device node is not instance state
		}
	})
}

func copyFile(from, to string) error {
	in, err := os.Open(from) //nolint:gosec // G304: a file of the live data directory
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm()) //nolint:gosec // G304: a path in this restore's staging directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// moveFile renames, or copies and removes when the rename crosses a file
// system.
func moveFile(from, to string) error {
	if err := os.Rename(from, to); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	if err := copyFile(from, to); err != nil {
		return err
	}
	return os.Remove(from)
}

// writeNew writes body to p as a NEW file (0600): whatever p was — a hard link
// into the live directory included — is removed first, so a write never
// reaches through a link into the directory the restore replaces.
func writeNew(p string, body io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a path in this restore's staging directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// appendFile adds text to the end of the existing file p and syncs it. It never
// creates or truncates p, so a failure leaves what p already said intact.
func appendFile(p, text string) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // G304: the swap marker in the operator's data directory
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, text); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// RestoreFile puts a SQLite pre-migration backup back in place of the database
// at dbPath, for `serve` when a migration fails: the failed database's WAL,
// shared-memory and journal files are removed first, since a WAL left beside
// the restored file would be replayed into it on the next open, and the backup
// is then renamed over the database. Every connection to dbPath must already be
// closed.
func RestoreFile(backup, dbPath string) error {
	if _, err := os.Stat(backup); err != nil {
		return fmt.Errorf("ops: the pre-migration backup: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(dbPath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("ops: remove %s: %w", dbPath+suffix, err)
		}
	}
	if err := os.Rename(backup, dbPath); err != nil {
		return fmt.Errorf("ops: put the pre-migration backup back: %w", err)
	}
	if d, err := os.Open(filepath.Dir(dbPath)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func hex8() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

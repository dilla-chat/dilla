//! One instance's encrypted local database: a SQLite3 Multiple Ciphers (ChaCha20-Poly1305) file keyed
//! by `dilla_core::native_store` with 32 random bytes that live only
//! in the OS keychain (service [`crate::keychain::SERVICE`], account `<install-id>/instance-<n>`, the
//! install id being the root's, from `instances.json`; see [`crate::instances`]).
//!
//! # Open, and recovery from a first open cut short
//!
//! The key is always stored (and read back) before the database is created, and a database is only
//! ever made under a staging name and renamed into place once its schema is complete. Whenever there is
//! no `store.db`, a fresh key is minted and stored over whatever the keychain held, so a key never
//! outlives its database: deleting the data dir is a crypto-erase. The states a crash can leave, and
//! what the next open does with them, are:
//!
//! | keychain | `store.db` | next open                                                            |
//! |----------|------------|----------------------------------------------------------------------|
//! | none     | absent     | first open: a fresh key, then a fresh database                       |
//! | key      | absent     | the key protects nothing: replace it with a fresh one, build the database |
//! | key      | present    | open it; a key that does not decrypt it is [`StoreError::Locked`]    |
//! | none     | present    | the key is lost: [`StoreError::KeyMissing`], the file is left alone  |
//!
//! A leftover staging file (`store.db.init` and its journal) is the unfinished half of a first open
//! and is deleted before anything else. With no `store.db`, a leftover `store.db-wal`, `-shm` or
//! `-journal` belongs to a database that is gone; it is deleted too, before the new database is
//! published, so SQLite never plays an old log into a new file.
//!
//! # Copied roots
//!
//! The install id lives in `instances.json`, so a copied data root (a backup restored beside the
//! original, a home directory cloned) carries the original's install id and would name the same
//! keychain accounts: the copy's next first open would mint a key over the one the original's
//! database needs. Each instance therefore keeps a generation record beside its lock file
//! (`instances/<n>.generation`: a generation id and the data dir it belongs to). The generation is
//! bound into the keychain account (`<install-id>/instance-<n>/<generation>`; the first generation of
//! a data dir is the plain `<install-id>/instance-<n>`). An open that finds the record naming another
//! data dir is in a copy: it takes a generation of its own, so the two roots never share a keychain
//! entry again (an existing database's key is copied to the new entry; a missing one gets a fresh key
//! under the new entry). Within one data dir the generation is kept, so re-creating a removed
//! instance still replaces its old key (a crypto-erase).
//!
//! # WAL, durability and file systems
//!
//! The published store runs in WAL mode with `synchronous=FULL` (as the Bot's store does): a commit
//! is durable once it returns. A file system that cannot hold WAL's shared-memory index (some network
//! and FUSE file systems) is [`StoreError::Unsupported`], whose message names the file system, never a
//! bare database error. Opens and removals of the same instance are serialised by an
//! exclusive lock on a file beside the data dir (`instances/<n>.lock`), so two processes cannot both
//! mint a key. That file is created on first use and never deleted, not even by
//! [`crate::instances::Instances::remove`]: unlinking it while someone holds it would let the next two
//! opens lock different files. Under the lock an open re-reads `instances.json` and refuses an instance
//! that is no longer listed, so it never recreates a data dir a removal has just deleted. The install
//! id is only ever read here, never minted, so a missing or damaged list is an error, not a new key.

use std::fs::{self, File, OpenOptions};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use dilla_core::client::{ClientCore, ClientError};
use dilla_core::mls::ConnHandle;
use dilla_core::native_store::{self, KeyError};
use rusqlite::{Connection, OpenFlags};
use zeroize::Zeroizing;

use crate::instances::{MAX_INSTANCES, install_id_for};
use crate::seams::{InstanceConfig, InstanceId, Keychain};

/// The encrypted database's file name inside an instance's data dir.
pub const DB_FILE: &str = "store.db";
/// A database under construction; never opened as the instance's store.
const STAGE_FILE: &str = "store.db.init";
const KEY_LEN: usize = 32;
/// How long an open waits for another connection's lock before failing with `Database`.
const BUSY_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(2);

#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    /// The keychain could not be read or written. There is no fallback.
    #[error("the platform keychain is unavailable")]
    KeychainUnavailable,
    /// The database cannot be decrypted with the stored key: a wrong key or a damaged page 1, which
    /// SQLite3 Multiple Ciphers both report as `SQLITE_NOTADB`; or the stored value is not a 32-byte key. Busy, I/O and other failures are
    /// [`StoreError::Database`], never this: a UI may offer a reset on `Locked`.
    #[error("the database cannot be decrypted (wrong key or damaged page 1)")]
    Locked,
    /// The database exists but the keychain holds no key for it; its contents are unrecoverable.
    #[error("the database key is missing from the keychain")]
    KeyMissing,
    #[error("at most 32 instances can exist")]
    TooManyInstances,
    #[error("the instance id is outside 0..=31")]
    InvalidInstance,
    #[error("the server address must be an http or https URL")]
    InvalidServer,
    #[error("the instance list is unreadable")]
    CorruptInstanceList,
    #[error("the operating system has no data directory")]
    NoDataDir,
    #[error("a database operation failed")]
    Database,
    /// The data directory's file system cannot hold the store in WAL mode (no shared memory for its index): the
    /// message names the file system and the directory.
    #[error("{0}")]
    Unsupported(String),
    #[error("the local store refused to open: {0:?}")]
    Core(ClientError),
    #[error("file system error: {0}")]
    Io(#[from] std::io::Error),
}

/// The keychain account that holds instance `id`'s database key in its data dir's first generation, scoped to its
/// root's install id ([`crate::instances::Instances::install_id`]). A data dir in a copied root uses a later
/// generation ([`store_keychain_account`]).
pub fn keychain_account(install_id: &str, id: InstanceId) -> String {
    format!("{install_id}/instance-{}", id.0)
}

/// The keychain account of a generation: the first (`None`) is [`keychain_account`]'s.
fn generation_account(install_id: &str, id: InstanceId, generation: Option<&str>) -> String {
    match generation {
        None => keychain_account(install_id, id),
        Some(g) => format!("{install_id}/instance-{}/{g}", id.0),
    }
}

/// The keychain account that holds `cfg`'s database key now (its generation record read as an open reads it), for a
/// caller that deletes the key of a removed instance or a test that reads it.
pub fn store_keychain_account(cfg: &InstanceConfig) -> Result<String, StoreError> {
    let install_id = install_id_for(&cfg.data_dir, cfg.id)?;
    let generation = read_generation(&cfg.data_dir)?.and_then(|r| r.generation);
    Ok(generation_account(
        &install_id,
        cfg.id,
        generation.as_deref(),
    ))
}

/// `instances/<n>.generation`: the data dir's generation and the data dir it was recorded for.
#[derive(Debug, PartialEq, Eq)]
struct Generation {
    /// `None`: the first generation (the plain account).
    generation: Option<String>,
    /// The canonical data dir the record was written for.
    dir: String,
}

const FIRST_GENERATION: &str = "-";

fn generation_path(data_dir: &Path) -> PathBuf {
    let mut name = data_dir.file_name().unwrap_or_default().to_os_string();
    name.push(".generation");
    data_dir.with_file_name(name)
}

fn canonical_dir(data_dir: &Path) -> Result<String, StoreError> {
    Ok(fs::canonicalize(data_dir)?.to_string_lossy().into_owned())
}

fn read_generation(data_dir: &Path) -> Result<Option<Generation>, StoreError> {
    let raw = match fs::read_to_string(generation_path(data_dir)) {
        Ok(raw) => raw,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e.into()),
    };
    let (generation, dir) = raw.split_once('\n').ok_or(StoreError::Database)?;
    let generation = match generation {
        FIRST_GENERATION => None,
        g if g.len() == 32 && g.bytes().all(|b| b.is_ascii_hexdigit()) => Some(g.to_owned()),
        _ => return Err(StoreError::Database),
    };
    Ok(Some(Generation {
        generation,
        dir: dir.trim_end_matches('\n').to_owned(),
    }))
}

fn write_generation(data_dir: &Path, record: &Generation) -> Result<(), StoreError> {
    use std::io::Write;
    let path = generation_path(data_dir);
    let mut tmp = path.clone().into_os_string();
    tmp.push(".tmp");
    let tmp = PathBuf::from(tmp);
    let mut out = private_file_options()
        .create(true)
        .truncate(true)
        .open(&tmp)?;
    let generation = record.generation.as_deref().unwrap_or(FIRST_GENERATION);
    out.write_all(format!("{generation}\n{}\n", record.dir).as_bytes())?;
    out.sync_all()?;
    drop(out);
    fs::rename(&tmp, &path)?;
    match path.parent() {
        Some(parent) => sync_dir(parent),
        None => Ok(()),
    }
}

fn new_generation() -> Result<String, StoreError> {
    let mut id = [0u8; 16];
    getrandom::fill(&mut id)
        .map_err(|_| StoreError::Io(std::io::Error::other("the OS random source failed")))?;
    Ok(id.iter().map(|b| format!("{b:02x}")).collect())
}

/// Opens (creating on first use) the instance's encrypted store and the client core on it.
pub fn open_store(cfg: &InstanceConfig, kc: &dyn Keychain) -> Result<ClientCore, StoreError> {
    let conn = open_connection(cfg, kc)?;
    ClientCore::open(conn).map_err(StoreError::Core)
}

/// The keyed, verified connection under [`open_store`].
pub fn open_connection(cfg: &InstanceConfig, kc: &dyn Keychain) -> Result<ConnHandle, StoreError> {
    if usize::from(cfg.id.0) >= MAX_INSTANCES {
        return Err(StoreError::InvalidInstance);
    }
    // Read-only: a root without a valid list (and so without its install id) is an error.
    let install_id = install_id_for(&cfg.data_dir, cfg.id)?;
    // Ask the keychain before touching the disk: without it nothing is written.
    drop(read_key(kc, &keychain_account(&install_id, cfg.id))?);

    let _lock = lock_instance(&cfg.data_dir)?;
    // Under the lock: the instance may have been removed, or the list replaced, meanwhile.
    if install_id_for(&cfg.data_dir, cfg.id)? != install_id {
        return Err(StoreError::CorruptInstanceList);
    }
    create_private_dir_all(&cfg.data_dir)?;
    remove_stage(&cfg.data_dir)?;
    let here = canonical_dir(&cfg.data_dir)?;
    let record = read_generation(&cfg.data_dir)?;
    // A record naming another data dir: this root is a copy (or a move) of that one.
    let copied = record.as_ref().is_some_and(|r| r.dir != here);
    let generation = record.and_then(|r| r.generation);
    // Read again under the lock: another process may have finished a first open meanwhile.
    let account = generation_account(&install_id, cfg.id, generation.as_deref());
    let stored = read_key(kc, &account)?;

    let db = cfg.data_dir.join(DB_FILE);
    let exists = match fs::symlink_metadata(&db) {
        Ok(_) => true,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => false,
        Err(e) => return Err(e.into()),
    };
    if exists {
        let key = stored.ok_or(StoreError::KeyMissing)?;
        let key = as_key(&key).ok_or(StoreError::Locked)?;
        let conn = open_keyed(&db, &key, &cfg.data_dir)?;
        if copied {
            // The copy's database works with the original's key; from now on it is held under an entry of its own,
            // which the original's later first open can never overwrite.
            let fresh = Generation {
                generation: Some(new_generation()?),
                dir: here,
            };
            let own = generation_account(&install_id, cfg.id, fresh.generation.as_deref());
            store_key(kc, &own, &key)?;
            write_generation(&cfg.data_dir, &fresh)?;
        } else {
            claim_generation(&cfg.data_dir, generation, here)?;
        }
        return Ok(Arc::new(Mutex::new(conn)));
    }

    // No database: a WAL, shared-memory index or journal left beside it belongs to a database that is gone.
    remove_stale_sidecars(&cfg.data_dir)?;
    // Whatever this data dir's generation holds in the keychain (a first open cut short, or a removed instance on this
    // id) protects nothing: a fresh key goes over it. In a copied root the original may still need that key, so the
    // copy takes a generation of its own first.
    drop(stored);
    let (account, generation) = if copied {
        let fresh = Some(new_generation()?);
        (
            generation_account(&install_id, cfg.id, fresh.as_deref()),
            fresh,
        )
    } else {
        (account, generation)
    };
    write_generation(
        &cfg.data_dir,
        &Generation {
            generation,
            dir: here,
        },
    )?;
    let key = new_key()?;
    store_key(kc, &account, &key)?;
    create_database(&cfg.data_dir, &db, &key)?;
    Ok(Arc::new(Mutex::new(open_keyed(&db, &key, &cfg.data_dir)?)))
}

/// Stores `key` under `account` and checks that it reads back.
fn store_key(kc: &dyn Keychain, account: &str, key: &[u8; KEY_LEN]) -> Result<(), StoreError> {
    kc.set(account, key.as_slice())
        .map_err(|_| StoreError::KeychainUnavailable)?;
    let back = read_key(kc, account)?.ok_or(StoreError::KeychainUnavailable)?;
    if back.as_slice() != key.as_slice() {
        return Err(StoreError::KeychainUnavailable);
    }
    Ok(())
}

/// Records this data dir as the owner of its (unchanged) generation, when no record says so yet: a store written
/// before records existed, or one whose record a crash lost.
fn claim_generation(
    data_dir: &Path,
    generation: Option<String>,
    here: String,
) -> Result<(), StoreError> {
    let record = Generation {
        generation,
        dir: here,
    };
    if read_generation(data_dir)?.as_ref() == Some(&record) {
        return Ok(());
    }
    write_generation(data_dir, &record)
}

/// The files SQLite keeps beside `store.db`, deleted when `store.db` itself is absent.
fn remove_stale_sidecars(dir: &Path) -> Result<(), StoreError> {
    for suffix in ["-wal", "-shm", "-journal"] {
        match fs::remove_file(dir.join(format!("{DB_FILE}{suffix}"))) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => return Err(e.into()),
        }
    }
    Ok(())
}

fn read_key(kc: &dyn Keychain, account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, StoreError> {
    kc.get(account).map_err(|_| StoreError::KeychainUnavailable)
}

/// A usable database key: exactly 32 bytes, not all zero.
fn as_key(bytes: &[u8]) -> Option<Zeroizing<[u8; KEY_LEN]>> {
    if bytes.len() != KEY_LEN || bytes.iter().all(|b| *b == 0) {
        return None;
    }
    // Copy straight into the zeroizing buffer, never through a plain array.
    let mut key = Zeroizing::new([0u8; KEY_LEN]);
    key.copy_from_slice(bytes);
    Some(key)
}

fn new_key() -> Result<Zeroizing<[u8; KEY_LEN]>, StoreError> {
    let mut key = Zeroizing::new([0u8; KEY_LEN]);
    while key.iter().all(|b| *b == 0) {
        getrandom::fill(key.as_mut_slice())
            .map_err(|_| StoreError::Io(std::io::Error::other("the OS random source failed")))?;
    }
    Ok(key)
}

/// Builds the schema under the staging name, then publishes it with an atomic rename.
fn create_database(dir: &Path, db: &Path, key: &[u8; KEY_LEN]) -> Result<(), StoreError> {
    let stage = dir.join(STAGE_FILE);
    // An empty file is a new SQLite database; creating it here sets its owner-only mode.
    drop(private_file_options().create_new(true).open(&stage)?);
    let core = ClientCore::open(Arc::new(Mutex::new(open_keyed(&stage, key, dir)?)))
        .map_err(StoreError::Core)?;
    // Dropping the core drops the only handle and closes the connection.
    drop(core);
    // A writable handle: Windows' FlushFileBuffers refuses a read-only one.
    private_file_options().open(&stage)?.sync_all()?;
    fs::rename(&stage, db)?;
    sync_dir(dir)
}

/// Opens an existing file with the raw key through `dilla_core::native_store`, as `dilla-bot-core`
/// does (cipher, then key, then a schema read that proves it). Only `SQLITE_NOTADB` (the key does not
/// decrypt the file) is [`StoreError::Locked`]; every other failure is [`StoreError::Database`].
fn open_keyed(path: &Path, key: &[u8; KEY_LEN], dir: &Path) -> Result<Connection, StoreError> {
    // No SQLITE_OPEN_CREATE: the file must exist already.
    let flags = OpenFlags::SQLITE_OPEN_READ_WRITE
        | OpenFlags::SQLITE_OPEN_NOFOLLOW
        | OpenFlags::SQLITE_OPEN_FULL_MUTEX;
    let keyed = native_store::open_keyed(path, flags, key, |conn| conn.busy_timeout(BUSY_TIMEOUT))
        .map_err(|e| match e {
            // NOTADB under every scheme; the error never carries key material.
            KeyError::WrongKey | KeyError::InvalidKey => StoreError::Locked,
            // Busy, I/O, a build that cannot encrypt: nothing about the key.
            KeyError::Open(_)
            | KeyError::Database(_)
            | KeyError::NotEncrypted
            | KeyError::LegacyHotJournal => StoreError::Database,
        })?;
    // A published database always has a schema; an empty one (truncated, say) would otherwise be
    // silently re-initialised. It says nothing about the key, so it is not `Locked`.
    let published = path.file_name().is_some_and(|n| n == DB_FILE);
    if keyed.tables == 0 && published {
        return Err(StoreError::Database);
    }
    // The published store runs in WAL mode: a crash leaves a WAL, which replays correctly, never a
    // hot rollback journal (see `dilla_core::native_store`). The staging file stays in rollback
    // mode, so it is one self-contained file when it is renamed into place.
    if published {
        enable_wal(&keyed.conn).map_err(|e| wal_error(&e, dir))?;
    }
    Ok(keyed.conn)
}

/// `native_store::use_wal` (and that it took), then `synchronous=FULL` as the Bot's store sets it, then a read of the
/// schema through the WAL index: a file system without shared memory fails here, at open, not at the first write.
fn enable_wal(conn: &Connection) -> Result<(), rusqlite::Error> {
    #[cfg(test)]
    if let Some(code) = tests::INJECT_WAL.with(std::cell::Cell::take) {
        return Err(rusqlite::Error::SqliteFailure(
            rusqlite::ffi::Error::new(code),
            None,
        ));
    }
    let mode: String = conn.query_row("PRAGMA journal_mode = WAL", [], |r| r.get(0))?;
    if !mode.eq_ignore_ascii_case("wal") {
        return Err(rusqlite::Error::SqliteFailure(
            rusqlite::ffi::Error::new(rusqlite::ffi::SQLITE_IOERR_SHMOPEN),
            Some(format!("journal mode stayed {mode}")),
        ));
    }
    conn.execute_batch("PRAGMA synchronous = FULL;")?;
    conn.query_row("SELECT count(*) FROM sqlite_master", [], |r| {
        r.get::<_, i64>(0)
    })?;
    Ok(())
}

/// A WAL failure: the shared-memory errors (SQLite's `SQLITE_IOERR_SHM*`, the answer of a file system that cannot
/// map the index) are [`StoreError::Unsupported`], naming the file system; anything else is `Database`.
fn wal_error(e: &rusqlite::Error, dir: &Path) -> StoreError {
    use rusqlite::ffi;
    let shm = matches!(
        e,
        rusqlite::Error::SqliteFailure(f, _) if matches!(
            f.extended_code,
            ffi::SQLITE_IOERR_SHMOPEN | ffi::SQLITE_IOERR_SHMSIZE | ffi::SQLITE_IOERR_SHMMAP
                | ffi::SQLITE_IOERR_SHMLOCK
        )
    );
    if !shm {
        return StoreError::Database;
    }
    let fs = file_system_of(dir).map_or_else(String::new, |t| format!(" ({t})"));
    StoreError::Unsupported(format!(
        "the file system{fs} holding {} cannot keep the local store: it has no shared memory for SQLite's \
         write-ahead log. Move the data directory to a local disk.",
        dir.display()
    ))
}

/// The type of the file system `dir` is on (Linux: the longest mount point over it in `/proc/self/mountinfo`), when
/// it can be told.
fn file_system_of(dir: &Path) -> Option<String> {
    #[cfg(target_os = "linux")]
    {
        let dir = fs::canonicalize(dir).ok()?;
        let info = fs::read_to_string("/proc/self/mountinfo").ok()?;
        let mut best: Option<(usize, String)> = None;
        for line in info.lines() {
            // `id parent major:minor root mount-point options [optional...] - fstype source super-options`
            let mut fields = line.split(' ');
            let mount = fields.nth(4)?.replace("\\040", " ");
            let fstype = line.split(" - ").nth(1)?.split(' ').next()?.to_owned();
            if dir.starts_with(&mount) && best.as_ref().is_none_or(|(len, _)| mount.len() >= *len) {
                best = Some((mount.len(), fstype));
            }
        }
        best.map(|(_, t)| t)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = dir;
        None
    }
}

fn remove_stage(dir: &Path) -> Result<(), StoreError> {
    for suffix in ["", "-journal", "-wal", "-shm"] {
        match fs::remove_file(dir.join(format!("{STAGE_FILE}{suffix}"))) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => return Err(e.into()),
        }
    }
    Ok(())
}

/// `instances/<n>.lock` for data dir `instances/<n>`: outside the data dir, so it holds only the store.
fn lock_path(data_dir: &Path) -> PathBuf {
    let mut name = data_dir.file_name().unwrap_or_default().to_os_string();
    name.push(".lock");
    data_dir.with_file_name(name)
}

/// Takes the instance's exclusive open lock, blocking until it is free. The lock file is never
/// deleted; see the module doc.
pub(crate) fn lock_instance(data_dir: &Path) -> Result<File, StoreError> {
    if let Some(parent) = data_dir.parent() {
        create_private_dir_all(parent)?;
    }
    let file = private_file_options()
        .create(true)
        .truncate(false)
        .open(lock_path(data_dir))?;
    file.lock()?;
    Ok(file)
}

fn private_file_options() -> OpenOptions {
    let mut options = OpenOptions::new();
    options.read(true).write(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    options
}

/// Creates `dir` and its missing parents, owner-only on Unix.
pub(crate) fn create_private_dir_all(dir: &Path) -> Result<(), StoreError> {
    let mut builder = fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        builder.mode(0o700);
    }
    builder.create(dir)?;
    Ok(())
}

/// Makes a rename or removal in `dir` durable (Unix; Windows has no directory handle to sync).
pub(crate) fn sync_dir(dir: &Path) -> Result<(), StoreError> {
    #[cfg(unix)]
    File::open(dir)?.sync_all()?;
    #[cfg(not(unix))]
    let _ = dir;
    Ok(())
}

#[cfg(test)]
mod tests {
    //! The Task 6b minors (screens plan Task 2): stale WAL files, durability, a file system without shared memory, and
    //! copied roots.
    use super::*;
    use crate::instances::Instances;
    use crate::keychain::MemoryKeychain;
    use std::sync::atomic::{AtomicU64, Ordering};

    thread_local! {
        /// An extended SQLite code the next WAL switch on this thread fails with.
        pub(super) static INJECT_WAL: std::cell::Cell<Option<i32>> = const { std::cell::Cell::new(None) };
    }

    static NEXT: AtomicU64 = AtomicU64::new(0);

    /// A fresh root under the workspace's target dir (not /tmp, which is small on the build hosts).
    fn root() -> PathBuf {
        let p = Path::new(env!("CARGO_MANIFEST_DIR")).join(format!(
            "../../../target/store-unit-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = fs::remove_dir_all(&p);
        p
    }

    fn server() -> url::Url {
        url::Url::parse("https://chat.example.org").unwrap()
    }

    fn holds(core: &ClientCore, needle: &str) -> bool {
        let cbor = core.settings().unwrap();
        cbor.windows(needle.len()).any(|w| w == needle.as_bytes())
    }

    fn copy_dir(from: &Path, to: &Path) {
        fs::create_dir_all(to).unwrap();
        for entry in fs::read_dir(from).unwrap() {
            let entry = entry.unwrap();
            let target = to.join(entry.file_name());
            if entry.file_type().unwrap().is_dir() {
                copy_dir(&entry.path(), &target);
            } else {
                fs::copy(entry.path(), target).unwrap();
            }
        }
    }

    #[test]
    fn absent_store_removes_stale_wal_shm_journal() {
        let root = root();
        let cfg = Instances::load(&root).unwrap().add(server()).unwrap();
        // A database deleted by hand (or by a crash between a removal's steps) leaves its log and index behind.
        create_private_dir_all(&cfg.data_dir).unwrap();
        let stale = b"stale bytes of a database that is gone";
        for suffix in ["-wal", "-shm", "-journal"] {
            fs::write(cfg.data_dir.join(format!("{DB_FILE}{suffix}")), stale).unwrap();
        }
        let kc = MemoryKeychain::new();
        let mut core = open_store(&cfg, &kc).unwrap();
        // While the new store is open, nothing of the old files is left: no journal, and the WAL and index are SQLite's.
        assert!(!cfg.data_dir.join(format!("{DB_FILE}-journal")).exists());
        for suffix in ["-wal", "-shm"] {
            let path = cfg.data_dir.join(format!("{DB_FILE}{suffix}"));
            if let Ok(bytes) = fs::read(&path) {
                assert!(
                    !bytes.starts_with(stale),
                    "{suffix} still holds the stale bytes"
                );
            }
        }
        core.setting_put("k", "fresh").unwrap();
        drop(core);
        assert!(holds(&open_store(&cfg, &kc).unwrap(), "fresh"));
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn store_sets_synchronous_full() {
        let root = root();
        let cfg = Instances::load(&root).unwrap().add(server()).unwrap();
        let kc = MemoryKeychain::new();
        // The first open (which builds the database) and a later one.
        for _ in 0..2 {
            let conn = open_connection(&cfg, &kc).unwrap();
            let conn = conn.lock().unwrap();
            let sync: i64 = conn
                .query_row("PRAGMA synchronous", [], |r| r.get(0))
                .unwrap();
            assert_eq!(sync, 2, "synchronous=FULL");
            let mode: String = conn
                .query_row("PRAGMA journal_mode", [], |r| r.get(0))
                .unwrap();
            assert_eq!(mode, "wal");
        }
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn wal_unsupported_filesystem_is_a_clear_error() {
        let root = root();
        let cfg = Instances::load(&root).unwrap().add(server()).unwrap();
        let kc = MemoryKeychain::new();
        INJECT_WAL.with(|c| c.set(Some(rusqlite::ffi::SQLITE_IOERR_SHMOPEN)));
        let err = match open_store(&cfg, &kc) {
            Err(e) => e,
            Ok(_) => panic!("an injected SQLITE_IOERR_SHMOPEN opened the store"),
        };
        let StoreError::Unsupported(message) = &err else {
            panic!("expected Unsupported, got {err:?}");
        };
        assert_eq!(err.to_string(), *message);
        let dir = fs::canonicalize(&cfg.data_dir).unwrap();
        assert!(
            message.contains(&cfg.data_dir.display().to_string())
                || message.contains(&dir.display().to_string()),
            "{message}"
        );
        if let Some(fs_type) = file_system_of(&cfg.data_dir) {
            assert!(message.contains(&format!("({fs_type})")), "{message}");
        }
        assert!(message.contains("local disk"), "{message}");
        // Any other failure of the switch stays a plain database error, and the store opens once WAL works.
        INJECT_WAL.with(|c| c.set(Some(rusqlite::ffi::SQLITE_IOERR_READ)));
        assert!(matches!(open_store(&cfg, &kc), Err(StoreError::Database)));
        open_store(&cfg, &kc).unwrap();
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn copied_data_root_does_not_share_keys() {
        let kc = MemoryKeychain::new();
        let root_a = root();
        let mut a = Instances::load(&root_a).unwrap();
        let cfg_a = a.add(server()).unwrap();
        let mut core = open_store(&cfg_a, &kc).unwrap();
        core.setting_put("k", "before the copy").unwrap();
        drop(core);
        // The root is copied whole (a restored backup beside the original): same install id, same files.
        let root_b = root();
        copy_dir(&root_a, &root_b);
        let mut b = Instances::load(&root_b).unwrap();
        assert_eq!(a.install_id(), b.install_id());
        let cfg_b = b.get(cfg_a.id).unwrap().clone();
        let core = open_store(&cfg_b, &kc).unwrap();
        assert!(holds(&core, "before the copy"), "the copy keeps its data");
        drop(core);
        let (acct_a, acct_b) = (
            store_keychain_account(&cfg_a).unwrap(),
            store_keychain_account(&cfg_b).unwrap(),
        );
        assert_ne!(acct_a, acct_b, "the copy holds its key under its own entry");
        assert_eq!(acct_a, keychain_account(a.install_id(), cfg_a.id));

        // The original removes and re-adds its instance: its fresh key replaces its own entry only (a crypto-erase).
        let old_a = kc.get(&acct_a).unwrap().unwrap();
        a.remove(cfg_a.id).unwrap();
        let cfg_a = a.add(server()).unwrap();
        let mut core = open_store(&cfg_a, &kc).unwrap();
        core.setting_put("k", "the original's new store").unwrap();
        drop(core);
        assert_eq!(store_keychain_account(&cfg_a).unwrap(), acct_a);
        assert_ne!(*kc.get(&acct_a).unwrap().unwrap(), *old_a);
        // The copy still opens, with its own data.
        let core = open_store(&cfg_b, &kc).unwrap();
        assert!(holds(&core, "before the copy"));
        drop(core);

        // And the other way round: the copy's re-created store never touches the original's key.
        let key_a = kc.get(&acct_a).unwrap().unwrap();
        b.remove(cfg_b.id).unwrap();
        let cfg_b = b.add(server()).unwrap();
        drop(open_store(&cfg_b, &kc).unwrap());
        assert_ne!(store_keychain_account(&cfg_b).unwrap(), acct_a);
        assert_eq!(*kc.get(&acct_a).unwrap().unwrap(), *key_a);
        assert!(holds(
            &open_store(&cfg_a, &kc).unwrap(),
            "the original's new store"
        ));
        let _ = fs::remove_dir_all(&root_a);
        let _ = fs::remove_dir_all(&root_b);
    }
}

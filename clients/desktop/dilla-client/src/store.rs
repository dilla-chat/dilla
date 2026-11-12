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
//! and is deleted before anything else. Opens and removals of the same instance are serialised by an
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
    #[error("the local store refused to open: {0:?}")]
    Core(ClientError),
    #[error("file system error: {0}")]
    Io(#[from] std::io::Error),
}

/// The keychain account that holds instance `id`'s database key, scoped to its root's install id
/// ([`crate::instances::Instances::install_id`]).
pub fn keychain_account(install_id: &str, id: InstanceId) -> String {
    format!("{install_id}/instance-{}", id.0)
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
    let account = keychain_account(&install_id, cfg.id);
    // Ask the keychain before touching the disk: without it nothing is written.
    drop(read_key(kc, &account)?);

    let _lock = lock_instance(&cfg.data_dir)?;
    // Under the lock: the instance may have been removed, or the list replaced, meanwhile.
    if install_id_for(&cfg.data_dir, cfg.id)? != install_id {
        return Err(StoreError::CorruptInstanceList);
    }
    // Read again under the lock: another process may have finished a first open meanwhile.
    let stored = read_key(kc, &account)?;
    create_private_dir_all(&cfg.data_dir)?;
    remove_stage(&cfg.data_dir)?;

    let db = cfg.data_dir.join(DB_FILE);
    let exists = match fs::symlink_metadata(&db) {
        Ok(_) => true,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => false,
        Err(e) => return Err(e.into()),
    };
    if exists {
        let key = stored.ok_or(StoreError::KeyMissing)?;
        let key = as_key(&key).ok_or(StoreError::Locked)?;
        return Ok(Arc::new(Mutex::new(open_keyed(&db, &key)?)));
    }

    // No database: whatever the keychain holds (a first open cut short, or a removed instance on
    // this id) protects nothing. Mint a fresh key, store it over the old one and check it reads back.
    drop(stored);
    let key = new_key()?;
    kc.set(&account, key.as_slice())
        .map_err(|_| StoreError::KeychainUnavailable)?;
    let back = read_key(kc, &account)?.ok_or(StoreError::KeychainUnavailable)?;
    if back.as_slice() != key.as_slice() {
        return Err(StoreError::KeychainUnavailable);
    }
    create_database(&cfg.data_dir, &db, &key)?;
    Ok(Arc::new(Mutex::new(open_keyed(&db, &key)?)))
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
    let core = ClientCore::open(Arc::new(Mutex::new(open_keyed(&stage, key)?)))
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
fn open_keyed(path: &Path, key: &[u8; KEY_LEN]) -> Result<Connection, StoreError> {
    // No SQLITE_OPEN_CREATE: the file must exist already.
    let flags = OpenFlags::SQLITE_OPEN_READ_WRITE
        | OpenFlags::SQLITE_OPEN_NOFOLLOW
        | OpenFlags::SQLITE_OPEN_FULL_MUTEX;
    let keyed = native_store::open_keyed(path, flags, key, |conn| conn.busy_timeout(BUSY_TIMEOUT))
        .map_err(|e| match e {
            // NOTADB under every scheme; the error never carries key material.
            KeyError::WrongKey | KeyError::InvalidKey => StoreError::Locked,
            // Busy, I/O, a build that cannot encrypt: nothing about the key.
            KeyError::Open(_) | KeyError::Database(_) | KeyError::NotEncrypted => {
                StoreError::Database
            }
        })?;
    // A published database always has a schema; an empty one (truncated, say) would otherwise be
    // silently re-initialised. It says nothing about the key, so it is not `Locked`.
    if keyed.tables == 0 && path.file_name().is_some_and(|n| n == DB_FILE) {
        return Err(StoreError::Database);
    }
    Ok(keyed.conn)
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

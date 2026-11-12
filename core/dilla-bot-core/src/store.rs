#![allow(unsafe_code)] // geteuid is the Linux ownership check for protected state files.

use rusqlite::{Connection, OpenFlags, params};
use std::{
    fs::{self, File, OpenOptions},
    io::{Read, Write},
    os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt},
    path::{Component, Path, PathBuf},
    sync::{Arc, Mutex},
};

const STORE_VERSION: i64 = 1;
const INIT_MAGIC: &[u8] = b"DILLA-BOT-INIT-v1\0";

pub(crate) fn cbor_shape() -> dilla_core::cbor::CborError {
    dilla_core::cbor::CborError::TypeMismatch {
        expected: "state transaction",
        offset: 0,
    }
}

#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    #[error("state directory is invalid or insecure")]
    Directory,
    #[error("state file is invalid or insecure")]
    File,
    #[error("state is locked by another helper")]
    Locked,
    #[error("database key is invalid or database cannot be decrypted")]
    Key,
    #[error("stored identity binding does not match")]
    Binding,
    #[error("state store version is unsupported")]
    Version,
    #[error("database operation failed")]
    Database,
}

pub struct Store {
    pub(crate) conn: dilla_core::mls::ConnHandle,
    // Retain the verified private directory handle for the store lifetime.
    _dir: File,
    _lock: File,
}

impl Store {
    pub fn open(
        dir: &Path,
        instance: [u8; 16],
        community: [u8; 16],
        installation: [u8; 16],
        key: &[u8; 32],
    ) -> Result<Self, StoreError> {
        if key.iter().all(|b| *b == 0) {
            return Err(StoreError::Key);
        }
        let dir_fd = secure_dir(dir)?;
        let lock_path = dir.join("state.lock");
        let (lock, _) = secure_file(&lock_path)?;
        lock.try_lock().map_err(|_| StoreError::Locked)?;
        let db_path = dir.join("state.db");
        let binding = (instance, community, installation);
        let conn = match fs::symlink_metadata(&db_path) {
            Ok(_) => {
                let conn = open_canonical(&db_path, key)?;
                validate_binding(&conn, binding)?;
                cleanup_published_stage(dir, &db_path, binding)?;
                conn.execute_batch(
                    "PRAGMA secure_delete=ON; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;",
                )
                .map_err(|_| StoreError::Database)?;
                conn
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                publish_new_store(dir, &db_path, binding, key)?;
                let conn = open_canonical(&db_path, key)?;
                validate_binding(&conn, binding)?;
                conn.execute_batch(
                    "PRAGMA secure_delete=ON; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;",
                )
                .map_err(|_| StoreError::Database)?;
                conn
            }
            Err(_) => return Err(StoreError::File),
        };
        ensure_command_tables(&conn)?;
        Ok(Self {
            conn: Arc::new(Mutex::new(conn)),
            _dir: dir_fd,
            _lock: lock,
        })
    }

    pub(crate) fn bind(&self) -> ([u8; 16], [u8; 16], [u8; 16]) {
        let conn = self.conn.lock().expect("single-threaded store lock");
        let read = |k: &str| {
            conn.query_row("SELECT v FROM bot_helper_meta WHERE k=?1", [k], |r| {
                r.get::<_, Vec<u8>>(0)
            })
            .expect("validated metadata")
        };
        (
            read("instance").try_into().expect("validated instance"),
            read("community").try_into().expect("validated community"),
            read("installation")
                .try_into()
                .expect("validated installation"),
        )
    }
}

fn open_canonical(path: &Path, key: &[u8; 32]) -> Result<Connection, StoreError> {
    let (_, created) = secure_file(path)?;
    if created {
        return Err(StoreError::File);
    }
    for suffix in ["-wal", "-shm", "-journal"] {
        check_optional_sidecar(&with_suffix(path, suffix))?;
    }
    let (conn, _) = open_keyed(path, key)?;
    Ok(conn)
}

fn open_keyed(path: &Path, key: &[u8; 32]) -> Result<(Connection, i64), StoreError> {
    let flags = OpenFlags::SQLITE_OPEN_READ_WRITE
        | OpenFlags::SQLITE_OPEN_NOFOLLOW
        | OpenFlags::SQLITE_OPEN_FULL_MUTEX;
    // dilla-core keys the file (ChaCha20; a store the SQLCipher build wrote before Task 6b opens
    // through the SQLCipher 4 scheme and stays in it) and proves the key with a schema read.
    match dilla_core::native_store::open_keyed(path, flags, key, |_| Ok(())) {
        Ok(keyed) => Ok((keyed.conn, keyed.tables)),
        Err(dilla_core::native_store::KeyError::Open(_)) => Err(StoreError::Database),
        Err(_) => Err(StoreError::Key),
    }
}

fn validate_binding(
    conn: &Connection,
    (instance, community, installation): ([u8; 16], [u8; 16], [u8; 16]),
) -> Result<(), StoreError> {
    let has_meta: bool = conn
        .query_row(
            "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='bot_helper_meta')",
            [],
            |r| r.get(0),
        )
        .map_err(|_| StoreError::Database)?;
    if !has_meta {
        return Err(StoreError::Binding);
    }
    let version: i64 = conn
        .query_row(
            "SELECT CAST(v AS INTEGER) FROM bot_helper_meta WHERE k='version'",
            [],
            |r| r.get(0),
        )
        .map_err(|_| StoreError::Version)?;
    if version != STORE_VERSION {
        return Err(StoreError::Version);
    }
    let found: (Vec<u8>, Vec<u8>, Vec<u8>) = conn
        .query_row("SELECT (SELECT v FROM bot_helper_meta WHERE k='instance'),(SELECT v FROM bot_helper_meta WHERE k='community'),(SELECT v FROM bot_helper_meta WHERE k='installation')", [], |r| Ok((r.get(0)?,r.get(1)?,r.get(2)?)))
        .map_err(|_| StoreError::Binding)?;
    if found.0.as_slice() != instance
        || found.1.as_slice() != community
        || found.2.as_slice() != installation
    {
        return Err(StoreError::Binding);
    }
    for name in ["bot_state", "bot_state_receipts"] {
        let exists: bool = conn
            .query_row(
                "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?1)",
                [name],
                |r| r.get(0),
            )
            .map_err(|_| StoreError::Database)?;
        if !exists {
            return Err(StoreError::Version);
        }
    }
    Ok(())
}

/// The sealed-command tables (Bot v2 task 8), additive on a validated store: no version change,
/// and a store that never sees a command only gains two empty tables.
///
/// `bot_command_keys` holds the private command keys. It is its own table, not a `bot_state`
/// area, because state areas are read and written by the Go side (ops 11 and 12); this table is
/// reachable only through ops 21 and 22. At most one current key (`retired_at` NULL) per
/// community, and one retired key until it is deleted 24 h after its retirement.
///
/// `command_replay` holds the request ids of opened, verified commands for 5 minutes;
/// `command_replay_floor` holds the highest prune bound ever applied, so a command old enough to
/// have been pruned is refused whatever the caller's clock does later.
///
/// `bot_device_list_behind` marks users whose pushed device list did not chain from the cache:
/// their commands are refused until a history reaching `head` is ingested.
///
/// The key boundary is a protocol boundary, not a privilege boundary: no helper op returns or
/// accepts a private command key, but the Go process holds the store key (`database.key`) and
/// owns the state directory, so a compromised Go process can read this table directly, as it
/// can the DSK and MLS secrets in the same store.
fn ensure_command_tables(conn: &Connection) -> Result<(), StoreError> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS bot_command_keys (\
            key_id BLOB PRIMARY KEY, \
            community_id BLOB NOT NULL, \
            created_at INTEGER NOT NULL, \
            retired_at INTEGER, \
            private_key BLOB NOT NULL, \
            publication BLOB NOT NULL); \
         CREATE UNIQUE INDEX IF NOT EXISTS bot_command_keys_current \
            ON bot_command_keys(community_id) WHERE retired_at IS NULL; \
         CREATE INDEX IF NOT EXISTS bot_command_keys_retired \
            ON bot_command_keys(retired_at) WHERE retired_at IS NOT NULL; \
         CREATE TABLE IF NOT EXISTS command_replay (\
            request_id BLOB PRIMARY KEY, \
            seen_at INTEGER NOT NULL) WITHOUT ROWID; \
         CREATE INDEX IF NOT EXISTS command_replay_seen ON command_replay(seen_at); \
         CREATE TABLE IF NOT EXISTS command_replay_floor (\
            id INTEGER PRIMARY KEY CHECK (id = 1), \
            floor INTEGER NOT NULL); \
         CREATE TABLE IF NOT EXISTS bot_device_list_behind (\
            user_id BLOB PRIMARY KEY, \
            head INTEGER NOT NULL) WITHOUT ROWID;",
    )
    .map_err(|_| StoreError::Database)
}

fn initialize_schema(
    conn: &mut Connection,
    (instance, community, installation): ([u8; 16], [u8; 16], [u8; 16]),
) -> Result<(), StoreError> {
    conn.execute_batch("PRAGMA secure_delete=ON; PRAGMA synchronous=FULL;")
        .map_err(|_| StoreError::Database)?;
    let tx = conn
        .transaction_with_behavior(rusqlite::TransactionBehavior::Immediate)
        .map_err(|_| StoreError::Database)?;
    tx.execute_batch("CREATE TABLE bot_helper_meta (k TEXT PRIMARY KEY, v BLOB NOT NULL); CREATE TABLE bot_state (area INTEGER NOT NULL, key BLOB NOT NULL, revision INTEGER NOT NULL, value BLOB, PRIMARY KEY(area,key)); CREATE TABLE bot_state_receipts (id BLOB PRIMARY KEY, digest BLOB NOT NULL, result BLOB NOT NULL);")
        .map_err(|_| StoreError::Database)?;
    tx.execute("INSERT INTO bot_helper_meta(k,v) VALUES('version',?1),('instance',?2),('community',?3),('installation',?4)", params![STORE_VERSION, instance.as_slice(), community.as_slice(), installation.as_slice()]).map_err(|_| StoreError::Database)?;
    tx.commit().map_err(|_| StoreError::Database)
}

fn publish_new_store(
    dir: &Path,
    canonical: &Path,
    binding: ([u8; 16], [u8; 16], [u8; 16]),
    key: &[u8; 32],
) -> Result<(), StoreError> {
    let marker = dir.join("state.init");
    let stage = match fs::symlink_metadata(&marker) {
        Ok(_) => {
            let nonce = read_init_marker(&marker, binding)?;
            let stage = stage_path(dir, nonce);
            if fs::symlink_metadata(&stage).is_err() {
                let (file, created) = secure_file(&stage)?;
                if !created {
                    return Err(StoreError::File);
                }
                file.sync_all().map_err(|_| StoreError::File)?;
                sync_dir(dir)?;
            }
            stage
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            create_init_stage(dir, marker.as_path(), binding)?
        }
        Err(_) => return Err(StoreError::File),
    };
    refuse_existing(&with_suffix(&stage, "-wal"))?;
    refuse_existing(&with_suffix(&stage, "-shm"))?;
    check_optional_sidecar(&with_suffix(&stage, "-journal"))?;
    let (stage_file, created) = secure_file(&stage)?;
    if created {
        stage_file.sync_all().map_err(|_| StoreError::File)?;
        sync_dir(dir)?;
    }
    drop(stage_file);

    let (mut conn, schema_count) = open_keyed(&stage, key)?;
    if schema_count == 0 {
        initialize_schema(&mut conn, binding)?;
    } else {
        validate_binding(&conn, binding)?;
    }
    let journal_mode: String = conn
        .query_row("PRAGMA journal_mode", [], |r| r.get(0))
        .map_err(|_| StoreError::Database)?;
    if journal_mode != "delete" {
        return Err(StoreError::File);
    }
    drop(conn);
    refuse_existing(&with_suffix(&stage, "-journal"))?;

    File::open(&stage)
        .and_then(|file| file.sync_all())
        .map_err(|_| StoreError::File)?;
    sync_dir(dir)?;
    fs::hard_link(&stage, canonical).map_err(|_| StoreError::File)?;
    sync_dir(dir)?;
    fs::remove_file(&stage).map_err(|_| StoreError::File)?;
    sync_dir(dir)?;
    fs::remove_file(marker).map_err(|_| StoreError::File)?;
    sync_dir(dir)?;
    Ok(())
}

fn create_init_stage(
    dir: &Path,
    marker: &Path,
    (instance, community, installation): ([u8; 16], [u8; 16], [u8; 16]),
) -> Result<PathBuf, StoreError> {
    let mut random = File::open("/dev/urandom").map_err(|_| StoreError::File)?;
    let (nonce, stage, stage_file) = loop {
        // This is only a read buffer; read_exact replaces every byte from the OS CSPRNG.
        let mut nonce = [0u8; 16];
        random
            .read_exact(&mut nonce)
            .map_err(|_| StoreError::File)?;
        if nonce.iter().all(|b| *b == 0) {
            continue;
        }
        let stage = stage_path(dir, nonce);
        match OpenOptions::new()
            .read(true)
            .write(true)
            .create_new(true)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
            .open(&stage)
        {
            Ok(file) => break (nonce, stage, file),
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(_) => return Err(StoreError::File),
        }
    };
    stage_file.sync_all().map_err(|_| StoreError::File)?;
    sync_dir(dir)?;

    let mut contents = Vec::with_capacity(INIT_MAGIC.len() + 64);
    contents.extend_from_slice(INIT_MAGIC);
    contents.extend_from_slice(&STORE_VERSION.to_be_bytes());
    contents.extend_from_slice(&instance);
    contents.extend_from_slice(&community);
    contents.extend_from_slice(&installation);
    contents.extend_from_slice(&nonce);
    let tmp = dir.join(format!("state.init-tmp-{}", nonce_hex(nonce)));
    let mut marker_tmp = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(&tmp)
        .map_err(|_| StoreError::File)?;
    marker_tmp
        .write_all(&contents)
        .and_then(|_| marker_tmp.sync_all())
        .map_err(|_| StoreError::File)?;
    fs::hard_link(&tmp, marker).map_err(|_| StoreError::File)?;
    sync_dir(dir)?;
    fs::remove_file(tmp).map_err(|_| StoreError::File)?;
    sync_dir(dir)?;
    Ok(stage)
}

fn read_init_marker(
    marker: &Path,
    (instance, community, installation): ([u8; 16], [u8; 16], [u8; 16]),
) -> Result<[u8; 16], StoreError> {
    let (mut file, created) = secure_file(marker)?;
    if created {
        return Err(StoreError::File);
    }
    let expected_len = INIT_MAGIC.len() + 8 + 16 * 4;
    if file.metadata().map_err(|_| StoreError::File)?.len() != expected_len as u64 {
        return Err(StoreError::File);
    }
    let mut contents = vec![0; expected_len];
    file.read_exact(&mut contents)
        .map_err(|_| StoreError::File)?;
    if contents.len() != expected_len
        || &contents[..INIT_MAGIC.len()] != INIT_MAGIC
        || i64::from_be_bytes(
            contents[INIT_MAGIC.len()..INIT_MAGIC.len() + 8]
                .try_into()
                .unwrap(),
        ) != STORE_VERSION
    {
        return Err(StoreError::File);
    }
    let mut cursor = INIT_MAGIC.len() + 8;
    for expected in [instance, community, installation] {
        if contents[cursor..cursor + 16] != expected {
            return Err(StoreError::Binding);
        }
        cursor += 16;
    }
    let nonce: [u8; 16] = contents[cursor..cursor + 16].try_into().unwrap();
    if nonce.iter().all(|b| *b == 0) {
        return Err(StoreError::File);
    }
    Ok(nonce)
}

fn stage_path(dir: &Path, nonce: [u8; 16]) -> PathBuf {
    dir.join(format!("state.db.init-{}", nonce_hex(nonce)))
}

fn nonce_hex(nonce: [u8; 16]) -> String {
    let mut out = String::with_capacity(32);
    for b in nonce {
        use std::fmt::Write;
        let _ = write!(out, "{b:02x}");
    }
    out
}

fn cleanup_published_stage(
    dir: &Path,
    canonical: &Path,
    binding: ([u8; 16], [u8; 16], [u8; 16]),
) -> Result<(), StoreError> {
    let marker = dir.join("state.init");
    match fs::symlink_metadata(&marker) {
        Ok(_) => {}
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(_) => return Err(StoreError::File),
    }
    let nonce = read_init_marker(&marker, binding)?;
    let stage = stage_path(dir, nonce);
    match fs::symlink_metadata(&stage) {
        Ok(_) => {
            let canonical_meta = fs::symlink_metadata(canonical).map_err(|_| StoreError::File)?;
            let stage_meta = fs::symlink_metadata(&stage).map_err(|_| StoreError::File)?;
            if canonical_meta.dev() != stage_meta.dev() || canonical_meta.ino() != stage_meta.ino()
            {
                return Err(StoreError::File);
            }
            fs::remove_file(&stage).map_err(|_| StoreError::File)?;
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(_) => return Err(StoreError::File),
    }
    fs::remove_file(marker).map_err(|_| StoreError::File)?;
    sync_dir(dir)
}

fn sync_dir(dir: &Path) -> Result<(), StoreError> {
    File::open(dir)
        .and_then(|file| file.sync_all())
        .map_err(|_| StoreError::File)
}

fn with_suffix(path: &Path, suffix: &str) -> PathBuf {
    let mut name = path.as_os_str().to_os_string();
    name.push(suffix);
    PathBuf::from(name)
}

fn secure_dir(path: &Path) -> Result<File, StoreError> {
    if !path.is_absolute() {
        return Err(StoreError::Directory);
    }
    let mut cursor = PathBuf::new();
    for component in path.components() {
        match component {
            Component::RootDir => cursor.push("/"),
            Component::Normal(p) => {
                cursor.push(p);
                let m = match fs::symlink_metadata(&cursor) {
                    Ok(m) => m,
                    Err(e) if e.kind() == std::io::ErrorKind::NotFound => continue,
                    Err(_) => return Err(StoreError::Directory),
                };
                if m.file_type().is_symlink() || !m.is_dir() {
                    return Err(StoreError::Directory);
                }
                // A sticky shared directory such as /tmp is safe for a private
                // child; other writable ancestors permit path replacement.
                if m.mode() & 0o022 != 0 && m.mode() & 0o1000 == 0 {
                    return Err(StoreError::Directory);
                }
            }
            Component::CurDir => {}
            _ => return Err(StoreError::Directory),
        }
    }
    if !path.exists() {
        fs::DirBuilder::new()
            .mode(0o700)
            .create(path)
            .map_err(|_| StoreError::Directory)?;
    }
    let m = fs::symlink_metadata(path).map_err(|_| StoreError::Directory)?;
    if !m.is_dir()
        || m.file_type().is_symlink()
        || m.uid() != unsafe { libc::geteuid() }
        || m.mode() & 0o777 != 0o700
    {
        return Err(StoreError::Directory);
    }
    let directory = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(path)
        .map_err(|_| StoreError::Directory)?;
    let m = directory.metadata().map_err(|_| StoreError::Directory)?;
    if !m.is_dir() || m.uid() != unsafe { libc::geteuid() } || m.mode() & 0o777 != 0o700 {
        return Err(StoreError::Directory);
    }
    Ok(directory)
}

fn secure_file(path: &Path) -> Result<(File, bool), StoreError> {
    let (file, created) = match OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .mode(0o600)
        .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(path)
    {
        Ok(f) => (f, true),
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => (
            OpenOptions::new()
                .read(true)
                .write(true)
                .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC)
                .open(path)
                .map_err(|_| StoreError::File)?,
            false,
        ),
        Err(_) => return Err(StoreError::File),
    };
    let m = file.metadata().map_err(|_| StoreError::File)?;
    if !m.is_file() || m.uid() != unsafe { libc::geteuid() } || m.mode() & 0o777 != 0o600 {
        return Err(StoreError::File);
    }
    Ok((file, created))
}

fn check_optional_sidecar(path: &Path) -> Result<(), StoreError> {
    match fs::symlink_metadata(path) {
        Ok(m)
            if m.file_type().is_symlink()
                || !m.is_file()
                || m.uid() != unsafe { libc::geteuid() }
                || m.mode() & 0o777 != 0o600 =>
        {
            Err(StoreError::File)
        }
        Ok(_) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(_) => Err(StoreError::File),
    }
}

fn refuse_existing(path: &Path) -> Result<(), StoreError> {
    match fs::symlink_metadata(path) {
        Ok(_) => Err(StoreError::File),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(_) => Err(StoreError::File),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        os::unix::fs::PermissionsExt,
        sync::atomic::{AtomicU64, Ordering},
    };
    static NEXT: AtomicU64 = AtomicU64::new(0);
    fn dir() -> PathBuf {
        let p = std::env::temp_dir().join(format!(
            "dilla-store-{}-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        fs::create_dir(&p).unwrap();
        fs::set_permissions(&p, fs::Permissions::from_mode(0o700)).unwrap();
        p
    }
    fn open(p: &Path) -> Result<Store, StoreError> {
        Store::open(p, [1; 16], [2; 16], [3; 16], &[0x41; 32])
    }

    #[test]
    fn store_lock_and_identity_binding_are_exclusive() {
        let p = dir();
        let first = open(&p).unwrap();
        assert!(matches!(open(&p), Err(StoreError::Locked)));
        drop(first);
        drop(open(&p).unwrap());
        assert!(matches!(
            Store::open(&p, [1; 16], [9; 16], [3; 16], &[0x41; 32]),
            Err(StoreError::Binding)
        ));
        let _ = fs::remove_dir_all(p);
    }

    #[test]
    fn absent_state_directory_is_created_with_private_mode() {
        let parent = dir();
        let path = parent.join("helper-state");
        let s = open(&path).unwrap();
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o700
        );
        drop(s);
        let _ = fs::remove_dir_all(parent);
    }

    #[test]
    fn insecure_directory_database_sidecar_and_symlink_are_refused() {
        let p = dir();
        fs::set_permissions(&p, fs::Permissions::from_mode(0o711)).unwrap();
        assert!(matches!(open(&p), Err(StoreError::Directory)));
        let _ = fs::remove_dir_all(p);
        let p = dir();
        let db = p.join("state.db");
        fs::write(&db, b"untrusted").unwrap();
        fs::set_permissions(&db, fs::Permissions::from_mode(0o644)).unwrap();
        assert!(matches!(open(&p), Err(StoreError::File)));
        let _ = fs::remove_dir_all(p);
        let p = dir();
        let wal = p.join("state.db-wal");
        fs::write(&wal, b"untrusted").unwrap();
        fs::set_permissions(&wal, fs::Permissions::from_mode(0o644)).unwrap();
        assert!(matches!(open(&p), Err(StoreError::File)));
        let _ = fs::remove_dir_all(p);
        let p = dir();
        let shm = p.join("state.db-shm");
        fs::write(&shm, b"untrusted").unwrap();
        fs::set_permissions(&shm, fs::Permissions::from_mode(0o644)).unwrap();
        assert!(matches!(open(&p), Err(StoreError::File)));
        let _ = fs::remove_dir_all(p);
        let p = dir();
        let lock = p.join("state.lock");
        fs::write(&lock, b"untrusted").unwrap();
        fs::set_permissions(&lock, fs::Permissions::from_mode(0o644)).unwrap();
        assert!(matches!(open(&p), Err(StoreError::File)));
        let _ = fs::remove_dir_all(p);
        let p = dir();
        let target = p.join("elsewhere");
        fs::write(&target, b"untrusted").unwrap();
        fs::set_permissions(&target, fs::Permissions::from_mode(0o600)).unwrap();
        std::os::unix::fs::symlink(&target, p.join("state.db")).unwrap();
        assert!(matches!(open(&p), Err(StoreError::File)));
        let _ = fs::remove_dir_all(p);
    }

    #[test]
    fn preexisting_unbound_encrypted_database_is_not_adopted() {
        let p = dir();
        let db = p.join("state.db");
        let c = Connection::open(&db).unwrap();
        c.execute_batch("PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:4141414141414141414141414141414141414141414141414141414141414141'; CREATE TABLE foreign_state(value BLOB);").unwrap();
        drop(c);
        fs::set_permissions(&db, fs::Permissions::from_mode(0o600)).unwrap();
        assert!(matches!(open(&p), Err(StoreError::Binding)));
        let verify = Connection::open(&db).unwrap();
        verify.execute_batch("PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:4141414141414141414141414141414141414141414141414141414141414141'").unwrap();
        assert_eq!(
            verify
                .query_row(
                    "SELECT count(*) FROM sqlite_schema WHERE name='foreign_state'",
                    [],
                    |r| r.get::<_, i64>(0)
                )
                .unwrap(),
            1
        );
        drop(verify);
        let _ = fs::remove_dir_all(p);
    }

    #[test]
    fn preexisting_plaintext_database_is_refused_without_modification() {
        use sha2::{Digest, Sha256};

        let p = dir();
        let db = p.join("state.db");
        let c = Connection::open(&db).unwrap();
        c.execute_batch("CREATE TABLE plaintext_marker(value TEXT); INSERT INTO plaintext_marker VALUES('keep');")
            .unwrap();
        drop(c);
        fs::set_permissions(&db, fs::Permissions::from_mode(0o600)).unwrap();
        let before = Sha256::digest(fs::read(&db).unwrap()).to_vec();
        assert!(matches!(
            open(&p),
            Err(StoreError::Key | StoreError::Binding)
        ));
        assert_eq!(Sha256::digest(fs::read(&db).unwrap()).as_slice(), before);
        let verify = Connection::open(&db).unwrap();
        assert_eq!(
            verify
                .query_row("SELECT value FROM plaintext_marker", [], |r| r
                    .get::<_, String>(0))
                .unwrap(),
            "keep"
        );
        drop(verify);
        let _ = fs::remove_dir_all(p);
    }

    #[test]
    fn refused_version_and_binding_leave_database_bytes_and_journal_mode_untouched() {
        use sha2::{Digest, Sha256};

        fn hash(path: &Path) -> Vec<u8> {
            Sha256::digest(fs::read(path).unwrap()).to_vec()
        }
        fn assert_unchanged(change: &str, expected: StoreError) {
            let p = dir();
            drop(open(&p).unwrap());
            let db = p.join("state.db");
            {
                let c = Connection::open(&db).unwrap();
                c.execute_batch("PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:4141414141414141414141414141414141414141414141414141414141414141'; PRAGMA journal_mode=DELETE;")
                    .unwrap();
                c.execute_batch(change).unwrap();
            }
            let before = hash(&db);
            assert!(
                matches!(open(&p), Err(e) if std::mem::discriminant(&e) == std::mem::discriminant(&expected))
            );
            assert_eq!(hash(&db), before);
            let verify = Connection::open(&db).unwrap();
            verify.execute_batch("PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:4141414141414141414141414141414141414141414141414141414141414141'").unwrap();
            let mode: String = verify
                .query_row("PRAGMA journal_mode", [], |r| r.get(0))
                .unwrap();
            assert_eq!(mode, "delete");
            drop(verify);
            let _ = fs::remove_dir_all(p);
        }

        assert_unchanged(
            "UPDATE bot_helper_meta SET v=99 WHERE k='version'",
            StoreError::Version,
        );
        assert_unchanged(
            "UPDATE bot_helper_meta SET v=zeroblob(16) WHERE k='community'",
            StoreError::Binding,
        );
    }

    /// A state directory holding a store the SQLCipher 4 build wrote (binding 1/2/3, key
    /// 0x41 * 32; `tests/fixtures/sqlcipher4/README.md`), copied with the modes a Bot leaves.
    fn legacy_state(variant: &str) -> PathBuf {
        let p = dir();
        let src = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("tests/fixtures/sqlcipher4")
            .join(variant);
        for name in ["state.db", "state.db-wal"] {
            if src.join(name).exists() {
                fs::copy(src.join(name), p.join(name)).unwrap();
                fs::set_permissions(p.join(name), fs::Permissions::from_mode(0o600)).unwrap();
            }
        }
        p
    }

    fn fixture_value(s: &Store) -> Vec<u8> {
        s.conn
            .lock()
            .unwrap()
            .query_row(
                "SELECT value FROM bot_state WHERE area=1 AND key=?1",
                [b"fixture-key".as_slice()],
                |r| r.get(0),
            )
            .unwrap()
    }

    fn cipher_on_disk(p: &Path) -> dilla_core::native_store::StoreCipher {
        dilla_core::native_store::open_keyed(
            &p.join("state.db"),
            OpenFlags::SQLITE_OPEN_READ_WRITE,
            &[0x41; 32],
            |_| Ok(()),
        )
        .unwrap()
        .cipher
    }

    #[test]
    fn a_store_the_sqlcipher_build_wrote_opens_and_stays_sqlcipher() {
        use dilla_core::native_store::StoreCipher;
        use sha2::{Digest, Sha256};

        for variant in ["clean", "hot-wal"] {
            let p = legacy_state(variant);
            let hash = |name: &str| Sha256::digest(fs::read(p.join(name)).unwrap_or_default());
            let before = (hash("state.db"), hash("state.db-wal"));
            assert!(
                matches!(
                    Store::open(&p, [1; 16], [2; 16], [3; 16], &[0x24; 32]),
                    Err(StoreError::Key)
                ),
                "{variant}: a wrong key opened the legacy store"
            );
            assert_eq!(
                (hash("state.db"), hash("state.db-wal")),
                before,
                "{variant}: a refused open changed the files"
            );
            assert!(matches!(
                Store::open(&p, [1; 16], [9; 16], [3; 16], &[0x41; 32]),
                Err(StoreError::Binding)
            ));

            let s = open(&p).unwrap_or_else(|e| panic!("{variant}: {e:?}"));
            assert_eq!(s.bind(), ([1; 16], [2; 16], [3; 16]));
            assert_eq!(fixture_value(&s), b"legacy-bot-value", "{variant}");
            s.conn
                .lock()
                .unwrap()
                .execute(
                    "INSERT INTO bot_state(area,key,revision,value) VALUES(1,?1,1,?2)",
                    params![b"after-6b".as_slice(), b"new".as_slice()],
                )
                .unwrap();
            let _core = dilla_core::client::ClientCore::open(s.conn.clone()).unwrap();
            drop(_core);
            drop(s);

            assert_eq!(cipher_on_disk(&p), StoreCipher::SqlCipher4, "{variant}");
            let s = open(&p).unwrap();
            assert_eq!(fixture_value(&s), b"legacy-bot-value");
            drop(s);
            let _ = fs::remove_dir_all(p);
        }
    }

    #[test]
    fn a_new_store_is_chacha20() {
        let p = dir();
        drop(open(&p).unwrap());
        assert_eq!(
            cipher_on_disk(&p),
            dilla_core::native_store::StoreCipher::ChaCha20
        );
        let _ = fs::remove_dir_all(p);
    }
}

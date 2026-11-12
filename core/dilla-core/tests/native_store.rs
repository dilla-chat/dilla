// Native only: the browser core keys its OPFS store in dilla-core-wasm.
#![cfg(not(target_arch = "wasm32"))]

//! Task 6b: the native store on SQLite3 Multiple Ciphers. New stores are ChaCha20-Poly1305 with a
//! raw key; stores the SQLCipher 4 build wrote (deployed Bots) keep opening through sqlite3mc's
//! SQLCipher scheme. The `tests/fixtures/sqlcipher4` files were written by the SQLCipher build
//! before the switch (`tests/fixtures/sqlcipher4/README.md`).

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use dilla_core::native_store::{KeyError, StoreCipher, open_keyed};
use rusqlite::{Connection, OpenFlags};

/// The fixtures' raw key: bytes 0x00..=0x1f.
const LEGACY_KEY: [u8; 32] = {
    let mut k = [0u8; 32];
    let mut i = 0;
    while i < 32 {
        k[i] = i as u8;
        i += 1;
    }
    k
};
const MARKER: &[u8] = b"legacy-plaintext-marker";

fn flags() -> OpenFlags {
    OpenFlags::SQLITE_OPEN_READ_WRITE
        | OpenFlags::SQLITE_OPEN_NOFOLLOW
        | OpenFlags::SQLITE_OPEN_FULL_MUTEX
}

fn no_prepare(_: &Connection) -> rusqlite::Result<()> {
    Ok(())
}

fn temp_dir() -> PathBuf {
    static NEXT: AtomicU64 = AtomicU64::new(0);
    let p = std::env::temp_dir().join(format!(
        "dilla-native-store-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    ));
    let _ = fs::remove_dir_all(&p);
    fs::create_dir_all(&p).unwrap();
    p
}

fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/sqlcipher4")
        .join(name)
}

/// Copies the named fixture files into a fresh directory; returns the first one's path.
fn copy_fixture(names: &[&str]) -> PathBuf {
    let dir = temp_dir();
    for name in names {
        fs::copy(fixture(name), dir.join(name)).unwrap();
    }
    dir.join(names[0])
}

fn bytes(path: &Path) -> Vec<u8> {
    fs::read(path).unwrap_or_default()
}

fn contains(haystack: &[u8], needle: &[u8]) -> bool {
    haystack.windows(needle.len()).any(|w| w == needle)
}

fn sidecar(path: &Path, suffix: &str) -> PathBuf {
    let mut p = path.as_os_str().to_owned();
    p.push(suffix);
    PathBuf::from(p)
}

fn new_empty_file(dir: &Path) -> PathBuf {
    let path = dir.join("store.db");
    fs::File::create(&path).unwrap();
    path
}

fn legacy_rows(conn: &Connection) -> Vec<(i64, String)> {
    let mut stmt = conn
        .prepare("SELECT id, label FROM legacy ORDER BY id")
        .unwrap();
    stmt.query_map([], |r| Ok((r.get(0)?, r.get(1)?)))
        .unwrap()
        .collect::<Result<_, _>>()
        .unwrap()
}

fn expected_legacy_rows() -> Vec<(i64, String)> {
    vec![
        (1, "legacy-plaintext-marker".into()),
        (2, "second row".into()),
        (3, "third row".into()),
    ]
}

#[test]
fn a_new_store_is_chacha20_with_no_plaintext_on_disk() {
    let dir = temp_dir();
    let path = new_empty_file(&dir);
    let key = [0x5a; 32];
    {
        let keyed = open_keyed(&path, flags(), &key, no_prepare).expect("create");
        assert_eq!(keyed.cipher, StoreCipher::ChaCha20);
        assert_eq!(keyed.tables, 0);
        keyed
            .conn
            .execute_batch(
                "CREATE TABLE t(v BLOB); INSERT INTO t VALUES (CAST('legacy-plaintext-marker' AS BLOB));",
            )
            .unwrap();
    }
    let on_disk = bytes(&path);
    assert!(on_disk.len() >= 4096);
    assert_ne!(&on_disk[..16], b"SQLite format 3\0", "header is plaintext");
    assert!(!contains(&on_disk, MARKER), "row content is plaintext");
    assert!(!contains(&on_disk, b"CREATE TABLE"), "schema is plaintext");

    let reopened = open_keyed(&path, flags(), &key, no_prepare).expect("reopen");
    assert_eq!(reopened.cipher, StoreCipher::ChaCha20);
    assert_eq!(reopened.tables, 1);
    let v: Vec<u8> = reopened
        .conn
        .query_row("SELECT v FROM t", [], |r| r.get(0))
        .unwrap();
    assert_eq!(v, MARKER);
    drop(reopened);

    // The browser's keying (cipher, then a raw hex key) opens the same file: one format.
    let hex: String = key.iter().map(|b| format!("{b:02x}")).collect();
    let plain = Connection::open(&path).unwrap();
    plain
        .execute_batch(&format!(
            "PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:{hex}';"
        ))
        .unwrap();
    let n: i64 = plain
        .query_row("SELECT count(*) FROM t", [], |r| r.get(0))
        .unwrap();
    assert_eq!(n, 1);
    let _ = fs::remove_dir_all(dir);
}

#[test]
fn a_wrong_key_is_refused_without_touching_the_file() {
    let dir = temp_dir();
    let path = new_empty_file(&dir);
    {
        let keyed = open_keyed(&path, flags(), &[0x11; 32], no_prepare).unwrap();
        keyed.conn.execute_batch("CREATE TABLE t(v);").unwrap();
    }
    let before = bytes(&path);
    let err = open_keyed(&path, flags(), &[0x22; 32], no_prepare).unwrap_err();
    assert!(matches!(err, KeyError::WrongKey), "{err:?}");
    assert_eq!(bytes(&path), before);
    let _ = fs::remove_dir_all(dir);
}

#[test]
fn a_plaintext_database_is_refused_and_left_alone() {
    let dir = temp_dir();
    let path = dir.join("plain.db");
    Connection::open(&path)
        .unwrap()
        .execute_batch("CREATE TABLE p(v TEXT); INSERT INTO p VALUES('keep');")
        .unwrap();
    let before = bytes(&path);
    let err = open_keyed(&path, flags(), &[0x33; 32], no_prepare).unwrap_err();
    assert!(matches!(err, KeyError::WrongKey), "{err:?}");
    assert_eq!(bytes(&path), before);
    let _ = fs::remove_dir_all(dir);
}

#[test]
fn a_sqlcipher4_store_from_the_old_build_still_opens() {
    let path = copy_fixture(&["legacy.db"]);
    let before = bytes(&path);
    // Wrong key first: refused under both schemes, and nothing is written.
    let err = open_keyed(&path, flags(), &[0x44; 32], no_prepare).unwrap_err();
    assert!(matches!(err, KeyError::WrongKey), "{err:?}");
    assert_eq!(bytes(&path), before);

    let keyed = open_keyed(&path, flags(), &LEGACY_KEY, no_prepare).expect("legacy open");
    assert_eq!(keyed.cipher, StoreCipher::SqlCipher4);
    assert_eq!(keyed.tables, 1);
    assert_eq!(legacy_rows(&keyed.conn), expected_legacy_rows());
    let blob: Vec<u8> = keyed
        .conn
        .query_row("SELECT payload FROM legacy WHERE id = 1", [], |r| r.get(0))
        .unwrap();
    assert_eq!(blob, [0, 1, 2, 3]);

    // A write on the legacy connection keeps the file SQLCipher 4 (no silent half-rekey).
    keyed
        .conn
        .execute(
            "INSERT INTO legacy(label, payload) VALUES('after the switch', x'01')",
            [],
        )
        .unwrap();
    drop(keyed);
    let after = bytes(&path);
    assert!(!contains(&after, b"after the switch"));
    let again = open_keyed(&path, flags(), &LEGACY_KEY, no_prepare).unwrap();
    assert_eq!(again.cipher, StoreCipher::SqlCipher4);
    let n: i64 = again
        .conn
        .query_row("SELECT count(*) FROM legacy", [], |r| r.get(0))
        .unwrap();
    assert_eq!(n, 4);
    let _ = fs::remove_dir_all(path.parent().unwrap());
}

#[test]
fn a_sqlcipher4_store_with_a_hot_wal_opens_and_a_wrong_key_does_not_checkpoint_it() {
    // The rows live only in the -wal file, as after a crash of a WAL-mode Bot store.
    let path = copy_fixture(&["legacy-wal.db", "legacy-wal.db-wal"]);
    let wal = sidecar(&path, "-wal");
    let (db_before, wal_before) = (bytes(&path), bytes(&wal));
    assert!(!wal_before.is_empty());

    let err = open_keyed(&path, flags(), &[0x55; 32], no_prepare).unwrap_err();
    assert!(matches!(err, KeyError::WrongKey), "{err:?}");
    assert_eq!(
        bytes(&path),
        db_before,
        "a refused open changed the database"
    );
    assert_eq!(bytes(&wal), wal_before, "a refused open changed the WAL");

    let keyed = open_keyed(&path, flags(), &LEGACY_KEY, no_prepare).expect("legacy WAL open");
    assert_eq!(keyed.cipher, StoreCipher::SqlCipher4);
    assert_eq!(legacy_rows(&keyed.conn), expected_legacy_rows());
    let early: String = keyed
        .conn
        .query_row("SELECT v FROM early", [], |r| r.get(0))
        .unwrap();
    assert_eq!(early, "checkpointed");
    drop(keyed);

    // Closing the good connection checkpointed the WAL into the file; it still opens.
    let again = open_keyed(&path, flags(), &LEGACY_KEY, no_prepare).unwrap();
    assert_eq!(legacy_rows(&again.conn), expected_legacy_rows());
    assert!(!contains(&bytes(&path), MARKER));
    let _ = fs::remove_dir_all(path.parent().unwrap());
}

#[test]
fn prepare_runs_before_keying_and_its_failure_is_an_open_error() {
    let dir = temp_dir();
    let path = new_empty_file(&dir);
    let err = open_keyed(&path, flags(), &[0x66; 32], |_| {
        Err(rusqlite::Error::InvalidQuery)
    })
    .unwrap_err();
    assert!(matches!(err, KeyError::Open(_)), "{err:?}");
    let missing = dir.join("absent.db");
    let err = open_keyed(&missing, flags(), &[0x66; 32], no_prepare).unwrap_err();
    assert!(matches!(err, KeyError::Open(_)), "{err:?}");
    assert!(!missing.exists());
    let _ = fs::remove_dir_all(dir);
}

#[test]
fn an_all_zero_key_is_refused() {
    let dir = temp_dir();
    let path = new_empty_file(&dir);
    let err = open_keyed(&path, flags(), &[0; 32], no_prepare).unwrap_err();
    assert!(matches!(err, KeyError::InvalidKey), "{err:?}");
    assert_eq!(fs::metadata(&path).unwrap().len(), 0);
    let _ = fs::remove_dir_all(dir);
}

#[test]
fn freed_sqlite_memory_is_wiped() {
    let dir = temp_dir();
    let path = new_empty_file(&dir);
    let keyed = open_keyed(&path, flags(), &[0x77; 32], no_prepare).unwrap();
    let level: String = keyed
        .conn
        .query_row("PRAGMA memory_security", [], |r| r.get(0))
        .unwrap();
    assert_eq!(level, "1", "memory_security is not `fill`");
    drop(keyed);
    let _ = fs::remove_dir_all(dir);
}

#[test]
fn a_hot_rollback_journal_from_the_sqlcipher_build_is_refused_untouched() {
    // A rollback-mode SQLCipher 4 store copied mid-transaction (pages already spilled into the
    // file). sqlite3mc checksums journal records over plaintext, SQLCipher over ciphertext, so
    // playing this back would fail or half-apply: refuse, whatever the key, and touch nothing.
    let path = copy_fixture(&["legacy-hot-journal.db", "legacy-hot-journal.db-journal"]);
    let journal = sidecar(&path, "-journal");
    let (db_before, journal_before) = (bytes(&path), bytes(&journal));
    for key in [LEGACY_KEY, [0x12; 32]] {
        let err = open_keyed(&path, flags(), &key, no_prepare).unwrap_err();
        assert!(matches!(err, KeyError::LegacyHotJournal), "{err:?}");
        assert_eq!(bytes(&path), db_before, "the database changed");
        assert_eq!(bytes(&journal), journal_before, "the journal changed");
    }
    let _ = fs::remove_dir_all(path.parent().unwrap());
}

/// A ChaCha20 store in rollback mode, copied while a transaction that spilled pages into the file
/// was open: `<dir>/store.db` + `store.db-journal`. Returns the copy's path.
fn chacha20_store_with_hot_journal(key: &[u8; 32]) -> PathBuf {
    let dir = temp_dir();
    let path = new_empty_file(&dir);
    let keyed = open_keyed(&path, flags(), key, no_prepare).unwrap();
    keyed
        .conn
        .execute_batch(
            "PRAGMA journal_mode = DELETE; PRAGMA synchronous = FULL;
             CREATE TABLE t(id INTEGER PRIMARY KEY, label TEXT NOT NULL, payload BLOB NOT NULL);
             WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 40)
             INSERT INTO t(label, payload) SELECT 'committed', zeroblob(1500) FROM n;",
        )
        .unwrap();
    let committed = bytes(&path);
    keyed
        .conn
        .execute_batch(
            "PRAGMA cache_size = 2; BEGIN;
             UPDATE t SET label = 'uncommitted', payload = randomblob(1500);",
        )
        .unwrap();
    let copy = temp_dir();
    fs::copy(&path, copy.join("store.db")).unwrap();
    fs::copy(sidecar(&path, "-journal"), copy.join("store.db-journal")).unwrap();
    keyed.conn.execute_batch("ROLLBACK").unwrap();
    drop(keyed);
    let _ = fs::remove_dir_all(dir);
    let copied = copy.join("store.db");
    assert_ne!(bytes(&copied), committed, "nothing spilled into the file");
    // A real hot journal: SQLite's magic and a record count (it was synced before the spill).
    let journal = bytes(&sidecar(&copied, "-journal"));
    assert!(journal.len() > 512);
    assert_eq!(
        journal[..8],
        [0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7]
    );
    assert_ne!(journal[8..12], [0, 0, 0, 0]);
    copied
}

#[test]
fn a_chacha20_store_with_its_own_hot_journal_recovers() {
    let key = [0x2b; 32];
    let path = chacha20_store_with_hot_journal(&key);
    let keyed = open_keyed(&path, flags(), &key, no_prepare).expect("recovering open");
    assert_eq!(keyed.cipher, StoreCipher::ChaCha20);
    let committed: i64 = keyed
        .conn
        .query_row(
            "SELECT count(*) FROM t WHERE label = 'committed'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        committed, 40,
        "the interrupted transaction was not rolled back"
    );
    let ok: String = keyed
        .conn
        .query_row("PRAGMA integrity_check", [], |r| r.get(0))
        .unwrap();
    assert_eq!(ok, "ok");
    drop(keyed);
    assert!(bytes(&sidecar(&path, "-journal")).is_empty());
    let _ = fs::remove_dir_all(path.parent().unwrap());
}

#[test]
fn a_wrong_key_on_a_chacha20_store_with_a_hot_wal_touches_neither_file() {
    // The SQLCipher fallback runs over a ChaCha20 WAL here: it must not checkpoint or rewrite it.
    let key = [0x3d; 32];
    let dir = temp_dir();
    let path = new_empty_file(&dir);
    let keyed = open_keyed(&path, flags(), &key, no_prepare).unwrap();
    let copy = temp_dir().join("store.db");
    let mode: String = keyed
        .conn
        .query_row("PRAGMA journal_mode = WAL", [], |r| r.get(0))
        .unwrap();
    assert_eq!(mode, "wal");
    keyed
        .conn
        .execute_batch(
            "PRAGMA wal_autocheckpoint = 0; CREATE TABLE t(v TEXT); INSERT INTO t VALUES('in the wal');",
        )
        .unwrap();
    fs::copy(&path, &copy).unwrap();
    fs::copy(sidecar(&path, "-wal"), sidecar(&copy, "-wal")).unwrap();
    drop(keyed);
    let _ = fs::remove_dir_all(dir);
    let wal = sidecar(&copy, "-wal");
    let (db_before, wal_before) = (bytes(&copy), bytes(&wal));
    assert!(!wal_before.is_empty());

    let err = open_keyed(&copy, flags(), &[0x4e; 32], no_prepare).unwrap_err();
    assert!(matches!(err, KeyError::WrongKey), "{err:?}");
    assert_eq!(
        bytes(&copy),
        db_before,
        "a refused open changed the database"
    );
    assert_eq!(bytes(&wal), wal_before, "a refused open changed the WAL");

    let keyed = open_keyed(&copy, flags(), &key, no_prepare).unwrap();
    let v: String = keyed
        .conn
        .query_row("SELECT v FROM t", [], |r| r.get(0))
        .unwrap();
    assert_eq!(v, "in the wal");
    drop(keyed);
    let _ = fs::remove_dir_all(copy.parent().unwrap());
}

//! The browser store's SQLite (sqlite-wasm-rs, SQLite3 Multiple Ciphers), under Node like
//! `tests/node.rs`. Node has no OPFS, so these run on sqlite-wasm-rs's in-memory VFS, which
//! SQLite3MC wraps exactly as it wraps the OPFS pool: the page and journal encryption, the cipher
//! pragmas and the file bytes are the same; only the storage under them differs.
//!
//! RUSTSEC-2026-0459 moved the store from sqlite-wasm-rs 0.5.5 (SQLite3MC 2.3.3 on SQLite 3.53.0)
//! to 0.6.3 (SQLite3MC 2.6.1 on SQLite 3.54.0). The fixtures under `tests/fixtures/` were written
//! by 0.5.5 with the store's own pragmas (`PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:<KEK>'`)
//! and the test KEK below, so every existing user's store must open the way they do here:
//!
//! - `store-sqlite3mc-2.3.3.db`: a store `ClientCore::open` created (the full app schema, FTS5
//!   included) plus a `fixture_rows` table of 400 rows.
//! - `hot-journal-sqlite3mc-2.3.3.db` and its `-journal`: a 400-row `fixture_rows` database whose
//!   uncommitted write (every label set to 'uncommitted', rows above 300 deleted) had already
//!   spilled into the main file when the old build stopped. The journal is hot.

#![cfg(target_arch = "wasm32")]

use std::cell::RefCell;
use std::rc::Rc;

use dilla_core::client::ClientCore;
use rusqlite::Connection;
use sqlite_wasm_rs::vfs::VfsFilesManager;
use sqlite_wasm_rs::vfs::memvfs::MemVfsUtil;
use sqlite_wasm_rs::vfs::transfer::DbTransfer;
use wasm_bindgen_test::*;

wasm_bindgen_test_configure!(run_in_node_experimental);

/// The KEK the fixtures were written with. Test material only.
const FIXTURE_KEK: &str = "5a17e5a17e5a17e5a17e5a17e5a17e5a17e5a17e5a17e5a17e5a17e5a17e5a1";
const OTHER_KEK: &str = "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b";

const STORE_FIXTURE: &[u8] = include_bytes!("fixtures/store-sqlite3mc-2.3.3.db");
const HOT_MAIN_FIXTURE: &[u8] = include_bytes!("fixtures/hot-journal-sqlite3mc-2.3.3.db");
const HOT_JOURNAL_FIXTURE: &[u8] =
    include_bytes!("fixtures/hot-journal-sqlite3mc-2.3.3.db-journal");

/// The first SQLite release with the RUSTSEC-2026-0459 / CVE-2026-119375 fix.
const FIXED_SQLITE: (u32, u32, u32) = (3, 54, 0);

// The workspace warns on unsafe code; this one call is the only way to reach the in-memory VFS's
// files, and it registers nothing.
#[allow(unsafe_code)]
fn memvfs() -> MemVfsUtil {
    // Opening any connection initialises SQLite, whose os_init registers the in-memory VFS.
    drop(Connection::open_in_memory().unwrap());
    // SAFETY: wasm32 under Node is single-threaded and nothing else (un)registers VFSes here.
    unsafe { MemVfsUtil::get() }.unwrap()
}

/// The store's `open_encrypted` sequence (store.rs): cipher, key, then a real read.
fn open_keyed(name: &str, kek: &str) -> rusqlite::Result<Connection> {
    let conn = Connection::open(name)?;
    conn.execute_batch(&format!(
        "PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:{kek}';"
    ))?;
    conn.query_row("SELECT count(*) FROM sqlite_schema", [], |r| {
        r.get::<_, i64>(0)
    })?;
    Ok(conn)
}

fn version_triple(v: &str) -> (u32, u32, u32) {
    let mut parts = v.split('.').map(|p| p.parse::<u32>().unwrap());
    (
        parts.next().unwrap(),
        parts.next().unwrap(),
        parts.next().unwrap_or(0),
    )
}

fn integrity(conn: &Connection) -> String {
    conn.query_row("PRAGMA integrity_check", [], |r| r.get(0))
        .unwrap()
}

#[wasm_bindgen_test]
fn the_keyed_store_connection_runs_on_sqlite_3_54_or_newer() {
    let util = memvfs();
    let conn = open_keyed("version.db", OTHER_KEK).unwrap();
    conn.execute_batch("CREATE TABLE t(v); INSERT INTO t VALUES (1);")
        .unwrap();

    let version: String = conn
        .query_row("SELECT sqlite_version()", [], |r| r.get(0))
        .unwrap();
    assert!(
        version_triple(&version) >= FIXED_SQLITE,
        "the browser store runs SQLite {version}, which RUSTSEC-2026-0459 affects (fixed in 3.54.0)"
    );
    assert!(rusqlite::version_number() >= 3_054_000);
    let mc: String = conn
        .query_row("SELECT sqlite3mc_version()", [], |r| r.get(0))
        .unwrap();
    assert!(
        mc.starts_with("SQLite3 Multiple Ciphers "),
        "the keyed connection is not SQLite3MC: {mc}"
    );
    drop(conn);

    // Keyed means encrypted on disk: the plain SQLite header is not in the file.
    let bytes = util.export_db("version.db").unwrap();
    assert_ne!(&bytes[..16], b"SQLite format 3\0");
    assert!(util.remove("version.db").unwrap());
}

#[wasm_bindgen_test]
fn a_store_the_0_5_5_build_wrote_opens_unchanged() {
    let util = memvfs();
    assert_ne!(
        &STORE_FIXTURE[..16],
        b"SQLite format 3\0",
        "the fixture is encrypted"
    );
    util.import_db_unchecked("legacy-store.db", STORE_FIXTURE)
        .unwrap();

    // A wrong key still fails at the first read: the store stays encrypted after the upgrade.
    let wrong = open_keyed("legacy-store.db", OTHER_KEK).unwrap_err();
    assert!(
        wrong.to_string().contains("file is not a database"),
        "a wrong key was not refused at the first read: {wrong}"
    );

    let conn = open_keyed("legacy-store.db", FIXTURE_KEK).unwrap();
    assert_eq!(integrity(&conn), "ok");
    let (rows, first_label, first_payload): (i64, String, Vec<u8>) = conn
        .query_row(
            "SELECT (SELECT count(*) FROM fixture_rows), label, payload FROM fixture_rows WHERE id = 1",
            [],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
        )
        .unwrap();
    assert_eq!(rows, 400);
    assert_eq!(first_label, "written by sqlite-wasm-rs 0.5.5");
    assert_eq!(first_payload, [0x00, 0xff, 0x10, 0xef]);
    let page_size: i64 = conn
        .query_row("PRAGMA page_size", [], |r| r.get(0))
        .unwrap();
    assert_eq!(page_size, 8192);

    // The app opens it: the schema check refuses nothing and the migrations find nothing to do.
    let conn = Rc::new(RefCell::new(conn));
    let core = ClientCore::open(Rc::clone(&conn)).expect("the app refused a 0.5.5 store");
    drop(core);
    let conn = Rc::try_unwrap(conn).ok().unwrap().into_inner();

    // And the new build writes to it, and reads that back after a reopen.
    conn.execute(
        "INSERT INTO fixture_rows(label, payload) VALUES ('written by sqlite-wasm-rs 0.6.3', x'01')",
        [],
    )
    .unwrap();
    drop(conn);
    let conn = open_keyed("legacy-store.db", FIXTURE_KEK).unwrap();
    let rows: i64 = conn
        .query_row("SELECT count(*) FROM fixture_rows", [], |r| r.get(0))
        .unwrap();
    assert_eq!(rows, 401);
    assert_eq!(integrity(&conn), "ok");
    drop(conn);
    assert!(util.remove("legacy-store.db").unwrap());
}

#[wasm_bindgen_test]
fn a_write_the_0_5_5_build_left_in_flight_rolls_back_on_the_next_open() {
    let util = memvfs();

    // Premise: without its journal the main file shows the uncommitted write (or is unreadable).
    util.import_db_unchecked("legacy-hot-alone.db", HOT_MAIN_FIXTURE)
        .unwrap();
    let alone = open_keyed("legacy-hot-alone.db", FIXTURE_KEK).and_then(|c| {
        c.query_row(
            "SELECT count(*), sum(label = 'uncommitted') FROM fixture_rows",
            [],
            |r| Ok((r.get::<_, i64>(0)?, r.get::<_, i64>(1)?)),
        )
    });
    assert!(
        !matches!(alone, Ok((400, 0))),
        "premise: the fixture's main file already carries the uncommitted write"
    );
    assert!(util.remove("legacy-hot-alone.db").unwrap());

    util.import_db_unchecked("legacy-hot.db", HOT_MAIN_FIXTURE)
        .unwrap();
    util.import_db_unchecked("legacy-hot.db-journal", HOT_JOURNAL_FIXTURE)
        .unwrap();

    let conn = open_keyed("legacy-hot.db", FIXTURE_KEK).unwrap();
    let (rows, uncommitted, first_payload): (i64, i64, Vec<u8>) = conn
        .query_row(
            "SELECT count(*), sum(label = 'uncommitted'),
                    (SELECT payload FROM fixture_rows WHERE id = 1) FROM fixture_rows",
            [],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
        )
        .unwrap();
    assert_eq!(
        (rows, uncommitted),
        (400, 0),
        "the old build's hot journal was not played back"
    );
    assert_eq!(first_payload, [0x00, 0xff, 0x10, 0xef]);
    assert_eq!(integrity(&conn), "ok");
    drop(conn);
    assert!(
        !util.contains("legacy-hot.db-journal").unwrap(),
        "the played-back journal is deleted"
    );
    assert!(util.remove("legacy-hot.db").unwrap());
}

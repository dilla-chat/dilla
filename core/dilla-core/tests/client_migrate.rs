// Native only: the fixture is a database file.
#![cfg(not(target_arch = "wasm32"))]

//! web-2a task 1 (L-CORE-20, Q23, lesson g): a store written at app schema v1 opens as v2.
//!
//! `tests/fixtures/app_v1.db` is a real v1 store. `write_the_v1_fixture` (ignored) wrote it: it
//! drives two cores through the web-1 API, then copies Alice's database into a file created from
//! `APP_SCHEMA_V1` — web-1's DDL, kept here verbatim and nowhere in `src/` — taking the openmls
//! tables whole, the app tables by their v1 columns, and the identity record in its v1 form. Run
//! it again only to regenerate the file; it yields a v1 store whatever schema this build writes.

mod client_support;

use client_support::*;
use dilla_core::cbor::{Encoder, decode_strict};
use dilla_core::client::{ClientCore, ClientError, mentions_me, migrate_app};
use dilla_core::envelope::EnvelopeType;
use dilla_core::mls::{ConnHandle, DillaStorage};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

const FIXTURE: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/tests/fixtures/app_v1.db");
const V3_FIXTURE: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/tests/fixtures/app_v3.db");

#[test]
#[ignore = "writes tests/fixtures/app_v3.db before v4 implementation"]
fn write_the_v3_fixture() {
    let _ = std::fs::remove_file(V3_FIXTURE);
    let handle: ConnHandle = Arc::new(Mutex::new(
        rusqlite::Connection::open(V3_FIXTURE).expect("create v3 fixture"),
    ));
    let mut core = ClientCore::open(Arc::clone(&handle)).expect("v3 open");
    core.setting_put("density", "compact").expect("setting");
    drop(core);
    let db = handle.lock().expect("lock fixture");
    let schema: Vec<u8> = db
        .query_row("SELECT v FROM app_meta WHERE k='schema'", [], |r| r.get(0))
        .expect("schema");
    assert_eq!(schema, vec![0x03]);
    db.execute_batch("PRAGMA wal_checkpoint(TRUNCATE)")
        .expect("checkpoint");
}

fn authority_tables(db: &rusqlite::Connection) -> i64 {
    db.query_row(
        "SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('app_authority','app_authority_known','app_authority_mentionable')",
        [],
        |r| r.get(0),
    )
    .expect("tables")
}

/// web3.5 R1: a v3 store gains the authority tables, empty, moves to the current schema, and keeps its pins
/// (no group has a known view, so every pin keeps the former rule).
#[test]
fn a_v3_store_gains_the_authority_tables_with_its_pins_unchanged() {
    let (path, handle) = copy_of(V3_FIXTURE, "v3-authority");
    let pin = (vec![0x44u8; 16], vec![0x71u8; 16], 9i64, vec![0xe5u8; 16]);
    {
        let db = handle.lock().expect("v3 db");
        assert_eq!(authority_tables(&db), 0, "the fixture predates the tables");
        db.execute(
            "INSERT INTO app_pins (group_id, target, seq, by_user) VALUES (?1, ?2, ?3, ?4)",
            rusqlite::params![pin.0, pin.1, pin.2, pin.3],
        )
        .expect("a v3 pin");
    }
    let core = ClientCore::open(Arc::clone(&handle)).expect("v4 open");
    drop(core);
    let db = handle.lock().expect("db");
    let schema: Vec<u8> = db
        .query_row("SELECT v FROM app_meta WHERE k='schema'", [], |r| r.get(0))
        .expect("schema");
    assert_eq!(schema, vec![0x0d]);
    assert_eq!(authority_tables(&db), 3);
    type PinRow = (Vec<u8>, Vec<u8>, i64, Vec<u8>);
    let pins: Vec<PinRow> = db
        .prepare("SELECT group_id, target, seq, by_user FROM app_pins")
        .expect("pins query")
        .query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)))
        .expect("pins")
        .collect::<Result<_, _>>()
        .expect("pin rows");
    assert_eq!(pins, vec![pin]);
    let known: i64 = db
        .query_row(
            "SELECT (SELECT count(*) FROM app_authority) + (SELECT count(*) FROM app_authority_known)",
            [],
            |r| r.get(0),
        )
        .expect("rows");
    assert_eq!(known, 0, "no group starts with a known view");
    drop(db);
    std::fs::remove_file(path).expect("remove copy");
}

/// web3.7 TB1 (R26): a v3 store gains `app_messages.thread_id` and its index at schema 5, and
/// every row it already held keeps a NULL thread id (no backfill), so it stays in the channel.
#[test]
fn a_v3_store_gains_the_thread_column_without_a_backfill() {
    let (path, handle) = copy_of(V3_FIXTURE, "v3-threads");
    let rows_before: i64 = handle
        .lock()
        .expect("v3 db")
        .query_row("SELECT count(*) FROM app_messages", [], |r| r.get(0))
        .expect("rows");
    assert!(rows_before > 0, "the fixture holds messages");
    drop(ClientCore::open(Arc::clone(&handle)).expect("v6 open"));
    let db = handle.lock().expect("db");
    let (rows, threaded): (i64, i64) = db
        .query_row(
            "SELECT count(*), count(thread_id) FROM app_messages",
            [],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .expect("thread column");
    assert_eq!((rows, threaded), (rows_before, 0));
    let index: i64 = db
        .query_row(
            "SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'app_messages_by_thread'",
            [],
            |r| r.get(0),
        )
        .expect("index");
    assert_eq!(index, 1);
    assert_eq!(meta_of(&db, "schema"), Some(vec![0x0d]));
    drop(db);
    std::fs::remove_file(path).expect("remove copy");
}

/// A store a web3.4 build left at schema 4 (before the tables) gains them on its next open, and
/// ends at schema 12.
#[test]
fn a_schema_4_store_without_the_authority_tables_gains_them() {
    let conn = memory();
    drop(ClientCore::open(Arc::clone(&conn)).expect("fresh"));
    conn.lock()
        .expect("lock")
        .execute_batch("UPDATE app_meta SET v=x'04' WHERE k='schema'; DROP TABLE app_authority; DROP TABLE app_authority_known; DROP TABLE app_authority_mentionable;")
        .expect("a web3.4 store");
    drop(ClientCore::open(Arc::clone(&conn)).expect("reopen"));
    let db = conn.lock().expect("lock");
    assert_eq!(authority_tables(&db), 3);
    assert_eq!(meta_of(&db, "schema"), Some(vec![0x0d]));
}

#[test]
fn a_schema_7_store_gains_the_native_keypackage_journal() {
    let conn = memory();
    drop(ClientCore::open(Arc::clone(&conn)).expect("fresh v8 store"));
    conn.lock()
        .expect("lock")
        .execute_batch(
            "UPDATE app_meta SET v=x'07' WHERE k='schema';
             DROP TABLE app_bot_keypackage_intents;",
        )
        .expect("simulate the schema 7 shape");
    drop(ClientCore::open(Arc::clone(&conn)).expect("migrate v7"));
    let c = conn.lock().expect("lock");
    assert_eq!(meta_of(&c, "schema"), Some(vec![0x0d]));
    assert!(
        c.query_row(
            "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='app_bot_keypackage_intents')",
            [],
            |r| r.get::<_, i64>(0),
        )
        .expect("journal exists")
            == 1
    );
}

#[test]
fn v3_fixture_migrates_and_keeps_settings_with_or_without_fts() {
    let (path, handle) = copy_of(V3_FIXTURE, "v3-to-v4");
    {
        let db = handle.lock().expect("v3 db");
        let schema: Vec<u8> = db
            .query_row("SELECT v FROM app_meta WHERE k='schema'", [], |r| r.get(0))
            .expect("fixture schema");
        assert_eq!(schema, [0x03], "fixture must be written by a v3 build");
    }
    let core = ClientCore::open(Arc::clone(&handle)).expect("v4 open");
    assert!(settings_of(&core).contains(&("density".to_owned(), "compact".to_owned())));
    let identity = decode_strict(&core.identity().expect("identity"), |d| {
        d.array(6)?;
        let phase = d.uint()?;
        d.opt_bytes_exact::<16>()?;
        let user = d.opt_bytes_exact::<16>()?;
        let device = d.opt_bytes_exact::<16>()?;
        Ok((phase, user, device, d.text()?.to_owned(), d.uint()?))
    })
    .expect("identity shape");
    assert_eq!(
        identity,
        (
            2,
            Some([0xa1; 16]),
            Some([
                0xaf, 0x23, 0x9b, 0xbd, 0x9d, 0x43, 0x37, 0x8b, 0xcf, 0x53, 0xba, 0x68, 0x34, 0xc0,
                0x75, 0xe9,
            ]),
            "alice".to_owned(),
            1,
        )
    );
    let db = handle.lock().expect("db");
    let schema: Vec<u8> = db
        .query_row("SELECT v FROM app_meta WHERE k='schema'", [], |r| r.get(0))
        .expect("schema");
    assert_eq!(schema, vec![0x0d]);
    let groups: Vec<(Vec<u8>, i64, i64)> = db
        .prepare("SELECT group_id, state, epoch FROM app_groups ORDER BY group_id")
        .expect("groups query")
        .query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)))
        .expect("groups")
        .collect::<Result<_, _>>()
        .expect("group rows");
    assert_eq!(groups, vec![(vec![0x44; 16], 2, 2), (vec![0x45; 16], 2, 0)]);
    let messages: Vec<(Vec<u8>, i64, String)> = db
        .prepare("SELECT group_id, seq, body FROM app_messages ORDER BY group_id, seq")
        .expect("messages query")
        .query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)))
        .expect("messages")
        .collect::<Result<_, _>>()
        .expect("message rows");
    assert_eq!(
        messages,
        vec![
            (vec![0x44; 16], 3, "hello from alice".to_owned()),
            (vec![0x44; 16], 4, "hello from bob".to_owned()),
            (
                vec![0x44; 16],
                5,
                "ping <@a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1>".to_owned()
            ),
            (
                vec![0x44; 16],
                6,
                "note to self <@a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1>".to_owned()
            ),
            (
                vec![0x44; 16],
                7,
                "shout <@A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1>".to_owned()
            ),
            (
                vec![0x44; 16],
                8,
                "from my other browser <@a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1>".to_owned()
            ),
            (vec![0x45; 16], 1, "alone here".to_owned()),
        ],
        "all v3 history survives unchanged"
    );
    for table in [
        "app_device_lists",
        "app_trust_pins",
        "app_quarantine",
        "app_trust_audit",
        "app_search_rows",
    ] {
        let count: i64 = db
            .query_row(
                "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?1",
                [table],
                |r| r.get(0),
            )
            .expect("table");
        assert_eq!(count, 1, "missing {table}");
    }
    let cap: Vec<u8> = db
        .query_row("SELECT v FROM app_meta WHERE k='search_fts5'", [], |r| {
            r.get(0)
        })
        .expect("capability");
    assert!(cap == [0] || cap == [1]);
    drop(db);
    drop(core);
    std::fs::remove_file(path).expect("remove copy");
}
const PENDING_FIXTURE: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/fixtures/app_v1_pending.db"
);

/// web-1's `APP_SCHEMA` (L-SQL-10), verbatim.
const APP_SCHEMA_V1: &str = "
CREATE TABLE IF NOT EXISTS app_meta (k TEXT PRIMARY KEY, v BLOB NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_groups (
  group_id     BLOB    PRIMARY KEY CHECK (length(group_id) = 16),
  kind         INTEGER NOT NULL,
  community_id BLOB    CHECK (community_id IS NULL OR length(community_id) = 16),
  target_id    BLOB    NOT NULL CHECK (length(target_id) = 16),
  state        INTEGER NOT NULL,
  next_seq     INTEGER NOT NULL DEFAULT 1,
  acked_seq    INTEGER NOT NULL DEFAULT 0,
  acked_epoch  INTEGER NOT NULL DEFAULT 0,
  resync       INTEGER NOT NULL DEFAULT 0 CHECK (resync IN (0, 1)),
  was_gone     INTEGER NOT NULL DEFAULT 0 CHECK (was_gone IN (0, 1)),
  max_epoch    INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE UNIQUE INDEX IF NOT EXISTS app_groups_by_target ON app_groups (target_id, kind) WHERE state <> 4;
CREATE TABLE IF NOT EXISTS app_handshake_tail (
  group_id BLOB NOT NULL, seq INTEGER NOT NULL, epoch INTEGER NOT NULL, kind INTEGER NOT NULL,
  sender INTEGER, blob BLOB NOT NULL, PRIMARY KEY (group_id, seq)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_proposals (
  group_id BLOB NOT NULL, ref BLOB NOT NULL, epoch INTEGER NOT NULL, PRIMARY KEY (group_id, ref)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_messages (
  group_id      BLOB    NOT NULL,
  seq           INTEGER NOT NULL,
  epoch         INTEGER NOT NULL,
  recv_ts       INTEGER NOT NULL,
  status        INTEGER NOT NULL,
  reason        TEXT    NOT NULL DEFAULT '',
  sender_user   BLOB,
  sender_device BLOB    NOT NULL,
  sender_leaf   INTEGER,
  sender_kind   INTEGER,
  sender_tier   INTEGER,
  msg_id        BLOB,
  type          INTEGER,
  body          TEXT    NOT NULL DEFAULT '',
  envelope      BLOB,
  franking_tag  BLOB    NOT NULL,
  PRIMARY KEY (group_id, seq)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_messages_by_msg ON app_messages (group_id, msg_id);
CREATE TABLE IF NOT EXISTS app_outbox (
  msg_id   BLOB    PRIMARY KEY CHECK (length(msg_id) = 16),
  group_id BLOB    NOT NULL,
  envelope BLOB    NOT NULL,
  created  INTEGER NOT NULL,
  state    INTEGER NOT NULL,
  error    TEXT    NOT NULL DEFAULT '',
  epoch    INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_outbox_by_group ON app_outbox (group_id, created, msg_id);
INSERT OR IGNORE INTO app_meta (k, v) VALUES ('schema', x'01');
";

/// The v1 columns of every app table but `app_meta`, in declaration order.
const V1_COLUMNS: [(&str, &str); 5] = [
    (
        "app_groups",
        "group_id,kind,community_id,target_id,state,next_seq,acked_seq,acked_epoch,resync,was_gone,max_epoch",
    ),
    ("app_handshake_tail", "group_id,seq,epoch,kind,sender,blob"),
    ("app_proposals", "group_id,ref,epoch"),
    (
        "app_messages",
        "group_id,seq,epoch,recv_ts,status,reason,sender_user,sender_device,sender_leaf,sender_kind,sender_tier,msg_id,type,body,envelope,franking_tag",
    ),
    (
        "app_outbox",
        "msg_id,group_id,envelope,created,state,error,epoch",
    ),
];

fn mention_body() -> String {
    format!("ping <@{}>", "a1".repeat(16))
}
fn self_mention_body() -> String {
    format!("note to self <@{}>", "a1".repeat(16))
}
fn shouted_body() -> String {
    format!("shout <@{}>", "A1".repeat(16))
}
/// Sent by Alice's other device (user 0xa1, device 0x5e): the own user, so never a mention.
fn twin_mention_body() -> String {
    format!("from my other browser <@{}>", "a1".repeat(16))
}
const TWIN_DEVICE: [u8; 16] = [0x5e; 16];

fn columns(c: &rusqlite::Connection, table: &str) -> Vec<String> {
    let mut stmt = c
        .prepare("SELECT name FROM pragma_table_info(?1) ORDER BY cid")
        .expect("prepare");
    stmt.query_map([table], |r| r.get::<_, String>(0))
        .expect("query")
        .collect::<Result<Vec<_>, _>>()
        .expect("names")
}

fn column_info(
    c: &rusqlite::Connection,
    table: &str,
) -> Vec<(String, String, Option<String>, i64)> {
    let mut stmt = c
        .prepare(
            "SELECT name, type, dflt_value, \"notnull\" FROM pragma_table_info(?1) ORDER BY cid",
        )
        .expect("prepare");
    stmt.query_map([table], |r| {
        Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?))
    })
    .expect("query")
    .collect::<Result<_, _>>()
    .expect("column info")
}

fn app_tables(c: &rusqlite::Connection) -> Vec<String> {
    let mut stmt = c
        .prepare("SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'app%' ORDER BY name")
        .expect("prepare");
    stmt.query_map([], |r| r.get::<_, String>(0))
        .expect("query")
        .collect::<Result<Vec<_>, _>>()
        .expect("names")
}

fn meta_of(c: &rusqlite::Connection, k: &str) -> Option<Vec<u8>> {
    use rusqlite::OptionalExtension;
    c.query_row("SELECT v FROM app_meta WHERE k = ?1", [k], |r| r.get(0))
        .optional()
        .expect("app_meta read")
}

/// The identity record as web-1 wrote it: eleven elements, version 1. A record this build wrote
/// (thirteen elements, version 2) is cut back to its first eleven.
fn identity_v1(record: &[u8]) -> Vec<u8> {
    let fields = decode_strict(record, |d| {
        let n = d.array_len()?;
        assert!(n == 11 || n == 13, "an identity record of {n} elements");
        d.uint()?;
        let fields = (
            d.bytes_exact::<16>()?,
            d.bytes_exact::<16>()?,
            d.bytes_exact::<16>()?,
            d.bytes_exact::<32>()?,
            d.bytes_exact::<32>()?,
            d.bytes_exact::<32>()?,
            d.text()?.to_owned(),
            d.bytes()?.to_vec(),
            d.bytes()?.to_vec(),
            d.uint()?,
        );
        if n == 13 {
            d.skip()?;
            d.skip()?;
        }
        Ok(fields)
    })
    .expect("the identity record");
    let (instance, user, device, dsk, umk, ssk, username, credential, list_body, published) =
        fields;
    let mut e = Encoder::new();
    e.array(11)
        .uint(1)
        .bytes(&instance)
        .bytes(&user)
        .bytes(&device)
        .bytes(&dsk)
        .bytes(&umk)
        .bytes(&ssk)
        .text(&username)
        .bytes(&credential)
        .bytes(&list_body)
        .uint(published);
    e.into_vec()
}

/// Alice and Bob in GROUP at epoch 2 (the external join of Alice's other device, a raw member with
/// user 0xa1 and device 0x5e, moved it from 1 to 2) with six messages (Bob's mention of Alice,
/// Alice's own mention of herself, Bob's upper-case-hex mention, and the other device's mention of
/// Alice among them) and one queued outbox row; Alice alone in OTHER_GROUP at epoch 0 with one
/// message and a commit built and never confirmed: seven messages in all. Alice's store is copied
/// into the fixture as a v1 store.
#[test]
#[ignore = "writes tests/fixtures/app_v1.db; run once with --ignored and commit the file"]
fn write_the_v1_fixture() {
    let (instance, mut relay, mut a, mut b) = alice_and_bob();
    let mut twin = RawPeer::new(0xa1, 0x5e);
    twin.join_external(&mut relay);
    a.sync(&relay);
    b.sync(&relay);
    a.send(&mut relay, &GROUP, "hello from alice", NOW + 1);
    b.send(&mut relay, &GROUP, "hello from bob", NOW + 2);
    b.send(&mut relay, &GROUP, &mention_body(), NOW + 3);
    a.send(&mut relay, &GROUP, &self_mention_body(), NOW + 4);
    b.send(&mut relay, &GROUP, &shouted_body(), NOW + 5);
    twin.send(&mut relay, &twin_mention_body());
    a.sync(&relay);
    a.prepare(&GROUP, "still queued", NOW + 6);

    let mut other = Relay::new(OTHER_GROUP);
    let body = a
        .core
        .group_create(
            &OTHER_GROUP,
            Some(&COMMUNITY),
            &OTHER_CHANNEL,
            POLICY,
            &instance.public(),
        )
        .expect("group_create");
    let created = other.register(&body).expect("register");
    let next_seq = decode_strict(&created, |d| {
        d.array(2)?;
        d.bytes_exact::<16>()?;
        d.uint()
    })
    .expect("201 body");
    a.core
        .group_registered(&OTHER_GROUP, next_seq)
        .expect("group_registered");
    a.send(&mut other, &OTHER_GROUP, "alone here", NOW + 7);
    a.core
        .commit_build(&OTHER_GROUP, &[0x80])
        .expect("commit_build");
    a.core.device_list_published().expect("published");

    let path = Path::new(FIXTURE);
    let _ = std::fs::remove_file(path);
    {
        let file = rusqlite::Connection::open(path).expect("create the fixture");
        file.execute_batch(APP_SCHEMA_V1)
            .expect("the v1 app schema");
        DillaStorage::new(Arc::new(Mutex::new(file)))
            .migrate()
            .expect("the openmls schema");
    }
    {
        let c = a.probe.lock().expect("lock");
        let identity: Vec<u8> = c
            .query_row("SELECT v FROM app_meta WHERE k = 'identity'", [], |r| {
                r.get(0)
            })
            .expect("identity record");
        c.execute("ATTACH DATABASE ?1 AS v1", [FIXTURE])
            .expect("attach");
        let tables: Vec<String> = {
            let mut stmt = c
                .prepare(
                    "SELECT name FROM main.sqlite_master WHERE type = 'table' \
                     AND (name LIKE 'openmls_%' OR name = 'storage_meta') ORDER BY name",
                )
                .expect("prepare");
            stmt.query_map([], |r| r.get(0))
                .expect("query")
                .collect::<Result<Vec<_>, _>>()
                .expect("names")
        };
        for t in &tables {
            c.execute(
                &format!("INSERT OR REPLACE INTO v1.\"{t}\" SELECT * FROM main.\"{t}\""),
                [],
            )
            .expect("copy an openmls table");
        }
        for (t, cols) in V1_COLUMNS {
            c.execute(
                &format!("INSERT INTO v1.{t} ({cols}) SELECT {cols} FROM main.{t}"),
                [],
            )
            .expect("copy an app table");
        }
        c.execute(
            "INSERT INTO v1.app_meta (k, v) SELECT k, v FROM main.app_meta \
             WHERE k NOT IN ('schema', 'identity')",
            [],
        )
        .expect("copy app_meta");
        c.execute(
            "INSERT INTO v1.app_meta (k, v) VALUES ('identity', ?1)",
            [identity_v1(&identity)],
        )
        .expect("the v1 identity record");
        c.execute("DETACH DATABASE v1", []).expect("detach");
    }
    let file = rusqlite::Connection::open(path).expect("reopen the fixture");
    file.execute_batch("VACUUM").expect("vacuum");
    for (t, cols) in V1_COLUMNS {
        assert_eq!(
            columns(&file, t),
            cols.split(',').collect::<Vec<_>>(),
            "{t}"
        );
    }
    assert_eq!(
        app_tables(&file),
        [
            "app_groups",
            "app_handshake_tail",
            "app_messages",
            "app_meta",
            "app_outbox",
            "app_proposals"
        ]
    );
    assert_eq!(meta_of(&file, "schema"), Some(vec![0x01]));
    let total: i64 = file
        .query_row("SELECT count(*) FROM app_messages", [], |r| r.get(0))
        .expect("count");
    assert_eq!(total, 7, "seven messages");
    let twin_row: (Vec<u8>, i64, i64, String) = file
        .query_row(
            "SELECT sender_user, status, type, body FROM app_messages WHERE sender_device = ?1",
            [TWIN_DEVICE.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
        )
        .expect("the other device's row");
    assert_eq!(
        twin_row,
        (vec![0xa1; 16], 0, 0, twin_mention_body()),
        "a status-0 type-0 row of the own user from another device"
    );
    let record = meta_of(&file, "identity").expect("identity");
    // decode_strict refuses trailing bytes, so every element is read past.
    let len = decode_strict(&record, |d| {
        let n = d.array_len()?;
        for _ in 0..n {
            d.skip()?;
        }
        Ok(n)
    })
    .expect("the identity record decodes");
    assert_eq!(len, 11, "an eleven-element v1 record");
}

/// A private copy of the committed fixture `src` under the temp directory, opened as a file.
fn copy_of(src: &str, name: &str) -> (PathBuf, ConnHandle) {
    let path = std::env::temp_dir().join(format!(
        "dilla-client-migrate-{}-{name}.db",
        std::process::id()
    ));
    let _ = std::fs::remove_file(&path);
    std::fs::copy(src, &path).expect("copy the fixture");
    let conn: ConnHandle = Arc::new(Mutex::new(
        rusqlite::Connection::open(&path).expect("open the copy"),
    ));
    (path, conn)
}

fn fixture_copy(name: &str) -> (PathBuf, ConnHandle) {
    copy_of(FIXTURE, name)
}

fn app_indexes(c: &rusqlite::Connection) -> Vec<String> {
    let mut stmt = c
        .prepare("SELECT name FROM sqlite_master WHERE type = 'index' AND name LIKE 'app%' ORDER BY name")
        .expect("prepare");
    stmt.query_map([], |r| r.get::<_, String>(0))
        .expect("query")
        .collect::<Result<Vec<_>, _>>()
        .expect("names")
}

#[test]
fn a_v1_store_opens_as_v4_with_every_column_backfilled() {
    let (path, conn) = fixture_copy("open");
    {
        let c = conn.lock().expect("lock");
        assert_eq!(
            meta_of(&c, "schema"),
            Some(vec![0x01]),
            "the committed fixture is v1"
        );
        assert!(!columns(&c, "app_groups").iter().any(|n| n == "epoch"));
    }
    let core = ClientCore::open(Arc::clone(&conn)).expect("open the v1 fixture");
    {
        let c = conn.lock().expect("lock");
        assert_eq!(meta_of(&c, "schema"), Some(vec![0x0d]));
        // A migrated store and a fresh one have the same columns, in the same order.
        let fresh = memory();
        drop(ClientCore::open(Arc::clone(&fresh)).expect("a fresh store"));
        let fresh = fresh.lock().expect("lock");
        assert_eq!(app_tables(&c), app_tables(&fresh));
        for t in app_tables(&fresh) {
            assert_eq!(column_info(&c, &t), column_info(&fresh, &t), "{t}");
        }
        assert_eq!(app_indexes(&c), app_indexes(&fresh));
        let group_columns = columns(&c, "app_groups");
        assert_eq!(
            &group_columns[group_columns.len() - 4..],
            [
                "epoch",
                "pending_commit",
                "interaction_installation_id",
                "interaction_device_id"
            ]
        );
        let message_columns = columns(&c, "app_messages");
        assert_eq!(
            &message_columns[message_columns.len() - 6..],
            [
                "mention",
                "reply_to",
                "edit_body",
                "edit_seq",
                "sender_sig_key",
                "thread_id"
            ]
        );
        let replies: i64 = c
            .query_row(
                "SELECT count(*) FROM app_messages WHERE reply_to IS NOT NULL",
                [],
                |r| r.get(0),
            )
            .expect("count");
        assert_eq!(replies, 0, "no v1 message replies to anything");
        assert_eq!(
            columns(&c, "app_read_state"),
            ["group_id", "last_read_seq", "last_read_at"]
        );
        assert_eq!(columns(&c, "app_settings"), ["k", "v"]);
    }

    // epoch and pending_commit equal the stored MLS groups: GROUP at epoch 2, OTHER_GROUP at 0
    // holding the commit Alice built and never confirmed.
    let rows: Vec<_> = groups(&core)
        .into_iter()
        .map(|r| (r.group_id, r.state, r.epoch, r.pending_commit))
        .collect();
    assert_eq!(rows, vec![(GROUP, 2, 2, 0), (OTHER_GROUP, 2, 0, 1)]);
    assert_mls_columns(&core, &conn, "the migration");

    // mention: Bob's lower-case mention only. Alice's own mention of herself (this device), the
    // mention from Alice's other device (the own user, by user: ruling 29) and the upper-case one
    // keep 0.
    {
        let c = conn.lock().expect("lock");
        let mut stmt = c
            .prepare("SELECT body FROM app_messages WHERE mention = 1 ORDER BY group_id, seq")
            .expect("prepare");
        let flagged = stmt
            .query_map([], |r| r.get::<_, String>(0))
            .expect("query")
            .collect::<Result<Vec<_>, _>>()
            .expect("rows");
        assert_eq!(flagged, vec![mention_body()]);
        let mut stmt = c
            .prepare("SELECT body FROM app_messages WHERE mention = 0 ORDER BY group_id, seq")
            .expect("prepare");
        let unflagged = stmt
            .query_map([], |r| r.get::<_, String>(0))
            .expect("query")
            .collect::<Result<Vec<_>, _>>()
            .expect("rows");
        assert!(
            unflagged.contains(&twin_mention_body()),
            "the own user's other device is not a mention"
        );
        assert!(unflagged.contains(&self_mention_body()));
        assert!(unflagged.contains(&shouted_body()));
        let twin_mention: i64 = c
            .query_row(
                "SELECT mention FROM app_messages WHERE sender_device = ?1",
                [TWIN_DEVICE.as_slice()],
                |r| r.get(0),
            )
            .expect("the other device's row");
        assert_eq!(twin_mention, 0);
        let total: i64 = c
            .query_row("SELECT count(*) FROM app_messages", [], |r| r.get(0))
            .expect("count");
        assert_eq!(total, 7);
    }

    // The identity record is v2: device_list is the v1 body's signed list, state_uploaded is 0.
    let record = meta_of(&conn.lock().expect("lock"), "identity").expect("identity");
    let (v, list_body, published, list, uploaded) = decode_strict(&record, |d| {
        d.array(13)?;
        let v = d.uint()?;
        for _ in 0..8 {
            d.skip()?;
        }
        Ok((
            v,
            d.bytes()?.to_vec(),
            d.uint()?,
            d.bytes()?.to_vec(),
            d.uint()?,
        ))
    })
    .expect("the thirteen-element identity record");
    let served = decode_strict(&list_body, |d| {
        d.array(4)?;
        d.uint()?;
        let blob = d.bytes()?.to_vec();
        d.skip()?;
        d.skip()?;
        Ok(blob)
    })
    .expect("[version, blob, ssk_signature, prev_hash]");
    assert_eq!((v, published, uploaded), (2, 1, 0));
    assert_eq!(list, served, "device_list is element 1 of device_list_body");
    let (phase, user, username, listed) = decode_strict(&core.identity().expect("identity"), |d| {
        d.array(6)?;
        let phase = d.uint()?;
        d.opt_bytes_exact::<16>()?;
        let user = d.opt_bytes_exact::<16>()?;
        d.opt_bytes_exact::<16>()?;
        Ok((phase, user, d.text()?.to_owned(), d.uint()?))
    })
    .expect("identity shape");
    assert_eq!(
        (phase, user, username.as_str(), listed),
        (2, Some([0xa1; 16]), "alice", 1)
    );

    // History and the outbox survive.
    let bodies: Vec<String> = decode_timeline(&core.timeline(&GROUP, 0, 200).expect("timeline"))
        .into_iter()
        .map(|r| r.body)
        .collect();
    assert_eq!(
        bodies,
        vec![
            "hello from alice".to_owned(),
            "hello from bob".to_owned(),
            mention_body(),
            self_mention_body(),
            shouted_body(),
            twin_mention_body(),
        ]
    );
    let outbox = decode_outbox(&core.outbox(&GROUP).expect("outbox"));
    assert_eq!(
        outbox
            .iter()
            .map(|o| (o.state, o.body.as_str()))
            .collect::<Vec<_>>(),
        vec![(0, "still queued")]
    );

    // The backfill runs once: a v3 store is opened without one.
    drop(core);
    conn.lock()
        .expect("lock")
        .execute(
            "UPDATE app_groups SET epoch = 99 WHERE group_id = ?1",
            [GROUP.as_slice()],
        )
        .expect("tamper");
    let core = ClientCore::open(Arc::clone(&conn)).expect("reopen at v3");
    assert_eq!(
        groups(&core)[0].epoch,
        99,
        "a v3 store is not backfilled again"
    );
    drop(core);
    drop(conn);
    std::fs::remove_file(&path).expect("cleanup");
}

/// The backfill evaluates `mentions_me` in SQL (`instr` over the same three needles; it keeps
/// the browser build inside task 1's budget). This pins the two to the same answer on
/// `mentions_me`'s own vectors and a few more (an embedded NUL, non-ASCII text), and pins the
/// row filter: status 0, type 0, another user.
#[test]
fn a_v1_store_flags_mentions_as_mentions_me_does() {
    let me = "a1".repeat(16);
    let bodies = [
        format!("hi <@{me}>"),
        format!("<@{me}>"),
        "<@everyone> standup".to_owned(),
        "x<@here>y".to_owned(),
        format!("hi <@{}>", "A1".repeat(16)),
        format!("hi <@{}>", "b2".repeat(16)),
        format!("hi <@{me}"),
        format!("hi @{me}"),
        "<@Everyone>".to_owned(),
        "<@everyone >".to_owned(),
        String::new(),
        "nul\0 then <@everyone>".to_owned(),
        format!("héllo <@{me}> ✓"),
        "<@here".to_owned(),
    ];
    // (status, type, sender_user): only the first is eligible.
    let senders: [(i64, i64, [u8; 16]); 4] = [
        (0, 0, [0xb2; 16]),
        (1, 0, [0xb2; 16]),
        (0, 1, [0xb2; 16]),
        (0, 0, [0xa1; 16]),
    ];
    let (path, conn) = fixture_copy("mention-vectors");
    let mut want = Vec::new();
    {
        let c = conn.lock().expect("lock");
        let mut seq = 10_000i64;
        for body in &bodies {
            for (status, ty, user) in senders {
                c.execute(
                    "INSERT INTO app_messages (group_id, seq, epoch, recv_ts, status, \
                     sender_user, sender_device, type, body, franking_tag) \
                     VALUES (?1, ?2, 1, 0, ?3, ?4, ?5, ?6, ?7, x'00')",
                    rusqlite::params![
                        OTHER_GROUP.as_slice(),
                        seq,
                        status,
                        user.as_slice(),
                        [0x0du8; 16].as_slice(),
                        ty,
                        body
                    ],
                )
                .expect("seed a row");
                let eligible = status == 0 && ty == 0 && user != [0xa1; 16];
                want.push((seq, i64::from(eligible && mentions_me(body, &[0xa1; 16]))));
                seq += 1;
            }
        }
    }
    assert!(
        want.iter().filter(|(_, m)| *m == 1).count() >= 6,
        "the vectors flag something"
    );
    drop(ClientCore::open(Arc::clone(&conn)).expect("open the v1 fixture"));
    let got: Vec<(i64, i64)> = {
        let c = conn.lock().expect("lock");
        let mut stmt = c
            .prepare("SELECT seq, mention FROM app_messages WHERE seq >= 10000 ORDER BY seq")
            .expect("prepare");
        stmt.query_map([], |r| Ok((r.get(0)?, r.get(1)?)))
            .expect("query")
            .collect::<Result<_, _>>()
            .expect("rows")
    };
    assert_eq!(got, want, "mention = 1 exactly where mentions_me says so");
    drop(conn);
    std::fs::remove_file(&path).expect("cleanup");
}

#[test]
fn a_v1_store_whose_backfill_fails_stays_v1() {
    let (path, conn) = fixture_copy("rollback");
    conn.lock()
        .expect("lock")
        .execute("UPDATE app_meta SET v = x'00' WHERE k = 'identity'", [])
        .expect("corrupt the identity record");
    let err = match ClientCore::open(Arc::clone(&conn)) {
        Ok(_) => panic!("a v1 store whose identity record does not decode must not open"),
        Err(e) => e,
    };
    assert_eq!(
        err,
        ClientError {
            code: "E_CORE_STORAGE",
            detail: "app_meta identity is malformed".into()
        }
    );
    let c = conn.lock().expect("lock");
    assert_eq!(
        meta_of(&c, "schema"),
        Some(vec![0x01]),
        "the migration rolled back with the backfill"
    );
    assert!(!columns(&c, "app_groups").iter().any(|n| n == "epoch"));
    assert!(!app_tables(&c).iter().any(|n| n == "app_read_state"));
    drop(c);
    drop(conn);
    std::fs::remove_file(&path).expect("cleanup");
}

#[test]
fn migrate_app_reports_the_schema_it_found() {
    let (path, conn) = fixture_copy("report");
    assert_eq!(
        migrate_app(&DillaStorage::new(Arc::clone(&conn))).expect("v1"),
        1
    );
    assert_eq!(
        meta_of(&conn.lock().expect("lock"), "schema"),
        Some(vec![0x0d])
    );
    assert_eq!(
        conn.lock()
            .expect("lock")
            .query_row(
                "SELECT epoch FROM app_groups WHERE group_id = ?1",
                [GROUP.as_slice()],
                |r| r.get::<_, i64>(0)
            )
            .expect("backfilled epoch"),
        2
    );
    let record = meta_of(&conn.lock().expect("lock"), "identity").expect("backfilled identity");
    assert_eq!(
        decode_strict(&record, |d| {
            d.array(13)?;
            let v = d.uint()?;
            for _ in 0..12 {
                d.skip()?;
            }
            Ok(v)
        })
        .expect("v2 identity"),
        2
    );
    assert_eq!(
        migrate_app(&DillaStorage::new(Arc::clone(&conn))).expect("v13"),
        13
    );
    assert_eq!(migrate_app(&DillaStorage::new(memory())).expect("fresh"), 3);
    drop(conn);
    std::fs::remove_file(&path).expect("cleanup");
}

/// The bytes a group id leaves inside the openmls tables' `group_id` key: the provider's CBOR
/// serialises the id's `Vec<u8>` as an array of integers, and each byte of GROUP and OTHER_GROUP
/// (0x44, 0x45 >= 24) is the two-byte integer `0x18, b`.
fn stored_id(id: &[u8; 16]) -> Vec<u8> {
    id.iter().flat_map(|b| [0x18, *b]).collect()
}

/// Pre-flight ruling (b): a group whose stored MLS state does not load fails the migration, in
/// `open` and in `migrate_app` alike; the unit rolls back and the store is exactly the v1 store it
/// was, so the next open migrates again. Attacker: none from outside. The store is device-local
/// and written only by this browser; a group state that does not decode was not written by this
/// build (or the disk failed). Keeping the row at epoch 0 instead would be silent: `take_group`
/// compares only id, kind, target and community, so nothing would report the wrong column.
#[test]
fn a_v1_store_whose_mls_group_fails_to_load_stays_v1() {
    let (path, conn) = fixture_copy("mls-load-error");
    let changed = conn
        .lock()
        .expect("lock")
        .execute(
            "UPDATE openmls_group_data SET group_data = x'00' \
             WHERE data_type = 'group_state' AND instr(group_id, ?1) > 0",
            [stored_id(&GROUP)],
        )
        .expect("corrupt GROUP's stored state");
    assert_eq!(changed, 1, "GROUP's group_state row");
    let before = {
        let c = conn.lock().expect("lock");
        (
            meta_of(&c, "identity").expect("identity"),
            column_info(&c, "app_groups"),
            app_tables(&c),
        )
    };

    let error = match ClientCore::open(Arc::clone(&conn)) {
        Ok(_) => panic!("a v1 store whose MLS group does not load must not open"),
        Err(e) => e,
    };
    assert_eq!(error.code, "E_CORE_STORAGE");
    let public_error = migrate_app(&DillaStorage::new(Arc::clone(&conn)))
        .expect_err("the public migration also rolls back a failed MLS load");
    assert!(
        matches!(public_error, dilla_core::mls::StorageError::Codec(_)),
        "{public_error:?}"
    );

    let c = conn.lock().expect("lock");
    assert_eq!(meta_of(&c, "schema"), Some(vec![0x01]), "still v1");
    assert_eq!(
        (
            meta_of(&c, "identity").expect("identity"),
            column_info(&c, "app_groups"),
            app_tables(&c),
        ),
        before,
        "the identity record, the columns and the tables are unchanged"
    );
    let flagged: i64 = c
        .query_row(
            "SELECT count(*) FROM pragma_table_info('app_messages') WHERE name = 'mention'",
            [],
            |r| r.get(0),
        )
        .expect("pragma");
    assert_eq!(flagged, 0, "no mention column");
    drop(c);
    drop(conn);
    std::fs::remove_file(&path).expect("cleanup");
}

/// Pre-flight ruling (b): a row whose MLS group is missing (no stored state, as after a discarded
/// resync) migrates with `epoch = 0, pending_commit = 0`, which is what the writers leave for it.
#[test]
fn a_v1_row_without_a_stored_mls_group_migrates_at_epoch_0() {
    let (path, conn) = fixture_copy("mls-missing");
    let deleted = conn
        .lock()
        .expect("lock")
        .execute(
            "DELETE FROM openmls_group_data WHERE instr(group_id, ?1) > 0",
            [stored_id(&OTHER_GROUP)],
        )
        .expect("drop OTHER_GROUP's stored state");
    assert!(deleted > 0, "OTHER_GROUP had stored state");
    let core = ClientCore::open(Arc::clone(&conn)).expect("open");
    let rows: Vec<_> = groups(&core)
        .into_iter()
        .map(|r| (r.group_id, r.state, r.epoch, r.pending_commit))
        .collect();
    assert_eq!(rows, vec![(GROUP, 2, 2, 0), (OTHER_GROUP, 2, 0, 0)]);
    assert_mls_columns(&core, &conn, "a migration with a missing group");
    drop(core);
    drop(conn);
    std::fs::remove_file(&path).expect("cleanup");
}

/// A web-1 store in phase 1 (pre-flight ruling (d), S2b): `signup_begin` ran and nothing else. It
/// is copied as `write_the_v1_fixture` copies Alice's store: the openmls tables whole (they hold
/// the DSK) and `app_meta` but its `schema` row, into a file created from `APP_SCHEMA_V1`.
#[test]
#[ignore = "writes tests/fixtures/app_v1_pending.db; run once with --ignored and commit the file"]
fn write_the_v1_pending_fixture() {
    let conn = memory();
    let mut core = ClientCore::open(Arc::clone(&conn)).expect("open");
    core.signup_begin(&INSTANCE).expect("signup_begin");
    drop(core);
    let path = Path::new(PENDING_FIXTURE);
    let _ = std::fs::remove_file(path);
    {
        let file = rusqlite::Connection::open(path).expect("create the fixture");
        file.execute_batch(APP_SCHEMA_V1)
            .expect("the v1 app schema");
        DillaStorage::new(Arc::new(Mutex::new(file)))
            .migrate()
            .expect("the openmls schema");
    }
    {
        let c = conn.lock().expect("lock");
        c.execute("ATTACH DATABASE ?1 AS v1", [PENDING_FIXTURE])
            .expect("attach");
        let tables: Vec<String> = {
            let mut stmt = c
                .prepare(
                    "SELECT name FROM main.sqlite_master WHERE type = 'table' \
                     AND (name LIKE 'openmls_%' OR name = 'storage_meta') ORDER BY name",
                )
                .expect("prepare");
            stmt.query_map([], |r| r.get(0))
                .expect("query")
                .collect::<Result<Vec<_>, _>>()
                .expect("names")
        };
        for t in &tables {
            c.execute(
                &format!("INSERT OR REPLACE INTO v1.\"{t}\" SELECT * FROM main.\"{t}\""),
                [],
            )
            .expect("copy an openmls table");
        }
        c.execute(
            "INSERT INTO v1.app_meta (k, v) SELECT k, v FROM main.app_meta WHERE k <> 'schema'",
            [],
        )
        .expect("copy app_meta");
        c.execute("DETACH DATABASE v1", []).expect("detach");
    }
    let file = rusqlite::Connection::open(path).expect("reopen the fixture");
    file.execute_batch("VACUUM").expect("vacuum");
    for (t, cols) in V1_COLUMNS {
        assert_eq!(
            columns(&file, t),
            cols.split(',').collect::<Vec<_>>(),
            "{t}"
        );
    }
    assert_eq!(meta_of(&file, "schema"), Some(vec![0x01]));
    assert!(meta_of(&file, "signup").is_some(), "a pending signup");
    assert!(meta_of(&file, "root_sealed").is_some(), "its sealed root");
    assert!(meta_of(&file, "identity").is_none(), "no identity yet");
}

/// Pre-flight ruling (d): a v1 store in phase 1 migrates (no identity record to rewrite, no
/// mention to flag, no group), stays in phase 1 with its signup record untouched, and
/// `signup_complete` then writes the thirteen-element v2 identity record.
#[test]
fn a_v1_pending_signup_migrates_and_completes_to_a_v2_identity() {
    let path = std::env::temp_dir().join(format!(
        "dilla-client-migrate-{}-pending.db",
        std::process::id()
    ));
    let _ = std::fs::remove_file(&path);
    std::fs::copy(PENDING_FIXTURE, &path).expect("copy the v1 pending fixture");
    let conn: ConnHandle = Arc::new(Mutex::new(
        rusqlite::Connection::open(&path).expect("open the copy"),
    ));
    let signup = {
        let c = conn.lock().expect("lock");
        assert_eq!(
            meta_of(&c, "schema"),
            Some(vec![0x01]),
            "the committed fixture is v1"
        );
        meta_of(&c, "signup").expect("the signup record")
    };
    let mut core = ClientCore::open(Arc::clone(&conn)).expect("open the v1 pending fixture");
    {
        let c = conn.lock().expect("lock");
        assert_eq!(meta_of(&c, "schema"), Some(vec![0x0d]));
        assert_eq!(
            meta_of(&c, "signup"),
            Some(signup),
            "the signup record is untouched"
        );
        assert_eq!(meta_of(&c, "identity"), None);
    }
    let phase = |core: &ClientCore| {
        decode_strict(&core.identity().expect("identity"), |d| {
            d.array(6)?;
            let phase = d.uint()?;
            for _ in 0..5 {
                d.skip()?;
            }
            Ok(phase)
        })
        .expect("identity shape")
    };
    assert_eq!(phase(&core), 1, "still a pending signup");
    core.signup_complete(&[0xa1; 16], "alice", NOW, &INSTANCE_SIGNER_PUB)
        .expect("signup_complete");
    assert_eq!(phase(&core), 2);
    let record = meta_of(&conn.lock().expect("lock"), "identity").expect("the identity record");
    let (len, version) = decode_strict(&record, |d| {
        let n = d.array_len()?;
        let v = d.uint()?;
        for _ in 1..n {
            d.skip()?;
        }
        Ok((n, v))
    })
    .expect("the identity record decodes");
    assert_eq!((len, version), (13, 2));
    drop(core);
    drop(conn);
    std::fs::remove_file(&path).expect("cleanup");
}

const V2_FIXTURE: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/tests/fixtures/app_v2.db");

/// web-2a's `APP_SCHEMA` (L-SQL-20), verbatim (`src/client/schema.rs:67-128` at `89ea4e0`).
const APP_SCHEMA_V2: &str = "
CREATE TABLE IF NOT EXISTS app_meta (k TEXT PRIMARY KEY, v BLOB NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_groups (
  group_id     BLOB    PRIMARY KEY CHECK (length(group_id) = 16),
  kind         INTEGER NOT NULL,
  community_id BLOB    CHECK (community_id IS NULL OR length(community_id) = 16),
  target_id    BLOB    NOT NULL CHECK (length(target_id) = 16),
  state        INTEGER NOT NULL,
  next_seq     INTEGER NOT NULL DEFAULT 1,
  acked_seq    INTEGER NOT NULL DEFAULT 0,
  acked_epoch  INTEGER NOT NULL DEFAULT 0,
  resync       INTEGER NOT NULL DEFAULT 0 CHECK (resync IN (0, 1)),
  was_gone     INTEGER NOT NULL DEFAULT 0 CHECK (was_gone IN (0, 1)),
  max_epoch    INTEGER NOT NULL DEFAULT 0,
  epoch          INTEGER NOT NULL DEFAULT 0,
  pending_commit INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE UNIQUE INDEX IF NOT EXISTS app_groups_by_target ON app_groups (target_id, kind) WHERE state <> 4;
CREATE TABLE IF NOT EXISTS app_handshake_tail (
  group_id BLOB NOT NULL, seq INTEGER NOT NULL, epoch INTEGER NOT NULL, kind INTEGER NOT NULL,
  sender INTEGER, blob BLOB NOT NULL, PRIMARY KEY (group_id, seq)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_proposals (
  group_id BLOB NOT NULL, ref BLOB NOT NULL, epoch INTEGER NOT NULL, PRIMARY KEY (group_id, ref)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_messages (
  group_id      BLOB    NOT NULL,
  seq           INTEGER NOT NULL,
  epoch         INTEGER NOT NULL,
  recv_ts       INTEGER NOT NULL,
  status        INTEGER NOT NULL,
  reason        TEXT    NOT NULL DEFAULT '',
  sender_user   BLOB,
  sender_device BLOB    NOT NULL,
  sender_leaf   INTEGER,
  sender_kind   INTEGER,
  sender_tier   INTEGER,
  msg_id        BLOB,
  type          INTEGER,
  body          TEXT    NOT NULL DEFAULT '',
  envelope      BLOB,
  franking_tag  BLOB    NOT NULL,
  mention       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (group_id, seq)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_messages_by_msg ON app_messages (group_id, msg_id);
CREATE TABLE IF NOT EXISTS app_outbox (
  msg_id   BLOB    PRIMARY KEY CHECK (length(msg_id) = 16),
  group_id BLOB    NOT NULL,
  envelope BLOB    NOT NULL,
  created  INTEGER NOT NULL,
  state    INTEGER NOT NULL,
  error    TEXT    NOT NULL DEFAULT '',
  epoch    INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_outbox_by_group ON app_outbox (group_id, created, msg_id);
CREATE TABLE IF NOT EXISTS app_read_state (
  group_id      BLOB    PRIMARY KEY CHECK (length(group_id) = 16),
  last_read_seq INTEGER NOT NULL DEFAULT 0,
  last_read_at  INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS app_settings (k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;
INSERT OR IGNORE INTO app_meta (k, v) VALUES ('schema', x'02');
";

/// The v2 columns of every app table but `app_meta`, in declaration order.
const V2_COLUMNS: [(&str, &str); 7] = [
    (
        "app_groups",
        "group_id,kind,community_id,target_id,state,next_seq,acked_seq,acked_epoch,resync,was_gone,max_epoch,epoch,pending_commit",
    ),
    ("app_handshake_tail", "group_id,seq,epoch,kind,sender,blob"),
    ("app_proposals", "group_id,ref,epoch"),
    (
        "app_messages",
        "group_id,seq,epoch,recv_ts,status,reason,sender_user,sender_device,sender_leaf,sender_kind,sender_tier,msg_id,type,body,envelope,franking_tag,mention",
    ),
    (
        "app_outbox",
        "msg_id,group_id,envelope,created,state,error,epoch",
    ),
    ("app_read_state", "group_id,last_read_seq,last_read_at"),
    ("app_settings", "k,v"),
];

/// The msg ids the v2 fixture's peer messages carry.
const X1: [u8; 16] = [0xc1; 16];
const X2: [u8; 16] = [0xc5; 16];
const UNHELD: [u8; 16] = [0xee; 16];

/// Alice (0xa1) created GROUP; a peer P (user 0xe5, device 0xe6) joined at seq 1 and a reader R
/// (user 0xd4, device 0xd5) at seq 2. Seq 3 is Alice's "alice says hi" (msg id A, random); then
/// 4 P's X1 "peer target"; 5 P's edit of X1 "peer target, edited"; 6 P's edit of A "hijacked"
/// (not the author); 7 P 👍 on A; 8 R 👍 on A; 9 R 🎉 on A; 10 R removes 🎉 from A; 11 P's X2
/// "to be deleted"; 12 R 👍 on X2; 13 P deletes X2 (the author); 14 R pins X1; 15 P 👍 on a msg
/// id nobody holds; 16 P's "replying to alice" with reply_to A. Alice alone in OTHER_GROUP with
/// "alone here". Fifteen messages; Alice's store is copied as a v2 store.
#[test]
#[ignore = "writes tests/fixtures/app_v2.db; run once with --ignored and commit the file"]
fn write_the_v2_fixture() {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut a = ready_core(0xa1, "alice");
    a.create_and_register(&mut relay, &instance);
    let mut p = RawPeer::new(0xe5, 0xe6);
    assert_eq!(p.join_external(&mut relay), 1);
    let mut r = RawPeer::new(0xd4, 0xd5);
    assert_eq!(r.join_external(&mut relay), 2);
    p.apply_commit(&relay, 2);
    a.sync(&relay);
    let (a_msg, a_seq) = a.send(&mut relay, &GROUP, "alice says hi", NOW + 1);
    assert_eq!(a_seq, 3);
    use EnvelopeType::{Delete, Edit, Message, Pin, ReactionAdd, ReactionRemove};
    let to_be_deleted = fold_envelope(X2, Message, None, "to be deleted");
    let sent = [
        p.send_envelope(&mut relay, &fold_envelope(X1, Message, None, "peer target")),
        p.send_envelope(
            &mut relay,
            &fold_envelope([0xc2; 16], Edit, Some(X1), "peer target, edited"),
        ),
        p.send_envelope(
            &mut relay,
            &fold_envelope([0xc3; 16], Edit, Some(a_msg), "hijacked"),
        ),
        p.send_envelope(
            &mut relay,
            &fold_envelope([0xc4; 16], ReactionAdd, Some(a_msg), "👍"),
        ),
        r.send_envelope(
            &mut relay,
            &fold_envelope([0xd1; 16], ReactionAdd, Some(a_msg), "👍"),
        ),
        r.send_envelope(
            &mut relay,
            &fold_envelope([0xd2; 16], ReactionAdd, Some(a_msg), "🎉"),
        ),
        r.send_envelope(
            &mut relay,
            &fold_envelope([0xd3; 16], ReactionRemove, Some(a_msg), "🎉"),
        ),
        p.send_envelope(&mut relay, &to_be_deleted),
        r.send_envelope(
            &mut relay,
            &fold_envelope([0xd4; 16], ReactionAdd, Some(X2), "👍"),
        ),
        p.send_envelope(&mut relay, &fold_envelope([0xc6; 16], Delete, Some(X2), "")),
        r.send_envelope(&mut relay, &fold_envelope([0xd5; 16], Pin, Some(X1), "")),
        p.send_envelope(
            &mut relay,
            &fold_envelope([0xc7; 16], ReactionAdd, Some(UNHELD), "👍"),
        ),
        p.send_envelope(
            &mut relay,
            &fold_envelope([0xc8; 16], Message, Some(a_msg), "replying to alice"),
        ),
    ];
    assert_eq!(sent, [4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16]);
    a.sync(&relay);

    let mut other = Relay::new(OTHER_GROUP);
    let body = a
        .core
        .group_create(
            &OTHER_GROUP,
            Some(&COMMUNITY),
            &OTHER_CHANNEL,
            POLICY,
            &instance.public(),
        )
        .expect("group_create");
    let created = other.register(&body).expect("register");
    let next_seq = decode_strict(&created, |d| {
        d.array(2)?;
        d.bytes_exact::<16>()?;
        d.uint()
    })
    .expect("201 body");
    a.core
        .group_registered(&OTHER_GROUP, next_seq)
        .expect("group_registered");
    a.send(&mut other, &OTHER_GROUP, "alone here", NOW + 7);

    let path = Path::new(V2_FIXTURE);
    let _ = std::fs::remove_file(path);
    {
        let file = rusqlite::Connection::open(path).expect("create the fixture");
        file.execute_batch(APP_SCHEMA_V2)
            .expect("the v2 app schema");
        DillaStorage::new(Arc::new(Mutex::new(file)))
            .migrate()
            .expect("the openmls schema");
    }
    {
        let c = a.probe.lock().expect("lock");
        c.execute("ATTACH DATABASE ?1 AS v2", [V2_FIXTURE])
            .expect("attach");
        let tables: Vec<String> = {
            let mut stmt = c
                .prepare(
                    "SELECT name FROM main.sqlite_master WHERE type = 'table' \
                     AND (name LIKE 'openmls_%' OR name = 'storage_meta') ORDER BY name",
                )
                .expect("prepare");
            stmt.query_map([], |r| r.get(0))
                .expect("query")
                .collect::<Result<Vec<_>, _>>()
                .expect("names")
        };
        for t in &tables {
            c.execute(
                &format!("INSERT OR REPLACE INTO v2.\"{t}\" SELECT * FROM main.\"{t}\""),
                [],
            )
            .expect("copy an openmls table");
        }
        for (t, cols) in V2_COLUMNS {
            c.execute(
                &format!("INSERT INTO v2.{t} ({cols}) SELECT {cols} FROM main.{t}"),
                [],
            )
            .expect("copy an app table");
        }
        c.execute(
            "INSERT INTO v2.app_meta (k, v) SELECT k, v FROM main.app_meta WHERE k <> 'schema'",
            [],
        )
        .expect("copy app_meta");
        // What web-2a's core stored for P's message X2, whatever this build folded: web-2a folded
        // nothing, so the deleted target is still a shown row (a no-op on a v2 build).
        c.execute(
            "UPDATE v2.app_messages SET status = 0, reason = '', body = ?3, envelope = ?4 \
             WHERE group_id = ?1 AND seq = ?2",
            rusqlite::params![
                GROUP.as_slice(),
                11i64,
                "to be deleted",
                to_be_deleted.encode().expect("encodes")
            ],
        )
        .expect("the v2 row of X2");
        c.execute("DETACH DATABASE v2", []).expect("detach");
    }
    let file = rusqlite::Connection::open(path).expect("reopen the fixture");
    file.execute_batch("VACUUM").expect("vacuum");
    for (t, cols) in V2_COLUMNS {
        assert_eq!(
            columns(&file, t),
            cols.split(',').collect::<Vec<_>>(),
            "{t}"
        );
    }
    assert_eq!(
        app_tables(&file),
        [
            "app_groups",
            "app_handshake_tail",
            "app_messages",
            "app_meta",
            "app_outbox",
            "app_proposals",
            "app_read_state",
            "app_settings"
        ]
    );
    assert_eq!(meta_of(&file, "schema"), Some(vec![0x02]));
    let total: i64 = file
        .query_row("SELECT count(*) FROM app_messages", [], |r| r.get(0))
        .expect("count");
    assert_eq!(total, 15, "fifteen messages");
    let x2: (i64, String) = file
        .query_row(
            "SELECT status, body FROM app_messages WHERE group_id = ?1 AND seq = 11",
            [GROUP.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .expect("X2");
    assert_eq!(x2, (0, "to be deleted".to_owned()));
    let folds: i64 = file
        .query_row(
            "SELECT count(*) FROM app_messages WHERE type BETWEEN 1 AND 6",
            [],
            |r| r.get(0),
        )
        .expect("count");
    assert_eq!(folds, 10, "ten rows of types 1-6");
}

#[test]
fn a_v2_store_opens_as_v4_with_its_folds_applied() {
    let (path, conn) = copy_of(V2_FIXTURE, "v2-open");
    let a_msg: [u8; 16] = {
        let c = conn.lock().expect("lock");
        assert_eq!(
            meta_of(&c, "schema"),
            Some(vec![0x02]),
            "the committed fixture is v2"
        );
        assert!(!columns(&c, "app_messages").iter().any(|n| n == "reply_to"));
        c.query_row(
            "SELECT msg_id FROM app_messages WHERE group_id = ?1 AND seq = 3",
            [GROUP.as_slice()],
            |r| r.get(0),
        )
        .expect("alice's message")
    };
    let core = ClientCore::open(Arc::clone(&conn)).expect("open the v2 fixture");
    {
        let c = conn.lock().expect("lock");
        assert_eq!(meta_of(&c, "schema"), Some(vec![0x0d]));
        let fresh = memory();
        drop(ClientCore::open(Arc::clone(&fresh)).expect("a fresh store"));
        let fresh = fresh.lock().expect("lock");
        assert_eq!(app_tables(&c), app_tables(&fresh));
        assert_eq!(app_indexes(&c), app_indexes(&fresh));
        for t in app_tables(&fresh) {
            assert_eq!(column_info(&c, &t), column_info(&fresh, &t), "{t}");
        }
        let mut stmt = c
            .prepare("SELECT seq, reply_to FROM app_messages WHERE group_id = ?1 ORDER BY seq")
            .expect("prepare");
        let replies: Vec<(i64, Option<Vec<u8>>)> = stmt
            .query_map([GROUP.as_slice()], |r| Ok((r.get(0)?, r.get(1)?)))
            .expect("query")
            .collect::<Result<_, _>>()
            .expect("rows");
        let to = |m: [u8; 16]| Some(m.to_vec());
        assert_eq!(
            replies,
            vec![
                (3, None),
                (4, None),
                (5, to(X1)),
                (6, to(a_msg)),
                (7, to(a_msg)),
                (8, to(a_msg)),
                (9, to(a_msg)),
                (10, to(a_msg)),
                (11, None),
                (12, to(X2)),
                (13, to(X2)),
                (14, to(X1)),
                (15, to(UNHELD)),
                (16, to(a_msg)),
            ]
        );
        let mut stmt = c
            .prepare("SELECT target, user_id, emoji, seq FROM app_reactions ORDER BY seq")
            .expect("prepare");
        let reactions: Vec<(Vec<u8>, Vec<u8>, String, i64)> = stmt
            .query_map([], |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)))
            .expect("query")
            .collect::<Result<_, _>>()
            .expect("rows");
        assert_eq!(
            reactions,
            vec![
                (a_msg.to_vec(), vec![0xe5; 16], "👍".to_owned(), 7),
                (a_msg.to_vec(), vec![0xd4; 16], "👍".to_owned(), 8),
            ],
            "R's 🎉 was removed, X2's 👍 went with X2, the unheld target's 👍 waits"
        );
        let pins: (Vec<u8>, i64, Vec<u8>) = c
            .query_row("SELECT target, seq, by_user FROM app_pins", [], |r| {
                Ok((r.get(0)?, r.get(1)?, r.get(2)?))
            })
            .expect("one pin");
        assert_eq!(pins, (X1.to_vec(), 14, vec![0xd4; 16]));
        let x2: (i64, String, Option<Vec<u8>>) = c
            .query_row(
                "SELECT status, body, envelope FROM app_messages WHERE group_id = ?1 AND seq = 11",
                [GROUP.as_slice()],
                |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
            )
            .expect("X2");
        assert_eq!(
            x2,
            (2, String::new(), None),
            "the author's delete folded at migration"
        );
        let hijack: (i64, String) = c
            .query_row(
                "SELECT status, body FROM app_messages WHERE group_id = ?1 AND seq = 6",
                [GROUP.as_slice()],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .expect("the ignored edit");
        assert_eq!(hijack, (0, "hijacked".to_owned()));
        let (total, purges, roles): (i64, i64, i64) = c
            .query_row(
                "SELECT (SELECT count(*) FROM app_messages), (SELECT count(*) FROM app_purges), (SELECT count(*) FROM app_roles)",
                [],
                |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
            )
            .expect("counts");
        assert_eq!((total, purges, roles), (15, 0, 0));
    }
    let rows = decode_timeline(&core.timeline(&GROUP, 0, 200).expect("timeline"));
    assert_eq!(
        rows.iter().map(|r| r.seq).collect::<Vec<_>>(),
        vec![3, 4, 11, 16],
        "fold rows are hidden"
    );
    let by = |seq: u64| rows.iter().find(|r| r.seq == seq).expect("row").clone();
    assert_eq!(
        (by(3).body, by(3).edited_seq, by(3).reactions, by(3).pinned),
        (
            "alice says hi".to_owned(),
            0,
            vec![("👍".to_owned(), 2, 0)],
            0
        )
    );
    assert_eq!(
        (by(4).body, by(4).edited_seq, by(4).pinned),
        ("peer target, edited".to_owned(), 5, 1)
    );
    assert_eq!((by(11).status, by(11).body), (2, String::new()));
    assert_eq!(
        by(16).reply,
        Some(Reply {
            reply_to: a_msg,
            target_seq: Some(3),
            target_user: Some([0xa1; 16]),
            excerpt: "alice says hi".into(),
            state: 0,
            thread_root: None
        })
    );
    let x1_ts = by(4).recv_ts;
    assert_eq!(
        decode_pins(&core.pins(&GROUP).expect("pins")),
        vec![PinRow {
            target_seq: 4,
            msg_id: X1,
            pinned_seq: 14,
            by_user: [0xd4; 16],
            author: Some([0xe5; 16]),
            excerpt: "peer target, edited".into(),
            target_ts: x1_ts,
            thread_root: None,
        }]
    );
    let other: Vec<String> =
        decode_timeline(&core.timeline(&OTHER_GROUP, 0, 200).expect("timeline"))
            .into_iter()
            .map(|r| r.body)
            .collect();
    assert_eq!(other, vec!["alone here".to_owned()]);
    drop(core);
    drop(conn);
    std::fs::remove_file(&path).expect("cleanup");
}

/// Lesson g and (ruled: AI-2): bytes that are not a nine-element envelope array were not written
/// by a core; the migration refuses them and rolls back, so the store stays exactly v2.
#[test]
fn a_v2_store_whose_v3_backfill_fails_stays_v2() {
    let (path, conn) = copy_of(V2_FIXTURE, "v2-rollback");
    let changed = conn
        .lock()
        .expect("lock")
        .execute(
            "UPDATE app_messages SET envelope = x'00' WHERE group_id = ?1 AND seq = 7",
            [GROUP.as_slice()],
        )
        .expect("corrupt one envelope");
    assert_eq!(changed, 1);
    let before = {
        let c = conn.lock().expect("lock");
        (
            column_info(&c, "app_messages"),
            app_tables(&c),
            app_indexes(&c),
        )
    };
    let err = match ClientCore::open(Arc::clone(&conn)) {
        Ok(_) => panic!("a store holding a non-envelope must not open"),
        Err(e) => e,
    };
    assert_eq!(
        err,
        ClientError {
            code: "E_CORE_STORAGE",
            detail: "app_messages envelope does not decode".into()
        }
    );
    assert!(matches!(
        migrate_app(&DillaStorage::new(Arc::clone(&conn))),
        Err(dilla_core::mls::StorageError::Codec(_))
    ));
    let c = conn.lock().expect("lock");
    assert_eq!(meta_of(&c, "schema"), Some(vec![0x02]), "still v2");
    assert_eq!(
        (
            column_info(&c, "app_messages"),
            app_tables(&c),
            app_indexes(&c)
        ),
        before
    );
    drop(c);
    drop(conn);
    std::fs::remove_file(&path).expect("cleanup");
}

/// (ruled: AI-2) and lesson e: an envelope web-2a's core accepted and this build refuses (a
/// reaction with no target, an attachment of eight elements) — which any member could have sent —
/// must not keep the store from opening.
/// Any member could have sent a web-2a browser these shapes; were the migration to
/// decode them, one message would keep that person's store from ever opening (lesson e). Mutations (f) and (g) of Step 11.
#[test]
fn a_v2_row_this_build_refuses_still_migrates() {
    fn legacy(kind: u64, reply_to: Option<&[u8; 16]>, eight_element_attachment: bool) -> Vec<u8> {
        let mut e = Encoder::new();
        e.array(9)
            .uint(1)
            .bytes(&[0x9a; 16])
            .uint(kind)
            .null()
            .opt_bytes(reply_to.map(|r| r.as_slice()))
            .text(if kind == 0 { "legacy" } else { "👍" });
        if eight_element_attachment {
            e.array(1);
            e.array(8)
                .bytes(&[0x03; 32])
                .bytes(&[0x04; 32])
                .bytes(&[0x05; 12])
                .uint(10)
                .text("image/png")
                .null()
                .null()
                .null();
        } else {
            e.array(0);
        }
        e.array(0);
        e.bytes(&[0x06; 32]);
        e.into_vec()
    }
    let (path, conn) = copy_of(V2_FIXTURE, "v2-legacy");
    let a_msg: [u8; 16] = {
        let c = conn.lock().expect("lock");
        let a_msg: [u8; 16] = c
            .query_row(
                "SELECT msg_id FROM app_messages WHERE group_id = ?1 AND seq = 3",
                [GROUP.as_slice()],
                |r| r.get(0),
            )
            .expect("alice's message");
        c.execute(
            "UPDATE app_messages SET envelope = ?2 WHERE group_id = ?1 AND seq = 7",
            rusqlite::params![GROUP.as_slice(), legacy(3, None, false)],
        )
        .expect("a reaction with no target");
        c.execute(
            "UPDATE app_messages SET envelope = ?2 WHERE group_id = ?1 AND seq = 16",
            rusqlite::params![GROUP.as_slice(), legacy(0, Some(&a_msg), true)],
        )
        .expect("an eight-element attachment");
        a_msg
    };
    let core = ClientCore::open(Arc::clone(&conn)).expect("open");
    let rows = decode_timeline(&core.timeline(&GROUP, 0, 200).expect("timeline"));
    let by = |seq: u64| rows.iter().find(|r| r.seq == seq).expect("row").clone();
    assert_eq!(
        by(3).reactions,
        vec![("👍".to_owned(), 1, 0)],
        "P's reaction has no target now; R's stays"
    );
    assert_eq!(
        by(16).attachments,
        Vec::new(),
        "a stored envelope this build refuses shows no files"
    );
    assert_eq!(
        by(16).reply.map(|r| r.reply_to),
        Some(a_msg),
        "its reply_to was still read"
    );
    drop(core);
    drop(conn);
    std::fs::remove_file(&path).expect("cleanup");
}

/// The database file's bytes with every WAL frame checkpointed into it: anything a refused open
/// wrote, in the file or in the WAL, shows up here.
fn file_bytes(conn: &ConnHandle, path: &Path) -> Vec<u8> {
    conn.lock()
        .expect("lock")
        .execute_batch("PRAGMA wal_checkpoint(TRUNCATE)")
        .expect("checkpoint");
    std::fs::read(path).expect("read the store")
}

/// A store a newer client moved to app schema 13 (a rollback from a later build) is
/// refused by `open` and by `migrate_app` with E_STORE_NEWER naming both numbers, and its bytes
/// are exactly as the newer client left them. Device-scoped Bot groups moved the supported schema to 12.
#[test]
fn a_v13_store_is_refused_untouched() {
    let (path, conn) = copy_of(V3_FIXTURE, "v13-refused");
    drop(ClientCore::open(Arc::clone(&conn)).expect("v12 open"));
    conn.lock()
        .expect("lock")
        .execute_batch("UPDATE app_meta SET v = x'0e' WHERE k = 'schema'; DROP TABLE app_roles;")
        .expect("simulate a v13 store");
    let before = file_bytes(&conn, &path);

    let error = match ClientCore::open(Arc::clone(&conn)) {
        Ok(_) => panic!("a v13 store must not open"),
        Err(e) => e,
    };
    assert_eq!(
        error,
        ClientError {
            code: "E_STORE_NEWER",
            detail: "found 14, supported 13".into(),
        }
    );
    assert_eq!(error.to_string(), "E_STORE_NEWER: found 14, supported 13");
    match migrate_app(&DillaStorage::new(Arc::clone(&conn))) {
        Err(dilla_core::mls::StorageError::StoreNewer { found, supported }) => {
            assert_eq!((found, supported), (14, 13));
        }
        other => panic!("migrate_app must refuse a v9 store, got {other:?}"),
    }
    assert_eq!(dilla_core::client::APP_SCHEMA_SUPPORTED, 13);
    assert!(
        file_bytes(&conn, &path) == before,
        "the refused store's bytes are unchanged"
    );
    assert_eq!(
        meta_of(&conn.lock().expect("lock"), "schema"),
        Some(vec![0x0e])
    );
    drop(conn);
    std::fs::remove_file(&path).expect("cleanup");
}

/// Every name in `sqlite_master`, sorted.
fn objects(conn: &ConnHandle) -> Vec<String> {
    let c = conn.lock().expect("lock");
    let mut stmt = c
        .prepare("SELECT name FROM sqlite_master ORDER BY name")
        .expect("prepare");
    stmt.query_map([], |r| r.get::<_, String>(0))
        .expect("query")
        .collect::<Result<Vec<_>, _>>()
        .expect("names")
}

/// A store seeded with only the given statements, for the refusals that must come before any
/// migration.
fn seeded(sql: &str) -> ConnHandle {
    let conn = memory();
    conn.lock().expect("lock").execute_batch(sql).expect("seed");
    conn
}

const APP_V13: &str = "CREATE TABLE app_meta (k TEXT PRIMARY KEY, v BLOB NOT NULL) WITHOUT ROWID;
    INSERT INTO app_meta (k, v) VALUES ('schema', x'0e');";
const PROVIDER_V2: &str = "CREATE TABLE storage_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
    INSERT INTO storage_meta (key, value) VALUES ('storage_provider_version', '2'), ('codec', 'cbor');";

fn refusal(conn: &ConnHandle) -> ClientError {
    match ClientCore::open(Arc::clone(conn)) {
        Ok(_) => panic!("the store must be refused"),
        Err(e) => e,
    }
}

/// The newer app schema is refused before the OpenMLS storage migration runs: no
/// `storage_meta` or `openmls_*` table is created in the newer client's store.
#[test]
fn a_v13_store_is_refused_before_the_openmls_migration() {
    let conn = seeded(APP_V13);
    let before = objects(&conn);
    assert_eq!(
        refusal(&conn),
        ClientError {
            code: "E_STORE_NEWER",
            detail: "found 14, supported 13".into(),
        }
    );
    assert_eq!(objects(&conn), before, "nothing was created");
}

/// C4x fix: a newer store whose OpenMLS provider version is newer too is still E_STORE_NEWER with
/// the app schema numbers, never E_CORE_STORAGE "UnsupportedStorage".
#[test]
fn a_v13_store_with_a_newer_provider_is_e_store_newer() {
    let conn = seeded(&format!("{APP_V13}{PROVIDER_V2}"));
    let before = objects(&conn);
    assert_eq!(
        refusal(&conn),
        ClientError {
            code: "E_STORE_NEWER",
            detail: "found 14, supported 13".into(),
        }
    );
    assert_eq!(objects(&conn), before, "nothing was created");
}

/// C4x fix: a store whose only newer part is the OpenMLS provider version is E_STORE_NEWER with
/// the provider's numbers, refused before the OpenMLS tables are created; another codec is not a
/// newer store and stays E_CORE_STORAGE.
#[test]
fn a_newer_provider_alone_is_e_store_newer_with_its_numbers() {
    let conn = seeded(PROVIDER_V2);
    let before = objects(&conn);
    assert_eq!(
        refusal(&conn),
        ClientError {
            code: "E_STORE_NEWER",
            detail: "found 2, supported 1".into(),
        }
    );
    assert_eq!(objects(&conn), before, "nothing was created");

    let other = seeded(
        "CREATE TABLE storage_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
         INSERT INTO storage_meta (key, value) VALUES ('storage_provider_version', '1'), ('codec', 'postcard');",
    );
    assert_eq!(refusal(&other).code, "E_CORE_STORAGE");
}

// Native only: the browser core links SQLite3MC through sqlite-wasm-rs, not SQLCipher.
#![cfg(not(target_arch = "wasm32"))]

//! The SQLCipher that rusqlite's `bundled-sqlcipher-vendored-openssl` feature compiles into the
//! native core is built on a SQLite with the RUSTSEC-2026-0459 fix (crafted `-wal` file, fixed in
//! SQLite 3.54.0; sqlcipher-src 420.0.0 bundles 3.54.1). The pin lives in
//! `core/vendor/libsqlite3-sys` through the workspace `[patch.crates-io]`; this test fails if that
//! patch is dropped while upstream libsqlite3-sys still bundles an older SQLite.

const MIN_SQLITE: (u32, u32, u32) = (3, 54, 1);

fn parse_version(v: &str) -> (u32, u32, u32) {
    let mut parts = v
        .split('.')
        .map(|p| p.parse::<u32>().expect("numeric version part"));
    let major = parts.next().expect("major");
    let minor = parts.next().expect("minor");
    let patch = parts.next().unwrap_or(0);
    (major, minor, patch)
}

#[test]
fn sqlcipher_connection_runs_on_sqlite_3_54_1_or_newer() {
    let path =
        std::env::temp_dir().join(format!("dilla-sqlcipher-build-{}.db", std::process::id()));
    let _ = std::fs::remove_file(&path);
    {
        let conn = rusqlite::Connection::open(&path).expect("open");
        conn.execute_batch(
            "PRAGMA key = \"x'4242424242424242424242424242424242424242424242424242424242424242'\";
             PRAGMA journal_mode = WAL;
             CREATE TABLE t(v BLOB); INSERT INTO t VALUES (x'00');",
        )
        .expect("keyed write");

        let cipher: String = conn
            .query_row("PRAGMA cipher_version", [], |r| r.get(0))
            .expect("cipher_version");
        assert!(
            !cipher.is_empty(),
            "PRAGMA cipher_version is empty: SQLCipher is not linked"
        );

        let sqlite: String = conn
            .query_row("SELECT sqlite_version()", [], |r| r.get(0))
            .expect("sqlite_version");
        assert!(
            parse_version(&sqlite) >= MIN_SQLITE,
            "SQLCipher {cipher} runs on SQLite {sqlite}, below {}.{}.{} (RUSTSEC-2026-0459)",
            MIN_SQLITE.0,
            MIN_SQLITE.1,
            MIN_SQLITE.2
        );
        assert!(
            rusqlite::version_number() >= 3_054_001,
            "{}",
            rusqlite::version()
        );
    }

    // The cipher is really active: the file on disk does not carry the plaintext SQLite header.
    let head = std::fs::read(&path).expect("read db");
    assert!(head.len() >= 16, "database file is too short");
    assert_ne!(
        &head[..16],
        b"SQLite format 3\0",
        "database file is not encrypted"
    );
    for suffix in ["", "-wal", "-shm"] {
        let mut p = path.clone().into_os_string();
        p.push(suffix);
        let _ = std::fs::remove_file(p);
    }
}

#[test]
fn version_parser_orders_releases() {
    assert!(parse_version("3.54.1") >= MIN_SQLITE);
    assert!(parse_version("3.55.0") >= MIN_SQLITE);
    assert!(parse_version("3.54.0") < MIN_SQLITE);
    assert!(parse_version("3.53.4") < MIN_SQLITE);
}

// Native only: the browser core links SQLite3MC through sqlite-wasm-rs.
#![cfg(not(target_arch = "wasm32"))]

//! The native store is SQLite3 Multiple Ciphers, compiled with its built-in crypto by the vendored
//! libsqlite3-sys's `bundled-sqlite3mc` feature (core/vendor/README.md), on a SQLite with the
//! RUSTSEC-2026-0459 fix (crafted `-wal` file, fixed in SQLite 3.54.0). No OpenSSL is linked:
//! its symbols collided with the BoringSSL inside LiveKit's libwebrtc (Task 6b).

const MIN_SQLITE: (u32, u32, u32) = (3, 54, 0);

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
fn the_native_sqlite_is_sqlite3mc_on_3_54_0_or_newer() {
    let conn = rusqlite::Connection::open_in_memory().expect("open");
    let mc: String = conn
        .query_row("SELECT sqlite3mc_version()", [], |r| r.get(0))
        .expect("sqlite3mc_version(): SQLite3 Multiple Ciphers is not linked");
    assert!(mc.contains("SQLite3 Multiple Ciphers"), "{mc}");

    let sqlite: String = conn
        .query_row("SELECT sqlite_version()", [], |r| r.get(0))
        .expect("sqlite_version");
    assert!(
        parse_version(&sqlite) >= MIN_SQLITE,
        "{mc} runs on SQLite {sqlite}, below {}.{}.{} (RUSTSEC-2026-0459)",
        MIN_SQLITE.0,
        MIN_SQLITE.1,
        MIN_SQLITE.2
    );
    assert!(
        rusqlite::version_number() >= 3_054_000,
        "{}",
        rusqlite::version()
    );
    // ChaCha20 is the compiled-in default, and the SQLCipher scheme is there for old stores.
    let default: String = conn
        .query_row("PRAGMA cipher", [], |r| r.get(0))
        .expect("PRAGMA cipher");
    assert_eq!(default, "chacha20");
    conn.execute_batch("PRAGMA cipher = 'sqlcipher'; PRAGMA legacy = 4;")
        .expect("the SQLCipher scheme is compiled in");
}

/// One SQLite in every native binary, and no OpenSSL anywhere in the workspace graph.
#[test]
fn the_lockfile_has_no_openssl_and_no_sqlcipher() {
    let lock = std::fs::read_to_string(concat!(env!("CARGO_MANIFEST_DIR"), "/../../Cargo.lock"))
        .expect("Cargo.lock");
    for banned in ["openssl-sys", "openssl-src", "sqlcipher-src"] {
        assert!(
            !lock.contains(&format!("name = \"{banned}\"")),
            "{banned} is back in Cargo.lock"
        );
    }
}

#[test]
fn version_parser_orders_releases() {
    assert!(parse_version("3.54.0") >= MIN_SQLITE);
    assert!(parse_version("3.54.1") >= MIN_SQLITE);
    assert!(parse_version("3.55.0") >= MIN_SQLITE);
    assert!(parse_version("3.53.4") < MIN_SQLITE);
}

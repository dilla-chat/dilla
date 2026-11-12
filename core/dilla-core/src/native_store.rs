//! Keying a native store file: SQLite3 Multiple Ciphers with a raw 32-byte key.
//!
//! Every native crate that opens an encrypted store (`dilla-bot-core`, the desktop client's
//! `dilla-client`) keys it here, so there is one order and one fallback.
//!
//! - A new store, and every store this build wrote, is ChaCha20-Poly1305 (sqlite3mc's `chacha20`)
//!   keyed with the raw key, exactly as the browser core keys its OPFS store
//!   (`core/dilla-core-wasm/src/store.rs`): `PRAGMA cipher = 'chacha20'`, then
//!   `PRAGMA key = 'raw:<hex>'`, then a real read. The order is load-bearing: the cipher must be
//!   chosen before the key, and `PRAGMA key` reports success whatever the key is, so only the read
//!   of `sqlite_schema` proves it.
//! - A store the SQLCipher 4 build wrote before Task 6b (the Bots deployed until then) is opened
//!   through sqlite3mc's SQLCipher scheme (`cipher = 'sqlcipher'`, `legacy = 4`, the same raw key
//!   in SQLCipher's `x'<hex>'` form). This is tried only when ChaCha20 reports `SQLITE_NOTADB`, on a
//!   fresh connection. Such a file stays SQLCipher 4 (AES-256-CBC with HMAC-SHA512, still sound):
//!   it is not rekeyed, because a rekey rewrites every page of a deployed Bot's WAL-mode store in
//!   place, and moving those files is a separate, separately tested step if it is ever wanted.
//!
//! The probing connection has checkpoint-on-close turned off until the key is proven, so a key that
//! does not decrypt the file can never make SQLite copy a hot WAL into it (sqlite3mc's WAL read
//! also fails on such a key; this is the second guard). Only a connection whose read succeeded gets
//! the default back.
//!
//! The key never appears in an error: failures are classified by SQLite result code only.

use std::path::Path;

use rusqlite::config::DbConfig;
use rusqlite::{Connection, ErrorCode, OpenFlags};
use zeroize::Zeroizing;

/// A store key's length in bytes.
pub const KEY_LEN: usize = 32;

/// The scheme a store file is encrypted with.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StoreCipher {
    /// sqlite3mc ChaCha20-Poly1305: every store this build creates.
    ChaCha20,
    /// SQLCipher 4 (raw key), as the pre-6b native build wrote it; kept as it is.
    SqlCipher4,
}

/// Why [`open_keyed`] refused.
#[derive(Debug, thiserror::Error)]
pub enum KeyError {
    /// The key is all zero.
    #[error("the store key is invalid")]
    InvalidKey,
    /// The file could not be opened, or the caller's `prepare` failed.
    #[error("the store could not be opened ({0:?})")]
    Open(Option<ErrorCode>),
    /// Neither scheme decrypts the file with this key (`SQLITE_NOTADB` under both): a wrong key,
    /// a damaged page 1, or a file that is not an encrypted store (plaintext SQLite included).
    #[error("the key does not decrypt the store")]
    WrongKey,
    /// This SQLite is not SQLite3 Multiple Ciphers with secure memory; nothing was keyed or written.
    #[error("this SQLite build cannot encrypt")]
    NotEncrypted,
    /// Any other failure while keying or reading (busy, I/O, ...): says nothing about the key.
    #[error("the store failed while being keyed ({0:?})")]
    Database(Option<ErrorCode>),
}

/// A connection whose key has been proven by a read.
#[derive(Debug)]
pub struct Keyed {
    pub conn: Connection,
    pub cipher: StoreCipher,
    /// `count(*)` of `sqlite_schema`: 0 for a new (empty) file.
    pub tables: i64,
}

/// Opens `path` with `flags`, runs `prepare` on the bare connection (busy timeout and the like;
/// nothing is read yet), keys it and proves the key with a read. ChaCha20 first; the SQLCipher 4
/// scheme only when ChaCha20 reports `SQLITE_NOTADB`. An empty file becomes a ChaCha20 store.
pub fn open_keyed(
    path: &Path,
    flags: OpenFlags,
    key: &[u8; KEY_LEN],
    prepare: impl Fn(&Connection) -> rusqlite::Result<()>,
) -> Result<Keyed, KeyError> {
    if key.iter().all(|b| *b == 0) {
        return Err(KeyError::InvalidKey);
    }
    match attempt(path, flags, key, &prepare, StoreCipher::ChaCha20) {
        Err(KeyError::WrongKey) => attempt(path, flags, key, &prepare, StoreCipher::SqlCipher4),
        other => other,
    }
}

fn attempt(
    path: &Path,
    flags: OpenFlags,
    key: &[u8; KEY_LEN],
    prepare: &impl Fn(&Connection) -> rusqlite::Result<()>,
    cipher: StoreCipher,
) -> Result<Keyed, KeyError> {
    let conn = Connection::open_with_flags(path, flags).map_err(|e| KeyError::Open(code(&e)))?;
    prepare(&conn).map_err(|e| KeyError::Open(code(&e)))?;
    // Until the key is proven, closing this connection must not checkpoint a WAL into the file.
    conn.set_db_config(DbConfig::SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE, true)
        .map_err(|e| KeyError::Database(code(&e)))?;
    require_sqlite3mc(&conn)?;
    conn.execute_batch("PRAGMA temp_store = MEMORY;")
        .map_err(|e| KeyError::Database(code(&e)))?;

    let hex = key_hex(key);
    let statement = Zeroizing::new(match cipher {
        StoreCipher::ChaCha20 => {
            format!(
                "PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:{}';",
                hex.as_str()
            )
        }
        StoreCipher::SqlCipher4 => format!(
            "PRAGMA cipher = 'sqlcipher'; PRAGMA legacy = 4; PRAGMA key = \"x'{}'\";",
            hex.as_str()
        ),
    });
    drop(hex);
    let keyed = conn.execute_batch(&statement);
    drop(statement);
    keyed.map_err(|e| classify(&e))?;

    let tables: i64 = conn
        .query_row("SELECT count(*) FROM sqlite_schema", [], |r| r.get(0))
        .map_err(|e| classify(&e))?;
    conn.set_db_config(DbConfig::SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE, false)
        .map_err(|e| KeyError::Database(code(&e)))?;
    Ok(Keyed {
        conn,
        cipher,
        tables,
    })
}

/// Refuses any SQLite that is not sqlite3mc with secure memory: there `PRAGMA cipher` and
/// `PRAGMA key` would be silently ignored and the store written in plaintext. Turns on wiping of
/// freed SQLite memory (a process-wide setting; sqlite3mc has no mlock).
///
/// A pragma, not `SELECT sqlite3mc_version()`: preparing a `SELECT` loads the schema, which on an
/// encrypted file fails before it is keyed. sqlite3mc answers its own pragmas without reading the
/// file; any other SQLite ignores an unknown pragma and returns no row.
fn require_sqlite3mc(conn: &Connection) -> Result<(), KeyError> {
    let level: String = conn
        .query_row("PRAGMA memory_security = fill", [], |r| r.get(0))
        .map_err(|_| KeyError::NotEncrypted)?;
    if level != "1" {
        return Err(KeyError::NotEncrypted);
    }
    Ok(())
}

fn key_hex(key: &[u8; KEY_LEN]) -> Zeroizing<String> {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut hex = Zeroizing::new(String::with_capacity(KEY_LEN * 2));
    for b in key {
        hex.push(char::from(HEX[usize::from(b >> 4)]));
        hex.push(char::from(HEX[usize::from(b & 0x0f)]));
    }
    hex
}

fn code(e: &rusqlite::Error) -> Option<ErrorCode> {
    e.sqlite_error_code()
}

/// `SQLITE_NOTADB` is the only evidence of a key that does not decrypt the file.
fn classify(e: &rusqlite::Error) -> KeyError {
    match code(e) {
        Some(ErrorCode::NotADatabase) => KeyError::WrongKey,
        other => KeyError::Database(other),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn key_hex_is_lowercase_and_complete() {
        let mut key = [0u8; KEY_LEN];
        key[0] = 0xab;
        key[31] = 0x0f;
        let hex = key_hex(&key);
        assert_eq!(hex.len(), 64);
        assert!(hex.starts_with("ab00"));
        assert!(hex.ends_with("000f"));
    }
}

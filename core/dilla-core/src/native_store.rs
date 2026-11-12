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
//! Legacy support covers a SQLCipher 4 file that is clean or has a WAL (hot or not; a Bot store
//! is in WAL mode). A **hot rollback journal** the SQLCipher build left is not supported: its
//! records are checksummed over ciphertext and sqlite3mc's over plaintext, so sqlite3mc would stop
//! at the first record or half-apply the rollback. [`open_keyed`] detects such a journal before
//! any connection exists (the first one would already play it back) and returns
//! [`KeyError::LegacyHotJournal`] with both files untouched; only the old build can recover it.
//! The reverse holds too: rolling a store back to the old SQLCipher build is safe when it is clean
//! or in WAL mode, not when this build left a hot rollback journal.
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
    /// The file has a hot rollback journal that the SQLCipher build wrote (its records are
    /// checksummed over ciphertext; sqlite3mc checksums plaintext). sqlite3mc cannot play it back
    /// correctly, so nothing was opened and neither file was touched. Only the pre-6b SQLCipher
    /// build can recover it.
    #[error("the store has an unfinished transaction from the SQLCipher build")]
    LegacyHotJournal,
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
    // Before any connection: the first one plays a hot journal back (and truncates the file).
    if sqlcipher_hot_journal(path)? {
        return Err(KeyError::LegacyHotJournal);
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

    let statement = key_statement(key, cipher);
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

/// The keying statement for `cipher`, built in one exact-capacity buffer so the key's hex never
/// sits in a reallocated (and so unwiped) copy.
fn key_statement(key: &[u8; KEY_LEN], cipher: StoreCipher) -> Zeroizing<String> {
    let (head, tail) = match cipher {
        StoreCipher::ChaCha20 => ("PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:", "';"),
        StoreCipher::SqlCipher4 => (
            "PRAGMA cipher = 'sqlcipher'; PRAGMA legacy = 4; PRAGMA key = \"x'",
            "'\";",
        ),
    };
    let mut s = Zeroizing::new(String::with_capacity(head.len() + KEY_LEN * 2 + tail.len()));
    s.push_str(head);
    push_hex(&mut s, key);
    s.push_str(tail);
    s
}

fn push_hex(out: &mut String, key: &[u8; KEY_LEN]) {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    for b in key {
        out.push(char::from(HEX[usize::from(b >> 4)]));
        out.push(char::from(HEX[usize::from(b & 0x0f)]));
    }
}

/// Whether `<path>-journal` is a hot rollback journal the SQLCipher build wrote.
///
/// Both builds write encrypted page images into the journal, and both protect each record with
/// SQLite's journal checksum (`cksum = nonce + data[n - 200] + data[n - 400] + ...`, from
/// `pager_cksum`). SQLCipher computes it over the **ciphertext** it writes; sqlite3mc over the
/// plaintext page, encrypting only on the way to disk. So the first record's checksum, computed
/// over the bytes on disk, matches only for a SQLCipher journal (for a sqlite3mc one with odds of
/// 2^-32, and then the store fails closed). sqlite3mc cannot play a SQLCipher journal back: it
/// stops at the first record (corrupt) or, worse, "finishes" with a transaction half rolled back.
///
/// A journal SQLite would not play back (absent, empty, zeroed header, no complete record) is not
/// one; `Database` only when the journal exists but cannot be read.
fn sqlcipher_hot_journal(path: &Path) -> Result<bool, KeyError> {
    use std::io::Read;

    const MAGIC: [u8; 8] = [0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7];
    let mut name = path.as_os_str().to_owned();
    name.push("-journal");
    let file = match std::fs::File::open(&name) {
        Ok(f) => f,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(false),
        Err(_) => return Err(KeyError::Database(None)),
    };
    // At most a maximal header sector plus one maximal record.
    let mut journal = Vec::new();
    file.take(65536 + 4 + 65536 + 4)
        .read_to_end(&mut journal)
        .map_err(|_| KeyError::Database(None))?;
    let be32 = |at: usize| -> Option<u32> {
        journal
            .get(at..at + 4)
            .map(|b| u32::from_be_bytes([b[0], b[1], b[2], b[3]]))
    };
    if journal.get(..8) != Some(MAGIC.as_slice()) {
        return Ok(false);
    }
    let (Some(n_rec), Some(nonce), Some(sector), Some(page)) =
        (be32(8), be32(12), be32(20), be32(24))
    else {
        return Ok(false);
    };
    let pow2 = |v: u32, lo: u32| v.is_power_of_two() && (lo..=65536).contains(&v);
    if n_rec == 0 || !pow2(sector, 32) || !pow2(page, 512) {
        return Ok(false);
    }
    // The first record follows the header, which is padded to the sector size.
    let (start, page) = (sector as usize + 4, page as usize);
    let (Some(data), Some(stored)) = (journal.get(start..start + page), be32(start + page)) else {
        return Ok(false);
    };
    let mut sum = nonce;
    let mut i = page - 200;
    while i > 0 {
        sum = sum.wrapping_add(u32::from(data[i]));
        i = i.saturating_sub(200);
    }
    Ok(sum == stored)
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
    fn key_statements_are_exact_and_never_reallocate() {
        let mut key = [0u8; KEY_LEN];
        key[0] = 0xab;
        key[31] = 0x0f;
        let hex = format!("ab{}0f", "00".repeat(30));
        let chacha = key_statement(&key, StoreCipher::ChaCha20);
        assert_eq!(
            chacha.as_str(),
            format!("PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:{hex}';")
        );
        assert_eq!(chacha.len(), chacha.capacity());
        let legacy = key_statement(&key, StoreCipher::SqlCipher4);
        assert_eq!(
            legacy.as_str(),
            format!("PRAGMA cipher = 'sqlcipher'; PRAGMA legacy = 4; PRAGMA key = \"x'{hex}'\";")
        );
        assert_eq!(legacy.len(), legacy.capacity());
    }
}

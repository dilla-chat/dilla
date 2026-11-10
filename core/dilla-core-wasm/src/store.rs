//! The browser store: OPFS sahpool + SQLite3 Multiple Ciphers `chacha20` (gap-13, gap-11, R6).
//!
//! wasm-only: `install()` returns `NotSupported` outside a dedicated worker in a secure context,
//! and `rusqlite` on this target is the `sqlite-wasm-rs` FFI backend. The target gate is on
//! `lib.rs`'s `pub mod store;` declaration, not an inner `#![cfg(…)]` here — one gate, one place, and
//! the re-exports in `lib.rs` carry the identical cfg.

use core::cell::RefCell;
use std::rc::Rc;

use dilla_core::identity::{CredentialIdentity, Kind, SignerTier, SskSigner, Tier, UmkSigner};
use dilla_core::ids::{DeviceId, InstanceId, UserId};
use dilla_core::mls::{
    CIPHERSUITE, ConnHandle, DillaBinding, DillaGroup, DillaProvider, GroupKind,
};
use js_sys::Reflect;
use openmls::prelude::{BasicCredential, CredentialWithKey, GroupId};
use openmls_basic_credential::SignatureKeyPair;
#[cfg(feature = "test-hooks")]
use rusqlite::OptionalExtension;
use rusqlite::{Connection, OpenFlags};
use sqlite_wasm_rs::WasmOsCallback;
use sqlite_wasm_vfs::sahpool::{OpfsSAHError, OpfsSAHPoolCfgBuilder, OpfsSAHPoolUtil, install};
use wasm_bindgen::prelude::*;
use zeroize::Zeroizing;

/// The pool registers under the plain name; sqlite3mc creates the encrypting wrapper lazily
/// (gap-13 §3). Opening `opfs-sahpool` directly fails at `PRAGMA key` with the exact message
/// "Setting key failed. Encryption is not supported by the VFS."
const POOL_VFS: &str = "opfs-sahpool";
const ENCRYPTED_VFS: &str = "multipleciphers-opfs-sahpool";
/// Slots include journals; 12 leaves room for one database plus its `-journal` and `-wal`.
const INITIAL_CAPACITY: u32 = 12;
/// Deviation A2-14: the message for a failure of the keyed-pragma batch. Fixed, because the only
/// detail rusqlite could add is built from the statement text, and that statement contains the KEK.
const E_STORE_CIPHER: &str = "E_STORE_CIPHER: setting the cipher or key failed";

#[wasm_bindgen]
#[derive(Clone)]
pub struct StoreOpenConfig {
    directory: String,
    db_name: String,
    kek_hex: String,
}

#[wasm_bindgen]
impl StoreOpenConfig {
    #[wasm_bindgen(constructor)]
    pub fn new(directory: String, db_name: String, kek_hex: String) -> StoreOpenConfig {
        StoreOpenConfig {
            directory,
            db_name,
            kek_hex,
        }
    }
    #[wasm_bindgen(getter)]
    pub fn directory(&self) -> String {
        self.directory.clone()
    }
    #[wasm_bindgen(setter)]
    pub fn set_directory(&mut self, v: String) {
        self.directory = v;
    }
    #[wasm_bindgen(getter)]
    pub fn db_name(&self) -> String {
        self.db_name.clone()
    }
    #[wasm_bindgen(setter)]
    pub fn set_db_name(&mut self, v: String) {
        self.db_name = v;
    }
    #[wasm_bindgen(getter)]
    pub fn kek_hex(&self) -> String {
        self.kek_hex.clone()
    }
    #[wasm_bindgen(setter)]
    pub fn set_kek_hex(&mut self, v: String) {
        self.kek_hex = v;
    }
}

/// NV-7: the variant carrying the `createSyncAccessHandle` DOMException on 0.2.0.
fn error_value(e: &OpfsSAHError) -> JsValue {
    match e {
        OpfsSAHError::CreateSyncAccessHandle(v) => v.clone(),
        other => JsValue::from(js_sys::Error::new(&format!("E_STORE_INSTALL: {other}"))),
    }
}

/// True when the error is OPFS sync-access-handle contention. This is SQLite upstream's own
/// predicate, including the documented Chromium `name` inconsistency (gap-14 §4). Never match on
/// the Display string: 0.2.0's omits the DOMException name entirely.
#[wasm_bindgen]
pub fn is_sah_contention(err: &JsValue) -> bool {
    let prop = |k: &str| {
        Reflect::get(err, &JsValue::from_str(k))
            .ok()
            .and_then(|v| v.as_string())
    };
    match (prop("name").as_deref(), prop("message").as_deref()) {
        (Some("NoModificationAllowedError"), _) => true,
        (Some("DOMException"), Some(m)) => m.starts_with("Access Handles cannot"),
        _ => false,
    }
}

/// Owns one connection handle and lends clones with `share()`. Pause and close refuse while any
/// clone lives, preserving the storage ownership invariant across OPFS suspension.
#[wasm_bindgen]
pub struct StoreHandle {
    util: OpfsSAHPoolUtil,
    conn: RefCell<Option<ConnHandle>>,
    db_name: String,
    /// Deviation A2-14. The raw 256-bit device KEK, kept only because `resume()` has to replay
    /// `PRAGMA key` after `unpause_vfs()`. `Zeroizing` wipes it when the handle drops; without it the
    /// key would sit in freed wasm linear memory for the life of the page.
    kek_hex: Zeroizing<String>,
}

fn open_encrypted(db_name: &str, kek_hex: &str) -> Result<Connection, JsValue> {
    let conn = Connection::open_with_flags_and_vfs(db_name, OpenFlags::default(), ENCRYPTED_VFS)
        .map_err(|e| JsValue::from(js_sys::Error::new(&format!("E_STORE_OPEN: {e}"))))?;
    // Order is load-bearing: cipher, then key, then a real read. `PRAGMA key` always reports ok
    // (gap-13 §2.2), so the SELECT is the key check. The statement text carries the key, so it is
    // built into a Zeroizing<String> and wiped as soon as SQLite has parsed it (deviation A2-14).
    let pragmas = Zeroizing::new(format!(
        "PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:{kek_hex}';"
    ));
    // Deviation A2-14. The rusqlite error is deliberately dropped instead of being formatted into
    // the message: `Error::SqlInputError` Displays as "{msg} in {sql} at offset {offset}" — the
    // whole statement text — and this statement carries the raw device KEK, which `store_open`
    // would then hand to JavaScript as an Error the worker logs. Only the SQLite result code is
    // safe to keep, and it adds nothing a caller can act on here, so the message is fixed.
    conn.execute_batch(&pragmas)
        .map_err(|_| JsValue::from(js_sys::Error::new(E_STORE_CIPHER)))?;
    drop(pragmas);
    conn.query_row("SELECT count(*) FROM sqlite_schema", [], |r| {
        r.get::<_, i64>(0)
    })
    .map_err(|e| JsValue::from(js_sys::Error::new(&format!("E_STORE_KEY: {e}"))))?;
    Ok(conn)
}

/// Deviation A2-3: the error type is `JsValue`, not `JsError`, so the original DOMException reaches
/// [`is_sah_contention`] unchanged. One attempt only — gap-14 §5's 6-attempt schedule lives in the
/// worker, which is where the attempt count is observable.
#[wasm_bindgen]
pub async fn store_open(cfg: StoreOpenConfig) -> Result<StoreHandle, JsValue> {
    if cfg.kek_hex.len() != 64 || !cfg.kek_hex.bytes().all(|b| b.is_ascii_hexdigit()) {
        return Err(JsValue::from(js_sys::Error::new(
            "E_STORE_KEK: kek_hex must be 64 hex characters",
        )));
    }
    let pool_cfg = OpfsSAHPoolCfgBuilder::new()
        .vfs_name(POOL_VFS)
        .directory(&cfg.directory)
        .clear_on_init(false)
        .initial_capacity(INITIAL_CAPACITY)
        .build();
    // default_vfs = false: making the pool the default would displace the sqlite3mc-wrapped default
    // and PRAGMA key would fail (gap-13 §3). The bound on 0.2.0 is `C: OsCallback` alone.
    let util = install::<WasmOsCallback>(&pool_cfg, false)
        .await
        .map_err(|e| error_value(&e))?;
    let conn = open_encrypted(&cfg.db_name, &cfg.kek_hex)?;
    Ok(StoreHandle {
        util,
        conn: RefCell::new(Some(Rc::new(RefCell::new(conn)))),
        db_name: cfg.db_name,
        kek_hex: Zeroizing::new(cfg.kek_hex),
    })
}

impl StoreHandle {
    /// Test-build-only v4-to-v3 downgrade of the encrypted open connection.
    #[cfg(feature = "test-hooks")]
    pub fn test_downgrade_v3(&self) -> Result<(), JsError> {
        self.with_conn(|c| {
            c.execute_batch(
                "DROP TRIGGER IF EXISTS app_search_rows_insert_v2;
            DROP TRIGGER IF EXISTS app_search_rows_delete_v2;
            DROP TRIGGER IF EXISTS app_search_rows_update_v2;
            DROP TABLE IF EXISTS app_search_v2;
            DROP TRIGGER IF EXISTS app_search_rows_insert;
            DROP TRIGGER IF EXISTS app_search_rows_delete;
            DROP TRIGGER IF EXISTS app_search_rows_update;
            DROP TABLE IF EXISTS app_search;
            DROP TABLE IF EXISTS app_search_rows;
            DROP TABLE IF EXISTS app_authority;
            DROP TABLE IF EXISTS app_authority_known;
            DROP TABLE IF EXISTS app_device_lists;
            DROP TABLE IF EXISTS app_trust_pins;
            DROP TABLE IF EXISTS app_quarantine;
            DROP TABLE IF EXISTS app_trust_audit;
            DROP INDEX IF EXISTS app_messages_legacy_sender;
            DROP INDEX IF EXISTS app_messages_live_by_sig;
            DROP INDEX IF EXISTS app_quarantine_device;
            DROP INDEX IF EXISTS app_pending_history_by_leaf_seq;
            DROP TABLE IF EXISTS app_pending_history;
            DELETE FROM app_meta WHERE k='search_fts5';
            DELETE FROM app_meta WHERE k='search_terms_v2';
            DELETE FROM app_meta WHERE k='search_contract_v4';
            UPDATE app_meta SET v=x'03' WHERE k='schema';",
            )
        })
    }

    /// Exact encrypted-store object and capability inventory before a test migration reopens it.
    #[cfg(feature = "test-hooks")]
    pub fn test_schema_inventory(&self) -> Result<Box<[u8]>, JsError> {
        self.with_conn(|c| {
            let mut objects =
                c.prepare("SELECT name FROM sqlite_master WHERE name LIKE 'app_%' ORDER BY name")?;
            let names = objects
                .query_map([], |r| r.get::<_, String>(0))?
                .collect::<Result<Vec<_>, _>>()?;
            let mut meta =
                c.prepare("SELECT k FROM app_meta WHERE k LIKE 'search_%' ORDER BY k")?;
            let markers = meta
                .query_map([], |r| r.get::<_, String>(0))?
                .collect::<Result<Vec<_>, _>>()?;
            let mut e = dilla_core::cbor::Encoder::new();
            e.array(2).array(names.len());
            for name in names {
                e.text(&name);
            }
            e.array(markers.len());
            for marker in markers {
                e.text(&marker);
            }
            Ok(e.into_vec().into_boxed_slice())
        })
    }

    /// Test-build-only capability probe, encoded as [schema, search_fts5].
    #[cfg(feature = "test-hooks")]
    pub fn test_schema_capability(&self) -> Result<Box<[u8]>, JsError> {
        self.with_conn(|c| {
            let schema: Vec<u8> =
                c.query_row("SELECT v FROM app_meta WHERE k='schema'", [], |r| r.get(0))?;
            let capability: Option<Vec<u8>> = c
                .query_row("SELECT v FROM app_meta WHERE k='search_fts5'", [], |r| {
                    r.get(0)
                })
                .optional()?;
            let mut e = dilla_core::cbor::Encoder::new();
            e.array(2)
                .uint(u64::from(*schema.first().unwrap_or(&0)))
                .uint(u64::from(
                    capability
                        .as_deref()
                        .and_then(|v| v.first())
                        .copied()
                        .unwrap_or(0),
                ));
            Ok(e.into_vec().into_boxed_slice())
        })
    }
    fn shared(&self) -> Option<ConnHandle> {
        self.conn.borrow().as_ref().map(Rc::clone)
    }

    #[allow(dead_code)] // task 7's facade consumes this; this task's probe uses shared directly.
    pub(crate) fn share(&self) -> Result<ConnHandle, JsError> {
        self.shared()
            .ok_or_else(|| JsError::new("E_STORE_PAUSED: the store is paused"))
    }

    fn with_conn<T>(
        &self,
        f: impl FnOnce(&Connection) -> rusqlite::Result<T>,
    ) -> Result<T, JsError> {
        let guard = self.conn.borrow();
        let conn = guard
            .as_ref()
            .ok_or_else(|| JsError::new("E_STORE_PAUSED: the store is paused"))?;
        let borrowed = conn
            .try_borrow()
            .map_err(|_| JsError::new("E_STORE_SQL: the connection is in use"))?;
        f(&borrowed).map_err(|e| JsError::new(&format!("E_STORE_SQL: {e}")))
    }
}

#[wasm_bindgen]
impl StoreHandle {
    pub fn exec(&self, sql: &str) -> Result<(), JsError> {
        self.with_conn(|c| c.execute_batch(sql))
    }

    pub fn query_scalar_i64(&self, sql: &str) -> Result<i64, JsError> {
        self.with_conn(|c| c.query_row(sql, [], |r| r.get::<_, i64>(0)))
    }

    /// `get_capacity` — the 0.2.0 spelling (gap-11 §0), not `capacity()`.
    pub fn capacity(&self) -> u32 {
        self.util.get_capacity()
    }

    /// `reserve_minimum_capacity` — the 0.2.0 spelling, not `ensure_capacity`.
    ///
    /// NV-13: `&self` on an exported `async fn` is what step 1 item 5 confirms. wasm-bindgen
    /// 0.2.128 accepts a borrowed receiver on an exported `async fn` — its codegen selects
    /// `LongRefFromWasmAbi` for exactly this case — so §2.11's signature stands unchanged.
    /// `reserve_capacity` is called for real by task 17's worker right after open; `resume()` is not
    /// exercised by the week-1 spike, which only ever pauses on the resign path.
    pub async fn reserve_capacity(&self, n: u32) -> Result<(), JsError> {
        self.util
            .reserve_minimum_capacity(n)
            .await
            .map_err(|e| JsError::new(&format!("E_STORE_CAPACITY: {e}")))
    }

    /// Deviation A2-4: drops the connection first, after refusing any live lent clones.
    pub fn pause(&self) -> Result<(), JsError> {
        let mut slot = self.conn.borrow_mut();
        if let Some(conn) = slot.as_ref() {
            let lent = Rc::strong_count(conn) - 1;
            if lent > 0 {
                return Err(JsError::new(&format!(
                    "E_STORE_BUSY: {lent} handles are still open"
                )));
            }
        }
        drop(slot.take());
        drop(slot);
        self.util
            .pause_vfs()
            .map_err(|e| JsError::new(&format!("E_STORE_PAUSE: {e}")))
    }

    /// Deviation A2-4: reacquires the slots, then reopens the connection and replays the pragmas.
    pub async fn resume(&self) -> Result<(), JsError> {
        self.util
            .unpause_vfs()
            .await
            .map_err(|e| JsError::new(&format!("E_STORE_RESUME: {e}")))?;
        // `open_encrypted` already redacts the one arm whose statement carries the KEK (A2-14).
        // The JsValue is still dropped rather than re-wrapped here, so a future arm added over
        // there cannot leak through this path either.
        let conn = open_encrypted(&self.db_name, &self.kek_hex)
            .map_err(|_| JsError::new("E_STORE_RESUME: reopening the keyed connection failed"))?;
        *self.conn.borrow_mut() = Some(Rc::new(RefCell::new(conn)));
        Ok(())
    }

    /// JS still sees `close(): void`; a live clone causes an `E_STORE_BUSY` throw.
    pub fn close(self) -> Result<(), JsError> {
        let mut slot = self.conn.borrow_mut();
        if let Some(conn) = slot.as_ref() {
            let lent = Rc::strong_count(conn) - 1;
            if lent > 0 {
                return Err(JsError::new(&format!(
                    "E_STORE_BUSY: {lent} handles are still open"
                )));
            }
        }
        drop(slot.take());
        Ok(())
    }
}

/// Deviation A2-14. The fixed stand-in for a rusqlite error raised by a statement that carries key
/// material and whose `Display` is not known to be key-free.
const E_STORE_PROBE_REDACTED: &str =
    "E_STORE_PROBE: the keyed pragma batch failed (detail withheld: it can carry the statement)";

/// Deviation A2-14, for deviation A2-10's two probes. The only rendering of a rusqlite error that
/// either of them may hand back, because the statement they ran is
/// `PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:<KEK>';` and the caller passes a real device KEK.
///
/// Keeps SQLite's own primary message (`sqlite3_errmsg`, e.g. "Setting key failed. Encryption is
/// not supported by the VFS." — the string the spike asserts on) and withholds every other
/// variant's `Display`, several of which embed text the caller handed to SQLite.
/// `rusqlite::Error::SqlInputError` is the worst of them: it renders as
/// `"{msg} in {sql} at offset {offset}"` — the whole statement — and `execute_batch` produces it
/// for a prepare-time failure whose result code maps to `ErrorCode::Unknown` (which is what
/// SQLITE_ERROR, the code sqlite3mc raises here, does) whenever `sqlite3_error_offset` is
/// non-negative (rusqlite-0.41.1/src/error.rs:341-346 and :493-500). rusqlite 0.41 dropped the
/// `modern_sqlite` gate on that variant, so under the `ffi-sqlite-wasm-rs` backend the leak is now
/// reachable; this guard is what withholds it (tests/node.rs asserts it on a real `SqlInputError`).
pub fn redacted_sqlite_message(e: &rusqlite::Error) -> String {
    match e {
        rusqlite::Error::SqliteFailure(_, Some(msg)) => msg.clone(),
        rusqlite::Error::SqliteFailure(code, None) => code.to_string(),
        _ => E_STORE_PROBE_REDACTED.to_owned(),
    }
}

/// Deviation A2-10. Proves gap-13 §3's trap is real and stays real: a connection opened on the
/// pool's **plain** VFS name cannot be keyed. Returns SQLite's primary error message (through
/// [`redacted_sqlite_message`] — deviation A2-14: the statement this runs carries the caller's
/// KEK), and errors if the open unexpectedly succeeds — that would mean the encrypting wrapper is
/// no longer needed and the store's VFS choice must be revisited. Call only after `store_open` has
/// installed the pool.
#[wasm_bindgen]
pub fn unencrypted_vfs_probe(db_name: &str, kek_hex: &str) -> Result<String, JsError> {
    let conn = Connection::open_with_flags_and_vfs(db_name, OpenFlags::default(), POOL_VFS)
        .map_err(|e| JsError::new(&format!("E_STORE_OPEN: {e}")))?;
    let pragmas = Zeroizing::new(format!(
        "PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:{kek_hex}';"
    ));
    match conn.execute_batch(&pragmas) {
        Ok(()) => Err(JsError::new(
            "E_STORE_PROBE: the plain VFS accepted a key; gap-13 §3 no longer holds",
        )),
        Err(e) => Ok(redacted_sqlite_message(&e)),
    }
}

/// Deviation A2-10. Opens an **existing, written** database on the encrypting VFS with the wrong key
/// and returns the text of the failure, so the spike can assert where it happened. gap-13 §2.2: the
/// `PRAGMA key` statement always reports ok, so the only evidence the store is really encrypted is
/// that the first real read fails. Returns an error if the wrong key is accepted all the way through
/// the SELECT — that would mean the database is not encrypted at all.
///
/// Call only after `store_open` has installed the pool **and** something has been written: on an
/// empty database there is no page to decrypt and any key succeeds.
#[wasm_bindgen]
pub async fn wrong_key_probe(db_name: &str, kek_hex: &str) -> Result<String, JsError> {
    if kek_hex.len() != 64 || !kek_hex.bytes().all(|b| b.is_ascii_hexdigit()) {
        return Err(JsError::new(
            "E_STORE_KEK: kek_hex must be 64 hex characters",
        ));
    }
    let conn = Connection::open_with_flags_and_vfs(db_name, OpenFlags::default(), ENCRYPTED_VFS)
        .map_err(|e| JsError::new(&format!("E_STORE_OPEN: {e}")))?;
    let pragmas = Zeroizing::new(format!(
        "PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:{kek_hex}';"
    ));
    // Deviation A2-14: the rusqlite error is rendered through the redacting helper, never with
    // `Display` — this statement carries the key the caller passed.
    conn.execute_batch(&pragmas).map_err(|e| {
        JsError::new(&format!(
            "E_STORE_PROBE: the pragmas rejected the wrong key, so this probe proves nothing: {}",
            redacted_sqlite_message(&e)
        ))
    })?;
    drop(pragmas);
    match conn.query_row("SELECT count(*) FROM sqlite_schema", [], |r| {
        r.get::<_, i64>(0)
    }) {
        Ok(_) => Err(JsError::new(
            "E_STORE_PROBE: a wrong key read sqlite_schema; the database is not encrypted",
        )),
        Err(e) => Ok(format!(
            "E_STORE_KEY: SELECT count(*) FROM sqlite_schema: {e}"
        )),
    }
}

thread_local! {
    static HELD: RefCell<Option<ConnHandle>> = const { RefCell::new(None) };
}

fn probe_binding() -> DillaBinding {
    DillaBinding {
        v: 1,
        instance_id: InstanceId::from_bytes([0x11; 16]),
        community_id: None,
        target_id: [0x11; 16],
        kind: GroupKind::Text,
        policy_version: 1,
        e2ee_version: 1,
        media_version: GroupKind::Text.media_version(),
    }
}

fn probe_credential(signer: &SignatureKeyPair) -> CredentialWithKey {
    let umk = UmkSigner::from_bytes(&[0x21; 32]);
    let ssk = SskSigner::from_bytes(&[0x24; 32]);
    let identity = CredentialIdentity {
        v: 1,
        umk_pub: umk.public(),
        user_id: UserId::from_bytes([0x22; 16]),
        device_id: DeviceId::from_bytes([0x23; 16]),
        kind: Kind::User,
        tier: Tier::Browser,
        signer_tier: SignerTier::Browser,
        ssk_pub: ssk.public(),
        sig_umk_ssk: umk.sign_ssk(&ssk.public()),
        sig_ssk_dev: [0; 64],
    };
    CredentialWithKey {
        credential: BasicCredential::new(identity.encode()).into(),
        signature_key: signer.public().into(),
    }
}

fn probe_err(detail: impl core::fmt::Display) -> JsError {
    JsError::new(&format!("E_STORE_PROBE: {detail}"))
}

/// Test probe for L-WASM-02. `create` and `load` report epoch and authenticator; `hold` and
/// `release` keep a clone across pause to prove the busy guard. Providers and groups drop before
/// returning, leaving the store's own handle as the only live connection when not held.
#[wasm_bindgen]
pub fn store_mls_probe(handle: &StoreHandle, op: &str) -> Result<String, JsError> {
    match op {
        "hold" => HELD.with(|slot| {
            let mut slot = slot.borrow_mut();
            if slot.is_some() {
                return Err(probe_err("a handle is already held"));
            }
            *slot = Some(
                handle
                    .shared()
                    .ok_or_else(|| probe_err("E_STORE_PAUSED: the store is paused"))?,
            );
            Ok(serde_json::json!({ "held": 1 }).to_string())
        }),
        "release" => HELD.with(|slot| {
            slot.borrow_mut().take();
            Ok(serde_json::json!({ "held": 0 }).to_string())
        }),
        "create" | "load" => {
            let conn = handle
                .shared()
                .ok_or_else(|| probe_err("E_STORE_PAUSED: the store is paused"))?;
            let provider = DillaProvider::new(conn);
            provider.storage().migrate().map_err(probe_err)?;
            let group_id = GroupId::from_slice(&[0x11; 16]);
            let group = if op == "create" {
                let signer =
                    SignatureKeyPair::new(CIPHERSUITE.signature_algorithm()).map_err(probe_err)?;
                signer.store(provider.storage()).map_err(probe_err)?;
                {
                    let conn = provider.storage().conn().try_borrow().map_err(probe_err)?;
                    conn.execute_batch("CREATE TABLE IF NOT EXISTS probe_mls (k TEXT PRIMARY KEY, v BLOB NOT NULL)")
                        .map_err(probe_err)?;
                    conn.execute(
                        "INSERT OR REPLACE INTO probe_mls (k, v) VALUES ('signer_pub', ?1)",
                        [signer.public()],
                    )
                    .map_err(probe_err)?;
                }
                let mut group = DillaGroup::create(
                    &provider,
                    &signer,
                    probe_credential(&signer),
                    group_id,
                    probe_binding(),
                    None,
                )
                .map_err(probe_err)?;
                group.self_update(&provider, &signer).map_err(probe_err)?;
                group.merge_pending_commit(&provider).map_err(probe_err)?;
                group
            } else {
                let public = {
                    let conn = provider.storage().conn().try_borrow().map_err(probe_err)?;
                    match conn.query_row(
                        "SELECT v FROM probe_mls WHERE k = 'signer_pub'",
                        [],
                        |r| r.get::<_, Vec<u8>>(0),
                    ) {
                        Ok(value) => value,
                        Err(rusqlite::Error::QueryReturnedNoRows) => {
                            return Err(probe_err("no probe group was created"));
                        }
                        Err(e) if e.to_string().contains("no such table") => {
                            return Err(probe_err("no probe group was created"));
                        }
                        Err(e) => return Err(probe_err(e)),
                    }
                };
                let _signer = SignatureKeyPair::read(
                    provider.storage(),
                    &public,
                    CIPHERSUITE.signature_algorithm(),
                )
                .ok_or_else(|| probe_err("the signer is missing"))?;
                DillaGroup::load(&provider, &group_id)
                    .map_err(probe_err)?
                    .ok_or_else(|| probe_err("the group is missing"))?
            };
            let result = serde_json::json!({
                "epoch": group.epoch(),
                "authenticator": crate::hex(&group.epoch_authenticator().map_err(probe_err)?)
            })
            .to_string();
            drop(group);
            drop(provider);
            Ok(result)
        }
        _ => Err(probe_err(format!("unknown op {op}"))),
    }
}

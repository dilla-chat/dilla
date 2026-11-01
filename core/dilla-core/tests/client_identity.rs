// Native only: every test opens a real SQLite connection through rusqlite.
#![cfg(not(target_arch = "wasm32"))]

//! web-1 task 4: `DillaStorage::unit` with savepoint nesting, the app schema v1, and the identity
//! surface of `dilla_core::client::ClientCore`: browser-rooted signup, the sealed root and state
//! objects, the session signature, the session record and KeyPackages.
//!
//! `TxError`, `StorageError` and `MlsError` are `#[non_exhaustive]` (`tx.rs:13`, `mod.rs:60`,
//! `group.rs:26`); every `match` on them in this file has a wildcard arm (`matches!` has its own).

use aes_gcm::aead::{Aead, Payload};
use aes_gcm::{Aes256Gcm, KeyInit, Nonce};
use dilla_core::ProtocolError;
use dilla_core::cbor::{Encoder, decode_strict};
use dilla_core::client::{
    ClientCore, ClientError, HANDSHAKE_TAIL, migrate_app, recovery_key_check, session_preimage,
};
use dilla_core::identity::{
    CredentialIdentity, DeviceEntry, DeviceList, DeviceListUnsigned, Kind, SignerTier, SskSigner,
    Tier, UmkSigner, k_backup, k_header, recovery_key_base32, recovery_key_from_base32,
};
use dilla_core::ids::{DeviceId, InstanceId, UserId};
use dilla_core::mls::{
    CIPHERSUITE, ConnHandle, DillaBinding, DillaGroup, DillaProvider, DillaStorage, GroupKind,
    MlsError, StorageError, TxError, UnitScope,
};
use dilla_core::public_group::validate_key_package;
use ed25519_dalek::{Signature, VerifyingKey};
use openmls::prelude::*;
use openmls_basic_credential::SignatureKeyPair;
use rusqlite::OptionalExtension;
use std::sync::{Arc, Mutex};

mod client_support;
use client_support::{
    INSTANCE_SIGNER_PUB, Instance, RawPeer, Relay, enrolled_core, ingest_enrolled_pair_bodies,
    ingest_list_body, ready_core_with_key, request,
};

const INSTANCE: [u8; 16] = [0x11; 16];
const USER: [u8; 16] = [0x42; 16];
const GROUP: [u8; 16] = [0x77; 16];
const NOW: u64 = 1_790_000_000;
const IDENTITY_JSON: &str = include_str!("../../../protocol/vectors/identity.json");

#[test]
fn human_instance_signer_key_is_pinned_at_enrollment_and_survives_reopen() {
    let (mut core, connection) = core();
    let seed = [0x71; 32];
    let pinned = ed25519_dalek::SigningKey::from_bytes(&seed)
        .verifying_key()
        .to_bytes();
    let rotated = ed25519_dalek::SigningKey::from_bytes(&[0x72; 32])
        .verifying_key()
        .to_bytes();

    core.signup_begin(&INSTANCE).expect("signup begin");
    assert!(core.instance_signer_key_matches(&pinned).is_err());
    assert_eq!(
        core.signup_complete(&USER, "alice", NOW, &[0; 32])
            .expect_err("invalid Ed25519 key must be rejected")
            .code,
        "E_CORE_INPUT"
    );
    let identity = core.identity().expect("identity");
    let phase = dilla_core::cbor::decode_strict(&identity, |d| {
        d.array(6)?;
        let phase = d.uint()?;
        for _ in 0..5 {
            d.skip()?;
        }
        Ok(phase)
    })
    .expect("identity phase");
    assert_eq!(phase, 1, "invalid key leaves signup pending");
    assert!(core.instance_signer_key_matches(&pinned).is_err());
    core.signup_complete(&USER, "alice", NOW, &pinned)
        .expect("complete authenticated enrollment");
    core.instance_signer_key_matches(&pinned)
        .expect("matching pin");
    assert_eq!(
        core.instance_signer_key_matches(&rotated)
            .expect_err("rotation must fail closed")
            .code,
        "E_CORE_STATE"
    );
    drop(core);

    let reopened = ClientCore::open(connection).expect("reopen");
    reopened
        .instance_signer_key_matches(&pinned)
        .expect("persisted pin");
    assert_eq!(
        reopened
            .instance_signer_key_matches(&rotated)
            .expect_err("rotation must still fail closed")
            .code,
        "E_CORE_STATE"
    );
}

#[test]
fn legacy_ready_identity_without_instance_pin_fails_closed() {
    let (mut core, connection) = core();
    let pinned = ed25519_dalek::SigningKey::from_bytes(&[0x71; 32])
        .verifying_key()
        .to_bytes();
    core.signup_begin(&INSTANCE).expect("signup begin");
    core.signup_complete(&USER, "alice", NOW, &pinned)
        .expect("complete");
    drop(core);
    connection
        .lock()
        .expect("sqlite lock")
        .execute("DELETE FROM app_meta WHERE k='instance_signer_key'", [])
        .expect("simulate an identity created before signer pins");

    let mut reopened = ClientCore::open(connection).expect("open legacy identity");
    assert_eq!(
        reopened
            .instance_signer_key_matches(&pinned)
            .expect_err("legacy key must not be learned from discovery or proof")
            .code,
        "E_CORE_STATE"
    );
    assert_eq!(
        reopened
            .signup_complete(&USER, "alice", NOW, &pinned)
            .expect_err("completed identity cannot be silently repinned")
            .code,
        "E_CORE_STATE"
    );
    assert!(reopened.instance_signer_key_matches(&pinned).is_err());
}

#[test]
fn raw_peer_identity_signs_its_actual_mls_key() {
    let peer = RawPeer::new(0x71, 0x72);
    let basic = BasicCredential::try_from(peer.credential.credential.clone()).expect("basic");
    let identity = CredentialIdentity::decode(basic.identity()).expect("identity");
    let dsk_pub: [u8; 32] = peer
        .credential
        .signature_key
        .as_slice()
        .try_into()
        .expect("dsk");
    identity
        .verify_signatures(&dsk_pub)
        .expect("signed device key");
}

// ---------------------------------------------------------------------------------------------
// fixtures

fn conn() -> ConnHandle {
    Arc::new(Mutex::new(
        rusqlite::Connection::open_in_memory().expect("sqlite"),
    ))
}

/// A core over a fresh in-memory database. The second value is another `Arc` on the same
/// connection; the tests read through it only while no core call is running.
fn core() -> (ClientCore, ConnHandle) {
    let c = conn();
    let core = ClientCore::open(c.clone()).expect("open");
    (core, c)
}

fn err<T>(r: Result<T, ClientError>) -> ClientError {
    match r {
        Ok(_) => panic!("expected an error"),
        Err(e) => e,
    }
}

fn meta(c: &ConnHandle, k: &str) -> Option<Vec<u8>> {
    let guard = c.lock().expect("lock");
    guard
        .query_row("SELECT v FROM app_meta WHERE k = ?1", [k], |r| {
            r.get::<_, Vec<u8>>(0)
        })
        .optional()
        .expect("app_meta read")
}

fn meta_rows(c: &ConnHandle) -> Vec<(String, Vec<u8>)> {
    let guard = c.lock().expect("lock");
    let mut stmt = guard.prepare("SELECT k, v FROM app_meta").expect("prepare");
    stmt.query_map([], |r| Ok((r.get(0)?, r.get(1)?)))
        .expect("query")
        .collect::<Result<_, _>>()
        .expect("rows")
}

/// Every BLOB and TEXT cell of every table, with its table name.
fn cells(c: &ConnHandle) -> Vec<(String, Vec<u8>)> {
    let guard = c.lock().expect("lock");
    let tables: Vec<String> = guard
        .prepare("SELECT name FROM sqlite_master WHERE type = 'table'")
        .expect("prepare")
        .query_map([], |r| r.get(0))
        .expect("query")
        .collect::<Result<_, _>>()
        .expect("names");
    let mut out = Vec::new();
    for table in tables {
        let mut stmt = guard
            .prepare(&format!("SELECT * FROM \"{table}\""))
            .expect("prepare");
        let n = stmt.column_count();
        let mut rows = stmt.query([]).expect("query");
        while let Some(row) = rows.next().expect("row") {
            for i in 0..n {
                match row.get_ref(i).expect("cell") {
                    rusqlite::types::ValueRef::Blob(b) | rusqlite::types::ValueRef::Text(b) => {
                        out.push((table.clone(), b.to_vec()));
                    }
                    _ => {}
                }
            }
        }
    }
    out
}

fn contains(hay: &[u8], needle: &[u8]) -> bool {
    hay.windows(needle.len()).any(|w| w == needle)
}

struct Info {
    phase: u64,
    instance: Option<[u8; 16]>,
    user: Option<[u8; 16]>,
    device: Option<[u8; 16]>,
    username: String,
    published: u64,
}

fn info(core: &ClientCore) -> Info {
    let bytes = core.identity().expect("identity");
    decode_strict(&bytes, |d| {
        d.array(6)?;
        Ok(Info {
            phase: d.uint()?,
            instance: d.opt_bytes_exact::<16>()?,
            user: d.opt_bytes_exact::<16>()?,
            device: d.opt_bytes_exact::<16>()?,
            username: d.text()?.to_owned(),
            published: d.uint()?,
        })
    })
    .expect("[phase, instance|null, user|null, device|null, username, published]")
}

struct Registration {
    invite: String,
    username: String,
    display: String,
    umk_pub: [u8; 32],
    ssk_pub: [u8; 32],
    sig_umk_ssk: [u8; 64],
    password: Option<String>,
    device_id: [u8; 16],
    dsk_pub: [u8; 32],
    tier: u64,
    signer_tier: u64,
    credential: Vec<u8>,
}

fn registration(body: &[u8]) -> Registration {
    decode_strict(body, |d| {
        d.array(8)?;
        let invite = d.text()?.to_owned();
        let username = d.text()?.to_owned();
        let display = d.text()?.to_owned();
        let umk_pub = d.bytes_exact::<32>()?;
        let ssk_pub = d.bytes_exact::<32>()?;
        let sig_umk_ssk = d.bytes_exact::<64>()?;
        let password = if d.try_null()? {
            None
        } else {
            Some(d.text()?.to_owned())
        };
        d.array(5)?;
        let device_id = d.bytes_exact::<16>()?;
        let dsk_pub = d.bytes_exact::<32>()?;
        let tier = d.uint()?;
        let signer_tier = d.uint()?;
        let credential = d.bytes()?.to_vec();
        Ok(Registration {
            invite,
            username,
            display,
            umk_pub,
            ssk_pub,
            sig_umk_ssk,
            password,
            device_id,
            dsk_pub,
            tier,
            signer_tier,
            credential,
        })
    })
    .expect("POST /v1/accounts body")
}

/// `signup_begin` then one `signup_request`: (core, connection, RK bytes, the registration).
fn begun() -> (ClientCore, ConnHandle, [u8; 32], Registration) {
    let (mut core, c) = core();
    let rk_text = core.signup_begin(&INSTANCE).expect("signup_begin");
    let rk = recovery_key_from_base32(&rk_text).expect("52 Crockford characters");
    let reg = registration(
        &core
            .signup_request("INV-1", "alice", "Alice", None)
            .expect("signup_request"),
    );
    (core, c, rk, reg)
}

/// The three elements of a stored object `[v, nonce, ciphertext]`.
fn stored_parts(stored: &[u8]) -> (u64, [u8; 12], Vec<u8>) {
    decode_strict(stored, |d| {
        d.array(3)?;
        Ok((d.uint()?, d.bytes_exact::<12>()?, d.bytes()?.to_vec()))
    })
    .expect("[1, nonce, ciphertext]")
}

fn open_sealed(key: &[u8; 32], aad: &[u8], stored: &[u8]) -> Vec<u8> {
    let (v, nonce, ct) = stored_parts(stored);
    assert_eq!(v, 1);
    Aes256Gcm::new_from_slice(key)
        .expect("key")
        .decrypt(&Nonce::from(nonce), Payload { msg: &ct, aad })
        .expect("the stored object opens")
}

/// (UMK_priv, SSK_priv) from the root object.
fn root_keys(root: &[u8], rk: &[u8; 32]) -> ([u8; 32], [u8; 32]) {
    let plain = open_sealed(&k_header(rk), b"dilla root v1", root);
    decode_strict(&plain, |d| {
        d.array(3)?;
        assert_eq!(d.uint()?, 1);
        Ok((d.bytes_exact::<32>()?, d.bytes_exact::<32>()?))
    })
    .expect("[1, umk_priv, ssk_priv]")
}

/// (instance_id, device_id, dsk_pub, ssk_priv, k_backup) from the `signup` record.
#[allow(clippy::type_complexity)] // The fixture keeps the five wire fields visible at call sites.
fn signup_record(bytes: &[u8]) -> ([u8; 16], [u8; 16], [u8; 32], [u8; 32], [u8; 32]) {
    decode_strict(bytes, |d| {
        d.array(10)?;
        assert_eq!(d.uint()?, 1);
        let instance = d.bytes_exact::<16>()?;
        let device = d.bytes_exact::<16>()?;
        let dsk = d.bytes_exact::<32>()?;
        d.bytes_exact::<32>()?;
        d.bytes_exact::<32>()?;
        d.bytes_exact::<64>()?;
        d.bytes_exact::<64>()?;
        let ssk = d.bytes_exact::<32>()?;
        let kb = d.bytes_exact::<32>()?;
        Ok((instance, device, dsk, ssk, kb))
    })
    .expect("the ten-element signup record")
}

fn sealed(root: Option<&[u8]>, state: Option<&[u8]>, state_uploaded: u64) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(3)
        .opt_bytes(root)
        .opt_bytes(state)
        .uint(state_uploaded);
    e.into_vec()
}

fn session_of(core: &ClientCore) -> Option<(String, u64, u64)> {
    let bytes = core.session().expect("session");
    decode_strict(&bytes, |d| {
        if d.try_null()? {
            return Ok(None);
        }
        d.array(3)?;
        Ok(Some((d.text()?.to_owned(), d.uint()?, d.uint()?)))
    })
    .expect("null | [token, expires, idle_expires]")
}

fn scratch(c: &ConnHandle) {
    c.lock()
        .expect("lock")
        .execute_batch("CREATE TABLE scratch (n INTEGER NOT NULL)")
        .expect("scratch table");
}

fn scratch_rows(c: &ConnHandle) -> i64 {
    c.lock()
        .expect("lock")
        .query_row("SELECT count(*) FROM scratch", [], |r| r.get(0))
        .expect("count")
}

fn insert_scratch(u: &UnitScope<'_>, n: i64) -> Result<(), StorageError> {
    u.with_conn(|conn| {
        conn.execute("INSERT INTO scratch (n) VALUES (?1)", [n])?;
        Ok(())
    })
}

fn mls_fixture(p: &DillaProvider) -> (SignatureKeyPair, CredentialWithKey) {
    let signer = SignatureKeyPair::new(CIPHERSUITE.signature_algorithm()).expect("keygen");
    signer.store(p.storage()).expect("store signer");
    let umk = UmkSigner::from_bytes(&[0x0a; 32]);
    let ssk = SskSigner::from_bytes(&[0x4a; 32]);
    let identity = CredentialIdentity {
        v: 1,
        umk_pub: umk.public(),
        user_id: UserId::from_bytes([0x0a; 16]),
        device_id: DeviceId::from_bytes([0x01; 16]),
        kind: Kind::User,
        tier: Tier::Native,
        signer_tier: SignerTier::Native,
        ssk_pub: ssk.public(),
        sig_umk_ssk: umk.sign_ssk(&ssk.public()),
        sig_ssk_dev: [0u8; 64],
    };
    let credential = CredentialWithKey {
        credential: BasicCredential::new(identity.encode()).into(),
        signature_key: signer.public().into(),
    };
    (signer, credential)
}

fn text_binding() -> DillaBinding {
    DillaBinding {
        v: 1,
        instance_id: InstanceId::from_bytes(INSTANCE),
        community_id: None,
        target_id: [0x33; 16],
        kind: GroupKind::Text,
        policy_version: 1,
        e2ee_version: 1,
        media_version: GroupKind::Text.media_version(),
    }
}

fn validated(bytes: &[u8]) -> KeyPackage {
    use tls_codec::Deserialize as _;
    let kp_in = match MlsMessageIn::tls_deserialize_exact(bytes)
        .expect("an MLSMessage")
        .extract()
    {
        MlsMessageBodyIn::KeyPackage(kp) => kp,
        other => panic!("expected a KeyPackage message, got {other:?}"),
    };
    validate_key_package(&openmls_rust_crypto::RustCrypto::default(), kp_in)
        .expect("the instance's validation accepts it")
}

// ---------------------------------------------------------------------------------------------
// DillaStorage::unit (L-CORE-03)

#[test]
fn a_unit_rollback_undoes_the_mls_write_made_inside_it() {
    let c = conn();
    scratch(&c);
    let p = DillaProvider::new(c.clone());
    p.storage().migrate().expect("migrate");
    let (signer, credential) = mls_fixture(&p);
    let out: Result<(), TxError<StorageError>> = p.storage().unit(|u: &UnitScope<'_>| {
        let group = DillaGroup::create(
            &p,
            &signer,
            credential.clone(),
            GroupId::from_slice(&GROUP),
            text_binding(),
            None,
        )
        .map_err(|e| StorageError::Codec(format!("create inside the unit: {e}")))?;
        assert_eq!(group.epoch(), 0);
        insert_scratch(u, 1)?;
        Err(StorageError::Codec("deliberate".into()))
    });
    assert!(
        matches!(out, Err(TxError::RolledBack(StorageError::Codec(ref m))) if m == "deliberate"),
        "{out:?}"
    );
    assert!(
        DillaGroup::load(&p, &GroupId::from_slice(&GROUP))
            .expect("load")
            .is_none(),
        "the group written inside the unit must be gone"
    );
    assert_eq!(scratch_rows(&c), 0);
}

#[test]
fn a_committed_unit_keeps_the_mls_write_and_the_app_row() {
    let c = conn();
    scratch(&c);
    let p = DillaProvider::new(c.clone());
    p.storage().migrate().expect("migrate");
    let (signer, credential) = mls_fixture(&p);
    let out: Result<u8, TxError<StorageError>> = p.storage().unit(|u| {
        DillaGroup::create(
            &p,
            &signer,
            credential.clone(),
            GroupId::from_slice(&GROUP),
            text_binding(),
            None,
        )
        .map_err(|e| StorageError::Codec(format!("create inside the unit: {e}")))?;
        insert_scratch(u, 1)?;
        Ok(7)
    });
    assert_eq!(out.expect("the unit commits"), 7);
    let group = DillaGroup::load(&p, &GroupId::from_slice(&GROUP))
        .expect("load")
        .expect("the group survives the commit");
    assert_eq!(group.epoch(), 0);
    assert_eq!(scratch_rows(&c), 1);
}

#[test]
fn a_failed_nested_transaction_rolls_back_to_its_savepoint_and_the_unit_goes_on() {
    let c = conn();
    scratch(&c);
    let s = DillaStorage::new(c.clone());
    s.migrate().expect("migrate");
    let doomed = SignatureKeyPair::new(CIPHERSUITE.signature_algorithm()).expect("keygen");
    let out: Result<(), TxError<StorageError>> = s.unit(|u| {
        insert_scratch(u, 1)?;
        let inner: Result<(), TxError<StorageError>> = s.transaction(|| {
            doomed.store(&s)?;
            Err(StorageError::Codec("inner".into()))
        });
        assert!(
            matches!(inner, Err(TxError::RolledBack(StorageError::Codec(ref m))) if m == "inner"),
            "{inner:?}"
        );
        insert_scratch(u, 2)?;
        Ok(())
    });
    out.expect("the unit commits after the inner rollback");
    assert_eq!(scratch_rows(&c), 2);
    assert!(
        SignatureKeyPair::read(&s, doomed.public(), CIPHERSUITE.signature_algorithm()).is_none(),
        "the write of the failed savepoint must be gone"
    );
}

#[test]
fn units_and_transactions_nest_exactly_one_level() {
    let s = DillaStorage::new(conn());
    s.migrate().expect("migrate");
    let out: Result<(), TxError<StorageError>> = s.unit(|u| {
        let second: Result<(), TxError<StorageError>> = s.unit(|_| Ok(()));
        assert!(matches!(second, Err(TxError::AlreadyOpen)), "{second:?}");
        let nested: Result<(), TxError<StorageError>> = s.transaction(|| {
            let third: Result<(), TxError<StorageError>> = s.transaction(|| Ok(()));
            assert!(matches!(third, Err(TxError::AlreadyOpen)), "{third:?}");
            Ok(())
        });
        nested.expect("one transaction nests in a unit");
        let inside: Result<Result<(), StorageError>, TxError<StorageError>> =
            s.transaction(|| Ok(u.with_conn(|_| Ok(()))));
        assert!(
            matches!(
                inside,
                Ok(Err(StorageError::Sqlite(ref m)))
                    if m == "the unit connection is not usable inside a nested transaction"
            ),
            "{inside:?}"
        );
        Ok(())
    });
    out.expect("unit");
    let plain: Result<u8, TxError<StorageError>> = s.transaction(|| Ok(5));
    assert_eq!(plain.expect("a plain transaction after the unit"), 5);
    let again: Result<u8, TxError<StorageError>> = s.unit(|_| Ok(6));
    assert_eq!(again.expect("a second unit after the first closed"), 6);
}

#[test]
fn another_thread_cannot_enter_an_open_unit() {
    let s = DillaStorage::new(conn());
    s.migrate().expect("migrate");
    let out: Result<(), TxError<StorageError>> = s.unit(|_| {
        std::thread::scope(|scope| {
            let t = scope.spawn(|| {
                let r: Result<(), TxError<StorageError>> = s.transaction(|| Ok(()));
                r
            });
            assert!(matches!(t.join().expect("join"), Err(TxError::AlreadyOpen)));
            let t = scope.spawn(|| {
                let r: Result<(), TxError<StorageError>> = s.unit(|_| Ok(()));
                r
            });
            assert!(matches!(t.join().expect("join"), Err(TxError::AlreadyOpen)));
        });
        Ok(())
    });
    out.expect("unit");
}

#[test]
fn a_panic_inside_a_unit_rolls_back_and_the_storage_takes_units_again() {
    let c = conn();
    scratch(&c);
    let s = DillaStorage::new(c.clone());
    s.migrate().expect("migrate");
    let unwound = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        let _: Result<(), TxError<StorageError>> = s.unit(|u| {
            insert_scratch(u, 1)?;
            let _: Result<(), TxError<StorageError>> =
                s.transaction(|| panic!("exploded inside the savepoint"));
            Ok(())
        });
    }));
    assert!(unwound.is_err(), "the panic must reach the caller");
    assert_eq!(scratch_rows(&c), 0);
    let again: Result<u8, TxError<StorageError>> = s.unit(|u| {
        insert_scratch(u, 2)?;
        Ok(9)
    });
    assert_eq!(again.expect("usable after the unwind"), 9);
    assert_eq!(scratch_rows(&c), 1);
    let plain: Result<u8, TxError<StorageError>> = s.transaction(|| Ok(1));
    assert_eq!(plain.expect("plain transaction after the unwind"), 1);
}

// ---------------------------------------------------------------------------------------------
// ClientError (L-CORE-02)

#[test]
fn client_errors_render_and_convert_as_the_code_table_says() {
    let bare = ClientError {
        code: "E_CORE_INPUT",
        detail: String::new(),
    };
    assert_eq!(bare.to_string(), "E_CORE_INPUT");
    let with = ClientError {
        code: "E_CORE_STATE",
        detail: "a signup is pending".into(),
    };
    assert_eq!(with.to_string(), "E_CORE_STATE: a signup is pending");
    assert_eq!(
        ClientError::from(ProtocolError::Credential),
        ClientError {
            code: "E_CREDENTIAL",
            detail: String::new()
        }
    );
    assert_eq!(
        ClientError::from(MlsError::Protocol(ProtocolError::EnvelopeLimit)),
        ClientError {
            code: "E_ENVELOPE_LIMIT",
            detail: String::new()
        }
    );
    assert_eq!(
        ClientError::from(MlsError::OpenMls("boom".into())),
        ClientError {
            code: "E_CORE_MLS",
            detail: "boom".into()
        }
    );
    assert_eq!(
        ClientError::from(MlsError::NeedsReload),
        ClientError {
            code: "E_CORE_RELOAD",
            detail: String::new()
        }
    );
    assert_eq!(
        ClientError::from(MlsError::NotFound),
        ClientError {
            code: "E_CORE_NOT_FOUND",
            detail: String::new()
        }
    );
    assert_eq!(
        ClientError::from(MlsError::Tx("x".into())),
        ClientError {
            code: "E_CORE_STORAGE",
            detail: "x".into()
        }
    );
    assert_eq!(
        ClientError::from(MlsError::Storage(StorageError::Poisoned)),
        ClientError {
            code: "E_CORE_STORAGE",
            detail: "lock poisoned".into()
        }
    );
    assert_eq!(
        ClientError::from(StorageError::Sqlite("disk".into())),
        ClientError {
            code: "E_CORE_STORAGE",
            detail: "sqlite: disk".into()
        }
    );
    assert_eq!(
        ClientError::from(TxError::<ClientError>::AlreadyOpen),
        ClientError {
            code: "E_CORE_STORAGE",
            detail: "a transaction is already open on this storage".into()
        }
    );
    assert_eq!(ClientError::from(TxError::RolledBack(with.clone())), with);
}

// ---------------------------------------------------------------------------------------------
// app schema v13 (v12's per-device Bot interaction groups; v13's account-kind attestations) and
// ClientCore::open
// (L-CORE-05)

#[test]
fn open_creates_app_schema_v13_and_reopens_idempotently() {
    let c = conn();
    drop(ClientCore::open(c.clone()).expect("first open"));
    drop(ClientCore::open(c.clone()).expect("second open over the same database"));
    assert_eq!(
        migrate_app(&DillaStorage::new(c.clone())).expect("migrate_app is idempotent on its own"),
        13
    );
    let guard = c.lock().expect("lock");
    let names: Vec<String> = guard
        .prepare(
            "SELECT name FROM sqlite_master WHERE type IN ('table', 'index') \
             AND name LIKE 'app%' ORDER BY name",
        )
        .expect("prepare")
        .query_map([], |r| r.get(0))
        .expect("query")
        .collect::<Result<_, _>>()
        .expect("names");
    assert_eq!(
        names,
        [
            "app_account_kinds",
            "app_authority",
            "app_authority_known",
            "app_authority_mentionable",
            "app_bot_event_state",
            "app_bot_events",
            "app_bot_events_page",
            "app_bot_intents",
            "app_bot_intents_by_group",
            "app_bot_intents_one_pending_per_group",
            "app_bot_keypackage_intents",
            "app_bot_keypackage_one_pending",
            "app_device_lists",
            "app_groups",
            "app_groups_by_interaction_device",
            "app_groups_by_target",
            "app_handshake_tail",
            "app_human_interaction_event_state",
            "app_human_interaction_events",
            "app_human_interaction_events_page",
            "app_human_interaction_receipts",
            "app_human_interaction_receipts_request",
            "app_human_interaction_response_claims",
            "app_manual_removal_evidence",
            "app_manual_removal_evidence_pending",
            "app_manual_warning_evidence",
            "app_manual_warning_evidence_pending",
            "app_messages",
            "app_messages_by_msg",
            "app_messages_by_pin",
            "app_messages_by_reaction",
            "app_messages_by_reply",
            "app_messages_by_thread",
            "app_messages_legacy_sender",
            "app_messages_live_by_sig",
            "app_meta",
            "app_outbox",
            "app_outbox_by_group",
            "app_pending_history",
            "app_pending_history_by_leaf_seq",
            "app_pins",
            "app_proposals",
            "app_purges",
            "app_quarantine",
            "app_quarantine_device",
            "app_reactions",
            "app_read_state",
            "app_removal_actor_lists",
            "app_removal_cursor",
            "app_removal_entries",
            "app_removal_tombstones",
            "app_roles",
            "app_search",
            "app_search_config",
            "app_search_data",
            "app_search_docsize",
            "app_search_idx",
            "app_search_rows",
            "app_search_rows_recent",
            "app_settings",
            "app_trust_audit",
            "app_trust_pins",
        ]
    );
    let schema: Vec<u8> = guard
        .query_row("SELECT v FROM app_meta WHERE k = 'schema'", [], |r| {
            r.get(0)
        })
        .expect("schema row");
    assert_eq!(schema, [0x0d]);
    let resync: i64 = guard
        .query_row(
            "SELECT count(*) FROM pragma_table_info('app_groups') WHERE name = 'resync'",
            [],
            |r| r.get(0),
        )
        .expect("pragma_table_info app_groups");
    assert_eq!(resync, 1, "app_groups.resync exists (task 5 reads it)");
    let epoch: i64 = guard
        .query_row(
            "SELECT count(*) FROM pragma_table_info('app_outbox') WHERE name = 'epoch'",
            [],
            |r| r.get(0),
        )
        .expect("pragma_table_info app_outbox");
    assert_eq!(epoch, 1, "app_outbox.epoch exists (task 5 reads it)");
    for (table, column) in [
        ("app_groups", "epoch"),
        ("app_groups", "pending_commit"),
        ("app_messages", "mention"),
        ("app_messages", "reply_to"),
        ("app_messages", "edit_body"),
        ("app_messages", "edit_seq"),
    ] {
        let n: i64 = guard
            .query_row(
                "SELECT count(*) FROM pragma_table_info(?1) WHERE name = ?2",
                [table, column],
                |r| r.get(0),
            )
            .expect("pragma_table_info");
        assert_eq!(n, 1, "{table}.{column} exists at schema 3");
    }
    let secure: i64 = guard
        .query_row("PRAGMA secure_delete", [], |r| r.get(0))
        .expect("pragma");
    assert_eq!(secure, 1, "open turns secure_delete on");
    assert_eq!(HANDSHAKE_TAIL, 64);
}

#[test]
fn app_schema_checks_lengths_and_keeps_one_live_group_per_target() {
    let (_core, c) = core();
    let g = c.lock().expect("lock");
    assert!(
        g.execute(
            "INSERT INTO app_groups (group_id, kind, target_id, state) VALUES (zeroblob(15), 0, zeroblob(16), 2)",
            [],
        )
        .is_err(),
        "a 15-byte group id is refused"
    );
    g.execute(
        "INSERT INTO app_groups (group_id, kind, target_id, state) VALUES (x'01010101010101010101010101010101', 0, zeroblob(16), 2)",
        [],
    )
    .expect("a 16-byte group id");
    assert!(
        g.execute(
            "INSERT INTO app_groups (group_id, kind, target_id, state) VALUES (x'02020202020202020202020202020202', 0, zeroblob(16), 2)",
            [],
        )
        .is_err(),
        "two live groups for one (target, kind) are refused"
    );
    g.execute(
        "INSERT INTO app_groups (group_id, kind, target_id, state) VALUES (x'03030303030303030303030303030303', 0, zeroblob(16), 4)",
        [],
    )
    .expect("a gone group does not count against the target");
    let defaults: (i64, i64, i64, i64) = g
        .query_row(
            "SELECT next_seq, acked_seq, acked_epoch, resync FROM app_groups WHERE group_id = x'01010101010101010101010101010101'",
            [],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
        )
        .expect("row");
    assert_eq!(defaults, (1, 0, 0, 0));
    assert!(
        g.execute(
            "UPDATE app_groups SET resync = 2 WHERE group_id = x'01010101010101010101010101010101'",
            [],
        )
        .is_err(),
        "resync is 0 or 1"
    );
    assert!(
        g.execute(
            "INSERT INTO app_groups (group_id, kind, community_id, target_id, state) VALUES (x'04040404040404040404040404040404', 0, zeroblob(15), x'05050505050505050505050505050505', 2)",
            [],
        )
        .is_err(),
        "a 15-byte community id is refused"
    );
    assert!(
        g.execute(
            "INSERT INTO app_outbox (msg_id, group_id, envelope, created, state) VALUES (zeroblob(17), zeroblob(16), x'00', 1, 0)",
            [],
        )
        .is_err(),
        "a 17-byte msg id is refused"
    );
    g.execute(
        "INSERT INTO app_outbox (msg_id, group_id, envelope, created, state) VALUES (zeroblob(16), zeroblob(16), x'00', 1, 0)",
        [],
    )
    .expect("a 16-byte msg id");
    let outbox: (String, i64) = g
        .query_row(
            "SELECT error, epoch FROM app_outbox WHERE msg_id = zeroblob(16)",
            [],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .expect("outbox row");
    assert_eq!(outbox, (String::new(), 0));
}

#[test]
fn open_refuses_an_app_schema_newer_than_this_build() {
    let c = conn();
    c.lock()
        .expect("lock")
        .execute_batch(
            "CREATE TABLE app_meta (k TEXT PRIMARY KEY, v BLOB NOT NULL) WITHOUT ROWID;
             INSERT INTO app_meta (k, v) VALUES ('schema', x'0e');",
        )
        .expect("seed");
    let e = match ClientCore::open(Arc::clone(&c)) {
        Ok(_) => panic!("a newer schema must be refused"),
        Err(e) => e,
    };
    // C4x: its own code, with both numbers, and nothing created in the store.
    assert_eq!(
        e,
        ClientError {
            code: "E_STORE_NEWER",
            detail: "found 14, supported 13".into()
        }
    );
    let app_tables: i64 = c
        .lock()
        .expect("lock")
        .query_row(
            "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name LIKE 'app%'",
            [],
            |r| r.get(0),
        )
        .expect("tables");
    assert_eq!(app_tables, 1, "only the seeded app_meta");
}

// ---------------------------------------------------------------------------------------------
// identity and signup (L-CORE-06)

#[test]
fn a_fresh_store_is_phase_0_and_refuses_every_identity_bound_call() {
    let (mut core, _c) = core();
    let i = info(&core);
    assert_eq!(
        (
            i.phase,
            i.instance,
            i.user,
            i.device,
            i.username.as_str(),
            i.published
        ),
        (0, None, None, None, "", 0)
    );
    assert_eq!(
        err(core.signup_request("i", "u", "d", None)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "no signup is pending".into()
        }
    );
    assert_eq!(
        err(core.signup_complete(&USER, "alice", NOW, &INSTANCE_SIGNER_PUB)).code,
        "E_CORE_STATE"
    );
    assert_eq!(err(core.signup_reset()).code, "E_CORE_STATE");
    assert_eq!(
        err(core.device_list_body()),
        ClientError {
            code: "E_CORE_NO_IDENTITY",
            detail: String::new()
        }
    );
    assert_eq!(err(core.device_list_published()).code, "E_CORE_NO_IDENTITY");
    assert_eq!(err(core.key_packages(1, false)).code, "E_CORE_NO_IDENTITY");
    assert_eq!(
        err(core.session_sign(&[0xab; 32], 0)).code,
        "E_CORE_NO_IDENTITY"
    );
    assert_eq!(
        err(core.session_store("t", 1, 1)).code,
        "E_CORE_NO_IDENTITY"
    );
    assert_eq!(core.session().expect("session"), [0xf6]);
    core.session_clear().expect("a no-op without a record");
    assert_eq!(
        core.sealed_objects().expect("sealed_objects"),
        [0x83, 0xf6, 0xf6, 0x00]
    );
    core.unload();
    assert_eq!(info(&core).phase, 0);
}

#[test]
fn signup_begin_seals_the_root_object_under_the_returned_recovery_key() {
    let (mut core, c) = core();
    let rk_text = core.signup_begin(&INSTANCE).expect("signup_begin");
    assert_eq!(rk_text.len(), 52);
    assert!(
        rk_text
            .bytes()
            .all(|b| b"0123456789ABCDEFGHJKMNPQRSTVWXYZ".contains(&b)),
        "{rk_text}"
    );
    let rk = recovery_key_from_base32(&rk_text).expect("decodes");
    let i = info(&core);
    assert_eq!(
        (
            i.phase,
            i.instance,
            i.user,
            i.username.as_str(),
            i.published
        ),
        (1, Some(INSTANCE), None, "", 0)
    );
    let root = meta(&c, "root_sealed").expect("root_sealed written");
    assert_eq!(root.len(), 103);
    let (umk, ssk) = root_keys(&root, &rk);
    let reg = registration(
        &core
            .signup_request("CODE", "alice", "Alice", None)
            .expect("request"),
    );
    assert_eq!(UmkSigner::from_bytes(&umk).public(), reg.umk_pub);
    assert_eq!(SskSigner::from_bytes(&ssk).public(), reg.ssk_pub);
    assert_eq!(i.device, Some(reg.device_id));
    let (_, nonce, ct) = stored_parts(&root);
    assert!(
        Aes256Gcm::new_from_slice(&k_header(&rk))
            .expect("key")
            .decrypt(
                &Nonce::from(nonce),
                Payload {
                    msg: &ct,
                    aad: b"dilla state v1"
                }
            )
            .is_err(),
        "the root object is bound to its own aad"
    );
    assert!(
        Aes256Gcm::new_from_slice(&k_backup(&rk))
            .expect("key")
            .decrypt(
                &Nonce::from(nonce),
                Payload {
                    msg: &ct,
                    aad: b"dilla root v1"
                }
            )
            .is_err(),
        "the root object is sealed under K_header, not K_backup"
    );
    let (instance, device, dsk, ssk_rec, kb_rec) =
        signup_record(&meta(&c, "signup").expect("signup record"));
    assert_eq!(
        (instance, device, dsk),
        (INSTANCE, reg.device_id, reg.dsk_pub)
    );
    assert_eq!(ssk_rec, ssk);
    assert_eq!(kb_rec, k_backup(&rk));
    assert!(meta(&c, "identity").is_none());
    assert!(meta(&c, "state_sealed").is_none());
    assert_eq!(
        core.sealed_objects().expect("sealed"),
        sealed(Some(&root), None, 0)
    );
    assert_eq!(
        err(core.signup_begin(&INSTANCE)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "a signup is pending".into()
        }
    );
}

#[test]
fn signup_request_is_the_registration_body_with_a_zero_user_id_credential() {
    let (mut core, c) = core();
    core.signup_begin(&INSTANCE).expect("begin");
    let a = registration(
        &core
            .signup_request("ABCD-EFGH", "alice", "Alice A.", Some("correct horse"))
            .expect("request"),
    );
    assert_eq!(
        (
            a.invite.as_str(),
            a.username.as_str(),
            a.display.as_str(),
            a.password.as_deref()
        ),
        ("ABCD-EFGH", "alice", "Alice A.", Some("correct horse"))
    );
    assert_eq!((a.tier, a.signer_tier), (1, 1));
    let (_, device, dsk, _, _) = signup_record(&meta(&c, "signup").expect("signup"));
    assert_eq!((a.device_id, a.dsk_pub), (device, dsk));
    let cred = CredentialIdentity::decode(&a.credential).expect("the ten-element credential");
    assert_eq!(cred.v, 1);
    assert_eq!(cred.user_id, UserId::from_bytes([0; 16]));
    assert_eq!(cred.device_id, DeviceId::from_bytes(a.device_id));
    assert_eq!(
        (cred.kind, cred.tier, cred.signer_tier),
        (Kind::User, Tier::Browser, SignerTier::Browser)
    );
    assert_eq!(
        (cred.umk_pub, cred.ssk_pub, cred.sig_umk_ssk),
        (a.umk_pub, a.ssk_pub, a.sig_umk_ssk)
    );
    cred.verify_signatures(&a.dsk_pub)
        .expect("both credential signatures verify against the DSK");
    let b = registration(
        &core
            .signup_request("", "bob", "Bob", None)
            .expect("request"),
    );
    assert_eq!(
        (b.invite.as_str(), b.username.as_str(), b.password),
        ("", "bob", None)
    );
    assert_eq!(
        (b.umk_pub, b.ssk_pub, b.device_id, b.dsk_pub, b.credential),
        (a.umk_pub, a.ssk_pub, a.device_id, a.dsk_pub, a.credential),
        "signup_request is pure: the keys do not change between calls"
    );
}

#[test]
fn signup_complete_signs_device_list_v1_and_seals_the_state_object() {
    let (mut core, c, rk, reg) = begun();
    assert_eq!(
        err(core.signup_complete(&[0u8; 16], "alice", NOW, &INSTANCE_SIGNER_PUB)),
        ClientError {
            code: "E_CORE_INPUT",
            detail: "user_id is all zero".into()
        }
    );
    let body = core
        .signup_complete(&USER, "alice", NOW, &INSTANCE_SIGNER_PUB)
        .expect("signup_complete");
    let (version, blob, sig, prev) = decode_strict(&body, |d| {
        d.array(4)?;
        Ok((
            d.uint()?,
            d.bytes()?.to_vec(),
            d.bytes_exact::<64>()?,
            d.bytes_exact::<32>()?,
        ))
    })
    .expect("[version, blob, ssk_signature, prev_hash]");
    assert_eq!((version, prev), (1, [0u8; 32]));
    let list = DeviceList::decode(&blob).expect("the six-element signed list");
    assert_eq!(list.sig_ssk, sig);
    assert_eq!(list.unsigned.user_id, UserId::from_bytes(USER));
    assert_eq!(list.unsigned.version, 1);
    assert_eq!(list.unsigned.entries.len(), 1);
    let e = &list.unsigned.entries[0];
    assert_eq!(
        (e.device_id, e.dsk_pub, e.tier, e.added_at, e.revoked_at),
        (
            DeviceId::from_bytes(reg.device_id),
            reg.dsk_pub,
            Tier::Browser,
            NOW,
            None
        )
    );
    list.accept(None, &reg.ssk_pub)
        .expect("list v1 verifies under the SSK");
    let i = info(&core);
    assert_eq!(
        (
            i.phase,
            i.instance,
            i.user,
            i.device,
            i.username.as_str(),
            i.published
        ),
        (
            2,
            Some(INSTANCE),
            Some(USER),
            Some(reg.device_id),
            "alice",
            0
        )
    );
    assert!(
        meta(&c, "signup").is_none(),
        "the pending signup record is gone"
    );
    let state = meta(&c, "state_sealed").expect("state object");
    let plain = open_sealed(&k_backup(&rk), b"dilla state v1", &state);
    let (v, inner, pins) = decode_strict(&plain, |d| {
        d.array(3)?;
        Ok((d.uint()?, d.bytes()?.to_vec(), d.array_len()?))
    })
    .expect("[1, device_list, pins]");
    assert_eq!((v, inner, pins), (1, blob.clone(), 0));
    let record = meta(&c, "identity").expect("identity record");
    let (v, cred_bytes, list, uploaded) = decode_strict(&record, |d| {
        d.array(13)?;
        let v = d.uint()?;
        for _ in 0..7 {
            d.skip()?;
        }
        let cred = d.bytes()?.to_vec();
        d.skip()?;
        d.skip()?;
        Ok((v, cred, d.bytes()?.to_vec(), d.uint()?))
    })
    .expect("the thirteen-element identity record");
    assert_eq!((v, uploaded), (2, 0));
    assert_eq!(list, blob, "device_list is list v1 from signup on");
    let cred = CredentialIdentity::decode(&cred_bytes).expect("credential");
    let placeholder = CredentialIdentity::decode(&reg.credential).expect("placeholder");
    assert_eq!(cred.user_id, UserId::from_bytes(USER));
    assert_eq!(
        CredentialIdentity {
            user_id: placeholder.user_id,
            ..cred.clone()
        },
        placeholder,
        "only user_id changed"
    );
    cred.verify_signatures(&reg.dsk_pub)
        .expect("still verifies");
    assert_eq!(core.device_list_body().expect("phase 2"), body);
    let root = meta(&c, "root_sealed").expect("root kept");
    assert_eq!(
        core.sealed_objects().expect("sealed"),
        sealed(Some(&root), Some(&state), 0)
    );
    core.device_list_published().expect("mark published");
    core.device_list_published().expect("idempotent");
    assert_eq!(info(&core).published, 1);
    assert_eq!(
        err(core.signup_complete(&USER, "alice", NOW, &INSTANCE_SIGNER_PUB)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "the identity is complete".into()
        }
    );
    assert_eq!(
        err(core.signup_request("i", "u", "d", None)).code,
        "E_CORE_STATE"
    );
    assert_eq!(err(core.signup_reset()).code, "E_CORE_STATE");
    assert_eq!(
        err(core.signup_begin(&INSTANCE)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "an identity exists".into()
        }
    );
}

#[test]
fn no_private_key_rests_outside_the_signup_record() {
    let (mut core, c, rk, _reg) = begun();
    let (umk, ssk) = root_keys(&meta(&c, "root_sealed").expect("root"), &rk);
    let kh = k_header(&rk);
    let kb = k_backup(&rk);
    for (table, cell) in cells(&c) {
        assert!(!contains(&cell, &umk), "UMK_priv found in {table}");
        assert!(!contains(&cell, &rk), "the recovery key found in {table}");
        assert!(!contains(&cell, &kh), "K_header found in {table}");
    }
    for (k, v) in meta_rows(&c) {
        if k != "signup" {
            assert!(!contains(&v, &ssk), "SSK_priv found in app_meta {k}");
            assert!(!contains(&v, &kb), "K_backup found in app_meta {k}");
        }
    }
    let ssk_cells = cells(&c)
        .iter()
        .filter(|(_, cell)| contains(cell, &ssk))
        .count();
    assert_eq!(
        ssk_cells, 1,
        "SSK_priv rests in exactly one cell: the signup record"
    );
    core.signup_complete(&USER, "alice", NOW, &INSTANCE_SIGNER_PUB)
        .expect("complete");
    for (table, cell) in cells(&c) {
        for (name, secret) in [
            ("UMK_priv", &umk[..]),
            ("SSK_priv", &ssk[..]),
            ("the recovery key", &rk[..]),
            ("K_header", &kh[..]),
            ("K_backup", &kb[..]),
        ] {
            assert!(
                !contains(&cell, secret),
                "{name} found in {table} after signup_complete"
            );
        }
    }
}

#[test]
fn the_deleted_signup_record_leaves_no_ssk_in_the_database_file() {
    let dir = std::env::temp_dir().join(format!("dilla-client-identity-{}", std::process::id()));
    std::fs::create_dir_all(&dir).expect("tmp dir");
    let path = dir.join("secure-delete.sqlite");
    let _ = std::fs::remove_file(&path);
    let c: ConnHandle = Arc::new(Mutex::new(
        rusqlite::Connection::open(&path).expect("open file"),
    ));
    let mut core = ClientCore::open(c.clone()).expect("open");
    let rk = recovery_key_from_base32(&core.signup_begin(&INSTANCE).expect("begin")).expect("rk");
    let (_, ssk) = root_keys(&meta(&c, "root_sealed").expect("root"), &rk);
    let kb = k_backup(&rk);
    let before = std::fs::read(&path).expect("read db");
    assert!(
        contains(&before, &ssk),
        "the scan sees the pending record in the file"
    );
    assert!(contains(&before, &kb), "the scan sees K_backup in the file");
    core.signup_complete(&USER, "alice", NOW, &INSTANCE_SIGNER_PUB)
        .expect("complete");
    drop(core);
    drop(c);
    let after = std::fs::read(&path).expect("read db");
    assert!(
        !contains(&after, &ssk),
        "SSK_priv left in the database file"
    );
    assert!(!contains(&after, &kb), "K_backup left in the database file");
    std::fs::remove_file(&path).expect("cleanup");
}

#[test]
fn signup_reset_forgets_the_pending_identity_and_its_device_key() {
    let (mut core, c, _rk, reg) = begun();
    core.signup_reset().expect("reset");
    let i = info(&core);
    assert_eq!((i.phase, i.instance, i.device), (0, None, None));
    for k in ["signup", "root_sealed"] {
        assert!(meta(&c, k).is_none(), "{k} survives the reset");
    }
    let keys: i64 = c
        .lock()
        .expect("lock")
        .query_row("SELECT count(*) FROM openmls_signature_keys", [], |r| {
            r.get(0)
        })
        .expect("count");
    assert_eq!(keys, 0, "the DSK is deleted");
    assert_eq!(
        err(core.session_sign(&[1; 32], 0)).code,
        "E_CORE_NO_IDENTITY"
    );
    core.signup_begin(&INSTANCE)
        .expect("a new signup may start");
    let again = registration(&core.signup_request("i", "u", "d", None).expect("request"));
    assert_ne!(again.device_id, reg.device_id);
    assert_ne!(again.dsk_pub, reg.dsk_pub);
}

#[test]
fn signup_reset_refuses_while_a_session_is_stored() {
    let (mut core, c) = core();
    core.signup_begin(&INSTANCE).expect("signup_begin");
    core.session_store("tok", 10, 10)
        .expect("session_store in phase 1");
    assert_eq!(
        err(core.signup_reset()),
        ClientError {
            code: "E_CORE_STATE",
            detail: "the account is registered".into()
        }
    );
    assert_eq!(
        info(&core).phase,
        1,
        "the pending signup survives the refused reset"
    );
    let pending: i64 = c
        .lock()
        .expect("lock")
        .query_row(
            "SELECT count(*) FROM app_meta WHERE k IN ('signup','root_sealed')",
            [],
            |r| r.get(0),
        )
        .expect("count");
    assert_eq!(pending, 2, "signup and root_sealed are still stored");
    assert_eq!(
        session_of(&core),
        Some(("tok".to_owned(), 10, 10)),
        "the session record is untouched"
    );
    core.session_sign(&[7u8; 32], 0)
        .expect("the DSK is still stored and loaded");
    let keys: i64 = c
        .lock()
        .expect("lock")
        .query_row("SELECT count(*) FROM openmls_signature_keys", [], |r| {
            r.get(0)
        })
        .expect("count");
    assert_eq!(keys, 1, "the DSK row is still stored");
    core.session_clear().expect("session_clear");
    core.signup_reset()
        .expect("reset once no session is stored");
    assert_eq!(info(&core).phase, 0);
}

#[test]
fn reopening_keeps_the_phase_and_reloads_the_device_key() {
    let c = conn();
    let mut core = ClientCore::open(c.clone()).expect("open");
    core.signup_begin(&INSTANCE).expect("begin");
    let reg = registration(
        &core
            .signup_request("i", "alice", "A", None)
            .expect("request"),
    );
    drop(core);
    let mut core = ClientCore::open(c.clone()).expect("reopen in phase 1");
    assert_eq!(info(&core).phase, 1);
    let body = core
        .session_sign(&[7; 32], 0)
        .expect("the DSK was reloaded");
    let sig = decode_strict(&body, |d| {
        d.array(5)?;
        d.skip()?;
        d.skip()?;
        let s = d.bytes_exact::<64>()?;
        d.skip()?;
        d.skip()?;
        Ok(s)
    })
    .expect("session body");
    VerifyingKey::from_bytes(&reg.dsk_pub)
        .expect("key")
        .verify_strict(
            &session_preimage(&INSTANCE, &reg.device_id, &[7; 32], 0),
            &Signature::from_bytes(&sig),
        )
        .expect("signed with the stored DSK");
    core.signup_complete(&USER, "alice", NOW, &INSTANCE_SIGNER_PUB)
        .expect("complete");
    drop(core);
    let mut core = ClientCore::open(c).expect("reopen in phase 2");
    let i = info(&core);
    assert_eq!(
        (i.phase, i.user, i.username.as_str()),
        (2, Some(USER), "alice")
    );
    core.key_packages(1, false).expect("phase 2 after reopen");
}

#[test]
fn open_refuses_a_store_whose_device_key_is_gone() {
    let c = conn();
    let mut core = ClientCore::open(c.clone()).expect("open");
    core.signup_begin(&INSTANCE).expect("begin");
    drop(core);
    c.lock()
        .expect("lock")
        .execute("DELETE FROM openmls_signature_keys", [])
        .expect("delete");
    let e = match ClientCore::open(c) {
        Ok(_) => panic!("a store without its DSK must be refused"),
        Err(e) => e,
    };
    assert_eq!(
        e,
        ClientError {
            code: "E_CORE_STATE",
            detail: "the device key is missing from the store".into()
        }
    );
}

// ---------------------------------------------------------------------------------------------
// session signature and record

#[test]
fn the_session_preimage_reproduces_the_vector() {
    let doc: serde_json::Value = serde_json::from_str(IDENTITY_JSON).expect("identity.json");
    let v = &doc["session_preimage"];
    let field = |k: &str| hex::decode(v[k].as_str().expect("hex string")).expect("hex");
    let instance: [u8; 16] = field("instance_id").try_into().expect("16 bytes");
    let device: [u8; 16] = field("device_id").try_into().expect("16 bytes");
    let nonce: [u8; 32] = field("nonce").try_into().expect("32 bytes");
    let purpose = u8::try_from(v["purpose"].as_u64().expect("purpose")).expect("one byte");
    let pre = session_preimage(&instance, &device, &nonce, purpose);
    assert_eq!(pre.len(), 81);
    assert_eq!(pre, field("preimage"));
}

#[test]
fn session_sign_signs_the_81_byte_preimage_with_the_device_key() {
    let (mut core, _c, _rk, reg) = begun();
    let nonce = [0xab; 32];
    let key = VerifyingKey::from_bytes(&reg.dsk_pub).expect("dsk_pub");
    for purpose in [0u8, 1] {
        let body = core
            .session_sign(&nonce, purpose)
            .expect("phase 1 may sign");
        let (n, p, sig) = decode_strict(&body, |d| {
            d.array(5)?;
            let n = d.bytes_exact::<32>()?;
            let p = d.uint()?;
            let sig = d.bytes_exact::<64>()?;
            d.null()?;
            d.null()?;
            Ok((n, p, sig))
        })
        .expect("[nonce, purpose, sig, null, null]");
        assert_eq!((n, p), (nonce, u64::from(purpose)));
        key.verify_strict(
            &session_preimage(&INSTANCE, &reg.device_id, &nonce, purpose),
            &Signature::from_bytes(&sig),
        )
        .expect("the signature covers the preimage with its purpose byte");
        let other = 1 - purpose;
        assert!(
            key.verify_strict(
                &session_preimage(&INSTANCE, &reg.device_id, &nonce, other),
                &Signature::from_bytes(&sig),
            )
            .is_err(),
            "a purpose-{purpose} signature must not pass as purpose {other}"
        );
    }
    assert_eq!(
        err(core.session_sign(&nonce, 2)),
        ClientError {
            code: "E_CORE_INPUT",
            detail: "purpose must be 0 or 1".into()
        }
    );
    core.signup_complete(&USER, "alice", NOW, &INSTANCE_SIGNER_PUB)
        .expect("complete");
    core.session_sign(&nonce, 0).expect("phase 2 signs too");
}

#[test]
fn the_session_record_is_stored_replaced_and_cleared() {
    let (mut core, c, _rk, _reg) = begun();
    assert_eq!(
        err(core.session_store("", 1, 1)),
        ClientError {
            code: "E_CORE_INPUT",
            detail: "token must be 1..=256 bytes".into()
        }
    );
    assert_eq!(
        err(core.session_store(&"t".repeat(257), 1, 1)).code,
        "E_CORE_INPUT"
    );
    core.session_store("tok-1", 1_790_604_800, 1_790_043_200)
        .expect("store");
    assert_eq!(
        session_of(&core),
        Some(("tok-1".to_owned(), 1_790_604_800, 1_790_043_200))
    );
    core.session_store("tok-2", 20, 10).expect("replace");
    assert_eq!(session_of(&core), Some(("tok-2".to_owned(), 20, 10)));
    core.session_clear().expect("clear");
    assert_eq!(session_of(&core), None);
    assert!(meta(&c, "session").is_none());
}

// ---------------------------------------------------------------------------------------------
// KeyPackages

#[test]
fn key_packages_are_mls_messages_the_instance_validates() {
    let (mut core, _c, _rk, reg) = begun();
    assert_eq!(err(core.key_packages(1, false)).code, "E_CORE_NO_IDENTITY");
    core.signup_complete(&USER, "alice", NOW, &INSTANCE_SIGNER_PUB)
        .expect("complete");
    for bad in [0u32, 33] {
        assert_eq!(
            err(core.key_packages(bad, false)),
            ClientError {
                code: "E_CORE_INPUT",
                detail: "count must be 1..=32".into()
            }
        );
    }
    let body = core.key_packages(3, true).expect("key_packages");
    let (packages, last) = decode_strict(&body, |d| {
        d.array(2)?;
        let n = d.array_len()?;
        let mut packages = Vec::new();
        for _ in 0..n {
            packages.push(d.bytes()?.to_vec());
        }
        let last = d.opt_bytes()?.map(<[u8]>::to_vec);
        Ok((packages, last))
    })
    .expect("[packages, last_resort]");
    assert_eq!(packages.len(), 3);
    let last = last.expect("a last-resort package was asked for");
    for (bytes, last_resort) in packages
        .iter()
        .map(|p| (p.as_slice(), false))
        .chain([(last.as_slice(), true)])
    {
        let kp = validated(bytes);
        assert_eq!(kp.last_resort(), last_resort);
        let basic = BasicCredential::try_from(kp.leaf_node().credential().clone())
            .expect("basic credential");
        let cred = CredentialIdentity::decode(basic.identity()).expect("credential identity");
        assert_eq!(
            (cred.user_id, cred.device_id, cred.tier),
            (
                UserId::from_bytes(USER),
                DeviceId::from_bytes(reg.device_id),
                Tier::Browser
            )
        );
        assert_eq!(kp.leaf_node().signature_key().as_slice(), &reg.dsk_pub[..]);
    }
    let body = core.key_packages(32, false).expect("32 packages");
    let (n, last_is_null) = decode_strict(&body, |d| {
        d.array(2)?;
        let n = d.array_len()?;
        for _ in 0..n {
            d.bytes()?;
        }
        Ok((n, d.try_null()?))
    })
    .expect("[packages, null]");
    assert_eq!((n, last_is_null), (32, true));
}

/// A store holding only an `enrol` record (L-CORE-22), written by hand as task 3's enrol_begin
/// will write it, with its DSK in OpenMLS's signature-key table.
fn enrolling(user: Option<[u8; 16]>) -> (ClientCore, ConnHandle, [u8; 16], [u8; 32]) {
    let c = conn();
    drop(ClientCore::open(c.clone()).expect("open"));
    let signer = SignatureKeyPair::new(CIPHERSUITE.signature_algorithm()).expect("keygen");
    signer
        .store(DillaProvider::new(c.clone()).storage())
        .expect("store the DSK");
    let dsk_pub: [u8; 32] = signer.public().try_into().expect("32 bytes");
    let device = [0x5d; 16];
    let mut e = Encoder::new();
    e.array(5)
        .uint(1)
        .bytes(&INSTANCE)
        .bytes(&device)
        .bytes(&dsk_pub);
    match user {
        Some(u) => {
            e.bytes(&u);
        }
        None => {
            e.null();
        }
    }
    c.lock()
        .expect("lock")
        .execute(
            "INSERT INTO app_meta (k, v) VALUES ('enrol', ?1)",
            [e.into_vec()],
        )
        .expect("enrol record");
    let core = ClientCore::open(c.clone()).expect("open in phase 3");
    (core, c, device, dsk_pub)
}

/// session_sign's phase-3 answer is not asserted here: task 1's forced arm is phase 0's refusal and
/// task 3 replaces it with the enrol record's ids (head L-CORE-22's split), asserting it there.
#[test]
fn an_enrol_record_is_phase_3_stores_sessions_and_refuses_the_identity_calls() {
    let (mut core, _c, device, _dsk_pub) = enrolling(None);
    let i = info(&core);
    assert_eq!(
        (
            i.phase,
            i.instance,
            i.user,
            i.device,
            i.username.as_str(),
            i.published
        ),
        (3, Some(INSTANCE), None, Some(device), "", 0)
    );
    core.session_store("pending-token", NOW + 60, NOW + 30)
        .expect("phase 3 stores a session");
    assert_eq!(
        session_of(&core),
        Some(("pending-token".into(), NOW + 60, NOW + 30))
    );
    assert_eq!(
        err(core.signup_begin(&INSTANCE)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "an enrolment is pending".into(),
        }
    );
    let no_signup = ClientError {
        code: "E_CORE_STATE",
        detail: "no signup is pending".into(),
    };
    assert_eq!(err(core.signup_request("i", "u", "d", None)), no_signup);
    assert_eq!(
        err(core.signup_complete(&USER, "alice", NOW, &INSTANCE_SIGNER_PUB)),
        no_signup
    );
    assert_eq!(err(core.signup_reset()), no_signup);
    for code in [
        err(core.device_list_body()).code,
        err(core.device_list_published()).code,
        err(core.key_packages(1, false)).code,
        err(core.send_prepare(&GROUP, &request(0, None, "x", &[]), NOW)).code,
    ] {
        assert_eq!(code, "E_CORE_NO_IDENTITY");
    }
    assert_eq!(
        core.sealed_objects().expect("every phase"),
        sealed(None, None, 0)
    );
    core.session_clear().expect("every phase");

    let (core, _c, _, _) = enrolling(Some(USER));
    assert_eq!(info(&core).user, Some(USER), "the recorded user is shown");
}

#[test]
fn an_enrol_record_beside_another_phase_or_without_its_key_is_refused() {
    let beside = ClientError {
        code: "E_CORE_STATE",
        detail: "enrol record beside another phase".into(),
    };
    let enrol = |device: u8| {
        let mut e = Encoder::new();
        e.array(5)
            .uint(1)
            .bytes(&INSTANCE)
            .bytes(&[device; 16])
            .bytes(&[0x09; 32])
            .null();
        e.into_vec()
    };
    // Beside a pending signup.
    let (core, c, _rk, _reg) = begun();
    drop(core);
    c.lock()
        .expect("lock")
        .execute(
            "INSERT INTO app_meta (k, v) VALUES ('enrol', ?1)",
            [enrol(1)],
        )
        .expect("seed");
    assert_eq!(err(ClientCore::open(c.clone())), beside);
    // Beside a complete identity.
    let (mut core, c, _rk, _reg) = begun();
    core.signup_complete(&USER, "alice", NOW, &INSTANCE_SIGNER_PUB)
        .expect("complete");
    drop(core);
    c.lock()
        .expect("lock")
        .execute(
            "INSERT INTO app_meta (k, v) VALUES ('enrol', ?1)",
            [enrol(2)],
        )
        .expect("seed");
    assert_eq!(err(ClientCore::open(c.clone())), beside);
    // Alone, but its DSK is not stored.
    let c = conn();
    drop(ClientCore::open(c.clone()).expect("open"));
    c.lock()
        .expect("lock")
        .execute(
            "INSERT INTO app_meta (k, v) VALUES ('enrol', ?1)",
            [enrol(3)],
        )
        .expect("seed");
    assert_eq!(
        err(ClientCore::open(c.clone())),
        ClientError {
            code: "E_CORE_STATE",
            detail: "the device key is missing from the store".into()
        }
    );
    // Malformed.
    c.lock()
        .expect("lock")
        .execute("UPDATE app_meta SET v = x'8102' WHERE k = 'enrol'", [])
        .expect("corrupt");
    assert_eq!(
        err(ClientCore::open(c)),
        ClientError {
            code: "E_CORE_STORAGE",
            detail: "app_meta enrol is malformed".into()
        }
    );
}

// ---------------------------------------------------------------------------------------------
// enrolment by recovery key, revocation and the own list (web-2a task 3; L-CORE-26…28)
//
// Who can trigger each refusal tested below against an honest person (lesson e):
// - E_RECOVERY_KEY: the instance can make every key fail by serving other bytes (a denial it
//   can mount anyway), but cannot make a wrong key pass under K_header.
// - The state object (ruling 28 as amended by the core-block security review): missing or not
//   opening under K_backup is accepted with no pins ([]) only while the served list is version 1,
//   where the signup's state holds pins = [] and nothing is protected; at any later version it is
//   "the backup state is missing" / "the backup state could not be read" (E_CORE_INPUT). An
//   enrolled session or the instance can trigger both by deleting or replacing the object; the
//   honest cure is a re-upload of the state from a device that holds it (the web liveness rule).
// - "the instance served a different device list at the stored version" (E_CORE_INPUT): only an
//   instance serving a fork the account's own SSK signed at the version this device stored.
// - E_CREDENTIAL on a served list: only a list the user's SSK did not sign (a hostile instance, or a
//   stored list for another user). The honest instance serves the user's own list, which verifies.
// - "device is listed" (any entry with this device id, live, revoked or under another key): an
//   honest caller reaches it only by repeating a completed enrolment, and recovers by
//   re-establishing (enrol_reset draws a fresh id).
// - E_DEVICE_LIST_STALE from own_device_list_update: only an instance that serves a gap or a lower
//   version. The honest instance serves the contiguous chain (L-HTTP-56).
// - "the instance served an older device list" (E_CORE_INPUT): only an instance that serves a
//   signed list older than the stored newest or the list in the opened state object. A hostile
//   instance must not make the device sign old+1, which could re-list a device revoked in between;
//   it still can with a consistent pair (list k and the state object written at k), the residue
//   documented at `floor` in client/identity.rs. The honest instance serves its newest accepted list. Clients must publish the candidate list
//   before uploading a state object that carries its version; otherwise the honest instance can
//   temporarily hold state(v+1) while serving list v and this floor refuses it too.
// - the device_ids refusals: only the caller's own arguments.
// None can be triggered by another member or another user: every input is the own user's objects.

const LATER: u64 = NOW + 60;

/// One pin row of protocol/06's state object, so the carried-through `pins` value is not `[]`.
fn pins() -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(1)
        .array(4)
        .bytes(&[0x55; 16])
        .bytes(&[0x66; 32])
        .uint(1_789_000_000)
        .uint(1);
    e.into_vec()
}

/// Core A after signup, its list v1 accepted: (core, connection, the key as shown, registration,
/// the v1 PUT body, which is also the shape of the GET 200 body).
fn signed_up() -> (ClientCore, ConnHandle, String, Registration, Vec<u8>) {
    let (mut core, c) = core();
    let rk_text = core.signup_begin(&INSTANCE).expect("signup_begin");
    let reg = registration(
        &core
            .signup_request("INV-1", "alice", "Alice", None)
            .expect("signup_request"),
    );
    let put = core
        .signup_complete(&USER, "alice", NOW, &INSTANCE_SIGNER_PUB)
        .expect("signup_complete");
    core.device_list_published().expect("device_list_published");
    (core, c, rk_text, reg, put)
}

/// `[root_sealed|null, state_sealed|null, state_uploaded]`.
fn sealed_of(core: &ClientCore) -> (Option<Vec<u8>>, Option<Vec<u8>>, u64) {
    let bytes = core.sealed_objects().expect("sealed_objects");
    decode_strict(&bytes, |d| {
        d.array(3)?;
        Ok((
            d.opt_bytes()?.map(<[u8]>::to_vec),
            d.opt_bytes()?.map(<[u8]>::to_vec),
            d.uint()?,
        ))
    })
    .expect("[root|null, state|null, state_uploaded]")
}

fn begin_enrol(core: &mut ClientCore) -> ([u8; 16], [u8; 32]) {
    let bytes = core.enrol_begin(&INSTANCE).expect("enrol_begin");
    decode_strict(&bytes, |d| {
        d.array(2)?;
        Ok((d.bytes_exact::<16>()?, d.bytes_exact::<32>()?))
    })
    .expect("[device_id, dsk_pub]")
}

/// `[device_list_put_body, state_sealed, null]`: a result with no interrupted publication.
fn list_and_state(bytes: &[u8]) -> (Vec<u8>, Vec<u8>) {
    let (put, state, interrupted) = signed_lists(bytes);
    assert_eq!(interrupted, None, "no interrupted publication");
    (put, state)
}

/// `[device_list_put_body, state_sealed, interrupted_put_body | null]` (BACKUPS-RECOVERY-02).
fn signed_lists(bytes: &[u8]) -> (Vec<u8>, Vec<u8>, Option<Vec<u8>>) {
    decode_strict(bytes, |d| {
        d.array(3)?;
        Ok((
            d.bytes()?.to_vec(),
            d.bytes()?.to_vec(),
            d.opt_bytes()?.map(<[u8]>::to_vec),
        ))
    })
    .expect("[device_list_put_body, state_sealed, interrupted|null]")
}

fn put_parts(body: &[u8]) -> (u64, Vec<u8>, [u8; 64], [u8; 32]) {
    decode_strict(body, |d| {
        d.array(4)?;
        Ok((
            d.uint()?,
            d.bytes()?.to_vec(),
            d.bytes_exact::<64>()?,
            d.bytes_exact::<32>()?,
        ))
    })
    .expect("[version, blob, ssk_signature, prev_hash]")
}

fn raw_put_body(version: u64, blob: &[u8], sig: &[u8; 64], prev: &[u8; 32]) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(4).uint(version).bytes(blob).bytes(sig).bytes(prev);
    e.into_vec()
}

fn put_body(list: &DeviceList) -> Vec<u8> {
    raw_put_body(
        list.unsigned.version,
        &list.encode(),
        &list.sig_ssk,
        &list.unsigned.prev_hash,
    )
}

/// The 200 body of `GET …/device-list?after=N`.
fn history(lists: &[&DeviceList]) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(lists.len());
    for l in lists {
        e.array(4)
            .uint(l.unsigned.version)
            .bytes(&l.encode())
            .bytes(&l.sig_ssk)
            .bytes(&l.unsigned.prev_hash);
    }
    e.into_vec()
}

fn seal_with(key: &[u8; 32], aad: &[u8], plaintext: &[u8]) -> Vec<u8> {
    let nonce = [0x24u8; 12];
    let ct = Aes256Gcm::new_from_slice(key)
        .expect("key")
        .encrypt(
            &Nonce::from(nonce),
            Payload {
                msg: plaintext,
                aad,
            },
        )
        .expect("seal");
    let mut e = Encoder::new();
    e.array(3).uint(1).bytes(&nonce).bytes(&ct);
    e.into_vec()
}

fn state_plain(list_blob: &[u8], pins: &[u8]) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(3).uint(1).bytes(list_blob).raw(pins);
    e.into_vec()
}

/// (device_list blob, raw pins) of a stored state object.
fn state_parts(key: &[u8; 32], stored: &[u8]) -> (Vec<u8>, Vec<u8>) {
    let plain = open_sealed(key, b"dilla state v1", stored);
    decode_strict(&plain, |d| {
        d.array(3)?;
        assert_eq!(d.uint()?, 1);
        Ok((d.bytes()?.to_vec(), d.skip()?.to_vec()))
    })
    .expect("[1, device_list, pins]")
}

/// The key as a person types it from paper: lower case, groups of four, hyphens, spaces, a newline.
fn as_typed(rk_text: &str) -> String {
    let groups: Vec<String> = rk_text
        .as_bytes()
        .chunks(4)
        .map(|g| String::from_utf8(g.to_vec()).expect("ascii").to_lowercase())
        .collect();
    format!("{}\n", groups.join(" - "))
}

fn browser(id: [u8; 16], dsk: [u8; 32], added: u64, revoked: Option<u64>) -> DeviceEntry {
    DeviceEntry {
        device_id: DeviceId::from_bytes(id),
        dsk_pub: dsk,
        tier: Tier::Browser,
        added_at: added,
        revoked_at: revoked,
    }
}

fn signed(
    ssk: &[u8; 32],
    user: [u8; 16],
    version: u64,
    prev_hash: [u8; 32],
    entries: Vec<DeviceEntry>,
) -> DeviceList {
    let unsigned = DeviceListUnsigned {
        v: 1,
        user_id: UserId::from_bytes(user),
        version,
        prev_hash,
        entries,
    };
    let sig_ssk = SskSigner::from_bytes(ssk).sign_device_list(&unsigned);
    DeviceList { unsigned, sig_ssk }
}

type OwnEntry = ([u8; 16], [u8; 32], u64, u64, Option<u64>);

fn own_list(core: &ClientCore) -> (u64, u64, Vec<OwnEntry>) {
    let bytes = core.own_device_list().expect("own_device_list");
    decode_strict(&bytes, |d| {
        d.array(3)?;
        let version = d.uint()?;
        let published = d.uint()?;
        let n = d.array_len()?;
        let mut entries = Vec::new();
        for _ in 0..n {
            d.array(5)?;
            entries.push((
                d.bytes_exact::<16>()?,
                d.bytes_exact::<32>()?,
                d.uint()?,
                d.uint()?,
                d.opt_uint()?,
            ));
        }
        Ok((version, published, entries))
    })
    .expect("[version, published, [[device_id, dsk_pub, tier, added_at, revoked_at]]]")
}

fn status(bytes: &[u8]) -> (u64, u64) {
    decode_strict(bytes, |d| {
        d.array(2)?;
        Ok((d.uint()?, d.uint()?))
    })
    .expect("[version, listed]")
}

struct IdentityV2 {
    user: [u8; 16],
    device: [u8; 16],
    dsk: [u8; 32],
    umk: [u8; 32],
    ssk: [u8; 32],
    username: String,
    credential: Vec<u8>,
    device_list_body: Vec<u8>,
    published: u64,
    device_list: Vec<u8>,
    state_uploaded: u64,
}

fn identity_v2(c: &ConnHandle) -> IdentityV2 {
    let record = meta(c, "identity").expect("identity record");
    decode_strict(&record, |d| {
        d.array(13)?;
        assert_eq!(d.uint()?, 2, "record version");
        assert_eq!(d.bytes_exact::<16>()?, INSTANCE);
        Ok(IdentityV2 {
            user: d.bytes_exact()?,
            device: d.bytes_exact()?,
            dsk: d.bytes_exact()?,
            umk: d.bytes_exact()?,
            ssk: d.bytes_exact()?,
            username: d.text()?.to_owned(),
            credential: d.bytes()?.to_vec(),
            device_list_body: d.bytes()?.to_vec(),
            published: d.uint()?,
            device_list: d.bytes()?.to_vec(),
            state_uploaded: d.uint()?,
        })
    })
    .expect("the thirteen-element identity record")
}

fn enrol_record(c: &ConnHandle) -> ([u8; 16], [u8; 16], [u8; 32], Option<[u8; 16]>) {
    let record = meta(c, "enrol").expect("enrol record");
    decode_strict(&record, |d| {
        d.array(5)?;
        assert_eq!(d.uint()?, 1);
        Ok((
            d.bytes_exact::<16>()?,
            d.bytes_exact::<16>()?,
            d.bytes_exact::<32>()?,
            d.opt_bytes_exact::<16>()?,
        ))
    })
    .expect("[1, instance_id, device_id, dsk_pub, user_id|null]")
}

fn keys(c: &ConnHandle) -> i64 {
    c.lock()
        .expect("lock")
        .query_row("SELECT count(*) FROM openmls_signature_keys", [], |r| {
            r.get(0)
        })
        .expect("count")
}

/// Everything a refused call must leave as it was: app_meta and the signature-key count.
fn snapshot(c: &ConnHandle) -> (Vec<(String, Vec<u8>)>, i64) {
    let mut rows = meta_rows(c);
    rows.sort();
    (rows, keys(c))
}

/// What a second browser fetches: A's account, its objects and its served list.
struct Material {
    rk_text: String,
    rk: [u8; 32],
    reg: Registration,
    root: Vec<u8>,
    state: Vec<u8>,
    put_v1: Vec<u8>,
    v1: DeviceList,
    umk: [u8; 32],
    ssk: [u8; 32],
}

/// The material of an account whose core the caller keeps (to adopt or revoke afterwards).
fn material_of(core: &ClientCore, rk_text: String, reg: Registration, put_v1: Vec<u8>) -> Material {
    let rk = recovery_key_from_base32(&rk_text).expect("rk");
    let (root, state, _) = sealed_of(core);
    let (root, state) = (root.expect("root"), state.expect("state"));
    let (umk, ssk) = root_keys(&root, &rk);
    let v1 = DeviceList::decode(&put_parts(&put_v1).1).expect("v1");
    Material {
        rk_text,
        rk,
        reg,
        root,
        state,
        put_v1,
        v1,
        umk,
        ssk,
    }
}

/// The material of a fresh account whose core is dropped.
fn material() -> Material {
    let (core, _c, rk_text, reg, put_v1) = signed_up();
    material_of(&core, rk_text, reg, put_v1)
}

/// Core B: enrol_begin, then enrol_registered for USER.
struct Second {
    core: ClientCore,
    c: ConnHandle,
    device: [u8; 16],
    dsk: [u8; 32],
}

fn second() -> Second {
    let (mut core, c) = core();
    let (device, dsk) = begin_enrol(&mut core);
    core.enrol_registered(&USER).expect("enrol_registered");
    Second {
        core,
        c,
        device,
        dsk,
    }
}

#[test]
fn an_enrolling_core_is_phase_3_and_signs_its_registration_with_the_login() {
    let (mut b, bc) = core();
    for e in [
        err(b.enrol_session_sign(&[0xab; 32], b"x")),
        err(b.enrol_registered(&USER)),
        err(b.enrol_reset()),
        err(b.enrol_complete("k", &[0x80], &[0x80], &[0x80], "alice", NOW)),
    ] {
        assert_eq!(
            e,
            ClientError {
                code: "E_CORE_STATE",
                detail: "no enrolment is pending".into()
            }
        );
    }
    for e in [
        err(b.own_device_list()),
        err(b.own_device_list_update(&[0x80])),
        err(b.state_sealed_uploaded()),
        err(b.device_list_revoke("k", &[0x80], &[0x80], &[0x80], &[0x77; 16], NOW)),
    ] {
        assert_eq!(
            e,
            ClientError {
                code: "E_CORE_NO_IDENTITY",
                detail: String::new()
            }
        );
    }

    let (device, dsk) = begin_enrol(&mut b);
    let i = info(&b);
    assert_eq!(
        (
            i.phase,
            i.instance,
            i.user,
            i.device,
            i.username.as_str(),
            i.published
        ),
        (3, Some(INSTANCE), None, Some(device), "", 0)
    );
    // Task 1's phase-3 answer of identity(), byte for byte: [3, instance_id, null, device_id, "", 0].
    let mut phase3 = Encoder::new();
    phase3
        .array(6)
        .uint(3)
        .bytes(&INSTANCE)
        .null()
        .bytes(&device)
        .text("")
        .uint(0);
    assert_eq!(
        b.identity().expect("identity in phase 3"),
        phase3.into_vec()
    );
    assert_eq!(enrol_record(&bc), (INSTANCE, device, dsk, None));
    assert_eq!(keys(&bc), 1, "the DSK is stored");
    assert_eq!(
        err(b.enrol_begin(&INSTANCE)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "an enrolment is pending".into()
        }
    );
    assert_eq!(
        b.sealed_objects().expect("sealed"),
        [0x83, 0xf6, 0xf6, 0x00]
    );

    let nonce = [0xab; 32];
    let body = b
        .enrol_session_sign(&nonce, b"assertion-token")
        .expect("enrol_session_sign");
    let (n, purpose, sig, reg_device, reg_dsk, tier, signer_tier, placeholder, login) =
        decode_strict(&body, |d| {
            d.array(5)?;
            let n = d.bytes_exact::<32>()?;
            let purpose = d.uint()?;
            let sig = d.bytes_exact::<64>()?;
            d.array(5)?;
            let reg_device = d.bytes_exact::<16>()?;
            let reg_dsk = d.bytes_exact::<32>()?;
            let tier = d.uint()?;
            let signer_tier = d.uint()?;
            let placeholder = d.bytes()?.to_vec();
            let login = d.bytes()?.to_vec();
            Ok((
                n,
                purpose,
                sig,
                reg_device,
                reg_dsk,
                tier,
                signer_tier,
                placeholder,
                login,
            ))
        })
        .expect("[nonce, 0, sig, [device_id, dsk_pub, 1, 1, credential], login]");
    assert_eq!((n, purpose), (nonce, 0));
    assert_eq!(
        (reg_device, reg_dsk, tier, signer_tier),
        (device, dsk, 1, 1)
    );
    assert_eq!(login, b"assertion-token");
    VerifyingKey::from_bytes(&dsk)
        .expect("dsk_pub")
        .verify_strict(
            &session_preimage(&INSTANCE, &device, &nonce, 0),
            &Signature::from_bytes(&sig),
        )
        .expect("purpose-0 signature over the registering device's preimage");
    assert_eq!(
        CredentialIdentity::decode(&placeholder).expect("ten-element placeholder"),
        CredentialIdentity {
            v: 1,
            umk_pub: [0; 32],
            user_id: UserId::from_bytes([0; 16]),
            device_id: DeviceId::from_bytes(device),
            kind: Kind::User,
            tier: Tier::Browser,
            signer_tier: SignerTier::Browser,
            ssk_pub: [0; 32],
            sig_umk_ssk: [0; 64],
            sig_ssk_dev: [0; 64],
        }
    );
    for bad in [0usize, 257] {
        assert_eq!(
            err(b.enrol_session_sign(&nonce, &vec![0x61; bad])),
            ClientError {
                code: "E_CORE_INPUT",
                detail: "login must be 1..=256 bytes".into()
            }
        );
    }
    b.enrol_session_sign(&nonce, &[0x61; 256])
        .expect("256 bytes is the bound");

    let renew = b.session_sign(&nonce, 1).expect("phase 3 signs sessions");
    let renew_sig = decode_strict(&renew, |d| {
        d.array(5)?;
        d.skip()?;
        assert_eq!(d.uint()?, 1);
        let s = d.bytes_exact::<64>()?;
        d.null()?;
        d.null()?;
        Ok(s)
    })
    .expect("[nonce, 1, sig, null, null]");
    VerifyingKey::from_bytes(&dsk)
        .expect("dsk_pub")
        .verify_strict(
            &session_preimage(&INSTANCE, &device, &nonce, 1),
            &Signature::from_bytes(&renew_sig),
        )
        .expect("purpose-1 signature");
    b.session_store("pending-token", 10, 10)
        .expect("phase 3 stores its pending session");
    assert_eq!(session_of(&b), Some(("pending-token".to_owned(), 10, 10)));

    assert_eq!(err(b.key_packages(1, false)).code, "E_CORE_NO_IDENTITY");
    assert_eq!(err(b.device_list_body()).code, "E_CORE_NO_IDENTITY");
    assert_eq!(err(b.device_list_published()).code, "E_CORE_NO_IDENTITY");
    assert_eq!(err(b.own_device_list()).code, "E_CORE_NO_IDENTITY");
    assert_eq!(
        err(b.signup_request("i", "u", "d", None)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "no signup is pending".into()
        }
    );
    assert_eq!(
        err(b.signup_begin(&INSTANCE)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "an enrolment is pending".into()
        },
        "task 1's phase-3 arm of signup_begin"
    );

    assert_eq!(
        err(b.enrol_complete("k", &[0x80], &[0x80], &[0x80], "alice", NOW)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "no user recorded".into()
        }
    );
    assert_eq!(
        err(b.enrol_registered(&[0; 16])),
        ClientError {
            code: "E_CORE_INPUT",
            detail: "user_id is all zero".into()
        }
    );
    b.enrol_registered(&USER).expect("enrol_registered");
    b.enrol_registered(&USER)
        .expect("the same id again is a no-op");
    assert_eq!(
        err(b.enrol_registered(&[0x43; 16])),
        ClientError {
            code: "E_CORE_STATE",
            detail: "user already recorded".into()
        }
    );
    assert_eq!(enrol_record(&bc), (INSTANCE, device, dsk, Some(USER)));
    assert_eq!((info(&b).phase, info(&b).user), (3, Some(USER)));

    drop(b);
    let b = ClientCore::open(bc.clone()).expect("reopen in phase 3");
    assert_eq!((info(&b).phase, info(&b).device), (3, Some(device)));
    let again = b
        .session_sign(&[0x01; 32], 0)
        .expect("the DSK of the enrol record was reloaded");
    // 0x85 | 0x58 0x20 nonce | 0x00 | 0x58 0x40 sig | 0xf6 0xf6
    assert_eq!(again.len(), 1 + 34 + 1 + 66 + 2);

    let (mut a, _ac, _rk, _reg, _put) = signed_up();
    assert_eq!(
        err(a.enrol_begin(&INSTANCE)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "an identity exists".into()
        }
    );
    let (mut p, _pc, _prk, _preg) = begun();
    assert_eq!(
        err(p.enrol_begin(&INSTANCE)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "a signup is pending".into()
        }
    );
}

#[test]
fn enrol_complete_adds_this_browser_to_the_list_the_root_object_vouches_for() {
    let (mut a, _ac, rk_text, reg, put_v1) = signed_up();
    let m = material_of(&a, rk_text, reg, put_v1);
    let state_in = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&m.v1.encode(), &pins()),
    );
    let mut b = second();
    let out = b
        .core
        .enrol_complete(
            &as_typed(&m.rk_text),
            &m.root,
            &state_in,
            &m.put_v1,
            "alice",
            LATER,
        )
        .expect("enrol_complete with the key as typed from paper");
    let (put_v2, state_b) = list_and_state(&out);

    let (version, blob_v2, sig_v2, prev_v2) = put_parts(&put_v2);
    let v2 = DeviceList::decode(&blob_v2).expect("v2");
    assert_eq!((version, prev_v2, v2.sig_ssk), (2, m.v1.hash(), sig_v2));
    assert_eq!(
        (
            v2.unsigned.user_id,
            v2.unsigned.version,
            v2.unsigned.prev_hash
        ),
        (UserId::from_bytes(USER), 2, m.v1.hash())
    );
    assert_eq!(
        v2.unsigned.entries,
        vec![
            m.v1.unsigned.entries[0].clone(),
            browser(b.device, b.dsk, LATER, None)
        ]
    );
    v2.accept(Some(&m.v1), &m.reg.ssk_pub)
        .expect("v2 chains from v1 under the account's SSK");

    let (inner, carried) = state_parts(&k_backup(&m.rk), &state_b);
    assert_eq!(inner, blob_v2, "the state object carries the new list");
    assert_eq!(
        carried,
        pins(),
        "the pins are carried through byte for byte"
    );

    let i = info(&b.core);
    assert_eq!(
        (
            i.phase,
            i.instance,
            i.user,
            i.device,
            i.username.as_str(),
            i.published
        ),
        (2, Some(INSTANCE), Some(USER), Some(b.device), "alice", 0)
    );
    assert!(meta(&b.c, "enrol").is_none(), "the enrol record is gone");
    assert_eq!(meta(&b.c, "root_sealed"), Some(m.root.clone()));
    assert_eq!(meta(&b.c, "state_sealed"), Some(state_b.clone()));
    assert_eq!(
        sealed_of(&b.core),
        (Some(m.root.clone()), Some(state_b.clone()), 0)
    );

    let rec = identity_v2(&b.c);
    assert_eq!(
        (
            rec.user,
            rec.device,
            rec.dsk,
            rec.umk,
            rec.ssk,
            rec.username.as_str()
        ),
        (USER, b.device, b.dsk, m.reg.umk_pub, m.reg.ssk_pub, "alice")
    );
    assert_eq!(
        (
            rec.device_list_body.clone(),
            rec.published,
            rec.state_uploaded
        ),
        (put_v2.clone(), 0, 0)
    );
    assert_eq!(
        rec.device_list,
        m.v1.encode(),
        "the accepted list is the served one"
    );
    let cred = CredentialIdentity::decode(&rec.credential).expect("credential");
    assert_eq!(
        (
            cred.user_id,
            cred.device_id,
            cred.kind,
            cred.tier,
            cred.signer_tier
        ),
        (
            UserId::from_bytes(USER),
            DeviceId::from_bytes(b.device),
            Kind::User,
            Tier::Browser,
            SignerTier::Browser
        )
    );
    assert_eq!((cred.umk_pub, cred.ssk_pub), (m.reg.umk_pub, m.reg.ssk_pub));
    cred.verify_signatures(&b.dsk)
        .expect("the credential signed with the recovered keys verifies");

    assert_eq!(b.core.device_list_body().expect("candidate"), put_v2);
    let (v, published, entries) = own_list(&b.core);
    assert_eq!((v, published, entries.len()), (1, 0, 1));
    b.core.device_list_published().expect("published");
    let (v, published, entries) = own_list(&b.core);
    assert_eq!((v, published), (2, 1), "the candidate was promoted");
    assert_eq!(
        entries[1],
        (b.device, b.dsk, 1, LATER, None),
        "this browser's entry"
    );
    assert_eq!(info(&b.core).published, 1);
    b.core.state_sealed_uploaded().expect("uploaded");
    b.core.state_sealed_uploaded().expect("idempotent");
    assert_eq!(sealed_of(&b.core).2, 1);

    let kp_body = b
        .core
        .key_packages(1, false)
        .expect("key_packages in phase 2");
    let package = decode_strict(&kp_body, |d| {
        d.array(2)?;
        d.array(1)?;
        let p = d.bytes()?.to_vec();
        d.null()?;
        Ok(p)
    })
    .expect("[[package], null]");
    let kp = validated(&package);
    let basic =
        BasicCredential::try_from(kp.leaf_node().credential().clone()).expect("basic credential");
    let leaf = CredentialIdentity::decode(basic.identity()).expect("credential identity");
    assert_eq!(
        (leaf.user_id, leaf.device_id),
        (UserId::from_bytes(USER), DeviceId::from_bytes(b.device))
    );
    assert_eq!(kp.leaf_node().signature_key().as_slice(), &b.dsk[..]);

    let Second {
        core: bcore, c: bc, ..
    } = b;
    drop(bcore);
    let mut reopened = ClientCore::open(bc).expect("reopen in phase 2");
    assert_eq!(info(&reopened).phase, 2);
    reopened
        .key_packages(1, false)
        .expect("the DSK survives a reopen");

    // A, the first browser, adopts v2 from the history route: listed, published.
    assert_eq!(
        status(
            &a.own_device_list_update(&history(&[&v2]))
                .expect("adopt v2")
        ),
        (2, 1)
    );
    let (v, published, entries) = own_list(&a);
    assert_eq!((v, published, entries.len()), (2, 1, 2));
    assert_eq!(
        status(
            &a.own_device_list_update(&history(&[&v2]))
                .expect("same row")
        ),
        (2, 1)
    );
    assert_eq!(
        status(&a.own_device_list_update(&[0x80]).expect("empty history")),
        (2, 1)
    );
}

#[test]
fn an_enrolled_browser_pins_only_authenticated_discovery_for_its_instance() {
    let (a, _ac, rk_text, reg, put_v1) = signed_up();
    let m = material_of(&a, rk_text, reg, put_v1);
    let mut b = second();
    b.core
        .enrol_complete(&m.rk_text, &m.root, &m.state, &m.put_v1, "alice", LATER)
        .expect("complete the browser enrolment");

    assert_eq!(
        err(b.core.instance_signer_key_matches(&INSTANCE_SIGNER_PUB)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "instance signing key is not pinned".into()
        }
    );
    b.core
        .instance_signer_key_pin(&INSTANCE, &INSTANCE_SIGNER_PUB)
        .expect("pin after authenticated enrolled discovery");
    b.core
        .instance_signer_key_matches(&INSTANCE_SIGNER_PUB)
        .expect("the stored pin matches discovery");

    let mut wrong_instance = INSTANCE;
    wrong_instance[0] ^= 1;
    assert_eq!(
        err(b
            .core
            .instance_signer_key_pin(&wrong_instance, &INSTANCE_SIGNER_PUB)),
        ClientError {
            code: "E_CORE_STATE",
            detail: "authenticated instance discovery does not match identity".into()
        }
    );
    b.core
        .instance_signer_key_matches(&INSTANCE_SIGNER_PUB)
        .expect("a failed discovery does not change the stored pin");
}

#[test]
fn a_wrong_or_malformed_recovery_key_is_e_recovery_key_and_writes_nothing() {
    let m = material();
    let mut b = second();
    let before = snapshot(&b.c);
    let recovery_key = ClientError {
        code: "E_RECOVERY_KEY",
        detail: String::new(),
    };
    let other_key = recovery_key_base32(&[0x0b; 32]);
    let with_u = format!("U{}", &m.rk_text[1..]);
    for key in [
        other_key.as_str(),
        "not a recovery key",
        &m.rk_text[..51],
        with_u.as_str(),
        "",
    ] {
        assert_eq!(
            err(b
                .core
                .enrol_complete(key, &m.root, &m.state, &m.put_v1, "alice", LATER)),
            recovery_key,
            "key {key:?}"
        );
        assert_eq!(snapshot(&b.c), before, "a refused key writes nothing");
    }
    let plain = open_sealed(&k_header(&m.rk), b"dilla root v1", &m.root);
    let resealed = seal_with(&k_header(&[0x0b; 32]), b"dilla root v1", &plain);
    assert_eq!(
        err(b
            .core
            .enrol_complete(&m.rk_text, &resealed, &m.state, &m.put_v1, "alice", LATER)),
        recovery_key,
        "a root object sealed under another key"
    );
    let wrong_aad = seal_with(&k_header(&m.rk), b"dilla state v1", &plain);
    assert_eq!(
        err(b
            .core
            .enrol_complete(&m.rk_text, &wrong_aad, &m.state, &m.put_v1, "alice", LATER)),
        recovery_key,
        "a root object bound to another aad"
    );
    assert_eq!(snapshot(&b.c), before);
    assert_eq!(info(&b.core).phase, 3);
    b.core
        .enrol_complete(&m.rk_text, &m.root, &m.state, &m.put_v1, "alice", LATER)
        .expect("the right key still enrols after the refusals");
}

/// REGISTRATION-DEVICES-02: the form check the sign-in runs before it registers is enrol_complete's
/// step 1: the shown and the typed forms pass; a short key, a character outside the alphabet, a
/// last character with payload in its four zero bits, and nothing at all are E_RECOVERY_KEY.
#[test]
fn recovery_key_check_is_the_form_check_of_enrol_complete() {
    let rk_text = recovery_key_base32(&[0x0b; 32]);
    recovery_key_check(&rk_text).expect("the shown form");
    recovery_key_check(&as_typed(&rk_text)).expect("the typed form");
    let refused = ClientError {
        code: "E_RECOVERY_KEY",
        detail: String::new(),
    };
    let mut last = rk_text.clone();
    last.replace_range(51.., "Z");
    for key in [
        &rk_text[..51],
        &format!("U{}", &rk_text[1..]),
        last.as_str(),
        "not a recovery key",
        "",
    ] {
        assert_eq!(err(recovery_key_check(key)), refused, "key {key:?}");
    }
    // A well-formed key of another account passes the form check; only the root object tells.
    let m = material();
    recovery_key_check(&m.rk_text).expect("a key of the right form");
}

#[test]
fn enrol_complete_refuses_malformed_objects_and_lists_and_writes_nothing() {
    let m = material();
    let mut b = second();
    let before = snapshot(&b.c);
    let input = |detail: &str| ClientError {
        code: "E_CORE_INPUT",
        detail: detail.to_owned(),
    };
    let mut wrong_root = Encoder::new();
    wrong_root.array(3).uint(2).bytes(&m.umk).bytes(&m.ssk);
    let wrong_root = seal_with(&k_header(&m.rk), b"dilla root v1", &wrong_root.into_vec());
    let blob = m.v1.encode();
    // The state object is no refusal at version 1 (ruling 28 as amended): its cases are in
    // `an_unopenable_or_missing_state_object_yields_no_pins_and_the_enrolment_succeeds`, the
    // refusals at later versions in `enrol_accepts_a_missing_or_unreadable_state_only_at_version_1`.
    type RefusalCase = (Vec<u8>, Vec<u8>, Vec<u8>, ClientError);
    let cases: Vec<RefusalCase> = vec![
        (
            vec![0x01, 0x02],
            m.state.clone(),
            m.put_v1.clone(),
            input("root_sealed is malformed"),
        ),
        (
            wrong_root,
            m.state.clone(),
            m.put_v1.clone(),
            input("root object is malformed"),
        ),
        (
            m.root.clone(),
            m.state.clone(),
            vec![0xa0],
            input("list_body is malformed"),
        ),
        // outer version disagrees (3 against the blob's 1)
        (
            m.root.clone(),
            m.state.clone(),
            raw_put_body(3, &blob, &m.v1.sig_ssk, &[0; 32]),
            input("list_body elements disagree"),
        ),
        // outer prev_hash disagrees (0x01… against the blob's zero hash)
        (
            m.root.clone(),
            m.state.clone(),
            raw_put_body(1, &blob, &m.v1.sig_ssk, &[0x01; 32]),
            input("list_body elements disagree"),
        ),
        // outer ssk_signature disagrees (zeros against the blob's signature, which verifies)
        (
            m.root.clone(),
            m.state.clone(),
            raw_put_body(1, &blob, &[0; 64], &[0; 32]),
            input("list_body elements disagree"),
        ),
        (
            m.root.clone(),
            m.state.clone(),
            raw_put_body(1, &[0x01], &m.v1.sig_ssk, &[0; 32]),
            ClientError {
                code: "E_CREDENTIAL",
                detail: String::new(),
            },
        ),
        (
            m.root.clone(),
            m.state.clone(),
            put_body(&signed(
                &m.ssk,
                [0x43; 16],
                1,
                [0; 32],
                m.v1.unsigned.entries.clone(),
            )),
            ClientError {
                code: "E_CREDENTIAL",
                detail: String::new(),
            },
        ),
    ];
    for (n, (root, state, list, want)) in cases.into_iter().enumerate() {
        assert_eq!(
            err(b
                .core
                .enrol_complete(&m.rk_text, &root, &state, &list, "alice", LATER)),
            want,
            "case {n}"
        );
        assert_eq!(snapshot(&b.c), before, "case {n} wrote something");
    }
    assert_eq!(info(&b.core).phase, 3);
}

/// L-CORE-26 step 3 (ruling 28 as amended): with the served list at version 1, a state object that is
/// absent, malformed, sealed under another key or shaped wrongly is no refusal; the enrolment proceeds
/// with no pins and re-seals a sound object.
#[test]
fn an_unopenable_or_missing_state_object_yields_no_pins_and_the_enrolment_succeeds() {
    let m = material();
    let state_under_header = seal_with(
        &k_header(&m.rk),
        b"dilla state v1",
        &state_plain(&m.v1.encode(), &[0x80]),
    );
    let mut two = Encoder::new();
    two.array(2).uint(1).bytes(&m.v1.encode());
    let state_two = seal_with(&k_backup(&m.rk), b"dilla state v1", &two.into_vec());
    let state_pins_uint = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&m.v1.encode(), &[0x07]),
    );
    for (name, state_in) in [
        ("empty: the instance holds no state object", vec![]),
        ("not [1, nonce, ct]", vec![0x80]),
        ("sealed under K_header", state_under_header),
        ("a two-element plaintext", state_two),
        ("pins that are not an array", state_pins_uint),
    ] {
        let mut b = second();
        let out = b
            .core
            .enrol_complete(&m.rk_text, &m.root, &state_in, &m.put_v1, "alice", LATER)
            .unwrap_or_else(|e| panic!("{name}: {e:?}"));
        let (put_v2, state_b) = list_and_state(&out);
        let (version, blob_v2, _, prev_v2) = put_parts(&put_v2);
        assert_eq!((version, prev_v2), (2, m.v1.hash()), "{name}");
        let v2 = DeviceList::decode(&blob_v2).expect("v2");
        v2.accept(Some(&m.v1), &m.reg.ssk_pub)
            .expect("v2 chains from v1 under the account's SSK");
        assert_eq!(
            state_parts(&k_backup(&m.rk), &state_b),
            (blob_v2.clone(), vec![0x80]),
            "{name}: the re-sealed state opens under K_backup and carries the empty pins array"
        );
        assert_eq!(
            b.core.device_list_body().expect("candidate"),
            put_v2,
            "{name}: the candidate is the v2 PUT body"
        );
        assert_eq!(meta(&b.c, "state_sealed"), Some(state_b), "{name}");
        assert_eq!(info(&b.core).phase, 2, "{name}");
    }

    // An array of pins is carried byte for byte (any major-type-4 item is accepted).
    let one_pin = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&m.v1.encode(), &[0x81, 0x07]),
    );
    let mut b = second();
    let (put_v2, state_b) = list_and_state(
        &b.core
            .enrol_complete(&m.rk_text, &m.root, &one_pin, &m.put_v1, "alice", LATER)
            .expect("enrol with a one-element pins array"),
    );
    assert_eq!(
        state_parts(&k_backup(&m.rk), &state_b),
        (put_parts(&put_v2).1, vec![0x81, 0x07])
    );
}

/// Requirement 18, as amended by the core-block security review: step 3's tolerance holds for
/// device_list_revoke at version 1 only. At a later version a missing or unreadable state object is
/// refused, so a 404 during a revoke cannot overwrite the account's state with an empty pin table.
#[test]
fn device_list_revoke_with_no_state_object_reseals_with_no_pins_only_at_version_1() {
    let (mut a, ac, rk_text, reg, put_v1) = signed_up();
    let m = material_of(&a, rk_text, reg, put_v1);
    let own_id = m.v1.unsigned.entries[0].device_id;

    // Version 1: accepted, re-sealed with no pins.
    let (put_v2, state_a2) = list_and_state(
        &a.device_list_revoke(
            &m.rk_text,
            &m.root,
            &[],
            &m.put_v1,
            own_id.as_bytes(),
            NOW + 60,
        )
        .expect("an empty state_sealed is no refusal at version 1"),
    );
    let (version, blob_v2, _, prev_v2) = put_parts(&put_v2);
    assert_eq!((version, prev_v2), (2, m.v1.hash()));
    assert_eq!(
        state_parts(&k_backup(&m.rk), &state_a2),
        (blob_v2, vec![0x80]),
        "the re-sealed state carries the empty pins array"
    );
    assert_eq!(meta(&ac, "state_sealed"), Some(state_a2.clone()));
    assert_eq!(sealed_of(&a), (Some(m.root.clone()), Some(state_a2), 0));
    assert_eq!(a.device_list_body().expect("candidate"), put_v2);

    // Version 2: refused (changed: the test pinned a reseal with no pins, which the amendment refuses
    // here; the version-1 acceptance above is unchanged). B enrols from v1 and stores v2.
    let mut b = second();
    let (put_v2, _) = list_and_state(
        &b.core
            .enrol_complete(&m.rk_text, &m.root, &m.state, &m.put_v1, "alice", LATER)
            .expect("enrol"),
    );
    b.core.device_list_published().expect("published");
    let before = snapshot(&b.c);
    assert_eq!(
        err(b
            .core
            .device_list_revoke(&m.rk_text, &m.root, &[], &put_v2, own_id.as_bytes(), LATER)),
        core_input("the backup state is missing")
    );
    for (name, state_in) in unreadable_states(&m) {
        assert_eq!(
            err(b.core.device_list_revoke(
                &m.rk_text,
                &m.root,
                &state_in,
                &put_v2,
                own_id.as_bytes(),
                LATER
            )),
            core_input("the backup state could not be read"),
            "{name}"
        );
    }
    assert_eq!(snapshot(&b.c), before, "every refusal wrote nothing");
    let state_v2 = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&put_parts(&put_v2).1, &pins()),
    );
    let (_, state_out) = list_and_state(
        &b.core
            .device_list_revoke(
                &m.rk_text,
                &m.root,
                &state_v2,
                &put_v2,
                own_id.as_bytes(),
                LATER,
            )
            .expect("a readable state at v2 revokes"),
    );
    assert_eq!(
        state_parts(&k_backup(&m.rk), &state_out).1,
        pins(),
        "the pins are carried"
    );
}

fn core_input(detail: &str) -> ClientError {
    ClientError {
        code: "E_CORE_INPUT",
        detail: detail.to_owned(),
    }
}

/// State objects that exist but do not yield a state under K_backup.
fn unreadable_states(m: &Material) -> Vec<(&'static str, Vec<u8>)> {
    let mut two = Encoder::new();
    two.array(2).uint(1).bytes(&m.v1.encode());
    vec![
        ("not [1, nonce, ct]", vec![0x80]),
        (
            "sealed under K_header",
            seal_with(
                &k_header(&m.rk),
                b"dilla state v1",
                &state_plain(&m.v1.encode(), &[0x80]),
            ),
        ),
        (
            "sealed under another recovery key's K_backup",
            seal_with(
                &k_backup(&[0x0b; 32]),
                b"dilla state v1",
                &state_plain(&m.v1.encode(), &[0x80]),
            ),
        ),
        (
            "a two-element plaintext",
            seal_with(&k_backup(&m.rk), b"dilla state v1", &two.into_vec()),
        ),
        (
            "pins that are not an array",
            seal_with(
                &k_backup(&m.rk),
                b"dilla state v1",
                &state_plain(&m.v1.encode(), &[0x07]),
            ),
        ),
        (
            "an inner list that does not decode",
            seal_with(
                &k_backup(&m.rk),
                b"dilla state v1",
                &state_plain(&[0x01], &[0x80]),
            ),
        ),
    ]
}

/// v2 and v3 of `m`'s account: the signup device plus X, then X revoked.
fn chain_to_v3(m: &Material) -> (DeviceList, DeviceList) {
    let own = m.v1.unsigned.entries[0].clone();
    let v2 = signed(
        &m.ssk,
        USER,
        2,
        m.v1.hash(),
        vec![own.clone(), browser([0x0d; 16], [0x0e; 32], NOW + 10, None)],
    );
    let v3 = signed(
        &m.ssk,
        USER,
        3,
        v2.hash(),
        vec![
            own,
            browser([0x0d; 16], [0x0e; 32], NOW + 10, Some(NOW + 20)),
        ],
    );
    (v2, v3)
}

/// Ruling 28 as amended (core-block security review §1): at version 1 a missing or unreadable state
/// object enrols with no pins; at version 3 it is refused with the two details and writes nothing.
#[test]
fn enrol_accepts_a_missing_or_unreadable_state_only_at_version_1() {
    let m = material();
    let states: Vec<(&str, Vec<u8>)> = std::iter::once(("missing", vec![]))
        .chain(unreadable_states(&m))
        .collect();
    for (name, state_in) in &states {
        let mut b = second();
        let (put_v2, state_b) = list_and_state(
            &b.core
                .enrol_complete(&m.rk_text, &m.root, state_in, &m.put_v1, "alice", LATER)
                .unwrap_or_else(|e| panic!("version 1, {name}: {e:?}")),
        );
        assert_eq!(put_parts(&put_v2).0, 2, "{name}");
        assert_eq!(
            state_parts(&k_backup(&m.rk), &state_b).1,
            vec![0x80],
            "{name}: no pins"
        );
    }

    let (_, v3) = chain_to_v3(&m);
    let mut b = second();
    let before = snapshot(&b.c);
    for (name, state_in) in &states {
        let want = if state_in.is_empty() {
            core_input("the backup state is missing")
        } else {
            core_input("the backup state could not be read")
        };
        assert_eq!(
            err(b.core.enrol_complete(
                &m.rk_text,
                &m.root,
                state_in,
                &put_body(&v3),
                "alice",
                LATER
            )),
            want,
            "version 3, {name}"
        );
        assert_eq!(snapshot(&b.c), before, "version 3, {name} wrote something");
    }
    let state_v3 = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&v3.encode(), &pins()),
    );
    let (put_v4, state_b) = list_and_state(
        &b.core
            .enrol_complete(
                &m.rk_text,
                &m.root,
                &state_v3,
                &put_body(&v3),
                "alice",
                LATER,
            )
            .expect("a readable state at v3 enrols"),
    );
    assert_eq!(put_parts(&put_v4).0, 4);
    assert_eq!(state_parts(&k_backup(&m.rk), &state_b).1, pins());
}

/// The rollback probe: the real list is v3 (X revoked); a hostile instance serves the signed v2 (X
/// live) and withholds or spoils the state object, which would lift the floor. Signing v2 + 1 would
/// re-list X.
#[test]
fn enrol_refuses_an_older_list_served_with_the_state_object_withheld() {
    let m = material();
    let (v2, v3) = chain_to_v3(&m);
    let mut b = second();
    let before = snapshot(&b.c);
    let stale = put_body(&v2);
    let state_v3 = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&v3.encode(), &pins()),
    );
    for (state_in, served, want) in [
        (vec![], &stale, core_input("the backup state is missing")),
        (
            vec![0x80],
            &stale,
            core_input("the backup state could not be read"),
        ),
        // v1 served with the v3 state: two versions behind, not an interrupted publication
        // (BACKUPS-RECOVERY-02 accepts only the served list's direct successor).
        (
            state_v3,
            &m.put_v1,
            core_input("the instance served an older device list"),
        ),
    ] {
        assert_eq!(
            err(b
                .core
                .enrol_complete(&m.rk_text, &m.root, &state_in, served, "alice", LATER)),
            want
        );
        assert_eq!(snapshot(&b.c), before);
    }
}

/// BACKUPS-RECOVERY-02: a device PUT the state object of list v+1 and then lost the list PUT (a
/// failed sign-out, a revocation, a forgotten browser). The instance serves list v and that state.
/// The state's list is v + 1, chains from the served list and verifies under the recovered SSK, so
/// it is an interrupted publication: it is returned for the caller to publish first, and the new
/// device is signed into v + 2 on it. The rollback probe's X stays revoked: the base is v3.
#[test]
fn enrol_completes_an_interrupted_publication_and_signs_on_it() {
    let m = material();
    let (v2, v3) = chain_to_v3(&m);
    let state_v3 = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&v3.encode(), &pins()),
    );
    let mut b = second();
    let (put_v4, state_b, interrupted) = signed_lists(
        &b.core
            .enrol_complete(
                &m.rk_text,
                &m.root,
                &state_v3,
                &put_body(&v2),
                "alice",
                LATER,
            )
            .expect("an interrupted publication enrols"),
    );
    assert_eq!(
        interrupted,
        Some(put_body(&v3)),
        "v3 is returned to be published first"
    );
    let (version, blob_v4, _, prev) = put_parts(&put_v4);
    assert_eq!((version, prev), (4, v3.hash()));
    let v4 = DeviceList::decode(&blob_v4).expect("v4");
    v4.accept(Some(&v3), &m.reg.ssk_pub)
        .expect("v4 chains from v3");
    let mut want = v3.unsigned.entries.clone();
    want.push(browser(b.device, b.dsk, LATER, None));
    assert_eq!(
        v4.unsigned.entries, want,
        "X stays revoked; this browser is added"
    );
    assert_eq!(state_parts(&k_backup(&m.rk), &state_b), (blob_v4, pins()));
    let rec = identity_v2(&b.c);
    assert_eq!(rec.device_list, v3.encode(), "the accepted list is v3");
    assert_eq!((rec.published, rec.device_list_body), (0, put_v4));
}

/// A state object whose list only looks like the next one is not an interrupted publication: the
/// floor refuses it as an older served list, and nothing is written.
#[test]
fn enrol_refuses_a_forged_near_chain_in_the_state_object() {
    let m = material();
    let (v2, v3) = chain_to_v3(&m);
    let other_ssk = [0x5c; 32];
    let not_chained = signed(&m.ssk, USER, 3, [0x99; 32], v3.unsigned.entries.clone());
    let other_signer = signed(&other_ssk, USER, 3, v2.hash(), v3.unsigned.entries.clone());
    let other_user = signed(
        &m.ssk,
        [0x43; 16],
        3,
        v2.hash(),
        v3.unsigned.entries.clone(),
    );
    // Signed and naming the served list as its predecessor, but two versions on.
    let gap = signed(&m.ssk, USER, 4, v2.hash(), v3.unsigned.entries.clone());
    let mut b = second();
    let before = snapshot(&b.c);
    for (name, list) in [
        ("a prev_hash that is not the served list's", &not_chained),
        ("a list another key signed", &other_signer),
        ("a list of another user", &other_user),
        ("a version gap after the served list", &gap),
    ] {
        let state = seal_with(
            &k_backup(&m.rk),
            b"dilla state v1",
            &state_plain(&list.encode(), &pins()),
        );
        assert_eq!(
            err(b
                .core
                .enrol_complete(&m.rk_text, &m.root, &state, &put_body(&v2), "alice", LATER)),
            core_input("the instance served an older device list"),
            "{name}"
        );
        assert_eq!(snapshot(&b.c), before, "{name} wrote something");
    }
}

/// The same rule on a revocation: device A stores v1; the instance serves v1 and the state object of
/// an interrupted v2. A revokes on v2, gets v2 back to publish first, and signs v3; dropping the v3
/// candidate leaves v2, the accepted list, as the next candidate (BACKUPS-RECOVERY-02).
#[test]
fn revoke_completes_an_interrupted_publication_and_a_dropped_candidate_falls_back_to_it() {
    let (mut a, ac, rk_text, reg, put_v1) = signed_up();
    let m = material_of(&a, rk_text, reg, put_v1);
    let own = m.v1.unsigned.entries[0].clone();
    let x = browser([0x0d; 16], [0x0e; 32], NOW + 10, None);
    let v2 = signed(&m.ssk, USER, 2, m.v1.hash(), vec![own.clone(), x.clone()]);
    let state_v2 = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&v2.encode(), &pins()),
    );
    let (put_v3, state_a, interrupted) = signed_lists(
        &a.device_list_revoke(
            &m.rk_text,
            &m.root,
            &state_v2,
            &m.put_v1,
            &[0x0d; 16],
            NOW + 20,
        )
        .expect("revoke X on the interrupted v2"),
    );
    assert_eq!(interrupted, Some(put_body(&v2)));
    let v3 = DeviceList::decode(&put_parts(&put_v3).1).expect("v3");
    v3.accept(Some(&v2), &m.reg.ssk_pub)
        .expect("v3 chains from v2");
    assert_eq!(v3.unsigned.entries[1].revoked_at, Some(NOW + 20));
    assert_eq!(state_parts(&k_backup(&m.rk), &state_a).0, v3.encode());
    let (v, published, _) = own_list(&a);
    assert_eq!(
        (v, published),
        (2, 0),
        "the accepted list is the interrupted v2"
    );
    let before = identity_v2(&ac).device_list;
    a.device_list_drop().expect("drop the v3 candidate");
    assert_eq!(a.device_list_body().expect("candidate"), put_body(&v2));
    assert_eq!(info(&a).published, 0);
    assert_eq!(
        identity_v2(&ac).device_list,
        before,
        "the accepted list is unchanged"
    );
    a.device_list_published().expect("v2 published");
    let (v, published, _) = own_list(&a);
    assert_eq!((v, published), (2, 1));
}

/// BACKUPS-RECOVERY-03: a browser cannot open the stored state object (it holds no K_backup), so
/// the repair at each ready rests on what it knows: whether its own sealed state object carries the
/// list it accepted as the newest. Only then is a different stored object behind or junk. A store
/// written before the record existed counts as not current.
#[test]
fn state_sealed_current_says_whether_the_own_state_object_carries_the_accepted_list() {
    let (mut a, _ac, rk_text, reg, put_v1) = signed_up();
    assert!(a.state_sealed_current().expect("signup: v1 and its state"));
    let m = material_of(&a, rk_text, reg, put_v1);
    let mut b = second();
    assert_eq!(
        err(b.core.state_sealed_current()).code,
        "E_CORE_NO_IDENTITY"
    );
    let (put_v2, _) = list_and_state(
        &b.core
            .enrol_complete(&m.rk_text, &m.root, &m.state, &m.put_v1, "alice", LATER)
            .expect("enrol"),
    );
    assert!(
        !b.core.state_sealed_current().expect("enrolled"),
        "the state carries v2, the accepted list is v1 until the publication"
    );
    b.core.device_list_published().expect("published");
    assert!(b.core.state_sealed_current().expect("published"));
    let v2 = DeviceList::decode(&put_parts(&put_v2).1).expect("v2");
    a.own_device_list_update(&history(&[&v2]))
        .expect("A adopts v2");
    assert!(
        !a.state_sealed_current().expect("adopted"),
        "A's state carries v1"
    );
    b.c.lock()
        .expect("lock")
        .execute("DELETE FROM app_meta WHERE k = 'state_list'", [])
        .expect("delete");
    assert!(!b.core.state_sealed_current().expect("no record"));
}

/// A failed sign-out drops its self-revoking candidate (BACKUPS-RECOVERY-02): the device stays
/// listed, and its next publication is the accepted list again, which the instance already holds.
#[test]
fn device_list_drop_returns_the_candidate_to_the_accepted_list() {
    let (mut a, ac, rk_text, reg, put_v1) = signed_up();
    let m = material_of(&a, rk_text, reg, put_v1);
    let own_id = m.v1.unsigned.entries[0].device_id;
    let (put_v2, _) = list_and_state(
        &a.device_list_revoke(
            &m.rk_text,
            &m.root,
            &m.state,
            &m.put_v1,
            own_id.as_bytes(),
            LATER,
        )
        .expect("sign out"),
    );
    assert_eq!(a.device_list_body().expect("candidate"), put_v2);
    a.device_list_drop().expect("drop");
    assert_eq!(a.device_list_body().expect("candidate"), m.put_v1);
    let rec = identity_v2(&ac);
    assert_eq!((rec.published, rec.device_list), (0, m.v1.encode()));
    let (v, _, entries) = own_list(&a);
    assert_eq!((v, entries[0].4), (1, None), "still listed");
    let (mut b, _) = core();
    assert_eq!(err(b.device_list_drop()).code, "E_CORE_NO_IDENTITY");
}

/// Fix-wave review NEW-2: the dropped sign-out takes its self-revoking state object with it. The
/// state from before the sign-out comes back with its upload mark, so no later ready uploads a
/// state whose list revokes this device; a revocation of another device afterwards revokes that
/// device and keeps this one listed, and its publication deletes the kept state.
#[test]
fn a_failed_sign_out_drops_its_state_and_a_later_revocation_keeps_this_device_listed() {
    let (mut a, ac, rk_text, reg, put_v1) = signed_up();
    a.state_sealed_uploaded()
        .expect("the signup's state is uploaded");
    let m = material_of(&a, rk_text, reg, put_v1);
    let own = m.v1.unsigned.entries[0].clone();
    // X is listed at v2, which A published with its own state.
    let x = browser([0x0d; 16], [0x0e; 32], NOW + 10, None);
    let v2 = signed(&m.ssk, USER, 2, m.v1.hash(), vec![own.clone(), x.clone()]);
    let state_v2 = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&v2.encode(), &pins()),
    );
    a.own_device_list_update(&history(&[&v2]))
        .expect("A adopts v2");
    let before = sealed_of(&a);
    let current = a.state_sealed_current().expect("current");

    // The sign-out: its state object and its v3 that revokes A; the list PUT fails.
    let (put_out, state_out) = list_and_state(
        &a.device_list_revoke(
            &m.rk_text,
            &m.root,
            &state_v2,
            &put_body(&v2),
            own.device_id.as_bytes(),
            LATER,
        )
        .expect("sign out"),
    );
    assert_eq!(sealed_of(&a).1, Some(state_out.clone()));
    a.device_list_drop().expect("drop the sign-out");
    assert_eq!(a.device_list_body().expect("candidate"), put_body(&v2));
    assert_eq!(
        sealed_of(&a),
        before,
        "the state, and its upload mark, from before the sign-out"
    );
    assert_eq!(a.state_sealed_current().expect("current"), current);
    assert!(
        meta(&ac, "state_prior").is_none(),
        "nothing is kept after the drop"
    );
    let v3_out = DeviceList::decode(&put_parts(&put_out).1).expect("v3");
    assert_eq!(v3_out.unsigned.entries[0].revoked_at, Some(LATER));

    // The instance kept v2 and its state (the sign-out's state PUT was refused): revoking X signs v3
    // on v2 with X revoked and A listed, and no interrupted publication.
    let (put_v3, state_v3) = list_and_state(
        &a.device_list_revoke(
            &m.rk_text,
            &m.root,
            &state_v2,
            &put_body(&v2),
            &[0x0d; 16],
            LATER + 5,
        )
        .expect("revoke X"),
    );
    let v3 = DeviceList::decode(&put_parts(&put_v3).1).expect("v3");
    v3.accept(Some(&v2), &m.reg.ssk_pub)
        .expect("v3 chains from v2");
    assert_eq!(v3.unsigned.entries[0].revoked_at, None, "A stays listed");
    assert_eq!(
        v3.unsigned.entries[1].revoked_at,
        Some(LATER + 5),
        "X is revoked"
    );
    assert!(
        meta(&ac, "state_prior").is_some(),
        "kept while v3 is unpublished"
    );
    a.device_list_published().expect("v3 published");
    assert!(
        meta(&ac, "state_prior").is_none(),
        "a published candidate keeps nothing"
    );
    assert_eq!(sealed_of(&a).1, Some(state_v3), "v3's state stays");
}

/// A candidate the chain overtook goes with its state (fix-wave review NEW-2), while a candidate the
/// instance holds (a lost answer) keeps its own.
#[test]
fn an_overtaken_candidate_drops_its_state_and_a_published_one_keeps_it() {
    let (mut a, ac, rk_text, reg, put_v1) = signed_up();
    a.state_sealed_uploaded().expect("uploaded");
    let m = material_of(&a, rk_text, reg, put_v1);
    let own = m.v1.unsigned.entries[0].clone();
    let x = browser([0x0d; 16], [0x0e; 32], NOW + 10, None);
    let y = browser([0x0f; 16], [0x10; 32], NOW + 11, None);
    let v2 = signed(
        &m.ssk,
        USER,
        2,
        m.v1.hash(),
        vec![own.clone(), x.clone(), y.clone()],
    );
    let state_v2 = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&v2.encode(), &pins()),
    );
    a.own_device_list_update(&history(&[&v2]))
        .expect("A adopts v2");
    let before = sealed_of(&a);
    // A revokes X; another device publishes v3 revoking Y first: A's candidate is overtaken.
    let (put_x, _) = list_and_state(
        &a.device_list_revoke(
            &m.rk_text,
            &m.root,
            &state_v2,
            &put_body(&v2),
            &[0x0d; 16],
            LATER,
        )
        .expect("revoke X"),
    );
    let mut other = v2.unsigned.entries.clone();
    other[2].revoked_at = Some(LATER);
    let v3 = signed(&m.ssk, USER, 3, v2.hash(), other);
    a.own_device_list_update(&history(&[&v3]))
        .expect("A adopts the other v3");
    assert_eq!(
        sealed_of(&a),
        before,
        "the overtaken candidate's state is dropped"
    );
    assert!(meta(&ac, "state_prior").is_none());

    // A lost answer: A revokes X on v3, the instance stored it, and the history shows it.
    let state_v3 = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&v3.encode(), &pins()),
    );
    let (put_v4, state_v4) = list_and_state(
        &a.device_list_revoke(
            &m.rk_text,
            &m.root,
            &state_v3,
            &put_body(&v3),
            &[0x0d; 16],
            LATER + 1,
        )
        .expect("revoke X on v3"),
    );
    assert_ne!(put_v4, put_x);
    let v4 = DeviceList::decode(&put_parts(&put_v4).1).expect("v4");
    a.own_device_list_update(&history(&[&v4]))
        .expect("A finds its own v4");
    assert_eq!(
        sealed_of(&a),
        (before.0, Some(state_v4), 0),
        "v4's state stays, to be uploaded"
    );
    assert!(meta(&ac, "state_prior").is_none());
}

/// F2: an entry with this device id refuses the enrolment whether it is live, revoked or under
/// another key, so the id is never listed twice (lookup would find the stale entry first).
#[test]
fn enrol_complete_refuses_a_list_that_names_this_device_id_in_any_entry() {
    let m = material();
    let mut b = second();
    let before = snapshot(&b.c);
    for (name, entry) in [
        ("revoked", browser(b.device, b.dsk, NOW, Some(NOW + 1))),
        (
            "under another key",
            browser(b.device, [0x0e; 32], NOW, None),
        ),
        (
            "revoked under another key",
            browser(b.device, [0x0e; 32], NOW, Some(NOW + 1)),
        ),
    ] {
        let listed = signed(
            &m.ssk,
            USER,
            2,
            m.v1.hash(),
            vec![m.v1.unsigned.entries[0].clone(), entry],
        );
        assert_eq!(
            err(b.core.enrol_complete(
                &m.rk_text,
                &m.root,
                &m.state,
                &put_body(&listed),
                "alice",
                LATER
            )),
            ClientError {
                code: "E_CORE_STATE",
                detail: "device is listed".into()
            },
            "{name}"
        );
        assert_eq!(snapshot(&b.c), before, "{name}");
    }
}

/// F6: at the stored version the served list must be the stored list byte for byte; a fork the
/// account's SSK signed at that version is refused rather than taken as the base.
#[test]
fn revoke_refuses_a_different_list_at_the_stored_version() {
    let (mut a, ac, rk_text, reg, put_v1) = signed_up();
    let m = material_of(&a, rk_text, reg, put_v1);
    let (v2, _) = chain_to_v3(&m);
    a.own_device_list_update(&history(&[&v2]))
        .expect("adopt v2");
    let fork = signed(
        &m.ssk,
        USER,
        2,
        m.v1.hash(),
        vec![
            m.v1.unsigned.entries[0].clone(),
            browser([0x0f; 16], [0x0e; 32], NOW + 10, None),
        ],
    );
    let state_of = |l: &DeviceList| {
        seal_with(
            &k_backup(&m.rk),
            b"dilla state v1",
            &state_plain(&l.encode(), &pins()),
        )
    };
    let before = snapshot(&ac);
    assert_eq!(
        err(a.device_list_revoke(
            &m.rk_text,
            &m.root,
            &state_of(&fork),
            &put_body(&fork),
            &m.reg.device_id,
            LATER
        )),
        core_input("the instance served a different device list at the stored version")
    );
    assert_eq!(snapshot(&ac), before);
    a.device_list_revoke(
        &m.rk_text,
        &m.root,
        &state_of(&v2),
        &put_body(&v2),
        &m.reg.device_id,
        LATER,
    )
    .expect("the stored list itself is a base");
}

/// F3: revoke compares the keys the root object yields with the identity record. Another account's
/// root, state and a list its own SSK signed for the same user id are all consistent with each other,
/// so only that comparison refuses them, before the state object is read.
#[test]
fn revoke_refuses_another_accounts_root_even_when_its_list_and_state_agree() {
    let (mut a, ac, _, _, _) = signed_up();
    let other = material();
    let other_v2 = signed(
        &other.ssk,
        USER,
        2,
        other.v1.hash(),
        other.v1.unsigned.entries.clone(),
    );
    let other_id = other.v1.unsigned.entries[0].device_id;
    let before = snapshot(&ac);
    for (name, state_in) in [("its state", other.state.clone()), ("no state", vec![])] {
        assert_eq!(
            err(a.device_list_revoke(
                &other.rk_text,
                &other.root,
                &state_in,
                &put_body(&other_v2),
                other_id.as_bytes(),
                LATER
            )),
            ClientError {
                code: "E_CREDENTIAL",
                detail: String::new()
            },
            "{name}"
        );
        assert_eq!(snapshot(&ac), before, "{name}");
    }
}

/// F5: enrolment applies the first-sight rules of `accept(None)` to the served list: version 0 is no
/// version and only version 1 chains from 32 zero bytes.
#[test]
fn enrol_refuses_a_served_list_with_no_version_or_a_v1_with_a_prev_hash() {
    let m = material();
    let mut b = second();
    let before = snapshot(&b.c);
    for (name, list) in [
        (
            "version 0",
            signed(&m.ssk, USER, 0, [0; 32], m.v1.unsigned.entries.clone()),
        ),
        (
            "version 1 with a non-zero prev_hash",
            signed(&m.ssk, USER, 1, [0x01; 32], m.v1.unsigned.entries.clone()),
        ),
    ] {
        assert_eq!(
            err(b.core.enrol_complete(
                &m.rk_text,
                &m.root,
                &m.state,
                &put_body(&list),
                "alice",
                LATER
            )),
            ClientError {
                code: "E_DEVICE_LIST_STALE",
                detail: String::new()
            },
            "{name}"
        );
        assert_eq!(snapshot(&b.c), before, "{name}");
    }
}

#[test]
fn enrol_complete_refuses_a_list_the_ssk_did_not_sign() {
    let m = material();
    let mut b = second();
    let before = snapshot(&b.c);
    let forged = signed(&[0x99; 32], USER, 1, [0; 32], m.v1.unsigned.entries.clone());
    let mut flipped = m.v1.clone();
    flipped.sig_ssk[0] ^= 0x01;
    for (name, list) in [("another SSK", forged), ("a flipped signature", flipped)] {
        assert_eq!(
            err(b.core.enrol_complete(
                &m.rk_text,
                &m.root,
                &m.state,
                &put_body(&list),
                "alice",
                LATER
            )),
            ClientError {
                code: "E_CREDENTIAL",
                detail: String::new()
            },
            "{name}"
        );
        assert_eq!(snapshot(&b.c), before);
    }
    assert_eq!(info(&b.core).phase, 3);
}

#[test]
fn enrol_complete_refuses_a_list_that_already_names_this_device() {
    let m = material();
    let mut b = second();
    let before = snapshot(&b.c);
    let listed = signed(
        &m.ssk,
        USER,
        2,
        m.v1.hash(),
        vec![
            m.v1.unsigned.entries[0].clone(),
            browser(b.device, b.dsk, NOW, None),
        ],
    );
    assert_eq!(
        err(b.core.enrol_complete(
            &m.rk_text,
            &m.root,
            &m.state,
            &put_body(&listed),
            "alice",
            LATER
        )),
        ClientError {
            code: "E_CORE_STATE",
            detail: "device is listed".into()
        }
    );
    assert_eq!(snapshot(&b.c), before);
}

#[test]
fn enrol_reset_returns_to_phase_0_and_deletes_the_device_key() {
    let mut b = second();
    b.core
        .session_store("pending-token", 10, 10)
        .expect("store");
    b.core.enrol_reset().expect("enrol_reset");
    let i = info(&b.core);
    assert_eq!(
        (i.phase, i.instance, i.user, i.device),
        (0, None, None, None)
    );
    assert!(meta(&b.c, "enrol").is_none());
    assert!(
        meta(&b.c, "session").is_none(),
        "the pending session record is gone"
    );
    assert_eq!(keys(&b.c), 0, "the DSK is deleted");
    assert_eq!(
        err(b.core.session_sign(&[1; 32], 0)).code,
        "E_CORE_NO_IDENTITY"
    );
    assert_eq!(
        err(b.core.enrol_reset()),
        ClientError {
            code: "E_CORE_STATE",
            detail: "no enrolment is pending".into()
        }
    );
    let (again, _) = begin_enrol(&mut b.core);
    assert_ne!(again, b.device, "a new enrolment draws a new device id");
}

#[test]
fn device_list_revoke_signs_the_next_version_with_the_devices_revoked() {
    let (mut a, ac, rk_text, reg, put_v1) = signed_up();
    let m = material_of(&a, rk_text, reg, put_v1);
    let state_in = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&m.v1.encode(), &pins()),
    );
    let mut b = second();
    let (put_v2, state_b) = list_and_state(
        &b.core
            .enrol_complete(&m.rk_text, &m.root, &state_in, &m.put_v1, "alice", LATER)
            .expect("enrol"),
    );
    b.core.device_list_published().expect("published");
    let v2 = DeviceList::decode(&put_parts(&put_v2).1).expect("v2");
    a.own_device_list_update(&history(&[&v2]))
        .expect("A adopts v2");

    let before = snapshot(&ac);
    let input = |detail: &str| ClientError {
        code: "E_CORE_INPUT",
        detail: detail.to_owned(),
    };
    let shape = input("device_ids must be 1..=64 ids of 16 bytes");
    for ids in [vec![], vec![0x77; 15], vec![0x77; 17], vec![0x77; 16 * 65]] {
        assert_eq!(
            err(a.device_list_revoke(&m.rk_text, &m.root, &state_b, &put_v2, &ids, NOW)),
            shape
        );
    }
    let twice = [b.device, b.device].concat();
    assert_eq!(
        err(a.device_list_revoke(&m.rk_text, &m.root, &state_b, &put_v2, &twice, NOW)),
        input("device_ids repeats an id")
    );
    assert_eq!(
        err(a.device_list_revoke(&m.rk_text, &m.root, &state_b, &put_v2, &[0x77; 16], NOW)),
        ClientError {
            code: "E_CORE_NOT_FOUND",
            detail: "device is not an unrevoked entry of the list".into()
        }
    );
    assert_eq!(
        err(a.device_list_revoke(
            &recovery_key_base32(&[0x0b; 32]),
            &m.root,
            &state_b,
            &put_v2,
            &b.device,
            NOW
        ))
        .code,
        "E_RECOVERY_KEY"
    );
    let for_other = put_body(&signed(
        &m.ssk,
        [0x43; 16],
        3,
        v2.hash(),
        v2.unsigned.entries.clone(),
    ));
    assert_eq!(
        err(a.device_list_revoke(&m.rk_text, &m.root, &state_b, &for_other, &b.device, NOW)).code,
        "E_CREDENTIAL"
    );
    let c_side = material();
    assert_eq!(
        err(a.device_list_revoke(
            &c_side.rk_text,
            &c_side.root,
            &c_side.state,
            &put_v2,
            &b.device,
            NOW
        )),
        ClientError {
            code: "E_CREDENTIAL",
            detail: String::new()
        },
        "another account's root object"
    );
    assert_eq!(snapshot(&ac), before, "every refusal wrote nothing");

    let (put_v3, state_a3) = list_and_state(
        &a.device_list_revoke(
            &as_typed(&m.rk_text),
            &m.root,
            &state_b,
            &put_v2,
            &b.device,
            NOW + 120,
        )
        .expect("revoke B"),
    );
    let (version, blob_v3, _, prev_v3) = put_parts(&put_v3);
    let v3 = DeviceList::decode(&blob_v3).expect("v3");
    assert_eq!((version, prev_v3), (3, v2.hash()));
    assert_eq!(
        v3.unsigned.entries,
        vec![
            v2.unsigned.entries[0].clone(),
            browser(b.device, b.dsk, LATER, Some(NOW + 120))
        ]
    );
    v3.accept(Some(&v2), &m.reg.ssk_pub)
        .expect("v3 chains from v2");
    let (inner, carried) = state_parts(&k_backup(&m.rk), &state_a3);
    assert_eq!((inner, carried), (blob_v3.clone(), pins()));
    assert_eq!(a.device_list_body().expect("candidate"), put_v3);
    assert_eq!(info(&a).published, 0);
    let (v, published, _) = own_list(&a);
    assert_eq!((v, published), (2, 0), "the accepted list is still v2");
    assert_eq!(
        sealed_of(&a),
        (Some(m.root.clone()), Some(state_a3.clone()), 0)
    );
    a.device_list_published().expect("published");
    let (v, published, entries) = own_list(&a);
    assert_eq!((v, published), (3, 1));
    assert_eq!(entries[1].4, Some(NOW + 120), "B is revoked");

    assert_eq!(
        status(
            &b.core
                .own_device_list_update(&history(&[&v3]))
                .expect("B adopts v3")
        ),
        (3, 0),
        "B learns it is no longer listed"
    );
    assert_eq!(
        err(a.device_list_revoke(&m.rk_text, &m.root, &state_a3, &put_v3, &b.device, NOW)),
        ClientError {
            code: "E_CORE_NOT_FOUND",
            detail: "device is not an unrevoked entry of the list".into()
        },
        "an already revoked device"
    );
    let own = put_parts(&m.put_v1).1;
    let own_id = DeviceList::decode(&own).expect("v1").unsigned.entries[0].device_id;
    let (put_v4, _) = list_and_state(
        &a.device_list_revoke(
            &m.rk_text,
            &m.root,
            &state_a3,
            &put_v3,
            own_id.as_bytes(),
            NOW + 180,
        )
        .expect("Q13: a device may revoke itself"),
    );
    let v4 = DeviceList::decode(&put_parts(&put_v4).1).expect("v4");
    assert_eq!(v4.unsigned.entries[0].revoked_at, Some(NOW + 180));
}

#[test]
fn own_device_list_update_walks_the_chain_and_refuses_a_gap() {
    let (mut a, ac, rk_text, reg, put_v1) = signed_up();
    let m = material_of(&a, rk_text, reg, put_v1);
    let own = m.v1.unsigned.entries[0].clone();
    assert_eq!(own.device_id, DeviceId::from_bytes(m.reg.device_id));
    let x = browser([0x0d; 16], [0x0e; 32], NOW + 10, None);
    let x_gone = browser([0x0d; 16], [0x0e; 32], NOW + 10, Some(NOW + 20));
    let v2 = signed(&m.ssk, USER, 2, m.v1.hash(), vec![own.clone(), x]);
    let v3 = signed(
        &m.ssk,
        USER,
        3,
        v2.hash(),
        vec![own.clone(), x_gone.clone()],
    );
    let v4 = signed(
        &m.ssk,
        USER,
        4,
        v3.hash(),
        vec![own.clone(), x_gone.clone()],
    );
    let v5 = signed(
        &m.ssk,
        USER,
        5,
        v4.hash(),
        vec![own.clone(), x_gone.clone()],
    );

    assert_eq!(
        status(
            &a.own_device_list_update(&history(&[&v2, &v3]))
                .expect("v2, v3")
        ),
        (3, 1)
    );
    assert_eq!(own_list(&a).0, 3);
    let before = snapshot(&ac);
    let stale = ClientError {
        code: "E_DEVICE_LIST_STALE",
        detail: String::new(),
    };
    assert_eq!(
        err(a.own_device_list_update(&history(&[&v5]))),
        stale,
        "a gap"
    );
    assert_eq!(
        err(a.own_device_list_update(&history(&[&v2]))),
        stale,
        "a lower version"
    );
    let forged5 = signed(&[0x99; 32], USER, 5, v4.hash(), vec![own.clone()]);
    assert_eq!(
        err(a.own_device_list_update(&history(&[&v4, &forged5]))).code,
        "E_CREDENTIAL"
    );
    assert_eq!(
        snapshot(&ac),
        before,
        "v4 is not kept when a later row is refused"
    );
    let mut outer = Encoder::new();
    outer
        .array(1)
        .array(4)
        .uint(5)
        .bytes(&v4.encode())
        .bytes(&v4.sig_ssk)
        .bytes(&v4.unsigned.prev_hash);
    assert_eq!(
        err(a.own_device_list_update(&outer.into_vec())),
        ClientError {
            code: "E_CORE_INPUT",
            detail: "history row elements disagree".into()
        }
    );
    let other_user = signed(&m.ssk, [0x43; 16], 4, v3.hash(), vec![own.clone()]);
    assert_eq!(
        err(a.own_device_list_update(&history(&[&other_user]))).code,
        "E_CREDENTIAL"
    );
    assert_eq!(
        err(a.own_device_list_update(&[0xa0])),
        ClientError {
            code: "E_CORE_INPUT",
            detail: "history_body is malformed".into()
        }
    );
    let mut junk = Encoder::new();
    junk.array(1)
        .array(4)
        .uint(4)
        .bytes(&[0x01, 0x02])
        .bytes(&v4.sig_ssk)
        .bytes(&v4.unsigned.prev_hash);
    assert_eq!(
        err(a.own_device_list_update(&junk.into_vec())).code,
        "E_CREDENTIAL"
    );
    assert_eq!(snapshot(&ac), before, "no refusal wrote anything");

    assert_eq!(
        status(
            &a.own_device_list_update(&history(&[&v3, &v4, &v5]))
                .expect("v3 skipped, v4, v5")
        ),
        (5, 1)
    );
    let gone = DeviceEntry {
        revoked_at: Some(NOW + 30),
        ..own.clone()
    };
    let v6 = signed(&m.ssk, USER, 6, v5.hash(), vec![gone, x_gone]);
    assert_eq!(
        status(&a.own_device_list_update(&history(&[&v6])).expect("v6")),
        (6, 0),
        "a list that revokes this device says so"
    );
}

#[test]
fn own_device_list_update_drops_a_candidate_the_chain_overtook() {
    let (mut a, _ac) = core();
    let rk_text = a.signup_begin(&INSTANCE).expect("signup_begin");
    let reg = registration(&a.signup_request("i", "alice", "A", None).expect("request"));
    let put_v1 = a
        .signup_complete(&USER, "alice", NOW, &INSTANCE_SIGNER_PUB)
        .expect("complete");
    let m = material_of(&a, rk_text, reg, put_v1);
    assert_eq!(
        info(&a).published,
        0,
        "v1 is a candidate the instance has not accepted"
    );
    let own = m.v1.unsigned.entries[0].clone();
    let x = browser([0x0d; 16], [0x0e; 32], NOW + 10, None);
    let v2 = signed(&m.ssk, USER, 2, m.v1.hash(), vec![own.clone(), x.clone()]);
    assert_eq!(
        status(&a.own_device_list_update(&history(&[&v2])).expect("v2")),
        (2, 1)
    );
    assert_eq!(
        a.device_list_body().expect("body"),
        put_body(&v2),
        "candidate v1 dropped"
    );
    assert_eq!(info(&a).published, 1);

    let (put_v3, _) = list_and_state(
        &a.device_list_revoke(
            &m.rk_text,
            &m.root,
            &m.state,
            &put_body(&v2),
            &[0x0d; 16],
            NOW + 40,
        )
        .expect("revoke X"),
    );
    assert_eq!(info(&a).published, 0);
    assert_eq!(
        status(
            &a.own_device_list_update(&history(&[&v2]))
                .expect("v2 again")
        ),
        (2, 1)
    );
    assert_eq!(
        (a.device_list_body().expect("body"), info(&a).published),
        (put_v3.clone(), 0),
        "a skipped row adopts nothing, so the newer candidate stays"
    );
    let y = browser([0x0f; 16], [0x10; 32], NOW + 50, None);
    let v3_elsewhere = signed(&m.ssk, USER, 3, v2.hash(), vec![own, x, y]);
    assert_eq!(
        status(
            &a.own_device_list_update(&history(&[&v3_elsewhere]))
                .expect("v3 from another device")
        ),
        (3, 1)
    );
    assert_eq!(
        (a.device_list_body().expect("body"), info(&a).published),
        (put_body(&v3_elsewhere), 1),
        "the candidate of the same version is dropped"
    );
}

#[test]
fn no_recovered_secret_rests_in_the_store_after_enrol_complete() {
    let m = material();
    let mut b = second();
    b.core
        .enrol_complete(&m.rk_text, &m.root, &m.state, &m.put_v1, "alice", LATER)
        .expect("enrol");
    let kh = k_header(&m.rk);
    let kb = k_backup(&m.rk);
    let root_plain = open_sealed(&kh, b"dilla root v1", &m.root);
    for (table, cell) in cells(&b.c) {
        for (name, secret) in [
            ("UMK_priv", &m.umk[..]),
            ("SSK_priv", &m.ssk[..]),
            ("the recovery key", &m.rk[..]),
            ("K_header", &kh[..]),
            ("K_backup", &kb[..]),
            ("the root plaintext", &root_plain[..]),
        ] {
            assert!(!contains(&cell, secret), "{name} found in {table}");
        }
    }
}

#[test]
fn the_enrol_record_and_the_recovered_keys_leave_no_trace_in_the_database_file() {
    let m = material();
    let dir = std::env::temp_dir().join(format!("dilla-client-enrol-{}", std::process::id()));
    std::fs::create_dir_all(&dir).expect("tmp dir");
    let path = dir.join("secure-delete-enrol.sqlite");
    let _ = std::fs::remove_file(&path);
    let c: ConnHandle = Arc::new(Mutex::new(
        rusqlite::Connection::open(&path).expect("open file"),
    ));
    let mut core = ClientCore::open(c.clone()).expect("open");
    begin_enrol(&mut core);
    let unregistered = meta(&c, "enrol").expect("enrol record");
    core.enrol_registered(&USER).expect("registered");
    let registered = meta(&c, "enrol").expect("enrol record");
    let before = std::fs::read(&path).expect("read db");
    assert!(
        contains(&before, &registered),
        "the scan sees the live enrol record"
    );
    core.enrol_complete(&m.rk_text, &m.root, &m.state, &m.put_v1, "alice", LATER)
        .expect("enrol");
    drop(core);
    drop(c);
    let after = std::fs::read(&path).expect("read db");
    let kh = k_header(&m.rk);
    let kb = k_backup(&m.rk);
    let root_plain = open_sealed(&kh, b"dilla root v1", &m.root);
    for (name, secret) in [
        ("the first enrol record", &unregistered[..]),
        ("the second enrol record", &registered[..]),
        ("UMK_priv", &m.umk[..]),
        ("SSK_priv", &m.ssk[..]),
        ("the 32 decoded recovery-key bytes", &m.rk[..]),
        ("the recovery key as text", m.rk_text.as_bytes()),
        ("K_header", &kh[..]),
        ("K_backup", &kb[..]),
        ("the root plaintext", &root_plain[..]),
    ] {
        assert!(
            !contains(&after, secret),
            "{name} left in the database file"
        );
    }
    std::fs::remove_file(&path).expect("cleanup");
}

#[test]
fn an_enrolled_browser_joins_a_group_of_its_account_and_both_read_each_other() {
    let instance = Instance::generate();
    let group = client_support::GROUP;
    let mut relay = Relay::new(group);
    let (mut first, rk_text) = ready_core_with_key(0xa1, "alice");
    let mut added = enrolled_core(&first, &rk_text, LATER);
    assert_eq!(added.user, first.user);
    assert_ne!(added.device, first.device);
    first.create_and_register(&mut relay, &instance);
    added.join_external(&mut relay);
    first.sync(&relay);
    let first_body = first.core.device_list_body().expect("first signed list");
    let added_body = added.core.device_list_body().expect("enrolled signed list");
    ingest_enrolled_pair_bodies(
        &mut first,
        &relay,
        added.user,
        added.device,
        &first_body,
        &added_body,
    );
    ingest_list_body(&mut added, &relay, first.user, first.device, &first_body);
    added.send(&mut relay, &group, "from the second browser", LATER + 1);
    first.send(&mut relay, &group, "from the first browser", LATER + 2);
    first.sync(&relay);
    added.sync(&relay);
    for (reader, author, body) in [
        (&first, &added, "from the second browser"),
        (&added, &first, "from the first browser"),
    ] {
        let row = reader
            .timeline(&group)
            .into_iter()
            .find(|r| r.body == body)
            .expect("the other browser's message is in the timeline");
        assert_eq!(
            (
                row.status,
                row.sender_user,
                row.sender_device,
                row.sender_tier
            ),
            (0, Some(author.user), author.device, Some(1))
        );
    }
}

// A served list older than the opened state list can re-list a revoked device when signed at old+1.
#[test]
fn enrol_refuses_a_served_list_older_than_the_opened_state_list() {
    let m = material();
    let mut b = second();
    let v2 = signed(&m.ssk, USER, 2, m.v1.hash(), m.v1.unsigned.entries.clone());
    // v3, not v2: a chained v2 with v1 served is an interrupted publication (BACKUPS-RECOVERY-02).
    let v3 = signed(&m.ssk, USER, 3, v2.hash(), m.v1.unsigned.entries.clone());
    let state = seal_with(
        &k_backup(&m.rk),
        b"dilla state v1",
        &state_plain(&v3.encode(), &pins()),
    );
    let before = snapshot(&b.c);
    assert_eq!(
        err(b
            .core
            .enrol_complete(&m.rk_text, &m.root, &state, &m.put_v1, "alice", LATER)),
        ClientError {
            code: "E_CORE_INPUT",
            detail: "the instance served an older device list".into()
        }
    );
    assert_eq!(snapshot(&b.c), before);
}

// A served list older than the stored newest can re-list a revoked device when signed at old+1.
#[test]
fn revoke_refuses_a_served_list_older_than_the_stored_newest() {
    let (mut a, ac, rk_text, reg, put_v1) = signed_up();
    let m = material_of(&a, rk_text, reg, put_v1);
    let v2 = signed(&m.ssk, USER, 2, m.v1.hash(), m.v1.unsigned.entries.clone());
    a.own_device_list_update(&history(&[&v2]))
        .expect("adopt v2");
    let before = snapshot(&ac);
    assert_eq!(
        err(a.device_list_revoke(
            &m.rk_text,
            &m.root,
            &m.state,
            &m.put_v1,
            &m.reg.device_id,
            LATER
        )),
        ClientError {
            code: "E_CORE_INPUT",
            detail: "the instance served an older device list".into()
        }
    );
    assert_eq!(snapshot(&ac), before);
}

// ---- Task S1: account_kind_ingest verifies before it stores (Ruling AW: no key) ----

/// The instance signer of the support fixtures: `INSTANCE_SIGNER_PUB` is the public half of seed 0x41.
fn instance_signer() -> ed25519_dalek::SigningKey {
    let key = ed25519_dalek::SigningKey::from_bytes(&[0x41; 32]);
    assert_eq!(key.verifying_key().to_bytes(), INSTANCE_SIGNER_PUB);
    key
}

fn signed_attestation(instance: &[u8; 16], user: &[u8; 16], kind: u8) -> Vec<u8> {
    use ed25519_dalek::Signer;
    let message = dilla_core::identity::account_kind_message(instance, user, kind);
    let sig = instance_signer().sign(&message).to_bytes();
    dilla_core::identity::AccountKindAttestation {
        key_id: [0x53; 16],
        kind,
        signature: sig,
    }
    .encode()
}

fn raw_attestation(v: u64, kind: u64) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(4)
        .uint(v)
        .bytes(&[0x53; 16])
        .uint(kind)
        .bytes(&[0x54; 64]);
    e.into_vec()
}

fn stored_signature(c: &ConnHandle, user: &[u8; 16]) -> Option<Vec<u8>> {
    c.lock()
        .expect("lock")
        .query_row(
            "SELECT signature FROM app_account_kinds WHERE user_id=?1",
            [user.as_slice()],
            |r| r.get(0),
        )
        .ok()
}

#[test]
fn account_kind_ingest_needs_the_pinned_instance_key() {
    let (mut fresh, _) = core();
    assert_eq!(
        err(fresh.account_kind_ingest(&USER, &raw_attestation(1, 0))).code,
        "E_CORE_STATE"
    );
    assert!(!fresh.account_kind_seen().expect("seen"));
}

#[test]
fn account_kind_ingest_verifies_before_it_stores_and_sets_seen_only_then() {
    let (mut core, c, _, _, _) = signed_up();
    let other = [0x77; 16];
    // Malformed, or well formed but not the instance's statement about this user: refused
    // (E_CORE_INPUT), nothing stored, no flag.
    for bad in [
        raw_attestation(2, 0),
        raw_attestation(1, 2),
        raw_attestation(1, 0),
        signed_attestation(&[0x99; 16], &other, 0),
        signed_attestation(&INSTANCE, &USER, 0),
    ] {
        assert_eq!(
            err(core.account_kind_ingest(&other, &bad)).code,
            "E_CORE_INPUT"
        );
    }
    assert_eq!(
        err(core.account_kind_ingest(&[0; 16], &raw_attestation(1, 0))).code,
        "E_CORE_INPUT"
    );
    assert!(
        !core.account_kind_seen().expect("seen"),
        "a refused attestation sets nothing"
    );
    assert_eq!(stored_signature(&c, &other), None);

    // The instance's statement about this user (no pin needed: it binds no key): stored, flag set,
    // generation bumped once.
    let before = core.trust_generation().expect("generation");
    let good = signed_attestation(&INSTANCE, &other, 0);
    core.account_kind_ingest(&other, &good).expect("ingest");
    assert!(core.account_kind_seen().expect("seen"));
    let after = core.trust_generation().expect("generation");
    assert!(after > before, "a new attestation is a trust change");
    let kept = stored_signature(&c, &other).expect("stored");

    // The same attestation again is no write and no generation bump.
    core.account_kind_ingest(&other, &good)
        .expect("ingest again");
    assert_eq!(core.trust_generation().expect("generation"), after);

    // Never replaced by one that does not verify.
    assert_eq!(
        err(core.account_kind_ingest(&other, &raw_attestation(1, 1))).code,
        "E_CORE_INPUT"
    );
    assert_eq!(
        err(core.account_kind_ingest(&other, &[0x80])).code,
        "E_CORE_INPUT"
    );
    assert_eq!(stored_signature(&c, &other), Some(kept));
    assert_eq!(core.trust_generation().expect("generation"), after);
}

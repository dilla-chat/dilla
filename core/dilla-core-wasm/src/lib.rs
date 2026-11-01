//! `dilla-core-wasm` — the browser binding of `dilla-core` (interfaces §2.11).
//!
//! `u64` arguments cross as JavaScript `BigInt`. Errors are `JsError` carrying a `protocol/*.md`
//! `E_*` code, except `store_open`, which rethrows the original DOMException so that
//! [`store::is_sah_contention`] can classify it (deviation A2-3).
//! `CoreHandle` is the browser face of `dilla_core::client::ClientCore`; `abi_version()` names the
//! browser binding ABI independently of the wasi request ABI.

// `facade`, `store` and `probe` exist only on the browser target. The gate lives here, on the declarations and
// the re-exports, and NOT as an inner `#![cfg(…)]` in the two files: a false inner cfg removes the
// module *item*, so an unconditional `pub mod store;` + `pub use store::{…};` fails the native build
// with `error[E0432]: unresolved import` and takes every native test of this crate with it.
#[cfg(all(target_arch = "wasm32", target_os = "unknown"))]
pub mod facade;
#[cfg(all(target_arch = "wasm32", target_os = "unknown"))]
pub mod probe;
#[cfg(all(target_arch = "wasm32", target_os = "unknown"))]
pub mod store;

// Ungated: `MediaSender`/`MediaReceiver` use no browser API, so they build natively too.
mod media;

use dilla_core::ProtocolError;
use dilla_core::envelope::{
    Attachment, Envelope, EnvelopeType, FrankingTagInput, Preview,
    franking_tag as core_franking_tag,
};
use dilla_core::identity::{
    CredentialIdentity, Kind, SignerTier, Tier, recovery_key_base32 as core_recovery_key_base32,
    safety_number as core_safety_number, sas as core_sas,
};
use dilla_core::ids::{DeviceId, MsgId, UserId};
use dilla_core::sframe::{Ctr, Kid, NK, NN, SframeError, derive_keys, encode_header};
use serde_json::{Map, Value, json};
use wasm_bindgen::prelude::*;

pub use media::*;

#[cfg(all(target_arch = "wasm32", target_os = "unknown"))]
pub use facade::{CoreHandle, core_open};
#[cfg(all(target_arch = "wasm32", target_os = "unknown"))]
pub use probe::probe_persistence;
#[cfg(all(target_arch = "wasm32", target_os = "unknown"))]
pub use store::{StoreHandle, StoreOpenConfig, is_sah_contention, store_mls_probe, store_open};

fn err(code: &str, detail: &str) -> JsError {
    JsError::new(&format!("{code}: {detail}"))
}

fn protocol(e: ProtocolError) -> JsError {
    JsError::new(e.code())
}

fn unhex(s: &str, field: &str) -> Result<Vec<u8>, JsError> {
    if !s.len().is_multiple_of(2) || !s.bytes().all(|b| b.is_ascii_hexdigit()) {
        return Err(err("E_ENVELOPE_SHAPE", &format!("{field}: not hex")));
    }
    Ok((0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap())
        .collect())
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn fixed<const N: usize>(bytes: &[u8], field: &str) -> Result<[u8; N], JsError> {
    <[u8; N]>::try_from(bytes).map_err(|_| {
        err(
            "E_ENVELOPE_SHAPE",
            &format!("{field}: expected {N} bytes, got {}", bytes.len()),
        )
    })
}

fn obj<'a>(v: &'a Value, field: &str) -> Result<&'a Map<String, Value>, JsError> {
    v.as_object()
        .ok_or_else(|| err("E_ENVELOPE_SHAPE", &format!("{field}: not an object")))
}

fn str_field<'a>(m: &'a Map<String, Value>, k: &str) -> Result<&'a str, JsError> {
    m.get(k)
        .and_then(Value::as_str)
        .ok_or_else(|| err("E_ENVELOPE_SHAPE", &format!("{k}: missing string")))
}

fn u64_field(m: &Map<String, Value>, k: &str) -> Result<u64, JsError> {
    m.get(k)
        .and_then(Value::as_u64)
        .ok_or_else(|| err("E_ENVELOPE_SHAPE", &format!("{k}: missing integer")))
}

fn opt_hex16(m: &Map<String, Value>, k: &str) -> Result<Option<[u8; 16]>, JsError> {
    match m.get(k) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(s)) => Ok(Some(fixed::<16>(&unhex(s, k)?, k)?)),
        Some(_) => Err(err(
            "E_ENVELOPE_SHAPE",
            &format!("{k}: expected a hex string or null"),
        )),
    }
}

fn opt_hex(m: &Map<String, Value>, k: &str) -> Result<Option<Vec<u8>>, JsError> {
    match m.get(k) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(s)) => Ok(Some(unhex(s, k)?)),
        Some(_) => Err(err(
            "E_ENVELOPE_SHAPE",
            &format!("{k}: expected a hex string or null"),
        )),
    }
}

#[wasm_bindgen]
pub fn core_version() -> String {
    dilla_core::CORE_VERSION.to_owned()
}

/// The browser binding ABI. Independent of the wasi request ABI.
pub const BROWSER_ABI_VERSION: u32 = 15;

#[wasm_bindgen]
pub fn abi_version() -> u32 {
    BROWSER_ABI_VERSION
}

#[cfg(feature = "vectors")]
#[wasm_bindgen]
pub fn vectors_check_json() -> String {
    let report = dilla_core::vectors::run_all();
    let suites: Vec<Value> = report
        .suites
        .iter()
        .map(|s| {
            json!({
                "name": s.name,
                "cases": s.cases.iter().map(|c| json!({
                    "case": c.case, "field": c.field, "ok": c.ok,
                    "expected": c.expected, "actual": c.actual,
                })).collect::<Vec<_>>(),
            })
        })
        .collect();
    json!({ "passed": report.passed, "failed": report.failed, "suites": suites }).to_string()
}

#[cfg(feature = "vectors")]
#[wasm_bindgen]
pub fn vectors_check_ok() -> bool {
    dilla_core::vectors::run_all().is_ok()
}

/// Reads the JSON shape `protocol/vectors/envelope.json` uses for its `envelope` object.
fn envelope_from_json(source: &str) -> Result<Envelope, JsError> {
    let v: Value =
        serde_json::from_str(source).map_err(|e| err("E_ENVELOPE_SHAPE", &e.to_string()))?;
    let m = obj(&v, "envelope")?;
    let empty: Vec<Value> = Vec::new();

    let mut attachments = Vec::new();
    for a in m
        .get("attachments")
        .and_then(Value::as_array)
        .unwrap_or(&empty)
    {
        let a = obj(a, "attachment")?;
        attachments.push(Attachment {
            blob_id: fixed::<32>(&unhex(str_field(a, "blobId")?, "blobId")?, "blobId")?,
            key: fixed::<32>(&unhex(str_field(a, "key")?, "key")?, "key")?,
            nonce: fixed::<12>(&unhex(str_field(a, "nonce")?, "nonce")?, "nonce")?,
            size: u64_field(a, "size")?,
            mime: str_field(a, "mime")?.to_owned(),
            w: a.get("w").and_then(Value::as_u64),
            h: a.get("h").and_then(Value::as_u64),
            thumb: opt_hex(a, "thumb")?,
            name: str_field(a, "name")?.to_owned(),
        });
    }

    let mut previews = Vec::new();
    for p in m
        .get("previews")
        .and_then(Value::as_array)
        .unwrap_or(&empty)
    {
        let p = obj(p, "preview")?;
        previews.push(Preview {
            url: str_field(p, "url")?.to_owned(),
            title: str_field(p, "title")?.to_owned(),
            description: str_field(p, "description")?.to_owned(),
            image: opt_hex(p, "image")?,
        });
    }

    Ok(Envelope {
        v: u64_field(m, "v")?,
        msg_id: MsgId::from_bytes(fixed::<16>(
            &unhex(str_field(m, "msgId")?, "msgId")?,
            "msgId",
        )?),
        kind: EnvelopeType::from_u64(u64_field(m, "type")?).map_err(protocol)?,
        thread_id: opt_hex16(m, "threadId")?.map(MsgId::from_bytes),
        reply_to: opt_hex16(m, "replyTo")?.map(MsgId::from_bytes),
        body: str_field(m, "body")?.to_owned(),
        attachments,
        previews,
        k_f: fixed::<32>(&unhex(str_field(m, "kf")?, "kf")?, "kf")?,
    })
}

fn envelope_to_json(e: &Envelope) -> String {
    json!({
        "v": e.v,
        "msgId": e.msg_id.to_hex(),
        "type": e.kind.as_u8(),
        "threadId": e.thread_id.map(|x| x.to_hex()),
        "replyTo": e.reply_to.map(|x| x.to_hex()),
        "body": e.body,
        "attachments": e.attachments.iter().map(|a| json!({
            "blobId": hex(&a.blob_id), "key": hex(&a.key), "nonce": hex(&a.nonce),
            "size": a.size, "mime": a.mime, "w": a.w, "h": a.h,
            "thumb": a.thumb.as_ref().map(|t| hex(t)), "name": a.name,
        })).collect::<Vec<_>>(),
        "previews": e.previews.iter().map(|p| json!({
            "url": p.url, "title": p.title, "description": p.description,
            "image": p.image.as_ref().map(|i| hex(i)),
        })).collect::<Vec<_>>(),
        "kf": hex(&e.k_f),
    })
    .to_string()
}

#[wasm_bindgen]
pub fn envelope_encode(json: &str) -> Result<Box<[u8]>, JsError> {
    Ok(envelope_from_json(json)?
        .encode()
        .map_err(protocol)?
        .into_boxed_slice())
}

#[wasm_bindgen]
pub fn envelope_decode_json(cbor: &[u8]) -> Result<String, JsError> {
    Ok(envelope_to_json(&Envelope::decode(cbor).map_err(protocol)?))
}

#[wasm_bindgen]
pub fn envelope_commitment(cbor: &[u8]) -> Result<Box<[u8]>, JsError> {
    let envelope = Envelope::decode(cbor).map_err(protocol)?;
    Ok(Box::new(envelope.commitment().map_err(protocol)?) as Box<[u8]>)
}

#[wasm_bindgen]
pub fn franking_tag(
    k_frank: &[u8],
    group_id: &[u8],
    epoch: u64,
    seq: u64,
    uploader_device: &[u8],
    commitment: &[u8],
    recv_ts: u64,
) -> Result<Box<[u8]>, JsError> {
    let input = FrankingTagInput {
        group_id: fixed::<16>(group_id, "group_id")?,
        epoch,
        seq,
        uploader_device: DeviceId::from_bytes(fixed::<16>(uploader_device, "uploader_device")?),
        commitment: fixed::<32>(commitment, "commitment")?,
        recv_ts,
    };
    Ok(Box::new(core_franking_tag(&fixed::<32>(k_frank, "k_frank")?, &input)) as Box<[u8]>)
}

#[wasm_bindgen]
pub struct SframeKeysJs {
    key: [u8; NK],
    salt: [u8; NN],
}

#[wasm_bindgen]
impl SframeKeysJs {
    #[wasm_bindgen(getter)]
    pub fn key(&self) -> Box<[u8]> {
        Box::new(self.key) as Box<[u8]>
    }
    #[wasm_bindgen(getter)]
    pub fn salt(&self) -> Box<[u8]> {
        Box::new(self.salt) as Box<[u8]>
    }
}

#[wasm_bindgen]
pub fn sframe_derive(
    base_key: &[u8],
    leaf_index: u32,
    epoch: u64,
) -> Result<SframeKeysJs, JsError> {
    let base = fixed::<NK>(base_key, "base_key")?;
    let leaf =
        u16::try_from(leaf_index).map_err(|_| JsError::new(SframeError::LeafOutOfRange.code()))?;
    let keys = derive_keys(&base, Kid::new(leaf, epoch));
    Ok(SframeKeysJs {
        key: keys.key,
        salt: keys.salt,
    })
}

/// A raw KID from JavaScript is held to the same rule as a received one: 2^24 or more is
/// `E_SFRAME_NON_CANONICAL_KID` (protocol/05 "Frame format").
#[wasm_bindgen]
pub fn sframe_header(kid: u64, ctr: u64) -> Result<Box<[u8]>, JsError> {
    let kid = Kid::canonical(kid).map_err(|e| JsError::new(e.code()))?;
    Ok(encode_header(kid, Ctr::from_raw(ctr)).into_boxed_slice())
}

#[wasm_bindgen]
pub fn safety_number(umk_a: &[u8], umk_b: &[u8]) -> Result<String, JsError> {
    Ok(core_safety_number(
        &fixed::<32>(umk_a, "umk_a")?,
        &fixed::<32>(umk_b, "umk_b")?,
    ))
}

#[wasm_bindgen]
pub fn sas(epoch_authenticator: &[u8]) -> Result<String, JsError> {
    Ok(core_sas(&fixed::<32>(
        epoch_authenticator,
        "epoch_authenticator",
    )?))
}

#[wasm_bindgen]
pub fn recovery_key_base32(rk: &[u8]) -> Result<String, JsError> {
    Ok(core_recovery_key_base32(&fixed::<32>(rk, "rk")?))
}

/// Reads the `credential_identity.fields` shape of `protocol/vectors/identity.json`.
#[wasm_bindgen]
pub fn credential_identity_cbor(json: &str) -> Result<Box<[u8]>, JsError> {
    let v: Value = serde_json::from_str(json).map_err(|e| err("E_CREDENTIAL", &e.to_string()))?;
    let m = obj(&v, "credential_identity")?;
    let identity = CredentialIdentity {
        v: m.get("v").and_then(Value::as_u64).unwrap_or(1),
        umk_pub: fixed::<32>(&unhex(str_field(m, "umk_pub")?, "umk_pub")?, "umk_pub")?,
        user_id: UserId::from_bytes(fixed::<16>(
            &unhex(str_field(m, "user_id")?, "user_id")?,
            "user_id",
        )?),
        device_id: DeviceId::from_bytes(fixed::<16>(
            &unhex(str_field(m, "device_id")?, "device_id")?,
            "device_id",
        )?),
        kind: Kind::from_u64(u64_field(m, "kind")?).map_err(protocol)?,
        tier: Tier::from_u64(u64_field(m, "tier")?).map_err(protocol)?,
        signer_tier: SignerTier::from_u64(u64_field(m, "signer_tier")?).map_err(protocol)?,
        ssk_pub: fixed::<32>(&unhex(str_field(m, "ssk_pub")?, "ssk_pub")?, "ssk_pub")?,
        sig_umk_ssk: fixed::<64>(
            &unhex(str_field(m, "sig_umk_ssk")?, "sig_umk_ssk")?,
            "sig_umk_ssk",
        )?,
        sig_ssk_dev: fixed::<64>(
            &unhex(str_field(m, "sig_ssk_dev")?, "sig_ssk_dev")?,
            "sig_ssk_dev",
        )?,
    };
    Ok(identity.encode().into_boxed_slice())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// `protocol/vectors/envelope.json`, case "reaction add in a thread".
    const VECTOR_ENVELOPE_JSON: &str = r#"{
        "v": 1,
        "msgId": "11111111111111111111111111111111",
        "type": 3,
        "threadId": "12121212121212121212121212121212",
        "replyTo": "13131313131313131313131313131313",
        "body": "⛏",
        "attachments": [],
        "previews": [],
        "kf": "1616161616161616161616161616161616161616161616161616161616161616"
    }"#;
    const VECTOR_ENVELOPE_CBOR: &str = "89015011111111111111111111111111111111035012121212121212121212121212121212501313131313131313131313131313131363e29b8f808058201616161616161616161616161616161616161616161616161616161616161616";
    const VECTOR_ENVELOPE_COMMITMENT: &str =
        "ab2930d97f3c839758c035d9b2ace3b85676e65a37807b66370ffb2885a23148";
    /// `protocol/vectors/identity.json`, `credential_identity.cbor`, verbatim — the committed
    /// vector task 7 regenerated from real Ed25519 material, not a placeholder pair. Asserting the
    /// whole 218-byte encoding is the point: a ten-byte prefix check would pass with a wrong
    /// `user_id`, a wrong `device_id`, swapped `kind`/`tier`/`signer_tier` bytes or truncated
    /// signatures, on the one function that encodes identity material.
    const VECTOR_CREDENTIAL_CBOR: &str = "8a015820db995fe25169d141cab9bbba92baa01f9f2e1ece7df4cb2ac05190f37fcc1f9d50d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d450e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e500010058202152f8d19b791d24453242e15f2eab6cb7cffa7b6a5ed30097960e069881db125840203a5d3b052bb587cc453ae05f171f75687e338847942acd50f9b44f7fee8a4a08d4bf56587f98d2701231e6af6668b03619b79d1ec5bb956a3ca78f8380ea035840caad6f3308aa16d76dea6eb86becc934651a88490c8de4b4236cc7cb9ae0e45377d0a573f1332c9519c707b87834e9937c5be53f1877fbe5284e37096375800e";

    fn unhex(s: &str) -> Vec<u8> {
        (0..s.len())
            .step_by(2)
            .map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap())
            .collect()
    }

    fn hexed(b: &[u8]) -> String {
        b.iter().map(|x| format!("{x:02x}")).collect()
    }

    #[test]
    fn envelope_encode_reproduces_the_vector_bytes() {
        let out = envelope_encode(VECTOR_ENVELOPE_JSON).expect("the vector envelope must encode");
        assert_eq!(hexed(&out), VECTOR_ENVELOPE_CBOR);
    }

    #[test]
    fn envelope_decode_json_round_trips_the_vector() {
        let json =
            envelope_decode_json(&unhex(VECTOR_ENVELOPE_CBOR)).expect("the vector must decode");
        let again = envelope_encode(&json).expect("the decoded JSON must re-encode");
        assert_eq!(hexed(&again), VECTOR_ENVELOPE_CBOR);
    }

    #[test]
    fn envelope_commitment_reproduces_the_vector() {
        let c = envelope_commitment(&unhex(VECTOR_ENVELOPE_CBOR)).unwrap();
        assert_eq!(hexed(&c), VECTOR_ENVELOPE_COMMITMENT);
    }

    /// `protocol/vectors/sframe.json`, case leaf_index 3 / epoch 41.
    #[test]
    fn sframe_derive_reproduces_the_vector() {
        let keys = sframe_derive(&unhex("0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"), 3, 41).unwrap();
        assert_eq!(hexed(&keys.key()), "3fe54e870b67caffa901a88054d2cb6f");
        assert_eq!(hexed(&keys.salt()), "f92a07e1770098f68ed24805");
    }

    /// `kid` and `ctr` in `sframe.json` are **decimal strings**, not hex (interfaces §2.9), so this
    /// case's `"kid": "809"` is 809 decimal — `Kid::new(leaf_index = 3, epoch = 41)` = `(3 << 8) | 41`.
    /// The expected bytes prove it: `91 03 29` carries the two-byte extended KID `0x0329` = 809.
    /// Writing `0x809` here would ask for leaf 8 / epoch-low 9 and fail.
    #[test]
    fn sframe_header_reproduces_the_vector() {
        assert_eq!(hexed(&sframe_header(809, 1).unwrap()), "910329");
    }

    /// `protocol/vectors/identity.json`.
    #[test]
    fn safety_number_and_sas_reproduce_the_vectors() {
        assert_eq!(
            safety_number(
                &unhex("a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"),
                &unhex("b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2")
            )
            .unwrap(),
            "097797588879462191319159221653839944788022939511249052334637"
        );
        assert_eq!(
            sas(&unhex(
                "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
            ))
            .unwrap(),
            "088546891769712384735671929712"
        );
    }

    #[test]
    fn recovery_key_base32_reproduces_the_vector() {
        assert_eq!(
            recovery_key_base32(&unhex(
                "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"
            ))
            .unwrap(),
            "1C5GP2RB1C5GP2RB1C5GP2RB1C5GP2RB1C5GP2RB1C5GP2RB1C5G"
        );
    }

    /// `protocol/vectors/identity.json`, `credential_identity`. The JSON below is
    /// `credential_identity.fields` verbatim and the constant is `credential_identity.cbor` verbatim,
    /// both as committed — the keys are the real Ed25519 pair task 7 generated, and the signatures
    /// are the real signatures over them, so this pins the wasm entry point to the same bytes
    /// `dilla_core::vectors::run_identity` pins the core to. Both signatures are **64 bytes = 128 hex
    /// characters**, which is what `fixed::<64>` accepts. A 130-character literal fails with
    /// `E_ENVELOPE_SHAPE: expected 64 bytes, got 65`.
    #[test]
    fn credential_identity_cbor_reproduces_the_vector() {
        let json = r#"{
            "umk_pub": "db995fe25169d141cab9bbba92baa01f9f2e1ece7df4cb2ac05190f37fcc1f9d",
            "user_id": "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4",
            "device_id": "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5",
            "kind": 0, "tier": 1, "signer_tier": 0,
            "ssk_pub": "2152f8d19b791d24453242e15f2eab6cb7cffa7b6a5ed30097960e069881db12",
            "sig_umk_ssk": "203a5d3b052bb587cc453ae05f171f75687e338847942acd50f9b44f7fee8a4a08d4bf56587f98d2701231e6af6668b03619b79d1ec5bb956a3ca78f8380ea03",
            "sig_ssk_dev": "caad6f3308aa16d76dea6eb86becc934651a88490c8de4b4236cc7cb9ae0e45377d0a573f1332c9519c707b87834e9937c5be53f1877fbe5284e37096375800e"
        }"#;
        assert_eq!(
            hexed(&credential_identity_cbor(json).unwrap()),
            VECTOR_CREDENTIAL_CBOR
        );
    }

    /// Deviation A2-14. `open_encrypted`'s pragma batch is
    /// `PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:<64-hex device KEK>';`, and rusqlite 0.40.2's
    /// `Error::SqlInputError` Displays as `"{msg} in {sql} at offset {offset}"` — the whole
    /// statement text (`rusqlite-0.40.2/src/error.rs:346-351`). Formatting that error into the
    /// message `store_open` hands JavaScript would put the raw device KEK into a JS `Error` the
    /// worker logs, which is exactly what A2-14 exists to prevent. `store.rs` compiles only on
    /// `wasm32-unknown-unknown` and `store_open` needs a dedicated worker in a secure context
    /// (gap-11 §8), so the error cannot be provoked from a native test or from a
    /// `wasm-bindgen-test` here; the guard is asserted on the module's source text, which is what a
    /// regression would change.
    #[test]
    fn the_cipher_error_never_carries_the_pragma_statement() {
        let src = include_str!("store.rs");
        assert!(
            !src.contains("E_STORE_CIPHER: {e}"),
            "the E_STORE_CIPHER arm must not format the rusqlite error into its message: that \
             error's Display can carry the pragma statement, and the statement carries the KEK"
        );
        assert!(
            src.contains("E_STORE_CIPHER: setting the cipher or key failed"),
            "the E_STORE_CIPHER arm must hand back the fixed, key-free message"
        );
        // The two arms whose statements carry no key material keep their rusqlite detail.
        assert!(src.contains("E_STORE_OPEN: {e}"));
        assert!(src.contains("E_STORE_KEY: {e}"));
    }

    /// Deviation A2-14, applied to deviation A2-10's two exports. `unencrypted_vfs_probe` and
    /// `wrong_key_probe` both run
    /// `PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:<64-hex device KEK>';`, and task 17's worker
    /// calls the first of them with the **real** store key and publishes the returned string on the
    /// page. Rendering a rusqlite error with `Display` on those two arms is the same hole the test
    /// above closes for `E_STORE_CIPHER`, on functions that are crate surface rather than
    /// spike-local helpers. Both arms must go through `store::redacted_sqlite_message`, which keeps
    /// only SQLite's own primary message; `tests/node.rs` asserts what that helper actually does.
    #[test]
    fn the_probe_errors_never_render_the_pragma_statement() {
        let src = include_str!("store.rs");
        assert!(
            !src.contains("Err(e) => Ok(e.to_string())"),
            "unencrypted_vfs_probe must not Display the rusqlite error: the statement it ran \
             carries the caller's KEK"
        );
        assert!(
            !src.contains("proves nothing: {e}"),
            "wrong_key_probe's pragma arm must not Display the rusqlite error: same statement, \
             same KEK"
        );
        assert_eq!(
            src.matches("redacted_sqlite_message(&e)").count(),
            2,
            "both probe arms must render their rusqlite error through the redacting helper"
        );
    }

    #[cfg(feature = "vectors")]
    #[test]
    fn the_vector_report_is_green_and_serialises_as_json() {
        assert!(vectors_check_ok());
        let json = vectors_check_json();
        assert!(
            json.contains("\"failed\":0"),
            "report must be green: {json}"
        );
    }

    #[test]
    fn the_version_getters_match_dilla_core() {
        assert_eq!(core_version(), dilla_core::CORE_VERSION);
        assert_eq!(abi_version(), BROWSER_ABI_VERSION);
        assert_eq!(BROWSER_ABI_VERSION, 15);
    }
}

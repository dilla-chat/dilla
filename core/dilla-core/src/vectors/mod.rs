//! The cross-target conformance runner.
//!
//! The six vector files are embedded at compile time (`include_bytes!`, minified by
//! `vector_json!`) rather than read from disk: `std::fs`
//! always errors on `wasm32-unknown-unknown`, so this is the only shape that compiles for native,
//! wasm32-unknown-unknown and wasm32-wasip1 alike. The same functions back the wasi
//! `vectors_check` export and the Node test, which is what stops the three builds diverging.

mod report;

pub use report::{CaseReport, SuiteReport, VectorReport};

use crate::attachment::{blob_id, open_blob, open_thumb, seal_blob, seal_thumb};
use crate::cbor::decode_strict;
use crate::envelope::{
    Attachment, Envelope, EnvelopeType, FrankingTagInput, Preview, franking_tag,
};
use crate::identity::{
    CredentialIdentity, Kind, SignerTier, Tier, k_backup, k_header, recovery_key_base32,
    safety_number, sas,
};
use crate::ids::{DeviceId, MsgId, UserId};
use crate::sframe::{
    Codec, Ctr, FrameKey, KeyRing, Kid, NK, SframeError, SframeSender, Slot, decode_header,
    derive_keys, encode_header, encrypt_frame, nonce, open_frame, peek_kid_ctr, prefix_len,
    protect, rbsp_escape, rbsp_unescape, unescape_protected,
};
use serde_json::Value;

/// Embeds a vector file without the whitespace JSON ignores between tokens, so the browser build
/// carries the vectors and not the generator's indentation (web-2b task 1: about 20 KB of the
/// wasm, against a 16 KB budget for the task). The work happens at compile time: the file's own
/// bytes never reach the binary, and the text parses to the same `Value` as the file, which stays
/// the record (`protocol/vectors` is written only by the generator).
macro_rules! vector_json {
    ($path:literal) => {{
        const SOURCE: &[u8] = include_bytes!($path);
        const LEN: usize = json_min_len(SOURCE);
        const MIN: [u8; LEN] = json_min::<LEN>(SOURCE);
        match core::str::from_utf8(&MIN) {
            Ok(text) => text,
            Err(_) => panic!(concat!($path, " is not UTF-8")),
        }
    }};
}

/// One byte of a JSON text: whether the minified text keeps it, then the in-string and escape
/// states after it. Inside a string every byte is kept; outside, only space, tab, CR and LF (the
/// four whitespace bytes of RFC 8259 § 2) are dropped.
const fn json_step(c: u8, in_str: bool, esc: bool) -> (bool, bool, bool) {
    if in_str {
        (true, c != b'"' || esc, !esc && c == b'\\')
    } else {
        (!matches!(c, b' ' | b'\t' | b'\r' | b'\n'), c == b'"', false)
    }
}

/// The length of `src` once [`json_min`] has dropped its insignificant whitespace.
const fn json_min_len(src: &[u8]) -> usize {
    let (mut i, mut n, mut in_str, mut esc) = (0, 0, false, false);
    while i < src.len() {
        let (keep, s, e) = json_step(src[i], in_str, esc);
        (in_str, esc) = (s, e);
        if keep {
            n += 1;
        }
        i += 1;
    }
    n
}

/// `src` without the whitespace outside its strings; `N` is [`json_min_len`] of `src`.
const fn json_min<const N: usize>(src: &[u8]) -> [u8; N] {
    let mut out = [0u8; N];
    let (mut i, mut n, mut in_str, mut esc) = (0, 0, false, false);
    while i < src.len() {
        let (keep, s, e) = json_step(src[i], in_str, esc);
        (in_str, esc) = (s, e);
        if keep {
            out[n] = src[i];
            n += 1;
        }
        i += 1;
    }
    assert!(n == N, "json_min: N is not json_min_len(src)");
    out
}

pub const ENVELOPE_JSON: &str = vector_json!("../../../../protocol/vectors/envelope.json");
pub const FRANKING_JSON: &str = vector_json!("../../../../protocol/vectors/franking.json");
pub const SFRAME_JSON: &str = vector_json!("../../../../protocol/vectors/sframe.json");
pub const IDENTITY_JSON: &str = vector_json!("../../../../protocol/vectors/identity.json");
pub const FRAMES_JSON: &str = vector_json!("../../../../protocol/vectors/frames.json");
pub const ATTACHMENT_JSON: &str = vector_json!("../../../../protocol/vectors/attachment.json");

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn unhex(s: &str) -> Vec<u8> {
    (0..s.len() / 2)
        .map(|i| u8::from_str_radix(&s[2 * i..2 * i + 2], 16).unwrap_or(0))
        .collect()
}

fn unhex_n<const N: usize>(s: &str) -> [u8; N] {
    let v = unhex(s);
    let mut out = [0u8; N];
    let n = v.len().min(N);
    out[..n].copy_from_slice(&v[..n]);
    out
}

/// Integers above 2^53 are decimal strings in the vector files; small ones may be either.
fn int(v: &Value) -> u64 {
    match v {
        Value::String(s) => s.parse().unwrap_or(0),
        other => other.as_u64().unwrap_or(0),
    }
}

/// The **expected** side of a comparison, read out of the vector file.
///
/// A missing or renamed JSON key yields this sentinel rather than `""`, so it can never compare
/// equal to a defaulted actual value: `expected: ""` against an `encode()` that failed and
/// defaulted to an empty vector is two faults reporting `ok: true`. Nothing the runner computes
/// can produce this string, so an absent field is always a failed case. Input-side accessors keep
/// their `unwrap_or("")`: a missing input produces a wrong actual, which then fails against the
/// real expected value.
const MISSING: &str = "<missing field>";

fn expect_str(v: &Value) -> &str {
    v.as_str().unwrap_or(MISSING)
}

/// The expected side of an integer comparison, as a decimal string. Missing is a failure, not 0.
fn expect_int(v: &Value) -> String {
    match v {
        Value::String(s) => s.clone(),
        Value::Number(n) => n.to_string(),
        _ => MISSING.to_owned(),
    }
}

/// Ed25519 verification for the two credential signatures, checked one field at a time so a
/// failure names which signature broke (interfaces.md section 2.9). `verify_strict` is the same
/// check `CredentialIdentity::verify_signatures` performs over both at once.
/// Verified against the vendored `ed25519-dalek 2.2.0` and `ed25519 2.2.3` sources:
/// `VerifyingKey::from_bytes(&[u8; 32]) -> Result<VerifyingKey, SignatureError>` and
/// `ed25519::Signature::from_bytes(&SignatureBytes) -> Signature` (`SignatureBytes = [u8; 64]`,
/// infallible), re-exported as `ed25519_dalek::Signature`.
fn ed25519_verifies(public: &[u8; 32], message: &[u8], signature: &[u8; 64]) -> bool {
    use ed25519_dalek::{Signature, VerifyingKey};
    VerifyingKey::from_bytes(public)
        .map(|k| {
            k.verify_strict(message, &Signature::from_bytes(signature))
                .is_ok()
        })
        .unwrap_or(false)
}

fn opt_msg_id(v: &Value) -> Option<MsgId> {
    v.as_str().map(|s| MsgId::from_bytes(unhex_n::<16>(s)))
}

fn envelope_from_json(j: &Value) -> Envelope {
    Envelope {
        v: int(&j["v"]),
        msg_id: MsgId::from_bytes(unhex_n::<16>(j["msgId"].as_str().unwrap_or(""))),
        kind: EnvelopeType::from_u64(int(&j["type"])).unwrap_or(EnvelopeType::Message),
        thread_id: opt_msg_id(&j["threadId"]),
        reply_to: opt_msg_id(&j["replyTo"]),
        body: j["body"].as_str().unwrap_or("").to_owned(),
        attachments: j["attachments"]
            .as_array()
            .map(|xs| {
                xs.iter()
                    .map(|a| Attachment {
                        blob_id: unhex_n::<32>(a["blobId"].as_str().unwrap_or("")),
                        key: unhex_n::<32>(a["key"].as_str().unwrap_or("")),
                        nonce: unhex_n::<12>(a["nonce"].as_str().unwrap_or("")),
                        size: int(&a["size"]),
                        mime: a["mime"].as_str().unwrap_or("").to_owned(),
                        w: a["w"].as_u64(),
                        h: a["h"].as_u64(),
                        thumb: a["thumb"].as_str().map(unhex),
                        name: a["name"].as_str().unwrap_or("").to_owned(),
                    })
                    .collect()
            })
            .unwrap_or_default(),
        previews: j["previews"]
            .as_array()
            .map(|xs| {
                xs.iter()
                    .map(|p| Preview {
                        url: p["url"].as_str().unwrap_or("").to_owned(),
                        title: p["title"].as_str().unwrap_or("").to_owned(),
                        description: p["description"].as_str().unwrap_or("").to_owned(),
                        image: p["image"].as_str().map(unhex),
                    })
                    .collect()
            })
            .unwrap_or_default(),
        k_f: unhex_n::<32>(j["kf"].as_str().unwrap_or("")),
    }
}

pub fn run_envelope() -> SuiteReport {
    let mut cases = Vec::new();
    let doc: Value = serde_json::from_str(ENVELOPE_JSON).unwrap_or(Value::Null);
    for case in doc["cases"].as_array().unwrap_or(&Vec::new()) {
        let name = case["name"].as_str().unwrap_or("?").to_owned();
        let env = envelope_from_json(&case["envelope"]);
        let bytes = env.encode().unwrap_or_default();
        cases.push(CaseReport::compare(
            name.clone(),
            "cbor",
            expect_str(&case["cbor"]),
            hex(&bytes),
        ));
        cases.push(CaseReport::compare(
            name.clone(),
            "length",
            expect_int(&case["length"]),
            bytes.len().to_string(),
        ));
        cases.push(CaseReport::compare(
            name,
            "commitment",
            expect_str(&case["commitment"]),
            hex(&env.commitment().unwrap_or_default()),
        ));
    }
    SuiteReport {
        name: "envelope",
        cases,
    }
}

pub fn run_franking() -> SuiteReport {
    let mut cases = Vec::new();
    let doc: Value = serde_json::from_str(FRANKING_JSON).unwrap_or(Value::Null);
    let k_frank = unhex_n::<32>(doc["instance_franking_key"].as_str().unwrap_or(""));

    // `envelope_cbor` is the file's own copy of the envelope every tag below is computed over, and
    // the `commitment` each case carries is derived from it. Decoding it and recomputing the
    // commitment is what ties the two halves of the file together: without this the tags are
    // checked against a commitment nothing in this suite ever produced, so an implementation that
    // got `frank_commitment` wrong but copied the file's `commitment` verbatim would pass. It also
    // gives the wasm and wasi legs a commitment case of their own, which the envelope suite only
    // exercises through `Envelope::commitment` on the envelope vectors.
    let envelope_cbor = unhex(doc["envelope_cbor"].as_str().unwrap_or(""));
    let expected_commitment = doc["cases"]
        .as_array()
        .and_then(|c| c.first())
        .map(|c| expect_str(&c["commitment"]))
        .unwrap_or(MISSING);
    cases.push(CaseReport::compare(
        "envelope_cbor",
        "commitment",
        expected_commitment,
        Envelope::decode(&envelope_cbor)
            .and_then(|e| e.commitment())
            .map(|c| hex(&c))
            .unwrap_or_else(|e| format!("<{}>", e.code())),
    ));
    for (i, case) in doc["cases"]
        .as_array()
        .unwrap_or(&Vec::new())
        .iter()
        .enumerate()
    {
        let input = FrankingTagInput {
            group_id: unhex_n::<16>(case["group_id"].as_str().unwrap_or("")),
            epoch: int(&case["epoch"]),
            seq: int(&case["seq"]),
            uploader_device: DeviceId::from_bytes(unhex_n::<16>(
                case["uploader_device"].as_str().unwrap_or(""),
            )),
            commitment: unhex_n::<32>(case["commitment"].as_str().unwrap_or("")),
            recv_ts: int(&case["recv_ts"]),
        };
        cases.push(CaseReport::compare(
            format!("case {i}"),
            "tag",
            expect_str(&case["tag"]),
            hex(&franking_tag(&k_frank, &input)),
        ));
    }
    SuiteReport {
        name: "franking",
        cases,
    }
}

pub fn run_sframe() -> SuiteReport {
    let mut cases = Vec::new();
    let doc: Value = serde_json::from_str(SFRAME_JSON).unwrap_or(Value::Null);
    let base_key = unhex_n::<NK>(doc["base_key"].as_str().unwrap_or(""));
    for case in doc["cases"].as_array().unwrap_or(&Vec::new()) {
        let leaf = u16::try_from(int(&case["leaf_index"])).unwrap_or(0);
        let epoch = int(&case["epoch"]);
        let name = format!("leaf {leaf} epoch {epoch}");
        let kid = Kid::new(leaf, epoch);
        cases.push(CaseReport::compare(
            name.clone(),
            "kid",
            expect_int(&case["kid"]),
            kid.value().to_string(),
        ));
        let keys = derive_keys(&base_key, kid);
        cases.push(CaseReport::compare(
            name.clone(),
            "key",
            expect_str(&case["key"]),
            hex(&keys.key),
        ));
        cases.push(CaseReport::compare(
            name.clone(),
            "salt",
            expect_str(&case["salt"]),
            hex(&keys.salt),
        ));
        let ctr = Ctr::new(
            u8::try_from(int(&case["slot"])).unwrap_or(0),
            u8::try_from(int(&case["layer"])).unwrap_or(0),
            int(&case["seq"]),
        )
        .unwrap_or(Ctr::from_raw(0));
        cases.push(CaseReport::compare(
            name.clone(),
            "ctr",
            expect_int(&case["ctr"]),
            ctr.value().to_string(),
        ));
        cases.push(CaseReport::compare(
            name.clone(),
            "nonce",
            expect_str(&case["nonce"]),
            hex(&nonce(&keys.salt, ctr)),
        ));
        cases.push(CaseReport::compare(
            name,
            "header",
            expect_str(&case["header"]),
            hex(&encode_header(kid, ctr)),
        ));
    }
    // RFC 9605 appendix C.1: encode, and strict decode back to (kid, ctr, length).
    for entry in doc["rfc9605_c1"].as_array().unwrap_or(&Vec::new()) {
        let (kid, ctr) = (int(&entry["kid"]), int(&entry["ctr"]));
        let name = format!("c1 kid {kid} ctr {ctr}");
        let header = expect_str(&entry["header"]);
        cases.push(CaseReport::compare(
            name.clone(),
            "header",
            header,
            hex(&encode_header(Kid::from_raw(kid), Ctr::from_raw(ctr))),
        ));
        cases.push(CaseReport::compare(
            name,
            "decode",
            format!("{kid} {ctr} {}", header.len() / 2),
            match decode_header(&unhex(header)) {
                Ok((k, c, n)) => format!("{} {} {n}", k.value(), c.value()),
                Err(e) => e.code().to_owned(),
            },
        ));
    }

    // RFC 9605 appendix C.3, suite 0x0004, as a dilla frame (the RFC's metadata is the prefix).
    let c3 = &doc["rfc9605_c3"];
    let c3_base = unhex_n::<NK>(c3["base_key"].as_str().unwrap_or(""));
    let (c3_kid, c3_ctr) = (
        Kid::from_raw(int(&c3["kid"])),
        Ctr::from_raw(int(&c3["ctr"])),
    );
    let c3_keys = derive_keys(&c3_base, c3_kid);
    let c3_prefix = unhex(c3["prefix"].as_str().unwrap_or(""));
    let mut c3_input = c3_prefix.clone();
    c3_input.extend_from_slice(&unhex(c3["plaintext"].as_str().unwrap_or("")));
    let c3_key = FrameKey::derive(&c3_base, c3_kid);
    cases.push(CaseReport::compare(
        "c3",
        "key",
        expect_str(&c3["key"]),
        hex(&c3_keys.key),
    ));
    cases.push(CaseReport::compare(
        "c3",
        "salt",
        expect_str(&c3["salt"]),
        hex(&c3_keys.salt),
    ));
    cases.push(CaseReport::compare(
        "c3",
        "nonce",
        expect_str(&c3["nonce"]),
        hex(&nonce(&c3_keys.salt, c3_ctr)),
    ));
    cases.push(CaseReport::compare(
        "c3",
        "frame",
        expect_str(&c3["frame"]),
        encrypt_frame(&c3_key, c3_kid, c3_ctr, c3_prefix.len(), &c3_input)
            .map(|f| hex(&f))
            .unwrap_or_else(|e| e.code().to_owned()),
    ));
    cases.push(CaseReport::compare(
        "c3",
        "open",
        hex(&c3_input),
        open_frame(
            &c3_key,
            c3_prefix.len(),
            &unhex(c3["frame"].as_str().unwrap_or("")),
        )
        .map(|(_, _, plain)| hex(&plain))
        .unwrap_or_else(|e| e.code().to_owned()),
    ));

    // One frame per codec rule: the prefix rule, the sender's whole path, the receiver's. A row
    // whose sender rewrites the input (H.264 canonicalisation) carries `canonical`: the prefix is
    // computed over it and the receiver opens to it.
    for f in doc["media_frames"].as_array().unwrap_or(&Vec::new()) {
        let name = format!("frame {}", f["name"].as_str().unwrap_or("?"));
        let codec = codec_of(&f["codec"]);
        let input = unhex(f["input"].as_str().unwrap_or(""));
        let opened = if f["canonical"].is_null() {
            &f["input"]
        } else {
            &f["canonical"]
        };
        let canonical = unhex(opened.as_str().unwrap_or(""));
        let kid = Kid::new(
            u16::try_from(int(&f["leaf_index"])).unwrap_or(0),
            int(&f["epoch"]),
        );
        let ctr = Ctr::new(
            u8::try_from(int(&f["slot"])).unwrap_or(0),
            u8::try_from(int(&f["layer"])).unwrap_or(0),
            int(&f["seq"]),
        )
        .unwrap_or(Ctr::from_raw(0));
        cases.push(CaseReport::compare(
            name.clone(),
            "prefix_len",
            expect_int(&f["prefix_len"]),
            codec
                .and_then(|c| prefix_len(c, &canonical))
                .map(|n| n.to_string())
                .unwrap_or_else(|e| e.code().to_owned()),
        ));
        cases.push(CaseReport::compare(
            name.clone(),
            "frame",
            expect_str(&f["frame"]),
            codec
                .and_then(|c| protect(&FrameKey::derive(&base_key, kid), kid, ctr, c, &input))
                .map(|out| hex(&out))
                .unwrap_or_else(|e| e.code().to_owned()),
        ));
        cases.push(CaseReport::compare(
            name,
            "open",
            expect_str(opened),
            open_vector_frame(&base_key, &f["codec"], f["frame"].as_str().unwrap_or(""))
                .map(|plain| hex(&plain))
                .unwrap_or_else(|e| e.code().to_owned()),
        ));
    }

    // Seeded RBSP escaping, both ways.
    for (i, e) in doc["escapes"]
        .as_array()
        .unwrap_or(&Vec::new())
        .iter()
        .enumerate()
    {
        let seed = u8::try_from(int(&e["seed_zeros"])).unwrap_or(0);
        let name = format!("escape {i} seed {seed}");
        cases.push(CaseReport::compare(
            name.clone(),
            "out",
            expect_str(&e["out"]),
            hex(&rbsp_escape(seed, &unhex(e["in"].as_str().unwrap_or("")))),
        ));
        cases.push(CaseReport::compare(
            name,
            "roundtrip",
            expect_str(&e["in"]),
            hex(&rbsp_unescape(
                seed,
                &unhex(e["out"].as_str().unwrap_or("")),
            )),
        ));
    }

    // Scripted key-ring runs: one case per `decrypt` step, expected `ok` or the code.
    for script in doc["receiver"].as_array().unwrap_or(&Vec::new()) {
        let name = script["name"].as_str().unwrap_or("?");
        let mut ring = KeyRing::new();
        for (i, step) in script["steps"]
            .as_array()
            .unwrap_or(&Vec::new())
            .iter()
            .enumerate()
        {
            if step["op"].as_str() == Some("install") {
                let roster: Vec<(u16, [u8; 16])> = step["roster"]
                    .as_array()
                    .unwrap_or(&Vec::new())
                    .iter()
                    .map(|m| {
                        (
                            u16::try_from(int(&m[0])).unwrap_or(u16::MAX),
                            unhex_n::<16>(m[1].as_str().unwrap_or("")),
                        )
                    })
                    .collect();
                let own = step["own_leaf"]
                    .as_i64()
                    .and_then(|l| u16::try_from(l).ok());
                ring.install_epoch(
                    int(&step["epoch"]),
                    &unhex_n::<NK>(step["base_key"].as_str().unwrap_or("")),
                    &roster,
                    own,
                    int(&step["now"]),
                );
                continue;
            }
            let device = unhex_n::<16>(step["device"].as_str().unwrap_or(""));
            let outcome = codec_of(&step["codec"]).and_then(|codec| {
                let slot =
                    Slot::from_u8(u8::try_from(int(&step["track_slot"])).unwrap_or(u8::MAX))?;
                ring.decrypt(
                    codec,
                    &unhex(step["frame"].as_str().unwrap_or("")),
                    Some(&device),
                    slot,
                    int(&step["now"]),
                )
            });
            cases.push(CaseReport::compare(
                format!("receiver {name} step {i}"),
                "result",
                expect_str(&step["expect"]),
                match outcome {
                    Ok(_) => "ok",
                    Err(e) => e.code(),
                },
            ));
        }
    }
    SuiteReport {
        name: "sframe",
        cases,
    }
}

/// What a fresh sender (leaf 1, epoch 41, floor 41) refuses for one `sender_rejects` row: the
/// encryption of `input` on (`slot`, `layer`), or for a row with `seq` the counter itself.
fn sender_refusal(base_key: &[u8; NK], row: &Value) -> Result<(), SframeError> {
    let slot = u8::try_from(int(&row["slot"])).unwrap_or(u8::MAX);
    let layer = u8::try_from(int(&row["layer"])).unwrap_or(u8::MAX);
    if !row["seq"].is_null() {
        return Ctr::new(slot, layer, int(&row["seq"])).map(|_| ());
    }
    let codec = codec_of(&row["codec"])?;
    let slot = Slot::from_u8(slot)?;
    SframeSender::new(base_key, 1, 41, 41)?
        .encrypt(
            codec,
            slot,
            layer,
            &unhex(row["input"].as_str().unwrap_or("")),
        )
        .map(|_| ())
}

/// `"opus" | "vp8" | "vp9" | "h264"` as the vector files spell a codec.
fn codec_of(v: &Value) -> Result<Codec, SframeError> {
    match v.as_str() {
        Some("opus") => Ok(Codec::Opus),
        Some("vp8") => Ok(Codec::Vp8),
        Some("vp9") => Ok(Codec::Vp9),
        Some("h264") => Ok(Codec::H264),
        _ => Err(SframeError::UnsupportedCodec),
    }
}

/// The receiver's path over a vector frame: unescape (H.264), read the KID from the header,
/// derive its key from the file's `base_key`, open. Returns `P || plaintext`.
fn open_vector_frame(
    base_key: &[u8; NK],
    codec: &Value,
    frame: &str,
) -> Result<Vec<u8>, SframeError> {
    let (unescaped, prefix) = unescape_protected(codec_of(codec)?, &unhex(frame))?;
    let (kid, _, _) = peek_kid_ctr(prefix, &unescaped)?;
    open_frame(&FrameKey::derive(base_key, kid), prefix, &unescaped).map(|(_, _, plain)| plain)
}

pub fn run_identity() -> SuiteReport {
    let mut cases = Vec::new();
    let doc: Value = serde_json::from_str(IDENTITY_JSON).unwrap_or(Value::Null);

    let sn = &doc["safety_number"];
    cases.push(CaseReport::compare(
        "safety_number",
        "digits",
        expect_str(&sn["digits"]),
        safety_number(
            &unhex_n::<32>(sn["umk_a"].as_str().unwrap_or("")),
            &unhex_n::<32>(sn["umk_b"].as_str().unwrap_or("")),
        ),
    ));

    let s = &doc["sas"];
    cases.push(CaseReport::compare(
        "sas",
        "digits",
        expect_str(&s["digits"]),
        sas(&unhex_n::<32>(
            s["epoch_authenticator"].as_str().unwrap_or(""),
        )),
    ));

    let r = &doc["recovery_key"];
    let rk = unhex_n::<32>(r["rk"].as_str().unwrap_or(""));
    cases.push(CaseReport::compare(
        "recovery_key",
        "base32",
        expect_str(&r["base32"]),
        recovery_key_base32(&rk),
    ));
    cases.push(CaseReport::compare(
        "recovery_key",
        "k_header",
        expect_str(&r["k_header"]),
        hex(&k_header(&rk)),
    ));
    cases.push(CaseReport::compare(
        "recovery_key",
        "k_backup",
        expect_str(&r["k_backup"]),
        hex(&k_backup(&rk)),
    ));

    let ci = &doc["credential_identity"];
    let f = &ci["fields"];
    let credential = CredentialIdentity {
        v: 1,
        umk_pub: unhex_n::<32>(f["umk_pub"].as_str().unwrap_or("")),
        user_id: UserId::from_bytes(unhex_n::<16>(f["user_id"].as_str().unwrap_or(""))),
        device_id: DeviceId::from_bytes(unhex_n::<16>(f["device_id"].as_str().unwrap_or(""))),
        kind: Kind::from_u64(int(&f["kind"])).unwrap_or(Kind::User),
        tier: Tier::from_u64(int(&f["tier"])).unwrap_or(Tier::Native),
        signer_tier: SignerTier::from_u64(int(&f["signer_tier"])).unwrap_or(SignerTier::Native),
        ssk_pub: unhex_n::<32>(f["ssk_pub"].as_str().unwrap_or("")),
        sig_umk_ssk: unhex_n::<64>(f["sig_umk_ssk"].as_str().unwrap_or("")),
        sig_ssk_dev: unhex_n::<64>(f["sig_ssk_dev"].as_str().unwrap_or("")),
    };
    cases.push(CaseReport::compare(
        "credential_identity",
        "cbor",
        expect_str(&ci["cbor"]),
        hex(&credential.encode()),
    ));
    // Task 7 put real signatures and the leaf key in the file, so both are verifiable here.
    // They are two cases, not one: interfaces.md section 2.9 lists `sig_umk_ssk` and `sig_ssk_dev`
    // as separate fields of the identity suite, and a single "signatures" case cannot say which of
    // the two broke.
    let dsk_pub = unhex_n::<32>(ci["dsk_pub"].as_str().unwrap_or(""));
    cases.push(CaseReport::compare(
        "credential_identity",
        "sig_umk_ssk",
        "verified",
        if ed25519_verifies(
            &credential.umk_pub,
            &CredentialIdentity::ssk_message(&credential.ssk_pub),
            &credential.sig_umk_ssk,
        ) {
            "verified"
        } else {
            "rejected"
        },
    ));
    cases.push(CaseReport::compare(
        "credential_identity",
        "sig_ssk_dev",
        "verified",
        if ed25519_verifies(
            &credential.ssk_pub,
            &CredentialIdentity::dsk_message(
                &credential.device_id,
                &dsk_pub,
                credential.kind,
                credential.tier,
                credential.signer_tier,
            ),
            &credential.sig_ssk_dev,
        ) {
            "verified"
        } else {
            "rejected"
        },
    ));
    // The chain as a whole, through the production entry point, must agree with the two field
    // checks above.
    debug_assert_eq!(
        credential.verify_signatures(&dsk_pub).is_ok(),
        cases[cases.len() - 2].ok && cases[cases.len() - 1].ok
    );

    // The instance-signed account-kind attestation (protocol/03 § Account kind attestation):
    // the message bytes, the signature under the instance key, and the decoded wire form.
    let ak = &doc["account_kind"];
    let instance_pub = unhex_n::<32>(ak["instance_pub"].as_str().unwrap_or(""));
    let instance_id = unhex_n::<16>(ak["instance_id"].as_str().unwrap_or(""));
    let ak_user = unhex_n::<16>(ak["user_id"].as_str().unwrap_or(""));
    for case in ak["cases"].as_array().map(Vec::as_slice).unwrap_or(&[]) {
        let kind = u8::try_from(int(&case["kind"])).unwrap_or(u8::MAX);
        let message = crate::identity::account_kind_message(&instance_id, &ak_user, kind);
        cases.push(CaseReport::compare(
            "account_kind",
            "message",
            expect_str(&case["message"]),
            hex(&message),
        ));
        let decoded = crate::identity::AccountKindAttestation::decode(&unhex(
            case["attestation"].as_str().unwrap_or(""),
        ));
        let verified = decoded.as_ref().is_ok_and(|a| {
            a.kind == kind && ed25519_verifies(&instance_pub, &message, &a.signature)
        });
        cases.push(CaseReport::compare(
            "account_kind",
            "attestation",
            "verified",
            if verified { "verified" } else { "rejected" },
        ));
        // The decoded form re-encodes to the same bytes (deterministic CBOR, no lenient decode).
        cases.push(CaseReport::compare(
            "account_kind",
            "reencode",
            expect_str(&case["attestation"]),
            decoded.map(|a| hex(&a.encode())).unwrap_or_default(),
        ));
    }
    for reject in ak["rejects"].as_array().map(Vec::as_slice).unwrap_or(&[]) {
        let refused = crate::identity::AccountKindAttestation::decode(&unhex(
            reject["attestation"].as_str().unwrap_or(""),
        ))
        .is_err();
        cases.push(CaseReport::compare(
            "account_kind",
            "reject",
            "refused",
            if refused { "refused" } else { "accepted" },
        ));
    }

    SuiteReport {
        name: "identity",
        cases,
    }
}

/// Every input protocol/04 says a receiver must reject, plus the deterministic-CBOR reject corpus.
/// A case passes when the decoder **refuses** the input.
pub fn run_rejects() -> SuiteReport {
    let mut cases = Vec::new();
    let mut expect_cbor_reject = |name: &str, hex_input: &str| {
        let bytes = unhex(hex_input);
        let refused = decode_strict(&bytes, |d| d.skip().map(|_| ())).is_err();
        cases.push(CaseReport::compare(
            format!("{name} ({hex_input})"),
            "cbor",
            "rejected",
            if refused { "rejected" } else { "accepted" },
        ));
    };

    for (name, input) in [
        ("non-minimal uint", "1801"),
        ("non-minimal uint", "1817"),
        ("non-minimal uint", "190017"),
        ("non-minimal uint", "1a00000017"),
        ("non-minimal uint", "1b0000000000000017"),
        ("non-minimal uint", "1900ff"),
        ("non-minimal length", "5800"),
        ("non-minimal length", "7800"),
        ("non-minimal length", "9800"),
        ("non-minimal length", "990003010203"),
        ("indefinite array", "9f01ff"),
        ("indefinite bstr", "5f41014102ff"),
        ("indefinite text", "7f6161ff"),
        ("indefinite map", "bf0101ff"),
        ("map", "a0"),
        ("map", "a10102"),
        ("tag", "c11a514b67b0"),
        ("tag", "d8ff01"),
        ("tag", "c001"),
        ("float64", "fb3ff0000000000000"),
        ("float16", "f93c00"),
        ("negative", "20"),
        ("undefined", "f7"),
        ("non-minimal null", "f816"),
        ("reserved ai", "1c"),
        ("reserved ai", "1d"),
        ("reserved ai", "1e"),
        ("trailing bytes", "0101"),
        ("trailing bytes", "01a0"),
        ("trailing bytes", "83010203ff"),
        ("trailing bytes", "83010203ffffffff"),
        ("truncated bstr", "5820"),
        ("invalid utf-8", "6263c3"),
    ] {
        expect_cbor_reject(name, input);
    }

    // protocol/04's envelope reject list, exercised through Envelope::decode.
    let doc: Value = serde_json::from_str(ENVELOPE_JSON).unwrap_or(Value::Null);
    let env = envelope_from_json(&doc["cases"][0]["envelope"]);
    let good = env.encode().unwrap_or_default();

    let mut wrong_count = good.clone();
    if !wrong_count.is_empty() {
        wrong_count[0] = 0x88;
    }
    let mut unknown_type = good.clone();
    if unknown_type.len() > 19 {
        unknown_type[19] = 0x07;
    }
    let mut trailing = good.clone();
    trailing.push(0x00);

    for (name, bytes) in [
        ("wrong element count", wrong_count),
        ("unknown type", unknown_type),
        ("trailing bytes", trailing),
    ] {
        let refused = Envelope::decode(&bytes).is_err();
        cases.push(CaseReport::compare(
            format!("envelope {name}"),
            "decode",
            "rejected",
            if refused { "rejected" } else { "accepted" },
        ));
    }

    let mut over_limit = env.clone();
    over_limit.body = "a".repeat(crate::envelope::MAX_BODY_LONG + 1);
    cases.push(CaseReport::compare(
        "envelope body over 4000 bytes",
        "validate",
        "rejected",
        if over_limit.validate().is_err() {
            "rejected"
        } else {
            "accepted"
        },
    ));

    let mut short_body = env.clone();
    short_body.kind = EnvelopeType::ReactionAdd;
    short_body.reply_to = Some(MsgId::from_bytes([0x02; 16]));
    short_body.body = "a".repeat(crate::envelope::MAX_BODY_SHORT + 1);
    cases.push(CaseReport::compare(
        "reaction body over 32 bytes",
        "validate",
        "rejected",
        if short_body.validate().is_err() {
            "rejected"
        } else {
            "accepted"
        },
    ));

    // The reject cases `envelope.json` itself carries (protocol/04 "Vectors"): well-formed
    // deterministic CBOR that a conforming decoder must still refuse, each naming the error code
    // it must refuse it with. Driving them from the file rather than from a literal here is what
    // makes the TypeScript reference and this decoder answer the same question.
    for case in doc["rejects"].as_array().unwrap_or(&Vec::new()) {
        let name = case["name"].as_str().unwrap_or("?").to_owned();
        let bytes = unhex(case["cbor"].as_str().unwrap_or(""));
        let actual = match Envelope::decode(&bytes) {
            Ok(_) => "accepted",
            Err(e) => e.code(),
        };
        cases.push(CaseReport::compare(
            format!("envelope reject: {name}"),
            "decode",
            expect_str(&case["error"]),
            actual,
        ));
    }

    let attachment: Value = serde_json::from_str(ATTACHMENT_JSON).unwrap_or(Value::Null);
    for case in attachment["rejects"].as_array().unwrap_or(&Vec::new()) {
        let name = case["name"].as_str().unwrap_or("?");
        let key = unhex_n::<32>(case["key"].as_str().unwrap_or(""));
        let nonce = unhex_n::<12>(case["nonce"].as_str().unwrap_or(""));
        let data = unhex(case["data"].as_str().unwrap_or(""));
        let outcome = if case["kind"] == "thumb" {
            open_thumb(&key, &nonce, &data)
        } else {
            let id = unhex_n::<32>(case["blob_id"].as_str().unwrap_or(""));
            open_blob(&key, &nonce, &id, int(&case["size"]), &data)
        };
        cases.push(CaseReport::compare(
            format!("attachment reject: {name}"),
            "open",
            expect_str(&case["error"]),
            match outcome {
                Ok(_) => "accepted",
                Err(e) => e.code(),
            },
        ));
    }

    // sframe.json's `rejects` (protocol/05 "Errors"): header rows go through the receiver's parse
    // (`peek_kid_ctr`: the strict RFC 9605 decode, then the KID < 2^24 rule), frame rows through
    // the receiver's whole path. Expected is the file's `error`; actual is the code the core
    // refuses with, or "accepted".
    let sframe: Value = serde_json::from_str(SFRAME_JSON).unwrap_or(Value::Null);
    let sframe_base = unhex_n::<NK>(sframe["base_key"].as_str().unwrap_or(""));
    for case in sframe["rejects"].as_array().unwrap_or(&Vec::new()) {
        let name = format!("sframe reject: {}", case["name"].as_str().unwrap_or("?"));
        let (field, outcome) = match case["header"].as_str() {
            Some(header) => ("decode", peek_kid_ctr(0, &unhex(header)).map(|_| ())),
            None => (
                "open",
                open_vector_frame(
                    &sframe_base,
                    &case["codec"],
                    case["frame"].as_str().unwrap_or(""),
                )
                .map(|_| ()),
            ),
        };
        cases.push(CaseReport::compare(
            name,
            field,
            expect_str(&case["error"]),
            match outcome {
                Ok(()) => "accepted",
                Err(e) => e.code(),
            },
        ));
    }
    // sframe.json's `sender_rejects`: what a sender refuses to emit.
    for case in sframe["sender_rejects"].as_array().unwrap_or(&Vec::new()) {
        cases.push(CaseReport::compare(
            format!(
                "sframe sender reject: {}",
                case["name"].as_str().unwrap_or("?")
            ),
            "encrypt",
            expect_str(&case["error"]),
            match sender_refusal(&sframe_base, case) {
                Ok(()) => "accepted",
                Err(e) => e.code(),
            },
        ));
    }

    let mut bad_aad = env;
    bad_aad.k_f = [0x00; 32];
    cases.push(CaseReport::compare(
        "authenticated_data not 32 bytes",
        "verify_commitment",
        "rejected",
        if bad_aad.verify_commitment(&[0u8; 31]).is_err() {
            "rejected"
        } else {
            "accepted"
        },
    ));

    SuiteReport {
        name: "rejects",
        cases,
    }
}

/// The gateway frame corpus (`02-delivery-service.md`). Each case is re-encoded as
/// `[op, n, group_id, payload]` and compared against the committed `frame`; `payload` is spliced
/// in as already-encoded bytes with `Encoder::raw` (`cbor/enc.rs:90`), because the payload's own
/// shape is the responsibility of the producing suite, not of the framing.
pub fn run_frames() -> SuiteReport {
    let mut cases = Vec::new();
    let doc: Value = serde_json::from_str(FRAMES_JSON).unwrap_or(Value::Null);
    for case in doc["cases"].as_array().unwrap_or(&Vec::new()) {
        let name = case["name"].as_str().unwrap_or("?").to_owned();
        let mut e = crate::cbor::Encoder::new();
        e.array(4).uint(int(&case["op"])).uint(int(&case["n"]));
        let group = case["group_id"].as_str().unwrap_or("");
        if group.is_empty() {
            e.null();
        } else {
            e.bytes(&unhex(group));
        }
        e.raw(&unhex(case["payload"].as_str().unwrap_or("")));
        cases.push(CaseReport::compare(
            name,
            "frame",
            expect_str(&case["frame"]),
            hex(e.as_slice()),
        ));
    }
    SuiteReport {
        name: "frames",
        cases,
    }
}

pub fn run_all() -> VectorReport {
    VectorReport::from_suites(vec![
        run_envelope(),
        run_franking(),
        run_sframe(),
        run_identity(),
        run_frames(),
        run_attachment(),
        run_rejects(),
    ])
}

/// The hex of a sealed or opened value, or the code that refused it: the actual of an attachment
/// field.
fn hex_or_code(r: Result<Vec<u8>, crate::error::ProtocolError>) -> String {
    r.map_or_else(|e| e.code().to_owned(), |x| hex(&x))
}

/// protocol/04 § Attachments: `attachment.json`'s cases, sealed and opened with
/// `crate::attachment` (fields `stored`, `blob_id`, `open`, and `thumb`, `thumb_open` where
/// `thumb_plaintext` is not null). Its rejects run in [`run_rejects`].
pub fn run_attachment() -> SuiteReport {
    let mut cases = Vec::new();
    let doc: Value = serde_json::from_str(ATTACHMENT_JSON).unwrap_or(Value::Null);
    for case in doc["cases"].as_array().unwrap_or(&Vec::new()) {
        let text = |field: &str| case[field].as_str().unwrap_or("");
        let name = case["name"].as_str().unwrap_or("?");
        let key = unhex_n::<32>(text("key"));
        let nonce = unhex_n::<12>(text("nonce"));
        let stored = unhex(text("stored"));
        let mut push = |field: &'static str, expected: &str, actual: String| {
            cases.push(CaseReport::compare(name, field, expected, actual));
        };
        push(
            "stored",
            expect_str(&case["stored"]),
            hex(&seal_blob(&key, &nonce, &unhex(text("plaintext")))),
        );
        push(
            "blob_id",
            expect_str(&case["blob_id"]),
            hex(&blob_id(&stored)),
        );
        let id = unhex_n::<32>(text("blob_id"));
        push(
            "open",
            expect_str(&case["plaintext"]),
            hex_or_code(open_blob(&key, &nonce, &id, int(&case["size"]), &stored)),
        );
        if let Some(tp) = case["thumb_plaintext"].as_str() {
            push(
                "thumb",
                expect_str(&case["thumb"]),
                hex_or_code(seal_thumb(&key, &nonce, &unhex(tp))),
            );
            push(
                "thumb_open",
                tp,
                hex_or_code(open_thumb(&key, &nonce, &unhex(text("thumb")))),
            );
        }
    }
    SuiteReport {
        name: "attachment",
        cases,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn json_min_drops_whitespace_between_tokens_and_keeps_strings_whole() {
        const SRC: &[u8] =
            b"{\n  \"a b\" : \"x \\\" y\\\\\",\r\n\t\"c\": [1, 2, \" \\\\\\\" \"]\n}\n";
        const LEN: usize = json_min_len(SRC);
        let min = json_min::<LEN>(SRC);
        assert_eq!(
            core::str::from_utf8(&min),
            Ok(r#"{"a b":"x \" y\\","c":[1,2," \\\" "]}"#)
        );
    }

    #[test]
    fn every_embedded_vector_file_parses_to_the_file_on_disk() {
        for (embedded, file) in [
            (
                ENVELOPE_JSON,
                include_str!("../../../../protocol/vectors/envelope.json"),
            ),
            (
                FRANKING_JSON,
                include_str!("../../../../protocol/vectors/franking.json"),
            ),
            (
                SFRAME_JSON,
                include_str!("../../../../protocol/vectors/sframe.json"),
            ),
            (
                IDENTITY_JSON,
                include_str!("../../../../protocol/vectors/identity.json"),
            ),
            (
                FRAMES_JSON,
                include_str!("../../../../protocol/vectors/frames.json"),
            ),
            (
                ATTACHMENT_JSON,
                include_str!("../../../../protocol/vectors/attachment.json"),
            ),
        ] {
            let want: Value = serde_json::from_str(file).expect("the file parses");
            let got: Value = serde_json::from_str(embedded).expect("the embedded text parses");
            assert_eq!(got, want);
            assert!(embedded.len() < file.len(), "the indentation is gone");
            assert!(!embedded.contains("\n  "), "no indented line is left");
        }
    }
}

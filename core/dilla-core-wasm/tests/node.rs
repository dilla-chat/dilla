//! Conformance under `wasm32-unknown-unknown`, executed by Node through
//! `wasm-bindgen-test-runner` (the `.cargo/config.toml` runner, R6). No browser and no
//! chromedriver is involved; OPFS is task 17's Playwright spike.

#![cfg(target_arch = "wasm32")]

use dilla_core::cbor::{CborError, Decoder, Encoder, decode_strict};
use dilla_core::client::ClientError;
use dilla_core_wasm::facade::{
    authority_ids_arg, client_js_error, community_arg, core_open, fixed_arg,
    human_interaction_request_arg, login_arg, media_epoch_out, nonzero_id_arg, purpose_arg,
    role_ids_arg,
};
use dilla_core_wasm::store::redacted_sqlite_message;
use dilla_core_wasm::{
    BROWSER_ABI_VERSION, MediaReceiver, MediaSender, StoreOpenConfig, abi_version, core_version,
    credential_identity_cbor, envelope_commitment, envelope_decode_json, envelope_encode,
    franking_tag, recovery_key_base32, safety_number, sas, sframe_derive, sframe_header,
};
#[cfg(feature = "vectors")]
use dilla_core_wasm::{vectors_check_json, vectors_check_ok};
use wasm_bindgen::{JsError, JsValue};
use wasm_bindgen_test::*;

// NV-8: delete this line if step 1 shows the identifier does not exist on 0.3.78; Node is the
// runner's default either way.
wasm_bindgen_test_configure!(run_in_node_experimental);

fn unhex(s: &str) -> Vec<u8> {
    (0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(&s[i..i + 2], 16).unwrap())
        .collect()
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

#[wasm_bindgen_test]
fn human_interaction_send_inputs_are_typed_bounded_and_nonzero() {
    let id = [0x31; 16];
    assert_eq!(nonzero_id_arg("request_id", &id).unwrap(), id);
    assert!(nonzero_id_arg("request_id", &[0x31; 15]).is_err());
    assert!(nonzero_id_arg("request_id", &[0; 16]).is_err());

    assert!(human_interaction_request_arg(&[0x01; 2991]).is_ok());
    assert!(human_interaction_request_arg(&[0x01; 2992]).is_err());
}

const VECTOR_ENVELOPE_CBOR: &str = "89015011111111111111111111111111111111035012121212121212121212121212121212501313131313131313131313131313131363e29b8f808058201616161616161616161616161616161616161616161616161616161616161616";
const VECTOR_ENVELOPE_COMMITMENT: &str =
    "ab2930d97f3c839758c035d9b2ace3b85676e65a37807b66370ffb2885a23148";

/// The committed golden of suite names, in order, from `dilla_core::vectors`' six runners
/// (Plan A task 11 writes `SuiteReport { name: "envelope" | "franking" | "sframe" | "identity" |
/// "rejects", .. }`; Plan B task 17 adds `"frames"`). Without it, comparing the wasm report
/// against a report produced by the same
/// wasm build proves only internal consistency: a build that silently lost an entire suite would
/// pass. Plan B task 4 holds the equivalent golden for the wasip1 leg.
#[cfg(feature = "vectors")]
const EXPECTED_SUITES: [&str; 7] = [
    "envelope",
    "franking",
    "sframe",
    "identity",
    "frames",
    "attachment",
    "rejects",
];

#[cfg(feature = "vectors")]
#[wasm_bindgen_test]
fn the_vector_report_is_green_on_wasm() {
    let report = dilla_core::vectors::run_all();
    assert_eq!(report.failed, 0, "wasm32 report:\n{}", report.to_text());
    assert!(report.passed > 0, "a zero-case report would pass vacuously");
    assert!(vectors_check_ok());
}

/// The point of the cross-target triangle is not "wasm is green" but "every field of every case
/// holds under wasm too", so this asserts per case, naming the one that broke.
#[cfg(feature = "vectors")]
#[wasm_bindgen_test]
fn every_case_of_every_suite_holds_on_wasm() {
    let report = dilla_core::vectors::run_all();
    let json = vectors_check_json();

    let names: Vec<&str> = report.suites.iter().map(|s| s.name).collect();
    assert_eq!(
        names, EXPECTED_SUITES,
        "the wasm build must run all seven suites, in order"
    );

    // Every CaseReport is one (case, field) check, so the two counters and the case rows must agree.
    // A suite dropped between `run_all()` and the counters would break this even if the names held.
    let cases: usize = report.suites.iter().map(|s| s.cases.len()).sum();
    assert_eq!(
        cases as u32,
        report.passed + report.failed,
        "passed + failed must account for every case row"
    );

    for suite in &report.suites {
        assert!(
            json.contains(&format!("\"name\":\"{}\"", suite.name)),
            "missing suite {}",
            suite.name
        );
        assert!(!suite.cases.is_empty(), "suite {} has no cases", suite.name);
        for case in &suite.cases {
            assert!(
                case.ok,
                "{}/{}/{}: expected {} got {}",
                suite.name, case.case, case.field, case.expected, case.actual
            );
        }
    }
    assert!(json.contains("\"failed\":0"));
}

#[wasm_bindgen_test]
fn the_cbor_reject_corpus_behaves_identically_on_wasm() {
    // interfaces §2.3's reject corpus, one entry per rejection class.
    let cases: [(&str, &str); 12] = [
        ("1817", "non-minimal uint"),
        ("190017", "non-minimal uint"),
        ("5800", "non-minimal length"),
        ("9f01ff", "indefinite array"),
        ("5f41014102ff", "indefinite bstr"),
        ("a0", "map"),
        ("c11a514b67b0", "tag"),
        ("fb3ff0000000000000", "double"),
        ("f93c00", "half"),
        ("20", "negative"),
        ("f7", "undefined"),
        ("1c", "reserved additional info"),
    ];
    for (hexed, what) in cases {
        let bytes = unhex(hexed);
        let out: Result<(), CborError> = decode_strict(&bytes, |d: &mut Decoder<'_>| {
            d.skip()?;
            Ok(())
        });
        assert!(
            out.is_err(),
            "{what} ({hexed}) must be rejected on wasm too"
        );
    }
    let trailing = unhex("0101");
    assert!(matches!(
        decode_strict(&trailing, |d: &mut Decoder<'_>| d.uint().map(|_| ())),
        Err(CborError::TrailingBytes)
    ));
}

#[wasm_bindgen_test]
fn the_cbor_accept_corpus_round_trips_byte_identically_on_wasm() {
    let mut e = Encoder::new();
    e.array(3).uint(1).uint(2).uint(3);
    assert_eq!(hex(&e.into_vec()), "83010203");
    let mut e = Encoder::new();
    e.uint(1_000_000);
    assert_eq!(hex(&e.into_vec()), "1a000f4240");
    let mut e = Encoder::new();
    e.text("IETF");
    assert_eq!(hex(&e.into_vec()), "6449455446");
    let mut e = Encoder::new();
    e.null();
    assert_eq!(hex(&e.into_vec()), "f6");
    let mut e = Encoder::new();
    e.bytes(&[1, 2, 3, 4]);
    assert_eq!(hex(&e.into_vec()), "4401020304");
}

#[wasm_bindgen_test]
fn the_wasm_bindgen_surface_reproduces_the_envelope_vector() {
    let json = envelope_decode_json(&unhex(VECTOR_ENVELOPE_CBOR)).unwrap();
    assert_eq!(hex(&envelope_encode(&json).unwrap()), VECTOR_ENVELOPE_CBOR);
    assert_eq!(
        hex(&envelope_commitment(&unhex(VECTOR_ENVELOPE_CBOR)).unwrap()),
        VECTOR_ENVELOPE_COMMITMENT
    );
}

#[wasm_bindgen_test]
fn the_wasm_bindgen_surface_reproduces_the_sframe_and_identity_vectors() {
    let keys = sframe_derive(&unhex("0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"), 3, 41).unwrap();
    assert_eq!(hex(&keys.key()), "3fe54e870b67caffa901a88054d2cb6f");
    assert_eq!(hex(&keys.salt()), "f92a07e1770098f68ed24805");
    // `kid` in `sframe.json` is the decimal string "809" (interfaces §2.9), not hex: it is
    // `Kid::new(leaf_index = 3, epoch = 41)` = `(3 << 8) | 41`. `0x809` would be leaf 8 / epoch-low 9.
    assert_eq!(hex(&sframe_header(809, 1).unwrap()), "910329");
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
    assert_eq!(
        recovery_key_base32(&unhex(
            "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"
        ))
        .unwrap(),
        "1C5GP2RB1C5GP2RB1C5GP2RB1C5GP2RB1C5GP2RB1C5GP2RB1C5G"
    );
}

/// Every rejection path of the wasm-bindgen surface lives here rather than in task 15's native test
/// module: constructing a `JsError` off wasm goes through `__wbindgen_error_new`, whose non-wasm stub
/// panics, so these assertions can only be made where the intrinsics are real (NV-12).
#[wasm_bindgen_test]
fn errors_cross_the_boundary_as_values_not_panics() {
    assert!(
        envelope_decode_json(&unhex("a0")).is_err(),
        "a CBOR map is not an envelope"
    );
    let mut trailing = unhex(VECTOR_ENVELOPE_CBOR);
    trailing.push(0x01);
    assert!(
        envelope_decode_json(&trailing).is_err(),
        "trailing bytes must not decode"
    );
    assert!(
        sframe_derive(&[0u8; 15], 0, 0).is_err(),
        "a 15-byte base key is not NK"
    );
    assert!(credential_identity_cbor("{}").is_err());
    assert!(
        franking_tag(&[0u8; 32], &[0u8; 16], 1, 1, &[0u8; 16], &[0u8; 31], 0).is_err(),
        "a 31-byte commitment is not a 32-byte one"
    );
}

/// Deviation A2-14 applied to deviation A2-10's probes. `unencrypted_vfs_probe` and
/// `wrong_key_probe` render the failure of
/// `PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:<64-hex device KEK>';`, and task 17's worker calls
/// the first of them with the real store key and publishes the result on the page, so whatever the
/// helper returns is readable by anything running in that page. `redacted_sqlite_message` is the
/// only renderer either probe may use: it keeps SQLite's own primary message — the string the spike
/// asserts on — and withholds every other variant's `Display`, several of which embed text the
/// caller handed to SQLite. `Error::SqlInputError` is the worst of them (it renders as
/// `"{msg} in {sql} at offset {offset}"`, i.e. the whole statement — rusqlite-0.40.2/src/error.rs
/// lines 346-351, produced by `error_with_offset` at lines 502-509); this build cannot construct it,
/// because `modern_sqlite` is off under the `ffi-sqlite-wasm-rs` backend and the variant is
/// `#[cfg(feature = "modern_sqlite")]`, so `InvalidParameterName` stands in for the whole class.
#[wasm_bindgen_test]
fn a_redacted_probe_message_never_carries_the_statement() {
    const KEK: &str = "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b";
    let statement = format!("PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:{KEK}';");

    // The arm the spike depends on: SQLite's primary message, kept verbatim. This is exactly the
    // string task 17's plain-VFS probe asserts on, so redaction must not blunt it.
    let primary = rusqlite::Error::SqliteFailure(
        rusqlite::ffi::Error::new(1),
        Some("Setting key failed. Encryption is not supported by the VFS.".to_owned()),
    );
    assert_eq!(
        redacted_sqlite_message(&primary),
        "Setting key failed. Encryption is not supported by the VFS."
    );

    // Any variant whose Display embeds text handed to SQLite is withheld whole.
    let leaky = rusqlite::Error::InvalidParameterName(statement);
    assert!(
        leaky.to_string().contains(KEK),
        "premise of this test: this variant's Display does carry the statement"
    );
    let rendered = redacted_sqlite_message(&leaky);
    assert!(
        !rendered.contains(KEK),
        "the redacted message leaked the KEK: {rendered}"
    );
    assert!(
        !rendered.contains("PRAGMA"),
        "the redacted message leaked the statement: {rendered}"
    );
}

#[wasm_bindgen_test]
fn the_version_getters_agree_with_dilla_core_on_wasm() {
    assert_eq!(core_version(), dilla_core::CORE_VERSION);
    assert_eq!(abi_version(), BROWSER_ABI_VERSION);
    assert_eq!(BROWSER_ABI_VERSION, 15);
}

/// L-WASM-34: `call_media_epoch` hands its CBOR to JavaScript as a copy made straight from the
/// core's buffer, which is then zero-filled in place: no un-zeroed key bytes stay in linear memory.
#[wasm_bindgen_test]
fn the_media_epoch_crosses_as_a_copy_and_its_buffer_is_zeroed() {
    let mut e = Encoder::new();
    e.array(5)
        .uint(7)
        .bytes(&[0x5b; 16])
        .uint(0)
        .array(1)
        .array(3)
        .uint(1)
        .bytes(&[0xb3; 16])
        .bytes(&[0xb2; 16]);
    e.text("00000 11111 22222 33333 44444 55555");
    let mut buffer = e.into_vec();
    let expected = buffer.clone();
    let out = media_epoch_out(&mut buffer);
    assert_eq!(
        out.to_vec(),
        expected,
        "the copy is the core's CBOR, byte for byte"
    );
    assert!(
        buffer.iter().all(|b| *b == 0),
        "the core's buffer is zeroed"
    );
    let (epoch, key) = decode_strict(&out.to_vec(), |d| {
        d.array(5)?;
        let epoch = d.uint()?;
        let key = d.bytes_exact::<16>()?;
        d.skip()?;
        d.skip()?;
        d.text()?;
        Ok((epoch, key))
    })
    .expect("[epoch, base_key, own_leaf, roster, code]");
    assert_eq!((epoch, key), (7, [0x5b; 16]));
}

/// Review M6, a source pin: `CoreHandle::call_media_epoch` hands JavaScript the copy
/// `media_epoch_out` makes from the core's `Zeroizing` buffer. A `Box<[u8]>` return would leave
/// wasm-bindgen's copy of the key in freed linear memory, and the TypeScript type would not show it.
#[wasm_bindgen_test]
fn call_media_epoch_returns_the_copied_and_zeroed_buffer() {
    let src = include_str!("../src/facade.rs");
    let start = src
        .find("pub fn call_media_epoch(")
        .expect("CoreHandle::call_media_epoch");
    let body = &src[start..];
    let body = &body[..body.find("\n    }\n").expect("end of the method")];
    assert!(
        body.contains("-> Result<js_sys::Uint8Array, JsError>"),
        "returns a JavaScript-owned copy, not Box<[u8]>:\n{body}"
    );
    assert!(
        body.contains("Zeroizing::new(self.with_core(|c| c.call_media_epoch(&group_id))?)"),
        "the core's buffer is Zeroizing:\n{body}"
    );
    assert!(
        body.contains("Ok(media_epoch_out(&mut epoch))"),
        "the copy is made by media_epoch_out, which zero-fills the buffer:\n{body}"
    );
    assert!(!body.contains("into_boxed_slice"), "no boxed copy:\n{body}");
}

/// The message a `JsError` carries across the boundary: what the media worker matches on.
fn message(e: JsError) -> String {
    js_sys::Error::from(JsValue::from(e)).message().into()
}

#[wasm_bindgen_test]
fn a_wrong_length_argument_is_core_input_naming_the_argument() {
    assert_eq!(
        fixed_arg::<16>("group_id", &[0x5a; 15]).unwrap_err(),
        ClientError {
            code: "E_CORE_INPUT",
            detail: "group_id: expected 16 bytes, got 15".to_owned()
        }
    );
    assert_eq!(
        fixed_arg::<32>("nonce", &[]).unwrap_err(),
        ClientError {
            code: "E_CORE_INPUT",
            detail: "nonce: expected 32 bytes, got 0".to_owned()
        }
    );
    let long = fixed_arg::<16>("msg_id", &[0x5a; 17]).unwrap_err();
    assert_eq!(long.detail, "msg_id: expected 16 bytes, got 17");
    assert!(
        !long.detail.contains("5a"),
        "the detail must not echo the argument bytes"
    );
    assert_eq!(fixed_arg::<16>("msg_id", &[7; 16]).unwrap(), [7u8; 16]);
}

#[wasm_bindgen_test]
fn a_purpose_other_than_0_or_1_is_core_input_before_narrowing() {
    let wrapped = purpose_arg(256).unwrap_err();
    assert_eq!(
        wrapped,
        ClientError {
            code: "E_CORE_INPUT",
            detail: "purpose: 256 is not 0 or 1".to_owned()
        }
    );
    assert_eq!(
        message(client_js_error(&wrapped)),
        "E_CORE_INPUT: purpose: 256 is not 0 or 1"
    );
    let two = purpose_arg(2).unwrap_err();
    assert_eq!(
        message(client_js_error(&two)),
        "E_CORE_INPUT: purpose: 2 is not 0 or 1"
    );
    assert_eq!(purpose_arg(0), Ok(0));
    assert_eq!(purpose_arg(1), Ok(1));
}

/// L-WASM-20: a community is absent (a DM) or exactly 16 bytes, checked before the core is called.
#[wasm_bindgen_test]
fn a_community_argument_is_absent_or_exactly_16_bytes() {
    assert_eq!(community_arg(None), Ok(None));
    assert_eq!(community_arg(Some(&[0x22; 16])), Ok(Some([0x22u8; 16])));
    let short = community_arg(Some(&[0x5a; 15])).unwrap_err();
    assert_eq!(
        short,
        ClientError {
            code: "E_CORE_INPUT",
            detail: "community_id: expected 16 bytes, got 15".to_owned()
        }
    );
    assert_eq!(
        message(client_js_error(&short)),
        "E_CORE_INPUT: community_id: expected 16 bytes, got 15"
    );
    assert_eq!(
        community_arg(Some(&[])).unwrap_err().detail,
        "community_id: expected 16 bytes, got 0"
    );
    assert_eq!(
        community_arg(Some(&[0x5a; 17])).unwrap_err().detail,
        "community_id: expected 16 bytes, got 17"
    );
}

/// L-WASM-20 / card 32: the enrolment login is 1..=256 bytes and is refused in the facade before the
/// core is borrowed; the detail never echoes the bytes.
#[wasm_bindgen_test]
fn a_login_is_1_to_256_bytes_before_the_core_is_called() {
    assert_eq!(login_arg(&[0x61; 1]), Ok(()));
    assert_eq!(login_arg(&[0x61; 256]), Ok(()));
    let long = login_arg(&[0x61; 257]).unwrap_err();
    assert_eq!(
        long,
        ClientError {
            code: "E_CORE_INPUT",
            detail: "login: expected 1..=256 bytes, got 257".to_owned()
        }
    );
    assert!(
        !long.detail.contains("aaaa"),
        "the detail must not echo the login"
    );
    assert_eq!(
        message(client_js_error(&long)),
        "E_CORE_INPUT: login: expected 1..=256 bytes, got 257"
    );
    assert_eq!(
        login_arg(&[]).unwrap_err().detail,
        "login: expected 1..=256 bytes, got 0"
    );
}

#[wasm_bindgen_test]
fn a_client_error_crosses_as_its_display() {
    let bare = ClientError {
        code: "E_CORE_NO_IDENTITY",
        detail: String::new(),
    };
    assert_eq!(message(client_js_error(&bare)), "E_CORE_NO_IDENTITY");
    let detailed = ClientError {
        code: "E_CORE_INPUT",
        detail: "limit: 0 is outside 1..=200".to_owned(),
    };
    assert_eq!(
        message(client_js_error(&detailed)),
        "E_CORE_INPUT: limit: 0 is outside 1..=200"
    );
    // C4x (web3.6): a store written by a newer client crosses as its own code with both numbers,
    // the exact text the worker reads (core-port.ts `storeNewerOf`).
    let newer = ClientError::from(dilla_core::mls::StorageError::StoreNewer {
        found: 5,
        supported: 4,
    });
    assert_eq!(
        message(client_js_error(&newer)),
        "E_STORE_NEWER: found 5, supported 4"
    );
}

#[wasm_bindgen_test]
async fn core_open_hands_back_the_store_rejection_unchanged() {
    let cfg = StoreOpenConfig::new(
        "dilla/node".to_owned(),
        "dilla.db".to_owned(),
        "not-a-key".to_owned(),
    );
    let err = match core_open(cfg).await {
        Ok(_) => panic!("core_open accepted a malformed KEK"),
        Err(e) => e,
    };
    let text: String = js_sys::Error::from(err).message().into();
    assert_eq!(text, "E_STORE_KEK: kek_hex must be 64 hex characters");
}

const BASE: [u8; 16] = [0x0a; 16];
const ALICE: [u8; 16] = [0xa1; 16];
const BOB: [u8; 16] = [0xb2; 16];

/// Alice's receiver in epoch 7: Alice is leaf 0, Bob leaf 1.
fn alice_receiver() -> MediaReceiver {
    let mut r = MediaReceiver::new();
    let devices: Vec<u8> = ALICE.iter().chain(BOB.iter()).copied().collect();
    r.install_epoch(7, BASE.to_vec(), &[0, 1], &devices, 0, 1_000.0)
        .unwrap();
    r
}

#[wasm_bindgen_test]
fn a_media_sender_and_receiver_round_trip_every_codec() {
    let mut bob = MediaSender::new(BASE.to_vec(), 1, 7, 7).unwrap();
    let mut alice = alice_receiver();
    let h264 = unhex("000000016742c01fda0280f68078442350000000016588842100000312ff");
    for (codec, slot, frame) in [
        (0u8, 0u8, unhex("fc0102030405060708")),
        (1, 1, unhex("5002009d012a8002e0010102030405060708")),
        (1, 1, unhex("310102030405060708")),
        (2, 2, unhex("8249834200")),
        (3, 1, h264),
    ] {
        let sealed = bob.encrypt(codec, slot, 0, &frame).unwrap();
        assert_ne!(&sealed[..], &frame[..]);
        let opened = alice.decrypt(codec, &sealed, &BOB, slot, 1_010.0).unwrap();
        assert_eq!(hex(&opened), hex(&frame), "codec {codec}");
    }
    assert!(!bob.exhausted());
}

#[wasm_bindgen_test]
fn a_rekeyed_sender_is_held_as_unknown_until_its_epoch_is_installed() {
    let mut bob = MediaSender::new(BASE.to_vec(), 1, 7, 7).unwrap();
    let mut alice = alice_receiver();
    bob.rekey(vec![0x0b; 16], 1, 8).unwrap();
    let sealed = bob.encrypt(0, 0, 0, &unhex("fc01")).unwrap();
    let err = alice.decrypt(0, &sealed, &BOB, 0, 1_020.0).unwrap_err();
    assert_eq!(message(err), "E_SFRAME_UNKNOWN_KID");
    let devices: Vec<u8> = ALICE.iter().chain(BOB.iter()).copied().collect();
    alice
        .install_epoch(8, vec![0x0b; 16], &[0, 1], &devices, 0, 1_030.0)
        .unwrap();
    assert_eq!(
        hex(&alice.decrypt(0, &sealed, &BOB, 0, 1_040.0).unwrap()),
        "fc01"
    );
}

/// An Opus frame from Bob (leaf 1, epoch 7) whose KID also has bit 24 set, sealed under the key
/// that KID derives: a receiver that did not check the KID's range would authenticate it.
fn non_canonical_frame() -> Vec<u8> {
    use dilla_core::sframe::{Codec, Ctr, FrameKey, Kid, protect};
    let kid = Kid::from_raw((1 << 24) | Kid::new(1, 7).value());
    let ctr = Ctr::new(0, 0, 0).unwrap();
    protect(
        &FrameKey::derive(&BASE, kid),
        kid,
        ctr,
        Codec::Opus,
        &unhex("fc01"),
    )
    .unwrap()
}

#[wasm_bindgen_test]
fn media_errors_are_bare_codes() {
    let mut bob = MediaSender::new(BASE.to_vec(), 1, 7, 7).unwrap();
    let mut alice = alice_receiver();
    let sealed = bob.encrypt(0, 0, 0, &unhex("fc01")).unwrap();
    for (got, want) in [
        (
            alice.decrypt(0, &sealed, &ALICE, 0, 1_010.0).unwrap_err(),
            "E_SFRAME_SENDER_MISMATCH",
        ),
        (
            alice.decrypt(0, &sealed, &BOB, 1, 1_010.0).unwrap_err(),
            "E_SFRAME_SLOT_MISMATCH",
        ),
        (
            alice.decrypt(9, &sealed, &BOB, 0, 1_010.0).unwrap_err(),
            "E_SFRAME_UNSUPPORTED_CODEC",
        ),
        (
            alice.decrypt(0, &sealed, &BOB, 4, 1_010.0).unwrap_err(),
            "E_SFRAME_SLOT_MISMATCH",
        ),
        (
            alice
                .decrypt(0, &sealed, &[0u8; 3], 0, 1_010.0)
                .unwrap_err(),
            "E_BAD_OPTIONS",
        ),
        // CRYPTO-10: an empty identity is not "no sender binding".
        (
            alice.decrypt(0, &sealed, &[], 0, 1_010.0).unwrap_err(),
            "E_BAD_OPTIONS",
        ),
        (
            bob.encrypt(1, 1, 0, &unhex("5002")).unwrap_err(),
            "E_SFRAME_MALFORMED_PREFIX",
        ),
        (
            bob.encrypt(0, 0, 16, &unhex("fc")).unwrap_err(),
            "E_SFRAME_LAYER_RANGE",
        ),
        (
            MediaSender::new(BASE.to_vec(), 70_000, 7, 7).err().unwrap(),
            "E_SFRAME_LEAF_RANGE",
        ),
        (
            MediaSender::new(BASE.to_vec(), 1, 6, 7).err().unwrap(),
            "E_SFRAME_STALE_EPOCH",
        ),
        (
            MediaSender::new(vec![0u8; 15], 1, 7, 7).err().unwrap(),
            "E_BAD_OPTIONS",
        ),
        (
            sframe_derive(&BASE, 65_536, 0).err().unwrap(),
            "E_SFRAME_LEAF_RANGE",
        ),
        // A raw KID from JavaScript goes through the same canonical check as a received one.
        (
            sframe_header(1 << 24, 0).err().unwrap(),
            "E_SFRAME_NON_CANONICAL_KID",
        ),
        // Bob's KID (leaf 1, epoch 7) with bit 24 set: refused while parsing, not held.
        (
            alice
                .decrypt(0, &non_canonical_frame(), &BOB, 0, 1_010.0)
                .unwrap_err(),
            "E_SFRAME_NON_CANONICAL_KID",
        ),
        // Bob's KID 0x000107 in three bytes instead of two.
        (
            alice
                .decrypt(
                    0,
                    &unhex(&format!("a0000107{}", "00".repeat(17))),
                    &BOB,
                    0,
                    1_010.0,
                )
                .unwrap_err(),
            "E_SFRAME_NON_MINIMAL_HEADER",
        ),
    ] {
        assert_eq!(message(got), want);
    }
    // The same frame again on the right track is accepted once, then replayed.
    assert!(alice.decrypt(0, &sealed, &BOB, 0, 1_010.0).is_ok());
    assert_eq!(
        message(alice.decrypt(0, &sealed, &BOB, 0, 1_010.0).unwrap_err()),
        "E_SFRAME_REPLAY"
    );
    let mut wrong = MediaReceiver::new();
    assert_eq!(
        message(
            wrong
                .install_epoch(7, BASE.to_vec(), &[0, 1], &ALICE, 0, 0.0)
                .unwrap_err()
        ),
        "E_BAD_OPTIONS"
    );
}

const ENVELOPE_JSON: &str = include_str!("../../../protocol/vectors/envelope.json");

/// web-2b task 1 (L-CORE-30): the JSON surface carries the attachment's ninth element, `name`.
#[wasm_bindgen_test]
fn the_wasm_bindgen_surface_round_trips_every_envelope_vector_with_its_name() {
    let doc: serde_json::Value = serde_json::from_str(ENVELOPE_JSON).unwrap();
    let cases = doc["cases"].as_array().unwrap();
    assert_eq!(cases.len(), 5);
    for case in cases {
        let cbor = case["cbor"].as_str().unwrap();
        let json = envelope_decode_json(&unhex(cbor)).unwrap();
        assert_eq!(
            hex(&envelope_encode(&json).unwrap()),
            cbor,
            "{}",
            case["name"]
        );
    }
    let first = envelope_decode_json(&unhex(cases[0]["cbor"].as_str().unwrap())).unwrap();
    assert!(first.contains("\"name\":\"wolf-capes.jpg\""), "{first}");
}

/// web3.5 R1: authority_set's pinners, broadcasters and mentionable are each 0..=4096 ids of 16 bytes, refused
/// in the facade before the core is borrowed; the detail names the list and length, never the bytes.
#[wasm_bindgen_test]
fn authority_ids_are_zero_to_4096_ids_of_sixteen_bytes() {
    assert_eq!(authority_ids_arg("pinners", &[]), Ok(()));
    assert_eq!(authority_ids_arg("pinners", &[0x5a; 16]), Ok(()));
    assert_eq!(
        authority_ids_arg("broadcasters", &vec![0x5a; 4096 * 16]),
        Ok(())
    );
    let short = authority_ids_arg("pinners", &[0x5a; 15]).unwrap_err();
    assert_eq!(
        short,
        ClientError {
            code: "E_CORE_INPUT",
            detail: "pinners: expected 0..=4096 ids of 16 bytes, got 15 bytes".to_owned()
        }
    );
    assert!(
        !short.detail.contains("5a"),
        "the detail must not echo the ids"
    );
    assert_eq!(
        authority_ids_arg("broadcasters", &vec![0x5a; 4097 * 16])
            .unwrap_err()
            .detail,
        "broadcasters: expected 0..=4096 ids of 16 bytes, got 65552 bytes"
    );
    // The mentionable role ids (W2a review finding 1) take the same bound.
    assert_eq!(
        authority_ids_arg("mentionable", &[0x5a; 17])
            .unwrap_err()
            .detail,
        "mentionable: expected 0..=4096 ids of 16 bytes, got 17 bytes"
    );
}

/// L-WASM-30: own_roles_set's role ids are 0..=64 ids of 16 bytes, refused in the facade before the
/// core is borrowed; the detail names the length and never the bytes.
#[wasm_bindgen_test]
fn role_ids_are_zero_to_sixty_four_ids_of_sixteen_bytes() {
    assert_eq!(role_ids_arg(&[]), Ok(()));
    assert_eq!(role_ids_arg(&[0x5a; 16]), Ok(()));
    assert_eq!(role_ids_arg(&[0x5a; 1024]), Ok(()));
    let odd = role_ids_arg(&[0x5a; 17]).unwrap_err();
    assert_eq!(
        odd,
        ClientError {
            code: "E_CORE_INPUT",
            detail: "role_ids: expected 0..=64 ids of 16 bytes, got 17 bytes".to_owned()
        }
    );
    assert!(
        !odd.detail.contains("5a"),
        "the detail must not echo the ids"
    );
    assert_eq!(
        message(client_js_error(&odd)),
        "E_CORE_INPUT: role_ids: expected 0..=64 ids of 16 bytes, got 17 bytes"
    );
    assert_eq!(
        role_ids_arg(&[0x5a; 1040]).unwrap_err().detail,
        "role_ids: expected 0..=64 ids of 16 bytes, got 1040 bytes"
    );
}

/// web3.7 TB1, a source pin: `thread_timeline` and `threads` are exported in the `timeline`
/// pattern (each 16-byte id checked by name before the core is borrowed, the CBOR handed back as
/// a boxed copy) and call the core's reads of the same name. ABI 11 is unchanged (R3).
#[wasm_bindgen_test]
fn the_thread_reads_are_exported_in_the_timeline_pattern() {
    let src = include_str!("../src/facade.rs");
    let method = |name: &str| {
        let start = src
            .find(&format!("pub fn {name}("))
            .unwrap_or_else(|| panic!("CoreHandle::{name}"));
        let body = &src[start..];
        body[..body.find("\n    }\n").expect("end of the method")].to_owned()
    };
    let thread = method("thread_timeline");
    for want in [
        "-> Result<Box<[u8]>, JsError>",
        "js_fixed_arg::<16>(\"group_id\", group_id)?",
        "js_fixed_arg::<16>(\"root_msg_id\", root_msg_id)?",
        "c.thread_timeline(&group_id, &root_msg_id, before_seq, limit)",
        ".map(Vec::into_boxed_slice)",
    ] {
        assert!(
            thread.contains(want),
            "thread_timeline lacks {want}:\n{thread}"
        );
    }
    let threads = method("threads");
    for want in [
        "-> Result<Box<[u8]>, JsError>",
        "js_fixed_arg::<16>(\"group_id\", group_id)?",
        "c.threads(&group_id)",
        ".map(Vec::into_boxed_slice)",
    ] {
        assert!(threads.contains(want), "threads lacks {want}:\n{threads}");
    }
    assert_eq!(
        fixed_arg::<16>("root_msg_id", &[0x5a; 15])
            .unwrap_err()
            .detail,
        "root_msg_id: expected 16 bytes, got 15"
    );
}

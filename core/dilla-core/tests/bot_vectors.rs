use dilla_core::bot::group_registration::{
    Receipt as GroupRegistrationReceipt, Request as GroupRegistrationRequest,
};
use dilla_core::bot::keypackage::{
    Intent, IntentResult, Publication, PublicationReceipt, Resolution,
};
use dilla_core::bot::{
    Event, HelperRequest, HelperResponse, OriginalSendResponse, Receipt, RemovalProof, Response,
    SendOutcome,
};
use dilla_core::bot::{Request, RequestArgs};
use dilla_core::cbor::Encoder;
use dilla_core::ids::{ChannelId, CommunityId, DeviceId, MsgId, UserId};
use serde_json::Value;
use std::collections::BTreeSet;

#[test]
fn overview_request_body_round_trips_canonically() {
    let request = Request {
        request_id: MsgId::from_bytes([1; 16]),
        community_id: CommunityId::from_bytes([2; 16]),
        channel_id: None,
        operation: 0,
        revision: None,
        expires: None,
        args: RequestArgs::None,
    };
    let body = request.encode_body().expect("valid overview request");
    assert!(body.starts_with("dilla.bot/1:"));
    assert!(Request::decode_body(body.as_bytes()).unwrap() == request);
}

#[test]
fn application_context_is_checked_explicitly() {
    let request = Request {
        request_id: MsgId::from_bytes([1; 16]),
        community_id: CommunityId::from_bytes([2; 16]),
        channel_id: None,
        operation: 0,
        revision: None,
        expires: None,
        args: RequestArgs::None,
    };
    assert!(
        request
            .validate_context(CommunityId::from_bytes([2; 16]), 100)
            .is_ok()
    );
    assert!(
        request
            .validate_context(CommunityId::from_bytes([3; 16]), 100)
            .is_err()
    );
}

#[test]
fn role_panel_publish_request_round_trips_and_requires_a_bounded_mapping_array() {
    let mut mappings = Encoder::new();
    mappings
        .array(1)
        .array(3)
        .text("📚")
        .bytes(&[5; 16])
        .text("Readers");
    let request = Request {
        request_id: MsgId::from_bytes([1; 16]),
        community_id: CommunityId::from_bytes([2; 16]),
        channel_id: None,
        operation: 31,
        revision: Some(0),
        expires: Some(120),
        args: RequestArgs::RolePanel {
            channel: ChannelId::from_bytes([4; 16]),
            mappings: mappings.into_vec(),
        },
    };
    let encoded = request
        .encode_cbor()
        .expect("role-panel publish is a valid mutation");
    assert!(Request::decode_cbor(&encoded).unwrap() == request);
    assert!(
        request
            .validate_context(CommunityId::from_bytes([2; 16]), 100)
            .is_ok()
    );

    let mut nonzero_revision = request.clone();
    nonzero_revision.revision = Some(1);
    assert!(nonzero_revision.encode_cbor().is_err());

    let mut missing_expiry = request.clone();
    missing_expiry.expires = None;
    assert!(missing_expiry.encode_cbor().is_err());
    let mut empty_mappings = request;
    if let RequestArgs::RolePanel { mappings, .. } = &mut empty_mappings.args {
        *mappings = vec![0x80];
    }
    assert!(empty_mappings.encode_cbor().is_err());
}

#[test]
fn send_receipt_encoder_rejects_invalid_typed_values() {
    let base = Receipt {
        operation_id: MsgId::from_bytes([1; 16]),
        attempt_id: MsgId::from_bytes([2; 16]),
        predecessor_attempt_id: None,
        community_id: CommunityId::from_bytes([3; 16]),
        group_id: MsgId::from_bytes([4; 16]),
        expected_generation: 0,
        ciphertext_sha256: [0; 32],
        class: 0,
        outcome: SendOutcome::ClosedNotAccepted,
        epoch: 0,
        send_response: None,
    };
    assert!(base.encode().is_ok());

    let mut response = Encoder::new();
    response.array(3).uint(1).bytes(&[0; 32]).uint(0);
    let response = OriginalSendResponse {
        sequence: 1,
        franking_tag: [0; 32],
        receive_timestamp: 0,
        body: response.into_vec(),
    };
    let mut accepted = base.clone();
    accepted.outcome = SendOutcome::Accepted;
    accepted.send_response = Some(response.clone());
    assert!(accepted.encode().is_ok());

    let mut invalid = base.clone();
    invalid.class = 2;
    assert!(invalid.encode().is_err());
    invalid = base.clone();
    invalid.expected_generation = i64::MAX as u64 + 1;
    assert!(invalid.encode().is_err());
    invalid = base.clone();
    invalid.outcome = SendOutcome::Accepted;
    assert!(invalid.encode().is_err());
    invalid = base.clone();
    invalid.send_response = Some(response.clone());
    assert!(invalid.encode().is_err());
    invalid = accepted;
    invalid.send_response.as_mut().unwrap().sequence = 2;
    assert!(invalid.encode().is_err());
}

#[test]
fn keypackage_result_worst_case_fits_helper_payload_by_45_bytes() {
    use dilla_core::bot::keypackage::{
        HELPER_PAYLOAD_MAX, Intent, IntentResult, MAX_UPLOAD_BODY, Publication, Resolution,
    };
    use dilla_core::bot::{HELPER_MAX, HelperResponse};
    // [upload_bstr, refs_array, greatest_expiry]: a 5-byte bstr head, a
    // 2-byte array head plus 33*(2-byte bstr head + 32 bytes), and a 9-byte uint.
    let encoded = 1 + 5 + MAX_UPLOAD_BODY + 2 + 33 * 34 + 9;
    assert_eq!(encoded, 1_048_435);
    assert_eq!(HELPER_PAYLOAD_MAX - encoded, 45);
    assert!(
        Intent {
            operation_id: MsgId::from_bytes([1; 16]),
            count: 33,
            last_resort: 0,
            expected_generation: 0,
        }
        .encode()
        .is_err()
    );
    assert!(
        Resolution {
            operation_id: MsgId::from_bytes([1; 16]),
            package_refs: vec![[7; 32], [7; 32]],
            outcome: 0,
            now: 0,
            current_generation: 0,
        }
        .encode()
        .is_err()
    );

    let upload = canonical_keypackage_upload(MAX_UPLOAD_BODY);
    assert_eq!(upload.len(), MAX_UPLOAD_BODY);
    let package_refs: Vec<[u8; 32]> = (1..=33).map(|value| [value; 32]).collect();
    let result = IntentResult {
        upload_body: upload.clone(),
        package_refs,
        greatest_expiry: i64::MAX as u64,
    };
    let encoded_result = result.encode().expect("maximum op5 result encodes");
    assert_eq!(encoded_result.len(), encoded);
    assert!(encoded_result.len() <= HELPER_PAYLOAD_MAX);
    let decoded_result = IntentResult::decode(&encoded_result).unwrap();
    assert_eq!(decoded_result, result);
    assert_eq!(decoded_result.encode().unwrap(), encoded_result);

    let publication = Publication {
        operation_id: MsgId::from_bytes([0x96; 16]),
        expected_generation: i64::MAX as u64,
        upload_body: upload.clone(),
    };
    let encoded_publication = publication.encode().expect("maximum publication encodes");
    let decoded_publication = Publication::decode(&encoded_publication).unwrap();
    assert_eq!(decoded_publication, publication);
    assert_eq!(decoded_publication.encode().unwrap(), encoded_publication);

    let helper = HelperResponse {
        request_id: MsgId::from_bytes([0x97; 16]),
        status: 0,
        payload: encoded_result.clone(),
    };
    let helper_bytes = helper.encode().expect("maximum helper response encodes");
    assert_eq!(helper_bytes.len(), 1_048_460);
    assert!(helper_bytes.len() <= HELPER_MAX);
    let decoded_helper = HelperResponse::decode(&helper_bytes).unwrap();
    assert_eq!(decoded_helper.request_id, helper.request_id);
    assert_eq!(decoded_helper.status, helper.status);
    assert_eq!(decoded_helper.payload, helper.payload);

    let oversized_upload = canonical_keypackage_upload(MAX_UPLOAD_BODY + 1);
    assert_eq!(oversized_upload.len(), MAX_UPLOAD_BODY + 1);
    let oversized_publication = Publication {
        upload_body: oversized_upload.clone(),
        ..publication
    };
    assert!(oversized_publication.encode().is_err());
    let mut publication_cbor = Encoder::new();
    publication_cbor
        .array(3)
        .bytes(oversized_publication.operation_id.as_bytes())
        .uint(oversized_publication.expected_generation)
        .bytes(&oversized_upload);
    assert!(Publication::decode(&publication_cbor.into_vec()).is_err());

    let oversized_result = IntentResult {
        upload_body: oversized_upload.clone(),
        package_refs: (1..=33).map(|value| [value; 32]).collect(),
        greatest_expiry: i64::MAX as u64,
    };
    assert!(oversized_result.encode().is_err());
    let mut result_cbor = Encoder::new();
    result_cbor.array(3).bytes(&oversized_upload).array(33);
    for value in 1..=33 {
        result_cbor.bytes(&[value; 32]);
    }
    result_cbor.uint(i64::MAX as u64);
    assert!(IntentResult::decode(&result_cbor.into_vec()).is_err());
}

fn canonical_keypackage_upload(target_len: usize) -> Vec<u8> {
    use dilla_core::cbor::Encoder;
    let mut encoder = Encoder::new();
    encoder
        .array(2)
        .array(32)
        .bytes(&vec![0x11; target_len - 72]);
    for _ in 0..31 {
        encoder.bytes(&[0x22]);
    }
    encoder.bytes(&[0x33]);
    encoder.into_vec()
}

#[test]
fn request_encoders_enforce_document_byte_limits() {
    for (limit, operation) in [(2400, 2), (2200, 51)] {
        let exact = document_of_encoded_len(limit);
        let over = document_of_encoded_len(limit + 1);
        let exact_request = document_request(operation, exact);
        let over_request = document_request(operation, over);
        assert!(
            exact_request.encode_cbor().is_ok(),
            "operation {operation} at limit"
        );
        assert!(
            over_request.encode_cbor().is_err(),
            "operation {operation} over limit"
        );
    }
}

fn document_of_encoded_len(size: usize) -> Vec<u8> {
    // One definite array containing a bstr; the bstr head is 3 bytes at these lengths.
    let mut e = Encoder::new();
    e.array(1).bytes(&vec![0; size - 4]);
    e.into_vec()
}

fn document_request(operation: u64, document: Vec<u8>) -> Request {
    Request {
        request_id: MsgId::from_bytes([1; 16]),
        community_id: CommunityId::from_bytes([2; 16]),
        channel_id: None,
        operation,
        revision: Some(1),
        expires: Some(120),
        args: if operation == 2 {
            RequestArgs::SettingsPut {
                section: 0,
                document,
            }
        } else {
            RequestArgs::AnnouncementPut {
                schedule: None,
                document,
            }
        },
    }
}

#[test]
fn shared_bot_corpus_round_trips_and_rejects() {
    let corpus: Value = serde_json::from_str(include_str!("../../../protocol/vectors/bot.json"))
        .expect("shared bot corpus parses");
    let cases = corpus["cases"].as_array().unwrap();
    let rejects = corpus["rejects"].as_array().unwrap();
    assert!(!cases.is_empty());
    assert!(!rejects.is_empty());
    let mut operations = BTreeSet::new();
    let mut event_kinds = BTreeSet::new();
    let format_kinds: BTreeSet<&str> = cases
        .iter()
        .map(|case| case["kind"].as_str().unwrap())
        .collect();
    assert_eq!(
        format_kinds,
        [
            "request",
            "response",
            "removal",
            "event",
            "receipt",
            "helper_request",
            "helper_response",
            "keypackage_publication",
            "keypackage_publication_receipt",
            "keypackage_intent",
            "keypackage_intent_result",
            "keypackage_resolution",
            "group_registration_request",
            "group_registration_receipt"
        ]
        .into_iter()
        .collect()
    );
    let reject_names: BTreeSet<&str> = rejects
        .iter()
        .filter_map(|case| case["name"].as_str())
        .collect();
    assert!(reject_names.contains("removal actor user mismatch"));
    assert!(reject_names.contains("removal actor device mismatch"));

    for case in cases {
        let kind = case["kind"].as_str().unwrap();
        let raw = hex::decode(case["hex"].as_str().unwrap()).unwrap();
        match kind {
            "request" => {
                let value = Request::decode_cbor(&raw).unwrap();
                operations.insert(value.operation);
                assert_eq!(value.encode_cbor().unwrap(), raw, "{}", case["name"]);
                if let Some(body) = case["body"].as_str() {
                    assert!(Request::decode_body(body.as_bytes()).unwrap() == value);
                    assert_eq!(value.encode_body().unwrap(), body);
                }
                context(case, |c, now, _g, _u, _d| {
                    value.validate_context(c, now).is_ok()
                });
            }
            "response" => {
                let value = Response::decode_cbor(&raw).unwrap();
                assert_eq!(value.encode_cbor().unwrap(), raw, "{}", case["name"]);
                if let Some(body) = case["body"].as_str() {
                    assert_eq!(
                        Response::decode_body(body.as_bytes())
                            .unwrap()
                            .encode_body()
                            .unwrap(),
                        body
                    );
                }
                context(case, |c, _now, _g, _u, _d| {
                    value.validate_context(c).is_ok()
                });
            }
            "removal" => {
                let value = RemovalProof::decode(&raw).unwrap();
                assert_eq!(value.encode().unwrap(), raw, "{}", case["name"]);
                assert_eq!(
                    hex::encode(value.signature_preimage().unwrap()),
                    case["preimage_hex"].as_str().unwrap()
                );
                context(case, |c, now, g, u, d| {
                    value.validate_context(c, g, u, d, now).is_ok()
                });
            }
            "event" => {
                let value = Event::decode(&raw).unwrap();
                event_kinds.insert(value.kind);
                assert_eq!(value.encode().unwrap(), raw, "{}", case["name"]);
                context(case, |c, _now, _g, _u, _d| {
                    value.validate_context(c).is_ok()
                });
            }
            "receipt" => {
                let value = Receipt::decode(&raw).unwrap();
                assert_eq!(value.encode().unwrap(), raw, "{}", case["name"]);
                context(case, |c, _now, g, _u, _d| {
                    value.validate_context(c, g).is_ok()
                });
            }
            "helper_request" => {
                let value = HelperRequest::decode(&raw).unwrap();
                assert_eq!(value.encode().unwrap(), raw, "{}", case["name"]);
                let frame = value.encode_frame().unwrap();
                assert_eq!(
                    HelperRequest::decode_frame(&frame)
                        .unwrap()
                        .encode()
                        .unwrap(),
                    raw
                );
            }
            "helper_response" => {
                let value = HelperResponse::decode(&raw).unwrap();
                assert_eq!(value.encode().unwrap(), raw, "{}", case["name"]);
                let frame = value.encode_frame().unwrap();
                assert_eq!(
                    HelperResponse::decode_frame(&frame)
                        .unwrap()
                        .encode()
                        .unwrap(),
                    raw
                );
            }
            "keypackage_publication" => {
                let value = Publication::decode(&raw).unwrap();
                assert_eq!(value.encode().unwrap(), raw, "{}", case["name"]);
            }
            "keypackage_publication_receipt" => {
                let value = PublicationReceipt::decode(&raw).unwrap();
                assert_eq!(value.encode().unwrap(), raw, "{}", case["name"]);
            }
            "keypackage_intent" => {
                let value = Intent::decode(&raw).unwrap();
                assert_eq!(value.encode().unwrap(), raw, "{}", case["name"]);
            }
            "keypackage_intent_result" => {
                let value = IntentResult::decode(&raw).unwrap();
                assert_eq!(value.encode().unwrap(), raw, "{}", case["name"]);
            }
            "keypackage_resolution" => {
                let value = Resolution::decode(&raw).unwrap();
                assert_eq!(value.encode().unwrap(), raw, "{}", case["name"]);
            }
            "group_registration_request" => {
                let value = GroupRegistrationRequest::decode(&raw).unwrap();
                assert_eq!(value.encode().unwrap(), raw, "{}", case["name"]);
                assert_eq!(
                    hex::encode(&value.registration_body),
                    case["registration_body_hex"].as_str().unwrap()
                );
            }
            "group_registration_receipt" => {
                let value = GroupRegistrationReceipt::decode(&raw).unwrap();
                assert_eq!(value.encode().unwrap(), raw, "{}", case["name"]);
            }
            other => panic!("unknown bot vector kind: {other}"),
        }
    }

    assert_eq!(
        operations,
        [
            0, 1, 2, 3, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 30, 31, 40, 50,
            51, 52, 60, 61, 62, 63, 64, 70, 71, 72, 73, 74, 75, 80, 81, 82, 83, 84
        ]
        .into_iter()
        .collect()
    );
    assert_eq!(event_kinds, [1, 2, 3, 4, 5, 6, 7, 8].into_iter().collect());

    for case in rejects {
        let kind = case["kind"].as_str().unwrap();
        let raw = case["hex"].as_str().map(hex::decode).transpose().unwrap();
        let body = case["body"].as_str();
        let accepted = match kind {
            "request" => {
                let parsed = body
                    .map(|s| Request::decode_body(s.as_bytes()))
                    .unwrap_or_else(|| Request::decode_cbor(raw.as_ref().unwrap()));
                match parsed {
                    Err(_) => false,
                    Ok(value) => context_result(case, |c, now, _g, _u, _d| {
                        value.validate_context(c, now).is_ok()
                    }),
                }
            }
            "response" => raw.as_ref().is_some_and(|r| {
                Response::decode_cbor(r).is_ok_and(|value| {
                    context_result(case, |c, _now, _g, _u, _d| {
                        value.validate_context(c).is_ok()
                    })
                })
            }),
            "removal" => raw.as_ref().is_some_and(|r| {
                RemovalProof::decode(r).is_ok_and(|value| {
                    context_result(case, |c, now, g, u, d| {
                        value.validate_context(c, g, u, d, now).is_ok()
                    })
                })
            }),
            "event" => raw.as_ref().is_some_and(|r| {
                Event::decode(r).is_ok_and(|value| {
                    context_result(case, |c, _now, _g, _u, _d| {
                        value.validate_context(c).is_ok()
                    })
                })
            }),
            "receipt" => raw.as_ref().is_some_and(|r| {
                Receipt::decode(r).is_ok_and(|value| {
                    context_result(case, |c, _now, g, _u, _d| {
                        value.validate_context(c, g).is_ok()
                    })
                })
            }),
            "helper_request" if case["name"].as_str() == Some("helper malformed length prefix") => {
                raw.as_ref()
                    .is_some_and(|r| dilla_core::bot::parse_frame(r).is_ok())
            }
            "helper_request" => raw
                .as_ref()
                .is_some_and(|r| HelperRequest::decode(r).is_ok()),
            "helper_response" => raw
                .as_ref()
                .is_some_and(|r| HelperResponse::decode(r).is_ok()),
            "keypackage_publication" => {
                raw.as_ref().is_some_and(|r| Publication::decode(r).is_ok())
            }
            "keypackage_publication_receipt" => raw
                .as_ref()
                .is_some_and(|r| PublicationReceipt::decode(r).is_ok()),
            "keypackage_intent" => raw.as_ref().is_some_and(|r| Intent::decode(r).is_ok()),
            "keypackage_intent_result" => raw
                .as_ref()
                .is_some_and(|r| IntentResult::decode(r).is_ok()),
            "keypackage_resolution" => raw.as_ref().is_some_and(|r| Resolution::decode(r).is_ok()),
            "group_registration_request" => raw
                .as_ref()
                .is_some_and(|r| GroupRegistrationRequest::decode(r).is_ok()),
            "group_registration_receipt" => raw
                .as_ref()
                .is_some_and(|r| GroupRegistrationReceipt::decode(r).is_ok()),
            other => panic!("unknown bot reject kind: {other}"),
        };
        assert!(!accepted, "reject vector accepted: {}", case["name"]);
    }
}

fn context(case: &Value, check: impl FnOnce(CommunityId, u64, MsgId, UserId, DeviceId) -> bool) {
    if case.get("context").is_some() {
        assert!(
            context_result(case, check),
            "context validation failed: {}",
            case["name"]
        );
    }
}

fn context_result(
    case: &Value,
    check: impl FnOnce(CommunityId, u64, MsgId, UserId, DeviceId) -> bool,
) -> bool {
    let Some(ctx) = case.get("context") else {
        return true;
    };
    let community = CommunityId::from_hex(ctx["community"].as_str().unwrap()).unwrap();
    let now = ctx["now"]
        .as_str()
        .map(str::parse)
        .transpose()
        .unwrap()
        .unwrap_or(0);
    let group = ctx["group"]
        .as_str()
        .map(MsgId::from_hex)
        .transpose()
        .unwrap()
        .unwrap_or(MsgId::from_bytes([1; 16]));
    let user = ctx["actor_user"]
        .as_str()
        .map(UserId::from_hex)
        .transpose()
        .unwrap()
        .unwrap_or(UserId::from_bytes([1; 16]));
    let device = ctx["actor_device"]
        .as_str()
        .map(DeviceId::from_hex)
        .transpose()
        .unwrap()
        .unwrap_or(DeviceId::from_bytes([1; 16]));
    check(community, now, group, user, device)
}

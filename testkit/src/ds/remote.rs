//! A synchronous HTTP + WebSocket delivery-service client, over the real `/v1` surface.
//!
//! Everything here speaks the wire a real client will speak: deterministic CBOR bodies, 32-hex
//! path identifiers, `Authorization: Bearer`, and the four-element gateway frame. There is no
//! test-only shortcut on the server side — `dillad serve` has no `--insecure-test-bootstrap`
//! flag, and account creation goes through `dillad init`'s bootstrap invite.
//!
//! The only non-`/v1` origin it talks to is the test host's control listener, whose base URL
//! arrives in `DILLA_TESTKIT_CONTROL` and which speaks JSON: see [`control_post`].

use std::collections::BTreeMap;
use std::net::TcpStream;
use std::time::{Duration, Instant};

use dilla_core::cbor::{CborError, Decoder, Encoder, decode_strict};
use dilla_core::identity::DeviceList;
use dilla_core::ids::DeviceId;
use tungstenite::{Message, WebSocket};

use crate::TestkitError;
use crate::totp::{self, TotpState};

use super::{
    CommitRequest, CommitResult, DeliveryService, Device, DsError, ErrorExtras, Frame, GroupId,
    GroupInfoResp, HandshakeItem, HealRequest, KeyPackageResp, MessageItem, ProposalItem,
    RegisterRequest, RegisterResult, ResyncRequest, TreeResp, UploadResult, WelcomeItem,
    group_id_of,
};

/// The gateway subprotocol `internal/gateway.Handler` negotiates.
const SUBPROTOCOL: &str = "dilla.v1";
/// The current wire version this client identifies with. Human upload and media shapes remain
/// unchanged in wire 3, which the media test host offers by default.
const WIRE_VERSION: u64 = 3;
/// How long a pump waits on an empty socket before it returns. Short, because `drain` is called
/// once per scenario step and the harness runs every step against loopback.
const PUMP_WAIT: Duration = Duration::from_millis(50);
/// How long the upgrade, `hello`, `identify` and `ready` may take together.
const HANDSHAKE_WAIT: Duration = Duration::from_secs(10);
/// Row 4's and row 12's page sizes, the handlers' own defaults.
const HANDSHAKE_PAGE: usize = 256;
const MESSAGE_PAGE: usize = 128;
/// Row 15's page size, the handler's own default.
const WELCOME_PAGE: usize = 64;

pub struct HttpDs {
    base: String,
    token: String,
    device: DeviceId,
    agent: ureq::Agent,
    ws: Option<WebSocket<TcpStream>>,
    pending: Vec<Frame>,
    /// What the scenario asked for with `go_offline` / `go_online`. A socket the instance closed
    /// while this is true is reopened by the next `drain`.
    online: bool,
    /// `hello`'s heartbeat interval; the client beats at half of it.
    heartbeat: Duration,
    last_beat: Instant,
    /// Heartbeats sent on this connection that no `heartbeat_ack` has answered yet. The gateway
    /// answers every heartbeat in order, so a connection that stays open while acks stop coming
    /// back is one that has stopped delivering: `pump` drops it after `ack_timeout` without
    /// progress, and the next `drain` reopens it and catches up — what a real client does.
    unacked: u32,
    /// Since when the oldest unanswered heartbeat has waited without the socket delivering
    /// anything: reset by every frame read and by a pump after a pause longer than the timeout
    /// (the silence while the client was not listening is not the connection's).
    ack_since: Instant,
    /// When the last `pump` finished reading.
    last_pump: Instant,
    /// The highest replay `n` received, which every heartbeat acknowledges.
    last_n: u64,
    /// The highest per-group `seq` delivered as a handshake or a message, per group. A reconnect
    /// catches up from here over rows 4 and 12, so a `drain` after a gap returns what the gap
    /// held — the per-group seq cursors and `GET ?from=` are the truth, not the replay ring.
    delivered: BTreeMap<Vec<u8>, u64>,
    connected_before: bool,
    /// The session's hard expiry in the instance's unix seconds, 0 when unknown: what tells a
    /// runner that has moved the instance clock whether this device must sign in again.
    expires: u64,
    /// Diagnostics for a failed `expect_*`: how many times the socket was reopened, why it was
    /// lost (the last few reasons), and how many frames `accept` queued. None of it steers
    /// behaviour; a scenario that fails on a slow runner reports it instead of "never decrypted".
    reconnects: u64,
    lost: Vec<String>,
    accepted: u64,
    /// How many connections were dropped because a heartbeat went unacknowledged.
    missed_acks: u64,
    /// Op 53 rings in arrival order, kept apart from the delivery-service frames until `take_rings` (web-3,
    /// L-GW-33): a ring is never replayed, so a `drain` that dropped one would lose it for good.
    rings: Vec<CallRing>,
    /// The authenticator-app secret this account enrolled at registration, and the last counter the instance took
    /// (protocol/09 § Second factors); `None` for a session that never enrolled one.
    totp: Option<TotpState>,
}

/// At most this many unclaimed rings are kept; the oldest goes first. A test reads its rings within a few frames.
const MAX_KEPT_RINGS: usize = 1024;

/// One op 53 `call_ring` frame: `[call_id, channel_id, user_id, state, expires_ts]` (web-3, L-GW-33).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CallRing {
    pub call: [u8; 16],
    pub channel: [u8; 16],
    pub user: [u8; 16],
    /// 1 ringing (`user` is the caller), 0 ring over, 2 declined (`user` is who declined).
    pub state: u64,
    pub expires: u64,
}

/// `GET /v1/channels/{id}/call` (web-3, L-HTTP-100): the call group a joiner takes (the live call's, else the
/// channel's newest open one), the live call, and its participants as `(user, device, flags)`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ChannelCall {
    pub group_id: Option<[u8; 16]>,
    pub call_id: Option<[u8; 16]>,
    pub participants: Vec<([u8; 16], [u8; 16], u64)>,
}

/// What `redeem_invite` returns: the identifiers the instance minted.
#[derive(Clone, Debug)]
pub struct Enrolled {
    pub user_id: [u8; 16],
    pub device_id: DeviceId,
    pub token: String,
    /// The first session's hard expiry, in the instance's unix seconds.
    pub expires: u64,
}

/// What `POST /v1/devices/{device_id}/sessions` answers (L-HTTP-54), without the idle expiry and
/// the generation.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Established {
    pub token: String,
    pub scope: u64,
    pub user_id: [u8; 16],
    pub device_id: DeviceId,
    pub expires: u64,
}
/// The registration array an enrolment establish carries in element 3 (L-HTTP-54).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Registration {
    pub device_id: DeviceId,
    pub dsk_pub: [u8; 32],
    pub tier: u8,
    pub signer_tier: u8,
    pub credential: Vec<u8>,
}
/// Element 3 of a session body: the device's credential, or its registration array.
pub enum Element3<'a> {
    Credential(&'a [u8]),
    Registration(&'a Registration),
}
/// `POST /v1/auth/password/login` → `[assertion, needs]` (protocol/09): `0` nothing owed, `1` a TOTP code (or a backup
/// code), `2` a passkey login (or a backup code) for an account that holds no TOTP.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PasswordLogin {
    pub assertion: String,
    pub needs: u8,
}
/// One served device-list row (L-HTTP-56): `[version, blob, ssk_signature, prev_hash]`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ServedDeviceList {
    pub version: u64,
    pub blob: Vec<u8>,
    pub ssk_signature: Vec<u8>,
    pub prev_hash: Vec<u8>,
}
/// One row of `GET /v1/backups` (L-HTTP-51).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BackupItem {
    pub kind: u64,
    pub chunk_seq: u64,
    pub size: u64,
    pub created: u64,
}
/// One row of `GET /v1/dms`: `[channel_id, kind, members]`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct DmInfo {
    pub channel_id: [u8; 16],
    pub kind: u64,
    pub members: Vec<[u8; 16]>,
}
/// What a driver reads of `GET /v1/channels/{id}`: the id, its community (none for a DM), the kind and the text
/// group.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ChannelInfo {
    pub id: [u8; 16],
    pub community_id: Option<[u8; 16]>,
    pub kind: u64,
    pub text_group_id: Option<[u8; 16]>,
}
/// A session in the pending scope: no gateway socket, only the routes L-HTTP-57 admits.
pub struct PendingSession {
    base: String,
    token: String,
    agent: ureq::Agent,
}

/// The account half and the first device of `POST /v1/accounts`, as protocol/09 fixes them:
/// `[code, username, display, umk_pub, ssk_pub, sig_umk_ssk, password|null, device]` with
/// `device = [device_id, dsk_pub, tier, signer_tier, credential]`.
#[derive(Clone, Debug)]
pub struct NewAccount {
    pub username: String,
    pub display: String,
    pub umk_pub: [u8; 32],
    pub ssk_pub: [u8; 32],
    pub sig_umk_ssk: [u8; 64],
    pub device_id: DeviceId,
    pub dsk_pub: [u8; 32],
    pub tier: u8,
    pub signer_tier: u8,
    /// The `CredentialIdentity` CBOR.
    pub credential: Vec<u8>,
}

fn encode(f: impl FnOnce(&mut Encoder)) -> Vec<u8> {
    let mut e = Encoder::new();
    f(&mut e);
    e.into_vec()
}

fn protocol(e: CborError) -> DsError {
    DsError::Protocol(e.to_string())
}

fn transport(e: impl std::fmt::Display) -> DsError {
    DsError::Transport(e.to_string())
}

/// A `bstr` field the Go encoder may have written as `null`: `fxamacker/cbor`'s core-deterministic
/// mode encodes a nil `[]byte` as `null`, not as an empty string.
fn bytes_or_null(d: &mut Decoder<'_>) -> Result<Vec<u8>, CborError> {
    Ok(d.opt_bytes()?.map(<[u8]>::to_vec).unwrap_or_default())
}

/// An array head the Go encoder may have written as `null`, for the same reason: a nil slice.
fn array_or_null(d: &mut Decoder<'_>) -> Result<usize, CborError> {
    if d.try_null()? { Ok(0) } else { d.array_len() }
}

fn device_id(d: &mut Decoder<'_>) -> Result<DeviceId, CborError> {
    Ok(DeviceId::from_bytes(d.bytes_exact::<16>()?))
}

fn small<T: TryFrom<u64>>(v: u64) -> Result<T, CborError> {
    T::try_from(v).map_err(|_| CborError::IntegerOverflow)
}

/// The upload class element (web3.7 SB1): a uint 0 or 1, or null for a legacy upload.
fn upload_class(d: &mut Decoder<'_>) -> Result<Option<u8>, CborError> {
    d.opt_uint()?.map(small).transpose()
}

/// One row 12 item: `[seq, epoch, uploader_device, blob|null, commitment|null, franking_tag,
/// recv_ts, deleted, class|null]`. Nine elements since web3.7 SB1; eight from an instance that
/// predates the class, which reads as `class: None`. Any other length is a shape error.
fn message_row(d: &mut Decoder<'_>) -> Result<MessageItem, CborError> {
    let len = d.array_len()?;
    if len != 8 && len != 9 {
        return Err(CborError::WrongArrayLen {
            expected: 9,
            actual: len,
        });
    }
    Ok(MessageItem {
        seq: d.uint()?,
        epoch: d.uint()?,
        uploader_device: device_id(d)?,
        blob: bytes_or_null(d)?,
        commitment: d.opt_bytes_exact::<32>()?.unwrap_or([0u8; 32]),
        franking_tag: d.bytes_exact::<32>()?,
        recv_ts: d.uint()?,
        deleted: d.uint()? == 1,
        class: if len == 9 { upload_class(d)? } else { None },
    })
}

/// `[seq, epoch, kind, sender|null, blob]`, row 4's item and op 16's payload alike.
fn handshake_item(d: &mut Decoder<'_>) -> Result<HandshakeItem, CborError> {
    d.array(5)?;
    Ok(HandshakeItem {
        seq: d.uint()?,
        epoch: d.uint()?,
        kind: small(d.uint()?)?,
        sender: d.opt_uint()?.map(small).transpose()?,
        blob: bytes_or_null(d)?,
    })
}

/// `http://host:port` → `host:port`. The harness only ever serves plain HTTP on loopback; an
/// `https://` base is refused rather than silently spoken to in cleartext.
fn host_of(base: &str) -> Result<&str, DsError> {
    let rest = base.strip_prefix("http://").ok_or_else(|| {
        DsError::Protocol(format!(
            "{base}: the testkit speaks plain http:// to a loopback instance only"
        ))
    })?;
    Ok(rest.split('/').next().unwrap_or(rest))
}

fn new_agent() -> ureq::Agent {
    // A non-2xx answer is a protocol/02 error body the scenario matches on, so it must come back
    // as a response, not as ureq's `Error::StatusCode` with the body thrown away.
    ureq::Agent::new_with_config(
        ureq::Agent::config_builder()
            .http_status_as_error(false)
            .timeout_global(Some(Duration::from_secs(60)))
            .build(),
    )
}

/// One request, answered with its status and body whatever the status is.
fn call(
    agent: &ureq::Agent,
    method: &str,
    url: &str,
    token: Option<&str>,
    body: Option<(&str, &[u8])>,
) -> Result<(u16, Vec<u8>), DsError> {
    let bearer = token.map(|t| format!("Bearer {t}"));
    let response = match (method, body) {
        ("GET", None) => {
            let mut r = agent.get(url);
            if let Some(b) = &bearer {
                r = r.header("Authorization", b);
            }
            r.call()
        }
        ("DELETE", None) => {
            let mut r = agent.delete(url);
            if let Some(b) = &bearer {
                r = r.header("Authorization", b);
            }
            r.call()
        }
        ("POST", None) => {
            let mut r = agent.post(url);
            if let Some(b) = &bearer {
                r = r.header("Authorization", b);
            }
            r.send_empty()
        }
        ("POST", Some((content_type, bytes))) => {
            let mut r = agent.post(url).content_type(content_type);
            if let Some(b) = &bearer {
                r = r.header("Authorization", b);
            }
            r.send(bytes)
        }
        ("PUT", Some((content_type, bytes))) => {
            let mut r = agent.put(url).content_type(content_type);
            if let Some(b) = &bearer {
                r = r.header("Authorization", b);
            }
            r.send(bytes)
        }
        ("PATCH", Some((content_type, bytes))) => {
            let mut r = agent.patch(url).content_type(content_type);
            if let Some(b) = &bearer {
                r = r.header("Authorization", b);
            }
            r.send(bytes)
        }
        (method, _) => {
            return Err(DsError::Protocol(format!(
                "no {method} request shape in the testkit client"
            )));
        }
    }
    .map_err(transport)?;
    let status = response.status().as_u16();
    let out = response.into_body().read_to_vec().map_err(transport)?;
    Ok((status, out))
}

/// A body-less request with an explicit response limit for attachment downloads.
fn call_limited(
    agent: &ureq::Agent,
    method: &str,
    url: &str,
    token: Option<&str>,
    limit: u64,
) -> Result<(u16, Vec<u8>), DsError> {
    let bearer = token.map(|t| format!("Bearer {t}"));
    let response = match method {
        "GET" => {
            let mut r = agent.get(url);
            if let Some(b) = &bearer {
                r = r.header("Authorization", b);
            }
            r.call()
        }
        _ => {
            return Err(DsError::Protocol(format!(
                "no {method} request shape in the testkit client"
            )));
        }
    }
    .map_err(transport)?;
    let status = response.status().as_u16();
    let out = response
        .into_body()
        .into_with_config()
        .limit(limit)
        .read_to_vec()
        .map_err(transport)?;
    Ok((status, out))
}

/// POSTs deterministic CBOR and answers the body of a 2xx, or the `DsError` a non-2xx carries.
fn post_cbor(
    agent: &ureq::Agent,
    url: &str,
    token: Option<&str>,
    body: &[u8],
) -> Result<Vec<u8>, DsError> {
    let (status, out) = call(agent, "POST", url, token, Some(("application/cbor", body)))?;
    check_status(status, &out)?;
    Ok(out)
}

/// What a client reads from `GET /v1/instance` before it holds a session (protocol/09 §
/// Instance): the instance id, bound into every session signature and every group's binding, and
/// the current external-sender key, which every `text` and `call` group's `external_senders`
/// extension must carry for the instance to propose into it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InstanceDocument {
    pub instance_id: [u8; 16],
    pub external_sender_key_id: [u8; 16],
    pub external_sender_pub: [u8; 32],
}

/// `GET /v1/instance`: elements 3 (instance_id), 9 (external_sender_key_id) and 10
/// (external_sender_pub) of the eleven-element discovery document.
fn instance_document_of(agent: &ureq::Agent, base: &str) -> Result<InstanceDocument, DsError> {
    let (status, out) = call(agent, "GET", &format!("{base}/v1/instance"), None, None)?;
    check_status(status, &out)?;
    decode_instance_document(&out).map_err(protocol)
}

fn decode_instance_document(body: &[u8]) -> Result<InstanceDocument, CborError> {
    decode_strict(body, |d| {
        d.array(11)?;
        for _ in 0..3 {
            d.skip()?;
        }
        let instance_id = d.bytes_exact::<16>()?;
        for _ in 4..9 {
            d.skip()?;
        }
        let external_sender_key_id = d.bytes_exact::<16>()?;
        let external_sender_pub = d.bytes_exact::<32>()?;
        Ok(InstanceDocument {
            instance_id,
            external_sender_key_id,
            external_sender_pub,
        })
    })
}

fn instance_id_of(agent: &ureq::Agent, base: &str) -> Result<[u8; 16], DsError> {
    instance_document_of(agent, base).map(|doc| doc.instance_id)
}

/// A non-2xx answer is decoded as protocol/02's error array, so a scenario's `expect_reject`
/// matches on the same `E_*` code a real client would see.
///
/// `[code, detail, retry_after_ms|null]`, extended for four codes: `E_COMMIT_CONFLICT` appends
/// `[winning_commit, proposals]`, `E_COMMIT_REQUIRED` `[proposals]`, `E_COMMIT_INVALID` `[rule]`
/// and `E_VERSION` three version lists. `decode_strict` refuses trailing bytes, so every declared
/// element is consumed; the ones this client has no use for are skipped.
fn check_status(status: u16, body: &[u8]) -> Result<(), DsError> {
    if (200..300).contains(&status) {
        return Ok(());
    }
    let (code, detail, extras) = decode_strict(body, |d: &mut Decoder<'_>| {
        let n = d.array_len()?;
        if n < 3 {
            return Err(CborError::WrongArrayLen {
                expected: 3,
                actual: n,
            });
        }
        let code = d.text()?.to_owned();
        let detail = d.text()?.to_owned();
        let mut extras = ErrorExtras {
            retry_after_ms: d.opt_uint()?,
            ..ErrorExtras::default()
        };
        let mut rest = n - 3;
        match (code.as_str(), rest) {
            ("E_COMMIT_CONFLICT", 2..) => {
                extras.winning_commit = bytes_or_null(d)?;
                for _ in 0..array_or_null(d)? {
                    extras.proposals.push(bytes_or_null(d)?);
                }
                rest -= 2;
            }
            ("E_COMMIT_REQUIRED", 1..) => {
                for _ in 0..array_or_null(d)? {
                    extras.proposals.push(bytes_or_null(d)?);
                }
                rest -= 1;
            }
            ("E_COMMIT_INVALID", 1..) => {
                extras.rule = d.text()?.to_owned();
                rest -= 1;
            }
            _ => {}
        }
        for _ in 0..rest {
            d.skip()?;
        }
        Ok((code, detail, extras))
    })
    .map_err(|e| {
        DsError::Protocol(format!(
            "HTTP {status} with an undecodable error body ({e}): {}",
            hex::encode(body)
        ))
    })?;
    Err(DsError::from_code(status, &code, &detail, extras))
}

/// What one inbound gateway frame means to the client.
enum Inbound {
    Hello {
        wire: Vec<u64>,
        e2ee: Vec<u64>,
        media: Vec<u64>,
        heartbeat_ms: u64,
    },
    Ready,
    /// `invalid_session` or `reconnect`: the connection is over and the reason says why.
    Closing(String),
    Frame(Frame),
    /// `heartbeat_ack` (7): the gateway handles one connection's frames in order, so the answer to
    /// a heartbeat proves every frame sent before it has been handled.
    HeartbeatAck,
    Signal {
        op: u64,
        target: [u8; 16],
        device: [u8; 16],
    },
    /// Op 53 `call_ring` (web-3): kept for `take_rings`, never a delivery-service frame.
    Ring(CallRing),
    /// Control frames the client has no use for (`resumed`) and the ops outside the delivery
    /// service (32 and above).
    Ignored,
}

fn uint_list(d: &mut Decoder<'_>) -> Result<Vec<u64>, CborError> {
    let n = array_or_null(d)?;
    let mut out = Vec::with_capacity(n);
    for _ in 0..n {
        out.push(d.uint()?);
    }
    Ok(out)
}

/// Decodes `[op, n, group_id|null, payload]`, the four-element frame of protocol/02 § Gateway
/// frames, and returns the replay `n` beside it.
fn decode_frame(bytes: &[u8]) -> Result<(u64, Inbound), DsError> {
    if bytes.len() > 4 * 1024 * 1024 {
        return Err(DsError::Protocol("gateway frame exceeds 4 MiB".into()));
    }
    decode_strict(bytes, |d: &mut Decoder<'_>| {
        d.array(4)?;
        let op = d.uint()?;
        let n = d.uint()?;
        let group = d.opt_bytes()?.map(<[u8]>::to_vec);
        let len = d.array_len()?;
        let expect = |want: usize| {
            if len == want {
                Ok(())
            } else {
                Err(CborError::WrongArrayLen {
                    expected: want,
                    actual: len,
                })
            }
        };
        // A delivery-service frame without a group is not a frame this client can route.
        let group_id = || {
            group.clone().ok_or(CborError::TypeMismatch {
                expected: "group_id",
                offset: 0,
            })
        };
        let inbound = match op {
            0 => {
                expect(9)?;
                let (wire, e2ee, media) = (uint_list(d)?, uint_list(d)?, uint_list(d)?);
                let heartbeat_ms = d.uint()?;
                for _ in 4..9 {
                    d.skip()?;
                }
                Inbound::Hello {
                    wire,
                    e2ee,
                    media,
                    heartbeat_ms,
                }
            }
            3 => {
                for _ in 0..len {
                    d.skip()?;
                }
                Inbound::Ready
            }
            5 => {
                expect(2)?;
                let _resumable = d.uint()?;
                Inbound::Closing(format!("invalid_session: {}", d.text()?))
            }
            8 => {
                expect(2)?;
                let reason = d.text()?.to_owned();
                let after_ms = d.uint()?;
                Inbound::Closing(format!("reconnect: {reason} (after {after_ms} ms)"))
            }
            7 => {
                for _ in 0..len {
                    d.skip()?;
                }
                Inbound::HeartbeatAck
            }
            9 => {
                expect(3)?;
                Inbound::Frame(Frame::GatewayError {
                    cid: d.uint()?,
                    code: d.text()?.to_owned(),
                    detail: d.text()?.to_owned(),
                })
            }
            16 => {
                // The same five positions as row 4's item, whose array head `len` already read.
                expect(5)?;
                let item = HandshakeItem {
                    seq: d.uint()?,
                    epoch: d.uint()?,
                    kind: small(d.uint()?)?,
                    sender: d.opt_uint()?.map(small).transpose()?,
                    blob: bytes_or_null(d)?,
                };
                Inbound::Frame(Frame::MlsHandshake {
                    group_id: group_id()?,
                    item,
                })
            }
            17 => {
                expect(4)?;
                let epoch = d.uint()?;
                let mut proposal_refs = Vec::new();
                for _ in 0..array_or_null(d)? {
                    proposal_refs.push(bytes_or_null(d)?);
                }
                Inbound::Frame(Frame::CommitNeeded {
                    group_id: group_id()?,
                    epoch,
                    proposal_refs,
                    deadline_ms: d.uint()?,
                    round: d.uint()?,
                })
            }
            18 => {
                expect(2)?;
                Inbound::Frame(Frame::MlsEpochChanged {
                    group_id: group_id()?,
                    epoch: d.uint()?,
                    seq: d.uint()?,
                })
            }
            19 => {
                // Seven elements since web3.7 SB1, the 7th the upload class; six from an instance
                // that predates it.
                if len != 6 && len != 7 {
                    expect(7)?;
                }
                let item = MessageItem {
                    seq: d.uint()?,
                    epoch: d.uint()?,
                    uploader_device: device_id(d)?,
                    blob: bytes_or_null(d)?,
                    // Op 19 does not carry the commitment (protocol/02 § Gateway frames); the
                    // recipient verifies `C` against the decrypted envelope, not against this.
                    commitment: [0u8; 32],
                    franking_tag: d.bytes_exact::<32>()?,
                    recv_ts: d.uint()?,
                    deleted: false,
                    class: if len == 7 { upload_class(d)? } else { None },
                };
                Inbound::Frame(Frame::MessageCt {
                    group_id: group_id()?,
                    item,
                })
            }
            20 => {
                expect(6)?;
                let (_welcome_id, _epoch, _commit_seq) = (d.uint()?, d.uint()?, d.uint()?);
                let blob = bytes_or_null(d)?;
                // The tree and its hash are row 15's to deliver: `join_welcome` collects the
                // Welcome there, so the frame is only the notice.
                d.skip()?;
                d.skip()?;
                Inbound::Frame(Frame::MlsWelcome {
                    group_id: group_id()?,
                    blob,
                })
            }
            21 => {
                expect(2)?;
                Inbound::Frame(Frame::MessageDeleted {
                    group_id: group_id()?,
                    seq: d.uint()?,
                    deleted_at: d.uint()?,
                })
            }
            49 => {
                expect(5)?;
                let target = d.bytes_exact::<16>()?;
                let _user = d.bytes_exact::<16>()?;
                let device = d.bytes_exact::<16>()?;
                let _on = d.uint()?;
                let _expires = d.uint()?;
                Inbound::Signal { op, target, device }
            }
            50 => {
                expect(4)?;
                let _user = d.bytes_exact::<16>()?;
                let device = d.bytes_exact::<16>()?;
                let target = d.bytes_exact::<16>()?;
                let _flags = d.uint()?;
                Inbound::Signal { op, target, device }
            }
            53 => {
                expect(5)?;
                Inbound::Ring(CallRing {
                    call: d.bytes_exact::<16>()?,
                    channel: d.bytes_exact::<16>()?,
                    user: d.bytes_exact::<16>()?,
                    state: d.uint()?,
                    expires: d.uint()?,
                })
            }
            // Op 54 `community_changed` `[community_id]` (web-3.1): checked for shape, then ignored; the testkit
            // reads a community's channels when a scenario asks, never on a notice.
            54 => {
                expect(1)?;
                let _community = d.bytes_exact::<16>()?;
                Inbound::Ignored
            }
            _ => {
                for _ in 0..len {
                    d.skip()?;
                }
                Inbound::Ignored
            }
        };
        Ok((n, inbound))
    })
    .map_err(|e| DsError::Protocol(format!("gateway frame: {e}")))
}

fn read_timeout(ws: &mut WebSocket<TcpStream>, wait: Duration) -> Result<(), DsError> {
    ws.get_mut().set_read_timeout(Some(wait)).map_err(transport)
}

fn is_timeout(e: &tungstenite::Error) -> bool {
    matches!(e, tungstenite::Error::Io(io) if matches!(
        io.kind(),
        std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut
    ))
}

/// The body of `POST /v1/devices/{device_id}/sessions` (L-HTTP-54):
/// `[nonce, purpose 0, sig, element 3, login]`. Element 3 is the credential bstr or the
/// registration array; `login` is the enrolment assertion's UTF-8 bytes as a bstr, or null.
fn session_body(
    nonce: &[u8; 32],
    sig: &[u8; 64],
    element3: &Element3<'_>,
    login: Option<&str>,
) -> Vec<u8> {
    encode(|e| {
        e.array(5).bytes(nonce).uint(0).bytes(sig);
        match element3 {
            Element3::Credential(c) => {
                e.bytes(c);
            }
            Element3::Registration(r) => {
                e.array(5)
                    .bytes(r.device_id.as_bytes())
                    .bytes(&r.dsk_pub)
                    .uint(u64::from(r.tier))
                    .uint(u64::from(r.signer_tier))
                    .bytes(&r.credential);
            }
        }
        if let Some(login) = login {
            e.bytes(login.as_bytes());
        } else {
            e.null();
        }
    })
}

fn decode_established(bytes: &[u8]) -> Result<Established, CborError> {
    decode_strict(bytes, |d| {
        d.array(7)?;
        let token = d.text()?.to_owned();
        let scope = d.uint()?;
        let user_id = d.bytes_exact::<16>()?;
        let device_id = device_id(d)?;
        let expires = d.uint()?;
        d.skip()?;
        d.skip()?;
        Ok(Established {
            token,
            scope,
            user_id,
            device_id,
            expires,
        })
    })
}

fn decode_password_login(bytes: &[u8]) -> Result<PasswordLogin, CborError> {
    decode_strict(bytes, |d| {
        d.array(2)?;
        let assertion = d.text()?.to_owned();
        let needs = match d.uint()? {
            n @ 0..=2 => n as u8,
            _ => return Err(CborError::IntegerOverflow),
        };
        Ok(PasswordLogin { assertion, needs })
    })
}

fn decode_served_list(d: &mut Decoder<'_>) -> Result<ServedDeviceList, CborError> {
    d.array(4)?;
    Ok(ServedDeviceList {
        version: d.uint()?,
        blob: bytes_or_null(d)?,
        ssk_signature: bytes_or_null(d)?,
        prev_hash: bytes_or_null(d)?,
    })
}

fn decode_history(bytes: &[u8]) -> Result<Vec<ServedDeviceList>, CborError> {
    decode_strict(bytes, |d| {
        (0..array_or_null(d)?)
            .map(|_| decode_served_list(d))
            .collect()
    })
}

fn decode_backups(bytes: &[u8]) -> Result<Vec<BackupItem>, CborError> {
    decode_strict(bytes, |d| {
        (0..array_or_null(d)?)
            .map(|_| {
                d.array(4)?;
                Ok(BackupItem {
                    kind: d.uint()?,
                    chunk_seq: d.uint()?,
                    size: d.uint()?,
                    created: d.uint()?,
                })
            })
            .collect()
    })
}

fn decode_dms(bytes: &[u8]) -> Result<Vec<DmInfo>, CborError> {
    decode_strict(bytes, |d| {
        (0..array_or_null(d)?)
            .map(|_| {
                d.array(3)?;
                let channel_id = d.bytes_exact::<16>()?;
                let kind = d.uint()?;
                let members = (0..array_or_null(d)?)
                    .map(|_| d.bytes_exact::<16>())
                    .collect::<Result<Vec<_>, _>>()?;
                Ok(DmInfo {
                    channel_id,
                    kind,
                    members,
                })
            })
            .collect()
    })
}

fn decode_channel(bytes: &[u8]) -> Result<ChannelInfo, CborError> {
    decode_strict(bytes, |d| {
        d.array(12)?;
        let id = d.bytes_exact::<16>()?;
        let community_id = d.opt_bytes_exact::<16>()?;
        let kind = d.uint()?;
        for _ in 3..11 {
            d.skip()?;
        }
        let text_group_id = d.opt_bytes_exact::<16>()?;
        Ok(ChannelInfo {
            id,
            community_id,
            kind,
            text_group_id,
        })
    })
}

/// L-HTTP-100's `[group_id|null, call_id|null, [[user_id, device_id, flags]]]`.
fn decode_channel_call(bytes: &[u8]) -> Result<ChannelCall, CborError> {
    decode_strict(bytes, |d| {
        d.array(3)?;
        let group_id = d.opt_bytes_exact::<16>()?;
        let call_id = d.opt_bytes_exact::<16>()?;
        let n = array_or_null(d)?;
        let mut participants = Vec::with_capacity(n.min(1024));
        for _ in 0..n {
            d.array(3)?;
            participants.push((d.bytes_exact::<16>()?, d.bytes_exact::<16>()?, d.uint()?));
        }
        Ok(ChannelCall {
            group_id,
            call_id,
            participants,
        })
    })
}

fn device_list_put_body(list: &DeviceList) -> Vec<u8> {
    encode(|e| {
        e.array(4)
            .uint(list.unsigned.version)
            .bytes(&list.encode())
            .bytes(&list.sig_ssk)
            .bytes(&list.unsigned.prev_hash);
    })
}

fn get_with(agent: &ureq::Agent, base: &str, token: &str, path: &str) -> Result<Vec<u8>, DsError> {
    let (status, out) = call(agent, "GET", &format!("{base}{path}"), Some(token), None)?;
    check_status(status, &out)?;
    Ok(out)
}

fn get_device_list_bounded(
    agent: &ureq::Agent,
    base: &str,
    token: &str,
    path: &str,
) -> Result<Vec<u8>, DsError> {
    let (status, out) = call_limited(
        agent,
        "GET",
        &format!("{base}{path}"),
        Some(token),
        4 * 1024 * 1024 + 4096,
    )?;
    check_status(status, &out)?;
    Ok(out)
}

fn get_device_list_with(
    agent: &ureq::Agent,
    base: &str,
    token: &str,
    user_id: &[u8; 16],
) -> Result<ServedDeviceList, DsError> {
    let path = format!("/v1/users/{}/device-list", hex::encode(user_id));
    decode_strict(
        &get_device_list_bounded(agent, base, token, &path)?,
        decode_served_list,
    )
    .map_err(protocol)
}

fn list_backups_with(
    agent: &ureq::Agent,
    base: &str,
    token: &str,
) -> Result<Vec<BackupItem>, DsError> {
    decode_backups(&get_with(agent, base, token, "/v1/backups")?).map_err(protocol)
}

fn put_device_list_with(
    agent: &ureq::Agent,
    base: &str,
    token: &str,
    user_id: &[u8; 16],
    list: &DeviceList,
) -> Result<(), DsError> {
    let path = format!("{base}/v1/users/{}/device-list", hex::encode(user_id));
    let body = device_list_put_body(list);
    let (status, out) = call(
        agent,
        "PUT",
        &path,
        Some(token),
        Some(("application/cbor", &body)),
    )?;
    check_status(status, &out)
}

impl PendingSession {
    pub fn get_device_list(&self, user_id: &[u8; 16]) -> Result<ServedDeviceList, DsError> {
        get_device_list_with(&self.agent, &self.base, &self.token, user_id)
    }
    pub fn put_device_list(&self, user_id: &[u8; 16], list: &DeviceList) -> Result<(), DsError> {
        put_device_list_with(&self.agent, &self.base, &self.token, user_id, list)
    }
    pub fn list_backups(&self) -> Result<Vec<BackupItem>, DsError> {
        list_backups_with(&self.agent, &self.base, &self.token)
    }
}

/// A challenge-response device session: a nonce, an Ed25519 signature over the 81-byte preimage
/// of `protocol/02`'s "Device sessions", then the session itself. Answers the bearer token and
/// its hard expiry, in the instance's unix seconds.
fn establish(
    agent: &ureq::Agent,
    base: &str,
    device: &Device,
    credential: &[u8],
) -> Result<(String, u64), DsError> {
    establish_with(agent, base, device, &Element3::Credential(credential), None)
        .map(|e| (e.token, e.expires))
}

/// `establish` with element 3 and the login chosen by the caller; answers the whole session.
fn establish_with(
    agent: &ureq::Agent,
    base: &str,
    device: &Device,
    element3: &Element3<'_>,
    login: Option<&str>,
) -> Result<Established, DsError> {
    let device_hex = hex::encode(device.id().as_bytes());
    let challenge = post_cbor(
        agent,
        &format!("{base}/v1/devices/{device_hex}/sessions/challenge"),
        None,
        &encode(|e| {
            e.array(0);
        }),
    )?;
    let (nonce, _expires) = decode_strict(&challenge, |d: &mut Decoder<'_>| {
        d.array(2)?;
        Ok((d.bytes_exact::<32>()?, d.uint()?))
    })
    .map_err(protocol)?;

    // "dilla session v1" (16) || instance_id (16) || device_id (16) || nonce (32) || purpose (1)
    let instance_id = instance_id_of(agent, base)?;
    let mut preimage = Vec::with_capacity(81);
    preimage.extend_from_slice(b"dilla session v1");
    preimage.extend_from_slice(&instance_id);
    preimage.extend_from_slice(device.id().as_bytes());
    preimage.extend_from_slice(&nonce);
    preimage.push(0); // purpose 0: session
    let sig = device.sign(&preimage);

    let body = session_body(&nonce, &sig, element3, login);
    let session = post_cbor(
        agent,
        &format!("{base}/v1/devices/{device_hex}/sessions"),
        None,
        &body,
    )?;
    // `[token, scope, user_id, device_id, expires, idle_expires, generation]` (protocol/09).
    decode_established(&session).map_err(protocol)
}

/// What `POST /v1/channels/{id}/calls` answers (protocol/09 § Voice, MD-10):
/// `[call_id, group_id, livekit_url, token, ice_servers, caps]`.
#[derive(Clone, Debug)]
pub struct CallStarted {
    pub call_id: [u8; 16],
    pub group_id: [u8; 16],
    pub livekit_url: String,
    pub token: String,
    pub ice_servers: Vec<(Vec<String>, String, String)>,
    /// `[max_audio_bitrate_bps, max_share_bitrate_bps, vp9]`.
    pub caps: [u64; 3],
    /// `201`: this start opened the call (and rang a DM's other participants, L-GW-33); `200`: it was live.
    pub created: bool,
}

/// `POST /v1/channels/{id}/calls`'s answer: six elements, or seven for a start that opened a DM or
/// group-DM call, whose seventh, `rung([user_id])`, lists the users the start rang (WB product walk
/// I6). The testkit reads past it.
fn decode_call_started(out: &[u8], created: bool) -> Result<CallStarted, CborError> {
    decode_strict(out, |d| {
        // Six elements, or seven for a start that opened a DM or group-DM call: the seventh,
        // `rung`, lists the users the start rang (WB product walk I6); the testkit reads past it.
        let len = d.array_len()?;
        if len != 6 && len != 7 {
            return Err(CborError::WrongArrayLen {
                expected: 6,
                actual: len,
            });
        }
        let call_id = d.bytes_exact::<16>()?;
        let group_id = d.bytes_exact::<16>()?;
        let livekit_url = d.text()?.to_owned();
        let token = d.text()?.to_owned();
        let servers = array_or_null(d)?;
        let mut ice_servers = Vec::with_capacity(servers);
        for _ in 0..servers {
            d.array(3)?;
            let urls = (0..d.array_len()?)
                .map(|_| d.text().map(str::to_owned))
                .collect::<Result<Vec<_>, _>>()?;
            let username = d.text()?.to_owned();
            let credential = d.text()?.to_owned();
            ice_servers.push((urls, username, credential));
        }
        d.array(3)?;
        let caps = [d.uint()?, d.uint()?, d.uint()?];
        if len == 7 {
            for _ in 0..d.array_len()? {
                d.bytes_exact::<16>()?;
            }
        }
        Ok(CallStarted {
            call_id,
            group_id,
            livekit_url,
            token,
            ice_servers,
            caps,
            created,
        })
    })
}

impl HttpDs {
    #[cfg(test)]
    pub(crate) fn for_device_list_test(base: &str) -> Self {
        Self {
            base: base.to_owned(),
            token: "test-token".into(),
            device: DeviceId::from_bytes([0; 16]),
            agent: new_agent(),
            ws: None,
            pending: Vec::new(),
            online: false,
            heartbeat: Duration::from_secs(30),
            last_beat: Instant::now(),
            unacked: 0,
            ack_since: Instant::now(),
            last_pump: Instant::now(),
            last_n: 0,
            delivered: BTreeMap::new(),
            connected_before: false,
            reconnects: 0,
            lost: Vec::new(),
            accepted: 0,
            missed_acks: 0,
            expires: 0,
            rings: Vec::new(),
            totp: None,
        }
    }

    /// Establishes a device session over the real endpoints: a challenge, an Ed25519 signature
    /// over the 81-byte preimage of `protocol/02`'s "Device sessions", then the session itself.
    pub fn connect(base: &str, device: &Device, credential: &[u8]) -> Result<Self, DsError> {
        let (token, expires) = establish(&new_agent(), base, device, credential)?;
        let mut ds = Self::with_session(base, device.id(), token)?;
        ds.expires = expires;
        Ok(ds)
    }

    /// Signs in again: a fresh challenge-response session replaces the one this client holds,
    /// and the gateway connection is reopened over it when the device is online. A native
    /// session lasts 30 days (protocol/02 § Device sessions item 5), so a scenario that moves the
    /// instance clock past that is one in which every device has to sign in again — which a real
    /// client does the same way.
    pub fn reauthenticate(&mut self, device: &Device, credential: &[u8]) -> Result<(), DsError> {
        (self.token, self.expires) = establish(&self.agent, &self.base, device, credential)?;
        // What the old connection already holds is read first, as `set_online(false)` does: the
        // reconnect's catch-up starts after `delivered`, which only a pumped frame advances, so
        // closing over them would ask the instance for frames it may since have pruned.
        self.pump()?;
        if let Some(mut ws) = self.ws.take() {
            let _ = ws.close(None);
            let _ = ws.flush();
        }
        if self.online {
            self.open_socket()?;
        }
        Ok(())
    }

    /// A client over a session the caller already holds — the one `POST /v1/accounts` returns
    /// beside the account (protocol/02 § Device sessions item 7), so a freshly registered device
    /// does not spend a challenge it has no need of. Opens the gateway connection.
    pub fn with_session(base: &str, device: DeviceId, token: String) -> Result<Self, DsError> {
        host_of(base)?;
        let mut ds = Self {
            base: base.trim_end_matches('/').to_owned(),
            token,
            device,
            agent: new_agent(),
            ws: None,
            pending: Vec::new(),
            online: true,
            heartbeat: Duration::from_secs(30),
            last_beat: Instant::now(),
            unacked: 0,
            ack_since: Instant::now(),
            last_pump: Instant::now(),
            last_n: 0,
            delivered: BTreeMap::new(),
            connected_before: false,
            reconnects: 0,
            lost: Vec::new(),
            accepted: 0,
            missed_acks: 0,
            expires: 0,
            rings: Vec::new(),
            totp: None,
        };
        ds.open_socket()?;
        Ok(ds)
    }

    /// Creates an account against `dillad init`'s bootstrap invite. There is no other way in: the
    /// instance is invite-only and the harness has no back door.
    pub fn redeem_invite(base: &str, code: &str, account: NewAccount) -> Result<Enrolled, DsError> {
        let agent = new_agent();
        let body = encode(|e| {
            e.array(8)
                .text(code)
                .text(&account.username)
                .text(&account.display)
                .bytes(&account.umk_pub)
                .bytes(&account.ssk_pub)
                .bytes(&account.sig_umk_ssk)
                .null();
            e.array(5)
                .bytes(account.device_id.as_bytes())
                .bytes(&account.dsk_pub)
                .uint(u64::from(account.tier))
                .uint(u64::from(account.signer_tier))
                .bytes(&account.credential);
        });
        let out = post_cbor(&agent, &format!("{base}/v1/accounts"), None, &body)?;
        decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(4)?;
            let user_id = d.bytes_exact::<16>()?;
            let device_id = device_id(d)?;
            let token = d.text()?.to_owned();
            let expires = d.uint()?;
            Ok(Enrolled {
                user_id,
                device_id,
                token,
                expires,
            })
        })
        .map_err(protocol)
    }

    /// Enrols the authenticator app every account owes (protocol/09 § Second factors) over the session
    /// `POST /v1/accounts` returned. An owed session reaches only the allow-list (and the gateway refuses
    /// it), so this runs before `with_session`. Returns `None` when the account owes nothing. The host clock
    /// is read from the control listener, since the confirming code is computed for the instance's time. With
    /// `machine_clock_ok` and no control listener, the machine's clock stands in: a real instance runs on real
    /// time and the confirm is the only code the caller ever spends (netprobe). Without it, an unset
    /// `DILLA_TESTKIT_CONTROL` is a scenario error.
    pub fn enrol_second_factor(
        base: &str,
        token: &str,
        machine_clock_ok: bool,
    ) -> Result<Option<TotpState>, TestkitError> {
        enrol_second_factor_on(base, token, machine_clock_ok, control_is_set())
    }
}

/// `HttpDs::enrol_second_factor` with the control listener's presence given, so a test need not touch the
/// process environment.
fn enrol_second_factor_on(
    base: &str,
    token: &str,
    machine_clock_ok: bool,
    control_set: bool,
) -> Result<Option<TotpState>, TestkitError> {
    {
        let agent = new_agent();
        let base = base.trim_end_matches('/');
        let (status, out) = call(
            &agent,
            "GET",
            &format!("{base}/v1/auth/factors"),
            Some(token),
            None,
        )?;
        check_status(status, &out)?;
        // Five elements, or six since Task 11 added password_changed: only owed is read here.
        let owed = decode_strict(&out, |d: &mut Decoder<'_>| {
            let len = d.array_len()?;
            if !(5..=6).contains(&len) {
                return Err(CborError::WrongArrayLen {
                    expected: 6,
                    actual: len,
                });
            }
            let owed = d.uint()?;
            for _ in 1..len {
                d.skip()?;
            }
            Ok(owed)
        })
        .map_err(protocol)?;
        if owed == 0 {
            return Ok(None);
        }
        if !control_set && !machine_clock_ok {
            return Err(TestkitError::Scenario(
                "the instance requires a second factor and DILLA_TESTKIT_CONTROL is unset".into(),
            ));
        }
        let out = post_cbor(
            &agent,
            &format!("{base}/v1/auth/totp/enroll"),
            Some(token),
            &encode(|e| {
                e.array(1).null();
            }),
        )?;
        let secret = decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(2)?;
            let secret = d.text()?.to_owned();
            d.skip()?;
            Ok(secret)
        })
        .map_err(protocol)?;
        let now = if control_set {
            host_unix()?
        } else {
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map_or(0, |d| d.as_secs())
        };
        let counter = now / totp::PERIOD;
        let code = totp::code(&secret, counter).map_err(TestkitError::Scenario)?;
        let out = post_cbor(
            &agent,
            &format!("{base}/v1/auth/totp/confirm"),
            Some(token),
            &encode(|e| {
                e.array(1).text(&code);
            }),
        )?;
        let backup_codes = decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(1)?;
            (0..array_or_null(d)?)
                .map(|_| Ok(d.text()?.to_owned()))
                .collect()
        })
        .map_err(protocol)?;
        Ok(Some(TotpState {
            secret,
            last_counter: counter,
            backup_codes,
        }))
    }
}

impl HttpDs {
    /// Keeps (or drops) the authenticator-app secret [`HttpDs::enrol_second_factor`] returned.
    pub fn set_totp(&mut self, totp: Option<TotpState>) {
        self.totp = totp;
    }

    /// The enrolled authenticator app, with the last counter the instance took.
    pub fn totp(&self) -> Option<&TotpState> {
        self.totp.as_ref()
    }

    /// Turns a password login into an assertion that owes nothing. `needs` 1 is answered with this account's
    /// TOTP code at `/v1/auth/totp/verify`, counter `max(c-1, last+1)` (advancing the host clock one period at a
    /// time while that is past `c+1`); `needs` 2 asks for a passkey, which a native peer has none of.
    pub fn second_factor_assertion(
        &mut self,
        login: PasswordLogin,
    ) -> Result<String, TestkitError> {
        match login.needs {
            0 => Ok(login.assertion),
            1 => {
                let code = self.next_totp_code()?;
                let out = post_cbor(
                    &self.agent,
                    &format!("{}/v1/auth/totp/verify", self.base),
                    None,
                    &encode(|e| {
                        e.array(2).text(&login.assertion).text(&code);
                    }),
                )?;
                let assertion = decode_strict(&out, |d: &mut Decoder<'_>| {
                    d.array(1)?;
                    Ok(d.text()?.to_owned())
                })
                .map_err(protocol)?;
                Ok(assertion)
            }
            _ => Err(TestkitError::Scenario(
                "the account asks for a passkey login and a native peer holds no passkey".into(),
            )),
        }
    }

    /// This account's next TOTP code, counter `max(c-1, last+1)`. The counter is spent from here on, whether or
    /// not the answer reaches us: reusing a counter is refused, skipping one is harmless.
    fn next_totp_code(&mut self) -> Result<String, TestkitError> {
        let state = self.totp.as_ref().ok_or_else(|| {
            TestkitError::Scenario(
                "the account asks for a TOTP code and this peer enrolled none".into(),
            )
        })?;
        let secret = state.secret.clone();
        let counter = totp::pick_counter(
            state.last_counter,
            || host_unix().map_err(|e| e.to_string()),
            || {
                control_post("/debug/clock", r#"{"seconds":30}"#)
                    .map(|_| ())
                    .map_err(|e| e.to_string())
            },
        )
        .map_err(TestkitError::Scenario)?;
        let code = totp::code(&secret, counter).map_err(TestkitError::Scenario)?;
        if let Some(state) = self.totp.as_mut() {
            state.last_counter = counter;
        }
        Ok(code)
    }

    /// `POST /v1/auth/stepup [method, secret]` → `[stepup, expires]`. It spends a backup code (method 2) while one is
    /// left, which never moves the test host's shared clock, and falls back to a TOTP code (method 1).
    pub fn step_up(&mut self) -> Result<String, TestkitError> {
        let backup = self.totp.as_mut().and_then(|s| s.backup_codes.pop());
        let (method, secret) = match backup {
            Some(code) => (2, code),
            None => (1, self.next_totp_code()?),
        };
        let token = self.post_decoded(
            "/v1/auth/stepup",
            &encode(|e| {
                e.array(2).uint(method).text(&secret);
            }),
            |d: &mut Decoder<'_>| {
                d.array(2)?;
                let token = d.text()?.to_owned();
                d.uint()?;
                Ok(token)
            },
        )?;
        Ok(token)
    }

    pub fn device(&self) -> DeviceId {
        self.device
    }

    /// Reads what the gateway has already written into `pending`, as `set_online(false)` does.
    /// A clock jump longer than the heartbeat grace makes the instance close every connection
    /// 4009 on its next maintenance tick, and the reconnect's catch-up resumes after `delivered`,
    /// which only a pumped frame advances: frames left unread in the dead socket would be asked
    /// for again after the jump, when retention may already have taken them.
    pub fn settle(&mut self) -> Result<(), DsError> {
        if !self.online {
            return Ok(());
        }
        self.pump()
    }

    fn send_ephemeral(&mut self, op: u64, target: &[u8; 16], flags: u64) -> Result<(), DsError> {
        if !self.online {
            return Err(DsError::Transport("no gateway connection".into()));
        }
        if self.ws.is_none() {
            self.open_socket()?;
        }
        let frame = encode(|e| {
            e.array(4).uint(op).uint(0).null();
            e.array(2).bytes(target).uint(flags);
        });
        self.ws
            .as_mut()
            .ok_or_else(|| DsError::Transport("no gateway connection".into()))?
            .send(Message::Binary(frame.into()))
            .map_err(transport)?;
        let deadline = Instant::now() + HANDSHAKE_WAIT;
        loop {
            if Instant::now() >= deadline {
                return Err(DsError::Transport("no gateway signal outcome".into()));
            }
            let ws = self
                .ws
                .as_mut()
                .ok_or_else(|| DsError::Transport("gateway closed".into()))?;
            match ws.read() {
                Ok(Message::Binary(bytes)) => {
                    let (n, inbound) = decode_frame(&bytes)?;
                    self.last_n = self.last_n.max(n);
                    match inbound {
                        Inbound::Signal {
                            op: got,
                            target: seen,
                            device,
                        } if got == op - 2
                            && seen == *target
                            && device == *self.device.as_bytes() =>
                        {
                            return Ok(());
                        }
                        Inbound::Frame(Frame::GatewayError { code, .. }) => {
                            return Err(DsError::from_code(0, &code, "", ErrorExtras::default()));
                        }
                        Inbound::Frame(frame) => self.accept(frame),
                        Inbound::HeartbeatAck => self.acked(),
                        Inbound::Ring(ring) => self.keep_ring(ring),
                        Inbound::Closing(reason) => return Err(DsError::Transport(reason)),
                        _ => {}
                    }
                }
                Ok(Message::Close(_)) => return Err(DsError::Transport("gateway closed".into())),
                Ok(_) => {}
                Err(e) if is_timeout(&e) => {}
                Err(e) => return Err(transport(e)),
            }
        }
    }

    pub fn typing_set(&mut self, channel: &[u8; 16], on: bool) -> Result<(), DsError> {
        self.send_ephemeral(51, channel, u64::from(on))
    }

    pub fn voice_self(&mut self, call: &[u8; 16], flags: u64) -> Result<(), DsError> {
        self.send_ephemeral(52, call, flags)
    }

    /// The session's hard expiry in the instance's unix seconds; 0 when unknown.
    pub fn session_expires(&self) -> u64 {
        self.expires
    }

    /// Records the hard expiry of the session `with_session` was handed (the one `POST
    /// /v1/accounts` answers with).
    pub fn set_session_expires(&mut self, expires: u64) {
        self.expires = expires;
    }

    /// Opens or joins the channel's call: the body is `[]`, or `[vdec]` with the comma-separated
    /// decode list dillad writes into the token's `dilla.vdec` attribute (DEV-07).
    pub fn start_call(&self, channel: &[u8; 16], vdec: &str) -> Result<CallStarted, DsError> {
        let body = encode(|e| {
            if vdec.is_empty() {
                e.array(0);
            } else {
                e.array(1).text(vdec);
            }
        });
        let path = format!("/v1/channels/{}/calls", hex::encode(channel));
        let (status, out) = call(
            &self.agent,
            "POST",
            &self.url(&path),
            Some(&self.token),
            Some(("application/cbor", &body)),
        )?;
        check_status(status, &out)?;
        decode_call_started(&out, status == 201)
            .map_err(|e| DsError::Protocol(format!("POST {path}: {e}")))
    }

    /// `GET /v1/channels/{id}/call` (web-3, L-HTTP-100).
    pub fn get_channel_call(&self, channel: &[u8; 16]) -> Result<ChannelCall, DsError> {
        let path = format!("/v1/channels/{}/call", hex::encode(channel));
        decode_channel_call(&get_with(&self.agent, &self.base, &self.token, &path)?)
            .map_err(protocol)
    }

    /// `POST /v1/calls/{call_id}/decline [expires_ts]` (web-3, L-HTTP-104): `204`. It acts only on the ring whose
    /// `expires_ts` equals the body's, so a decline of an earlier ring stops nothing.
    pub fn decline_call(&self, call_id: &[u8; 16], expires_ts: u64) -> Result<(), DsError> {
        let path = format!("/v1/calls/{}/decline", hex::encode(call_id));
        self.post_status(
            &path,
            &encode(|e| {
                e.array(1).uint(expires_ts);
            }),
        )
    }

    /// Every op 53 this connection received and no earlier call claimed, in arrival order. It reads up to a
    /// heartbeat barrier first: the gateway writes one connection's frames in order, so a ring queued for this
    /// device before the call (a start's ring, written before the start answers) is in the answer.
    pub fn take_rings(&mut self) -> Result<Vec<CallRing>, DsError> {
        if self.online && self.ws.is_none() {
            self.open_socket()?;
        }
        if self.ws.is_some() {
            self.barrier()?;
        }
        Ok(std::mem::take(&mut self.rings))
    }

    fn keep_ring(&mut self, ring: CallRing) {
        if self.rings.len() >= MAX_KEPT_RINGS {
            self.rings.remove(0);
        }
        self.rings.push(ring);
    }

    /// Takes the call's publisher lease for this device (`POST /v1/calls/{call_id}/share`, `204`):
    /// dillad then pushes the camera and screen sources to the live participant (DEV-02, F1).
    pub fn share_call(&self, call_id: &[u8; 16]) -> Result<(), DsError> {
        let path = format!("/v1/calls/{}/share", hex::encode(call_id));
        self.post_status(
            &path,
            &encode(|e| {
                e.array(0);
            }),
        )
    }

    /// `PUT /v1/users/{user_id}/device-list`: `[version, blob, ssk_signature, prev_hash]`, where
    /// `blob` is protocol/03's full six-element signed list — the form the instance hands to the
    /// guest's `device_list_entries` when invariant 4 checks an Add's DSK against it.
    pub fn put_device_list(&self, user_id: &[u8; 16], list: &DeviceList) -> Result<(), DsError> {
        put_device_list_with(&self.agent, &self.base, &self.token, user_id, list)
    }

    /// `POST /v1/auth/password/login [username, password]` with no token: the enrolment assertion.
    pub fn password_login(
        base: &str,
        username: &str,
        password: &str,
    ) -> Result<PasswordLogin, DsError> {
        let out = post_cbor(
            &new_agent(),
            &format!("{base}/v1/auth/password/login"),
            None,
            &encode(|e| {
                e.array(2).text(username).text(password);
            }),
        )?;
        decode_password_login(&out).map_err(protocol)
    }

    /// An enrolment establish (L-HTTP-54): the registration array and the assertion. The instance
    /// must answer scope 1, a session that reaches only the routes of L-HTTP-57.
    pub fn establish_with_login(
        base: &str,
        device: &Device,
        registration: &Registration,
        login: &str,
    ) -> Result<(PendingSession, Established), DsError> {
        let agent = new_agent();
        let established = establish_with(
            &agent,
            base,
            device,
            &Element3::Registration(registration),
            Some(login),
        )?;
        if established.scope != 1 {
            return Err(DsError::Protocol(format!(
                "the registration answered scope {}, want 1",
                established.scope
            )));
        }
        let pending = PendingSession {
            base: base.trim_end_matches('/').to_owned(),
            token: established.token.clone(),
            agent,
        };
        Ok((pending, established))
    }

    /// web-1's establish body (credential, null login), answering the whole session with its scope.
    pub fn establish_scoped(
        base: &str,
        device: &Device,
        credential: &[u8],
    ) -> Result<Established, DsError> {
        establish_with(
            &new_agent(),
            base,
            device,
            &Element3::Credential(credential),
            None,
        )
    }

    /// `POST /v1/auth/password [null, password, stepup|null]`: a first password, set from a fresh session. An
    /// account that holds a factor spends a step-up token on it (protocol/09), taken here from the peer's TOTP.
    pub fn set_password(&mut self, password: &str) -> Result<(), TestkitError> {
        let stepup = match self.totp {
            Some(_) => Some(self.step_up()?),
            None => None,
        };
        self.post_status(
            "/v1/auth/password",
            &encode(|e| {
                e.array(3).null().text(password);
                match &stepup {
                    Some(token) => e.text(token),
                    None => e.null(),
                };
            }),
        )?;
        Ok(())
    }

    /// `GET /v1/users/{user_id}/device-list`: the newest row (L-HTTP-56).
    pub fn get_device_list(&self, user_id: &[u8; 16]) -> Result<ServedDeviceList, DsError> {
        get_device_list_with(&self.agent, &self.base, &self.token, user_id)
    }

    /// `GET /v1/users/{user_id}/device-list?after=N`: the rows above `after`, ascending.
    pub fn get_device_list_history(
        &self,
        user_id: &[u8; 16],
        after: u64,
    ) -> Result<Vec<ServedDeviceList>, DsError> {
        let path = format!(
            "/v1/users/{}/device-list?after={after}",
            hex::encode(user_id)
        );
        let out = get_device_list_bounded(&self.agent, &self.base, &self.token, &path)?;
        decode_history(&out).map_err(protocol)
    }

    /// `GET /v1/backups` (L-HTTP-51).
    pub fn list_backups(&self) -> Result<Vec<BackupItem>, DsError> {
        list_backups_with(&self.agent, &self.base, &self.token)
    }

    /// `POST /v1/dms [recipients]`: the DM's channel id and whether this call created it (`201`).
    pub fn post_dm(&self, recipients: &[[u8; 16]]) -> Result<([u8; 16], bool), DsError> {
        let body = encode(|e| {
            e.array(1).array(recipients.len());
            for id in recipients {
                e.bytes(id);
            }
        });
        let (status, out) = call(
            &self.agent,
            "POST",
            &self.url("/v1/dms"),
            Some(&self.token),
            Some(("application/cbor", &body)),
        )?;
        check_status(status, &out)?;
        let id = decode_strict(&out, |d| {
            d.array(1)?;
            d.bytes_exact::<16>()
        })
        .map_err(protocol)?;
        Ok((id, status == 201))
    }

    /// `GET /v1/dms`: the caller's DMs.
    pub fn list_dms(&self) -> Result<Vec<DmInfo>, DsError> {
        decode_dms(&get_with(&self.agent, &self.base, &self.token, "/v1/dms")?).map_err(protocol)
    }

    /// `GET /v1/channels/{id}`: its id, kind and text group.
    pub fn get_channel(&self, channel_id: &[u8; 16]) -> Result<ChannelInfo, DsError> {
        let path = format!("/v1/channels/{}", hex::encode(channel_id));
        decode_channel(&get_with(&self.agent, &self.base, &self.token, &path)?).map_err(protocol)
    }

    fn blob_path(channel_id: &[u8; 16], blob_id: &[u8; 32]) -> String {
        format!(
            "/v1/channels/{}/blobs/{}",
            hex::encode(channel_id),
            hex::encode(blob_id)
        )
    }

    pub fn delete_message(&self, group_id: &[u8], seq: u64) -> Result<(), DsError> {
        let group: &GroupId = group_id;
        let path = Self::group_path(group, &format!("/messages/{seq}"));
        let (status, out) = call(
            &self.agent,
            "DELETE",
            &self.url(&path),
            Some(&self.token),
            None,
        )?;
        check_status(status, &out)
    }

    pub fn put_blob(
        &self,
        channel_id: &[u8; 16],
        blob_id: &[u8; 32],
        stored: &[u8],
    ) -> Result<bool, DsError> {
        let path = Self::blob_path(channel_id, blob_id);
        let (status, out) = call(
            &self.agent,
            "PUT",
            &self.url(&path),
            Some(&self.token),
            Some(("application/octet-stream", stored)),
        )?;
        check_status(status, &out)?;
        let (answered, size) = decode_strict(&out, |d| {
            d.array(2)?;
            Ok((d.bytes_exact::<32>()?, d.uint()?))
        })
        .map_err(protocol)?;
        if answered != *blob_id || size != stored.len() as u64 {
            return Err(DsError::Protocol(format!(
                "PUT {path}: the instance answered another blob"
            )));
        }
        Ok(status == 201)
    }

    pub fn confirm_blob(&self, channel_id: &[u8; 16], blob_id: &[u8; 32]) -> Result<(), DsError> {
        let path = format!("{}/confirm", Self::blob_path(channel_id, blob_id));
        let (status, out) = call(
            &self.agent,
            "POST",
            &self.url(&path),
            Some(&self.token),
            None,
        )?;
        check_status(status, &out)
    }

    pub fn get_blob(&self, channel_id: &[u8; 16], blob_id: &[u8; 32]) -> Result<Vec<u8>, DsError> {
        let path = Self::blob_path(channel_id, blob_id);
        let (status, out) = call_limited(
            &self.agent,
            "GET",
            &self.url(&path),
            Some(&self.token),
            104_857_600,
        )?;
        check_status(status, &out)?;
        Ok(out)
    }

    pub fn delete_blob(&self, channel_id: &[u8; 16], blob_id: &[u8; 32]) -> Result<(), DsError> {
        let path = Self::blob_path(channel_id, blob_id);
        let (status, out) = call(
            &self.agent,
            "DELETE",
            &self.url(&path),
            Some(&self.token),
            None,
        )?;
        check_status(status, &out)
    }

    pub fn blob_status(&self, channel_id: &[u8; 16], blob_id: &[u8; 32]) -> Result<u16, DsError> {
        let path = Self::blob_path(channel_id, blob_id);
        let (status, out) = call_limited(
            &self.agent,
            "GET",
            &self.url(&path),
            Some(&self.token),
            104_857_600,
        )?;
        if status == 200 || status == 404 {
            return Ok(status);
        }
        check_status(status, &out)?;
        Err(DsError::Protocol(format!(
            "GET {path}: unexpected status {status}"
        )))
    }

    /// POST /v1/communities [name, policy bstr "{}", 0, 0].
    pub fn create_community(&self, name: &str) -> Result<[u8; 16], DsError> {
        self.post_decoded(
            "/v1/communities",
            &encode(|e| {
                e.array(4).text(name).bytes(b"{}").uint(0).uint(0);
            }),
            |d| {
                d.array(3)?;
                let id = d.bytes_exact::<16>()?;
                d.skip()?;
                d.skip()?;
                Ok(id)
            },
        )
    }

    /// What the owner's roles tab does to give everyone a right (flow 09): `GET /v1/communities/{id}/roles` finds
    /// @everyone (position 0), then `PATCH /v1/roles/{everyone}` `[null, null, null, allow | bits, null, null, null]`.
    /// Answers the new mask.
    pub fn allow_everyone(&self, community: &[u8; 16], bits: u64) -> Result<u64, DsError> {
        let path = format!("/v1/communities/{}/roles", hex::encode(community));
        let roles = get_with(&self.agent, &self.base, &self.token, &path)?;
        let (everyone, allow) = decode_strict(&roles, |d| {
            let mut found = None;
            for _ in 0..d.array_len()? {
                d.array(8)?;
                let id = d.bytes_exact::<16>()?;
                d.text()?;
                d.uint()?;
                let position = d.uint()?;
                let allow = d.uint()?;
                d.uint()?;
                d.uint()?;
                d.uint()?;
                if position == 0 {
                    found = Some((id, allow));
                }
            }
            Ok(found)
        })
        .map_err(protocol)?
        .ok_or_else(|| DsError::Protocol(format!("GET {path}: no @everyone role")))?;
        let mask = allow | bits;
        let body = encode(|e| {
            e.array(7)
                .null()
                .null()
                .null()
                .uint(mask)
                .null()
                .null()
                .null();
        });
        let role_path = format!("/v1/roles/{}", hex::encode(everyone));
        let (status, out) = call(
            &self.agent,
            "PATCH",
            &self.url(&role_path),
            Some(&self.token),
            Some(("application/cbor", &body)),
        )?;
        check_status(status, &out)?;
        Ok(mask)
    }

    /// POST /v1/communities/{id}/channels [0, 0, 0, null, name, "", 0, 0].
    pub fn create_text_channel(
        &self,
        community: &[u8; 16],
        name: &str,
    ) -> Result<([u8; 16], u64, u64), DsError> {
        self.create_channel(community, 0, name)
    }

    /// POST /v1/communities/{id}/channels [kind, 0, 0, null, name, "", 0, 0]: kind 0 text, 1 voice.
    pub fn create_channel(
        &self,
        community: &[u8; 16],
        kind: u64,
        name: &str,
    ) -> Result<([u8; 16], u64, u64), DsError> {
        let path = format!("/v1/communities/{}/channels", hex::encode(community));
        self.post_decoded(
            &path,
            &encode(|e| {
                e.array(8)
                    .uint(kind)
                    .uint(0)
                    .uint(0)
                    .null()
                    .text(name)
                    .text("")
                    .uint(0)
                    .uint(0);
            }),
            |d| {
                d.array(3)?;
                Ok((d.bytes_exact::<16>()?, d.uint()?, d.uint()?))
            },
        )
    }

    /// POST /v1/communities/{id}/invites [max_uses, ttl_seconds, 0].
    pub fn create_invite(
        &self,
        community: &[u8; 16],
        max_uses: u64,
        ttl_seconds: u64,
    ) -> Result<String, DsError> {
        let path = format!("/v1/communities/{}/invites", hex::encode(community));
        self.post_decoded(
            &path,
            &encode(|e| {
                e.array(3).uint(max_uses).uint(ttl_seconds).uint(0);
            }),
            |d| {
                d.array(4)?;
                d.skip()?;
                let code = d.text()?.to_owned();
                d.skip()?;
                d.skip()?;
                Ok(code)
            },
        )
    }

    /// POST /v1/communities/{id}/join [code].
    pub fn join_community(&self, community: &[u8; 16], code: &str) -> Result<(), DsError> {
        let path = format!("/v1/communities/{}/join", hex::encode(community));
        self.post_decoded(
            &path,
            &encode(|e| {
                e.array(1).text(code);
            }),
            |d| {
                d.array(1)?;
                d.bytes_exact::<16>()?;
                Ok(())
            },
        )
    }

    /// Opens the gateway: the upgrade with the bearer on the `Authorization` header, `hello`,
    /// `identify` at the highest versions both sides share, and `ready`.
    fn open_socket(&mut self) -> Result<(), DsError> {
        let host = host_of(&self.base)?.to_owned();
        let url = format!("ws://{host}/gateway");
        let request = tungstenite::http::Request::builder()
            .uri(&url)
            .header("Authorization", format!("Bearer {}", self.token))
            .header("Sec-WebSocket-Protocol", SUBPROTOCOL)
            .header("Host", host.as_str())
            .header("Connection", "Upgrade")
            .header("Upgrade", "websocket")
            .header("Sec-WebSocket-Version", "13")
            .header(
                "Sec-WebSocket-Key",
                tungstenite::handshake::client::generate_key(),
            )
            .body(())
            .map_err(|e| DsError::Protocol(e.to_string()))?;
        // The stream is built here so its read timeout is set before the handshake: see `pump`'s
        // comment for why a timeout, not a non-blocking toggle, is how a pump returns promptly.
        let stream = TcpStream::connect(&host).map_err(transport)?;
        stream.set_nodelay(true).map_err(transport)?;
        stream
            .set_read_timeout(Some(HANDSHAKE_WAIT))
            .map_err(transport)?;
        let (mut socket, _response) = tungstenite::client(request, stream).map_err(transport)?;

        let (wire, e2ee, media, heartbeat_ms) = match Self::read_one(&mut socket)? {
            (
                _,
                Inbound::Hello {
                    wire,
                    e2ee,
                    media,
                    heartbeat_ms,
                },
            ) => (wire, e2ee, media, heartbeat_ms),
            _ => {
                return Err(DsError::Protocol(
                    "the gateway's first frame was not hello".into(),
                ));
            }
        };
        // protocol/07 "Negotiation": the client takes the highest version it shares with the
        // instance. This client speaks wire version 3 and version 1 of e2ee and media.
        for (name, offered, speaks) in [
            ("wire", &wire, WIRE_VERSION),
            ("e2ee", &e2ee, 1),
            ("media", &media, 1),
        ] {
            if !offered.contains(&speaks) {
                return Err(DsError::Protocol(format!(
                    "the instance offers {name} versions {offered:?}; this client speaks {speaks}"
                )));
            }
        }
        let identify = encode(|e| {
            e.array(4).uint(1).uint(1).null();
            e.array(5)
                .text(&self.token)
                .uint(WIRE_VERSION)
                .uint(1)
                .uint(1)
                .uint(0);
        });
        socket
            .send(Message::Binary(identify.into()))
            .map_err(transport)?;
        loop {
            match Self::read_one(&mut socket)? {
                (_, Inbound::Ready) => break,
                (_, Inbound::Frame(Frame::GatewayError { code, detail, .. })) => {
                    return Err(DsError::Protocol(format!(
                        "identify refused: {code}: {detail}"
                    )));
                }
                (_, Inbound::Closing(reason)) => return Err(DsError::Protocol(reason)),
                // Nothing else can precede `ready` on a fresh identify.
                _ => {}
            }
        }
        read_timeout(&mut socket, PUMP_WAIT)?;
        self.heartbeat = Duration::from_millis(heartbeat_ms.max(1_000));
        self.last_beat = Instant::now();
        self.unacked = 0;
        self.ack_since = Instant::now();
        self.last_pump = Instant::now();
        self.last_n = 0;
        self.ws = Some(socket);
        // A reconnect is a fresh identify with an empty replay ring, so what arrived while the
        // socket was closed is fetched over rows 4 and 12 before any live frame is returned.
        if self.connected_before {
            self.reconnects += 1;
            self.catch_up()?;
        }
        self.connected_before = true;
        Ok(())
    }

    /// One binary frame, blocking up to the socket's current read timeout.
    fn read_one(socket: &mut WebSocket<TcpStream>) -> Result<(u64, Inbound), DsError> {
        loop {
            match socket.read().map_err(transport)? {
                Message::Binary(bytes) => return decode_frame(&bytes),
                Message::Close(frame) => {
                    return Err(DsError::Transport(format!(
                        "the gateway closed the connection: {frame:?}"
                    )));
                }
                _ => {}
            }
        }
    }

    /// Everything each known group accumulated past `delivered` while the socket was closed, in
    /// seq order, as the frames a live connection would have carried.
    ///
    /// A group the instance answers `E_NOT_FOUND` for is one this device is no longer a member
    /// of (a non-member is E_NOT_FOUND, never E_FORBIDDEN — `internal/ds/sequencer.go`): it is
    /// forgotten rather than failing the reconnect, which is what a live socket does too, since
    /// the gateway stops fanning that group out to the device.
    fn catch_up(&mut self) -> Result<(), DsError> {
        let groups: Vec<(Vec<u8>, u64)> = self
            .delivered
            .iter()
            .map(|(g, s)| (g.clone(), *s))
            .collect();
        for (group_id, after) in groups {
            let mut frames: Vec<(u64, Frame)> = Vec::new();
            let handshakes = match self.handshakes(&group_id, after + 1) {
                Err(e) if e.code() == "E_NOT_FOUND" => {
                    self.delivered.remove(&group_id);
                    continue;
                }
                other => other?,
            };
            for item in handshakes {
                frames.push((
                    item.seq,
                    Frame::MlsHandshake {
                        group_id: group_id.clone(),
                        item,
                    },
                ));
            }
            for item in self.messages(&group_id, after + 1)? {
                frames.push((
                    item.seq,
                    Frame::MessageCt {
                        group_id: group_id.clone(),
                        item,
                    },
                ));
            }
            frames.sort_by_key(|(seq, _)| *seq);
            for (_, frame) in frames {
                self.accept(frame);
            }
        }
        Ok(())
    }

    /// Drops the socket and remembers why, for `diagnostics`. The next `drain` reopens it.
    fn socket_lost(&mut self, why: String) {
        self.ws = None;
        if self.lost.len() >= 4 {
            self.lost.remove(0);
        }
        self.lost.push(why);
    }

    /// Queues one frame for the next `drain`, dropping a handshake or a message whose seq was
    /// already delivered — the catch-up and the live socket can overlap by a frame or two.
    fn accept(&mut self, frame: Frame) {
        let seq_of = |f: &Frame| match f {
            Frame::MlsHandshake { group_id, item } => Some((group_id.clone(), item.seq)),
            Frame::MessageCt { group_id, item } => Some((group_id.clone(), item.seq)),
            _ => None,
        };
        if let Some((group_id, seq)) = seq_of(&frame) {
            let high = self.delivered.entry(group_id).or_insert(0);
            if seq <= *high {
                return;
            }
            *high = seq;
        }
        self.accepted += 1;
        self.pending.push(frame);
    }

    /// Records that this device takes part in `group_id` from `seq` onward, for a group it learned
    /// of without a frame — its own registration, a Welcome row, its own external commit. Without
    /// this the reconnect's catch-up, which walks `delivered`, never visits the group: a Welcome
    /// joiner that went offline before its first `drain` would miss every message sent meanwhile.
    ///
    /// Only ever raises the high-water mark: what was already delivered is never fetched twice.
    fn learn(&mut self, group_id: &[u8], seq: u64) {
        let high = self.delivered.entry(group_id.to_vec()).or_insert(0);
        *high = (*high).max(seq.saturating_sub(1));
    }

    fn send_heartbeat(&mut self) -> Result<(), DsError> {
        let beat = encode(|e| {
            e.array(4).uint(6).uint(0).null();
            e.array(2).uint(self.last_n).uint(1);
        });
        if let Some(ws) = self.ws.as_mut() {
            ws.send(Message::Binary(beat.into())).map_err(transport)?;
            if self.unacked == 0 {
                self.ack_since = Instant::now();
            }
            self.unacked += 1;
        }
        self.last_beat = Instant::now();
        Ok(())
    }

    /// How long a heartbeat may go unanswered, with the socket delivering nothing, before the
    /// connection is treated as dead: half the heartbeat interval, between 5 and 15 seconds.
    fn ack_timeout(&self) -> Duration {
        (self.heartbeat / 2).clamp(Duration::from_secs(5), Duration::from_secs(15))
    }

    /// A `heartbeat_ack` answers the oldest heartbeat still outstanding.
    fn acked(&mut self) {
        self.unacked = self.unacked.saturating_sub(1);
        self.ack_since = Instant::now();
    }

    /// Reads every frame that is already buffered, without blocking on an empty socket.
    ///
    /// The socket carries a short `SO_RCVTIMEO` (set once the handshake is done) rather than a
    /// non-blocking toggle per pump: a short read timeout gives the same "return promptly when
    /// there is nothing to read" behaviour, surfaces as `WouldBlock`/`TimedOut`, and leaves any
    /// partially read frame in tungstenite's own buffer for the next pump.
    ///
    /// A connection the instance closes (`invalid_session`, `reconnect`, a close frame, a reset)
    /// is dropped here; the next `drain` reopens it while the scenario has the device online. So
    /// is one that stays open but stops delivering: a heartbeat left unanswered for `ack_timeout`
    /// of reading with nothing arriving (protocol/02's liveness, seen from the client's side).
    fn pump(&mut self) -> Result<(), DsError> {
        if self.ws.is_none() {
            return Ok(());
        }
        // Silence while this client was busy elsewhere is not the connection's fault: the ack
        // may be sitting behind a backlog the socket has not been asked for yet.
        if self.unacked > 0 && self.last_pump.elapsed() >= self.ack_timeout() {
            self.ack_since = Instant::now();
        }
        if self.last_beat.elapsed() >= self.heartbeat / 2 {
            self.send_heartbeat()?;
        }
        loop {
            let Some(ws) = self.ws.as_mut() else {
                return Ok(());
            };
            match ws.read() {
                Ok(Message::Binary(bytes)) => {
                    let (n, inbound) = decode_frame(&bytes)?;
                    self.last_n = self.last_n.max(n);
                    // The socket is delivering, so an ack still owed is only queued behind this.
                    self.ack_since = Instant::now();
                    match inbound {
                        Inbound::Frame(frame) => self.accept(frame),
                        Inbound::Closing(reason) => self.socket_lost(format!("closing: {reason}")),
                        Inbound::HeartbeatAck => self.acked(),
                        Inbound::Ring(ring) => self.keep_ring(ring),
                        Inbound::Hello { .. }
                        | Inbound::Ready
                        | Inbound::Ignored
                        | Inbound::Signal { .. } => {}
                    }
                }
                Ok(Message::Close(frame)) => self.socket_lost(format!("close frame: {frame:?}")),
                Ok(_) => {}
                Err(e) if is_timeout(&e) => {
                    self.last_pump = Instant::now();
                    let timeout = self.ack_timeout();
                    if self.unacked > 0 && self.ack_since.elapsed() >= timeout {
                        self.missed_acks += 1;
                        let unacked = self.unacked;
                        self.socket_lost(format!(
                            "no heartbeat_ack within {timeout:?} ({unacked} unanswered)"
                        ));
                    }
                    return Ok(());
                }
                Err(
                    e @ (tungstenite::Error::ConnectionClosed
                    | tungstenite::Error::AlreadyClosed
                    | tungstenite::Error::Protocol(_)
                    | tungstenite::Error::Io(_)),
                ) => self.socket_lost(format!("read error: {e}")),
                Err(e) => return Err(transport(e)),
            }
        }
    }

    /// Sends a heartbeat and reads until its `heartbeat_ack`, keeping every frame that arrives
    /// meanwhile. The gateway handles one connection's frames in order, so on return every frame
    /// this client sent before the heartbeat has been handled — which is what makes a following
    /// control-listener call (an `advance_clock` that runs the watchdog) see a `commit_ack`.
    ///
    /// It waits for every heartbeat still outstanding, not the first ack: an ack owed to an
    /// earlier `pump`'s heartbeat answers that one, not this barrier's.
    fn barrier(&mut self) -> Result<(), DsError> {
        self.send_heartbeat()?;
        let deadline = Instant::now() + HANDSHAKE_WAIT;
        loop {
            let Some(ws) = self.ws.as_mut() else {
                return Err(DsError::Transport(
                    "the gateway closed the connection before answering the heartbeat".into(),
                ));
            };
            match ws.read() {
                Ok(Message::Binary(bytes)) => {
                    let (n, inbound) = decode_frame(&bytes)?;
                    self.last_n = self.last_n.max(n);
                    self.ack_since = Instant::now();
                    match inbound {
                        Inbound::HeartbeatAck => {
                            self.acked();
                            if self.unacked == 0 {
                                return Ok(());
                            }
                        }
                        Inbound::Frame(frame) => self.accept(frame),
                        Inbound::Ring(ring) => self.keep_ring(ring),
                        Inbound::Closing(reason) => {
                            self.ws = None;
                            return Err(DsError::Transport(reason));
                        }
                        Inbound::Hello { .. }
                        | Inbound::Ready
                        | Inbound::Ignored
                        | Inbound::Signal { .. } => {}
                    }
                }
                Ok(Message::Close(_)) => {
                    self.ws = None;
                    return Err(DsError::Transport(
                        "the gateway closed the connection".into(),
                    ));
                }
                Ok(_) => {}
                Err(e) if is_timeout(&e) => {
                    if Instant::now() >= deadline {
                        return Err(DsError::Transport("no heartbeat_ack".into()));
                    }
                }
                Err(e) => return Err(transport(e)),
            }
        }
    }

    fn url(&self, path: &str) -> String {
        format!("{}{path}", self.base)
    }

    fn get_decoded<T>(
        &self,
        path: &str,
        f: impl FnOnce(&mut Decoder<'_>) -> Result<T, CborError>,
    ) -> Result<T, DsError> {
        let (status, out) = call(&self.agent, "GET", &self.url(path), Some(&self.token), None)?;
        check_status(status, &out)?;
        decode_strict(&out, f).map_err(|e| DsError::Protocol(format!("GET {path}: {e}")))
    }

    fn post_decoded<T>(
        &self,
        path: &str,
        body: &[u8],
        f: impl FnOnce(&mut Decoder<'_>) -> Result<T, CborError>,
    ) -> Result<T, DsError> {
        let out = post_cbor(&self.agent, &self.url(path), Some(&self.token), body)?;
        decode_strict(&out, f).map_err(|e| DsError::Protocol(format!("POST {path}: {e}")))
    }

    /// A POST whose success is a status and no body the client reads (`204`, or row 9's
    /// `202 []`).
    fn post_status(&self, path: &str, body: &[u8]) -> Result<(), DsError> {
        post_cbor(&self.agent, &self.url(path), Some(&self.token), body).map(|_| ())
    }

    fn group_path(g: &GroupId, tail: &str) -> String {
        format!("/v1/groups/{}{tail}", hex::encode(g))
    }

    /// POSTs JSON to the test host's control listener. See [`control_post`].
    fn post_control(&self, path: &str, body: &str) -> Result<Vec<u8>, DsError> {
        control(&self.agent, "POST", path, Some(body))
    }
}

/// The test host's control listener, whose base URL arrives in `DILLA_TESTKIT_CONTROL`. It is a
/// separate origin from the instance on purpose: the control routes are never mounted on the
/// public mux, so a scenario cannot reach them by accident and no production build can serve
/// them. `/debug` is not a `/v1` route and speaks JSON, not deterministic CBOR — the Go handler
/// decodes `{"seconds": n}`, and a CBOR body there is a 400.
fn control(
    agent: &ureq::Agent,
    method: &str,
    path: &str,
    json: Option<&str>,
) -> Result<Vec<u8>, DsError> {
    let base = control_base().ok_or_else(|| {
        DsError::Protocol(
            "DILLA_TESTKIT_CONTROL is unset: this scenario needs the test host's control \
             listener (advance_clock, kick, snapshot, restore_snapshot, expect_quarantined, \
             expect_closed)"
                .into(),
        )
    })?;
    let url = format!("{}{path}", base.trim_end_matches('/'));
    let (status, out) = call(
        agent,
        method,
        &url,
        None,
        json.map(|j| ("application/json", j.as_bytes())),
    )?;
    if !(200..300).contains(&status) {
        return Err(DsError::Protocol(format!(
            "control {method} {path}: {status} {}",
            String::from_utf8_lossy(&out)
        )));
    }
    Ok(out)
}

/// The discovery document of the instance at `base`: its id, which a remote scenario binds its
/// groups to (invariant 1 refuses a binding that names another instance), and its current
/// external-sender key.
pub fn instance_document(base: &str) -> Result<InstanceDocument, DsError> {
    host_of(base)?;
    instance_document_of(&new_agent(), base.trim_end_matches('/'))
}

/// `POST <control><path>` with a JSON body, for the runner's control-listener verbs.
pub fn control_post(path: &str, json: &str) -> Result<Vec<u8>, DsError> {
    control(&new_agent(), "POST", path, Some(json))
}

/// `GET <control><path>`, answered as the JSON body's bytes.
pub fn control_get(path: &str) -> Result<Vec<u8>, DsError> {
    control(&new_agent(), "GET", path, None)
}

/// The test host's fake clock in unix seconds (`GET /debug/clock` → `{"now": n}`): a TOTP code is computed for the
/// instance's time, which `advance_clock` moves, not for the machine's.
/// The control listener's base URL; an empty `DILLA_TESTKIT_CONTROL` (the e2e driver passes `''`) is unset.
fn control_base() -> Option<String> {
    std::env::var("DILLA_TESTKIT_CONTROL")
        .ok()
        .filter(|v| !v.is_empty())
}

fn control_is_set() -> bool {
    control_base().is_some()
}

fn host_unix() -> Result<u64, DsError> {
    let out = control_get("/debug/clock")?;
    serde_json::from_slice::<serde_json::Value>(&out)
        .ok()
        .and_then(|v| v.get("now").and_then(serde_json::Value::as_u64))
        .ok_or_else(|| DsError::Protocol("GET /debug/clock answered no `now`".into()))
}

impl DeliveryService for HttpDs {
    /// Row 10: `[packages([bstr]), last_resort(bstr|null)]` -> `201 [count]`. The session names
    /// the device; `d` must be the device this client holds a session for.
    fn publish_key_packages(
        &mut self,
        d: &Device,
        kps: Vec<Vec<u8>>,
        last_resort: Option<Vec<u8>>,
    ) -> Result<usize, DsError> {
        if d.id() != self.device {
            return Err(DsError::Protocol(format!(
                "this client holds a session for {}, not {}",
                self.device.to_hex(),
                d.id().to_hex()
            )));
        }
        let body = encode(|e| {
            e.array(2).array(kps.len());
            for kp in &kps {
                e.bytes(kp);
            }
            e.opt_bytes(last_resort.as_deref());
        });
        self.post_decoded("/v1/keypackages", &body, |d| {
            d.array(1)?;
            small(d.uint()?)
        })
    }

    /// Row 1: `[group_id, binding, group_info, ratchet_tree]` -> `201 [group_id, next_seq]`. The
    /// group id is the binding's `target_id` (invariant 1), which the instance checks.
    ///
    /// The answer's `next_seq` is the first seq the group's log will hold, so the creator catches
    /// up from there on a reconnect.
    fn register_group(&mut self, r: RegisterRequest) -> Result<RegisterResult, DsError> {
        let binding = dilla_core::mls::DillaBinding::decode(&r.binding)
            .map_err(|_| DsError::BindingInvalid)?;
        // Row 1's group_id is the GroupInfo's (the instance refuses any other). A group made with the target as its
        // id names the target; a channel's second group (a DM's call group, web-3) names its own. Bytes that are no
        // GroupInfo (a unit test's placeholder) fall back to the target, which the instance would refuse anyway.
        let group_id = group_id_of(&r.group_info).unwrap_or_else(|| binding.target_id.to_vec());
        let body = encode(|e| {
            e.array(4)
                .bytes(&group_id)
                .bytes(&r.binding)
                .bytes(&r.group_info)
                .bytes(&r.ratchet_tree);
        });
        let registered = self.post_decoded("/v1/groups", &body, |d| {
            d.array(2)?;
            Ok(RegisterResult {
                group_id: d.bytes()?.to_vec(),
                seq: d.uint()?,
            })
        })?;
        self.learn(&registered.group_id, registered.seq);
        Ok(registered)
    }

    /// Row 2: `[epoch, group_info, tree_hash, next_seq]`.
    fn group_info(&mut self, g: &GroupId) -> Result<GroupInfoResp, DsError> {
        self.get_decoded(&Self::group_path(g, "/info"), |d| {
            d.array(4)?;
            Ok(GroupInfoResp {
                epoch: d.uint()?,
                group_info: bytes_or_null(d)?,
                tree_hash: bytes_or_null(d)?,
                seq: d.uint()?,
            })
        })
    }

    /// Row 3: `[epoch, ratchet_tree, tree_hash]`.
    fn ratchet_tree(&mut self, g: &GroupId) -> Result<TreeResp, DsError> {
        self.get_decoded(&Self::group_path(g, "/tree"), |d| {
            d.array(3)?;
            Ok(TreeResp {
                epoch: d.uint()?,
                ratchet_tree: bytes_or_null(d)?,
                tree_hash: bytes_or_null(d)?,
            })
        })
    }

    /// Row 4, paged until a short page: `[[seq, epoch, kind, sender, blob]]`.
    fn handshakes(&mut self, g: &GroupId, from: u64) -> Result<Vec<HandshakeItem>, DsError> {
        let mut out: Vec<HandshakeItem> = Vec::new();
        let mut next = from;
        loop {
            let path = Self::group_path(
                g,
                &format!("/handshakes?from={next}&limit={HANDSHAKE_PAGE}"),
            );
            let page = self.get_decoded(&path, |d| {
                let n = array_or_null(d)?;
                (0..n)
                    .map(|_| handshake_item(d))
                    .collect::<Result<Vec<_>, _>>()
            })?;
            let short = page.len() < HANDSHAKE_PAGE;
            if let Some(last) = page.last() {
                next = last.seq + 1;
            }
            out.extend(page);
            if short {
                return Ok(out);
            }
        }
    }

    /// Row 11: `[blob, last_resort(uint), kp_ref]`.
    fn take_key_package(&mut self, target: &DeviceId) -> Result<KeyPackageResp, DsError> {
        let path = format!("/v1/devices/{}/keypackage", hex::encode(target.as_bytes()));
        self.get_decoded(&path, |d| {
            d.array(3)?;
            Ok(KeyPackageResp {
                blob: bytes_or_null(d)?,
                last_resort: d.uint()? != 0,
                kp_ref: bytes_or_null(d)?,
            })
        })
    }

    /// Row 5: `[epoch, commit, group_info, welcomes([[device_id, blob]]), ratchet_tree|null]` ->
    /// `[seq, epoch]`.
    fn post_commit(&mut self, g: &GroupId, c: CommitRequest) -> Result<CommitResult, DsError> {
        let body = encode(|e| {
            e.array(5)
                .uint(c.epoch)
                .bytes(&c.commit)
                .bytes(&c.group_info);
            e.array(c.welcomes.len());
            for (device, blob) in &c.welcomes {
                e.array(2).bytes(device.as_bytes()).bytes(blob);
            }
            e.null();
        });
        self.post_decoded(&Self::group_path(g, "/commit"), &body, |d| {
            d.array(2)?;
            Ok(CommitResult {
                seq: d.uint()?,
                epoch: d.uint()?,
            })
        })
    }

    /// Row 8: `[external_commit, group_info]` -> `[seq, epoch]`.
    ///
    /// The joiner is a member from its own commit on, and the gateway fans that commit out to it
    /// like to any member, so a reconnect catches up from the commit's own seq.
    fn post_external_commit(
        &mut self,
        g: &GroupId,
        c: ResyncRequest,
    ) -> Result<CommitResult, DsError> {
        let body = encode(|e| {
            e.array(2).bytes(&c.external_commit).bytes(&c.group_info);
        });
        let committed = self.post_decoded(&Self::group_path(g, "/resync"), &body, |d| {
            d.array(2)?;
            Ok(CommitResult {
                seq: d.uint()?,
                epoch: d.uint()?,
            })
        })?;
        self.learn(g, committed.seq);
        Ok(committed)
    }

    /// Row 6: `[epoch, proposal]` -> `[seq]`.
    fn post_proposal(
        &mut self,
        g: &GroupId,
        epoch: u64,
        proposal: Vec<u8>,
    ) -> Result<u64, DsError> {
        let body = encode(|e| {
            e.array(2).uint(epoch).bytes(&proposal);
        });
        self.post_decoded(&Self::group_path(g, "/proposal"), &body, |d| {
            d.array(1)?;
            d.uint()
        })
    }

    /// Row 7: `[epoch, private_message, class]` -> `[seq, franking_tag, recv_ts]` (web3.7 SB1; the
    /// two-element body is refused since web3.8 S1). The instance reads the commitment out of the
    /// message's own `authenticated_data`.
    fn post_message_from(
        &mut self,
        g: &GroupId,
        epoch: u64,
        pm: Vec<u8>,
        class: u8,
    ) -> Result<UploadResult, DsError> {
        let body = encode(|e| {
            e.array(3).uint(epoch).bytes(&pm).uint(u64::from(class));
        });
        self.post_decoded(&Self::group_path(g, "/message"), &body, |d| {
            d.array(3)?;
            Ok(UploadResult {
                seq: d.uint()?,
                franking_tag: bytes_or_null(d)?,
                recv_ts: d.uint()?,
            })
        })
    }

    /// Row 12, paged until a short page:
    /// `[[seq, epoch, uploader_device, blob|null, commitment|null, franking_tag, recv_ts, deleted,
    /// class|null]]` (message_row). A deleted message comes back with an empty blob and
    /// `deleted: true`.
    fn messages(&mut self, g: &GroupId, from: u64) -> Result<Vec<MessageItem>, DsError> {
        let mut out: Vec<MessageItem> = Vec::new();
        let mut next = from;
        loop {
            let path = Self::group_path(g, &format!("/messages?from={next}&limit={MESSAGE_PAGE}"));
            let page = self.get_decoded(&path, |d| {
                let n = array_or_null(d)?;
                let mut items = Vec::with_capacity(n);
                for _ in 0..n {
                    items.push(message_row(d)?);
                }
                Ok(items)
            })?;
            let short = page.len() < MESSAGE_PAGE;
            if let Some(last) = page.last() {
                next = last.seq + 1;
            }
            out.extend(page);
            if short {
                return Ok(out);
            }
        }
    }

    /// Row 15, looped on `after` until a short page, because the endpoint pages and the trait's
    /// signature does not (interface deviation B7):
    /// `[[welcome_id, group_id, epoch, commit_seq, blob, ratchet_tree, tree_hash]]`.
    fn welcomes(&mut self) -> Result<Vec<WelcomeItem>, DsError> {
        let mut out: Vec<WelcomeItem> = Vec::new();
        let mut after = 0u64;
        loop {
            let path = format!("/v1/welcomes?after={after}&limit={WELCOME_PAGE}");
            let page = self.get_decoded(&path, |d| {
                let n = array_or_null(d)?;
                let mut items = Vec::with_capacity(n);
                for _ in 0..n {
                    d.array(7)?;
                    items.push(WelcomeItem {
                        welcome_id: d.uint()?,
                        group_id: d.bytes()?.to_vec(),
                        epoch: d.uint()?,
                        commit_seq: d.uint()?,
                        blob: bytes_or_null(d)?,
                        ratchet_tree: bytes_or_null(d)?,
                        tree_hash: bytes_or_null(d)?,
                    });
                }
                Ok(items)
            })?;
            let short = page.len() < WELCOME_PAGE;
            if let Some(last) = page.last() {
                after = last.welcome_id;
            }
            out.extend(page);
            if short {
                // The commit that added this device replaced the member list before its fan-out,
                // so the device is sent that Add handshake and everything after it: a reconnect
                // catches up from `commit_seq`, whether or not a frame was drained meanwhile.
                for item in &out {
                    self.learn(&item.group_id, item.commit_seq);
                }
                return Ok(out);
            }
        }
    }

    /// Row 16: `DELETE /v1/welcomes/{welcome_id}` -> `204`. The id is the table's decimal
    /// surrogate key, the one protocol identifier that is not 16 random bytes.
    fn ack_welcome(&mut self, welcome_id: u64) -> Result<(), DsError> {
        let (status, out) = call(
            &self.agent,
            "DELETE",
            &self.url(&format!("/v1/welcomes/{welcome_id}")),
            Some(&self.token),
            None,
        )?;
        check_status(status, &out)
    }

    /// Row 9: `[epoch, seq, reason]` -> `202 []`.
    fn fork_report(
        &mut self,
        g: &GroupId,
        epoch: u64,
        seq: u64,
        reason: &str,
    ) -> Result<(), DsError> {
        let body = encode(|e| {
            e.array(3).uint(epoch).uint(seq).text(reason);
        });
        self.post_status(&Self::group_path(g, "/fork-report"), &body)
    }

    /// Row 14: `[group_info, tail([[seq, epoch, kind, sender, blob]]), ratchet_tree|null]` ->
    /// `[epoch, next_seq]`. The answer is NOT row 5's `[seq, epoch]`: protocol/02 spells the two
    /// rows differently and a client decodes by position, so `seq` here is `next_seq - 1`, the
    /// last seq the heal wrote.
    fn heal(&mut self, g: &GroupId, h: HealRequest) -> Result<CommitResult, DsError> {
        let body = encode(|e| {
            e.array(3).bytes(&h.group_info).array(h.tail.len());
            for item in &h.tail {
                e.array(5)
                    .uint(item.seq)
                    .uint(item.epoch)
                    .uint(u64::from(item.kind))
                    .opt_uint(item.sender.map(u64::from))
                    .bytes(&item.blob);
            }
            e.opt_bytes(h.ratchet_tree.as_deref());
        });
        self.post_decoded(&Self::group_path(g, "/heal"), &body, |d| {
            d.array(2)?;
            let epoch = d.uint()?;
            let next_seq = d.uint()?;
            Ok(CommitResult {
                seq: next_seq.saturating_sub(1),
                epoch,
            })
        })
    }

    /// Row 18: `[last_seq, last_epoch]` -> `204`.
    fn advance_cursor(&mut self, g: &GroupId, seq: u64, epoch: u64) -> Result<(), DsError> {
        let body = encode(|e| {
            e.array(2).uint(seq).uint(epoch);
        });
        self.post_status(&Self::group_path(g, "/cursor"), &body)
    }

    /// Frames received since the last drain. Offline, nothing is read and nothing is returned —
    /// what the socket held when the device went offline stays queued, and what the instance
    /// sends meanwhile is fetched by the catch-up when the device comes back, which is
    /// `DsStub::drain`'s "an offline device's queue is left untouched".
    /// The client's side, then the instance's (`GET /debug/conn` on the test host's control
    /// listener): the device's live connections with their writers' enqueued and written counts,
    /// its place in the group's fan-out list, and the group's epoch and seq. A failed
    /// `expect_decrypts` prints both, so a device that stopped receiving says which side stopped.
    /// Without a control listener the instance's side is the reason it is missing.
    fn diagnostics(&self, g: &GroupId) -> String {
        let server = control_get(&format!(
            "/debug/conn?device={}&group={}",
            self.device.to_hex(),
            hex::encode(g)
        ))
        .map_or_else(
            |e| format!("unavailable: {e}"),
            |body| String::from_utf8_lossy(&body).trim().to_owned(),
        );
        format!(
            "socket {}, reconnects {}, missed acks {}, heartbeats unanswered {}, lost {:?}, frames accepted {}, delivered seq for the group {:?}, last n {}; server {server}",
            if self.ws.is_some() { "open" } else { "closed" },
            self.reconnects,
            self.missed_acks,
            self.unacked,
            self.lost,
            self.accepted,
            self.delivered.get(g),
            self.last_n,
        )
    }

    fn drain(&mut self) -> Result<Vec<Frame>, DsError> {
        if !self.online {
            return Ok(Vec::new());
        }
        if self.ws.is_none() {
            self.open_socket()?;
        }
        self.pump()?;
        // A connection the instance closed under this drain (a restart, a restore, an expired
        // session) is reopened now rather than on the next drain: the reconnect's catch-up is what
        // brings in what the dead connection would have carried, and a drain that returned empty
        // here would tell the scenario there was nothing to read.
        if self.ws.is_none() {
            self.open_socket()?;
            self.pump()?;
        }
        Ok(std::mem::take(&mut self.pending))
    }

    fn set_online(&mut self, online: bool) -> Result<(), DsError> {
        if !online {
            // The gateway may already have written frames the device has not drained. Closing
            // over them would lose them for good: the catch-up resumes after `delivered`, which
            // only a pumped frame advances, and the stub keeps an offline device's queue. So they
            // are read into `pending` first and wait there for the next online `drain`.
            self.pump()?;
        }
        self.online = online;
        match (online, self.ws.take()) {
            (false, Some(mut ws)) => {
                // A courtesy close; the instance's online predicate turns on the connection
                // being gone, which dropping the socket guarantees either way.
                let _ = ws.close(None);
                let _ = ws.flush();
                Ok(())
            }
            (true, None) => self.open_socket(),
            (_, ws) => {
                self.ws = ws;
                Ok(())
            }
        }
    }

    /// The instance's clock lives behind the test host's `/debug` listener, never the public mux:
    /// no `dillad` build carries that route.
    ///
    /// `/debug` speaks JSON, not deterministic CBOR — it is not a `/v1` route, and the Go handler
    /// decodes `{"seconds": n}`. Posting CBOR here is a 400 on every advance_clock.
    fn advance_clock(&mut self, secs: u64) -> Result<(), DsError> {
        let body = format!("{{\"seconds\":{secs}}}");
        self.post_control("/debug/clock", &body).map(|_| ())
    }

    /// Row 19: `[[ref, kind, target_leaf|null, blob, void(uint)]]`.
    fn proposals(&mut self, g: &GroupId) -> Result<Vec<ProposalItem>, DsError> {
        self.get_decoded(&Self::group_path(g, "/proposals"), |d| {
            let n = array_or_null(d)?;
            (0..n)
                .map(|_| {
                    d.array(5)?;
                    Ok(ProposalItem {
                        proposal_ref: bytes_or_null(d)?,
                        kind: small(d.uint()?)?,
                        target_leaf: d.opt_uint()?.map(small).transpose()?,
                        blob: bytes_or_null(d)?,
                        void: d.uint()? != 0,
                    })
                })
                .collect::<Result<Vec<_>, CborError>>()
        })
    }

    /// Opcode 12, `[round]`, group-scoped: the one client frame that carries a `group_id`
    /// (protocol/02 § Gateway frames). An offline device has no connection to send it on.
    fn ack_commit(&mut self, g: &GroupId, round: u64) -> Result<(), DsError> {
        if !self.online {
            return Err(DsError::Protocol(
                "an offline device cannot acknowledge an election".into(),
            ));
        }
        if self.ws.is_none() {
            self.open_socket()?;
        }
        let frame = encode(|e| {
            e.array(4).uint(12).uint(0).bytes(g);
            e.array(1).uint(round);
        });
        let ws = self
            .ws
            .as_mut()
            .ok_or_else(|| DsError::Transport("no gateway connection".into()))?;
        ws.send(Message::Binary(frame.into())).map_err(transport)?;
        self.barrier()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cbor(f: impl FnOnce(&mut Encoder)) -> Vec<u8> {
        encode(f)
    }

    /// protocol/09's eleven-element discovery document: the instance id is element 3 and the
    /// external-sender key id and public key are elements 9 and 10, which a remote scenario puts
    /// in every text group's external_senders extension instead of asking the test host.
    #[test]
    fn the_discovery_document_carries_the_external_sender_key() {
        let doc = cbor(|e| {
            e.array(11);
            e.array(1).uint(1);
            e.array(1).uint(1);
            e.array(1).uint(1);
            e.bytes(&[0x11; 16]).uint(1).text("dilla.test").uint(0);
            e.array(2).uint(0).uint(1);
            e.uint(1).bytes(&[0x22; 16]).bytes(&[0x33; 32]);
        });
        let got = decode_instance_document(&doc).unwrap();
        assert_eq!(
            got,
            InstanceDocument {
                instance_id: [0x11; 16],
                external_sender_key_id: [0x22; 16],
                external_sender_pub: [0x33; 32],
            }
        );
        // The nine-element document of before the key was published is refused, not misread.
        let old = cbor(|e| {
            e.array(9);
            e.array(1).uint(1);
            e.array(1).uint(1);
            e.array(1).uint(1);
            e.bytes(&[0x11; 16]).uint(1).text("dilla.test").uint(0);
            e.array(2).uint(0).uint(1);
            e.uint(1);
        });
        assert!(decode_instance_document(&old).is_err());
    }

    /// WB product walk I6: a DM start's seventh element, `rung`, is read past; six elements still
    /// decode, and five or eight are refused.
    #[test]
    fn a_call_start_answer_has_six_or_seven_elements() {
        let answer = |n: usize| {
            cbor(|e| {
                e.array(n)
                    .bytes(&[1; 16])
                    .bytes(&[2; 16])
                    .text("ws://x")
                    .text("t");
                e.array(1)
                    .array(3)
                    .array(1)
                    .text("turn:x")
                    .text("u")
                    .text("c");
                e.array(3).uint(64_000).uint(2_500_000).uint(1);
                if n >= 7 {
                    e.array(1).bytes(&[3; 16]);
                }
                if n == 8 {
                    e.uint(0);
                }
            })
        };
        for n in [6, 7] {
            let got = decode_call_started(&answer(n), true).unwrap();
            assert_eq!(
                (got.call_id, got.group_id, got.caps),
                ([1; 16], [2; 16], [64_000, 2_500_000, 1])
            );
            assert!(got.created);
        }
        assert!(decode_call_started(&answer(8), true).is_err());
    }

    /// protocol/02's error array: the code, the detail and every extended position, whatever the
    /// status, so `expect_reject` sees the same thing against either delivery service.
    #[test]
    fn a_non_2xx_answer_is_the_protocol_02_error_it_carries() {
        let conflict = cbor(|e| {
            e.array(5)
                .text("E_COMMIT_CONFLICT")
                .text("another commit won this epoch")
                .null()
                .bytes(&[7, 7])
                .array(1)
                .bytes(&[8]);
        });
        match check_status(409, &conflict) {
            Err(DsError::CommitConflict {
                winning_commit,
                proposals,
            }) => {
                assert_eq!(winning_commit, vec![7, 7]);
                assert_eq!(proposals, vec![vec![8]]);
            }
            other => panic!("{other:?}"),
        }

        let required = cbor(|e| {
            e.array(4)
                .text("E_COMMIT_REQUIRED")
                .text("outstanding proposals must be committed first")
                .uint(1500)
                .null();
        });
        let err = check_status(425, &required).unwrap_err();
        assert_eq!(err.code(), "E_COMMIT_REQUIRED");
        assert_eq!(err.http_status(), 425);

        let invalid = cbor(|e| {
            e.array(4)
                .text("E_COMMIT_INVALID")
                .text("commit refused by rule group_info_epoch")
                .null()
                .text("group_info_epoch");
        });
        match check_status(422, &invalid) {
            Err(DsError::CommitInvalid { reason }) => assert_eq!(reason, "group_info_epoch"),
            other => panic!("{other:?}"),
        }

        // A code the stub never raises keeps its code and its real status.
        let forbidden = cbor(|e| {
            e.array(3).text("E_FORBIDDEN").text("not a member").null();
        });
        let err = check_status(403, &forbidden).unwrap_err();
        assert_eq!(err.code(), "E_FORBIDDEN");
        assert_eq!(err.http_status(), 403);
        assert!(err.to_string().contains("E_FORBIDDEN"), "{err}");

        // The one status override protocol/02 permits: the code keeps the status it came with.
        let duplicate = cbor(|e| {
            e.array(3).text("E_NOT_FOUND").text("gone").null();
        });
        assert_eq!(
            check_status(410, &duplicate).unwrap_err().http_status(),
            410
        );

        assert!(check_status(204, &[]).is_ok());
        assert!(matches!(
            check_status(500, b"not cbor"),
            Err(DsError::Protocol(_))
        ));
        let unknown = cbor(|e| {
            e.array(3).text("E_MADE_UP").text("").null();
        });
        assert!(matches!(
            check_status(400, &unknown),
            Err(DsError::Protocol(_))
        ));
    }

    /// web3.7 SB1: op 19 carries the upload class as its 7th element, a uint or null; a six-element
    /// op 19 still decodes (class `None`), and five or eight elements are refused.
    #[test]
    fn op_19_carries_the_upload_class() {
        let group = [0x33u8; 16];
        let frame = |n: u64, class: Option<Option<u64>>, extra: bool| {
            cbor(|e| {
                e.array(4).uint(19).uint(1).bytes(&group);
                let len = 6 + usize::from(class.is_some()) + usize::from(extra);
                e.array(len)
                    .uint(n)
                    .uint(3)
                    .bytes(&[0xaa; 16])
                    .bytes(&[4])
                    .bytes(&[0xbb; 32])
                    .uint(1_758_659_640);
                match class {
                    Some(Some(c)) => {
                        e.uint(c);
                    }
                    Some(None) => {
                        e.null();
                    }
                    None => {}
                }
                if extra {
                    e.uint(0);
                }
            })
        };
        for (class, want) in [
            (Some(Some(0)), Some(0)),
            (Some(Some(1)), Some(1)),
            (Some(None), None),
            (None, None),
        ] {
            match decode_frame(&frame(5, class, false)).unwrap() {
                (_, Inbound::Frame(Frame::MessageCt { item, .. })) => {
                    assert_eq!(item.seq, 5);
                    assert_eq!(item.class, want, "{class:?}");
                }
                _ => panic!("message.ct {class:?}"),
            }
        }
        assert!(
            decode_frame(&frame(5, Some(Some(0)), true)).is_err(),
            "eight elements"
        );
        let five = cbor(|e| {
            e.array(4).uint(19).uint(1).bytes(&group);
            e.array(5)
                .uint(5)
                .uint(3)
                .bytes(&[0xaa; 16])
                .bytes(&[4])
                .bytes(&[0xbb; 32]);
        });
        assert!(decode_frame(&five).is_err(), "five elements");
    }

    /// web3.7 SB1: a row 12 item is nine elements, the 9th the upload class; an eight-element row
    /// still decodes (class `None`), and a ten-element row is a shape error.
    #[test]
    fn message_rows_carry_the_upload_class() {
        let row = |len: usize, class: Option<u64>| {
            cbor(|e| {
                e.array(len)
                    .uint(5)
                    .uint(2)
                    .bytes(&[0xaa; 16])
                    .bytes(&[4])
                    .null()
                    .bytes(&[0xbb; 32])
                    .uint(1_758_659_640)
                    .uint(0);
                if len >= 9 {
                    match class {
                        Some(c) => {
                            e.uint(c);
                        }
                        None => {
                            e.null();
                        }
                    }
                }
                if len >= 10 {
                    e.uint(0);
                }
            })
        };
        let decode = |b: &[u8]| decode_strict(b, |d: &mut Decoder<'_>| message_row(d));
        let nine = decode(&row(9, Some(0))).unwrap();
        assert_eq!((nine.seq, nine.class, nine.deleted), (5, Some(0), false));
        assert_eq!(decode(&row(9, Some(1))).unwrap().class, Some(1));
        assert_eq!(decode(&row(9, None)).unwrap().class, None);
        let eight = decode(&row(8, None)).unwrap();
        assert_eq!((eight.seq, eight.class), (5, None));
        assert!(matches!(
            decode(&row(10, Some(0))),
            Err(CborError::WrongArrayLen { actual: 10, .. })
        ));
    }

    /// Every delivery-service opcode decodes into the frame the stub would have queued, and the
    /// replay `n` comes back beside it.
    #[test]
    fn gateway_frames_decode_into_the_stubs_frame_shapes() {
        let group = [0x33u8; 16];
        let handshake = cbor(|e| {
            e.array(4).uint(16).uint(7).bytes(&group);
            e.array(5).uint(4).uint(2).uint(1).uint(0).bytes(&[1, 2, 3]);
        });
        match decode_frame(&handshake).unwrap() {
            (7, Inbound::Frame(Frame::MlsHandshake { group_id, item })) => {
                assert_eq!(group_id, group.to_vec());
                assert_eq!(
                    (item.seq, item.epoch, item.kind, item.sender),
                    (4, 2, 1, Some(0))
                );
                assert_eq!(item.blob, vec![1, 2, 3]);
            }
            _ => panic!("handshake"),
        }

        let external = cbor(|e| {
            e.array(4).uint(16).uint(8).bytes(&group);
            e.array(5).uint(5).uint(2).uint(2).null().bytes(&[9]);
        });
        match decode_frame(&external).unwrap() {
            (_, Inbound::Frame(Frame::MlsHandshake { item, .. })) => {
                assert_eq!(item.sender, None)
            }
            _ => panic!("external commit"),
        }

        let message = cbor(|e| {
            e.array(4).uint(19).uint(9).bytes(&group);
            e.array(6)
                .uint(5)
                .uint(3)
                .bytes(&[0xaa; 16])
                .bytes(&[4])
                .bytes(&[0xbb; 32])
                .uint(1_758_659_640);
        });
        match decode_frame(&message).unwrap() {
            (9, Inbound::Frame(Frame::MessageCt { item, .. })) => {
                assert_eq!(item.seq, 5);
                assert_eq!(item.uploader_device, DeviceId::from_bytes([0xaa; 16]));
                assert_eq!(item.franking_tag, [0xbb; 32]);
                assert_eq!(item.recv_ts, 1_758_659_640);
                assert_eq!(item.class, None, "a six-element op 19 predates the class");
            }
            _ => panic!("message.ct"),
        }

        let needed = cbor(|e| {
            e.array(4).uint(17).uint(0).bytes(&group);
            e.array(4).uint(3).null().uint(5000).uint(2);
        });
        match decode_frame(&needed).unwrap() {
            (0, Inbound::Frame(frame @ Frame::CommitNeeded { .. })) => {
                assert_eq!(frame.label(), "mls.commit_needed");
                assert_eq!(frame.field("epoch").as_deref(), Some("3"));
                assert_eq!(frame.field("round").as_deref(), Some("2"));
                assert_eq!(frame.field("proposals").as_deref(), Some("0"));
            }
            _ => panic!("commit_needed"),
        }

        let welcome = cbor(|e| {
            e.array(4).uint(20).uint(10).bytes(&group);
            e.array(6)
                .uint(1)
                .uint(1)
                .uint(3)
                .bytes(&[5])
                .bytes(&[6])
                .bytes(&[0xcc; 32]);
        });
        assert!(matches!(
            decode_frame(&welcome).unwrap(),
            (10, Inbound::Frame(Frame::MlsWelcome { .. }))
        ));

        let epoch = cbor(|e| {
            e.array(4).uint(18).uint(11).bytes(&group);
            e.array(2).uint(3).uint(4);
        });
        assert!(matches!(
            decode_frame(&epoch).unwrap(),
            (
                11,
                Inbound::Frame(Frame::MlsEpochChanged {
                    epoch: 3,
                    seq: 4,
                    ..
                })
            )
        ));

        let hello = cbor(|e| {
            e.array(4).uint(0).uint(0).null();
            e.array(9)
                .array(1)
                .uint(1)
                .array(1)
                .uint(1)
                .array(1)
                .uint(1)
                .uint(30_000)
                .uint(131_584)
                .bytes(&[0x11; 16])
                .uint(1)
                .uint(2_000)
                .uint(500);
        });
        assert!(matches!(
            decode_frame(&hello).unwrap(),
            (
                0,
                Inbound::Hello {
                    heartbeat_ms: 30_000,
                    ..
                }
            )
        ));

        // A group frame without its group is not routable, and a truncated frame is refused.
        let orphan = cbor(|e| {
            e.array(4).uint(18).uint(1).null();
            e.array(2).uint(3).uint(4);
        });
        assert!(decode_frame(&orphan).is_err());
        assert!(decode_frame(&handshake[..handshake.len() - 1]).is_err());
        // An opcode outside the delivery service is read past, not refused.
        let presence = cbor(|e| {
            e.array(4).uint(48).uint(0).null();
            e.array(4).bytes(&[1; 16]).uint(1).uint(2).text("");
        });
        assert!(matches!(
            decode_frame(&presence).unwrap(),
            (0, Inbound::Ignored)
        ));
    }

    #[test]
    fn only_a_plain_http_base_is_spoken_to() {
        assert_eq!(host_of("http://127.0.0.1:4567").unwrap(), "127.0.0.1:4567");
        assert_eq!(host_of("http://127.0.0.1:4567/").unwrap(), "127.0.0.1:4567");
        assert!(host_of("https://dilla.example").is_err());
    }

    /// A client in the state `open_socket` leaves after a first connection, over a socket the test
    /// already holds (or none), so the offline and catch-up paths run without a gateway.
    fn connected(base: &str, ws: Option<WebSocket<TcpStream>>) -> HttpDs {
        HttpDs {
            base: base.to_owned(),
            token: "token".into(),
            device: DeviceId::from_bytes([0xdd; 16]),
            agent: new_agent(),
            ws,
            pending: Vec::new(),
            online: true,
            heartbeat: Duration::from_secs(30),
            last_beat: Instant::now(),
            unacked: 0,
            ack_since: Instant::now(),
            last_pump: Instant::now(),
            last_n: 0,
            delivered: BTreeMap::new(),
            connected_before: true,
            reconnects: 0,
            lost: Vec::new(),
            accepted: 0,
            missed_acks: 0,
            expires: 0,
            rings: Vec::new(),
            totp: None,
        }
    }

    fn seq_of(frame: &Frame) -> Option<u64> {
        match frame {
            Frame::MlsHandshake { item, .. } => Some(item.seq),
            Frame::MessageCt { item, .. } => Some(item.seq),
            _ => None,
        }
    }

    /// `go_offline` must not throw away what the gateway already wrote to the socket: the stub
    /// keeps an offline device's queue, and a frame lost here is also a group the reconnect's
    /// catch-up never learns of.
    #[test]
    fn going_offline_keeps_the_frames_the_socket_already_holds() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let group = [0x44u8; 16];
        let (sent, written) = std::sync::mpsc::channel();
        let gateway = std::thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            let mut ws = tungstenite::accept(stream).unwrap();
            let handshake = encode(|e| {
                e.array(4).uint(16).uint(1).bytes(&group);
                e.array(5).uint(4).uint(2).uint(1).uint(0).bytes(&[1]);
            });
            let message = encode(|e| {
                e.array(4).uint(19).uint(2).bytes(&group);
                e.array(6)
                    .uint(5)
                    .uint(3)
                    .bytes(&[0xaa; 16])
                    .bytes(&[4])
                    .bytes(&[0xbb; 32])
                    .uint(1_758_659_640);
            });
            ws.send(Message::Binary(handshake.into())).unwrap();
            ws.send(Message::Binary(message.into())).unwrap();
            sent.send(()).unwrap();
            // Hold the connection until the client lets it go.
            while ws.read().is_ok() {}
        });

        let stream = TcpStream::connect(addr).unwrap();
        let (mut socket, _) = tungstenite::client(format!("ws://{addr}/gateway"), stream).unwrap();
        read_timeout(&mut socket, PUMP_WAIT).unwrap();
        written.recv().unwrap();
        let mut ds = connected(&format!("http://{addr}"), Some(socket));

        ds.set_online(false).unwrap();
        assert!(ds.ws.is_none(), "offline means no socket");
        assert!(
            ds.drain().unwrap().is_empty(),
            "an offline drain returns nothing"
        );
        let held: Vec<u64> = ds.pending.iter().filter_map(seq_of).collect();
        assert_eq!(
            held,
            vec![4, 5],
            "the buffered frames wait for the next online drain"
        );
        assert_eq!(ds.delivered.get(group.as_slice()), Some(&5));
        gateway.join().unwrap();
    }

    /// With no control listener and the machine's clock allowed (netprobe against a real instance), enrolment
    /// reads no `/debug` route and confirms a code for the machine's time; a caller that does not allow it is
    /// refused with the scenario error before it enrols anything.
    #[test]
    fn enrolment_without_a_control_listener_uses_the_machine_clock() {
        use std::io::{Read, Write};
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ";
        let server = std::thread::spawn(move || {
            let mut seen: Vec<(String, Vec<u8>)> = Vec::new();
            // factors (owed), enroll, confirm.
            for _ in 0..3 {
                let (mut stream, _) = listener.accept().unwrap();
                let mut buf = Vec::new();
                let mut chunk = [0u8; 1024];
                let (head_end, length) = loop {
                    let n = stream.read(&mut chunk).unwrap();
                    buf.extend_from_slice(&chunk[..n]);
                    if let Some(i) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
                        let head = String::from_utf8_lossy(&buf[..i]).to_lowercase();
                        let length = head
                            .lines()
                            .find_map(|l| l.strip_prefix("content-length:"))
                            .and_then(|v| v.trim().parse::<usize>().ok())
                            .unwrap_or(0);
                        break (i + 4, length);
                    }
                };
                while buf.len() < head_end + length {
                    let n = stream.read(&mut chunk).unwrap();
                    buf.extend_from_slice(&chunk[..n]);
                }
                let line = String::from_utf8_lossy(&buf)
                    .lines()
                    .next()
                    .unwrap()
                    .to_owned();
                let body = match seen.len() {
                    0 => cbor(|e| {
                        e.array(5).uint(1).null().array(0).uint(0).uint(0);
                    }),
                    1 => cbor(|e| {
                        e.array(2).text(secret).text("otpauth://totp/x");
                    }),
                    _ => cbor(|e| {
                        e.array(1).array(0);
                    }),
                };
                seen.push((line, buf[head_end..].to_vec()));
                write!(
                    stream,
                    "HTTP/1.1 200 OK\r\nContent-Type: application/cbor\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                )
                .unwrap();
                stream.write_all(&body).unwrap();
            }
            seen
        });

        let base = format!("http://{addr}");
        let state = enrol_second_factor_on(&base, "tok", true, false)
            .unwrap()
            .expect("an owed account enrols");
        let seen = server.join().unwrap();
        let lines: Vec<&str> = seen.iter().map(|(l, _)| l.as_str()).collect();
        assert_eq!(
            lines,
            [
                "GET /v1/auth/factors HTTP/1.1",
                "POST /v1/auth/totp/enroll HTTP/1.1",
                "POST /v1/auth/totp/confirm HTTP/1.1"
            ],
            "no /debug call"
        );
        // The confirm carries the code for the machine's counter (allow the step to roll over mid-test).
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs()
            / totp::PERIOD;
        assert!(
            (now - 1..=now).contains(&state.last_counter),
            "counter {} vs {now}",
            state.last_counter
        );
        let want = totp::code(secret, state.last_counter).unwrap();
        assert!(
            seen[2].1.windows(6).any(|w| w == want.as_bytes()),
            "the confirm body carries {want}"
        );
    }

    #[test]
    fn enrolment_without_control_or_permission_is_a_scenario_error() {
        use std::io::{Read, Write};
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let server = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut chunk = [0u8; 1024];
            let _ = stream.read(&mut chunk).unwrap();
            let body = cbor(|e| {
                e.array(5).uint(1).null().array(0).uint(0).uint(0);
            });
            write!(
                stream,
                "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                body.len()
            )
            .unwrap();
            stream.write_all(&body).unwrap();
        });
        let err = enrol_second_factor_on(&format!("http://{addr}"), "tok", false, false)
            .unwrap_err()
            .to_string();
        server.join().unwrap();
        assert!(
            err.contains(
                "the instance requires a second factor and DILLA_TESTKIT_CONTROL is unset"
            ),
            "{err}"
        );
    }

    const REGISTERED: [u8; 16] = [0x01; 16];
    const WELCOMED: [u8; 16] = [0x02; 16];
    const RESYNCED: [u8; 16] = [0x03; 16];
    /// Welcomed, then removed: the instance answers a non-member E_NOT_FOUND (sequencer.go).
    const LEFT: [u8; 16] = [0x04; 16];

    /// The instance side of `a_group_learned_without_a_frame_is_caught_up_on_reconnect`.
    fn instance(method: &str, path: &str) -> (u16, Vec<u8>) {
        let reads = |g: &[u8; 16]| {
            let at = format!("/v1/groups/{}/", hex::encode(g));
            path.starts_with(&format!("{at}handshakes?"))
                || path.starts_with(&format!("{at}messages?"))
        };
        let resync = format!("/v1/groups/{}/resync", hex::encode(RESYNCED));
        match (method, path) {
            ("POST", "/v1/groups") => (
                201,
                encode(|e| {
                    e.array(2).bytes(&REGISTERED).uint(1);
                }),
            ),
            ("GET", "/v1/welcomes?after=0&limit=64") => (
                200,
                encode(|e| {
                    e.array(2);
                    e.array(7)
                        .uint(1)
                        .bytes(&WELCOMED)
                        .uint(2)
                        .uint(3)
                        .bytes(&[5])
                        .bytes(&[6])
                        .bytes(&[0xcc; 32]);
                    e.array(7)
                        .uint(2)
                        .bytes(&LEFT)
                        .uint(4)
                        .uint(9)
                        .bytes(&[5])
                        .bytes(&[6])
                        .bytes(&[0xcc; 32]);
                }),
            ),
            ("POST", p) if p == resync => (
                200,
                encode(|e| {
                    e.array(2).uint(7).uint(3);
                }),
            ),
            ("GET", _) if reads(&LEFT) => (
                404,
                encode(|e| {
                    e.array(3).text("E_NOT_FOUND").text("group").null();
                }),
            ),
            ("GET", _) if reads(&REGISTERED) || reads(&WELCOMED) || reads(&RESYNCED) => (
                200,
                encode(|e| {
                    e.array(0);
                }),
            ),
            _ => (500, Vec::new()),
        }
    }

    type Seen = std::sync::Arc<std::sync::Mutex<Vec<String>>>;

    /// A throwaway HTTP/1.1 origin, one request per connection, answering from `route` and
    /// recording every `METHOD path` it served.
    fn origin(route: fn(&str, &str) -> (u16, Vec<u8>)) -> (String, Seen) {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let base = format!("http://{}", listener.local_addr().unwrap());
        let seen: Seen = Default::default();
        let log = std::sync::Arc::clone(&seen);
        std::thread::spawn(move || {
            for stream in listener.incoming() {
                let Ok(stream) = stream else { return };
                answer(stream, route, &log);
            }
        });
        (base, seen)
    }

    /// Answers one HTTP/1.1 request on `stream` from `route`, recording `METHOD path` in `log`.
    fn answer(mut stream: TcpStream, route: fn(&str, &str) -> (u16, Vec<u8>), log: &Seen) {
        use std::io::{BufRead, BufReader, Read, Write};
        let mut reader = BufReader::new(stream.try_clone().unwrap());
        let mut line = String::new();
        reader.read_line(&mut line).unwrap();
        let mut length = 0usize;
        loop {
            let mut header = String::new();
            reader.read_line(&mut header).unwrap();
            if header.trim().is_empty() {
                break;
            }
            if let Some((name, value)) = header.split_once(':')
                && name.eq_ignore_ascii_case("content-length")
            {
                length = value.trim().parse().unwrap();
            }
        }
        let mut body = vec![0u8; length];
        reader.read_exact(&mut body).unwrap();
        let mut parts = line.split_whitespace();
        let method = parts.next().unwrap_or_default().to_owned();
        let path = parts.next().unwrap_or_default().to_owned();
        log.lock().unwrap().push(format!("{method} {path}"));
        let (status, out) = route(&method, &path);
        let head = format!(
            "HTTP/1.1 {status} X\r\nContent-Type: application/cbor\r\n\
             Content-Length: {}\r\nConnection: close\r\n\r\n",
            out.len()
        );
        stream.write_all(head.as_bytes()).unwrap();
        stream.write_all(&out).unwrap();
    }

    /// A device learns of a group from `register_group`, a Welcome row or its own external commit
    /// without a single frame for it having been pumped. The reconnect's catch-up must still walk
    /// that group from where the device came in — otherwise `go_offline bob; send alice …;
    /// go_online bob` delivers nothing to a Welcome joiner, where the stub delivers everything.
    #[test]
    fn a_group_learned_without_a_frame_is_caught_up_on_reconnect() {
        use dilla_core::ids::InstanceId;
        use dilla_core::mls::{DillaBinding, GroupKind};

        let (base, seen) = origin(instance);
        let mut ds = connected(&base, None);
        let binding = DillaBinding {
            v: 1,
            instance_id: InstanceId::from_bytes([0x11; 16]),
            community_id: None,
            target_id: REGISTERED,
            kind: GroupKind::Text,
            policy_version: 1,
            e2ee_version: 1,
            media_version: 0,
        };
        ds.register_group(RegisterRequest {
            binding: binding.encode(),
            group_info: vec![1],
            ratchet_tree: vec![2],
        })
        .unwrap();
        assert_eq!(ds.welcomes().unwrap().len(), 2);
        ds.post_external_commit(
            &RESYNCED,
            ResyncRequest {
                external_commit: vec![3],
                group_info: vec![4],
            },
        )
        .unwrap();
        seen.lock().unwrap().clear();

        ds.catch_up().unwrap();

        let served = seen.lock().unwrap().clone();
        for (group, from) in [(REGISTERED, 1), (WELCOMED, 3), (RESYNCED, 7)] {
            let g = hex::encode(group);
            for want in [
                format!("GET /v1/groups/{g}/handshakes?from={from}&limit={HANDSHAKE_PAGE}"),
                format!("GET /v1/groups/{g}/messages?from={from}&limit={MESSAGE_PAGE}"),
            ] {
                assert!(served.contains(&want), "{want} not in {served:#?}");
            }
        }
        // A group the device can no longer read is dropped, not a failed reconnect.
        let left = format!(
            "GET /v1/groups/{}/handshakes?from=9&limit={HANDSHAKE_PAGE}",
            hex::encode(LEFT)
        );
        assert!(served.contains(&left), "{left} not in {served:#?}");
        assert!(!ds.delivered.contains_key(LEFT.as_slice()));

        // Learning of a group again never rewinds past what was already delivered.
        ds.accept(Frame::MessageCt {
            group_id: WELCOMED.to_vec(),
            item: MessageItem {
                seq: 8,
                epoch: 2,
                uploader_device: DeviceId::from_bytes([0xaa; 16]),
                blob: vec![1],
                commitment: [0; 32],
                franking_tag: [0; 32],
                recv_ts: 0,
                deleted: false,
                class: None,
            },
        });
        ds.welcomes().unwrap();
        assert_eq!(ds.delivered.get(WELCOMED.as_slice()), Some(&8));
    }

    fn heartbeat_ack() -> Vec<u8> {
        encode(|e| {
            e.array(4).uint(7).uint(0).null();
            e.array(1).uint(1_758_659_640);
        })
    }

    /// Answers every binary frame on `ws` with a `heartbeat_ack` until the client lets go.
    fn ack_every_frame(mut ws: WebSocket<TcpStream>) {
        while let Ok(message) = ws.read() {
            if matches!(message, Message::Binary(_))
                && ws.send(Message::Binary(heartbeat_ack().into())).is_err()
            {
                return;
            }
        }
    }

    /// Pumps until no heartbeat is outstanding, or fails after a few seconds.
    fn pump_until_acked(ds: &mut HttpDs) {
        let deadline = Instant::now() + Duration::from_secs(5);
        while ds.unacked > 0 {
            assert!(Instant::now() < deadline, "the heartbeat was never acked");
            ds.pump().unwrap();
        }
    }

    fn a_minute_ago() -> Instant {
        Instant::now()
            .checked_sub(Duration::from_secs(60))
            .expect("a monotonic clock older than a minute")
    }

    /// A heartbeat is outstanding from the moment it is sent until its `heartbeat_ack` is read,
    /// and an answered heartbeat leaves the connection alone.
    #[test]
    fn an_acknowledged_heartbeat_keeps_the_connection() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let gateway = std::thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            ack_every_frame(tungstenite::accept(stream).unwrap());
        });
        let stream = TcpStream::connect(addr).unwrap();
        let (mut socket, _) = tungstenite::client(format!("ws://{addr}/gateway"), stream).unwrap();
        read_timeout(&mut socket, PUMP_WAIT).unwrap();
        let mut ds = connected(&format!("http://{addr}"), Some(socket));

        ds.last_beat = a_minute_ago();
        ds.pump().unwrap();
        assert!(ds.unacked <= 1, "one heartbeat sent");
        pump_until_acked(&mut ds);
        assert!(ds.ws.is_some(), "an answered heartbeat keeps the socket");
        assert!(ds.lost.is_empty(), "nothing lost: {:?}", ds.lost);
        assert_eq!(ds.missed_acks, 0);
        assert_eq!(ds.ack_timeout(), Duration::from_secs(15));

        ds.set_online(false).unwrap();
        gateway.join().unwrap();
    }

    const STALLED: [u8; 16] = [0x05; 16];

    /// The HTTP side of `an_unanswered_heartbeat_reopens_and_catches_up`: `STALLED` holds one
    /// message the stalled connection never carried, seq 5.
    fn stalled_instance(method: &str, path: &str) -> (u16, Vec<u8>) {
        let at = format!("/v1/groups/{}/", hex::encode(STALLED));
        match method {
            "GET" if path.starts_with(&format!("{at}handshakes?from=5&")) => (
                200,
                encode(|e| {
                    e.array(0);
                }),
            ),
            "GET" if path.starts_with(&format!("{at}messages?from=5&")) => (
                200,
                encode(|e| {
                    e.array(1);
                    e.array(8)
                        .uint(5)
                        .uint(2)
                        .bytes(&[0xaa; 16])
                        .bytes(&[4])
                        .null()
                        .bytes(&[0xbb; 32])
                        .uint(1_758_659_640)
                        .uint(0);
                }),
            ),
            _ => (500, Vec::new()),
        }
    }

    /// A connection that stays open but stops delivering — the instance wrote every frame, the
    /// client reads nothing and no `heartbeat_ack` comes back — is dropped once the ack is overdue,
    /// and the next `drain` reopens it with a fresh identify and catches up over rows 4 and 12.
    #[test]
    #[allow(
        clippy::result_large_err,
        reason = "tungstenite's accept_hdr callback fixes its error type as an HTTP response"
    )]
    fn an_unanswered_heartbeat_reopens_and_catches_up() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let seen: Seen = Default::default();
        let log = std::sync::Arc::clone(&seen);
        std::thread::spawn(move || {
            let mut upgrades = 0;
            for stream in listener.incoming() {
                let Ok(stream) = stream else { return };
                let mut head = [0u8; 12];
                while stream.peek(&mut head).unwrap() < head.len() {}
                if &head != b"GET /gateway" {
                    answer(stream, stalled_instance, &log);
                    continue;
                }
                upgrades += 1;
                // `open_socket` asks for the subprotocol, and tungstenite holds the answer to it.
                let echo =
                    |request: &tungstenite::handshake::server::Request,
                     mut response: tungstenite::handshake::server::Response| {
                        if let Some(asked) = request.headers().get("Sec-WebSocket-Protocol") {
                            response
                                .headers_mut()
                                .insert("Sec-WebSocket-Protocol", asked.clone());
                        }
                        Ok(response)
                    };
                let mut ws = tungstenite::accept_hdr(stream, echo).unwrap();
                if upgrades == 1 {
                    // The stalled connection: open, reading, answering nothing.
                    std::thread::spawn(move || while ws.read().is_ok() {});
                    continue;
                }
                // The instance offers the current wire version alone.
                let hello = encode(|e| {
                    e.array(4).uint(0).uint(0).null();
                    e.array(9)
                        .array(1)
                        .uint(WIRE_VERSION)
                        .array(1)
                        .uint(1)
                        .array(1)
                        .uint(1)
                        .uint(30_000)
                        .uint(131_584)
                        .bytes(&[0x11; 16])
                        .uint(1)
                        .uint(2_000)
                        .uint(500);
                });
                let ready = encode(|e| {
                    e.array(4).uint(3).uint(0).null();
                    e.array(0);
                });
                ws.send(Message::Binary(hello.into())).unwrap();
                ws.read().unwrap(); // identify
                ws.send(Message::Binary(ready.into())).unwrap();
                std::thread::spawn(move || ack_every_frame(ws));
            }
        });

        let stream = TcpStream::connect(addr).unwrap();
        let (mut socket, _) = tungstenite::client(format!("ws://{addr}/gateway"), stream).unwrap();
        read_timeout(&mut socket, PUMP_WAIT).unwrap();
        let mut ds = connected(&format!("http://{addr}"), Some(socket));
        ds.delivered.insert(STALLED.to_vec(), 4);

        ds.last_beat = a_minute_ago();
        ds.pump().unwrap();
        assert_eq!(ds.unacked, 1, "the heartbeat is outstanding");
        assert!(ds.ws.is_some(), "not yet overdue");

        // The ack has now waited past the timeout with nothing arriving.
        ds.ack_since = a_minute_ago();
        let frames = ds.drain().unwrap();

        assert_eq!(ds.missed_acks, 1);
        assert!(
            ds.lost
                .first()
                .is_some_and(|why| why.starts_with("no heartbeat_ack within 15s")),
            "{:?}",
            ds.lost
        );
        assert_eq!(ds.reconnects, 1, "the drain reopened the connection");
        assert!(ds.ws.is_some());
        assert_eq!(ds.unacked, 0, "a fresh connection owes nothing");
        assert_eq!(
            frames.iter().filter_map(seq_of).collect::<Vec<_>>(),
            vec![5],
            "the catch-up brings in what the stalled connection held back"
        );
        let served = seen.lock().unwrap().clone();
        let g = hex::encode(STALLED);
        assert!(
            served.contains(&format!(
                "GET /v1/groups/{g}/handshakes?from=5&limit={HANDSHAKE_PAGE}"
            )),
            "{served:#?}"
        );

        // The reopened connection answers its heartbeats.
        ds.last_beat = a_minute_ago();
        ds.pump().unwrap();
        pump_until_acked(&mut ds);
        assert!(ds.ws.is_some());
        assert_eq!(ds.missed_acks, 1);
    }

    /// A pump after a pause longer than the ack timeout starts the wait afresh: the ack may be
    /// queued behind a backlog the client simply has not read yet.
    #[test]
    fn a_pause_in_pumping_is_not_a_missed_ack() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let gateway = std::thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            let mut ws = tungstenite::accept(stream).unwrap();
            while ws.read().is_ok() {}
        });
        let stream = TcpStream::connect(addr).unwrap();
        let (mut socket, _) = tungstenite::client(format!("ws://{addr}/gateway"), stream).unwrap();
        read_timeout(&mut socket, PUMP_WAIT).unwrap();
        let mut ds = connected(&format!("http://{addr}"), Some(socket));

        ds.last_beat = a_minute_ago();
        ds.pump().unwrap();
        assert_eq!(ds.unacked, 1);
        // The heartbeat went out a minute ago, and this client has not pumped since.
        ds.ack_since = a_minute_ago();
        ds.last_pump = a_minute_ago();
        ds.pump().unwrap();
        assert!(ds.ws.is_some(), "lost {:?}", ds.lost);
        assert_eq!(ds.missed_acks, 0);

        ds.set_online(false).unwrap();
        gateway.join().unwrap();
    }

    /// L-HTTP-54: with a login the establish body carries the registration array in element 3 and the
    /// assertion's UTF-8 bytes as a bstr in element 4; without one it is web-1's body.
    #[test]
    fn an_enrolment_establish_carries_the_registration_array_and_the_login_as_bytes() {
        let reg = Registration {
            device_id: DeviceId::from_bytes([0x44; 16]),
            dsk_pub: [0x55; 32],
            tier: 1,
            signer_tier: 1,
            credential: vec![0x66; 10],
        };
        let body = session_body(
            &[0x11; 32],
            &[0x22; 64],
            &Element3::Registration(&reg),
            Some("asrt-1"),
        );
        let want = cbor(|e| {
            e.array(5).bytes(&[0x11; 32]).uint(0).bytes(&[0x22; 64]);
            e.array(5)
                .bytes(&[0x44; 16])
                .bytes(&[0x55; 32])
                .uint(1)
                .uint(1)
                .bytes(&[0x66; 10]);
            e.bytes(b"asrt-1");
        });
        assert_eq!(body, want);
        let plain = session_body(
            &[0x11; 32],
            &[0x22; 64],
            &Element3::Credential(&[0x77; 3]),
            None,
        );
        assert_eq!(
            plain,
            cbor(|e| {
                e.array(5)
                    .bytes(&[0x11; 32])
                    .uint(0)
                    .bytes(&[0x22; 64])
                    .bytes(&[0x77; 3])
                    .null();
            })
        );
    }

    #[test]
    fn the_web_2a_answers_decode_as_the_instance_writes_them() {
        let est = cbor(|e| {
            e.array(7)
                .text("tok")
                .uint(1)
                .bytes(&[0x01; 16])
                .bytes(&[0x02; 16])
                .uint(100)
                .uint(50)
                .uint(3);
        });
        assert_eq!(
            decode_established(&est).unwrap(),
            Established {
                token: "tok".into(),
                scope: 1,
                user_id: [0x01; 16],
                device_id: DeviceId::from_bytes([0x02; 16]),
                expires: 100
            }
        );
        let login = cbor(|e| {
            e.array(2).text("asrt").uint(1);
        });
        assert_eq!(
            decode_password_login(&login).unwrap(),
            PasswordLogin {
                assertion: "asrt".into(),
                needs: 1
            }
        );
        // `needs` is 0, 1 or 2 (protocol/09); anything else is a protocol error.
        for (wire, want) in [(0, Some(0)), (1, Some(1)), (2, Some(2)), (3, None)] {
            let login = cbor(|e| {
                e.array(2).text("asrt").uint(wire);
            });
            assert_eq!(
                decode_password_login(&login).ok().map(|l| l.needs),
                want,
                "needs {wire}"
            );
        }
        let served = cbor(|e| {
            e.array(4)
                .uint(2)
                .bytes(&[0x03; 5])
                .bytes(&[0x04; 64])
                .bytes(&[0x05; 32]);
        });
        let row = ServedDeviceList {
            version: 2,
            blob: vec![0x03; 5],
            ssk_signature: vec![0x04; 64],
            prev_hash: vec![0x05; 32],
        };
        assert_eq!(decode_strict(&served, decode_served_list).unwrap(), row);
        let history = cbor(|e| {
            e.array(2);
            e.array(4)
                .uint(2)
                .bytes(&[0x03; 5])
                .bytes(&[0x04; 64])
                .bytes(&[0x05; 32]);
            e.array(4)
                .uint(3)
                .bytes(&[0x06; 5])
                .bytes(&[0x04; 64])
                .bytes(&[0x07; 32]);
        });
        let got = decode_history(&history).unwrap();
        assert_eq!(got.len(), 2);
        assert_eq!(got[0], row);
        assert_eq!(
            (got[1].version, got[1].prev_hash.clone()),
            (3, vec![0x07; 32])
        );
        assert_eq!(
            decode_history(&cbor(|e| {
                e.array(0);
            }))
            .unwrap(),
            Vec::new()
        );
        let backups = cbor(|e| {
            e.array(1);
            e.array(4).uint(0).uint(0).uint(103).uint(1_800_000_000);
        });
        assert_eq!(
            decode_backups(&backups).unwrap(),
            vec![BackupItem {
                kind: 0,
                chunk_seq: 0,
                size: 103,
                created: 1_800_000_000
            }]
        );
        let dm = cbor(|e| {
            e.array(12)
                .bytes(&[0x06; 16])
                .null()
                .uint(3)
                .uint(0)
                .uint(0)
                .null()
                .text("")
                .text("")
                .uint(0)
                .uint(0)
                .uint(0)
                .bytes(&[0x07; 16]);
        });
        assert_eq!(
            decode_channel(&dm).unwrap(),
            ChannelInfo {
                id: [0x06; 16],
                community_id: None,
                kind: 3,
                text_group_id: Some([0x07; 16])
            }
        );
        let fresh = cbor(|e| {
            e.array(12)
                .bytes(&[0x06; 16])
                .null()
                .uint(3)
                .uint(0)
                .uint(0)
                .null()
                .text("")
                .text("")
                .uint(0)
                .uint(0)
                .uint(0)
                .null();
        });
        assert_eq!(decode_channel(&fresh).unwrap().text_group_id, None);
        let dms = cbor(|e| {
            e.array(2);
            e.array(3).bytes(&[0x06; 16]).uint(3);
            e.array(2).bytes(&[0x01; 16]).bytes(&[0x02; 16]);
            e.array(3).bytes(&[0x08; 16]).uint(4).null();
        });
        assert_eq!(
            decode_dms(&dms).unwrap(),
            vec![
                DmInfo {
                    channel_id: [0x06; 16],
                    kind: 3,
                    members: vec![[0x01; 16], [0x02; 16]]
                },
                // A nil Go slice arrives as null and is an empty member list.
                DmInfo {
                    channel_id: [0x08; 16],
                    kind: 4,
                    members: Vec::new()
                },
            ]
        );
    }

    /// web-3 L-GW-33: op 53 `[call_id, channel_id, user_id, state, expires_ts]` decodes into a ring; any other
    /// length is refused, as every other gateway frame of the wrong shape is.
    #[test]
    fn the_call_ring_frame_decodes_and_a_misshapen_one_is_refused() {
        let ring = cbor(|e| {
            e.array(4).uint(53).uint(0).null();
            e.array(5)
                .bytes(&[0x01; 16])
                .bytes(&[0x02; 16])
                .bytes(&[0x03; 16])
                .uint(1)
                .uint(1_800_000_030);
        });
        match decode_frame(&ring).unwrap() {
            (0, Inbound::Ring(r)) => assert_eq!(
                r,
                CallRing {
                    call: [0x01; 16],
                    channel: [0x02; 16],
                    user: [0x03; 16],
                    state: 1,
                    expires: 1_800_000_030,
                }
            ),
            _ => panic!("op 53 is a ring"),
        }
        let short = cbor(|e| {
            e.array(4).uint(53).uint(0).null();
            e.array(4)
                .bytes(&[0x01; 16])
                .bytes(&[0x02; 16])
                .bytes(&[0x03; 16])
                .uint(1);
        });
        assert!(decode_frame(&short).is_err());
    }

    /// web-3.1: op 54 `[community_id]` is read and ignored; a second element or a short id is refused, as every
    /// other gateway frame of the wrong shape is.
    #[test]
    fn the_community_changed_frame_is_ignored_and_a_misshapen_one_is_refused() {
        let changed = cbor(|e| {
            e.array(4).uint(54).uint(0).null();
            e.array(1).bytes(&[0x0e; 16]);
        });
        assert!(matches!(
            decode_frame(&changed).unwrap(),
            (0, Inbound::Ignored)
        ));
        let long = cbor(|e| {
            e.array(4).uint(54).uint(0).null();
            e.array(2).bytes(&[0x0e; 16]).uint(1);
        });
        assert!(decode_frame(&long).is_err());
        let short_id = cbor(|e| {
            e.array(4).uint(54).uint(0).null();
            e.array(1).bytes(&[0x0e; 15]);
        });
        assert!(decode_frame(&short_id).is_err());
    }

    /// web-3 L-HTTP-100 and the community of `GET /v1/channels/{id}`, as the instance writes them: a nil Go slice
    /// of participants arrives as null and is an empty list.
    #[test]
    fn the_web_3_call_answers_decode_as_the_instance_writes_them() {
        let live = cbor(|e| {
            e.array(3).bytes(&[0x04; 16]).bytes(&[0x05; 16]);
            e.array(2);
            e.array(3).bytes(&[0x01; 16]).bytes(&[0x02; 16]).uint(3);
            e.array(3).bytes(&[0x01; 16]).bytes(&[0x09; 16]).uint(0);
        });
        assert_eq!(
            decode_channel_call(&live).unwrap(),
            ChannelCall {
                group_id: Some([0x04; 16]),
                call_id: Some([0x05; 16]),
                participants: vec![([0x01; 16], [0x02; 16], 3), ([0x01; 16], [0x09; 16], 0)],
            }
        );
        let idle = cbor(|e| {
            e.array(3).null().null().null();
        });
        assert_eq!(
            decode_channel_call(&idle).unwrap(),
            ChannelCall {
                group_id: None,
                call_id: None,
                participants: Vec::new(),
            }
        );
        let voice = cbor(|e| {
            e.array(12)
                .bytes(&[0x06; 16])
                .bytes(&[0x08; 16])
                .uint(1)
                .uint(0)
                .uint(0)
                .null()
                .text("voice")
                .text("")
                .uint(0)
                .uint(0)
                .uint(0)
                .null();
        });
        assert_eq!(
            decode_channel(&voice).unwrap(),
            ChannelInfo {
                id: [0x06; 16],
                community_id: Some([0x08; 16]),
                kind: 1,
                text_group_id: None
            }
        );
    }

    /// A ring a pump reads is kept beside the delivery-service frames, so a `drain` never loses it, and
    /// `take_rings` reads up to a heartbeat barrier first: a ring queued before the barrier is in its answer.
    #[test]
    fn rings_survive_a_drain_and_take_rings_reads_up_to_a_barrier() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let ring = |state: u64| {
            encode(|e| {
                e.array(4).uint(53).uint(0).null();
                e.array(5)
                    .bytes(&[0x01; 16])
                    .bytes(&[0x02; 16])
                    .bytes(&[0x03; 16])
                    .uint(state)
                    .uint(1_800_000_030);
            })
        };
        let (sent, written) = std::sync::mpsc::channel();
        let gateway = std::thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            let mut ws = tungstenite::accept(stream).unwrap();
            ws.send(Message::Binary(ring(1).into())).unwrap();
            sent.send(()).unwrap();
            // The first heartbeat: the second ring is queued before its ack, as the gateway's one writer would.
            let mut beats = 0;
            loop {
                match ws.read() {
                    Ok(Message::Binary(bytes))
                        if bytes.first() == Some(&0x84) && bytes.get(1) == Some(&6) =>
                    {
                        beats += 1;
                        if beats == 1 {
                            ws.send(Message::Binary(ring(0).into())).unwrap();
                        }
                        ws.send(Message::Binary(heartbeat_ack().into())).unwrap();
                    }
                    Ok(_) => {}
                    Err(_) => break,
                }
            }
        });
        let stream = TcpStream::connect(addr).unwrap();
        let (mut socket, _) = tungstenite::client(format!("ws://{addr}/gateway"), stream).unwrap();
        read_timeout(&mut socket, PUMP_WAIT).unwrap();
        written.recv().unwrap();
        let mut ds = connected(&format!("http://{addr}"), Some(socket));
        // A drain reads the first ring off the socket and returns no delivery-service frame for it.
        let deadline = Instant::now() + Duration::from_secs(5);
        while ds.rings.is_empty() && Instant::now() < deadline {
            assert!(ds.drain().unwrap().is_empty());
        }
        let rings = ds.take_rings().unwrap();
        assert_eq!(
            rings.iter().map(|r| r.state).collect::<Vec<_>>(),
            vec![1, 0],
            "the drained ring, then the one queued before the barrier's ack"
        );
        assert!(
            ds.take_rings().unwrap().is_empty(),
            "take_rings hands each ring out once"
        );
        drop(ds);
        gateway.join().unwrap();
    }
}

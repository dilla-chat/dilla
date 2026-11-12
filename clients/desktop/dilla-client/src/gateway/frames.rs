//! Gateway frames (`protocol/02-delivery-service.md` § Gateway frames): the deterministic-CBOR array
//! `[op, n, group_id, payload]`. A port of `packages/client-core/src/gateway/frames.ts`: the same shape checks on
//! decode, the same refusals on encode, built on dilla-core's deterministic CBOR so every encoding is canonical.

use dilla_core::cbor::{CborError, Decoder, Encoder, MAX_NESTING};

/// The opcodes this client reads or writes (frames.ts `Op`).
pub struct Op;

impl Op {
    pub const HELLO: u8 = 0;
    pub const IDENTIFY: u8 = 1;
    pub const READY: u8 = 3;
    pub const INVALID_SESSION: u8 = 5;
    pub const HEARTBEAT: u8 = 6;
    pub const HEARTBEAT_ACK: u8 = 7;
    pub const RECONNECT: u8 = 8;
    pub const ERROR: u8 = 9;
    pub const COMMIT_ACK: u8 = 12;
    pub const HANDSHAKE: u8 = 16;
    pub const COMMIT_NEEDED: u8 = 17;
    pub const EPOCH_CHANGED: u8 = 18;
    pub const MESSAGE_CT: u8 = 19;
    pub const WELCOME: u8 = 20;
    pub const MESSAGE_DELETED: u8 = 21;
    pub const PRESENCE: u8 = 48;
    pub const TYPING: u8 = 49;
    pub const VOICE_STATE: u8 = 50;
    pub const TYPING_SET: u8 = 51;
    pub const VOICE_SELF: u8 = 52;
    pub const CALL_RING: u8 = 53;
    pub const COMMUNITY_CHANGED: u8 = 54;
    pub const AFK_AWAY: u8 = 55;
    pub const APP_BUILD: u8 = 56;
    pub const PRESENCE_ACTIVITY: u8 = 57;
    pub const CALL_STATS: u8 = 58;
    pub const REQUEST: u8 = 59;
    pub const RESPONSE: u8 = 60;
    pub const BOT_CATALOG: u8 = 61;
    pub const BOT_REPLY: u8 = 62;
    pub const BOT_STATE_CHANGED: u8 = 63;
}

/// One CBOR item of the dilla subset: uint, bstr, tstr, null and arrays (no maps, tags, floats or negatives).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Value {
    Uint(u64),
    Bytes(Vec<u8>),
    Text(String),
    Null,
    Array(Vec<Value>),
}

impl Value {
    /// The deterministic encoding of this item.
    pub fn encode(&self) -> Vec<u8> {
        let mut e = Encoder::new();
        self.write(&mut e);
        e.into_vec()
    }

    fn write(&self, e: &mut Encoder) {
        match self {
            Value::Uint(v) => {
                e.uint(*v);
            }
            Value::Bytes(b) => {
                e.bytes(b);
            }
            Value::Text(t) => {
                e.text(t);
            }
            Value::Null => {
                e.null();
            }
            Value::Array(items) => {
                e.array(items.len());
                for item in items {
                    item.write(e);
                }
            }
        }
    }

    /// Decodes exactly one item, strictly: non-minimal heads, indefinite lengths, maps, tags, floats, negatives, simple
    /// values but null, invalid UTF-8, nesting past 8 and trailing bytes are all refused.
    pub fn decode(bytes: &[u8]) -> Result<Value, CborError> {
        // `skip` validates the whole item (every strictness rule and the nesting bound) before it is materialised.
        let mut whole = Decoder::new(bytes);
        let raw = whole.skip()?;
        whole.finish()?;
        let mut d = Decoder::new(raw);
        let v = Self::read(&mut d, raw)?;
        d.finish()?;
        Ok(v)
    }

    fn read(d: &mut Decoder<'_>, input: &[u8]) -> Result<Value, CborError> {
        let at = d.position();
        let head = *input.get(at).ok_or(CborError::Truncated)?;
        Ok(match head >> 5 {
            0 => Value::Uint(d.uint()?),
            2 => Value::Bytes(d.bytes()?.to_vec()),
            3 => Value::Text(d.text()?.to_owned()),
            4 => {
                let n = d.array_len()?;
                let mut items = Vec::with_capacity(n);
                for _ in 0..n {
                    items.push(Self::read(d, input)?);
                }
                Value::Array(items)
            }
            7 => {
                d.null()?;
                Value::Null
            }
            _ => {
                return Err(CborError::TypeMismatch {
                    expected: "item",
                    offset: at,
                });
            }
        })
    }

    pub fn as_uint(&self) -> Option<u64> {
        match self {
            Value::Uint(v) => Some(*v),
            _ => None,
        }
    }

    pub fn as_bytes(&self) -> Option<&[u8]> {
        match self {
            Value::Bytes(b) => Some(b),
            _ => None,
        }
    }

    pub fn as_text(&self) -> Option<&str> {
        match self {
            Value::Text(t) => Some(t),
            _ => None,
        }
    }

    pub fn as_array(&self) -> Option<&[Value]> {
        match self {
            Value::Array(a) => Some(a),
            _ => None,
        }
    }

    fn depth_fits(&self, depth: usize) -> bool {
        depth <= MAX_NESTING
            && match self {
                Value::Array(items) => items.iter().all(|v| v.depth_fits(depth + 1)),
                _ => true,
            }
    }
}

/// One gateway frame. `n` is the server's replay sequence (0: not replayable) or the client's correlation id.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Frame {
    pub op: u8,
    pub n: u64,
    pub group_id: Option<[u8; 16]>,
    pub payload: Vec<Value>,
}

#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum FrameError {
    #[error("undecodable frame: {0}")]
    Cbor(#[from] CborError),
    #[error("E_FRAME_SHAPE")]
    Shape,
    #[error("E_FRAME_LIMIT")]
    Limit,
}

impl FrameError {
    /// The code the TS client names the same refusal with (`CborError.code`, or the frame error's message).
    pub fn code(&self) -> &'static str {
        match self {
            FrameError::Shape => "E_FRAME_SHAPE",
            FrameError::Limit => "E_FRAME_LIMIT",
            FrameError::Cbor(e) => match e {
                CborError::NonMinimalInt => "E_CBOR_NONCANONICAL",
                CborError::Truncated => "E_CBOR_TRUNCATED",
                CborError::TrailingBytes => "E_CBOR_TRAILING",
                CborError::InvalidUtf8 => "E_CBOR_UTF8",
                CborError::TooDeep(_) => "E_CBOR_DEPTH",
                CborError::IntegerOverflow => "E_CBOR_RANGE",
                _ => "E_CBOR_TYPE",
            },
        }
    }
}

/// `Number.MAX_SAFE_INTEGER`: frames.ts refuses to encode an `n` past it.
pub const MAX_SAFE_INTEGER: u64 = (1 << 53) - 1;

/// The payload arity of every opcode frames.ts knows; `grouped` ops (12, 16..=21) carry a group id, no other does.
fn spec(op: u8) -> Option<usize> {
    Some(match op {
        0 => 9,
        1 => 5,
        2 => 4,
        3 => 10,
        4 => 3,
        5 => 2,
        6 => 2,
        7 => 1,
        8 => 2,
        9 => 3,
        10..=12 => 1,
        16 => 5,
        17 => 4,
        18 => 2,
        19 => 7,
        20 => 6,
        21 => 2,
        32 => 7,
        33 => 4,
        48 => 4,
        49 => 5,
        50 => 4,
        51 | 52 => 2,
        53 => 5,
        54 => 1,
        55 => 3,
        56 => 1,
        57 => 0,
        58 => 2,
        59 => 5,
        60 => 3,
        61 => 4,
        62 => 4,
        63 => 3,
        _ => return None,
    })
}

fn grouped(op: u8) -> bool {
    op == 12 || (16..=21).contains(&op)
}

/// frames.ts `encodeFrame`: an `n` past the safe integers or a payload nested past 8 is refused.
pub fn encode(frame: &Frame) -> Result<Vec<u8>, FrameError> {
    if frame.n > MAX_SAFE_INTEGER {
        return Err(FrameError::Shape);
    }
    if !frame.payload.iter().all(|v| v.depth_fits(2)) {
        return Err(FrameError::Cbor(CborError::TooDeep(MAX_NESTING)));
    }
    let mut e = Encoder::new();
    e.array(4).uint(u64::from(frame.op)).uint(frame.n);
    match &frame.group_id {
        Some(g) => e.bytes(g),
        None => e.null(),
    };
    e.array(frame.payload.len());
    for v in &frame.payload {
        v.write(&mut e);
    }
    Ok(e.into_vec())
}

/// frames.ts `decodeFrame`: the envelope, the arity and grouping of every known op, and the per-op checks of the ops
/// this client acts on. An unknown op is accepted with any payload.
pub fn decode(bytes: &[u8]) -> Result<Frame, FrameError> {
    arity_precheck(bytes)?;
    let Value::Array(mut parts) = Value::decode(bytes)? else {
        return Err(FrameError::Shape);
    };
    if parts.len() != 4 {
        return Err(FrameError::Shape);
    }
    let payload = match parts.pop() {
        Some(Value::Array(p)) => p,
        _ => return Err(FrameError::Shape),
    };
    let group_id = match parts.pop() {
        Some(Value::Null) => None,
        Some(Value::Bytes(b)) => {
            Some(<[u8; 16]>::try_from(b.as_slice()).map_err(|_| FrameError::Shape)?)
        }
        _ => return Err(FrameError::Shape),
    };
    let n = parts
        .pop()
        .and_then(|v| v.as_uint())
        .ok_or(FrameError::Shape)?;
    let op = parts
        .pop()
        .and_then(|v| v.as_uint())
        .and_then(|op| u8::try_from(op).ok())
        .ok_or(FrameError::Shape)?;
    let frame = Frame {
        op,
        n,
        group_id,
        payload,
    };
    check(&frame)?;
    Ok(frame)
}

fn check(f: &Frame) -> Result<(), FrameError> {
    let p = &f.payload;
    let at = |i: usize| p.get(i);
    let ok = |b: bool| if b { Ok(()) } else { Err(FrameError::Shape) };
    // op 19 message.ct: seven elements, the 7th the upload class (0, 1 or null); six from a web3.6 host.
    if f.op == Op::MESSAGE_CT {
        ok(p.len() == 6 || (p.len() == 7 && class_or_null(at(6))))?;
    }
    if let Some(elements) = spec(f.op) {
        ok((f.op == Op::MESSAGE_CT || p.len() == elements)
            && grouped(f.op) == f.group_id.is_some())?;
    }
    let n0 = f.n == 0;
    match f.op {
        Op::TYPING => ok(is_id(at(0)) && is_id(at(1)) && is_id(at(2)) && bit(at(3)) && uint(at(4))),
        Op::VOICE_STATE => ok(is_id(at(0)) && is_id(at(1)) && is_id(at(2)) && max(at(3), 31)),
        Op::TYPING_SET => ok(n0 && is_id(at(0)) && bit(at(1))),
        Op::VOICE_SELF => ok(n0 && is_id(at(0)) && max(at(1), 3)),
        Op::CALL_RING => {
            ok(n0 && is_id(at(0)) && is_id(at(1)) && is_id(at(2)) && max(at(3), 2) && uint(at(4)))
        }
        Op::COMMUNITY_CHANGED => ok(n0 && is_id(at(0))),
        Op::AFK_AWAY => ok(n0 && is_id(at(0)) && is_id(at(1)) && uint(at(2))),
        Op::APP_BUILD => ok(n0 && at(0).and_then(Value::as_text).is_some_and(valid_app_entry)),
        Op::REQUEST => ok(valid_request_payload(p)),
        Op::RESPONSE => ok(n0 && uint(at(0)) && http_status(at(1)) && is_bytes(at(2))),
        Op::BOT_CATALOG => ok(n0
            && is_id(at(0))
            && uint(at(1))
            && at(2).and_then(Value::as_array).is_some()
            && (matches!(at(3), Some(Value::Null)) || is_bytes(at(3)))),
        Op::BOT_REPLY => {
            ok(is_id(at(0)) && max(at(1), u64::from(u32::MAX)) && is_bytes(at(2)) && bit(at(3)))
        }
        Op::BOT_STATE_CHANGED => ok(n0 && max(at(0), 2) && is_id(at(1)) && uint(at(2))),
        _ => Ok(()),
    }
}

fn is_id(v: Option<&Value>) -> bool {
    v.and_then(Value::as_bytes).is_some_and(|b| b.len() == 16)
}
fn is_bytes(v: Option<&Value>) -> bool {
    v.and_then(Value::as_bytes).is_some()
}
fn uint(v: Option<&Value>) -> bool {
    v.and_then(Value::as_uint).is_some()
}
fn max(v: Option<&Value>, max: u64) -> bool {
    v.and_then(Value::as_uint).is_some_and(|x| x <= max)
}
fn bit(v: Option<&Value>) -> bool {
    max(v, 1)
}
fn class_or_null(v: Option<&Value>) -> bool {
    matches!(v, Some(Value::Null)) || bit(v)
}
fn http_status(v: Option<&Value>) -> bool {
    v.and_then(Value::as_uint)
        .is_some_and(|s| (100..=599).contains(&s))
}

/// frames.ts `validAppEntry`: `/assets/<[A-Za-z0-9_./-]+>.js`, at most 512 characters, never a parent traversal.
pub fn valid_app_entry(s: &str) -> bool {
    let Some(rest) = s
        .strip_prefix("/assets/")
        .and_then(|r| r.strip_suffix(".js"))
    else {
        return false;
    };
    s.len() <= 512
        && !rest.is_empty()
        && rest
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'.' | b'/' | b'-'))
        && !s.contains("..")
}

/// The instance's bounds on op 59 (internal/gateway/request.go).
const MAX_METHOD: usize = 16;
const MAX_ROUTE: usize = 8192;
const MAX_IDEM: usize = 64;
/// The instance's request body cap (gateway.MaxRequestBodyBytes).
pub const MAX_REQUEST_BODY: usize = 16 * 1024 * 1024;
/// The largest request or response frame (the instance's maxRequestFrameBytes): it applies to ops 59 and 60 only;
/// every other frame keeps the hello's `max_frame_bytes`.
pub const MAX_RESPONSE_FRAME: usize = MAX_REQUEST_BODY + 16 * 1024;

fn valid_request_payload(p: &[Value]) -> bool {
    let (
        Some(id),
        Some(Value::Text(method)),
        Some(Value::Text(route)),
        Some(Value::Bytes(_)),
        Some(idem),
    ) = (p.first(), p.get(1), p.get(2), p.get(3), p.get(4))
    else {
        return false;
    };
    id.as_uint().is_some()
        && !method.is_empty()
        && method.len() <= MAX_METHOD
        && !route.is_empty()
        && route.len() <= MAX_ROUTE
        && match idem {
            Value::Null => true,
            Value::Bytes(b) => !b.is_empty() && b.len() <= MAX_IDEM,
            _ => false,
        }
}

/// True when the bytes begin a four-element frame whose op is 60 (0x84, then uint8 60), read without decoding.
pub fn is_response_frame_head(bytes: &[u8]) -> bool {
    bytes.len() >= 3 && bytes[0] == 0x84 && bytes[1] == 0x18 && bytes[2] == Op::RESPONSE
}

/// True when the bytes begin a request frame (op 59): the frame the instance reads up to `MAX_RESPONSE_FRAME`.
pub fn is_request_frame_head(bytes: &[u8]) -> bool {
    bytes.len() >= 3 && bytes[0] == 0x84 && bytes[1] == 0x18 && bytes[2] == Op::REQUEST
}

/// The op of a frame whose envelope is a four-element array with an op of at most 255; nothing else is read.
pub fn frame_op(bytes: &[u8]) -> Option<u8> {
    let mut d = Decoder::new(bytes);
    if d.array_len().ok()? != 4 {
        return None;
    }
    u8::try_from(d.uint().ok()?).ok()
}

/// Reads the envelope's heads and the payload's element count without building anything, and refuses a known op whose
/// payload count is not its own before a single item is materialised: a 16 MiB frame of one-byte items would
/// otherwise cost about 32 times its size in `Value`s before `check` refused its arity. Anything this cannot read from
/// the heads is left to the full decode, which names the exact error.
fn arity_precheck(bytes: &[u8]) -> Result<(), FrameError> {
    let mut d = Decoder::new(bytes);
    let head = (|| {
        if d.array_len().ok()? != 4 {
            return None;
        }
        let op = u8::try_from(d.uint().ok()?).ok()?;
        d.uint().ok()?;
        d.skip().ok()?;
        Some((op, d.array_len().ok()?))
    })();
    let Some((op, len)) = head else {
        return Ok(());
    };
    let fits = match (op, spec(op)) {
        (Op::MESSAGE_CT, _) => len == 6 || len == 7,
        (_, Some(elements)) => len == elements,
        (_, None) => true,
    };
    if fits { Ok(()) } else { Err(FrameError::Shape) }
}

/// The ops whose malformed frame a ready gateway drops without closing the socket (frames.ts `DROPPABLE_OPS`).
pub const DROPPABLE_OPS: &[u8] = &[
    Op::VOICE_STATE,
    Op::CALL_RING,
    Op::COMMUNITY_CHANGED,
    Op::AFK_AWAY,
    Op::APP_BUILD,
    Op::BOT_CATALOG,
    Op::BOT_REPLY,
    Op::BOT_STATE_CHANGED,
];

/// The socket request shim (Bot v2): one HTTP-shaped request over the gateway, answered by op 60 with its id.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RequestFrame {
    pub id: u64,
    pub method: String,
    pub route: String,
    pub body: Vec<u8>,
    pub idem: Option<Vec<u8>>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ResponseFrame {
    pub id: u64,
    pub status: u16,
    pub body: Vec<u8>,
}

/// op 59 `[id, method, route, body, idem | null]`, `n = 0`, no group. A body over `MAX_REQUEST_BODY` is
/// `E_FRAME_LIMIT`; a request the instance would refuse as `E_FRAME_SHAPE` is refused here, before it is sent.
pub fn encode_request(r: &RequestFrame) -> Result<Vec<u8>, FrameError> {
    if r.id > MAX_SAFE_INTEGER {
        return Err(FrameError::Shape);
    }
    if r.body.len() > MAX_REQUEST_BODY {
        return Err(FrameError::Limit);
    }
    let payload = vec![
        Value::Uint(r.id),
        Value::Text(r.method.clone()),
        Value::Text(r.route.clone()),
        Value::Bytes(r.body.clone()),
        r.idem.clone().map_or(Value::Null, Value::Bytes),
    ];
    if !valid_request_payload(&payload) {
        return Err(FrameError::Shape);
    }
    encode(&Frame {
        op: Op::REQUEST,
        n: 0,
        group_id: None,
        payload,
    })
}

/// op 60 `[id, status, body]`, checked as `decode` checks it; any other frame, or an id past the safe integers, is
/// `E_FRAME_SHAPE`.
///
/// Read straight off the heads, never through `Value`: the frame may be 16 MiB, and the body is copied once.
pub fn decode_response(bytes: &[u8]) -> Result<ResponseFrame, FrameError> {
    // A wrong type or count is the frame's shape; anything else is the CBOR's own error.
    let shape = |e: CborError| match e {
        CborError::TypeMismatch { .. }
        | CborError::WrongArrayLen { .. }
        | CborError::WrongByteLen { .. } => FrameError::Shape,
        e => FrameError::Cbor(e),
    };
    let mut d = Decoder::new(bytes);
    d.array(4).map_err(shape)?;
    // frames.ts decodeFrame: op 60 is never replayed (n = 0) and carries no group.
    if d.uint().map_err(shape)? != u64::from(Op::RESPONSE) || d.uint().map_err(shape)? != 0 {
        return Err(FrameError::Shape);
    }
    d.null().map_err(shape)?;
    d.array(3).map_err(shape)?;
    let id = d.uint().map_err(shape)?;
    let status = d.uint().map_err(shape)?;
    let body = d.bytes().map_err(shape)?.to_vec();
    d.finish()?;
    if id > MAX_SAFE_INTEGER || !(100..=599).contains(&status) {
        return Err(FrameError::Shape);
    }
    Ok(ResponseFrame {
        id,
        status: status as u16,
        body,
    })
}

fn id16(v: &Value) -> Option<[u8; 16]> {
    v.as_bytes().and_then(|b| <[u8; 16]>::try_from(b).ok())
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct VoiceStateFrame {
    pub user_id: [u8; 16],
    pub device_id: [u8; 16],
    pub call_id: [u8; 16],
    pub flags: u8,
}

/// op 50 `[user_id, device_id, call_id, flags]`.
pub fn voice_state_of(f: &Frame) -> Result<VoiceStateFrame, FrameError> {
    match (f.op, f.payload.as_slice()) {
        (Op::VOICE_STATE, [u, d, c, Value::Uint(flags)]) if *flags <= 31 => Ok(VoiceStateFrame {
            user_id: id16(u).ok_or(FrameError::Shape)?,
            device_id: id16(d).ok_or(FrameError::Shape)?,
            call_id: id16(c).ok_or(FrameError::Shape)?,
            flags: *flags as u8,
        }),
        _ => Err(FrameError::Shape),
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CallRingFrame {
    pub call_id: [u8; 16],
    pub channel_id: [u8; 16],
    pub user_id: [u8; 16],
    pub state: u8,
    pub expires_ts: u64,
}

/// op 53 `[call_id, channel_id, user_id, state, expires_ts]`; an expiry past the safe integers is a shape error.
pub fn call_ring_of(f: &Frame) -> Result<CallRingFrame, FrameError> {
    match (f.op, f.payload.as_slice()) {
        (Op::CALL_RING, [c, ch, u, Value::Uint(state), Value::Uint(exp)])
            if *state <= 2 && *exp <= MAX_SAFE_INTEGER =>
        {
            Ok(CallRingFrame {
                call_id: id16(c).ok_or(FrameError::Shape)?,
                channel_id: id16(ch).ok_or(FrameError::Shape)?,
                user_id: id16(u).ok_or(FrameError::Shape)?,
                state: *state as u8,
                expires_ts: *exp,
            })
        }
        _ => Err(FrameError::Shape),
    }
}

/// op 54 `[community_id]`.
pub fn community_changed_of(f: &Frame) -> Result<[u8; 16], FrameError> {
    match (f.op, f.payload.as_slice()) {
        (Op::COMMUNITY_CHANGED, [c]) => id16(c).ok_or(FrameError::Shape),
        _ => Err(FrameError::Shape),
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AfkAwayFrame {
    pub community_id: [u8; 16],
    pub user_id: [u8; 16],
    /// 0: cleared.
    pub since: u64,
}

/// op 55 `[community_id, user_id, since_ts]`; a since past the safe integers is a shape error.
pub fn afk_away_of(f: &Frame) -> Result<AfkAwayFrame, FrameError> {
    match (f.op, f.payload.as_slice()) {
        (Op::AFK_AWAY, [c, u, Value::Uint(since)]) if *since <= MAX_SAFE_INTEGER => {
            Ok(AfkAwayFrame {
                community_id: id16(c).ok_or(FrameError::Shape)?,
                user_id: id16(u).ok_or(FrameError::Shape)?,
                since: *since,
            })
        }
        _ => Err(FrameError::Shape),
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BotCatalogFrame {
    pub community_id: [u8; 16],
    pub revision: u64,
    pub commands: Vec<Value>,
    pub publication: Option<Vec<u8>>,
}

/// op 61 `[community_id, revision, filtered_catalog, publication | null]`.
pub fn bot_catalog_of(f: &Frame) -> Result<BotCatalogFrame, FrameError> {
    match (f.op, f.payload.as_slice()) {
        (
            Op::BOT_CATALOG,
            [
                c,
                Value::Uint(revision),
                Value::Array(commands),
                publication,
            ],
        ) => Ok(BotCatalogFrame {
            community_id: id16(c).ok_or(FrameError::Shape)?,
            revision: *revision,
            commands: commands.clone(),
            publication: match publication {
                Value::Null => None,
                Value::Bytes(b) => Some(b.clone()),
                _ => return Err(FrameError::Shape),
            },
        }),
        _ => Err(FrameError::Shape),
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BotReplyFrame {
    pub request_id: [u8; 16],
    pub seq: u32,
    pub sealed: Vec<u8>,
    pub final_: bool,
}

/// op 62 `[request_id, seq, sealed, final]`.
pub fn bot_reply_of(f: &Frame) -> Result<BotReplyFrame, FrameError> {
    match (f.op, f.payload.as_slice()) {
        (Op::BOT_REPLY, [r, Value::Uint(seq), Value::Bytes(sealed), Value::Uint(fin)])
            if *fin <= 1 =>
        {
            Ok(BotReplyFrame {
                request_id: id16(r).ok_or(FrameError::Shape)?,
                seq: u32::try_from(*seq).map_err(|_| FrameError::Shape)?,
                sealed: sealed.clone(),
                final_: *fin == 1,
            })
        }
        _ => Err(FrameError::Shape),
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BotStateChangedFrame {
    pub scope_kind: u8,
    pub scope_id: [u8; 16],
    pub revision: u64,
}

/// op 63 `[scope_kind 0..2, scope_id, revision]`.
pub fn bot_state_changed_of(f: &Frame) -> Result<BotStateChangedFrame, FrameError> {
    match (f.op, f.payload.as_slice()) {
        (Op::BOT_STATE_CHANGED, [Value::Uint(kind), s, Value::Uint(revision)]) if *kind <= 2 => {
            Ok(BotStateChangedFrame {
                scope_kind: *kind as u8,
                scope_id: id16(s).ok_or(FrameError::Shape)?,
                revision: *revision,
            })
        }
        _ => Err(FrameError::Shape),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn frame(op: u8, n: u64, group_id: Option<[u8; 16]>, payload: Vec<Value>) -> Vec<u8> {
        Value::Array(vec![
            Value::Uint(u64::from(op)),
            Value::Uint(n),
            group_id.map_or(Value::Null, |g| Value::Bytes(g.to_vec())),
            Value::Array(payload),
        ])
        .encode()
    }

    /// A maximum-size frame (16 MiB) of one-byte items is refused from its heads, before any item is built.
    #[test]
    fn many_tiny_items_refused_by_arity() {
        let items = MAX_RESPONSE_FRAME - 16;
        for op in [Op::RESPONSE, Op::MESSAGE_CT, Op::PRESENCE] {
            let mut bytes = Encoder::new();
            bytes
                .array(4)
                .uint(u64::from(op))
                .uint(0)
                .null()
                .array(items);
            let mut bytes = bytes.into_vec();
            bytes.resize(bytes.len() + items, 0);
            assert!(bytes.len() <= MAX_RESPONSE_FRAME);
            assert_eq!(arity_precheck(&bytes), Err(FrameError::Shape), "op {op}");
            assert_eq!(decode(&bytes), Err(FrameError::Shape), "op {op}");
        }
        let mut bytes = Encoder::new();
        bytes
            .array(4)
            .uint(u64::from(Op::RESPONSE))
            .uint(0)
            .null()
            .array(items);
        let mut bytes = bytes.into_vec();
        bytes.resize(bytes.len() + items, 0);
        assert_eq!(decode_response(&bytes), Err(FrameError::Shape));
        assert_eq!(frame_op(&bytes), Some(Op::RESPONSE));
    }

    #[test]
    fn response_errors_match_decode() {
        let ok = frame(
            Op::RESPONSE,
            0,
            None,
            vec![Value::Uint(7), Value::Uint(200), Value::Bytes(vec![])],
        );
        assert!(decode_response(&ok).is_ok());
        let replayed = frame(
            Op::RESPONSE,
            3,
            None,
            vec![Value::Uint(7), Value::Uint(200), Value::Bytes(vec![])],
        );
        assert_eq!(decode_response(&replayed), Err(FrameError::Shape));
        assert_eq!(decode(&replayed), Err(FrameError::Shape));
        let grouped = frame(
            Op::RESPONSE,
            0,
            Some([1; 16]),
            vec![Value::Uint(7), Value::Uint(200), Value::Bytes(vec![])],
        );
        assert_eq!(decode_response(&grouped), Err(FrameError::Shape));
        let text_body = frame(
            Op::RESPONSE,
            0,
            None,
            vec![Value::Uint(7), Value::Uint(200), Value::Text("x".into())],
        );
        assert_eq!(decode_response(&text_body), Err(FrameError::Shape));
        let mut trailing = ok.clone();
        trailing.push(0);
        assert_eq!(
            decode_response(&trailing).unwrap_err().code(),
            "E_CBOR_TRAILING"
        );
        let other = frame(Op::PRESENCE, 0, None, vec![]);
        assert_eq!(decode_response(&other), Err(FrameError::Shape));
    }

    #[test]
    fn unknown_op_any_payload() {
        let f = decode(&frame(99, 1, None, vec![])).unwrap();
        assert_eq!(
            f,
            Frame {
                op: 99,
                n: 1,
                group_id: None,
                payload: vec![]
            }
        );
    }

    #[test]
    fn op_above_255_text_group_and_short_ready_refused() {
        let big = Value::Array(vec![
            Value::Uint(256),
            Value::Uint(0),
            Value::Null,
            Value::Array(vec![]),
        ])
        .encode();
        assert_eq!(decode(&big), Err(FrameError::Shape));
        let text_gid = Value::Array(vec![
            Value::Uint(19),
            Value::Uint(1),
            Value::Text("g".into()),
            Value::Array(vec![]),
        ])
        .encode();
        assert_eq!(decode(&text_gid), Err(FrameError::Shape));
        let short_ready = frame(
            Op::READY,
            1,
            None,
            vec![Value::Uint(1), Value::Uint(2), Value::Uint(3)],
        );
        assert_eq!(decode(&short_ready), Err(FrameError::Shape));
    }

    #[test]
    fn encode_refuses_unsafe_n_and_deep_payload() {
        let f = Frame {
            op: Op::HEARTBEAT,
            n: MAX_SAFE_INTEGER + 1,
            group_id: None,
            payload: vec![],
        };
        assert_eq!(encode(&f), Err(FrameError::Shape));
        let mut deep = Value::Uint(0);
        for _ in 0..8 {
            deep = Value::Array(vec![deep]);
        }
        let f = Frame {
            op: 99,
            n: 0,
            group_id: None,
            payload: vec![deep],
        };
        assert_eq!(encode(&f).unwrap_err().code(), "E_CBOR_DEPTH");
    }

    #[test]
    fn message_ct_with_or_without_class() {
        let g = Some([7; 16]);
        let ct = vec![
            Value::Uint(4130),
            Value::Uint(42),
            Value::Bytes(vec![8; 16]),
            Value::Bytes(vec![1, 2, 3]),
            Value::Bytes(vec![10; 32]),
            Value::Uint(1_758_659_700),
        ];
        let with = |extra: Vec<Value>| [ct.clone(), extra].concat();
        assert!(decode(&frame(19, 9, g, with(vec![Value::Uint(0)]))).is_ok());
        assert!(decode(&frame(19, 9, g, with(vec![Value::Uint(1)]))).is_ok());
        assert!(decode(&frame(19, 9, g, with(vec![Value::Null]))).is_ok());
        assert!(decode(&frame(19, 9, g, ct.clone())).is_ok());
        assert!(decode(&frame(19, 9, g, with(vec![Value::Uint(0), Value::Uint(0)]))).is_err());
        assert!(decode(&frame(19, 9, g, with(vec![Value::Uint(2)]))).is_err());
        assert!(decode(&frame(19, 9, g, with(vec![Value::Text("post".into())]))).is_err());
        assert!(decode(&frame(19, 9, None, with(vec![Value::Uint(0)]))).is_err());
    }

    #[test]
    fn app_entry_grammar() {
        assert!(valid_app_entry("/assets/index-abc_1.js"));
        assert!(!valid_app_entry("/assets/../x.js"));
        assert!(!valid_app_entry("https://x/assets/a.js"));
        assert!(!valid_app_entry("/assets/a b.js"));
        assert!(!valid_app_entry("/assets/.js"));
    }

    #[test]
    fn response_round_trip_and_heads() {
        let bytes = frame(
            Op::RESPONSE,
            0,
            None,
            vec![Value::Uint(7), Value::Uint(201), Value::Bytes(vec![0x80])],
        );
        assert!(is_response_frame_head(&bytes));
        assert_eq!(
            decode_response(&bytes).unwrap(),
            ResponseFrame {
                id: 7,
                status: 201,
                body: vec![0x80]
            }
        );
        let req = encode_request(&RequestFrame {
            id: 7,
            method: "POST".into(),
            route: "/v1/communities".into(),
            body: vec![0x80],
            idem: Some(vec![1; 16]),
        })
        .unwrap();
        assert!(is_request_frame_head(&req));
        assert_eq!(frame_op(&req), Some(Op::REQUEST));
        let empty_method = RequestFrame {
            id: 1,
            method: String::new(),
            route: "/v1/x".into(),
            body: vec![],
            idem: None,
        };
        assert_eq!(encode_request(&empty_method), Err(FrameError::Shape));
        let long_idem = RequestFrame {
            idem: Some(vec![0; 65]),
            method: "GET".into(),
            ..empty_method
        };
        assert_eq!(encode_request(&long_idem), Err(FrameError::Shape));
    }

    #[test]
    fn typed_views() {
        let vs = decode(&frame(
            Op::VOICE_STATE,
            0,
            None,
            vec![
                Value::Bytes(vec![1; 16]),
                Value::Bytes(vec![2; 16]),
                Value::Bytes(vec![3; 16]),
                Value::Uint(31),
            ],
        ))
        .unwrap();
        assert_eq!(voice_state_of(&vs).unwrap().flags, 31);
        let ring = decode(&frame(
            Op::CALL_RING,
            0,
            None,
            vec![
                Value::Bytes(vec![1; 16]),
                Value::Bytes(vec![2; 16]),
                Value::Bytes(vec![3; 16]),
                Value::Uint(2),
                Value::Uint(MAX_SAFE_INTEGER + 1),
            ],
        ))
        .unwrap();
        assert_eq!(call_ring_of(&ring), Err(FrameError::Shape));
        let reply = decode(&frame(
            Op::BOT_REPLY,
            5,
            None,
            vec![
                Value::Bytes(vec![1; 16]),
                Value::Uint(3),
                Value::Bytes(vec![9]),
                Value::Uint(1),
            ],
        ))
        .unwrap();
        assert!(bot_reply_of(&reply).unwrap().final_);
        let bad_reply = frame(
            Op::BOT_REPLY,
            5,
            None,
            vec![
                Value::Bytes(vec![1; 16]),
                Value::Uint(1 << 32),
                Value::Bytes(vec![9]),
                Value::Uint(1),
            ],
        );
        assert_eq!(decode(&bad_reply), Err(FrameError::Shape));
    }
}

//! `dilla-testkit web-driver`: the native peer of the browser tests (dilla-web-1, L-E2E-01; the
//! enrolment, revocation, DM and per-channel verbs of dilla-web-2a, L-E2E-10; and the typed
//! envelopes, deletes and attachment verbs of dilla-web-2b, L-E2E-20; the call and ring verbs of dilla-web-3,
//! L-E2E-30).

use std::collections::{BTreeMap, BTreeSet};
use std::io::{BufRead, Write};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use dilla_core::attachment;
use dilla_core::cbor::{Decoder, Encoder, decode_strict};
use dilla_core::envelope::{Attachment, EnvelopeType};
use dilla_core::identity::{DeviceList, Tier};
use dilla_core::ids::{CommunityId, DeviceId, InstanceId, MsgId};
use dilla_core::mls::{DillaBinding, GroupKind, external_senders};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

use crate::ds::remote::{HttpDs, ServedDeviceList, instance_document};
use crate::{DeliveryService, DsError, Received, Runner, TestClient, TestkitError};

const GROUP: &str = "text";
const BODY_MAX_BYTES: usize = 4000;
const REQUEST_MAX_BYTES: usize = 4 * 1024 * 1024;

fn scenario(msg: impl Into<String>) -> TestkitError {
    TestkitError::Scenario(msg.into())
}

fn new_verb_error(error: TestkitError) -> TestkitError {
    match error {
        TestkitError::Ds(ref ds) if ds.code().starts_with("E_") => scenario(ds.code()),
        other => other,
    }
}

fn seeded_plaintext(seed: u64, size: usize) -> Vec<u8> {
    let mut out = Vec::with_capacity(size);
    for k in 0..size.div_ceil(32) {
        let mut hash = Sha256::new();
        hash.update(seed.to_be_bytes());
        hash.update((k as u64).to_be_bytes());
        out.extend_from_slice(&hash.finalize());
    }
    out.truncate(size);
    out
}

struct Peer {
    name: String,
    community: [u8; 16],
    channel: [u8; 16],
    channels: Vec<[u8; 16]>,
    password: Option<String>,
}

/// Who a call verb acts as (L-E2E-30): the peer's acting device, or the device `enrol` added to its account.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
enum Actor {
    Peer,
    Second,
}

/// The device `enrol` added to the peer's account, kept until `revoke` names it.
struct Second {
    client: TestClient,
    /// The enrolled device's own session: the call verbs act through it for `actor: "second"`.
    ds: HttpDs,
    /// The device `rotate_umk` replaced: it holds no leaf any more and acts as no actor.
    replaced: bool,
}

struct Sent {
    msg_id: MsgId,
    channel: [u8; 16],
    attachments: Vec<Attachment>,
}

pub struct WebDriver {
    runner: Runner,
    ds_url: String,
    seed: u64,
    peer: Option<Peer>,
    group: Option<Vec<u8>>,
    pending: Vec<Received>,
    held: BTreeMap<(Vec<u8>, u64), Received>,
    deleted: BTreeSet<(Vec<u8>, u64)>,
    pending_deleted: Vec<(Vec<u8>, u64)>,
    sent: BTreeMap<(Vec<u8>, u64), Sent>,
    attachments_made: u64,
    groups: BTreeMap<[u8; 16], Vec<u8>>,
    registered_all: bool,
    dms: BTreeMap<[u8; 16], Vec<u8>>,
    second: Option<Second>,
    /// The community of each channel this driver joined (`join`): a rotation in a joined group binds that channel.
    joined: BTreeMap<[u8; 16], [u8; 16]>,
    /// The call group each actor holds per channel (`call_join` until `call_leave`).
    calls: BTreeMap<(Actor, [u8; 16]), Vec<u8>>,
    /// How many call groups this driver registered: each gets its own id (a DM's text group holds the channel's).
    call_groups_made: u64,
    /// The `expires_ts` of the last op 53 state 1 each actor's gateway received, per call (`call_decline`).
    ringing: BTreeMap<(Actor, [u8; 16]), u64>,
    /// The peer's devices `rotate_umk` replaced: a rejoin of a call group commits the Remove of any leaf of theirs.
    replaced_devices: Vec<DeviceId>,
}

impl WebDriver {
    pub fn new(ds: String, seed: u64) -> Self {
        Self {
            runner: Runner::new(seed).with_ds(Some(ds.clone())),
            ds_url: ds,
            seed,
            peer: None,
            group: None,
            pending: Vec::new(),
            held: BTreeMap::new(),
            deleted: BTreeSet::new(),
            pending_deleted: Vec::new(),
            sent: BTreeMap::new(),
            attachments_made: 0,
            groups: BTreeMap::new(),
            registered_all: false,
            dms: BTreeMap::new(),
            second: None,
            joined: BTreeMap::new(),
            calls: BTreeMap::new(),
            call_groups_made: 0,
            ringing: BTreeMap::new(),
            replaced_devices: Vec::new(),
        }
    }

    pub fn username(seed: u64) -> String {
        format!("peer{:06x}", seed & 0xff_ffff)
    }

    fn read_line_bounded(input: &mut impl BufRead) -> std::io::Result<Option<(String, bool)>> {
        let mut bytes = Vec::new();
        let mut oversized = false;
        let mut saw_any = false;
        loop {
            let available = input.fill_buf()?;
            if available.is_empty() {
                break;
            }
            saw_any = true;
            let end = available
                .iter()
                .position(|b| *b == b'\n')
                .map_or(available.len(), |n| n + 1);
            if !oversized {
                if bytes.len().saturating_add(end) > REQUEST_MAX_BYTES {
                    oversized = true;
                    bytes.clear();
                } else {
                    bytes.extend_from_slice(&available[..end]);
                }
            }
            let complete = available[end - 1] == b'\n';
            input.consume(end);
            if complete {
                break;
            }
        }
        if !saw_any {
            return Ok(None);
        }
        if oversized {
            return Ok(Some((String::new(), true)));
        }
        let line = String::from_utf8(bytes)
            .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e))?;
        Ok(Some((line, false)))
    }

    pub fn serve(&mut self, mut input: impl BufRead, mut out: impl Write) -> std::io::Result<()> {
        while let Some((line, oversized)) = Self::read_line_bounded(&mut input)? {
            if oversized {
                writeln!(
                    out,
                    "{}",
                    json!({"id": null, "ok": false, "error": "request exceeds 4 MiB"})
                )?;
                out.flush()?;
                continue;
            }
            if line.trim().is_empty() {
                continue;
            }
            let answer = match serde_json::from_str::<Value>(&line) {
                Ok(req) => self.handle(&req),
                Err(e) => {
                    json!({"id": Value::Null, "ok": false, "error": format!("not JSON: {e}")})
                }
            };
            writeln!(out, "{answer}")?;
            out.flush()?;
        }
        Ok(())
    }

    pub fn handle(&mut self, req: &Value) -> Value {
        let id = req.get("id").cloned().unwrap_or(Value::Null);
        let op = req.get("op").and_then(Value::as_str).unwrap_or("");
        match self.dispatch(op, req) {
            Ok(Value::Object(mut fields)) => {
                fields.insert("id".into(), id);
                fields.insert("ok".into(), Value::Bool(true));
                Value::Object(fields)
            }
            Ok(other) => json!({"id": id, "ok": true, "value": other}),
            Err(e) => json!({"id": id, "ok": false, "error": e.to_string()}),
        }
    }

    fn text<'a>(req: &'a Value, key: &str) -> Result<&'a str, TestkitError> {
        req.get(key)
            .and_then(Value::as_str)
            .ok_or_else(|| scenario(format!("the request carries no {key}")))
    }

    fn hex16(req: &Value, key: &str) -> Result<[u8; 16], TestkitError> {
        let s = Self::text(req, key)?;
        if s.len() != 32
            || !s
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        {
            return Err(scenario(format!("{key} is not 32 lowercase hex")));
        }
        let bytes =
            hex::decode(s).map_err(|_| scenario(format!("{key} is not 32 lowercase hex")))?;
        bytes
            .try_into()
            .map_err(|_| scenario(format!("{key} is not 32 lowercase hex")))
    }

    fn digest32(req: &Value, key: &str) -> Result<[u8; 32], TestkitError> {
        let value = req
            .get(key)
            .ok_or_else(|| scenario(format!("the request carries no {key}")))?;
        let s = value
            .as_str()
            .ok_or_else(|| scenario(format!("{key} is not 64 lowercase hex")))?;
        if s.len() != 64
            || !s
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        {
            return Err(scenario(format!("{key} is not 64 lowercase hex")));
        }
        hex::decode(s)
            .map_err(|_| scenario(format!("{key} is not 64 lowercase hex")))?
            .try_into()
            .map_err(|_| scenario(format!("{key} is not 64 lowercase hex")))
    }

    fn uint(req: &Value, key: &str) -> Option<Option<u64>> {
        req.get(key).map(Value::as_u64)
    }

    fn hex_bytes(s: &str, max: usize) -> Option<Vec<u8>> {
        if !s.len().is_multiple_of(2)
            || s.len() / 2 > max
            || !s
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        {
            return None;
        }
        hex::decode(s).ok()
    }

    fn validate(op: &str, req: &Value) -> Result<(), TestkitError> {
        match op {
            "setup" => {
                let community = Self::text(req, "community")?;
                let channel = Self::text(req, "channel")?;
                if !(1..=255).contains(&community.len()) {
                    return Err(scenario("community must be 1..=255 bytes"));
                }
                if !(1..=100).contains(&channel.len()) {
                    return Err(scenario("channel must be 1..=100 bytes"));
                }
                let channels = req
                    .get("channels")
                    .map(Value::as_u64)
                    .unwrap_or(Some(1))
                    .filter(|n| (1..=200).contains(n))
                    .ok_or_else(|| scenario("channels must be 1..=200"))?;
                if channels > 1 && channel.len() > 96 {
                    return Err(scenario("channel must be 1..=96 bytes when channels > 1"));
                }
                if let Some(password) = req.get("password") {
                    let valid = password
                        .as_str()
                        .is_some_and(|s| (1..=128).contains(&s.len()));
                    if !valid {
                        return Err(scenario("password must be 1..=128 bytes"));
                    }
                }
                if req.get("voice").is_some_and(|v| !v.is_boolean()) {
                    return Err(scenario("voice must be boolean"));
                }
                if req.get("voice").and_then(Value::as_bool) == Some(true) && channel.len() > 94 {
                    return Err(scenario(
                        "channel must be 1..=94 bytes with a voice channel",
                    ));
                }
            }
            "join" => {
                Self::hex16(req, "community_id")?;
                Self::hex16(req, "channel_id")?;
                Self::hex16(req, "group_id")?;
                Self::text(req, "invite_code")?;
            }
            "send" => {
                let kind = match Self::uint(req, "type") {
                    None => 0,
                    Some(Some(n)) if n <= 6 => n,
                    _ => return Err(scenario("type must be 0..=6")),
                };
                if req.get("reply_to").is_some() {
                    if !req["reply_to"].is_string() {
                        return Err(scenario("reply_to is not 32 lowercase hex"));
                    }
                    Self::hex16(req, "reply_to")?;
                } else if kind != 0 {
                    return Err(scenario("reply_to is required"));
                }
                if req.get("thread_id").is_some() {
                    if !req["thread_id"].is_string() {
                        return Err(scenario("thread_id is not 32 lowercase hex"));
                    }
                    Self::hex16(req, "thread_id")?;
                    if kind != 0 {
                        return Err(scenario("thread_id is only for type 0"));
                    }
                }
                let body_value = req.get("body");
                let body = body_value.and_then(Value::as_str).unwrap_or("");
                match kind {
                    0 | 1
                        if body_value.is_some_and(|v| !v.is_string())
                            || !(1..=BODY_MAX_BYTES).contains(&body.len()) =>
                    {
                        return Err(scenario("body must be 1..=4000 bytes"));
                    }
                    3 | 4
                        if body_value.is_some_and(|v| !v.is_string())
                            || !(1..=32).contains(&body.len()) =>
                    {
                        return Err(scenario("body must be 1..=32 bytes for a reaction"));
                    }
                    2 | 5 | 6 if body_value.is_some_and(|v| !v.is_string()) || !body.is_empty() => {
                        return Err(scenario("body must be empty for types 2, 5 and 6"));
                    }
                    _ => {}
                }
                if req.get("channel_id").is_some() {
                    Self::hex16(req, "channel_id")?;
                }
            }
            "delete" => {
                Self::hex16(req, "msg_id")?;
                if !matches!(Self::uint(req, "seq"), Some(Some(n)) if n >= 1) {
                    return Err(scenario(if req.get("seq").is_none() {
                        "the request carries no seq"
                    } else {
                        "seq must be at least 1"
                    }));
                }
                if req.get("channel_id").is_some() {
                    Self::hex16(req, "channel_id")?;
                }
            }
            "attach" => {
                if req.get("channel_id").is_some() {
                    Self::hex16(req, "channel_id")?;
                }
                let name = Self::text(req, "name")?;
                if name.len() > 255 {
                    return Err(scenario("name must be 0..=255 bytes"));
                }
                let mime = Self::text(req, "mime")?;
                if !(1..=255).contains(&mime.len()) {
                    return Err(scenario("mime must be 1..=255 bytes"));
                }
                if req
                    .get("body")
                    .is_some_and(|v| v.as_str().is_none_or(|s| s.len() > 4000))
                {
                    return Err(scenario("body must be 0..=4000 bytes"));
                }
                let bytes = req.get("bytes_hex");
                let size = req.get("size");
                let seed = req.get("seed");
                if (bytes.is_some() == size.is_some()) || (size.is_some() != seed.is_some()) {
                    return Err(scenario("attach takes bytes_hex or size with seed"));
                }
                if let Some(bytes) = bytes
                    && bytes
                        .as_str()
                        .and_then(|s| Self::hex_bytes(s, 1_048_576))
                        .is_none()
                {
                    return Err(scenario(
                        "bytes_hex must be lowercase hex of at most 1048576 bytes",
                    ));
                }
                if size.is_some() && !matches!(Self::uint(req, "size"), Some(Some(1..=26_214_400)))
                {
                    return Err(scenario("size must be 1..=26214400"));
                }
                if seed.is_some() && !matches!(Self::uint(req, "seed"), Some(Some(_))) {
                    return Err(scenario("seed must be an unsigned integer"));
                }
                for key in ["w", "h"] {
                    if matches!(Self::uint(req, key), Some(None)) {
                        return Err(scenario(format!("{key} must be an unsigned integer")));
                    }
                }
                if let Some(v) = req.get("thumb_hex")
                    && v.as_str().and_then(|s| Self::hex_bytes(s, 8_176)).is_none()
                {
                    return Err(scenario(
                        "thumb_hex must be lowercase hex of at most 8176 bytes",
                    ));
                }
            }
            "fetch_attachment" => {
                if !matches!(Self::uint(req, "seq"), Some(Some(n)) if n >= 1) {
                    return Err(scenario(if req.get("seq").is_none() {
                        "the request carries no seq"
                    } else {
                        "seq must be at least 1"
                    }));
                }
                if !matches!(Self::uint(req, "index"), Some(Some(_))) {
                    return Err(scenario(if req.get("index").is_none() {
                        "the request carries no index"
                    } else {
                        "index must be an unsigned integer"
                    }));
                }
                if req.get("channel_id").is_some() {
                    Self::hex16(req, "channel_id")?;
                }
            }
            "blob_status" => {
                Self::digest32(req, "blob_id")?;
                if req.get("channel_id").is_some() {
                    Self::hex16(req, "channel_id")?;
                }
            }
            "send_dm" => {
                Self::hex16(req, "channel_id")?;
                let body = Self::text(req, "body")?;
                if !(1..=BODY_MAX_BYTES).contains(&body.len()) {
                    return Err(scenario("body must be 1..=4000 bytes"));
                }
                if req.get("thread_id").is_some() {
                    if !req["thread_id"].is_string() {
                        return Err(scenario("thread_id is not 32 lowercase hex"));
                    }
                    Self::hex16(req, "thread_id")?;
                }
            }
            "sync_dm" => {
                Self::hex16(req, "channel_id")?;
            }
            "sync" | "members" => {
                if req.get("channel_id").is_some() {
                    Self::hex16(req, "channel_id")?;
                }
            }
            "revoke" => {
                Self::hex16(req, "device_id")?;
            }
            "open_dm" => {
                Self::hex16(req, "user_id")?;
            }
            "trust_list" => {
                Self::hex16(req, "user")?;
            }
            "rotate_umk" => {
                if req.get("user").is_some() {
                    Self::hex16(req, "user")?;
                }
            }
            "typing" => {
                Self::hex16(req, "channel")?;
                if req.get("on").and_then(Value::as_bool).is_none() {
                    return Err(scenario("on must be boolean"));
                }
            }
            "search_seed" => {
                Self::hex16(req, "channel")?;
                let body = Self::text(req, "body")?;
                if !(1..=BODY_MAX_BYTES).contains(&body.len()) {
                    return Err(scenario("body must be 1..=4000 bytes"));
                }
            }
            "voice_self" => {
                Self::hex16(req, "call")?;
                if !matches!(Self::uint(req, "flags"), Some(Some(0..=3))) {
                    return Err(scenario("flags must be 0..=3"));
                }
            }
            "leave" => {
                if req.get("channel_id").is_some() {
                    Self::hex16(req, "channel_id")?;
                }
            }
            "call_join" | "call_leave" | "call_members" | "call_start" | "call_read" => {
                Self::actor(req)?;
                Self::hex16(req, "channel")?;
            }
            "call_decline" => {
                Self::actor(req)?;
                Self::hex16(req, "call")?;
            }
            "call_check" => {
                Self::actor(req)?;
                Self::hex16(req, "channel")?;
            }
            "allow_everyone" => match Self::uint(req, "bits") {
                Some(Some(bits)) if (1..=0xFF_FFFF).contains(&bits) => {}
                _ => return Err(scenario("bits must be 1..=0xffffff")),
            },
            "forge_device" => {}
            "register" | "update" | "enrol" | "dms" => {}
            _ => return Err(scenario(format!("unknown op {op}"))),
        }
        Ok(())
    }

    fn dispatch(&mut self, op: &str, req: &Value) -> Result<Value, TestkitError> {
        Self::validate(op, req)?;
        if op != "setup" && self.peer.is_none() {
            return Err(scenario("setup first"));
        }
        if matches!(
            op,
            "send"
                | "sync"
                | "members"
                | "update"
                | "delete"
                | "attach"
                | "fetch_attachment"
                | "blob_status"
                | "leave"
        ) && self.group.is_none()
        {
            return Err(scenario("no group yet: register or join first"));
        }
        if matches!(op, "register" | "join")
            && self.group.is_some()
            && !(op == "register" && self.registered_all)
        {
            return Err(scenario("the driver already holds a group"));
        }
        match op {
            "setup" => self.setup(req),
            "register" => self.register(),
            "join" => self.join(req),
            "send" => self.send(req),
            "delete" => self.delete(req),
            "attach" => self.attach(req),
            "fetch_attachment" => self.fetch_attachment(req),
            "blob_status" => self.blob_status(req),
            "sync" => self.sync_group(self.group_of(req)?),
            "members" => self.members(self.group_of(req)?),
            "update" => self.update(),
            "enrol" => self.enrol(),
            "revoke" => self.revoke(req),
            "open_dm" => self.open_dm(req),
            "dms" => self.list_dms(),
            "send_dm" => self.send_dm(req),
            "sync_dm" => self.sync_dm(req),
            "typing" => self.typing(req).map_err(new_verb_error),
            "allow_everyone" => self.allow_everyone(req).map_err(new_verb_error),
            "search_seed" => self.search_seed(req).map_err(new_verb_error),
            "voice_self" => self.voice_self(req).map_err(new_verb_error),
            "trust_list" => self.trust_list(req).map_err(new_verb_error),
            "rotate_umk" => self.rotate_umk(req).map_err(new_verb_error),
            "leave" => self.leave(req).map_err(new_verb_error),
            "call_join" => self.call_join(req).map_err(new_verb_error),
            "call_leave" => self.call_leave(req).map_err(new_verb_error),
            "call_members" => self.call_members(req).map_err(new_verb_error),
            "call_start" => self.call_start(req).map_err(new_verb_error),
            "call_read" => self.call_read(req).map_err(new_verb_error),
            "call_decline" => self.call_decline(req).map_err(new_verb_error),
            "call_check" => self.call_check(req).map_err(new_verb_error),
            "forge_device" => self.forge_device().map_err(new_verb_error),
            _ => unreachable!(),
        }
    }

    fn setup(&mut self, req: &Value) -> Result<Value, TestkitError> {
        if self.peer.is_some() {
            return Err(scenario("setup runs once per driver"));
        }
        let community_name = Self::text(req, "community")?;
        let channel_name = Self::text(req, "channel")?;
        let channel_count = req.get("channels").and_then(Value::as_u64).unwrap_or(1);
        let password = req
            .get("password")
            .and_then(Value::as_str)
            .map(str::to_owned);
        let with_voice = req.get("voice").and_then(Value::as_bool).unwrap_or(false);
        let name = Self::username(self.seed);
        self.runner.exec_line("instance web")?;
        self.runner
            .exec_line(&format!("client {name} key_packages=none"))?;
        self.runner
            .exec_line(&format!("publish_key_packages {name} 32"))?;
        let (user_id, device_id, community, channels, voice, invite_code) = self.runner.with_session(&name, |client, ds| {
            let user_id = client.user_id().to_hex();
            let device_id = client.device_id().to_hex();
            if let Some(password) = &password { ds.set_password(password)?; }
            let community = ds.create_community(community_name)?;
            let mut channels = Vec::new();
            for k in 1..=channel_count {
                let channel_label = if k == 1 { channel_name.to_owned() } else { format!("{channel_name}-{k}") };
                let mut retries = 0;
                let (channel, mode, visibility) = loop {
                    match ds.create_text_channel(&community, &channel_label) {
                        Ok(v) => break v,
                        Err(e) if e.retry_after_ms().is_some() && retries < 10 => {
                            retries += 1;
                            std::thread::sleep(Duration::from_millis(e.retry_after_ms().unwrap_or(0).min(60_000)));
                        }
                        Err(e) => return Err(e.into()),
                    }
                };
                if mode != 0 || visibility != 0 {
                    return Err(scenario(format!("the instance answered mode {mode} visibility {visibility} for a text channel")));
                }
                channels.push(channel);
            }
            let voice = if with_voice {
                let (voice, mode, visibility) = ds.create_channel(&community, 1, &format!("{channel_name}-voice"))?;
                if mode != 0 || visibility != 0 {
                    return Err(scenario(format!("the instance answered mode {mode} visibility {visibility} for a voice channel")));
                }
                Some(voice)
            } else {
                None
            };
            let invite_code = ds.create_invite(&community, 100, 3600)?;
            Ok((user_id, device_id, community, channels, voice, invite_code))
        })?;
        let channel = channels[0];
        self.peer = Some(Peer {
            name: name.clone(),
            community,
            channel,
            channels: channels.clone(),
            password,
        });
        if channel_count > 1 {
            for (index, channel) in channels.iter().enumerate() {
                let label = if index == 0 {
                    GROUP.to_owned()
                } else {
                    format!("text-{}", index + 1)
                };
                self.runner.exec_line(&format!(
                    "group {label} kind=text target={} community={} creator={name}",
                    hex::encode(channel),
                    hex::encode(community)
                ))?;
                let group = self.runner.group_id(&label)?;
                self.groups.insert(*channel, group.clone());
                if index == 0 {
                    self.group = Some(group);
                }
            }
            self.registered_all = true;
        }
        Ok(
            json!({"username": name, "display": name, "user_id": user_id, "device_id": device_id,
            "community_id": hex::encode(community), "channel_id": hex::encode(channel),
            "channel_ids": channels.iter().map(hex::encode).collect::<Vec<_>>(), "invite_code": invite_code,
            "voice_channel_id": voice.map(hex::encode)}),
        )
    }

    fn peer(&self) -> &Peer {
        self.peer.as_ref().expect("dispatch checked setup")
    }
    fn group(&self) -> Vec<u8> {
        self.group.as_ref().expect("dispatch checked group").clone()
    }

    fn group_of(&self, req: &Value) -> Result<Vec<u8>, TestkitError> {
        if req.get("channel_id").is_none() {
            return Ok(self.group());
        }
        let channel = Self::hex16(req, "channel_id")?;
        self.groups
            .get(&channel)
            .cloned()
            .ok_or_else(|| scenario("channel_id is not a channel of this driver with a group"))
    }

    fn channel_and_group(&self, req: &Value) -> Result<([u8; 16], Vec<u8>), TestkitError> {
        if req.get("channel_id").is_some() {
            let channel = Self::hex16(req, "channel_id")?;
            let group = self.group_of(req)?;
            return Ok((channel, group));
        }
        let group = self.group();
        self.groups
            .iter()
            .find(|(_, g)| *g == &group)
            .map(|(c, _)| (*c, group))
            .ok_or_else(|| scenario("no channel holds the driver's group"))
    }

    fn take_pending(&mut self, group: &[u8]) -> Vec<Received> {
        let mut taken = Vec::new();
        self.pending.retain(|row| {
            if row.group_id == group {
                taken.push(row.clone());
                false
            } else {
                true
            }
        });
        taken
    }

    fn now_unix() -> u64 {
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_secs())
            .unwrap_or(0)
    }

    fn binding(
        &self,
        channel: [u8; 16],
        community: Option<[u8; 16]>,
        kind: GroupKind,
    ) -> Result<DillaBinding, TestkitError> {
        let doc = instance_document(&self.ds_url)?;
        Ok(Self::binding_of(
            InstanceId::from_bytes(doc.instance_id),
            channel,
            community,
            kind,
        ))
    }

    /// The one binding shape of every group this driver makes or joins: only the kind and the target differ.
    fn binding_of(
        instance_id: InstanceId,
        channel: [u8; 16],
        community: Option<[u8; 16]>,
        kind: GroupKind,
    ) -> DillaBinding {
        DillaBinding {
            v: 1,
            instance_id,
            community_id: community.map(CommunityId::from_bytes),
            target_id: channel,
            kind,
            policy_version: 1,
            e2ee_version: 1,
            media_version: kind.media_version(),
        }
    }

    fn served_list(blob: &[u8], version: u64) -> Result<DeviceList, TestkitError> {
        let list = DeviceList::decode(blob)?;
        if list.unsigned.version != version {
            return Err(scenario(
                "the served device list disagrees with its outer version",
            ));
        }
        Ok(list)
    }

    /// Requirement 8 of web-2a task 9. The password precondition is checked before the held-group
    /// one: the harness proves the password refusal on a peer that was set up and holds no group.
    fn enrol(&mut self) -> Result<Value, TestkitError> {
        let peer = self.peer();
        let name = peer.name.clone();
        let password = peer
            .password
            .clone()
            .ok_or_else(|| scenario("enrol needs setup with a password"))?;
        let group = self
            .group
            .as_ref()
            .ok_or_else(|| scenario("no group yet: register or join first"))?
            .clone();
        if self.second.is_some() {
            return Err(scenario("the driver already holds an enrolled device"));
        }
        let login = HttpDs::password_login(&self.ds_url, &name, &password)?;
        let assertion = self
            .runner
            .with_session(&name, |_, ds| ds.second_factor_assertion(login))?;
        let seed = self.seed.wrapping_add(0x1_0000_0000);
        let second = self.runner.with_member(&name, |first, _| {
            first.second_device(&format!("{name}-enrolled"), seed)
        })?;
        let (pending, established) = HttpDs::establish_with_login(
            &self.ds_url,
            &second.device(),
            &second.registration(),
            &assertion,
        )?;
        let _backups = pending.list_backups()?;
        let served = pending.get_device_list(&established.user_id)?;
        let prev = Self::served_list(&served.blob, served.version)?;
        let now = Self::now_unix();
        let next = self.runner.with_member(&name, |first, _| {
            let mut entries = prev.unsigned.entries.clone();
            entries.push(second.device_entry(now));
            Ok(first.sign_device_list_v(&prev, entries))
        })?;
        pending.put_device_list(&established.user_id, &next)?;
        let enrolled =
            HttpDs::establish_scoped(&self.ds_url, &second.device(), &second.credential_blob())?;
        if enrolled.scope != 0 {
            return Err(DsError::Protocol(format!(
                "the re-established session is scope {}, want 0",
                enrolled.scope
            ))
            .into());
        }
        let mut ds = HttpDs::with_session(&self.ds_url, second.device_id(), enrolled.token)?;
        ds.set_session_expires(enrolled.expires);
        let mut second = second;
        second.publish_key_packages(&mut ds, 8)?;
        let binding = self.binding(
            self.peer().channel,
            Some(self.peer().community),
            GroupKind::Text,
        )?;
        second.join_external(&mut ds, &group, &binding)?;
        let epoch = second
            .epoch_of(&group)
            .ok_or_else(|| scenario("the join left no group state"))?;
        let device_id = second.device_id().to_hex();
        self.second = Some(Second {
            client: second,
            ds,
            replaced: false,
        });
        Ok(
            json!({"device_id": device_id, "version": next.unsigned.version, "epoch": epoch, "scopes": [1, 0]}),
        )
    }

    fn revoke(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let target = Self::hex16(req, "device_id")?;
        let name = self.peer().name.clone();
        let next = self.runner.with_session(&name, |first, ds| {
            let user_id = *first.user_id().as_bytes();
            let served = ds.get_device_list(&user_id)?;
            let prev = Self::served_list(&served.blob, served.version)?;
            let mut entries = prev.unsigned.entries.clone();
            let entry = entries
                .iter_mut()
                .find(|e| e.device_id.as_bytes() == &target && e.revoked_at.is_none())
                .ok_or_else(|| {
                    scenario("device_id is not an unrevoked entry of the device list")
                })?;
            entry.revoked_at = Some(Self::now_unix());
            let next = first.sign_device_list_v(&prev, entries);
            ds.put_device_list(&user_id, &next)?;
            Ok(next)
        })?;
        if self
            .second
            .as_ref()
            .is_some_and(|s| s.client.device_id().as_bytes() == &target)
        {
            self.second = None;
            self.calls.retain(|(actor, _), _| *actor != Actor::Second);
        }
        Ok(json!({"version": next.unsigned.version}))
    }

    fn open_dm(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let user = Self::hex16(req, "user_id")?;
        let name = self.peer().name.clone();
        let (channel, created, info) = self.runner.with_session(&name, |_, ds| {
            let (channel, created) = ds.post_dm(&[user])?;
            let info = ds.get_channel(&channel)?;
            Ok((channel, created, info))
        })?;
        let group = match info.text_group_id {
            None => {
                let label = format!("dm-{}", hex::encode(channel));
                self.runner.exec_line(&format!(
                    "group {label} kind=text target={} community=none creator={name}",
                    hex::encode(channel)
                ))?;
                let group = self.runner.group_id(&label)?;
                self.dms.insert(channel, group.clone());
                group
            }
            Some(id) => self
                .dms
                .get(&channel)
                .filter(|g| g.as_slice() == id)
                .cloned()
                .ok_or_else(|| scenario("the DM's group exists: run dms to join it"))?,
        };
        let epoch = self.runner.with_member(&name, |c, _| {
            c.epoch_of(&group)
                .ok_or_else(|| scenario("the peer holds no state for its group"))
        })?;
        Ok(
            json!({"channel_id": hex::encode(channel), "group_id": hex::encode(group), "epoch": epoch, "created": created}),
        )
    }

    fn list_dms(&mut self) -> Result<Value, TestkitError> {
        let name = self.peer().name.clone();
        let list = self
            .runner
            .with_session(&name, |_, ds| Ok(ds.list_dms()?))?;
        let mut held = Vec::new();
        for dm in list {
            let info = self
                .runner
                .with_session(&name, |_, ds| Ok(ds.get_channel(&dm.channel_id)?))?;
            let Some(group_id) = info.text_group_id else {
                continue;
            };
            let group = group_id.to_vec();
            if !self.dms.contains_key(&dm.channel_id) {
                let binding = self.binding(dm.channel_id, None, GroupKind::Text)?;
                let joined = self.runner.with_member(&name, |c, ds| {
                    // A group the peer already holds is never joined a second time.
                    if c.epoch_of(&group).is_some() {
                        return Ok(true);
                    }
                    // Welcome-first (Q25): no external join; a Welcome for a group no listed DM
                    // names stays in the queue.
                    if ds.welcomes()?.iter().any(|w| w.group_id == group) {
                        c.join_welcome(ds, &group, &binding)?;
                        Ok(true)
                    } else {
                        Ok(false)
                    }
                })?;
                if joined {
                    self.dms.insert(dm.channel_id, group.clone());
                }
            }
            if self.dms.get(&dm.channel_id) == Some(&group) {
                let epoch = self.runner.with_member(&name, |c, _| {
                    c.epoch_of(&group)
                        .ok_or_else(|| scenario("the peer holds no state for its group"))
                })?;
                held.push(json!({"channel_id": hex::encode(dm.channel_id), "group_id": hex::encode(&group), "epoch": epoch}));
            }
        }
        held.sort_by(|a, b| a["channel_id"].as_str().cmp(&b["channel_id"].as_str()));
        Ok(json!({"dms": held}))
    }

    fn dm_group(&self, req: &Value) -> Result<Vec<u8>, TestkitError> {
        let channel = Self::hex16(req, "channel_id")?;
        self.dms
            .get(&channel)
            .cloned()
            .ok_or_else(|| scenario("no DM group for channel_id: open_dm or dms first"))
    }

    fn send_dm(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let group = self.dm_group(req)?;
        let name = self.peer().name.clone();
        let body = Self::text(req, "body")?;
        let thread_id = req
            .get("thread_id")
            .map(|_| Self::hex16(req, "thread_id").map(MsgId::from_bytes))
            .transpose()?;
        match thread_id {
            Some(root) => {
                let (seq, msg_id) = self.runner.with_member(&name, |c, ds| {
                    c.send_in_thread(ds, &group, root, None, body)
                })?;
                Ok(json!({"seq": seq, "msg_id": msg_id.to_hex()}))
            }
            None => {
                let seq = self
                    .runner
                    .with_member(&name, |c, ds| c.send_seq(ds, &group, body))?;
                Ok(json!({"seq": seq}))
            }
        }
    }

    fn sync_dm(&mut self, req: &Value) -> Result<Value, TestkitError> {
        self.sync_group(self.dm_group(req)?)
    }

    fn register(&mut self) -> Result<Value, TestkitError> {
        if self.registered_all {
            return Ok(
                json!({"group_id": hex::encode(self.group()), "epoch": self.runner.epoch_of(&self.peer().name, GROUP)?}),
            );
        }
        let peer = self.peer();
        let name = peer.name.clone();
        let channel = peer.channels[0];
        let line = format!(
            "group {GROUP} kind=text target={} community={} creator={name}",
            hex::encode(peer.channel),
            hex::encode(peer.community)
        );
        self.runner.exec_line(&line)?;
        let group = self.runner.group_id(GROUP)?;
        let epoch = self.runner.epoch_of(&name, GROUP)?;
        self.group = Some(group.clone());
        self.groups.insert(channel, group.clone());
        Ok(json!({"group_id": hex::encode(group), "epoch": epoch}))
    }

    fn join(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let community = Self::hex16(req, "community_id")?;
        let channel = Self::hex16(req, "channel_id")?;
        let group = Self::hex16(req, "group_id")?;
        let code = Self::text(req, "invite_code")?;
        let name = self.peer().name.clone();
        let doc = instance_document(&self.ds_url)?;
        let binding = DillaBinding {
            v: 1,
            instance_id: InstanceId::from_bytes(doc.instance_id),
            community_id: Some(CommunityId::from_bytes(community)),
            target_id: channel,
            kind: GroupKind::Text,
            policy_version: 1,
            e2ee_version: 1,
            media_version: GroupKind::Text.media_version(),
        };
        self.runner.with_session(&name, |_, ds| {
            ds.join_community(&community, code)?;
            Ok(())
        })?;
        let epoch = self.runner.with_member(&name, |client, ds| {
            client.join_external(ds, &group, &binding)?;
            client
                .epoch_of(&group)
                .ok_or_else(|| scenario("the join left no group state"))
        })?;
        self.group = Some(group.to_vec());
        self.groups.insert(channel, group.to_vec());
        self.joined.insert(channel, community);
        Ok(json!({"group_id": hex::encode(group), "epoch": epoch}))
    }

    fn send(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let name = self.peer().name.clone();
        let (channel, group) = self.channel_and_group(req)?;
        let body = req.get("body").and_then(Value::as_str).unwrap_or("");
        let kind = EnvelopeType::from_u64(req.get("type").and_then(Value::as_u64).unwrap_or(0))?;
        let reply_to = req
            .get("reply_to")
            .map(|_| Self::hex16(req, "reply_to").map(MsgId::from_bytes))
            .transpose()?;
        let thread_id = req
            .get("thread_id")
            .map(|_| Self::hex16(req, "thread_id").map(MsgId::from_bytes))
            .transpose()?;
        let (seq, msg_id) = self.runner.with_member(&name, |c, ds| match thread_id {
            Some(root) => c.send_in_thread(ds, &group, root, reply_to, body),
            None => c.send_envelope(ds, &group, kind, reply_to, body, Vec::new()),
        })?;
        self.sent.insert(
            (group, seq),
            Sent {
                msg_id,
                channel,
                attachments: Vec::new(),
            },
        );
        Ok(json!({"seq": seq, "msg_id": msg_id.to_hex()}))
    }

    fn search_seed(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let channel = Self::hex16(req, "channel")?;
        let group = self
            .groups
            .get(&channel)
            .ok_or_else(|| scenario("E_NOT_FOUND"))?
            .clone();
        let body = Self::text(req, "body")?;
        let name = self.peer().name.clone();
        let (seq, _) = self.runner.with_member(&name, |c, ds| {
            c.sync(ds)?;
            c.send_envelope(ds, &group, EnvelopeType::Message, None, body, Vec::new())
        })?;
        Ok(json!({"seq": seq}))
    }

    fn typing(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let channel = Self::hex16(req, "channel")?;
        let on = req["on"].as_bool().expect("validated on");
        let name = self.peer().name.clone();
        self.runner.with_session(&name, |_, ds| {
            ds.typing_set(&channel, on).map_err(Into::into)
        })?;
        Ok(json!({"accepted": true}))
    }

    /// The owner gives @everyone in the peer's server the `bits` (the roles tab's toggles, flow 09) through the real
    /// roles API: a plain member then holds them, as a member of a server whose owner did that would.
    fn allow_everyone(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let bits = req["bits"].as_u64().expect("validated bits");
        let community = self.peer().community;
        let name = self.peer().name.clone();
        let allow = self.runner.with_session(&name, |_, ds| {
            ds.allow_everyone(&community, bits).map_err(Into::into)
        })?;
        Ok(json!({"allow": allow}))
    }

    fn voice_self(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let call = Self::hex16(req, "call")?;
        let flags = req["flags"].as_u64().expect("validated flags");
        let name = self.peer().name.clone();
        self.runner.with_session(&name, |_, ds| {
            ds.voice_self(&call, flags).map_err(Into::into)
        })?;
        Ok(json!({"accepted": true}))
    }

    fn signed_history(
        ds: &HttpDs,
        user: &[u8; 16],
        cached: Option<&(u64, Vec<u8>)>,
    ) -> Result<(Vec<u8>, Vec<u8>), TestkitError> {
        let mut budget_bytes = 0usize;
        let mut budget_rows = 0usize;
        for attempt in 0..2 {
            let head = ds.get_device_list(user)?;
            if head.version == 0 {
                return Err(scenario("E_DEVICE_LIST_STALE"));
            }
            budget_bytes = budget_bytes.saturating_add(head.blob.len() + 128);
            if budget_bytes > 4 * 1024 * 1024 {
                return Err(scenario("E_DEVICE_LIST_STALE"));
            }
            let mut rows = Vec::<ServedDeviceList>::new();
            let mut total_bytes = 0usize;
            let mut version = 0;
            if let Some((v, blob)) = cached {
                if head.version < *v {
                    return Err(scenario("E_DEVICE_LIST_STALE"));
                }
                let list = Self::served_list(blob, *v)?;
                rows.push(ServedDeviceList {
                    version: *v,
                    blob: blob.clone(),
                    ssk_signature: list.sig_ssk.to_vec(),
                    prev_hash: list.unsigned.prev_hash.to_vec(),
                });
                total_bytes += blob.len() + 128;
                budget_bytes = budget_bytes.saturating_add(blob.len() + 128);
                budget_rows += 1;
                if budget_bytes > 4 * 1024 * 1024 {
                    return Err(scenario("E_DEVICE_LIST_STALE"));
                }
                version = *v;
                if version == head.version && rows[0].blob != head.blob {
                    return Err(scenario("E_DEVICE_LIST_STALE"));
                }
            }
            while version < head.version {
                let page = ds.get_device_list_history(user, version)?;
                if page.is_empty() || page.len() > 64 {
                    return Err(scenario("E_DEVICE_LIST_STALE"));
                }
                for row in page {
                    if row.version != version + 1 || row.version > head.version {
                        return Err(scenario("E_DEVICE_LIST_STALE"));
                    }
                    version = row.version;
                    total_bytes = total_bytes.saturating_add(
                        row.blob.len() + row.ssk_signature.len() + row.prev_hash.len() + 32,
                    );
                    budget_bytes = budget_bytes.saturating_add(
                        row.blob.len() + row.ssk_signature.len() + row.prev_hash.len() + 32,
                    );
                    budget_rows += 1;
                    if total_bytes > 4 * 1024 * 1024
                        || budget_bytes > 4 * 1024 * 1024
                        || budget_rows > 1024
                    {
                        return Err(scenario("E_DEVICE_LIST_STALE"));
                    }
                    rows.push(row);
                }
                if rows.len() > 1024 {
                    return Err(scenario("E_DEVICE_LIST_STALE"));
                }
            }
            if rows.last().is_none_or(|row| row != &head) {
                return Err(scenario("E_DEVICE_LIST_STALE"));
            }
            let again = ds.get_device_list(user)?;
            budget_bytes = budget_bytes.saturating_add(again.blob.len() + 128);
            if budget_bytes > 4 * 1024 * 1024 {
                return Err(scenario("E_DEVICE_LIST_STALE"));
            }
            if again != head {
                if attempt == 0 {
                    continue;
                }
                return Err(scenario("E_DEVICE_LIST_STALE"));
            }
            let mut e = Encoder::new();
            e.array(rows.len());
            for row in rows {
                e.array(4)
                    .uint(row.version)
                    .bytes(&row.blob)
                    .bytes(&row.ssk_signature)
                    .bytes(&row.prev_hash);
            }
            let history = e.into_vec();
            if history.len() > 4 * 1024 * 1024 {
                return Err(scenario("E_DEVICE_LIST_STALE"));
            }
            return Ok((history, head.blob));
        }
        Err(scenario("E_DEVICE_LIST_STALE"))
    }

    fn trust_list(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let user = Self::hex16(req, "user")?;
        let name = self.peer().name.clone();
        let doc = instance_document(&self.ds_url)?;
        self.runner.with_member(&name, |client, ds| {
            client.sync(ds)?;
            Ok(())
        })?;
        let (leaf, cached) = self.runner.with_member(&name, |client, _| {
            let core = client.trust_core(doc.instance_id)?;
            let pending = core.pending_leaves().map_err(|e| scenario(e.code))?;
            let leaf = decode_strict(&pending, |d: &mut Decoder<'_>| {
                let n = d.array_len()?;
                let mut found = None;
                for _ in 0..n {
                    d.array(5)?;
                    let group = d.bytes_exact::<16>()?;
                    let index = d.uint()?;
                    let candidate = d.bytes_exact::<16>()?;
                    d.uint()?;
                    d.text()?;
                    if candidate == user && found.is_none() {
                        found = Some((group, index as u32));
                    }
                }
                Ok(found)
            })
            .map_err(|_| scenario("E_DEVICE_LIST_STALE"))?
            .ok_or_else(|| scenario("E_CORE_NOT_FOUND"))?;
            Ok((leaf, client.cached_trust_head(&user)?))
        })?;
        let (history, head) = self.runner.with_session(&name, |_, ds| {
            Self::signed_history(ds, &user, cached.as_ref())
        })?;
        self.runner.with_member(&name, |client, _| {
            let mut core = client.trust_core(doc.instance_id)?;
            let result = core
                .device_list_ingest(&leaf.0, leaf.1, &user, &history, &head, Self::now_unix())
                .map_err(|e| scenario(e.code))?;
            let version = decode_strict(&result, |d: &mut Decoder<'_>| {
                d.array(3)?;
                let v = d.uint()?;
                d.uint()?;
                d.uint()?;
                Ok(v)
            })
            .map_err(|_| scenario("E_DEVICE_LIST_STALE"))?;
            let states = core.trust_state().map_err(|e| scenario(e.code))?;
            let (pin, verified, changed) = decode_strict(&states, |d: &mut Decoder<'_>| {
                let n = d.array_len()?;
                let mut found = None;
                for _ in 0..n {
                    d.array(6)?;
                    let candidate = d.bytes_exact::<16>()?;
                    let pin = d.bytes_exact::<32>()?;
                    d.uint()?;
                    let verified = d.uint()? == 1;
                    let changed = d.opt_bytes_exact::<32>()?;
                    d.skip()?;
                    if candidate == user {
                        found = Some((pin, verified, changed));
                    }
                }
                Ok(found)
            })
            .map_err(|_| scenario("E_DEVICE_LIST_STALE"))?
            .ok_or_else(|| scenario("E_DEVICE_LIST_STALE"))?;
            Ok(
                json!({"version": version, "pin": hex::encode(pin), "verified": verified,
                "changed": changed.map(hex::encode),
                "pendingUmk": hex::encode(changed.unwrap_or(pin))}),
            )
        })
    }

    fn rotate_umk(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let name = self.peer().name.clone();
        let group = self
            .group
            .clone()
            .ok_or_else(|| scenario("no group yet: register or join first"))?;
        let user = self
            .runner
            .with_member(&name, |client, _| Ok(*client.user_id().as_bytes()))?;
        if req.get("user").is_some() && Self::hex16(req, "user")? != user {
            return Err(scenario("E_NOT_FOUND"));
        }
        if self.second.is_some() {
            return Err(scenario("E_CORE_STATE"));
        }
        let password = format!("rotation-{:016x}-fixture", self.seed);
        self.runner.with_session(&name, |_, ds| {
            ds.set_password(&password)?;
            Ok(())
        })?;
        let login = HttpDs::password_login(&self.ds_url, &name, &password)?;
        let assertion = self
            .runner
            .with_session(&name, |_, ds| ds.second_factor_assertion(login))?;
        let seed = self.seed.wrapping_add(0x2_0000_0000);
        let rotated = self
            .runner
            .with_member(&name, |first, _| first.rotated_device(&name, seed))?;
        let (pending, _established) = HttpDs::establish_with_login(
            &self.ds_url,
            &rotated.device(),
            &rotated.registration(),
            &assertion,
        )?;
        let served = pending.get_device_list(&user)?;
        let prev = Self::served_list(&served.blob, served.version)?;
        let now = Self::now_unix();
        let next = self.runner.with_member(&name, |first, _| {
            let mut entries = prev.unsigned.entries.clone();
            entries.push(rotated.device_entry(now));
            Ok(first.sign_device_list_v(&prev, entries))
        })?;
        pending.put_device_list(&user, &next)?;
        let enrolled =
            HttpDs::establish_scoped(&self.ds_url, &rotated.device(), &rotated.credential_blob())?;
        if enrolled.scope != 0 {
            return Err(scenario("E_DEVICE_UNLISTED"));
        }
        let mut ds = HttpDs::with_session(&self.ds_url, rotated.device_id(), enrolled.token)?;
        ds.set_session_expires(enrolled.expires);
        // The replacement is the peer's session from now on, and the account's authenticator app goes with it.
        let totp = self
            .runner
            .with_session(&name, |_, old| Ok(old.totp().cloned()))?;
        ds.set_totp(totp);
        let mut rotated = rotated;
        rotated.publish_key_packages(&mut ds, 8)?;
        // The group's own channel and community: a joined group binds the other peer's channel, not this peer's.
        let (channel, community) = self
            .groups
            .iter()
            .find(|(_, g)| **g == group)
            .map(|(c, _)| {
                (
                    *c,
                    self.joined.get(c).copied().unwrap_or(self.peer().community),
                )
            })
            .unwrap_or((self.peer().channel, self.peer().community));
        let binding = self.binding(channel, Some(community), GroupKind::Text)?;
        rotated.join_external(&mut ds, &group, &binding)?;
        let old_device = self
            .runner
            .with_member(&name, |first, _| Ok(first.device_id()))?;
        rotated.remove(&mut ds, &group, old_device)?;
        self.retire_replaced_calls(&name)?;
        // The old leaf has left the text group: the replacement is the peer's acting device from now on (its sends,
        // syncs and trust witness), and the old device is held only so that a second rotation or enrolment stays
        // refused. `rotate_umk` refuses while a second device exists, so the second actor holds no call group here.
        let (old, old_ds) = self.runner.replace_member(&name, rotated, ds)?;
        self.replaced_devices.push(old.device_id());
        self.second = Some(Second {
            client: old,
            ds: old_ds,
            replaced: true,
        });
        Ok(json!({"user": hex::encode(user), "version": next.unsigned.version}))
    }

    fn retire_replaced_calls(&mut self, name: &str) -> Result<(), TestkitError> {
        // Keep the old call leaf until the replacement's `call_join` removes it. Posting an own Remove here
        // lets another member advance the epoch while the replacement builds its external join, turning that
        // join into E_COMMIT_INVALID. Rotation still forgets the old device's local call state immediately.
        let left: Vec<([u8; 16], Vec<u8>)> = self
            .calls
            .iter()
            .filter(|((actor, _), _)| *actor == Actor::Peer)
            .map(|((_, channel), g)| (*channel, g.clone()))
            .collect();
        for (channel, call_group) in left {
            self.runner
                .with_member(name, |c, _| c.forget(&call_group))?;
            self.calls.remove(&(Actor::Peer, channel));
        }
        Ok(())
    }

    /// `leave`: the acting device proposes its own removal from the group (protocol/01 "Leaving"), the member Remove a
    /// browser test needs for the validated-Remove recovery of a changed contact. Another member commits it.
    fn leave(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let name = self.peer().name.clone();
        let group = self.group_of(req)?;
        self.runner
            .with_member(&name, |client, ds| client.leave(ds, &group))?;
        Ok(json!({"group_id": hex::encode(group)}))
    }

    fn actor(req: &Value) -> Result<Actor, TestkitError> {
        match req.get("actor") {
            None => Ok(Actor::Peer),
            Some(v) => match v.as_str() {
                Some("peer") => Ok(Actor::Peer),
                Some("second") => Ok(Actor::Second),
                _ => Err(scenario("actor must be peer or second")),
            },
        }
    }

    fn second_actor(&mut self) -> Result<&mut Second, TestkitError> {
        self.second
            .as_mut()
            .filter(|s| !s.replaced)
            .ok_or_else(|| scenario("no second device: enrol first"))
    }

    /// Runs `f` with the actor's client and its own `/v1` session (the call routes are no `DeliveryService` call).
    fn session_of<T>(
        &mut self,
        actor: Actor,
        f: impl FnOnce(&TestClient, &mut HttpDs) -> Result<T, TestkitError>,
    ) -> Result<T, TestkitError> {
        match actor {
            Actor::Peer => {
                let name = self.peer().name.clone();
                self.runner.with_session(&name, f)
            }
            Actor::Second => {
                let second = self.second_actor()?;
                f(&second.client, &mut second.ds)
            }
        }
    }

    /// Runs `f` with the actor's client and its view of the delivery service, as a scenario statement does.
    fn member_of<T>(
        &mut self,
        actor: Actor,
        f: impl FnOnce(&mut TestClient, &mut dyn DeliveryService) -> Result<T, TestkitError>,
    ) -> Result<T, TestkitError> {
        match actor {
            Actor::Peer => {
                let name = self.peer().name.clone();
                self.runner.with_member(&name, f)
            }
            Actor::Second => {
                let second = self.second_actor()?;
                f(&mut second.client, &mut second.ds)
            }
        }
    }

    /// The id of the next call group this driver registers: unique per driver seed, actor, channel and count.
    fn next_call_group_id(&mut self, actor: Actor, channel: [u8; 16]) -> [u8; 16] {
        self.call_groups_made += 1;
        let mut hash = Sha256::new();
        hash.update(b"dilla-testkit call group");
        hash.update(self.seed.to_be_bytes());
        hash.update([actor as u8]);
        hash.update(channel);
        hash.update(self.call_groups_made.to_be_bytes());
        let digest: [u8; 32] = hash.finalize().into();
        let mut id = [0u8; 16];
        id.copy_from_slice(&digest[..16]);
        id
    }

    /// `call_join {actor, channel}` → `{group, leaf}` (L-E2E-30): reads the channel's call group from the instance
    /// (L-HTTP-100) and joins it by external commit, or registers a fresh one when the instance names none. A
    /// joiner that still holds state of that group (a rejoin) replaces its leaf, as a resync does.
    fn call_join(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let actor = Self::actor(req)?;
        let channel = Self::hex16(req, "channel")?;
        let (info, call) = self.session_of(actor, |_, ds| {
            Ok((ds.get_channel(&channel)?, ds.get_channel_call(&channel)?))
        })?;
        let doc = instance_document(&self.ds_url)?;
        let instance_id = InstanceId::from_bytes(doc.instance_id);
        let binding = Self::binding_of(instance_id, channel, info.community_id, GroupKind::Call);
        let group = match call.group_id {
            Some(group) => {
                let group = group.to_vec();
                let replaced = if actor == Actor::Peer {
                    self.replaced_devices.clone()
                } else {
                    Vec::new()
                };
                self.member_of(actor, |c, ds| {
                    if c.epoch_of(&group).is_some() {
                        c.resync(ds, &group, &binding)?;
                    } else {
                        c.join_external(ds, &group, &binding)?;
                    }
                    // Rotation forgets the replaced device's local call state without proposing a Remove.
                    // Its leaf stays until the replacement joins and commits this removal itself.
                    let stale: Vec<DeviceId> = c
                        .roster(&group)
                        .unwrap_or_default()
                        .iter()
                        .map(|e| e.device_id)
                        .filter(|d| replaced.contains(d))
                        .collect();
                    for device in stale {
                        c.remove(ds, &group, device)?;
                    }
                    Ok(())
                })?;
                group
            }
            None => {
                let id = self.next_call_group_id(actor, channel);
                let senders =
                    external_senders(doc.external_sender_pub.to_vec().into(), &instance_id);
                self.member_of(actor, |c, ds| {
                    c.create_group_as(ds, &id, binding.clone(), Some(senders))
                })?
            }
        };
        let leaf = self.member_of(actor, |c, _| {
            let own = c.device_id();
            c.roster(&group)
                .and_then(|r| r.iter().find(|e| e.device_id == own).map(|e| e.leaf_index))
                .ok_or_else(|| scenario("the join left no leaf of this device"))
        })?;
        self.calls.insert((actor, channel), group.clone());
        Ok(json!({"group": hex::encode(group), "leaf": leaf}))
    }

    fn call_group(&self, actor: Actor, channel: [u8; 16]) -> Result<Vec<u8>, TestkitError> {
        self.calls.get(&(actor, channel)).cloned().ok_or_else(|| {
            scenario("the actor holds no call group of this channel: call_join first")
        })
    }

    /// `call_leave {actor, channel}` → `{}`: the actor proposes its own Remove (`POST /v1/groups/{id}/proposal`, a
    /// `409 E_REMOVE_PENDING` being fine) and forgets the group, as the worker's `callLeave` does. Another member's
    /// commit applies the Remove.
    fn call_leave(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let actor = Self::actor(req)?;
        let channel = Self::hex16(req, "channel")?;
        let group = self.call_group(actor, channel)?;
        self.member_of(actor, |c, ds| {
            c.leave(ds, &group)?;
            c.forget(&group)
        })?;
        self.calls.remove(&(actor, channel));
        Ok(json!({}))
    }

    /// `call_members {actor, channel}` → `{group, devices}`: after a sync that also commits any outstanding
    /// proposal (a leaver's own Remove), the device ids of the actor's call-group roster, sorted.
    fn call_members(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let actor = Self::actor(req)?;
        let channel = Self::hex16(req, "channel")?;
        let group = self.call_group(actor, channel)?;
        let (got, deleted, roster) = self.member_of(actor, |c, ds| {
            let mut got = c.sync(ds)?;
            for attempt in 1..=3 {
                let outstanding = ds.proposals(&group)?.iter().any(|p| !p.void);
                if !outstanding {
                    break;
                }
                match c.commit(ds, &group) {
                    Ok(()) => break,
                    Err(TestkitError::Ds(DsError::CommitConflict { .. })) if attempt < 3 => {
                        got.extend(c.sync(ds)?)
                    }
                    Err(e) => return Err(e),
                }
            }
            got.extend(c.sync(ds)?);
            Ok((got, c.take_deleted(), c.roster(&group)))
        })?;
        // What the peer's sync took for its text groups stays for its `sync`.
        if actor == Actor::Peer {
            self.hold(&got);
            self.pending.extend(got);
            self.pending_deleted.extend(deleted);
        }
        let roster =
            roster.ok_or_else(|| scenario("the actor holds no state for its call group"))?;
        let mut devices: Vec<String> = roster.iter().map(|r| r.device_id.to_hex()).collect();
        devices.sort();
        Ok(json!({"group": hex::encode(group), "devices": devices}))
    }

    /// `call_read {actor, channel}` → `{group, call, participants}`: `GET /v1/channels/{id}/call` (L-HTTP-100) as the
    /// actor reads it — the channel's open call group and live call (each null without one) and who is in its room,
    /// each `{user, device, flags}`. Web-3 task 10's negative checks read who reached the room from here.
    fn call_read(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let actor = Self::actor(req)?;
        let channel = Self::hex16(req, "channel")?;
        let call = self.session_of(actor, |_, ds| Ok(ds.get_channel_call(&channel)?))?;
        let participants: Vec<Value> = call
            .participants
            .iter()
            .map(|(user, device, flags)| {
                json!({"user": hex::encode(user), "device": hex::encode(device), "flags": flags})
            })
            .collect();
        Ok(json!({
            "group": call.group_id.map(hex::encode),
            "call": call.call_id.map(hex::encode),
            "participants": participants,
        }))
    }

    /// `call_start {actor, channel}` → `{call, created}`: `POST /v1/channels/{id}/calls`. A start that opens a DM's
    /// call (`201`) rings its other participants; the peer never connects to the SFU.
    fn call_start(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let actor = Self::actor(req)?;
        let channel = Self::hex16(req, "channel")?;
        let started = self.session_of(actor, |_, ds| Ok(ds.start_call(&channel, "")?))?;
        Ok(json!({"call": hex::encode(started.call_id), "created": started.created}))
    }

    /// `call_decline {actor, call}` → `{}`: `POST /v1/calls/{id}/decline` with the `expires_ts` of the last op 53
    /// state 1 the actor's gateway received for that call, or 0 when none did (a no-op `204`, L-HTTP-104).
    fn call_decline(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let actor = Self::actor(req)?;
        let call = Self::hex16(req, "call")?;
        let rings = self.session_of(actor, |_, ds| Ok(ds.take_rings()?))?;
        for ring in rings.iter().filter(|r| r.state == 1) {
            self.ringing.insert((actor, ring.call), ring.expires);
        }
        let expires = self.ringing.get(&(actor, call)).copied().unwrap_or(0);
        self.session_of(actor, |_, ds| Ok(ds.decline_call(&call, expires)?))?;
        Ok(json!({}))
    }

    /// `forge_device` → `{device_id, version}` (task 10's forged-list case): a second device of the peer, enrolled as
    /// `enrol` enrols one, whose signed-list entry names it with its id and DSK but at tier Native while its MLS
    /// credential says Browser. The instance checks a joiner's id, key and listing (`checkExternalJoiner`), not its
    /// tier, so it admits the device's leaf; a validating client fails it (`check_leaf`, `E_TIER_MISMATCH`). The
    /// device acts as `actor: "second"`.
    fn forge_device(&mut self) -> Result<Value, TestkitError> {
        let name = self.peer().name.clone();
        let password = self
            .peer()
            .password
            .clone()
            .ok_or_else(|| scenario("forge_device needs setup with a password"))?;
        if self.second.is_some() {
            return Err(scenario("the driver already holds a second device"));
        }
        let login = HttpDs::password_login(&self.ds_url, &name, &password)?;
        let assertion = self
            .runner
            .with_session(&name, |_, ds| ds.second_factor_assertion(login))?;
        let seed = self.seed.wrapping_add(0x3_0000_0000);
        let forged = self.runner.with_member(&name, |first, _| {
            first.second_device(&format!("{name}-forged"), seed)
        })?;
        let (pending, established) = HttpDs::establish_with_login(
            &self.ds_url,
            &forged.device(),
            &forged.registration(),
            &assertion,
        )?;
        let served = pending.get_device_list(&established.user_id)?;
        let prev = Self::served_list(&served.blob, served.version)?;
        let now = Self::now_unix();
        let next = self.runner.with_member(&name, |first, _| {
            let mut entries = prev.unsigned.entries.clone();
            let mut entry = forged.device_entry(now);
            // The forgery: the list's tier for this device is not the tier its credential carries.
            entry.tier = match entry.tier {
                Tier::Browser => Tier::Native,
                Tier::Native => Tier::Browser,
            };
            entries.push(entry);
            Ok(first.sign_device_list_v(&prev, entries))
        })?;
        pending.put_device_list(&established.user_id, &next)?;
        let enrolled =
            HttpDs::establish_scoped(&self.ds_url, &forged.device(), &forged.credential_blob())?;
        if enrolled.scope != 0 {
            return Err(scenario("E_DEVICE_UNLISTED"));
        }
        let mut ds = HttpDs::with_session(&self.ds_url, forged.device_id(), enrolled.token)?;
        ds.set_session_expires(enrolled.expires);
        let mut forged = forged;
        forged.publish_key_packages(&mut ds, 8)?;
        let device_id = forged.device_id().to_hex();
        self.second = Some(Second {
            client: forged,
            ds,
            replaced: false,
        });
        Ok(json!({"device_id": device_id, "version": next.unsigned.version}))
    }

    /// `call_check {actor, channel}` → `{code, leaves}`: what a validating client decides about the actor's call
    /// group. After a sync, every pending leaf of the group gets the signed-list refresh a browser runs
    /// (`device_list_ingest` over the signed history), and the answer is the group's `send_block_code` (null when
    /// nothing blocks) with each refreshed leaf's own outcome (`"ok"` or its code).
    fn call_check(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let actor = Self::actor(req)?;
        let channel = Self::hex16(req, "channel")?;
        let group = self.call_group(actor, channel)?;
        let group16: [u8; 16] = group
            .as_slice()
            .try_into()
            .map_err(|_| scenario("the call group id is not 16 bytes"))?;
        let doc = instance_document(&self.ds_url)?;
        let got = self.member_of(actor, |c, ds| c.sync(ds))?;
        if actor == Actor::Peer {
            self.hold(&got);
            self.pending.extend(got);
        }
        let pending = self.member_of(actor, |c, _| {
            let core = c.trust_core(doc.instance_id)?;
            let pending = core.pending_leaves().map_err(|e| scenario(e.code))?;
            decode_strict(&pending, |d: &mut Decoder<'_>| {
                let n = d.array_len()?;
                let mut out = Vec::new();
                for _ in 0..n {
                    d.array(5)?;
                    let g = d.bytes_exact::<16>()?;
                    let index = d.uint()?;
                    let user = d.bytes_exact::<16>()?;
                    d.uint()?;
                    d.text()?;
                    if g == group16 {
                        out.push((index as u32, user));
                    }
                }
                Ok(out)
            })
            .map_err(|_| scenario("E_DEVICE_LIST_STALE"))
        })?;
        let mut leaves = Vec::new();
        for (index, user) in pending {
            let cached = self.member_of(actor, |c, _| c.cached_trust_head(&user))?;
            let (history, head) = self.session_of(actor, |_, ds| {
                Self::signed_history(ds, &user, cached.as_ref())
            })?;
            let outcome = self.member_of(actor, |c, _| {
                let mut core = c.trust_core(doc.instance_id)?;
                Ok(
                    match core.device_list_ingest(
                        &group16,
                        index,
                        &user,
                        &history,
                        &head,
                        Self::now_unix(),
                    ) {
                        Ok(_) => "ok".to_owned(),
                        Err(e) => e.code.to_owned(),
                    },
                )
            })?;
            leaves.push(json!({"leaf": index, "user": hex::encode(user), "outcome": outcome}));
        }
        let code = self.member_of(actor, |c, _| {
            let core = c.trust_core(doc.instance_id)?;
            core.send_block_code(&group16).map_err(|e| scenario(e.code))
        })?;
        Ok(json!({"code": code, "leaves": leaves}))
    }

    fn attachment_material(seed: u64, counter: u64) -> ([u8; 32], [u8; 12]) {
        let mut key_hash = Sha256::new();
        key_hash.update(b"dilla-testkit attachment key");
        key_hash.update(seed.to_be_bytes());
        key_hash.update(counter.to_be_bytes());
        let key: [u8; 32] = key_hash.finalize().into();
        let mut nonce_hash = Sha256::new();
        nonce_hash.update(b"dilla-testkit attachment nonce");
        nonce_hash.update(seed.to_be_bytes());
        nonce_hash.update(counter.to_be_bytes());
        let hash: [u8; 32] = nonce_hash.finalize().into();
        let mut nonce = [0u8; 12];
        nonce.copy_from_slice(&hash[..12]);
        (key, nonce)
    }

    fn delete(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let msg_id = MsgId::from_bytes(Self::hex16(req, "msg_id")?);
        let seq = req["seq"].as_u64().expect("validated seq");
        let (_, group) = self.channel_and_group(req)?;
        let sent = self
            .sent
            .get(&(group.clone(), seq))
            .ok_or_else(|| scenario("seq is not a message this driver sent"))?;
        if sent.msg_id != msg_id {
            return Err(scenario("msg_id is not the message at seq"));
        }
        let channel = sent.channel;
        let blob_ids: Vec<[u8; 32]> = sent.attachments.iter().map(|a| a.blob_id).collect();
        let name = self.peer().name.clone();
        let (deleted_seq, _) = self.runner.with_member(&name, |c, ds| {
            c.send_envelope(
                ds,
                &group,
                EnvelopeType::Delete,
                Some(msg_id),
                "",
                Vec::new(),
            )
        })?;
        self.runner.with_session(&name, |_, ds| {
            ds.delete_message(&group, seq).map_err(Into::into)
        })?;
        for blob_id in blob_ids {
            self.runner.with_session(&name, |_, ds| {
                ds.delete_blob(&channel, &blob_id).map_err(Into::into)
            })?;
        }
        Ok(json!({"seq": deleted_seq}))
    }

    fn attach(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let (channel, group) = self.channel_and_group(req)?;
        let plaintext = if let Some(s) = req.get("bytes_hex") {
            Self::hex_bytes(s.as_str().expect("validated hex"), 1_048_576).expect("validated hex")
        } else {
            seeded_plaintext(
                req["seed"].as_u64().expect("validated seed"),
                req["size"].as_u64().expect("validated size") as usize,
            )
        };
        self.attachments_made += 1;
        let (key, nonce) = Self::attachment_material(self.seed, self.attachments_made);
        let stored = attachment::seal_blob(&key, &nonce, &plaintext);
        let blob_id = attachment::blob_id(&stored);
        let name = self.peer().name.clone();
        self.runner.with_session(&name, |_, ds| {
            ds.put_blob(&channel, &blob_id, &stored)?;
            ds.confirm_blob(&channel, &blob_id)?;
            Ok(())
        })?;
        let thumb = req
            .get("thumb_hex")
            .map(|s| {
                let plain = Self::hex_bytes(s.as_str().expect("validated thumb"), 8_176)
                    .expect("validated thumb");
                attachment::seal_thumb(&key, &nonce, &plain)
            })
            .transpose()?;
        let att = Attachment {
            blob_id,
            key,
            nonce,
            size: plaintext.len() as u64,
            mime: Self::text(req, "mime")?.to_owned(),
            w: req.get("w").and_then(Value::as_u64),
            h: req.get("h").and_then(Value::as_u64),
            thumb,
            name: Self::text(req, "name")?.to_owned(),
        };
        let body = req.get("body").and_then(Value::as_str).unwrap_or("");
        let (seq, msg_id) = self.runner.with_member(&name, |c, ds| {
            c.send_envelope(
                ds,
                &group,
                EnvelopeType::Message,
                None,
                body,
                vec![att.clone()],
            )
        })?;
        self.sent.insert(
            (group, seq),
            Sent {
                msg_id,
                channel,
                attachments: vec![att],
            },
        );
        Ok(
            json!({"seq": seq, "msg_id": msg_id.to_hex(), "blob_id": hex::encode(blob_id),
            "sha256": hex::encode(Sha256::digest(&plaintext))}),
        )
    }

    fn fetch_attachment(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let (channel, group) = self.channel_and_group(req)?;
        let seq = req["seq"].as_u64().expect("validated seq");
        let index = req["index"].as_u64().expect("validated index") as usize;
        let key = (group, seq);
        let attachments = if let Some(sent) = self.sent.get(&key) {
            &sent.attachments
        } else if let Some(row) = self.held.get(&key) {
            &row.envelope.attachments
        } else {
            return Err(scenario("seq is not a message this driver holds"));
        };
        if self.deleted.contains(&key) {
            return Err(scenario("the message at seq was deleted"));
        }
        let att = attachments
            .get(index)
            .ok_or_else(|| scenario("index is past the attachments of the message at seq"))?;
        let name = self.peer().name.clone();
        let stored = self.runner.with_session(&name, |_, ds| {
            ds.get_blob(&channel, &att.blob_id).map_err(Into::into)
        })?;
        let plain = attachment::open_blob(&att.key, &att.nonce, &att.blob_id, att.size, &stored)
            .map_err(|e| scenario(e.code()))?;
        let thumb_sha256 = att
            .thumb
            .as_ref()
            .map(|bytes| {
                attachment::open_thumb(&att.key, &att.nonce, bytes)
                    .map(|plain| hex::encode(Sha256::digest(&plain)))
                    .map_err(|e| scenario(e.code()))
            })
            .transpose()?;
        Ok(
            json!({"sha256": hex::encode(Sha256::digest(&plain)), "size": att.size,
            "mime": att.mime, "name": att.name, "thumb_sha256": thumb_sha256}),
        )
    }

    fn blob_status(&mut self, req: &Value) -> Result<Value, TestkitError> {
        let (channel, _) = self.channel_and_group(req)?;
        let id = Self::digest32(req, "blob_id")?;
        let name = self.peer().name.clone();
        let status = self.runner.with_session(&name, |_, ds| {
            ds.blob_status(&channel, &id).map_err(Into::into)
        })?;
        Ok(json!({"status": status}))
    }

    fn received_json(r: &Received, deleted: bool) -> Value {
        let attachments: Vec<Value> = if deleted {
            Vec::new()
        } else {
            r.envelope
                .attachments
                .iter()
                .enumerate()
                .map(|(index, a)| {
                    json!({
                        "index": index, "blob_id": hex::encode(a.blob_id), "size": a.size,
                        "mime": a.mime, "name": a.name, "thumb": a.thumb.is_some()
                    })
                })
                .collect()
        };
        json!({"seq": r.seq, "body": if deleted { "" } else { &r.envelope.body },
            "sender_user": r.sender_user.to_hex(), "sender_device": r.sender.to_hex(),
            "tier": r.tier as u8, "msg_id": r.envelope.msg_id.to_hex(),
            "type": r.envelope.kind.as_u8(), "reply_to": r.envelope.reply_to.map(|id| id.to_hex()),
            "deleted": deleted, "attachments": attachments})
    }

    fn unheld_deleted_json(seq: u64) -> Value {
        json!({"seq": seq, "body": "", "deleted": true, "msg_id": null, "type": null,
            "reply_to": null, "sender_user": null, "sender_device": null, "tier": null,
            "attachments": []})
    }

    fn hold(&mut self, got: &[Received]) {
        for row in got {
            self.held
                .insert((row.group_id.clone(), row.seq), row.clone());
        }
    }

    fn sync_group(&mut self, group: Vec<u8>) -> Result<Value, TestkitError> {
        let name = self.peer().name.clone();
        let (mut got, deleted, epoch, members) = self.runner.with_member(&name, |c, ds| {
            let mut got = c.sync(ds)?;
            for attempt in 1..=3 {
                let outstanding = ds.proposals(&group)?.iter().any(|p| !p.void);
                if !outstanding {
                    break;
                }
                match c.commit(ds, &group) {
                    Ok(()) => break,
                    Err(TestkitError::Ds(DsError::CommitConflict { .. })) if attempt < 3 => {
                        got.extend(c.sync(ds)?)
                    }
                    Err(e) => return Err(e),
                }
            }
            got.extend(c.sync(ds)?);
            Ok((
                got,
                c.take_deleted(),
                c.epoch_of(&group),
                c.member_count(&group),
            ))
        })?;
        let epoch = epoch.ok_or_else(|| scenario("the peer holds no state for its group"))?;
        let members = members.ok_or_else(|| scenario("the peer holds no state for its group"))?;
        self.hold(&got);
        self.pending.append(&mut got);
        self.pending_deleted.extend(deleted);
        let mut received: Vec<Value> = self
            .take_pending(&group)
            .iter()
            .map(|r| Self::received_json(r, false))
            .collect();
        let mut other = Vec::new();
        for (g, seq) in std::mem::take(&mut self.pending_deleted) {
            if g != group {
                other.push((g, seq));
                continue;
            }
            if !self.deleted.insert((g.clone(), seq)) {
                continue;
            }
            received.push(self.held.get(&(g, seq)).map_or_else(
                || Self::unheld_deleted_json(seq),
                |r| Self::received_json(r, true),
            ));
        }
        self.pending_deleted = other;
        Ok(json!({"epoch": epoch, "members": members, "received": received}))
    }

    fn members(&mut self, group: Vec<u8>) -> Result<Value, TestkitError> {
        let name = self.peer().name.clone();
        let (got, deleted, roster) = self.runner.with_member(&name, |c, ds| {
            let got = c.sync(ds)?;
            Ok((got, c.take_deleted(), c.roster(&group)))
        })?;
        self.hold(&got);
        self.pending.extend(got);
        self.pending_deleted.extend(deleted);
        let roster = roster.ok_or_else(|| scenario("the peer holds no state for its group"))?;
        let devices: Vec<String> = roster.iter().map(|r| r.device_id.to_hex()).collect();
        Ok(json!({"devices": devices}))
    }

    fn update(&mut self) -> Result<Value, TestkitError> {
        let name = self.peer().name.clone();
        let group = self.group();
        let (got, deleted, epoch) = self.runner.with_member(&name, |c, ds| {
            let got = c.sync(ds)?;
            c.commit(ds, &group)?;
            Ok((got, c.take_deleted(), c.epoch_of(&group)))
        })?;
        self.hold(&got);
        self.pending.extend(got);
        self.pending_deleted.extend(deleted);
        let epoch = epoch.ok_or_else(|| scenario("the peer holds no state for its group"))?;
        Ok(json!({"epoch": epoch}))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_core::envelope::{Attachment, Envelope, EnvelopeType};
    use dilla_core::identity::{DeviceListUnsigned, SskSigner, Tier};
    use dilla_core::ids::{DeviceId, MsgId, UserId};
    use serde_json::json;
    use sha2::{Digest, Sha256};

    #[test]
    fn rotation_keeps_the_old_call_leaf_until_the_replacement_joins_and_removes_it() {
        let mut d = driver(1);
        d.runner = Runner::new(1);
        for line in [
            "instance dilla",
            "client peer",
            "client alice",
            "client bob",
            "group call kind=call target=33333333333333333333333333333333 creator=peer",
            "join alice call via=external",
            "join bob call via=external",
            "sync peer",
            "sync alice",
            "sync bob",
        ] {
            d.runner.exec_line(line).expect("real MLS call fixture");
        }
        let group = d.runner.group_id("call").unwrap();
        let channel = [0x33; 16];
        d.calls.insert((Actor::Peer, channel), group.clone());
        let (old_device, mut replacement, before) = d
            .runner
            .with_member("peer", |c, ds| {
                Ok((
                    c.device_id(),
                    c.rotated_device("peer", 99)?,
                    ds.handshakes(&group, 0)?.len(),
                ))
            })
            .unwrap();
        d.retire_replaced_calls("peer").unwrap();
        assert!(!d.calls.contains_key(&(Actor::Peer, channel)));
        d.runner
            .with_member("peer", |old, ds| {
                assert!(
                    old.epoch_of(&group).is_none(),
                    "the replaced device forgets its local call state"
                );
                let after = ds.handshakes(&group, 0)?;
                assert_eq!(
                    after.len(),
                    before,
                    "rotation must not post a Remove that races the replacement's external join"
                );
                let binding = DillaBinding {
                    v: 1,
                    instance_id: InstanceId::from_bytes([0x11; 16]),
                    community_id: None,
                    target_id: channel,
                    kind: GroupKind::Call,
                    policy_version: 1,
                    e2ee_version: 1,
                    media_version: 1,
                };
                replacement.join_external(ds, &group, &binding)?;
                let joined = replacement.roster(&group).unwrap();
                assert_eq!(
                    joined.len(),
                    4,
                    "the old leaf remains through the replacement join"
                );
                assert!(joined.iter().any(|leaf| leaf.device_id == old_device));
                replacement.remove(ds, &group, old_device)?;
                Ok(())
            })
            .unwrap();
        let replacement_device = replacement.device_id();
        for name in ["alice", "bob"] {
            d.runner
                .with_member(name, |c, ds| {
                    c.sync(ds)?;
                    let roster = c.roster(&group).unwrap();
                    assert_eq!(
                        roster.len(),
                        3,
                        "both original participants stay in the call"
                    );
                    assert!(roster.iter().any(|leaf| leaf.device_id == c.device_id()));
                    assert!(
                        roster
                            .iter()
                            .any(|leaf| leaf.device_id == replacement_device)
                    );
                    assert!(!roster.iter().any(|leaf| leaf.device_id == old_device));
                    Ok(())
                })
                .unwrap();
        }
    }

    #[test]
    fn signed_history_accepts_a_cached_1024_anchor_and_signed_1025_head() {
        use std::io::{Read, Write};
        use std::net::TcpListener;
        use std::time::Instant;

        let signer = SskSigner::from_bytes(&[0x42; 32]);
        let user = [0x43; 16];
        let anchor_unsigned = DeviceListUnsigned {
            v: 1,
            user_id: UserId::from_bytes(user),
            version: 1024,
            prev_hash: [0x44; 32],
            entries: Vec::new(),
        };
        let anchor = DeviceList {
            sig_ssk: signer.sign_device_list(&anchor_unsigned),
            unsigned: anchor_unsigned,
        };
        let head_unsigned = DeviceListUnsigned {
            v: 1,
            user_id: UserId::from_bytes(user),
            version: 1025,
            prev_hash: anchor.hash(),
            entries: Vec::new(),
        };
        let head = DeviceList {
            sig_ssk: signer.sign_device_list(&head_unsigned),
            unsigned: head_unsigned,
        };
        let served = ServedDeviceList {
            version: 1025,
            blob: head.encode(),
            ssk_signature: head.sig_ssk.to_vec(),
            prev_hash: head.unsigned.prev_hash.to_vec(),
        };
        let head_body = {
            let mut e = Encoder::new();
            e.array(4)
                .uint(served.version)
                .bytes(&served.blob)
                .bytes(&served.ssk_signature)
                .bytes(&served.prev_hash);
            e.into_vec()
        };
        let history_body = {
            let mut e = Encoder::new();
            e.array(1)
                .array(4)
                .uint(served.version)
                .bytes(&served.blob)
                .bytes(&served.ssk_signature)
                .bytes(&served.prev_hash);
            e.into_vec()
        };
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let base = format!("http://{}", listener.local_addr().unwrap());
        let server = std::thread::spawn(move || {
            let deadline = Instant::now() + Duration::from_secs(3);
            let mut paths = Vec::new();
            while paths.len() < 3 && Instant::now() < deadline {
                let Ok((mut stream, _)) = listener.accept() else {
                    std::thread::sleep(Duration::from_millis(5));
                    continue;
                };
                stream
                    .set_read_timeout(Some(Duration::from_secs(1)))
                    .unwrap();
                let mut request = Vec::new();
                let mut buf = [0u8; 1024];
                while !request.ends_with(b"\r\n\r\n") {
                    let n = stream.read(&mut buf).unwrap();
                    assert!(n > 0, "HTTP request closed before headers");
                    request.extend_from_slice(&buf[..n]);
                    assert!(request.len() < 8192);
                }
                let line = String::from_utf8_lossy(&request);
                let path = line.split_whitespace().nth(1).unwrap().to_owned();
                let body = if path.contains("?after=1024") {
                    &history_body
                } else {
                    &head_body
                };
                let response = format!(
                    "HTTP/1.1 200 OK\r\nContent-Type: application/cbor\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                stream.write_all(response.as_bytes()).unwrap();
                stream.write_all(body).unwrap();
                paths.push(path);
            }
            paths
        });
        let ds = HttpDs::for_device_list_test(&base);
        let result = WebDriver::signed_history(&ds, &user, Some(&(1024, anchor.encode())));
        let paths = server.join().unwrap();
        let (history, returned_head) = result.expect("one signed row above the cached anchor");
        assert_eq!(returned_head, head.encode());
        assert_eq!(paths.len(), 3, "head, history, and head recheck: {paths:?}");
        assert!(paths[1].ends_with("?after=1024"), "{paths:?}");
        let lists = decode_strict(&history, |d: &mut Decoder<'_>| {
            let n = d.array_len()?;
            let mut lists = Vec::new();
            for _ in 0..n {
                d.array(4)?;
                let version = d.uint()?;
                let blob = d.bytes()?.to_vec();
                let signature = d.bytes()?.to_vec();
                let prev_hash = d.bytes()?.to_vec();
                let list = DeviceList::decode(&blob).unwrap();
                assert_eq!(version, list.unsigned.version);
                assert_eq!(signature, list.sig_ssk);
                assert_eq!(prev_hash, list.unsigned.prev_hash);
                lists.push(list);
            }
            Ok(lists)
        })
        .unwrap();
        assert_eq!(lists.len(), 2);
        assert_eq!(lists[0], anchor);
        lists[0].verify(&signer.public()).unwrap();
        lists[1].accept(Some(&lists[0]), &signer.public()).unwrap();
    }

    #[test]
    fn new_web_driver_verbs_reject_unbounded_or_wrong_shape_json() {
        use serde_json::json;
        for (op, input) in [
            ("typing", json!({"channel": "ab".repeat(16), "on": 1})),
            ("typing", json!({"channel": "ab".repeat(17), "on": true})),
            (
                "search_seed",
                json!({"channel": "ab".repeat(16), "body": "x".repeat(4001)}),
            ),
            ("voice_self", json!({"call": "ab".repeat(16), "flags": 4})),
            ("rotate_umk", json!({"user": "AB".repeat(16)})),
            ("leave", json!({"channel_id": "ab".repeat(17)})),
            ("leave", json!({"channel_id": 7})),
            ("allow_everyone", json!({})),
            ("allow_everyone", json!({"bits": 0})),
            ("allow_everyone", json!({"bits": 0x100_0000})),
            ("allow_everyone", json!({"bits": "8"})),
        ] {
            assert!(WebDriver::validate(op, &input).is_err(), "{op}: {input}");
        }
        assert!(
            WebDriver::validate("typing", &json!({"channel": "ab".repeat(16), "on": true})).is_ok()
        );
        assert!(WebDriver::validate("leave", &json!({})).is_ok());
        assert!(WebDriver::validate("allow_everyone", &json!({"bits": 8})).is_ok());
        assert!(WebDriver::validate("leave", &json!({"channel_id": "ab".repeat(16)})).is_ok());
        assert!(
            WebDriver::validate(
                "search_seed",
                &json!({"channel": "ab".repeat(16), "body": "needle"})
            )
            .is_ok()
        );
    }

    #[test]
    fn the_call_verbs_validate_their_fields_before_the_setup_check() {
        let mut d = driver(1);
        let channel = "ab".repeat(16);
        let cases = [
            (
                json!({"id": 1, "op": "call_join", "actor": "peer"}),
                "scenario: the request carries no channel",
            ),
            (
                json!({"id": 2, "op": "call_join", "actor": "third", "channel": channel}),
                "scenario: actor must be peer or second",
            ),
            (
                json!({"id": 3, "op": "call_leave", "actor": 2, "channel": channel}),
                "scenario: actor must be peer or second",
            ),
            (
                json!({"id": 4, "op": "call_members", "actor": "second", "channel": "AB".repeat(16)}),
                "scenario: channel is not 32 lowercase hex",
            ),
            (
                json!({"id": 5, "op": "call_start", "channel": "ab".repeat(17)}),
                "scenario: channel is not 32 lowercase hex",
            ),
            (
                json!({"id": 6, "op": "call_decline", "actor": "peer"}),
                "scenario: the request carries no call",
            ),
            (
                json!({"id": 7, "op": "setup", "community": "c", "channel": "general", "voice": 1}),
                "scenario: voice must be boolean",
            ),
            // Well-formed requests reach the setup check; an absent actor is the peer.
            (
                json!({"id": 8, "op": "call_join", "actor": "second", "channel": channel}),
                "scenario: setup first",
            ),
            (
                json!({"id": 9, "op": "call_start", "channel": channel}),
                "scenario: setup first",
            ),
            (
                json!({"id": 10, "op": "call_decline", "actor": "peer", "call": channel}),
                "scenario: setup first",
            ),
            (
                json!({"id": 11, "op": "call_check", "actor": "peer"}),
                "scenario: the request carries no channel",
            ),
            (
                json!({"id": 12, "op": "call_check", "actor": "x", "channel": channel}),
                "scenario: actor must be peer or second",
            ),
            (
                json!({"id": 13, "op": "call_check", "channel": channel}),
                "scenario: setup first",
            ),
            (
                json!({"id": 14, "op": "forge_device"}),
                "scenario: setup first",
            ),
            (
                json!({"id": 15, "op": "call_read", "actor": "third", "channel": channel}),
                "scenario: actor must be peer or second",
            ),
            (
                json!({"id": 16, "op": "call_read", "channel": "AB".repeat(16)}),
                "scenario: channel is not 32 lowercase hex",
            ),
            (
                json!({"id": 17, "op": "call_read", "channel": channel}),
                "scenario: setup first",
            ),
        ];
        for (req, want) in cases {
            assert_eq!(error_of(&d.handle(&req)), want, "{req}");
        }
    }

    fn driver(seed: u64) -> WebDriver {
        // Port 9 (discard): nothing below may reach the network.
        WebDriver::new("http://127.0.0.1:9".into(), seed)
    }

    fn error_of(answer: &serde_json::Value) -> &str {
        assert_eq!(answer["ok"], json!(false), "{answer}");
        answer["error"].as_str().expect("an error string")
    }

    #[test]
    fn an_unknown_op_is_an_error_line_that_carries_the_request_id() {
        let mut d = driver(1);
        assert_eq!(
            d.handle(&json!({"id": 7, "op": "nope"})),
            json!({"id": 7, "ok": false, "error": "scenario: unknown op nope"})
        );
    }

    #[test]
    fn an_oversized_request_is_refused_and_the_next_line_still_runs() {
        let mut d = driver(1);
        let mut input = format!(
            "{{\"id\":1,\"op\":\"nope\"{}}}\n",
            " ".repeat(4 * 1024 * 1024)
        );
        input.push_str("{\"id\":2,\"op\":\"nope\"}\n");
        let mut out = Vec::new();
        d.serve(std::io::Cursor::new(input), &mut out).unwrap();
        let lines: Vec<Value> = String::from_utf8(out)
            .unwrap()
            .lines()
            .map(|s| serde_json::from_str(s).unwrap())
            .collect();
        assert_eq!(
            lines[0],
            json!({"id": null, "ok": false, "error": "request exceeds 4 MiB"})
        );
        assert_eq!(
            lines[1],
            json!({"id": 2, "ok": false, "error": "scenario: unknown op nope"})
        );
    }

    #[test]
    fn every_op_but_setup_is_refused_before_setup() {
        let mut d = driver(1);
        let requests = [
            json!({"id": 1, "op": "register"}),
            json!({"id": 2, "op": "send", "body": "hi"}),
            json!({"id": 3, "op": "sync"}),
            json!({"id": 4, "op": "members"}),
            json!({"id": 5, "op": "update"}),
            json!({"id": 6, "op": "join", "community_id": "11".repeat(16),
                   "channel_id": "22".repeat(16), "group_id": "33".repeat(16), "invite_code": "abc"}),
        ];
        for (i, req) in requests.iter().enumerate() {
            assert_eq!(
                d.handle(req),
                json!({"id": i + 1, "ok": false, "error": "scenario: setup first"}),
                "{req}"
            );
        }
    }

    #[test]
    fn setup_validates_its_names_before_it_touches_the_instance() {
        let mut d = driver(1);
        let cases = [
            (
                json!({"id": 1, "op": "setup", "channel": "general"}),
                "scenario: the request carries no community",
            ),
            (
                json!({"id": 2, "op": "setup", "community": "c"}),
                "scenario: the request carries no channel",
            ),
            (
                json!({"id": 3, "op": "setup", "community": "", "channel": "general"}),
                "scenario: community must be 1..=255 bytes",
            ),
            (
                json!({"id": 4, "op": "setup", "community": "c".repeat(256), "channel": "general"}),
                "scenario: community must be 1..=255 bytes",
            ),
            (
                json!({"id": 5, "op": "setup", "community": "c", "channel": ""}),
                "scenario: channel must be 1..=100 bytes",
            ),
            (
                json!({"id": 6, "op": "setup", "community": "c", "channel": "x".repeat(101)}),
                "scenario: channel must be 1..=100 bytes",
            ),
        ];
        for (req, want) in cases {
            assert_eq!(error_of(&d.handle(&req)), want, "{req}");
        }
    }

    #[test]
    fn join_and_send_validate_their_fields_before_the_setup_check() {
        let mut d = driver(1);
        let good = |k: &str, v: &str| {
            let mut req = json!({"id": 1, "op": "join", "community_id": "11".repeat(16),
                "channel_id": "22".repeat(16), "group_id": "33".repeat(16), "invite_code": "abc"});
            req[k] = json!(v);
            req
        };
        assert_eq!(
            error_of(&d.handle(&good("community_id", "XYZ"))),
            "scenario: community_id is not 32 lowercase hex"
        );
        assert_eq!(
            error_of(&d.handle(&good("channel_id", "AA".repeat(16).as_str()))),
            "scenario: channel_id is not 32 lowercase hex"
        );
        assert_eq!(
            error_of(&d.handle(&good("group_id", "3".repeat(31).as_str()))),
            "scenario: group_id is not 32 lowercase hex"
        );
        let mut no_code = good("invite_code", "x");
        no_code.as_object_mut().unwrap().remove("invite_code");
        assert_eq!(
            error_of(&d.handle(&no_code)),
            "scenario: the request carries no invite_code"
        );
        assert_eq!(
            error_of(&d.handle(&json!({"id": 2, "op": "send", "body": ""}))),
            "scenario: body must be 1..=4000 bytes"
        );
        assert_eq!(
            error_of(&d.handle(&json!({"id": 3, "op": "send", "body": "a".repeat(4001)}))),
            "scenario: body must be 1..=4000 bytes"
        );
        // 2001 two-byte characters: 2001 characters, 4002 bytes. A character count would let it through.
        assert_eq!(
            error_of(&d.handle(&json!({"id": 4, "op": "send", "body": "é".repeat(2001)}))),
            "scenario: body must be 1..=4000 bytes"
        );
        // Exactly 4000 bytes passes validation and reaches the setup check.
        assert_eq!(
            error_of(&d.handle(&json!({"id": 5, "op": "send", "body": "é".repeat(2000)}))),
            "scenario: setup first"
        );
    }

    #[test]
    fn send_validates_thread_id_before_the_setup_check() {
        let mut d = driver(1);
        let root = "ab".repeat(16);
        let send = |extra: Value| {
            let mut req = json!({"id": 1, "op": "send", "body": "in a thread"});
            req.as_object_mut()
                .unwrap()
                .extend(extra.as_object().unwrap().clone());
            req
        };
        // The control: a type-0 post with a lower-case root reaches the setup check.
        assert_eq!(
            error_of(&d.handle(&send(json!({"thread_id": root})))),
            "scenario: setup first"
        );
        for bad in [json!("AB".repeat(16)), json!("ab".repeat(15)), json!(7)] {
            assert_eq!(
                error_of(&d.handle(&send(json!({"thread_id": bad})))),
                "scenario: thread_id is not 32 lowercase hex"
            );
        }
        // A fold or any other non-post type cannot join a thread.
        for kind in 1..=6 {
            let req = send(json!({"type": kind, "reply_to": "cd".repeat(16), "thread_id": root}));
            assert_eq!(
                error_of(&d.handle(&req)),
                "scenario: thread_id is only for type 0",
                "{req}"
            );
        }
    }

    #[test]
    fn usernames_are_peer_and_six_hex_digits_unique_per_seed() {
        assert_eq!(WebDriver::username(0x5eed), "peer005eed");
        assert_eq!(WebDriver::username(0x0100_0005), "peer000005");
        assert_ne!(WebDriver::username(1), WebDriver::username(2));
    }

    #[test]
    fn serve_answers_one_line_per_request_and_skips_blank_lines() {
        let mut d = driver(1);
        let input = "\n{\"id\":1,\"op\":\"nope\"}\nnot json\n";
        let mut out = Vec::new();
        d.serve(input.as_bytes(), &mut out).unwrap();
        let lines: Vec<serde_json::Value> = String::from_utf8(out)
            .unwrap()
            .lines()
            .map(|l| serde_json::from_str(l).unwrap())
            .collect();
        assert_eq!(lines.len(), 2);
        assert_eq!(lines[0]["id"], json!(1));
        assert_eq!(lines[1]["id"], serde_json::Value::Null);
        assert!(
            lines[1]["error"].as_str().unwrap().starts_with("not JSON"),
            "{}",
            lines[1]
        );
    }

    #[test]
    fn the_web_2a_ops_validate_their_fields_before_the_setup_check() {
        let mut d = driver(1);
        let cases = [
            (
                json!({"id": 1, "op": "revoke"}),
                "scenario: the request carries no device_id",
            ),
            (
                json!({"id": 2, "op": "revoke", "device_id": "AB".repeat(16)}),
                "scenario: device_id is not 32 lowercase hex",
            ),
            (
                json!({"id": 3, "op": "open_dm", "user_id": "1".repeat(31)}),
                "scenario: user_id is not 32 lowercase hex",
            ),
            (
                json!({"id": 4, "op": "open_dm"}),
                "scenario: the request carries no user_id",
            ),
            (
                json!({"id": 5, "op": "send_dm", "channel_id": "11".repeat(16), "body": ""}),
                "scenario: body must be 1..=4000 bytes",
            ),
            (
                json!({"id": 6, "op": "send_dm", "body": "hi"}),
                "scenario: the request carries no channel_id",
            ),
            (
                json!({"id": 7, "op": "sync_dm", "channel_id": "zz".repeat(16)}),
                "scenario: channel_id is not 32 lowercase hex",
            ),
            (
                json!({"id": 8, "op": "send", "body": "hi", "channel_id": "x"}),
                "scenario: channel_id is not 32 lowercase hex",
            ),
            (
                json!({"id": 9, "op": "setup", "community": "c", "channel": "general", "channels": 0}),
                "scenario: channels must be 1..=200",
            ),
            (
                json!({"id": 10, "op": "setup", "community": "c", "channel": "general", "channels": 201}),
                "scenario: channels must be 1..=200",
            ),
            (
                json!({"id": 11, "op": "setup", "community": "c", "channel": "general", "channels": "3"}),
                "scenario: channels must be 1..=200",
            ),
            (
                json!({"id": 12, "op": "setup", "community": "c", "channel": "x".repeat(97), "channels": 2}),
                "scenario: channel must be 1..=96 bytes when channels > 1",
            ),
            (
                json!({"id": 13, "op": "setup", "community": "c", "channel": "general", "password": ""}),
                "scenario: password must be 1..=128 bytes",
            ),
            (
                json!({"id": 14, "op": "setup", "community": "c", "channel": "general", "password": "p".repeat(129)}),
                "scenario: password must be 1..=128 bytes",
            ),
            (
                json!({"id": 15, "op": "setup", "community": "c", "channel": "general", "password": 7}),
                "scenario: password must be 1..=128 bytes",
            ),
            (
                json!({"id": 16, "op": "sync", "channel_id": "AB".repeat(16)}),
                "scenario: channel_id is not 32 lowercase hex",
            ),
            (
                json!({"id": 17, "op": "members", "channel_id": "1".repeat(31)}),
                "scenario: channel_id is not 32 lowercase hex",
            ),
        ];
        for (req, want) in cases {
            assert_eq!(error_of(&d.handle(&req)), want, "{req}");
        }
        // Well-formed requests of every new op reach the setup check.
        for req in [
            json!({"id": 20, "op": "enrol"}),
            json!({"id": 21, "op": "dms"}),
            json!({"id": 22, "op": "revoke", "device_id": "ab".repeat(16)}),
            json!({"id": 23, "op": "open_dm", "user_id": "ab".repeat(16)}),
            json!({"id": 24, "op": "send_dm", "channel_id": "ab".repeat(16), "body": "hi"}),
            json!({"id": 25, "op": "sync_dm", "channel_id": "ab".repeat(16)}),
            json!({"id": 26, "op": "send", "channel_id": "ab".repeat(16), "body": "hi"}),
            json!({"id": 27, "op": "sync", "channel_id": "ab".repeat(16)}),
            json!({"id": 28, "op": "members", "channel_id": "ab".repeat(16)}),
        ] {
            assert_eq!(error_of(&d.handle(&req)), "scenario: setup first", "{req}");
        }
    }

    fn held(group: u8, seq: u64) -> Received {
        Received {
            group_id: vec![group; 16],
            seq,
            sender: DeviceId::from_bytes([0x0d; 16]),
            sender_user: UserId::from_bytes([0x0e; 16]),
            tier: Tier::Native,
            envelope: Envelope {
                v: 1,
                msg_id: MsgId::from_bytes([group; 16]),
                kind: EnvelopeType::Message,
                thread_id: None,
                reply_to: None,
                body: format!("row {seq} of group {group:#04x}"),
                attachments: Vec::new(),
                previews: Vec::new(),
                k_f: [0x06; 32],
            },
        }
    }

    /// Requirement 13 (head task 9, mutation (b)): a sync of one group takes that group's held rows and
    /// leaves every other group's rows for the sync that addresses them. No network is used.
    #[test]
    fn a_channel_sync_takes_only_its_groups_rows_and_leaves_the_others_pending() {
        let mut d = driver(1);
        d.pending.push(held(0xaa, 1));
        d.pending.push(held(0xbb, 2));
        let taken = d.take_pending(&[0xaa; 16]);
        assert_eq!(
            d.pending.len(),
            1,
            "pending still holds 1 row, got {}",
            d.pending.len()
        );
        assert_eq!(
            taken.len(),
            1,
            "the sync of group A answers 1 row, got {}",
            taken.len()
        );
        assert_eq!(
            (taken[0].group_id.clone(), taken[0].seq),
            (vec![0xaa; 16], 1)
        );
        assert_eq!(
            (d.pending[0].group_id.clone(), d.pending[0].seq),
            (vec![0xbb; 16], 2)
        );
        let rest = d.take_pending(&[0xbb; 16]);
        assert_eq!(rest.len(), 1);
        assert!(d.pending.is_empty());
        assert!(d.take_pending(&[0xaa; 16]).is_empty());
    }
    #[test]
    fn the_web_2b_ops_validate_their_fields_before_the_setup_check() {
        let mut d = driver(1);
        let msg = "ab".repeat(16);
        let cases = [
            (
                json!({"id": 1, "op": "send", "body": "x", "type": 7}),
                "scenario: type must be 0..=6",
            ),
            (
                json!({"id": 2, "op": "send", "body": "x", "type": "1"}),
                "scenario: type must be 0..=6",
            ),
            (
                json!({"id": 3, "op": "send", "body": "edited", "type": 1}),
                "scenario: reply_to is required",
            ),
            (
                json!({"id": 4, "op": "send", "type": 2}),
                "scenario: reply_to is required",
            ),
            (
                json!({"id": 5, "op": "send", "body": "x", "type": 1, "reply_to": "AB".repeat(16)}),
                "scenario: reply_to is not 32 lowercase hex",
            ),
            (
                json!({"id": 5, "op": "send", "body": "x", "type": 1, "reply_to": null}),
                "scenario: reply_to is not 32 lowercase hex",
            ),
            (
                json!({"id": 6, "op": "send", "type": 3, "reply_to": msg, "body": ""}),
                "scenario: body must be 1..=32 bytes for a reaction",
            ),
            (
                json!({"id": 7, "op": "send", "type": 4, "reply_to": msg, "body": "x".repeat(33)}),
                "scenario: body must be 1..=32 bytes for a reaction",
            ),
            (
                json!({"id": 8, "op": "send", "type": 2, "reply_to": msg, "body": "gone"}),
                "scenario: body must be empty for types 2, 5 and 6",
            ),
            (
                json!({"id": 9, "op": "send", "type": 5, "reply_to": msg, "body": " "}),
                "scenario: body must be empty for types 2, 5 and 6",
            ),
            (
                json!({"id": 10, "op": "send", "type": 1, "reply_to": msg, "body": ""}),
                "scenario: body must be 1..=4000 bytes",
            ),
            (
                json!({"id": 10, "op": "send", "type": 0, "body": 1}),
                "scenario: body must be 1..=4000 bytes",
            ),
            (
                json!({"id": 10, "op": "send", "type": 2, "reply_to": msg, "body": 1}),
                "scenario: body must be empty for types 2, 5 and 6",
            ),
            (
                json!({"id": 11, "op": "delete", "seq": 3}),
                "scenario: the request carries no msg_id",
            ),
            (
                json!({"id": 12, "op": "delete", "msg_id": msg}),
                "scenario: the request carries no seq",
            ),
            (
                json!({"id": 13, "op": "delete", "msg_id": msg, "seq": 0}),
                "scenario: seq must be at least 1",
            ),
            (
                json!({"id": 14, "op": "delete", "msg_id": msg, "seq": "3"}),
                "scenario: seq must be at least 1",
            ),
            (
                json!({"id": 15, "op": "attach", "mime": "text/plain", "bytes_hex": "00"}),
                "scenario: the request carries no name",
            ),
            (
                json!({"id": 16, "op": "attach", "name": "n".repeat(256), "mime": "text/plain", "bytes_hex": "00"}),
                "scenario: name must be 0..=255 bytes",
            ),
            (
                json!({"id": 17, "op": "attach", "name": "a", "bytes_hex": "00"}),
                "scenario: the request carries no mime",
            ),
            (
                json!({"id": 18, "op": "attach", "name": "a", "mime": "", "bytes_hex": "00"}),
                "scenario: mime must be 1..=255 bytes",
            ),
            (
                json!({"id": 19, "op": "attach", "name": "a", "mime": "x", "body": "b".repeat(4001), "bytes_hex": "00"}),
                "scenario: body must be 0..=4000 bytes",
            ),
            (
                json!({"id": 20, "op": "attach", "name": "a", "mime": "x"}),
                "scenario: attach takes bytes_hex or size with seed",
            ),
            (
                json!({"id": 21, "op": "attach", "name": "a", "mime": "x", "bytes_hex": "00", "size": 1, "seed": 1}),
                "scenario: attach takes bytes_hex or size with seed",
            ),
            (
                json!({"id": 22, "op": "attach", "name": "a", "mime": "x", "size": 5}),
                "scenario: attach takes bytes_hex or size with seed",
            ),
            (
                json!({"id": 23, "op": "attach", "name": "a", "mime": "x", "bytes_hex": "0G"}),
                "scenario: bytes_hex must be lowercase hex of at most 1048576 bytes",
            ),
            (
                json!({"id": 24, "op": "attach", "name": "a", "mime": "x", "bytes_hex": "0"}),
                "scenario: bytes_hex must be lowercase hex of at most 1048576 bytes",
            ),
            (
                json!({"id": 25, "op": "attach", "name": "a", "mime": "x", "bytes_hex": "00".repeat(1_048_577)}),
                "scenario: bytes_hex must be lowercase hex of at most 1048576 bytes",
            ),
            (
                json!({"id": 26, "op": "attach", "name": "a", "mime": "x", "size": 0, "seed": 1}),
                "scenario: size must be 1..=26214400",
            ),
            (
                json!({"id": 27, "op": "attach", "name": "a", "mime": "x", "size": 26_214_401, "seed": 1}),
                "scenario: size must be 1..=26214400",
            ),
            (
                json!({"id": 28, "op": "attach", "name": "a", "mime": "x", "size": 1, "seed": -1}),
                "scenario: seed must be an unsigned integer",
            ),
            (
                json!({"id": 29, "op": "attach", "name": "a", "mime": "x", "bytes_hex": "00", "w": "1"}),
                "scenario: w must be an unsigned integer",
            ),
            (
                json!({"id": 30, "op": "attach", "name": "a", "mime": "x", "bytes_hex": "00", "h": -2}),
                "scenario: h must be an unsigned integer",
            ),
            (
                json!({"id": 31, "op": "attach", "name": "a", "mime": "x", "bytes_hex": "00", "thumb_hex": "00".repeat(8177)}),
                "scenario: thumb_hex must be lowercase hex of at most 8176 bytes",
            ),
            (
                json!({"id": 32, "op": "fetch_attachment", "index": 0}),
                "scenario: the request carries no seq",
            ),
            (
                json!({"id": 33, "op": "fetch_attachment", "seq": 4}),
                "scenario: the request carries no index",
            ),
            (
                json!({"id": 34, "op": "fetch_attachment", "seq": 4, "index": "0"}),
                "scenario: index must be an unsigned integer",
            ),
            (
                json!({"id": 35, "op": "blob_status"}),
                "scenario: the request carries no blob_id",
            ),
            (
                json!({"id": 36, "op": "blob_status", "blob_id": "ab".repeat(16)}),
                "scenario: blob_id is not 64 lowercase hex",
            ),
            (
                json!({"id": 37, "op": "blob_status", "blob_id": "AB".repeat(32)}),
                "scenario: blob_id is not 64 lowercase hex",
            ),
            (
                json!({"id": 37, "op": "blob_status", "blob_id": null}),
                "scenario: blob_id is not 64 lowercase hex",
            ),
        ];
        for (req, want) in cases {
            assert_eq!(error_of(&d.handle(&req)), want, "{req}");
        }
        // Well-formed requests of every new or changed op reach the setup check.
        for req in [
            json!({"id": 40, "op": "send", "body": "plain"}),
            json!({"id": 41, "op": "send", "type": 1, "reply_to": msg, "body": "edited"}),
            json!({"id": 42, "op": "send", "type": 2, "reply_to": msg}),
            json!({"id": 43, "op": "send", "type": 6, "reply_to": msg, "body": ""}),
            json!({"id": 44, "op": "send", "type": 3, "reply_to": msg, "body": "👍"}),
            json!({"id": 45, "op": "send", "type": 0, "reply_to": msg, "body": "a reply"}),
            json!({"id": 46, "op": "delete", "msg_id": msg, "seq": 3}),
            json!({"id": 47, "op": "attach", "name": "", "mime": "x", "bytes_hex": ""}),
            json!({"id": 48, "op": "attach", "name": "a", "mime": "x", "size": 26_214_400, "seed": 7,
                   "w": 2, "h": 2, "thumb_hex": "00".repeat(8176)}),
            json!({"id": 49, "op": "fetch_attachment", "seq": 4, "index": 0}),
            json!({"id": 50, "op": "blob_status", "blob_id": "ab".repeat(32), "channel_id": msg}),
        ] {
            assert_eq!(error_of(&d.handle(&req)), "scenario: setup first", "{req}");
        }
    }

    #[test]
    fn seeded_plaintext_is_sha256_of_seed_and_block_index() {
        let block = |seed: u64, k: u64| {
            let mut h = Sha256::new();
            h.update(seed.to_be_bytes());
            h.update(k.to_be_bytes());
            h.finalize().to_vec()
        };
        let p = seeded_plaintext(7, 70_000);
        assert_eq!(p.len(), 70_000);
        assert_eq!(&p[..32], block(7, 0).as_slice());
        assert_eq!(&p[32..64], block(7, 1).as_slice());
        // 70 000 = 2 187 × 32 + 16: the last block is cut to 16 bytes.
        assert_eq!(&p[69_984..], &block(7, 2_187)[..16]);
        assert_eq!(seeded_plaintext(7, 5), block(7, 0)[..5].to_vec());
        assert_ne!(seeded_plaintext(8, 32), p[..32].to_vec());
    }

    #[test]
    fn attachment_key_material_is_fresh_per_attach_and_per_seed() {
        let a = WebDriver::attachment_material(1, 1);
        assert_eq!(
            a,
            WebDriver::attachment_material(1, 1),
            "derived, so a rerun reproduces it"
        );
        let b = WebDriver::attachment_material(1, 2);
        let c = WebDriver::attachment_material(2, 1);
        assert_ne!(a.0, b.0);
        assert_ne!(a.1, b.1);
        assert_ne!(a.0, c.0);
        assert_ne!(a.1, c.1);
        assert_ne!(a.0[..12], a.1[..], "the nonce is not a prefix of the key");
    }

    #[test]
    fn a_received_row_carries_its_type_reply_and_attachments_and_never_a_key() {
        let mut react = held(0xaa, 5);
        react.envelope.kind = EnvelopeType::ReactionAdd;
        react.envelope.body = "👍".into();
        react.envelope.reply_to = Some(MsgId::from_bytes([0x71; 16]));
        let plain = WebDriver::received_json(&react, false);
        assert_eq!(plain["seq"], json!(5));
        assert_eq!(plain["body"], json!("👍"));
        assert_eq!(plain["type"], json!(3));
        assert_eq!(plain["msg_id"], json!("aa".repeat(16)));
        assert_eq!(plain["reply_to"], json!("71".repeat(16)));
        assert_eq!(plain["deleted"], json!(false));
        assert_eq!(plain["attachments"], json!([]));
        assert_eq!(plain["sender_user"], json!("0e".repeat(16)));
        assert_eq!(plain["sender_device"], json!("0d".repeat(16)));
        assert_eq!(plain["tier"], json!(0));

        let mut file = held(0xbb, 6);
        file.envelope.attachments = vec![Attachment {
            blob_id: [0x43; 32],
            key: [0x44; 32],
            nonce: [0x45; 12],
            size: 1234,
            mime: "image/png".into(),
            w: Some(640),
            h: Some(480),
            thumb: Some(vec![0x46; 40]),
            name: "map.png".into(),
        }];
        let with = WebDriver::received_json(&file, false);
        assert_eq!(with["reply_to"], json!(null));
        assert_eq!(with["type"], json!(0));
        assert_eq!(
            with["attachments"],
            json!([{"index": 0, "blob_id": "43".repeat(32), "size": 1234, "mime": "image/png", "name": "map.png", "thumb": true}])
        );
        let text = with.to_string();
        assert!(
            !text.contains(&"44".repeat(32)),
            "the key never leaves the driver: {text}"
        );
        assert!(
            !text.contains(&"45".repeat(12)),
            "the nonce never leaves the driver: {text}"
        );
        assert!(
            !text.contains(&"46".repeat(40)),
            "the sealed thumbnail never leaves the driver: {text}"
        );
        assert_eq!(
            with.as_object().unwrap().len(),
            10,
            "exactly the ten keys of L-E2E-20: {text}"
        );

        let gone = WebDriver::received_json(&file, true);
        assert_eq!(gone["deleted"], json!(true));
        assert_eq!(gone["body"], json!(""));
        assert_eq!(gone["attachments"], json!([]));
        assert_eq!(gone["msg_id"], json!("bb".repeat(16)));
        assert_eq!(gone["seq"], json!(6));
        assert_eq!(
            WebDriver::unheld_deleted_json(9),
            json!({"seq": 9, "body": "", "deleted": true, "msg_id": null, "type": null, "reply_to": null,
                   "sender_user": null, "sender_device": null, "tier": null, "attachments": []})
        );
    }
}

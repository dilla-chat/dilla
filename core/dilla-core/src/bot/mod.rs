//! Strict deterministic-CBOR codecs for the built-in bot protocol.
//!
//! This module is deliberately structural: it validates the wire contract and explicit
//! authenticated context, while dispatchers remain responsible for authorization and state.

pub mod group_registration;
pub mod human_interaction_context;
pub mod interaction_admission;
pub mod interaction_snapshot;
pub mod keypackage;

use crate::cbor::{CborError, Decoder, Encoder, decode_strict};
use crate::ids::{ChannelId, CommunityId, DeviceId, MsgId, UserId};
use std::collections::BTreeSet;

pub const BODY_PREFIX: &str = "dilla.bot/1:";
pub const APP_BODY_MAX: usize = 4000;
const APP_CBOR_MAX: usize = (APP_BODY_MAX - BODY_PREFIX.len()) * 3 / 4;
pub const EVENT_MAX: usize = 4096;
pub const RECEIPT_MAX: usize = 512;
pub const HELPER_MAX: usize = 1_048_576;
pub const HELPER_PAYLOAD_MAX: usize = 1_048_480;

/// Sanitized, stable bot-codec failure categories. No variant retains caller data.
#[derive(Clone, Copy, PartialEq, Eq, Debug, thiserror::Error)]
pub enum CodecError {
    #[error("E_BOT_FORMAT")]
    Format,
    #[error("E_BOT_CONTEXT")]
    Context,
    #[error("E_BOT_EXPIRED")]
    Expired,
    #[error("E_BOT_LIMIT")]
    Limit,
    #[error("E_BOT_OPERATION")]
    Operation,
    #[error("E_BOT_VALUE")]
    Value,
}

impl From<CborError> for CodecError {
    fn from(_: CborError) -> Self {
        Self::Format
    }
}

fn nonzero<const N: usize>(bytes: [u8; N]) -> Result<[u8; N], CodecError> {
    if bytes.iter().all(|b| *b == 0) {
        Err(CodecError::Value)
    } else {
        Ok(bytes)
    }
}

fn read_msg(d: &mut Decoder<'_>) -> Result<MsgId, CborError> {
    Ok(MsgId::from_bytes(d.bytes_exact::<16>()?))
}
fn read_comm(d: &mut Decoder<'_>) -> Result<CommunityId, CborError> {
    Ok(CommunityId::from_bytes(d.bytes_exact::<16>()?))
}
fn read_channel(d: &mut Decoder<'_>) -> Result<ChannelId, CborError> {
    Ok(ChannelId::from_bytes(d.bytes_exact::<16>()?))
}
fn read_user(d: &mut Decoder<'_>) -> Result<UserId, CborError> {
    Ok(UserId::from_bytes(d.bytes_exact::<16>()?))
}
fn read_device(d: &mut Decoder<'_>) -> Result<DeviceId, CborError> {
    Ok(DeviceId::from_bytes(d.bytes_exact::<16>()?))
}
fn check_root_array(bytes: &[u8]) -> Result<(), CodecError> {
    if bytes.first().is_none_or(|b| b >> 5 != 4) {
        return Err(CodecError::Format);
    }
    decode_strict(bytes, |d| {
        d.skip()?;
        Ok(())
    })
    .map_err(Into::into)
}
fn bounded_bytes(
    d: &mut Decoder<'_>,
    min: usize,
    max: usize,
    nested: bool,
) -> Result<Vec<u8>, CborError> {
    let b = d.bytes()?;
    if b.len() < min || b.len() > max {
        return Err(CborError::WrongByteLen {
            expected: max,
            actual: b.len(),
        });
    }
    if nested {
        check_root_array(b).map_err(|_| CborError::TypeMismatch {
            expected: "array bytes",
            offset: d.position(),
        })?;
    }
    Ok(b.to_vec())
}
fn bounded_text(
    d: &mut Decoder<'_>,
    min: usize,
    max: usize,
    clean: bool,
) -> Result<String, CborError> {
    let s = d.text()?;
    if s.len() < min
        || s.len() > max
        || (clean && (s.trim().is_empty() || s.chars().any(|c| c <= '\u{1f}' || c == '\u{7f}')))
    {
        return Err(CborError::TypeMismatch {
            expected: "bounded text",
            offset: d.position(),
        });
    }
    Ok(s.to_owned())
}
fn check_id16(b: &[u8; 16]) -> Result<(), CodecError> {
    nonzero(*b).map(|_| ())
}

#[derive(Clone, PartialEq, Eq)]
pub enum RequestArgs {
    None,
    Cursor(Option<Vec<u8>>),
    Section {
        section: u64,
    },
    SettingsPut {
        section: u64,
        document: Vec<u8>,
    },
    Text(String),
    Number(u64),
    Id(MsgId),
    Move {
        item: MsgId,
        before: Option<MsgId>,
    },
    AnnouncementPut {
        schedule: Option<MsgId>,
        document: Vec<u8>,
    },
    AnnouncementDelete {
        schedule: MsgId,
        expected_revision: u64,
    },
    RolePanel {
        channel: ChannelId,
        mappings: Vec<u8>,
    },
    Evidence {
        action: MsgId,
        reason: String,
        excerpt: String,
    },
    AutoModerationPut {
        grant_token: String,
        document: Vec<u8>,
    },
    UtilityPayload(Vec<u8>),
    UtilityId(MsgId),
    PollList {
        channel: ChannelId,
        thread: Option<MsgId>,
        cursor: Option<Vec<u8>>,
    },
    PollVote {
        poll: MsgId,
        selected: Vec<u8>,
    },
    PollVoters {
        poll: MsgId,
        option: u64,
        cursor: Option<Vec<u8>>,
    },
}

#[derive(Clone, PartialEq, Eq)]
pub struct Request {
    pub request_id: MsgId,
    pub community_id: CommunityId,
    pub channel_id: Option<ChannelId>,
    pub operation: u64,
    pub revision: Option<u64>,
    pub expires: Option<u64>,
    pub args: RequestArgs,
}

impl Request {
    pub fn encode_cbor(&self) -> Result<Vec<u8>, CodecError> {
        self.validate_shape()?;
        let mut e = Encoder::new();
        e.array(8)
            .uint(1)
            .bytes(self.request_id.as_bytes())
            .bytes(self.community_id.as_bytes())
            .opt_bytes(
                self.channel_id
                    .as_ref()
                    .map(ChannelId::as_bytes)
                    .map(|x| x.as_slice()),
            )
            .uint(self.operation)
            .opt_uint(self.revision)
            .opt_uint(self.expires);
        encode_args(&mut e, &self.args);
        bounded_body(e.into_vec())
    }
    pub fn encode_body(&self) -> Result<String, CodecError> {
        let body = format!("{BODY_PREFIX}{}", b64url_encode(&self.encode_cbor()?));
        if body.len() > APP_BODY_MAX {
            Err(CodecError::Limit)
        } else {
            Ok(body)
        }
    }
    pub fn decode_body(body: &[u8]) -> Result<Self, CodecError> {
        if body.len() > APP_BODY_MAX || !body.starts_with(BODY_PREFIX.as_bytes()) {
            return Err(CodecError::Limit);
        }
        let raw = b64url_decode(&body[BODY_PREFIX.len()..])?;
        Self::decode_cbor(&raw)
    }
    pub fn decode_cbor(bytes: &[u8]) -> Result<Self, CodecError> {
        check_body_len(bytes.len())?;
        decode_strict(bytes, |d| {
            d.array(8)?;
            let version = d.uint()?;
            let request_id = read_msg(d)?;
            let community_id = read_comm(d)?;
            let channel_id = d.opt_bytes_exact::<16>()?.map(ChannelId::from_bytes);
            let operation = d.uint()?;
            let revision = d.opt_uint()?;
            let expires = d.opt_uint()?;
            let args = decode_args(d, operation)?;
            if version != 1 {
                return Err(CborError::TypeMismatch {
                    expected: "version",
                    offset: 0,
                });
            }
            Ok(Self {
                request_id,
                community_id,
                channel_id,
                operation,
                revision,
                expires,
                args,
            })
        })
        .map_err(Into::into)
        .and_then(|v| {
            v.validate_shape()?;
            Ok(v)
        })
    }
    pub fn validate_context(
        &self,
        expected_community: CommunityId,
        now: u64,
    ) -> Result<(), CodecError> {
        if self.community_id != expected_community {
            return Err(CodecError::Context);
        }
        if is_mutation(self.operation) {
            let expiry = self.expires.ok_or(CodecError::Value)?;
            let upper = now.checked_add(30).ok_or(CodecError::Expired)?;
            if expiry <= now || expiry > upper {
                return Err(CodecError::Expired);
            }
        }
        Ok(())
    }
    fn validate_shape(&self) -> Result<(), CodecError> {
        check_id16(self.request_id.as_bytes())?;
        check_id16(self.community_id.as_bytes())?;
        if let Some(ch) = self.channel_id {
            check_id16(ch.as_bytes())?;
        }
        let expects_channel = matches!(self.operation, 10..=23 | 25);
        if expects_channel != self.channel_id.is_some() {
            return Err(CodecError::Value);
        }
        let mutation = is_mutation(self.operation);
        if mutation != (self.revision.is_some() && self.expires.is_some()) {
            return Err(CodecError::Value);
        }
        if self.revision.is_some() != self.expires.is_some() {
            return Err(CodecError::Value);
        }
        if !matches!(self.operation, 0|1|2|3|10..=25|30|31|40|50..=52|60..=64|70..=75|80..=84) {
            return Err(CodecError::Operation);
        }
        if matches!(self.operation, 25 | 31 | 70 | 80) && self.revision != Some(0) {
            return Err(CodecError::Value);
        }
        if !args_match(self.operation, &self.args) {
            return Err(CodecError::Value);
        }
        match &self.args {
            RequestArgs::SettingsPut { document, .. } => {
                if document.is_empty() || document.len() > 2400 {
                    return Err(CodecError::Limit);
                }
                check_root_array(document)?;
            }
            RequestArgs::AutoModerationPut {
                grant_token,
                document,
            } => {
                if !text_valid(grant_token, 1, 512, true) {
                    return Err(CodecError::Value);
                }
                if document.is_empty() || document.len() > 2400 {
                    return Err(CodecError::Limit);
                }
                check_root_array(document)?;
            }
            RequestArgs::AnnouncementPut { schedule, document } => {
                if document.is_empty() || document.len() > 2200 {
                    return Err(CodecError::Limit);
                }
                check_root_array(document)?;
                if let Some(id) = schedule {
                    check_id16(id.as_bytes())?;
                }
            }
            RequestArgs::Id(id) => check_id16(id.as_bytes())?,
            RequestArgs::AnnouncementDelete {
                schedule,
                expected_revision,
            } => {
                check_id16(schedule.as_bytes())?;
                if *expected_revision == 0 || *expected_revision > i64::MAX as u64 {
                    return Err(CodecError::Value);
                }
            }
            RequestArgs::Move { item, before } => {
                check_id16(item.as_bytes())?;
                if let Some(id) = before {
                    check_id16(id.as_bytes())?;
                }
            }
            RequestArgs::Evidence { action, .. } => check_id16(action.as_bytes())?,
            RequestArgs::RolePanel { channel, mappings } => {
                check_id16(channel.as_bytes())?;
                check_role_panel_mappings(mappings)?;
            }
            RequestArgs::UtilityPayload(payload) => {
                validate_utility_payload(self.operation, payload)?
            }
            RequestArgs::UtilityId(id) => check_id16(id.as_bytes())?,
            RequestArgs::PollList {
                channel,
                thread,
                cursor,
            } => {
                check_id16(channel.as_bytes())?;
                if let Some(thread) = thread {
                    check_id16(thread.as_bytes())?;
                }
                check_cursor(cursor)?;
            }
            RequestArgs::PollVote { poll, selected } => {
                check_id16(poll.as_bytes())?;
                validate_poll_selection(selected)?;
            }
            RequestArgs::PollVoters {
                poll,
                option,
                cursor,
            } => {
                check_id16(poll.as_bytes())?;
                if *option > 9 {
                    return Err(CodecError::Value);
                }
                check_cursor(cursor)?;
            }
            _ => {}
        }
        Ok(())
    }
}

fn check_role_panel_mappings(bytes: &[u8]) -> Result<(), CodecError> {
    if bytes.is_empty() || bytes.len() > 2200 {
        return Err(CodecError::Limit);
    }
    let mut emojis = BTreeSet::new();
    let mut roles = BTreeSet::new();
    decode_strict(bytes, |d| {
        let len = d.array_len()?;
        if !(1..=100).contains(&len) {
            return Err(CborError::WrongArrayLen {
                expected: 100,
                actual: len,
            });
        }
        for _ in 0..len {
            d.array(3)?;
            let emoji = d.text()?;
            let role = d.bytes_exact::<16>()?;
            let name = d.text()?;
            if emoji.is_empty()
                || emoji.len() > 32
                || emoji.chars().any(|ch| ch < ' ' || ch == '\u{7f}')
                || role.iter().all(|byte| *byte == 0)
                || name.is_empty()
                || name.len() > 100
                || name.trim().is_empty()
                || name.chars().any(|ch| ch < ' ' || ch == '\u{7f}')
                || !emojis.insert(emoji.to_owned())
                || !roles.insert(role)
            {
                return Err(CborError::TypeMismatch {
                    expected: "role panel mapping",
                    offset: d.position(),
                });
            }
        }
        Ok(())
    })
    .map_err(Into::into)
}

fn check_cursor(cursor: &Option<Vec<u8>>) -> Result<(), CodecError> {
    if cursor
        .as_ref()
        .is_some_and(|v| !(1..=256).contains(&v.len()))
    {
        return Err(CodecError::Value);
    }
    Ok(())
}
fn read_opt_cursor(d: &mut Decoder<'_>) -> Result<Option<Vec<u8>>, CborError> {
    if d.try_null()? {
        return Ok(None);
    }
    let v = d.bytes()?.to_vec();
    if !(1..=256).contains(&v.len()) {
        return Err(CborError::WrongByteLen {
            expected: 256,
            actual: v.len(),
        });
    }
    Ok(Some(v))
}
fn validate_poll_selection(bytes: &[u8]) -> Result<(), CodecError> {
    if bytes.is_empty() || bytes.len() > 2200 {
        return Err(CodecError::Limit);
    }
    decode_strict(bytes, |d| {
        let n = d.array_len()?;
        if n > 10 {
            return Err(CborError::WrongArrayLen {
                expected: 10,
                actual: n,
            });
        }
        let mut seen = BTreeSet::new();
        for _ in 0..n {
            let v = d.uint()?;
            if v > 9 || !seen.insert(v) {
                return Err(CborError::TypeMismatch {
                    expected: "unique option indices",
                    offset: d.position(),
                });
            }
        }
        Ok(())
    })
    .map_err(Into::into)
}
fn validate_utility_payload(op: u64, bytes: &[u8]) -> Result<(), CodecError> {
    let poll = op == 70;
    let max = if poll { 1800 } else { 2200 };
    if bytes.is_empty() || bytes.len() > max {
        return Err(CodecError::Limit);
    }
    decode_strict(bytes, |d| {
        d.array(7)?;
        if d.uint()? != 1 {
            return Err(CborError::TypeMismatch {
                expected: "payload version",
                offset: d.position(),
            });
        }
        if poll {
            let id = d.bytes_exact::<16>()?;
            if id.iter().all(|b| *b == 0) {
                return Err(CborError::TypeMismatch {
                    expected: "poll id",
                    offset: d.position(),
                });
            }
            if let Some(thread) = d.opt_bytes_exact::<16>()?
                && thread.iter().all(|b| *b == 0)
            {
                return Err(CborError::TypeMismatch {
                    expected: "thread id",
                    offset: d.position(),
                });
            }
            let q = d.text()?;
            if q.trim().is_empty() || q.chars().count() > 300 {
                return Err(CborError::TypeMismatch {
                    expected: "poll question",
                    offset: d.position(),
                });
            }
            let n = d.array_len()?;
            if !(2..=10).contains(&n) {
                return Err(CborError::WrongArrayLen {
                    expected: 10,
                    actual: n,
                });
            }
            let mut seen = BTreeSet::new();
            for _ in 0..n {
                let v = d.text()?;
                if v.trim().is_empty() || v.chars().count() > 100 || !seen.insert(v.to_owned()) {
                    return Err(CborError::TypeMismatch {
                        expected: "unique poll answers",
                        offset: d.position(),
                    });
                }
            }
            if d.uint()? > 1 {
                return Err(CborError::TypeMismatch {
                    expected: "poll multi flag",
                    offset: d.position(),
                });
            }
            if !matches!(d.uint()?, 3600 | 14400 | 28800 | 86400 | 259200 | 604800) {
                return Err(CborError::TypeMismatch {
                    expected: "poll duration preset",
                    offset: d.position(),
                });
            }
        } else {
            let kind = d.uint()?;
            if kind > 1 {
                return Err(CborError::TypeMismatch {
                    expected: "reminder kind",
                    offset: d.position(),
                });
            }
            let when = d.uint()?;
            if (kind == 0 && !(60..=31536000).contains(&when))
                || (kind == 1 && !(1..=253402300799).contains(&when))
            {
                return Err(CborError::TypeMismatch {
                    expected: "reminder time",
                    offset: d.position(),
                });
            }
            let text = d.text()?;
            if text.is_empty() || text.len() > 1500 {
                return Err(CborError::TypeMismatch {
                    expected: "reminder text",
                    offset: d.position(),
                });
            }
            let zone = d.text()?;
            if zone.is_empty() || zone.len() > 100 {
                return Err(CborError::TypeMismatch {
                    expected: "timezone",
                    offset: d.position(),
                });
            }
            let channel = d.opt_bytes_exact::<16>()?;
            let thread = d.opt_bytes_exact::<16>()?;
            if thread.is_some() && channel.is_none() {
                return Err(CborError::TypeMismatch {
                    expected: "reminder source",
                    offset: d.position(),
                });
            }
            if channel.is_some_and(|x| x.iter().all(|b| *b == 0))
                || thread.is_some_and(|x| x.iter().all(|b| *b == 0))
            {
                return Err(CborError::TypeMismatch {
                    expected: "source id",
                    offset: d.position(),
                });
            }
        }
        Ok(())
    })
    .map_err(Into::into)
}

fn is_mutation(op: u64) -> bool {
    matches!(
        op,
        2 | 12..=23 | 25 | 31 | 51 | 52 | 61 | 63 | 70 | 73 | 74 | 80 | 82 | 83
    )
}
fn args_match(op: u64, a: &RequestArgs) -> bool {
    match op {
        0 | 3 | 10 | 13 | 14 | 16 | 19 | 23 | 24 => matches!(a, RequestArgs::None),
        15 => {
            matches!(a, RequestArgs::None)
                || matches!(a, RequestArgs::Text(s) if s.is_empty()
                || s.len() == 32 && s.bytes().any(|b| b != b'0')
                    && s.bytes().all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)))
        }
        1 => matches!(a, RequestArgs::Section { section: 0..=5 }),
        2 => matches!(a, RequestArgs::SettingsPut { section: 0..=5, .. }),
        11 => matches!(a,RequestArgs::Text(s) if text_valid(s,1,200,true)),
        12 => matches!(a,RequestArgs::Text(s) if text_valid(s,1,2048,true)),
        17 => matches!(a, RequestArgs::Number(_)),
        18 => matches!(a, RequestArgs::Number(0..=100)),
        20 => matches!(a, RequestArgs::Number(0..=2)),
        // 25: the music announcement names its text channel (Jonas 2026-10-28).
        21 | 25 => matches!(a, RequestArgs::Id(_)),
        22 => matches!(a, RequestArgs::Move { .. }),
        30 => matches!(a,RequestArgs::Text(s) if text_valid(s,1,2000,false)),
        31 => matches!(a, RequestArgs::RolePanel { .. }),
        40 | 50 => matches!(a, RequestArgs::Number(_)),
        60 | 64 => {
            matches!(a, RequestArgs::Cursor(cursor) if cursor.as_ref().is_none_or(|v| (1..=256).contains(&v.len())))
        }
        51 => matches!(a, RequestArgs::AnnouncementPut { .. }),
        52 => matches!(a, RequestArgs::AnnouncementDelete { .. }),
        61 => {
            matches!(a,RequestArgs::Evidence{reason,excerpt,..} if reason.len()<=1024 && excerpt.len()<=512)
        }
        62 => matches!(a, RequestArgs::None),
        63 => {
            matches!(a,RequestArgs::AutoModerationPut{grant_token,document} if text_valid(grant_token,1,512,true) && !document.is_empty() && document.len()<=2400)
        }
        70 | 80 => matches!(a, RequestArgs::UtilityPayload(_)),
        71 | 74 | 82 | 83 => matches!(a, RequestArgs::UtilityId(_)),
        72 => matches!(a, RequestArgs::PollList { .. }),
        73 => matches!(a, RequestArgs::PollVote { .. }),
        75 => matches!(a, RequestArgs::PollVoters { .. }),
        81 | 84 => {
            matches!(a, RequestArgs::Cursor(cursor) if cursor.as_ref().is_none_or(|v| (1..=256).contains(&v.len())))
        }
        _ => false,
    }
}
fn text_valid(s: &str, min: usize, max: usize, clean: bool) -> bool {
    s.len() >= min
        && s.len() <= max
        && (!clean || (!s.trim().is_empty() && !s.chars().any(|c| c <= '\u{1f}' || c == '\u{7f}')))
}
fn encode_args(e: &mut Encoder, a: &RequestArgs) {
    match a {
        RequestArgs::None => {
            e.array(0);
        }
        RequestArgs::Section { section } => {
            e.array(1).uint(*section);
        }
        RequestArgs::SettingsPut { section, document } => {
            e.array(2).uint(*section).bytes(document);
        }
        RequestArgs::Text(s) => {
            e.array(1).text(s);
        }
        RequestArgs::Number(n) => {
            e.array(1).uint(*n);
        }
        RequestArgs::Cursor(cursor) => {
            e.array(1);
            if let Some(cursor) = cursor {
                e.bytes(cursor);
            } else {
                e.null();
            }
        }
        RequestArgs::Id(id) => {
            e.array(1).bytes(id.as_bytes());
        }
        RequestArgs::Move { item, before } => {
            e.array(2)
                .bytes(item.as_bytes())
                .opt_bytes(before.as_ref().map(MsgId::as_bytes).map(|x| x.as_slice()));
        }
        RequestArgs::AnnouncementPut { schedule, document } => {
            e.array(2)
                .opt_bytes(schedule.as_ref().map(MsgId::as_bytes).map(|x| x.as_slice()))
                .bytes(document);
        }
        RequestArgs::AnnouncementDelete {
            schedule,
            expected_revision,
        } => {
            e.array(2)
                .bytes(schedule.as_bytes())
                .uint(*expected_revision);
        }
        RequestArgs::Evidence {
            action,
            reason,
            excerpt,
        } => {
            e.array(3)
                .bytes(action.as_bytes())
                .text(reason)
                .text(excerpt);
        }
        RequestArgs::AutoModerationPut {
            grant_token,
            document,
        } => {
            e.array(2).text(grant_token).bytes(document);
        }
        RequestArgs::RolePanel { channel, mappings } => {
            e.array(2).bytes(channel.as_bytes()).bytes(mappings);
        }
        RequestArgs::UtilityPayload(payload) => {
            e.array(1).bytes(payload);
        }
        RequestArgs::UtilityId(id) => {
            e.array(1).bytes(id.as_bytes());
        }
        RequestArgs::PollList {
            channel,
            thread,
            cursor,
        } => {
            e.array(3)
                .bytes(channel.as_bytes())
                .opt_bytes(thread.as_ref().map(MsgId::as_bytes).map(|x| x.as_slice()));
            e.opt_bytes(cursor.as_deref());
        }
        RequestArgs::PollVote { poll, selected } => {
            e.array(2).bytes(poll.as_bytes()).bytes(selected);
        }
        RequestArgs::PollVoters {
            poll,
            option,
            cursor,
        } => {
            e.array(3)
                .bytes(poll.as_bytes())
                .uint(*option)
                .opt_bytes(cursor.as_deref());
        }
    }
}
fn decode_args(d: &mut Decoder<'_>, op: u64) -> Result<RequestArgs, CborError> {
    let typeerr = || CborError::TypeMismatch {
        expected: "operation args",
        offset: d.position(),
    };
    Ok(match op {
        0 | 3 | 10 | 13 | 14 | 16 | 19 | 23 | 24 => {
            d.array(0)?;
            RequestArgs::None
        }
        15 => match d.array_len()? {
            0 => RequestArgs::None,
            1 => RequestArgs::Text(bounded_text(d, 0, 32, false)?),
            _ => {
                return Err(CborError::TypeMismatch {
                    expected: "skip args",
                    offset: d.position(),
                });
            }
        },
        1 => {
            d.array(1)?;
            RequestArgs::Section { section: d.uint()? }
        }
        2 => {
            d.array(2)?;
            RequestArgs::SettingsPut {
                section: d.uint()?,
                document: bounded_bytes(d, 1, 2400, true)?,
            }
        }
        11 | 12 | 30 => {
            d.array(1)?;
            let (lo, hi, clean) = if op == 11 {
                (1, 200, true)
            } else if op == 12 {
                (1, 2048, true)
            } else {
                (1, 2000, false)
            };
            RequestArgs::Text(bounded_text(d, lo, hi, clean)?)
        }
        31 => {
            d.array(2)?;
            RequestArgs::RolePanel {
                channel: read_channel(d)?,
                mappings: bounded_bytes(d, 1, 2200, true)?,
            }
        }
        17 | 18 | 20 | 40 | 50 => {
            d.array(1)?;
            RequestArgs::Number(d.uint()?)
        }
        60 | 64 => {
            d.array(1)?;
            let cursor = if d.try_null()? {
                None
            } else {
                Some(d.bytes()?.to_vec())
            };
            if cursor
                .as_ref()
                .is_some_and(|v| !(1..=256).contains(&v.len()))
            {
                return Err(CborError::WrongByteLen {
                    expected: 256,
                    actual: cursor.as_ref().map_or(0, Vec::len),
                });
            }
            RequestArgs::Cursor(cursor)
        }
        21 | 25 => {
            d.array(1)?;
            RequestArgs::Id(read_msg(d)?)
        }
        52 => {
            d.array(2)?;
            RequestArgs::AnnouncementDelete {
                schedule: read_msg(d)?,
                expected_revision: d.uint()?,
            }
        }
        22 => {
            d.array(2)?;
            RequestArgs::Move {
                item: read_msg(d)?,
                before: d.opt_bytes_exact::<16>()?.map(MsgId::from_bytes),
            }
        }
        51 => {
            d.array(2)?;
            RequestArgs::AnnouncementPut {
                schedule: d.opt_bytes_exact::<16>()?.map(MsgId::from_bytes),
                document: bounded_bytes(d, 1, 2200, true)?,
            }
        }
        61 => {
            d.array(3)?;
            RequestArgs::Evidence {
                action: read_msg(d)?,
                reason: bounded_text(d, 0, 1024, false)?,
                excerpt: bounded_text(d, 0, 512, false)?,
            }
        }
        62 => {
            d.array(0)?;
            RequestArgs::None
        }
        63 => {
            d.array(2)?;
            RequestArgs::AutoModerationPut {
                grant_token: bounded_text(d, 1, 512, true)?,
                document: bounded_bytes(d, 1, 2400, true)?,
            }
        }
        70 | 80 => {
            d.array(1)?;
            RequestArgs::UtilityPayload(bounded_bytes(
                d,
                1,
                if op == 70 { 1800 } else { 2200 },
                false,
            )?)
        }
        71 | 74 | 82 | 83 => {
            d.array(1)?;
            RequestArgs::UtilityId(read_msg(d)?)
        }
        72 => {
            d.array(3)?;
            let channel = read_channel(d)?;
            let thread = d.opt_bytes_exact::<16>()?.map(MsgId::from_bytes);
            let cursor = read_opt_cursor(d)?;
            RequestArgs::PollList {
                channel,
                thread,
                cursor,
            }
        }
        73 => {
            d.array(2)?;
            RequestArgs::PollVote {
                poll: read_msg(d)?,
                selected: bounded_bytes(d, 1, 2200, false)?,
            }
        }
        75 => {
            d.array(3)?;
            RequestArgs::PollVoters {
                poll: read_msg(d)?,
                option: d.uint()?,
                cursor: read_opt_cursor(d)?,
            }
        }
        81 | 84 => {
            d.array(1)?;
            RequestArgs::Cursor(read_opt_cursor(d)?)
        }
        _ => return Err(typeerr()),
    })
}
fn check_body_len(len: usize) -> Result<(), CodecError> {
    if len > APP_CBOR_MAX {
        Err(CodecError::Limit)
    } else {
        Ok(())
    }
}
fn bounded_body(b: Vec<u8>) -> Result<Vec<u8>, CodecError> {
    check_body_len(b.len())?;
    Ok(b)
}

#[derive(Clone, PartialEq, Eq)]
pub struct Response {
    pub request_id: MsgId,
    pub community_id: CommunityId,
    pub status: u64,
    pub revision: Option<u64>,
    pub data: Vec<u8>,
}
impl Response {
    pub fn encode_cbor(&self) -> Result<Vec<u8>, CodecError> {
        check_id16(self.request_id.as_bytes())?;
        check_id16(self.community_id.as_bytes())?;
        if self.status > 9 || self.data.len() > 2400 {
            return Err(CodecError::Value);
        };
        if !self.data.is_empty() {
            check_root_array(&self.data)?;
        }
        let mut e = Encoder::new();
        e.array(6)
            .uint(1)
            .bytes(self.request_id.as_bytes())
            .bytes(self.community_id.as_bytes())
            .uint(self.status)
            .opt_uint(self.revision)
            .bytes(&self.data);
        bounded_body(e.into_vec())
    }
    pub fn encode_body(&self) -> Result<String, CodecError> {
        let body = format!("{BODY_PREFIX}{}", b64url_encode(&self.encode_cbor()?));
        if body.len() > APP_BODY_MAX {
            Err(CodecError::Limit)
        } else {
            Ok(body)
        }
    }
    pub fn decode_cbor(b: &[u8]) -> Result<Self, CodecError> {
        check_body_len(b.len())?;
        decode_strict(b, |d| {
            d.array(6)?;
            let version = d.uint()?;
            let request_id = read_msg(d)?;
            let community_id = read_comm(d)?;
            let status = d.uint()?;
            let revision = d.opt_uint()?;
            let data = bounded_bytes(d, 0, 2400, false)?;
            if version != 1 || status > 9 {
                return Err(CborError::TypeMismatch {
                    expected: "response",
                    offset: 0,
                });
            };
            if !data.is_empty() {
                check_root_array(&data).map_err(|_| CborError::TypeMismatch {
                    expected: "array data",
                    offset: 0,
                })?;
            }
            Ok(Self {
                request_id,
                community_id,
                status,
                revision,
                data,
            })
        })
        .map_err(Into::into)
        .and_then(|x| {
            check_id16(x.request_id.as_bytes())?;
            check_id16(x.community_id.as_bytes())?;
            Ok(x)
        })
    }
    pub fn decode_body(b: &[u8]) -> Result<Self, CodecError> {
        if b.len() > APP_BODY_MAX || !b.starts_with(BODY_PREFIX.as_bytes()) {
            return Err(CodecError::Limit);
        };
        Self::decode_cbor(&b64url_decode(&b[BODY_PREFIX.len()..])?)
    }
    pub fn validate_context(&self, expected: CommunityId) -> Result<(), CodecError> {
        if self.community_id == expected {
            Ok(())
        } else {
            Err(CodecError::Context)
        }
    }
}

#[derive(Clone, PartialEq, Eq)]
pub struct RemovalProof {
    pub request_id: MsgId,
    pub actor_user: UserId,
    pub actor_device: DeviceId,
    pub community_id: CommunityId,
    pub group_id: MsgId,
    pub target_seq: u64,
    pub expires: u64,
}
impl RemovalProof {
    pub fn encode(&self) -> Result<Vec<u8>, CodecError> {
        self.validate()?;
        let mut e = Encoder::new();
        e.array(8)
            .uint(1)
            .bytes(self.request_id.as_bytes())
            .bytes(self.actor_user.as_bytes())
            .bytes(self.actor_device.as_bytes())
            .bytes(self.community_id.as_bytes())
            .bytes(self.group_id.as_bytes())
            .uint(self.target_seq)
            .uint(self.expires);
        Ok(e.into_vec())
    }
    pub fn decode(b: &[u8]) -> Result<Self, CodecError> {
        decode_strict(b, |d| {
            d.array(8)?;
            let v = d.uint()?;
            let x = Self {
                request_id: read_msg(d)?,
                actor_user: read_user(d)?,
                actor_device: read_device(d)?,
                community_id: read_comm(d)?,
                group_id: read_msg(d)?,
                target_seq: d.uint()?,
                expires: d.uint()?,
            };
            if v != 1 {
                return Err(CborError::TypeMismatch {
                    expected: "version",
                    offset: 0,
                });
            };
            Ok(x)
        })
        .map_err(Into::into)
        .and_then(|x| {
            x.validate()?;
            Ok(x)
        })
    }
    fn validate(&self) -> Result<(), CodecError> {
        for b in [
            self.request_id.as_bytes(),
            self.actor_user.as_bytes(),
            self.actor_device.as_bytes(),
            self.community_id.as_bytes(),
            self.group_id.as_bytes(),
        ] {
            check_id16(b)?;
        }
        if self.target_seq == 0 || self.expires == 0 {
            return Err(CodecError::Value);
        }
        Ok(())
    }
    pub fn signature_preimage(&self) -> Result<Vec<u8>, CodecError> {
        let mut p = b"dilla moderation removal v1".to_vec();
        p.extend(self.encode()?);
        Ok(p)
    }
    pub fn validate_context(
        &self,
        community: CommunityId,
        group: MsgId,
        actor: UserId,
        device: DeviceId,
        now: u64,
    ) -> Result<(), CodecError> {
        if self.community_id != community
            || self.group_id != group
            || self.actor_user != actor
            || self.actor_device != device
        {
            return Err(CodecError::Context);
        };
        if self.expires <= now || self.expires > now.checked_add(30).ok_or(CodecError::Expired)? {
            return Err(CodecError::Expired);
        }
        Ok(())
    }
}

#[derive(Clone, PartialEq, Eq)]
pub enum EventData {
    Member {
        user: UserId,
        episode: MsgId,
    },
    Installation {
        id: MsgId,
        state: u64,
        revision: u64,
    },
    Permission {
        target_kind: u64,
        target: MsgId,
        revision: u64,
    },
    Channel {
        id: ChannelId,
        change: u64,
        revision: u64,
    },
    Moderation {
        action_id: MsgId,
        request_id: MsgId,
        actor: UserId,
        device: DeviceId,
        target: Option<UserId>,
        group: Option<MsgId>,
        target_seq: Option<u64>,
        action: u64,
        expires: Option<u64>,
    },
    RolePanel {
        id: MsgId,
        revision: u64,
        active: u64,
    },
    DeviceList {
        user: UserId,
        version: u64,
    },
}
#[derive(Clone, PartialEq, Eq)]
pub struct Event {
    pub event_id: MsgId,
    pub sequence: u64,
    pub community_id: CommunityId,
    pub timestamp: u64,
    pub kind: u64,
    pub data: EventData,
}
impl Event {
    pub fn encode(&self) -> Result<Vec<u8>, CodecError> {
        self.validate()?;
        let mut e = Encoder::new();
        e.array(7)
            .uint(1)
            .bytes(self.event_id.as_bytes())
            .uint(self.sequence)
            .bytes(self.community_id.as_bytes())
            .uint(self.timestamp)
            .uint(self.kind);
        encode_event_data(&mut e, &self.data);
        let b = e.into_vec();
        if b.len() > EVENT_MAX {
            return Err(CodecError::Limit);
        }
        Ok(b)
    }
    pub fn decode(b: &[u8]) -> Result<Self, CodecError> {
        if b.len() > EVENT_MAX {
            return Err(CodecError::Limit);
        };
        decode_strict(b, |d| {
            d.array(7)?;
            let v = d.uint()?;
            let event_id = read_msg(d)?;
            let sequence = d.uint()?;
            let community_id = read_comm(d)?;
            let timestamp = d.uint()?;
            let kind = d.uint()?;
            let data = decode_event_data(d, kind)?;
            if v != 1 {
                return Err(CborError::TypeMismatch {
                    expected: "version",
                    offset: 0,
                });
            };
            Ok(Self {
                event_id,
                sequence,
                community_id,
                timestamp,
                kind,
                data,
            })
        })
        .map_err(Into::into)
        .and_then(|x| {
            x.validate()?;
            Ok(x)
        })
    }
    fn validate(&self) -> Result<(), CodecError> {
        check_id16(self.event_id.as_bytes())?;
        check_id16(self.community_id.as_bytes())?;
        if self.sequence == 0 || !(1..=8).contains(&self.kind) {
            return Err(CodecError::Value);
        };
        if !event_matches(self.kind, &self.data, self.timestamp) {
            return Err(CodecError::Value);
        };
        if let EventData::Moderation { target, group, .. } = &self.data {
            if let Some(id) = target {
                check_id16(id.as_bytes())?;
            }
            if let Some(id) = group {
                check_id16(id.as_bytes())?;
            }
        }
        Ok(())
    }
    pub fn validate_context(&self, community: CommunityId) -> Result<(), CodecError> {
        if self.community_id == community {
            Ok(())
        } else {
            Err(CodecError::Context)
        }
    }
}
fn event_matches(k: u64, d: &EventData, t: u64) -> bool {
    match (k, d) {
        (1, EventData::Member { user, episode }) => {
            nonzero(*user.as_bytes()).is_ok() && nonzero(*episode.as_bytes()).is_ok()
        }
        (2, EventData::Member { user, episode }) => {
            nonzero(*user.as_bytes()).is_ok() && nonzero(*episode.as_bytes()).is_ok()
        }
        (3, EventData::Installation { id, state, .. }) => {
            nonzero(*id.as_bytes()).is_ok() && *state <= 5
        }
        (
            4,
            EventData::Permission {
                target_kind,
                target,
                ..
            },
        ) => *target_kind <= 3 && nonzero(*target.as_bytes()).is_ok(),
        (5, EventData::Channel { id, change, .. }) => {
            *change <= 2 && nonzero(*id.as_bytes()).is_ok()
        }
        (
            6,
            EventData::Moderation {
                action_id,
                request_id,
                actor,
                device,
                target,
                group,
                target_seq,
                action,
                expires,
            },
        ) => {
            let ids = nonzero(*action_id.as_bytes()).is_ok()
                && nonzero(*request_id.as_bytes()).is_ok()
                && nonzero(*actor.as_bytes()).is_ok()
                && nonzero(*device.as_bytes()).is_ok();
            if !ids || *action > 6 {
                return false;
            }
            if *action == 4 {
                group.is_some() && target_seq.is_some_and(|x| x > 0) && expires.is_none()
            } else {
                target.is_some()
                    && group.is_none()
                    && target_seq.is_none()
                    && if *action == 1 {
                        expires.is_some_and(|x| x > t)
                    } else if *action == 3 {
                        expires.is_none_or(|x| x > t)
                    } else {
                        expires.is_none()
                    }
            }
        }
        (7, EventData::RolePanel { id, active, .. }) => {
            nonzero(*id.as_bytes()).is_ok() && *active <= 1
        }
        (8, EventData::DeviceList { user, version }) => {
            nonzero(*user.as_bytes()).is_ok() && (1..=i64::MAX as u64).contains(version)
        }
        _ => false,
    }
}
fn encode_event_data(e: &mut Encoder, d: &EventData) {
    match d {
        EventData::Member { user, episode } => {
            e.array(2).bytes(user.as_bytes()).bytes(episode.as_bytes());
        }
        EventData::Installation {
            id,
            state,
            revision,
        } => {
            e.array(3).bytes(id.as_bytes()).uint(*state).uint(*revision);
        }
        EventData::Permission {
            target_kind,
            target,
            revision,
        } => {
            e.array(3)
                .uint(*target_kind)
                .bytes(target.as_bytes())
                .uint(*revision);
        }
        EventData::Channel {
            id,
            change,
            revision,
        } => {
            e.array(3)
                .bytes(id.as_bytes())
                .uint(*change)
                .uint(*revision);
        }
        EventData::Moderation {
            action_id,
            request_id,
            actor,
            device,
            target,
            group,
            target_seq,
            action,
            expires,
        } => {
            e.array(9)
                .bytes(action_id.as_bytes())
                .bytes(request_id.as_bytes())
                .bytes(actor.as_bytes())
                .bytes(device.as_bytes())
                .opt_bytes(target.as_ref().map(UserId::as_bytes).map(|x| x.as_slice()))
                .opt_bytes(group.as_ref().map(MsgId::as_bytes).map(|x| x.as_slice()))
                .opt_uint(*target_seq)
                .uint(*action)
                .opt_uint(*expires);
        }
        EventData::RolePanel {
            id,
            revision,
            active,
        } => {
            e.array(3)
                .bytes(id.as_bytes())
                .uint(*revision)
                .uint(*active);
        }
        EventData::DeviceList { user, version } => {
            e.array(2).bytes(user.as_bytes()).uint(*version);
        }
    }
}
fn decode_event_data(d: &mut Decoder<'_>, k: u64) -> Result<EventData, CborError> {
    Ok(match k {
        1 | 2 => {
            d.array(2)?;
            EventData::Member {
                user: read_user(d)?,
                episode: read_msg(d)?,
            }
        }
        3 => {
            d.array(3)?;
            EventData::Installation {
                id: read_msg(d)?,
                state: d.uint()?,
                revision: d.uint()?,
            }
        }
        4 => {
            d.array(3)?;
            EventData::Permission {
                target_kind: d.uint()?,
                target: read_msg(d)?,
                revision: d.uint()?,
            }
        }
        5 => {
            d.array(3)?;
            EventData::Channel {
                id: read_channel(d)?,
                change: d.uint()?,
                revision: d.uint()?,
            }
        }
        6 => {
            d.array(9)?;
            EventData::Moderation {
                action_id: read_msg(d)?,
                request_id: read_msg(d)?,
                actor: read_user(d)?,
                device: read_device(d)?,
                target: d.opt_bytes_exact::<16>()?.map(UserId::from_bytes),
                group: d.opt_bytes_exact::<16>()?.map(MsgId::from_bytes),
                target_seq: d.opt_uint()?,
                action: d.uint()?,
                expires: d.opt_uint()?,
            }
        }
        7 => {
            d.array(3)?;
            EventData::RolePanel {
                id: read_msg(d)?,
                revision: d.uint()?,
                active: d.uint()?,
            }
        }
        8 => {
            d.array(2)?;
            EventData::DeviceList {
                user: read_user(d)?,
                version: d.uint()?,
            }
        }
        _ => {
            return Err(CborError::TypeMismatch {
                expected: "event kind",
                offset: d.position(),
            });
        }
    })
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum SendOutcome {
    ClosedNotAccepted,
    Accepted,
    UnknownAfterRestore,
}

#[derive(Clone, PartialEq, Eq)]
pub struct OriginalSendResponse {
    pub sequence: u64,
    pub franking_tag: [u8; 32],
    pub receive_timestamp: u64,
    /// The exact canonical response bytes returned by the original send.
    pub body: Vec<u8>,
}

#[derive(Clone, PartialEq, Eq)]
pub struct Receipt {
    pub operation_id: MsgId,
    pub attempt_id: MsgId,
    pub predecessor_attempt_id: Option<MsgId>,
    pub community_id: CommunityId,
    pub group_id: MsgId,
    pub expected_generation: u64,
    pub ciphertext_sha256: [u8; 32],
    pub class: u64,
    pub outcome: SendOutcome,
    pub epoch: u64,
    pub send_response: Option<OriginalSendResponse>,
}

fn decode_original_send_response(raw: &[u8]) -> Result<OriginalSendResponse, CodecError> {
    decode_strict(raw, |d| {
        d.array(3)?;
        let sequence = d.uint()?;
        let franking_tag = d.bytes_exact::<32>()?;
        let receive_timestamp = d.uint()?;
        if sequence == 0 || sequence > i64::MAX as u64 || receive_timestamp > i64::MAX as u64 {
            return Err(CborError::TypeMismatch {
                expected: "bounded send response",
                offset: d.position(),
            });
        }
        Ok((sequence, franking_tag, receive_timestamp))
    })
    .map(
        |(sequence, franking_tag, receive_timestamp)| OriginalSendResponse {
            sequence,
            franking_tag,
            receive_timestamp,
            body: raw.to_vec(),
        },
    )
    .map_err(Into::into)
}

impl Receipt {
    pub fn encode(&self) -> Result<Vec<u8>, CodecError> {
        self.validate()?;
        let mut e = Encoder::new();
        e.array(12)
            .uint(1)
            .bytes(self.operation_id.as_bytes())
            .bytes(self.attempt_id.as_bytes())
            .opt_bytes(
                self.predecessor_attempt_id
                    .as_ref()
                    .map(MsgId::as_bytes)
                    .map(|x| x.as_slice()),
            )
            .bytes(self.community_id.as_bytes())
            .bytes(self.group_id.as_bytes())
            .uint(self.expected_generation)
            .bytes(&self.ciphertext_sha256)
            .uint(self.class)
            .uint(match self.outcome {
                SendOutcome::ClosedNotAccepted => 0,
                SendOutcome::Accepted => 1,
                SendOutcome::UnknownAfterRestore => 2,
            })
            .uint(self.epoch)
            .opt_bytes(self.send_response.as_ref().map(|x| x.body.as_slice()));
        let b = e.into_vec();
        if b.len() > RECEIPT_MAX {
            Err(CodecError::Limit)
        } else {
            Ok(b)
        }
    }
    pub fn decode(b: &[u8]) -> Result<Self, CodecError> {
        if b.len() > RECEIPT_MAX {
            return Err(CodecError::Limit);
        };
        decode_strict(b, |d| {
            d.array(12)?;
            let v = d.uint()?;
            if v != 1 {
                return Err(CborError::TypeMismatch {
                    expected: "version",
                    offset: 0,
                });
            };
            let operation_id = read_msg(d)?;
            let attempt_id = read_msg(d)?;
            let predecessor_attempt_id = d.opt_bytes_exact::<16>()?.map(MsgId::from_bytes);
            let community_id = read_comm(d)?;
            let group_id = read_msg(d)?;
            let expected_generation = d.uint()?;
            let ciphertext_sha256 = d.bytes_exact::<32>()?;
            let class = d.uint()?;
            let outcome = match d.uint()? {
                0 => SendOutcome::ClosedNotAccepted,
                1 => SendOutcome::Accepted,
                2 => SendOutcome::UnknownAfterRestore,
                _ => {
                    return Err(CborError::TypeMismatch {
                        expected: "send outcome",
                        offset: d.position(),
                    });
                }
            };
            let epoch = d.uint()?;
            let send_response = d
                .opt_bytes()?
                .map(decode_original_send_response)
                .transpose()
                .map_err(|_| CborError::TypeMismatch {
                    expected: "canonical send response",
                    offset: d.position(),
                })?;
            Ok(Self {
                operation_id,
                attempt_id,
                predecessor_attempt_id,
                community_id,
                group_id,
                expected_generation,
                ciphertext_sha256,
                class,
                outcome,
                epoch,
                send_response,
            })
        })
        .map_err(Into::into)
        .and_then(|x| {
            x.validate()?;
            Ok(x)
        })
    }
    fn validate(&self) -> Result<(), CodecError> {
        for b in [
            self.operation_id.as_bytes(),
            self.attempt_id.as_bytes(),
            self.community_id.as_bytes(),
            self.group_id.as_bytes(),
        ] {
            check_id16(b)?;
        }
        if let Some(predecessor) = self.predecessor_attempt_id {
            check_id16(predecessor.as_bytes())?;
        }
        if self.expected_generation > i64::MAX as u64
            || self.epoch > i64::MAX as u64
            || self.class > 1
        {
            return Err(CodecError::Value);
        }
        match (self.outcome, &self.send_response) {
            (SendOutcome::Accepted, Some(response)) => {
                let decoded = decode_original_send_response(&response.body)?;
                if decoded != *response {
                    return Err(CodecError::Value);
                }
                Ok(())
            }
            (SendOutcome::ClosedNotAccepted | SendOutcome::UnknownAfterRestore, None) => Ok(()),
            _ => Err(CodecError::Value),
        }
    }
    pub fn validate_context(&self, c: CommunityId, g: MsgId) -> Result<(), CodecError> {
        if self.community_id == c && self.group_id == g {
            Ok(())
        } else {
            Err(CodecError::Context)
        }
    }
}

#[derive(Clone, PartialEq, Eq)]
pub struct HelperRequest {
    pub request_id: MsgId,
    pub operation: u64,
    pub payload: Vec<u8>,
}
#[derive(Clone, PartialEq, Eq)]
pub struct HelperResponse {
    pub request_id: MsgId,
    pub status: u64,
    pub payload: Vec<u8>,
}
impl HelperRequest {
    pub fn encode(&self) -> Result<Vec<u8>, CodecError> {
        helper_encode(self.request_id, self.operation, &self.payload, true)
    }
    pub fn decode(b: &[u8]) -> Result<Self, CodecError> {
        let (id, op, payload) = helper_decode(b, true)?;
        Ok(Self {
            request_id: id,
            operation: op,
            payload,
        })
    }
    pub fn encode_frame(&self) -> Result<Vec<u8>, CodecError> {
        frame(&self.encode()?)
    }
    pub fn decode_frame(bytes: &[u8]) -> Result<Self, CodecError> {
        Self::decode(parse_frame(bytes)?)
    }
}
impl HelperResponse {
    pub fn encode(&self) -> Result<Vec<u8>, CodecError> {
        helper_encode(self.request_id, self.status, &self.payload, false)
    }
    pub fn decode(b: &[u8]) -> Result<Self, CodecError> {
        let (id, status, payload) = helper_decode(b, false)?;
        Ok(Self {
            request_id: id,
            status,
            payload,
        })
    }
    pub fn encode_frame(&self) -> Result<Vec<u8>, CodecError> {
        frame(&self.encode()?)
    }
    pub fn decode_frame(bytes: &[u8]) -> Result<Self, CodecError> {
        Self::decode(parse_frame(bytes)?)
    }
}
fn helper_encode(id: MsgId, n: u64, p: &[u8], request: bool) -> Result<Vec<u8>, CodecError> {
    check_id16(id.as_bytes())?;
    if p.is_empty()
        || p.len() > HELPER_PAYLOAD_MAX
        || (request && !(1..=20).contains(&n))
        || (!request && n > 9)
    {
        return Err(CodecError::Value);
    };
    check_root_array(p)?;
    let mut e = Encoder::new();
    e.array(4).uint(1).bytes(id.as_bytes()).uint(n).bytes(p);
    let b = e.into_vec();
    if b.len() > HELPER_MAX {
        Err(CodecError::Limit)
    } else {
        Ok(b)
    }
}
fn helper_decode(b: &[u8], request: bool) -> Result<(MsgId, u64, Vec<u8>), CodecError> {
    if b.len() > HELPER_MAX {
        return Err(CodecError::Limit);
    };
    decode_strict(b, |d| {
        d.array(4)?;
        let v = d.uint()?;
        let id = read_msg(d)?;
        let n = d.uint()?;
        let p = bounded_bytes(d, 1, HELPER_PAYLOAD_MAX, false)?;
        if v != 1 || (request && !(1..=20).contains(&n)) || (!request && n > 9) {
            return Err(CborError::TypeMismatch {
                expected: "helper envelope",
                offset: 0,
            });
        };
        check_root_array(&p).map_err(|_| CborError::TypeMismatch {
            expected: "array payload",
            offset: 0,
        })?;
        Ok((id, n, p))
    })
    .map_err(Into::into)
    .and_then(|x| {
        check_id16(x.0.as_bytes())?;
        Ok(x)
    })
}
pub fn frame(body: &[u8]) -> Result<Vec<u8>, CodecError> {
    if body.is_empty() || body.len() > HELPER_MAX {
        return Err(CodecError::Limit);
    };
    let len = u32::try_from(body.len()).map_err(|_| CodecError::Limit)?;
    let mut out = Vec::with_capacity(body.len() + 4);
    out.extend_from_slice(&len.to_be_bytes());
    out.extend_from_slice(body);
    Ok(out)
}
pub fn parse_frame(frame: &[u8]) -> Result<&[u8], CodecError> {
    if frame.len() < 4 {
        return Err(CodecError::Format);
    };
    let n = u32::from_be_bytes([frame[0], frame[1], frame[2], frame[3]]) as usize;
    if n == 0 || n > HELPER_MAX || frame.len() != n + 4 {
        return Err(CodecError::Limit);
    };
    Ok(&frame[4..])
}

fn b64url_encode(data: &[u8]) -> String {
    const T: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
    let mut out = String::with_capacity((data.len() * 4).div_ceil(3));
    for c in data.chunks(3) {
        let a = c[0];
        let b = *c.get(1).unwrap_or(&0);
        let d = *c.get(2).unwrap_or(&0);
        out.push(T[(a >> 2) as usize] as char);
        out.push(T[(((a & 3) << 4) | (b >> 4)) as usize] as char);
        if c.len() > 1 {
            out.push(T[(((b & 15) << 2) | (d >> 6)) as usize] as char)
        }
        if c.len() > 2 {
            out.push(T[(d & 63) as usize] as char)
        }
    }
    out
}
fn b64url_decode(s: &[u8]) -> Result<Vec<u8>, CodecError> {
    if s.is_empty()
        || s.iter()
            .any(|b| !matches!(b,b'A'..=b'Z'|b'a'..=b'z'|b'0'..=b'9'|b'-'|b'_'))
        || s.len() % 4 == 1
    {
        return Err(CodecError::Format);
    }
    let mut out = Vec::with_capacity(s.len() * 3 / 4);
    let mut acc = 0u32;
    let mut bits = 0u8;
    for &b in s {
        let v = match b {
            b'A'..=b'Z' => b - b'A',
            b'a'..=b'z' => b - b'a' + 26,
            b'0'..=b'9' => b - b'0' + 52,
            b'-' => 62,
            b'_' => 63,
            _ => return Err(CodecError::Format),
        };
        acc = (acc << 6) | u32::from(v);
        bits += 6;
        if bits >= 8 {
            bits -= 8;
            out.push((acc >> bits) as u8);
            acc &= (1 << bits) - 1;
        }
    }
    if acc != 0 || b64url_encode(&out).as_bytes() != s {
        return Err(CodecError::Format);
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::cbor::Encoder;
    #[test]
    fn helper_protocol_accepts_call_group_operations_only_through_twenty() {
        for operation in [19, 20] {
            let request = HelperRequest {
                request_id: MsgId::from_bytes([1; 16]),
                operation,
                payload: vec![0x80],
            };
            let encoded = request.encode().expect("encode helper call operation");
            let decoded = HelperRequest::decode(&encoded).unwrap();
            assert_eq!(decoded.request_id, request.request_id);
            assert_eq!(decoded.operation, request.operation);
            assert_eq!(decoded.payload, request.payload);
        }
        let unknown = HelperRequest {
            request_id: MsgId::from_bytes([1; 16]),
            operation: 21,
            payload: vec![0x80],
        };
        assert!(unknown.encode().is_err());
    }

    #[test]
    fn music_announce_names_a_text_channel_on_the_voice_channel_at_revision_zero() {
        fn request(revision: u64) -> Vec<u8> {
            let mut e = Encoder::new();
            e.array(8)
                .uint(1)
                .bytes(&[1; 16])
                .bytes(&[2; 16])
                .bytes(&[3; 16])
                .uint(25)
                .uint(revision)
                .uint(130)
                .array(1)
                .bytes(&[4; 16]);
            e.into_vec()
        }
        let decoded = Request::decode_cbor(&request(0)).unwrap();
        assert_eq!(decoded.operation, 25);
        assert!(matches!(decoded.args, RequestArgs::Id(id) if id.as_bytes() == &[4; 16]));
        assert_eq!(decoded.encode_cbor().unwrap(), request(0));
        assert!(Request::decode_cbor(&request(1)).is_err());
    }

    #[test]
    fn music_skip_accepts_optional_expected_current_item() {
        let request = |args: RequestArgs| Request {
            request_id: MsgId::from_bytes([1; 16]),
            community_id: CommunityId::from_bytes([2; 16]),
            channel_id: Some(ChannelId::from_bytes([3; 16])),
            operation: 15,
            revision: Some(0),
            expires: Some(130),
            args,
        };
        for args in [
            RequestArgs::None,
            RequestArgs::Text("ab".repeat(16)),
            RequestArgs::Text(String::new()),
        ] {
            let original = request(args);
            let encoded = original.encode_cbor().expect("valid skip guard");
            let decoded = Request::decode_cbor(&encoded).expect("decode skip guard");
            assert!(decoded == original);
        }
        for value in ["bad-id".to_owned(), "AB".repeat(16), "00".repeat(16)] {
            assert!(request(RequestArgs::Text(value)).encode_cbor().is_err());
        }
    }

    #[test]
    fn base64url_rejects_nonzero_unused_bits_and_padding() {
        assert!(b64url_decode(b"Zh").is_err());
        assert!(b64url_decode(b"Zg==").is_err());
        assert_eq!(b64url_decode(b"Zg").unwrap(), b"f");
    }

    #[test]
    fn moderation_cases_request_uses_optional_bounded_opaque_cursor() {
        fn request(operation: u64, cursor: Option<&[u8]>) -> Vec<u8> {
            let mut e = Encoder::new();
            e.array(8)
                .uint(1)
                .bytes(&[1; 16])
                .bytes(&[2; 16])
                .null()
                .uint(operation)
                .null()
                .null()
                .array(1);
            if let Some(cursor) = cursor {
                e.bytes(cursor);
            } else {
                e.null();
            }
            e.into_vec()
        }
        for operation in [60, 64] {
            let first = Request::decode_cbor(&request(operation, None)).unwrap();
            assert_eq!(first.operation, operation);
            assert!(matches!(first.args, RequestArgs::Cursor(None)));
            assert_eq!(first.encode_cbor().unwrap(), request(operation, None));
            let next = Request::decode_cbor(&request(operation, Some(&[1, 2, 3]))).unwrap();
            assert!(
                matches!(next.args, RequestArgs::Cursor(Some(ref cursor)) if cursor.as_slice() == [1, 2, 3])
            );
            assert_eq!(
                next.encode_cbor().unwrap(),
                request(operation, Some(&[1, 2, 3]))
            );
            assert!(Request::decode_cbor(&request(operation, Some(&[]))).is_err());
            assert!(Request::decode_cbor(&request(operation, Some(&[1; 257]))).is_err());
        }
        let mut number = Encoder::new();
        number
            .array(8)
            .uint(1)
            .bytes(&[1; 16])
            .bytes(&[2; 16])
            .null()
            .uint(64)
            .null()
            .null()
            .array(1)
            .uint(0);
        assert!(Request::decode_cbor(&number.into_vec()).is_err());
    }

    #[test]
    fn manual_evidence_request_uses_the_three_field_receipt_bound_tuple() {
        fn request(tail: &[&str]) -> Vec<u8> {
            let mut e = Encoder::new();
            e.array(8)
                .uint(1)
                .bytes(&[1; 16])
                .bytes(&[2; 16])
                .null()
                .uint(61)
                .uint(0)
                .uint(110)
                .array(1 + tail.len())
                .bytes(&[3; 16]);
            for value in tail {
                e.text(value);
            }
            e.into_vec()
        }

        let valid = request(&["warning reason", ""]);
        let decoded = Request::decode_cbor(&valid).unwrap();
        assert!(matches!(decoded.args, RequestArgs::Evidence { .. }));
        assert_eq!(decoded.encode_cbor().unwrap(), valid);
        assert!(Request::decode_cbor(&request(&["warn", "warning reason", ""])).is_err());
    }
}

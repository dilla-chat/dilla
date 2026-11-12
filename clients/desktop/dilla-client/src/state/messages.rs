//! `timeline:<channel>` (types.ts `TimelineState`, `TimelineItem`), built from the core's stored rows and its outbox
//! as derive.ts `buildTimeline` builds it: one item per displayable stored row (type 0), in the core's order, then one
//! per outbox post the store does not hold yet; an own stored row whose message id is known keeps its outbox key
//! (`o<msg id>`), so a sent message is never shown twice and does not change key when it is confirmed.

use std::collections::HashSet;

use crate::gateway::frames::Value;
use crate::sync::value::{DecodeError, Rows, arr, bin16, opt, text, u53, uint};

use super::{ChannelGroupState, ChannelId, DeviceId, MessageId, UserId, hex};

/// types.ts `TIMELINE_PAGE`: the rows read for an open timeline.
pub const TIMELINE_PAGE: u32 = 100;

/// `TimelineItemState`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TimelineItemState {
    Ok,
    Pending,
    Failed,
    CannotRead,
    Deleted,
}

/// `ReplyRef`, as the core resolved it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ReplyRef {
    pub msg_id: MessageId,
    /// 0 ok, 1 missing, 2 deleted.
    pub state: u8,
    pub sender_user: Option<UserId>,
    pub excerpt: String,
    pub seq: Option<u64>,
}

/// `ReactionSummary`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Reaction {
    pub emoji: String,
    pub count: u32,
    pub mine: bool,
}

/// What the UI may know of an attachment: never its key, nonce or blob id.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Attachment {
    pub index: u32,
    pub size: u64,
    pub mime: String,
    pub name: String,
}

/// `TimelineItem`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TimelineItem {
    /// `o<msg id hex>` for an outbox row and an own stored row with a known message id; `s<seq>` otherwise.
    pub key: String,
    pub state: TimelineItemState,
    /// The raw code for `CannotRead` and `Failed`, else empty.
    pub reason: String,
    pub sender_user: Option<UserId>,
    pub sender_device: Option<DeviceId>,
    pub own: bool,
    /// The sender credential claims a Bot (types.ts `bot`).
    pub bot: bool,
    /// Unix seconds: the stored row's receive time, or the outbox row's creation.
    pub ts: u64,
    /// Empty unless the state is `Ok`, `Pending` or `Failed`.
    pub body: String,
    pub msg_id: Option<MessageId>,
    /// The stored seq; `None` for an outbox item.
    pub seq: Option<u64>,
    pub edited: bool,
    pub reply: Option<ReplyRef>,
    pub reactions: Vec<Reaction>,
    pub pinned: bool,
    pub attachments: Vec<Attachment>,
    pub mention: bool,
}

/// `TimelineState`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Timeline {
    pub channel_id: ChannelId,
    pub group: ChannelGroupState,
    pub items: Vec<TimelineItem>,
    pub has_earlier: bool,
}

impl Timeline {
    /// The stored items' bodies, oldest first (for tests and logs).
    pub fn bodies(&self) -> Vec<&str> {
        self.items.iter().map(|i| i.body.as_str()).collect()
    }
}

/// core-port.ts `TimelineRow` (18 elements, 19 since web3.7).
#[derive(Clone, Debug)]
pub(crate) struct TimelineRow {
    pub seq: u64,
    pub recv_ts: u64,
    /// 0 readable, 1 cannot be read, 2 deleted.
    pub status: u8,
    pub reason: String,
    pub sender_user: Option<[u8; 16]>,
    pub sender_device: [u8; 16],
    pub sender_kind: Option<u64>,
    pub msg_id: Option<[u8; 16]>,
    pub kind: Option<u64>,
    pub body: String,
    pub edited_seq: u64,
    pub reply: Option<ReplyRef>,
    pub reactions: Vec<Reaction>,
    pub pinned: bool,
    pub attachments: Vec<Attachment>,
    pub mention: bool,
}

/// core-port.ts `OutboxRow`: `[msg_id, state, error, created, body, type, reply_to|null, attachments]`.
#[derive(Clone, Debug)]
pub(crate) struct OutboxRow {
    pub msg_id: [u8; 16],
    /// 0 queued, 1 in flight, 2 failed.
    pub state: u8,
    pub error: String,
    pub created: u64,
    pub body: String,
    pub kind: u8,
    pub reply_to: Option<[u8; 16]>,
    pub attachments: Vec<Attachment>,
}

fn flag(v: &Value) -> Result<bool, DecodeError> {
    match u53(v)? {
        0 => Ok(false),
        1 => Ok(true),
        _ => Err(DecodeError("a flag is 0 or 1")),
    }
}

fn reply(v: &Value) -> Result<ReplyRef, DecodeError> {
    let a = v.as_array().ok_or(DecodeError("reply"))?;
    if a.len() != 5 && a.len() != 6 {
        return Err(DecodeError("a reply has 5 or 6 elements"));
    }
    let state = u53(&a[4])?;
    if state > 2 {
        return Err(DecodeError("reply state"));
    }
    Ok(ReplyRef {
        msg_id: MessageId(bin16(&a[0])?),
        seq: opt(&a[1], uint)?,
        sender_user: opt(&a[2], bin16)?.map(UserId),
        excerpt: text(&a[3])?,
        state: state as u8,
    })
}

fn reaction(v: &Value) -> Result<Reaction, DecodeError> {
    let a = arr(v, 3)?;
    Ok(Reaction {
        emoji: text(&a[0])?,
        count: u32::try_from(u53(&a[1])?).unwrap_or(u32::MAX),
        mine: flag(&a[2])?,
    })
}

/// `[index, size, mime, w|null, h|null, has_thumb, name]`.
fn attachment_info(v: &Value) -> Result<Attachment, DecodeError> {
    let a = arr(v, 7)?;
    Ok(Attachment {
        index: u32::try_from(u53(&a[0])?).unwrap_or(u32::MAX),
        size: u53(&a[1])?,
        mime: text(&a[2])?,
        name: text(&a[6])?,
    })
}

pub(crate) fn decode_timeline(raw: &[u8]) -> Result<Vec<TimelineRow>, DecodeError> {
    Rows::of(raw)?
        .iter()
        .map(|row| {
            let a = row.as_array().ok_or(DecodeError("timeline row"))?;
            if a.len() != 18 && a.len() != 19 {
                return Err(DecodeError("a timeline row has 18 or 19 elements"));
            }
            let status = u53(&a[3])?;
            if status > 2 {
                return Err(DecodeError("timeline status"));
            }
            Ok(TimelineRow {
                seq: uint(&a[0])?,
                recv_ts: uint(&a[2])?,
                status: status as u8,
                reason: text(&a[4])?,
                sender_user: opt(&a[5], bin16)?,
                sender_device: bin16(&a[6])?,
                sender_kind: opt(&a[7], u53)?,
                msg_id: opt(&a[9], bin16)?,
                kind: opt(&a[10], u53)?,
                body: text(&a[11])?,
                edited_seq: uint(&a[12])?,
                reply: opt(&a[13], reply)?,
                reactions: list(&a[14], reaction)?,
                pinned: flag(&a[15])?,
                attachments: list(&a[16], attachment_info)?,
                mention: flag(&a[17])?,
            })
        })
        .collect()
}

pub(crate) fn decode_outbox(raw: &[u8]) -> Result<Vec<OutboxRow>, DecodeError> {
    Rows::of(raw)?
        .iter()
        .map(|row| {
            let a = arr(row, 8)?;
            let state = u53(&a[1])?;
            let kind = u53(&a[5])?;
            if state > 2 || kind > 6 {
                return Err(DecodeError("outbox state or type"));
            }
            Ok(OutboxRow {
                msg_id: bin16(&a[0])?,
                state: state as u8,
                error: text(&a[2])?,
                created: uint(&a[3])?,
                body: text(&a[4])?,
                kind: kind as u8,
                reply_to: opt(&a[6], bin16)?,
                attachments: list(&a[7], |v| {
                    let b = arr(v, 4)?;
                    Ok(Attachment {
                        index: 0,
                        size: u53(&b[1])?,
                        mime: text(&b[2])?,
                        name: text(&b[3])?,
                    })
                })?
                .into_iter()
                .enumerate()
                .map(|(i, mut a)| {
                    a.index = i as u32;
                    a
                })
                .collect(),
            })
        })
        .collect()
}

fn list<T>(v: &Value, f: impl Fn(&Value) -> Result<T, DecodeError>) -> Result<Vec<T>, DecodeError> {
    v.as_array()
        .ok_or(DecodeError("a list"))?
        .iter()
        .map(f)
        .collect()
}

/// derive.ts `buildTimeline`, without the fold actions (edits, reactions and pins still queued) and threads.
pub(crate) fn build_timeline(
    channel_id: ChannelId,
    group: ChannelGroupState,
    rows: &[TimelineRow],
    outbox: &[OutboxRow],
    own_user: [u8; 16],
    own_device: [u8; 16],
    limit: u32,
) -> Timeline {
    let mut items = Vec::with_capacity(rows.len() + outbox.len());
    let mut stored = HashSet::new();
    for row in rows {
        if let Some(id) = row.msg_id {
            stored.insert(id);
        }
        // The core answers displayable rows only; a stored fold row is still never an item (defensive).
        if row.kind.is_some_and(|k| k != 0) {
            continue;
        }
        let own = row.sender_device == own_device;
        items.push(TimelineItem {
            key: match row.msg_id {
                Some(id) if own => format!("o{}", hex(&id)),
                _ => format!("s{}", row.seq),
            },
            state: match row.status {
                0 => TimelineItemState::Ok,
                1 => TimelineItemState::CannotRead,
                _ => TimelineItemState::Deleted,
            },
            reason: if row.status == 1 {
                row.reason.clone()
            } else {
                String::new()
            },
            sender_user: row.sender_user.map(UserId),
            sender_device: Some(DeviceId(row.sender_device)),
            own,
            bot: row.sender_kind == Some(1),
            ts: row.recv_ts,
            body: if row.status == 0 {
                row.body.clone()
            } else {
                String::new()
            },
            msg_id: row.msg_id.map(MessageId),
            seq: Some(row.seq),
            edited: row.edited_seq > 0,
            reply: row.reply.clone(),
            reactions: row.reactions.clone(),
            pinned: row.pinned,
            attachments: row.attachments.clone(),
            mention: row.mention,
        });
    }
    for row in outbox {
        // An edit, delete, reaction or pin is never an item of its own.
        if row.kind != 0 || stored.contains(&row.msg_id) {
            continue;
        }
        let failed = row.state == 2;
        items.push(TimelineItem {
            key: format!("o{}", hex(&row.msg_id)),
            state: if failed {
                TimelineItemState::Failed
            } else {
                TimelineItemState::Pending
            },
            reason: if failed {
                row.error.clone()
            } else {
                String::new()
            },
            sender_user: Some(UserId(own_user)),
            sender_device: Some(DeviceId(own_device)),
            own: true,
            bot: false,
            ts: row.created,
            body: row.body.clone(),
            msg_id: Some(MessageId(row.msg_id)),
            seq: None,
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: row.attachments.clone(),
            mention: false,
        });
    }
    Timeline {
        channel_id,
        group,
        items,
        has_earlier: rows.len() >= limit as usize,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn stored(seq: u64, device: u8, msg: Option<u8>, body: &str) -> TimelineRow {
        TimelineRow {
            seq,
            recv_ts: 100 + seq,
            status: 0,
            reason: String::new(),
            sender_user: Some([device; 16]),
            sender_device: [device; 16],
            sender_kind: Some(0),
            msg_id: msg.map(|m| [m; 16]),
            kind: Some(0),
            body: body.into(),
            edited_seq: 0,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
        }
    }

    fn queued(msg: u8, body: &str) -> OutboxRow {
        OutboxRow {
            msg_id: [msg; 16],
            state: 0,
            error: String::new(),
            created: 50,
            body: body.into(),
            kind: 0,
            reply_to: None,
            attachments: Vec::new(),
        }
    }

    #[test]
    fn a_confirmed_send_keeps_its_key_and_is_not_shown_twice() {
        let ch = ChannelId([9; 16]);
        let me = [1; 16];
        let pending = build_timeline(
            ch,
            ChannelGroupState::Active,
            &[stored(1, 2, Some(5), "hi")],
            &[queued(7, "mine")],
            me,
            me,
            100,
        );
        assert_eq!(pending.bodies(), ["hi", "mine"]);
        assert_eq!(pending.items[1].key, format!("o{}", hex(&[7; 16])));
        assert_eq!(pending.items[1].state, TimelineItemState::Pending);
        // Stored now, and the outbox still lists it (the confirm and the read raced): one item, same key.
        let both = build_timeline(
            ch,
            ChannelGroupState::Active,
            &[stored(1, 2, Some(5), "hi"), stored(2, 1, Some(7), "mine")],
            &[queued(7, "mine")],
            me,
            me,
            100,
        );
        assert_eq!(both.bodies(), ["hi", "mine"]);
        assert_eq!(both.items[1].key, pending.items[1].key);
        assert_eq!(both.items[1].seq, Some(2));
        assert_eq!(both.items[0].key, "s1");
    }
}

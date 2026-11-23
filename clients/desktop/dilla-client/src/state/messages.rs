//! `timeline:<channel>` (types.ts `TimelineState`, `TimelineItem`), built from the core's stored rows and its outbox
//! as derive.ts `buildTimeline` builds it: one item per displayable stored row (type 0), in the core's order, then one
//! per outbox post the store does not hold yet; an own stored row whose message id is known keeps its outbox key
//! (`o<msg id>`), so a sent message is never shown twice and does not change key when it is confirmed.

use std::collections::{HashMap, HashSet};

use crate::gateway::frames::Value;
use crate::sync::value::{DecodeError, Rows, arr, bin16, bin32, opt, text, u53, uint};

use super::{ChannelGroupState, ChannelId, DeviceId, MessageId, Redacted, UserId, hex};

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
#[derive(Clone, PartialEq, Eq)]
pub struct ReplyRef {
    pub msg_id: MessageId,
    /// 0 ok, 1 missing, 2 deleted.
    pub state: u8,
    pub sender_user: Option<UserId>,
    pub excerpt: String,
    pub seq: Option<u64>,
}

/// `ReactionSummary`. Its Debug prints the emoji's length only: a reaction is message content.
#[derive(Clone, PartialEq, Eq)]
pub struct Reaction {
    pub emoji: String,
    pub count: u32,
    pub mine: bool,
}

/// What the UI may know of an attachment: never its key, nonce or blob id.
#[derive(Clone, PartialEq, Eq)]
pub struct Attachment {
    pub index: u32,
    pub size: u64,
    pub mime: String,
    pub name: String,
}

/// types.ts `PendingAction`: an edit, delete, reaction or pin still in the outbox, shown on the message it aims at
/// (derive.ts `buildTimeline`'s folds).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PendingAction {
    /// The fold row's own outbox message id: retry and discard take it.
    pub msg_id: MessageId,
    /// 1 edit, 2 delete, 3 react, 4 unreact, 5 pin, 6 unpin.
    pub kind: u8,
    /// Failed (`state 2`), else pending.
    pub failed: bool,
    /// The failure's code; empty while pending.
    pub reason: String,
}

/// `TimelineItem`.
#[derive(Clone, PartialEq, Eq)]
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
    /// The edits, deletes, reactions and pins of this message still in the outbox, in queue order.
    pub actions: Vec<PendingAction>,
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
#[derive(Clone)]
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
#[derive(Clone)]
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
    /// The attachments' blob ids, in the same order: a discarded row's references are deleted (send.ts `discardSend`,
    /// controller.ts `onDiscarded`).
    pub blobs: Vec<[u8; 32]>,
}

// R21: the decrypted texts of a message (its body, a reply's excerpt, an attachment's name) never reach Debug output; it
// shows their lengths, beside the ids and states that are not content.

impl std::fmt::Debug for ReplyRef {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ReplyRef")
            .field("msg_id", &self.msg_id)
            .field("state", &self.state)
            .field("sender_user", &self.sender_user)
            .field("excerpt", &Redacted(&self.excerpt))
            .field("seq", &self.seq)
            .finish()
    }
}

impl std::fmt::Debug for Reaction {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Reaction")
            .field("emoji", &Redacted(&self.emoji))
            .field("count", &self.count)
            .field("mine", &self.mine)
            .finish()
    }
}

impl std::fmt::Debug for Attachment {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Attachment")
            .field("index", &self.index)
            .field("size", &self.size)
            .field("mime", &self.mime)
            .field("name", &Redacted(&self.name))
            .finish()
    }
}

impl std::fmt::Debug for TimelineItem {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TimelineItem")
            .field("key", &self.key)
            .field("state", &self.state)
            .field("reason", &self.reason)
            .field("sender_user", &self.sender_user)
            .field("sender_device", &self.sender_device)
            .field("own", &self.own)
            .field("bot", &self.bot)
            .field("ts", &self.ts)
            .field("body", &Redacted(&self.body))
            .field("msg_id", &self.msg_id)
            .field("seq", &self.seq)
            .field("edited", &self.edited)
            .field("reply", &self.reply)
            .field("reactions", &self.reactions)
            .field("pinned", &self.pinned)
            .field("attachments", &self.attachments)
            .field("mention", &self.mention)
            .field("actions", &self.actions)
            .finish()
    }
}

impl std::fmt::Debug for TimelineRow {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TimelineRow")
            .field("seq", &self.seq)
            .field("status", &self.status)
            .field("msg_id", &self.msg_id)
            .field("kind", &self.kind)
            .field("body", &Redacted(&self.body))
            .field("reply", &self.reply)
            .field("attachments", &self.attachments)
            .finish_non_exhaustive()
    }
}

impl std::fmt::Debug for OutboxRow {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("OutboxRow")
            .field("msg_id", &self.msg_id)
            .field("state", &self.state)
            .field("error", &self.error)
            .field("body", &Redacted(&self.body))
            .field("kind", &self.kind)
            .field("attachments", &self.attachments)
            .finish_non_exhaustive()
    }
}

impl std::fmt::Debug for PinnedItem {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PinnedItem")
            .field("msg_id", &self.msg_id)
            .field("seq", &self.seq)
            .field("excerpt", &Redacted(&self.excerpt))
            .field("pinned_seq", &self.pinned_seq)
            .field("thread_root", &self.thread_root)
            .finish_non_exhaustive()
    }
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
                blobs: list(&a[7], |v| bin32(&arr(v, 4)?[0]))?,
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
            actions: Vec::new(),
        });
    }
    for row in outbox {
        // An edit, delete, reaction or pin is never an item of its own; nor is a pending thread reply, which is not a
        // row of the channel (controller.ts `threadSends`; Task 11).
        if row.kind != 0
            || stored.contains(&row.msg_id)
            || crate::features::threads::is_thread_send(&row.msg_id)
        {
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
            actions: Vec::new(),
        });
    }
    // derive.ts: each fold (an edit, delete, reaction or pin) shows on the item it aims at, in queue order.
    for row in outbox.iter().filter(|r| r.kind != 0) {
        let Some(target) = row.reply_to else { continue };
        let Some(item) = items
            .iter_mut()
            .find(|i| i.msg_id == Some(MessageId(target)))
        else {
            continue;
        };
        let failed = row.state == 2;
        item.actions.push(PendingAction {
            msg_id: MessageId(row.msg_id),
            kind: row.kind,
            failed,
            reason: if failed {
                row.error.clone()
            } else {
                String::new()
            },
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
            blobs: Vec::new(),
        }
    }

    /// R21: a stored row, an outbox row, the timeline built from them (its items, their reply excerpts, attachment
    /// names and reaction emoji), a pinned item, and the whole `AppState` holding them, a reply target, a search, a tray
    /// file, a poll and a reminder never print a decrypted text in Debug output.
    #[test]
    fn decrypted_texts_stay_out_of_debug() {
        const SECRET: &str = "zebra-body-7";
        let ch = ChannelId([9; 16]);
        let mut row = stored(1, 1, Some(5), &format!("{SECRET} stored"));
        row.reply = Some(ReplyRef {
            msg_id: MessageId([4; 16]),
            state: 0,
            sender_user: None,
            excerpt: format!("{SECRET} excerpt"),
            seq: Some(1),
        });
        row.attachments = vec![Attachment {
            index: 0,
            size: 3,
            mime: "image/png".into(),
            name: format!("{SECRET}.png"),
        }];
        let out = queued(7, &format!("{SECRET} outbox"));
        let t = build_timeline(
            ch,
            ChannelGroupState::Active,
            std::slice::from_ref(&row),
            std::slice::from_ref(&out),
            [1; 16],
            [1; 16],
            100,
        );
        assert!(
            t.items.iter().any(|i| i.body.contains(SECRET)),
            "the body is held"
        );
        let mut t = t;
        t.items[0].reactions.push(Reaction {
            emoji: format!("{SECRET}-emoji"),
            count: 1,
            mine: true,
        });
        let pin = PinnedItem {
            msg_id: MessageId([5; 16]),
            seq: 1,
            sender_user: None,
            excerpt: format!("{SECRET} pinned"),
            pinned_by: UserId([1; 16]),
            pinned_seq: 2,
            ts: 3,
            thread_root: None,
        };
        let mut state = crate::state::AppState::default();
        state.timelines.insert(ch, t.clone());
        state.messages.pins.insert(ch, vec![pin.clone()]);
        state.threads.replying = Some(crate::state::threads::ReplyTarget {
            channel: ch,
            msg_id: MessageId([5; 16]),
            sender: None,
            excerpt: format!("{SECRET} replying"),
        });
        state.search = crate::state::search::SearchState {
            request: 1,
            query: SECRET.into(),
            hits: vec![crate::state::search::SearchHit {
                channel: ch,
                community: None,
                seq: 1,
                snippet: format!("{SECRET} snippet"),
                author: None,
                ts: None,
                thread_root: None,
            }],
            loading: false,
        };
        state.attachments.trays.insert(
            ch,
            vec![crate::state::attachments::TrayItem {
                id: "t1".into(),
                name: format!("{SECRET}.txt"),
                size: 1,
                mime: "text/plain".into(),
                image: false,
                phase: crate::state::attachments::TrayPhase::Ready,
                reason: String::new(),
            }],
        );
        state.bot.polls.insert(
            MessageId([5; 16]),
            crate::state::bot::PollView {
                id: [1; 16],
                channel: ch,
                thread: None,
                creator: UserId([1; 16]),
                question: format!("{SECRET} question"),
                answers: vec![format!("{SECRET} answer")],
                multi: false,
                created_at: 0,
                due_at: 0,
                closed: false,
                counts: vec![0],
                my_choices: None,
                revision: 1,
                previews: vec![Vec::new()],
                distinct: 0,
                capped: false,
            },
        );
        state.bot.reminders.insert(
            crate::state::CommunityId([2; 16]),
            vec![crate::state::bot::ReminderView {
                id: [2; 16],
                text: format!("{SECRET} reminder"),
                due: 0,
                timezone: "UTC".into(),
                source_channel: None,
                source_thread: None,
                state: crate::state::bot::ReminderStatus::Scheduled,
                revision: 1,
            }],
        );
        state.bot.private_results.insert(
            crate::state::bot::Conversation {
                channel: ch,
                thread: None,
            },
            vec![crate::state::bot::PrivateResult {
                id: 1,
                at: 0,
                status: crate::state::bot::ResultStatus::Success,
                text: format!("{SECRET} private"),
                provider: crate::state::bot::ResultProvider::Bot,
                hold: false,
                settled_at: None,
            }],
        );
        for shown in [
            format!("{row:?}"),
            format!("{out:?}"),
            format!("{t:#?}"),
            format!("{pin:?}"),
            format!("{state:?}"),
            format!("{state:#?}"),
        ] {
            assert!(
                !shown.contains("zebra"),
                "a decrypted text: {} bytes",
                shown.len()
            );
        }
        // The tests' way to look for a plaintext in the state (`testing::decrypted_texts`) still finds each one.
        let texts = crate::testing::decrypted_texts(&state);
        for kind in [
            "stored", "excerpt", ".png", "outbox", "pinned", "replying", "snippet", ".txt",
            "question", "answer", "reminder", "private",
        ] {
            assert!(
                texts.iter().any(|t| t.contains(SECRET) && t.contains(kind)),
                "{kind} is read"
            );
        }
    }

    /// derive.ts: an edit, delete, reaction or pin in the outbox is no item of its own; it shows on the message it aims
    /// at, pending or failed with its code, in queue order; one aimed at nothing held is dropped.
    #[test]
    fn a_fold_shows_on_its_target() {
        let ch = ChannelId([9; 16]);
        let me = [1; 16];
        let fold = |msg: u8, kind: u8, target: u8, state: u8| OutboxRow {
            kind,
            reply_to: Some([target; 16]),
            state,
            error: if state == 2 {
                "E_FORBIDDEN".into()
            } else {
                String::new()
            },
            ..queued(msg, "")
        };
        let t = build_timeline(
            ch,
            ChannelGroupState::Active,
            &[stored(1, 1, Some(5), "hi")],
            &[fold(7, 1, 5, 0), fold(8, 5, 5, 2), fold(9, 1, 6, 0)],
            me,
            me,
            100,
        );
        assert_eq!(t.bodies(), ["hi"]);
        let actions = &t.items[0].actions;
        assert_eq!(actions.len(), 2);
        assert_eq!(
            (actions[0].msg_id, actions[0].kind, actions[0].failed),
            (MessageId([7; 16]), 1, false)
        );
        assert_eq!(
            (
                actions[1].kind,
                actions[1].failed,
                actions[1].reason.as_str()
            ),
            (5, true, "E_FORBIDDEN")
        );
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

/// Someone else typing in a channel (`TypingState`): the row lives until `expires_at_ms` (unix ms, this machine's clock).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Typer {
    pub user: UserId,
    pub name: String,
    pub expires_at_ms: u64,
}

/// A pinned message (`PinnedItem`): what the pins dialog lists.
#[derive(Clone, PartialEq, Eq)]
pub struct PinnedItem {
    pub msg_id: MessageId,
    pub seq: u64,
    pub sender_user: Option<UserId>,
    pub excerpt: String,
    pub pinned_by: UserId,
    pub pinned_seq: u64,
    pub ts: u64,
    pub thread_root: Option<MessageId>,
}

/// Task 9's slice of [`super::AppState`]: typing (received and sent), the pins lists and the timelines that cannot be
/// shown (Task 2). [`Default`] is the empty slice.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct MessagesState {
    /// `typing:<channel>`: the others typing there now, oldest first.
    pub typing: HashMap<ChannelId, Vec<Typer>>,
    /// `pins:<channel>`: the pinned messages of a channel whose pins were loaded; it keeps its last value when closed.
    pub pins: HashMap<ChannelId, Vec<PinnedItem>>,
    /// The channels this device has signalled typing in, with the time of the last signal (unix ms): the 3 s throttle of
    /// gateway.ts `sendTyping`.
    pub typing_sent: HashMap<ChannelId, u64>,
    /// The channels whose typing rows have a sweep running (one per channel, however many frames arrive).
    pub typing_sweeping: HashSet<ChannelId>,
    /// Task 2 (removal recovery): the channels whose timeline cannot be shown because their removal feed cannot be
    /// read, with why (channel.ts `openChannelFlow` rejects the open with the same error). The timeline itself is
    /// absent: what the store holds is never shown while a removal may be missing from it.
    pub unavailable: HashMap<ChannelId, TimelineUnavailable>,
}

/// Why a channel's timeline cannot be shown (Task 2). Never carries message content.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum TimelineUnavailable {
    /// The instance refused the removal feed (its status and code: 403 `E_FORBIDDEN`, 404 `E_NOT_FOUND` after the
    /// resync a lost leaf gets, ...).
    FeedRefused { status: u16, code: String },
    /// The feed does not hold together (`E_REMOVAL_FEED`: a revision gap, a proof for another group, a bad page).
    FeedInvalid { code: String },
    /// A removal could not be verified (the core's code: a moderator this device cannot pin, a proof that does not
    /// check).
    RemovalUnverified { code: String },
}

/// `pins` rows: `[target_seq, msg_id, pinned_seq, by_user, author|null, excerpt, target_ts, thread_root?]`.
pub(crate) fn decode_pins(raw: &[u8]) -> Result<Vec<PinnedItem>, DecodeError> {
    Rows::of(raw)?
        .iter()
        .map(|row| {
            let a = row.as_array().ok_or(DecodeError("pin row"))?;
            if a.len() != 7 && a.len() != 8 {
                return Err(DecodeError("a pin row has 7 or 8 elements"));
            }
            Ok(PinnedItem {
                seq: uint(&a[0])?,
                msg_id: MessageId(bin16(&a[1])?),
                pinned_seq: uint(&a[2])?,
                pinned_by: UserId(bin16(&a[3])?),
                sender_user: opt(&a[4], bin16)?.map(UserId),
                excerpt: text(&a[5])?,
                ts: uint(&a[6])?,
                thread_root: match a.get(7) {
                    Some(v) => opt(v, bin16)?.map(MessageId),
                    None => None,
                },
            })
        })
        .collect()
}

#[cfg(test)]
mod pin_tests {
    use super::*;

    fn row(len: usize) -> Value {
        let mut a = vec![
            Value::Uint(9),
            Value::Bytes(vec![1; 16]),
            Value::Uint(12),
            Value::Bytes(vec![2; 16]),
            Value::Null,
            Value::Text("the plan".into()),
            Value::Uint(1_700_000_000),
        ];
        if len == 8 {
            a.push(Value::Bytes(vec![3; 16]));
        }
        Value::Array(a)
    }

    #[test]
    fn a_pin_row_has_seven_elements_or_eight_with_a_thread_root() {
        let seven = decode_pins(&Value::Array(vec![row(7)]).encode()).expect("seven");
        assert_eq!(seven[0].seq, 9);
        assert_eq!(seven[0].excerpt, "the plan");
        assert_eq!(seven[0].sender_user, None);
        assert_eq!(seven[0].thread_root, None);
        let eight = decode_pins(&Value::Array(vec![row(8)]).encode()).expect("eight");
        assert_eq!(eight[0].thread_root, Some(MessageId([3; 16])));
        assert_eq!(eight[0].pinned_by, UserId([2; 16]));
        assert!(
            decode_pins(&Value::Array(vec![Value::Array(vec![Value::Uint(1)])]).encode()).is_err()
        );
    }
}

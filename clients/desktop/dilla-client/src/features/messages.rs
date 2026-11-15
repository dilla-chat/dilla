//! Area **messages** (Task 9): edit, delete, pin and unpin, typing (sent and received) and the pins list.
//!
//! Ports: packages/client-core/src/worker/controller.ts (`fold`, `editMessage`, `deleteMessage`, `pin`, `typing`,
//! `incomingTypingFrame`, `loadPins`, `publishPins`), gateway.ts `sendTyping`.
//!
//! An edit, a delete and a pin are queued as their fold type aimed at the message (requirement 10): the timeline changes
//! only when the core folds them. Slow mode needs nothing here: the engine already holds a post back while the channel's
//! wait runs ([`crate::sync::Engine::send_request`]); the composer derives the countdown from the timeline.
//!
//! Receives op 49 `typing` from the gateway (`crate::sync::route`), through [`AreaFrames`].

use std::sync::Arc;
use std::time::Duration;

use tokio::sync::oneshot;

use crate::client::{Client, ClientError, Inner};
use crate::gateway::frames::{Frame, Op, Value};
use crate::state::messages::{Typer, decode_pins};
use crate::state::{ChannelId, ConnectionStatus, MessageId, UserId};
use crate::sync::api::try_core;
use crate::sync::route::AreaFrames;

/// The relay window of a typing signal (controller.ts: the instance's now + 10 s).
const TYPING_WINDOW_MS: u64 = 10_000;
/// gateway.ts `sendTyping`: an "on" within this of the last one is not sent again.
const TYPING_THROTTLE_MS: u64 = 3_000;

/// The stored seq of `msg` in `channel`, from wherever its row is held (review HL m3: the web takes it from the row): the
/// channel's log, else a thread of the channel (its replies or its root).
fn stored_seq(s: &crate::state::AppState, channel: ChannelId, msg: MessageId) -> Option<u64> {
    let in_log = s
        .timelines
        .get(&channel)
        .and_then(|t| t.items.iter().find(|i| i.msg_id == Some(msg)));
    let in_thread = || {
        s.threads
            .threads
            .values()
            .filter(|t| t.channel == channel)
            .flat_map(|t| t.root.iter().chain(t.replies.iter()))
            .find(|i| i.msg_id == Some(msg))
    };
    in_log.or_else(in_thread).and_then(|i| i.seq)
}

/// The messages area's handle for messages: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Messages {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Messages`] handle.
    pub fn messages(&self) -> Messages {
        Messages {
            inner: self.inner().clone(),
        }
    }
}

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| u64::try_from(d.as_millis()).unwrap_or(u64::MAX))
}

/// The fold types of envelope.rs.
const EDIT: u8 = 1;
const DELETE: u8 = 2;
const PIN: u8 = 5;
const UNPIN: u8 = 6;

impl Messages {
    /// The channel's text group, opened (a no-op when it is).
    async fn group_of(inner: &Arc<Inner>, channel: ChannelId) -> Result<[u8; 16], ClientError> {
        let known = inner
            .snapshot()
            .channel(channel)
            .cloned()
            .ok_or(ClientError::UnknownChannel)?;
        if !known.is_text() {
            return Err(ClientError::UnknownChannel);
        }
        Ok(inner
            .engine()
            .open_channel(
                Some(known.community_id.0),
                channel.0,
                known.text_group_id.map(|g| g.0),
            )
            .await?)
    }

    fn fold(
        &self,
        channel: ChannelId,
        msg: MessageId,
        kind: u8,
        body: String,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let g = Self::group_of(&inner, channel).await?;
            inner
                .engine()
                .send_request(g, kind, Some(msg.0), body)
                .await?;
            Ok(())
        })
    }

    /// Replaces the text of the own message `msg`. A blank text is refused (controller.ts `editMessage`).
    pub fn edit(
        &self,
        channel: ChannelId,
        msg: MessageId,
        text: String,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        if text.trim().is_empty() {
            let (tx, rx) = oneshot::channel();
            let _ = tx.send(Err(ClientError::Sync("E_BAD_INPUT".into())));
            return rx;
        }
        self.fold(channel, msg, EDIT, text)
    }

    /// Deletes the own message `msg` (a type-2 fold).
    pub fn delete(
        &self,
        channel: ChannelId,
        msg: MessageId,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let g = Self::group_of(&inner, channel).await?;
            // controller.ts `deleteMessage` (Task 28 item 13): a member timed out in the channel's community may still
            // remove their own message. The instance refuses every upload of theirs while the timeout lasts, a delete
            // fold included, so the message is deleted from the group through the instance instead.
            let s = inner.snapshot();
            if let (Some(community), Some(me)) = (
                s.channel(channel).map(|c| c.community_id),
                s.account.user_id,
            ) && crate::features::moderation::Moderation::of(&inner)
                .timeout_of(community, me)
                .await
                .map_err(crate::features::profile::refusal)?
                .is_some()
            {
                let seq = stored_seq(&s, channel, msg)
                    .ok_or_else(|| ClientError::Sync("E_BAD_INPUT".into()))?;
                inner
                    .api()
                    .socket_route::<crate::http::routes::DeleteGroupMessage>(
                        &[&crate::state::hex(&g), &seq.to_string()],
                        None,
                    )
                    .await
                    .map_err(crate::features::profile::refusal)?;
                return Ok(());
            }
            inner
                .engine()
                .send_request(g, DELETE, Some(msg.0), String::new())
                .await?;
            Ok(())
        })
    }

    /// Pins (`on`) or unpins `msg`.
    pub fn pin(
        &self,
        channel: ChannelId,
        msg: MessageId,
        on: bool,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        self.fold(channel, msg, if on { PIN } else { UNPIN }, String::new())
    }

    /// Reads the channel's pins into [`crate::state::messages::MessagesState::pins`]. The caller asks: when the pins
    /// dialog opens and after it unpins; a pin that folds while the dialog is open shows at the next open (no refresh
    /// is wired to the fold).
    pub fn load_pins(&self, channel: ChannelId) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let g = Self::group_of(&inner, channel).await?;
            let raw = try_core(inner.engine().core(), move |c| c.pins(&g))
                .await
                .map_err(|e| ClientError::Runtime(e.to_string()))?
                .map_err(|e| ClientError::Sync(e.code.to_owned()))?;
            let items = decode_pins(&raw).map_err(|e| ClientError::Sync(e.to_string()))?;
            inner.publish(move |s| {
                s.messages.pins.insert(channel, items);
            });
            Ok(())
        })
    }

    /// Tells the channel the person starts (`on`) or stops typing (`gateway.sendTyping`, controller.ts `typing`): an
    /// "on" is sent at most every 3 s, an "off" only after an "on"; nothing is sent while offline.
    pub fn typing(&self, channel: ChannelId, on: bool) {
        let snapshot = self.inner.snapshot();
        let sent = snapshot.messages.typing_sent.get(&channel).copied();
        if !on && sent.is_none() {
            return;
        }
        if snapshot.connection.status != ConnectionStatus::Online {
            return;
        }
        let now = now_ms();
        if on && sent.is_some_and(|t| now.saturating_sub(t) < TYPING_THROTTLE_MS) {
            return;
        }
        let frame = Frame {
            op: Op::TYPING_SET,
            n: 0,
            group_id: None,
            payload: vec![Value::Bytes(channel.0.to_vec()), Value::Uint(u64::from(on))],
        };
        let Ok(bytes) = crate::gateway::frames::encode(&frame) else {
            return;
        };
        if !self.inner.engine().send_gateway_frame(bytes) {
            return;
        }
        self.inner.publish(move |s| {
            if on {
                s.messages.typing_sent.insert(channel, now);
            } else {
                s.messages.typing_sent.remove(&channel);
            }
        });
    }
}

/// The name a typing row shows: the member's display name, else the username (controller.ts `nameOf`).
fn name_of(state: &crate::state::AppState, channel: ChannelId, user: UserId) -> String {
    state
        .channel(channel)
        .and_then(|c| state.members.get(&c.community_id))
        .and_then(|m| m.iter().find(|m| m.user_id == user))
        .map_or_else(
            // A person no member list names yet: the first eight digits of the id, as the log names an unknown author.
            || user.hex()[..8].to_owned(),
            |m| {
                if !m.display.is_empty() {
                    m.display.clone()
                } else if !m.username.is_empty() {
                    m.username.clone()
                } else {
                    user.hex()[..8].to_owned()
                }
            },
        )
}

impl AreaFrames for Messages {
    /// controller.ts `incomingTypingFrame`: `[channel, user, device, on, expires]`. The row lives on this machine's clock
    /// for the relay window, whatever the frame's own expiry says (this client keeps no estimate of the instance's
    /// clock), and a sweep removes it when it lapses.
    fn on_frame(inner: &Arc<Inner>, f: &Frame) {
        if f.op != Op::TYPING {
            return;
        }
        let (Some(channel), Some(user), Some(on)) = (
            f.payload.first().and_then(Value::as_bytes),
            f.payload.get(1).and_then(Value::as_bytes),
            f.payload.get(3).and_then(Value::as_uint),
        ) else {
            return;
        };
        let (Ok(channel), Ok(user)) = (<[u8; 16]>::try_from(channel), <[u8; 16]>::try_from(user))
        else {
            return;
        };
        let (channel, user) = (ChannelId(channel), UserId(user));
        let snap = inner.snapshot();
        // Never one's own signal; only a channel this device holds, and only while the link is up.
        if snap.account.user_id == Some(user)
            || !snap.timelines.contains_key(&channel)
            || snap.connection.status != ConnectionStatus::Online
        {
            return;
        }
        let expires = now_ms() + TYPING_WINDOW_MS;
        let name = name_of(&snap, channel, user);
        inner.publish(move |s| {
            let rows = s.messages.typing.entry(channel).or_default();
            rows.retain(|r| r.user != user && r.expires_at_ms > now_ms());
            if on == 1 {
                rows.push(Typer {
                    user,
                    name,
                    expires_at_ms: expires,
                });
            }
        });
        if on == 1 && !inner.snapshot().messages.typing_sweeping.contains(&channel) {
            // One sweep per channel, however many frames arrive: it sleeps to the nearest expiry, drops what lapsed and
            // looks again, until no row is left.
            inner.publish(move |s| {
                s.messages.typing_sweeping.insert(channel);
            });
            let swept = inner.clone();
            drop(inner.spawn(async move {
                loop {
                    let now = now_ms();
                    let nearest = swept
                        .snapshot()
                        .messages
                        .typing
                        .get(&channel)
                        .and_then(|rows| rows.iter().map(|r| r.expires_at_ms).min());
                    let Some(nearest) = nearest else {
                        // Nothing to wait for (the first publish may not have landed yet): look once more shortly.
                        tokio::time::sleep(Duration::from_millis(100)).await;
                        if swept
                            .snapshot()
                            .messages
                            .typing
                            .get(&channel)
                            .is_none_or(Vec::is_empty)
                        {
                            break;
                        }
                        continue;
                    };
                    let wait = nearest.saturating_sub(now) + 50;
                    tokio::time::sleep(Duration::from_millis(wait.max(50))).await;
                    swept.publish(move |s| {
                        if let Some(rows) = s.messages.typing.get_mut(&channel) {
                            rows.retain(|r| r.expires_at_ms > now_ms());
                        }
                    });
                }
                swept.publish(move |s| {
                    s.messages.typing_sweeping.remove(&channel);
                });
                Ok::<(), ClientError>(())
            }));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_fold_type_is_the_envelope_s() {
        assert_eq!([EDIT, DELETE, PIN, UNPIN], [1, 2, 5, 6]);
    }

    fn item(msg: u8, seq: u64) -> crate::state::TimelineItem {
        crate::state::TimelineItem {
            key: format!("s{seq}"),
            state: crate::state::TimelineItemState::Ok,
            reason: String::new(),
            sender_user: None,
            sender_device: None,
            own: true,
            bot: false,
            ts: 0,
            body: String::new(),
            msg_id: Some(MessageId([msg; 16])),
            seq: Some(seq),
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
        }
    }

    /// Review HL m3: the timed-out delete takes the message's seq from wherever its row is held, as the web takes it
    /// from the row: the channel's log, or a thread's replies or root (the thread pane's Delete).
    #[test]
    fn a_timed_out_delete_finds_the_seq_of_a_thread_reply() {
        use crate::state::threads::ThreadData;
        let channel = ChannelId([1; 16]);
        let mut s = crate::state::AppState::default();
        s.timelines.insert(
            channel,
            crate::state::Timeline {
                channel_id: channel,
                group: crate::state::ChannelGroupState::Active,
                items: vec![item(1, 10)],
                has_earlier: false,
            },
        );
        s.threads.threads.insert(
            MessageId([1; 16]),
            ThreadData {
                channel,
                root: Some(item(1, 10)),
                replies: vec![item(2, 11)],
                has_earlier: false,
                count: 1,
            },
        );
        assert_eq!(stored_seq(&s, channel, MessageId([1; 16])), Some(10));
        assert_eq!(
            stored_seq(&s, channel, MessageId([2; 16])),
            Some(11),
            "a thread reply"
        );
        assert_eq!(stored_seq(&s, channel, MessageId([3; 16])), None);
        assert_eq!(
            stored_seq(&s, ChannelId([9; 16]), MessageId([2; 16])),
            None,
            "another channel's thread"
        );
    }
}

//! The built-in commands that act on another area (builtin.ts): what each does once it may run, after its question
//! (if it asks one, [`super::confirm`]) was answered. [`dilla_client::features::bot::plan_in`] names the command
//! ([`Builtin`]); Tasks 10, 11, 24 and 25 add their commands here, each finishing the pending row it was given with
//! [`dilla_client::features::bot::Bot::finish`].
//!
//! Task 10: the message commands, `/reply /edit /delete /pin /unpin /topic` (builtin.ts:42-60, :116-152, :337-376,
//! :414-423). A decision made as the command runs reads the client's live state, not the frame's snapshot: a question
//! may have been open a while.

use std::rc::Rc;

use dilla_client::features::bot::{self, Builtin, RemovalStep};
use dilla_client::features::moderation::{Evidence, Moderation, Removal, RemovalOutcome};
use dilla_client::features::roles::{perm, viewer_channel_bits};
use dilla_client::features::server_admin::Parent;
use dilla_client::state::bot::{Conversation, ResultStatus};
use dilla_client::state::{AppState, ChannelId, CommunityId, MessageId};
use dilla_client::{Client, ClientError};
use gpui::App;
use zeroize::Zeroizing;

use crate::screens::messages::composing::MAX_LEN;
use crate::slots::SlotCx;

/// The moderation excerpt's budget in UTF-8 bytes (builtin.test.ts:204, the worker's limit).
const EXCERPT_MAX_BYTES: usize = 512;

/// builtin.ts `publish`: the codes for which the post was definitely not sent.
const NOT_SENT: [&str; 7] = [
    "E_BAD_INPUT",
    "E_FORBIDDEN",
    "E_NOT_FOUND",
    "E_ENVELOPE_LIMIT",
    "E_NOT_READY",
    "E_TRAY_NOT_READY",
    "E_OFFLINE",
];

/// What `on_send` hands on: `run` with the composer's encoding of its text (`Reply`, `Edit`), and for the removal of
/// another person's message its excerpt (shell-model.ts `excerpt` of the body, at most 512 UTF-8 bytes), read from
/// `state`, the conversation the draft was typed in.
pub fn prepared(
    run: Builtin,
    state: &AppState,
    channel: ChannelId,
    encode: &dyn Fn(&str) -> String,
) -> Builtin {
    match run {
        Builtin::Reply { to, text } => Builtin::Reply {
            to,
            text: encode(&text),
        },
        Builtin::Edit { msg, text } => Builtin::Edit {
            msg,
            text: encode(&text),
        },
        Builtin::RemoveOther {
            msg, seq, reason, ..
        } => {
            let body = bot::exact_message(state, channel, &msg.hex()).map_or("", |i| &i.body);
            let mut excerpt = crate::screens::threads::excerpt(body);
            let mut cut = excerpt.len().min(EXCERPT_MAX_BYTES);
            while !excerpt.is_char_boundary(cut) {
                cut -= 1;
            }
            excerpt.truncate(cut);
            Builtin::RemoveOther {
                msg,
                seq,
                reason,
                excerpt,
            }
        }
        other => other,
    }
}

/// Runs `run` for the pending private row `row` of `conversation`; the row says how it ended.
pub fn run(
    run: Builtin,
    row: u64,
    conversation: Conversation,
    ctx: &SlotCx,
    _window: gpui::WindowId,
    cx: &mut App,
) {
    let Some(client) = ctx.client.clone() else {
        return;
    };
    let channel = conversation.channel;
    let bot = client.bot();
    let finish = move |(status, text): (ResultStatus, String)| {
        bot.finish(conversation, row, status, text);
    };
    match run {
        Builtin::Reply { to, text } => {
            if text.trim().is_empty() {
                return finish(failed("Message cannot be empty."));
            }
            if text.len() > MAX_LEN {
                return finish(failed("Message exceeds the 4000 byte limit."));
            }
            let sent = client.threads().reply(channel, to, text);
            cx.background_executor()
                .spawn(async move {
                    finish(sent_answer(sent.await.unwrap_or_else(|_| {
                        Err(ClientError::Network("stopped".into()))
                    })));
                })
                .detach();
        }
        Builtin::Edit { msg, text } => {
            if text.len() > MAX_LEN {
                return finish(failed("Message exceeds the 4000 byte limit."));
            }
            let done = client.messages().edit(channel, msg, text);
            settle(done, "Message edited.", finish, cx);
        }
        Builtin::DeleteOwn { msg } => {
            let done = client.messages().delete(channel, msg);
            settle(done, "Message deleted.", finish, cx);
        }
        Builtin::RemoveOther {
            msg,
            seq,
            reason,
            excerpt,
        } => remove_other(&client, channel, (msg, seq), reason, excerpt, finish, cx),
        Builtin::Pin { msg, on } => {
            let done = client.messages().pin(channel, msg, on);
            let text = if on {
                "Message pinned."
            } else {
                "Message unpinned."
            };
            settle(done, text, finish, cx);
        }
        Builtin::Topic { channel, text } => {
            let reply = client.server_admin().update_channel(
                channel,
                None,
                Some(&text),
                Parent::Keep,
                None,
            );
            cx.background_executor()
                .spawn(async move {
                    finish(match reply.wait().await {
                        Ok(()) => (ResultStatus::Success, "Channel topic updated.".to_owned()),
                        Err(e) => (
                            ResultStatus::Failed,
                            bot::mutation_error(&dilla_client::sync::api::ApiError::local(
                                e.status, &e.code, "",
                            )),
                        ),
                    });
                })
                .detach();
        }
    }
}

fn failed(text: &str) -> (ResultStatus, String) {
    (ResultStatus::Failed, text.to_owned())
}

/// Waits for a message fold and answers `done`, or the refusal (builtin.ts `mutationError`).
fn settle(
    done: tokio::sync::oneshot::Receiver<Result<(), ClientError>>,
    ok: &'static str,
    finish: impl FnOnce((ResultStatus, String)) + Send + 'static,
    cx: &mut App,
) {
    cx.background_executor()
        .spawn(async move {
            finish(match done.await {
                Ok(Ok(())) => (ResultStatus::Success, ok.to_owned()),
                Ok(Err(e)) => failed(&bot::mutation_error_of(&e)),
                Err(_) => failed(&bot::mutation_error_of(&ClientError::Network(
                    "stopped".into(),
                ))),
            });
        })
        .detach();
}

/// builtin.ts `publish`: what the row says of a reply's send.
fn sent_answer(sent: Result<MessageId, ClientError>) -> (ResultStatus, String) {
    match sent {
        Ok(id) => (
            ResultStatus::Success,
            format!("Sent. Message {} is in the conversation outbox.", id.hex()),
        ),
        Err(ClientError::Sync(code)) if NOT_SENT.contains(&code.as_str()) => {
            failed(&format!("Message was not sent ({code})."))
        }
        Err(_) => failed(
            "The send result is uncertain and this attempt cannot safely be retried. Check the conversation and outbox \
             before starting another send.",
        ),
    }
}

/// builtin.ts `removeOtherMessage` (:337-376): Manage Messages is checked again against the live state, then the one
/// `remove_message`, then only the steps [`bot::removal_step`] names, on the client's runtime.
fn remove_other(
    client: &Rc<Client>,
    channel: ChannelId,
    (msg, seq): (MessageId, u64),
    reason: String,
    excerpt: String,
    finish: impl FnOnce((ResultStatus, String)) + Send + 'static,
    cx: &mut App,
) {
    let (reason, excerpt) = (Zeroizing::new(reason), Zeroizing::new(excerpt));
    let state = client.state().borrow().clone();
    let manages = viewer_channel_bits(&state, channel)
        .is_some_and(|b| b & u64::from(perm::MANAGE_MESSAGES) != 0);
    if !manages || reason.trim().is_empty() {
        return finish(failed(bot::DELETE_OTHER_REFUSED));
    }
    let Some(community) = state.channel(channel).map(|c| c.community_id) else {
        return finish(failed(
            "That message cannot be removed in this conversation.",
        ));
    };
    let job = client.moderation().run(move |mo| async move {
        Ok(removal(&mo, community, channel, msg, seq, &reason, &excerpt).await)
    });
    // A client that stopped refuses the job (E_STOPPED): the row still ends, as one whose outcome is unknown.
    cx.background_executor()
        .spawn(async move {
            finish(
                job.await
                    .unwrap_or_else(|e| failed(&bot::removal_error(&e))),
            );
        })
        .detach();
}

/// The removal's sequence: one `remove_message`, then each [`RemovalStep`] at most once.
async fn removal(
    mo: &Moderation,
    community: CommunityId,
    channel: ChannelId,
    msg: MessageId,
    seq: u64,
    reason: &str,
    excerpt: &str,
) -> (ResultStatus, String) {
    let mut o = match mo.remove_message(channel, msg, seq, reason, excerpt).await {
        Ok(o) => o,
        Err(e) => return failed(&bot::removal_error(&e)),
    };
    let (mut resolved, mut evidence_sent) = (false, false);
    loop {
        match bot::removal_step(&o, resolved, evidence_sent) {
            RemovalStep::Resolve(proof) => {
                // manual-removal.ts `resolveManualRemoval`: any failure is an unknown outcome. The evidence goes with
                // the removal's own request id (builtin.ts:362).
                let r = mo.resolve_removal(proof).await.unwrap_or(RemovalOutcome {
                    removal: Removal::Unknown,
                    proof_request_id: proof,
                    evidence_request_id: None,
                    evidence: Evidence::Unknown,
                });
                o = RemovalOutcome {
                    evidence_request_id: o.evidence_request_id,
                    ..r
                };
                resolved = true;
            }
            RemovalStep::SendEvidence(proof) => {
                o.evidence = mo
                    .send_removal_evidence(community, proof, reason, excerpt)
                    .await
                    .1;
                evidence_sent = true;
            }
            // Task 30 maps `uncertain` to its own outcome; until then an uncertain line is a failed row.
            RemovalStep::Done { status, text, .. } => return (status, text),
        }
    }
}

#[cfg(test)]
pub(super) mod tests {
    use super::*;
    use dilla_client::state::{
        ChannelGroupState, Timeline, TimelineItem, TimelineItemState, UserId,
    };

    /// A DM channel (no community) holding this account's message `[0xa; 16]` with `body`, and the state reading it.
    pub(in crate::screens::commands) fn dm_with(body: &str) -> (AppState, ChannelId, MessageId) {
        let (dm, msg, me) = (ChannelId([5; 16]), MessageId([0xa; 16]), UserId([4; 16]));
        let mut s = AppState::default();
        s.account.user_id = Some(me);
        s.timelines.insert(
            dm,
            Timeline {
                channel_id: dm,
                group: ChannelGroupState::Active,
                items: vec![TimelineItem {
                    key: "s1".into(),
                    state: TimelineItemState::Ok,
                    reason: String::new(),
                    sender_user: Some(me),
                    sender_device: None,
                    own: true,
                    bot: false,
                    ts: 0,
                    body: body.into(),
                    msg_id: Some(msg),
                    seq: Some(1),
                    edited: false,
                    reply: None,
                    reactions: Vec::new(),
                    pinned: false,
                    attachments: Vec::new(),
                    mention: false,
                    actions: Vec::new(),
                }],
                has_earlier: false,
            },
        );
        (s, dm, msg)
    }

    /// builtin.ts `publish` and `edit` send `context.encodeMessage(text)`; `removeOtherMessage` sends `excerpt(item.body)`
    /// (line breaks as spaces, 120 characters) within the worker's 512 bytes.
    #[test]
    fn a_command_carries_the_encoded_text_and_the_excerpt() {
        let encode = |t: &str| format!("<{t}>");
        let (s, dm, msg) = dm_with("first line\nsecond line");
        assert_eq!(
            prepared(
                Builtin::Reply {
                    to: msg,
                    text: "hi".into()
                },
                &s,
                dm,
                &encode
            ),
            Builtin::Reply {
                to: msg,
                text: "<hi>".into()
            }
        );
        assert_eq!(
            prepared(
                Builtin::Edit {
                    msg,
                    text: "fixed".into()
                },
                &s,
                dm,
                &encode
            ),
            Builtin::Edit {
                msg,
                text: "<fixed>".into()
            }
        );
        let remove = |s: &AppState| match prepared(
            Builtin::RemoveOther {
                msg,
                seq: 1,
                reason: "spam".into(),
                excerpt: String::new(),
            },
            s,
            dm,
            &encode,
        ) {
            Builtin::RemoveOther {
                reason, excerpt, ..
            } => (reason, excerpt),
            other => panic!("{other:?}"),
        };
        assert_eq!(
            remove(&s),
            ("spam".to_owned(), "first line second line".to_owned()),
            "the reason is not encoded"
        );
        let (long, ..) = dm_with(&"🦀".repeat(300));
        let (_, excerpt) = remove(&long);
        assert_eq!(excerpt.chars().count(), 120);
        assert!(excerpt.len() <= EXCERPT_MAX_BYTES);
    }

    /// builtin.ts:52-59: a send that definitely did not leave names its code; any other failure may have sent it.
    #[test]
    fn a_reply_that_may_have_been_sent_is_uncertain() {
        let id = MessageId([1; 16]);
        assert_eq!(
            sent_answer(Ok(id)),
            (
                ResultStatus::Success,
                format!("Sent. Message {} is in the conversation outbox.", id.hex())
            )
        );
        assert_eq!(
            sent_answer(Err(ClientError::Sync("E_OFFLINE".into()))),
            failed("Message was not sent (E_OFFLINE).")
        );
        for maybe in [
            ClientError::Sync("E_CONFLICT".into()),
            ClientError::Network("reset".into()),
        ] {
            assert!(
                sent_answer(Err(maybe))
                    .1
                    .starts_with("The send result is uncertain"),
                "uncertain"
            );
        }
    }
}

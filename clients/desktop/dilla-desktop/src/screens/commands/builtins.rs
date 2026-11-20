//! The built-in commands that act on another area (builtin.ts): what each does once it may run, after its question
//! (if it asks one, [`super::confirm`]) was answered. [`dilla_client::features::bot::plan_in`] names the command
//! ([`Builtin`]); Tasks 10, 11, 24 and 25 add their commands here, each finishing the pending row it was given with
//! [`dilla_client::features::bot::Bot::finish`].
//!
//! Task 10: the message commands, `/reply /edit /delete /pin /unpin /topic` (builtin.ts:42-60, :116-152, :337-376,
//! :414-423). A decision made as the command runs reads the client's live state, not the frame's snapshot: a question
//! may have been open a while.
//!
//! Task 11: the member moderation commands, `/timeout /untimeout /warn /unban` (builtin.ts:168-182, :204-264). Each
//! checks the member (or Ban Members) again as it runs; `/untimeout` asks its question only once the timeout was read.
//!
//! Task 25: the role commands, `/role add|remove` and `/selfrole list|add|remove` (builtin.ts:265-271, :395-413). A
//! role grant or revocation checks the role and the member again as it runs; a self role is picked only from the roles
//! read as the command runs. The server stays the authority on both.
//!
//! Task 24: the navigation, UI-opening and voice commands, `/giphy /invite /search /thread /dm /modlog /join /leave /mute
//! /unmute /deafen /undeafen` (builtin.ts:88-130, :153-162, :272-277, :400-408). Each opens its screen or changes the
//! call and answers its row; `/dm`, `/invite` and `/join` answer when the thing they wait for has ended.

use std::cell::RefCell;
use std::rc::Rc;
use std::time::Duration;

use dilla_client::calls::CallPhase;
use dilla_client::features::bot::{self, Ask, Builtin, GateAction, RemovalStep};
use dilla_client::features::moderation::{self, Evidence, Moderation, Removal, RemovalOutcome};
use dilla_client::features::roles::{perm, request_id, viewer_channel_bits};
use dilla_client::features::server_admin::{Parent, hex_id};
use dilla_client::state::bot::{Conversation, ResultStatus};
use dilla_client::state::roles::SelfRole;
use dilla_client::state::{AppState, ChannelId, CommunityId, MessageId, UserId};
use dilla_client::sync::api::ApiError;
use dilla_client::{Client, ClientError};
use gpui::App;
use zeroize::Zeroizing;

use crate::screens::messages::composing::MAX_LEN;
use crate::slots::SlotCx;

/// builtin.ts `waitForVoiceJoin`: how long `/join` waits for the call before its row says the join is still pending.
const JOIN_ANSWER_WAIT: Duration = Duration::from_secs(35);

/// How often `/join` looks at the call and the conversation (builtin.ts:309).
const JOIN_POLL: Duration = Duration::from_millis(50);

/// The row's answer once the join is neither done nor failed (builtin.ts:304; also a join cancelled without a notice).
const JOIN_PENDING: &str = "Voice join is still pending. Check the call before trying again.";

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
    window: gpui::WindowId,
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
        Builtin::Timeout {
            community,
            user,
            name,
            seconds,
        } => {
            if !still_target(&client, community, user) {
                return finish(failed(RANK_CHANGED));
            }
            let job = client.moderation().run(move |mo| async move {
                let expires = moderation::now_s() + seconds;
                mo.timeout_for(community, user, request_id(), Some(expires))
                    .await
            });
            moderated(job, format!("{name} was timed out."), finish, cx);
        }
        Builtin::Untimeout {
            community,
            user,
            name,
        } => untimeout(
            &client,
            (community, user, name),
            row,
            conversation,
            window,
            finish,
            cx,
        ),
        Builtin::UntimeoutConfirmed {
            community,
            user,
            name,
        } => {
            if !still_target(&client, community, user) {
                return finish(failed(RANK_CHANGED));
            }
            let job = client.moderation().run(move |mo| async move {
                mo.timeout_for(community, user, request_id(), None).await
            });
            moderated(job, format!("{name}’s timeout was removed."), finish, cx);
        }
        Builtin::Warn {
            community,
            user,
            name,
            reason,
        } => {
            let reason = Zeroizing::new(reason);
            if !still_target(&client, community, user) {
                return finish(failed(RANK_CHANGED));
            }
            let job = client.moderation().run(move |mo| async move {
                mo.warn(community, user, request_id(), &reason)
                    .await
                    .map(|_| ())
            });
            moderated(job, format!("{name} was warned."), finish, cx);
        }
        Builtin::Unban {
            community,
            user,
            name,
        } => {
            if !bot::may_unban(&client.state().borrow(), community) {
                return finish(failed("Your permissions or conversation changed."));
            }
            // builtin.ts:177-180: the bans are read again; a ban lifted meanwhile is not lifted twice.
            let job = client.moderation().run(move |mo| async move {
                let bans = mo.load_bans(community).await?;
                if !bans.iter().any(|b| b.user_id == user) {
                    return Ok(false);
                }
                mo.unban(community, user).await.map(|()| true)
            });
            cx.background_executor()
                .spawn(async move {
                    finish(match job.await {
                        Ok(true) => (ResultStatus::Success, format!("{name} was unbanned.")),
                        Ok(false) => failed("That member is no longer banned."),
                        Err(e) => failed(&bot::mutation_error(&e)),
                    });
                })
                .detach();
        }
        Builtin::Role {
            community,
            user,
            role,
            add,
            ..
        } => {
            // builtin.ts:398-401: the role and the member are checked again against the live state (the question may
            // have been open a while), and the row names them as they are now.
            let Some((role, member)) = bot::assignable_role(
                &client.state().borrow(),
                community,
                &user.hex(),
                &hex_id(&role),
                add,
            ) else {
                return finish(failed("Your permissions or role rank changed."));
            };
            let (user, id) = (member.user, role.id);
            let job = client.roles().run(move |r| async move {
                if add {
                    r.grant_role(community, user, id).await
                } else {
                    r.revoke_role(community, user, id).await
                }
            });
            let ok = if add {
                format!("{} added to {}.", role.name, member.name)
            } else {
                format!("{} removed from {}.", role.name, member.name)
            };
            moderated(job, ok, finish, cx);
        }
        Builtin::SelfRoleList { community } => {
            let job = client
                .roles()
                .run(move |r| async move { r.load_self_roles(community).await });
            cx.background_executor()
                .spawn(async move {
                    finish(match job.await {
                        Ok(rows) => (ResultStatus::Success, self_role_rows(&rows)),
                        Err(e) => failed(&bot::mutation_error(&e)),
                    });
                })
                .detach();
        }
        Builtin::SelfRole {
            community,
            role,
            add,
        } => {
            // builtin.ts `selfRole` (:405-413): the offered roles are read first; only one of them is picked or
            // dropped, and one already in the asked state is not sent again.
            let job = client.roles().run(move |r| async move {
                let rows = r.load_self_roles(community).await?;
                let Some(row) = rows.into_iter().find(|row| hex_id(&row.id) == role) else {
                    return Ok(failed("That role is not currently self-assignable."));
                };
                if row.self_selected == add {
                    let text = if add {
                        format!("You already have {}.", row.name)
                    } else {
                        format!("You do not have {}.", row.name)
                    };
                    return Ok((ResultStatus::Success, text));
                }
                r.select_self_role(community, row.id, add).await?;
                let verb = if add { "added" } else { "removed" };
                Ok((ResultStatus::Success, format!("{} {verb}.", row.name)))
            });
            cx.background_executor()
                .spawn(async move {
                    finish(
                        job.await
                            .unwrap_or_else(|e| failed(&bot::mutation_error(&e))),
                    );
                })
                .detach();
        }
        Builtin::Giphy { query } => {
            let community = client
                .state()
                .borrow()
                .channel(channel)
                .map(|c| c.community_id);
            let Some(community) = community else {
                return finish(failed(
                    "GIF search is available only in a community channel.",
                ));
            };
            // CommandHost.tsx:186 lists `/giphy` only where GIFs are on, so a typed one elsewhere is no command at all.
            let on = crate::screens::attachments::ui::gif_on(&client, community, cx);
            if let Err(row) = giphy_gate(on) {
                return finish(row);
            }
            crate::screens::attachments::ui::open_gifs(
                client.clone(),
                window,
                channel,
                community,
                query,
                cx,
            );
            finish((ResultStatus::Success, "GIF search opened.".to_owned()));
        }
        Builtin::Invite { community } => {
            // The row waits for the dialog: its link shows (created), or it closes first (cancelled).
            crate::screens::create::open_invite_then(
                window,
                community,
                Box::new(move |created, _| {
                    finish(if created {
                        (
                            ResultStatus::Success,
                            "Invite created and shown privately.".to_owned(),
                        )
                    } else {
                        failed("Invite creation cancelled.")
                    });
                }),
                cx,
            );
        }
        Builtin::Search { query } => {
            crate::screens::search::request_open_with(window, query, cx);
            finish((ResultStatus::Success, "Search opened.".to_owned()));
        }
        Builtin::Thread { root } => {
            crate::screens::threads::open_thread(channel, root, ctx, cx);
            finish((ResultStatus::Success, "Thread opened.".to_owned()));
        }
        Builtin::Dm { user } => open_dm(&client, user, channel, window, finish, cx),
        Builtin::Modlog { community } => {
            super::modlog_dialog::open(window, community, conversation, cx);
            finish((
                ResultStatus::Success,
                "Moderation history opened.".to_owned(),
            ));
        }
        Builtin::Join { channel: target } => join(&client, target, (window, channel), finish, cx),
        Builtin::Leave => {
            crate::screens::voice::leave(window, cx);
            finish((ResultStatus::Success, "Left voice.".to_owned()));
        }
        Builtin::Voice { muted, deafened } => {
            finish(set_voice(&client, muted, deafened));
        }
    }
}

/// `/giphy` runs only where the community's GIFs are on; elsewhere the row says it is not a command here.
fn giphy_gate(gifs_on: bool) -> Result<(), (ResultStatus, String)> {
    if gifs_on {
        Ok(())
    } else {
        Err(failed(&bot::not_a_command("giphy")))
    }
}

/// A command typed in a fixture window, which has no client: only `/modlog` has anything to show without one (its dialog
/// draws from the fixture's state), and its row ends as a client's would. The others stay pending, as before.
pub fn run_in_fixture(
    run: Builtin,
    row: u64,
    conversation: Conversation,
    window: gpui::WindowId,
    cx: &mut App,
) {
    if let Builtin::Modlog { community } = run {
        super::modlog_dialog::open(window, community, conversation, cx);
        super::finish_fixture_row(
            window,
            row,
            ResultStatus::Success,
            "Moderation history opened.".to_owned(),
            cx,
        );
    }
}

/// builtin.ts `dm` (:98-106): opens (or makes) the direct message with `user` and goes there, unless the window moved off
/// `channel`, the conversation the command was typed in, while it was made.
fn open_dm(
    client: &Rc<Client>,
    user: UserId,
    channel: ChannelId,
    window: gpui::WindowId,
    finish: impl FnOnce((ResultStatus, String)) + Send + 'static,
    cx: &mut App,
) {
    let answer = client.dms().open_dm(user);
    let handle = cx.windows().into_iter().find(|h| h.window_id() == window);
    cx.spawn(async move |cx| {
        let Ok(Ok(dm)) = answer.await else {
            return finish(failed("Could not open that direct message."));
        };
        let current = cx.update(|cx| crate::screens::threads::channel_of(window, cx));
        if current != Some(channel) {
            return finish(failed(
                "The direct message was opened, but navigation was skipped because the conversation changed.",
            ));
        }
        let went = handle.is_some_and(|handle| {
            gpui::AppContext::update_window(cx, handle, |_, window, cx| {
                crate::screens::dms::navigate(crate::screens::dms::Nav::Dm(dm), window, cx);
            })
            .is_ok()
        });
        finish(if went {
            (ResultStatus::Success, "Direct message opened.".to_owned())
        } else {
            failed("The direct message was opened, but navigation was skipped because the conversation changed.")
        });
    })
    .detach();
}

/// A row's way out that is taken once: the first answer ends it, a later one finds it gone.
#[derive(Clone)]
struct OnceRow(Rc<RefCell<Option<RowEnd>>>);

/// What ends a row, given how it ended.
type RowEnd = Box<dyn FnOnce((ResultStatus, String))>;

impl OnceRow {
    fn new(finish: impl FnOnce((ResultStatus, String)) + 'static) -> Self {
        Self(Rc::new(RefCell::new(Some(Box::new(finish)))))
    }

    fn open(&self) -> bool {
        self.0.borrow().is_some()
    }

    fn end(&self, answer: (ResultStatus, String)) {
        if let Some(finish) = self.0.borrow_mut().take() {
            finish(answer);
        }
    }
}

/// builtin.ts `waitForVoiceJoin` (:296-300): what a look at the call decides. The conversation changing comes first (the
/// join goes on, but the row can no longer say how it ended), then a call that is joined.
fn join_poll(conversation_moved: bool, joined: bool) -> Option<(ResultStatus, String)> {
    if conversation_moved {
        Some(failed(
            "Voice join is still running after this conversation changed.",
        ))
    } else if joined {
        Some((ResultStatus::Success, "Joined voice.".to_owned()))
    } else {
        None
    }
}

/// What a join's own answer says (builtin.ts:301): the notice's key when it has one. A join with no notice (cancelled,
/// or a trust refusal, which the web sets no join notice for either: `joinFailureKey` is `None`) waits out the web's 35
/// seconds, so its row is the pending sentence.
fn join_answer(answer: Result<(), dilla_client::ClientError>) -> (ResultStatus, String) {
    match answer {
        Ok(()) => (ResultStatus::Success, "Joined voice.".to_owned()),
        Err(e) => match crate::screens::voice::join_failure_key(&e) {
            Some(key) => failed(&format!("Could not join voice ({key}).")),
            None => failed(JOIN_PENDING),
        },
    }
}

/// builtin.ts `join` and `waitForVoiceJoin` (:128-130, :294-307): joins `target` and answers once the call is joined, the
/// join failed (with the notice's key), the conversation changed, or [`JOIN_ANSWER_WAIT`] passed. A call already in
/// `target` is joined; one still on its way in is waited for, not started again.
fn join(
    client: &Rc<Client>,
    target: ChannelId,
    (window, channel): (gpui::WindowId, ChannelId),
    finish: impl FnOnce((ResultStatus, String)) + Send + 'static,
    cx: &mut App,
) {
    let (joined, joining) = {
        let state = client.state();
        let state = state.borrow();
        let here = state.voice.call.channel == Some(target);
        (
            here && state.voice.phase() == CallPhase::In,
            here && state.voice.in_call(),
        )
    };
    if joined {
        return finish(join_poll(false, true).expect("joined"));
    }
    let row = OnceRow::new(finish);
    if !joining {
        let on_join = row.clone();
        crate::screens::voice::join_then(
            target,
            window,
            Box::new(move |answer, _| on_join.end(join_answer(answer))),
            cx,
        );
    }
    // The web looks every 50 ms (a join that ends in the call state, a conversation that changed) and stops at 35 s.
    let client = Rc::downgrade(client);
    cx.spawn(async move |cx| {
        let mut waited = Duration::ZERO;
        while row.open() && waited < JOIN_ANSWER_WAIT {
            let look = cx.update(|cx| {
                let joined = client.upgrade().is_some_and(|c| {
                    let state = c.state();
                    let state = state.borrow();
                    state.voice.call.channel == Some(target) && state.voice.phase() == CallPhase::In
                });
                let moved = crate::screens::threads::channel_of(window, cx) != Some(channel);
                join_poll(moved, joined)
            });
            if let Some(answer) = look {
                row.end(answer);
                return;
            }
            cx.background_executor().timer(JOIN_POLL).await;
            waited += JOIN_POLL;
        }
        row.end(failed(JOIN_PENDING));
    })
    .detach();
}

/// builtin.ts `setVoice` (:442-450): the call ends muted and deafened as asked. The deafen is set first, as the web
/// toggles it first, and the mute only where it then differs (a deafened device is muted too, and unmuting one undeafens
/// it: `Voice::set_muted`).
fn set_voice(client: &Rc<Client>, muted: bool, deafened: bool) -> (ResultStatus, String) {
    let (in_call, self_mute, self_deaf) = {
        let state = client.state();
        let state = state.borrow();
        (
            state.voice.phase() == CallPhase::In,
            state.voice.self_mute,
            state.voice.self_deaf,
        )
    };
    if !in_call {
        return failed("Join a voice call first.");
    }
    let voice = client.voice();
    let (deafen, mute) = voice_steps((self_mute, self_deaf), (muted, deafened));
    if let Some(deafened) = deafen {
        voice.set_deafened(deafened);
    }
    if let Some(muted) = mute {
        voice.set_muted(muted);
    }
    (ResultStatus::Success, "Voice settings updated.".to_owned())
}

/// The calls `/mute`, `/unmute`, `/deafen` and `/undeafen` make to end as `(muted, deafened)` from the call's
/// `(self_mute, self_deaf)` (builtin.ts:446-448): the deafen first and only if it differs, then the mute only where the
/// device, now deafened or not, still differs. A deafened device is muted too, so deafening needs no mute call, and
/// unmuting one is the call that undeafens it (`Voice::set_muted`).
fn voice_steps(
    (self_mute, self_deaf): (bool, bool),
    (muted, deafened): (bool, bool),
) -> (Option<bool>, Option<bool>) {
    let deafen = (self_deaf != deafened).then_some(deafened);
    let mute = ((self_mute || deafened) != muted).then_some(muted);
    (deafen, mute)
}

/// builtin.ts `selfrole list` (:267-271): each offered role as `[x] {name}` (held) or `[ ] {name}`, in the order read.
fn self_role_rows(rows: &[SelfRole]) -> String {
    if rows.is_empty() {
        return "No self-assignable roles are available.".to_owned();
    }
    rows.iter()
        .map(|r| format!("{} {}", if r.self_selected { "[x]" } else { "[ ]" }, r.name))
        .collect::<Vec<_>>()
        .join(" · ")
}

/// builtin.ts: a member's moderation re-checked as it runs (`moderationTarget`) fails with this.
const RANK_CHANGED: &str = "Your permissions or the member’s rank changed.";

/// builtin.ts `moderationTarget(context, member.userId, 'timeout')` against the client's live state: this account still
/// holds Moderate Members in `community` and outranks `user`.
fn still_target(client: &Rc<Client>, community: CommunityId, user: UserId) -> bool {
    bot::moderation_target_for(
        &client.state().borrow(),
        community,
        &user.hex(),
        GateAction::Timeout,
    )
    .is_some_and(|t| t.user == user)
}

/// Waits for a moderation `job` and answers `ok`, or the refusal (builtin.ts `mutationError`). A client that stopped
/// refuses the job (E_STOPPED): the row still ends.
fn moderated(
    job: impl std::future::Future<Output = Result<(), ApiError>> + Send + 'static,
    ok: String,
    finish: impl FnOnce((ResultStatus, String)) + Send + 'static,
    cx: &mut App,
) {
    cx.background_executor()
        .spawn(async move {
            finish(match job.await {
                Ok(()) => (ResultStatus::Success, ok),
                Err(e) => failed(&bot::mutation_error(&e)),
            });
        })
        .detach();
}

/// builtin.ts:245-254: `/untimeout` reads the member's timeout first. None: the row says so and nothing is asked; one:
/// the question ([`super::confirm::ask_builtin`]), its run [`Builtin::UntimeoutConfirmed`] on this same pending row.
fn untimeout(
    client: &Rc<Client>,
    (community, user, name): (CommunityId, UserId, String),
    row: u64,
    conversation: Conversation,
    window: gpui::WindowId,
    finish: impl FnOnce((ResultStatus, String)) + Send + 'static,
    cx: &mut App,
) {
    let job = client
        .moderation()
        .run(move |mo| async move { mo.timeout_of(community, user).await });
    // Weak: a client released while its timeout was read (Forget, sign-out) asks nothing, so its store can close.
    let client = Rc::downgrade(client);
    cx.spawn(async move |cx| {
        let held = job.await;
        // Released meanwhile: nothing is asked, and the row still ends (as a stopped client's does in `settle`).
        let Some(client) = client.upgrade() else {
            return finish(failed(&bot::mutation_error_of(&ClientError::Network(
                "stopped".into(),
            ))));
        };
        let ask = |cx: &mut App| {
            super::confirm::ask_builtin(
                window,
                super::confirm::AskingBuiltin {
                    ask: Ask {
                        title: format!("Remove {name}’s timeout?"),
                        body: "This restores the member’s ability to send messages.".to_owned(),
                    },
                    run: Builtin::UntimeoutConfirmed {
                        community,
                        user,
                        name: name.clone(),
                    },
                    row,
                    conversation,
                    client: Some(client),
                },
                cx,
            );
        };
        match held {
            Ok(Some(_)) => cx.update(ask),
            Ok(None) => finish(failed(&format!("{name} is not timed out."))),
            Err(e) => finish(failed(&bot::mutation_error(&e))),
        }
    })
    .detach();
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

    /// CommandHost.tsx:186: `/giphy` is no command where GIFs are off, so a typed one answers as an unknown command does.
    #[test]
    fn a_typed_giphy_is_refused_where_gifs_are_off() {
        assert_eq!(giphy_gate(true), Ok(()));
        assert_eq!(
            giphy_gate(false),
            Err(failed(
                "/giphy isn’t a command here. To post it as text, start with //."
            ))
        );
    }

    /// builtin.ts `setVoice` (:442-450): the deafen goes first and only if it differs; the mute only where the device
    /// then still differs, counting a deafened device as muted.
    #[test]
    fn the_voice_commands_call_only_what_differs() {
        // (self_mute, self_deaf) -> (muted, deafened): (deafen call, mute call)
        let cases = [
            // /mute, /unmute on a plain device
            ((false, false), (true, false), (None, Some(true))),
            ((true, false), (false, false), (None, Some(false))),
            ((true, false), (true, false), (None, None)),
            // /deafen: muted by it, so no mute call
            ((false, false), (true, true), (Some(true), None)),
            ((true, false), (true, true), (Some(true), None)),
            ((false, true), (true, true), (None, None)),
            // /undeafen keeps the mute it had; one that was not muted needs no mute call
            ((false, true), (false, false), (Some(false), None)),
            ((true, true), (true, false), (Some(false), None)),
            // /unmute while deafened: the one call is the mute's, which undeafens
            ((false, true), (false, true), (None, Some(false))),
        ];
        for (call, want, calls) in cases {
            assert_eq!(voice_steps(call, want), calls, "{call:?} to {want:?}");
        }
    }

    /// builtin.ts:296-300: a changed conversation ends the wait before a joined call does; neither leaves it running.
    #[test]
    fn a_join_poll_names_a_moved_conversation_before_a_joined_call() {
        assert_eq!(join_poll(false, false), None);
        assert_eq!(
            join_poll(false, true),
            Some((ResultStatus::Success, "Joined voice.".to_owned()))
        );
        let moved = failed("Voice join is still running after this conversation changed.");
        assert_eq!(join_poll(true, false), Some(moved.clone()));
        assert_eq!(join_poll(true, true), Some(moved));
    }

    /// builtin.ts:301: a refused join names its notice's key; a cancelled one or a trust refusal has none and so waits out
    /// the web's 35 seconds; the first answer ends the row and a second finds it gone.
    #[test]
    fn a_join_answers_with_its_notice_key_once() {
        let sync = |code: &str| Err(ClientError::Sync(code.into()));
        assert_eq!(join_answer(Ok(())).1, "Joined voice.");
        assert_eq!(
            join_answer(sync("E_CALL_FULL")).1,
            "Could not join voice (voice.full)."
        );
        assert_eq!(
            join_answer(Err(ClientError::Network("x".into()))).1,
            "Could not join voice (voice.failed)."
        );
        for none in ["E_CANCELLED", "E_UMK_CHANGED", "E_DEVICE_REVOKED"] {
            assert_eq!(join_answer(sync(none)).1, JOIN_PENDING, "{none}");
        }
        let seen = Rc::new(RefCell::new(Vec::new()));
        let row = {
            let seen = seen.clone();
            OnceRow::new(move |a| seen.borrow_mut().push(a.1))
        };
        assert!(row.open());
        row.end(failed("first"));
        row.end(failed("second"));
        assert!(!row.open());
        assert_eq!(*seen.borrow(), ["first"]);
    }

    /// builtin.ts:267-271: the self roles are listed held or not, in the order read, or the row says there are none.
    #[test]
    fn self_roles_are_listed_with_their_check() {
        let row = |n: u8, name: &str, self_selected| SelfRole {
            id: [n; 16],
            name: name.into(),
            position: 1,
            self_selected,
        };
        assert_eq!(
            self_role_rows(&[]),
            "No self-assignable roles are available."
        );
        assert_eq!(
            self_role_rows(&[row(1, "Crew", false), row(2, "Night owls", true)]),
            "[ ] Crew · [x] Night owls"
        );
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

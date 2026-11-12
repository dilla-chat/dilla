//! Area **threads** (Task 11): the reply line, the reply bar, the thread pane and the thread count.
//!
//! Ports packages/web `ThreadView.tsx`, `thread-request.ts` and packages/ui `ReplyChip`, `ThreadPane` and the reply and
//! thread parts of `MessageRow`. The slots (`crate::slots`) call the hooks below; the pane itself is an entity
//! ([`view::PaneView`]) with a composer of its own.
//!
//! What the hooks cannot be handed by their signatures is kept in one [`Ui`] global: the last [`SlotCx`] a hook saw (the
//! aside's hook gets only a route), the focus handles of the thread chips (a closing pane gives focus back to its chip),
//! and, in a fixture with no client, the open thread and the reply target a running client would keep in
//! `AppState::threads`.

use std::collections::{HashMap, HashSet};
use std::rc::Rc;

use dilla_client::state::{
    AppState, ChannelId, CommunityId, MessageId, TimelineItem, TimelineItemState, UserId,
    threads::{OpenThread, ReplyTarget},
};
use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::threads::{
    ReplyBar, ReplyLine, ReplyState, ThreadChip, reply_line, thread_chip,
};
use gpui::{
    AnyElement, App, AppContext as _, Entity, FocusHandle, Global, IntoElement, ParentElement,
    Window, div,
};

use crate::router::Route;
use crate::screens::shell::Clock;
use crate::slots::{Extra, MessageAction, SlotCx};
use crate::strings::{t, tf};
use crate::{format, tracked_box, tracked_focus};
use dilla_client::Area;

pub mod keys;
mod view;

pub use view::PaneView;

/// The longest excerpt the reply bar and the reply line keep (shell-model.ts `excerpt`, `max = 120`).
const EXCERPT_MAX: usize = 120;

/// shell-model.ts `excerpt`: line breaks become spaces, at most 120 characters.
pub fn excerpt(body: &str) -> String {
    body.replace(['\r', '\n'], " ")
        .chars()
        .take(EXCERPT_MAX)
        .collect()
}

/// What the area keeps between frames.
#[derive(Default)]
struct Ui {
    /// The latest view a hook was drawn for.
    ctx: Option<SlotCx>,
    pane: Option<Entity<PaneView>>,
    /// A thread chip's focus handle, by root: Enter and Space press it, and a closing pane gives focus back to it.
    chips: HashMap<MessageId, FocusHandle>,
    /// Channels this client was asked to keep the summaries of.
    tracked: HashSet<ChannelId>,
    /// With no client (a fixture): the open thread and the reply target.
    local_open: Option<OpenThread>,
    local_last: Option<OpenThread>,
    local_replying: Option<ReplyTarget>,
    /// A reply line was pressed: the message to scroll to and flash. The log (Task 9) takes it.
    jump: Option<MessageId>,
}

impl Global for Ui {}

fn ui(cx: &mut App) -> &mut Ui {
    cx.default_global::<Ui>()
}

/// Remembers `ctx` for the hooks that get none and asks the client to keep `ctx.channel`'s thread summaries.
fn remember(ctx: &SlotCx, cx: &mut App) {
    let mut start = None;
    {
        let u = ui(cx);
        u.ctx = Some(ctx.clone());
        if let (Some(client), Some(channel)) = (&ctx.client, ctx.channel)
            && u.tracked.insert(channel)
        {
            start = Some((client.clone(), channel));
        }
    }
    if let Some((client, channel)) = start {
        client.threads().track(channel);
    }
}

/// The last [`SlotCx`] a hook was drawn for.
fn current(cx: &mut App) -> Option<SlotCx> {
    ui(cx).ctx.clone()
}

/// The open thread: the client's, or a fixture's own.
fn open_of(ctx: &SlotCx, cx: &mut App) -> Option<OpenThread> {
    if ctx.client.is_some() {
        ctx.state.threads.open
    } else {
        ui(cx).local_open
    }
}

/// The reply target: the client's, or a fixture's own.
fn replying_of(ctx: &SlotCx, cx: &mut App) -> Option<ReplyTarget> {
    if ctx.client.is_some() {
        ctx.state.threads.replying.clone()
    } else {
        ui(cx).local_replying.clone()
    }
}

/// Opens the thread of `root` in `channel`.
pub fn open_thread(channel: ChannelId, root: MessageId, ctx: &SlotCx, cx: &mut App) {
    let open = OpenThread { channel, root };
    match &ctx.client {
        Some(client) => drop(client.threads().open(channel, root)),
        None => {
            let u = ui(cx);
            u.local_open = Some(open);
            u.local_last = Some(open);
        }
    }
    cx.refresh_windows();
}

/// Closes the aside and gives focus back to the thread's chip, if it is on screen.
pub fn close_thread(window: &mut Window, cx: &mut App) {
    let Some(ctx) = current(cx) else { return };
    let Some(open) = open_of(&ctx, cx) else {
        return;
    };
    match &ctx.client {
        Some(client) => client.threads().close(),
        None => ui(cx).local_open = None,
    }
    if let Some(handle) = ui(cx).chips.get(&open.root).cloned() {
        window.focus(&handle, cx);
    }
    cx.refresh_windows();
}

/// Opens or closes the aside. The channel header's button (Task 8) calls it: it closes the open thread, else reopens the
/// last one directly, with no list in between (e2e Q9).
pub fn toggle_pane(cx: &mut App) {
    let Some(ctx) = current(cx) else { return };
    if open_of(&ctx, cx).is_some() {
        match &ctx.client {
            Some(client) => client.threads().close(),
            None => ui(cx).local_open = None,
        }
    } else {
        match &ctx.client {
            Some(client) => drop(client.threads().reopen_last()),
            None => {
                let u = ui(cx);
                u.local_open = u.local_last;
            }
        }
    }
    cx.refresh_windows();
}

/// Aims the composer at `target` (or at nothing).
pub fn set_replying(target: Option<ReplyTarget>, ctx: &SlotCx, cx: &mut App) {
    match &ctx.client {
        Some(client) => client.threads().set_replying(target),
        None => ui(cx).local_replying = target,
    }
    cx.refresh_windows();
}

/// What a send from the channel composer takes along: the message it replies to, once. The shell's send (Task 8 and 9)
/// calls it with the channel it sends to; a reply sends through [`dilla_client::features::threads::Threads::reply`].
pub fn take_reply(ctx: &SlotCx, channel: ChannelId, cx: &mut App) -> Option<MessageId> {
    let target = replying_of(ctx, cx).filter(|t| t.channel == channel)?;
    set_replying(None, ctx, cx);
    Some(target.msg_id)
}

/// The message a pressed reply line asked to see, once. The log scrolls to it and flashes it (Task 9).
pub fn take_jump(cx: &mut App) -> Option<MessageId> {
    ui(cx).jump.take()
}

/// The name a person goes by in `community`.
pub(crate) fn name_of(
    state: &AppState,
    community: Option<CommunityId>,
    user: Option<UserId>,
) -> String {
    let Some(user) = user else {
        return String::new();
    };
    community
        .and_then(|c| state.members.get(&c))
        .into_iter()
        .flatten()
        .find(|m| m.user_id == user)
        .map_or_else(
            // The log (timeline.rs) names a sender no member list holds the same way.
            || "unknown".to_owned(),
            |m| {
                if m.display.is_empty() {
                    m.username.clone()
                } else {
                    m.display.clone()
                }
            },
        )
}

/// `HH:MM` in the clock's zone.
pub(crate) fn time_of(ts: u64, clock: Clock) -> String {
    match clock {
        Clock::Fixed { utc: true, .. } => format::time_in(ts, &|_| {
            chrono::FixedOffset::east_opt(0).expect("zero is an offset")
        }),
        _ => format::time(ts),
    }
}

fn readable(item: &TimelineItem) -> bool {
    matches!(
        item.state,
        TimelineItemState::Ok | TimelineItemState::Pending | TimelineItemState::Failed
    )
}

/// The reply line above a reply's head (`slots::message_extras`, first).
pub fn reply_chip(
    item: &TimelineItem,
    ctx: &SlotCx,
    _window: &mut Window,
    cx: &mut App,
) -> Option<Extra> {
    remember(ctx, cx);
    let reply = item.reply.as_ref()?;
    let state = match reply.state {
        0 => ReplyState::Ok,
        1 => ReplyState::Missing,
        _ => ReplyState::Deleted,
    };
    let author = name_of(&ctx.state, ctx.community, reply.sender_user);
    let text = match state {
        ReplyState::Ok => excerpt(&reply.excerpt),
        ReplyState::Missing => t("shell.message.replyMissing").to_owned(),
        ReplyState::Deleted => t("shell.message.replyDeleted").to_owned(),
    };
    let target = reply.msg_id;
    let mut line = ReplyLine::new("reply-line", author, text, state);
    if state != ReplyState::Missing {
        line = line.on_jump(move |_, _, cx| {
            ui(cx).jump = Some(target);
            cx.refresh_windows();
        });
    }
    Some(Extra {
        element: tracked_box("reply-line", line).into_any_element(),
        height: reply_line::height(),
    })
}

/// A thread's reply count under its first message (`slots::message_extras`, after the reactions).
pub fn thread_count(
    item: &TimelineItem,
    ctx: &SlotCx,
    _window: &mut Window,
    cx: &mut App,
) -> Option<Extra> {
    remember(ctx, cx);
    let (channel, root) = (ctx.channel?, item.msg_id?);
    let summary = ctx
        .state
        .threads
        .summary(channel, root)
        .filter(|s| s.count > 0)?;
    let label = if summary.count == 1 {
        t("thread.summary.one").to_owned()
    } else {
        tf(
            "thread.summary.other",
            &[("count", &summary.count.to_string())],
        )
    };
    let last = tf(
        "thread.summary.last",
        &[("time", &time_of(summary.last_ts, ctx.clock))],
    );
    let focus = ui(cx)
        .chips
        .get(&root)
        .cloned()
        .unwrap_or_else(|| cx.focus_handle().tab_stop(true));
    ui(cx).chips.insert(root, focus.clone());
    let opener = ctx.clone();
    let probe = focus.clone();
    let chip = ThreadChip::new("thread-chip", label)
        .last(last)
        .focus(focus)
        .on_open(move |_, _, cx| open_thread(channel, root, &opener, cx));
    Some(Extra {
        element: tracked_focus("thread-chip", &probe, chip).into_any_element(),
        height: thread_chip::height(),
    })
}

/// This area's entries in the message toolbar and context menu: reply, and open the thread (a reply in a thread has none).
pub fn message_actions(item: &TimelineItem) -> Vec<MessageAction> {
    let (Some(msg_id), true) = (item.msg_id, readable(item)) else {
        return Vec::new();
    };
    let target = (item.sender_user, excerpt(&item.body));
    let reply = MessageAction {
        id: "reply".into(),
        label: "shell.message.reply",
        icon: IconName::Reply,
        area: Area::Threads,
        run: Rc::new(move |ctx, _window, cx| {
            let Some(channel) = ctx.channel else { return };
            set_replying(
                Some(ReplyTarget {
                    channel,
                    msg_id,
                    sender: target.0,
                    excerpt: target.1.clone(),
                }),
                ctx,
                cx,
            );
        }),
    };
    let thread = MessageAction {
        id: "thread".into(),
        label: "thread.open",
        icon: IconName::MessagesSquare,
        area: Area::Threads,
        run: Rc::new(move |ctx, _window, cx| {
            if let Some(channel) = ctx.channel {
                open_thread(channel, msg_id, ctx, cx);
            }
        }),
    };
    vec![reply, thread]
}

/// The "replying to" bar over the composer (`slots::composer_leading`, first).
pub fn reply_bar(ctx: &SlotCx, _window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    remember(ctx, cx);
    let target = replying_of(ctx, cx).filter(|t| Some(t.channel) == ctx.channel)?;
    let name = name_of(&ctx.state, ctx.community, target.sender);
    let label = tf("shell.composer.replying", &[("name", &name)]);
    let cleared = ctx.clone();
    let bar = ReplyBar::new(
        "reply-bar",
        "reply-cancel",
        &label,
        &name,
        target.excerpt.clone(),
    )
    .wrap_cancel(|cancel| tracked_box("reply-cancel", cancel).into_any_element())
    .on_cancel(move |_, _, cx| set_replying(None, &cleared, cx));
    Some(
        div()
            .child(tracked_box("reply-bar", bar))
            .into_any_element(),
    )
}

/// The thread pane (`slots::aside`): `None` while no thread is open.
pub fn pane(_route: &Route, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let ctx = current(cx)?;
    open_of(&ctx, cx)?;
    let existing = ui(cx).pane.clone();
    let view = match existing {
        Some(view) => view,
        None => {
            let view = cx.new(|cx| PaneView::new(window, cx));
            ui(cx).pane = Some(view.clone());
            view
        }
    };
    Some(view.into_any_element())
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::{Channel, ChannelGroupState, DeviceId, Member};
    use gpui::TestAppContext;
    use std::sync::Arc;

    #[test]
    fn an_excerpt_is_one_line_of_at_most_120_characters() {
        assert_eq!(excerpt("a\r\nb\nc"), "a  b c");
        assert_eq!(excerpt(&"x".repeat(300)).chars().count(), 120);
        assert_eq!(excerpt("åäö"), "åäö");
    }

    fn item(state: TimelineItemState, body: &str) -> TimelineItem {
        TimelineItem {
            key: "s1".into(),
            state,
            reason: String::new(),
            sender_user: Some(UserId([2; 16])),
            sender_device: Some(DeviceId([2; 16])),
            own: false,
            bot: false,
            ts: 1_794_481_700,
            body: body.into(),
            msg_id: Some(MessageId([7; 16])),
            seq: Some(1),
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
        }
    }

    #[test]
    fn a_readable_message_offers_reply_and_thread_and_others_offer_nothing() {
        let ids = |i: &TimelineItem| {
            message_actions(i)
                .iter()
                .map(|a| a.id.to_string())
                .collect::<Vec<_>>()
        };
        assert_eq!(ids(&item(TimelineItemState::Ok, "hi")), ["reply", "thread"]);
        assert_eq!(
            ids(&item(TimelineItemState::Pending, "hi")),
            ["reply", "thread"]
        );
        assert!(ids(&item(TimelineItemState::Deleted, "")).is_empty());
        assert!(ids(&item(TimelineItemState::CannotRead, "")).is_empty());
        let mut unsent = item(TimelineItemState::Pending, "hi");
        unsent.msg_id = None;
        assert!(ids(&unsent).is_empty(), "no message id, nothing to name");
    }

    /// The toolbar's reply action aims the composer: the bar names the person and the excerpt, a send of the channel takes
    /// the target once, and another channel's send takes none.
    #[gpui::test]
    fn the_reply_action_aims_the_composer_once(cx: &mut TestAppContext) {
        let (community, channel, other) =
            (CommunityId([1; 16]), ChannelId([3; 16]), ChannelId([4; 16]));
        let mut state = AppState::default();
        state.members.insert(
            community,
            vec![Member {
                user_id: UserId([2; 16]),
                username: "bjorn".into(),
                display: "Björn".into(),
                kind: 0,
                role_ids: Vec::new(),
                status_msg: String::new(),
                bio: String::new(),
            }],
        );
        state.channels.push(Channel {
            id: channel,
            community_id: community,
            kind: 0,
            mode: 0,
            name: "general".into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::Active,
        });
        let ctx = SlotCx {
            state: Arc::new(state),
            community: Some(community),
            channel: Some(channel),
            ..SlotCx::default()
        };
        let window = cx.add_empty_window();
        window.update(|window, cx| {
            assert!(reply_bar(&ctx, window, cx).is_none(), "no target, no bar");
            let reply = &message_actions(&item(TimelineItemState::Ok, "two\nlines"))[0];
            (reply.run)(&ctx, window, cx);
            let target = replying_of(&ctx, cx).expect("aimed");
            assert_eq!(target.excerpt, "two lines");
            assert_eq!(target.msg_id, MessageId([7; 16]));
            assert!(reply_bar(&ctx, window, cx).is_some(), "the bar shows");
            assert_eq!(
                take_reply(&ctx, other, cx),
                None,
                "another channel takes none"
            );
            assert_eq!(take_reply(&ctx, channel, cx), Some(MessageId([7; 16])));
            assert!(reply_bar(&ctx, window, cx).is_none(), "taken once");
            assert_eq!(
                name_of(&ctx.state, ctx.community, Some(UserId([2; 16]))),
                "Björn"
            );
        });
    }
}

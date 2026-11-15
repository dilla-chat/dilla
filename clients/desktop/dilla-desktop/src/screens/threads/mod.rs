//! Area **threads** (Task 11): the reply line, the reply bar, the thread pane and the thread count.
//!
//! Ports packages/web `ThreadView.tsx`, `thread-request.ts` and packages/ui `ReplyChip`, `ThreadPane` and the reply and
//! thread parts of `MessageRow`. The slots (`crate::slots`) call the hooks below; the pane itself is an entity
//! ([`view::PaneView`]) with a composer of its own.
//!
//! What the hooks cannot be handed by their signatures is kept in one [`Ui`] global: per window, the [`SlotCx`] its shell
//! last drew (the aside's hook gets only a route), its pane, its pending jump and the focus handles of its thread chips (a
//! closing pane gives focus back to its chip); and, in a fixture with no client, the open thread and the reply target a running client would keep in
//! `AppState::threads`.

use std::collections::HashMap;
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
    Window, WindowId, div,
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

/// One window's part of the area: its shell's view, its thread pane and the original a reply line in it asked for.
/// Keyed by window, so with two instances open a thread reply typed in one window is never sent through the other's
/// client or into its thread.
#[derive(Default)]
struct PerWindow {
    /// The view this window's shell last drew ([`sync`]).
    ctx: Option<SlotCx>,
    pane: Option<Entity<PaneView>>,
    /// A reply line was pressed: the message to scroll to and flash. This window's log (Task 9) takes it.
    jump: Option<MessageId>,
    /// This window's thread chips' focus handles, by root: Enter and Space press one, and a closing pane gives focus
    /// back to it (never to a chip of the same thread drawn in another window).
    chips: HashMap<MessageId, FocusHandle>,
}

/// What the area keeps between frames.
#[derive(Default)]
struct Ui {
    windows: HashMap<WindowId, PerWindow>,
    /// The channel each client (by the address of its `Rc<Client>`) was asked to keep the summaries of: one per client, so
    /// leaving a channel untracks it, and one client's channel is never another's.
    tracked: HashMap<usize, ChannelId>,
    /// With no client (a fixture): the open thread and the reply target.
    local_open: Option<OpenThread>,
    local_last: Option<OpenThread>,
    local_replying: Option<ReplyTarget>,
}

impl Global for Ui {}

fn ui(cx: &mut App) -> &mut Ui {
    cx.default_global::<Ui>()
}

/// Lets go of `client` (Forget, sign-out: `crate::release::release_client`): every window whose view held it drops its
/// part (the view, the pane drawn for it, its jump and chips), and its tracked channel goes.
pub fn release(client: &std::rc::Rc<dilla_client::Client>, cx: &mut App) {
    let u = ui(cx);
    u.tracked.remove(&(std::rc::Rc::as_ptr(client) as usize));
    u.windows.retain(|_, w| {
        !w.ctx.as_ref().is_some_and(|ctx| {
            ctx.client
                .as_ref()
                .is_some_and(|c| std::rc::Rc::ptr_eq(c, client))
        })
    });
}

fn wid(window: &Window) -> WindowId {
    window.window_handle().window_id()
}

fn per_window(window: WindowId, cx: &mut App) -> &mut PerWindow {
    ui(cx).windows.entry(window).or_default()
}

/// Asks the client to keep `ctx.channel`'s thread summaries.
fn remember(ctx: &SlotCx, cx: &mut App) {
    let mut change = None;
    {
        let u = ui(cx);
        if let (Some(client), Some(channel)) = (&ctx.client, ctx.channel) {
            let key = Rc::as_ptr(client) as usize;
            let old = u.tracked.insert(key, channel);
            if old != Some(channel) {
                change = Some((client.clone(), old, channel));
            }
        }
    }
    if let Some((client, old, channel)) = change {
        let threads = client.threads();
        // The route left the old channel: its summaries are no longer kept.
        if let Some(old) = old {
            threads.untrack(old);
        }
        threads.track(channel);
    }
}

/// `window`'s shell view, each time it draws (the shell calls it before its slots): that window's pane reads this one,
/// never another window's. Windows that are gone are let go (with the client their view held).
pub fn sync(ctx: &SlotCx, window: &Window, cx: &mut App) {
    remember(ctx, cx);
    let live: std::collections::HashSet<WindowId> =
        cx.windows().iter().map(|w| w.window_id()).collect();
    let here = wid(window);
    ui(cx).windows.retain(|w, _| *w == here || live.contains(w));
    per_window(here, cx).ctx = Some(ctx.clone());
}

/// Whether a thread is open in `ctx`'s view: the header draws its Threads button pressed.
pub fn is_open(ctx: &SlotCx, cx: &mut App) -> bool {
    open_of(ctx, cx).is_some()
}

/// Escape in the channel composer: the reply aimed at `ctx`'s channel is cancelled. True when there was one.
pub fn cancel_reply(ctx: &SlotCx, cx: &mut App) -> bool {
    let aimed = replying_of(ctx, cx).is_some_and(|t| Some(t.channel) == ctx.channel);
    if aimed {
        set_replying(None, ctx, cx);
    }
    aimed
}

/// Whether the viewer may send in `ctx`'s channel: MessageItem.tsx `canSend` (unknown bits allow, as the web does).
fn can_send_in(ctx: &SlotCx) -> bool {
    ctx.channel
        .map(|c| ctx.state.threads.can_send(c))
        .unwrap_or(true)
}

/// Whether the message a reply names is no longer readable here (deleted, removed, cannot be read): the bar must not keep
/// its excerpt, so the target is dropped.
fn original_gone(ctx: &SlotCx, target: &ReplyTarget) -> bool {
    ctx.state
        .timelines
        .get(&target.channel)
        .and_then(|t| t.items.iter().find(|i| i.msg_id == Some(target.msg_id)))
        .is_some_and(|i| !readable(i))
}

/// Lets go of `window`'s part as it closes (`windows::on_closed`): its pane and the view (with its client) it held.
pub fn window_closed(window: WindowId, cx: &mut App) {
    if cx.has_global::<Ui>() {
        ui(cx).windows.remove(&window);
    }
}

/// The [`SlotCx`] `window`'s shell last drew.
fn current(window: WindowId, cx: &mut App) -> Option<SlotCx> {
    ui(cx).windows.get(&window)?.ctx.clone()
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
pub(crate) fn replying_of(ctx: &SlotCx, cx: &mut App) -> Option<ReplyTarget> {
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
    let Some(ctx) = current(wid(window), cx) else {
        return;
    };
    let Some(open) = open_of(&ctx, cx) else {
        return;
    };
    match &ctx.client {
        Some(client) => client.threads().close(),
        None => ui(cx).local_open = None,
    }
    if let Some(handle) = per_window(wid(window), cx).chips.get(&open.root).cloned() {
        window.focus(&handle, cx);
    }
    cx.refresh_windows();
}

/// Opens or closes the aside of the shell whose view is `ctx`. The channel header's button (Task 8) calls it: it closes
/// the open thread, else reopens the last one directly, with no list in between (e2e Q9).
pub fn toggle_pane(ctx: &SlotCx, cx: &mut App) {
    if open_of(ctx, cx).is_some() {
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
    // A reply to a message that has since gone is not sent as one.
    (!original_gone(ctx, &target)).then_some(target.msg_id)
}

/// The message a pressed reply line in `window` asked to see, once. That window's log scrolls to it and flashes it
/// (Task 9).
pub fn take_jump(window: &Window, cx: &mut App) -> Option<MessageId> {
    ui(cx).windows.get_mut(&wid(window))?.jump.take()
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
        line = line.on_jump(move |_, window, cx| {
            per_window(wid(window), cx).jump = Some(target);
            window.refresh();
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
    window: &mut Window,
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
    let here = wid(window);
    let focus = per_window(here, cx)
        .chips
        .get(&root)
        .cloned()
        .unwrap_or_else(|| cx.focus_handle().tab_stop(true));
    per_window(here, cx).chips.insert(root, focus.clone());
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
pub fn message_actions(item: &TimelineItem, ctx: &SlotCx) -> Vec<MessageAction> {
    // MessageItem.tsx: reply and thread lead to the composer, so a reader who cannot send there is not offered them, and
    // only a stored, readable message (`state ok`, a seq) can be replied to or threaded.
    let (Some(msg_id), true, true) = (
        item.msg_id,
        item.state == TimelineItemState::Ok && item.seq.is_some(),
        can_send_in(ctx),
    ) else {
        return Vec::new();
    };
    let target = (item.sender_user, excerpt(&item.body));
    let reply = MessageAction {
        id: "reply".into(),
        label: "shell.message.reply",
        icon: IconName::Reply,
        area: Area::Threads,
        run: Rc::new(move |ctx, _window, cx| {
            // The bits may have changed since the menu was drawn.
            let Some(channel) = ctx.channel.filter(|_| can_send_in(ctx)) else {
                return;
            };
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
            if let Some(channel) = ctx.channel.filter(|_| can_send_in(ctx)) {
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
    if original_gone(ctx, &target) {
        set_replying(None, ctx, cx);
        return None;
    }
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
    let here = wid(window);
    let ctx = current(here, cx)?;
    open_of(&ctx, cx)?;
    let existing = per_window(here, cx).pane.clone();
    let view = match existing {
        Some(view) => view,
        None => {
            let view = cx.new(|cx| PaneView::new(window, cx));
            per_window(here, cx).pane = Some(view.clone());
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
            message_actions(i, &SlotCx::default())
                .iter()
                .map(|a| a.id.to_string())
                .collect::<Vec<_>>()
        };
        assert_eq!(ids(&item(TimelineItemState::Ok, "hi")), ["reply", "thread"]);
        assert!(
            ids(&item(TimelineItemState::Pending, "hi")).is_empty(),
            "a pending message has no seq to reply to (MessageItem.tsx: ok, a msg id and a seq)"
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
            let reply = &message_actions(&item(TimelineItemState::Ok, "two\nlines"), &ctx)[0];
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

    fn channel_ctx(bits: Option<u64>) -> (SlotCx, ChannelId) {
        let (community, channel) = (CommunityId([1; 16]), ChannelId([3; 16]));
        let mut state = AppState::default();
        state.channels.push(Channel {
            id: channel,
            community_id: community,
            kind: 0,
            mode: 0,
            name: "news".into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::Active,
        });
        if let Some(bits) = bits {
            state.threads.channel_bits.insert(channel, bits);
        }
        let ctx = SlotCx {
            state: Arc::new(state),
            community: Some(community),
            channel: Some(channel),
            ..SlotCx::default()
        };
        (ctx, channel)
    }

    /// MessageItem.tsx `canSend`: a read-only channel offers neither reply nor thread; unknown bits and the send bit allow.
    #[gpui::test]
    fn a_read_only_channel_offers_neither_action(cx: &mut TestAppContext) {
        use dilla_client::state::threads::SEND_MESSAGES;
        let window = cx.add_empty_window();
        window.update(|window, cx| {
            let ids = |ctx: &SlotCx| {
                message_actions(&item(TimelineItemState::Ok, "hi"), ctx)
                    .iter()
                    .map(|a| a.id.to_string())
                    .collect::<Vec<_>>()
            };
            assert!(
                ids(&channel_ctx(Some(0)).0).is_empty(),
                "no send bit: nothing to offer"
            );
            let (allowed, _) = channel_ctx(Some(SEND_MESSAGES | 1));
            assert_eq!(ids(&allowed), ["reply", "thread"]);
            assert_eq!(
                ids(&channel_ctx(None).0),
                ["reply", "thread"],
                "unknown bits allow, as the web does"
            );
            // A menu drawn while sending was allowed does nothing once the bit is gone (re-checked when chosen).
            let reply = &message_actions(&item(TimelineItemState::Ok, "hi"), &allowed)[0];
            let (denied, _) = channel_ctx(Some(0));
            (reply.run)(&denied, window, cx);
            assert_eq!(replying_of(&denied, cx), None);
            // Escape cancels an aimed reply once.
            (reply.run)(&allowed, window, cx);
            assert!(cancel_reply(&allowed, cx));
            assert!(!cancel_reply(&allowed, cx));
        });
    }

    /// A reply bar over a message that has become unreadable drops its target and blanks the excerpt.
    #[gpui::test]
    fn the_reply_bar_drops_a_target_that_was_deleted(cx: &mut TestAppContext) {
        let window = cx.add_empty_window();
        window.update(|window, cx| {
            let (ctx, channel) = channel_ctx(None);
            let mut readable_state = (*ctx.state).clone();
            readable_state.timelines.insert(
                channel,
                dilla_client::state::Timeline {
                    channel_id: channel,
                    group: ChannelGroupState::Active,
                    items: vec![item(TimelineItemState::Ok, "secret words")],
                    has_earlier: false,
                },
            );
            let ctx = SlotCx {
                state: Arc::new(readable_state.clone()),
                ..ctx
            };
            let reply = &message_actions(&item(TimelineItemState::Ok, "secret words"), &ctx)[0];
            (reply.run)(&ctx, window, cx);
            assert!(reply_bar(&ctx, window, cx).is_some());
            // The original is deleted: the bar goes, the target with it, and no send names it.
            let mut gone = readable_state;
            gone.timelines.get_mut(&channel).unwrap().items[0].state = TimelineItemState::Deleted;
            let ctx = SlotCx {
                state: Arc::new(gone),
                ..ctx
            };
            assert!(reply_bar(&ctx, window, cx).is_none());
            assert_eq!(replying_of(&ctx, cx), None, "the target was dropped");
            let target = ReplyTarget {
                channel,
                msg_id: MessageId([7; 16]),
                sender: None,
                excerpt: "secret words".into(),
            };
            set_replying(Some(target), &ctx, cx);
            assert_eq!(
                take_reply(&ctx, channel, cx),
                None,
                "a send is not a reply to a deleted message"
            );
        });
    }

    struct Blank;
    impl gpui::Render for Blank {
        fn render(&mut self, _: &mut Window, _: &mut gpui::Context<Self>) -> impl IntoElement {
            div()
        }
    }

    /// Review C2: each window's thread pane reads and sends through its own shell's view. Window A (may send) drawn
    /// first and window B (read-only) last: Enter in A's pane still sends, under A's view; Enter in B's is refused.
    #[gpui::test]
    fn each_window_s_thread_pane_sends_through_its_own_view(cx: &mut TestAppContext) {
        use dilla_client::state::threads::SEND_MESSAGES;
        use gpui_component::input::InputEvent;
        let (allowed, channel) = channel_ctx(Some(SEND_MESSAGES | 1));
        let (read_only, _) = channel_ctx(Some(0));
        let root = MessageId([7; 16]);
        let a = cx.add_window(|_, _| Blank);
        let b = cx.add_window(|_, _| Blank);
        let draw = |ctx: &SlotCx, window: &mut Window, cx: &mut App| {
            sync(ctx, window, cx);
            pane(&Route::Root, window, cx).expect("the thread is open");
        };
        a.update(cx, |_, window, cx| {
            open_thread(channel, root, &allowed, cx);
            draw(&allowed, window, cx);
        })
        .unwrap();
        b.update(cx, |_, window, cx| draw(&read_only, window, cx))
            .unwrap();
        let sent = Rc::new(std::cell::RefCell::new(Vec::new()));
        let press = |handle: gpui::WindowHandle<Blank>, cx: &mut TestAppContext| {
            let id = handle.window_id();
            handle
                .update(cx, |_, window, cx| {
                    let pane = ui(cx).windows.get(&id).unwrap().pane.clone().unwrap();
                    let input = pane.read(cx).input();
                    input.update(cx, |s, cx| {
                        s.set_value("hello", window, cx);
                        cx.emit(InputEvent::PressEnter {
                            secondary: false,
                            shift: false,
                        });
                    });
                    pane
                })
                .unwrap()
        };
        let pane_a = press(a, cx);
        let log = sent.clone();
        let _keep = cx.update(|cx| {
            cx.subscribe(&pane_a, move |_, e: &view::Sent, _| {
                log.borrow_mut().push(e.1.clone())
            })
        });
        // The subscription is made after the first press: press again, now observed.
        press(a, cx);
        cx.run_until_parked();
        assert_eq!(
            sent.borrow().as_slice(),
            ["hello"],
            "A's pane sends under A's view, not B's (drawn last)"
        );
        let pane_b = press(b, cx);
        let log_b = Rc::new(std::cell::RefCell::new(0));
        let count = log_b.clone();
        let _keep_b = cx.update(|cx| {
            cx.subscribe(&pane_b, move |_, _: &view::Sent, _| {
                *count.borrow_mut() += 1
            })
        });
        press(b, cx);
        cx.run_until_parked();
        assert_eq!(*log_b.borrow(), 0, "B's read-only view refuses the send");
    }

    /// Review N4: a thread chip's focus handle is its window's. The same thread drawn in two windows (two accounts on one
    /// server) has a chip in each, and a closing pane in A gives focus back to A's chip, never B's.
    #[gpui::test]
    fn each_window_keeps_its_own_thread_chips(cx: &mut TestAppContext) {
        use dilla_client::state::threads::ThreadSummary;
        let (ctx, channel) = channel_ctx(None);
        let root = MessageId([7; 16]);
        let mut state = (*ctx.state).clone();
        state.threads.summaries.insert(
            channel,
            std::iter::once((
                root,
                ThreadSummary {
                    count: 2,
                    last_ts: 1,
                },
            ))
            .collect(),
        );
        let ctx = SlotCx {
            state: Arc::new(state),
            ..ctx
        };
        let a = cx.add_window(|_, _| Blank);
        let b = cx.add_window(|_, _| Blank);
        for w in [a, b] {
            w.update(cx, |_, window, cx| {
                thread_count(&item(TimelineItemState::Ok, "root"), &ctx, window, cx)
                    .expect("the chip draws");
            })
            .unwrap();
        }
        let chip = |w: gpui::WindowHandle<Blank>, cx: &mut TestAppContext| {
            let id = w.window_id();
            cx.update(|cx| {
                ui(cx)
                    .windows
                    .get(&id)
                    .and_then(|p| p.chips.get(&root).cloned())
            })
            .expect("a chip handle")
        };
        assert_ne!(chip(a, cx), chip(b, cx), "each window has its own chip");
    }

    /// Task 28 item 12 (ThreadView.tsx's MessageItem rows): an own, stored message of the thread offers Edit and Delete;
    /// another's, or one not stored yet, offers neither.
    #[test]
    fn an_own_thread_message_offers_edit_and_delete() {
        let mut own = item(TimelineItemState::Ok, "mine");
        own.own = true;
        assert_eq!(view::row_actions(&own, None), ["edit", "delete"]);
        assert!(view::row_actions(&item(TimelineItemState::Ok, "theirs"), None).is_empty());
        let mut pending = own.clone();
        pending.state = TimelineItemState::Pending;
        pending.seq = None;
        assert!(view::row_actions(&pending, None).is_empty());
    }

    /// Review A M4 (ThreadView.tsx offers MessageItem's `moderateRemove`): another person's stored thread message offers
    /// Remove for everyone to a reader whose known channel bits hold manage_messages; unknown bits, or bits without it,
    /// offer nothing, and an own message is edited and deleted instead.
    #[test]
    fn another_s_thread_message_offers_remove_for_everyone_to_a_moderator() {
        let manage = u64::from(dilla_client::features::roles::perm::MANAGE_MESSAGES);
        let theirs = item(TimelineItemState::Ok, "theirs");
        assert_eq!(view::row_actions(&theirs, Some(manage)), ["moderateRemove"]);
        assert!(view::row_actions(&theirs, None).is_empty(), "unknown bits");
        assert!(view::row_actions(&theirs, Some(0)).is_empty());
        let mut own = theirs.clone();
        own.own = true;
        assert_eq!(view::row_actions(&own, Some(manage)), ["edit", "delete"]);
    }

    /// Review HL item 12 (weak test) and A M4, driven through the drawn pane as a person does: hovering a row shows its
    /// toolbar; the own reply's Edit opens the messages area's editor on it and its Delete asks through the delete dialog;
    /// another person's reply offers a moderator Remove for everyone, which opens the moderation dialog on it.
    #[gpui::test]
    fn the_thread_pane_s_toolbar_acts_through_the_drawn_rows(cx: &mut TestAppContext) {
        use crate::screens::shell::{Clock, ShellView};
        cx.update(crate::install);
        let (me, them) = (UserId([5; 16]), UserId([6; 16]));
        let channel = ChannelId([2; 16]);
        let root = MessageId([7; 16]);
        let mut s = dilla_client::state::AppState::default();
        // The owner: the viewer's bits are known and hold manage_messages.
        s.communities.push(dilla_client::state::Community {
            id: CommunityId([1; 16]),
            name: "Midgard".into(),
            owner: me,
            policy_version: 0,
        });
        s.channels = vec![Channel {
            id: channel,
            community_id: CommunityId([1; 16]),
            kind: 0,
            mode: 0,
            name: "general".into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::Active,
        }];
        s.account.user_id = Some(me);
        let reply = |key: &str, msg: u8, seq: u64, own: bool| {
            let mut i = item(TimelineItemState::Ok, "a reply");
            i.key = key.into();
            i.msg_id = Some(MessageId([msg; 16]));
            i.seq = Some(seq);
            i.own = own;
            i.sender_user = Some(if own { me } else { them });
            i
        };
        s.threads.threads.insert(
            root,
            dilla_client::state::threads::ThreadData {
                channel,
                root: None,
                replies: vec![reply("s2", 8, 2, true), reply("s3", 9, 3, false)],
                has_earlier: false,
                count: 2,
            },
        );
        let state = Arc::new(s);
        let (_shell, vcx) = cx.add_window_view(move |window, cx| {
            ShellView::new(
                state,
                None,
                None,
                Clock::Fixed { now: 0, utc: true },
                window,
                cx,
            )
        });
        vcx.run_until_parked();
        let here = vcx.update(|window, _| wid(window));
        let ctx = vcx
            .update(|_, cx| current(here, cx))
            .expect("the shell gave the threads area its context");
        vcx.update(|_, cx| open_thread(channel, root, &ctx, cx));
        vcx.run_until_parked();
        vcx.update(|window, _| window.refresh());
        vcx.run_until_parked();
        assert!(
            vcx.debug_bounds("thread-close").is_some(),
            "the pane is drawn"
        );
        let press =
            |vcx: &mut gpui::VisualTestContext, row: &'static str, selector: &'static str| {
                // The toolbar shows on hover only: the row is hovered first, then the button is clicked.
                assert!(
                    vcx.debug_bounds(selector).is_none(),
                    "{selector} is hidden until its row is hovered"
                );
                let row = vcx
                    .debug_bounds(row)
                    .unwrap_or_else(|| panic!("{row} is drawn"));
                vcx.simulate_mouse_move(row.center(), None, gpui::Modifiers::none());
                vcx.run_until_parked();
                vcx.update(|window, _| window.refresh());
                vcx.run_until_parked();
                let at = vcx
                    .debug_bounds(selector)
                    .unwrap_or_else(|| panic!("{selector} shows on hover"))
                    .center();
                vcx.simulate_click(at, gpui::Modifiers::none());
                vcx.run_until_parked();
                // The pointer leaves for the window's corner, so the next row's toolbar starts hidden.
                vcx.simulate_mouse_move(
                    gpui::point(gpui::Pixels::from(1.0f32), gpui::Pixels::from(1.0f32)),
                    None,
                    gpui::Modifiers::none(),
                );
                vcx.run_until_parked();
                vcx.update(|window, _| window.refresh());
                vcx.run_until_parked();
            };
        let ui = vcx
            .update(|window, cx| crate::screens::messages::ui::handle(window, cx))
            .expect("the messages area's ui");
        press(vcx, "thread-row-s2", "thread-edit-s2");
        assert_eq!(
            ui.read_with(vcx, |u, _| u.editing.as_ref().map(|e| e.key.clone())),
            Some("s2".to_owned()),
            "Edit opens the editor on the own reply"
        );
        vcx.update(|_, cx| ui.update(cx, |u, cx| u.cancel_edit(cx)));
        vcx.run_until_parked();
        press(vcx, "thread-row-s2", "thread-delete-s2");
        assert!(
            ui.read_with(vcx, |u, _| u
                .deleting
                .is_some_and(|d| d.msg == MessageId([8; 16]))),
            "Delete asks through the delete dialog"
        );
        assert!(
            vcx.debug_bounds("thread-edit-s3").is_none(),
            "another's reply has no Edit"
        );
        vcx.update(|_, cx| ui.update(cx, |u, cx| u.cancel_delete(cx)));
        vcx.run_until_parked();
        press(vcx, "thread-row-s3", "thread-moderate-s3");
        assert!(
            ui.read_with(vcx, |u, _| u.moderating.is_some()),
            "Remove for everyone opens the moderation dialog"
        );
    }

    /// Task 28 item 12: the thread pane's Edit opens the messages area's editor on that message, and its Delete asks
    /// first, through the messages area's dialog (one editor and one delete dialog per window, as the web's).
    #[gpui::test]
    fn the_thread_pane_edits_and_deletes_through_the_messages_area(cx: &mut TestAppContext) {
        let channel = ChannelId([3; 16]);
        let mut own = item(TimelineItemState::Ok, "mine");
        own.own = true;
        let w = cx.add_window(|_, _| Blank);
        let ui = w
            .update(cx, |_, window, cx| {
                crate::screens::messages::ui::ui_for_test(window.window_handle().window_id(), cx)
            })
            .unwrap();
        let ctx = SlotCx {
            channel: Some(channel),
            ..SlotCx::default()
        };
        w.update(cx, |_, window, cx| view::edit_row(&ctx, &own, window, cx))
            .unwrap();
        assert_eq!(
            ui.read_with(cx, |u, _| u.editing.as_ref().map(|e| e.key.clone())),
            Some(own.key.clone())
        );
        w.update(cx, |_, window, cx| {
            view::delete_row(channel, &own, window, cx)
        })
        .unwrap();
        assert!(ui.read_with(cx, |u, _| {
            u.deleting
                .is_some_and(|d| d.channel == channel && Some(d.msg) == own.msg_id)
        }));
    }
}

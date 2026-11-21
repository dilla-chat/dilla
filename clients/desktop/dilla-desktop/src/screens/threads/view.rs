//! The thread pane (ThreadView.tsx, ThreadPane.css): the root, a rule with the reply count, the replies and the thread's own
//! composer. It reads the client's `AppState::threads` through the [`SlotCx`] its own window's shell last drew (see the
//! module docs), and
//! sends through [`dilla_client::features::threads::Threads`]. Opening it leaves the composer unfocused (wave B parity round 2).

use std::rc::Rc;

use dilla_client::state::bot::Conversation;
use dilla_client::state::threads::{OpenThread, ThreadData};
use dilla_client::state::{MessageId, TimelineItem, TimelineItemState};
use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::button::{Button, ButtonVariant};
use dilla_desktop_ui::components::icon_button::{IconButton, IconButtonSize};
use dilla_desktop_ui::components::threads::{
    PaneParts, ThreadRow, ThreadRowKind, pane, thread_row,
};
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, radius, text};
use gpui::{
    AppContext as _, Context, Entity, EntityInputHandler, EventEmitter, FocusHandle, Focusable,
    InteractiveElement, IntoElement, ParentElement, Render, SharedString, Styled, Subscription,
    Window, div, px,
};
use gpui_component::input::{Enter, InputEvent, Textarea, TextareaState};

use super::{close_thread, current, name_of, open_of, time_of};
use crate::strings::{t, tf};
use crate::{tracked_box, tracked_focus};

/// The thread composer's field: the CSS's `.d-composer__input` lines (20px) in a 30px minimum, at most 13.75rem.
const FIELD_LINE: f32 = 20.0;
const FIELD_PAD: f32 = 5.0;
const FIELD_MAX: f32 = 220.0;
/// `.d-thread-pane__composer .d-composer` padding 8px 12px 12px; `.d-composer__row` gap 8px, padding 4px 4px 4px 10px.
const COMPOSER_TOP: f32 = 8.0;
const COMPOSER_BOTTOM: f32 = 12.0;
const ROW_GAP: f32 = 8.0;
const ROW_LEFT: f32 = 10.0;
const ROW_PAD: f32 = 4.0;
/// Extra space between the list and the composer's top edge. No CSS rule of the web gives it: ThreadPane.css:44 pads the
/// composer 8px and CommandPicker.css:71 lifts the list 6px (`bottom: calc(100% + 6px)`), which the list's own gap and the
/// padding above already draw; the web frame still has the list 4px higher (the reference of `threads/command-list`).
const POPUP_GAP: f32 = 4.0;
/// The hover toolbar's gap between its buttons (MessageRow.css `__toolbar` gap 2px).
const TOOL_GAP: f32 = 2.0;

fn max_rows() -> usize {
    ((FIELD_MAX - 2.0 * FIELD_PAD) / FIELD_LINE).floor() as usize
}

/// Whether the pane draws its "load earlier" button, and whether it can be pressed: `None` without one (no thread read, or
/// no older replies), `Some(enabled)` otherwise; it is disabled while an older page is being read.
pub fn earlier_shown(data: Option<&ThreadData>, loading: bool) -> Option<bool> {
    data.filter(|d| d.has_earlier).map(|_| !loading)
}

/// Task 28 item 12 (ThreadView.tsx's MessageItem rows): what a thread message offers: Edit and Delete for the own message
/// once it is stored (an id and a seq, read as sent); and (review A M4) Remove for everyone on another person's stored
/// message, for a reader whose channel `bits` are known and hold manage_messages (`may_remove_for_everyone`).
pub fn row_actions(item: &TimelineItem, bits: Option<u64>) -> Vec<&'static str> {
    let stored = item.state == TimelineItemState::Ok && item.msg_id.is_some() && item.seq.is_some();
    if !stored {
        return Vec::new();
    }
    if item.own {
        vec!["edit", "delete"]
    } else if dilla_client::features::moderation::may_remove_for_everyone(false, true, bits) {
        vec![crate::screens::messages::actions::MODERATE_REMOVE]
    } else {
        Vec::new()
    }
}

/// One button of a row's hover toolbar, findable by `selector` (a test drives it as a person does: hover, then click).
fn tool(selector: String, button: IconButton) -> impl IntoElement {
    let id = SharedString::from(format!("{selector}-box"));
    div()
        .id(id)
        .debug_selector(move || selector.clone())
        .child(button)
}

/// The thread pane's Edit on `item`: the messages area's editor opens on it (one editor per window), in its place.
pub fn edit_row(
    ctx: &crate::slots::SlotCx,
    item: &TimelineItem,
    window: &mut Window,
    cx: &mut gpui::App,
) {
    if let Some(ui) = crate::screens::messages::ui::handle(window, cx) {
        ui.update(cx, |ui, cx| ui.begin_edit(ctx, item, window, cx));
    }
}

/// The thread pane's Delete on `item` of `channel`: the messages area's delete dialog asks first.
pub fn delete_row(
    channel: dilla_client::state::ChannelId,
    item: &TimelineItem,
    window: &mut Window,
    cx: &mut gpui::App,
) {
    let (Some(msg), Some(ui)) = (
        item.msg_id,
        crate::screens::messages::ui::handle(window, cx),
    ) else {
        return;
    };
    ui.update(cx, |ui, cx| ui.ask_delete(channel, msg, cx));
}

/// The conversation of `open`'s composer: its own command memory, private rows and message lookups (CommandHost.tsx:74).
fn conversation(open: OpenThread) -> Conversation {
    Conversation {
        channel: open.channel,
        thread: Some(open.root),
    }
}

/// Emitted when the thread composer sends: the thread's root and the text.
pub struct Sent(pub MessageId, pub String);

/// The thread pane's scope for what its rows remember of their entrances (`motion::scope`).
pub const THREAD_SCOPE: &str = "thread-log";

pub struct PaneView {
    /// The window this pane is drawn in: it reads and sends through that window's shell view only.
    window: gpui::WindowId,
    input: Entity<TextareaState>,
    close_focus: FocusHandle,
    /// A send is in flight: the field is empty but the draft stays in the prefs until the send succeeds.
    in_flight: bool,
    /// The root whose draft the field currently holds.
    shown: Option<MessageId>,
    /// The "load earlier" press is in flight (ThreadView's pages read apart from the open thread's own read).
    loading_earlier: bool,
    /// The pane's log mount and the entrances of its rows (Task 36c), per thread.
    log: crate::screens::messages::ui::LogEntrances<MessageId>,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<Sent> for PaneView {}

impl Focusable for PaneView {
    fn focus_handle(&self, cx: &gpui::App) -> FocusHandle {
        self.input.read(cx).focus_handle(cx)
    }
}

impl PaneView {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let input = cx.new(|cx| TextareaState::new(window, cx).submit_on_enter(true));
        let sub = cx.subscribe_in(&input, window, Self::on_input_event);
        Self {
            window: window.window_handle().window_id(),
            input,
            close_focus: cx.focus_handle().tab_stop(true),
            in_flight: false,
            shown: None,
            loading_earlier: false,
            log: Default::default(),
            _subscriptions: vec![sub],
        }
    }

    /// Whether drawing the pane's log for thread `root` mounts it anew (tests).
    #[cfg(test)]
    pub(super) fn log_remounts(&self, root: MessageId) -> bool {
        self.log.remounts(root)
    }

    /// How long the log's row `key` has been entering at `now`, and its delay (tests).
    #[cfg(test)]
    pub(super) fn log_of(
        &self,
        key: &str,
        now: std::time::Instant,
    ) -> Option<(std::time::Duration, std::time::Duration)> {
        self.log.of(key, now)
    }

    /// The composer's field (tests drive it).
    #[cfg(test)]
    pub(super) fn input(&self) -> Entity<TextareaState> {
        self.input.clone()
    }

    fn on_input_event(
        &mut self,
        input: &Entity<TextareaState>,
        event: &InputEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(ctx) = current(self.window, cx) else {
            return;
        };
        let Some(open) = open_of(&ctx, cx) else {
            return;
        };
        let wid = window.window_handle().window_id();
        match event {
            InputEvent::Change => {
                // A thread's draft is its own, apart from the channel's (`threads.draft.<root>`).
                if !self.in_flight
                    && let Some(client) = &ctx.client
                {
                    client
                        .threads()
                        .set_draft(open.root, &input.read(cx).value());
                }
                // The `/` list reads the draft this composer reports, as the channel's does (composer.rs). Only a draft
                // typed here counts: the field also changes as the pane loads another thread's draft.
                let focus = input.read(cx).focus_handle(cx);
                if focus.is_focused(window) {
                    crate::screens::commands::set_composer(wid, &focus, cx);
                    crate::screens::commands::draft_changed(wid, &input.read(cx).value(), cx);
                }
                cx.notify();
            }
            InputEvent::PressEnter { shift: false, .. } => {
                let composing = input
                    .update(cx, |s, cx| s.marked_text_range(window, cx))
                    .is_some();
                // A picked command's chips are what is sent (composer.rs `send`), while this composer's own are shown.
                let focus = input.read(cx).focus_handle(cx);
                if crate::screens::commands::chips::hold_unnamed_member(&ctx, &focus, wid, cx) {
                    window.refresh();
                    return;
                }
                let draft = crate::screens::commands::chips::draft(&ctx, &focus, wid, cx)
                    .unwrap_or_else(|| input.read(cx).value().to_string());
                // A reader who cannot send has a disabled composer (ThreadView `blocked`).
                if composing || draft.trim().is_empty() || !ctx.state.threads.can_send(open.channel)
                {
                    return;
                }
                // The Bot area decides first, as for the channel's composer: a literal text or `//x` is posted into the
                // thread, a built-in answers privately or runs in it, and a command still lacking arguments keeps its
                // draft. The thread composer encodes no mentions, so the encoder is the identity.
                let at_thread = at_thread(&ctx, open);
                let draft = match crate::screens::commands::on_send_in(
                    &at_thread,
                    Some(open.root),
                    Some(&focus),
                    &draft,
                    &|t| t.to_owned(),
                    wid,
                    cx,
                ) {
                    crate::screens::commands::SendDecision::Post(text) => text,
                    crate::screens::commands::SendDecision::Handled => {
                        input.update(cx, |s, cx| s.set_value("", window, cx));
                        if let Some(client) = &ctx.client {
                            client.threads().set_draft(open.root, "");
                        }
                        cx.notify();
                        return;
                    }
                    crate::screens::commands::SendDecision::Keep => return,
                };
                self.in_flight = true;
                input.update(cx, |s, cx| s.set_value("", window, cx));
                if let Some(client) = ctx.client.clone() {
                    let done =
                        client
                            .threads()
                            .send_in_thread(open.channel, open.root, draft.clone());
                    let (root, text) = (open.root, draft.clone());
                    // The draft is cleared when the send has succeeded; a failed send puts the text back (ThreadView `send`).
                    cx.spawn_in(window, async move |this, cx| {
                        let sent = matches!(done.await, Ok(Ok(_)));
                        let _ = this.update_in(cx, |this, window, cx| {
                            this.in_flight = false;
                            if sent {
                                client.threads().set_draft(root, "");
                            } else if this.input.read(cx).value().is_empty() {
                                this.input.update(cx, |s, cx| s.set_value(text, window, cx));
                            }
                        });
                    })
                    .detach();
                } else {
                    self.in_flight = false;
                }
                cx.emit(Sent(open.root, draft));
                cx.notify();
            }
            _ => {}
        }
    }

    /// The field holds the open thread's draft, not the previous thread's.
    ///
    /// Task 31 (Task 30's memory): the draft of an uncertain command of this thread comes back before the stored draft, as
    /// the channel composer's load prefers it (`loaded_draft`).
    fn settle_draft(&mut self, open: OpenThread, window: &mut Window, cx: &mut Context<Self>) {
        let (root, at) = (open.root, conversation(open));
        if self.shown == Some(root) {
            return;
        }
        self.shown = Some(root);
        // Another thread starts with an empty picker, as the web's `key={rootMsgId}` remounts its composer.
        let focus = self.input.read(cx).focus_handle(cx);
        crate::screens::commands::reset_composer(self.window, &focus, cx);
        let draft = current(self.window, cx)
            .and_then(|c| {
                c.client.map(|client| {
                    crate::screens::commands::with_memory(&client, |m| m.take_reopen(at))
                        .unwrap_or_else(|| client.threads().draft(root))
                })
            })
            .unwrap_or_default();
        self.input
            .update(cx, |s, cx| s.set_value(draft, window, cx));
    }

    /// The composer's field and the root whose draft it holds (`None` while no thread is shown).
    pub(super) fn composer(&self, cx: &gpui::App) -> Option<(FocusHandle, MessageId)> {
        Some((self.input.read(cx).focus_handle(cx), self.shown?))
    }

    /// The pane was closed: its composer's picker starts empty when it opens again, and its draft is read afresh.
    pub(super) fn closed(&mut self, cx: &mut Context<Self>) {
        // The web unmounts the pane's log with it: it mounts anew (and enters again) when a thread opens.
        self.log.unmount();
        if self.shown.take().is_some() {
            let focus = self.input.read(cx).focus_handle(cx);
            crate::screens::commands::reset_composer(self.window, &focus, cx);
        }
    }

    /// Task 30 (CommandHost.tsx:293-296): a command run from this thread's composer whose answer was uncertain comes back
    /// into it, so the list opens on the same draft and a resend is the same request. The draft the window was handed
    /// (`commands::reopen`) is taken first, else the one the command memory keeps for this thread. Only for the composer
    /// of the thread it was typed in, and of the client that ran it.
    fn reopen(&mut self, open: OpenThread, window: &mut Window, cx: &mut Context<Self>) {
        let at = conversation(open);
        let wid = window.window_handle().window_id();
        let client = current(self.window, cx).and_then(|c| c.client);
        let serial = client.as_ref().map(|c| c.serial());
        let handed = crate::screens::commands::take_reopen(wid, serial, at, cx);
        let text = handed.or_else(|| {
            client
                .as_ref()
                .and_then(|c| crate::screens::commands::with_memory(c, |m| m.take_reopen(at)))
        });
        if let Some(text) = text {
            self.input
                .update(cx, |s, cx| s.set_value(text.clone(), window, cx));
            let focus = self.input.read(cx).focus_handle(cx);
            crate::screens::commands::set_composer(wid, &focus, cx);
            crate::screens::commands::draft_changed(wid, &text, cx);
        }
    }

    fn row(
        &self,
        item: &TimelineItem,
        spaced: bool,
        // The thread the pane shows (the root's id, also for the root row): where a poll must be held to be drawn.
        thread: Option<MessageId>,
        ctx: &crate::slots::SlotCx,
        window: &mut Window,
        cx: &mut gpui::App,
    ) -> gpui::AnyElement {
        let author = name_of(&ctx.state, ctx.community, item.sender_user);
        let bot = is_bot_row(ctx, item);
        let live = ctx.client.is_some();
        let (kind, body) = match item.state {
            TimelineItemState::Ok => (ThreadRowKind::Ok, thread_body(item, ctx, live, thread)),
            TimelineItemState::Pending => (ThreadRowKind::Pending, item.body.clone()),
            TimelineItemState::Failed => (ThreadRowKind::Failed, item.body.clone()),
            TimelineItemState::CannotRead => (
                ThreadRowKind::Note,
                t("shell.message.cannotRead").to_owned(),
            ),
            TimelineItemState::Deleted => {
                (ThreadRowKind::Note, t("shell.message.deleted").to_owned())
            }
        };
        let pane_width = f32::from(dilla_desktop_ui::tokens::layout::THREAD);
        let mut row = ThreadRow::new(
            SharedString::from(format!("thread-row-{}", item.key)),
            author,
            time_of(item.ts, ctx.clock),
            body,
            pane_width,
        )
        // ThreadView.tsx passes no `grouped` to MessageItem: every thread message keeps its avatar and head.
        .spaced(spaced)
        .kind(kind)
        .bot(bot);
        // A Bot's poll post has its card under the body (MessageItem mounts `PollCard` in a thread as in the log).
        let column = dilla_desktop_ui::components::threads::thread_row::body_width(pane_width);
        if let Some(e) =
            crate::screens::commands::poll_card_in_width(item, ctx, thread, column, window, cx)
        {
            row = row.extra(e.element, e.height);
        }
        // Task 28 item 12: the message being edited shows the messages area's editor in its body's place.
        let ui = crate::screens::messages::ui::handle(window, cx);
        let editing = ui.as_ref().and_then(|ui| {
            ui.read(cx)
                .editing
                .as_ref()
                .filter(|e| e.key == item.key)
                .map(|e| (e.input.clone(), e.used(cx)))
        });
        if let (Some((input, used)), Some(ui)) = (editing, &ui) {
            let width = dilla_desktop_ui::components::threads::thread_row::body_width(pane_width);
            row = row.replace_body(crate::screens::messages::editor::editor(
                &input, used, ui, width, window, cx,
            ));
        }
        let bits = ctx
            .channel
            .and_then(|c| dilla_client::features::roles::viewer_channel_bits(&ctx.state, c));
        let actions = row_actions(item, bits);
        let open_channel = ctx.channel;
        let (Some(channel), false) = (open_channel, actions.is_empty()) else {
            return row.into_any_element();
        };
        // MessageRow's hover toolbar, at the row's top right: Edit and Delete of the own message, or Remove for everyone
        // of another person's.
        let group = SharedString::from(format!("thread-row-group-{}", item.key));
        let mut toolbar = div()
            .absolute()
            .top_0()
            .right_0()
            .flex()
            .gap(px(TOOL_GAP))
            .invisible()
            .group_hover(group.clone(), |s| s.visible());
        if actions.contains(&"edit") {
            let (edit_item, delete_item, edit_ctx) = (item.clone(), item.clone(), ctx.clone());
            let edit = format!("thread-edit-{}", item.key);
            let delete = format!("thread-delete-{}", item.key);
            toolbar = toolbar
                .child(tool(
                    edit.clone(),
                    IconButton::new(
                        SharedString::from(edit),
                        IconName::Pencil,
                        t("shell.message.edit"),
                    )
                    .size(IconButtonSize::S28)
                    .on_click(move |_, window, cx| edit_row(&edit_ctx, &edit_item, window, cx)),
                ))
                .child(tool(
                    delete.clone(),
                    IconButton::new(
                        SharedString::from(delete),
                        IconName::Trash2,
                        t("shell.message.delete"),
                    )
                    .size(IconButtonSize::S28)
                    .on_click(move |_, window, cx| delete_row(channel, &delete_item, window, cx)),
                ));
        }
        if actions.contains(&crate::screens::messages::actions::MODERATE_REMOVE) {
            let (moderate_item, moderate_ctx) = (item.clone(), ctx.clone());
            let moderate = format!("thread-moderate-{}", item.key);
            toolbar = toolbar.child(tool(
                moderate.clone(),
                IconButton::new(
                    SharedString::from(moderate),
                    IconName::Trash2,
                    t("shell.message.moderateRemove"),
                )
                .size(IconButtonSize::S28)
                .on_click(move |_, window, cx| {
                    crate::screens::messages::actions::ask_moderate_remove(
                        &moderate_ctx,
                        &moderate_item,
                        window,
                        cx,
                    )
                }),
            ));
        }
        let selector = format!("thread-row-{}", item.key);
        div()
            .id(SharedString::from(format!("{selector}-hover")))
            .debug_selector(move || selector.clone())
            .group(group)
            .relative()
            .child(row)
            .child(toolbar)
            .into_any_element()
    }
}

/// Whether a thread row is the community Bot's: the tag and the Bot's tone need the credential's claim and a Bot member row both
/// (MessageItem `item.bot && isCommunityBot`).
pub(super) fn is_bot_row(ctx: &crate::slots::SlotCx, item: &TimelineItem) -> bool {
    let members = ctx
        .community
        .and_then(|c| ctx.state.members.get(&c))
        .map_or(&[][..], Vec::as_slice);
    crate::screens::timeline::is_community_bot(members, item)
}

/// What a thread message's body reads (MessageItem.tsx `displayBody`): a poll post is its `{name} asked:` line, nothing while
/// its card is being verified, or its own text without the marker line; any other message, and any state but Ok, is as it
/// was. `live` is whether a client is running. The times in the fallback are in the zone of the log's (UTC under a
/// frozen fixture clock).
pub fn thread_body(
    item: &TimelineItem,
    ctx: &crate::slots::SlotCx,
    live: bool,
    thread: Option<MessageId>,
) -> String {
    if item.state != TimelineItemState::Ok {
        return item.body.clone();
    }
    let zone = crate::screens::timeline::zone_for(ctx.clock);
    match crate::screens::timeline::poll_body_in(
        item,
        &ctx.state,
        ctx.community,
        live,
        &zone,
        thread,
    ) {
        crate::screens::timeline::PollBody::Asked(s) => tf("poll.asked", &[("name", s.as_str())]),
        crate::screens::timeline::PollBody::Pending => String::new(),
        crate::screens::timeline::PollBody::Fallback(s) => s,
        crate::screens::timeline::PollBody::NoMarker => item.body.clone(),
    }
}

/// `ctx` aimed at `open`'s conversation: its channel, and the server that channel is in (a DM's thread has none). The pane
/// is drawn only beside its own channel ([`super::open_of`]), so this is the shown server too; the community is still read
/// from the thread's channel, so no command of the thread is planned or run under another server's roles and Bots. A
/// channel the state does not list (a fixture's) keeps the view's.
pub(super) fn at_thread(ctx: &crate::slots::SlotCx, open: OpenThread) -> crate::slots::SlotCx {
    let community = if ctx.state.dms.get(open.channel).is_some() {
        None
    } else {
        ctx.state
            .channel(open.channel)
            .map(|c| c.community_id)
            .or(ctx.community)
    };
    crate::slots::SlotCx {
        channel: Some(open.channel),
        community,
        ..ctx.clone()
    }
}

impl Render for PaneView {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let Some(ctx) = current(self.window, cx) else {
            return div().into_any_element();
        };
        let Some(open) = open_of(&ctx, cx) else {
            return div().into_any_element();
        };
        self.settle_draft(open, window, cx);
        self.reopen(open, window, cx);
        // The rows' actions act on the thread's channel and its server.
        let ctx = at_thread(&ctx, open);
        let data = ctx
            .state
            .threads
            .threads
            .get(&open.root)
            .filter(|d| d.channel == open.channel);
        let sub = ctx
            .state
            .channel(open.channel)
            .map(|c| format!("#{}", c.name));
        let root = data.map(|d| match &d.root {
            // The web passes the root's id for the root row too (ThreadView `rootMsgId`): a channel poll that is a thread's root
            // is no card in the pane, its record is the log's.
            Some(root) => self.row(root, false, Some(open.root), &ctx, window, cx),
            None => line(
                t("thread.rootMissing"),
                text::SM,
                line_height(text::SM, 1.5),
                color::FG_3,
            )
            .into_any_element(),
        });
        // Task 36c: the pane's log mounts per thread (Shell.tsx:2257 `key={rootMsgId}`) and fades in with a 6px rise
        // (MessageLog.css:3-4); its replies rise in (`d-up`), the first paint's last 20 staggered. What lives in the rows
        // (cards, chips, voters) enters again with a new mount.
        dilla_desktop_ui::motion::set_scope(cx, THREAD_SCOPE);
        if self.log.remounts(open.root) {
            dilla_desktop_ui::motion::remount_log(cx, THREAD_SCOPE);
        }
        let mut replies: Vec<_> = data
            .map(|d| {
                d.replies
                    .iter()
                    .enumerate()
                    .map(|(n, item)| self.row(item, n > 0, Some(open.root), &ctx, window, cx))
                    .collect()
            })
            .unwrap_or_default();
        let mut enter_keys: Vec<String> = data
            .map(|d| d.replies.iter().map(|i| i.key.clone()).collect())
            .unwrap_or_default();
        // The thread's private results (a command's answer, only this device shows it) come after its replies
        // (ThreadView.tsx:243-244), in the log's width: the pane less the rows' 14px sides.
        let result_width =
            f32::from(dilla_desktop_ui::tokens::layout::THREAD) - 2.0 * thread_row::SIDE;
        let privates = crate::screens::commands::private_results_in(
            &ctx,
            Some(open.root),
            result_width,
            window,
            cx,
        );
        enter_keys.extend((0..privates.len()).map(|n| format!("private-{n}")));
        replies.extend(privates.into_iter().map(|(row, height)| {
            div()
                .w(px(result_width))
                .h(px(height))
                .child(row)
                .into_any_element()
        }));
        let now = cx.background_executor().now();
        let entering = self
            .log
            .enter(open.root, &enter_keys, now, cx.reduce_motion());
        if entering.running {
            window.request_animation_frame();
        }
        let replies: Vec<gpui::AnyElement> = replies
            .into_iter()
            .zip(&enter_keys)
            .map(|(row, key)| match self.log.of(key, now) {
                Some((elapsed, delay)) if !cx.reduce_motion() => {
                    // MessageRow.css:8, as in the channel's log.
                    dilla_desktop_ui::motion::rise_style(
                        div().child(row),
                        elapsed,
                        delay,
                        dilla_desktop_ui::tokens::motion::ENTER,
                        dilla_desktop_ui::tokens::motion::EASE_OUT,
                    )
                    .into_any_element()
                }
                _ => row,
            })
            .collect();
        // Fix round 2 of the web: the rule counts the thread's replies (the root's summary), not only those read so far.
        let count = data.map_or(0, |d| d.count.max(d.replies.len() as u32));
        let rule = match count {
            0 => None,
            1 => Some(t("thread.summary.one").to_owned()),
            n => Some(tf("thread.summary.other", &[("count", &n.to_string())])),
        };
        // MessageLog.css `.d-message-log__earlier`: the button's `margin-bottom: var(--group-gap)`, the window's density.
        let earlier_gap = dilla_desktop_ui::metrics::Density::of(window, cx).group_gap();
        let earlier = earlier_shown(data, self.loading_earlier).map(|enabled| {
            let this = cx.entity();
            let window_id = self.window;
            let client = ctx.client.clone();
            div()
                .w_full()
                .flex()
                .justify_center()
                .pb(earlier_gap)
                .child(tracked_box(
                    "thread-earlier",
                    Button::new("thread-earlier", t("shell.log.earlier"))
                        .variant(ButtonVariant::Ghost)
                        .disabled(!enabled)
                        .on_click(move |_, _, cx| {
                            let Some(client) = client.clone() else {
                                return;
                            };
                            this.update(cx, |this, cx| {
                                this.loading_earlier = true;
                                cx.notify();
                            });
                            let answer = client.threads().load_earlier();
                            let this = this.clone();
                            cx.spawn(async move |cx| {
                                let failed = match answer.await {
                                    Ok(Ok(())) => None,
                                    Ok(Err(dilla_client::ClientError::Sync(code))) => Some(code),
                                    Ok(Err(_)) => Some("E_NETWORK".to_owned()),
                                    Err(_) => Some("E_STOPPED".to_owned()),
                                };
                                cx.update(|cx| {
                                    this.update(cx, |this, cx| {
                                        this.loading_earlier = false;
                                        cx.notify();
                                    });
                                    if let Some(code) = failed {
                                        dilla_desktop_ui::components::toast::show_toast_in(
                                            Some(window_id),
                                            tf("shell.banner.commandError", &[("code", &code)]),
                                            cx,
                                        );
                                    }
                                });
                            })
                            .detach();
                        }),
                ))
                .into_any_element()
        });
        let empty = data
            .is_some_and(|d| d.replies.is_empty())
            .then(|| t("thread.noReplies"));
        let rows = self
            .input
            .read(cx)
            .value()
            .split('\n')
            .count()
            .clamp(1, max_rows());
        let focus = self.focus_handle(cx);
        let focused = focus.is_focused(window);
        let value_empty = self.input.read(cx).value().is_empty();
        // ThreadView `blocked`: the viewer's own timeout in the community first (Shell.tsx `timeoutReason`).
        let timed_out = ctx.state.channel(open.channel).and_then(|c| {
            crate::screens::messages::composing::timed_out(&ctx.state, c, ctx.clock.now())
        });
        let blocked = timed_out.is_some() || !ctx.state.threads.can_send(open.channel);
        let placeholder = if let Some(expires) = timed_out {
            tf(
                "shell.composer.timedOut",
                &[(
                    "time",
                    &crate::screens::threads::time_of(expires, ctx.clock),
                )],
            )
        } else if blocked {
            tf(
                "chperm.cannotSend",
                &[(
                    "name",
                    ctx.state
                        .channel(open.channel)
                        .map_or("", |c| c.name.as_str()),
                )],
            )
        } else {
            t("thread.composer").to_owned()
        };
        // Task 31: the command list over the composer and a picked command's chips in place of its field, while this
        // composer is the one that reported the draft. Only the command list is drawn here: the emoji grid of
        // `slots::composer_popup` belongs to the channel's composer.
        let popup = crate::screens::commands::picker(&ctx, &focus, window, cx);
        let (chips_bar, chips) = match crate::slots::composer_chips(&ctx, &focus, window, cx) {
            Some(v) => (Some(v.bar), Some(v.row)),
            None => (None, None),
        };
        let field = div()
            .relative()
            .flex_1()
            .min_w_0()
            .min_h(px(FIELD_LINE + 2.0 * FIELD_PAD))
            .py(px(FIELD_PAD))
            .text_size(text::INPUT)
            .line_height(px(FIELD_LINE))
            .font_family(font::MONO)
            .text_color(color::FG)
            .child(tracked_focus(
                "thread-composer",
                &focus,
                crate::screens::composer::typed_text(
                    div()
                        .relative()
                        .w_full()
                        .h(px(rows as f32 * FIELD_LINE))
                        .child(crate::screens::messages::textarea_origin(
                            // The textarea's own size sets 14px text; the field's 13px on its lines are set on it, which
                            // wins.
                            Textarea::new(&self.input)
                                .aria_label(crate::strings::t("thread.composer"))
                                .appearance(false)
                                .disabled(blocked)
                                .text_size(text::INPUT)
                                .line_height(px(FIELD_LINE))
                                .h(px(rows as f32 * FIELD_LINE)),
                        )),
                    &self.input.read(cx).value(),
                    FIELD_LINE,
                ),
            ))
            .children(value_empty.then(|| {
                div()
                    .absolute()
                    .left_0()
                    .top(px(FIELD_PAD))
                    .right_0()
                    .flex()
                    .child(line(placeholder, text::INPUT, FIELD_LINE, color::FG_FAINT).ellipsis())
            }));
        // `.d-thread-pane__composer .d-composer`: 8px 12px 12px around a --bg-3 box in the hairline, 8px corners; the accent
        // line while the field is focused; `.d-composer__row` 4px 4px 4px 10px, the accent `›` and no send button.
        let composer = div()
            .relative()
            .on_action(cx.listener(|_, _: &Enter, _, _| {}))
            .px(px(thread_row::SIDE - 2.0))
            .pt(px(COMPOSER_TOP))
            .pb(px(COMPOSER_BOTTOM))
            // The picked command's bar sits over the box, as in the channel's composer (ComposerArea.tsx `top`).
            .children(chips_bar)
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(ROW_GAP))
                    .pl(px(ROW_LEFT))
                    .pr(px(ROW_PAD))
                    .py(px(ROW_PAD))
                    .bg(color::BG_3)
                    .border_1()
                    .border_color(if focused {
                        color::ACCENT
                    } else {
                        color::HAIRLINE_2
                    })
                    .rounded(radius::LG)
                    .child(
                        line("›", text::BASE, line_height(text::BASE, 1.5), color::ACCENT)
                            .weight(font::WEIGHT_DISPLAY),
                    )
                    .child(match chips {
                        // The textarea stays in the tree, empty-sized: it keeps the focus and its Enter sends the chips.
                        Some(chips) => div()
                            .flex_1()
                            .min_w_0()
                            .flex()
                            .child(chips)
                            .child(div().w_0().h_0().overflow_hidden().child(field))
                            .into_any_element(),
                        None => field.into_any_element(),
                    }),
            )
            .children(popup.map(|popup| {
                div()
                    .absolute()
                    .left_0()
                    .right_0()
                    .bottom_full()
                    .mb(px(POPUP_GAP))
                    .child(popup)
            }))
            .into_any_element();
        let close_focus = self.close_focus.clone();
        let parts = PaneParts {
            title: t("thread.title").into(),
            sub: sub.map(Into::into),
            close_id: "thread-close".into(),
            close_label: t("thread.close").into(),
            on_close: Rc::new(|_, window, cx| close_thread(window, cx)),
            wrap_close: Some(Box::new(move |close| {
                tracked_focus("thread-close", &close_focus, close).into_any_element()
            })),
            close_focus: Some(self.close_focus.clone()),
            root,
            rule: rule.map(Into::into),
            replies,
            log_enter: entering.log,
            earlier,
            busy: self.loading_earlier,
            empty: empty.map(Into::into),
            composer,
        };
        div()
            .key_context("ThreadPane")
            .on_action(cx.listener(|_, _: &super::keys::ClosePane, window, cx| {
                close_thread(window, cx);
            }))
            .h_full()
            .child(pane(parts))
            .into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::ChannelId;

    fn data(has_earlier: bool) -> ThreadData {
        ThreadData {
            channel: ChannelId([0; 16]),
            root: None,
            replies: Vec::new(),
            has_earlier,
            count: 0,
        }
    }

    /// The button follows `has_earlier` and is disabled while a page is being read.
    #[test]
    fn the_earlier_button_follows_has_earlier() {
        assert_eq!(earlier_shown(None, false), None);
        assert_eq!(earlier_shown(Some(&data(false)), false), None);
        assert_eq!(earlier_shown(Some(&data(true)), false), Some(true));
        assert_eq!(earlier_shown(Some(&data(true)), true), Some(false));
    }

    /// The window's composers (the channel's and a thread's) both draw the list slot: only the one that reported the draft
    /// draws the list.
    #[gpui::test]
    fn the_list_belongs_to_the_composer_that_reported_the_draft(cx: &mut gpui::TestAppContext) {
        use crate::screens::commands;
        use crate::slots::SlotCx;
        cx.update(crate::install);
        let window = cx.add_empty_window();
        window.update(|window, cx| {
            let w = window.window_handle().window_id();
            let (channel_focus, thread_focus) = (cx.focus_handle(), cx.focus_handle());
            let ctx = SlotCx::default();
            commands::set_composer(w, &thread_focus, cx);
            commands::draft_changed(w, "/", cx);
            assert!(commands::picker(&ctx, &channel_focus, window, cx).is_none());
            assert!(commands::picker(&ctx, &thread_focus, window, cx).is_some());
            // The chips follow the same owner.
            assert!(crate::slots::composer_chips(&ctx, &channel_focus, window, cx).is_none());
        });
    }

    /// The picker's chips belong to the composer that reported the draft. The other composer sends its own text, never reads
    /// or clears the owner's chips or draft, and the owner's send still runs them. Both directions: the thread owns them,
    /// then the channel's composer does.
    #[gpui::test]
    fn a_composer_never_sends_or_clears_the_other_composers_chips(cx: &mut gpui::TestAppContext) {
        use crate::screens::commands::{self, SendDecision, chips};
        cx.update(crate::install);
        let ctx = crate::slots::SlotCx {
            channel: Some(ChannelId([3; 16])),
            ..crate::slots::SlotCx::default()
        };
        let root = MessageId([7; 16]);
        let window = cx.add_empty_window();
        window.update(|window, cx| {
            let w = window.window_handle().window_id();
            let (channel, thread) = (cx.focus_handle(), cx.focus_handle());
            let picked = |cx: &mut gpui::App| {
                let mut c = chips::Chips::new("shrug".into());
                c.values = vec!["hi".into()];
                let s = commands::picker_state_mut(w, cx);
                s.draft = "/shrug".into();
                s.chips = Some(c);
            };
            let send = |ctx: &crate::slots::SlotCx,
                        thread: Option<MessageId>,
                        from: &gpui::FocusHandle,
                        draft: &str,
                        cx: &mut gpui::App| {
                commands::on_send_in(ctx, thread, Some(from), draft, &|t| t.to_owned(), w, cx)
            };
            for (owner, other, other_thread) in
                [(&thread, &channel, None), (&channel, &thread, Some(root))]
            {
                commands::set_composer(w, owner, cx);
                picked(cx);
                assert_eq!(chips::draft(&ctx, other, w, cx), None, "not the other's");
                assert!(!chips::hold_unnamed_member(&ctx, other, w, cx));
                assert_eq!(
                    send(&ctx, other_thread, other, "own text", cx),
                    SendDecision::Post("own text".into())
                );
                assert_eq!(
                    chips::draft(&ctx, owner, w, cx).as_deref(),
                    Some("/shrug text:\"hi\""),
                    "the owner's chips stay"
                );
                assert_eq!(
                    commands::picker_state(w, cx).draft,
                    "/shrug",
                    "and its draft"
                );
                // The owner's own send runs its chips and clears them.
                let owner_thread = if owner == &thread { Some(root) } else { None };
                assert!(matches!(
                    send(&ctx, owner_thread, owner, "/shrug text:\"hi\"", cx),
                    SendDecision::Post(t) if t.ends_with(" hi") && t.contains('¯')
                ));
                assert!(commands::picker_state(w, cx).chips.is_none());
            }
        });
    }

    /// Each composer has its own picker (CommandPicker.tsx:153-169): a command picked in the thread, with its chip values,
    /// survives typing in the channel's composer, and the channel's draft survives going back.
    #[gpui::test]
    fn typing_in_one_composer_keeps_the_other_composers_picked_command(
        cx: &mut gpui::TestAppContext,
    ) {
        use crate::screens::commands::{self, chips};
        cx.update(crate::install);
        let ctx = crate::slots::SlotCx {
            channel: Some(ChannelId([3; 16])),
            ..crate::slots::SlotCx::default()
        };
        let window = cx.add_empty_window();
        window.update(|window, cx| {
            let w = window.window_handle().window_id();
            let (channel, thread) = (cx.focus_handle(), cx.focus_handle());
            commands::set_composer(w, &thread, cx);
            let mut picked = chips::Chips::new("shrug".into());
            picked.values = vec!["hi".into()];
            let s = commands::picker_state_mut(w, cx);
            s.draft = "/shrug".into();
            s.chips = Some(picked);
            // The channel composer types: its own state, its own draft.
            commands::set_composer(w, &channel, cx);
            commands::draft_changed(w, "hello", cx);
            assert!(commands::picker_state(w, cx).chips.is_none());
            assert_eq!(
                chips::draft(&ctx, &thread, w, cx).as_deref(),
                Some("/shrug text:\"hi\""),
                "the thread's picked command and its chip values survive"
            );
            assert!(commands::activate(w, &channel, false, cx));
            assert_eq!(commands::picker_state(w, cx).draft, "hello");
        });
    }

    /// Both composers show a list at once: a click or hover on one's row acts on that composer's own state, whichever drew
    /// last (so whichever is active between frames).
    #[gpui::test]
    fn a_click_on_a_list_picks_in_the_composer_it_belongs_to(cx: &mut gpui::TestAppContext) {
        use crate::screens::commands;
        cx.update(crate::install);
        let window = cx.add_empty_window();
        window.update(|window, cx| {
            let w = window.window_handle().window_id();
            let (channel, thread) = (cx.focus_handle(), cx.focus_handle());
            for (focus, rows) in [(&thread, ["help", "me"]), (&channel, ["shrug", "code"])] {
                commands::set_composer(w, focus, cx);
                let s = commands::picker_state_mut(w, cx);
                s.draft = "/".into();
                s.rows = Some(rows.map(str::to_owned).to_vec());
            }
            // The channel's state is the active one (it drew last); the click is on the thread's list.
            commands::pick_in(w, &thread, 1, cx);
            assert_eq!(
                commands::picker_state(w, cx).chips.map(|c| c.command),
                Some("me".into()),
                "the thread's row 1"
            );
            assert!(commands::highlight_in(w, &channel, 1, cx));
            assert_eq!(commands::picker_state(w, cx).selected, 1);
            commands::pick_in(w, &channel, 0, cx);
            assert_eq!(
                commands::picker_state(w, cx).chips.map(|c| c.command),
                Some("shrug".into()),
                "the channel's row 0"
            );
            assert!(commands::activate(w, &thread, false, cx));
            assert_eq!(
                commands::picker_state(w, cx).chips.map(|c| c.command),
                Some("me".into()),
                "the thread's pick is still its own"
            );
        });
    }

    /// The pane starts a thread's composer with an empty picker (the web's `key={rootMsgId}`): what was picked for thread
    /// A is not there to run in thread B.
    #[gpui::test]
    fn another_thread_starts_with_an_empty_picker(cx: &mut gpui::TestAppContext) {
        use crate::screens::commands::{self, chips};
        cx.update(crate::install);
        let channel = ChannelId([3; 16]);
        let (a, b) = (MessageId([7; 16]), MessageId([8; 16]));
        let ctx = crate::slots::SlotCx {
            channel: Some(channel),
            ..crate::slots::SlotCx::default()
        };
        let (_host, vcx) = cx.add_window_view(|_, _| Host(ctx.clone()));
        let draw = |vcx: &mut gpui::VisualTestContext| {
            vcx.update(|window, cx| window.draw(cx).clear(cx));
            vcx.run_until_parked();
        };
        let w = vcx.update(|window, cx| {
            super::super::open_thread(channel, a, &ctx, cx);
            window.window_handle().window_id()
        });
        draw(vcx);
        let field = vcx.update(|_, cx| {
            let pane = super::super::ui(cx)
                .windows
                .get(&w)
                .and_then(|p| p.pane.clone())
                .expect("the pane is drawn");
            let input = pane.read(cx).input();
            input.read(cx).focus_handle(cx)
        });
        vcx.update(|_, cx| {
            commands::set_composer(w, &field, cx);
            let mut picked = chips::Chips::new("shrug".into());
            picked.values = vec!["hi".into()];
            commands::picker_state_mut(w, cx).chips = Some(picked);
            assert!(chips::draft(&ctx, &field, w, cx).is_some(), "picked in A");
        });
        vcx.update(|_, cx| super::super::open_thread(channel, b, &ctx, cx));
        draw(vcx);
        vcx.update(|_, cx| {
            assert_eq!(chips::draft(&ctx, &field, w, cx), None, "nothing of A in B");
        });
    }

    /// A Bot's poll post in a thread reads as in the log (MessageItem.tsx `displayBody`): who asked while the poll is held,
    /// nothing while it is being verified, its own text once given up; a plain reply is untouched. The poll is held under
    /// its message's id with the thread it was posted in, so a poll of the log never shares its record.
    #[test]
    fn a_poll_in_a_thread_reads_who_asked() {
        use dilla_client::state::bot::{PollCheck, PollView};
        use dilla_client::state::{CommunityId, Member, UserId};
        let (community, channel) = (CommunityId([7; 16]), ChannelId([8; 16]));
        let (root, reply, plain) = (MessageId([3; 16]), MessageId([4; 16]), MessageId([5; 16]));
        let member = |user: u8, name: &str, kind: u8| Member {
            user_id: UserId([user; 16]),
            username: name.to_lowercase(),
            display: name.to_owned(),
            kind,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        };
        let mut state = dilla_client::state::AppState::default();
        state.members.insert(
            community,
            vec![
                member(1, "Ada", 0),
                member(2, "bob", 0),
                member(9, "Glenn", 1),
            ],
        );
        state.account.user_id = Some(UserId([1; 16]));
        let item = |msg: MessageId, bot: bool, body: String| TimelineItem {
            key: format!("k{}", msg.0[0]),
            state: TimelineItemState::Ok,
            reason: String::new(),
            sender_user: Some(UserId([if bot { 9 } else { 1 }; 16])),
            sender_device: Some(dilla_client::state::DeviceId([9; 16])),
            own: false,
            bot,
            ts: 5,
            body,
            msg_id: Some(msg),
            seq: Some(5),
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
            actions: Vec::new(),
        };
        let marker = format!("[dilla-poll:v1:{}]", "ab".repeat(16));
        let poll_post = item(
            reply,
            true,
            format!("Raid?\nBridge\nCloses: <t:1800003600:R>\n{marker}"),
        );
        let ctx = |state: &dilla_client::state::AppState| crate::slots::SlotCx {
            state: std::sync::Arc::new(state.clone()),
            community: Some(community),
            channel: Some(channel),
            clock: crate::screens::threads::Clock::Fixed { now: 0, utc: true },
            ..crate::slots::SlotCx::default()
        };
        let held = |thread| PollView {
            id: [0xab; 16],
            channel,
            thread,
            creator: UserId([2; 16]),
            question: "Raid?".into(),
            answers: vec!["Bridge".into()],
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
        };
        let at = Some(root);
        // Bodies are decrypted text: a mismatch prints lengths only.
        let same =
            |a: &str, b: &str| assert!(a == b, "{} bytes against {} bytes", a.len(), b.len());
        // Nothing held and a client running: the card's loading line stands for the text.
        same(&thread_body(&poll_post, &ctx(&state), true, at), "");
        // A thread's poll and the log's, held side by side under their own message ids.
        let log_post = item(root, true, format!("Other?\n{marker}"));
        state.bot.polls.insert(reply, held(Some(root)));
        state.bot.polls.insert(root, held(None));
        same(
            &thread_body(&poll_post, &ctx(&state), true, at),
            "bob asked:",
        );
        same(
            &thread_body(&log_post, &ctx(&state), true, None),
            "bob asked:",
        );
        // Each record is read only for its own place: the log's poll shown as the thread's root (the web passes the root's
        // id for the root row too) reads on its own check, here given up, and the thread's poll is none in the log.
        state
            .bot
            .root_poll_checks
            .insert(root, PollCheck::GaveUp(channel));
        let in_pane = thread_body(&log_post, &ctx(&state), true, at);
        assert!(
            in_pane.contains("Other?") && !in_pane.contains("[dilla-poll"),
            "{} bytes",
            in_pane.len()
        );
        let in_log = thread_body(&poll_post, &ctx(&state), true, None);
        assert!(in_log.contains("Raid?"), "{} bytes", in_log.len());
        // Dropping the thread's record leaves the log's one drawing as before.
        state.bot.polls.remove(&reply);
        same(
            &thread_body(&log_post, &ctx(&state), true, None),
            "bob asked:",
        );
        same(&thread_body(&poll_post, &ctx(&state), true, at), "");
        // A read that was given up shows the post's own text, local time and no marker.
        state
            .bot
            .poll_checks
            .insert(reply, PollCheck::GaveUp(channel));
        let text = thread_body(&poll_post, &ctx(&state), true, at);
        assert!(
            text.contains("Closes: Fri 15 Jan, 09:00 UTC"),
            "{} bytes",
            text.len()
        );
        assert!(!text.contains("[dilla-poll"), "{} bytes", text.len());
        // A plain reply is its body.
        let plain_post = item(plain, false, "hello".into());
        same(&thread_body(&plain_post, &ctx(&state), true, at), "hello");
        // A forged marker (a person's reply, or one whose credential claims Bot while the member is not) reads as its raw text,
        // with no "asked" line even where a record is held for its id; the card's gate is `a_forged_poll_marker_draws_no_card`.
        let mut forged = item(MessageId([6; 16]), false, format!("Raid?\n{marker}"));
        let mut claimed = forged.clone();
        claimed.bot = true;
        forged.msg_id = Some(MessageId([6; 16]));
        claimed.msg_id = Some(MessageId([7; 16]));
        state.bot.polls.insert(MessageId([6; 16]), held(Some(root)));
        state.bot.polls.insert(MessageId([7; 16]), held(Some(root)));
        for post in [&forged, &claimed] {
            let body = thread_body(post, &ctx(&state), true, at);
            same(&body, &post.body);
        }
    }

    /// A message of the open thread as a `TimelineItem`: `bot` is the credential's claim.
    fn post(sender: u8, bot: bool, msg: u8, body: &str) -> TimelineItem {
        use dilla_client::state::UserId;
        TimelineItem {
            key: format!("k{msg}"),
            state: TimelineItemState::Ok,
            reason: String::new(),
            sender_user: Some(UserId([sender; 16])),
            sender_device: None,
            own: false,
            bot,
            ts: 1,
            body: body.into(),
            msg_id: Some(MessageId([msg; 16])),
            seq: Some(1),
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
            actions: Vec::new(),
        }
    }

    fn member_of(user: u8, kind: u8) -> dilla_client::state::Member {
        dilla_client::state::Member {
            user_id: dilla_client::state::UserId([user; 16]),
            username: format!("u{user}"),
            display: String::new(),
            kind,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        }
    }

    /// The root row of a poll post, with nothing held: while the pane's own read runs the body is blank (its loading card
    /// stands for it), once that gave up (or a hint asks it again) it is the post's text, and the log's own checks of the same
    /// post (given up there) decide nothing here. Reverting the root's own checks makes the log's GaveUp blank-or-text
    /// decide, which fails the Pending and the log-independent cases.
    #[test]
    fn a_thread_roots_body_follows_its_own_checks() {
        use dilla_client::state::bot::PollCheck;
        use dilla_client::state::{CommunityId, UserId};
        let (community, channel) = (CommunityId([7; 16]), ChannelId([8; 16]));
        let root = MessageId([3; 16]);
        let marker = format!("[dilla-poll:v1:{}]", "ab".repeat(16));
        let post = post(9, true, 3, &format!("Raid?\n{marker}"));
        let mut state = dilla_client::state::AppState::default();
        state
            .members
            .insert(community, vec![member_of(1, 0), member_of(9, 1)]);
        state.account.user_id = Some(UserId([1; 16]));
        let ctx = |state: &dilla_client::state::AppState| crate::slots::SlotCx {
            state: std::sync::Arc::new(state.clone()),
            community: Some(community),
            channel: Some(channel),
            clock: crate::screens::threads::Clock::Fixed { now: 0, utc: true },
            ..crate::slots::SlotCx::default()
        };
        let text = |state: &dilla_client::state::AppState, thread| {
            thread_body(&post, &ctx(state), true, thread)
        };
        let is_text = |b: &str| b.contains("Raid?") && !b.contains("[dilla-poll");
        // Nothing read yet: blank, as the log's loading post (the card then starts its read).
        assert!(text(&state, Some(root)).is_empty(), "blank while loading");
        state.bot.root_poll_checks.insert(root, PollCheck::Pending);
        assert!(text(&state, Some(root)).is_empty(), "blank while read");
        // Given up (or asked again after a hint, which stays given up until it succeeds): the post's text.
        for gave_up in [PollCheck::GaveUp(channel), PollCheck::Retry(channel)] {
            state.bot.root_poll_checks.insert(root, gave_up);
            let body = text(&state, Some(root));
            assert!(is_text(&body), "the fallback text, {} bytes", body.len());
        }
        // The log's check of the same post is its own: given up there, the pane's root still reads.
        state.bot.root_poll_checks.clear();
        state
            .bot
            .poll_checks
            .insert(root, PollCheck::GaveUp(channel));
        assert!(
            text(&state, Some(root)).is_empty(),
            "the log's check is not the pane's"
        );
        let in_log = text(&state, None);
        assert!(
            is_text(&in_log),
            "the log's own fallback, {} bytes",
            in_log.len()
        );
    }

    /// The row of the thread pane draws the Bot's tag for a Bot's post only when the credential's claim and the member row
    /// agree: a full pane is drawn with one reply, so reverting `row`'s call of the tag's decision fails this.
    #[gpui::test]
    fn the_panes_row_draws_the_bot_tag_only_for_a_real_bot(cx: &mut gpui::TestAppContext) {
        use dilla_client::state::CommunityId;
        cx.update(crate::install);
        let (community, channel) = (CommunityId([7; 16]), ChannelId([8; 16]));
        let root = MessageId([3; 16]);
        let pane_of = |sender: u8, claim: bool| {
            let mut state = dilla_client::state::AppState::default();
            state.members.insert(
                community,
                vec![member_of(1, 0), member_of(2, 0), member_of(9, 1)],
            );
            state.threads.threads.insert(
                root,
                ThreadData {
                    channel,
                    root: Some(post(1, false, 3, "root")),
                    replies: vec![post(sender, claim, 4, "reply")],
                    has_earlier: false,
                    count: 1,
                },
            );
            view_of(
                community,
                channel,
                crate::slots::SlotCx {
                    state: std::sync::Arc::new(state),
                    ..crate::slots::SlotCx::default()
                },
            )
        };
        let (host, vcx) = cx.add_window_view(|_, _| Host(pane_of(9, true)));
        let draw = |vcx: &mut gpui::VisualTestContext| {
            vcx.update(|window, cx| window.draw(cx).clear(cx));
            vcx.run_until_parked();
        };
        vcx.update(|_, cx| super::super::open_thread(channel, root, &pane_of(9, true), cx));
        draw(vcx);
        assert!(
            vcx.debug_bounds("thread-bot-tag").is_some(),
            "claim and member row"
        );
        for (sender, claim, why) in [
            (2, true, "the claim alone"),
            (9, false, "the member row alone"),
        ] {
            vcx.update(|_, cx| host.update(cx, |host, _| host.0 = pane_of(sender, claim)));
            draw(vcx);
            assert!(vcx.debug_bounds("thread-bot-tag").is_none(), "{why}");
        }
    }

    /// The Bot's tag and tone need the credential's claim and a Bot member row both: a claim alone (a member who set the bit in
    /// its own credential) is a person (MessageItem `item.bot && isCommunityBot`).
    #[test]
    fn a_self_signed_bot_claim_is_no_bot_in_a_thread() {
        use dilla_client::state::{Member, UserId};
        let member = |user: u8, kind: u8| Member {
            user_id: UserId([user; 16]),
            username: "x".into(),
            display: String::new(),
            kind,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        };
        let members = [member(1, 0), member(9, 1)];
        let post = |sender: u8, bot: bool| TimelineItem {
            key: "k".into(),
            state: TimelineItemState::Ok,
            reason: String::new(),
            sender_user: Some(UserId([sender; 16])),
            sender_device: None,
            own: false,
            bot,
            ts: 1,
            body: "hi".into(),
            msg_id: None,
            seq: None,
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
            actions: Vec::new(),
        };
        let community = dilla_client::state::CommunityId([7; 16]);
        let mut state = dilla_client::state::AppState::default();
        state.members.insert(community, members.to_vec());
        let ctx = crate::slots::SlotCx {
            state: std::sync::Arc::new(state),
            community: Some(community),
            ..crate::slots::SlotCx::default()
        };
        // `row` draws the tag and the tone from this.
        assert!(is_bot_row(&ctx, &post(9, true)), "claim and member row");
        assert!(!is_bot_row(&ctx, &post(1, true)), "the claim alone");
        assert!(!is_bot_row(&ctx, &post(9, false)), "the member row alone");
    }

    /// A view of `channel`, which the state lists in `community`.
    fn view_of(
        community: dilla_client::state::CommunityId,
        channel: ChannelId,
        shown: crate::slots::SlotCx,
    ) -> crate::slots::SlotCx {
        let mut state = (*shown.state).clone();
        state.channels.push(dilla_client::state::Channel {
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
            group: dilla_client::state::ChannelGroupState::Active,
        });
        crate::slots::SlotCx {
            state: std::sync::Arc::new(state),
            community: Some(community),
            channel: Some(channel),
            ..shown
        }
    }

    /// The web closes the thread when the conversation changes (Shell.tsx:391) and draws one only beside its own channel
    /// (Shell.tsx:1478): moving to another server's channel closes the pane and empties its composer's picker, so no
    /// command picked in the thread runs under the other server. A thread of a channel that is not shown is no pane.
    #[gpui::test]
    fn another_server_closes_the_thread_and_runs_none_of_its_commands(
        cx: &mut gpui::TestAppContext,
    ) {
        use crate::screens::commands::{self, chips};
        use dilla_client::state::CommunityId;
        cx.update(crate::install);
        let (x, y) = (ChannelId([3; 16]), ChannelId([4; 16]));
        let root = MessageId([7; 16]);
        let in_a = view_of(CommunityId([1; 16]), x, crate::slots::SlotCx::default());
        let in_b = view_of(CommunityId([2; 16]), y, in_a.clone());
        let (host, vcx) = cx.add_window_view(|_, _| Host(in_a.clone()));
        let draw = |vcx: &mut gpui::VisualTestContext| {
            vcx.update(|window, cx| window.draw(cx).clear(cx));
            vcx.run_until_parked();
        };
        let w = vcx.update(|window, cx| {
            super::super::open_thread(x, root, &in_a, cx);
            window.window_handle().window_id()
        });
        draw(vcx);
        let field = vcx.update(|_, cx| {
            let (field, shown) = super::super::composer_of(w, cx).expect("the pane is drawn");
            assert_eq!(shown, root);
            commands::set_composer(w, &field, cx);
            commands::picker_state_mut(w, cx).chips = Some(chips::Chips::new("shrug".into()));
            assert!(
                chips::draft(&in_a, &field, w, cx).is_some(),
                "picked in the thread"
            );
            field
        });
        vcx.update(|_, cx| host.update(cx, |host, _| host.0 = in_b.clone()));
        draw(vcx);
        vcx.update(|window, cx| {
            assert_eq!(
                super::super::ui(cx).local_open,
                None,
                "the thread is closed"
            );
            assert!(!super::super::is_open(&in_b, cx));
            assert!(super::super::pane(&crate::router::Route::Root, window, cx).is_none());
            assert_eq!(super::super::composer_of(w, cx), None, "no thread is shown");
            assert_eq!(
                chips::draft(&in_b, &field, w, cx),
                None,
                "nothing picked runs"
            );
        });
        // A thread still open in a channel that is not shown (another window of the client opened it) draws no pane.
        vcx.update(|_, cx| super::super::ui(cx).local_open = Some(OpenThread { channel: x, root }));
        draw(vcx);
        vcx.update(|window, cx| {
            assert!(!super::super::is_open(&in_b, cx));
            assert!(super::super::pane(&crate::router::Route::Root, window, cx).is_none());
            assert_eq!(
                super::super::ui(cx).local_open,
                Some(OpenThread { channel: x, root }),
                "not this window's to close"
            );
        });
        // Another channel of the same server closes it too.
        vcx.update(|_, cx| host.update(cx, |host, _| host.0 = in_a.clone()));
        draw(vcx);
        assert!(vcx.update(|_, cx| super::super::is_open(&in_a, cx)));
        let in_a_other = view_of(CommunityId([1; 16]), y, in_a.clone());
        vcx.update(|_, cx| host.update(cx, |host, _| host.0 = in_a_other));
        draw(vcx);
        assert_eq!(vcx.update(|_, cx| super::super::ui(cx).local_open), None);
    }

    /// A thread's commands carry the thread's own server, read from its channel, never only the view's.
    #[test]
    fn a_thread_command_runs_under_the_threads_community() {
        use dilla_client::state::CommunityId;
        let (a, b) = (CommunityId([1; 16]), CommunityId([2; 16]));
        let (x, y) = (ChannelId([3; 16]), ChannelId([4; 16]));
        let in_a = view_of(a, x, crate::slots::SlotCx::default());
        let in_b = view_of(b, y, in_a);
        let at = at_thread(
            &in_b,
            OpenThread {
                channel: x,
                root: MessageId([7; 16]),
            },
        );
        assert_eq!(at.channel, Some(x));
        assert_eq!(
            at.community,
            Some(a),
            "the thread's server, not the shown one"
        );
        let own = at_thread(
            &in_b,
            OpenThread {
                channel: y,
                root: MessageId([8; 16]),
            },
        );
        assert_eq!(own.community, Some(b));
    }

    /// The pane's window: it records what it draws as the shell does (`threads::sync`), then draws the pane.
    struct Host(crate::slots::SlotCx);

    impl Render for Host {
        fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
            super::super::sync(&self.0, window, cx);
            div().child(
                super::super::pane(&crate::router::Route::Root, window, cx)
                    .unwrap_or_else(|| div().into_any_element()),
            )
        }
    }

    /// Task 30's command memory binds a reopened draft to the thread it was typed in: the thread's composer takes it, and
    /// the picker opens on it; a draft left for another thread stays untaken.
    #[gpui::test]
    fn an_uncertain_thread_command_reopens_in_the_thread(cx: &mut gpui::TestAppContext) {
        use crate::screens::commands;
        cx.update(crate::install);
        let channel = ChannelId([3; 16]);
        let (root, other) = (MessageId([7; 16]), MessageId([8; 16]));
        let ctx = crate::slots::SlotCx {
            channel: Some(channel),
            ..crate::slots::SlotCx::default()
        };
        let (_host, vcx) = cx.add_window_view(|_, _| Host(ctx.clone()));
        let draw = |vcx: &mut gpui::VisualTestContext| {
            vcx.update(|window, cx| window.draw(cx).clear(cx));
            vcx.run_until_parked();
        };
        let w = vcx.update(|window, cx| {
            super::super::open_thread(channel, root, &ctx, cx);
            window.window_handle().window_id()
        });
        draw(vcx);
        let field = |vcx: &mut gpui::VisualTestContext| {
            vcx.update(|_, cx| {
                let pane = super::super::ui(cx)
                    .windows
                    .get(&w)
                    .and_then(|p| p.pane.clone())
                    .expect("the pane is drawn");
                let input = pane.read(cx).input();
                input.read(cx).value().to_string()
            })
        };
        assert_eq!(field(vcx), "");
        let at = |thread| Conversation {
            channel,
            thread: Some(thread),
        };
        vcx.update(|_, cx| commands::reopen(w, None, at(other), "/pin message:y".into(), cx));
        draw(vcx);
        assert_eq!(field(vcx), "", "another thread's draft is not taken here");
        assert!(
            vcx.update(|_, cx| commands::picker_state(w, cx).reopen)
                .is_some(),
            "it waits for its own thread"
        );
        vcx.update(|_, cx| commands::reopen(w, None, at(root), "/pin message:x".into(), cx));
        draw(vcx);
        assert_eq!(field(vcx), "/pin message:x", "the thread's composer");
        assert_eq!(
            vcx.update(|_, cx| commands::picker_state(w, cx).draft),
            "/pin message:x",
            "the picker opens on it"
        );
        assert!(
            vcx.update(|_, cx| commands::picker_state(w, cx).reopen)
                .is_none(),
            "taken once"
        );
    }
}

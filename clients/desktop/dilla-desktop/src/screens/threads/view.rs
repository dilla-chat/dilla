//! The thread pane (ThreadView.tsx, ThreadPane.css): the root, a rule with the reply count, the replies and the thread's own
//! composer. It reads the client's `AppState::threads` through the [`SlotCx`] its own window's shell last drew (see the
//! module docs), and
//! sends through [`dilla_client::features::threads::Threads`]. Opening it leaves the composer unfocused (wave B parity round 2).

use std::rc::Rc;

use dilla_client::state::{MessageId, TimelineItem, TimelineItemState};
use dilla_desktop_ui::IconName;
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
use crate::tracked_focus;

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
/// The hover toolbar's gap between its buttons (MessageRow.css `__toolbar` gap 2px).
const TOOL_GAP: f32 = 2.0;

fn max_rows() -> usize {
    ((FIELD_MAX - 2.0 * FIELD_PAD) / FIELD_LINE).floor() as usize
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

/// Emitted when the thread composer sends: the thread's root and the text.
pub struct Sent(pub MessageId, pub String);

pub struct PaneView {
    /// The window this pane is drawn in: it reads and sends through that window's shell view only.
    window: gpui::WindowId,
    input: Entity<TextareaState>,
    close_focus: FocusHandle,
    /// A send is in flight: the field is empty but the draft stays in the prefs until the send succeeds.
    in_flight: bool,
    /// The root whose draft the field currently holds.
    shown: Option<MessageId>,
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
            _subscriptions: vec![sub],
        }
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
                cx.notify();
            }
            InputEvent::PressEnter { shift: false, .. } => {
                let composing = input
                    .update(cx, |s, cx| s.marked_text_range(window, cx))
                    .is_some();
                let draft = input.read(cx).value().to_string();
                // A reader who cannot send has a disabled composer (ThreadView `blocked`).
                if composing || draft.trim().is_empty() || !ctx.state.threads.can_send(open.channel)
                {
                    return;
                }
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
    fn settle_draft(&mut self, root: MessageId, window: &mut Window, cx: &mut Context<Self>) {
        if self.shown == Some(root) {
            return;
        }
        self.shown = Some(root);
        let draft = current(self.window, cx)
            .and_then(|c| c.client.map(|client| client.threads().draft(root)))
            .unwrap_or_default();
        self.input
            .update(cx, |s, cx| s.set_value(draft, window, cx));
    }

    fn row(
        &self,
        item: &TimelineItem,

        spaced: bool,
        ctx: &crate::slots::SlotCx,
        window: &mut Window,
        cx: &mut gpui::App,
    ) -> gpui::AnyElement {
        let author = name_of(&ctx.state, ctx.community, item.sender_user);
        let bot = ctx
            .state
            .members
            .get(
                &ctx.community
                    .unwrap_or(dilla_client::state::CommunityId([0; 16])),
            )
            .into_iter()
            .flatten()
            .any(|m| Some(m.user_id) == item.sender_user && m.kind == 1)
            || item.bot;
        let (kind, body) = match item.state {
            TimelineItemState::Ok => (ThreadRowKind::Ok, item.body.clone()),
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

impl Render for PaneView {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let Some(ctx) = current(self.window, cx) else {
            return div().into_any_element();
        };
        let Some(open) = open_of(&ctx, cx) else {
            return div().into_any_element();
        };
        self.settle_draft(open.root, window, cx);
        // The rows' actions act on the thread's channel (the shell's view may show another one).
        let ctx = crate::slots::SlotCx {
            channel: Some(open.channel),
            ..ctx
        };
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
            Some(root) => self.row(root, false, &ctx, window, cx),
            None => line(
                t("thread.rootMissing"),
                text::SM,
                line_height(text::SM, 1.5),
                color::FG_3,
            )
            .into_any_element(),
        });
        let replies: Vec<_> = data
            .map(|d| {
                d.replies
                    .iter()
                    .enumerate()
                    .map(|(n, item)| self.row(item, n > 0, &ctx, window, cx))
                    .collect()
            })
            .unwrap_or_default();
        // Fix round 2 of the web: the rule counts the thread's replies (the root's summary), not only those read so far.
        let count = data.map_or(0, |d| d.count.max(d.replies.len() as u32));
        let rule = match count {
            0 => None,
            1 => Some(t("thread.summary.one").to_owned()),
            n => Some(tf("thread.summary.other", &[("count", &n.to_string())])),
        };
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
                div().w_full().h(px(rows as f32 * FIELD_LINE)).child(
                    // The textarea's own size sets 14px text; the field's 13px on its lines are set on it, which wins.
                    Textarea::new(&self.input)
                        .aria_label(crate::strings::t("thread.composer"))
                        .appearance(false)
                        .disabled(blocked)
                        .text_size(text::INPUT)
                        .line_height(px(FIELD_LINE))
                        .h(px(rows as f32 * FIELD_LINE)),
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
            .on_action(cx.listener(|_, _: &Enter, _, _| {}))
            .px(px(thread_row::SIDE - 2.0))
            .pt(px(COMPOSER_TOP))
            .pb(px(COMPOSER_BOTTOM))
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
                    .child(field),
            )
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

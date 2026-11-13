//! The messages area's dialogs: the delete confirmation and the pins list (packages/ui `PinsDialog`, web
//! `conversation/Pins.tsx` and the delete `Dialog` of `Conversation.tsx`). They are drawn over the whole window, so the
//! shell hosts them with the other overlays (`slots::overlays` calls [`overlays`]).

use dilla_client::state::{AppState, ChannelId, Member, MessageId};
use gpui::{AnyElement, App, AppContext as _, IntoElement, ParentElement, Styled, Window, div};

use dilla_desktop_ui::components::button::{Button, ButtonSize, ButtonVariant};
use dilla_desktop_ui::components::dialog::Dialog;
use dilla_desktop_ui::text::{line, line_height, paragraph};
use dilla_desktop_ui::tokens::{color, font, text};

use super::mentions::excerpt;
use super::ui::{self, handle};
use crate::format;
use crate::screens::messages::css::len;
use crate::slots::SlotCx;
use crate::strings::{t, tf};

/// The dialogs the state asks for, in paint order: the delete confirmation, then the pins. They draw with what the
/// timeline was last drawn with.
pub fn overlays(window: &mut Window, cx: &mut App) -> Vec<AnyElement> {
    let Some(ui) = handle(window, cx) else {
        return Vec::new();
    };
    let Some(ctx) = ui.read(cx).ctx.clone() else {
        return Vec::new();
    };
    let ctx = &ctx;
    let (deleting, pins) = {
        let s = ui.read(cx);
        (s.deleting, s.pins)
    };
    let mut out = Vec::new();
    if deleting.is_some() {
        let close = ui.clone();
        let button = Button::new("message-delete-confirm", t("shell.delete.confirm"))
            .variant(ButtonVariant::Danger)
            .size(ButtonSize::Dialog)
            .on_click(move |_, window, cx| confirm_delete(window, cx));
        let body = div().flex().flex_col().children(paragraph(
            t("shell.delete.body"),
            text::UI,
            line_height(text::UI, 1.5),
            color::FG_2,
            440.0,
        ));
        out.push(
            Dialog::new(
                "message-delete",
                t("shell.delete.title"),
                t("shell.delete.cancel"),
            )
            .body(body)
            .footer(button)
            .on_close(move |_, cx| close.update(cx, |ui, cx| ui.cancel_delete(cx)))
            .into_any_element(),
        );
    }
    if let Some(channel) = pins {
        out.push(pins_dialog(ctx, channel, &ui, window));
    }
    out
}

fn name_of(
    state: &AppState,
    channel: ChannelId,
    user: Option<dilla_client::state::UserId>,
) -> String {
    let members: &[Member] = state
        .channel(channel)
        .and_then(|c| state.members.get(&c.community_id))
        .map_or(&[], Vec::as_slice);
    user.and_then(|u| members.iter().find(|m| m.user_id == u))
        .map_or_else(
            || t("shell.status.unknown").to_owned(),
            |m| {
                if m.display.is_empty() {
                    m.username.clone()
                } else {
                    m.display.clone()
                }
            },
        )
}

fn pins_dialog(
    ctx: &SlotCx,
    channel: ChannelId,
    ui: &gpui::Entity<ui::MessageUi>,
    _window: &mut Window,
) -> AnyElement {
    let state = &ctx.state;
    let name = state
        .channel(channel)
        .map_or_else(String::new, |c| c.name.clone());
    let pins = state
        .messages
        .pins
        .get(&channel)
        .cloned()
        .unwrap_or_default();
    let now = ctx.clock.now();
    let mut list = div().flex().flex_col().w_full();
    if pins.is_empty() {
        list = list.child(
            div()
                .w_full()
                .py(len(12.))
                .flex()
                .justify_center()
                .child(line(
                    t("shell.pins.empty"),
                    text::UI,
                    line_height(text::UI, 1.5),
                    color::FG_3,
                )),
        );
    }
    for (n, pin) in pins.iter().enumerate() {
        let author = name_of(state, channel, pin.sender_user);
        let by = tf(
            "shell.pins.by",
            &[("name", &name_of(state, channel, Some(pin.pinned_by)))],
        );
        let when = format::relative(pin.ts, now);
        let text_lines = paragraph(
            &excerpt(&pin.excerpt, 120),
            text::SM,
            line_height(text::SM, 1.5),
            color::FG_BODY,
            480.0 - 32.0,
        );
        let unpin = {
            let msg = pin.msg_id;
            Button::new(("pins-unpin", n), t("shell.message.unpin"))
                .variant(ButtonVariant::Ghost)
                .on_click(move |_, window, cx| unpin_from_pins(channel, msg, window, cx))
        };
        let jump = {
            let msg = pin.msg_id;
            Button::new(("pins-jump", n), t("shell.pins.jump"))
                .variant(ButtonVariant::Default)
                .on_click(move |_, window, cx| jump_from_pins(channel, msg, window, cx))
        };
        let item = div()
            .flex()
            .flex_col()
            .gap(len(4.))
            .py(len(10.))
            .px(len(16.))
            .when_else_border(n);
        list = list.child(
            item.child(
                div()
                    .flex()
                    .items_baseline()
                    .gap(len(8.))
                    .child(
                        line(author, text::SM, line_height(text::SM, 1.5), color::FG)
                            .weight(font::WEIGHT_DISPLAY),
                    )
                    .child(line(
                        when,
                        text::META,
                        line_height(text::META, 1.5),
                        color::FG_3,
                    )),
            )
            .child(div().flex().flex_col().children(text_lines))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(len(6.))
                    .child(div().flex_1().child(line(
                        by,
                        text::META,
                        line_height(text::META, 1.5),
                        color::FG_3,
                    )))
                    .child(jump)
                    .child(unpin),
            ),
        );
    }
    let close = ui.clone();
    Dialog::new(
        "message-pins",
        tf("shell.pins.title", &[("channel", &name)]),
        t("shell.pins.close"),
    )
    .body(list)
    .on_close(move |_, cx| close.update(cx, |ui, cx| ui.close_pins(cx)))
    .into_any_element()
}

/// The delete dialog's confirm: the message waiting for its confirmation is deleted for everyone, and the dialog closes.
pub fn confirm_delete(window: &mut Window, cx: &mut App) {
    let Some(ui) = handle(window, cx) else {
        return;
    };
    let (deleting, client) = {
        let s = ui.read(cx);
        (s.deleting, s.ctx.as_ref().and_then(|c| c.client.clone()))
    };
    if let (Some(d), Some(client)) = (deleting, client) {
        let done = client.messages().delete(d.channel, d.msg);
        cx.background_spawn(async move {
            let _ = done.await;
        })
        .detach();
    }
    ui.update(cx, |ui, cx| ui.cancel_delete(cx));
}

/// The pins dialog's Unpin on the pin of `msg` in `channel`: the pin goes and the list is read again; the dialog stays.
pub fn unpin_from_pins(channel: ChannelId, msg: MessageId, window: &mut Window, cx: &mut App) {
    let Some(ui) = handle(window, cx) else {
        return;
    };
    if let Some(client) = ui.read(cx).ctx.as_ref().and_then(|c| c.client.clone()) {
        let done = client.messages().pin(channel, msg, false);
        let reload = client.messages().load_pins(channel);
        cx.background_spawn(async move {
            let _ = done.await;
            let _ = reload.await;
        })
        .detach();
    }
    ui.update(cx, |_, cx| cx.notify());
}

/// The pins dialog's Jump on the pin of `msg` in `channel`: the dialog closes and the log scrolls to the message and
/// flashes it; the pin of a thread's reply (no row of the channel) opens that thread instead (Shell.tsx `threadOfJump`).
pub fn jump_from_pins(channel: ChannelId, msg: MessageId, window: &mut Window, cx: &mut App) {
    let Some(ui) = handle(window, cx) else {
        return;
    };
    let Some(ctx) = ui.read(cx).ctx.clone() else {
        return;
    };
    let root = ctx
        .state
        .messages
        .pins
        .get(&channel)
        .and_then(|pins| pins.iter().find(|p| p.msg_id == msg))
        .and_then(|p| p.thread_root);
    if let Some(root) = root {
        ui.update(cx, |ui, cx| ui.close_pins(cx));
        crate::screens::threads::open_thread(channel, root, &ctx, cx);
        return;
    }
    let key = ctx.state.timelines.get(&channel).and_then(|tl| {
        tl.items
            .iter()
            .find(|i| i.msg_id == Some(msg))
            .map(|i| i.key.clone())
    });
    ui.update(cx, |ui, cx| {
        ui.close_pins(cx);
        if let Some(key) = key {
            ui.jump_to(key, cx);
        }
    });
}

/// A pin row's top rule: every row but the first has a 1px hairline above it.
trait PinRow: Styled + Sized {
    fn when_else_border(self, n: usize) -> Self;
}

impl PinRow for gpui::Div {
    fn when_else_border(self, n: usize) -> Self {
        if n == 0 {
            self.pt(len(0.))
        } else {
            self.border_t_1().border_color(color::HAIRLINE)
        }
    }
}

//! The message composer (packages/ui Composer, Signal §5.6), built on Task 4's proof (`experiments/gpui-input-proof`):
//! gpui-component's `Textarea` with `submit_on_enter`, so Enter sends and Shift+Enter starts a new line, as on the web.
//! The owner swallows the `Enter` action the textarea propagates (else GPUI would also type its "\n" into the draft),
//! and Enter during an IME composition confirms it and never sends (the web's `isComposing`).
//!
//! Layout (Composer.css): 16px in from the sides and 14px above the window's edge, a bordered box with a 2px sweep, a row
//! (padding 6 6 4 12, 8px gaps: the `›` prompt, the field, `send ↵`) and a foot (padding 0 6 6, 2px gaps: attach, emoji,
//! code, then the `⇧↵ new line` hint). The field grows with its lines from one (30px: 20px lines, 5px above and below)
//! up to the CSS cap of 13.75rem.

use gpui::{
    AppContext as _, Context, Entity, EntityInputHandler, EventEmitter, FocusHandle, Focusable,
    InteractiveElement as _, IntoElement, ParentElement, Render, SharedString, Styled,
    Subscription, Window, div, px,
};
use gpui_component::input::{Enter, InputEvent, Textarea, TextareaState};

use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::tracked_box;

/// Composer.css `.d-composer__input`: 20px lines, 5px above and below, at most 13.75rem (220px) high.
const FIELD_LINE: f32 = 20.0;
const FIELD_PAD: f32 = 5.0;
const FIELD_MAX: f32 = 220.0;

/// The rows the field grows to before it scrolls.
fn max_rows() -> usize {
    ((FIELD_MAX - 2.0 * FIELD_PAD) / FIELD_LINE).floor() as usize
}

/// Emitted when the person sends the draft: its text, as typed.
pub struct Sent(pub String);

pub struct Composer {
    pub input: Entity<TextareaState>,
    /// `#general`: the placeholder names the channel.
    target: SharedString,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<Sent> for Composer {}

impl Focusable for Composer {
    fn focus_handle(&self, cx: &gpui::App) -> FocusHandle {
        self.input.read(cx).focus_handle(cx)
    }
}

impl Composer {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let input = cx.new(|cx| TextareaState::new(window, cx).submit_on_enter(true));
        let sub = cx.subscribe_in(&input, window, Self::on_input_event);
        Self {
            input,
            target: SharedString::default(),
            _subscriptions: vec![sub],
        }
    }

    /// The channel the draft goes to (`#general`).
    pub fn set_target(&mut self, target: SharedString, cx: &mut Context<Self>) {
        if self.target != target {
            self.target = target;
            cx.notify();
        }
    }

    pub fn value(&self, cx: &gpui::App) -> SharedString {
        self.input.read(cx).value()
    }

    fn on_input_event(
        &mut self,
        input: &Entity<TextareaState>,
        event: &InputEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            InputEvent::Change => cx.notify(),
            InputEvent::PressEnter { shift: false, .. } => {
                let composing = input
                    .update(cx, |s, cx| s.marked_text_range(window, cx))
                    .is_some();
                let draft = input.read(cx).value().to_string();
                if composing || draft.trim().is_empty() {
                    return;
                }
                input.update(cx, |s, cx| s.set_value("", window, cx));
                cx.emit(Sent(draft));
                cx.notify();
            }
            _ => {}
        }
    }
}

/// A 26x24 tool button of the foot with a 14px icon.
fn tool(icon: IconName) -> impl IntoElement {
    div()
        .w(px(26.))
        .h(px(24.))
        .flex_none()
        .flex()
        .items_center()
        .justify_center()
        .rounded(radius::SM)
        .child(Icon::new(icon).size(px(14.)).color(color::FG_3))
}

impl Render for Composer {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let value = self.input.read(cx).value();
        let empty = value.is_empty();
        // The field is as high as the draft's lines (20px each), up to the cap; soft-wrapped lines are not counted.
        let rows = value.split('\n').count().clamp(1, max_rows());
        let placeholder = format!("Message {}", self.target);
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
            .child(tracked_box(
                "composer",
                div().w_full().h(px(rows as f32 * FIELD_LINE)).child(
                    Textarea::new(&self.input)
                        .appearance(false)
                        .h(px(rows as f32 * FIELD_LINE)),
                ),
            ))
            .children(empty.then(|| {
                div()
                    .absolute()
                    .left_0()
                    .top(px(FIELD_PAD))
                    .right_0()
                    .flex()
                    .child(line(placeholder, text::INPUT, FIELD_LINE, color::FG_FAINT).ellipsis())
            }));
        // An empty draft's `send` is the disabled one (`--surface-hi`, `--fg-4`); a draft lights it up.
        let (send_bg, send_fg) = if empty {
            (color::SURFACE_HI, color::FG_4)
        } else {
            (color::ACCENT, color::ACCENT_INK)
        };
        let send = div()
            .flex_none()
            .h(px(28.))
            .px(px(10.))
            .rounded(radius::ROW)
            .bg(send_bg)
            .flex()
            .items_center()
            .child(
                line("send ↵", text::BTN, line_height(text::BTN, 1.5), send_fg)
                    .weight(font::WEIGHT_DISPLAY),
            );
        let row = div()
            .flex()
            .items_center()
            .gap(px(8.))
            .pt(px(6.))
            .pr(px(6.))
            .pb(px(4.))
            .pl(px(12.))
            .child(
                line("›", text::BASE, line_height(text::BASE, 1.5), color::ACCENT)
                    .weight(font::WEIGHT_DISPLAY),
            )
            .child(field)
            .child(send);
        let foot = div()
            .flex()
            .items_center()
            .gap(px(2.))
            .px(px(6.))
            .pb(px(6.))
            .child(tool(IconName::Paperclip))
            .child(tool(IconName::Smile))
            .child(tool(IconName::CodeXml))
            .child(div().flex_1())
            .child(div().ml(px(10.)).child(line(
                "⇧↵ new line",
                text::META,
                line_height(text::META, 1.5),
                color::FG_3,
            )));
        div()
            // The textarea propagates a plain Enter so its owner can send; swallowing it here stops GPUI from then also
            // typing the key's "\n" into the draft.
            .on_action(cx.listener(|_, _: &Enter, _, _| {}))
            .w_full()
            .bg(color::BG)
            .px(px(16.))
            .pb(px(14.))
            .child(
                div()
                    .w_full()
                    .bg(color::SURFACE)
                    .border_1()
                    .border_color(color::HAIRLINE_2)
                    .rounded(radius::LG)
                    .flex()
                    .flex_col()
                    .child(div().h(px(2.)))
                    .child(row)
                    .child(foot),
            )
    }
}

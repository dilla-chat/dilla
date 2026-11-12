//! The message composer (packages/ui Composer, Signal §5.6), built on Task 4's proof (`experiments/gpui-input-proof`):
//! gpui-component's `Textarea` with `submit_on_enter`, so Enter sends and Shift+Enter starts a new line, as on the web.
//! The owner swallows the `Enter` action the textarea propagates (else GPUI would also type its "\n" into the draft),
//! and Enter during an IME composition confirms it and never sends (the web's `isComposing`).

use gpui::{
    AppContext as _, Context, Entity, EntityInputHandler, EventEmitter, FocusHandle, Focusable,
    InteractiveElement as _, IntoElement, ParentElement, Render, SharedString, Styled,
    Subscription, Window, div, px,
};
use gpui_component::input::{Enter, InputEvent, Textarea, TextareaState};

use dilla_desktop_ui::tokens::{color, font, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::text::{Edge, line};
use crate::tracked_box;

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

/// A 26x24 tool button of the composer's foot with a 14px icon.
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
        let empty = self.input.read(cx).value().is_empty();
        let placeholder = format!("Message {}", self.target);
        // Composer.css: the box 16px in from the sides and 14px above the window's edge; the 2px sweep, then a 40px row
        // (the `›` prompt, the field, `send ↵`) and a 30px foot (attach, emoji, code, the `⇧↵ new line` hint).
        let field = div()
            .absolute()
            .left(px(28.11))
            .top(px(6.))
            .w(px(839.73))
            .h(px(30.))
            .child(
                div()
                    .size_full()
                    .text_size(text::INPUT)
                    .line_height(px(20.))
                    .font_family(font::MONO)
                    .text_color(color::FG)
                    .child(tracked_box(
                        "composer",
                        div()
                            .w(px(839.73))
                            .h(px(30.))
                            .child(Textarea::new(&self.input).appearance(false).h(px(30.))),
                    )),
            )
            .children(empty.then(|| {
                div().absolute().left_0().top(px(5.)).child(
                    line(placeholder, text::INPUT, 20.0, color::FG_FAINT)
                        .at(Edge::Start(345.11), Edge::End(74.0)),
                )
            }));
        let send = div()
            .absolute()
            .right(px(6.))
            .top(px(7.))
            .w(px(64.16))
            .h(px(28.))
            .rounded(radius::ROW)
            .bg(color::SURFACE_HI)
            .flex()
            .items_center()
            .pl(px(10.))
            .child(
                line("send ↵", text::BTN, 17.25, color::FG_4)
                    .weight(font::WEIGHT_DISPLAY)
                    .at(Edge::End(77.16), Edge::End(72.625)),
            );
        let row = div()
            .absolute()
            .left_0()
            .top(px(2.))
            .w_full()
            .h(px(40.))
            .child(
                div().absolute().left(px(12.)).top(px(10.875)).child(
                    line("›", text::BASE, 20.25, color::ACCENT)
                        .weight(font::WEIGHT_DISPLAY)
                        .at(Edge::Start(329.0), Edge::End(74.125)),
                ),
            )
            .child(field)
            .child(send);
        let foot = div()
            .absolute()
            .left_0()
            .top(px(42.))
            .w_full()
            .h(px(30.))
            .child(
                div()
                    .absolute()
                    .left(px(6.))
                    .top_0()
                    .flex()
                    .gap(px(2.))
                    .child(tool(IconName::Paperclip))
                    .child(tool(IconName::Smile))
                    .child(tool(IconName::CodeXml)),
            )
            .child(
                div().absolute().right(px(6.)).top(px(4.125)).child(
                    line("⇧↵ new line", text::META, 15.75, color::FG_3)
                        .at_end(Edge::End(23.0), Edge::End(40.875)),
                ),
            );
        div()
            // The textarea propagates a plain Enter so its owner can send; swallowing it here stops GPUI from then also
            // typing the key's "\n" into the draft.
            .on_action(cx.listener(|_, _: &Enter, _, _| {}))
            .size_full()
            .bg(color::BG)
            .px(px(16.))
            .pb(px(14.))
            .child(
                div()
                    .relative()
                    .size_full()
                    .bg(color::SURFACE)
                    .border_1()
                    .border_color(color::HAIRLINE_2)
                    .rounded(radius::LG)
                    .child(row)
                    .child(foot),
            )
    }
}

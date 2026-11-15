//! The thread pane's frame (ThreadPane.css): a head (the 14px glyph, "Thread", the place, a 32px ×), the root with its
//! rule ("3 REPLIES", uppercase, a hairline to the end), the replies and the thread's composer at the bottom. It is a
//! column in the shell's aside, `--thread-w` wide on `--bg-2`.

use gpui::{
    AnyElement, App, ClickEvent, ElementId, FocusHandle, InteractiveElement, IntoElement,
    ParentElement, Pixels, SharedString, StatefulInteractiveElement, Styled, Window, div, px,
    relative,
};
use std::rc::Rc;

use crate::a11y::{Accessible, Role};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::text::{line, line_height};
use crate::tokens::{color, font, layout, radius, text};

use super::thread_row::SIDE;

/// `--bar-h` plus the 1px rule under the head (`box-sizing: border-box`).
const HEAD_PAD_LEFT: f32 = 16.0;
const HEAD_PAD_RIGHT: f32 = 8.0;
const HEAD_GAP: f32 = 8.0;
const GLYPH: f32 = 14.0;
const CLOSE_BOX: f32 = 32.0;
const CLOSE_GLYPH: f32 = 15.0;
/// The root: `padding: 14px 14px 0`, at most 40% of the scroll area.
const ROOT_TOP: f32 = 14.0;
const ROOT_MAX: f32 = 0.4;
/// The rule: `margin: 12px 14px 0`, `gap: 8px`.
const RULE_TOP: f32 = 12.0;
const RULE_GAP: f32 = 8.0;
/// The replies: `padding: 12px 14px 6px`.
const REPLIES_TOP: f32 = 12.0;
const REPLIES_BOTTOM: f32 = 6.0;

/// The pane's width.
pub fn width() -> Pixels {
    layout::THREAD
}

/// The head's height: `--bar-h` and its bottom rule.
pub fn head_height() -> f32 {
    f32::from(layout::BAR) + 1.0
}

type Close = Rc<dyn Fn(&ClickEvent, &mut Window, &mut App)>;

/// What the pane holds.
pub struct PaneParts {
    pub title: SharedString,
    /// `#general`, after the title.
    pub sub: Option<SharedString>,
    pub close_id: ElementId,
    pub close_label: SharedString,
    pub on_close: Close,
    /// Wraps the × button (the screen's automation probe).
    pub wrap_close: Option<Box<dyn FnOnce(AnyElement) -> AnyElement>>,
    /// The × button is a tab stop with this handle; Enter and Space press it.
    pub close_focus: Option<FocusHandle>,
    /// The root's row, or the "cannot be shown" note.
    pub root: Option<AnyElement>,
    /// "3 replies", drawn uppercase; `None` draws no rule.
    pub rule: Option<SharedString>,
    pub replies: Vec<AnyElement>,
    /// A note over an empty reply list ("No replies yet.").
    pub empty: Option<SharedString>,
    pub composer: AnyElement,
}

/// The pane.
pub fn pane(parts: PaneParts) -> impl IntoElement {
    let ui = text::UI;
    let head = div()
        .flex()
        .flex_none()
        .items_center()
        .gap(px(HEAD_GAP))
        .h(px(head_height()))
        .min_w_0()
        .pl(px(HEAD_PAD_LEFT))
        .pr(px(HEAD_PAD_RIGHT))
        .border_b_1()
        .border_color(color::HAIRLINE)
        .child(
            Icon::new(IconName::MessagesSquare)
                .size(px(GLYPH))
                .color(color::FG),
        )
        .child(
            div().flex_1().min_w_0().flex().child(
                line(parts.title, ui, line_height(ui, 1.5), color::FG)
                    .weight(font::WEIGHT_DISPLAY)
                    .ellipsis(),
            ),
        )
        .children(
            parts
                .sub
                .map(|sub| line(sub, text::META, line_height(text::META, 1.5), color::FG_3)),
        )
        .child({
            let on_close = parts.on_close.clone();
            let mut close = div()
                .id(parts.close_id)
                .a11y(Role::Button, parts.close_label.clone())
                .flex()
                .flex_none()
                .items_center()
                .justify_center()
                .size(px(CLOSE_BOX))
                .rounded(radius::MD)
                .cursor_pointer()
                .hover(|s| s.bg(color::HOVER))
                .on_click(move |e, window, cx| on_close(e, window, cx))
                .child(
                    Icon::new(IconName::X)
                        .size(px(CLOSE_GLYPH))
                        .color(color::FG_2),
                );
            if let Some(handle) = &parts.close_focus {
                let press = parts.on_close.clone();
                close = close
                    .track_focus(handle)
                    .tab_stop(true)
                    .focus_visible(|s| s.bg(color::HOVER))
                    .on_key_down(move |event, window, cx| {
                        if matches!(event.keystroke.key.as_str(), "enter" | "space") {
                            cx.stop_propagation();
                            press(&ClickEvent::default(), window, cx);
                        }
                    });
            }
            match parts.wrap_close {
                Some(wrap) => wrap(close.into_any_element()),
                None => close.into_any_element(),
            }
        });
    let root = parts.root.map(|root| {
        div()
            .id("thread-root")
            .flex_none()
            .min_w_0()
            .max_h(relative(ROOT_MAX))
            .overflow_y_scroll()
            .px(px(SIDE))
            .pt(px(ROOT_TOP))
            .child(root)
    });
    let rule = parts.rule.filter(|_| root.is_some()).map(|label| {
        let size = text::MICRO;
        div()
            .flex()
            .flex_none()
            .items_center()
            .gap(px(RULE_GAP))
            .mt(px(RULE_TOP))
            .mx(px(SIDE))
            .child(
                line(
                    label.to_uppercase(),
                    size,
                    line_height(size, 1.5),
                    color::FG_3,
                )
                .tracking(crate::metrics::LABEL_TRACKING_WIDE),
            )
            .child(div().flex_1().h(px(1.0)).bg(color::HAIRLINE))
    });
    let replies = div()
        .id("thread-replies")
        .flex_1()
        .min_h_0()
        .min_w_0()
        .overflow_y_scroll()
        .px(px(SIDE))
        .pt(px(REPLIES_TOP))
        .pb(px(REPLIES_BOTTOM))
        .children(
            parts
                .empty
                .map(|note| line(note, text::SM, line_height(text::SM, 1.5), color::FG_3)),
        )
        .children(parts.replies);
    div()
        .id("thread-pane")
        .flex()
        .flex_col()
        .flex_none()
        .w(width())
        .h_full()
        .min_h_0()
        .bg(color::BG_2)
        .child(head)
        .child(
            div()
                .flex()
                .flex_col()
                .flex_1()
                .min_h_0()
                .min_w_0()
                .children(root)
                .children(rule)
                .child(replies),
        )
        .child(div().flex_none().min_w_0().child(parts.composer))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_pane_is_the_threads_column_and_its_head_the_bar_and_a_rule() {
        assert_eq!(width(), px(340.0));
        assert_eq!(head_height(), 45.0);
    }
}

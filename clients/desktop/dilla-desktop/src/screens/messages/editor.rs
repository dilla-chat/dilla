//! The inline editor that replaces a row's body while its message is edited (packages/ui `MessageEditor`,
//! MessageEditor.css): a bordered field (1px `--hairline-2`, 8px corners, `--surface`, 5px 12px padding, 13px text on 20px
//! lines, at most 10rem of lines), 4px below it the foot: the hint (10.5px `--fg-3`), the counter near the budget and
//! Cancel and Save. Enter saves, Shift+Enter breaks the line, Escape cancels.

use gpui::{
    AnyElement, App, Focusable as _, InteractiveElement as _, IntoElement, ParentElement, Styled,
    Window, div, prelude::FluentBuilder as _, px,
};
use gpui_component::input::{Enter, Textarea, TextareaState};

use dilla_desktop_ui::components::button::{Button, ButtonVariant};
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, radius, text};

use super::ui::{EDIT_MAX, MessageUi};
use crate::screens::messages::css::len;
use crate::strings::{t, tf};

const LINE: f32 = 20.0;
const PAD_Y: f32 = 5.0;
const PAD_X: f32 = 12.0;
/// `min-height: 1.875rem` and `max-height: 10rem`, both on the content box.
const MIN_FIELD: f32 = 30.0;
const MAX_FIELD: f32 = 160.0;
/// A foot button's height (`Button`'s Md size).
const FOOT: f32 = 32.0;
const GAP: f32 = 4.0;
/// The focused field's `--accent-soft` ring: `box-shadow: 0 0 0 3px`.
const FOCUS_RING: f32 = 3.0;

/// The field's height for `lines` lines of text: the content box (clamped) plus padding and the 1px edge.
pub fn field_height(lines: usize) -> f32 {
    // `box-sizing: border-box` (the 5px padding and the 1px edge are inside the 30px minimum and the 10rem maximum).
    (lines as f32 * LINE + 2.0 * PAD_Y + 2.0).clamp(MIN_FIELD, MAX_FIELD)
}

/// The editor's height: the field, the gap, the foot.
pub fn height(lines: usize) -> f32 {
    field_height(lines) + GAP + FOOT
}

/// The counter shows when at most a tenth of the budget is left (`maxLength / 10`).
pub fn counter_visible(remaining: i64) -> bool {
    remaining <= (EDIT_MAX / 10) as i64
}

/// The rows of the text being edited in a field `width` px wide (the textarea grows with its soft-wrapped lines,
/// `field-sizing: content`): each hard line takes as many rows as the mono pre-wrap breaks it into.
/// The width the field gives its text: less the padding and the 1px edge on each side.
fn text_width(width: f32) -> f32 {
    width - 2.0 * PAD_X - 2.0
}

pub fn lines_of(value: &str, width: f32) -> usize {
    let inner = text_width(width);
    let columns = dilla_desktop_ui::text::columns(text::INPUT, inner);
    dilla_desktop_ui::text::wrap_mono(value, columns)
        .len()
        .max(1)
}

/// The editor of `editing`, `width` wide, as an element of height [`height`].
pub fn editor(
    input: &gpui::Entity<TextareaState>,
    used: usize,
    ui: &gpui::Entity<MessageUi>,
    width: f32,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    let value = input.read(cx).value();
    let lines = lines_of(&value, width);
    let used = used as i64;
    let remaining = EDIT_MAX as i64 - used;
    let refused = value.trim().is_empty() || remaining < 0;
    // `--hairline-2`; focused, the accent edge and a 3px `--accent-soft` ring (`.d-message-editor__input:focus`).
    let focused = input.read(cx).focus_handle(cx).is_focused(window);
    let field = div()
        .w_full()
        .h(px(field_height(lines)))
        .px(px(PAD_X))
        .py(px(PAD_Y))
        .bg(color::SURFACE)
        .border_1()
        .border_color(if focused {
            color::ACCENT
        } else {
            color::HAIRLINE_2
        })
        .when(focused, |d| {
            d.shadow(vec![gpui::BoxShadow {
                color: color::ACCENT_SOFT,
                offset: gpui::point(px(0.), px(0.)),
                blur_radius: px(0.),
                spread_radius: px(FOCUS_RING),
                inset: false,
            }])
        })
        .rounded(radius::LG)
        .text_size(text::INPUT)
        .line_height(px(LINE))
        .font_family(font::MONO)
        .text_color(color::FG)
        .child(super::textarea_origin(
            // The textarea's own size sets 14px text; the field's 13px on 20px lines are set on it, which wins.
            Textarea::new(input)
                .appearance(false)
                .text_size(text::INPUT)
                .line_height(px(LINE))
                .w(px(text_width(width) + super::TEXTAREA_WRAP_EXTRA))
                // Its own 8px above and below the lines take room in its box too, or the last line is cut.
                .h(px(
                    field_height(lines) - 2.0 * PAD_Y - 2.0 + 2.0 * super::TEXTAREA_PAD_Y
                )),
        ));
    let mut foot = div()
        .flex()
        .items_center()
        .gap(len(8.))
        .h(px(FOOT))
        .child(line(
            t("shell.edit.hint"),
            text::META,
            line_height(text::META, 1.5),
            color::FG_3,
        ));
    if counter_visible(remaining) {
        let label = if remaining < 0 {
            tf("shell.composer.over", &[("n", &(-remaining).to_string())])
        } else {
            tf("shell.composer.remaining", &[("n", &remaining.to_string())])
        };
        let tone = if remaining < 0 {
            color::DANGER
        } else {
            color::FG_3
        };
        foot = foot.child(line(label, text::XS, line_height(text::XS, 1.5), tone));
    }
    let cancel = ui.clone();
    let save = ui.clone();
    foot = foot.child(
        div()
            .flex()
            .gap(len(8.))
            .ml_auto()
            .child(
                Button::new("message-edit-cancel", t("shell.edit.cancel"))
                    .variant(ButtonVariant::Ghost)
                    .on_click(move |_, _, cx| cancel.update(cx, |ui, cx| ui.cancel_edit(cx))),
            )
            .child(
                Button::new("message-edit-save", t("shell.edit.save"))
                    .variant(ButtonVariant::Accent)
                    .disabled(refused)
                    .on_click(move |_, _, cx| save.update(cx, |ui, cx| ui.save_edit(cx))),
            ),
    );
    let escape = ui.clone();
    crate::tracked_box(
        "message-editor",
        div()
            // The textarea propagates a plain Enter so its owner can save; swallowing it stops GPUI typing "\n".
            .on_action(|_: &Enter, _, _| {})
            .on_key_down(move |event, _, cx| {
                if event.keystroke.key == "escape" {
                    cx.stop_propagation();
                    escape.update(cx, |ui, cx| ui.cancel_edit(cx));
                }
            })
            .w(px(width))
            .flex()
            .flex_col()
            .gap(px(GAP))
            .child(field)
            .child(foot),
    )
    .into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_field_is_30px_of_content_plus_padding_and_edge_and_grows_to_10rem() {
        assert_eq!(field_height(1), 32.0);
        assert_eq!(field_height(5), 112.0);
        assert_eq!(field_height(50), 160.0);
        assert_eq!(height(1), 32.0 + 4.0 + 32.0);
    }

    #[test]
    fn the_counter_shows_within_a_tenth_of_the_budget() {
        assert!(!counter_visible(401));
        assert!(counter_visible(400));
        assert!(counter_visible(-3));
    }

    #[test]
    fn lines_are_the_hard_breaks() {
        assert_eq!(lines_of("", 400.0), 1);
        assert_eq!(lines_of("a\nb\n", 400.0), 3);
        // 400px less the padding and edge is 48 columns of 7.8px: a 100-character line takes three rows.
        assert_eq!(lines_of(&"x".repeat(100), 400.0), 3);
    }
}

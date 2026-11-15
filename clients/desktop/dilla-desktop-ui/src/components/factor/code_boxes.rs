//! `CodeBoxes` (packages/ui/src/CodeBoxes): the prototype's six-digit code, six 48px boxes in a six-column grid with 8px
//! gaps, `--surface-2` with a 1px edge (empty `--control-edge-field`, filled `--control-edge-key`, the caret's box the
//! accent with its 3px `--accent-ring`), radius `--r-lg`, the digit `--text-heading` bold. An invalid code edges every box
//! in the danger colour.
//!
//! It draws only: the host owns the digits, the caret and the focus (it reads keys and the clipboard), so typing, a paste
//! of a whole code and Backspace all go through one place (`screens::factor::code`).

use std::time::Duration;

use gpui::{
    App, BoxShadow, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, Styled,
    Window, div, point, px,
};

use crate::text::line;
use crate::tokens::{color, font, radius, text};

/// Digits in a code.
pub const CODE_LENGTH: usize = 6;
/// The prototype's pause between the sixth digit and the step going on (`COMPLETE_DELAY_MS`).
pub const COMPLETE_DELAY: Duration = Duration::from_millis(350);
/// The box's height inside its 1px edge (`3rem`).
const BOX_INNER: f32 = 48.0;
/// The grid's gap.
const GAP: f32 = 8.0;
/// The caret ring's spread (`0 0 0 3px var(--accent-ring)`).
const RING: f32 = 3.0;

/// CodeBoxes' `digitsOf`: the digits of `text` (anything else dropped), at most six.
pub fn code_digits(text: &str) -> String {
    text.chars()
        .filter(char::is_ascii_digit)
        .take(CODE_LENGTH)
        .collect()
}

#[derive(IntoElement)]
pub struct CodeBoxes {
    id: ElementId,
    value: String,
    caret: usize,
    focused: bool,
    invalid: bool,
}

impl CodeBoxes {
    /// `value` is the digits typed so far (0 to 6).
    pub fn new(id: impl Into<ElementId>, value: &str) -> Self {
        let value = code_digits(value);
        let caret = value.len();
        Self {
            id: id.into(),
            value,
            caret,
            focused: false,
            invalid: false,
        }
    }

    /// Where the next digit lands (the host's caret); clamped to the digits typed, and with all six to the last box.
    pub fn caret(mut self, caret: usize) -> Self {
        self.caret = caret;
        self
    }

    /// The host's input has focus: the caret's box lights.
    pub fn focused(mut self, focused: bool) -> Self {
        self.focused = focused;
        self
    }

    pub fn invalid(mut self, invalid: bool) -> Self {
        self.invalid = invalid;
        self
    }
}

impl RenderOnce for CodeBoxes {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let caret = self.caret.min(self.value.len()).min(CODE_LENGTH - 1);
        let digits: Vec<char> = self.value.chars().collect();
        let boxes = (0..CODE_LENGTH).map(|i| {
            let filled = i < digits.len();
            let lit = self.focused && i == caret;
            let edge = if self.invalid {
                color::DANGER
            } else if lit {
                color::ACCENT
            } else if filled {
                color::CONTROL_EDGE_KEY
            } else {
                color::CONTROL_EDGE_FIELD
            };
            let mut cell = div()
                .flex_1()
                .min_w_0()
                .h(px(BOX_INNER + 2.0))
                .bg(color::SURFACE_2)
                .border_1()
                .border_color(edge)
                .rounded(radius::LG)
                .flex()
                .items_center()
                .justify_center();
            if lit && !self.invalid {
                cell = cell.shadow(vec![BoxShadow {
                    color: color::ACCENT_RING,
                    offset: point(px(0.0), px(0.0)),
                    blur_radius: px(0.0),
                    spread_radius: px(RING),
                    inset: false,
                }]);
            }
            let size = f32::from(text::HEADING);
            cell.children(digits.get(i).map(|d| {
                line(d.to_string(), text::HEADING, size, color::FG).weight(font::WEIGHT_DISPLAY)
            }))
        });
        div()
            .id(self.id)
            .w_full()
            .flex()
            .gap(px(GAP))
            .children(boxes)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// CodeBoxes.test.tsx: non-digits are dropped and a grouped or newline-ended paste fills all six.
    #[test]
    fn digits_of_a_paste() {
        assert_eq!(code_digits("123 456"), "123456");
        assert_eq!(code_digits("123456\n"), "123456");
        assert_eq!(code_digits("12a3"), "123");
        assert_eq!(code_digits("1234567"), "123456");
    }
}

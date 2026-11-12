//! Task 10's components (`components/reactions/`): the reaction bar (`ReactionBar`), the emoji grid (`EmojiGrid`) and the
//! who-reacted tooltip (`ReactionTooltip`), ported from packages/ui/src/{ReactionBar,EmojiGrid,ReactionTooltip}.

pub mod bar;
pub mod grid;
pub mod tooltip;

pub use bar::{Chip, Decorate, ReactionBar, Slot, bar_height, chip_width, wrap_lines};
pub use grid::{EmojiGrid, EmojiItem, GridEvent, Part, grid_next};
pub use tooltip::ReactionTooltip;

use gpui::{AnyElement, IntoElement, ParentElement, Pixels, SharedString, Styled, div};

use crate::text::{line, line_height};
use crate::tokens::color;

/// The platform's colour emoji face.
fn color_family() -> &'static str {
    if cfg!(target_os = "macos") {
        "Apple Color Emoji"
    } else if cfg!(target_os = "windows") {
        "Segoe UI Emoji"
    } else {
        "Noto Color Emoji"
    }
}

/// Noto Color Emoji's advance (2550 units of 2048 per em): what Chromium lays one emoji out at.
pub const EMOJI_ADVANCE_EM: f32 = 2550.0 / 2048.0;

/// Whether `text` is emoji and nothing else: symbols and pictographs (and the selectors and joiners between them), no
/// letter, digit or ASCII, so a received reaction that is plain text keeps the mono face.
pub fn is_emoji(text: &str) -> bool {
    !text.is_empty()
        && text
            .chars()
            .all(|c| u32::from(c) >= 0x2190 || c == '\u{200d}' || c == '\u{fe0f}')
}

/// The face for `text` when it is emoji: the platform's colour face, which draws the emoji presentation (U+FE0F) the mono
/// face would show as a flat text glyph.
pub fn emoji_family(text: &str) -> Option<&'static str> {
    is_emoji(text).then(color_family)
}

/// The width one emoji takes at `size` in Chromium (its advance), or `None` for text that is not emoji.
pub fn emoji_advance(text: &str, size: Pixels) -> Option<f32> {
    is_emoji(text).then(|| f32::from(size) * EMOJI_ADVANCE_EM)
}

/// An emoji set at `size` with line height 1 (`line-height: 1`), in a box of its advance (so the layout is Chromium's
/// whichever face draws it); other text in the mono face.
pub fn emoji_text(text: impl Into<SharedString>, size: Pixels) -> AnyElement {
    let text = text.into();
    match emoji_family(&text) {
        Some(family) => div()
            .flex_none()
            .whitespace_nowrap()
            .w(gpui::px(f32::from(size) * EMOJI_ADVANCE_EM))
            .text_size(size)
            .line_height(gpui::px(line_height(size, 1.0)))
            .font_family(family)
            .text_color(color::FG)
            .child(text)
            .into_any_element(),
        None => line(text, size, line_height(size, 1.0), color::FG).into_any_element(),
    }
}

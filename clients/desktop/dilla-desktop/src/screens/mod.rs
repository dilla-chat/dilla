//! The slice's screens and the pieces they share.

pub mod channel_list;
pub mod composer;
pub mod shell;
pub mod sign_in;
pub mod timeline;

use gpui::{App, IntoElement, KeyBinding, ParentElement, Pixels, Styled, actions, div};

actions!(dilla_desktop, [FocusNext, FocusPrevious]);

/// `--label-tracking-wide` (packages/design-tokens): the letter spacing of tracked capitals, in em.
pub const LABEL_TRACKING_WIDE: f32 = 0.12;

/// Tab and Shift+Tab move the focus through the screen's fields and buttons in their order, as in the browser.
pub fn bind_keys(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("tab", FocusNext, None),
        KeyBinding::new("shift-tab", FocusPrevious, None),
    ]);
}

/// A square of `size` holding a centred child (an icon button's hit area).
pub fn centred(size: Pixels, child: impl IntoElement) -> gpui::Div {
    div()
        .size(size)
        .flex_none()
        .flex()
        .items_center()
        .justify_center()
        .child(child)
}

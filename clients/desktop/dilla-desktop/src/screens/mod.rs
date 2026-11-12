//! The screens and the pieces they share. One module (a file or a directory) per area of the ownership map
//! (clients/desktop/OWNERSHIP.md); the list is Task 1's, and no Wave 1 task adds a line here.

// Task 8 (shell).
pub mod channel_list;
pub mod header;
pub mod rail;
pub mod shell;
pub mod titlebar;
pub mod user_panel;
// Task 9 (messages).
pub mod composer;
pub mod messages;
pub mod timeline;
// Tasks 10-23.
pub mod attachments;
pub mod bot_admin;
pub mod commands;
pub mod dms;
pub mod members;
pub mod notify;
pub mod reactions;
pub mod ring;
pub mod search;
pub mod server_settings;
pub mod threads;
pub mod user_settings;
pub mod voice;
pub mod voice_settings;
// Task 24 (diagnostics).
pub mod boot;
pub mod diagnostics;
// Task 25 (onboarding).
pub mod create;
pub mod onboarding;
// Task 26 (factors).
pub mod factor;
pub mod sign_in;
// Task 27 (instances).
pub mod instances;

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

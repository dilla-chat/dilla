//! The slice's screens and the pieces they share.

pub mod channel_list;
pub mod composer;
pub mod shell;
pub mod sign_in;
pub mod timeline;

use gpui::{App, Hsla, IntoElement, KeyBinding, ParentElement, Pixels, Styled, actions, div, hsla};

use dilla_desktop_ui::components::avatar::{colour_of, initials_of};
use dilla_desktop_ui::tokens::{color, font};

use crate::text::line;

actions!(dilla_desktop, [FocusNext, FocusPrevious]);

/// Tab and Shift+Tab move the focus through the screen's fields and buttons in their order, as in the browser.
pub fn bind_keys(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("tab", FocusNext, None),
        KeyBinding::new("shift-tab", FocusPrevious, None),
    ]);
}

/// The avatar's disc colour: the Signal palette entry of the person's name (`avatarColour` in Avatar.tsx).
pub fn avatar_colour(name: &str) -> Hsla {
    let (h, s, l) = colour_of(name);
    hsla(
        f32::from(h) / 360.0,
        f32::from(s) / 100.0,
        f32::from(l) / 100.0,
        1.0,
    )
}

/// A round avatar with the name's initials (Avatar.tsx): `size` the disc, `text_size` / `line_height` the initials.
pub fn avatar(name: &str, size: Pixels, text_size: Pixels, line_height: f32) -> impl IntoElement {
    div()
        .size(size)
        .flex_none()
        .rounded_full()
        .bg(avatar_colour(name))
        .flex()
        .items_center()
        .justify_center()
        .child(
            line(initials_of(name), text_size, line_height, color::BRAND_INK)
                .weight(font::WEIGHT_DISPLAY),
        )
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

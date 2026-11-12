//! `DropOverlay` (packages/ui/src/DropOverlay): over the main pane while files are dragged over it, a dashed accent
//! frame and a panel with its title and body. Pointer-only, so it takes no focus; it covers its positioned parent.

use gpui::{AnyElement, IntoElement, ParentElement, SharedString, Styled, div, px};

use super::LEADING;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

/// `.d-drop-overlay::before { inset: 0.5rem }`.
const FRAME_INSET: f32 = 8.0;

/// The overlay; place it as the last child of a `relative` box.
pub fn drop_overlay(title: impl Into<SharedString>, body: impl Into<SharedString>) -> AnyElement {
    // `color-mix(in srgb, var(--bg) 80%, transparent)`.
    let mut veil = color::BG;
    veil.a = 0.8;
    div()
        .absolute()
        .inset_0()
        .flex()
        .items_center()
        .justify_center()
        .bg(veil)
        .child(
            div()
                .absolute()
                .inset(px(FRAME_INSET))
                .border_2()
                .border_dashed()
                .border_color(color::ACCENT)
                .rounded(radius::XL),
        )
        .child(
            div()
                .relative()
                .flex()
                .flex_col()
                .items_center()
                .gap(px(4.0))
                .px(px(24.0))
                .py(px(16.0))
                .bg(color::SURFACE_2)
                .border_1()
                .border_color(color::LINE_STRONG)
                .rounded(radius::XL)
                .child(
                    line(title, text::MD, line_height(text::MD, LEADING), color::FG)
                        .weight(font::WEIGHT_DISPLAY),
                )
                .child(line(body, text::SM, line_height(text::SM, LEADING), color::FG_2)),
        )
        .into_any_element()
}

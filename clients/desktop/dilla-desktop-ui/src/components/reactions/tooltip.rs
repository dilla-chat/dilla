//! `ReactionTooltip` (packages/ui/src/ReactionTooltip): the who-reacted tooltip, the content of a tooltip over a reaction
//! chip: the emoji at 22px, then up to six mini avatars (16px, overlapping by 4px, each ringed in the tooltip's colour)
//! over the caller's sentence. The box is 240px at most, `--toast` with radius `--r-lg` and `--shadow-reaction`.
//!
//! [`tooltip`] is the builder for `StatefulInteractiveElement::tooltip`, with the same 400 ms delay as the shared tooltip.

use gpui::prelude::FluentBuilder as _;
use gpui::{
    AnyView, App, AppContext, Context, IntoElement, ParentElement, Render, SharedString, Styled,
    Window, div, hsla, px,
};

use super::super::avatar::{colour_of, initials_of};
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

/// At most this many faces (`avatars.slice(0, 6)`).
pub const MAX_FACES: usize = 6;
/// The face's size (`1rem`), the overlap (`-0.25rem`), the ring (`2px`) and the initials (`0.40625rem`).
pub const FACE: f32 = 16.0;
pub const FACE_OVERLAP: f32 = 4.0;
pub const FACE_RING: f32 = 2.0;
pub const FACE_INITIALS: f32 = 6.5;
/// The box's widest (`15rem`), its padding (`0.5rem 0.625rem`) and the gap between emoji and body (`0.625rem`).
pub const MAX_WIDTH: f32 = 240.0;

pub struct ReactionTooltip {
    emoji: SharedString,
    text: SharedString,
    names: Vec<SharedString>,
}

impl ReactionTooltip {
    pub fn new(
        emoji: impl Into<SharedString>,
        text: impl Into<SharedString>,
        names: Vec<SharedString>,
    ) -> Self {
        Self {
            emoji: emoji.into(),
            text: text.into(),
            names,
        }
    }

    /// The faces the tooltip draws: the first [`MAX_FACES`] names.
    pub fn faces(&self) -> &[SharedString] {
        &self.names[..self.names.len().min(MAX_FACES)]
    }
}

/// A 16px face: the name's colour with its initials, ringed in the tooltip's colour by a 2px spread shadow (the ring takes
/// no room), each overlapping the next by 4px.
fn face(name: &str) -> impl IntoElement + use<> {
    let (h, s, l) = colour_of(name);
    let disc = hsla(f32::from(h) / 360.0, f32::from(s) / 100.0, f32::from(l) / 100.0, 1.0);
    div()
        .flex()
        .flex_none()
        .items_center()
        .justify_center()
        .size(px(FACE))
        .mr(px(-FACE_OVERLAP))
        .rounded(radius::AVATAR)
        .bg(disc)
        .shadow(vec![gpui::BoxShadow {
            color: color::TOAST,
            offset: gpui::point(px(0.0), px(0.0)),
            blur_radius: px(0.0),
            spread_radius: px(FACE_RING),
            inset: false,
        }])
        .child(
            line(
                initials_of(name),
                px(FACE_INITIALS),
                line_height(px(FACE_INITIALS), 1.5),
                color::BRAND_INK,
            )
            .weight(font::WEIGHT_DISPLAY),
        )
}

impl Render for ReactionTooltip {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let faces = div()
            .flex()
            .items_center()
            .h(px(FACE))
            .children(self.faces().iter().map(|n| face(n)));
        let body = div()
            .flex()
            .flex_col()
            .gap(px(4.0))
            .when(!self.names.is_empty(), |d| d.child(faces))
            .child(
                div()
                    .text_size(text::XS)
                    .line_height(px(line_height(text::XS, 1.5)))
                    .font_family(font::MONO)
                    .text_color(color::FG_CHIP)
                    .child(self.text.clone()),
            );
        div()
            .flex()
            .items_center()
            .gap(px(10.0))
            .max_w(px(MAX_WIDTH))
            .px(px(10.0))
            .py(px(8.0))
            .bg(color::TOAST)
            .rounded(radius::LG)
            .shadow(vec![gpui::BoxShadow {
                color: hsla(0.0, 0.0, 0.0, 0.55),
                offset: gpui::point(px(0.0), px(12.0)),
                blur_radius: px(30.0),
                spread_radius: px(0.0),
                inset: false,
            }])
            .child(
                div()
                    .flex_none()
                    .child(line(
                        self.emoji.clone(),
                        px(22.0),
                        line_height(px(22.0), 1.0),
                        color::FG,
                    )),
            )
            .child(body)
    }
}

/// The builder `tooltip(...)` takes: a who-reacted tooltip for `emoji`, with the caller's sentence and the names of the
/// reactors it knows.
pub fn tooltip(
    emoji: impl Into<SharedString>,
    text: impl Into<SharedString>,
    names: Vec<SharedString>,
) -> impl Fn(&mut Window, &mut App) -> AnyView + 'static {
    let (emoji, text) = (emoji.into(), text.into());
    move |_window, cx| {
        let view = ReactionTooltip::new(emoji.clone(), text.clone(), names.clone());
        cx.new(|_| view).into()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn at_most_six_faces_are_drawn() {
        let names: Vec<SharedString> = (0..9).map(|i| format!("p{i}").into()).collect();
        let tip = ReactionTooltip::new("👍", "9 people", names);
        assert_eq!(tip.faces().len(), 6);
        assert_eq!(ReactionTooltip::new("👍", "x", Vec::new()).faces().len(), 0);
    }
}

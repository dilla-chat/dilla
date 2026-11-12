//! `Tooltip` (packages/ui/src/Tooltip): the label (and the chord) of an icon control, 11px in a `--surface-2` box with the
//! `--line-strong` edge, shown 400 ms after the pointer enters (R15, WCAG 1.4.13) and hidden when it leaves.
//!
//! Use [`tooltip`] with `StatefulInteractiveElement::tooltip` and [`SHOW_DELAY`] with `tooltip_show_delay`; GPUI keeps the
//! tooltip while the pointer is on its owner, hides it on leave, and a press dismisses it.

use std::time::Duration;

use gpui::{
    AnyView, App, AppContext, Context, IntoElement, ParentElement, Render, SharedString, Styled, Window, div,
    px,
};

use crate::metrics::shadow_1;
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

/// Tooltip.tsx `SHOW_MS`.
pub const SHOW_DELAY: Duration = Duration::from_millis(400);
/// Tooltip.tsx `HIDE_MS`.
pub const HIDE_DELAY: Duration = Duration::from_millis(100);

pub struct TooltipView {
    label: SharedString,
    keys: Vec<SharedString>,
}

impl TooltipView {
    pub fn new(label: impl Into<SharedString>, keys: &[&str]) -> Self {
        Self {
            label: label.into(),
            keys: keys
                .iter()
                .map(|k| SharedString::from((*k).to_owned()))
                .collect(),
        }
    }

    /// The text of the chord as the tooltip shows it: the keys joined with `+`.
    pub fn chord(&self) -> String {
        self.keys
            .iter()
            .map(SharedString::as_ref)
            .collect::<Vec<_>>()
            .join("+")
    }
}

impl Render for TooltipView {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let mut root = div()
            .flex()
            .items_center()
            .gap(px(8.0))
            .max_w(px(320.0))
            .px(px(8.0))
            .py(px(4.0))
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::LINE_STRONG)
            .rounded(radius::MD)
            .shadow(shadow_1())
            .child(line(self.label.clone(), text::XS, 16.0, color::FG));
        if !self.keys.is_empty() {
            root = root.child(line(
                self.chord(),
                text::XS,
                line_height(text::XS, 1.5),
                color::FG_3,
            ));
        }
        root
    }
}

/// The builder `tooltip(...)` takes: a tooltip of `label` and the chord `keys`.
pub fn tooltip(
    label: impl Into<SharedString>,
    keys: &[&str],
) -> impl Fn(&mut Window, &mut App) -> AnyView + 'static {
    let label: SharedString = label.into();
    let keys: Vec<String> = keys.iter().map(|k| (*k).to_owned()).collect();
    move |_window, cx| {
        let refs: Vec<&str> = keys.iter().map(String::as_str).collect();
        let view = TooltipView::new(label.clone(), &refs);
        cx.new(|_| view).into()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_delays_are_the_webs() {
        assert_eq!(SHOW_DELAY.as_millis(), 400);
        assert_eq!(HIDE_DELAY.as_millis(), 100);
    }

    #[test]
    fn the_chord_joins_with_plus() {
        assert_eq!(
            TooltipView::new("Mute", &["Ctrl", "Shift", "M"]).chord(),
            "Ctrl+Shift+M"
        );
        assert_eq!(TooltipView::new("Mute", &[]).chord(), "");
    }
}

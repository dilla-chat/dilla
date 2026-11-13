//! `Segmented` (packages/ui/src/Segmented): a single choice among a few options, the ARIA radio group with selection
//! following focus. One tab stop, on the checked option (the first when none is checked); the arrows move and select,
//! wrapping, and Home and End go to the ends.

use std::rc::Rc;

use gpui::prelude::FluentBuilder;
use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    Styled, Window, div, px,
};

use crate::metrics::{OPACITY_MUTED, focus_ring};
use crate::roving::Roving;
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

type PickHandler = Rc<dyn Fn(&str, &mut Window, &mut App) + 'static>;

/// The group's width as the web paints it: its options are as wide as their mono labels (fractional pixels) and the web
/// paints the group between whole CSS pixels; with one edge on a whole pixel (left- or right-aligned) that is the natural
/// width rounded. `None` when a label falls back to another face (then the layout's own width stands).
pub fn painted_width(labels: &[&str], tall: bool) -> Option<f32> {
    let pad = if tall { 12.0 } else { 10.0 };
    let advance = f32::from(text::BTN) * crate::text::MONO_ADVANCE;
    if !labels
        .iter()
        .all(|l| l.chars().all(crate::text::mono_covers))
    {
        return None;
    }
    let options: f32 = labels
        .iter()
        .map(|l| (2.0 * pad + l.chars().count() as f32 * advance).max(24.0))
        .sum();
    Some(crate::text::painted_height(options + 2.0 * (2.0 + 1.0)))
}

/// The index of the option holding the tab stop: the checked one, else the first.
pub fn tab_stop(values: &[&str], value: &str) -> usize {
    values.iter().position(|v| *v == value).unwrap_or(0)
}

#[derive(IntoElement)]
pub struct Segmented {
    id: ElementId,
    options: Vec<(SharedString, SharedString)>,
    value: SharedString,
    disabled: bool,
    tall: bool,
    on_change: Option<PickHandler>,
}

impl Segmented {
    /// `options` are (value, label) pairs.
    pub fn new(
        id: impl Into<ElementId>,
        options: &[(&str, &str)],
        value: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            options: options
                .iter()
                .map(|(v, l)| {
                    (
                        SharedString::from((*v).to_owned()),
                        SharedString::from((*l).to_owned()),
                    )
                })
                .collect(),
            value: value.into(),
            disabled: false,
            tall: false,
            on_change: None,
        }
    }

    /// Shown but not changeable.
    pub fn disabled(mut self, disabled: bool) -> Self {
        self.disabled = disabled;
        self
    }

    /// The Voice page's input mode (`.d-voice-settings__mode .d-segmented__option`): 28px options, 12px across.
    pub fn tall(mut self, tall: bool) -> Self {
        self.tall = tall;
        self
    }

    pub fn on_change(mut self, handler: impl Fn(&str, &mut Window, &mut App) + 'static) -> Self {
        self.on_change = Some(Rc::new(handler));
        self
    }
}

impl RenderOnce for Segmented {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let count = self.options.len();
        // One focus handle per option, kept across frames, so an arrow can move focus to the next option.
        let handles = crate::roving::focus_handles((self.id.clone(), "focus"), count, window, cx);
        let values: Vec<&str> = self.options.iter().map(|(v, _)| v.as_ref()).collect();
        let stop = tab_stop(&values, &self.value);
        let (disabled, on_change) = (self.disabled, self.on_change);
        let group_values: Vec<SharedString> = self.options.iter().map(|(v, _)| v.clone()).collect();
        let group_handles = handles.clone();
        let labels: Vec<&str> = self.options.iter().map(|(_, l)| l.as_ref()).collect();
        let painted = painted_width(&labels, self.tall);
        let mut group = div()
            .id(self.id.clone())
            .flex()
            .flex_none()
            // At its painted width the options fit on one line by construction (a width rounded down leaves them at most
            // half a pixel over, which must not wrap); without one they wrap as the web's do.
            .when_some(painted, |g, w| g.w(px(w)).max_w_full())
            .when(painted.is_none(), |g| g.flex_wrap())
            .relative()
            .p(px(2.0))
            // Its options are as wide as their labels (fractional pixels): the group and the chosen option are painted
            // on the web's whole pixels (`snap::painted_box`).
            .border_1()
            .border_color(gpui::transparent_black())
            .rounded(radius::SEG)
            .child(crate::snap::painted_box(
                radius::SEG,
                color::SURFACE_2,
                Some(color::CONTROL_EDGE_FIELD),
            ));
        group = group.on_key_down(move |event, window, cx| {
            let Some(from) = group_handles.iter().position(|h| h.is_focused(window)) else {
                return;
            };
            let Some(next) = (Roving::Radio { both: true }).next(&event.keystroke, from, count)
            else {
                return;
            };
            cx.stop_propagation();
            if disabled {
                return;
            }
            group_handles[next].focus(window, cx);
            if next != from
                && let Some(on_change) = &on_change
            {
                on_change(&group_values[next], window, cx);
            }
        });
        for (i, (value, label)) in self.options.iter().enumerate() {
            let checked = *value == self.value;
            let fg = if checked {
                color::ACCENT_INK
            } else {
                color::FG_STRONG
            };
            let mut option = div()
                .id((self.id.clone(), format!("option-{i}")))
                // The handle's own flag decides the tab stop of a tracked element.
                .track_focus(&handles[i].clone().tab_stop(i == stop))
                .flex()
                .items_center()
                .min_w(px(24.0))
                .min_h(px(if self.tall { 28.0 } else { 26.0 }))
                .px(px(if self.tall { 12.0 } else { 10.0 }))
                .py(px(if self.tall { 0.0 } else { 4.0 }))
                .relative()
                .rounded(radius::SEG - px(2.0))
                .when(checked, |o| {
                    o.child(crate::snap::painted_box(
                        radius::SEG - px(2.0),
                        color::ACCENT,
                        None,
                    ))
                })
                .focus_visible(|s| s.shadow(focus_ring()))
                .child(line(
                    label.clone(),
                    text::BTN,
                    line_height(text::BTN, 1.5),
                    fg,
                ));
            option = if disabled {
                option.cursor_not_allowed().opacity(OPACITY_MUTED)
            } else if checked {
                option.cursor_pointer()
            } else {
                option
                    .cursor_pointer()
                    .hover(|s| s.bg(color::HOVER).text_color(color::FG))
            };
            group = group.child(option);
        }
        group
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_group_paints_whole_pixels() {
        // Automatic / Lightweight / Off: 224.72px of options and frame, painted 225 wide (the web's 758..983).
        assert_eq!(
            painted_width(&["Automatic", "Lightweight", "Off"], false),
            Some(225.0)
        );
        assert_eq!(
            painted_width(&["Voice activity", "Push to talk"], true),
            Some(233.0)
        );
    }

    #[test]
    fn the_checked_option_holds_the_tab_stop() {
        assert_eq!(tab_stop(&["a", "b", "c"], "b"), 1);
        assert_eq!(tab_stop(&["a", "b", "c"], "x"), 0);
    }
}

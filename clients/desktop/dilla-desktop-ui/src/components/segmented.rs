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

/// The index of the option holding the tab stop: the checked one, else the first.
pub fn tab_stop(values: &[&str], value: &str) -> usize {
    values.iter().position(|v| *v == value).unwrap_or(0)
}

/// An extra child of an option, by its value.
type OptionChild = Rc<dyn Fn(&str) -> Option<gpui::AnyElement>>;

#[derive(IntoElement)]
pub struct Segmented {
    id: ElementId,
    options: Vec<(SharedString, SharedString)>,
    value: SharedString,
    disabled: bool,
    tall: bool,
    on_change: Option<PickHandler>,
    /// An extra child of each option, by its value (the app's automation probe; the ui crate has none of its own).
    option_child: Option<OptionChild>,
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
            option_child: None,
        }
    }

    /// Adds `f(value)` to each option (absolutely placed over it), as the app's automation build names the options.
    pub fn option_child(mut self, f: impl Fn(&str) -> Option<gpui::AnyElement> + 'static) -> Self {
        self.option_child = Some(Rc::new(f));
        self
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
        let mut group = div()
            .id(self.id.clone())
            .flex()
            .flex_none()
            // The options wrap as the web's do when the row is too narrow (`flex-wrap: wrap`).
            .flex_wrap()
            .p(px(2.0))
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::CONTROL_EDGE_FIELD)
            .rounded(radius::SEG);
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
                .rounded(radius::SEG - px(2.0))
                .when(checked, |o| o.bg(color::ACCENT))
                .focus_visible(|s| s.shadow(focus_ring()))
                .relative()
                .child(line(
                    label.clone(),
                    text::BTN,
                    line_height(text::BTN, 1.5),
                    fg,
                ))
                .children(self.option_child.as_ref().and_then(|f| f(value)));
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
    fn the_checked_option_holds_the_tab_stop() {
        assert_eq!(tab_stop(&["a", "b", "c"], "b"), 1);
        assert_eq!(tab_stop(&["a", "b", "c"], "x"), 0);
    }
}

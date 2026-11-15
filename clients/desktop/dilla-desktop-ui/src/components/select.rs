//! `Select` (packages/ui/src/Select): a label over a 36px field (`--surface-2`, the `--control-edge-field` edge, radius
//! `--r-seg`) showing the chosen option, and a hint. Enter, Space or a click open the list; Up and Down move through it (and
//! open it from the field), Enter or Space choose, Escape closes. A press outside closes.

use std::rc::Rc;

use gpui::{
    App, ElementId, FocusHandle, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, deferred, div, px,
};

use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{OPACITY_MUTED, focus_ring, shadow_menu};
use crate::roving::menu_next;
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

type PickHandler = Rc<dyn Fn(&str, &mut Window, &mut App) + 'static>;

struct Memory {
    field: FocusHandle,
    open: bool,
    /// The highlighted option while the list is open.
    cursor: usize,
}

/// The index of `value` among the options, else the first.
pub fn selected_index(values: &[&str], value: &str) -> usize {
    values.iter().position(|v| *v == value).unwrap_or(0)
}

#[derive(IntoElement)]
pub struct Select {
    id: ElementId,
    label: SharedString,
    hint: Option<SharedString>,
    hint_width: Option<f32>,
    value: SharedString,
    options: Vec<(SharedString, SharedString)>,
    disabled: bool,
    show_label: bool,
    on_change: Option<PickHandler>,
}

impl Select {
    /// `options` are (value, label) pairs.
    pub fn new(
        id: impl Into<ElementId>,
        label: impl Into<SharedString>,
        options: &[(&str, &str)],
        value: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            hint: None,
            hint_width: None,
            value: value.into(),
            options: options
                .iter()
                .map(|(v, l)| {
                    (
                        SharedString::from((*v).to_owned()),
                        SharedString::from((*l).to_owned()),
                    )
                })
                .collect(),
            disabled: false,
            show_label: true,
            on_change: None,
        }
    }

    /// Draws the bare field: the host shows the label itself (the Voice page's `.d-voice-settings__label`, 8px over a native
    /// `<select>` named by it), so the field's own label and its 6px gap take no room.
    pub fn bare(mut self) -> Self {
        self.show_label = false;
        self
    }

    /// The measure the hint wraps at (the field's width, when the host knows it: a dialog's body); without one
    /// it stays on one line.
    pub fn hint_width(mut self, width: f32) -> Self {
        self.hint_width = Some(width);
        self
    }

    pub fn hint(mut self, hint: impl Into<SharedString>) -> Self {
        self.hint = Some(hint.into());
        self
    }

    pub fn disabled(mut self, disabled: bool) -> Self {
        self.disabled = disabled;
        self
    }

    pub fn on_change(mut self, handler: impl Fn(&str, &mut Window, &mut App) + 'static) -> Self {
        self.on_change = Some(Rc::new(handler));
        self
    }
}

impl RenderOnce for Select {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let memory = window.use_keyed_state((self.id.clone(), "select"), cx, |_, cx| Memory {
            field: cx.focus_handle().tab_stop(true),
            open: false,
            cursor: 0,
        });
        let (field_focus, open, cursor) = memory.read_with(cx, |m, _| {
            (m.field.clone(), m.open && !self.disabled, m.cursor)
        });
        let values: Vec<SharedString> = self.options.iter().map(|(v, _)| v.clone()).collect();
        let current = self
            .options
            .iter()
            .find(|(v, _)| *v == self.value)
            .or(self.options.first())
            .map(|(_, l)| l.clone())
            .unwrap_or_default();
        let selected = selected_index(
            &values.iter().map(AsRef::as_ref).collect::<Vec<_>>(),
            &self.value,
        );

        let set_open = {
            let memory = memory.clone();
            move |open: bool, cursor: usize, cx: &mut App| {
                memory.update(cx, |m, cx| {
                    m.open = open;
                    m.cursor = cursor;
                    cx.notify();
                });
            }
        };
        let (disabled, on_change) = (self.disabled, self.on_change);
        let pick = {
            let (values, on_change) = (values.clone(), on_change.clone());
            let set_open = set_open.clone();
            move |index: usize, window: &mut Window, cx: &mut App| {
                set_open(false, index, cx);
                if let (Some(h), Some(v)) = (&on_change, values.get(index)) {
                    h(v, window, cx);
                }
            }
        };

        let edge = if open {
            color::ACCENT
        } else {
            color::CONTROL_EDGE_FIELD
        };
        let mut field = div()
            .id((self.id.clone(), "field"))
            .track_focus(&field_focus)
            .tab_stop(true)
            .flex()
            .items_center()
            .justify_between()
            .gap(px(8.0))
            .w_full()
            .h(px(36.0))
            .px(px(10.0))
            .bg(color::SURFACE_2)
            .rounded(radius::SEG)
            .border_1()
            .border_color(edge)
            .hover(|s| s.border_color(color::FG_3))
            .focus_visible(|s| s.border_color(color::ACCENT))
            .child(line(current, text::SM, line_height(text::SM, 1.5), color::FG).ellipsis())
            // The web's field is a native <select>: Chromium draws its arrow in the text colour, 9 x 5.5px with its right
            // tip 5px in from the outer edge, past the 10px padding. A 16px chevron-down (whose path spans 6..18 of 24)
            // reaching 10px into that padding draws the same.
            .child(
                div().flex_none().mr(px(-10.0)).child(
                    Icon::new(IconName::ChevronDown)
                        .size(px(16.0))
                        .color(color::FG),
                ),
            );
        field = if disabled {
            field.opacity(OPACITY_MUTED).cursor_not_allowed()
        } else {
            let (click_set, key_set, key_pick) = (set_open.clone(), set_open.clone(), pick.clone());
            let count = values.len();
            field
                .cursor_pointer()
                .on_click(move |_, _, cx| click_set(!open, selected, cx))
                .on_key_down(move |event, window, cx| {
                    let key = event.keystroke.key.as_str();
                    if !open {
                        if matches!(key, "down" | "up") {
                            cx.stop_propagation();
                            key_set(true, selected, cx);
                        }
                        return;
                    }
                    match key {
                        "escape" => {
                            cx.stop_propagation();
                            key_set(false, selected, cx);
                        }
                        "tab" => key_set(false, selected, cx),
                        "enter" | "space" => {
                            cx.stop_propagation();
                            key_pick(cursor, window, cx);
                        }
                        _ => {
                            if let Some(next) = menu_next(key, Some(cursor), count) {
                                cx.stop_propagation();
                                key_set(true, next, cx);
                            }
                        }
                    }
                })
        };

        let mut wrap = div().relative().w_full().child(field);
        if open {
            let down_out = set_open.clone();
            let mut list = div()
                .id((self.id.clone(), "list"))
                .flex()
                .flex_col()
                .w_full()
                .p(px(4.0))
                .bg(color::SURFACE_2)
                .border_1()
                .border_color(color::LINE_STRONG)
                .rounded(radius::MENU)
                .shadow(shadow_menu())
                .occlude()
                .on_mouse_down_out(move |_, _, cx| down_out(false, selected, cx));
            for (i, (value, label)) in self.options.iter().enumerate() {
                let pick = pick.clone();
                let on = i == cursor;
                list = list.child(
                    div()
                        .id((self.id.clone(), format!("option-{value}")))
                        .flex()
                        .items_center()
                        .h(px(28.0))
                        .px(px(8.0))
                        .rounded(radius::ROW)
                        .cursor_pointer()
                        .when_bg(on)
                        .hover(|s| s.bg(color::SURFACE_HI))
                        .child(line(
                            label.clone(),
                            text::SM,
                            line_height(text::SM, 1.5),
                            if *value == self.value {
                                color::ACCENT
                            } else {
                                color::FG
                            },
                        ))
                        .on_click(move |_, window, cx| pick(i, window, cx)),
                );
            }
            wrap = wrap.child(
                deferred(
                    div()
                        .absolute()
                        .top_full()
                        .left_0()
                        .w_full()
                        .pt(px(2.0))
                        .child(list),
                )
                .with_priority(5),
            );
        }

        let mut root = div().id(self.id).flex().flex_col().gap(px(6.0)).min_w_0();
        if self.show_label {
            root = root.child(line(
                self.label,
                text::XS,
                line_height(text::XS, 1.5),
                color::FG_2,
            ));
        }
        root = root.child(wrap);
        if let Some(hint) = self.hint {
            root = root.child(super::text_field::hint_lines(hint, self.hint_width));
        }
        root
    }
}

/// `when_bg(true)` fills a highlighted option with `--surface-hi`.
trait WhenBg: Styled + Sized {
    fn when_bg(self, on: bool) -> Self {
        if on { self.bg(color::SURFACE_HI) } else { self }
    }
}
impl<T: Styled + Sized> WhenBg for T {}

#[allow(dead_code)]
fn _ring() -> Vec<gpui::BoxShadow> {
    focus_ring()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_chosen_option_is_found_else_the_first() {
        assert_eq!(selected_index(&["a", "b"], "b"), 1);
        assert_eq!(selected_index(&["a", "b"], "z"), 0);
    }
}

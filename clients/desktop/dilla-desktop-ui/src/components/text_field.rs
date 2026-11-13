//! `TextField` (packages/ui/src/TextField): a label over a 36px field (`--surface-2` in the `--control-edge-field` edge, radius
//! `--r-seg`), then a hint, a note and a refusal. The host owns the `InputState` (or `TextareaState`) and reads its value;
//! the field draws the frame around it. Hover raises the edge to `--fg-3`, focus to the accent, an error to the danger colour.

use gpui::prelude::FluentBuilder;
use gpui::{
    App, ElementId, Entity, Focusable, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, Styled, Window, div, px,
};
use gpui_component::input::{Input, InputState, Textarea, TextareaState};

use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::OPACITY_MUTED;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

/// The state of the control inside the frame.
pub enum Control {
    Single(Entity<InputState>),
    Multi(Entity<TextareaState>),
}

/// The edge a field draws (`TextField.css`): the danger colour on error, else the accent when focused, else `--fg-3` under
/// the pointer, else the field edge.
pub fn edge(error: bool, focused: bool, hovered: bool) -> gpui::Hsla {
    if error {
        color::DANGER
    } else if focused {
        color::ACCENT
    } else if hovered {
        color::FG_3
    } else {
        color::CONTROL_EDGE_FIELD
    }
}

#[derive(IntoElement)]
pub struct TextField {
    id: ElementId,
    label: SharedString,
    control: Control,
    hint: Option<SharedString>,
    note: Option<SharedString>,
    error: Option<SharedString>,
    disabled: bool,
    rows: usize,
}

impl TextField {
    pub fn new(
        id: impl Into<ElementId>,
        label: impl Into<SharedString>,
        input: &Entity<InputState>,
    ) -> Self {
        Self::with_control(id, label, Control::Single(input.clone()))
    }

    /// A multi-line field of `rows` lines (3 by default).
    pub fn multiline(
        id: impl Into<ElementId>,
        label: impl Into<SharedString>,
        input: &Entity<TextareaState>,
    ) -> Self {
        Self::with_control(id, label, Control::Multi(input.clone()))
    }

    fn with_control(
        id: impl Into<ElementId>,
        label: impl Into<SharedString>,
        control: Control,
    ) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            control,
            hint: None,
            note: None,
            error: None,
            disabled: false,
            rows: 3,
        }
    }

    pub fn hint(mut self, hint: impl Into<SharedString>) -> Self {
        self.hint = Some(hint.into());
        self
    }

    pub fn note(mut self, note: impl Into<SharedString>) -> Self {
        self.note = Some(note.into());
        self
    }

    /// A refusal, announced as an alert, with the cross glyph before it.
    pub fn error(mut self, error: impl Into<SharedString>) -> Self {
        self.error = Some(error.into());
        self
    }

    pub fn disabled(mut self, disabled: bool) -> Self {
        self.disabled = disabled;
        self
    }

    pub fn rows(mut self, rows: usize) -> Self {
        self.rows = rows.max(1);
        self
    }
}

impl RenderOnce for TextField {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let invalid = self.error.is_some();
        let focused = match &self.control {
            Control::Single(input) => input.focus_handle(cx).contains_focused(window, cx),
            Control::Multi(input) => input.focus_handle(cx).contains_focused(window, cx),
        };
        let (frame_h, frame_pad_y) = match self.control {
            Control::Single(_) => (Some(px(36.0)), 0.0),
            Control::Multi(_) => (None, 8.0),
        };
        let mut frame = div()
            .id((self.id.clone(), "frame"))
            .flex()
            .items_center()
            .w_full()
            .px(px(12.0))
            .py(px(frame_pad_y))
            .bg(color::SURFACE_2)
            .rounded(radius::SEG)
            .border_1()
            .border_color(edge(invalid, focused, false))
            .text_size(text::INPUT)
            .font_family(font::MONO)
            .text_color(color::FG)
            .when_some(frame_h, |d, h| d.h(h))
            .hover(move |s| s.border_color(edge(invalid, focused, true)));
        frame = match &self.control {
            // `.d-text-field__input` is 13px (`--text-input`) with the frame's own 12px sides: the editor's own size
            // (gpui-component's 14px `text_sm`) and its inner padding would set the text larger and further in.
            Control::Single(input) => frame.child(
                Input::new(input)
                    .appearance(false)
                    .text_size(text::INPUT)
                    .px_0()
                    .py_0(),
            ),
            Control::Multi(input) => frame
                .min_h(px(34.0 + 20.0 * (self.rows.saturating_sub(1)) as f32))
                .child(Textarea::new(input).appearance(false)),
        };
        if self.disabled {
            frame = frame.opacity(OPACITY_MUTED).cursor_not_allowed();
        }
        let mut root = div()
            .id(self.id)
            .flex()
            .flex_col()
            .gap(px(6.0))
            .min_w_0()
            .child(line(
                self.label,
                text::XS,
                line_height(text::XS, 1.5),
                color::FG_2,
            ))
            // Under a label of fractional height (16.5px after a 20.25px line) the frame paints on the web's pixel (`snap`).
            .child(crate::snap::snapped(frame));
        for note in [self.hint, self.note].into_iter().flatten() {
            root = root.child(line(
                note,
                text::SM,
                line_height(text::SM, 1.5),
                color::FG_3,
            ));
        }
        if let Some(error) = self.error {
            root = root.child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(4.0))
                    .child(Icon::new(IconName::X).size(px(12.0)).color(color::DANGER))
                    .child(line(
                        error,
                        text::SM,
                        line_height(text::SM, 1.5),
                        color::DANGER,
                    )),
            );
        }
        root
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_edge_follows_the_state() {
        assert_eq!(edge(false, false, false), color::CONTROL_EDGE_FIELD);
        assert_eq!(edge(false, false, true), color::FG_3);
        assert_eq!(edge(false, true, true), color::ACCENT);
        assert_eq!(edge(true, true, true), color::DANGER);
    }
}

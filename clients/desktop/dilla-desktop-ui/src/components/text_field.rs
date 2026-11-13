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
    hint_width: Option<f32>,
    note: Option<SharedString>,
    error: Option<SharedString>,
    disabled: bool,
    rows: usize,
    frame: Option<Frame>,
    overlay: Option<gpui::AnyElement>,
}

/// A host's own size for the input's frame (a form that restyles `.d-text-field__input`, as `.dw-profile-form` does).
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Frame {
    /// The frame's height, its 1px edge included.
    pub height: f32,
    /// The padding over and under a multi-line field's text.
    pub pad_y: f32,
    /// A multi-line field's line height (the `<textarea>`'s `line-height`).
    pub line: f32,
}

/// gpui-component's textarea pads its text by 10px at the sides and 8px over and under it, and wraps 10px short of its
/// text box (`input/base/element.rs` `RIGHT_MARGIN`): a field that sets the text box itself shifts the textarea back by
/// its padding and widens it by the padding and the margin, so its text sits and wraps where the CSS field's does.
const TEXTAREA_PAD_X: f32 = 10.0;
const TEXTAREA_PAD_Y: f32 = 8.0;
const TEXTAREA_RIGHT_MARGIN: f32 = 10.0;
/// `.d-text-field__input`'s side padding (`0 0.75rem`).
const FIELD_PAD_X: f32 = 12.0;

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
            hint_width: None,
            note: None,
            error: None,
            disabled: false,
            rows: 3,
            frame: None,
            overlay: None,
        }
    }

    /// The host's own frame size (see [`Frame`]).
    pub fn frame(mut self, frame: Frame) -> Self {
        self.frame = Some(frame);
        self
    }

    /// An element laid over a framed multi-line field's text box (the automation build's typed-text probe).
    pub fn text_overlay(mut self, overlay: impl IntoElement) -> Self {
        self.overlay = Some(overlay.into_any_element());
        self
    }

    /// The measure the hint and note wrap at (the field's width, when the host knows it: a dialog's body); without one
    /// they stay on one line.
    pub fn hint_width(mut self, width: f32) -> Self {
        self.hint_width = Some(width);
        self
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
        let (frame_h, frame_pad_y) = match (&self.control, self.frame) {
            (Control::Single(_), Some(f)) => (Some(px(f.height)), 0.0),
            (Control::Multi(_), Some(f)) => (Some(px(f.height)), f.pad_y),
            (Control::Single(_), None) => (Some(px(36.0)), 0.0),
            (Control::Multi(_), None) => (None, 8.0),
        };
        let mut frame = div()
            .id((self.id.clone(), "frame"))
            .flex()
            .items_center()
            .w_full()
            .px(px(FIELD_PAD_X))
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
            Control::Multi(input) => match self.frame {
                None => frame
                    .min_h(px(34.0 + 20.0 * (self.rows.saturating_sub(1)) as f32))
                    .child(Textarea::new(input).appearance(false)),
                // A fixed frame holds its text from the top, as a `<textarea>` does: the text box is the frame's content
                // box, and the textarea is shifted back by its own padding so its lines start at the box's corner.
                Some(f) => frame.items_start().child(
                    div()
                        .relative()
                        .flex_1()
                        .h_full()
                        .children(self.overlay)
                        // The `<textarea>` clips its text at its padding box: a line that does not fit shows through
                        // the bottom padding, up to the edge.
                        .child(
                            div()
                                .absolute()
                                .left(px(-FIELD_PAD_X))
                                .right(px(-FIELD_PAD_X))
                                .top(px(-f.pad_y))
                                .bottom(px(-f.pad_y))
                                .overflow_hidden()
                                .child(
                                    div()
                                        .absolute()
                                        .left(px(FIELD_PAD_X - TEXTAREA_PAD_X))
                                        .right(px(FIELD_PAD_X
                                            - TEXTAREA_PAD_X
                                            - TEXTAREA_RIGHT_MARGIN))
                                        .top(px(f.pad_y - TEXTAREA_PAD_Y))
                                        .bottom(px(-f.line))
                                        .child(
                                            Textarea::new(input)
                                                .appearance(false)
                                                .text_size(text::INPUT)
                                                .line_height(px(f.line))
                                                .size_full(),
                                        ),
                                ),
                        ),
                ),
            },
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
            .child(frame);
        for note in [self.hint, self.note].into_iter().flatten() {
            root = root.child(hint_lines(note, self.hint_width));
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

/// A field's hint (12.5px `--fg-3`): wrapped at `width` when given, else one line.
pub(crate) fn hint_lines(hint: SharedString, width: Option<f32>) -> gpui::AnyElement {
    let lh = line_height(text::SM, 1.5);
    match width {
        Some(w) => div()
            .flex()
            .flex_col()
            .children(crate::text::paragraph(&hint, text::SM, lh, color::FG_3, w))
            .into_any_element(),
        None => line(hint, text::SM, lh, color::FG_3).into_any_element(),
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

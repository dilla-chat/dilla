//! `TextField` (packages/ui/src/TextField): a label over a 36px field (`--surface-2` in the `--control-edge-field` edge, radius
//! `--r-seg`), then a hint, a note and a refusal. The host owns the `InputState` (or `TextareaState`) and reads its value;
//! the field draws the frame around it. Hover raises the edge to `--fg-3`, focus to the accent, an error to the danger colour.

use std::cell::Cell;
use std::rc::Rc;

use gpui::prelude::FluentBuilder;
use gpui::{
    App, Bounds, ElementId, Entity, Focusable, InteractiveElement, IntoElement, MouseButton,
    MouseDownEvent, MouseMoveEvent, MouseUpEvent, ParentElement, PathBuilder, Pixels, RenderOnce,
    SharedString, Styled, Window, canvas, div, point, px,
};
use gpui_component::input::{Input, InputState, Textarea, TextareaState};

use crate::a11y::{Accessible, Live, Role};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::OPACITY_MUTED;
use crate::text::line;
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
    resizable: bool,
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

/// `textarea.d-text-field__input { min-height: 2.125rem }`: the least a drag leaves a resizable field, edges included.
pub const MIN_HEIGHT: f32 = 34.0;

/// A resizable field's height after the grip is dragged `drag` px down (up is negative) from `start`: the grip follows
/// the pointer, and the field stops at [`MIN_HEIGHT`].
pub fn resized_height(start: f32, drag: f32) -> f32 {
    (start + drag).max(MIN_HEIGHT)
}

/// The grip's box: Chromium's `resize: vertical` corner, 15px square at the border box's bottom-right.
const GRIP_SIZE: f32 = 15.0;
/// The grip's two strokes in px from the border box's bottom-right corner (measured on the web reference frame: 1px wide,
/// at 45 degrees): each runs from its start, 2.5px in from the right edge, to its end, 3.5px up from the bottom.
const GRIP_STROKES: [([f32; 2], [f32; 2]); 2] =
    [([-2.5, -9.5], [-8.5, -3.5]), ([-2.5, -5.5], [-4.5, -3.5])];
/// The strokes' width: Chromium draws them aliased, three device pixels across a diagonal; GPUI's anti-aliased path
/// covers as much at 1.3px.
const GRIP_STROKE_WIDTH: f32 = 1.3;
/// Chromium's resizer on a dark colour scheme.
const GRIP_COLOR: u32 = 0x9fa0a1;

/// What a resizable field remembers between frames: the height a drag has left it at, and the drag in progress as the
/// pointer's starting y with the height then.
struct Resize {
    height: Rc<Cell<Option<f32>>>,
    drag: Rc<Cell<Option<(f32, f32)>>>,
}

/// The grip: the corner's hit box (a row-resize cursor, a press starts the drag) and the two strokes painted over it. The
/// window follows the pointer for the drag, wherever it goes, and the release ends it.
fn resize_grip(
    id: ElementId,
    state: Entity<Resize>,
    height: Rc<Cell<Option<f32>>>,
    drag: Rc<Cell<Option<(f32, f32)>>>,
    shown: f32,
) -> impl IntoElement {
    let (down_drag, down_state) = (drag.clone(), state.clone());
    div()
        .id(id)
        .absolute()
        .right(px(-1.0))
        .bottom(px(-1.0))
        .size(px(GRIP_SIZE))
        .cursor_row_resize()
        .on_mouse_down(
            MouseButton::Left,
            move |event: &MouseDownEvent, window, cx| {
                cx.stop_propagation();
                window.prevent_default();
                down_drag.set(Some((f32::from(event.position.y), shown)));
                down_state.update(cx, |_, cx| cx.notify());
            },
        )
        .child(
            canvas(
                |_, _, _| (),
                move |bounds: Bounds<Pixels>, (), window, _| {
                    let corner = bounds.bottom_right();
                    for ([x0, y0], [x1, y1]) in GRIP_STROKES {
                        let mut path = PathBuilder::stroke(px(GRIP_STROKE_WIDTH));
                        path.move_to(corner + point(px(x0), px(y0)));
                        path.line_to(corner + point(px(x1), px(y1)));
                        if let Ok(path) = path.build() {
                            window.paint_path(path, gpui::rgb(GRIP_COLOR));
                        }
                    }
                    let (move_drag, move_height, move_state) =
                        (drag.clone(), height.clone(), state.clone());
                    window.on_mouse_event(move |event: &MouseMoveEvent, _, _, cx| {
                        if let (Some((from, start)), true) = (move_drag.get(), event.dragging()) {
                            let moved = f32::from(event.position.y) - from;
                            move_height.set(Some(resized_height(start, moved)));
                            move_state.update(cx, |_, cx| cx.notify());
                        }
                    });
                    let up_drag = drag.clone();
                    window.on_mouse_event(move |_: &MouseUpEvent, _, _, _| {
                        up_drag.set(None);
                    });
                },
            )
            .size_full(),
        )
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
            hint_width: None,
            note: None,
            error: None,
            disabled: false,
            rows: 3,
            frame: None,
            overlay: None,
            resizable: false,
        }
    }

    /// The host's own frame size (see [`Frame`]).
    pub fn frame(mut self, frame: Frame) -> Self {
        self.frame = Some(frame);
        self
    }

    /// An element laid over the field's text box (the automation build's typed-text probe): a framed multi-line field's
    /// text box, or a single-line field's editor.
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

    /// `resize: vertical` (`textarea.d-text-field__input`, Decision D16): the framed multi-line field draws the browser's
    /// grip in its bottom-right corner and its height follows a drag of the grip, down to [`MIN_HEIGHT`].
    pub fn resizable(mut self) -> Self {
        self.resizable = true;
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
        // A resizable field's height once its grip has been dragged; the host's frame height until then.
        let resize = (self.resizable
            && matches!(self.control, Control::Multi(_))
            && self.frame.is_some())
        .then(|| {
            let state = window.use_keyed_state((self.id.clone(), "resize"), cx, |_, _| Resize {
                height: Rc::new(Cell::new(None)),
                drag: Rc::new(Cell::new(None)),
            });
            let (height, drag) = state.read_with(cx, |r, _| (r.height.clone(), r.drag.clone()));
            (state, height, drag)
        });
        let (frame_h, frame_pad_y) = match (&self.control, self.frame) {
            (Control::Single(_), Some(f)) => (Some(px(f.height)), 0.0),
            (Control::Multi(_), Some(f)) => (
                Some(px(resize
                    .as_ref()
                    .and_then(|(_, height, _)| height.get())
                    .unwrap_or(f.height))),
                f.pad_y,
            ),
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
            Control::Single(input) => {
                let field = Input::new(input)
                    .aria_label(self.label.clone())
                    .appearance(false)
                    .text_size(text::INPUT)
                    .px_0()
                    .py_0();
                match self.overlay {
                    None => frame.child(field),
                    // The overlay lies over the editor's own box (its one line).
                    Some(overlay) => frame.child(
                        div()
                            .relative()
                            .flex_1()
                            .min_w_0()
                            .child(field)
                            .child(div().absolute().inset_0().child(overlay)),
                    ),
                }
            }
            Control::Multi(input) => match self.frame {
                None => frame
                    .min_h(px(34.0 + 20.0 * (self.rows.saturating_sub(1)) as f32))
                    .child(
                        Textarea::new(input)
                            .aria_label(self.label.clone())
                            .appearance(false),
                    ),
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
                                                .aria_label(self.label.clone())
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
        if let (Some((state, height, drag)), Some(f)) = (resize, self.frame) {
            let shown = height.get().unwrap_or(f.height);
            frame = frame.relative().child(resize_grip(
                (self.id.clone(), "grip").into(),
                state,
                height,
                drag,
                shown,
            ));
        }
        if self.disabled {
            frame = frame.opacity(OPACITY_MUTED).cursor_not_allowed();
        }
        let error_id: ElementId = (self.id.clone(), "error").into();
        let mut root = div()
            .id(self.id)
            .flex()
            .flex_col()
            .gap(px(6.0))
            .min_w_0()
            .child(line(
                self.label,
                text::XS,
                crate::text::root_line_height(text::XS),
                color::FG_2,
            ))
            .child(frame);
        for note in [self.hint, self.note].into_iter().flatten() {
            root = root.child(hint_lines(note, self.hint_width));
        }
        if let Some(error) = self.error {
            // `role="alert"`: the field's refusal is read out as it appears.
            root = root.child(
                div()
                    .id(error_id)
                    .a11y(Role::Alert, error.clone())
                    .live(Live::Assertive)
                    .flex()
                    .items_center()
                    .gap(px(4.0))
                    .child(Icon::new(IconName::X).size(px(12.0)).color(color::DANGER))
                    .child(line(
                        error,
                        text::SM,
                        crate::text::root_line_height(text::SM),
                        color::DANGER,
                    )),
            );
        }
        root
    }
}

/// A field's hint (12.5px `--fg-3`): wrapped at `width` when given, else one line.
pub(crate) fn hint_lines(hint: SharedString, width: Option<f32>) -> gpui::AnyElement {
    let lh = crate::text::root_line_height(text::SM);
    // The hint is the field's `<p>`: a node a reader's cursor reads.
    let painted = match width {
        Some(w) => div()
            .flex()
            .flex_col()
            .children(crate::text::paragraph(&hint, text::SM, lh, color::FG_3, w))
            .into_any_element(),
        None => line(hint.clone(), text::SM, lh, color::FG_3).into_any_element(),
    };
    crate::a11y::paragraph(hint, painted).into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// D16: a drag of the grip by +40px grows the field by 40px; a drag of -500px stops at the 34px minimum.
    #[test]
    fn a_resizable_textarea_drags_vertically() {
        assert_eq!(resized_height(80.5, 40.0), 120.5);
        assert_eq!(resized_height(80.5, -500.0), 34.0);
        assert_eq!(resized_height(80.5, 0.0), 80.5);
    }

    #[test]
    fn the_edge_follows_the_state() {
        assert_eq!(edge(false, false, false), color::CONTROL_EDGE_FIELD);
        assert_eq!(edge(false, false, true), color::FG_3);
        assert_eq!(edge(false, true, true), color::ACCENT);
        assert_eq!(edge(true, true, true), color::DANGER);
    }
}

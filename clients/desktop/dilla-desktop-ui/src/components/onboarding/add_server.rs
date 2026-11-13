//! `AddServerDialog`'s choices (packages/ui/src/AddServerDialog): two full-width rows in the dialog's body, 8px apart,
//! each a 44px mono row on `--surface-2` in the `--hairline-2` edge, radius `--r-md`, 12px in, `--text-base` in `--fg`;
//! under the pointer `--surface-hi` in the accent edge. The host puts them in a `Dialog` and wires each choice: the
//! dialog closes itself, then opens the join or create flow.

use gpui::{
    AnyElement, App, ClickEvent, ElementId, FocusHandle, InteractiveElement, IntoElement,
    ParentElement, RenderOnce, SharedString, Styled, Window, div, px,
};

use super::frame::{Click, pressable};
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

/// One choice row.
#[derive(IntoElement)]
pub struct AddServerChoice {
    id: ElementId,
    label: SharedString,
    focus: Option<FocusHandle>,
    on_click: Option<Click>,
}

impl AddServerChoice {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            focus: None,
            on_click: None,
        }
    }

    /// Tracks `handle` as its focus (the dialog's first choice takes focus when it opens).
    pub fn focus(mut self, handle: &FocusHandle) -> Self {
        self.focus = Some(handle.clone());
        self
    }

    pub fn on_click(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_click = Some(Box::new(f));
        self
    }
}

impl RenderOnce for AddServerChoice {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        let base = div()
            .id(self.id)
            .w_full()
            .min_h(px(44.0))
            .px(px(12.0))
            .flex()
            .items_center()
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::HAIRLINE_2)
            .rounded(radius::MD)
            .hover(|s| s.bg(color::SURFACE_HI).border_color(color::ACCENT))
            .child(line(
                self.label,
                text::BASE,
                line_height(text::BASE, 1.5),
                color::FG,
            ));
        pressable(base, self.focus.as_ref(), self.on_click)
    }
}

/// The list of the two choices (already wrapped by the host for automation).
#[derive(IntoElement)]
pub struct AddServerChoices {
    rows: Vec<AnyElement>,
}

impl AddServerChoices {
    pub fn new(join: impl IntoElement, create: impl IntoElement) -> Self {
        Self {
            rows: vec![join.into_any_element(), create.into_any_element()],
        }
    }
}

impl RenderOnce for AddServerChoices {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        div()
            .w_full()
            .flex()
            .flex_col()
            .gap(px(8.0))
            .children(self.rows)
    }
}

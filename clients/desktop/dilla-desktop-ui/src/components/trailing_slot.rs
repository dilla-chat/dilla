//! `TrailingSlot` (packages/ui/src/TrailingSlot): the trailing column of a list row, `--slot-w` (32px) wide, never
//! shrinking, its content centred; empty it still holds its width, so the right edges of a list's badges line up.

use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    Styled, Window, div,
};

use crate::tokens::layout;

#[derive(IntoElement)]
pub struct TrailingSlot {
    id: ElementId,
    child: Option<AnyElement>,
    auto_width: bool,
}

impl TrailingSlot {
    pub fn new(id: impl Into<ElementId>) -> Self {
        Self {
            id: id.into(),
            child: None,
            auto_width: false,
        }
    }

    pub fn child(mut self, child: impl IntoElement) -> Self {
        self.child = Some(child.into_any_element());
        self
    }

    /// In a channel row the slot grows with its tag but keeps `--slot-w` as its minimum, content to the right
    /// (`.d-chrow > .d-slot`).
    pub fn in_row(mut self) -> Self {
        self.auto_width = true;
        self
    }
}

impl RenderOnce for TrailingSlot {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let slot = div()
            .id(self.id)
            .flex()
            .flex_none()
            .items_center()
            .justify_center();
        let slot = if self.auto_width {
            slot.min_w(layout::SLOT).justify_end()
        } else {
            slot.w(layout::SLOT)
        };
        slot.children(self.child)
    }
}

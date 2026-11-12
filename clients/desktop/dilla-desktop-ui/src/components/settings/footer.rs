//! The settings `footer` (packages/ui/src/SettingsFrame/SettingsFooter): a status line (`--fg-3`, 12.5px) at the left and
//! the actions at the right, wrapping on a narrow pane. The status is a polite live region on the web; here the host
//! announces it through the screen's accessibility tree.

use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, Styled, Window, div, px,
};

use crate::text::{line, line_height};
use crate::tokens::{color, text};

#[derive(IntoElement)]
pub struct SettingsFooter {
    id: ElementId,
    status: Option<SharedString>,
    actions: Vec<AnyElement>,
}

impl SettingsFooter {
    pub fn new(id: impl Into<ElementId>) -> Self {
        Self {
            id: id.into(),
            status: None,
            actions: Vec::new(),
        }
    }

    pub fn status(mut self, status: impl Into<SharedString>) -> Self {
        self.status = Some(status.into());
        self
    }

    /// One action (a button); the actions sit at the right in the order given.
    pub fn action(mut self, action: impl IntoElement) -> Self {
        self.actions.push(action.into_any_element());
        self
    }
}

impl RenderOnce for SettingsFooter {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        div()
            .id(self.id)
            .flex()
            .flex_wrap()
            .items_center()
            .justify_between()
            .gap_x(px(16.0))
            .gap_y(px(8.0))
            .w_full()
            .child(
                div().flex_1().min_w(px(192.0)).children(self.status.map(|status| {
                    line(status, text::SM, line_height(text::SM, 1.5), color::FG_3)
                })),
            )
            .child(
                div()
                    .flex()
                    .flex_wrap()
                    .items_center()
                    .justify_end()
                    .gap(px(8.0))
                    .ml_auto()
                    .children(self.actions),
            )
    }
}

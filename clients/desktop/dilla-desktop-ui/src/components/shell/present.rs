//! The message log's "viewing older messages" bar (MessageLog.tsx `present`, MessageLog.css `.d-message-log__present`):
//! while the log is away from its newest messages, a bar across its foot says so and offers "jump to present" (a ghost
//! button). Its parent is the log's relative box; the bar sits over the log's bottom edge.

use gpui::{
    App, ElementId, IntoElement, ParentElement, RenderOnce, SharedString, Styled, Window, div, px,
};

use crate::components::button::{Button, ButtonVariant, ClickHandler};
use crate::metrics::Density;
use crate::text::{line, line_height};
use crate::tokens::{color, text};

/// `gap: 0.5rem; padding: 0.25rem var(--row-pad-x)`.
const GAP: f32 = 8.0;
const PAD_Y: f32 = 4.0;

#[derive(IntoElement)]
pub struct PresentBar {
    id: ElementId,
    older: SharedString,
    jump: SharedString,
    on_jump: Option<ClickHandler>,
}

impl PresentBar {
    pub fn new(
        id: impl Into<ElementId>,
        older: impl Into<SharedString>,
        jump: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            older: older.into(),
            jump: jump.into(),
            on_jump: None,
        }
    }

    pub fn on_jump(
        mut self,
        handler: impl Fn(&gpui::ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_jump = Some(Box::new(handler));
        self
    }
}

impl RenderOnce for PresentBar {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let density = Density::of(window, cx);
        let mut jump = Button::new(self.id, self.jump).variant(ButtonVariant::Ghost);
        if let Some(on_jump) = self.on_jump {
            jump = jump.on_click(on_jump);
        }
        div()
            .absolute()
            .left_0()
            .right_0()
            .bottom_0()
            .flex()
            .items_center()
            .justify_center()
            .gap(px(GAP))
            .py(px(PAD_Y))
            .px(density.row_pad_x())
            .bg(color::SURFACE)
            .border_t_1()
            .border_color(color::EDGE)
            // No line height of its own: the root's `--line-height`.
            .child(line(
                self.older,
                text::SM,
                line_height(text::SM, density.line_height()),
                color::FG_2,
            ))
            .child(jump)
    }
}

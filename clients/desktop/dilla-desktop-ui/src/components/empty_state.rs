//! `EmptyState` (packages/ui/src/EmptyState): a pane with nothing to show yet: a title, a line of body (at most 40ch), and
//! one or two ways forward, centred.

use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, Styled, Window, div, px,
};

use super::button::{Button, ButtonVariant, ClickHandler};
use crate::text::{line, line_height, paragraph};
use crate::tokens::{color, font, text};

/// `max-width: 40ch` at the body's 13.5px mono advance.
fn body_width() -> f32 {
    40.0 * f32::from(text::BASE) * crate::text::MONO_ADVANCE
}

struct Action {
    label: SharedString,
    on_click: ClickHandler,
}

#[derive(IntoElement)]
pub struct EmptyState {
    id: ElementId,
    title: SharedString,
    body: SharedString,
    action: Option<Action>,
    secondary: Option<Action>,
    /// Wraps each way forward, by its part (`"action"`, `"secondary"`): the app's automation probes.
    wrap: Option<Box<dyn Fn(&'static str, AnyElement) -> AnyElement>>,
}

impl EmptyState {
    pub fn new(
        id: impl Into<ElementId>,
        title: impl Into<SharedString>,
        body: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            title: title.into(),
            body: body.into(),
            action: None,
            secondary: None,
            wrap: None,
        }
    }

    /// The primary way forward (an accent button).
    pub fn action(
        mut self,
        label: impl Into<SharedString>,
        on_click: impl Fn(&gpui::ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.action = Some(Action {
            label: label.into(),
            on_click: Box::new(on_click),
        });
        self
    }

    /// A second way forward after the primary one (a plain button).
    pub fn secondary(
        mut self,
        label: impl Into<SharedString>,
        on_click: impl Fn(&gpui::ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.secondary = Some(Action {
            label: label.into(),
            on_click: Box::new(on_click),
        });
        self
    }
}

impl EmptyState {
    /// Wraps each button (`"action"`, `"secondary"`) in what the caller adds around it (an automation probe); the
    /// layout is the button's own.
    pub fn wrap_actions(
        mut self,
        wrap: impl Fn(&'static str, AnyElement) -> AnyElement + 'static,
    ) -> Self {
        self.wrap = Some(Box::new(wrap));
        self
    }
}

impl RenderOnce for EmptyState {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let id = self.id.clone();
        let mut root = div()
            .id(self.id)
            .flex()
            .flex_col()
            .items_center()
            .justify_center()
            .gap(px(8.0))
            .px(px(24.0))
            .py(px(48.0))
            .child(
                line(self.title, text::LG, line_height(text::LG, 1.5), color::FG)
                    .weight(font::WEIGHT_DISPLAY),
            )
            .child(
                div()
                    .flex()
                    .flex_col()
                    .items_center()
                    .mb(px(8.0))
                    .children(paragraph(
                        &self.body,
                        text::BASE,
                        line_height(text::BASE, 1.5),
                        color::FG_2,
                        body_width(),
                    )),
            );
        if self.action.is_some() || self.secondary.is_some() {
            let wrap = self.wrap;
            let wrapped = move |part: &'static str, el: AnyElement| match &wrap {
                Some(w) => w(part, el),
                None => el,
            };
            let mut actions = div().flex().flex_wrap().justify_center().gap(px(8.0));
            if let Some(a) = self.action {
                actions = actions.child(wrapped(
                    "action",
                    Button::new((id.clone(), "action"), a.label)
                        .variant(ButtonVariant::Accent)
                        .on_click(a.on_click)
                        .into_any_element(),
                ));
            }
            if let Some(a) = self.secondary {
                actions = actions.child(wrapped(
                    "secondary",
                    Button::new((id, "secondary"), a.label)
                        .on_click(a.on_click)
                        .into_any_element(),
                ));
            }
            root = root.child(actions);
        }
        root
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_body_measure_is_forty_characters() {
        assert!((body_width() - 324.0).abs() < 0.01);
    }
}

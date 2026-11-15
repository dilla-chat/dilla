//! `KeyHint` (packages/ui/src/KeyHint): keycaps (22px, a 2px bottom edge) and an optional uppercase label.

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    Styled, Window, div, px,
};

use crate::metrics::LABEL_TRACKING;
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

#[derive(IntoElement)]
pub struct KeyHint {
    id: ElementId,
    keys: Vec<SharedString>,
    label: Option<SharedString>,
}

impl KeyHint {
    pub fn new(id: impl Into<ElementId>, keys: &[&str]) -> Self {
        Self {
            id: id.into(),
            keys: keys
                .iter()
                .map(|k| SharedString::from((*k).to_owned()))
                .collect(),
            label: None,
        }
    }

    pub fn label(mut self, label: impl Into<SharedString>) -> Self {
        self.label = Some(label.into());
        self
    }
}

impl RenderOnce for KeyHint {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let mut root = div()
            .id(self.id)
            .flex()
            .flex_none()
            .items_center()
            .gap(px(4.0));
        for key in self.keys {
            root = root.child(
                div()
                    .flex()
                    .items_center()
                    .justify_center()
                    .min_w(px(22.0))
                    .h(px(22.0))
                    .px(px(6.0))
                    .border_1()
                    .border_b_2()
                    .border_color(color::CONTROL_EDGE_KEY)
                    .rounded(radius::SM)
                    .child(line(key, text::XS, line_height(text::XS, 1.5), color::FG)),
            );
        }
        if let Some(label) = self.label {
            root = root.child(
                line(
                    label.to_uppercase(),
                    text::MICRO,
                    line_height(text::MICRO, 1.5),
                    color::FG_3,
                )
                .tracking(LABEL_TRACKING),
            );
        }
        root
    }
}

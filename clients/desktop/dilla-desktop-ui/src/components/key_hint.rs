//! `KeyHint` (packages/ui/src/KeyHint): keycaps (22px, a 2px bottom edge) and an optional uppercase label.

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    Styled, Window, div, px,
};

use crate::metrics::LABEL_TRACKING;
use crate::text::line;
use crate::tokens::{color, radius, text};

/// `.d-keyhint__kbd`'s `min-width: 1.375rem` and `height: 1.375rem` (22px) and its `padding: 0 6px`.
const KEYCAP_SIZE: f32 = 22.0;
const KEYCAP_PAD_X: f32 = 6.0;

/// The keycap's letter-spacing in em of its own 11px text. `.d-keyhint` sets `letter-spacing: var(--label-tracking)` on
/// the 10px root and the keycap inherits the computed 0.8px, not the em: each cap is as wide as its letters plus 0.8px
/// a character (`Ctrl` is 29.6px wide, not 26.4).
fn keycap_tracking_em() -> f32 {
    LABEL_TRACKING * f32::from(text::MICRO) / f32::from(text::XS)
}

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
                    .min_w(px(KEYCAP_SIZE))
                    .h(px(KEYCAP_SIZE))
                    .px(px(KEYCAP_PAD_X))
                    .border_1()
                    .border_b_2()
                    .border_color(color::CONTROL_EDGE_KEY)
                    .rounded(radius::SM)
                    .child(
                        line(
                            key,
                            text::XS,
                            crate::text::root_line_height(text::XS),
                            color::FG,
                        )
                        .tracking(keycap_tracking_em()),
                    ),
            );
        }
        if let Some(label) = self.label {
            root = root.child(
                line(
                    label.to_uppercase(),
                    text::MICRO,
                    crate::text::root_line_height(text::MICRO),
                    color::FG_3,
                )
                .tracking(LABEL_TRACKING),
            );
        }
        root
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The keycap's box is `.d-keyhint__kbd`'s: a 22px minimum with 6px of padding, and the 0.8px letter-spacing the cap
    /// inherits from the hint (the layout.json of the keyboard settings shot: `Ctrl` 29.609 + 2 x 7 = 43.609 wide).
    #[test]
    fn keycap_min_width() {
        assert_eq!(KEYCAP_SIZE, 22.0);
        assert_eq!(KEYCAP_PAD_X, 6.0);
        let spacing = keycap_tracking_em() * f32::from(text::XS);
        assert!((spacing - 0.8).abs() < 1e-4, "{spacing}");
    }
}

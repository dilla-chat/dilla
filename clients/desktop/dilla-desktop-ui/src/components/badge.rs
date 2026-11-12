//! `Badge` (packages/ui/src/Pill): the unread and mention count pill.

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    Styled, Window, div, px,
};

use crate::tokens::{color, font, radius, text};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BadgeKind {
    Unread,
    Mention,
}

/// A count as the pill draws it: capped at `99+`.
pub fn capped(count: u32) -> String {
    if count > 99 {
        "99+".to_owned()
    } else {
        count.to_string()
    }
}

#[derive(IntoElement)]
pub struct Badge {
    id: ElementId,
    kind: BadgeKind,
    count: u32,
    text: Option<SharedString>,
    in_row: bool,
}

impl Badge {
    pub fn new(id: impl Into<ElementId>, kind: BadgeKind, count: u32) -> Self {
        Self {
            id: id.into(),
            kind,
            count,
            text: None,
            in_row: false,
        }
    }

    /// The visible text in place of the count (a row's `@2`).
    pub fn text(mut self, text: impl Into<SharedString>) -> Self {
        self.text = Some(text.into());
        self
    }

    /// The pill inside a list row (`ChannelRow.css`): radius 4px, 5px padding, 10px text, no ring.
    pub fn in_row(mut self) -> Self {
        self.in_row = true;
        self
    }
}

impl RenderOnce for Badge {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let root = div().id(self.id);
        if self.count == 0 {
            return root.size_0();
        }
        let (bg, fg) = match self.kind {
            BadgeKind::Unread => (color::ACCENT, color::ACCENT_INK),
            BadgeKind::Mention => (color::MENTION, color::MENTION_INK),
        };
        let label = self.text.unwrap_or_else(|| capped(self.count).into());
        let pill = root
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .h(px(16.0))
            .bg(bg)
            .text_color(fg)
            .font_family(font::MONO)
            .font_weight(font::WEIGHT_DISPLAY)
            .child(label);
        if self.in_row {
            pill.px(px(5.0)).text_size(text::MICRO).rounded(radius::SM)
        } else {
            pill.min_w(px(16.0))
                .px(px(4.0))
                .text_size(text::CHIP)
                .rounded(radius::PILL)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn counts_cap_at_99() {
        assert_eq!(capped(3), "3");
        assert_eq!(capped(99), "99");
        assert_eq!(capped(100), "99+");
    }
}

//! `Badge` (packages/ui/src/Pill): the unread and mention count pill.

use gpui::{
    App, BoxShadow, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, Styled, Window, div, point, px,
};

use crate::text::line;
use crate::tokens::{color, font, radius, text};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BadgeKind {
    Unread,
    Mention,
}

/// `box-shadow: 0 0 0 2.5px var(--bg-2)`: the mention pill's ring, which sets it off from what it sits on.
pub fn mention_ring() -> Vec<BoxShadow> {
    vec![BoxShadow {
        color: color::BG_2,
        offset: point(px(0.0), px(0.0)),
        blur_radius: px(0.0),
        spread_radius: px(2.5),
        inset: false,
    }]
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
            .bg(bg);
        let size = if self.in_row { text::MICRO } else { text::CHIP };
        let pill = pill.child(
            line(label, size, crate::text::line_height(size, 1.5), fg).weight(font::WEIGHT_DISPLAY),
        );
        if self.in_row {
            pill.px(px(5.0)).rounded(radius::SM)
        } else {
            let pill = pill.min_w(px(16.0)).px(px(4.0)).rounded(radius::PILL);
            if self.kind == BadgeKind::Mention {
                pill.shadow(mention_ring())
            } else {
                pill
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_mention_pill_has_a_2_5px_ring_in_bg_2() {
        let ring = mention_ring();
        assert_eq!(ring.len(), 1);
        assert_eq!((ring[0].color, ring[0].spread_radius), (color::BG_2, px(2.5)));
    }

    #[test]
    fn counts_cap_at_99() {
        assert_eq!(capped(3), "3");
        assert_eq!(capped(99), "99");
        assert_eq!(capped(100), "99+");
    }
}

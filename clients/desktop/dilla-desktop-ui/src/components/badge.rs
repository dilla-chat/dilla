//! `Badge` (packages/ui/src/Pill): the unread and mention count pill.

use std::time::Duration;

use gpui::{
    App, BoxShadow, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, Stateful, Styled, Window, div, point, px,
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
    ring(1.0)
}

/// The mention pill's ring at `scale`.
fn ring(scale: f32) -> Vec<BoxShadow> {
    vec![BoxShadow {
        color: color::BG_2,
        offset: point(px(0.0), px(0.0)),
        blur_radius: px(0.0),
        spread_radius: px(2.5 * scale),
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

/// How a badge pops in (`d-pop`): over `run`, on `curve`.
#[derive(Clone, Copy, Debug)]
struct Pop {
    run: Duration,
    curve: [f32; 4],
}

#[derive(IntoElement)]
pub struct Badge {
    id: ElementId,
    kind: BadgeKind,
    count: u32,
    text: Option<SharedString>,
    in_row: bool,
    pop: Option<Pop>,
}

impl Badge {
    pub fn new(id: impl Into<ElementId>, kind: BadgeKind, count: u32) -> Self {
        Self {
            id: id.into(),
            kind,
            count,
            text: None,
            in_row: false,
            pop: None,
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

    /// The badge pops in with `d-pop` over `run` on `curve` when it appears (ChannelRow.css:35, CommunityRail.css:25), once
    /// per mount of its id.
    pub fn pop(mut self, run: Duration, curve: [f32; 4]) -> Self {
        self.pop = Some(Pop { run, curve });
        self
    }
}

/// The pill at `scale`: 1.0 is its size, and every length in it and its text are multiplied by the scale, which is how a CSS
/// `scale()` of the whole pill, ring included, draws.
fn pill(
    id: ElementId,
    kind: BadgeKind,
    label: SharedString,
    in_row: bool,
    scale: f32,
) -> Stateful<gpui::Div> {
    let (bg, fg) = match kind {
        BadgeKind::Unread => (color::ACCENT, color::ACCENT_INK),
        BadgeKind::Mention => (color::MENTION, color::MENTION_INK),
    };
    let pill = div()
        .id(id)
        .flex()
        .flex_none()
        .items_center()
        .justify_center()
        .h(px(16.0 * scale))
        .bg(bg);
    let size = if in_row { text::MICRO } else { text::CHIP };
    let pill = pill.child(
        line(
            label,
            size * scale,
            crate::text::root_line_height(size) * scale,
            fg,
        )
        .weight(font::WEIGHT_DISPLAY),
    );
    if in_row {
        pill.px(px(5.0 * scale)).rounded(radius::SM * scale)
    } else {
        let pill = pill
            .min_w(px(16.0 * scale))
            .px(px(4.0 * scale))
            .rounded(radius::PILL * scale);
        if kind == BadgeKind::Mention {
            pill.shadow(ring(scale))
        } else {
            pill
        }
    }
}

impl RenderOnce for Badge {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        if self.count == 0 {
            return div().id(self.id).size_0().into_any_element();
        }
        let label = self.text.unwrap_or_else(|| capped(self.count).into());
        let (kind, in_row, id) = (self.kind, self.in_row, self.id);
        let Some(pop) = self.pop else {
            return pill(id, kind, label, in_row, 1.0).into_any_element();
        };
        let pop_id = (id.clone(), "pop");
        crate::motion::pop_in(pop_id, pop.run, pop.curve, cx, move |scale| {
            // The badge keeps its id; a scaled copy of it has its own.
            let (id, scale) = match scale {
                None => (id.clone(), 1.0),
                Some(scale) => ((id.clone(), "scaled").into(), scale),
            };
            pill(id, kind, label.clone(), in_row, scale).into_any_element()
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_mention_pill_has_a_2_5px_ring_in_bg_2() {
        let ring = mention_ring();
        assert_eq!(ring.len(), 1);
        assert_eq!(
            (ring[0].color, ring[0].spread_radius),
            (color::BG_2, px(2.5))
        );
    }

    #[test]
    fn counts_cap_at_99() {
        assert_eq!(capped(3), "3");
        assert_eq!(capped(99), "99");
        assert_eq!(capped(100), "99+");
    }
}

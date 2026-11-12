//! `StateDot` (packages/ui/src/StateDot): a small state marker drawn as a shape. Filled for ok, warn and danger; a ring for
//! off; a dashed ring for pending. Idle and dnd are solid discs in Signal. It never carries meaning alone: every caller keeps
//! its text beside it.

use gpui::{
    App, ElementId, Hsla, InteractiveElement, IntoElement, RenderOnce, Styled, Window, div, px,
};

use crate::tokens::color;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StateDotTone {
    Ok,
    Off,
    Pending,
    Idle,
    Dnd,
    Warn,
    Danger,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum StateDotSize {
    /// 8px.
    #[default]
    Sm,
    /// 10px.
    Md,
}

/// How a tone draws: a fill, or a ring (solid or dashed) of `--fg-3` (`StateDot.css`).
#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Shape {
    Fill(Hsla),
    Ring { dashed: bool },
}

pub fn shape_of(tone: StateDotTone) -> Shape {
    match tone {
        StateDotTone::Ok => Shape::Fill(color::OK),
        StateDotTone::Warn | StateDotTone::Idle => Shape::Fill(color::WARN),
        StateDotTone::Danger | StateDotTone::Dnd => Shape::Fill(color::DANGER),
        StateDotTone::Off => Shape::Ring { dashed: false },
        StateDotTone::Pending => Shape::Ring { dashed: true },
    }
}

#[derive(IntoElement)]
pub struct StateDot {
    id: ElementId,
    tone: StateDotTone,
    size: StateDotSize,
}

impl StateDot {
    pub fn new(id: impl Into<ElementId>, tone: StateDotTone) -> Self {
        Self {
            id: id.into(),
            tone,
            size: StateDotSize::Sm,
        }
    }

    pub fn size(mut self, size: StateDotSize) -> Self {
        self.size = size;
        self
    }
}

impl RenderOnce for StateDot {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let side = match self.size {
            StateDotSize::Sm => 8.0,
            StateDotSize::Md => 10.0,
        };
        let dot = div()
            .id(self.id)
            .flex_none()
            .size(px(side))
            .rounded(px(side / 2.0));
        match shape_of(self.tone) {
            Shape::Fill(c) => dot.bg(c),
            Shape::Ring { dashed } => {
                let ring = dot.border(px(1.5)).border_color(color::FG_3);
                if dashed { ring.border_dashed() } else { ring }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tones_draw_the_css_shapes() {
        assert_eq!(shape_of(StateDotTone::Idle), Shape::Fill(color::WARN));
        assert_eq!(shape_of(StateDotTone::Dnd), Shape::Fill(color::DANGER));
        assert_eq!(shape_of(StateDotTone::Off), Shape::Ring { dashed: false });
        assert_eq!(
            shape_of(StateDotTone::Pending),
            Shape::Ring { dashed: true }
        );
    }
}

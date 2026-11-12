//! `AudioMeter` (packages/ui/src/AudioMeter): twelve bars that rise left to right, lit in `--ok`, unlit as a 2px baseline
//! tick in the edge colour, so the level reads by height and fill without colour too.

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, Styled, Window,
    div, px,
};

use crate::tokens::color;

pub const BARS: usize = 12;
/// `.d-audio-meter`: 14px tall.
const HEIGHT: f32 = 14.0;

/// The number of lit bars for a level (`AudioMeter.tsx`): rounded, clamped to 0..=12, a non-finite level is 0.
pub fn lit(level: f32) -> usize {
    if !level.is_finite() {
        return 0;
    }
    level.round().clamp(0.0, BARS as f32) as usize
}

/// Bar `i`'s height in percent: `30 + round(70 * i / 11)`.
pub fn bar_percent(i: usize) -> u32 {
    30 + ((70.0 * i as f32) / (BARS as f32 - 1.0)).round() as u32
}

#[derive(IntoElement)]
pub struct AudioMeter {
    id: ElementId,
    level: f32,
}

impl AudioMeter {
    /// `level` is 0..=12 bars (the caller scales its signal).
    pub fn new(id: impl Into<ElementId>, level: f32) -> Self {
        Self {
            id: id.into(),
            level,
        }
    }
}

impl RenderOnce for AudioMeter {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let on = lit(self.level);
        let mut meter = div()
            .id(self.id)
            .flex()
            .flex_none()
            .items_end()
            .gap(px(2.0))
            .h(px(HEIGHT));
        for i in 0..BARS {
            let h = HEIGHT * bar_percent(i) as f32 / 100.0;
            let bar = div()
                .relative()
                .flex_none()
                .w(px(4.0))
                .h(px(h))
                .rounded(px(1.0));
            meter = meter.child(if i < on {
                bar.bg(color::OK)
            } else {
                // Unlit: the 2px baseline tick, `inset 0 -2px 0 var(--edge)`.
                bar.child(
                    div()
                        .absolute()
                        .left_0()
                        .right_0()
                        .bottom_0()
                        .h(px(2.0))
                        .bg(color::EDGE),
                )
            });
        }
        meter
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_level_rounds_and_clamps() {
        assert_eq!(lit(0.0), 0);
        assert_eq!(lit(4.4), 4);
        assert_eq!(lit(4.6), 5);
        assert_eq!(lit(99.0), 12);
        assert_eq!(lit(-3.0), 0);
        assert_eq!(lit(f32::NAN), 0);
    }

    #[test]
    fn bars_rise_from_30_to_100_percent() {
        assert_eq!(bar_percent(0), 30);
        assert_eq!(bar_percent(11), 100);
        assert_eq!(bar_percent(5), 62);
    }
}

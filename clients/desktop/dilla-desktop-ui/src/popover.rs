//! Where a popover that hangs from its control goes (the old client's VoiceToolbar popover).

use gpui::{Bounds, Pixels, Point, point, px};

/// The gap between a popover and the control it hangs from (VoiceToolbar's 10px).
pub const POPOVER_GAP: f32 = 10.0;
/// How far a popover stays inside the window's edges (VoiceToolbar's 12px).
pub const POPOVER_EDGE: f32 = 12.0;

/// Where a popover's bottom-right corner goes: [`POPOVER_GAP`] above `control`, its right edge on the control's; moved
/// right when a popover `panel_w` wide would pass the window's left [`POPOVER_EDGE`], and kept that far inside the right
/// edge of a window `viewport_w` wide (the left edge wins in a window narrower than the popover).
pub fn corner_above_right(
    control: Bounds<Pixels>,
    viewport_w: Pixels,
    panel_w: Pixels,
) -> Point<Pixels> {
    let edge = px(POPOVER_EDGE);
    let right = control.right().min(viewport_w - edge).max(edge + panel_w);
    point(right, control.top() - px(POPOVER_GAP))
}

#[cfg(test)]
mod tests {
    use gpui::{Bounds, point, px, size};

    use super::corner_above_right;

    /// KDE share picker plan, Task 2: the share panel's bottom right is 10px above the Share control, on its right edge;
    /// moved right when the panel would pass the window's left 12px, and kept 12px inside the right edge.
    #[test]
    fn a_popover_hangs_above_its_control() {
        let button = Bounds::new(point(px(900.0), px(700.0)), size(px(120.0), px(36.0)));
        assert_eq!(
            corner_above_right(button, px(1280.0), px(320.0)),
            point(px(1020.0), px(690.0))
        );
        // Near the left edge: the panel stays 12px in.
        let left = Bounds::new(point(px(20.0), px(500.0)), size(px(120.0), px(36.0)));
        assert_eq!(
            corner_above_right(left, px(1280.0), px(320.0)),
            point(px(332.0), px(490.0))
        );
        // Past the right edge (a narrow window): 12px in.
        let right = Bounds::new(point(px(560.0), px(500.0)), size(px(120.0), px(36.0)));
        assert_eq!(
            corner_above_right(right, px(600.0), px(320.0)),
            point(px(588.0), px(490.0))
        );
        // A window narrower than the panel: its left edge wins.
        assert_eq!(corner_above_right(right, px(300.0), px(320.0)).x, px(332.0));
    }
}

//! Resizing a window that draws its own frame (client-side decorations, Linux and Windows): thin strips along the four
//! edges and squares at the four corners, each starting the platform's interactive resize
//! (`Window::start_window_resize`) with the matching cursor. They are drawn over everything, at the window's rim, only
//! while the window is client-decorated, resizable and not maximised.

use gpui::{
    AnyElement, CursorStyle, InteractiveElement, IntoElement, MouseButton, MouseDownEvent, Pixels,
    ResizeEdge, Styled, div,
};

/// The strip along an edge, and the square at a corner, logical px.
pub const EDGE: f32 = 6.0;
pub const CORNER: f32 = 12.0;

/// A hit strip: its edge and its box (x, y, width, height) in a window `width` by `height`.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Strip {
    pub edge: ResizeEdge,
    pub x: f32,
    pub y: f32,
    pub width: f32,
    pub height: f32,
}

/// The cursor of an edge.
pub fn cursor_of(edge: ResizeEdge) -> CursorStyle {
    match edge {
        ResizeEdge::Top | ResizeEdge::Bottom => CursorStyle::ResizeUpDown,
        ResizeEdge::Left | ResizeEdge::Right => CursorStyle::ResizeLeftRight,
        ResizeEdge::TopLeft | ResizeEdge::BottomRight => CursorStyle::ResizeUpLeftDownRight,
        ResizeEdge::TopRight | ResizeEdge::BottomLeft => CursorStyle::ResizeUpRightDownLeft,
    }
}

/// The eight strips of a window, edges first and corners last (so a corner wins where they meet).
pub fn strips(width: f32, height: f32) -> Vec<Strip> {
    let strip = |edge, x, y, w, h| Strip {
        edge,
        x,
        y,
        width: w,
        height: h,
    };
    vec![
        strip(ResizeEdge::Top, CORNER, 0.0, width - 2.0 * CORNER, EDGE),
        strip(
            ResizeEdge::Bottom,
            CORNER,
            height - EDGE,
            width - 2.0 * CORNER,
            EDGE,
        ),
        strip(ResizeEdge::Left, 0.0, CORNER, EDGE, height - 2.0 * CORNER),
        strip(
            ResizeEdge::Right,
            width - EDGE,
            CORNER,
            EDGE,
            height - 2.0 * CORNER,
        ),
        strip(ResizeEdge::TopLeft, 0.0, 0.0, CORNER, CORNER),
        strip(ResizeEdge::TopRight, width - CORNER, 0.0, CORNER, CORNER),
        strip(ResizeEdge::BottomLeft, 0.0, height - CORNER, CORNER, CORNER),
        strip(
            ResizeEdge::BottomRight,
            width - CORNER,
            height - CORNER,
            CORNER,
            CORNER,
        ),
    ]
}

/// The strips as elements: a press on one starts the resize.
pub fn render(width: f32, height: f32) -> Vec<AnyElement> {
    strips(width, height)
        .into_iter()
        .map(|s| {
            let edge = s.edge;
            div()
                .absolute()
                .left(Pixels::from(s.x))
                .top(Pixels::from(s.y))
                .w(Pixels::from(s.width))
                .h(Pixels::from(s.height))
                .cursor(cursor_of(edge))
                .on_mouse_down(MouseButton::Left, move |_: &MouseDownEvent, window, cx| {
                    cx.stop_propagation();
                    window.start_window_resize(edge);
                })
                .into_any_element()
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn eight_strips_with_the_right_cursors_and_corners_last() {
        let all = strips(1000.0, 700.0);
        assert_eq!(all.len(), 8);
        assert!(
            all[..4]
                .iter()
                .all(|s| !matches!(s.edge, ResizeEdge::TopLeft | ResizeEdge::BottomRight))
        );
        let at = |edge| all.iter().find(|s| s.edge == edge).copied().unwrap();
        let right = at(ResizeEdge::Right);
        assert_eq!(
            (right.x, right.y, right.width, right.height),
            (994.0, 12.0, 6.0, 676.0)
        );
        let corner = at(ResizeEdge::BottomRight);
        assert_eq!((corner.x, corner.y), (988.0, 688.0));
        assert_eq!(cursor_of(ResizeEdge::Left), CursorStyle::ResizeLeftRight);
        assert_eq!(cursor_of(ResizeEdge::Bottom), CursorStyle::ResizeUpDown);
        assert_eq!(
            cursor_of(ResizeEdge::TopLeft),
            CursorStyle::ResizeUpLeftDownRight
        );
        assert_eq!(
            cursor_of(ResizeEdge::BottomLeft),
            CursorStyle::ResizeUpRightDownLeft
        );
    }
}

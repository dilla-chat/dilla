//! Chromium paints every box between edges rounded to whole CSS pixels (a box laid out at y = 191.78 paints from 192, at
//! every device scale), while GPUI rounds layout edges to device pixels (191.5 stays 191.5 at scale 2). A box whose
//! position comes out on a half CSS pixel (a 32px control centred in a 35px row) therefore paints one device pixel off the
//! web's. [`snapped`] wraps such a box and moves it, with everything inside it, onto the whole CSS pixel the web paints it
//! on, rounding halves down the page and to the right, as the web's fractional carry does.

use std::cell::Cell;

use gpui::{
    AnyElement, App, BorderStyle, Bounds, Element, ElementId, GlobalElementId, Hsla,
    InspectorElementId, IntoElement, LayoutId, Pixels, Point, Styled, Window, canvas, point, px,
    quad, transparent_black,
};

thread_local! {
    /// The shift the snapped boxes around the one being prepainted applied (logical px).
    static SHIFT: Cell<(f32, f32)> = const { Cell::new((0.0, 0.0)) };
}

/// The whole CSS pixel Chromium paints a box edge at `v` (logical px) on: halves go up.
pub fn css_pixel(v: f32) -> f32 {
    (v + 0.5).floor()
}

/// The child, painted from the whole CSS pixel its layout origin rounds to (see the module).
pub fn snapped(child: impl IntoElement) -> Snapped {
    Snapped {
        child: child.into_any_element(),
    }
}

/// A 1px `edge` box with `radius` over its parent's whole area, drawn between edges each rounded to the CSS pixel the
/// web paints it on: a card whose height ends on a half pixel (138.5) paints 139 tall, as the web's does. The parent keeps
/// a transparent 1px border for the layout and puts this first among its children.
pub fn edge_box(radius: Pixels, edge: Hsla) -> impl IntoElement {
    canvas(
        |bounds, _, _| {
            let carried = SHIFT.with(Cell::get);
            let snap = |v: Pixels, carry: f32| px(css_pixel(f32::from(v) - carry));
            Bounds::from_corners(
                point(
                    snap(bounds.left(), carried.0),
                    snap(bounds.top(), carried.1),
                ),
                point(
                    snap(bounds.right(), carried.0),
                    snap(bounds.bottom(), carried.1),
                ),
            )
        },
        move |_, snapped, window, _| {
            window.paint_quad(quad(
                snapped,
                radius,
                transparent_black(),
                px(1.0),
                edge,
                BorderStyle::Solid,
            ));
        },
    )
    // Over the border box: the parent's own (transparent) border is outside its padding box.
    .absolute()
    .top(px(-1.0))
    .left(px(-1.0))
    .right(px(-1.0))
    .bottom(px(-1.0))
}

pub struct Snapped {
    child: AnyElement,
}

impl IntoElement for Snapped {
    type Element = Self;

    fn into_element(self) -> Self::Element {
        self
    }
}

impl Element for Snapped {
    type RequestLayoutState = ();
    type PrepaintState = ();

    fn id(&self) -> Option<ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        _id: Option<&GlobalElementId>,
        _inspector_id: Option<&InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, ()) {
        (self.child.request_layout(window, cx), ())
    }

    fn prepaint(
        &mut self,
        _id: Option<&GlobalElementId>,
        _inspector_id: Option<&InspectorElementId>,
        bounds: Bounds<Pixels>,
        _request_layout: &mut (),
        window: &mut Window,
        cx: &mut App,
    ) {
        // Each box snaps from its own layout position, as Chromium's do: a snapped box inside a snapped one first takes
        // back the shift its ancestors applied.
        let (x, y) = (f32::from(bounds.origin.x), f32::from(bounds.origin.y));
        let carried = SHIFT.with(Cell::get);
        let (lx, ly) = (x - carried.0, y - carried.1);
        let shift = (css_pixel(lx) - lx, css_pixel(ly) - ly);
        let delta = Point::new(px(shift.0 - carried.0), px(shift.1 - carried.1));
        SHIFT.with(|s| s.set(shift));
        window.with_element_offset(delta, |window| {
            self.child.prepaint(window, cx);
        });
        SHIFT.with(|s| s.set(carried));
    }

    fn paint(
        &mut self,
        _id: Option<&GlobalElementId>,
        _inspector_id: Option<&InspectorElementId>,
        _bounds: Bounds<Pixels>,
        _request_layout: &mut (),
        _prepaint: &mut (),
        window: &mut Window,
        cx: &mut App,
    ) {
        self.child.paint(window, cx);
    }
}

#[cfg(test)]
mod tests {
    use super::css_pixel;

    #[test]
    fn halves_round_down_the_page() {
        assert_eq!(css_pixel(191.5), 192.0);
        assert_eq!(css_pixel(191.25), 191.0);
        assert_eq!(css_pixel(191.75), 192.0);
        assert_eq!(css_pixel(191.0), 191.0);
    }
}

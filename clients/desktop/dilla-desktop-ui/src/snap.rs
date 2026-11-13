//! Chromium paints every box between edges rounded to whole CSS pixels (a box laid out at y = 191.78 paints from 192, at
//! every device scale), while GPUI rounds layout edges to device pixels (191.5 stays 191.5 at scale 2). A box whose
//! position comes out on a half CSS pixel (a 32px control centred in a 35px row) therefore paints one device pixel off the
//! web's. [`snapped`] wraps such a box and moves it, with everything inside it, onto the whole CSS pixel the web paints it
//! on, rounding halves down the page and to the right, as the web's fractional carry does.

use gpui::{
    AnyElement, App, Bounds, Element, ElementId, GlobalElementId, InspectorElementId, IntoElement,
    LayoutId, Pixels, Point, Window, px,
};

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
        let (x, y) = (f32::from(bounds.origin.x), f32::from(bounds.origin.y));
        let delta = Point::new(px(css_pixel(x) - x), px(css_pixel(y) - y));
        window.with_element_offset(delta, |window| {
            self.child.prepaint(window, cx);
        });
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

//! The vendored GPUI's DILLA PATCHes that only a window can show (core/vendor/README.md): the layout-unit rules and the
//! edge snap have their table tests in `dilla-desktop-ui/src/metrics.rs`; these draw a box and look at what the window did.

use std::cell::Cell;
use std::rc::Rc;

use gpui::{
    App, Bounds, Context, Element, ElementId, GlobalElementId, InspectorElementId, IntoElement,
    LayoutId, ParentElement, Pixels, Render, Styled, TestAppContext, Window, div, px,
};

/// A box that records its laid-out size (`Window::laid_out_size`) and its snapped bounds.
struct Probe {
    seen: Rc<Cell<Option<(f32, f32)>>>,
    style: gpui::StyleRefinement,
}

impl IntoElement for Probe {
    type Element = Self;

    fn into_element(self) -> Self {
        self
    }
}

impl Styled for Probe {
    fn style(&mut self) -> &mut gpui::StyleRefinement {
        &mut self.style
    }
}

impl Element for Probe {
    type RequestLayoutState = LayoutId;
    type PrepaintState = ();

    fn id(&self) -> Option<ElementId> {
        None
    }

    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }

    fn request_layout(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, LayoutId) {
        use gpui::Refineable as _;
        let mut style = gpui::Style::default();
        style.refine(&self.style);
        let id = window.request_layout(style, [], cx);
        (id, id)
    }

    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        bounds: Bounds<Pixels>,
        layout: &mut LayoutId,
        window: &mut Window,
        _: &mut App,
    ) {
        let laid_out = f32::from(window.laid_out_size(*layout).width);
        self.seen
            .set(Some((laid_out, f32::from(bounds.size.width))));
    }

    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&InspectorElementId>,
        _: Bounds<Pixels>,
        _: &mut LayoutId,
        _: &mut (),
        _: &mut Window,
        _: &mut App,
    ) {
    }
}

struct Host {
    seen: Rc<Cell<Option<(f32, f32)>>>,
}

impl Render for Host {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        let probe = Probe {
            seen: self.seen.clone(),
            style: gpui::StyleRefinement::default(),
        }
        .absolute()
        .left(px(100.5))
        .top(px(10.0))
        .w(px(54.609375))
        .h(px(20.0));
        div().size_full().relative().child(probe)
    }
}

/// Task 28: `Window::laid_out_size` answers the box as laid out (on the layout unit), not as its edges snap: a box at x
/// 100.5, 54.609375 wide, paints 54 px (101 to 155) at scale 2, and is laid out 54.609375 wide (what Chromium decides
/// its text's ellipsis against).
#[gpui::test]
fn laid_out_size_is_the_unsnapped_box(cx: &mut TestAppContext) {
    let seen = Rc::new(Cell::new(None));
    let host_seen = seen.clone();
    let (_, vcx) = cx.add_window_view(move |_, _| Host { seen: host_seen });
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    let (laid_out, painted) = seen.get().expect("the probe drew");
    assert_eq!(laid_out, 54.609375);
    assert_eq!(painted, 54.0);
}

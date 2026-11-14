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

/// A clickable box laid out at x 100.5, y 10.5, 54.609375 x 20 (it paints 101..155 x 11..31 at scale 2).
struct Clickable {
    clicks: Rc<Cell<u32>>,
}

impl Render for Clickable {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        use gpui::InteractiveElement as _;
        let clicks = self.clicks.clone();
        div().size_full().relative().child(
            div()
                .id("target")
                .absolute()
                .left(px(100.5))
                .top(px(10.5))
                .w(px(54.609375))
                .h(px(20.0))
                .on_mouse_down(gpui::MouseButton::Left, move |_, _, _| {
                    clicks.set(clicks.get() + 1)
                }),
        )
    }
}

/// Task 28 (T6c review m5): the edge snap moves the hit box with the painted box: a pointer lands on a box exactly where
/// the box paints (101..155, 11..31), not where it was laid out (100.5..155.11, 10.5..30.5).
#[gpui::test]
fn hit_bounds_are_the_painted_bounds(cx: &mut TestAppContext) {
    let clicks = Rc::new(Cell::new(0));
    let c = clicks.clone();
    let (_, vcx) = cx.add_window_view(move |_, _| Clickable { clicks: c });
    vcx.update(|window, _| {
        assert_eq!(window.scale_factor(), 2.0, "the test platform's scale");
        window.refresh();
    });
    vcx.run_until_parked();
    let mut lands = |x: f32, y: f32| {
        let before = clicks.get();
        vcx.simulate_mouse_down(
            gpui::point(px(x), px(y)),
            gpui::MouseButton::Left,
            gpui::Modifiers::none(),
        );
        vcx.simulate_mouse_up(
            gpui::point(px(x), px(y)),
            gpui::MouseButton::Left,
            gpui::Modifiers::none(),
        );
        clicks.get() > before
    };
    // Inside the laid-out box, outside the painted one: no hit.
    assert!(!lands(100.75, 20.0), "left of the painted edge");
    assert!(!lands(155.05, 20.0), "right of the painted edge");
    assert!(!lands(120.0, 10.75), "above the painted edge");
    // Inside the painted box, at its edges: a hit.
    assert!(lands(101.25, 20.0));
    assert!(lands(154.75, 20.0));
    assert!(lands(120.0, 11.25));
    assert!(lands(120.0, 30.75));
    assert!(!lands(120.0, 31.25), "below the painted edge");
}

/// Task 28 (T6c review m5): the dashed-border patch of the vendored wgpu shader (core/vendor/README.md, "gpui-pre-wgpu
/// 0.3.12") is in place: a thin border (under 3 device px, the web's 1px at scale 2) dashes 3 widths on and 2 off, as
/// Blink's StrokeData does; a thicker one keeps upstream's 2 on, 1 off. The shader runs on the GPU only (the parity
/// screens that draw a dashed edge pin its pixels); this pins the pattern the shader computes, and mirrors it.
#[test]
fn the_dashed_border_follows_blink_s_pattern() {
    let shader = include_str!("../../../../core/vendor/gpui-pre-wgpu/src/shaders.wgsl");
    let wgsl: String = shader.split_whitespace().collect::<Vec<_>>().join(" ");
    for line in [
        "max(max(quad.border_widths.top, quad.border_widths.bottom), max(quad.border_widths.left, quad.border_widths.right)) < 3.0;",
        "let dash_length_per_width = select(2.0, 3.0, thin);",
        "let dash_gap_per_width = select(1.0, 2.0, thin);",
        "let dash_period_per_width = dash_length_per_width + dash_gap_per_width;",
    ] {
        assert!(wgsl.contains(line), "the shader computes: {line}");
    }
    // The pattern (dash, gap) in device px for a border `w` device px wide, as the shader selects it.
    let pattern = |w: f32| {
        let thin = w < 3.0;
        let (on, off) = if thin { (3.0, 2.0) } else { (2.0, 1.0) };
        (on * w, off * w)
    };
    // The web's 1px dashed border at scale 2: 6 device px on, 4 off (Blink: 3 css px on, 2 off).
    assert_eq!(pattern(2.0), (6.0, 4.0));
    assert_eq!(pattern(1.0), (3.0, 2.0));
    assert_eq!(pattern(4.0), (8.0, 4.0), "a thick border keeps upstream's");
}

/// Two focusable boxes with clicks: `own` also handles Enter and Space on key-down itself (stopping propagation, as the
/// member row, the rail and the channel rows do); `plain` has only its click.
struct Activation {
    own_clicks: Rc<Cell<u32>>,
    own_keys: Rc<Cell<u32>>,
    plain_clicks: Rc<Cell<u32>>,
    own: gpui::FocusHandle,
    plain: gpui::FocusHandle,
}

impl Render for Activation {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        use gpui::InteractiveElement as _;
        use gpui::StatefulInteractiveElement as _;
        let (own_clicks, own_keys, plain_clicks) = (
            self.own_clicks.clone(),
            self.own_keys.clone(),
            self.plain_clicks.clone(),
        );
        div()
            .size_full()
            .child(
                div()
                    .id("own")
                    .size(px(20.0))
                    .track_focus(&self.own)
                    .on_click(move |_, _, _| own_clicks.set(own_clicks.get() + 1))
                    .on_key_down(move |e: &gpui::KeyDownEvent, _, cx| {
                        if matches!(e.keystroke.key.as_str(), "enter" | "space") {
                            own_keys.set(own_keys.get() + 1);
                            cx.stop_propagation();
                        }
                    }),
            )
            .child(
                div()
                    .id("plain")
                    .size(px(20.0))
                    .track_focus(&self.plain)
                    .on_click(move |_, _, _| plain_clicks.set(plain_clicks.get() + 1)),
            )
    }
}

fn press_and_release(vcx: &mut gpui::VisualTestContext, key: &'static str) {
    let keystroke = gpui::Keystroke::parse(key).unwrap_or_else(|_| panic!("{key} is a key"));
    vcx.simulate_event(gpui::KeyDownEvent {
        keystroke: keystroke.clone(),
        is_held: false,
        prefer_character_input: false,
    });
    vcx.simulate_event(gpui::KeyUpEvent { keystroke });
    vcx.run_until_parked();
}

/// Task 28 wave 2 (T28 parity review I4): an element that handles Enter or Space on key-down itself (stopping
/// propagation) is not also clicked by the key-up, so a toggle (the member row's card) does not open and close again; an
/// element with only a click is still clicked once by Enter and by Space, as a browser's activation behaviour runs only
/// for a keydown whose default was not prevented.
#[gpui::test]
fn a_key_down_the_element_handles_does_not_also_click(cx: &mut TestAppContext) {
    let own_clicks = Rc::new(Cell::new(0));
    let own_keys = Rc::new(Cell::new(0));
    let plain_clicks = Rc::new(Cell::new(0));
    let (oc, ok, pc) = (own_clicks.clone(), own_keys.clone(), plain_clicks.clone());
    let (view, vcx) = cx.add_window_view(move |_, cx| Activation {
        own_clicks: oc,
        own_keys: ok,
        plain_clicks: pc,
        own: cx.focus_handle(),
        plain: cx.focus_handle(),
    });
    vcx.update(|window, cx| {
        let own = view.read(cx).own.clone();
        window.focus(&own, cx);
    });
    vcx.run_until_parked();
    for key in ["enter", "space"] {
        press_and_release(vcx, key);
    }
    assert_eq!(own_keys.get(), 2, "the element's own key-down ran");
    assert_eq!(own_clicks.get(), 0, "and the key-up did not click it too");
    vcx.update(|window, cx| {
        let plain = view.read(cx).plain.clone();
        window.focus(&plain, cx);
    });
    vcx.run_until_parked();
    press_and_release(vcx, "enter");
    assert_eq!(plain_clicks.get(), 1, "Enter clicks a plain element once");
    press_and_release(vcx, "space");
    assert_eq!(plain_clicks.get(), 2, "and so does Space");
}

/// The member list (I4): Enter on a focused member row opens its card and leaves it open (the row toggles the card, so
/// a second activation from the key-up closed it again).
struct Members {
    card: Rc<Cell<bool>>,
}

impl Render for Members {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        use dilla_desktop_ui::components::Presence;
        use dilla_desktop_ui::components::members::{GroupKind, ListGroup, ListMember, MemberList};
        let card = self.card.clone();
        div().size_full().child(
            MemberList::new(
                "members",
                vec![ListGroup {
                    id: "online".into(),
                    kind: GroupKind::Online,
                    title: "online".into(),
                    color: None,
                    members: vec![ListMember {
                        id: "ada".into(),
                        name: "ada".into(),
                        is_owner: false,
                        is_bot: false,
                        status: "".into(),
                        color: None,
                        presence: Presence::Online,
                    }],
                }],
            )
            .on_open(move |_, _, _| card.set(!card.get())),
        )
    }
}

#[gpui::test]
fn enter_on_a_member_row_leaves_its_card_open(cx: &mut TestAppContext) {
    let card = Rc::new(Cell::new(false));
    let c = card.clone();
    let (_, vcx) = cx.add_window_view(move |_, _| Members { card: c });
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    // The list is one tab stop: its first row.
    vcx.update(|window, cx| window.focus_next(cx));
    vcx.run_until_parked();
    press_and_release(vcx, "enter");
    assert!(card.get(), "the card is open after Enter");
    press_and_release(vcx, "enter");
    assert!(!card.get(), "a second Enter closes it");
}

/// A box of 13px text at 1.5 that records the window's line height inside it.
struct LineHeight {
    seen: Rc<Cell<f32>>,
}

impl Render for LineHeight {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        let seen = self.seen.clone();
        div()
            .size_full()
            .text_size(px(13.0))
            .line_height(gpui::relative(1.5))
            .child(
                gpui::canvas(
                    move |_, window, _| seen.set(f32::from(window.line_height())),
                    |_, _, _, _| {},
                )
                .size_full(),
            )
    }
}

/// Task 28 wave 2 (the bio textarea's residue): at a whole scale the window's line height lies on Chromium's layout unit,
/// so gpui-component's textarea lays 13px lines at 1.5 19.5px apart as the web's `<textarea>` does (upstream: 20).
#[gpui::test]
fn the_line_height_lies_on_the_layout_unit(cx: &mut TestAppContext) {
    let seen = Rc::new(Cell::new(0.0));
    let s = seen.clone();
    let (_, vcx) = cx.add_window_view(move |_, _| LineHeight { seen: s });
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    assert_eq!(seen.get(), 19.5);
}

//! `Tooltip` (packages/ui/src/Tooltip): the label (and the chord) of an icon control, 11px in a `--surface-2` box with the
//! `--line-strong` edge, shown 400 ms after the pointer enters (R15, WCAG 1.4.13) and hidden when it leaves.
//!
//! Use [`tooltip`] with `StatefulInteractiveElement::tooltip` and [`SHOW_DELAY`] with `tooltip_show_delay`; GPUI keeps the
//! tooltip while the pointer is on its owner, hides it on leave, and a press dismisses it. GPUI places that tooltip at the
//! pointer; [`WithTooltip`] places it against its control as Tooltip.tsx does ([`place`]: above, centred, 6px off, below
//! when there is no room above, kept 8px inside the window).

use std::time::Duration;

use gpui::{
    AnyElement, AnyView, App, AppContext, AvailableSpace, Bounds, Context, Div, Edges, Element,
    ElementId, GlobalElementId, InspectorElementId, InteractiveElement, IntoElement, LayoutId,
    MouseButton, ParentElement, Pixels, Point, Position, Render, RenderOnce, SharedString, Size,
    StatefulInteractiveElement, Style, Styled, Task, Window, div, point, px,
};

use crate::metrics::shadow_1;
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

/// Tooltip.tsx `SHOW_MS`.
pub const SHOW_DELAY: Duration = Duration::from_millis(400);
/// Tooltip.tsx `HIDE_MS`.
pub const HIDE_DELAY: Duration = Duration::from_millis(100);

pub struct TooltipView {
    label: SharedString,
    keys: Vec<SharedString>,
}

impl TooltipView {
    pub fn new(label: impl Into<SharedString>, keys: &[&str]) -> Self {
        Self {
            label: label.into(),
            keys: keys
                .iter()
                .map(|k| SharedString::from((*k).to_owned()))
                .collect(),
        }
    }

    /// The text of the chord as the tooltip shows it: the keys joined with `+`.
    pub fn chord(&self) -> String {
        self.keys
            .iter()
            .map(SharedString::as_ref)
            .collect::<Vec<_>>()
            .join("+")
    }
}

impl Render for TooltipView {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        tip_box(self.label.clone(), &self.chord())
    }
}

/// The tooltip's box (`.d-tooltip`: padding 4px 8px, gap 8px, 11px on 16px lines, the `--line-strong` edge), the chord
/// (when there is one) after the label in `--fg-3`.
fn tip_box(label: SharedString, chord: &str) -> Div {
    let mut root = div()
        .flex()
        .items_center()
        .gap(px(8.0))
        .max_w(px(320.0))
        .px(px(8.0))
        .py(px(4.0))
        .bg(color::SURFACE_2)
        .border_1()
        .border_color(color::LINE_STRONG)
        .rounded(radius::MD)
        .shadow(shadow_1())
        .child(line(label, text::XS, 16.0, color::FG));
    if !chord.is_empty() {
        root = root.child(line(
            chord.to_owned(),
            text::XS,
            line_height(text::XS, 1.5),
            color::FG_3,
        ));
    }
    root
}

/// Tooltip.tsx `placement`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Placement {
    Top,
    Bottom,
    Right,
}

/// Tooltip.tsx `GAP`: between the control and the tooltip.
pub const GAP: f32 = 6.0;
/// Tooltip.tsx `MARGIN`: the tooltip stays this far inside the window.
pub const MARGIN: f32 = 8.0;

/// Tooltip.tsx `place`: where a tooltip `tip` big goes against the control at `anchor` in a window `viewport` big.
pub fn place(
    anchor: Bounds<Pixels>,
    tip: Size<Pixels>,
    viewport: Size<Pixels>,
    placement: Placement,
) -> Point<Pixels> {
    let (a, t) = (anchor, tip);
    let (gap, margin) = (px(GAP), px(MARGIN));
    let (left, top) = match placement {
        Placement::Right => (
            a.right() + gap,
            a.top() + a.size.height / 2.0 - t.height / 2.0,
        ),
        Placement::Top | Placement::Bottom => {
            let left = a.left() + a.size.width / 2.0 - t.width / 2.0;
            let mut top = if placement == Placement::Top {
                a.top() - t.height - gap
            } else {
                a.bottom() + gap
            };
            // A top tooltip with no room above its control opens below it instead.
            if placement == Placement::Top && top < margin {
                top = a.bottom() + gap;
            }
            (left, top)
        }
    };
    let left = left.min(viewport.width - t.width - margin).max(margin);
    let top = top.min(viewport.height - t.height - margin).max(margin);
    point(left, top)
}

#[derive(Default)]
struct TipState {
    hovered: bool,
    shown: bool,
    timer: Option<Task<()>>,
}

/// A control with a tooltip placed as Tooltip.tsx places it ([`place`]): shown [`SHOW_DELAY`] after the pointer enters,
/// hidden when it leaves and on a press.
#[derive(IntoElement)]
pub struct WithTooltip {
    id: ElementId,
    label: SharedString,
    chord: String,
    placement: Placement,
    delay: Duration,
    child: AnyElement,
}

impl WithTooltip {
    pub fn new(
        id: impl Into<ElementId>,
        label: impl Into<SharedString>,
        keys: &[&str],
        child: impl IntoElement,
    ) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            chord: keys.join("+"),
            placement: Placement::Top,
            delay: SHOW_DELAY,
            child: child.into_any_element(),
        }
    }

    /// How long the pointer rests before the tooltip shows ([`SHOW_DELAY`] unless set).
    pub fn delay(mut self, delay: Duration) -> Self {
        self.delay = delay;
        self
    }

    pub fn placement(mut self, placement: Placement) -> Self {
        self.placement = placement;
        self
    }
}

impl RenderOnce for WithTooltip {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let state = window.use_keyed_state(self.id.clone(), cx, |_, _| TipState::default());
        let shown = state.read(cx).shown;
        let (on_hover, on_press) = (state.clone(), state);
        let delay = self.delay;
        let mut root = div()
            .id(self.id)
            .relative()
            .on_hover(move |hovered, _, cx| {
                let hovered = *hovered;
                on_hover.update(cx, |st, cx| {
                    st.hovered = hovered;
                    if !hovered {
                        st.timer = None;
                        if st.shown {
                            st.shown = false;
                            cx.notify();
                        }
                    } else if !st.shown && st.timer.is_none() {
                        st.timer = Some(cx.spawn(async move |this, cx| {
                            cx.background_executor().timer(delay).await;
                            let _ = this.update(cx, |st, cx| {
                                st.timer = None;
                                if st.hovered {
                                    st.shown = true;
                                    cx.notify();
                                }
                            });
                        }));
                    }
                });
            })
            .on_mouse_down(MouseButton::Left, move |_, _, cx| {
                on_press.update(cx, |st, cx| {
                    st.timer = None;
                    if st.shown {
                        st.shown = false;
                        cx.notify();
                    }
                });
            })
            .child(self.child);
        if shown {
            root = root.child(TipLayer {
                tip: Some(tip_box(self.label, &self.chord).into_any_element()),
                placement: self.placement,
            });
        }
        root
    }
}

/// Covers its control (absolute, inset 0) so its bounds are the control's, and draws the tooltip over everything at the
/// place [`place`] gives.
struct TipLayer {
    tip: Option<AnyElement>,
    placement: Placement,
}

impl IntoElement for TipLayer {
    type Element = Self;

    fn into_element(self) -> Self::Element {
        self
    }
}

impl Element for TipLayer {
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
    ) -> (LayoutId, Self::RequestLayoutState) {
        let style = Style {
            position: Position::Absolute,
            inset: Edges::all(px(0.).into()),
            ..Style::default()
        };
        (window.request_layout(style, [], cx), ())
    }

    fn prepaint(
        &mut self,
        _id: Option<&GlobalElementId>,
        _inspector_id: Option<&InspectorElementId>,
        bounds: Bounds<Pixels>,
        _request_layout: &mut Self::RequestLayoutState,
        window: &mut Window,
        cx: &mut App,
    ) {
        let Some(mut tip) = self.tip.take() else {
            return;
        };
        let size = tip.layout_as_root(AvailableSpace::min_size(), window, cx);
        let origin = place(bounds, size, window.viewport_size(), self.placement);
        window.defer_draw(tip, origin, 2, None);
    }

    fn paint(
        &mut self,
        _id: Option<&GlobalElementId>,
        _inspector_id: Option<&InspectorElementId>,
        _bounds: Bounds<Pixels>,
        _request_layout: &mut Self::RequestLayoutState,
        _prepaint: &mut Self::PrepaintState,
        _window: &mut Window,
        _cx: &mut App,
    ) {
    }
}

/// The builder `tooltip(...)` takes: a tooltip of `label` and the chord `keys`.
pub fn tooltip(
    label: impl Into<SharedString>,
    keys: &[&str],
) -> impl Fn(&mut Window, &mut App) -> AnyView + 'static {
    let label: SharedString = label.into();
    let keys: Vec<String> = keys.iter().map(|k| (*k).to_owned()).collect();
    move |_window, cx| {
        let refs: Vec<&str> = keys.iter().map(String::as_str).collect();
        let view = TooltipView::new(label.clone(), &refs);
        cx.new(|_| view).into()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_delays_are_the_webs() {
        assert_eq!(SHOW_DELAY.as_millis(), 400);
        assert_eq!(HIDE_DELAY.as_millis(), 100);
    }

    fn b(x: f32, y: f32, w: f32, h: f32) -> Bounds<Pixels> {
        Bounds::new(point(px(x), px(y)), gpui::size(px(w), px(h)))
    }

    #[test]
    fn a_tooltip_is_placed_as_the_webs() {
        let view = gpui::size(px(1280.), px(800.));
        let tip = gpui::size(px(60.), px(26.));
        // Centred, 6px above.
        assert_eq!(
            place(b(600., 100., 40., 28.), tip, view, Placement::Top),
            point(px(590.), px(68.))
        );
        // At the right edge: 8px inside.
        assert_eq!(
            place(b(1240., 52., 30., 28.), tip, view, Placement::Top),
            point(px(1212.), px(20.))
        );
        // No room above (under 8px): below.
        assert_eq!(
            place(b(600., 10., 40., 28.), tip, view, Placement::Top),
            point(px(590.), px(44.))
        );
        assert_eq!(
            place(b(10., 100., 20., 20.), tip, view, Placement::Right),
            point(px(36.), px(97.))
        );
    }

    #[test]
    fn the_chord_joins_with_plus() {
        assert_eq!(
            TooltipView::new("Mute", &["Ctrl", "Shift", "M"]).chord(),
            "Ctrl+Shift+M"
        );
        assert_eq!(TooltipView::new("Mute", &[]).chord(), "");
    }
}

//! `OnboardingFrame` (packages/ui/src/OnboardingFrame): the brand mark at the top left (24px in, 22px down) over `--bg`,
//! and every step in one centred card: 400px wide, `--bg-2`, a hairline edge, radius `--r-xxl`, `--shadow-card`, 28px of
//! padding and a 20px gap. The card's first row is the step line (10px tracked capitals in `--fg-3`) with the progress
//! dots at its right; then the 20px/700 title, the body (12.5px `--fg-2`, 6px under the title, a 14px gap) and the footer
//! (14px under the body): the 40px accent action over the row of links.
//!
//! Also the frame's parts the steps share: [`PrimaryAction`] (the 40px action with its arrow), [`NavLink`] (the 11.5px
//! links under it), [`OnboardingField`] (a text field 40px tall, as the frame sets `.d-text-field__input`) and
//! [`ReadyPanel`] (the done step's panel).
//!
//! The brand glow (`radial-gradient(900px 420px at 50% -10%, --brand-glow, transparent 70%)`) is drawn by
//! [`brand_glow`]: GPUI has no radial gradient, so the ramp is painted in 4px cells. A cell's step of it is about 0.27
//! of a colour level across (the ramp runs over 630px) and about 0.58 of one down (over 294px).

use gpui::{
    AnyElement, App, ClickEvent, ElementId, Entity, Focusable, InteractiveElement, IntoElement,
    ParentElement, RenderOnce, SharedString, StatefulInteractiveElement, Styled, Window, div,
    prelude::FluentBuilder, px,
};
use gpui_component::input::{Input, InputState};

use crate::a11y::{Accessible, Role};
use crate::components::text_field::edge;
use crate::text::{line, line_height, paragraph};
use crate::tokens::{color, font, radius, text};
use crate::{Icon, IconName, metrics};

/// The card's width (`max-width: 25rem`).
pub const CARD_W: f32 = 400.0;
/// Its padding.
pub const CARD_PAD: f32 = 28.0;
/// The room inside the card: the width less the padding and the 1px edge on each side.
pub const CARD_INNER_W: f32 = CARD_W - 2.0 * CARD_PAD - 2.0;
/// The card's row gap.
const CARD_GAP: f32 = 20.0;
/// The body's row gap and its pull towards the title (`margin-top: -14px`).
const BODY_GAP: f32 = 14.0;
/// The footer's pull towards the body (`margin-top: -6px`) and its gap.
const FOOTER_PULL: f32 = 6.0;
const FOOTER_GAP: f32 = 12.0;
/// The primary action's height (`2.5rem`) and the onboarding field's.
pub const ACTION_H: f32 = 40.0;
/// `--card-shadow`: `0 30px 80px rgba(0,0,0,0.55)`.
pub fn shadow_card() -> Vec<gpui::BoxShadow> {
    crate::metrics::css_shadow(gpui::hsla(0.0, 0.0, 0.0, 0.55), 30.0, 80.0)
}

/// The body text's line height: `--text-sm` at the base 1.5.
pub fn body_line() -> f32 {
    line_height(text::SM, 1.5)
}

/// A paragraph of the frame's body, wrapped to the card.
pub fn body_paragraph(text_: &str, color_: gpui::Hsla) -> AnyElement {
    div()
        .flex()
        .flex_col()
        .children(paragraph(
            text_,
            text::SM,
            body_line(),
            color_,
            CARD_INNER_W,
        ))
        .into_any_element()
}

/// TopBar.tsx `BrandMark` at page size: the 20px brand tile with its D, the tracked DILLA and the 6x12 caret.
#[derive(IntoElement, Default)]
pub struct BrandMark;

impl RenderOnce for BrandMark {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        div()
            .flex()
            .items_center()
            .gap(px(8.0))
            .child(
                div()
                    .size(px(20.0))
                    .flex_none()
                    .rounded(radius::SM)
                    .bg(color::BRAND)
                    .flex()
                    .items_center()
                    .justify_center()
                    .child(
                        line("D", text::UI, f32::from(text::UI), color::BRAND_INK)
                            .weight(font::WEIGHT_DISPLAY),
                    ),
            )
            .child(
                line("DILLA", text::XS, line_height(text::XS, 1.5), color::FG)
                    .weight(font::WEIGHT_DISPLAY)
                    .tracking(metrics::LABEL_TRACKING_WIDE),
            )
            .child(div().w(px(6.0)).h(px(12.0)).flex_none().bg(color::BRAND))
    }
}

/// The onboarding frame: the brand and one card. The host passes the body's parts and the footer's.
#[derive(IntoElement)]
pub struct OnboardingFrame {
    id: ElementId,
    title: SharedString,
    step: Option<SharedString>,
    progress: Option<(u8, u8)>,
    banner: Option<AnyElement>,
    body: Vec<AnyElement>,
    primary: Option<AnyElement>,
    back: Option<AnyElement>,
    link: Option<AnyElement>,
}

impl OnboardingFrame {
    pub fn new(id: impl Into<ElementId>, title: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            title: title.into(),
            step: None,
            progress: None,
            banner: None,
            body: Vec::new(),
            primary: None,
            back: None,
            link: None,
        }
    }

    /// The step line (`New account · step 2 of 6`), drawn upper case; empty draws none.
    pub fn step(mut self, label: impl Into<SharedString>) -> Self {
        let label = label.into();
        self.step = (!label.is_empty()).then_some(label);
        self
    }

    /// The progress dots: step `n` (1-based) of `total`.
    pub fn progress(mut self, n: u8, total: u8) -> Self {
        self.progress = Some((n, total));
        self
    }

    /// A banner at the top of the body (a refusal, the reload notice).
    pub fn banner(mut self, banner: impl IntoElement) -> Self {
        self.banner = Some(banner.into_any_element());
        self
    }

    /// The next part of the body.
    pub fn child(mut self, part: impl IntoElement) -> Self {
        self.body.push(part.into_any_element());
        self
    }

    /// The footer's action ([`PrimaryAction`]).
    pub fn primary(mut self, action: impl IntoElement) -> Self {
        self.primary = Some(action.into_any_element());
        self
    }

    /// The row under the action: Back at the left, the other link at the right.
    pub fn nav(mut self, back: Option<AnyElement>, link: Option<AnyElement>) -> Self {
        self.back = back;
        self.link = link;
        self
    }
}

/// The progress dots (OnboardingFrame.css): 4px tall, 6px wide (the current one 18px), 4px apart; done and current in
/// the accent, the rest `--control-edge-field`.
fn dots(n: u8, total: u8) -> impl IntoElement {
    div()
        .flex()
        .flex_none()
        .gap(px(4.0))
        .children((1..=total).map(move |i| {
            div()
                .w(px(if i == n { 18.0 } else { 6.0 }))
                .h(px(4.0))
                .rounded(px(4.0))
                .bg(if i <= n {
                    color::ACCENT
                } else {
                    color::CONTROL_EDGE_FIELD
                })
        }))
}

impl RenderOnce for OnboardingFrame {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let desktop = desktop_window(window, cx);
        let card = div()
            .id(self.id.clone())
            .flex_none()
            .p(px(CARD_PAD))
            .flex()
            .flex_col()
            .gap(px(CARD_GAP))
            .bg(color::BG_2);
        let mut card = card_edge(card, desktop, |c| c.w(px(CARD_W)))
            .relative()
            .children(card_measure(desktop));
        if self.step.is_some() || self.progress.is_some() {
            card = card.child(
                div()
                    .flex()
                    .items_center()
                    .justify_between()
                    .gap(px(12.0))
                    .mb(px(2.0))
                    .child(match self.step {
                        Some(step) => line(
                            step.to_uppercase(),
                            text::MICRO,
                            line_height(text::MICRO, 1.5),
                            color::FG_3,
                        )
                        .weight(font::WEIGHT_LABEL)
                        .tracking(metrics::LABEL_TRACKING_WIDE)
                        .into_any_element(),
                        None => div().into_any_element(),
                    })
                    .when_some(self.progress, |d, (n, total)| d.child(dots(n, total))),
            );
        }
        card = card.child(
            line(
                self.title,
                text::HEADING,
                line_height(text::HEADING, 1.5),
                color::FG,
            )
            .weight(font::WEIGHT_DISPLAY)
            .tracking(-0.01),
        );
        let mut body = div()
            .flex()
            .flex_col()
            .gap(px(BODY_GAP))
            .mt(px(-BODY_GAP))
            .min_w_0();
        body = body.children(self.banner);
        for (i, part) in self.body.into_iter().enumerate() {
            // The first paragraph keeps 6px more under it (OnboardingFrame.css fix round 2).
            body = body.child(if i == 0 {
                div().mb(px(6.0)).child(part).into_any_element()
            } else {
                part
            });
        }
        card = card.child(body);
        if self.primary.is_some() || self.back.is_some() || self.link.is_some() {
            let mut footer = div()
                .flex()
                .flex_col()
                .gap(px(FOOTER_GAP))
                .mt(px(-FOOTER_PULL));
            footer = footer.children(self.primary);
            if self.back.is_some() || self.link.is_some() {
                footer = footer.child(
                    div()
                        .flex()
                        .items_center()
                        .justify_between()
                        .gap(px(14.0))
                        .mb(px(-3.5))
                        .child(self.back.unwrap_or_else(|| div().into_any_element()))
                        .children(self.link),
                );
            }
            card = card.child(footer);
        }
        centred_scroll(
            (self.id, "page"),
            PAGE_PAD,
            desktop,
            div()
                .absolute()
                .left(px(24.0))
                .top(px(22.0))
                .child(BrandMark),
            card,
        )
    }
}

/// The windows that show an authentication card as the desktop app's own compact window (beta.5
/// `html[data-desktop-window="authentication"]`), by id. The app marks a window while it is in that mode
/// ([`set_desktop_window`]); a fixture or a browser-sized page is never marked and keeps the web's frame.
#[derive(Default)]
struct DesktopAuthWindows(std::collections::HashSet<gpui::WindowId>);

impl gpui::Global for DesktopAuthWindows {}

/// Marks `window` as the desktop app's authentication window (`on`), or not.
pub fn set_desktop_window(window: &Window, on: bool, cx: &mut App) {
    let id = window.window_handle().window_id();
    let marked = cx
        .try_global::<DesktopAuthWindows>()
        .is_some_and(|w| w.0.contains(&id));
    if marked == on {
        return;
    }
    let set = &mut cx.default_global::<DesktopAuthWindows>().0;
    if on {
        set.insert(id);
    } else {
        set.remove(&id);
    }
}

/// `window` is the desktop app's authentication window: its card fills the window (DesktopTitlebar.css:24-26).
pub fn desktop_window(window: &Window, cx: &App) -> bool {
    cx.try_global::<DesktopAuthWindows>()
        .is_some_and(|w| w.0.contains(&window.window_handle().window_id()))
}

/// The card heights of the desktop authentication windows, by window: what the window fits its height to (beta.5
/// `startDesktopWindowLayout`: the card's `offsetHeight`, watched with a ResizeObserver).
#[derive(Default)]
struct DesktopCardHeights(std::collections::HashMap<gpui::WindowId, f32>);

impl gpui::Global for DesktopCardHeights {}

/// The height the card of `window` last drew at, logical px, while `window` is a desktop authentication window.
pub fn desktop_card_height(window: &Window, cx: &App) -> Option<f32> {
    cx.try_global::<DesktopCardHeights>()?
        .0
        .get(&window.window_handle().window_id())
        .copied()
}

/// Notes the height of `window`'s card; a new height draws the window again, so the app can fit it.
pub fn note_card_height(window: &mut Window, height: f32, cx: &mut App) {
    let id = window.window_handle().window_id();
    let heights = &mut cx.default_global::<DesktopCardHeights>().0;
    if heights.get(&id) != Some(&height) {
        heights.insert(id, height);
        window.refresh();
    }
}

/// Measures the card it is placed in (absolutely, over the whole card) for [`note_card_height`]: only in the desktop
/// authentication window.
pub fn card_measure(desktop: bool) -> Option<impl IntoElement> {
    desktop.then(|| {
        gpui::canvas(
            |bounds, window, cx| note_card_height(window, f32::from(bounds.size.height), cx),
            |_, (), _, _| {},
        )
        .absolute()
        .top_0()
        .left_0()
        .size_full()
    })
}

/// The card's edge: on the web (and in a fixture) the 400px card with its hairline, `--r-xxl` and `--shadow-card`;
/// in the desktop authentication window (beta.5 DesktopTitlebar.css:26, `max-width: none; border: 0; border-radius: 0;
/// box-shadow: none`) the card is the window's whole width, with no edge. `web_width` sizes the web card.
pub fn card_edge<E: Styled>(card: E, desktop: bool, web_width: impl FnOnce(E) -> E) -> E {
    if desktop {
        card.w_full()
    } else {
        web_width(card)
            .border_1()
            .border_color(color::HAIRLINE)
            .rounded(radius::XXL)
            .shadow(shadow_card())
    }
}

/// The page's padding round the card.
const PAGE_PAD: f32 = 20.0;

/// The page an authentication card sits on (the sign-up, sign-in and factor screens): `--bg`, the card centred while it
/// fits, and a vertical scroll when it does not, as the web's page scrolls a card taller than the window. The compact
/// authentication window keeps its fixed size (Task 8), so every step's controls stay reachable by scrolling. `brand`
/// is drawn over the page's top-left corner (absolutely placed by the caller) and scrolls with the card.
///
/// `desktop`: the desktop app's authentication window (beta.5 DesktopTitlebar.css:24-25,
/// `.d-onboarding-frame { padding: 0; background: var(--bg-2); align-items: start }`, the bar hidden): no glow, no
/// brand, no padding, `--bg-2`, the card at the top.
pub fn centred_scroll(
    id: impl Into<ElementId>,
    pad: f32,
    desktop: bool,
    brand: impl IntoElement,
    card: impl IntoElement,
) -> AnyElement {
    let page = div()
        .relative()
        .w_full()
        .min_h_full()
        .flex()
        .justify_center();
    let page = if desktop {
        page.items_start().child(card)
    } else {
        page.items_center()
            .p(px(pad))
            .child(brand_glow())
            .child(brand)
            .child(card)
    };
    div()
        .id(id.into())
        .size_full()
        .bg(if desktop { color::BG_2 } else { color::BG })
        .overflow_y_scroll()
        .child(page)
        .into_any_element()
}

/// The glow's ellipse (`900px 420px`), its centre (`at 50% -10%` of the page) and where it ends (`transparent 70%`).
const GLOW_RX: f32 = 900.0;
const GLOW_RY: f32 = 420.0;
const GLOW_AT_Y: f32 = -0.10;
const GLOW_END: f32 = 0.70;
/// The cell the ramp is painted in. The glow's 0.28 alpha of its blue (153) falls to nothing over the ramp's 630px
/// across (0.28 x 153 x 4 / 630, about 0.27 of a level per cell) and its 294px down (about 0.58 of a level per cell);
/// a resolution choice from the ramp's slope, not a fit to a reference.
const GLOW_CELL: f32 = 4.0;

/// OnboardingFrame.css's page background `radial-gradient(900px 420px at 50% -10%, var(--brand-glow), transparent 70%)`
/// over `--bg`: drawn over the parent's box (absolutely placed), under the brand and the card.
pub fn brand_glow() -> impl IntoElement {
    gpui::canvas(
        |_, _, _| {},
        |bounds, (), window, _| paint_brand_glow(bounds, window),
    )
    .absolute()
    .top_0()
    .left_0()
    .size_full()
}

/// The CSS ramp: `--brand-glow` at the centre to transparent at 70% of the ellipse, linear in the ellipse's distance (in
/// premultiplied colour, as browsers interpolate, so the colour stays the glow's and only its alpha falls). Each cell
/// takes the ramp's value at its centre; cells past the ramp's end are not painted.
fn paint_brand_glow(bounds: gpui::Bounds<gpui::Pixels>, window: &mut Window) {
    let glow = color::BRAND_GLOW;
    let (x0, y0) = (f32::from(bounds.origin.x), f32::from(bounds.origin.y));
    let (w, h) = (f32::from(bounds.size.width), f32::from(bounds.size.height));
    let (centre_x, centre_y) = (x0 + w / 2.0, y0 + h * GLOW_AT_Y);
    // The ramp ends on the ellipse of radii 70% of 900 by 420: only cells in its box can be lit.
    let (rx, ry) = (GLOW_RX * GLOW_END, GLOW_RY * GLOW_END);
    let rows = ((centre_y + ry - y0).min(h) / GLOW_CELL).ceil().max(0.0) as usize;
    let first_col = ((centre_x - rx - x0).max(0.0) / GLOW_CELL).floor() as usize;
    let last_col = ((centre_x + rx - x0).min(w) / GLOW_CELL).ceil() as usize;
    for row in 0..rows {
        let top = y0 + row as f32 * GLOW_CELL;
        let dy = (top + GLOW_CELL / 2.0 - centre_y) / GLOW_RY;
        for col in first_col..last_col {
            let left = x0 + col as f32 * GLOW_CELL;
            let dx = (left + GLOW_CELL / 2.0 - centre_x) / GLOW_RX;
            let t = (dx * dx + dy * dy).sqrt();
            if t >= GLOW_END {
                continue;
            }
            let a = glow.a * (1.0 - t / GLOW_END);
            window.paint_quad(gpui::fill(
                gpui::Bounds::new(
                    gpui::point(px(left), px(top)),
                    gpui::size(px(GLOW_CELL), px(GLOW_CELL)),
                ),
                gpui::Hsla { a, ..glow },
            ));
        }
    }
}

pub(crate) type Click = Box<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

/// A pressable part of the onboarding screens: focusable (the host's handle, when it tracks focus for automation, else
/// a plain tab stop), with the accent focus ring; Enter and Space click it, as GPUI does for a focused element with a
/// click handler.
/// [`pressable`] without the ring round the element: the caller draws it on a part (the key step's tick).
pub(crate) fn pressable_quiet(
    d: gpui::Stateful<gpui::Div>,
    focus: Option<&gpui::FocusHandle>,
    on_click: Option<Click>,
) -> gpui::Stateful<gpui::Div> {
    let d = match focus {
        Some(handle) => d.track_focus(handle).tab_stop(true),
        None => d.tab_index(0),
    };
    d.cursor_pointer().when_some(on_click, |d, f| d.on_click(f))
}

pub(crate) fn pressable(
    d: gpui::Stateful<gpui::Div>,
    focus: Option<&gpui::FocusHandle>,
    on_click: Option<Click>,
) -> gpui::Stateful<gpui::Div> {
    let d = match focus {
        Some(handle) => d.track_focus(handle).tab_stop(true),
        None => d.tab_index(0),
    };
    d.cursor_pointer()
        .focus_visible(|s| s.shadow(metrics::focus_ring()))
        .when_some(on_click, |d, f| d.on_click(f))
}

/// The frame's action: 40px, the card's width, radius `--r-lg`, 13px/700 with a 15px arrow after it. Ready, it is the
/// accent fill; not ready (a missing tick, a step at work) it is the prototype's `--hover-hi` bar with `--fg-3` text,
/// still pressable (the host refuses the press and says why).
#[derive(IntoElement)]
pub struct PrimaryAction {
    id: ElementId,
    label: SharedString,
    ready: bool,
    arrow: bool,
    focus: Option<gpui::FocusHandle>,
    on_click: Option<Click>,
}

impl PrimaryAction {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            ready: true,
            arrow: true,
            focus: None,
            on_click: None,
        }
    }

    /// Tracks `handle` as its focus (for a host that reports or moves focus).
    pub fn focus(mut self, handle: &gpui::FocusHandle) -> Self {
        self.focus = Some(handle.clone());
        self
    }

    pub fn ready(mut self, ready: bool) -> Self {
        self.ready = ready;
        self
    }

    /// Whether the arrow follows the label (every step's action has it).
    pub fn arrow(mut self, arrow: bool) -> Self {
        self.arrow = arrow;
        self
    }

    pub fn on_click(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_click = Some(Box::new(f));
        self
    }
}

impl RenderOnce for PrimaryAction {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        let (bg, ink) = if self.ready {
            (color::ACCENT, color::ACCENT_INK)
        } else {
            (color::HOVER_HI, color::FG_3)
        };
        let ready = self.ready;
        let base = div()
            .id(self.id)
            .a11y(Role::Button, self.label.clone())
            .w(px(CARD_INNER_W))
            .h(px(ACTION_H))
            .flex_none()
            .rounded(radius::LG)
            .border_1()
            .border_color(bg)
            .bg(bg)
            .when(ready, |d| {
                d.hover(|s| s.bg(color::ACCENT_2).border_color(color::ACCENT_2))
            })
            .flex()
            .items_center()
            .justify_center()
            .gap(px(8.0))
            .child(
                line(self.label, text::INPUT, line_height(text::INPUT, 1.5), ink)
                    .weight(font::WEIGHT_DISPLAY),
            )
            .when(self.arrow, |d| {
                d.child(Icon::new(IconName::ArrowRight).size(px(15.0)).color(ink))
            });
        pressable(base, self.focus.as_ref(), self.on_click)
    }
}

/// A link under the action (OnboardingFrame.css `__nav`): 11.5px in `--fg-link`, Back in `--fg-2`; a 24px target. (The
/// web's `--accent-2` under the pointer is not drawn: a line paints its own colour.)
#[derive(IntoElement)]
pub struct NavLink {
    id: ElementId,
    label: SharedString,
    back: bool,
    focus: Option<gpui::FocusHandle>,
    on_click: Option<Click>,
}

impl NavLink {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            back: false,
            focus: None,
            on_click: None,
        }
    }

    /// Tracks `handle` as its focus.
    pub fn focus(mut self, handle: &gpui::FocusHandle) -> Self {
        self.focus = Some(handle.clone());
        self
    }

    /// Back is `--fg-2`.
    pub fn back(mut self) -> Self {
        self.back = true;
        self
    }

    pub fn on_click(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_click = Some(Box::new(f));
        self
    }
}

impl RenderOnce for NavLink {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        let ink = if self.back {
            color::FG_2
        } else {
            color::FG_LINK
        };
        let base = div()
            .id(self.id)
            .a11y(Role::Button, self.label.clone())
            .min_h(px(24.0))
            .flex()
            .items_center()
            .child(line(
                self.label,
                text::BTN,
                line_height(text::BTN, 1.5),
                ink,
            ));
        pressable(base, self.focus.as_ref(), self.on_click)
    }
}

/// A text field of the frame: the label over a 40px field (the frame's `.d-text-field__input { height: 2.5rem }`), then
/// the error. The shared `TextField` is 36px; this is its onboarding state.
#[derive(IntoElement)]
pub struct OnboardingField {
    id: ElementId,
    label: SharedString,
    input: Entity<InputState>,
    hint: Option<SharedString>,
    error: Option<SharedString>,
}

impl OnboardingField {
    pub fn new(
        id: impl Into<ElementId>,
        label: impl Into<SharedString>,
        input: &Entity<InputState>,
    ) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            input: input.clone(),
            hint: None,
            error: None,
        }
    }

    pub fn hint(mut self, hint: impl Into<SharedString>) -> Self {
        self.hint = Some(hint.into());
        self
    }

    pub fn error(mut self, error: Option<SharedString>) -> Self {
        self.error = error;
        self
    }
}

impl RenderOnce for OnboardingField {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let invalid = self.error.is_some();
        let handle = self.input.focus_handle(cx);
        let focused = handle.contains_focused(window, cx);
        let frame = div()
            // A press anywhere in the frame focuses the field, as the browser's input box does.
            .on_mouse_down(gpui::MouseButton::Left, move |_, window, cx| {
                window.focus(&handle, cx)
            })
            .id((self.id.clone(), "frame"))
            .w(px(CARD_INNER_W))
            .h(px(ACTION_H))
            .px(px(12.0))
            .flex()
            .items_center()
            .bg(color::SURFACE_2)
            .rounded(radius::SEG)
            .border_1()
            .border_color(edge(invalid, focused, false))
            .hover(move |s| s.border_color(edge(invalid, focused, true)))
            .text_size(text::INPUT)
            .font_family(font::MONO)
            .text_color(color::FG)
            // As `TextField`: 13px (`--text-input`) with the frame's own 12px sides, never the editor's 14px and padding.
            .child(
                Input::new(&self.input)
                    .aria_label(self.label.clone())
                    .appearance(false)
                    .text_size(text::INPUT)
                    .px_0()
                    .py_0(),
            );
        div()
            .id(self.id)
            .flex()
            .flex_col()
            .gap(px(6.0))
            .child(line(
                self.label,
                text::XS,
                line_height(text::XS, 1.5),
                color::FG_2,
            ))
            .child(frame)
            .when_some(self.hint, |d, hint| {
                d.child(line(
                    hint,
                    text::BTN,
                    line_height(text::BTN, 1.5),
                    color::FG_3,
                ))
            })
            .when_some(self.error, |d, error| {
                d.child(
                    div()
                        .flex()
                        .items_center()
                        .gap(px(4.0))
                        .child(Icon::new(IconName::X).size(px(12.0)).color(color::DANGER))
                        .child(line(error, text::SM, body_line(), color::DANGER)),
                )
            })
    }
}

/// The done step's panel (OnboardingFrame.css `__ready`): 14px in, `--ok-panel` in the `--ok-border` edge, radius
/// `--r-menu`; a 30px `--ok` disc with the 16px check in `--ok-ink`, then 12px `--ok-panel-text`.
#[derive(IntoElement)]
pub struct ReadyPanel {
    text: SharedString,
}

impl ReadyPanel {
    pub fn new(text: impl Into<SharedString>) -> Self {
        Self { text: text.into() }
    }
}

impl RenderOnce for ReadyPanel {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        div()
            .flex()
            .items_center()
            .gap(px(12.0))
            .p(px(14.0))
            .mb(px(-2.0))
            .bg(color::OK_PANEL)
            .border_1()
            .border_color(color::OK_BORDER)
            .rounded(radius::MENU)
            .child(
                div()
                    .size(px(30.0))
                    .flex_none()
                    .rounded(radius::PILL)
                    .bg(color::OK)
                    .flex()
                    .items_center()
                    .justify_center()
                    .child(
                        Icon::new(IconName::Check)
                            .size(px(16.0))
                            .color(color::OK_INK),
                    ),
            )
            .child(line(
                self.text,
                text::UI,
                line_height(text::UI, 1.5),
                color::OK_PANEL_TEXT,
            ))
    }
}

#[cfg(test)]
mod shadow_tests {
    /// Review m6: `--card-shadow`'s 80px blur is drawn as GPUI's deviation, half of it (`metrics::css_blur`).
    #[test]
    fn the_card_shadow_blurs_as_css_does() {
        assert_eq!(super::shadow_card()[0].blur_radius, gpui::px(40.0));
    }
}

//! The auth card the sign-in and factor screens share (packages/ui `OnboardingFrame`, the prototype's sign-in screens):
//! the brand mark at the top left over `--bg`, and one centred card at most 400px wide (`--bg-2`, a hairline edge,
//! `--r-xxl`, 28px inside, 20px between its parts). The card holds the step line with its progress dots, the title
//! (`--text-heading` bold), an optional banner, the body (`--text-sm` `--fg-2`, 14px apart), the 40px accent action with
//! its arrow, and the links under it.
//!
//! Every size is the stylesheet's (OnboardingFrame.css), named below; positions come from layout.

use gpui::prelude::FluentBuilder;
use gpui::{
    AnyElement, App, ClickEvent, InteractiveElement, IntoElement, ParentElement, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use dilla_desktop_ui::a11y::{Accessible, Role};
use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::button::{Button, ButtonSize, ButtonVariant};
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::tracked_box;

/// `.d-onboarding-frame__card`: `max-width: 25rem`, `padding: 28px`, `gap: 20px`.
const CARD_MAX: f32 = 400.0;
const CARD_PAD: f32 = 28.0;
const CARD_GAP: f32 = 20.0;
/// `.d-onboarding-frame__bar`: `top: 22px; left: 24px`.
const BAR_TOP: f32 = 22.0;
const BAR_LEFT: f32 = 24.0;
/// `.d-onboarding-frame__body`: `gap: 14px`, pulled 14px towards the title; its first paragraph keeps 6px more under it.
/// The footer is pulled 6px towards the body, and the nav row 3.5px into the card's padding.
const BODY_GAP: f32 = 14.0;
const FIRST_PARA_GAP: f32 = 6.0;
const FOOTER_PULL: f32 = 6.0;
const NAV_PULL: f32 = 3.5;
/// The step row's margin under it (`.d-onboarding-frame__steprow`).
const STEP_ROW_GAP: f32 = 2.0;
/// `.d-onboarding-frame .d-text-field__input { height: 2.5rem }`: the card's fields.
pub const FIELD_H: f32 = 40.0;
/// The footer's gap (12px) and the links row's (14px).
const FOOTER_GAP: f32 = 12.0;
const LINKS_GAP: f32 = 14.0;
const LINKS_PULL_TOP: f32 = 1.5;
const LINKS_PULL_BOTTOM: f32 = 11.5;
/// The dots: 4px high, 6px wide, the current 18px, 4px apart.
const DOT_H: f32 = 4.0;
const DOT_W: f32 = 6.0;
const DOT_CURRENT: f32 = 18.0;
const DOT_GAP: f32 = 4.0;
/// The accent action: 40px, its arrow 15px, 8px from the label.
const ACTION_H: f32 = 40.0;
const ARROW: f32 = 15.0;
const ARROW_GAP: f32 = 8.0;
/// A link's box: 24px.
const LINK_H: f32 = 24.0;

/// `--label-tracking-wide` of the step line.
const STEP_TRACKING: f32 = crate::screens::LABEL_TRACKING_WIDE;

type OnClick = Box<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

/// The card's primary action.
pub struct Action {
    id: &'static str,
    label: SharedString,
    ready: bool,
    on_click: OnClick,
}

impl Action {
    pub fn new(
        id: &'static str,
        label: impl Into<SharedString>,
        on_click: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        Self {
            id,
            label: label.into(),
            ready: true,
            on_click: Box::new(on_click),
        }
    }

    /// Not ready (a short code, a step at work): the prototype's `--hover-hi` bar with `--fg-3` text. It still takes the
    /// press, which the screen ignores.
    pub fn ready(mut self, ready: bool) -> Self {
        self.ready = ready;
        self
    }

    /// The same action as a dialog's footer button (Settings embeds the set-up in a dialog).
    pub fn into_button(self, variant: ButtonVariant) -> AnyElement {
        let on_click = self.on_click;
        tracked_box(
            self.id,
            Button::new(self.id, self.label)
                .variant(variant)
                .size(ButtonSize::Dialog)
                .disabled(!self.ready)
                .on_click(move |e, window, cx| on_click(e, window, cx)),
        )
        .into_any_element()
    }
}

/// A link of the card (`--fg-link`, `--text-btn`); `back` draws it in `--fg-2` (the nav row's Back).
pub fn link(
    id: &'static str,
    label: impl Into<SharedString>,
    back: bool,
    on_click: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
) -> AnyElement {
    let label: SharedString = label.into();
    let ink = if back { color::FG_2 } else { color::FG_LINK };
    tracked_box(
        id,
        // A `<button>` on the web: keyboard reachable, with the focus ring.
        div()
            .id(id)
            .a11y(Role::Button, label.clone())
            .tab_index(0)
            .focus_visible(|s| s.shadow(dilla_desktop_ui::metrics::focus_ring()))
            .h(px(LINK_H))
            .flex()
            .items_center()
            .cursor_pointer()
            .on_click(on_click)
            .child(line(label, text::BTN, line_height(text::BTN, 1.5), ink)),
    )
    .into_any_element()
}

/// The nav row under the action: Back at the left, the other link at the right.
pub fn nav_row(back: AnyElement, other: Option<AnyElement>) -> AnyElement {
    div()
        .flex()
        .items_center()
        .justify_between()
        .gap(px(LINKS_GAP))
        .mb(px(-NAV_PULL))
        .child(back)
        .children(other)
        .into_any_element()
}

/// A row of links (`.d-onboarding-frame__links`: `margin: -1.5px 0 -11.5px`, the 24px link boxes centred on the
/// prototype's 17px line).
pub fn links_row(links: Vec<AnyElement>) -> AnyElement {
    div()
        .flex()
        .flex_wrap()
        .mt(px(-LINKS_PULL_TOP))
        .mb(px(-LINKS_PULL_BOTTOM))
        .gap_x(px(LINKS_GAP))
        .children(links)
        .into_any_element()
}

/// A paragraph of the body.
pub fn para(text_: impl Into<SharedString>) -> AnyElement {
    div()
        .text_size(text::SM)
        .line_height(px(line_height(text::SM, 1.5)))
        .font_family(font::MONO)
        .text_color(color::FG_2)
        .child(text_.into())
        .into_any_element()
}

/// The code's or a field's error under it (`.d-onboarding-frame__error`).
pub fn error_line(text_: impl Into<SharedString>) -> AnyElement {
    div()
        .text_size(text::SM)
        .line_height(px(line_height(text::SM, 1.5)))
        .font_family(font::MONO)
        .text_color(color::DANGER)
        .child(text_.into())
        .into_any_element()
}

/// The quiet note (`.d-onboarding-frame__note`).
pub fn note(text_: impl Into<SharedString>) -> AnyElement {
    div()
        .text_size(text::BTN)
        .line_height(px(line_height(text::BTN, 1.5)))
        .font_family(font::MONO)
        .text_color(color::FG_3)
        .child(text_.into())
        .into_any_element()
}

/// One step of the card.
#[derive(Default)]
pub struct AuthFrame {
    title: SharedString,
    step: Option<SharedString>,
    progress: Option<(u8, u8)>,
    banner: Option<(BannerTone, SharedString)>,
    body: Vec<AnyElement>,
    action: Option<Action>,
    below: Vec<AnyElement>,
}

impl AuthFrame {
    pub fn new(title: impl Into<SharedString>) -> Self {
        Self {
            title: title.into(),
            ..Self::default()
        }
    }

    /// The step line (empty: none).
    pub fn step(mut self, label: impl Into<SharedString>) -> Self {
        let label = label.into();
        self.step = (!label.is_empty()).then_some(label);
        self
    }

    /// The dots: step `n` of `total`.
    pub fn progress(mut self, n: u8, total: u8) -> Self {
        self.progress = Some((n, total));
        self
    }

    pub fn banner(mut self, banner: Option<(BannerTone, String)>) -> Self {
        self.banner = banner.map(|(tone, text_)| (tone, text_.into()));
        self
    }

    pub fn child(mut self, child: impl IntoElement) -> Self {
        self.body.push(child.into_any_element());
        self
    }

    pub fn action(mut self, action: Action) -> Self {
        self.action = Some(action);
        self
    }

    /// A row under the action (the links, the nav row).
    pub fn below(mut self, row: AnyElement) -> Self {
        self.below.push(row);
        self
    }

    /// The frame in `window`: the desktop app's authentication window draws beta.5's full-window card
    /// ([`dilla_desktop_ui::components::onboarding::frame::desktop_window`]).
    pub fn render(self, window: &Window, cx: &App) -> AnyElement {
        use dilla_desktop_ui::components::onboarding::frame::{card_edge, desktop_window};
        let desktop = desktop_window(window, cx);
        let banner = self.banner.clone();
        let has_banner = banner.is_some();
        let head = (self.step.is_some() || self.progress.is_some()).then(|| {
            div()
                .flex()
                .items_center()
                .justify_between()
                .gap(px(FOOTER_GAP))
                .mb(px(STEP_ROW_GAP))
                .child(match self.step {
                    Some(step) => line(
                        step.to_uppercase(),
                        text::MICRO,
                        line_height(text::MICRO, 1.5),
                        color::FG_3,
                    )
                    .weight(font::WEIGHT_LABEL)
                    .tracking(STEP_TRACKING)
                    .into_any_element(),
                    None => div().into_any_element(),
                })
                .children(self.progress.map(|(n, total)| {
                    div()
                        .flex()
                        .flex_none()
                        .gap(px(DOT_GAP))
                        .children((1..=total).map(|i| {
                            div()
                                .w(px(if i == n { DOT_CURRENT } else { DOT_W }))
                                .h(px(DOT_H))
                                .rounded(px(DOT_H))
                                .bg(if i <= n {
                                    color::ACCENT
                                } else {
                                    color::CONTROL_EDGE_FIELD
                                })
                        }))
                }))
        });
        let action = self.action.map(|a| {
            let (bg, ink) = if a.ready {
                (color::ACCENT, color::ACCENT_INK)
            } else {
                (color::HOVER_HI, color::FG_3)
            };
            tracked_box(
                a.id,
                div()
                    .id(a.id)
                    .a11y(Role::Button, a.label.clone())
                    .tab_index(0)
                    .focus_visible(|s| s.shadow(dilla_desktop_ui::metrics::focus_ring()))
                    .w_full()
                    .h(px(ACTION_H))
                    .rounded(radius::LG)
                    .bg(bg)
                    .flex()
                    .items_center()
                    .justify_center()
                    .gap(px(ARROW_GAP))
                    .cursor_pointer()
                    .on_click(a.on_click)
                    .child(
                        line(a.label, text::INPUT, line_height(text::INPUT, 1.5), ink)
                            .weight(font::WEIGHT_DISPLAY),
                    )
                    .child(Icon::new(IconName::ArrowRight).size(px(ARROW)).color(ink)),
            )
        });
        let card = card_edge(div().p(px(CARD_PAD)).bg(color::BG_2), desktop, |c| {
            c.w_full().max_w(px(CARD_MAX))
        })
        .flex()
        .flex_col()
        .gap(px(CARD_GAP))
        .children(head)
        // The step's `<h1>`: a heading node a reader jumps to.
        .child(
            div()
                .id("auth-title")
                .a11y(Role::Heading, self.title.clone())
                .aria_level(1)
                .child(
                    line(
                        self.title,
                        text::HEADING,
                        line_height(text::HEADING, 1.5),
                        color::FG,
                    )
                    .weight(font::WEIGHT_DISPLAY)
                    .tracking(-0.01),
                ),
        )
        .child(
            div()
                .flex()
                .flex_col()
                .gap(px(BODY_GAP))
                .mt(px(-BODY_GAP))
                .min_w_0()
                .when_some(banner, |d, (tone, t)| {
                    d.child(tracked_box(
                        "auth-banner",
                        Banner::new("auth-banner", tone, t),
                    ))
                })
                // `.d-onboarding-frame__body > p:first-child`: a banner drawn first in the body takes the place
                // of the first child, and the paragraph after it keeps no 6px.
                .children(self.body.into_iter().enumerate().map(|(i, part)| {
                    if i == 0 && !has_banner {
                        div().mb(px(FIRST_PARA_GAP)).child(part).into_any_element()
                    } else {
                        part
                    }
                })),
        )
        .when(action.is_some() || !self.below.is_empty(), |d| {
            d.child(
                div()
                    .flex()
                    .flex_col()
                    .gap(px(FOOTER_GAP))
                    .mt(px(-FOOTER_PULL))
                    .children(action)
                    .children(self.below),
            )
        });
        dilla_desktop_ui::components::onboarding::frame::centred_scroll(
            "auth-page",
            0.0,
            desktop,
            brand_mark(),
            card,
        )
    }
}

/// The brand mark of the top bar (`.d-onboarding-frame__bar`, TopBar.tsx `BrandMark`).
fn brand_mark() -> impl IntoElement {
    div()
        .absolute()
        .left(px(BAR_LEFT))
        .top(px(BAR_TOP))
        .child(dilla_desktop_ui::components::onboarding::BrandMark)
}

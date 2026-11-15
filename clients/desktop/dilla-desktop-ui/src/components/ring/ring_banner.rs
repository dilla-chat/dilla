//! `RingBanner` (packages/ui/src/RingBanner, Signal README §13): each ring on `--surface-2` inside the `--ok-border` edge
//! with 10px corners, the caller's 32px avatar ringed in `--ok` while the call is incoming (the reduced-motion form of the
//! `d-ring` wave: the static ring), the ring's text, then Answer (`--ok`) and Decline (the plain button). Rings after the
//! first are parted by a hairline.
//!
//! Keyboard: each ring is a key context ([`CONTEXT`]) holding its own focus, so Enter ([`Answer`]) answers and Escape
//! ([`Decline`]) declines the ring that has focus (the banner itself, or one of its buttons). Decline's button is a context
//! of its own ([`DECLINE_CONTEXT`]) so Enter on it declines. The host binds the keys (dilla-desktop's
//! `screens::ring::keys`) and decides where focus goes.

use std::rc::Rc;

use gpui::{
    App, BoxShadow, ElementId, FocusHandle, InteractiveElement, IntoElement, ParentElement,
    RenderOnce, SharedString, StatefulInteractiveElement, Styled, Window, actions, div, point, px,
};

use crate::a11y::{Accessible, Live, Role};
use crate::components::avatar::{Avatar, AvatarSize};
use crate::metrics::focus_ring;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

actions!(ring_banner, [Answer, Decline]);

/// The key context of one ring (its row and everything in it).
pub const CONTEXT: &str = "RingBanner";
/// The key context of a ring's Decline button: Enter there declines.
pub const DECLINE_CONTEXT: &str = "RingDecline";

/// CSS `rem`.
const REM: f32 = 16.0;
/// `.d-ring-banner__ring`: `padding: 0.625rem 0.75rem; gap: 0.5rem 0.75rem`.
const PAD_Y: f32 = 0.625 * REM;
const PAD_X: f32 = 0.75 * REM;
const GAP_Y: f32 = 0.5 * REM;
const GAP_X: f32 = 0.75 * REM;
/// `.d-ring-banner__text { flex: 1 1 14rem }`.
const TEXT_BASIS: f32 = 14.0 * REM;
/// `.d-ring-banner__actions { gap: 0.375rem }`, and its buttons: `height: 2rem; min-width: 2.75rem; padding: 0 0.875rem`.
const ACTIONS_GAP: f32 = 0.375 * REM;
const BUTTON_H: f32 = 2.0 * REM;
const BUTTON_MIN_W: f32 = 2.75 * REM;
const BUTTON_PAD_X: f32 = 0.875 * REM;
/// The ring around an incoming caller's avatar: `box-shadow: 0 0 0 2px var(--ok)`.
const WAVE: f32 = 2.0;

/// What a ring's action does: called with the ring's key.
pub type OnRing = Rc<dyn Fn(&SharedString, &mut Window, &mut App)>;

/// The focus handles of one ring, kept by the host across frames.
#[derive(Clone)]
pub struct RingFocus {
    pub row: FocusHandle,
    pub answer: FocusHandle,
    pub decline: FocusHandle,
}

impl RingFocus {
    pub fn new(cx: &mut App) -> Self {
        Self {
            row: cx.focus_handle(),
            answer: cx.focus_handle().tab_stop(true),
            decline: cx.focus_handle().tab_stop(true),
        }
    }
}

/// One ring as the banner shows it.
#[derive(Clone)]
pub struct RingRow {
    /// Stable per ring (the host's `<call>:<ring_ts>`): the automation ids are `ring-<key>`, `ring-answer-<key>` and
    /// `ring-decline-<key>`.
    pub key: SharedString,
    /// The caller, for the avatar.
    pub name: SharedString,
    /// The filled `call.ring.incoming`, or `call.ring.busy` while this device is in voice.
    pub text: SharedString,
    pub busy: bool,
    pub focus: RingFocus,
}

#[derive(IntoElement)]
pub struct RingBanner {
    id: ElementId,
    rows: Vec<RingRow>,
    answer: SharedString,
    decline: SharedString,
    on_answer: OnRing,
    on_decline: OnRing,
    /// The region's name (`call.ring.region`, "incoming calls").
    region: SharedString,
}

impl RingBanner {
    /// `answer` and `decline` are the labels (`call.ring.answer`, `call.ring.decline`).
    pub fn new(
        id: impl Into<ElementId>,
        rows: Vec<RingRow>,
        answer: impl Into<SharedString>,
        decline: impl Into<SharedString>,
        on_answer: OnRing,
        on_decline: OnRing,
    ) -> Self {
        Self {
            id: id.into(),
            rows,
            answer: answer.into(),
            decline: decline.into(),
            on_answer,
            on_decline,
            region: SharedString::default(),
        }
    }

    /// Names the banner's region for a screen reader (`call.ring.region`).
    pub fn region(mut self, label: impl Into<SharedString>) -> Self {
        self.region = label.into();
        self
    }
}

fn button(
    id: impl Into<ElementId>,
    label: SharedString,
    focus: &FocusHandle,
    answer: bool,
) -> gpui::Stateful<gpui::Div> {
    // Answer: `--ok-ink` on `--ok`, bold (`.d-ring-banner__answer`); Decline: the default button (`--fg` inside
    // `--control-edge`).
    let (fg, bg, edge, weight) = if answer {
        (color::OK_INK, color::OK, color::OK, font::WEIGHT_DISPLAY)
    } else {
        (
            color::FG,
            gpui::transparent_black(),
            color::CONTROL_EDGE,
            font::WEIGHT_BTN,
        )
    };
    div()
        .id(id)
        .a11y(Role::Button, label.clone())
        .track_focus(focus)
        .tab_stop(true)
        .flex()
        .flex_none()
        .items_center()
        .justify_center()
        .h(px(BUTTON_H))
        .min_w(px(BUTTON_MIN_W))
        .px(px(BUTTON_PAD_X))
        .font_family(font::MONO)
        .text_size(text::BTN)
        .bg(bg)
        .border_1()
        .border_color(edge)
        .rounded(radius::MD)
        .cursor_pointer()
        .focus_visible(|s| s.shadow(focus_ring()))
        .child(line(label, text::BTN, line_height(text::BTN, 1.5), fg).weight(weight))
}

impl RenderOnce for RingBanner {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let rows = self.rows.into_iter().enumerate().map(|(i, ring)| {
            let key = ring.key.clone();
            let (answer_key, answer_click, answer_on) =
                (key.clone(), key.clone(), self.on_answer.clone());
            let answer_by_click = self.on_answer.clone();
            let (decline_key, decline_click, decline_on) =
                (key.clone(), key.clone(), self.on_decline.clone());
            let decline_by_click = self.on_decline.clone();
            let mut glyph = div()
                .relative()
                .flex()
                .flex_none()
                .rounded(radius::AVATAR)
                .child(
                    Avatar::new(
                        SharedString::from(format!("ring-avatar-{key}")),
                        ring.name.clone(),
                    )
                    .size(AvatarSize::Row),
                );
            if !ring.busy {
                glyph = glyph.shadow(vec![BoxShadow {
                    color: color::OK,
                    offset: point(px(0.0), px(0.0)),
                    blur_radius: px(0.0),
                    spread_radius: px(WAVE),
                    inset: false,
                }]);
            }
            // A ring is read out at once as it arrives (the web's ring region is `aria-live`; a ring interrupts).
            let mut row = div()
                .id(SharedString::from(format!("ring-{key}")))
                .a11y(Role::Alert, ring.text.clone())
                .aria_value(ring.text.clone())
                .live(Live::Assertive)
                .key_context(CONTEXT)
                .track_focus(&ring.focus.row)
                .on_action(move |_: &Answer, window, cx| answer_on(&answer_key, window, cx))
                .on_action(move |_: &Decline, window, cx| decline_on(&decline_key, window, cx))
                .flex()
                .flex_wrap()
                .items_center()
                .gap_x(px(GAP_X))
                .gap_y(px(GAP_Y))
                .px(px(PAD_X))
                .py(px(PAD_Y))
                .child(glyph)
                .child(
                    div()
                        .flex_grow(1.0)
                        .flex_shrink(1.0)
                        .flex_basis(px(TEXT_BASIS))
                        .min_w_0()
                        .child(
                            line(ring.text, text::UI, line_height(text::UI, 1.5), color::FG)
                                .weight(font::WEIGHT_LABEL),
                        ),
                )
                .child(
                    div()
                        .flex()
                        .flex_none()
                        .gap(px(ACTIONS_GAP))
                        .child(
                            button(
                                SharedString::from(format!("ring-answer-{key}")),
                                self.answer.clone(),
                                &ring.focus.answer,
                                true,
                            )
                            .on_click(move |_, window, cx| {
                                answer_by_click(&answer_click, window, cx)
                            }),
                        )
                        .child(
                            button(
                                SharedString::from(format!("ring-decline-{key}")),
                                self.decline.clone(),
                                &ring.focus.decline,
                                false,
                            )
                            .key_context(DECLINE_CONTEXT)
                            .on_click(move |_, window, cx| {
                                decline_by_click(&decline_click, window, cx)
                            }),
                        ),
                );
            if i > 0 {
                row = row.border_t_1().border_color(color::HAIRLINE);
            }
            row
        });
        div()
            .id(self.id)
            .landmark(Role::Region, self.region)
            .w_full()
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::OK_BORDER)
            .rounded(radius::XL)
            .children(rows)
    }
}

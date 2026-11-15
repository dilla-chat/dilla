//! `FactorRow` (packages/ui/src/FactorRow): one sign-in factor in Settings → Sign-in, a setting row (56px, 10px 16px)
//! whose title (`--text-sm`, 600) sits over its detail (`--text-xs`, `--fg-2`) with the action on the right. The rows of
//! one list draw one `--r-xl` group with hairlines between. An unavailable action stays focusable, does nothing, and its
//! reason (`--text-xs`, `--fg-strong`) is drawn under the detail.

use gpui::prelude::FluentBuilder;
use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, Styled, Window, div, px,
};

use crate::text::line;
use crate::tokens::{color, font, radius, text};

/// `min-height: 3.5rem`.
const MIN_HEIGHT: f32 = 56.0;
/// `padding: 10px 16px`.
const PAD_Y: f32 = 10.0;
const PAD_X: f32 = 16.0;
/// `gap: 10px 14px`.
const GAP: f32 = 14.0;
/// `line-height: 1.4`.
const LEADING: f32 = 1.4;

/// Where a row sits in its list: its corners and its shared edge follow.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Place {
    Only,
    First,
    Middle,
    Last,
}

impl Place {
    pub fn of(index: usize, n: usize) -> Self {
        match (index, n) {
            (_, 0 | 1) => Self::Only,
            (0, _) => Self::First,
            (i, n) if i + 1 == n => Self::Last,
            _ => Self::Middle,
        }
    }
}

#[derive(IntoElement)]
pub struct FactorRow {
    id: ElementId,
    title: SharedString,
    detail: SharedString,
    reason: Option<SharedString>,
    action: Option<AnyElement>,
    place: Place,
}

impl FactorRow {
    pub fn new(
        id: impl Into<ElementId>,
        title: impl Into<SharedString>,
        detail: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            title: title.into(),
            detail: detail.into(),
            reason: None,
            action: None,
            place: Place::Only,
        }
    }

    /// The action on the right (a `Button`); the host makes it do nothing when [`Self::reason`] is set.
    pub fn action(mut self, action: impl IntoElement) -> Self {
        self.action = Some(action.into_any_element());
        self
    }

    /// Why the action is unavailable, drawn under the detail.
    pub fn reason(mut self, reason: impl Into<SharedString>) -> Self {
        self.reason = Some(reason.into());
        self
    }

    pub fn place(mut self, place: Place) -> Self {
        self.place = place;
        self
    }
}

impl RenderOnce for FactorRow {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let sm = f32::from(text::SM);
        let xs = f32::from(text::XS);
        let (top, bottom) = match self.place {
            Place::Only => (true, true),
            Place::First => (true, false),
            Place::Middle => (false, false),
            Place::Last => (false, true),
        };
        div()
            .id(self.id)
            .min_h(px(MIN_HEIGHT))
            .px(px(PAD_X))
            .py(px(PAD_Y))
            .flex()
            .items_center()
            .gap(px(GAP))
            .border_1()
            .border_color(color::HAIRLINE)
            // `.d-factor-row + .d-factor-row { margin-top: -1px }`: the rows share their edge.
            .when(!top, |d| d.mt(px(-1.0)))
            .when(top, |d| d.rounded_t(radius::XL))
            .when(bottom, |d| d.rounded_b(radius::XL))
            .child(
                div()
                    .flex_1()
                    .min_w_0()
                    .flex()
                    .flex_col()
                    .child(
                        line(self.title, text::SM, sm * LEADING, color::FG)
                            .weight(font::WEIGHT_LABEL),
                    )
                    .child(line(self.detail, text::XS, xs * LEADING, color::FG_2))
                    .children(
                        self.reason
                            .map(|r| line(r, text::XS, xs * LEADING, color::FG_STRONG)),
                    ),
            )
            .children(self.action)
    }
}

//! `SafetyCompare`'s body (packages/ui/src/SafetyCompare, SafetyCompare.css): the contact and its state, the first-seen
//! line, the instructions, the public key behind a disclosure, the twelve groups of five digits in six columns, the
//! loading or error line, the QR code when shown, and the actions: Copy, show/hide QR and the "all 12 blocks match"
//! checkbox. The dialog around it (the shared `Dialog`) holds the footer's approval; this body holds no state.
//!
//! Automation ids: `safety-group-<i>` (the twelve groups), `safety-public-umk`, `safety-details`, `safety-copy`,
//! `safety-qr-toggle`, `safety-qr`, `safety-confirm`.

use gpui::{
    App, ClickEvent, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::a11y::{Accessible, Role};
use crate::components::button::Button;
use crate::components::qr_code::QrCode;
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::focus_ring;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

type Handler = Box<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

/// `.d-safety-compare { gap: .75rem }`.
const GAP: f32 = 12.0;
/// `.d-safety-compare__groups { gap: .5rem .75rem }`.
const GROUP_ROW_GAP: f32 = 8.0;
const GROUP_COL_GAP: f32 = 12.0;
/// The QR's side (`size={256}`).
const QR_SIDE: f32 = 256.0;
/// `.d-safety-compare__confirm input { min-width: 24px; min-height: 24px }`.
const CHECK: f32 = 24.0;

#[derive(IntoElement)]
pub struct SafetyCompareBody {
    identity: SharedString,
    first_seen: Option<SharedString>,
    instructions: SharedString,
    details: Option<(SharedString, SharedString, bool, Handler)>,
    groups: Vec<String>,
    loading: Option<SharedString>,
    error: Option<SharedString>,
    qr: Option<(String, SharedString)>,
    copy: (SharedString, Handler),
    qr_toggle: Option<(SharedString, bool, Handler)>,
    confirm: (SharedString, bool, Handler),
}

impl SafetyCompareBody {
    /// `identity` is `"{name} · {state}"`; `copy` the Copy button, `confirm` the checkbox (its label and whether it is
    /// checked).
    pub fn new(
        identity: impl Into<SharedString>,
        instructions: impl Into<SharedString>,
        copy: (
            SharedString,
            impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
        ),
        confirm: (
            SharedString,
            bool,
            impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
        ),
    ) -> Self {
        Self {
            identity: identity.into(),
            first_seen: None,
            instructions: instructions.into(),
            details: None,
            groups: Vec::new(),
            loading: None,
            error: None,
            qr: None,
            copy: (copy.0, Box::new(copy.1)),
            qr_toggle: None,
            confirm: (confirm.0, confirm.1, Box::new(confirm.2)),
        }
    }

    pub fn first_seen(mut self, text_: impl Into<SharedString>) -> Self {
        self.first_seen = Some(text_.into());
        self
    }

    /// The disclosure the exact public key waits behind: its summary, the key, whether it is open, and its toggle.
    pub fn details(
        mut self,
        summary: impl Into<SharedString>,
        key: impl Into<SharedString>,
        open: bool,
        on_toggle: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.details = Some((summary.into(), key.into(), open, Box::new(on_toggle)));
        self
    }

    pub fn groups(mut self, groups: Vec<String>) -> Self {
        self.groups = groups;
        self
    }

    /// Shown in the groups' place while the number is worked out.
    pub fn loading(mut self, text_: impl Into<SharedString>) -> Self {
        self.loading = Some(text_.into());
        self
    }

    pub fn error(mut self, text_: impl Into<SharedString>) -> Self {
        self.error = Some(text_.into());
        self
    }

    /// The QR code of `value`, shown, with its text alternative.
    pub fn qr(mut self, value: impl Into<String>, label: impl Into<SharedString>) -> Self {
        self.qr = Some((value.into(), label.into()));
        self
    }

    /// The show/hide QR toggle (its label and whether the QR is shown).
    pub fn qr_toggle(
        mut self,
        label: impl Into<SharedString>,
        pressed: bool,
        on_click: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.qr_toggle = Some((label.into(), pressed, Box::new(on_click)));
        self
    }
}

fn para(text_: SharedString, colour: gpui::Hsla) -> gpui::Div {
    div()
        .text_size(text::SM)
        .line_height(px(line_height(text::SM, 1.5)))
        .text_color(colour)
        .child(text_)
}

impl RenderOnce for SafetyCompareBody {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let lh = line_height(text::SM, 1.5);
        let mut body = div()
            .id("safety-compare")
            .flex()
            .flex_col()
            .gap(px(GAP))
            .min_w_0()
            .child(line(self.identity, text::SM, lh, color::FG).weight(font::WEIGHT_LABEL));
        if let Some(first) = self.first_seen {
            body = body.child(para(first, color::FG));
        }
        body = body.child(para(self.instructions, color::FG));
        if let Some((summary, key, open, toggle)) = self.details {
            let chevron = if open {
                IconName::ChevronDown
            } else {
                IconName::ChevronRight
            };
            let mut details = div().flex().flex_col().child(
                div()
                    .id("safety-details")
                    .a11y(Role::Button, summary.clone())
                    .aria_expanded(open)
                    .flex()
                    .items_center()
                    .min_h(px(CHECK))
                    .gap(px(6.0))
                    .tab_index(0)
                    .cursor_pointer()
                    .focus_visible(|s| s.shadow(focus_ring()))
                    .on_click(toggle)
                    .child(Icon::new(chevron).size(px(12.0)).color(color::FG_2))
                    .child(line(summary, text::SM, lh, color::FG_2)),
            );
            if open {
                details = details.child(
                    div()
                        .id("safety-public-umk")
                        .mt(px(6.0))
                        .text_size(text::SM)
                        .line_height(px(lh))
                        .text_color(color::FG)
                        .child(key),
                );
            }
            body = body.child(details);
        }
        if !self.groups.is_empty() {
            let rows: Vec<_> = self
                .groups
                .chunks(6)
                .enumerate()
                .map(|(r, chunk)| {
                    div()
                        .flex()
                        .gap(px(GROUP_COL_GAP))
                        .children(chunk.iter().enumerate().map(move |(c, g)| {
                            div().id(("safety-group", r * 6 + c)).child(line(
                                g.clone(),
                                text::SM,
                                lh,
                                color::FG,
                            ))
                        }))
                })
                .collect();
            body = body.child(
                div()
                    .flex()
                    .flex_col()
                    .gap(px(GROUP_ROW_GAP))
                    .children(rows),
            );
        } else if self.error.is_none()
            && let Some(loading) = self.loading
        {
            body = body.child(para(loading, color::FG_2));
        }
        if let Some((value, _label)) = self.qr {
            body = body.child(
                div()
                    .flex()
                    .child(QrCode::new("safety-qr", value).size(px(QR_SIDE))),
            );
        }
        if let Some(error) = self.error {
            body = body.child(
                div()
                    .pl(px(8.0))
                    .border_l(px(3.0))
                    .border_color(color::DANGER)
                    .child(para(error, color::FG)),
            );
        }
        let (copy_label, copy) = self.copy;
        let mut actions = div()
            .flex()
            .flex_wrap()
            .items_center()
            .gap(px(GAP))
            .child(Button::new("safety-copy", copy_label).on_click(copy));
        if let Some((label, pressed, toggle)) = self.qr_toggle {
            actions = actions.child(
                Button::new("safety-qr-toggle", label)
                    .pressed(pressed)
                    .on_click(toggle),
            );
        }
        let (confirm_label, checked, on_confirm) = self.confirm;
        let mut box_ = div()
            .size(px(CHECK))
            .flex()
            .items_center()
            .justify_center()
            .rounded(radius::SM)
            .border_1()
            .border_color(color::CONTROL_EDGE);
        if checked {
            box_ = box_.bg(color::ACCENT).border_color(color::ACCENT).child(
                Icon::new(IconName::Check)
                    .size(px(14.0))
                    .color(color::ACCENT_INK),
            );
        }
        actions = actions.child(
            div()
                .id("safety-confirm")
                .a11y(Role::CheckBox, confirm_label.clone())
                .toggled(checked)
                .flex()
                .items_center()
                .gap(px(8.0))
                .tab_index(0)
                .cursor_pointer()
                .focus_visible(|s| s.shadow(focus_ring()))
                .on_click(on_confirm)
                .child(box_)
                .child(line(confirm_label, text::SM, lh, color::FG)),
        );
        body.child(actions)
    }
}

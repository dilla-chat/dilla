//! `RecoveryKey` (packages/ui/src/RecoveryKey): the recovery key as text, one cell per group of four in four columns
//! (12px in, 6px apart, on `--surface-2` in the `--control-edge-field` edge, radius `--r-lg`; each group 13px/600 with
//! 0.06em tracking, centred, 4px above and below), 10px over the row of actions (34px tall, sharing the row 8px apart,
//! the `--control-edge-field` edge, radius `--r-seg`, a 13px icon 6px before the 12px label; the accent edge under the
//! pointer), 14px over the 16px check box (`--control-edge-key` edge, radius `--r-sm`; ticked: the accent with an
//! `--accent-ink` check) and its 12px `--fg-strong` label 10px after it, 8px over the step's action.
//!
//! There is no copy control (protocol/03 "Recovery": the person writes the key down, prints it or saves it as a file).
//! The groups are passed in by the host, which holds the key in a zeroizing buffer and drops it once the step is left.
//!
//! The parts are separate so the host can wrap each pressable one for automation: [`KeyGroups`], [`KeyAction`],
//! [`KeyAck`], laid out by [`RecoveryKeyLayout`].

use gpui::{
    AnyElement, App, ClickEvent, ElementId, FocusHandle, InteractiveElement, IntoElement,
    ParentElement, RenderOnce, SharedString, Styled, Window, div, prelude::FluentBuilder, px,
};

use super::frame::{CARD_INNER_W, Click, pressable, pressable_quiet};
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};
use crate::{Icon, IconName, metrics};

/// The columns of the grid.
pub const COLUMNS: usize = 4;
/// The gap between the two actions.
const ACTIONS_GAP: f32 = 8.0;

/// The key's grid.
#[derive(IntoElement)]
pub struct KeyGroups {
    groups: Vec<SharedString>,
}

impl KeyGroups {
    pub fn new(groups: Vec<SharedString>) -> Self {
        Self { groups }
    }
}

impl RenderOnce for KeyGroups {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        let group_h = line_height(text::INPUT, 1.5);
        let rows = self.groups.chunks(COLUMNS).map(|row| {
            let mut cells: Vec<AnyElement> = row
                .iter()
                .map(|g| {
                    div()
                        .flex_1()
                        .py(px(4.0))
                        .flex()
                        .justify_center()
                        .child(
                            line(g.clone(), text::INPUT, group_h, color::FG)
                                .weight(font::WEIGHT_LABEL)
                                .tracking(metrics::TRACKING_TAG),
                        )
                        .into_any_element()
                })
                .collect();
            while cells.len() < COLUMNS {
                cells.push(div().flex_1().into_any_element());
            }
            div().flex().gap(px(6.0)).children(cells)
        });
        div()
            .w(px(CARD_INNER_W))
            .p(px(12.0))
            .flex()
            .flex_col()
            .gap(px(6.0))
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::CONTROL_EDGE_FIELD)
            .rounded(radius::LG)
            .children(rows)
    }
}

/// One of the two actions under the grid (Print, Save as file): half the row.
#[derive(IntoElement)]
pub struct KeyAction {
    id: ElementId,
    icon: IconName,
    label: SharedString,
    focus: Option<FocusHandle>,
    on_click: Option<Click>,
}

impl KeyAction {
    pub fn new(id: impl Into<ElementId>, icon: IconName, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            icon,
            label: label.into(),
            focus: None,
            on_click: None,
        }
    }

    pub fn focus(mut self, handle: &FocusHandle) -> Self {
        self.focus = Some(handle.clone());
        self
    }

    pub fn on_click(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_click = Some(Box::new(f));
        self
    }
}

impl RenderOnce for KeyAction {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        let base = div()
            .id(self.id)
            .w(px((CARD_INNER_W - ACTIONS_GAP) / 2.0))
            .h(px(34.0))
            .px(px(12.0))
            .flex()
            .items_center()
            .justify_center()
            .gap(px(6.0))
            .border_1()
            .border_color(color::CONTROL_EDGE_FIELD)
            .rounded(radius::SEG)
            .hover(|s| s.border_color(color::ACCENT))
            .child(Icon::new(self.icon).size(px(13.0)).color(color::FG))
            .child(line(
                self.label,
                text::UI,
                line_height(text::UI, 1.5),
                color::FG,
            ));
        pressable(base, self.focus.as_ref(), self.on_click)
    }
}

/// The acknowledgement: the 16px box and its label; the whole row is its target.
#[derive(IntoElement)]
pub struct KeyAck {
    id: ElementId,
    label: SharedString,
    on: bool,
    focus: Option<FocusHandle>,
    on_click: Option<Click>,
}

impl KeyAck {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>, on: bool) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            on,
            focus: None,
            on_click: None,
        }
    }

    pub fn focus(mut self, handle: &FocusHandle) -> Self {
        self.focus = Some(handle.clone());
        self
    }

    pub fn on_click(mut self, f: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static) -> Self {
        self.on_click = Some(Box::new(f));
        self
    }
}

impl RenderOnce for KeyAck {
    fn render(self, window: &mut Window, _: &mut App) -> impl IntoElement {
        let on = self.on;
        // The focus ring is drawn on the tick (RecoveryKey.css `:focus-visible + __tick`), not round the row.
        let focused = self.focus.as_ref().is_some_and(|h| h.is_focused(window));
        let tick = div()
            .when(focused, |d| d.shadow(metrics::focus_ring()))
            .size(px(18.0))
            .flex_none()
            .flex()
            .items_center()
            .justify_center()
            .border_1()
            .rounded(radius::SM)
            .border_color(if on {
                color::ACCENT
            } else {
                color::CONTROL_EDGE_KEY
            })
            .when(on, |d| {
                d.bg(color::ACCENT).child(
                    Icon::new(IconName::Check)
                        .size(px(11.0))
                        .color(color::ACCENT_INK),
                )
            });
        let base = div()
            .id(self.id)
            .w(px(CARD_INNER_W))
            .flex()
            .items_center()
            .gap(px(10.0))
            .child(tick)
            .child(line(
                self.label,
                text::UI,
                line_height(text::UI, 1.5),
                color::FG_STRONG,
            ));
        pressable_quiet(base, self.focus.as_ref(), self.on_click)
    }
}

/// The key step's block: the grid, 10px, the actions, 14px, the acknowledgement, 8px to the step's action (the frame's
/// 14px gap less the block's -14px margin, then 8).
#[derive(IntoElement)]
pub struct RecoveryKeyLayout {
    groups: AnyElement,
    actions: [AnyElement; 2],
    ack: AnyElement,
}

impl RecoveryKeyLayout {
    pub fn new(
        groups: impl IntoElement,
        print: impl IntoElement,
        save: impl IntoElement,
        ack: impl IntoElement,
    ) -> Self {
        Self {
            groups: groups.into_any_element(),
            actions: [print.into_any_element(), save.into_any_element()],
            ack: ack.into_any_element(),
        }
    }
}

impl RenderOnce for RecoveryKeyLayout {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        let [print, save] = self.actions;
        div()
            .flex()
            .flex_col()
            .mb(px(-14.0))
            .child(div().mb(px(10.0)).child(self.groups))
            .child(
                div()
                    .flex()
                    .gap(px(ACTIONS_GAP))
                    .mb(px(14.0))
                    .child(print)
                    .child(save),
            )
            .child(div().mb(px(8.0)).child(self.ack))
    }
}

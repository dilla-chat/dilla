//! `CheckList` (packages/ui/src/CheckList): a labelled list of check boxes (a member's roles, a channel's permissions),
//! rows 32px apart with a hairline above, an optional detail line, and a role colour dot. A row whose save is in flight is
//! dimmed and ignores another toggle.

use std::rc::Rc;

use gpui::{
    App, ElementId, Hsla, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px, rgb,
};

use crate::a11y::{Accessible, Role};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING, OPACITY_MUTED, focus_ring};
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

#[derive(Clone, Debug, PartialEq)]
pub struct CheckListItem {
    pub id: SharedString,
    pub label: SharedString,
    pub detail: Option<SharedString>,
    pub checked: bool,
    pub disabled: bool,
    /// A role colour as `0xRRGGBB`; zero or none draws no dot (`item.color ? ... : null`).
    pub color: Option<u32>,
}

impl CheckListItem {
    pub fn new(id: impl Into<SharedString>, label: impl Into<SharedString>, checked: bool) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            detail: None,
            checked,
            disabled: false,
            color: None,
        }
    }

    pub fn detail(mut self, detail: impl Into<SharedString>) -> Self {
        self.detail = Some(detail.into());
        self
    }

    pub fn disabled(mut self, disabled: bool) -> Self {
        self.disabled = disabled;
        self
    }

    pub fn color(mut self, color: u32) -> Self {
        self.color = Some(color);
        self
    }
}

/// The 8px role colour dot (`RoleDot`): decorative, filled from data.
pub fn role_dot(color: u32) -> gpui::Div {
    div()
        .flex_none()
        .size(px(8.0))
        .rounded(radius::PILL)
        .bg(Hsla::from(rgb(color)))
}

/// Whether a toggle of `item` goes through: not while disabled, not while its own save is in flight.
pub fn can_toggle(item: &CheckListItem, busy_id: Option<&str>) -> bool {
    !item.disabled && busy_id != Some(item.id.as_ref())
}

type ToggleHandler = Rc<dyn Fn(&str, bool, &mut Window, &mut App) + 'static>;

#[derive(IntoElement)]
pub struct CheckList {
    id: ElementId,
    label: SharedString,
    items: Vec<CheckListItem>,
    busy_id: Option<SharedString>,
    empty_label: SharedString,
    on_toggle: Option<ToggleHandler>,
}

impl CheckList {
    pub fn new(
        id: impl Into<ElementId>,
        label: impl Into<SharedString>,
        items: Vec<CheckListItem>,
        empty_label: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            items,
            busy_id: None,
            empty_label: empty_label.into(),
            on_toggle: None,
        }
    }

    /// The item whose save is in flight.
    pub fn busy(mut self, id: Option<SharedString>) -> Self {
        self.busy_id = id;
        self
    }

    pub fn on_toggle(
        mut self,
        handler: impl Fn(&str, bool, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_toggle = Some(Rc::new(handler));
        self
    }
}

/// The check box: 14px (the native box inside its 24px hit area), the accent fill and the tick when checked.
fn check_box(checked: bool, disabled: bool) -> impl IntoElement {
    let edge = if checked {
        color::ACCENT
    } else {
        color::CONTROL_EDGE_KEY
    };
    let mut b = div()
        .flex()
        .flex_none()
        .items_center()
        .justify_center()
        .size(px(14.0))
        .rounded(radius::TAG)
        .border_1()
        .border_color(edge);
    if checked {
        b = b.bg(color::ACCENT).child(
            Icon::new(IconName::Check)
                .size(px(10.0))
                .color(color::ACCENT_INK),
        );
    }
    div()
        .flex()
        .flex_none()
        .items_center()
        .justify_center()
        .size(px(24.0))
        .opacity(if disabled { OPACITY_MUTED } else { 1.0 })
        .child(b)
}

impl RenderOnce for CheckList {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let mut root = div().id(self.id.clone()).flex().flex_col().min_w_0().child(
            div().pb(px(6.0)).child(
                line(
                    self.label.to_uppercase(),
                    text::MICRO,
                    line_height(text::MICRO, 1.5),
                    color::FG_3,
                )
                .weight(font::WEIGHT_LABEL)
                .tracking(LABEL_TRACKING),
            ),
        );
        if self.items.is_empty() {
            return root.child(line(
                self.empty_label,
                text::SM,
                line_height(text::SM, 1.5),
                color::FG_3,
            ));
        }
        for item in self.items {
            let busy = self.busy_id.as_deref() == Some(item.id.as_ref());
            let allowed = can_toggle(&item, self.busy_id.as_deref());
            let mut label = div()
                .flex()
                .flex_1()
                .items_center()
                .gap(px(8.0))
                .min_w_0()
                .min_h(px(24.0));
            if let Some(c) = item.color.filter(|c| *c != 0) {
                label = label.child(role_dot(c));
            }
            label = label.child(line(
                item.label.clone(),
                text::BASE,
                line_height(text::BASE, 1.5),
                if item.disabled {
                    color::FG_2
                } else {
                    color::FG
                },
            ));
            let mut row = div()
                .id((self.id.clone(), format!("row-{}", item.id)))
                .a11y(Role::CheckBox, item.label.clone())
                .toggled(item.checked)
                .flex()
                .flex_wrap()
                .items_center()
                .gap_x(px(8.0))
                .min_h(px(32.0))
                .min_w_0()
                .border_t_1()
                .border_color(color::HAIRLINE)
                .tab_index(0)
                .focus_visible(|s| s.shadow(focus_ring()))
                .child(check_box(item.checked, item.disabled))
                .child(label);
            if busy {
                row = row.opacity(OPACITY_MUTED);
            }
            if let Some(detail) = item.detail {
                row = row.child(div().w_full().pl(px(32.0)).pb(px(4.0)).child(line(
                    detail,
                    text::SM,
                    line_height(text::SM, 1.5),
                    color::FG_3,
                )));
            }
            row = if allowed {
                row.cursor_pointer()
            } else {
                row.cursor_not_allowed()
            };
            if let Some(handler) = self.on_toggle.clone() {
                let (id, next) = (item.id.clone(), !item.checked);
                let activate = move |window: &mut Window, cx: &mut App| {
                    if allowed {
                        handler(&id, next, window, cx);
                    }
                };
                row = row.on_click(move |_, window, cx| activate(window, cx));
            }
            root = root.child(row);
        }
        root
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_disabled_or_busy_row_does_not_toggle() {
        let item = CheckListItem::new("a", "Admin", false);
        assert!(can_toggle(&item, None));
        assert!(!can_toggle(&item, Some("a")));
        assert!(can_toggle(&item, Some("b")));
        assert!(!can_toggle(&item.clone().disabled(true), None));
    }
}

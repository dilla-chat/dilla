//! The settings `nav` (packages/ui/src/SettingsNav): 34px items, radius 6, 12.5px in `--fg-2`; the current one `--fg` on
//! `--surface-hi` with the channel row's 2px accent bar; group headings are 10px uppercase micro labels. One tab stop (the
//! last focused item, else the current one, else the first); Up and Down move focus without selecting, Home and End jump,
//! Enter and Space choose.

use std::rc::Rc;

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING_WIDE, focus_ring};
use crate::roving::{Roving, focus_handles};
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

#[derive(Clone, Debug, PartialEq)]
pub struct NavItem {
    pub id: SharedString,
    pub label: SharedString,
    pub icon: Option<IconName>,
    pub group: Option<SharedString>,
}

impl NavItem {
    pub fn new(id: impl Into<SharedString>, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            icon: None,
            group: None,
        }
    }

    pub fn icon(mut self, icon: IconName) -> Self {
        self.icon = Some(icon);
        self
    }

    pub fn group(mut self, group: impl Into<SharedString>) -> Self {
        self.group = Some(group.into());
        self
    }
}

/// The headings to draw: an item's group is shown above it when it differs from the item before's.
pub fn group_headings(items: &[NavItem]) -> Vec<Option<SharedString>> {
    let mut previous: Option<&SharedString> = None;
    items
        .iter()
        .map(|item| {
            let show = item.group.is_some() && item.group.as_ref() != previous;
            previous = item.group.as_ref();
            if show { item.group.clone() } else { None }
        })
        .collect()
}

type SelectHandler = Rc<dyn Fn(&str, &mut Window, &mut App) + 'static>;

#[derive(IntoElement)]
pub struct SettingsNav {
    id: ElementId,
    items: Vec<NavItem>,
    active: SharedString,
    on_select: Option<SelectHandler>,
}

impl SettingsNav {
    pub fn new(
        id: impl Into<ElementId>,
        items: Vec<NavItem>,
        active: impl Into<SharedString>,
    ) -> Self {
        Self {
            id: id.into(),
            items,
            active: active.into(),
            on_select: None,
        }
    }

    pub fn on_select(mut self, handler: impl Fn(&str, &mut Window, &mut App) + 'static) -> Self {
        self.on_select = Some(Rc::new(handler));
        self
    }
}

impl RenderOnce for SettingsNav {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let count = self.items.len();
        let handles = focus_handles((self.id.clone(), "focus"), count, window, cx);
        let active = self.items.iter().position(|i| i.id == self.active);
        // The tab stop is the item that last had focus, else the current one.
        let last_focused = handles.iter().position(|h| h.contains_focused(window, cx));
        let stop = Roving::stop(last_focused, active, count);
        let glyphs = self.items.iter().any(|i| i.icon.is_some());
        let headings = group_headings(&self.items);
        let key_handles = handles.clone();
        let mut list = div()
            .id(self.id.clone())
            .flex()
            .flex_col()
            .gap(px(2.0))
            .on_key_down(move |event, window, cx| {
                let Some(from) = key_handles.iter().position(|h| h.is_focused(window)) else {
                    return;
                };
                if let Some(next) = Roving::List.next(&event.keystroke, from, key_handles.len()) {
                    cx.stop_propagation();
                    key_handles[next].focus(window, cx);
                }
            });
        for (i, item) in self.items.iter().enumerate() {
            if let Some(heading) = &headings[i] {
                list = list.child(
                    div().px(px(10.0)).pt(px(14.0)).pb(px(6.0)).child(
                        line(
                            heading.to_uppercase(),
                            text::MICRO,
                            line_height(text::MICRO, 1.5),
                            color::FG_3,
                        )
                        .weight(font::WEIGHT_LABEL)
                        .tracking(LABEL_TRACKING_WIDE),
                    ),
                );
            }
            let current = item.id == self.active;
            let mut row = div()
                .id((self.id.clone(), format!("item-{}", item.id)))
                // The handle's own flag decides the tab stop of a tracked element.
                .track_focus(&handles[i].clone().tab_stop(i == stop))
                .relative()
                .flex()
                .items_center()
                .gap(px(10.0))
                .w_full()
                .h(px(34.0))
                .px(px(10.0))
                .rounded(radius::MD)
                .cursor_pointer()
                .focus_visible(|s| s.shadow(focus_ring()));
            row = if current {
                row.bg(color::SURFACE_HI)
            } else {
                row.hover(|s| s.bg(color::HOVER))
            };
            if current {
                // The channel row's bar: 2px at the left edge, 8px in from the top and bottom.
                row = row.child(
                    div()
                        .absolute()
                        .left_0()
                        .top(px(8.0))
                        .bottom(px(8.0))
                        .w(px(2.0))
                        .rounded(px(2.0))
                        .bg(color::ACCENT),
                );
            }
            if glyphs {
                row = row.child(
                    div()
                        .flex()
                        .flex_none()
                        .items_center()
                        .justify_center()
                        .size(px(15.0))
                        .children(item.icon.map(|icon| {
                            Icon::new(icon).size(px(15.0)).color(if current {
                                color::FG
                            } else {
                                color::FG_2
                            })
                        })),
                );
            }
            row = row.child(
                div().min_w_0().child(
                    line(
                        item.label.clone(),
                        text::SM,
                        line_height(text::SM, 1.5),
                        if current { color::FG } else { color::FG_2 },
                    )
                    .ellipsis(),
                ),
            );
            if let Some(handler) = self.on_select.clone() {
                let id = item.id.clone();
                row = row.on_click(move |_, window, cx| handler(&id, window, cx));
            }
            list = list.child(row);
        }
        list
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_group_heading_shows_where_the_group_changes() {
        let items = vec![
            NavItem::new("profile", "Profile").group("Account"),
            NavItem::new("devices", "Devices").group("Account"),
            NavItem::new("voice", "Voice").group("App"),
            NavItem::new("about", "About"),
            NavItem::new("keys", "Keyboard").group("App"),
        ];
        let shown: Vec<_> = group_headings(&items)
            .into_iter()
            .map(|h| h.map(|s| s.to_string()))
            .collect();
        assert_eq!(
            shown,
            [
                Some("Account".to_owned()),
                None,
                Some("App".to_owned()),
                None,
                Some("App".to_owned())
            ]
        );
    }
}

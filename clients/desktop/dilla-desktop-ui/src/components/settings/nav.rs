//! The settings `nav` (packages/ui/src/SettingsNav): 34px items, radius 6, 12.5px in `--fg-2`; the current one `--fg` on
//! `--surface-hi` with the channel row's 2px accent bar; group headings are 10px uppercase micro labels. One tab stop (the
//! last focused item, else the current one, else the first); Up and Down move focus without selecting, Home and End jump,
//! Enter and Space choose.

use std::rc::Rc;

use std::collections::HashMap;

use gpui::{
    App, ElementId, Global, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    ScrollHandle, SharedString, StatefulInteractiveElement, Styled, Window, WindowId, canvas, div,
    point, px,
};

use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING_WIDE, focus_ring};
use crate::roving::{Roving, focus_handles};
use crate::text::{line, line_height, wrap_mono};
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

/// The scroll handle of each window's settings side column (the frame registers it): the nav scrolls its current item
/// into it, as the web's `scrollIntoView({ block: 'nearest' })` does.
#[derive(Default)]
pub(crate) struct SideScroll(HashMap<WindowId, ScrollHandle>);

impl Global for SideScroll {}

pub(crate) fn register_side(handle: &ScrollHandle, window: &Window, cx: &mut App) {
    // Closed windows' handles go (review m7): only the open windows keep an entry.
    let open: Vec<WindowId> = cx.windows().iter().map(|w| w.window_id()).collect();
    let sides = &mut cx.default_global::<SideScroll>().0;
    sides.retain(|id, _| open.contains(id));
    sides.insert(window.window_handle().window_id(), handle.clone());
}

/// The nav's scroll-into-view of its current item: which item, whether it is in view, and how many frames it took.
#[derive(Default)]
struct ScrolledTo {
    id: Option<SharedString>,
    done: bool,
    tries: u8,
}

impl ScrolledTo {
    /// The frames a newly current item may take to come into view.
    const TRIES: u8 = 4;

    /// Whether to try (again) to bring `id` into view: always for a newly current item, then until it is in view or the
    /// tries run out.
    fn try_for(&mut self, id: &SharedString) -> bool {
        if self.id.as_ref() != Some(id) {
            *self = Self {
                id: Some(id.clone()),
                done: false,
                tries: 0,
            };
        }
        if self.done || self.tries >= Self::TRIES {
            return false;
        }
        self.tries += 1;
        true
    }
}

/// Asks for another frame from inside a draw: `Window::refresh` is a no-op while the window draws, so it runs once this
/// frame is done.
fn redraw(window: &mut Window, cx: &mut App) {
    window.defer(cx, |window, _| window.refresh());
}

/// The scroll offset that brings `item` (top, bottom) fully into `port` (top, bottom) by the least movement, from
/// `offset` (negative down the column), or `None` when it is already in view.
pub fn nearest_offset(offset: f32, item: (f32, f32), port: (f32, f32)) -> Option<f32> {
    if item.1 - item.0 > port.1 - port.0 || item.0 < port.0 {
        (item.0 != port.0).then_some(offset + (port.0 - item.0))
    } else if item.1 > port.1 {
        Some(offset - (item.1 - port.1))
    } else {
        None
    }
}

/// The side column's inner width less a heading's 10px sides (`.d-settings-frame__side` 240px, 14px padding, 1px edge).
fn heading_width() -> f32 {
    240.0 - 2.0 * 14.0 - 1.0 - 2.0 * 10.0
}

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
        // The item the side column last scrolled to: the web scrolls on a route change only, so a column the person
        // wheels away from the current item stays where they left it (review I1).
        let scrolled_to = window.use_keyed_state((self.id.clone(), "scrolled-to"), cx, |_, _| {
            ScrolledTo::default()
        });
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
                // A long heading (a server's name) wraps in the column, as the web's does.
                let advance =
                    f32::from(text::MICRO) * (crate::text::MONO_ADVANCE + LABEL_TRACKING_WIDE);
                let columns = ((heading_width() / advance).floor() as usize).max(1);
                list = list.child(
                    div()
                        .flex()
                        .flex_col()
                        .px(px(10.0))
                        .pt(px(14.0))
                        .pb(px(6.0))
                        .children(wrap_mono(&heading.to_uppercase(), columns).into_iter().map(
                            |l| {
                                line(l, text::MICRO, line_height(text::MICRO, 1.5), color::FG_3)
                                    .weight(font::WEIGHT_LABEL)
                                    .tracking(LABEL_TRACKING_WIDE)
                            },
                        )),
                );
            }
            let current = item.id == self.active;
            let selector = format!("nav-item-{}", item.id);
            let mut row = div()
                .id((self.id.clone(), format!("item-{}", item.id)))
                .debug_selector(move || selector.clone())
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
                // When it becomes current, scrolls the side column the least that shows this item whole
                // (`scrollIntoView({ block: 'nearest' })` on the route change); afterwards the column is the person's.
                let (scrolled_to, current_id) = (scrolled_to.clone(), item.id.clone());
                row = row.child(
                    canvas(
                        move |bounds, window, cx| {
                            // One frame per try: a column that cannot show the item (no size, no room to scroll) stops
                            // asking after a few.
                            if !scrolled_to.update(cx, |s, _| s.try_for(&current_id)) {
                                return;
                            }
                            let Some(side) = cx
                                .try_global::<SideScroll>()
                                .and_then(|s| s.0.get(&window.window_handle().window_id()))
                                .cloned()
                            else {
                                return;
                            };
                            let port = side.bounds();
                            if port.size.height <= px(0.0) {
                                // The column is not laid out yet (its first frame): try again on the next one.
                                redraw(window, cx);
                                return;
                            }
                            let offset = side.offset();
                            if let Some(y) = nearest_offset(
                                f32::from(offset.y),
                                (f32::from(bounds.top()), f32::from(bounds.bottom())),
                                (f32::from(port.top()), f32::from(port.bottom())),
                            ) {
                                // The offset lands on a later frame; this item counts as scrolled to once a frame
                                // finds it in view.
                                side.set_offset(point(offset.x, px(y)));
                                redraw(window, cx);
                            } else {
                                scrolled_to.update(cx, |s, _| s.done = true);
                            }
                        },
                        |_, _, _, _| {},
                    )
                    .absolute()
                    .inset_0(),
                );
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
    fn the_current_item_scrolls_the_least_into_view() {
        // Below the port: its bottom meets the port's bottom.
        assert_eq!(
            nearest_offset(0.0, (760.0, 794.0), (0.0, 700.0)),
            Some(-94.0)
        );
        // Above the port: its top meets the port's top.
        assert_eq!(
            nearest_offset(-94.0, (-10.0, 24.0), (0.0, 700.0)),
            Some(-84.0)
        );
        // In view: no change.
        assert_eq!(nearest_offset(0.0, (100.0, 134.0), (0.0, 700.0)), None);
    }

    #[test]
    fn a_long_heading_wraps_at_the_column() {
        let advance = f32::from(text::MICRO) * (crate::text::MONO_ADVANCE + LABEL_TRACKING_WIDE);
        let columns = (heading_width() / advance).floor() as usize;
        assert_eq!(
            wrap_mono(
                "MIDGARD CREW OF THE NORTHERN LIGHTS AND THE VERY LONG WINTER NIGHTS SOCIETY",
                columns
            ),
            [
                "MIDGARD CREW OF THE",
                "NORTHERN LIGHTS AND THE",
                "VERY LONG WINTER NIGHTS",
                "SOCIETY"
            ]
        );
    }

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

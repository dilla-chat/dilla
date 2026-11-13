//! `ActionMenu` (packages/ui/src/ActionMenu): a WAI-ARIA menu button and its popup, the prototype's context menu (4px in,
//! `--surface-2` in the `--line-strong` edge, 9px corners, the `--shadow-menu`).
//!
//! `open` is the caller's, so a row can also open the menu as a context menu (`at`, no trigger drawn). Keys:
//! Enter, Space or a click open it from the trigger, Down on its first item, Up on its last; **Shift+F10 and the Menu key
//! open it from the trigger**, and from any element that routes them through [`opens_context_menu`]; inside, the arrows
//! wrap, Home and End jump, Enter or Space choose, Escape closes and refocuses the trigger (or `return_focus`), Tab closes
//! and moves on, and a press outside closes. A check item toggles and keeps the menu open.

use std::rc::Rc;

use gpui::{
    AnyElement, App, ElementId, FocusHandle, InteractiveElement, IntoElement, ParentElement,
    Pixels, Point, RenderOnce, SharedString, StatefulInteractiveElement, Styled, Window, anchored,
    deferred, div, px,
};

use super::dialog::{MENU_CONTEXT, MenuNext, MenuPrevious};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING_WIDE, focus_ring, shadow_menu};
use crate::roving::menu_next;
pub use crate::roving::opens_context_menu;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

#[derive(Clone, Debug, Default, PartialEq)]
pub struct MenuItem {
    pub id: SharedString,
    pub label: SharedString,
    pub danger: bool,
    /// A single-choice item: the tick is drawn when `Some(true)`.
    pub checked: Option<bool>,
    /// A 14px glyph before the label.
    pub icon: Option<IconName>,
    /// An on/off item with the prototype's 14px box; choosing it toggles and keeps the menu open.
    pub check: Option<bool>,
    /// A 1px rule above the item.
    pub sep: bool,
    /// A trailing hint, 10px `--fg-faint` (a shortcut's keys, a room's head count).
    pub hint: Option<SharedString>,
    /// A small uppercase heading above the item.
    pub heading: Option<SharedString>,
    /// A room row of the person menu's "Move to" block (30px, indented).
    pub room: bool,
}

impl MenuItem {
    pub fn new(id: impl Into<SharedString>, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            ..Self::default()
        }
    }

    pub fn danger(mut self) -> Self {
        self.danger = true;
        self
    }

    pub fn icon(mut self, icon: IconName) -> Self {
        self.icon = Some(icon);
        self
    }

    pub fn checked(mut self, checked: bool) -> Self {
        self.checked = Some(checked);
        self
    }

    pub fn check(mut self, on: bool) -> Self {
        self.check = Some(on);
        self
    }

    pub fn sep(mut self) -> Self {
        self.sep = true;
        self
    }

    pub fn hint(mut self, hint: impl Into<SharedString>) -> Self {
        self.hint = Some(hint.into());
        self
    }

    pub fn heading(mut self, heading: impl Into<SharedString>) -> Self {
        self.heading = Some(heading.into());
        self
    }

    pub fn room(mut self) -> Self {
        self.room = true;
        self
    }

    /// Whether choosing the item leaves the menu open.
    pub fn keeps_open(&self) -> bool {
        self.check.is_some()
    }
}

type OpenHandler = Rc<dyn Fn(bool, &mut Window, &mut App) + 'static>;
type SelectHandler = Rc<dyn Fn(&str, &mut Window, &mut App) + 'static>;

struct Memory {
    trigger: FocusHandle,
    was_open: bool,
    start_last: bool,
}

#[derive(IntoElement)]
pub struct ActionMenu {
    id: ElementId,
    label: SharedString,
    items: Vec<MenuItem>,
    open: bool,
    at: Option<Point<Pixels>>,
    icon: Option<IconName>,
    icon_size: Pixels,
    trigger_size: Option<Pixels>,
    no_trigger: bool,
    width: Option<Pixels>,
    return_focus: Option<FocusHandle>,
    head: Option<AnyElement>,
    on_open_change: Option<OpenHandler>,
    on_select: Option<SelectHandler>,
}

impl ActionMenu {
    /// `label` names the trigger (`actions for Astrid`).
    pub fn new(
        id: impl Into<ElementId>,
        label: impl Into<SharedString>,
        items: Vec<MenuItem>,
    ) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            items,
            open: false,
            at: None,
            icon: None,
            icon_size: px(22.0),
            trigger_size: None,
            no_trigger: false,
            width: None,
            return_focus: None,
            head: None,
            on_open_change: None,
            on_select: None,
        }
    }

    pub fn open(mut self, open: bool) -> Self {
        self.open = open;
        self
    }

    /// Where a context menu sits: the pointer (or the row's corner), clamped to the window.
    pub fn at(mut self, at: Point<Pixels>) -> Self {
        self.at = Some(at);
        self
    }

    /// A monoline icon on the trigger in place of the `⋯` glyph.
    pub fn icon(mut self, icon: IconName) -> Self {
        self.icon = Some(icon);
        self
    }

    /// The trigger icon's size (22px unless set; a message toolbar's `⋯` is 14px in its 26px button).
    pub fn icon_size(mut self, size: Pixels) -> Self {
        self.icon_size = size;
        self
    }

    /// A square trigger of `size` with no padding (a list head's button, `.d-channel-list__head-btn`: 26px), in place of
    /// the default 24px minimum with 6px sides.
    pub fn trigger_size(mut self, size: Pixels) -> Self {
        self.trigger_size = Some(size);
        self
    }

    /// No trigger is drawn: the menu exists only while open, at `at` (a context menu).
    pub fn no_trigger(mut self, return_focus: Option<&FocusHandle>) -> Self {
        self.no_trigger = true;
        self.return_focus = return_focus.cloned();
        self
    }

    /// The popup's width (the person menu's 236px, the status menu's 220px).
    pub fn width(mut self, width: Pixels) -> Self {
        self.width = Some(width);
        self
    }

    /// Content above the items inside the popup.
    pub fn head(mut self, head: impl IntoElement) -> Self {
        self.head = Some(head.into_any_element());
        self
    }

    pub fn on_open_change(
        mut self,
        handler: impl Fn(bool, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.on_open_change = Some(Rc::new(handler));
        self
    }

    pub fn on_select(mut self, handler: impl Fn(&str, &mut Window, &mut App) + 'static) -> Self {
        self.on_select = Some(Rc::new(handler));
        self
    }
}

/// The item's height: 32px, a room row 30px.
pub fn item_height(item: &MenuItem) -> f32 {
    if item.room { 30.0 } else { 32.0 }
}

impl RenderOnce for ActionMenu {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let memory = window.use_keyed_state((self.id.clone(), "menu"), cx, |_, cx| Memory {
            trigger: cx.focus_handle().tab_stop(true),
            was_open: false,
            start_last: false,
        });
        let trigger = memory.read(cx).trigger.clone();
        let count = self.items.len();
        let handles = crate::roving::focus_handles((self.id.clone(), "items"), count, window, cx);
        let back = self.return_focus.clone().unwrap_or_else(|| trigger.clone());

        // Opening puts focus on the first item (the last after Up), after this frame.
        let was_open = memory.read(cx).was_open;
        if self.open != was_open {
            let start_last = memory.update(cx, |m, _| {
                m.was_open = self.open;
                std::mem::take(&mut m.start_last)
            });
            if self.open && count > 0 {
                let target = handles[if start_last { count - 1 } else { 0 }].clone();
                window.defer(cx, move |window, cx| target.focus(window, cx));
            }
        }

        let on_open_change = self.on_open_change;
        let on_select = self.on_select;
        let open_with = {
            let on_open_change = on_open_change.clone();
            move |open: bool, window: &mut Window, cx: &mut App| {
                if let Some(h) = &on_open_change {
                    h(open, window, cx);
                }
            }
        };

        let mut root = div().id(self.id.clone()).relative().flex().flex_none();
        if !self.no_trigger {
            let (open, memory_for_keys) = (self.open, memory.clone());
            let open_with = open_with.clone();
            let glyph = match self.icon {
                Some(icon) => div().child(Icon::new(icon).size(self.icon_size).color(if open {
                    color::FG
                } else {
                    color::FG_2
                })),
                None => div().child(line(
                    "⋯",
                    text::BASE,
                    line_height(text::BASE, 1.5),
                    if open { color::FG } else { color::FG_2 },
                )),
            };
            let click_open = open_with.clone();
            let label = self.label.clone();
            let mut button = div()
                .id((self.id.clone(), "trigger"))
                .track_focus(&trigger)
                .tab_stop(true)
                .flex()
                .items_center()
                .justify_center()
                .rounded(radius::MD)
                .cursor_pointer()
                .hover(|s| s.bg(color::HOVER))
                .focus_visible(|s| s.shadow(focus_ring()))
                .child(glyph)
                .tooltip(super::tooltip::tooltip(label, &[]))
                .tooltip_show_delay(super::tooltip::SHOW_DELAY)
                .on_click(move |_, window, cx| click_open(!open, window, cx))
                .on_key_down(move |event, window, cx| {
                    let key = &event.keystroke;
                    let from_start = match key.key.as_str() {
                        "down" => Some(false),
                        "up" => Some(true),
                        _ if opens_context_menu(key) => Some(false),
                        _ => None,
                    };
                    if let Some(last) = from_start {
                        cx.stop_propagation();
                        memory_for_keys.update(cx, |m, _| m.start_last = last);
                        open_with(true, window, cx);
                    }
                });
            button = match self.trigger_size {
                Some(size) => button.size(size),
                None => button.min_w(px(24.0)).min_h(px(24.0)).px(px(6.0)),
            };
            if open {
                button = button.bg(color::HOVER);
            }
            root = root.child(button);
        }
        if !self.open {
            return root;
        }

        // The popup.
        let mut popup = div()
            .id((self.id.clone(), "popup"))
            .flex()
            .flex_col()
            .min_w(px(176.0))
            .max_w(px(300.0))
            .p(px(4.0))
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::LINE_STRONG)
            .rounded(radius::MENU)
            .shadow(shadow_menu())
            .key_context(MENU_CONTEXT)
            .occlude();
        if let Some(width) = self.width {
            popup = popup.w(width);
        }
        if let Some(head) = self.head {
            popup = popup.child(head);
        }
        let key_handles = handles.clone();
        let close = {
            let open_with = open_with.clone();
            let back = back.clone();
            move |window: &mut Window, cx: &mut App| {
                open_with(false, window, cx);
                back.focus(window, cx);
            }
        };
        let down_out = open_with.clone();
        let (next_close, prev_close) = (open_with.clone(), open_with.clone());
        let (next_back, prev_back) = (back.clone(), back.clone());
        // Tab closes the menu and moves on from where it was opened (a binding runs before key listeners, so this is an
        // action bound in the popup's context, like the dialog's trap).
        popup = popup
            .on_action(move |_: &MenuNext, window, cx| {
                next_back.focus(window, cx);
                next_close(false, window, cx);
                window.focus_next(cx);
            })
            .on_action(move |_: &MenuPrevious, window, cx| {
                prev_back.focus(window, cx);
                prev_close(false, window, cx);
                window.focus_prev(cx);
            })
            .on_mouse_down_out(move |_, window, cx| down_out(false, window, cx))
            .on_key_down(move |event, window, cx| {
                let key = event.keystroke.key.as_str();
                match key {
                    "escape" => {
                        cx.stop_propagation();
                        close(window, cx);
                    }
                    _ => {
                        let from = key_handles.iter().position(|h| h.is_focused(window));
                        if let Some(next) = menu_next(key, from, key_handles.len()) {
                            cx.stop_propagation();
                            key_handles[next].focus(window, cx);
                        }
                    }
                }
            });
        for (i, item) in self.items.iter().enumerate() {
            if let Some(heading) = &item.heading {
                popup = popup.child(
                    div().px(px(8.0)).pt(px(6.0)).pb(px(2.0)).child(
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
            if item.sep {
                popup = popup.child(div().h(px(1.0)).mx(px(6.0)).my(px(4.0)).bg(color::HAIRLINE));
            }
            popup = popup.child(menu_row(
                (self.id.clone(), format!("item-{}", item.id)),
                item,
                &handles[i],
                on_select.clone(),
                open_with.clone(),
                back.clone(),
            ));
        }

        let popup: AnyElement = match self.at {
            Some(at) if self.no_trigger => anchored()
                .position(at)
                .snap_to_window()
                .child(popup)
                .into_any_element(),
            _ => div()
                .absolute()
                .top_full()
                .right_0()
                .pt(px(2.0))
                .child(popup)
                .into_any_element(),
        };
        root.child(deferred(popup).with_priority(5))
    }
}

fn menu_row(
    id: impl Into<ElementId>,
    item: &MenuItem,
    handle: &FocusHandle,
    on_select: Option<SelectHandler>,
    open_with: impl Fn(bool, &mut Window, &mut App) + Clone + 'static,
    back: FocusHandle,
) -> impl IntoElement {
    let fg = if item.danger {
        color::DANGER
    } else if item.room {
        color::FG_CHIP
    } else {
        color::FG
    };
    let hover = if item.danger {
        color::DANGER_SOFT
    } else {
        color::SURFACE_HI
    };
    let selector = format!("menu-item-{}", item.id);
    let mut row = div()
        .id(id)
        .debug_selector(move || selector.clone())
        .track_focus(handle)
        .tab_stop(false)
        .flex()
        .items_center()
        .gap(px(10.0))
        .h(px(item_height(item)))
        .pl(px(if item.room { 12.0 } else { 8.0 }))
        .pr(px(8.0))
        .rounded(radius::ROW)
        .cursor_pointer()
        .hover(move |s| s.bg(hover))
        .focus_visible(move |s| s.bg(hover).shadow(focus_ring_inset()));
    if let Some(icon) = item.icon {
        row = row.child(
            div()
                .flex_none()
                .opacity(if item.room { 1.0 } else { 0.85 })
                .child(
                    Icon::new(icon)
                        .size(px(if item.room { 13.0 } else { 14.0 }))
                        .color(fg),
                ),
        );
    }
    row = row.child(
        div()
            .flex_1()
            .min_w_0()
            .child(line(item.label.clone(), text::UI, line_height(text::UI, 1.5), fg).ellipsis()),
    );
    if let Some(hint) = &item.hint {
        let size = if item.room { text::META } else { text::MICRO };
        row = row.child(div().flex_none().child(line(
            hint.clone(),
            size,
            line_height(size, 1.5),
            color::FG_FAINT,
        )));
    }
    if let Some(on) = item.check {
        let mut b = div()
            .flex()
            .flex_none()
            .items_center()
            .justify_center()
            .size(px(14.0))
            .rounded(radius::TAG)
            .border_1()
            .border_color(if on {
                color::ACCENT
            } else {
                color::CONTROL_EDGE_KEY
            });
        if on {
            b = b.bg(color::ACCENT).child(
                Icon::new(IconName::Check)
                    .size(px(10.0))
                    .color(color::ACCENT_INK),
            );
        }
        row = row.child(b);
    }
    if item.checked == Some(true) {
        row = row.child(
            div().flex_none().child(
                Icon::new(IconName::Check)
                    .size(px(13.0))
                    .color(color::ACCENT),
            ),
        );
    }
    let (id, keeps_open) = (item.id.clone(), item.keeps_open());
    row.on_click(move |_, window, cx| {
        if let Some(h) = &on_select {
            h(&id, window, cx);
        }
        if !keeps_open {
            open_with(false, window, cx);
            back.focus(window, cx);
        }
    })
}

/// The ring drawn inside a menu row (`outline-offset: -2px`).
fn focus_ring_inset() -> Vec<gpui::BoxShadow> {
    let mut ring = focus_ring();
    for s in &mut ring {
        s.inset = true;
    }
    ring
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn heights_and_open_behaviour_follow_the_css() {
        let plain = MenuItem::new("a", "Copy");
        assert_eq!(item_height(&plain), 32.0);
        assert_eq!(item_height(&plain.clone().room()), 30.0);
        assert!(!plain.keeps_open());
        assert!(MenuItem::new("b", "Mute").check(false).keeps_open());
    }
}

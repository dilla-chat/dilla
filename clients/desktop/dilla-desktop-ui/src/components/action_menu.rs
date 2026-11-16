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
    AnyElement, App, ElementId, FocusHandle, Hsla, InteractiveElement, IntoElement, ParentElement,
    Pixels, Point, RenderOnce, SharedString, StatefulInteractiveElement, Styled, Window, anchored,
    deferred, div, px,
};

use super::dialog::{MENU_CONTEXT, MenuNext, MenuPrevious};
use crate::a11y::{Accessible, Role};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING_WIDE, focus_ring, shadow_menu, shadow_status};
use crate::roving::menu_next;
pub use crate::roving::opens_context_menu;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};
use gpui::accesskit::HasPopup;

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
    /// An 8px dot before the label in this colour (the status menu's presence dots, UserPanel.css `::before`).
    pub dot: Option<Hsla>,
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

    pub fn dot(mut self, colour: Hsla) -> Self {
        self.dot = Some(colour);
        self
    }

    /// Whether choosing the item leaves the menu open.
    pub fn keeps_open(&self) -> bool {
        self.check.is_some()
    }
}

/// How a menu's popup is drawn.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum MenuLook {
    /// The prototype's context menu.
    #[default]
    Context,
    /// The user panel's status menu (UserPanel.css `.d-user-panel__menu`): the `--control-edge-field` edge, 8px corners
    /// (`--r-lg`), the `--shadow-status`; items 10px in on both sides, the chosen one on the `--hover` fill while the
    /// pointer and the focus are elsewhere.
    Status,
}

/// The status menu's popup width: 220px of rows, plus its 4px padding and 1px edge on each side.
pub const STATUS_MENU_WIDTH: f32 = 230.0;

type OpenHandler = Rc<dyn Fn(bool, &mut Window, &mut App) + 'static>;
type SelectHandler = Rc<dyn Fn(&str, &mut Window, &mut App) + 'static>;

struct Memory {
    trigger: FocusHandle,
    was_open: bool,
    start_last: bool,
    /// The press now going on started on the trigger: the open popup's press-outside leaves it to the trigger's click,
    /// which closes the menu. Else the press-outside closed it and the click, drawn closed by then, opened it again.
    trigger_press: bool,
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
    /// A trigger drawn by the caller (`triggerContent`): its group name, its accessible name and its content.
    trigger_content: Option<(SharedString, SharedString, AnyElement)>,
    /// The popup opens above the trigger, this far from it (`data-placement="top"` plus the caller's margin).
    above: Option<Pixels>,
    look: MenuLook,
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
            trigger_content: None,
            above: None,
            look: MenuLook::Context,
            on_open_change: None,
            on_select: None,
        }
    }

    /// The trigger is `content` (ActionMenu.tsx `triggerContent`), filling the menu's box from the left, 3px 6px in, the
    /// `--hover` fill on hover and while open; `name` names it (`labelledBy`), and `group` is its hover group, for content
    /// that changes on hover. The menu itself grows to fill its row.
    pub fn trigger_content(
        mut self,
        group: impl Into<SharedString>,
        name: impl Into<SharedString>,
        content: impl IntoElement,
    ) -> Self {
        self.trigger_content = Some((group.into(), name.into(), content.into_any_element()));
        self
    }

    /// The popup opens above the trigger, `gap` from it, at its left edge.
    pub fn above(mut self, gap: Pixels) -> Self {
        self.above = Some(gap);
        self
    }

    pub fn look(mut self, look: MenuLook) -> Self {
        self.look = look;
        self
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

/// The status menu's presence dot: 8px.
pub const STATUS_DOT: f32 = 8.0;

/// An item's left and right padding: 8px (a room row 12px on the left), the status menu's 10px.
pub fn item_padding(item: &MenuItem, look: MenuLook) -> (f32, f32) {
    match look {
        MenuLook::Status => (10.0, 10.0),
        MenuLook::Context => (if item.room { 12.0 } else { 8.0 }, 8.0),
    }
}

/// The item's height: 32px, a room row 30px.
pub fn item_height(item: &MenuItem) -> f32 {
    if item.room { 30.0 } else { 32.0 }
}

impl RenderOnce for ActionMenu {
    fn render(mut self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let memory = window.use_keyed_state((self.id.clone(), "menu"), cx, |_, cx| Memory {
            trigger: cx.focus_handle().tab_stop(true),
            was_open: false,
            start_last: false,
            trigger_press: false,
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
                m.trigger_press = false;
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

        let mut root = div().id(self.id.clone()).relative().flex();
        root = if self.trigger_content.is_some() {
            root.flex_1().min_w_0()
        } else {
            root.flex_none()
        };
        if let Some((group, name, content)) =
            self.trigger_content.take().filter(|_| !self.no_trigger)
        {
            let (open, memory_for_keys) = (self.open, memory.clone());
            let open_with = open_with.clone();
            let click_open = open_with.clone();
            let mut button = div()
                .id((self.id.clone(), "trigger"))
                .group(group)
                .a11y(Role::Button, name)
                .aria_expanded(open)
                .node_props(|n| n.set_has_popup(HasPopup::Menu))
                .track_focus(&trigger)
                .tab_stop(true)
                .flex()
                .flex_1()
                .min_w_0()
                .items_center()
                .justify_start()
                .px(px(6.0))
                .py(px(3.0))
                .rounded(radius::MD)
                .cursor_pointer()
                .hover(|s| s.bg(color::HOVER))
                .focus_visible(|s| s.shadow(focus_ring()))
                .child(content)
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
            if open {
                button = button.bg(color::HOVER);
            }
            root = root.child(button);
        } else if !self.no_trigger {
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
            let (press_memory, click_memory) = (memory.clone(), memory.clone());
            let label = self.label.clone();
            // `aria-haspopup="menu" aria-expanded`.
            let mut button = div()
                .id((self.id.clone(), "trigger"))
                .a11y(Role::Button, self.label.clone())
                .aria_expanded(open)
                .node_props(|n| n.set_has_popup(HasPopup::Menu))
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
                // Capture runs back to front, so this marks the press before the (deferred, later painted) popup's
                // press-outside sees it.
                .capture_any_mouse_down(move |_, _, cx| {
                    press_memory.update(cx, |m, _| m.trigger_press = true);
                })
                .on_click(move |_, window, cx| {
                    click_memory.update(cx, |m, _| m.trigger_press = false);
                    click_open(!open, window, cx)
                })
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
            .a11y(Role::Menu, self.label.clone())
            .flex()
            .flex_col()
            .max_w(px(300.0))
            .p(px(4.0))
            .bg(color::SURFACE_2)
            .border_1()
            .key_context(MENU_CONTEXT)
            .occlude();
        popup = match self.look {
            MenuLook::Context => popup
                .min_w(px(176.0))
                .border_color(color::LINE_STRONG)
                .rounded(radius::MENU)
                .shadow(shadow_menu()),
            MenuLook::Status => popup
                .w(px(STATUS_MENU_WIDTH))
                .border_color(color::CONTROL_EDGE_FIELD)
                .rounded(radius::LG)
                .shadow(shadow_status()),
        };
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
        let (down_out, out_memory) = (open_with.clone(), memory.clone());
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
            .on_mouse_down_out(move |_, window, cx| {
                if !out_memory.update(cx, |m, _| std::mem::take(&mut m.trigger_press)) {
                    down_out(false, window, cx);
                }
            })
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
            // ActionMenu.tsx: the rule, then the heading, then the item.
            if item.sep {
                popup = popup.child(div().h(px(1.0)).mx(px(6.0)).my(px(4.0)).bg(color::HAIRLINE));
            }
            if let Some(heading) = &item.heading {
                popup = popup.child(
                    div().px(px(8.0)).pt(px(6.0)).pb(px(2.0)).child(
                        line(
                            heading.to_uppercase(),
                            text::MICRO,
                            // The web's heading is 21px tall: 6 + its line at 1.3 (13) + 2.
                            line_height(text::MICRO, 1.3),
                            color::FG_3,
                        )
                        .weight(font::WEIGHT_LABEL)
                        .tracking(LABEL_TRACKING_WIDE),
                    ),
                );
            }
            popup = popup.child(menu_row(
                (self.id.clone(), format!("item-{}", item.id)),
                item,
                &handles[i],
                on_select.clone(),
                open_with.clone(),
                back.clone(),
                self.look,
            ));
        }

        let popup: AnyElement = match self.at {
            Some(at) if self.no_trigger => anchored()
                .position(at)
                .snap_to_window()
                .child(popup)
                .into_any_element(),
            _ if self.above.is_some() => div()
                .absolute()
                .bottom_full()
                .left_0()
                .pb(self.above.unwrap_or_default())
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
    look: MenuLook,
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
        .pl(px(item_padding(item, look).0))
        .pr(px(item_padding(item, look).1))
        .rounded(radius::ROW)
        .cursor_pointer();
    // The status menu draws its chosen row on the `--hover` fill; the pointer and the focus keep `--surface-hi`.
    if look == MenuLook::Status && item.checked == Some(true) {
        row = row.bg(color::HOVER);
    }
    row = row
        .hover(move |s| s.bg(hover))
        .focus_visible(move |s| s.bg(hover).shadow(focus_ring_inset()));
    if let Some(dot) = item.dot {
        row = row.child(
            div()
                .flex_none()
                .size(px(STATUS_DOT))
                .rounded(radius::PILL)
                .bg(dot),
        );
    }
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
    // `menuitem`, `menuitemcheckbox` (an on/off item) or `menuitemradio` (a single choice), named with its hint (a
    // shortcut or a head count) as the description.
    let role = if item.check.is_some() {
        Role::MenuItemCheckBox
    } else if item.checked.is_some() {
        Role::MenuItemRadio
    } else {
        Role::MenuItem
    };
    row = row.a11y(role, item.label.clone());
    if let Some(on) = item.check.or(item.checked) {
        row = row.toggled(on);
    }
    if let Some(hint) = &item.hint {
        row = row.aria_description(hint.clone());
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

    #[test]
    fn the_status_look_follows_user_panel_css() {
        let plain = MenuItem::new("online", "online");
        assert_eq!(item_padding(&plain, MenuLook::Status), (10.0, 10.0));
        assert_eq!(item_padding(&plain, MenuLook::Context), (8.0, 8.0));
        assert_eq!(
            item_padding(&plain.clone().room(), MenuLook::Context),
            (12.0, 8.0)
        );
        assert_eq!(STATUS_MENU_WIDTH, 220.0 + 2.0 * 4.0 + 2.0 * 1.0);
        assert_eq!(STATUS_DOT, 8.0);
    }
}

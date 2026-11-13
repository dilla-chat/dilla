//! One device of the account in Settings -> Devices (packages/ui/src/DeviceRow).
//!
//! A device is a setting row: the 32px monitor tile, the name (12.5/600) with its tags, the meta line (11px `--fg-2`) and the
//! actions on the right. The rows of one list draw one 10px-radius group with hairlines between (a group of n rows is 56n + 2
//! tall); the host says where a row sits ([`GroupPosition`]). A row with a menu is a tab stop that opens it with a right
//! click, Shift+F10 or the Menu key (Settings -> Devices puts Rename there, so the row draws only the one button).

use std::rc::Rc;

use gpui::{
    AnyElement, App, ClickEvent, ElementId, InteractiveElement, IntoElement, MouseButton,
    ParentElement, Pixels, Point, RenderOnce, SharedString, Styled, Window, div, px,
};

use dilla_desktop_ui::components::action_menu::{ActionMenu, MenuItem, opens_context_menu};
use dilla_desktop_ui::components::button::{Button, ButtonVariant};
use dilla_desktop_ui::components::settings::row::GroupPosition;
use dilla_desktop_ui::icon::Icon;
use dilla_desktop_ui::icon_names::IconName;
use dilla_desktop_ui::metrics::{TRACKING_TAG, focus_ring};
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, radius, text};

/// `.d-device-row`: the horizontal and vertical padding, the gaps between the tile, the text and the actions.
const PAD_X: f32 = 16.0;
const PAD_Y: f32 = 10.0;
const GAP_X: f32 = 14.0;
const GAP_Y: f32 = 10.0;
/// `.d-device-row__icon`: 2rem square; its glyph 15px.
const TILE: f32 = 32.0;
const GLYPH: f32 = 15.0;
/// `.d-device-row__main`: `flex: 1 1 220px`.
const MAIN_BASIS: f32 = 220.0;
const HEAD_GAP: f32 = 8.0;
const ACTIONS_GAP: f32 = 8.0;
/// A tag: `padding: 0 5px`.
const TAG_PAD_X: f32 = 5.0;

type Select = Rc<dyn Fn(&str, &mut Window, &mut App) + 'static>;
type Press = Rc<dyn Fn(&ClickEvent, &mut Window, &mut App) + 'static>;

struct Menu {
    label: SharedString,
    items: Vec<MenuItem>,
    on_select: Select,
}

struct Action {
    label: SharedString,
    danger: bool,
    on_press: Press,
}

#[derive(IntoElement)]
pub struct DeviceRow {
    id: ElementId,
    name: SharedString,
    own_label: Option<SharedString>,
    platform_label: Option<SharedString>,
    state: Option<SharedString>,
    seen: Option<SharedString>,
    position: GroupPosition,
    action: Option<Action>,
    menu: Option<Menu>,
}

impl DeviceRow {
    pub fn new(id: impl Into<ElementId>, name: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            name: name.into(),
            own_label: None,
            platform_label: None,
            state: None,
            seen: None,
            position: GroupPosition::Only,
            action: None,
            menu: None,
        }
    }

    /// The "This browser" tag of this device's row.
    pub fn own_label(mut self, label: impl Into<SharedString>) -> Self {
        self.own_label = Some(label.into());
        self
    }

    /// Where the device runs ("Desktop · Linux"): informational, never a trust signal.
    pub fn platform_label(mut self, label: impl Into<SharedString>) -> Self {
        self.platform_label = Some(label.into());
        self
    }

    pub fn state(mut self, state: impl Into<SharedString>) -> Self {
        self.state = Some(state.into());
        self
    }

    pub fn seen(mut self, seen: impl Into<SharedString>) -> Self {
        self.seen = Some(seen.into());
        self
    }

    pub fn position(mut self, position: GroupPosition) -> Self {
        self.position = position;
        self
    }

    pub fn action(
        mut self,
        label: impl Into<SharedString>,
        danger: bool,
        on_press: impl Fn(&ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.action = Some(Action {
            label: label.into(),
            danger,
            on_press: Rc::new(on_press),
        });
        self
    }

    pub fn menu(
        mut self,
        label: impl Into<SharedString>,
        items: Vec<MenuItem>,
        on_select: impl Fn(&str, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.menu = Some(Menu {
            label: label.into(),
            items,
            on_select: Rc::new(on_select),
        });
        self
    }
}

struct Memory {
    open: bool,
    at: Option<Point<Pixels>>,
    focus: gpui::FocusHandle,
}

fn tag(label: &SharedString, own: bool) -> AnyElement {
    let (fg, bg) = if own {
        (color::ACCENT_2, color::ACCENT_TAG)
    } else {
        (color::FG_2, color::SURFACE_HI)
    };
    div()
        .px(px(TAG_PAD_X))
        .rounded(radius::TAG)
        .bg(bg)
        .child(
            line(
                label.to_uppercase(),
                text::CHIP,
                line_height(text::CHIP, 1.4),
                fg,
            )
            .weight(font::WEIGHT_LABEL)
            .tracking(TRACKING_TAG),
        )
        .into_any_element()
}

impl RenderOnce for DeviceRow {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let memory = window.use_keyed_state((self.id.clone(), "row"), cx, |_, cx| Memory {
            open: false,
            at: None,
            focus: cx.focus_handle().tab_stop(true),
        });
        let (open, at, focus) = memory.read_with(cx, |m, _| (m.open, m.at, m.focus.clone()));
        let set = {
            let memory = memory.clone();
            move |open: bool, at: Option<Point<Pixels>>, cx: &mut App| {
                memory.update(cx, |m, cx| {
                    m.open = open;
                    m.at = at;
                    cx.notify();
                });
            }
        };

        let mut head = div()
            .flex()
            .flex_wrap()
            .items_center()
            .gap(px(HEAD_GAP))
            .min_w_0()
            .child(
                line(
                    self.name.clone(),
                    text::SM,
                    line_height(text::SM, 1.4),
                    color::FG,
                )
                .weight(font::WEIGHT_LABEL),
            );
        if let Some(label) = &self.platform_label {
            head = head.child(tag(label, false));
        }
        if let Some(label) = &self.own_label {
            head = head.child(tag(label, true));
        }
        let mut main = div()
            .flex()
            .flex_col()
            .flex_1()
            .min_w(px(MAIN_BASIS))
            .child(head);
        if self.state.is_some() || self.seen.is_some() {
            let mut meta = div().flex().items_center();
            if let Some(state) = &self.state {
                meta = meta.child(line(
                    state.clone(),
                    text::XS,
                    line_height(text::XS, 1.4),
                    color::FG_2,
                ));
            }
            if self.state.is_some() && self.seen.is_some() {
                meta = meta.child(line(
                    " · ",
                    text::XS,
                    line_height(text::XS, 1.4),
                    color::FG_2,
                ));
            }
            if let Some(seen) = &self.seen {
                meta = meta.child(line(
                    seen.clone(),
                    text::XS,
                    line_height(text::XS, 1.4),
                    color::FG_2,
                ));
            }
            main = main.child(meta);
        }

        let mut menus: Vec<AnyElement> = Vec::new();
        if let Some(menu) = &self.menu {
            let on_select = menu.on_select.clone();
            let close = set.clone();
            let mut context = ActionMenu::new(
                (self.id.clone(), "context"),
                menu.label.clone(),
                menu.items.clone(),
            )
            .no_trigger(Some(&focus))
            .open(open)
            .on_select(move |id, window, cx| on_select(id, window, cx))
            .on_open_change(move |open, _, cx| close(open, None, cx));
            if let Some(at) = at {
                context = context.at(at);
            }
            menus.push(context.into_any_element());
        }

        let mut actions = div().flex().flex_wrap().justify_end().gap(px(ACTIONS_GAP));
        if let Some(action) = &self.action {
            let press = action.on_press.clone();
            actions = actions.child(
                Button::new((self.id.clone(), "action"), action.label.clone())
                    .variant(if action.danger {
                        ButtonVariant::Danger
                    } else {
                        ButtonVariant::Default
                    })
                    .on_click(move |e, window, cx| press(e, window, cx)),
            );
        }

        let mut row = div()
            .id(self.id.clone())
            .relative()
            .flex()
            .flex_wrap()
            .items_center()
            .gap_x(px(GAP_X))
            .gap_y(px(GAP_Y))
            .w_full()
            .min_h(px(self.position.min_height()))
            .min_w_0()
            .px(px(PAD_X))
            .py(px(PAD_Y))
            .border_1()
            .border_color(color::HAIRLINE);
        row = match self.position {
            GroupPosition::Only => row.rounded(radius::XL),
            GroupPosition::First => row.rounded_t(radius::XL).border_b_0(),
            GroupPosition::Middle => row.border_b_0(),
            GroupPosition::Last => row.rounded_b(radius::XL),
        };
        if self.menu.is_some() {
            let key_set = set.clone();
            let mouse_set = set.clone();
            row = row
                .track_focus(&focus)
                .tab_stop(true)
                .focus_visible(|s| s.shadow(focus_ring()))
                .on_key_down(move |event, _, cx| {
                    if opens_context_menu(&event.keystroke) {
                        cx.stop_propagation();
                        key_set(true, None, cx);
                    }
                })
                .on_mouse_down(MouseButton::Right, move |event, _, cx| {
                    mouse_set(true, Some(event.position), cx);
                });
        }
        row.child(
            div()
                .flex()
                .flex_none()
                .items_center()
                .justify_center()
                .size(px(TILE))
                .rounded(radius::LG)
                .bg(color::SURFACE_TILE)
                .child(
                    Icon::new(IconName::MonitorSmartphone)
                        .size(px(GLYPH))
                        .color(color::ACCENT),
                ),
        )
        .child(main)
        .child(actions)
        .children(menus)
    }
}

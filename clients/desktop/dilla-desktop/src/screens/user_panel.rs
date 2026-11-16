//! The user panel at the bottom left of the sidebar (packages/ui UserPanel, Signal §5.4): the avatar with its status dot,
//! the name over the status line, and the mute and deafen buttons (30px, a 16px glyph; on, the danger glyph on the danger tint).
//! The buttons call the voice area's handle (`Client::voice().set_muted`, `set_deafened`: Task 17 implements them).
//!
//! The avatar and name block is the status menu's trigger (UserPanel.tsx `status`, Discord-style; the prototype's
//! `toggleStatusMenu`): online, idle, do not disturb, invisible, the chosen one ticked, each with its dot. Choosing one sends
//! the account-wide presence choice (`Client::profile().set_presence_choice`).

use std::rc::Rc;

use gpui::{
    App, ClickEvent, InteractiveElement, IntoElement, ParentElement, Pixels, SharedString,
    StatefulInteractiveElement, Styled, Window, div,
};

use dilla_client::state::profile::PresenceChoice;
use dilla_desktop_ui::a11y::{Accessible, Role};
use dilla_desktop_ui::components::action_menu::{ActionMenu, MenuItem, MenuLook};
use dilla_desktop_ui::components::tooltip::tooltip;
use dilla_desktop_ui::components::{Avatar, AvatarSize, Presence};
use dilla_desktop_ui::metrics::focus_ring;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, layout, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::strings::{t, tf};

/// UserPanel.css.
pub mod css {
    /// Padding 0 8px, gap 4px; the avatar and name block is 36px high (a 30px avatar, 3px 6px in), 9px between its parts.
    pub const SIDE: f32 = 8.0;
    pub const GAP: f32 = 4.0;
    pub const WHO_H: f32 = 36.0;
    pub const WHO_GAP: f32 = 9.0;
    /// The mute and deafen buttons: 30px with a 16px glyph.
    pub const BUTTON: f32 = 30.0;
    pub const GLYPH: f32 = 16.0;
    /// The sidebar's edge runs on past the list: a 1px line over the panel's last column.
    pub const EDGE: f32 = 1.0;
    /// `.d-user-panel__ptt`: 2px out past the mute button's bottom right, 3px in, 16px high.
    pub const PTT_OUT: f32 = -2.0;
    pub const PTT_PAD_X: f32 = 3.0;
    pub const PTT_LINE: f32 = 16.0;
    /// `.d-user-panel__avatar[data-speaking]`: 2px of the panel, then 1.5px of `--ok`.
    pub const RING_GAP: f32 = 2.0;
    pub const RING: f32 = 1.5;
    /// `.d-user-panel__menu .d-action-menu__popup`: the status menu floats 7px above the block.
    pub const MENU_GAP: f32 = 7.0;
    /// `.dw-panel-error`: padding 4px 8px.
    pub const ERROR_PAD_Y: f32 = 4.0;
    pub const ERROR_PAD_X: f32 = 8.0;
}

/// The hover group of the avatar and name block: the status line turns `--fg-2` on its hovered fill.
const WHO_GROUP: &str = "user-panel-who";

/// The PTT tag over the mute button's corner (UserPanel.css `.d-user-panel__ptt`).
fn ptt_tag() -> impl IntoElement {
    div()
        .debug_selector(|| "user-panel-ptt".into())
        .absolute()
        .right(Pixels::from(css::PTT_OUT))
        .bottom(Pixels::from(css::PTT_OUT))
        .px(Pixels::from(css::PTT_PAD_X))
        .bg(color::BG_2)
        .border_1()
        .border_color(color::HAIRLINE)
        .rounded(radius::SM)
        .child(
            line(t("layout.ptt.tag"), text::BADGE, css::PTT_LINE, color::FG_2)
                .tracking(dilla_desktop_ui::metrics::TRACKING_SOFT),
        )
}

pub struct PanelModel<'a> {
    pub name: &'a str,
    /// The account's presence choice (the avatar's dot and the menu's tick); online until it is read.
    pub choice: PresenceChoice,
    /// The account's custom status ("packing rope"); empty, the presence label stands.
    pub status_msg: &'a str,
    /// UserPanel.tsx `inRoom`: in a call the line under the name says where ("in {channel}") in place of the presence.
    pub in_room: Option<&'a str>,
    pub muted: bool,
    pub deafened: bool,
    /// Push to talk is the input mode (UserPanel.tsx `ptt`): the PTT tag on the mute button, and whether the key is held
    /// now (the avatar's speaking ring).
    pub ptt: Option<bool>,
    /// Whether the status menu is open (the shell's, as the web keeps `menuOpen`).
    pub status_menu_open: bool,
}

/// The status menu opens (`true`) or closes.
pub type OnStatusOpen = Rc<dyn Fn(bool, &mut Window, &mut App)>;
/// A status was chosen.
pub type OnStatusPick = Rc<dyn Fn(PresenceChoice, &mut Window, &mut App)>;

#[derive(Clone)]
pub struct PanelHandlers {
    pub on_mute: super::shell::Action,
    pub on_deafen: super::shell::Action,
    pub on_status_open: OnStatusOpen,
    pub on_status_pick: OnStatusPick,
}

/// UserPanel.tsx `line`: "in {room}" while this device is in a call, else the custom status, else the presence label.
pub fn status_line(choice: PresenceChoice, status_msg: &str, in_room: Option<&str>) -> String {
    match in_room {
        Some(room) => room.to_owned(),
        None if !status_msg.is_empty() => status_msg.to_owned(),
        None => presence_label(choice).to_owned(),
    }
}

/// `presence.<choice>`: "online", "idle", "do not disturb", "invisible".
pub fn presence_label(choice: PresenceChoice) -> &'static str {
    t(&format!("presence.{}", choice.id()))
}

/// UserPanel.tsx `DOT`: the avatar's dot for a choice; invisible draws the offline dot.
pub fn dot_of(choice: PresenceChoice) -> Presence {
    match choice {
        PresenceChoice::Online => Presence::Online,
        PresenceChoice::Idle => Presence::Idle,
        PresenceChoice::Dnd => Presence::Dnd,
        PresenceChoice::Invisible => Presence::Offline,
    }
}

/// UserPanel.css `.d-action-menu__item[data-id]::before`: online `--ok`, idle `--warn`, dnd `--danger`, invisible `--fg-3`.
pub fn dot_colour(choice: PresenceChoice) -> gpui::Hsla {
    match choice {
        PresenceChoice::Online => color::OK,
        PresenceChoice::Idle => color::WARN,
        PresenceChoice::Dnd => color::DANGER,
        PresenceChoice::Invisible => color::FG_3,
    }
}

/// The status menu's items (UserPanel.tsx `CHOICES`): every choice in order, its dot, the current one checked.
pub fn status_items(current: PresenceChoice) -> Vec<MenuItem> {
    PresenceChoice::ALL
        .into_iter()
        .map(|c| {
            MenuItem::new(c.id(), presence_label(c))
                .dot(dot_colour(c))
                .checked(c == current)
        })
        .collect()
}

/// `panel.status`: "your status: {status}", the menu's name.
pub fn status_menu_label(choice: PresenceChoice) -> String {
    tf("panel.status", &[("status", presence_label(choice))])
}

/// The tooltip chord of a shortcut: Ctrl+Shift+M on Windows and Linux, the Command key on macOS.
fn chord(letter: &'static str) -> [&'static str; 3] {
    [
        if cfg!(target_os = "macos") {
            "⌘"
        } else {
            "Ctrl"
        },
        "Shift",
        letter,
    ]
}

fn toggle(
    id: &'static str,
    off: IconName,
    on: IconName,
    label: &'static str,
    letter: &'static str,
    pressed: bool,
    handler: super::shell::Action,
) -> impl IntoElement {
    let (fg, bg) = if pressed {
        (color::DANGER, color::DANGER_SOFT)
    } else {
        (color::FG_2, gpui::transparent_black())
    };
    div()
        .id(id)
        .a11y(Role::Button, t(label))
        .toggled(pressed)
        .tab_index(0)
        .flex()
        .flex_none()
        .items_center()
        .justify_center()
        .size(Pixels::from(css::BUTTON))
        .rounded(radius::MD)
        .bg(bg)
        .cursor_pointer()
        .hover(|s| s.bg(color::HOVER_HI))
        .focus_visible(|s| s.shadow(focus_ring()))
        .tooltip(tooltip(t(label), &chord(letter)))
        .on_click(move |_: &ClickEvent, window, cx| handler(window, cx))
        .child(
            Icon::new(if pressed { on } else { off })
                .size(Pixels::from(css::GLYPH))
                .color(fg),
        )
}

/// The avatar and name block: the trigger's content.
fn who(model: &PanelModel<'_>) -> impl IntoElement {
    let avatar = Avatar::new("user-panel-avatar", model.name.to_owned())
        .size(AvatarSize::Panel)
        .presence(dot_of(model.choice));
    // Push to talk held: the speaking ring, 2px of the panel and 1.5px of `--ok`.
    let held = model.ptt == Some(true);
    let inner = div()
        .flex_none()
        .rounded(radius::AVATAR)
        .border(Pixels::from(css::RING_GAP))
        .border_color(if held {
            color::BG_2
        } else {
            gpui::transparent_black()
        })
        .m(Pixels::from(-css::RING_GAP))
        .child(avatar);
    let ring = div()
        .flex_none()
        .rounded(radius::AVATAR)
        .border(Pixels::from(css::RING))
        .border_color(if held {
            color::OK
        } else {
            gpui::transparent_black()
        })
        .m(Pixels::from(-css::RING))
        .child(inner);
    let text_line = status_line(model.choice, model.status_msg, model.in_room);
    let sub = |colour| {
        line(
            text_line.clone(),
            text::META,
            line_height(text::META, 1.3),
            colour,
        )
        .ellipsis()
    };
    // `--fg-3`, `--fg-2` on the hovered or open fill (README §10.4 keeps `--fg-3` off lifted fills).
    let status = if model.status_menu_open {
        div().min_w_0().child(sub(color::FG_2))
    } else {
        div()
            .relative()
            .min_w_0()
            .child(
                div()
                    .min_w_0()
                    .group_hover(WHO_GROUP, |s| s.invisible())
                    .child(sub(color::FG_3)),
            )
            .child(
                div()
                    .absolute()
                    .top_0()
                    .left_0()
                    .w_full()
                    .invisible()
                    .group_hover(WHO_GROUP, |s| s.visible())
                    .child(sub(color::FG_2)),
            )
    };
    div()
        .flex_1()
        .min_w_0()
        .h(Pixels::from(css::WHO_H - 6.0))
        .flex()
        .items_center()
        .gap(Pixels::from(css::WHO_GAP))
        .child(ring)
        .child(
            div()
                .min_w_0()
                .flex()
                .flex_col()
                .child(
                    line(
                        model.name.to_owned(),
                        text::UI,
                        line_height(text::UI, 1.3),
                        color::FG,
                    )
                    .weight(font::WEIGHT_LABEL)
                    .ellipsis(),
                )
                .child(status),
        )
}

pub fn render(model: &PanelModel<'_>, handlers: &PanelHandlers) -> impl IntoElement + use<> {
    let label = status_menu_label(model.choice);
    // WCAG 2.5.3: the button's name holds its visible text, then the status (UserPanel.tsx `nameId`).
    let trigger_name = format!("{}, {label}", model.name);
    let (on_open, on_pick) = (
        handlers.on_status_open.clone(),
        handlers.on_status_pick.clone(),
    );
    let menu = ActionMenu::new("user-panel-status", label, status_items(model.choice))
        .look(MenuLook::Status)
        .above(Pixels::from(css::MENU_GAP))
        .trigger_content(WHO_GROUP, trigger_name, who(model))
        .open(model.status_menu_open)
        .on_open_change(move |open, window, cx| on_open(open, window, cx))
        .on_select(move |id, window, cx| {
            if let Some(choice) = PresenceChoice::from_id(id) {
                on_pick(choice, window, cx);
            }
        });
    let panel = div()
        .id(SharedString::from("user-panel"))
        .landmark(Role::Region, t("panel.region"))
        .flex_none()
        .relative()
        .h(layout::USER_PANEL)
        .bg(color::BG_2)
        .border_t_1()
        .border_color(color::HAIRLINE)
        .flex()
        .items_center()
        .px(Pixels::from(css::SIDE))
        .gap(Pixels::from(css::GAP))
        // `.d-user-panel__menu`: the block fills the panel up to the toggles.
        .child(
            div()
                .debug_selector(|| "user-panel-status".into())
                .flex()
                .flex_1()
                .min_w_0()
                .child(menu),
        )
        .child({
            let mic = div().relative().flex_none().child(toggle(
                "user-panel-mute",
                IconName::Mic,
                IconName::MicOff,
                "voice.mute",
                "M",
                model.muted,
                handlers.on_mute.clone(),
            ));
            if model.ptt.is_some() {
                mic.child(ptt_tag())
            } else {
                mic
            }
        })
        .child(toggle(
            "user-panel-deafen",
            IconName::Headphones,
            IconName::HeadphoneOff,
            "voice.deafen",
            "D",
            model.deafened,
            handlers.on_deafen.clone(),
        ))
        .child(
            div()
                .absolute()
                .right_0()
                .top_0()
                .w(Pixels::from(css::EDGE))
                .h_full()
                .bg(color::HAIRLINE),
        );
    #[cfg(feature = "automation")]
    let panel = panel.child(crate::automation::bounds_probe("shell-user-panel"));
    panel
}

/// UserPanel.tsx's `presence.error` under the panel (`.dw-panel-error`, `role="alert"`): the choice was not taken.
pub fn status_error() -> impl IntoElement {
    div()
        .id("user-panel-error")
        .flex_none()
        .px(Pixels::from(css::ERROR_PAD_X))
        .py(Pixels::from(css::ERROR_PAD_Y))
        .bg(color::BG_2)
        .a11y(Role::Alert, t("presence.error"))
        .child(line(
            t("presence.error"),
            text::XS,
            line_height(text::XS, 1.5),
            color::DANGER,
        ))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_line_is_the_room_then_the_custom_status_then_the_presence() {
        assert_eq!(
            status_line(PresenceChoice::Dnd, "packing rope", Some("in harbour")),
            "in harbour"
        );
        assert_eq!(
            status_line(PresenceChoice::Dnd, "packing rope", None),
            "packing rope"
        );
        assert_eq!(status_line(PresenceChoice::Dnd, "", None), "do not disturb");
        assert_eq!(status_line(PresenceChoice::Online, "", None), "online");
    }

    #[test]
    fn the_menu_lists_every_status_with_its_dot_and_ticks_the_current_one() {
        let items = status_items(PresenceChoice::Idle);
        let ids: Vec<_> = items.iter().map(|i| i.id.to_string()).collect();
        assert_eq!(ids, ["online", "idle", "dnd", "invisible"]);
        let labels: Vec<_> = items.iter().map(|i| i.label.to_string()).collect();
        assert_eq!(labels, ["online", "idle", "do not disturb", "invisible"]);
        let checked: Vec<_> = items.iter().map(|i| i.checked).collect();
        assert_eq!(checked, [Some(false), Some(true), Some(false), Some(false)]);
        assert_eq!(items[0].dot, Some(color::OK));
        assert_eq!(items[1].dot, Some(color::WARN));
        assert_eq!(items[2].dot, Some(color::DANGER));
        assert_eq!(items[3].dot, Some(color::FG_3));
        assert_eq!(status_menu_label(PresenceChoice::Idle), "your status: idle");
    }

    #[test]
    fn invisible_wears_the_offline_dot() {
        assert_eq!(dot_of(PresenceChoice::Online), Presence::Online);
        assert_eq!(dot_of(PresenceChoice::Idle), Presence::Idle);
        assert_eq!(dot_of(PresenceChoice::Dnd), Presence::Dnd);
        assert_eq!(dot_of(PresenceChoice::Invisible), Presence::Offline);
    }
}

//! The user panel at the bottom left of the sidebar (packages/ui UserPanel, Signal §5.4): the avatar with its presence, the
//! name over the status line, and the mute and deafen buttons (30px, a 16px glyph; on, the danger glyph on the danger tint).
//! The buttons call the voice area's handle (`Client::voice().set_muted`, `set_deafened`: Task 17 implements them).

use gpui::{
    ClickEvent, InteractiveElement, IntoElement, ParentElement, Pixels, SharedString,
    StatefulInteractiveElement, Styled, div,
};

use dilla_desktop_ui::a11y::{Accessible, Role};
use dilla_desktop_ui::components::tooltip::tooltip;
use dilla_desktop_ui::components::{Avatar, AvatarSize, Presence};
use dilla_desktop_ui::metrics::focus_ring;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, layout, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::strings::t;

/// UserPanel.css.
mod css {
    /// Padding 0 8px, gap 4px; the avatar and name block is 36px high, 6px in, 9px between its parts.
    pub const SIDE: f32 = 8.0;
    pub const GAP: f32 = 4.0;
    pub const WHO_H: f32 = 36.0;
    pub const WHO_LEFT: f32 = 6.0;
    pub const WHO_GAP: f32 = 9.0;
    /// The mute and deafen buttons: 30px with a 16px glyph.
    pub const BUTTON: f32 = 30.0;
    pub const GLYPH: f32 = 16.0;
    /// The sidebar's edge runs on past the list: a 1px line over the panel's last column.
    pub const EDGE: f32 = 1.0;
}

pub struct PanelModel<'a> {
    pub name: &'a str,
    /// `online`, `connecting` or `offline`: the presence line under the name.
    pub status: &'a str,
    /// UserPanel.tsx `inRoom`: in a call the line under the name says where ("in {channel}") in place of the presence.
    pub in_room: Option<&'a str>,
    pub muted: bool,
    pub deafened: bool,
}

#[derive(Clone)]
pub struct PanelHandlers {
    pub on_mute: super::shell::Action,
    pub on_deafen: super::shell::Action,
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

pub fn render(model: &PanelModel<'_>, handlers: &PanelHandlers) -> impl IntoElement + use<> {
    let presence = if model.status == "online" {
        Presence::Online
    } else {
        Presence::Offline
    };
    let who = div()
        .flex_1()
        .min_w_0()
        .h(Pixels::from(css::WHO_H))
        .flex()
        .items_center()
        .pl(Pixels::from(css::WHO_LEFT))
        .gap(Pixels::from(css::WHO_GAP))
        .child(
            Avatar::new("user-panel-avatar", model.name.to_owned())
                .size(AvatarSize::Panel)
                .presence(presence),
        )
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
                .child(
                    line(
                        model.in_room.unwrap_or(model.status).to_owned(),
                        text::META,
                        line_height(text::META, 1.3),
                        color::FG_3,
                    )
                    .ellipsis(),
                ),
        );
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
        .child(who)
        .child(toggle(
            "user-panel-mute",
            IconName::Mic,
            IconName::MicOff,
            "voice.mute",
            "M",
            model.muted,
            handlers.on_mute.clone(),
        ))
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

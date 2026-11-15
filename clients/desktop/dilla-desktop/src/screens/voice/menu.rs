//! The stage's person menu (VoiceView.tsx `personMenu`, packages/ui PersonContextMenu, Signal §5.4): a right-click on a
//! tile, or Shift+F10 / the Menu key on the selected one, opens it at the pointer (under the tile for a key).
//!
//! - Another person: their volume ("User volume", a shared screen's "Stream volume", 0-200 % in steps of 10, which the
//!   call applies to what it plays, `session::gains`), Profile and Message (on a person tile), Mute for me (the volume at 0,
//!   and back to what it was) and Copy username ("@{name} copied").
//! - Yourself: Mute and Deafen with their shortcuts, Profile, Move to the server's other voice channels (with how many are
//!   in each; the AFK channel is not offered) and Leave voice. A move joins that room and says "You moved to {room}".
//!
//! Edit roles (Manage roles) and Mention (only with an open composer, which the stage has none of) are not offered here.

use std::collections::HashMap;

use dilla_client::features::voice_prefs::values::{Setting, VoiceValues};
use dilla_client::state::{AppState, Channel, ChannelId, UserId};
use gpui::{
    AnyElement, App, Bounds, IntoElement, ParentElement, Pixels, Point, Styled, Window, WindowId,
    div, point, px,
};

use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::action_menu::{ActionMenu, MenuItem};
use dilla_desktop_ui::components::voice_settings::Slider;
use dilla_desktop_ui::components::{Avatar, AvatarShape, AvatarSize};
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, text};

use super::{TileKey, client_of, is_bot, keycaps, name_of, ui_mut, ui_of};
use crate::strings::{t, tf};

/// PersonContextMenu.css: 236px wide; the head 8px in (10px under), the avatar 10px from the name; the volume block 6px
/// by 8px in (8px under), its row 4px over the range.
mod m {
    pub const WIDTH: f32 = 236.0;
    pub const HEAD_PAD: f32 = 8.0;
    pub const HEAD_PAD_B: f32 = 10.0;
    pub const HEAD_GAP: f32 = 10.0;
    pub const HEAD_MB: f32 = 4.0;
    pub const VOLUME_PAD_X: f32 = 8.0;
    pub const VOLUME_PAD_T: f32 = 6.0;
    pub const VOLUME_PAD_B: f32 = 8.0;
    pub const VOLUME_GAP: f32 = 4.0;
}

/// The id prefix of a Move to room.
const MOVE: &str = "move:";

/// The menu's state in a window: whose tile, where it opened, and what Mute for me goes back to.
#[derive(Default)]
pub struct MenuState {
    pub open: Option<(TileKey, Point<Pixels>)>,
    /// Mute for me's volume to go back to, by (stream, user).
    before_mute: HashMap<(bool, UserId), u32>,
    /// Each tile's bounds as last laid out: a key opens the menu under its tile.
    pub bounds: std::rc::Rc<std::cell::RefCell<HashMap<TileKey, Bounds<Pixels>>>>,
}

/// Opens the person menu of `key`'s tile at `at` (a right-click's position).
pub fn open(w: WindowId, key: TileKey, at: Point<Pixels>, cx: &mut App) {
    if ui_of(w, cx).is_some() {
        ui_mut(w, cx).menu.open = Some((key, at));
    }
}

/// Opens the person menu of `key`'s tile from the keyboard: under the tile's top left, as the web places it for a key.
pub fn open_from_key(w: WindowId, key: TileKey, cx: &mut App) {
    let at = ui_of(w, cx)
        .and_then(|u| u.menu.bounds.borrow().get(&key).copied())
        .map_or(point(px(0.0), px(0.0)), |b| b.bottom_left());
    open(w, key, at, cx);
}

/// Whether `w`'s person menu is open (tests).
pub fn is_open(w: WindowId, cx: &App) -> bool {
    ui_of(w, cx).is_some_and(|u| u.menu.open.is_some())
}

/// A person's volume (call/volume.ts: 100 when absent or not a volume), their voice's or their stream's.
pub fn volume_of(values: &VoiceValues, user: UserId, stream: bool) -> u32 {
    let map = if stream {
        &values.stream_volumes
    } else {
        &values.volumes
    };
    map.get(&user.hex())
        .copied()
        .filter(|v| dilla_client::features::voice_prefs::volume::is_volume(*v))
        .unwrap_or(100)
}

/// Sets `user`'s volume (or their stream's) in this device's voice settings; the call applies it.
pub fn set_volume(w: WindowId, user: UserId, stream: bool, percent: u32, cx: &mut App) {
    let Some(client) = client_of(w, cx) else {
        return;
    };
    let user = user.hex();
    let setting = if stream {
        Setting::StreamVolume { user, percent }
    } else {
        Setting::UserVolume { user, percent }
    };
    if let Err(e) = client.voice_prefs().set(setting) {
        log::warn!("voice: the volume was not saved: {e}");
    }
    cx.refresh_windows();
}

/// Mute for me: the volume to 0, or back to what it was before (100 when unknown).
pub fn toggle_mute_for_me(w: WindowId, user: UserId, stream: bool, cx: &mut App) {
    let Some(client) = client_of(w, cx) else {
        return;
    };
    let level = volume_of(&client.voice_prefs().values(), user, stream);
    let next = if level == 0 {
        ui_mut(w, cx)
            .menu
            .before_mute
            .remove(&(stream, user))
            .unwrap_or(100)
    } else {
        ui_mut(w, cx).menu.before_mute.insert((stream, user), level);
        0
    };
    set_volume(w, user, stream, next, cx);
}

/// The server's other voice channels a move can go to (Shell.tsx `rooms`): not `exclude`, not the AFK channel, each with
/// how many people are in it.
pub fn rooms(state: &AppState, channel: &Channel) -> Vec<(ChannelId, String, usize)> {
    let afk = state
        .ring
        .afk
        .get(&channel.community_id)
        .and_then(|s| s.channel);
    state
        .channels
        .iter()
        .filter(|c| {
            c.community_id == channel.community_id
                && c.kind == 1
                && c.id != channel.id
                && Some(c.id) != afk
        })
        .map(|c| (c.id, c.name.clone(), super::count(state, c.id)))
        .collect()
}

/// The person menu, while one is open on `channel`'s stage.
pub fn person_menu(w: WindowId, channel: &Channel, cx: &mut App) -> Option<AnyElement> {
    let ui = ui_of(w, cx)?;
    let (key, at) = ui.menu.open.clone()?;
    let state = ui.state.clone();
    let return_focus = ui.stage_focus.clone();
    let p = state
        .voice
        .participants(channel.id)
        .iter()
        .find(|p| p.device_id == key.device)
        .cloned()?;
    let values = client_of(w, cx).map(|c| c.voice_prefs().values());
    let own = p.own;
    let user = key.user;
    let stream = key.screen;
    let name = name_of(&state, Some(channel), user);
    let bot = is_bot(&state, Some(channel), user);
    let title = if own {
        tf("callStage.tile.you", &[("name", &name)])
    } else {
        name.clone()
    };
    let username = state
        .members
        .get(&channel.community_id)
        .and_then(|m| m.iter().find(|m| m.user_id == user))
        .map(|m| m.username.clone())
        .filter(|u| !u.is_empty())
        .unwrap_or_else(|| name.clone());
    let level = values.as_ref().map_or(100, |v| volume_of(v, user, stream));
    let tunable = !own;
    let person = !stream;
    let mut items: Vec<MenuItem> = Vec::new();
    let mut move_rooms = Vec::new();
    if own {
        let (mute_keys, deafen_keys) = keycaps();
        let (muted, deafened) = (state.voice.self_mute, state.voice.self_deaf);
        items.push(
            MenuItem::new(
                "mute",
                t(if muted {
                    "callStage.bar.unmute"
                } else {
                    "callStage.bar.mute"
                }),
            )
            .icon(if muted {
                IconName::Mic
            } else {
                IconName::MicOff
            })
            .hint(mute_keys),
        );
        items.push(
            MenuItem::new(
                "deafen",
                t(if deafened {
                    "callStage.bar.undeafen"
                } else {
                    "callStage.bar.deafen"
                }),
            )
            .icon(if deafened {
                IconName::Headphones
            } else {
                IconName::HeadphoneOff
            })
            .hint(deafen_keys),
        );
        items.push(MenuItem::new("profile", t("members.menu.profile")).icon(IconName::User));
        move_rooms = rooms(&state, channel);
        for (i, (id, room, n)) in move_rooms.iter().enumerate() {
            let mut item = MenuItem::new(format!("{MOVE}{}", id.hex()), room.clone())
                .icon(IconName::Volume2)
                .room()
                .hint(n.to_string());
            if i == 0 {
                item = item.sep().heading(t("callStage.menu.moveTo"));
            }
            items.push(item);
        }
        let mut leave = MenuItem::new("leave", t("callStage.menu.leave"))
            .icon(IconName::PhoneOff)
            .danger();
        if move_rooms.is_empty() {
            leave = leave.sep();
        }
        items.push(leave);
    } else {
        if person {
            items.push(MenuItem::new("profile", t("members.menu.profile")).icon(IconName::User));
            items.push(
                MenuItem::new("message", t("members.menu.message")).icon(IconName::MessageSquare),
            );
        }
        if tunable {
            items.push(
                MenuItem::new("mute-for-me", t("callStage.menu.muteForMe"))
                    .icon(IconName::VolumeX)
                    .check(level == 0),
            );
        }
        items.push(MenuItem::new("copy", t("callStage.menu.copy")).icon(IconName::Copy));
    }

    let where_ = tf("callStage.menu.where", &[("room", &channel.name)]);
    let mut head = div().flex().flex_col().child(
        div()
            .flex()
            .items_center()
            .gap(px(m::HEAD_GAP))
            .px(px(m::HEAD_PAD))
            .pt(px(m::HEAD_PAD))
            .pb(px(m::HEAD_PAD_B))
            .mb(px(m::HEAD_MB))
            .border_b_1()
            .border_color(color::HAIRLINE)
            .child(
                Avatar::new("voice-person-menu-avatar", name.clone())
                    .size(AvatarSize::Member)
                    .shape(if bot {
                        AvatarShape::Bot
                    } else {
                        AvatarShape::Circle
                    }),
            )
            .child(
                div()
                    .flex()
                    .flex_col()
                    .min_w_0()
                    .child(
                        line(title, text::SM, line_height(text::SM, 1.3), color::FG)
                            .weight(font::WEIGHT_DISPLAY)
                            .ellipsis(),
                    )
                    .child(line(
                        where_,
                        text::META,
                        line_height(text::META, 1.3),
                        color::FG_3,
                    )),
            ),
    );
    if tunable {
        let label = t(if stream {
            "callStage.menu.streamVolume"
        } else {
            "callStage.menu.volume"
        });
        let value = tf("callStage.menu.value", &[("n", &level.to_string())]);
        head = head.child(
            div()
                .flex()
                .flex_col()
                .gap(px(m::VOLUME_GAP))
                .px(px(m::VOLUME_PAD_X))
                .pt(px(m::VOLUME_PAD_T))
                .pb(px(m::VOLUME_PAD_B))
                .child(
                    div()
                        .flex()
                        .justify_between()
                        .child(line(
                            label,
                            text::XS,
                            line_height(text::XS, 1.5),
                            color::FG_2,
                        ))
                        .child(line(value, text::XS, line_height(text::XS, 1.5), color::FG)),
                )
                .child(
                    Slider::new("voice-person-menu-volume", level as i32, 0, 200)
                        .step(10)
                        .page(50)
                        .on_change(move |v, _commit, _window, cx| {
                            set_volume(w, user, stream, v.clamp(0, 200) as u32, cx);
                        }),
                ),
        );
    }
    let room_names: HashMap<String, (ChannelId, String)> = move_rooms
        .into_iter()
        .map(|(id, room, _)| (format!("{MOVE}{}", id.hex()), (id, room)))
        .collect();
    Some(
        ActionMenu::new(
            "voice-person-menu",
            tf("callStage.menu.label", &[("name", &name)]),
            items,
        )
        .no_trigger(return_focus.as_ref())
        .open(true)
        .at(at)
        .width(px(m::WIDTH))
        .head(head)
        .on_open_change(move |open, _w, cx| {
            if !open {
                if ui_of(w, cx).is_some() {
                    ui_mut(w, cx).menu.open = None;
                }
                cx.refresh_windows();
            }
        })
        .on_select(move |id, window, cx| {
            choose(w, id, user, stream, &username, &room_names, window, cx);
        })
        .into_any_element(),
    )
}

#[allow(clippy::too_many_arguments)]
fn choose(
    w: WindowId,
    id: &str,
    user: UserId,
    stream: bool,
    username: &str,
    rooms: &HashMap<String, (ChannelId, String)>,
    window: &mut Window,
    cx: &mut App,
) {
    let close = |cx: &mut App| {
        if ui_of(w, cx).is_some() {
            ui_mut(w, cx).menu.open = None;
        }
    };
    match id {
        "mute" => {
            close(cx);
            super::toggle_mute_in(w, cx);
        }
        "deafen" => {
            close(cx);
            super::toggle_deafen_in(w, cx);
        }
        "leave" => {
            close(cx);
            super::leave(w, cx);
        }
        "mute-for-me" => toggle_mute_for_me(w, user, stream, cx),
        "copy" => {
            close(cx);
            cx.write_to_clipboard(gpui::ClipboardItem::new_string(format!("@{username}")));
            dilla_desktop_ui::components::toast::show_toast_in(
                Some(w),
                tf("callStage.menu.copied", &[("name", username)]),
                cx,
            );
        }
        "profile" => {
            close(cx);
            let at = window.mouse_position();
            crate::screens::members::open_card(w, user, at, cx);
        }
        "message" => {
            close(cx);
            message(w, user, window, cx);
        }
        other => {
            close(cx);
            if let Some((channel, room)) = rooms.get(other) {
                move_to(w, *channel, room, cx);
            }
        }
    }
    cx.refresh_windows();
}

/// Move to (Shell.tsx `onMove`): joins `channel`'s call (which moves this device there), opens it, and says so.
pub fn move_to(w: WindowId, channel: ChannelId, room: &str, cx: &mut App) {
    super::join(channel, w, cx);
    super::open_voice(w, channel, cx);
    dilla_desktop_ui::components::toast::show_toast_in(
        Some(w),
        tf("callStage.menu.moved", &[("room", room)]),
        cx,
    );
}

/// Message (Shell.tsx `startDm`): opens (or makes) the DM with `user` and goes there.
fn message(w: WindowId, user: UserId, window: &mut Window, cx: &mut App) {
    let Some(client) = client_of(w, cx) else {
        return;
    };
    let answer = client.dms().open_dm(user);
    let handle = window.window_handle();
    cx.spawn(async move |cx| {
        if let Ok(Ok(channel)) = answer.await {
            let _ = gpui::AppContext::update_window(cx, handle, |_, window, cx| {
                crate::screens::dms::navigate(crate::screens::dms::Nav::Dm(channel), window, cx);
            });
        }
    })
    .detach();
}

/// The caption's volume chip (CallTile.tsx `d-call-tile__chip`): "{n} %" on another person's tile while it is not 100.
pub fn volume_chip(level: u32) -> AnyElement {
    div()
        .flex_none()
        .px(px(6.0))
        .rounded(dilla_desktop_ui::tokens::radius::MD)
        .bg(color::SURFACE_HI)
        .child(line(
            t("voice.volumeValue").replace("{n}", &level.to_string()),
            text::MICRO,
            line_height(text::MICRO, 1.5),
            color::FG_2,
        ))
        .into_any_element()
}

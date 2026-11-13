//! Area **voice** (Task 17): the voice channel row's people, the dock, the stage and the call controls.
//!
//! Ports packages/ui `VoiceChannelRow` (the people), `VoiceDock`, `CallStage`, `CallTile`, `CallControls` and the
//! keyboard of packages/web/src/screens/voice/shortcuts.ts. The hooks below are where `crate::slots` draws this area
//! inside the shell; they read the voice slice through [`VoiceUi`], which the shell keeps current with [`sync`].
//!
//! The media of the call runs in [`session::MediaSession`] (one per process); the stage's video tiles draw its
//! newest remote frames.

pub mod keys;
pub mod session;

use std::collections::HashMap;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::time::Duration;

use dilla_client::Client;
use dilla_client::calls::CallPhase;
use dilla_client::state::voice::{VoiceNotice, VoiceParticipant};
use dilla_client::state::{AppState, Channel, ChannelId, DeviceId, UserId};
use dilla_media::{ShareKind, ShareOptions, ShareResolution, StreamKey};
use gpui::{
    AnyElement, App, Div, ElementId, FocusHandle, Global, ImageSource, InteractiveElement,
    IntoElement, ParentElement, RenderImage, StatefulInteractiveElement, Styled, StyledImage,
    Window, WindowId, canvas, div, img, px, relative,
};

use dilla_desktop_ui::components::icon_button::{IconButton, IconButtonSize};
use dilla_desktop_ui::components::{Avatar, AvatarShape, AvatarSize};
use dilla_desktop_ui::text::line;
use dilla_desktop_ui::tokens::{color, font, layout, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::strings::{t, tf};
use crate::{tracked, tracked_box};

use session::MediaSession;

/// The area's sizes, logical px, named after the packages/ui CSS they come from (R21: no literal `px(..)` in screens).
mod m {
    /// VoiceChannelRow.css `__people`: indented 30px under the row, 8px off the right.
    pub const PEOPLE_INDENT: f32 = 30.0;
    pub const PEOPLE_END: f32 = 8.0;
    /// A person's row gap.
    pub const PERSON_GAP: f32 = 8.0;
    /// The speaking ring: 2px of the sidebar between the avatar and the 1.5px `--ok` ring.
    pub const RING_GAP: f32 = 2.0;
    pub const RING: f32 = 1.5;
    /// The camera, muted and deafened glyphs of a row and a tile caption.
    pub const GLYPH: f32 = 12.0;
    /// The LIVE tag's side padding.
    pub const LIVE_PAD_X: f32 = 4.0;
    /// VoiceDock.css: 8px from the sidebar's sides and the user panel, 10px in, 8px between its rows and parts, 2px
    /// between its buttons.
    pub const DOCK_INSET: f32 = 8.0;
    pub const DOCK_PAD: f32 = 10.0;
    pub const DOCK_GAP: f32 = 8.0;
    pub const DOCK_CONTROLS_GAP: f32 = 2.0;
    /// CallStage.css: 14px round the stage's title and body, 10px between tiles, the grid's 2px inset (the speaking
    /// edge), the focus layout's 188px film-strip tiles.
    pub const STAGE_PAD: f32 = 14.0;
    pub const TILE_GAP: f32 = 10.0;
    pub const GRID_PAD: f32 = 2.0;
    pub const FILM_TILE_W: f32 = 188.0;
    /// CallTile.css: the caption and the stats tag 8px from the corners, 6px in and 6px between parts; the edge 1px,
    /// 2px while speaking or selected.
    pub const CAPTION_INSET: f32 = 8.0;
    pub const CAPTION_GAP: f32 = 6.0;
    pub const CAPTION_PAD_X: f32 = 6.0;
    pub const TILE_EDGE: f32 = 1.0;
    pub const TILE_EDGE_STRONG: f32 = 2.0;
    /// The media notice: 12x10 in, a 12px gap, the 14px warning glyph.
    pub const NOTICE_PAD_X: f32 = 12.0;
    pub const NOTICE_PAD_Y: f32 = 10.0;
    pub const NOTICE_GAP: f32 = 12.0;
    pub const NOTICE_GLYPH: f32 = 14.0;
    /// CallControls.css: the 64px bar, 6px between buttons.
    pub const CONTROLS_H: f32 = 64.0;
    pub const CONTROLS_GAP: f32 = 6.0;
    /// The stream-quality menu: 320px wide, 72px over the stage's foot (the bar and its gap), 12px in, 8px gaps.
    pub const MENU_BOTTOM: f32 = 72.0;
    pub const MENU_W: f32 = 320.0;
    pub const MENU_PAD: f32 = 12.0;
    pub const MENU_GAP: f32 = 8.0;
}

/// What one window's voice hooks draw from: the newest state of its instance, its running client and that client's
/// call media, and the stage's own view state (which tile is focused, which is selected). One per instance window
/// ([`VoiceAreas`]): a call belongs to its instance, and another window's state never replaces it.
#[derive(Default)]
pub struct VoiceUi {
    pub state: Arc<AppState>,
    client: Option<Rc<Client>>,
    session: Option<Rc<MediaSession>>,
    /// The stage's focused stream (focus layout), `None`: the grid.
    pub focused: Option<TileKey>,
    /// The tile the arrows move to.
    pub selected: usize,
    stage_focus: Option<FocusHandle>,
    images: HashMap<StreamKey, (u64, Arc<RenderImage>)>,
    /// Each stream's frame rate, measured over the last second: `(at, frames then, fps)`.
    rates: HashMap<StreamKey, (std::time::Instant, u64, u32)>,
    /// The stream-quality menu (the share button's split half, or the picker before a share) is open.
    pub share_menu: bool,
    /// The stream quality the next (or the running) screen share uses.
    pub share_options: ShareOptions,
    video_seq: u64,
    /// What the ring area (18) was last told: where this device's call is, and the roster of the outgoing ring's call.
    told_in_call: Option<Option<String>>,
    told_roster: Option<(ChannelId, Vec<UserId>)>,
    /// A repaint loop runs for this window's live call ([`repaint_on_video`]).
    repainting: bool,
}

/// Every instance window's voice area, by window. A window that closes is let go at once (`windows::on_closed`: its
/// call left, its media session and client dropped).
#[derive(Default)]
pub struct VoiceAreas {
    windows: HashMap<WindowId, VoiceUi>,
    /// The repaint loops running now, one per live call ([`repaint_on_video`]).
    loops: usize,
}

impl Global for VoiceAreas {}

/// How many repaint loops run (one per window with a live call).
pub fn repaint_loops(cx: &App) -> usize {
    cx.try_global::<VoiceAreas>().map_or(0, |a| a.loops)
}

/// The window an element is drawn in, as the voice areas are keyed.
pub fn wid(window: &Window) -> WindowId {
    window.window_handle().window_id()
}

/// `window`'s voice area, once its shell has synced it.
pub fn ui_of(window: WindowId, cx: &App) -> Option<&VoiceUi> {
    cx.try_global::<VoiceAreas>()?.windows.get(&window)
}

/// `window`'s voice area, made on first use.
pub fn ui_mut(window: WindowId, cx: &mut App) -> &mut VoiceUi {
    cx.default_global::<VoiceAreas>()
        .windows
        .entry(window)
        .or_default()
}

/// The client `window`'s voice area acts through.
pub fn client_of(window: WindowId, cx: &App) -> Option<Rc<Client>> {
    ui_of(window, cx)?.client.clone()
}

/// The media session of `window`'s client.
pub fn session_of(window: WindowId, cx: &App) -> Option<Rc<MediaSession>> {
    ui_of(window, cx)?.session.clone()
}

/// The window whose call is this device's (one call per device), else the active window: where the app-wide mute and
/// deafen keys act.
fn call_window(cx: &App) -> Option<WindowId> {
    cx.try_global::<VoiceAreas>()
        .and_then(|a| {
            a.windows
                .iter()
                .find(|(_, u)| in_a_call(u))
                .map(|(w, _)| *w)
        })
        .or_else(|| cx.active_window().map(|w| w.window_id()))
}

/// Whether `ui`'s client is joining or in a call, by the client's own current state (not the window's last synced
/// one, which may not show a join that just started).
fn in_a_call(ui: &VoiceUi) -> bool {
    ui.client
        .as_ref()
        .map_or(ui.state.voice.call.phase != CallPhase::Idle, |c| {
            c.state().borrow().voice.call.phase != CallPhase::Idle
        })
}

/// Lets go of `window`'s voice area as it closes (`windows::on_closed`): a call still on there is left at once (the
/// media link is cut, Idle published, the Remove sent), its media session is dropped with the area, and its client is
/// no longer held.
pub fn window_closed(window: WindowId, cx: &mut App) {
    let Some(ui) = cx
        .try_global::<VoiceAreas>()
        .is_some()
        .then(|| cx.global_mut::<VoiceAreas>().windows.remove(&window))
        .flatten()
    else {
        return;
    };
    if in_a_call(&ui)
        && let Some(client) = &ui.client
    {
        drop(client.voice().leave());
    }
    drop(ui);
}

/// Lets go of the voice areas of windows that are gone and were not let go by the close hook.
fn prune(keep: WindowId, cx: &mut App) {
    let live: std::collections::HashSet<WindowId> =
        cx.windows().iter().map(|w| w.window_id()).collect();
    let Some(areas) = cx.try_global::<VoiceAreas>() else {
        return;
    };
    let gone: Vec<WindowId> = areas
        .windows
        .keys()
        .filter(|w| **w != keep && !live.contains(w))
        .copied()
        .collect();
    for w in gone {
        window_closed(w, cx);
    }
}

impl VoiceUi {
    /// The stage's focus handle, once the stage has drawn (the arrows and Escape act while it holds the focus).
    pub fn stage_focus_handle(&self) -> Option<FocusHandle> {
        self.stage_focus.clone()
    }
}

/// One stage tile: a person (their camera when it is on), or a person's screen share.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct TileKey {
    pub user: UserId,
    pub device: DeviceId,
    pub screen: bool,
}

/// Keeps `window`'s voice hooks current: its shell calls it with every new state (and the client once it runs). Starts
/// the voice area's following of the instance and the call's media session once per client of that window; another
/// window's sync never touches them. A repaint loop runs while the window's call is live.
pub fn sync(window: WindowId, state: Arc<AppState>, client: Option<&Rc<Client>>, cx: &mut App) {
    prune(window, cx);
    let fresh_client = {
        let ui = ui_mut(window, cx);
        match (client, &ui.client) {
            (Some(c), Some(old)) => !Rc::ptr_eq(c, old),
            (Some(_), None) => true,
            _ => false,
        }
    };
    if fresh_client && let Some(client) = client {
        client.voice().start();
        let session = MediaSession::start(client, session::default_sources())
            .map(Rc::new)
            .map_err(|e| log::warn!("voice: no media session: {e}"))
            .ok();
        let ui = ui_mut(window, cx);
        ui.client = Some(client.clone());
        ui.session = session;
        ui.told_in_call = None;
        ui.told_roster = None;
    }
    if let Some(client) = client {
        tell_ring(window, &state, client, cx);
    }
    let ui = ui_mut(window, cx);
    if ui.state.voice.call.phase != CallPhase::Idle && state.voice.call.phase == CallPhase::Idle {
        ui.focused = None;
        ui.selected = 0;
    }
    let live = state.voice.call.phase != CallPhase::Idle;
    ui.state = state;
    if live && !ui.repainting {
        ui.repainting = true;
        repaint_on_video(window, cx);
    }
}

/// What a repaint loop does on a tick.
enum Tick {
    /// The window or its call is gone: the loop ends.
    Stop,
    Wait,
    /// A new remote frame arrived: the window repaints.
    Repaint(u64),
}

/// Repaints `window` while new remote video frames arrive (at most 30 times a second), for as long as its call is live
/// and the window is open; then the loop ends.
fn repaint_on_video(window: WindowId, cx: &mut App) {
    cx.default_global::<VoiceAreas>().loops += 1;
    cx.spawn(async move |cx| {
        loop {
            cx.background_executor()
                .timer(Duration::from_millis(33))
                .await;
            let tick = cx.update(|cx| {
                let open = cx.windows().iter().any(|w| w.window_id() == window);
                let Some(ui) = ui_of(window, cx) else {
                    return Tick::Stop;
                };
                if !open || ui.state.voice.call.phase == CallPhase::Idle {
                    ui_mut(window, cx).repainting = false;
                    return Tick::Stop;
                }
                match ui.session.as_ref() {
                    Some(s) => {
                        let seq = s.video_seq.load(Ordering::Relaxed);
                        if seq == ui.video_seq {
                            Tick::Wait
                        } else {
                            Tick::Repaint(seq)
                        }
                    }
                    None => Tick::Wait,
                }
            });
            match tick {
                Tick::Stop => break,
                Tick::Wait => {}
                Tick::Repaint(seq) => cx.update(|cx| {
                    ui_mut(window, cx).video_seq = seq;
                    if let Some(handle) = cx.windows().into_iter().find(|w| w.window_id() == window)
                    {
                        let _ = handle.update(cx, |_, window, _| window.refresh());
                    }
                }),
            }
        }
        cx.update(|cx| {
            let a = cx.default_global::<VoiceAreas>();
            a.loops = a.loops.saturating_sub(1);
        });
    })
    .detach();
}

/// Where this device's call is, for the ring's busy text (`call.ring.busy`'s `{where}`): its channel's or DM's name while
/// it is joining or in a call, `None` outside one.
fn call_place(state: &AppState) -> Option<String> {
    let call = &state.voice.call;
    if call.phase == CallPhase::Idle {
        return None;
    }
    let channel = call.channel?;
    Some(
        state
            .channel(channel)
            .map(|c| c.name.clone())
            .or_else(|| state.dms.get(channel).map(|d| d.name.clone()))
            .unwrap_or_default(),
    )
}

/// The ring area (18) follows this device's call: rings turn busy (or not) when the call's place changes
/// (`Ring::set_in_call`), and the outgoing ring stops ringing whoever joined its call (`Ring::voice_roster`, op 50). Each
/// is told only on a change.
fn tell_ring(window: WindowId, state: &AppState, client: &Rc<Client>, cx: &mut App) {
    let place = call_place(state);
    let roster = state
        .ring
        .outgoing
        .as_ref()
        .and_then(|o| o.channel)
        .map(|channel| {
            let mut users: Vec<UserId> = state
                .voice
                .participants(channel)
                .iter()
                .map(|p| p.user_id)
                .collect();
            users.dedup();
            (channel, users)
        });
    let ui = ui_mut(window, cx);
    if ui.told_in_call.as_ref() != Some(&place) {
        ui.told_in_call = Some(place.clone());
        client.ring().set_in_call(place);
    }
    if roster.is_some() && ui.told_roster != roster {
        ui.told_roster = roster.clone();
        if let Some((channel, users)) = roster {
            client.ring().voice_roster(channel, &users);
        }
    }
}

fn voice(window: WindowId, cx: &App) -> Option<dilla_client::features::voice::Voice> {
    ui_of(window, cx)?.client.as_ref().map(|c| c.voice())
}

/// The name a person shows in voice: the member's display name, else the username, else the short id.
pub fn name_of(state: &AppState, channel: Option<&Channel>, user: UserId) -> String {
    channel
        .and_then(|c| state.members.get(&c.community_id))
        .and_then(|m| m.iter().find(|m| m.user_id == user))
        .map(|m| {
            if m.display.is_empty() {
                m.username.clone()
            } else {
                m.display.clone()
            }
        })
        .or_else(|| (state.account.user_id == Some(user)).then(|| state.account.username.clone()))
        .unwrap_or_else(|| user.hex()[..8].to_owned())
}

fn is_bot(state: &AppState, channel: Option<&Channel>, user: UserId) -> bool {
    channel
        .and_then(|c| state.members.get(&c.community_id))
        .and_then(|m| m.iter().find(|m| m.user_id == user))
        .is_some_and(|m| m.kind == 1)
}

/// One person of a voice channel, their devices grouped (VoiceChannelRow.tsx `groupPeople`).
#[derive(Clone, Debug, PartialEq)]
pub struct Person {
    pub user: UserId,
    pub name: String,
    pub bot: bool,
    pub muted: bool,
    pub deafened: bool,
    pub video: bool,
    pub screen: bool,
    pub speaking: bool,
}

/// The people of `channel`'s call, in the roster's order, each user once.
pub fn people(state: &AppState, channel: &Channel) -> Vec<Person> {
    let mut out: Vec<(UserId, Vec<&VoiceParticipant>)> = Vec::new();
    for p in state.voice.participants(channel.id) {
        match out.iter_mut().find(|(u, _)| *u == p.user_id) {
            Some((_, devices)) => devices.push(p),
            None => out.push((p.user_id, vec![p])),
        }
    }
    out.into_iter()
        .map(|(user, devices)| Person {
            user,
            name: name_of(state, Some(channel), user),
            bot: is_bot(state, Some(channel), user),
            muted: devices.iter().all(|d| d.muted() || d.deafened()),
            deafened: devices.iter().all(|d| d.deafened()),
            video: devices.iter().any(|d| d.video()),
            screen: devices.iter().any(|d| d.screen()),
            speaking: devices
                .iter()
                .any(|d| state.voice.speaking.contains(&d.device_id)),
        })
        .collect()
}

/// How many people are in `channel`'s call (the voice row's count, Task 8's `in_voice`).
pub fn count(state: &AppState, channel: ChannelId) -> usize {
    let mut users: Vec<UserId> = state
        .voice
        .participants(channel)
        .iter()
        .map(|p| p.user_id)
        .collect();
    users.dedup();
    users.len()
}

/// Whether this device is in `channel`'s call (the voice row's green glyph).
pub fn joined(state: &AppState, channel: ChannelId) -> bool {
    state.voice.call.channel == Some(channel) && state.voice.call.phase == CallPhase::In
}

/// The clients of other instance windows whose call is live (one call per device: a join elsewhere leaves them).
pub fn other_calls(window: WindowId, cx: &App) -> Vec<Rc<Client>> {
    let own = client_of(window, cx);
    cx.try_global::<VoiceAreas>()
        .map(|a| {
            a.windows
                .iter()
                .filter(|(w, u)| **w != window && in_a_call(u))
                .filter_map(|(_, u)| u.client.clone())
                .filter(|c| own.as_ref().is_none_or(|o| !Rc::ptr_eq(o, c)))
                .collect()
        })
        .unwrap_or_default()
}

/// Leaves the call of every other instance window (one call per device, as the web has one per tab): what a join or an
/// answered ring in `window` does first.
pub fn leave_other_calls(window: WindowId, cx: &mut App) {
    after_leaving_other_calls(window, cx, || {});
}

/// Leaves the call of every other instance window, then runs `then` once each leave has answered. A join started while
/// another instance's leave is still committing races it in the call group (`E_COMMIT_INVALID`), so the join waits.
/// The leaves answer on their clients' runtimes; the wait runs on a thread of its own, never on the UI's.
pub fn after_leaving_other_calls(
    window: WindowId,
    cx: &mut App,
    then: impl FnOnce() + Send + 'static,
) {
    let leaves: Vec<_> = other_calls(window, cx)
        .into_iter()
        .map(|c| c.voice().leave())
        .collect();
    if leaves.is_empty() {
        then();
        return;
    }
    let waited = std::thread::Builder::new()
        .name("dilla-voice-handover".into())
        .spawn(move || {
            for leave in leaves {
                let _ = leave.blocking_recv();
            }
            then();
        });
    if let Err(e) = waited {
        log::warn!("voice: the join after leaving another instance's call did not start: {e}");
    }
}

/// Joins `channel`'s call through `window`'s own client (the voice row's join, the stage's join), once a call another
/// instance window is in has been left.
pub fn join(channel: ChannelId, window: WindowId, cx: &mut App) {
    let Some(v) = voice(window, cx) else { return };
    after_leaving_other_calls(window, cx, move || drop(v.join(channel)));
}

/// Leaves `window`'s call; the focus goes back to the call's voice row (leave-reload.spec.ts).
pub fn leave(window: WindowId, cx: &mut App) {
    if let Some(v) = voice(window, cx) {
        drop(v.leave());
    }
}

/// Mutes or unmutes this device's call (the window it is in, else the active one): the app-wide key.
pub fn toggle_mute(cx: &mut App) {
    if let Some(w) = call_window(cx) {
        toggle_mute_in(w, cx);
    }
}

/// Mutes or unmutes `window`'s call.
pub fn toggle_mute_in(window: WindowId, cx: &mut App) {
    if let (Some(v), Some(ui)) = (voice(window, cx), ui_of(window, cx)) {
        let s = &ui.state.voice;
        v.set_muted(!(s.self_mute || s.self_deaf));
    }
}

/// Deafens or undeafens this device's call (the window it is in, else the active one): the app-wide key.
pub fn toggle_deafen(cx: &mut App) {
    if let Some(w) = call_window(cx) {
        toggle_deafen_in(w, cx);
    }
}

/// Deafens or undeafens `window`'s call.
pub fn toggle_deafen_in(window: WindowId, cx: &mut App) {
    if let (Some(v), Some(ui)) = (voice(window, cx), ui_of(window, cx)) {
        v.set_deafened(!ui.state.voice.self_deaf);
    }
}

pub fn toggle_share(kind: ShareKind, window: WindowId, cx: &mut App) {
    let Some(ui) = ui_of(window, cx) else { return };
    let on = match kind {
        ShareKind::Camera => !ui.state.voice.share.camera,
        ShareKind::Screen => !ui.state.voice.share.screen,
    };
    if let Some(s) = &ui.session {
        s.share(kind, on, ui.share_options);
    }
}

// ---- the voice row's people ---------------------------------------------------------------------------------------

/// The people in a voice channel, nested under its row (`slots::channel_row_extras`): 26px rows, 30px in, the avatar
/// ringed while they speak, then the name, the camera and LIVE, and the muted or deafened glyph at the right.
pub fn channel_people(channel: &Channel, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let state = ui_of(wid(window), cx)?.state.clone();
    if channel.kind != 1 {
        return None;
    }
    let people = people(&state, channel);
    if people.is_empty() {
        return None;
    }
    let rows = people.into_iter().map(|p| person_row(&p));
    Some(
        div()
            .flex()
            .flex_col()
            .pl(px(m::PEOPLE_INDENT))
            .pr(px(m::PEOPLE_END))
            .children(rows)
            .into_any_element(),
    )
}

fn person_row(p: &Person) -> Div {
    let avatar = Avatar::new(
        ElementId::Name(format!("voice-person-{}", p.user.hex()).into()),
        p.name.clone(),
    )
    .size(AvatarSize::Voice)
    .shape(if p.bot {
        AvatarShape::Bot
    } else {
        AvatarShape::Circle
    });
    // Speaking is only a ring on the avatar: 2px of the sidebar, then 1.5px of --ok.
    let ring = div()
        .flex_none()
        .rounded(radius::AVATAR)
        .border(px(m::RING_GAP))
        .border_color(if p.speaking {
            color::BG_2
        } else {
            gpui::transparent_black()
        })
        .m(px(-m::RING_GAP))
        .child(avatar);
    let ring = div()
        .flex_none()
        .rounded(radius::AVATAR)
        .border(px(m::RING))
        .border_color(if p.speaking {
            color::OK
        } else {
            gpui::transparent_black()
        })
        .m(px(-m::RING))
        .child(ring);
    let glyph = if p.deafened {
        Some(IconName::HeadphoneOff)
    } else if p.muted {
        Some(IconName::MicOff)
    } else {
        None
    };
    div()
        .flex()
        .items_center()
        .gap(px(m::PERSON_GAP))
        .h(layout::ROW_H_SUB)
        .min_w_0()
        .child(ring)
        .child(div().flex_initial().min_w_0().overflow_hidden().child(line(
            p.name.clone(),
            text::UI,
            15.0,
            color::FG_STRONG,
        )))
        .child(div().flex_1())
        .when_some(p.video.then_some(()), |d, ()| {
            d.child(
                Icon::new(IconName::Video)
                    .size(px(m::GLYPH))
                    .color(color::FG_2),
            )
        })
        .when_some(p.screen.then_some(()), |d, ()| d.child(live_tag()))
        .when_some(glyph, |d, g| {
            d.child(Icon::new(g).size(px(m::GLYPH)).color(color::DANGER))
        })
}

fn live_tag() -> Div {
    div()
        .flex_none()
        .px(px(m::LIVE_PAD_X))
        .rounded(radius::TAG)
        .bg(color::DANGER)
        .child(line("LIVE", text::MICRO, 14.0, color::DANGER_INK).weight(font::WEIGHT_LABEL))
}

use gpui::prelude::FluentBuilder as _;

// ---- the dock -----------------------------------------------------------------------------------------------------

/// The voice dock over the user panel (`slots::dock`, first): the call's channel and its state, camera, share and
/// leave. Shown from the join's start to the leave.
pub fn dock(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let w = wid(window);
    let state = ui_of(w, cx)?.state.clone();
    let call = &state.voice.call;
    if call.phase == CallPhase::Idle {
        return None;
    }
    let channel = call.channel.and_then(|c| state.channel(c));
    let name = channel.map_or_else(String::new, |c| c.name.clone());
    let where_ = match call.phase {
        CallPhase::In => t("voice.dock.pingNone").to_owned(),
        CallPhase::WaitingForRemove => t("voice.announce.rejoining").to_owned(),
        _ => t("voice.dock.connecting").to_owned(),
    };
    let share = state.voice.share;
    let controls = div()
        .flex()
        .flex_none()
        .items_center()
        .gap(px(m::DOCK_CONTROLS_GAP))
        .child(tracked_box(
            "voice-dock-camera",
            IconButton::new(
                "voice-dock-camera-b",
                if share.camera {
                    IconName::VideoOff
                } else {
                    IconName::Video
                },
                t("callStage.bar.camera"),
            )
            .size(IconButtonSize::S28)
            .pressed(share.camera)
            .on_click(|_, window, cx| toggle_share(ShareKind::Camera, wid(window), cx)),
        ))
        .child(tracked_box(
            "voice-dock-share",
            IconButton::new(
                "voice-dock-share-b",
                if share.screen {
                    IconName::MonitorX
                } else {
                    IconName::MonitorUp
                },
                t("callStage.bar.share"),
            )
            .size(IconButtonSize::S28)
            .pressed(share.screen)
            .on_click(|_, window, cx| toggle_share(ShareKind::Screen, wid(window), cx)),
        ))
        .child(tracked_box(
            "voice-leave",
            IconButton::new("voice-leave-b", IconName::PhoneOff, t("voice.leave"))
                .size(IconButtonSize::S28)
                .on_click(|_, window, cx| leave(wid(window), cx)),
        ));
    let status = div()
        .flex()
        .flex_col()
        .flex_1()
        .min_w_0()
        .child(line(name, text::UI, 15.6, color::OK_TEXT).weight(font::WEIGHT_LABEL))
        .child(line(where_, text::META, 13.65, color::FG_3));
    let card = div()
        .flex()
        .flex_col()
        .gap(px(m::DOCK_GAP))
        .mx(px(m::DOCK_INSET))
        .mb(px(m::DOCK_INSET))
        .p(px(m::DOCK_PAD))
        .min_w_0()
        .bg(color::OK_SOFT)
        .border_1()
        .border_color(color::OK_SOFT_BORDER)
        .rounded(radius::LG)
        .child(
            div()
                .flex()
                .items_center()
                .gap(px(m::DOCK_GAP))
                .min_w_0()
                .child(status)
                .child(controls),
        )
        .child(ptt_listener(w));
    Some(tracked_box("voice-dock", card).into_any_element())
}

/// Push to talk while the window is focused: the person's chord (`voice.pttChord`) held opens the microphone, its
/// release closes it. Listened for while a call is on (the dock is drawn).
fn ptt_listener(w: WindowId) -> impl IntoElement {
    canvas(
        |_, _, _| {},
        move |_, _, window, cx| {
            let chord = ui_of(w, cx)
                .and_then(|u| u.client.as_ref())
                .and_then(|c| c.prefs().get::<String>("voice.pttChord"));
            let Some(chord) = chord else { return };
            let key = keys::ptt_key(&chord);
            let down = key.clone();
            window.on_key_event(move |e: &gpui::KeyDownEvent, phase, _, cx| {
                if phase == gpui::DispatchPhase::Capture
                    && !e.is_held
                    && e.keystroke.key.eq_ignore_ascii_case(&down)
                    && let Some(v) = voice(w, cx)
                {
                    v.set_ptt_held(true);
                }
            });
            window.on_key_event(move |e: &gpui::KeyUpEvent, phase, _, cx| {
                if phase == gpui::DispatchPhase::Capture
                    && e.keystroke.key.eq_ignore_ascii_case(&key)
                    && let Some(v) = voice(w, cx)
                {
                    v.set_ptt_held(false);
                }
            });
        },
    )
    .size_0()
}

// ---- the stage ----------------------------------------------------------------------------------------------------

/// The tiles of `channel`'s stage: each device's person tile, then a screen tile for each device that shares.
pub fn tiles(state: &AppState, channel: ChannelId) -> Vec<TileKey> {
    let mut out = Vec::new();
    for p in state.voice.participants(channel) {
        out.push(TileKey {
            user: p.user_id,
            device: p.device_id,
            screen: false,
        });
    }
    for p in state.voice.participants(channel) {
        if p.screen() {
            out.push(TileKey {
                user: p.user_id,
                device: p.device_id,
                screen: true,
            });
        }
    }
    out
}

/// The grid's band (CallStage.css): up to two tiles 46% (at most 560px) wide, up to four 46% (420px), more 31.5%
/// (340px).
pub fn band(n: usize) -> (f32, f32) {
    match n {
        0..=2 => (0.46, 560.0),
        3..=4 => (0.46, 420.0),
        _ => (0.315, 340.0),
    }
}

/// The stage of a voice channel, which takes over the conversation area: its title bar, the tiles (a grid, or the
/// focused stream over a strip of the others) and the call controls. `None` for any other channel.
pub fn stage(channel: &Channel, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    if channel.kind != 1 {
        return None;
    }
    let w = wid(window);
    if ui_of(w, cx)?.stage_focus.is_none() {
        let handle = cx.focus_handle();
        ui_mut(w, cx).stage_focus = Some(handle);
    }
    let images = refresh_images(window, cx);
    let stats: HashMap<StreamKey, (u32, u32)> = ui_of(w, cx)
        .map(|u| {
            let videos = u.session.as_ref().map(|s| s.videos()).unwrap_or_default();
            u.rates
                .iter()
                .filter_map(|(k, (_, _, fps))| Some((k.clone(), (videos.get(k)?.height, *fps))))
                .collect()
        })
        .unwrap_or_default();
    let menu = ui_of(w, cx).and_then(share_menu);
    let ui = ui_of(w, cx)?;
    let state = ui.state.clone();
    let focus = ui.stage_focus.clone()?;
    let keys = tiles(&state, channel.id);
    let focused = ui.focused.clone().filter(|k| keys.contains(k));
    let selected = ui.selected.min(keys.len().saturating_sub(1));
    let joined = joined(&state, channel.id);
    let tile_views: Vec<AnyElement> = keys
        .iter()
        .enumerate()
        .map(|(i, k)| {
            tile(
                &state,
                channel,
                k,
                (i == selected, focused.as_ref()),
                &images,
                &stats,
            )
        })
        .collect();
    let body = if keys.is_empty() {
        div()
            .flex_1()
            .flex()
            .items_center()
            .justify_center()
            .child(line(t("voice.empty"), text::UI, 15.6, color::FG_2))
            .into_any_element()
    } else if let Some(f) = &focused {
        let at = keys.iter().position(|k| k == f).unwrap_or(0);
        let mut views = tile_views;
        let main = views.remove(at);
        div()
            .flex_1()
            .min_h_0()
            .flex()
            .flex_col()
            .gap(px(m::TILE_GAP))
            .child(
                div().flex_1().min_h_0().flex().justify_center().child(
                    div()
                        .h_full()
                        .aspect_ratio(16.0 / 9.0)
                        .max_w_full()
                        .child(main),
                ),
            )
            .child(div().flex().justify_center().gap(px(m::TILE_GAP)).children(
                views.into_iter().map(|v| {
                    div()
                        .flex_none()
                        .w(px(m::FILM_TILE_W))
                        .aspect_ratio(16.0 / 9.0)
                        .child(v)
                }),
            ))
            .into_any_element()
    } else {
        let (share, max) = band(keys.len());
        div()
            .flex_1()
            .min_h_0()
            .flex()
            .flex_wrap()
            .content_center()
            .items_center()
            .justify_center()
            .gap(px(m::TILE_GAP))
            .p(px(m::GRID_PAD))
            .children(tile_views.into_iter().map(|v| {
                div()
                    .flex_none()
                    .w(relative(share))
                    .max_w(px(max))
                    .aspect_ratio(16.0 / 9.0)
                    .child(v)
            }))
            .into_any_element()
    };
    let title = div()
        .flex_none()
        .h(layout::BAR)
        .px(px(m::STAGE_PAD))
        .flex()
        .items_center()
        .border_b_1()
        .border_color(color::HAIRLINE)
        .child(line(channel.name.clone(), text::MD, 18.0, color::FG).weight(font::WEIGHT_LABEL));
    let n = keys.len();
    let stage = div()
        .id("voice-stage")
        .relative()
        .track_focus(&focus)
        .size_full()
        .flex()
        .flex_col()
        .bg(color::BG)
        .on_key_down(move |e, window, cx| {
            stage_key(&e.keystroke.key, n, wid(window), cx);
            window.refresh();
        })
        .child(title)
        .child(notice(&state))
        .child(
            div()
                .flex_1()
                .min_h_0()
                .flex()
                .flex_col()
                .p(px(m::STAGE_PAD))
                .child(body),
        )
        .child(controls(&state, joined, channel.id))
        .children(menu);
    let _ = window;
    Some(tracked("voice-stage-root", stage).into_any_element())
}

/// The stage's keys: the arrows move between tiles, Enter focuses the selected one, Escape leaves focus mode.
fn stage_key(key: &str, n: usize, window: WindowId, cx: &mut App) {
    let Some(ui) = ui_of(window, cx) else { return };
    if n == 0 {
        return;
    }
    let channel = ui.state.voice.call.channel;
    let ui = ui_mut(window, cx);
    match key {
        "right" | "down" => ui.selected = (ui.selected + 1).min(n - 1),
        "left" | "up" => ui.selected = ui.selected.saturating_sub(1),
        "enter" | "space" => {
            if let Some(ch) = channel {
                ui.focused = tiles(&ui.state, ch).get(ui.selected).cloned();
            }
        }
        "escape" => {
            ui.focused = None;
            ui.share_menu = false;
        }
        _ => {}
    }
}

/// The newest remote frames as images, rebuilt only for streams with a new frame.
/// A replaced or ended stream's image leaves the GPU atlas (`drop_image`): without that every frame would stay there.
fn refresh_images(window: &mut Window, cx: &mut App) -> HashMap<StreamKey, Arc<RenderImage>> {
    let w = wid(window);
    let Some(videos) = ui_of(w, cx)
        .and_then(|u| u.session.as_ref())
        .map(|s| s.videos())
    else {
        return HashMap::new();
    };
    let mut stale: Vec<Arc<RenderImage>> = Vec::new();
    let ui = ui_mut(w, cx);
    let now = std::time::Instant::now();
    ui.rates.retain(|k, _| videos.contains_key(k));
    for (k, f) in &videos {
        let rate = ui.rates.entry(k.clone()).or_insert((now, f.frames, 0));
        let dt = now.duration_since(rate.0).as_secs_f32();
        if dt >= 1.0 {
            rate.2 = ((f.frames.saturating_sub(rate.1)) as f32 / dt).round() as u32;
            *rate = (now, f.frames, rate.2);
        }
    }
    ui.images.retain(|k, (_, image)| {
        let keep = videos.contains_key(k);
        if !keep {
            stale.push(image.clone());
        }
        keep
    });
    for (k, f) in &videos {
        if ui.images.get(k).is_some_and(|(n, _)| *n == f.frames) {
            continue;
        }
        // gpui draws BGRA.
        let mut bgra = f.rgba.as_ref().clone();
        for px in bgra.as_chunks_mut::<4>().0 {
            px.swap(0, 2);
        }
        if let Some(buf) = image::RgbaImage::from_raw(f.width, f.height, bgra) {
            let frame = image::Frame::new(buf);
            if let Some((_, old)) = ui.images.insert(
                k.clone(),
                (f.frames, Arc::new(RenderImage::new(vec![frame]))),
            ) {
                stale.push(old);
            }
        }
    }
    let out = ui
        .images
        .iter()
        .map(|(k, (_, i))| (k.clone(), i.clone()))
        .collect();
    for image in stale {
        cx.drop_image(image, Some(window));
    }
    out
}

fn tile(
    state: &AppState,
    channel: &Channel,
    k: &TileKey,
    (selected, focused): (bool, Option<&TileKey>),
    images: &HashMap<StreamKey, Arc<RenderImage>>,
    stats: &HashMap<StreamKey, (u32, u32)>,
) -> AnyElement {
    let p = state
        .voice
        .participants(channel.id)
        .iter()
        .find(|p| p.device_id == k.device);
    let name = name_of(state, Some(channel), k.user);
    let speaking = !k.screen && state.voice.speaking.contains(&k.device);
    let stream = StreamKey {
        device: k.device.hex(),
        kind: if k.screen {
            ShareKind::Screen
        } else {
            ShareKind::Camera
        },
    };
    let video = images.get(&stream).cloned();
    let stat = video
        .is_some()
        .then(|| stats.get(&stream))
        .flatten()
        .map(|(h, fps)| frame_stat(*h, *fps));
    let muted = p.is_some_and(|p| p.muted() || p.deafened());
    let deafened = p.is_some_and(VoiceParticipant::deafened);
    let is_focused = focused == Some(k);
    let face = if focused.is_some() && !is_focused {
        AvatarSize::Confirm
    } else {
        AvatarSize::Lg
    };
    let media = match video {
        Some(image) => img(ImageSource::Render(image))
            .size_full()
            .object_fit(gpui::ObjectFit::Contain)
            .into_any_element(),
        None if k.screen => div()
            .size_full()
            .flex()
            .items_center()
            .justify_center()
            .child(line(
                t("voice.state.screen"),
                text::MICRO,
                14.0,
                color::FG_4,
            ))
            .into_any_element(),
        None => div()
            .size_full()
            .flex()
            .items_center()
            .justify_center()
            .child(
                Avatar::new(
                    ElementId::Name(format!("voice-tile-face-{}", k.device.hex()).into()),
                    name.clone(),
                )
                .size(face),
            )
            .into_any_element(),
    };
    let caption = div()
        .absolute()
        .left(px(m::CAPTION_INSET))
        .bottom(px(m::CAPTION_INSET))
        .flex()
        .items_center()
        .gap(px(m::CAPTION_GAP))
        .px(px(m::CAPTION_PAD_X))
        .rounded(radius::MD)
        .bg(color::BG_2)
        .child(line(name, text::XS, 14.0, color::FG).weight(font::WEIGHT_LABEL))
        .when(deafened, |d| {
            d.child(
                Icon::new(IconName::HeadphoneOff)
                    .size(px(m::GLYPH))
                    .color(color::DANGER),
            )
        })
        .when(muted && !deafened, |d| {
            d.child(
                Icon::new(IconName::MicOff)
                    .size(px(m::GLYPH))
                    .color(color::DANGER),
            )
        });
    let key = k.clone();
    div()
        .id(ElementId::Name(
            format!(
                "voice-tile-{}{}",
                k.device.hex(),
                if k.screen { "-screen" } else { "" }
            )
            .into(),
        ))
        .relative()
        .size_full()
        .overflow_hidden()
        .rounded(radius::XL)
        .bg(color::STAGE_TILE)
        .border(if speaking || selected {
            px(m::TILE_EDGE_STRONG)
        } else {
            px(m::TILE_EDGE)
        })
        .border_color(if speaking {
            color::OK
        } else if selected {
            color::ACCENT
        } else {
            color::HAIRLINE
        })
        .cursor_pointer()
        .on_click(move |_, window, cx| {
            let w = wid(window);
            if ui_of(w, cx).is_some() {
                let ui = ui_mut(w, cx);
                ui.focused = if ui.focused.as_ref() == Some(&key) {
                    None
                } else {
                    Some(key.clone())
                };
                window.refresh();
            }
        })
        .child(media)
        .child(caption)
        .when_some(stat, |d, stat| {
            d.child(
                div()
                    .absolute()
                    .top(px(m::CAPTION_INSET))
                    .right(px(m::CAPTION_INSET))
                    .px(px(m::CAPTION_PAD_X))
                    .rounded(radius::MD)
                    .bg(color::BG_2)
                    .child(line(stat, text::MICRO, 14.0, color::FG_2)),
            )
        })
        .into_any_element()
}

/// The media notice over the tiles, until dismissed.
fn notice(state: &AppState) -> AnyElement {
    let key = match state.voice.notice {
        None => return div().into_any_element(),
        Some(VoiceNotice::MicLost) => "voice.micMissing",
        Some(VoiceNotice::SpeakerLost) => "voice.outputFailed",
        Some(VoiceNotice::DevicesChanged) => "voice.outputFailed",
        Some(VoiceNotice::EnhancedUnavailable | VoiceNotice::EnhancedFailed) => "voice.micFailed",
        Some(VoiceNotice::ShareCancelled) => "voice.why.noShare",
        Some(VoiceNotice::ShareWithoutSound) => "voice.shareAudio.none.application",
        Some(VoiceNotice::MediaFailed) => "voice.failed",
    };
    div()
        .id("voice-notice")
        .mx(px(m::STAGE_PAD))
        .mt(px(m::STAGE_PAD))
        .px(px(m::NOTICE_PAD_X))
        .py(px(m::NOTICE_PAD_Y))
        .flex()
        .items_center()
        .gap(px(m::NOTICE_GAP))
        .bg(color::SURFACE_2)
        .border_1()
        .border_color(color::LINE_STRONG)
        .rounded(radius::MD)
        .child(
            Icon::new(IconName::TriangleAlert)
                .size(px(m::NOTICE_GLYPH))
                .color(color::WARN),
        )
        .child(
            div()
                .flex_1()
                .child(line(t(key), text::UI, 15.6, color::FG)),
        )
        .child(
            IconButton::new(
                "voice-notice-close",
                IconName::X,
                t("voice.unsupported.close"),
            )
            .size(IconButtonSize::S28)
            .on_click(|_, window, cx| {
                if let Some(v) = voice(wid(window), cx) {
                    v.set_notice(None);
                }
            }),
        )
        .into_any_element()
}

/// The call controls under the stage (CallControls, the 64px bar): mute, deafen, camera, share and leave while in the
/// call; join before.
fn controls(state: &AppState, joined: bool, channel: ChannelId) -> AnyElement {
    let bar = div()
        .flex_none()
        .h(px(m::CONTROLS_H))
        .flex()
        .items_center()
        .justify_center()
        .gap(px(m::CONTROLS_GAP))
        .border_t_1()
        .border_color(color::HAIRLINE);
    if !joined {
        let busy = state.voice.call.channel == Some(channel);
        return bar
            .child(tracked_box(
                "voice-join",
                IconButton::new("voice-join-b", IconName::Phone, t("voice.join"))
                    .size(IconButtonSize::S40)
                    .pressed(busy)
                    .on_click(move |_, window, cx| join(channel, wid(window), cx)),
            ))
            .into_any_element();
    }
    let v = &state.voice;
    let muted = v.self_mute || v.self_deaf;
    bar.child(tracked_box(
        "voice-mute",
        IconButton::new(
            "voice-mute-b",
            if muted {
                IconName::MicOff
            } else {
                IconName::Mic
            },
            t("voice.mute"),
        )
        .size(IconButtonSize::S40)
        .pressed(muted)
        .on_click(|_, window, cx| toggle_mute_in(wid(window), cx)),
    ))
    .child(tracked_box(
        "voice-deafen",
        IconButton::new(
            "voice-deafen-b",
            if v.self_deaf {
                IconName::HeadphoneOff
            } else {
                IconName::Headphones
            },
            t("voice.deafen"),
        )
        .size(IconButtonSize::S40)
        .pressed(v.self_deaf)
        .on_click(|_, window, cx| toggle_deafen_in(wid(window), cx)),
    ))
    .child(tracked_box(
        "voice-camera",
        IconButton::new(
            "voice-camera-b",
            if v.share.camera {
                IconName::VideoOff
            } else {
                IconName::Video
            },
            t("callStage.bar.camera"),
        )
        .size(IconButtonSize::S40)
        .pressed(v.share.camera)
        .on_click(|_, window, cx| toggle_share(ShareKind::Camera, wid(window), cx)),
    ))
    .child(share_split(v.share.screen))
    .child(tracked_box(
        "voice-stage-leave",
        IconButton::new("voice-stage-leave-b", IconName::PhoneOff, t("voice.leave"))
            .size(IconButtonSize::S40)
            .on_click(|_, window, cx| leave(wid(window), cx)),
    ))
    .into_any_element()
}

/// The share button and its split half, the stream-quality menu (CallControls' split button).
fn share_split(sharing: bool) -> Div {
    div()
        .flex()
        .items_center()
        .child(tracked_box(
            "voice-share",
            IconButton::new(
                "voice-share-b",
                if sharing {
                    IconName::MonitorX
                } else {
                    IconName::MonitorUp
                },
                t(if sharing {
                    "callStage.bar.stopShare"
                } else {
                    "callStage.bar.share"
                }),
            )
            .size(IconButtonSize::S40)
            .pressed(sharing)
            .on_click(|_, window, cx| toggle_share(ShareKind::Screen, wid(window), cx)),
        ))
        .child(tracked_box(
            "voice-share-quality",
            IconButton::new(
                "voice-share-quality-b",
                IconName::ChevronUp,
                t("callStage.share.quality"),
            )
            .size(IconButtonSize::S28)
            .on_click(|_, window, cx| {
                let w = wid(window);
                if ui_of(w, cx).is_some() {
                    let ui = ui_mut(w, cx);
                    ui.share_menu = !ui.share_menu;
                    window.refresh();
                }
            }),
        ))
}

/// The stream-quality menu (SharePicker's options on a desktop: resolution and frame rate, with the upload estimate;
/// no surface or sound choice, share-options.ts `shareSupport` with `desktop`). Before a share its button goes live;
/// while sharing a change applies to the running share.
fn share_menu(ui: &VoiceUi) -> Option<AnyElement> {
    if !ui.share_menu {
        return None;
    }
    let o = ui.share_options;
    let sharing = ui.state.voice.share.screen;
    let (mbps, heavy) = o.estimate(None);
    let estimate = tf(
        if heavy {
            "callStage.share.estimateHeavy"
        } else {
            "callStage.share.estimate"
        },
        &[("mbps", &mbps)],
    );
    let resolutions: Vec<(&str, &str)> = ShareResolution::ALL
        .iter()
        .map(|r| {
            (
                r.name(),
                if *r == ShareResolution::Source {
                    t("callStage.share.source")
                } else {
                    r.name()
                },
            )
        })
        .collect();
    let pick = move |next: ShareOptions, window: &mut Window, cx: &mut App| {
        let w = wid(window);
        if ui_of(w, cx).is_none() {
            return;
        }
        let ui = ui_mut(w, cx);
        ui.share_options = next;
        if ui.state.voice.share.screen
            && let Some(s) = &ui.session
        {
            s.quality(next);
        }
        window.refresh();
    };
    let label = |key: &'static str| line(t(key), text::XS, 14.0, color::FG_2);
    let panel = div()
        .id("voice-share-menu")
        .absolute()
        .bottom(px(m::MENU_BOTTOM))
        .w(px(m::MENU_W))
        .p(px(m::MENU_PAD))
        .flex()
        .flex_col()
        .gap(px(m::MENU_GAP))
        .bg(color::SURFACE_2)
        .border_1()
        .border_color(color::LINE_STRONG)
        .rounded(radius::MENU)
        .child(
            line(
                t(if sharing {
                    "callStage.share.titleSharing"
                } else {
                    "callStage.share.title"
                }),
                text::UI,
                15.6,
                color::FG,
            )
            .weight(font::WEIGHT_LABEL),
        )
        .child(label("callStage.share.resolution"))
        .child(
            dilla_desktop_ui::components::segmented::Segmented::new(
                "voice-share-resolution",
                &resolutions,
                o.resolution.name(),
            )
            .on_change(move |v, window, cx| {
                if let Some(resolution) = ShareResolution::from_name(v) {
                    pick(ShareOptions { resolution, ..o }, window, cx);
                }
            }),
        )
        .child(label("callStage.share.frameRate"))
        .child(
            dilla_desktop_ui::components::segmented::Segmented::new(
                "voice-share-fps",
                &[("15", "15"), ("30", "30"), ("60", "60")],
                o.fps.to_string(),
            )
            .on_change(move |v, window, cx| {
                if let Ok(fps) = v.parse() {
                    pick(ShareOptions { fps, ..o }, window, cx);
                }
            }),
        )
        .child(line(
            estimate,
            text::XS,
            14.0,
            if heavy { color::WARN } else { color::FG_3 },
        ))
        .when(!sharing, |d| {
            d.child(tracked_box(
                "voice-share-go",
                dilla_desktop_ui::components::Button::new(
                    "voice-share-go-b",
                    t("callStage.share.go"),
                )
                .on_click(|_, window, cx| {
                    let w = wid(window);
                    if ui_of(w, cx).is_some() {
                        ui_mut(w, cx).share_menu = false;
                    }
                    toggle_share(ShareKind::Screen, w, cx);
                }),
            ))
        });
    Some(
        div()
            .absolute()
            .left_0()
            .right_0()
            .bottom_0()
            .flex()
            .justify_center()
            .child(panel)
            .into_any_element(),
    )
}

/// The tile stats (tile-stats.ts `frameStat`): the stream's height and its frame rate over the last second.
pub fn frame_stat(height: u32, fps: u32) -> String {
    tf(
        "callStage.stat.frame",
        &[("height", &height.to_string()), ("fps", &fps.to_string())],
    )
}

#[allow(
    dead_code,
    reason = "the count label the voice row's screen-reader text uses (Task 8 draws the row)"
)]
fn count_label(n: usize) -> String {
    tf("voice.count", &[("n", &n.to_string())])
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::voice::flags;

    fn state_with(people: &[(u8, u8, u8)]) -> (AppState, Channel) {
        let ch = Channel {
            id: ChannelId([9; 16]),
            community_id: dilla_client::state::CommunityId([1; 16]),
            kind: 1,
            mode: 0,
            name: "Lounge".into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: dilla_client::state::ChannelGroupState::Unsupported,
        };
        let mut s = AppState::default();
        s.channels.push(ch.clone());
        s.voice.place(
            ch.id,
            Some(ch.id.0),
            people
                .iter()
                .map(|(u, d, f)| VoiceParticipant {
                    user_id: UserId([*u; 16]),
                    device_id: DeviceId([*d; 16]),
                    flags: *f,
                    own: false,
                })
                .collect(),
        );
        (s, ch)
    }

    #[test]
    fn people_group_devices_by_user_and_a_mute_counts_when_every_device_is_muted() {
        let (s, ch) = state_with(&[
            (1, 1, flags::IN_CALL | flags::SELF_MUTE),
            (1, 2, flags::IN_CALL),
            (2, 3, 7),
        ]);
        let p = people(&s, &ch);
        assert_eq!(p.len(), 2);
        assert!(!p[0].muted, "one live device keeps the person unmuted");
        assert!(p[1].muted && p[1].deafened);
        assert_eq!(count(&s, ch.id), 2);
    }

    #[test]
    fn the_stage_has_a_tile_per_device_and_one_per_screen_share() {
        let (s, ch) = state_with(&[
            (1, 1, flags::IN_CALL | flags::SCREEN),
            (2, 2, flags::IN_CALL | flags::VIDEO),
        ]);
        let t = tiles(&s, ch.id);
        assert_eq!(t.len(), 3);
        assert!(t[2].screen && t[2].device == DeviceId([1; 16]));
    }

    #[test]
    fn a_tile_stat_names_height_and_rate() {
        assert_eq!(frame_stat(720, 30), "720p · 30 fps");
    }

    #[test]
    fn the_grid_band_follows_the_tile_count() {
        assert_eq!(band(2), (0.46, 560.0));
        assert_eq!(band(4), (0.46, 420.0));
        assert_eq!(band(12), (0.315, 340.0));
    }
}
